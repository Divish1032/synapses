package graph

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Graph is the core in-memory code graph. It is safe for concurrent reads
// and writes — a RWMutex serialises mutations while allowing parallel queries.
type Graph struct {
	mu        sync.RWMutex
	repoID    string
	root      string // absolute path to the repository root; may be empty
	nodes     map[NodeID]*Node
	outEdges  map[NodeID][]*Edge // edges leaving a node
	inEdges   map[NodeID][]*Edge // edges arriving at a node
	callSites []CallSite         // temporary: accumulated during parse, drained by resolver
	cache     *subgraphCache     // in-memory cache for carved subgraphs (30s TTL, max 20 entries)

	// Cached ProjectIdentity result — invalidated on graph mutation.
	piCache   *ProjectIdentity
	piCacheAt int64 // unix timestamp of when piCache was computed
}

// New creates an empty Graph for the given repository identifier.
func New(repoID string) *Graph {
	return &Graph{
		repoID:   repoID,
		nodes:    make(map[NodeID]*Node),
		outEdges: make(map[NodeID][]*Edge),
		inEdges:  make(map[NodeID][]*Edge),
		cache:    newSubgraphCache(),
	}
}

// RepoID returns the repository identifier this graph was built for.
func (g *Graph) RepoID() string {
	return g.repoID
}

// Root returns the absolute filesystem path of the repository root.
// It is empty if the graph was loaded from a store that predates this field.
func (g *Graph) Root() string {
	return g.root
}

// SetRoot stores the absolute path of the repository root.
func (g *Graph) SetRoot(root string) {
	g.root = root
}

// MakeNodeID constructs a canonical NodeID from its components.
// Format: "repoID::file::name"
//
// When the graph has a repo root set, file paths are stored as project-relative
// paths (e.g. "cmd/synapses/main.go" instead of "/Users/you/.../main.go").
// This significantly reduces token consumption in MCP responses. Node.File
// retains the absolute path for internal operations like RemoveFile.
func (g *Graph) MakeNodeID(file, name string) NodeID {
	relFile := file
	if g.root != "" {
		prefix := g.root
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if strings.HasPrefix(file, prefix) {
			relFile = strings.TrimPrefix(file, prefix)
		}
	}
	return NodeID(fmt.Sprintf("%s::%s::%s", g.repoID, relFile, name))
}

// AddNode inserts or replaces a node. If a node with the same ID already
// exists it is overwritten — the caller is responsible for deduplication.
func (g *Graph) AddNode(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.ID] = n
	g.piCache = nil // invalidate ProjectIdentity cache
}

// AddEdge inserts a directed edge. Both endpoint nodes must already exist;
// if either is absent the edge is silently dropped to avoid dangling refs.
func (g *Graph) AddEdge(e *Edge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.nodes[e.From]; !ok {
		return
	}
	if _, ok := g.nodes[e.To]; !ok {
		return
	}
	g.outEdges[e.From] = append(g.outEdges[e.From], e)
	g.inEdges[e.To] = append(g.inEdges[e.To], e)
	g.piCache = nil // invalidate ProjectIdentity cache
}

// GetNode returns the node for a given ID, or nil if absent.
func (g *Graph) GetNode(id NodeID) *Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.nodes[id]
}

// FindByName returns all nodes whose Name field matches the given string
// (case-insensitive). An empty slice is returned if nothing matches.
func (g *Graph) FindByName(name string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	lower := strings.ToLower(name)
	var results []*Node
	for _, n := range g.nodes {
		if strings.ToLower(n.Name) == lower {
			results = append(results, n)
		}
	}
	return results
}

// FindByPattern returns all nodes whose Name contains the given substring
// (case-insensitive). Useful for fuzzy "find entity" queries.
func (g *Graph) FindByPattern(pattern string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	lower := strings.ToLower(pattern)
	var results []*Node
	for _, n := range g.nodes {
		if strings.Contains(strings.ToLower(n.Name), lower) {
			results = append(results, n)
		}
	}
	return results
}

// Fanout returns the number of outgoing edges from the given node.
func (g *Graph) Fanout(id NodeID) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.outEdges[id])
}

