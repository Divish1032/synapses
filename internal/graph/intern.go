package graph

import (
	"sync"
	"unique"
)

// StringID is a compact index into the StringPool.
type StringID uint32

// ReservedGhostRange is the number of StringIDs reserved at the beginning of the pool
// for transient or unindexed strings (Ghost Nodes). This prevents out-of-bounds panics
// when an agent requests a file that hasn't been saved to the SQLite BLOB yet.
const ReservedGhostRange = 1000

// StringPool implements a bi-directional mapping between strings and uint32 IDs.
// It leverages the Go 1.23 `unique` package to ensure that identical strings
// share the same underlying memory allocation across the entire application,
// massively reducing heap usage in large repositories.
//
// StringPool is safe for concurrent use.
type StringPool struct {
	mu sync.RWMutex

	// forward maps memory-deduplicated string handles to their ID.
	forward map[unique.Handle[string]]StringID

	// reverse stores the handles sequentially, where the slice index
	// (plus ReservedGhostRange) corresponds to the StringID.
	reverse []unique.Handle[string]

	// ghostPool handles transient strings within the reserved range.
	// We use a simple slice that wraps around to prevent infinite growth.
	ghostMu    sync.Mutex
	ghostCache []string
	ghostNext  uint32
}

// NewStringPool creates a new, empty string interning pool.
func NewStringPool() *StringPool {
	return &StringPool{
		forward:    make(map[unique.Handle[string]]StringID),
		reverse:    make([]unique.Handle[string], 0),
		ghostCache: make([]string, ReservedGhostRange),
		ghostNext:  1, // 0 is essentially the "empty string" ID.
	}
}

// Intern takes a raw string, deduplicates its memory using the `unique` package,
// and returns a compact StringID. If the string has already been interned,
// it returns the existing ID.
func (p *StringPool) Intern(s string) StringID {
	if s == "" {
		return 0
	}

	h := unique.Make(s)

	p.mu.RLock()
	id, ok := p.forward[h]
	p.mu.RUnlock()

	if ok {
		return id
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check under write lock
	if id, ok := p.forward[h]; ok {
		return id
	}

	// Assign the next available ID sequence (offset by the ghost range)
	id = StringID(len(p.reverse) + ReservedGhostRange)
	p.forward[h] = id
	p.reverse = append(p.reverse, h)

	return id
}

// Value looks up the string associated with the given StringID.
// It handles both properly interned strings and transient "Ghost" strings.
func (p *StringPool) Value(id StringID) string {
	if id == 0 {
		return ""
	}

	// Check if this ID falls into the Ghost range
	if id < ReservedGhostRange {
		p.ghostMu.Lock()
		defer p.ghostMu.Unlock()
		return p.ghostCache[id]
	}

	// Real interned string
	idx := int(id) - ReservedGhostRange

	p.mu.RLock()
	defer p.mu.RUnlock()

	if idx >= 0 && idx < len(p.reverse) {
		return p.reverse[idx].Value() // Extract the deduplicated string
	}

	return "" // Out of bounds safety fallback
}

