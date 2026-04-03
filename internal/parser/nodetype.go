package parser

import (
	"sync"
	"unsafe"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
)

// nodeTypeMaps stores per-language type caches. Keyed by the raw C language
// pointer (uintptr) — unique and stable per grammar.
var (
	nodeTypeMu   sync.RWMutex
	nodeTypeMaps = make(map[uintptr]*langCache)
)

// langCache is a fixed-size slice indexed by TSSymbol for O(1) lookup.
type langCache struct {
	mu    sync.RWMutex
	types []string // indexed by Symbol; "" = uncached
}

// langPtr extracts the raw C pointer from a *sitter.Language without
// allocating. The Language struct layout is: { ptr unsafe.Pointer; once sync.Once }.
func langPtr(lang *sitter.Language) uintptr {
	return uintptr(*(*unsafe.Pointer)(unsafe.Pointer(lang)))
}

// ensureLangCache returns (or creates) the per-language cache.
func ensureLangCache(lang *sitter.Language) *langCache {
	key := langPtr(lang)

	nodeTypeMu.RLock()
	lc, ok := nodeTypeMaps[key]
	nodeTypeMu.RUnlock()
	if ok {
		return lc
	}

	nodeTypeMu.Lock()
	defer nodeTypeMu.Unlock()
	if lc, ok = nodeTypeMaps[key]; ok {
		return lc
	}
	lc = &langCache{types: make([]string, lang.SymbolCount()+1)}
	nodeTypeMaps[key] = lc
	return lc
}

// nodeTypeFor returns the type name for a node using the per-language cache.
// This is the fast path — no allocation for cache hits.
func nodeTypeFor(n sitter.Node, lc *langCache) string {
	sym := int(n.Symbol())
	lc.mu.RLock()
	if sym < len(lc.types) {
		if t := lc.types[sym]; t != "" {
			lc.mu.RUnlock()
			return t
		}
	}
	lc.mu.RUnlock()
	t := n.Type()
	lc.mu.Lock()
	if sym < len(lc.types) {
		lc.types[sym] = t
	}
	lc.mu.Unlock()
	return t
}

// nodeType is the package-level convenience. It resolves the per-language
// cache from the node's language pointer. The Language() call allocates a
// *Language wrapper, but the result is only used to extract the raw pointer
// for cache lookup — subsequent calls for the same grammar hit the fast path
// in ensureLangCache.
func nodeType(n sitter.Node) string {
	lang := n.Language()
	lc := ensureLangCache(lang)
	return nodeTypeFor(n, lc)
}
