package namematcher

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

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

// crossDomainExts is the set of file extensions that produce non-code domain
// entities. A batch containing any of these extensions always triggers the
// name-matching pass regardless of hasCrossDomain state.
var crossDomainExts = map[string]bool{
	".tf":      true, // Terraform (DomainInfra)
	".hcl":     true, // HCL (DomainInfra)
	".graphql": true, // GraphQL (DomainAPI)
	".gql":     true, // GraphQL (DomainAPI)
	".json":    true, // OpenAPI / JSON schemas (DomainAPI)
	".yaml":    true, // OpenAPI YAML (DomainAPI)
	".yml":     true, // OpenAPI YAML (DomainAPI)
	".md":      true, // Markdown documentation (DomainDocs)
	".rst":     true, // reStructuredText documentation (DomainDocs)
}

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

// domainWeight defines BFS traversal priority for edge ordering.
// Heavier domains (code) are edge sources; lighter domains (docs) are targets.
// Package-level to avoid per-call map allocation in orderEdge.
var domainWeight = map[graph.DomainType]int{
	graph.DomainCode:  4,
	graph.DomainAPI:   3,
	graph.DomainInfra: 2,
	graph.DomainDocs:  1,
}

// Matcher scans the graph after each reindex and creates MENTIONS edges
// between entities that share the same logical name across domains.
//
// At most one pass runs at a time — if a new reindex completes while a prior
// pass is still running, the new trigger is silently dropped. The next reindex
// will trigger a fresh pass, so no data is permanently lost.
//
// The pass is skipped entirely when changedFiles contains only code-domain
// files AND no cross-domain entities have ever been observed in the graph.
// This avoids wasteful full-graph scans on code-only projects.
type Matcher struct {
	brainClient    *brain.Client // optional — nil means no LLM validation
	running        atomic.Int32  // 1 while RunAsync is executing; CAS guards single-flight
	hasCrossDomain atomic.Bool   // true once a non-code domain entity is observed in the graph
}

// New creates a Matcher. brainClient may be nil (brain-enhanced path is optional).
func New(brainClient *brain.Client) *Matcher {
	return &Matcher{brainClient: brainClient}
}

// PrimeCrossDomain scans g once to determine whether any non-code-domain
// entities are already present. Must be called after the graph is loaded from
// disk so that subsequent incremental reindex events with code-only changed
// files are not incorrectly skipped by the hasCrossDomain gate.
//
// Safe to call concurrently — reads g under its own lock and writes via an
// atomic store. Idempotent: calling more than once is harmless.
func (m *Matcher) PrimeCrossDomain(g *graph.Graph) {
	if g == nil || m.hasCrossDomain.Load() {
		return // already primed or nothing to scan
	}
	for _, n := range g.AllNodes() {
		if n.Domain != graph.DomainCode && n.Domain != "" {
			m.hasCrossDomain.Store(true)
			return
		}
	}
}

// RunAsync implements watcher.NameMatcherRunner.
//
// changedFiles is the list of files processed in the triggering applyBatch.
// Pass nil to indicate a full re-walk (all domains affected — always run).
//
// The pass is skipped when changedFiles contains only code files AND no
// cross-domain entities have ever been observed, avoiding unnecessary work on
// code-only projects. Once a cross-domain entity is seen (hasCrossDomain=true),
// the check is never re-enabled — cross-domain entities typically persist.
//
// At most one invocation runs at a time — concurrent calls return immediately.
// Respects ctx cancellation. All errors are logged and skipped (fail-open).
func (m *Matcher) RunAsync(ctx context.Context, g *graph.Graph, st *store.Store, changedFiles []string) {
	if g == nil || st == nil {
		return
	}

	// Domain-relevance gate: skip if no cross-domain files changed and no
	// cross-domain entities have been seen yet. This avoids a full graph scan
	// on every .go file save in a code-only project.
	// nil changedFiles = full re-walk; always run.
	if changedFiles != nil && !m.hasCrossDomain.Load() && !hasCrossDomainFiles(changedFiles) {
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
		// Record the first time a non-code entity is observed so future
		// code-only batches still trigger the pass (new code entity may match
		// an existing infra/api/docs entity).
		if n.Domain != graph.DomainCode {
			m.hasCrossDomain.Store(true)
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

				// Sprint 28: classify cross-domain edge type based on the
				// non-code node's domain instead of always using MENTIONS.
				// This enables get_context to populate specific cross-domain
				// buckets (deploys, consumes, configured_by, documented_in).
				from, to := orderEdge(a.node, b.node)
				edgeType := classifyCrossDomainEdge(a.node, b.node)

				me, err := st.SaveSyntheticEdge(from.ID, to.ID, edgeType, conf)
				if err != nil {
					logutil.Warn("synapses/namematcher: persist edge %s→%s: %v\n", from.ID, to.ID, err)
					continue
				}
				// Skip re-injection if a human rejected this edge — suppressed edges must
				// not re-appear in the live graph between restarts.
				if me.Suppressed {
					continue
				}
				g.AddEdge(&graph.Edge{From: from.ID, To: to.ID, Type: edgeType})
				created++
			}
		}
	}

	if created > 0 {
		logutil.Info("synapses/namematcher: created %d MENTIONS edges\n", created)
	}
}

