package store

import (
	"os"
	"testing"
	"time"
)

// openTestStore is a helper that creates a temp SQLite store and registers cleanup.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	f, err := os.CreateTemp("", "test-agents-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	st, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestClassifyPresence checks the three presence tiers.
func TestClassifyPresence(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		lastSeen time.Time
		want     string
	}{
		{now.Add(-1 * time.Minute), "active"},
		{now.Add(-9 * time.Minute), "idle"},
		{now.Add(-20 * time.Minute), "inactive"},
	}
	for _, tc := range cases {
		got := classifyPresence(tc.lastSeen.Format(time.RFC3339), now)
		if got != tc.want {
			t.Errorf("classifyPresence(%v) = %q, want %q", tc.lastSeen, got, tc.want)
		}
	}
}

// ── Item 3: message TTL pruning ───────────────────────────────────────────────

// TestSendMessage_PrunesReadMessages verifies that read messages older than
// 24 h are removed on the next SendMessage call.
func TestSendMessage_PrunesReadMessages(t *testing.T) {
	st := openTestStore(t)

	// Insert a message manually with a timestamp in the past (> 24 h ago)
	// and mark it as read, simulating an aged-out message.
	oldTime := time.Now().Add(-25 * time.Hour).Unix()
	readTime := time.Now().Add(-24 * time.Hour).Unix()
	_, err := st.db.Exec(
		`INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at, read_at)
		 VALUES ('old-msg-1', 'agent-a', 'agent-b', 'test', '{}', '', ?, ?)`,
		oldTime, readTime,
	)
	if err != nil {
		t.Fatalf("insert old message: %v", err)
	}

	// Verify it exists before pruning.
	var count int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE id='old-msg-1'`).Scan(&count)
	if count != 1 {
		t.Fatalf("setup: expected old message to exist")
	}

	// Sending a new message triggers pruning.
	if _, err := st.SendMessage("agent-a", "agent-b", "ping", "{}", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// Old read message must be gone.
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE id='old-msg-1'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected old read message to be pruned, but it still exists")
	}
}

// TestSendMessage_KeepsUnreadMessages verifies that unread messages within the
// 7-day window are NOT pruned.
func TestSendMessage_KeepsUnreadMessages(t *testing.T) {
	st := openTestStore(t)

	// Insert an unread message from 2 days ago (well within 7-day window).
	recentTime := time.Now().Add(-48 * time.Hour).Unix()
	_, err := st.db.Exec(
		`INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at)
		 VALUES ('recent-unread', 'agent-a', 'agent-b', 'test', '{}', '', ?)`,
		recentTime,
	)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	// Trigger pruning via SendMessage.
	if _, err := st.SendMessage("agent-a", "agent-b", "ping", "{}", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var count int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE id='recent-unread'`).Scan(&count)
	if count != 1 {
		t.Errorf("expected unread message within 7-day window to survive pruning")
	}
}

// TestSendMessage_PrunesVeryOldUnread verifies that unread messages older than
// 7 days are also pruned.
func TestSendMessage_PrunesVeryOldUnread(t *testing.T) {
	st := openTestStore(t)

	veryOld := time.Now().Add(-8 * 24 * time.Hour).Unix()
	_, err := st.db.Exec(
		`INSERT INTO agent_messages (id, from_agent, to_agent, topic, payload, project_id, created_at)
		 VALUES ('stale-unread', 'agent-a', 'agent-b', 'test', '{}', '', ?)`,
		veryOld,
	)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}

	if _, err := st.SendMessage("agent-a", "agent-b", "ping", "{}", ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var count int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM agent_messages WHERE id='stale-unread'`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 8-day-old unread message to be pruned")
	}
}

