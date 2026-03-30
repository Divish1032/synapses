package graph

import (
	"container/list"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	cacheMaxSize = 512
	cacheTTL     = 90 * time.Second
)

type cacheEntry struct {
	sub       *SubGraph
	files     map[string]struct{} // set of source files referenced by nodes in this subgraph
	expiresAt time.Time
}

// subgraphCache is a bounded, TTL-based in-memory cache for carved subgraphs.
// It is safe for concurrent use. Supports both full invalidation and
// file-scoped invalidation so that a single file change doesn't flush the
// entire cache.
//
// Uses container/list for O(1) LRU promote/remove instead of O(N) slice scan.
type subgraphCache struct {
	mu       sync.RWMutex
	entries  map[string]*cacheEntry
	order    *list.List               // doubly-linked list for LRU; Back = most-recently-used
	elements map[string]*list.Element // key → list element for O(1) lookup

	// Observability counters (atomic, lock-free reads).
	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64
}

func newSubgraphCache() *subgraphCache {
	return &subgraphCache{
		entries:  make(map[string]*cacheEntry, cacheMaxSize),
		order:    list.New(),
		elements: make(map[string]*list.Element, cacheMaxSize),
	}
}

// cacheKeyFor produces a compact string key from a root node ID, the
// CarveConfig fields that affect BFS output, and the structural fingerprint
// of the root node.
//
// The fingerprint encodes the root node's signature and direct neighbourhood
// (see Graph.nodeFingerprintLocked).  Including it in the key means that a
// cache entry is only reused when the root node's observable structure is
// unchanged — comment-only edits do not change the fingerprint, so the
// cached subgraph remains valid across such edits without any explicit
// invalidation.  Structural changes (signature update, edge added/removed)
// produce a different fingerprint → new key → automatic cache miss.
//
// DirectionBoost is included because intent-specific configs vary it.
// IntentID is included to prevent intent-specific weight overrides from
// colliding with each other or with the default (non-intent) subgraph.
// UsePPR and Alpha are included because PPR produces different score distributions.
// HybridLambda is included because semantic blending changes node ranking.
// EmbeddingLookup/QualityScoreLookup presence booleans are included so that
// a call without enrichment does not collide with an enriched call for the
// same entity (they produce different Relevance scores in CarvedNode).
// LearnedEdgeWeights uses a two-field discriminator in the cache key:
//   - lew:%d  — len(LearnedEdgeWeights): fast first-pass check that covers
//     callers who set the map directly without going through the store (e.g.
//     unit tests). Baseline (nil) has len=0; enriched callers have len=N.
//   - lewv:%d — LearnedEdgeWeightsVersion: the store's monotonic write counter.
//     In production, two weight maps with the same len but different edge entries
//     (e.g. learned over different sessions) would collide on len alone. The
//     version increments on every write, so any change to the weight table
//     automatically produces a cache miss without explicit subgraph cache
//     invalidation. Zero when the caller bypasses the store (test path); the
//     len discriminator then prevents any collision.
func cacheKeyFor(rootID NodeID, cfg CarveConfig, fingerprint string) string {
	// Build a deterministic string for ExcludeTypes (sorted).
	excludeTypes := ""
	if len(cfg.ExcludeTypes) > 0 {
		types := make([]string, 0, len(cfg.ExcludeTypes))
		for t, v := range cfg.ExcludeTypes {
			if v {
				types = append(types, string(t))
			}
		}
		sort.Strings(types)
		excludeTypes = strings.Join(types, ",")
	}
	// Build a deterministic fingerprint for EdgeWeights (sorted by key) so
	// that two configs differing only in their custom weight map don't collide.
	ewFP := "ew:default"
	if len(cfg.EdgeWeights) > 0 {
		keys := make([]string, 0, len(cfg.EdgeWeights))
		for k := range cfg.EdgeWeights {
			keys = append(keys, string(k))
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s=%.4f", k, cfg.EdgeWeights[EdgeType(k)])
		}
		ewFP = "ew:" + strings.Join(parts, ",")
	}
	return fmt.Sprintf("%s|%d|%d|%.6f|%.6f|%.4f|%s|%s|%v|%.4f|%.4f|%.4f|emb:%v|qs:%v|lew:%d|lewv:%d|excl:%s|extest:%v|%s",
		rootID, cfg.MaxDepth, cfg.TokenBudget, cfg.MinRelevance, cfg.DecayFactor,
		cfg.DirectionBoost, cfg.IntentID, fingerprint, cfg.UsePPR, cfg.Alpha,
		cfg.HybridLambda, cfg.CrossDomainDecay,
		cfg.EmbeddingLookup != nil,
		cfg.QualityScoreLookup != nil,
		len(cfg.LearnedEdgeWeights),
		cfg.LearnedEdgeWeightsVersion,
		excludeTypes,
		cfg.ExcludeTestFiles,
		ewFP)
}

