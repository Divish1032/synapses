package graph

import (
	"fmt"
	"sync"
	"time"
)

const (
	cacheMaxSize = 20
	cacheTTL     = 30 * time.Second
)

type cacheEntry struct {
	sub       *SubGraph
	expiresAt time.Time
}

// subgraphCache is a bounded, TTL-based in-memory cache for carved subgraphs.
// It is safe for concurrent use. Invalidate must be called whenever the graph
// is mutated so stale results are not served.
type subgraphCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	order   []string // insertion-order keys for FIFO eviction
}

func newSubgraphCache() *subgraphCache {
	return &subgraphCache{
		entries: make(map[string]*cacheEntry, cacheMaxSize),
	}
}

// cacheKeyFor produces a compact string key from a root node ID and the
// CarveConfig fields that agents typically vary (depth, budget, minRel, decay).
// EdgeWeights and ExcludeTypes are assumed to be the project defaults.
func cacheKeyFor(rootID NodeID, cfg CarveConfig) string {
	return fmt.Sprintf("%s|%d|%d|%.6f|%.6f",
		rootID, cfg.MaxDepth, cfg.TokenBudget, cfg.MinRelevance, cfg.DecayFactor)
}

// get returns a cached SubGraph if one exists and has not expired.
func (c *subgraphCache) get(rootID NodeID, cfg CarveConfig) (*SubGraph, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKeyFor(rootID, cfg)
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, key)
		return nil, false
	}
	return e.sub, true
}

// put stores a SubGraph in the cache. If the cache is at capacity the oldest
// entry (by insertion order) is evicted first.
func (c *subgraphCache) put(rootID NodeID, cfg CarveConfig, sub *SubGraph) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKeyFor(rootID, cfg)
	if _, exists := c.entries[key]; !exists {
		// New entry: evict the oldest if we are at capacity.
		for len(c.entries) >= cacheMaxSize && len(c.order) > 0 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = &cacheEntry{
		sub:       sub,
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
