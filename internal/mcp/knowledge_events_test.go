package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// awaitEvent polls GetEvents until an event of the given type appears or the
// deadline passes. Returns the events found (may be empty on timeout).
// This replaces time.Sleep which is flaky under load in slow CI environments.
func awaitEvent(t *testing.T, st *store.Store, typ string, deadline time.Duration) []store.Event {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		events, _, err := st.GetEvents(0, []string{typ}, "", 20)
		if err != nil {
			t.Fatalf("GetEvents(%q): %v", typ, err)
		}
		if len(events) > 0 {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	events, _, _ := st.GetEvents(0, []string{typ}, "", 20)
	return events
}

// TestKnowledgeEvent_Accessed verifies that a successful recall() call with
// results emits a knowledge_accessed event.
func TestKnowledgeEvent_Accessed(t *testing.T) {
	srv := newTestServer(t)

	// Seed a memory so recall() can return a result.
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "auth token validation switched to RS256 — knowledge_accessed test sentinel",
		AgentID: "agent-access",
		Source:  store.SourceManual,
	})
	if err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	// Call recall() with a query that matches the seeded memory.
	res, callErr := srv.handleRecall(ctx, callTool(map[string]any{
		"query":    "auth token RS256",
		"agent_id": "agent-access",
	}))
	mustResult(t, res, callErr)

	// knowledge_accessed is emitted in a background goroutine — poll with deadline.
	events := awaitEvent(t, srv.store, "knowledge_accessed", 2*time.Second)
	if len(events) == 0 {
		t.Fatal("expected knowledge_accessed event after recall with results, got none")
	}

	// Verify payload structure.
	latest := events[len(events)-1]
	if !strings.Contains(latest.Payload, `"query"`) {
		t.Errorf("knowledge_accessed payload missing query field; got: %s", latest.Payload)
	}
}

// TestKnowledgeEvent_NotAccessedOnEmptyRecall verifies that recall() with no
// matching results does NOT emit a knowledge_accessed event.
func TestKnowledgeEvent_NotAccessedOnEmptyRecall(t *testing.T) {
	srv := newTestServer(t)

	// No memories seeded — recall returns empty.
	res, callErr := srv.handleRecall(ctx, callTool(map[string]any{
		"query":    "xyzzy-no-match-sentinel-12345",
		"agent_id": "agent-empty",
	}))
	mustResult(t, res, callErr)

	// Give any goroutine a chance to run (even though none should fire).
	time.Sleep(50 * time.Millisecond)

	events, _, err := srv.store.GetEvents(0, []string{"knowledge_accessed"}, "", 20)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected no knowledge_accessed events on empty recall, got %d", len(events))
	}
}
