package namematcher

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/store"
)

const (
	// minConfidence is the minimum score required to auto-create a MENTIONS edge.
	// False positive edges erode trust faster than missing edges (precision > recall).
	minConfidence = 0.6

	// baseConfidence is the starting score for a cross-domain normalized name match.
	// Set above minConfidence so that any normalized cross-domain name match is
	// auto-created without requiring additional context signals.
	baseConfidence = 0.65

	// exactNameBoost is added when names match exactly (not just after normalization).
	exactNameBoost = 0.10

	// sameDirectoryBoost is added when both entities share the same parent directory.
	sameDirectoryBoost = 0.10

	// semanticTypeBoost is added when entity types are semantically correlated
	// across domains (e.g. code struct ↔ infra resource, function ↔ API endpoint).
	semanticTypeBoost = 0.10

	// brainBoost is added to confidence when the brain LLM validates a match.
	brainBoost = 0.15

	// minNameLen is the minimum normalized name length to consider.
	// Very short names (< 4 chars) are too likely to collide accidentally.
	minNameLen = 4
)

// skipNodeTypes contains node types that should not be matched.
// These are structural containers, not semantic entities.
var skipNodeTypes = map[graph.NodeType]bool{
	graph.NodeFile:    true,
	graph.NodePackage: true,
	"directory":       true,
	"module":          true,
}

// semanticTypeCorrelations maps (sourceDomain+nodeType, targetDomain+nodeType) pairs
// that are semantically correlated across domains. Key format: "domain:type".
// Symmetric — checked in both directions.
var semanticTypeCorrelations = [][2]string{
	{"code:struct", "infra:resource"},
	{"code:interface", "api:endpoint"},
	{"code:function", "api:operation"},
	{"code:struct", "api:schema"},
	{"code:const", "infra:variable"},
	{"docs:section", "code:function"},
	{"docs:section", "code:struct"},
}

// Matcher scans the graph after each reindex and creates MENTIONS edges
// between entities that share the same logical name across domains.
//
// At most one pass runs at a time — if a new reindex completes while a prior
// pass is still running, the new trigger is silently dropped. The next reindex
// will trigger a fresh pass, so no data is permanently lost.
type Matcher struct {
	brainClient *brain.Client // optional — nil means no LLM validation
	running     atomic.Int32  // 1 while RunAsync is executing; CAS guards single-flight
}

// New creates a Matcher. brainClient may be nil (brain-enhanced path is optional).
func New(brainClient *brain.Client) *Matcher {
	return &Matcher{brainClient: brainClient}
}

// RunAsync implements watcher.NameMatcherRunner.
// Scans g for cross-domain name matches, scores candidate pairs, and
// creates MENTIONS edges with confidence >= minConfidence via st.
// At most one invocation runs at a time — concurrent calls return immediately.
// Respects ctx cancellation. All errors are logged and skipped (fail-open).
func (m *Matcher) RunAsync(ctx context.Context, g *graph.Graph, st *store.Store) {
	if g == nil || st == nil {
		return
	}
	// Single-flight guard: if a prior pass is still running, drop this trigger.
	// The next applyBatch will fire another trigger so no data is permanently skipped.
	if !m.running.CompareAndSwap(0, 1) {
		return
	}
	defer m.running.Store(0)

	// Prune stale synthetic edges before scanning — removes DB rows for renamed
	// or deleted entities so the table does not accumulate garbage indefinitely.
	if err := st.PruneStaleSyntheticEdges(g); err != nil {
		logutil.Warn("synapses/namematcher: prune stale edges: %v\n", err)
		// non-fatal — continue with the matching pass
	}

	nodes := g.AllNodes()
	if len(nodes) == 0 {
		return
	}

	// Group nodes by normalized name, filtering out generic names and
	// structural node types. Only nodes from code/infra/api/docs domains are candidates.
	type candidate struct {
		node       *graph.Node
		normalized string
	}
	byName := make(map[string][]candidate, len(nodes)/4)
	for _, n := range nodes {
		if skipNodeTypes[n.Type] {
			continue
		}
		// Only match across meaningful domains (not knowledge/issues/custom).
		switch n.Domain {
		case graph.DomainCode, graph.DomainInfra, graph.DomainAPI, graph.DomainDocs:
		default:
			continue
		}
		if n.Name == "" {
			continue
		}
		norm := normalizeEntityName(n.Name)
		if len(norm) < minNameLen || isGenericName(norm) {
			continue
		}
		byName[norm] = append(byName[norm], candidate{node: n, normalized: norm})
	}

	// For each name group with entities from >1 domain, score all cross-domain pairs.
	created := 0
	for _, candidates := range byName {
		// Check context cancellation between name groups.
		select {
		case <-ctx.Done():
			logutil.Info("synapses/namematcher: cancelled after %d edges created\n", created)
			return
		default:
		}

		if len(candidates) < 2 {
			continue
		}

		// Only proceed if candidates span at least 2 distinct domains.
		domains := make(map[graph.DomainType]bool, 4)
		for _, c := range candidates {
			domains[c.node.Domain] = true
		}
		if len(domains) < 2 {
			continue
		}

		// Score all cross-domain pairs. Skip same-domain pairs.
		for i := 0; i < len(candidates); i++ {
			for j := i + 1; j < len(candidates); j++ {
				a, b := candidates[i], candidates[j]
				if a.node.Domain == b.node.Domain {
					continue
				}

				conf := scoreMatch(a.node, b.node)

				// Brain enhancement (optional): only for sub-threshold pairs that
				// the brain could push over the minimum. Avoids calling the LLM for
				// every pair that already exceeds minConfidence on heuristics alone.
				if m.brainClient != nil && m.brainClient.Available() &&
					conf < minConfidence && conf >= minConfidence-brainBoost {
					conf = m.brainEnhance(ctx, a.node, b.node, conf)
				}

				if conf < minConfidence {
					continue
				}

				// Create MENTIONS edge from code→api→infra→docs (heavier domain first).
				from, to := orderEdge(a.node, b.node)

				me, err := st.SaveSyntheticEdge(from.ID, to.ID, graph.EdgeMentions, conf)
				if err != nil {
					logutil.Warn("synapses/namematcher: persist edge %s→%s: %v\n", from.ID, to.ID, err)
					continue
				}
				// Skip re-injection if a human rejected this edge — suppressed edges must
				// not re-appear in the live graph between restarts.
				if me.Suppressed {
					continue
				}
				g.AddEdge(&graph.Edge{From: from.ID, To: to.ID, Type: graph.EdgeMentions})
				created++
			}
		}
	}

	if created > 0 {
		logutil.Info("synapses/namematcher: created %d MENTIONS edges\n", created)
	}
}

