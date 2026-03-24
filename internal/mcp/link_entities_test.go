package mcp

import (
	"context"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestHandleLinkEntities_HappyPath verifies that link_entities creates a
// persistent manual edge and adds it to the live in-memory graph.
func TestHandleLinkEntities_HappyPath(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	res, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a":        "AuthLogin",
		"b":        "AuthLogout",
		"relation": "DEPENDS_ON",
		"domain":   "code-to-code",
		"agent_id": "test-agent",
	}))
	out := mustResult(t, res, err)

	if linked, _ := out["linked"].(bool); !linked {
		t.Errorf("expected linked=true, got: %v", out["linked"])
	}
	if rel, _ := out["relation"].(string); rel != "DEPENDS_ON" {
		t.Errorf("expected relation=DEPENDS_ON, got %q", rel)
	}
	if dom, _ := out["domain"].(string); dom != "code-to-code" {
		t.Errorf("expected domain=code-to-code, got %q", dom)
	}

	// Verify the edge is live in the in-memory graph.
	if !srv.graph.HasEdge(loginID, logoutID, graph.EdgeDependsOn) {
		t.Error("expected DEPENDS_ON edge in live graph, not found")
	}

	// Verify persistence: LoadManualEdges should return the edge.
	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	found := false
	for _, e := range edges {
		if e.FromID == loginID && e.ToID == logoutID && e.Relation == "DEPENDS_ON" {
			found = true
			if e.Domain != "code-to-code" {
				t.Errorf("persisted domain = %q, want code-to-code", e.Domain)
			}
			if e.CreatedBy != "test-agent" {
				t.Errorf("persisted created_by = %q, want test-agent", e.CreatedBy)
			}
		}
	}
	if !found {
		t.Error("manual edge not found in store after link_entities")
	}
}

// TestHandleLinkEntities_MissingA verifies an error is returned when a is absent.
func TestHandleLinkEntities_MissingA(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	res, err := srv.handleLinkEntities(context.Background(), callTool(map[string]any{
		"b":        "AuthLogout",
		"relation": "CALLS",
	}))
	mustErrorResult(t, res, err)
}

// TestHandleLinkEntities_EntityNotFound verifies a helpful error for unknown entities.
func TestHandleLinkEntities_EntityNotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t) // empty graph
	res, err := srv.handleLinkEntities(context.Background(), callTool(map[string]any{
		"a":        "NonExistentFoo",
		"b":        "NonExistentBar",
		"relation": "CALLS",
	}))
	mustErrorResult(t, res, err)
}

// TestHandleLinkEntities_CustomRelation verifies custom (non-catalog) relation types
// are stored and a weight_note is present in the response.
func TestHandleLinkEntities_CustomRelation(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	res, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a":        "AuthLogin",
		"b":        "AuthLogout",
		"relation": "DEPLOYS",
		"domain":   "code-to-infra",
	}))
	out := mustResult(t, res, err)

	// The edge should exist in the graph with type "DEPLOYS".
	if !srv.graph.HasEdge(loginID, logoutID, graph.EdgeType("DEPLOYS")) {
		t.Error("expected DEPLOYS edge in live graph, not found")
	}
	// Custom type should produce a weight_note.
	if _, ok := out["weight_note"]; !ok {
		t.Error("expected weight_note for custom relation type, not present")
	}
}

// TestHandleLinkEntities_Idempotent verifies that calling link_entities twice
// for the same pair doesn't create duplicates in the store.
func TestHandleLinkEntities_Idempotent(t *testing.T) {
	t.Parallel()
	srv, _, _ := newPopulatedServer(t)
	ctx := context.Background()

	args := map[string]any{
		"a":        "AuthLogin",
		"b":        "AuthLogout",
		"relation": "CALLS",
	}
	for range 2 {
		res, err := srv.handleLinkEntities(ctx, callTool(args))
		mustResult(t, res, err)
	}

	edges, err := srv.store.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	count := 0
	for _, e := range edges {
		if e.Relation == "CALLS" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 CALLS manual edge, got %d", count)
	}
}

// TestHandleLinkEntities_NoStore verifies a clear error when store is nil.
func TestHandleLinkEntities_NoStore(t *testing.T) {
	t.Parallel()
	g := graph.New("test-repo")
	srv := &Server{graph: g} // no store
	res, err := srv.handleLinkEntities(context.Background(), callTool(map[string]any{
		"a": "Foo", "b": "Bar", "relation": "CALLS",
	}))
	mustErrorResult(t, res, err)
}

// TestHandleLinkEntities_ByNodeID verifies resolution using a full node ID.
func TestHandleLinkEntities_ByNodeID(t *testing.T) {
	t.Parallel()
	srv, loginID, logoutID := newPopulatedServer(t)
	ctx := context.Background()

	res, err := srv.handleLinkEntities(ctx, callTool(map[string]any{
		"a":        string(loginID),
		"b":        string(logoutID),
		"relation": "CALLS",
	}))
	out := mustResult(t, res, err)

	if linked, _ := out["linked"].(bool); !linked {
		t.Error("expected linked=true for node ID resolution")
	}
	if !srv.graph.HasEdge(loginID, logoutID, graph.EdgeCalls) {
		t.Error("expected CALLS edge after node-ID link")
	}
}