// extractFiles collects the set of source files referenced by nodes in the subgraph.
func extractFiles(sub *SubGraph) map[string]struct{} {
	files := make(map[string]struct{}, len(sub.Nodes))
	for _, cn := range sub.Nodes {
		if cn.Node != nil && cn.Node.File != "" {
			files[cn.Node.File] = struct{}{}
		}
	}
	return files
}

// get returns a cached SubGraph if one exists and has not expired.
// fingerprint is the structural fingerprint of the root node computed by
// Graph.nodeFingerprintLocked; it is included in the cache key so that a
// structural change to the root node automatically produces a cache miss
// without requiring explicit invalidation.
func (c *subgraphCache) get(rootID NodeID, cfg CarveConfig, fingerprint string) (*SubGraph, bool) {
	c.mu.RLock()
	key := cacheKeyFor(rootID, cfg, fingerprint)
	e, ok := c.entries[key]
	expired := ok && time.Now().After(e.expiresAt)
	c.mu.RUnlock()

	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	if expired {
		c.mu.Lock()
		// Re-check under write lock: another goroutine may have replaced the
		// entry between our RLock check and this Lock acquisition.
		if e2, still := c.entries[key]; still && time.Now().After(e2.expiresAt) {
			delete(c.entries, key)
			c.removeFromOrder(key)
			c.evictions.Add(1)
		}
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, false
	}
	c.mu.Lock()
	// Re-check entry existence after re-acquiring write lock: the entry may
	// have been evicted between the RLock release and this Lock acquisition.
	e2, still := c.entries[key]
	if still {
		c.promoteKey(key)
	}
	c.mu.Unlock()
	if !still {
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return e2.sub, true
}

// put stores a SubGraph in the cache. If the cache is at capacity the least
// recently used entry is evicted first.
// fingerprint is the structural fingerprint of the root node (see get).
func (c *subgraphCache) put(rootID NodeID, cfg CarveConfig, fingerprint string, sub *SubGraph) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKeyFor(rootID, cfg, fingerprint)
	if _, exists := c.entries[key]; exists {
		// Existing entry: promote to most-recently-used.
		c.promoteKey(key)
	} else {
		// New entry: evict the LRU if we are at capacity.
		for len(c.entries) >= cacheMaxSize && c.order.Len() > 0 {
			front := c.order.Front()
			lru := front.Value.(string)
			c.order.Remove(front)
			delete(c.elements, lru)
			delete(c.entries, lru)
			c.evictions.Add(1)
		}
		elem := c.order.PushBack(key)
		c.elements[key] = elem
	}
	c.entries[key] = &cacheEntry{
		sub:       sub,
		files:     extractFiles(sub),
		expiresAt: time.Now().Add(cacheTTL),
	}
}

// invalidate clears all cached entries. Must be called after any graph mutation
// so that stale subgraphs are not served to clients.
func (c *subgraphCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*cacheEntry, cacheMaxSize)
	c.order.Init()
	c.elements = make(map[string]*list.Element, cacheMaxSize)
}

// invalidateForFile evicts only cached entries whose subgraph references the
// given file path. Entries for unrelated entities survive, dramatically
// improving cache hit rates when a single file changes. The match is
// suffix-based so both absolute and relative paths work.
func (c *subgraphCache) invalidateForFile(file string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var toRemove []string
	for key, e := range c.entries {
		if entryReferencesFile(e, file) {
			toRemove = append(toRemove, key)
		}
	}
	for _, key := range toRemove {
		delete(c.entries, key)
		if elem, ok := c.elements[key]; ok {
			c.order.Remove(elem)
			delete(c.elements, key)
		}
	}
}

// entryReferencesFile checks if any node file in the cache entry matches the
// given path (suffix-based match to handle absolute vs relative).
func entryReferencesFile(e *cacheEntry, file string) bool {
	for f := range e.files {
		if f == file || strings.HasSuffix(f, "/"+file) {
			return true
		}
	}
	return false
}

// Len returns the number of entries currently in the cache (P9-8).
func (c *subgraphCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// CacheStats holds subgraph cache observability counters.
type CacheStats struct {
	Hits      int64
	Misses    int64
	Evictions int64
	Size      int
}

// Stats returns a snapshot of cache hit/miss/eviction counters and current size.
func (c *subgraphCache) Stats() CacheStats {
	c.mu.RLock()
	size := len(c.entries)
	c.mu.RUnlock()
	return CacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		Size:      size,
	}
}

// removeFromOrder removes key from the LRU list. O(1).
// Caller must hold c.mu.
func (c *subgraphCache) removeFromOrder(key string) {
	if elem, ok := c.elements[key]; ok {
		c.order.Remove(elem)
		delete(c.elements, key)
	}
}

// promoteKey moves key to the back of the list (most-recently-used position). O(1).
// Caller must hold c.mu.
func (c *subgraphCache) promoteKey(key string) {
	if elem, ok := c.elements[key]; ok {
		c.order.MoveToBack(elem)
	}
}
