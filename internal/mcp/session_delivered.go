package mcp

import (
	"sync"
	"sync/atomic"
	"time"
)

// sessionDeliveredTracker tracks intelligence already delivered to an agent
// in the current session, preventing re-delivery of identical content.
//
// Keyed by sessionID + ":" + contentHash (from kvContentHash). The value is
// a Unix-second timestamp (for future eviction or debugging).
//
// Design principles:
//   - Empty sessionID → no tracking (anonymous agents are excluded)
//   - Per-session cap: 2000 entries. When full, new entries are dropped without
//     error (fail-open: dedup becomes best-effort, no correctness violation).
//   - clearSession removes all entries for a session (called at end_session).
//   - Zero external dependencies — only sync primitives.
type sessionDeliveredTracker struct {
	mu      sync.Mutex
	entries map[string]int64 // key → unix-second timestamp
	counts  sync.Map         // sessionID → *int64 (atomic counter)
}

const maxDeliveredPerSession = 2000

// markDelivered records that content (identified by hash) was delivered in session.
// Returns true if this is a new entry (not previously delivered).
// Returns false if already delivered or sessionID is empty.
func (t *sessionDeliveredTracker) markDelivered(sessionID, contentHash string) bool {
	if sessionID == "" || contentHash == "" {
		return false
	}
	key := sessionID + ":" + contentHash

	// Check and enforce per-session cap
	cnt := t.sessionCount(sessionID)
	if atomic.LoadInt64(cnt) >= maxDeliveredPerSession {
		return false // cap exceeded — best-effort dedup, do not track
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.entries == nil {
		t.entries = make(map[string]int64)
	}

	if _, exists := t.entries[key]; exists {
		return false // already delivered
	}

	t.entries[key] = nowUnixFn()
	atomic.AddInt64(cnt, 1)
	return true
}

// wasDelivered returns true if this content was previously delivered in the session.
// Returns false for empty sessionID (always deliver to anonymous agents).
func (t *sessionDeliveredTracker) wasDelivered(sessionID, contentHash string) bool {
	if sessionID == "" || contentHash == "" {
		return false
	}
	key := sessionID + ":" + contentHash

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.entries == nil {
		return false
	}
	_, exists := t.entries[key]
	return exists
}

// clearSession removes all delivered-set entries for the given session.
// Should be called from end_session handling to free memory.
func (t *sessionDeliveredTracker) clearSession(sessionID string) {
	if sessionID == "" {
		return
	}
	prefix := sessionID + ":"

	t.mu.Lock()
	defer t.mu.Unlock()

	for k := range t.entries {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(t.entries, k)
		}
	}

	// Reset counter
	t.counts.Delete(sessionID)
}

// sessionCount returns (or creates) the atomic counter for a session's entry count.
func (t *sessionDeliveredTracker) sessionCount(sessionID string) *int64 {
	v, _ := t.counts.LoadOrStore(sessionID, new(int64))
	return v.(*int64)
}

// nowUnixFn is a variable so tests can stub it.
var nowUnixFn = func() int64 {
	return time.Now().Unix()
}
