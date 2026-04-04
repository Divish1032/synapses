package mcp

import (
	"testing"
)

func TestSessionDelivered_BasicMarkAndCheck(t *testing.T) {
	tr := &sessionDeliveredTracker{}

	// Not yet delivered
	if tr.wasDelivered("sess1", "abc123") {
		t.Error("should not be delivered before marking")
	}

	// Mark delivered — first time returns true (new entry)
	if !tr.markDelivered("sess1", "abc123") {
		t.Error("first mark should return true")
	}

	// Now it's delivered
	if !tr.wasDelivered("sess1", "abc123") {
		t.Error("should be delivered after marking")
	}

	// Second mark returns false (already exists)
	if tr.markDelivered("sess1", "abc123") {
		t.Error("second mark should return false")
	}
}

func TestSessionDelivered_EmptySessionIDSkipped(t *testing.T) {
	tr := &sessionDeliveredTracker{}

	// Empty sessionID → always returns false (anonymous agents)
	if tr.markDelivered("", "abc123") {
		t.Error("empty sessionID should return false")
	}
	if tr.wasDelivered("", "abc123") {
		t.Error("empty sessionID wasDelivered should return false")
	}
}

func TestSessionDelivered_EmptyHashSkipped(t *testing.T) {
	tr := &sessionDeliveredTracker{}
	if tr.markDelivered("sess1", "") {
		t.Error("empty hash should return false")
	}
}

func TestSessionDelivered_IsolatedBySessions(t *testing.T) {
	tr := &sessionDeliveredTracker{}

	tr.markDelivered("sessA", "hash1")

	// sessB should not see sessA's delivery
	if tr.wasDelivered("sessB", "hash1") {
		t.Error("different sessions should be isolated")
	}
}

func TestSessionDelivered_ClearSession(t *testing.T) {
	tr := &sessionDeliveredTracker{}
	tr.markDelivered("sess1", "hash1")
	tr.markDelivered("sess1", "hash2")
	tr.markDelivered("sess2", "hash3")

	tr.clearSession("sess1")

	if tr.wasDelivered("sess1", "hash1") {
		t.Error("hash1 should be cleared for sess1")
	}
	if tr.wasDelivered("sess1", "hash2") {
		t.Error("hash2 should be cleared for sess1")
	}
	// sess2 unaffected
	if !tr.wasDelivered("sess2", "hash3") {
		t.Error("sess2 should be unaffected by sess1 clear")
	}
}

func TestSessionDelivered_ClearEmptySession(t *testing.T) {
	tr := &sessionDeliveredTracker{}
	// Should not panic on clearing unknown or empty session
	tr.clearSession("")
	tr.clearSession("nonexistent")
}

func TestSessionDelivered_CapEnforced(t *testing.T) {
	tr := &sessionDeliveredTracker{}
	sessID := "bigSession"

	// Fill to cap
	for i := 0; i < maxDeliveredPerSession; i++ {
		hash := kvContentHash(string(rune(i + 1)))
		tr.markDelivered(sessID, hash)
	}

	// One more should fail (cap exceeded)
	newHash := kvContentHash("overflow-entry")
	if tr.markDelivered(sessID, newHash) {
		t.Error("should return false when cap exceeded")
	}
	// And wasDelivered for the overflow should be false
	if tr.wasDelivered(sessID, newHash) {
		t.Error("overflow entry should not be tracked")
	}
}

func TestSessionDelivered_Concurrent(t *testing.T) {
	tr := &sessionDeliveredTracker{}
	done := make(chan struct{})

	// Concurrent marks from multiple goroutines — should not race
	for i := 0; i < 10; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			hash := kvContentHash(string(rune(n + 65))) // A, B, C...
			tr.markDelivered("shared", hash)
			tr.wasDelivered("shared", hash)
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}
