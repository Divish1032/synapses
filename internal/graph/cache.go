package graph

import (
	"fmt"
	"strings"
	"sync"
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
type subgraphCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	order   []string // access-order keys for LRU eviction (most recent at tail)
}

func newSubgraphCache() *subgraphCache {
	return &subgraphCache{
		entries: make(map[string]*cacheEntry, cacheMaxSize),
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
func cacheKeyFor(rootID NodeID, cfg CarveConfig, fingerprint string) string {
	return fmt.Sprintf("%s|%d|%d|%.6f|%.6f|%.4f|%s|%s",
		rootID, cfg.MaxDepth, cfg.TokenBudget, cfg.MinRelevance, cfg.DecayFactor,
		cfg.DirectionBoost, cfg.IntentID, fingerprint)
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
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKeyFor(rootID, cfg, fingerprint)
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	c.promoteKey(key)
	return e.sub, true
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
		for len(c.entries) >= cacheMaxSize && len(c.order) > 0 {
			lru := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, lru)
		}
		c.order = append(c.order, key)
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
	c.order = c.order[:0]
}

// invalidateForFile evicts only cached entries whose subgraph references the
// given file path. Entries for unrelated entities survive, dramatically
// improving cache hit rates when a single file changes. The match is
// suffix-based so both absolute and relative paths work.
func (c *subgraphCache) invalidateForFile(file string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var surviving []string
	for _, key := range c.order {
		e, ok := c.entries[key]
		if !ok {
			continue
		}
		if entryReferencesFile(e, file) {
			delete(c.entries, key)
		} else {
			surviving = append(surviving, key)
		}
	}
	c.order = surviving
}

// entryReferencesFile checks if any node file in the cache entry matches the
// given path (suffix-based match to handle absolute vs relative).
func entryReferencesFile(e *cacheEntry, file string) bool {
	for f := range e.files {
		if f == file || strings.HasSuffix(f, "/"+file) || strings.HasSuffix(file, "/"+f) {
			return true
		}
	}
	return false
}

// promoteKey moves key to the tail of c.order (most-recently-used position).
// Caller must hold c.mu.
func (c *subgraphCache) promoteKey(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, key)
			return
		}
	}
}