// Fanin returns the number of incoming edges to the given node.
func (g *Graph) Fanin(id NodeID) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.inEdges[id])
}

// OutEdges returns a copy of all edges leaving the given node.
func (g *Graph) OutEdges(id NodeID) []*Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	edges := g.outEdges[id]
	out := make([]*Edge, len(edges))
	copy(out, edges)
	return out
}

// InEdges returns a copy of all edges arriving at the given node.
func (g *Graph) InEdges(id NodeID) []*Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	edges := g.inEdges[id]
	out := make([]*Edge, len(edges))
	copy(out, edges)
	return out
}

// AddCallSite records an unresolved call site for post-parse resolution.
func (g *Graph) AddCallSite(cs CallSite) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.callSites = append(g.callSites, cs)
}

// InvalidateCache discards all cached subgraph results. Call this after any
// batch of graph mutations (e.g. after the watcher re-parses a file) so that
// subsequent get_context calls see fresh data.
func (g *Graph) InvalidateCache() {
	g.cache.invalidate()
}

// PeekCallSites returns a copy of all pending call sites without clearing them.
// Used to persist call sites to the store before the resolver drains them.
func (g *Graph) PeekCallSites() []CallSite {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]CallSite, len(g.callSites))
	copy(out, g.callSites)
	return out
}

// DrainCallSites returns all pending call sites and clears the internal list.
// Called by the resolver after all files have been parsed.
func (g *Graph) DrainCallSites() []CallSite {
	g.mu.Lock()
	defer g.mu.Unlock()
	cs := g.callSites
	g.callSites = nil
	return cs
}

// AllNodes returns a snapshot of every node in the graph.
func (g *Graph) AllNodes() []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]*Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n)
	}
	return out
}

// AllEdges returns a snapshot of every edge in the graph.
func (g *Graph) AllEdges() []*Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []*Edge
	for _, edges := range g.outEdges {
		out = append(out, edges...)
	}
	return out
}

// NodeCount returns the total number of nodes.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

// EdgeCount returns the total number of edges.
func (g *Graph) EdgeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	total := 0
	for _, edges := range g.outEdges {
		total += len(edges)
	}
	return total
}

// EdgeCountsByType returns the number of edges per edge type.
func (g *Graph) EdgeCountsByType() map[EdgeType]int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	counts := make(map[EdgeType]int)
	for _, edges := range g.outEdges {
		for _, e := range edges {
			counts[e.Type]++
		}
	}
	return counts
}

// MergeFrom copies all nodes and edges from other into g.
// Existing nodes in g are never overwritten — other's data is purely additive.
// This is used at startup to merge federated (linked) project graphs so that
// cross-project context is available via get_context and find_entity.
func (g *Graph) MergeFrom(other *Graph) {
	// Snapshot other under its read lock, then release before acquiring g's write lock.
	nodes := other.AllNodes()
	edges := other.AllEdges()

	g.mu.Lock()
	defer g.mu.Unlock()

	// Copy nodes (skip if already present in g).
	for _, n := range nodes {
		if _, exists := g.nodes[n.ID]; !exists {
			g.nodes[n.ID] = n
		}
	}
	// Copy edges — both endpoints must now be present.
	for _, e := range edges {
		if _, ok := g.nodes[e.From]; !ok {
			continue
		}
		if _, ok := g.nodes[e.To]; !ok {
			continue
		}
		g.outEdges[e.From] = append(g.outEdges[e.From], e)
		g.inEdges[e.To] = append(g.inEdges[e.To], e)
	}
}

// RemoveCallSitesForFile removes any pending call sites whose CallerFile matches
// the given path. Called by the watcher before re-parsing a changed file so that
// stale call sites from the old version are not mixed with the newly parsed ones.
func (g *Graph) RemoveCallSitesForFile(file string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	filtered := g.callSites[:0]
	for _, cs := range g.callSites {
		if cs.CallerFile != file {
			filtered = append(filtered, cs)
		}
	}
	g.callSites = filtered
}

