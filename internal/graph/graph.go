package graph

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // SHA1 used for structural fingerprint, not cryptographic security
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// stableIDRecord holds the identity data used to migrate stable UUIDs across
// file renames and re-parses.
type stableIDRecord struct {
	name string
	pkg  string
	sig  string
	id   string // the stable UUID to reuse
}

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

	// fileStableIDs stores stable UUID snapshots keyed by absolute file path.
	// Populated by SnapshotFileStableIDs before RemoveFile; consumed by MigrateStableID.
	fileStableIDs map[string][]stableIDRecord

	// Cached ProjectIdentity result — invalidated on graph mutation.
	piCache   *ProjectIdentity
	piCacheAt int64 // unix timestamp of when piCache was computed

	// index is the read-optimised columnar (SoA) view of the graph.
	// It is rebuilt asynchronously after each full parse cycle via RebuildIndex().
	// BFS traversal uses it when index.Ready() is true; falls back to the map otherwise.
	index *GraphIndex

	// indexMu ensures only one index rebuild runs at a time.
	indexMu sync.Mutex

	// pool is the shared string interning pool used by GraphIndex.
	// Kept on Graph so it persists across index rebuilds (strings stay interned).
	pool *StringPool

	// varTypes stores per-file variable type annotations collected during parsing.
	// Maps file path → variable name → type name (e.g. "repo" → "Repository").
	// Used by the resolver to resolve obj.method() call sites cross-file.
	varTypes map[string]map[string]string
}

