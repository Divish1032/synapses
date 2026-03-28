package graph

import (
	"sync"
	"sync/atomic"
	"unique"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// StringID is a compact index into the StringPool.
type StringID uint32

// ReservedGhostRange is the number of StringIDs reserved at the beginning of the pool
// for transient or unindexed strings (Ghost Nodes). This prevents out-of-bounds panics
// when an agent requests a file that hasn't been saved to the SQLite BLOB yet.
const ReservedGhostRange = 1000

// MaxPoolSize is the upper bound on the number of interned strings.
// Intern returns a ghost ID once this limit is reached, preventing unbounded
// memory growth in extremely large repositories.
const MaxPoolSize = 5_000_000

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
	// Protected by p.mu (read lock for Value, write lock for internGhost).
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

	// Cap check: if the pool is full, return a ghost ID instead of growing forever.
	if len(p.reverse) >= MaxPoolSize {
		return p.internGhost(s)
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
		p.mu.RLock()
		defer p.mu.RUnlock()
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

// internGhost allocates a transient ghost ID for s. Caller must hold p.mu write lock.
// ghostWarnOnce ensures we log the saturation warning only once.
var ghostWarnOnce atomic.Int32

func (p *StringPool) internGhost(s string) StringID {
	id := p.ghostNext
	if id >= ReservedGhostRange {
		// Wrap around: evict the oldest ghost entry. Ghost strings are transient
		// so losing old mappings is acceptable — callers already treat them as
		// best-effort. Start at 1 (0 is the empty-string sentinel).
		if ghostWarnOnce.CompareAndSwap(0, 1) {
			logutil.Warn("synapses: StringPool ghost range wrapped (%d entries); oldest ghost strings evicted\n", ReservedGhostRange)
		}
		id = 1
	}
	p.ghostCache[id] = s
	// Guard against uint32 overflow: ghostNext is always in [1, ReservedGhostRange)
	// due to the wrap check above, so this can only trigger if ReservedGhostRange
	// is ever set close to math.MaxUint32. Saturate to 1 (wraps to start).
	if id+1 < id { // overflow
		p.ghostNext = 1
	} else {
		p.ghostNext = id + 1
	}
	return StringID(id)
}
