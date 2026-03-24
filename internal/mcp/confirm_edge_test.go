package mcp

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestHandleConfirmEdge_Approve verifies that confirmed=true sets confidence=1.0
// and marks the edge as confirmed in the store. The edge must remain in the live graph.
func TestHandleConfirmEdge_Approve(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	// First create a synthetic edge via link_entities to have something to confirm.
	_, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a":        "AuthLogin",
		"b":        "AuthLogout",
		"relation": "MENTIONS",
	}))
	if err != nil {
		t.Fatalf("link_entities setup: %v", err)
	}

	res, err := srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a":         "AuthLogin",
		"b":         "AuthLogout",
		"relation":  "MENTIONS",
		"confirmed": true,
	}))
	out := mustResult(t, res, err)

	if status, _ := out["status"].(string); status != "confirmed" {
		t.Errorf("expected status=confirmed, got %q", status)
	}

	// Edge must still be live in the graph.
	if !srv.graph.HasEdge(loginID, logoutID, graph.EdgeMentions) {
		t.Error("approved edge should remain in live graph")
	}

	// Verify confidence=1.0 and confirmed=true in store.
	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "MENTIONS" {
			if !e.Confirmed {
				t.Error("expected confirmed=true in store")
			}
			if e.Confidence != 1.0 {
				t.Errorf("expected confidence=1.0, got %v", e.Confidence)
			}
			if e.Suppressed {
				t.Error("approved edge must not be suppressed")
			}
			return
		}
	}
	t.Error("MENTIONS edge not found in store after confirm")
}

// TestHandleConfirmEdge_Reject verifies that confirmed=false suppresses the edge
// permanently and removes it from the live graph immediately.
func TestHandleConfirmEdge_Reject(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	_, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a":        "AuthLogin",
		"b":        "AuthLogout",
		"relation": "MENTIONS",
	}))
	if err != nil {
		t.Fatalf("link_entities setup: %v", err)
	}

	res, err := srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a":         "AuthLogin",
		"b":         "AuthLogout",
		"relation":  "MENTIONS",
		"confirmed": false,
	}))
	out := mustResult(t, res, err)

	if status, _ := out["status"].(string); status != "suppressed" {
		t.Errorf("expected status=suppressed, got %q", status)
	}

	// Edge must be removed from live graph immediately.
	if srv.graph.HasEdge(loginID, logoutID, graph.EdgeMentions) {
		t.Error("rejected edge should be removed from live graph immediately")
	}

	// Verify suppressed=true in store.
	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "MENTIONS" {
			if !e.Suppressed {
				t.Error("expected suppressed=true in store")
			}
			return
		}
	}
	t.Error("MENTIONS edge not found in store after reject")
}

// TestHandleConfirmEdge_EdgeNotFound verifies a clear error when the edge doesn't exist.
func TestHandleConfirmEdge_EdgeNotFound(t *testing.T) {
	t.Parallel()
	srv, _, _ := newPopulatedServer(t)
	res, err := srv.handleConfirmEdge(context.Background(), callTool(map[string]any{
		"a":         "AuthLogin",
		"b":         "AuthLogout",
		"relation":  "MENTIONS",
		"confirmed": true,
	}))
	mustErrorResult(t, res, err)
}

// TestHandleConfirmEdge_NoStore verifies a clear error when no store is attached.
func TestHandleConfirmEdge_NoStore(t *testing.T) {
	t.Parallel()
	g := graph.New("test-repo")
	srv := &Server{graph: g}
	res, err := srv.handleConfirmEdge(context.Background(), callTool(map[string]any{
		"a":         "Foo",
		"b":         "Bar",
		"relation":  "MENTIONS",
		"confirmed": true,
	}))
	mustErrorResult(t, res, err)
}

// TestHandleConfirmEdge_MissingConfirmed verifies an error when confirmed param is absent.
func TestHandleConfirmEdge_MissingConfirmed(t *testing.T) {
	t.Parallel()
	srv, _, _ := newPopulatedServer(t)
	res, err := srv.handleConfirmEdge(context.Background(), callTool(map[string]any{
		"a":        "AuthLogin",
		"b":        "AuthLogout",
		"relation": "MENTIONS",
		// confirmed intentionally omitted
	}))
	mustErrorResult(t, res, err)
}

// TestHandleConfirmEdge_ReapproveAfterReject verifies that an edge rejected then
// re-approved (confirmed=true) is cleared of suppressed and restored in the live graph.
func TestHandleConfirmEdge_ReapproveAfterReject(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	// Create edge, reject it, then re-approve it.
	for _, args := range []map[string]any{
		{"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS"},
	} {
		if _, err := srv.handleLinkEntities(ctx, callTool(args)); err != nil {
			t.Fatalf("link_entities: %v", err)
		}
	}
	if _, err := srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS", "confirmed": false,
	})); err != nil {
		t.Fatalf("reject: %v", err)
	}
	// Edge must be gone from live graph after rejection.
	if srv.graph.HasEdge(loginID, logoutID, graph.EdgeMentions) {
		t.Fatal("edge should be absent after rejection")
	}

	// Re-approve.
	res, err := srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS", "confirmed": true,
	}))
	out := mustResult(t, res, err)
	if status, _ := out["status"].(string); status != "confirmed" {
		t.Errorf("expected status=confirmed after re-approve, got %q", status)
	}

	// Edge must be live again.
	if !srv.graph.HasEdge(loginID, logoutID, graph.EdgeMentions) {
		t.Error("re-approved edge should be back in live graph")
	}

	// Store must have confirmed=true, suppressed=false.
	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "MENTIONS" {
			if !e.Confirmed {
				t.Error("expected confirmed=true after re-approve")
			}
			if e.Suppressed {
				t.Error("expected suppressed=false after re-approve")
			}
			return
		}
	}
	t.Error("MENTIONS edge not found in store")
}

// TestHandleConfirmEdge_RejectPreventsReinject verifies that ReinjectManualEdges
// skips suppressed edges — the persistence-layer contract confirm_edge relies on.
func TestHandleConfirmEdge_RejectPreventsReinject(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	_, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a":        "AuthLogin",
		"b":        "AuthLogout",
		"relation": "MENTIONS",
	}))
	if err != nil {
		t.Fatalf("link_entities setup: %v", err)
	}

	// Reject the edge.
	_, err = srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a":         "AuthLogin",
		"b":         "AuthLogout",
		"relation":  "MENTIONS",
		"confirmed": false,
	}))
	if err != nil {
		t.Fatalf("handleConfirmEdge: %v", err)
	}

	// Simulate a graph rebuild: reinject all persisted edges.
	freshGraph := graph.New("test-repo")
	// Re-add the nodes so endpoints exist.
	for _, n := range srv.graph.AllNodes() {
		freshGraph.AddNode(n)
	}
	if err := srv.store.ReinjectManualEdges(freshGraph); err != nil {
		t.Fatalf("ReinjectManualEdges: %v", err)
	}

	// The suppressed edge must NOT have been re-injected.
	if freshGraph.HasEdge(loginID, logoutID, graph.EdgeMentions) {
		t.Error("suppressed edge was re-injected after graph rebuild — should have been skipped")
	}
}