// generateStableID returns a random UUID v4 using crypto/rand (no external deps).
func generateStableID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand.Read failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// New creates an empty Graph for the given repository identifier.
func New(repoID string) *Graph {
	return &Graph{
		repoID:        repoID,
		nodes:         make(map[NodeID]*Node),
		outEdges:      make(map[NodeID][]*Edge),
		inEdges:       make(map[NodeID][]*Edge),
		cache:         newSubgraphCache(),
		fileStableIDs: make(map[string][]stableIDRecord),
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
// A stable UUID is generated for n.StableID if it is empty.
func (g *Graph) AddNode(n *Node) {
	if n.StableID == "" {
		n.StableID = generateStableID()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nodes[n.ID] = n
	g.piCache = nil // invalidate ProjectIdentity cache
}

// AddEdge inserts a directed edge. Both endpoint nodes must already exist;
// if either is absent the edge is silently dropped to avoid dangling refs.
// Duplicate edges (same From, To, Type) are silently dropped so that
// repeated calls from incremental reindex or heuristic passes are idempotent.
func (g *Graph) AddEdge(e *Edge) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.nodes[e.From]; !ok {
		return
	}
	if _, ok := g.nodes[e.To]; !ok {
		return
	}
	// Deduplicate: skip if this exact (From, To, Type) triple already exists.
	for _, existing := range g.outEdges[e.From] {
		if existing.To == e.To && existing.Type == e.Type {
			return
		}
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
// (case-insensitive). Also matches qualified names: searching "Close" will
// match a node named "Store.Close" (suffix after the last dot). An empty
// slice is returned if nothing matches.
//
// Uses the secondary index for O(1) lookup when available; falls back to O(N)
// scan during initial parsing when the index is not yet ready.
func (g *Graph) FindByName(name string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Fast path: index is built and immutable — no extra locking needed.
	if idx := g.index; idx != nil && idx.Ready() {
		seqs := idx.nameSeqs(name)
		if len(seqs) == 0 {
			return []*Node{}
		}
		results := make([]*Node, 0, len(seqs))
		for _, seq := range seqs {
			if int(seq) < len(idx.SeqIDs) {
				if n := g.nodes[idx.SeqIDs[seq]]; n != nil {
					results = append(results, n)
				}
			}
		}
		return results
	}

	// Fallback: linear scan (during parsing before index is ready).
	lower := strings.ToLower(name)
	var results []*Node
	for _, n := range g.nodes {
		nodeLower := strings.ToLower(n.Name)
		if nodeLower == lower {
			results = append(results, n)
			continue
		}
		if dotPos := strings.LastIndex(nodeLower, "."); dotPos >= 0 && nodeLower[dotPos+1:] == lower {
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

// FindByFile returns all nodes whose File field matches the given path.
// The match is suffix-based so callers may pass either a full absolute path or
// a relative path such as "internal/graph/graph.go"; both resolve correctly
// against the absolute paths that the parser stores on each node.
//
// Uses the secondary index for O(1) lookup when available; falls back to O(N)
// scan during initial parsing when the index is not yet ready.
func (g *Graph) FindByFile(filePath string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Fast path: index is built and immutable — no extra locking needed.
	if idx := g.index; idx != nil && idx.Ready() {
		seqs := idx.fileSeqs(filePath)
		if len(seqs) == 0 {
			return []*Node{}
		}
		results := make([]*Node, 0, len(seqs))
		for _, seq := range seqs {
			if int(seq) < len(idx.SeqIDs) {
				if n := g.nodes[idx.SeqIDs[seq]]; n != nil {
					results = append(results, n)
				}
			}
		}
		return results
	}

	// Fallback: linear scan with suffix matching (during parsing before index is ready).
	var results []*Node
	for _, n := range g.nodes {
		if n.File == filePath || strings.HasSuffix(n.File, "/"+filePath) {
			results = append(results, n)
		}
	}
	return results
}

// UpsertRouteNode atomically inserts a synthetic route node if one with the
// same ID does not already exist. Returns true if the node was newly created.
// This is the safe alternative to the non-atomic GetNode+AddNode pattern: by
// holding the write lock for both the existence check and the insert, two
// concurrent incremental-reindex goroutines cannot both create the same route
// with different StableIDs.
func (g *Graph) UpsertRouteNode(n *Node) bool {
	if n.StableID == "" {
		n.StableID = generateStableID()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.nodes[n.ID]; exists {
		return false
	}
	g.nodes[n.ID] = n
	g.piCache = nil
	return true
}

// SetFileProvenance sets the Provenance field on all nodes whose File matches
// filePath. Runs under a write lock to avoid the data race that would occur if
// callers mutated node pointers returned by FindByFile after releasing the lock.
func (g *Graph) SetFileProvenance(filePath string, p ProvenanceType) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.nodes {
		if n.File == filePath || strings.HasSuffix(n.File, "/"+filePath) {
			n.Provenance = p
		}
	}
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

// InvalidateCacheForFile evicts only cached subgraphs that reference the given
// file. Entries for unrelated entities survive, dramatically improving cache
// hit rates when a single file changes. Prefer this over InvalidateCache when
// you know which file was modified.
func (g *Graph) InvalidateCacheForFile(file string) {
	g.cache.invalidateForFile(file)
}

// nodeFingerprintLocked computes a structural fingerprint for the node with
// the given ID.  The fingerprint encodes the node's signature and its
// direct structural neighbourhood (sorted neighbour IDs + edge types) so that
// it changes when and only when the node's observable structure changes.
//
// Comment-only edits do not alter the signature or the edge set, so the
// fingerprint stays the same and cached subgraphs built around this node
// remain valid.
//
// Format: SHA1(nodeType:signature|from:edgeType:to|from:edgeType:to|...)
// where the edge tokens are sorted lexicographically for determinism.
//
// Must be called with g.mu.RLock (or g.mu.Lock) already held.
// Returns an empty string if the node does not exist.
func (g *Graph) nodeFingerprintLocked(id NodeID) string {
	n, ok := g.nodes[id]
	if !ok {
		return ""
	}

	// Collect edge tokens: each edge is represented as "from:edgeType:to".
	// Collecting both out-edges and in-edges gives a complete structural view.
	var tokens []string
	for _, e := range g.outEdges[id] {
		tokens = append(tokens, string(e.From)+":"+string(e.Type)+":"+string(e.To))
	}
	for _, e := range g.inEdges[id] {
		tokens = append(tokens, string(e.From)+":"+string(e.Type)+":"+string(e.To))
	}
	sort.Strings(tokens)

	// Build the fingerprint input: nodeType, signature, then sorted edge tokens.
	sig := ""
	if n.Metadata != nil {
		sig = n.Metadata["signature"]
	}
	h := sha1.New() //nolint:gosec
	_, _ = fmt.Fprintf(h, "%s:%s", string(n.Type), sig)
	for _, tok := range tokens {
		_, _ = fmt.Fprintf(h, "|%s", tok)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
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

// BulkAddCallSites appends multiple call sites in a single lock acquisition.
// Used by the watcher to re-register stored call sites from other files before
// a resolver pass so that ResolveCallEdges can recreate CALLS edges pointing
// into a file that was just re-parsed (those edges were deleted by RemoveFile).
func (g *Graph) BulkAddCallSites(sites []CallSite) {
	if len(sites) == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.callSites = append(g.callSites, sites...)
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

// AddVarType records that variable varName in file has type typeName.
// Called by language parsers during AST traversal to enable cross-file
// obj.method() resolution in the post-parse resolver pass.
func (g *Graph) AddVarType(file, varName, typeName string) {
	if varName == "" || typeName == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.varTypes == nil {
		g.varTypes = make(map[string]map[string]string)
	}
	if g.varTypes[file] == nil {
		g.varTypes[file] = make(map[string]string)
	}
	g.varTypes[file][varName] = typeName
}

// GetVarTypes returns the variable → type map for the given file.
// Returns nil if no type annotations were recorded for the file.
func (g *Graph) GetVarTypes(file string) map[string]string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.varTypes[file]
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

// FindByType returns all nodes of the given NodeType.
func (g *Graph) FindByType(t NodeType) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []*Node
	for _, n := range g.nodes {
		if n.Type == t {
			out = append(out, n)
		}
	}
	return out
}

// NodesForFile returns all nodes whose source file matches the given path.
// Used by the watcher to migrate stable IDs after a re-parse.
func (g *Graph) NodesForFile(file string) []*Node {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []*Node
	for _, n := range g.nodes {
		if n.File == file {
			out = append(out, n)
		}
	}
	return out
}

// UpdateFileNodeMetadata calls update(n) for every node whose File matches
// absFile, holding the graph write lock for the duration. The callback may
// safely read and write n.Metadata — no structural graph changes (add/remove
// nodes or edges) should be made inside update.
//
// This is the correct way to write node metadata from a background goroutine
// while the MCP server is live: git I/O should happen before calling this
// method; the write lock is held only for the in-memory metadata writes
// (typically microseconds).
func (g *Graph) UpdateFileNodeMetadata(absFile string, update func(n *Node)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.nodes {
		if n.File == absFile {
			update(n)
		}
	}
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

// OutEdgesForFile returns all outgoing edges from nodes whose File matches
// the given path. Complexity is O(total_nodes + file_out_edges), which is
// significantly cheaper than AllEdges() + filter when only one file changed.
func (g *Graph) OutEdgesForFile(file string) []*Edge {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []*Edge
	for id, n := range g.nodes {
		if n.File == file {
			out = append(out, g.outEdges[id]...)
		}
	}
	return out
}

// NodeCount returns the total number of nodes.
func (g *Graph) NodeCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.nodes)
}

// Index returns the current columnar GraphIndex, or nil if it has not been
// built yet. Callers should check Index().Ready() before using it.
func (g *Graph) Index() *GraphIndex {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.index
}

// SetIndex atomically replaces the graph's columnar index with the provided one.
// Used during warm-boot to install a snapshot-loaded index without a full rebuild.
// Also sets g.pool to idx.Pool so subsequent RebuildIndex calls share the same pool.
func (g *Graph) SetIndex(idx *GraphIndex) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.index = idx
	if idx != nil && g.pool == nil {
		g.pool = idx.Pool
	}
}

// RebuildIndex builds a fresh columnar GraphIndex from the current map state
// and atomically replaces g.index. Only one rebuild runs at a time.
// It returns the zstd-compressed snapshot bytes for the caller to persist to the
// store for fast warm-boot loading. Returns nil bytes on serialisation error.
// Typical usage:
//
//	go func() {
//	    blob, err := g.RebuildIndex()
//	    if err == nil { st.SaveIndexSnapshot(blob) }
//	}()
func (g *Graph) RebuildIndex() ([]byte, error) {
	g.indexMu.Lock()
	defer g.indexMu.Unlock()

	// Lazily initialise the shared string pool on first rebuild.
	g.mu.Lock()
	if g.pool == nil {
		g.pool = NewStringPool()
	}
	pool := g.pool
	g.mu.Unlock()

	newIdx := buildIndex(g, pool)

	g.mu.Lock()
	g.index = newIdx
	g.mu.Unlock()

	blob, err := newIdx.SaveSnapshot()
	return blob, err
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

// CrossRepoCalls returns statistics about cross-repository CALLS edges.
// It iterates the internal edge map directly without allocating a snapshot
// slice. The returned linkedRepos slice is sorted and excludes primaryRepoID.
func (g *Graph) CrossRepoCalls(primaryRepoID string) (crossCallCount int, linkedRepos []string) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	linkedSet := make(map[string]bool)
	for from, edges := range g.outEdges {
		fromIdx := strings.Index(string(from), "::")
		if fromIdx < 0 {
			continue
		}
		fromRepo := string(from)[:fromIdx]
		for _, e := range edges {
			if e.Type != EdgeCalls {
				continue
			}
			toIdx := strings.Index(string(e.To), "::")
			if toIdx < 0 {
				continue
			}
			toRepo := string(e.To)[:toIdx]
			if fromRepo != toRepo {
				crossCallCount++
				if fromRepo != primaryRepoID && !linkedSet[fromRepo] {
					linkedSet[fromRepo] = true
				}
				if toRepo != primaryRepoID && !linkedSet[toRepo] {
					linkedSet[toRepo] = true
				}
			}
		}
	}
	for repo := range linkedSet {
		linkedRepos = append(linkedRepos, repo)
	}
	sort.Strings(linkedRepos)
	return crossCallCount, linkedRepos
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

// SnapshotFileStableIDs records the stable UUIDs of all nodes in the given file
// so that MigrateStableID can reuse them after the file is re-parsed.
// Must be called BEFORE RemoveFile for the migration to work correctly.
func (g *Graph) SnapshotFileStableIDs(file string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var records []stableIDRecord
	for _, n := range g.nodes {
		if n.File != file || n.StableID == "" {
			continue
		}
		sig := ""
		if n.Metadata != nil {
			sig = n.Metadata["signature"]
		}
		records = append(records, stableIDRecord{
			name: n.Name,
			pkg:  n.Package,
			sig:  sig,
			id:   n.StableID,
		})
	}
	if g.fileStableIDs == nil {
		g.fileStableIDs = make(map[string][]stableIDRecord)
	}
	g.fileStableIDs[file] = records
}

// MigrateStableID attempts to reuse a stable UUID from a previous snapshot for
// the given node. It checks snapshots for the node's file in two tiers:
//   - Tier 1: exact (name, pkg, signature) match → certain same entity
//   - Tier 2: same (pkg, signature) with different name → likely rename
//
// If no match is found, the node's current StableID is left unchanged.
// Must be called AFTER re-parsing and AddNode, but before SnapshotFileStableIDs
// is called again for the same file.
func (g *Graph) MigrateStableID(n *Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	records, ok := g.fileStableIDs[n.File]
	if !ok || len(records) == 0 {
		return
	}
	sig := ""
	if n.Metadata != nil {
		sig = n.Metadata["signature"]
	}
	// Tier 1: exact match on name + pkg + sig.
	for _, r := range records {
		if r.name == n.Name && r.pkg == n.Package && r.sig == sig {
			n.StableID = r.id
			g.nodes[n.ID] = n
			return
		}
	}
	// Tier 2: same pkg + sig, different name (rename).
	if sig != "" {
		for _, r := range records {
			if r.pkg == n.Package && r.sig == sig {
				n.StableID = r.id
				g.nodes[n.ID] = n
				return
			}
		}
	}
}

// ClearFileSnapshot removes the stable ID snapshot for a file once migration
// is complete. Optional — snapshots are small and automatically replaced on
// the next SnapshotFileStableIDs call for the same file.
func (g *Graph) ClearFileSnapshot(file string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fileStableIDs, file)
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

		// Tombstone removed nodes in the columnar index so BFS skips them
		// immediately without waiting for the next full rebuild.
		if idx := g.index; idx != nil && idx.Ready() {
			for _, id := range toRemove {
				if seq := idx.Seq(id); seq != 0 {
					idx.MarkTombstone(seq)
				}
			}
		}
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

	// Compute scale from semantic node count (functions+methods+structs+interfaces).
	semanticNodes := summary.Functions + summary.Methods + summary.Structs + summary.Interfaces
	var scale Scale
	var toolGuidance string
	switch {
	case semanticNodes < 100:
		scale = ScaleMicro
		toolGuidance = "Micro repo (<100 semantic nodes): Read/Grep is often faster for targeted edits. Use Synapses tools (get_context, find_entity, search) for structural understanding and cross-file analysis. Always use validate_plan before multi-file changes."
	case semanticNodes < 500:
		scale = ScaleSmall
		toolGuidance = "Small repo (100–499 nodes): prefer Synapses for exploration (get_context, search), use Read/Grep for targeted single-file edits. Use validate_plan before multi-file changes."
	case semanticNodes < 2000:
		scale = ScaleMedium
		toolGuidance = "Medium repo (500–1999 nodes): Synapses tools recommended for exploration — they surface callers, callees, and architecture rules that Glob/Grep miss. Use Read/Grep when you know the exact file to edit."
	default:
		scale = ScaleLarge
		toolGuidance = "Large repo (2000+ nodes): Synapses tools recommended — graph queries return richer context (callers, callees, rules, violations) than file scanning at this scale. Use Read/Grep for targeted edits to files you have already identified."
	}

	result := &ProjectIdentity{
		RepoID:         g.repoID,
		Summary:        summary,
		EntryPoints:    entryPoints,
		KeyEntities:    keyEntities,
		SuggestedRules: g.SuggestRules(),
		Scale:          scale,
		ToolGuidance:   toolGuidance,
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