// RemoveFile removes all nodes and their associated edges for a given file path.
// Used by the file watcher to prune stale data before re-parsing.
func (g *Graph) RemoveFile(file string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Collect node IDs belonging to this file.
	var toRemove []NodeID
	for id, n := range g.nodes {
		if n.File == file {
			toRemove = append(toRemove, id)
		}
	}

	// Remove edges then nodes.
	for _, id := range toRemove {
		for _, e := range g.outEdges[id] {
			g.removeInEdge(e.To, id)
		}
		for _, e := range g.inEdges[id] {
			g.removeOutEdge(e.From, id)
		}
		delete(g.outEdges, id)
		delete(g.inEdges, id)
		delete(g.nodes, id)
	}
	if len(toRemove) > 0 {
		g.piCache = nil // invalidate ProjectIdentity cache
	}
}

func (g *Graph) removeInEdge(nodeID, fromID NodeID) {
	edges := g.inEdges[nodeID]
	filtered := edges[:0]
	for _, e := range edges {
		if e.From != fromID {
			filtered = append(filtered, e)
		}
	}
	g.inEdges[nodeID] = filtered
}

func (g *Graph) removeOutEdge(nodeID, toID NodeID) {
	edges := g.outEdges[nodeID]
	filtered := edges[:0]
	for _, e := range edges {
		if e.To != toID {
			filtered = append(filtered, e)
		}
	}
	g.outEdges[nodeID] = filtered
}

// relPath trims the repo root prefix from an absolute file path, returning a
// project-relative path. If the path doesn't start with the root, it is
// returned as-is. Caller must hold at least g.mu.RLock().
func (g *Graph) relPath(abs string) string {
	if g.root == "" {
		return abs
	}
	prefix := g.root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if strings.HasPrefix(abs, prefix) {
		return strings.TrimPrefix(abs, prefix)
	}
	return abs
}