// classifyCrossDomainEdge determines the specific edge type for a cross-domain
// name match based on the non-code node's domain. Falls back to EdgeMentions.
func classifyCrossDomainEdge(a, b *graph.Node) graph.EdgeType {
	// Determine which node is the non-code node.
	nonCode := b
	if a.Domain != graph.DomainCode && a.Domain != "" {
		nonCode = a
	}

	switch nonCode.Domain {
	case graph.DomainInfra:
		// Infrastructure files (Terraform, K8s, Docker, CI) → EdgeConfiguredBy or EdgeDeploys.
		// Heuristic: Dockerfile/K8s manifests deploy code; Terraform/config configures it.
		ext := strings.ToLower(filepath.Ext(nonCode.File))
		base := strings.ToLower(filepath.Base(nonCode.File))
		if strings.Contains(base, "dockerfile") || strings.Contains(base, "docker-compose") ||
			ext == ".yaml" || ext == ".yml" {
			// Check if it's a K8s/deploy manifest vs general config.
			if strings.Contains(base, "deploy") || strings.Contains(base, "kube") ||
				strings.Contains(base, "k8s") || strings.Contains(base, "docker") ||
				strings.Contains(base, "helm") {
				return graph.EdgeDeploys
			}
		}
		return graph.EdgeConfiguredBy
	case graph.DomainAPI:
		// API schema files (OpenAPI, GraphQL, protobuf) → EdgeConsumes.
		return graph.EdgeConsumes
	case graph.DomainDocs:
		// Documentation files → EdgeDocuments (which routes to documented_in bucket).
		return graph.EdgeDocuments
	default:
		return graph.EdgeMentions
	}
}

// hasCrossDomainFiles returns true if any file in the list has an extension
// associated with a non-code domain (infra, api, docs).
func hasCrossDomainFiles(files []string) bool {
	for _, f := range files {
		if crossDomainExts[strings.ToLower(filepath.Ext(f))] {
			return true
		}
	}
	return false
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
	if isSemanticallyCorrelated(a, b) {
		score += semanticTypeBoost
	}

	if score > 1.0 {
		score = 1.0
	}
	return score
}

// isSemanticallyCorrelated returns true when the two nodes' domain+type
// combination appears in the semanticTypeCorrelations table.
func isSemanticallyCorrelated(a, b *graph.Node) bool {
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
// Uses package-level domainWeight to avoid per-call map allocation.
func orderEdge(a, b *graph.Node) (*graph.Node, *graph.Node) {
	if domainWeight[a.Domain] >= domainWeight[b.Domain] {
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
// brainEnhanceTimeout caps each LLM call so a hanging brain API cannot stall
// the entire namematcher pass. 5 s is generous for a YES/NO answer.
const brainEnhanceTimeout = 5 * time.Second

func (m *Matcher) brainEnhance(ctx context.Context, a, b *graph.Node, baseConf float64) float64 {
	prompt := "Do these two entities refer to the same concept? Answer YES or NO only.\n" +
		"Entity 1: " + a.Name + " (domain=" + string(a.Domain) + ", type=" + string(a.Type) + ", file=" + filepath.Base(a.File) + ")\n" +
		"Entity 2: " + b.Name + " (domain=" + string(b.Domain) + ", type=" + string(b.Type) + ", file=" + filepath.Base(b.File) + ")"

	callCtx, cancel := context.WithTimeout(ctx, brainEnhanceTimeout)
	defer cancel()
	resp, err := m.brainClient.Generate(callCtx, prompt)
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
