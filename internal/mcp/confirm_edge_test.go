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

// TestHandleConfirmEdge_LinkEntitiesPreservesConfirmed verifies that calling
// link_entities on an already-confirmed edge does NOT reset the confirmed flag.
// Bug: clearSuppressed=true previously also set confirmed=0, silently undoing
// a human's review decision if they called link_entities a second time.
func TestHandleConfirmEdge_LinkEntitiesPreservesConfirmed(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	// Create, confirm, then call link_entities again (e.g. to update domain).
	if _, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS", "domain": "code-to-infra",
	})); err != nil {
		t.Fatalf("link_entities initial: %v", err)
	}
	if _, err := srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS", "confirmed": true,
	})); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// Re-call link_entities to update domain. Must NOT reset confirmed flag.
	if _, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS", "domain": "updated-domain",
	})); err != nil {
		t.Fatalf("link_entities re-call: %v", err)
	}

	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "MENTIONS" {
			if !e.Confirmed {
				t.Error("link_entities must not reset confirmed=1 — confirm_edge owns that flag")
			}
			if e.Suppressed {
				t.Error("edge should not be suppressed")
			}
			return
		}
	}
	t.Error("MENTIONS edge not found in store")
}

// TestHandleConfirmEdge_LinkEntitiesAfterRejectClearsSuppressed verifies that
// explicitly calling link_entities after a rejection clears the suppressed flag.
// Bug: previously the ON CONFLICT didn't reset suppressed=0, so the edge stayed
// invisible after restart even though link_entities returned linked=true.
func TestHandleConfirmEdge_LinkEntitiesAfterRejectClearsSuppressed(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	// Create, reject, then re-create via link_entities.
	if _, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS",
	})); err != nil {
		t.Fatalf("link_entities initial: %v", err)
	}
	if _, err := srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS", "confirmed": false,
	})); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// Re-create via link_entities — this must clear suppressed.
	res, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS",
	}))
	mustResult(t, res, err)

	// Edge must be live and DB must have suppressed=false.
	if !srv.graph.HasEdge(loginID, logoutID, graph.EdgeMentions) {
		t.Error("edge should be live after re-link")
	}
	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "MENTIONS" {
			if e.Suppressed {
				t.Error("re-linked edge must not remain suppressed — link_entities clears suppressions")
			}
			return
		}
	}
	t.Error("MENTIONS edge not found in store")
}

// TestHandleConfirmEdge_ConfirmedConfidenceProtected verifies that the name-matcher
// cannot downgrade a confirmed edge's confidence on subsequent runs.
// Bug: SaveManualEdge ON CONFLICT always overwrote confidence, so confirmed edges
// would silently lose their 1.0 confidence on next NameMatcher pass.
func TestHandleConfirmEdge_ConfirmedConfidenceProtected(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	// Create and confirm the edge.
	if _, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS",
	})); err != nil {
		t.Fatalf("link_entities: %v", err)
	}
	if _, err := srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS", "confirmed": true,
	})); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	// Simulate NameMatcher re-scoring with lower confidence.
	if _, err := srv.store.SaveSyntheticEdge(loginID, logoutID, graph.EdgeMentions, 0.65); err != nil {
		t.Fatalf("SaveSyntheticEdge (simulated re-run): %v", err)
	}

	// Confidence must still be 1.0 — confirmed edges are protected.
	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "MENTIONS" {
			if e.Confidence != 1.0 {
				t.Errorf("confirmed edge confidence was overwritten to %v — must stay 1.0", e.Confidence)
			}
			if !e.Confirmed {
				t.Error("confirmed flag was cleared by SaveSyntheticEdge")
			}
			return
		}
	}
	t.Error("MENTIONS edge not found in store")
}

// TestHandleConfirmEdge_ReverseDirectionAutoRetry verifies that confirm_edge
// automatically tries the reverse direction when the primary lookup fails.
// The name-matcher uses orderEdge (heavier domain first) so users may not know
// which direction the edge was stored in.
func TestHandleConfirmEdge_ReverseDirectionAutoRetry(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	// Create edge in loginID→logoutID direction.
	if _, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a": "AuthLogin", "b": "AuthLogout", "relation": "MENTIONS",
	})); err != nil {
		t.Fatalf("link_entities: %v", err)
	}

	// Call confirm_edge with REVERSED direction (b→a).
	res, err := srv.handleConfirmEdge(ctx, callTool(map[string]any{
		"a":         "AuthLogout", // reversed
		"b":         "AuthLogin",  // reversed
		"relation":  "MENTIONS",
		"confirmed": true,
	}))
	out := mustResult(t, res, err)

	// Should succeed via auto-retry.
	if status, _ := out["status"].(string); status != "confirmed" {
		t.Errorf("expected confirmed after reverse-direction retry, got %q", status)
	}

	// Verify the original-direction edge is confirmed in the store.
	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "MENTIONS" {
			if !e.Confirmed {
				t.Error("original-direction edge should be confirmed after reverse-direction call")
			}
			return
		}
	}
	t.Error("original MENTIONS edge not found in store after reverse-direction confirm")
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
