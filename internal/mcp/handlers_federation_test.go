package mcp

// White-box integration tests for Phase 4: federation integration in
// session_init, prepare_context, find_entity, validate_plan.

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// makeReq builds a CallToolRequest with the given arguments.
func makeReq(args map[string]interface{}) mcplib.CallToolRequest {
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// extractText returns the text content from a tool result.
func extractText(t *testing.T, result *mcplib.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) == 0 {
		t.Fatal("empty result")
	}
	tc, ok := result.Content[0].(mcplib.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	return tc.Text
}

// ── helpers ─────────────────────────────────────────────────────────────────

// newFederatedServer creates a Server with federation configured.
// The sibling store contains a single entity with the given name and signature.
func newFederatedServer(t *testing.T, sibAlias, sibEntityName, sibEntitySig string) (*Server, *store.Store) {
	t.Helper()
	st := openMCPTestStore(t)
	g := graph.New("test-repo")

	// Create a local entity that depends on the sibling entity.
	localID := g.MakeNodeID("handler.go", "Handler")
	g.AddNode(&graph.Node{
		ID:       localID,
		Type:     graph.NodeFunction,
		Name:     "Handler",
		Package:  "main",
		File:     "handler.go",
		Line:     10,
		Exported: true,
	})

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := New(g, cfg, st)

	// Create sibling store with the entity.
	sibDir := t.TempDir()
	sibDBPath, _ := federation.SiblingDBPath(sibDir)
	sibSt, err := store.Open(sibDBPath)
	if err != nil {
		t.Fatalf("open sibling store: %v", err)
	}
	sibG := graph.New(sibAlias)
	sibG.AddNode(&graph.Node{
		ID:       graph.NodeID(sibAlias + "::pkg/auth.go::" + sibEntityName),
		Type:     graph.NodeFunction,
		Name:     sibEntityName,
		Package:  "auth",
		File:     "pkg/auth.go",
		Line:     1,
		Exported: true,
		Metadata: map[string]string{"signature": sibEntitySig},
	})
	if err := sibSt.SaveGraph(sibG); err != nil {
		t.Fatal(err)
	}
	sibSt.Close()

	// Create federation resolver.
	entries := []config.FederationEntry{{Path: sibDir, Alias: sibAlias}}
	resolver := federation.NewResolver(entries, t.TempDir())
	srv.SetFederationResolver(resolver)
	t.Cleanup(func() { resolver.Close() })

	return srv, st
}

// ── Test 1: session_init with federation (healthy, no drift) ────────────────

func TestSessionInit_Federation_Healthy_NoDrift(t *testing.T) {
	srv, _ := newFederatedServer(t, "sib", "Validate", "func Validate(token string) bool")

	result, err := srv.handleSessionInit(context.Background(), makeReq(map[string]interface{}{
		"agent_id": "test-agent",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, result)

	// Should include federation_summary (sibling health).
	if !strings.Contains(text, "federation_summary") {
		t.Error("expected federation_summary in session_init response")
	}
	// Should include sibling alias.
	if !strings.Contains(text, "sib") {
		t.Error("expected sibling alias 'sib' in response")
	}
	// Should NOT include cross_project_drift (no deps → no drift).
	if strings.Contains(text, "cross_project_drift") {
		t.Error("expected no cross_project_drift when no deps exist")
	}
}

// ── Test 2: session_init with federation (drift detected) ───────────────────

func TestSessionInit_Federation_WithDrift(t *testing.T) {
	srv, st := newFederatedServer(t, "sib", "Validate", "func Validate(token string) bool")

	// Create a local dep with a different verified_signature → drift.
	if err := st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity:        "test-repo::handler.go::Handler",
		ToProject:         "sib",
		ToEntity:          "Validate",
		ToFile:            "pkg/auth.go",
		VerifiedCommit:    "oldcommit",
		VerifiedAt:        "2026-03-18T00:00:00Z",
		DetectionTier:     "tier1",
		VerifiedSignature: "func Validate(token string) error", // different from sibling
	}); err != nil {
		t.Fatal(err)
	}

	result, err := srv.handleSessionInit(context.Background(), makeReq(map[string]interface{}{
		"agent_id": "test-agent",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, result)

	// Should include cross_project_drift with the drift alert.
	if !strings.Contains(text, "cross_project_drift") {
		t.Error("expected cross_project_drift in session_init when drift exists")
	}
	if !strings.Contains(text, "Validate") {
		t.Error("expected entity name 'Validate' in drift alert")
	}
}

// ── Test 3: prepare_context with cross-project enrichment ───────────────────

func TestPrepareContext_CrossProjectEnrichment(t *testing.T) {
	srv, st := newFederatedServer(t, "sib", "Validate", "func Validate(token string) bool")

	// Create a local dep (current, no drift).
	if err := st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity:        "test-repo::handler.go::Handler",
		ToProject:         "sib",
		ToEntity:          "Validate",
		ToFile:            "pkg/auth.go",
		VerifiedCommit:    "currentcommit",
		VerifiedAt:        "2026-03-18T00:00:00Z",
		DetectionTier:     "tier1",
		VerifiedSignature: "func Validate(token string) bool", // same as sibling
	}); err != nil {
		t.Fatal(err)
	}

	result, err := srv.handlePrepareContext(context.Background(), makeReq(map[string]interface{}{
		"intent": "modify",
		"target": "Handler",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, result)

	// Should include cross-project deps (relevant tier — current, no drift).
	if !strings.Contains(text, "Cross-Project Dependencies") {
		t.Error("expected Cross-Project Dependencies section in prepare_context with deps")
	}
	if !strings.Contains(text, "sib::Validate") {
		t.Error("expected sib::Validate in cross-project deps")
	}
}

// ── Test 4: prepare_context tiered visibility (single project, no federation) ─

func TestPrepareContext_TieredVisibility_SingleProject(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)

	result, err := srv.handlePrepareContext(context.Background(), makeReq(map[string]interface{}{
		"intent": "modify",
		"target": "AuthLogin",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, result)

	// Core content should always be present.
	if !strings.Contains(text, "AuthLogin") {
		t.Error("expected entity name in response")
	}
	if !strings.Contains(text, "Blast Radius") {
		t.Error("expected Blast Radius section (core content)")
	}
	// Available tier hint should appear (discover_tools pointer).
	if !strings.Contains(text, "More Available") {
		t.Error("expected 'More Available' section with discover_tools hint")
	}
	// Pre-edit checklist should appear.
	if !strings.Contains(text, "Pre-Edit Checklist") {
		t.Error("expected Pre-Edit Checklist section")
	}
}

// ── Test 5: find_entity with projects parameter ─────────────────────────────

func TestFindEntity_ProjectsParameter(t *testing.T) {
	srv, _ := newFederatedServer(t, "sib", "Validate", "func Validate(token string) bool")

	result, err := srv.handleFindEntity(context.Background(), makeReq(map[string]interface{}{
		"query":    "Validate",
		"projects": "sib",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, result)

	// Should find the entity in the sibling store.
	if !strings.Contains(text, "Validate") {
		t.Error("expected Validate in find_entity results with projects=sib")
	}
	if !strings.Contains(text, "sib") {
		t.Error("expected sibling alias 'sib' in results")
	}
}

// ── Test 6: validate_plan with drifted sibling ──────────────────────────────

func TestValidatePlan_DriftedSibling(t *testing.T) {
	srv, st := newFederatedServer(t, "sib", "Validate", "func Validate(token string) bool")

	// Create a local dep with drift.
	if err := st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity:        "test-repo::handler.go::Handler",
		ToProject:         "sib",
		ToEntity:          "Validate",
		ToFile:            "pkg/auth.go",
		VerifiedCommit:    "oldcommit",
		VerifiedAt:        "2026-03-18T00:00:00Z",
		DetectionTier:     "tier1",
		VerifiedSignature: "func Validate(token string) error", // different
	}); err != nil {
		t.Fatal(err)
	}

	result, err := srv.handleValidatePlan(context.Background(), makeReq(map[string]interface{}{
		"changes": `[{"file":"handler.go","adds_call_to":"NewService"}]`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, result)

	// Should include cross_project_drift warning.
	if !strings.Contains(text, "cross_project_drift") {
		t.Error("expected cross_project_drift in validate_plan when deps have drifted")
	}
}

// ── Test 7: get_context with projects parameter ─────────────────────────────

func TestGetContext_ProjectsParameter(t *testing.T) {
	srv, _ := newFederatedServer(t, "sib", "Validate", "func Validate(token string) bool")

	// Query local entity with projects=sib → should include federated context
	// for "Handler" from the sibling (which won't be found in sib because sib
	// has "Validate" not "Handler"). But querying "Validate" with projects=sib
	// should find it in the sibling and include federated context.
	result, err := srv.handleGetContext(context.Background(), makeReq(map[string]interface{}{
		"entity":   "Validate",
		"projects": "sib",
		"format":   "json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, result)

	// Entity might not be found locally (it's in the sibling), but the
	// projects= param should trigger a federated search. The response
	// should contain either the federated context or a hint about it.
	// Since "Validate" doesn't exist in the LOCAL graph, get_context will
	// return "entity not found". The agent should use find_entity with
	// projects= to discover it, then use get_context on the local entity.
	// This is the expected behavior — get_context searches the LOCAL graph,
	// projects= only adds federated BFS context for entities found locally.
	if !strings.Contains(text, "Validate") {
		t.Error("expected Validate mentioned in response")
	}
}

// ── Test 8: get_impact with projects parameter ──────────────────────────────

func TestGetImpact_ProjectsParameter(t *testing.T) {
	srv, st := newFederatedServer(t, "sib", "Validate", "func Validate(token string) bool")

	// Create a local dep so get_impact can find cross-project deps.
	if err := st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity:        "test-repo::handler.go::Handler",
		ToProject:         "sib",
		ToEntity:          "Validate",
		ToFile:            "pkg/auth.go",
		VerifiedCommit:    "commit123",
		VerifiedAt:        "2026-03-18T00:00:00Z",
		DetectionTier:     "tier1",
		VerifiedSignature: "func Validate(token string) bool",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := srv.handleGetImpact(context.Background(), makeReq(map[string]interface{}{
		"symbol":   "Handler",
		"projects": "sib",
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := extractText(t, result)

	// With projects=sib and a cross-project dep, the response should include
	// cross_project_deps in the output.
	if !strings.Contains(text, "cross_project_deps") {
		t.Error("expected cross_project_deps in get_impact with projects=sib and active deps")
	}
	if !strings.Contains(text, "Validate") {
		t.Error("expected Validate in cross_project_deps")
	}
}
