package store

import (
	"os"
	"testing"
)

func TestAgentContext_CRUD(t *testing.T) {
	f, err := os.CreateTemp("", "test-agentctx-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	st, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Unknown agent returns nil.
	ac, err := st.GetAgentContext("unknown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ac != nil {
		t.Fatalf("expected nil, got %+v", ac)
	}

	// Create.
	err = st.UpsertAgentContext(&AgentContext{
		AgentID:      "agent-1",
		LastEventSeq: 42,
		IdentityHash: "abc123",
		LastSession:  "2026-03-11T10:00:00Z",
	})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}

	// Retrieve.
	ac, err = st.GetAgentContext("agent-1")
	if err != nil || ac == nil {
		t.Fatalf("get after create: ac=%v, err=%v", ac, err)
	}
	if ac.LastEventSeq != 42 || ac.IdentityHash != "abc123" {
		t.Errorf("data mismatch: got seq=%d hash=%s", ac.LastEventSeq, ac.IdentityHash)
	}

	// Update.
	err = st.UpsertAgentContext(&AgentContext{
		AgentID:      "agent-1",
		LastEventSeq: 100,
		IdentityHash: "newHash",
		LastSession:  "2026-03-11T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	ac, _ = st.GetAgentContext("agent-1")
	if ac.LastEventSeq != 100 || ac.IdentityHash != "newHash" {
		t.Errorf("update mismatch: got seq=%d hash=%s", ac.LastEventSeq, ac.IdentityHash)
	}
}
