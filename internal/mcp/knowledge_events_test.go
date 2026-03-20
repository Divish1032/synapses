package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

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

	// knowledge_accessed is emitted in a background goroutine — wait briefly.
	time.Sleep(100 * time.Millisecond)

	events, _, err := srv.store.GetEvents(0, []string{"knowledge_accessed"}, "", 20)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
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
	res2, callErr2 := srv.handleRecall(ctx, callTool(map[string]any{
		"query":    "xyzzy-no-match-sentinel-12345",
		"agent_id": "agent-empty",
	}))
	mustResult(t, res2, callErr2)

	time.Sleep(100 * time.Millisecond)

	events, _, err := srv.store.GetEvents(0, []string{"knowledge_accessed"}, "", 20)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected no knowledge_accessed events on empty recall, got %d", len(events))
	}
}
