package webcache

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// IndexProjectImports walks the project's IMPORTS edges to find external Go
// packages, then fetches and caches their pkg.go.dev documentation in order
// of usage (most-called packages first).
//
// It must be called as a goroutine — it blocks until all packages are indexed
// or ctx is cancelled:
//
//	go webcache.IndexProjectImports(ctx, projectPath, g, cache, 20)
//
// It skips packages already in the cache (TTL=0 entries are never stale).
// Fetches are paced at 1 per 2 seconds to be polite to pkg.go.dev.
func IndexProjectImports(ctx context.Context, projectPath string, g *graph.Graph, cache *Cache, maxPackages int) {
	// Parse go.mod for the version map. If go.mod doesn't exist (non-Go project)
	// versions will be nil and we'll cache without version pinning.
	versions, err := ParseGoMod(projectPath)
	if err != nil {
		log.Printf("webcache: parse go.mod at %s: %v", projectPath, err)
	}

	// Collect external package import paths and count CALLS fanout as a
	// proxy for how heavily each library is used.
	type pkgEntry struct {
		importPath string
		callCount  int
	}

	// Map from import path → call count.
	seen := make(map[string]int)

	for _, node := range g.AllNodes() {
		if node.Type != graph.NodePackage {
			continue
		}
		if IsStdlib(node.Name) {
			continue
		}
		// Count outgoing CALLS edges from this package node as a usage signal.
		calls := 0
		for _, e := range g.OutEdges(node.ID) {
			if e.Type == graph.EdgeCalls {
				calls++
			}
		}
		seen[node.Name] += calls
	}

	if len(seen) == 0 {
		return
	}

	// Sort by call count descending (most-used first).
	entries := make([]pkgEntry, 0, len(seen))
	for path, count := range seen {
		entries = append(entries, pkgEntry{importPath: path, callCount: count})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].callCount > entries[j].callCount
	})

	if maxPackages > 0 && len(entries) > maxPackages {
		entries = entries[:maxPackages]
	}

	fetched := 0
	for _, e := range entries {
		select {
		case <-ctx.Done():
			return
		default:
		}

		version := ""
		if versions != nil {
			version = versions[e.importPath]
		}

		// Skip if already cached at this version.
		key := PackageCacheKey(e.importPath, version)
		if _, ok := cache.store.GetWebCache(key); ok {
			continue
		}

		log.Printf("webcache: indexing docs for %s@%s", e.importPath, version)
		_, _, fetchErr := cache.FetchPackageDocs(ctx, e.importPath, version)
		if fetchErr != nil {
			log.Printf("webcache: fetch %s: %v", e.importPath, fetchErr)
		} else {
			fetched++
		}

		// Polite crawling — 2s between requests.
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	if fetched > 0 {
		log.Printf("webcache: indexed %d/%d external packages for %s", fetched, len(entries), projectPath)
	}
}