// ProjectIdentity computes a compact architectural summary of the graph.
// This is the payload returned by the get_project_identity MCP tool.
// Results are cached for 30 seconds and invalidated on graph mutations.
func (g *Graph) ProjectIdentity() *ProjectIdentity {
	g.mu.RLock()
	// Return cached result if still fresh (30s TTL).
	if g.piCache != nil && time.Now().Unix()-g.piCacheAt < 30 {
		cached := g.piCache
		g.mu.RUnlock()
		return cached
	}
	g.mu.RUnlock()

	g.mu.RLock()
	defer g.mu.RUnlock()

	summary := GraphSummary{}

	for _, n := range g.nodes {
		switch n.Type {
		case NodeFile:
			summary.Files++
		case NodePackage:
			summary.Packages++
		case NodeFunction:
			summary.Functions++
		case NodeMethod:
			summary.Methods++
		case NodeStruct:
			summary.Structs++
		case NodeInterface:
			summary.Interfaces++
		}
	}

	// Count all edges.
	for _, edges := range g.outEdges {
		summary.Edges += len(edges)
	}

	// Identify entry points: exported functions named "main" or functions
	// with no incoming CALLS edges (nothing calls them — they are roots).
	var entryPoints []EntityRef
	for _, n := range g.nodes {
		if n.Type != NodeFunction {
			continue
		}
		if n.Name == "main" || (n.Exported && len(g.inEdges[n.ID]) == 0) {
			entryPoints = append(entryPoints, EntityRef{
				ID:   n.ID,
				Name: n.Name,
				Type: n.Type,
				File: g.relPath(n.File),
				Line: n.Line,
			})
		}
	}

	// Key entities: top-10 nodes by combined fanin + fanout (connectivity).
	type scored struct {
		node  *Node
		score int
	}
	var candidates []scored
	for _, n := range g.nodes {
		if n.Type == NodeFile || n.Type == NodePackage {
			continue
		}
		// Skip test files — test helpers like assertNode dominate connectivity
		// but are not architecturally significant.
		if strings.Contains(n.File, "_test.go") || strings.HasSuffix(n.File, "_test.py") ||
			strings.Contains(n.File, ".test.") || strings.Contains(n.File, ".spec.") {
			continue
		}
		s := len(g.inEdges[n.ID]) + len(g.outEdges[n.ID])
		if s > 0 {
			candidates = append(candidates, scored{n, s})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > 10 {
		candidates = candidates[:10]
	}
	keyEntities := make([]EntityInfo, 0, len(candidates))
	for _, c := range candidates {
		keyEntities = append(keyEntities, EntityInfo{
			EntityRef: EntityRef{
				ID:   c.node.ID,
				Name: c.node.Name,
				Type: c.node.Type,
				File: g.relPath(c.node.File),
				Line: c.node.Line,
			},
			Fanin:  len(g.inEdges[c.node.ID]),
			Fanout: len(g.outEdges[c.node.ID]),
		})
	}

	result := &ProjectIdentity{
		RepoID:         g.repoID,
		Summary:        summary,
		EntryPoints:    entryPoints,
		KeyEntities:    keyEntities,
		SuggestedRules: g.SuggestRules(),
	}
	// Best-effort cache — safe because piCache is only read under RLock
	// and invalidated under Lock in AddNode/AddEdge/RemoveFile.
	g.piCache = result
	g.piCacheAt = time.Now().Unix()
	return result
}

// SuggestRules detects high-density CALLS coupling between directory groups.
// It groups CALLS edges by their source and target directories, then surfaces
// directory pairs where ≥85% of nodes in the from-dir call into the to-dir
// (minimum 3 samples) as suggested architectural rules.
//
// Must be called under g.mu.RLock() — used by ProjectIdentity() which already
// holds the read lock. Do NOT call g.mu.RLock() inside this method.
//
// Returns up to 5 suggestions ordered by confidence descending.
func (g *Graph) SuggestRules() []SuggestedRule {
	type pair struct{ from, to string }

	// edgeNodes[pair] = set of from-node IDs that call into to-dir
	edgeNodes := make(map[pair]map[NodeID]bool)
	// allCallers[fromDir] = set of all from-node IDs that make any CALLS edge
	allCallers := make(map[string]map[NodeID]bool)

	for _, edges := range g.outEdges {
		for _, e := range edges {
			if e.Type != EdgeCalls {
				continue
			}
			fromNode, toNode := g.nodes[e.From], g.nodes[e.To]
			if fromNode == nil || toNode == nil {
				continue
			}
			fromDir := path.Dir(g.relPath(fromNode.File))
			toDir := path.Dir(g.relPath(toNode.File))
			// Skip same-directory calls and top-level files.
			if fromDir == "." || toDir == "." || fromDir == toDir {
				continue
			}

			p := pair{fromDir, toDir}
			if edgeNodes[p] == nil {
				edgeNodes[p] = make(map[NodeID]bool)
			}
			if allCallers[fromDir] == nil {
				allCallers[fromDir] = make(map[NodeID]bool)
			}
			edgeNodes[p][e.From] = true
			allCallers[fromDir][e.From] = true
		}
	}

	var out []SuggestedRule
	for p, callers := range edgeNodes {
		total := len(allCallers[p.from])
		if total == 0 {
			continue
		}
		conf := float64(len(callers)) / float64(total)
		// Only surface high-confidence patterns with enough samples to be meaningful.
		if conf < 0.85 || len(callers) < 3 {
			continue
		}
		fromBase := path.Base(p.from)
		toBase := path.Base(p.to)
		out = append(out, SuggestedRule{
			ID: "suggest-" + sanitizeDirName(fromBase) + "-calls-" + sanitizeDirName(toBase),
			Description: fmt.Sprintf(
				"%d/%d nodes in %s call into %s (%.0f%% coupling) — consider formalizing as a rule",
				len(callers), total, p.from, p.to, conf*100,
			),
			Confidence:     math.Round(conf*100) / 100,
			SampleCount:    len(callers),
			FromDirPattern: "*/" + fromBase + "/*",
			ToDirPattern:   "*/" + toBase + "/*",
			EdgeType:       EdgeCalls,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Confidence > out[j].Confidence })
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// sanitizeDirName replaces path separators and spaces with dashes, making a
// directory name safe for use as part of a rule ID slug.
func sanitizeDirName(s string) string {
	return strings.NewReplacer("/", "-", "\\", "-", " ", "-").Replace(s)
}