// scoreMatch computes a confidence score (0.0-1.0) for a cross-domain name match.
func scoreMatch(a, b *graph.Node) float64 {
	score := baseConfidence

	// Exact name match (not just normalized) — higher confidence.
	if strings.EqualFold(a.Name, b.Name) {
		score += exactNameBoost
	}

	// Same directory — strong co-location signal.
	if a.File != "" && b.File != "" {
		if filepath.Dir(a.File) == filepath.Dir(b.File) {
			score += sameDirectoryBoost
		}
	}

	// Semantic type correlation across domains.
	if isSemanticallyCorrrelated(a, b) {
		score += semanticTypeBoost
	}

	if score > 1.0 {
		score = 1.0
	}
	return score
}

// isSemanticallyCorrrelated returns true when the two nodes' domain+type
// combination appears in the semanticTypeCorrelations table.
func isSemanticallyCorrrelated(a, b *graph.Node) bool {
	keyA := string(a.Domain) + ":" + string(a.Type)
	keyB := string(b.Domain) + ":" + string(b.Type)
	for _, pair := range semanticTypeCorrelations {
		if (pair[0] == keyA && pair[1] == keyB) || (pair[0] == keyB && pair[1] == keyA) {
			return true
		}
	}
	return false
}

// orderEdge returns (from, to) so that the MENTIONS edge points from the
// "heavier" domain (code) toward the "lighter" domain (infra/api/docs).
// This makes BFS traversal consistent and predictable.
func orderEdge(a, b *graph.Node) (*graph.Node, *graph.Node) {
	weight := map[graph.DomainType]int{
		graph.DomainCode:  4,
		graph.DomainAPI:   3,
		graph.DomainInfra: 2,
		graph.DomainDocs:  1,
	}
	if weight[a.Domain] >= weight[b.Domain] {
		return a, b
	}
	return b, a
}

// brainEnhance asks the brain LLM whether two entities with matching names
// are semantically related. Only called for sub-threshold pairs (conf < minConfidence)
// where the brain boost could push them over the threshold — never called for pairs
// that already exceed minConfidence on heuristics alone.
// Returns the boosted confidence if the brain validates the match, or the original
// confidence if unavailable or it rejects. Best-effort: any error returns baseConf.
func (m *Matcher) brainEnhance(ctx context.Context, a, b *graph.Node, baseConf float64) float64 {
	prompt := "Do these two entities refer to the same concept? Answer YES or NO only.\n" +
		"Entity 1: " + a.Name + " (domain=" + string(a.Domain) + ", type=" + string(a.Type) + ", file=" + filepath.Base(a.File) + ")\n" +
		"Entity 2: " + b.Name + " (domain=" + string(b.Domain) + ", type=" + string(b.Type) + ", file=" + filepath.Base(b.File) + ")"

	resp, err := m.brainClient.Generate(ctx, prompt)
	if err != nil {
		return baseConf
	}
	answer := strings.TrimSpace(strings.ToUpper(resp))
	if strings.HasPrefix(answer, "YES") {
		boosted := baseConf + brainBoost
		if boosted > 1.0 {
			boosted = 1.0
		}
		return boosted
	}
	// Brain rejected the match — it stays below threshold (no change needed since
	// we only call brainEnhance for pairs already below minConfidence).
	return baseConf
}
