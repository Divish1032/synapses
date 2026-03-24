package mcp

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestHandleUnlinkEntities_HappyPath verifies that unlink_entities removes
// both the in-memory edge and the persisted manual edge.
func TestHandleUnlinkEntities_HappyPath(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	// First, create the edge.
	_, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "DEPENDS_ON",
	}))
	if err != nil {
		t.Fatalf("link_entities: %v", err)
	}
	if !srv.graph.HasEdge(loginID, logoutID, graph.EdgeDependsOn) {
		t.Fatal("edge not created by link_entities")
	}

	// Now unlink.
	res, err := srv.handleUnlinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "DEPENDS_ON",
	}))
	out := mustResult(t, res, err)

	if unlinked, _ := out["unlinked"].(bool); !unlinked {
		t.Errorf("expected unlinked=true, got: %v", out["unlinked"])
	}

	// Verify edge is gone from live graph.
	if srv.graph.HasEdge(loginID, logoutID, graph.EdgeDependsOn) {
		t.Error("expected DEPENDS_ON edge to be removed from live graph")
	}

	// Verify edge is gone from store.
	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "DEPENDS_ON" {
			t.Error("manual edge still present in store after unlink")
		}
	}
}

// TestHandleUnlinkEntities_EdgeNotFound verifies a clear error when the edge
// doesn't exist in the manual edges store.
func TestHandleUnlinkEntities_EdgeNotFound(t *testing.T) {
	t.Parallel()
	srv, _, _ := newPopulatedServer(t)
	ctx := context.Background()

	res, err := srv.handleUnlinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "DEPENDS_ON",
	}))
	mustErrorResult(t, res, err)
}

// TestHandleUnlinkEntities_NoStore verifies a clear error when store is nil.
func TestHandleUnlinkEntities_NoStore(t *testing.T) {
	t.Parallel()
	g := graph.New("test-repo")
	srv := &Server{graph: g}

	res, err := srv.handleUnlinkEntities(context.Background(), callTool(map[string]any{
		"a": "Foo", "b": "Bar", "relation": "CALLS",
	}))
	mustErrorResult(t, res, err)
}

// TestHandleUnlinkEntities_DoesNotRemoveAutoEdge verifies that unlink only
// removes manual edges and returns not-found for structural edges.
func TestHandleUnlinkEntities_DoesNotRemoveAutoEdge(t *testing.T) {
	t.Parallel()
	srv, loginID, _ := newPopulatedServer(t)
	ctx := context.Background()

	// newPopulatedServer creates HandleRequest→AuthLogin CALLS edge automatically
	// (not via link_entities), so it's NOT in manual_edges.
	// Attempting to unlink "AuthLogin" → itself should return an error.
	res, err := srv.handleUnlinkEntities(ctx, callTool(map[string]any{
		"a": "HandleRequest", "b": "AuthLogin", "relation": "CALLS",
	}))
	mustErrorResult(t, res, err)

	// Auto-detected edge must still be present.
	callerID := srv.graph.MakeNodeID("pkg/api/handler.go", "HandleRequest")
	if !srv.graph.HasEdge(callerID, loginID, graph.EdgeCalls) {
		t.Error("auto-detected CALLS edge was incorrectly removed")
	}
}

// TestHandleUnlinkEntities_MissingParam verifies an error when 'a' is absent.
func TestHandleUnlinkEntities_MissingParam(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	res, err := srv.handleUnlinkEntities(context.Background(), callTool(map[string]any{
		"b": "AuthLogout", "relation": "CALLS",
	}))
	mustErrorResult(t, res, err)
}

// TestHandleUnlinkEntities_RelinkAfterUnlink verifies the full lifecycle:
// link → unlink → link again succeeds and the edge re-appears in store.
func TestHandleUnlinkEntities_RelinkAfterUnlink(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	args := map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "DEPENDS_ON",
	}

	_, err := srv.handleLinkEntities(ctx, callTool(args))
	if err != nil {
		t.Fatalf("first link: %v", err)
	}
	_, err = srv.handleUnlinkEntities(ctx, callTool(args))
	if err != nil {
		t.Fatalf("unlink: %v", err)
	}
	res, err := srv.handleLinkEntities(ctx, callTool(args))
	out := mustResult(t, res, err)

	if linked, _ := out["linked"].(bool); !linked {
		t.Error("expected linked=true on re-link")
	}
	if !srv.graph.HasEdge(loginID, logoutID, graph.EdgeDependsOn) {
		t.Error("edge missing after re-link")
	}

	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	count := 0
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "DEPENDS_ON" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 DEPENDS_ON edge after re-link, got %d", count)
	}
}
