package store_test

import (
	"testing"
)

// ── SendMessage / GetMessages ─────────────────────────────────────────────────

func TestGetMessages_DirectAndBroadcast(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Direct message to "alice".
	id1, err := st.SendMessage("bob", "alice", "ping", `{"hi":"there"}`, "")
	if err != nil || id1 == "" {
		t.Fatalf("SendMessage direct: %v", err)
	}

	// Broadcast (no to_agent).
	id2, err := st.SendMessage("sys", "", "announce", `{"msg":"broadcast"}`, "")
	if err != nil || id2 == "" {
		t.Fatalf("SendMessage broadcast: %v", err)
	}

	// Message to "carol" — should NOT appear for alice.
	_, _ = st.SendMessage("bob", "carol", "other", `{}`, "")

	msgs, latestSeq, err := st.GetMessages("alice", 0, "", false, 50)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages (1 direct + 1 broadcast), got %d", len(msgs))
	}
	if latestSeq <= 0 {
		t.Error("expected latestSeq > 0")
	}
}

func TestGetMessages_TopicFilter(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, _ = st.SendMessage("src", "dst", "alpha", `{}`, "")
	_, _ = st.SendMessage("src", "dst", "beta", `{}`, "")
	_, _ = st.SendMessage("src", "dst", "alpha", `{}`, "")

	msgs, _, err := st.GetMessages("dst", 0, "alpha", false, 50)
	if err != nil {
		t.Fatalf("GetMessages with topic: %v", err)
	}
	for _, m := range msgs {
		if m.Topic != "alpha" {
			t.Errorf("topic filter leaked message with topic=%q", m.Topic)
		}
	}
	if len(msgs) != 2 {
		t.Errorf("expected 2 alpha messages, got %d", len(msgs))
	}
}

func TestGetMessages_UnreadOnly(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	msgID, _ := st.SendMessage("src", "dst", "work", `{}`, "")

	// Before mark-read: 1 unread.
	msgs, _, err := st.GetMessages("dst", 0, "", true, 50)
	if err != nil {
		t.Fatalf("GetMessages unread: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected 1 unread, got %d", len(msgs))
	}

	// Mark as read.
	if err := st.MarkRead(msgID, "dst"); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	// After mark-read: 0 unread.
	msgs2, _, err := st.GetMessages("dst", 0, "", true, 50)
	if err != nil {
		t.Fatalf("GetMessages after read: %v", err)
	}
	if len(msgs2) != 0 {
		t.Errorf("expected 0 unread after MarkRead, got %d", len(msgs2))
	}
}

func TestGetMessages_SinceCursor(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_, _ = st.SendMessage("s", "r", "t1", `{}`, "")
	_, _ = st.SendMessage("s", "r", "t2", `{}`, "")
	_, _ = st.SendMessage("s", "r", "t3", `{}`, "")

	// Get all to find first seq.
	all, latestSeq, _ := st.GetMessages("r", 0, "", false, 50)
	if len(all) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(all))
	}
	firstSeq := all[0].Seq

	// Get since firstSeq — should return all but the first.
	subset, _, err := st.GetMessages("r", firstSeq, "", false, 50)
	if err != nil {
		t.Fatalf("GetMessages since cursor: %v", err)
	}
	if len(subset) != len(all)-1 {
		t.Errorf("expected %d messages since seq %d, got %d", len(all)-1, firstSeq, len(subset))
	}
	_ = latestSeq
}

// ── CountUnreadMessages ───────────────────────────────────────────────────────

func TestCountUnreadMessages_DirectAndBroadcast(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// 1 direct + 1 broadcast.
	_, _ = st.SendMessage("x", "agent", "work", `{}`, "")
	_, _ = st.SendMessage("x", "", "news", `{}`, "")

	count, err := st.CountUnreadMessages("agent")
	if err != nil {
		t.Fatalf("CountUnreadMessages: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 unread, got %d", count)
	}
}

func TestCountUnreadMessages_ZeroAfterMarkRead(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	msgID, _ := st.SendMessage("src", "reader", "ping", `{}`, "")
	_ = st.MarkRead(msgID, "reader")

	count, err := st.CountUnreadMessages("reader")
	if err != nil {
		t.Fatalf("CountUnreadMessages: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 unread after MarkRead, got %d", count)
	}
}

// ── MarkRead ──────────────────────────────────────────────────────────────────

func TestMarkRead_Idempotent(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	msgID, _ := st.SendMessage("a", "b", "topic", `{}`, "")

	// Mark twice — should not error.
	if err := st.MarkRead(msgID, "b"); err != nil {
		t.Fatalf("first MarkRead: %v", err)
	}
	if err := st.MarkRead(msgID, "b"); err != nil {
		t.Fatalf("second MarkRead (idempotent): %v", err)
	}
}

func TestMarkRead_OnlyVisibleMessages(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Message sent to "carol", not "alice".
	msgID, _ := st.SendMessage("x", "carol", "secret", `{}`, "")

	// Alice trying to mark carol's message — should be a no-op, not an error.
	if err := st.MarkRead(msgID, "alice"); err != nil {
		t.Errorf("MarkRead for non-recipient should not error: %v", err)
	}

	// Message should still be unread by carol.
	count, _ := st.CountUnreadMessages("carol")
	if count != 1 {
		t.Errorf("carol's message should still be unread, count=%d", count)
	}
}
