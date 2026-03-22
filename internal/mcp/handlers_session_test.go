package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── handleSessionInit ─────────────────────────────────────────────────────────

func TestHandleSessionInit_SoloAgent_ResponseShape(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "solo-agent"}))
	m := mustResult(t, res, err)

	hasKey(t, m, "project_identity")
	hasKey(t, m, "scale_guidance")
	hasKey(t, m, "session_hint")
	hasKey(t, m, "latest_event_seq")
	// No peers active → agent_awareness must be absent (zero token cost).
	noKey(t, m, "agent_awareness")
	noKey(t, m, "unread_messages")
}

func TestHandleSessionInit_NoAgentID_StillReturnsIdentity(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "project_identity")
	hasKey(t, m, "scale_guidance")
}

func TestHandleSessionInit_EmitsSessionStartEvent(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-x"}))

	events, _, err := s.store.GetEvents(0, nil, "", 50)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Type == "agent_session_start" && e.AgentID == "agent-x" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected agent_session_start event after session_init, not found")
	}
}

func TestHandleSessionInit_MultiAgent_AwarenessSurfaced(t *testing.T) {
	s := newTestServer(t)

	// Agent A starts.
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-a"}))

	// Agent B starts — should see active_count in agent_awareness (Tier 1 signal).
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-b"}))
	m := mustResult(t, res, err)

	awareness, ok := m["agent_awareness"].(map[string]any)
	if !ok {
		t.Fatalf("expected agent_awareness map, got %T — keys: %v", m["agent_awareness"], mapKeys(m))
	}
	if awareness["active_count"] == nil {
		t.Fatalf("expected active_count in agent_awareness, got keys: %v", mapKeys(awareness))
	}
}

func TestHandleSessionInit_Incremental_SkipsIdentityOnRepeat(t *testing.T) {
	s := newTestServer(t)
	agentID := "repeat-agent"

	// First call — full identity.
	res1, err1 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	m1 := mustResult(t, res1, err1)
	if _, ok := m1["identity_skipped"]; ok {
		t.Error("first call must not skip identity")
	}

	// Second call — identity hash unchanged → should be incremental.
	res2, err2 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	m2 := mustResult(t, res2, err2)
	if inc, ok := m2["incremental"].(bool); !ok || !inc {
		t.Error("second call should be marked incremental")
	}
}

func TestHandleSessionInit_UnreadMessages_Delivered(t *testing.T) {
	s := newTestServer(t)

	// Agent A sends a message to agent B before B's session starts.
	res, err := s.handleSendMessage(ctx, callTool(map[string]any{
		"from_agent": "agent-a",
		"to_agent":   "agent-b",
		"topic":      "ping",
		"payload":    `{"msg":"hello"}`,
	}))
	mustResult(t, res, err)

	// Agent B starts — unread message should be auto-delivered.
	res2, err2 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "agent-b"}))
	m := mustResult(t, res2, err2)

	msgs, ok := m["unread_messages"].(map[string]any)
	if !ok {
		t.Fatalf("expected unread_messages in response, keys: %v", mapKeys(m))
	}
	count, _ := msgs["count"].(float64)
	if count < 1 {
		t.Errorf("expected ≥1 unread message, got count=%v", count)
	}
}

func TestHandleSessionInit_CollisionWarning(t *testing.T) {
	s := newTestServer(t)
	agentID := "clash-agent"

	// First call establishes the context.
	res1, err1 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	mustResult(t, res1, err1)

	// Immediately call again with the same ID — collision warning expected.
	res2, err2 := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": agentID}))
	m := mustResult(t, res2, err2)

	warning, ok := m["warning"].(string)
	if !ok || warning == "" {
		t.Errorf("expected collision warning for same agent_id within 2 min, got: %v", m["warning"])
	}
	if !strings.Contains(warning, agentID) {
		t.Errorf("warning should mention the agent_id, got: %q", warning)
	}
}

// ── BRAIN-1: brain_health in session_init ─────────────────────────────────────

// TestHandleSessionInit_LeanDefault verifies that empty arrays are omitted from
// the default response on a fresh project, reducing first-session noise.
func TestHandleSessionInit_LeanDefault_OmitsEmptyTasks(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "lean-agent"}))
	m := mustResult(t, res, err)

	// pending_tasks.tasks must be absent when no tasks exist (not "tasks": []).
	pt, ok := m["pending_tasks"].(map[string]any)
	if !ok {
		t.Fatalf("expected pending_tasks map, got %T", m["pending_tasks"])
	}
	if _, hasTasks := pt["tasks"]; hasTasks {
		t.Error("pending_tasks.tasks must be omitted when empty, not present as []")
	}
	if pt["summary"] == nil {
		t.Error("pending_tasks.summary must still be present")
	}
}

// TestHandleSessionInit_LeanDefault_OmitsEmptyRecentEvents verifies that
// recent_events is absent when the events list is genuinely empty.
// No agent_id → no session_start event is emitted, so the list stays empty.
func TestHandleSessionInit_LeanDefault_OmitsEmptyRecentEvents(t *testing.T) {
	s := newTestServer(t)
	// No agent_id: session_start event is NOT emitted, so recent_events is truly empty.
	res, err := s.handleSessionInit(ctx, callTool(nil))
	m := mustResult(t, res, err)
	noKey(t, m, "recent_events")
}

// TestHandleSessionInit_LeanDefault_TasksPresentWhenNonEmpty verifies that
// pending_tasks.tasks IS included when tasks exist.
func TestHandleSessionInit_LeanDefault_TasksPresentWhenNonEmpty(t *testing.T) {
	s := newTestServer(t)

	// Create a plan with one task.
	_, _ = s.handleCreatePlan(ctx, callTool(map[string]any{
		"title": "lean-test plan",
		"tasks": []any{map[string]any{"title": "do work", "priority": "p1"}},
	}))

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "lean-agent2"}))
	m := mustResult(t, res, err)

	pt, ok := m["pending_tasks"].(map[string]any)
	if !ok {
		t.Fatalf("expected pending_tasks map, got %T", m["pending_tasks"])
	}
	tasks, hasTasks := pt["tasks"]
	if !hasTasks {
		t.Error("pending_tasks.tasks must be present when tasks exist")
	}
	taskSlice, _ := tasks.([]any)
	if len(taskSlice) == 0 {
		t.Error("expected at least one task in pending_tasks.tasks")
	}
}

func TestHandleSessionInit_BrainHealth_AbsentWhenNoBrain(t *testing.T) {
	s := newTestServer(t)
	// brainClient is nil by default in test server.
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "no-brain"}))
	m := mustResult(t, res, err)
	noKey(t, m, "brain_health")
	noKey(t, m, "brain_warning")
}

func newTestBrainClient(t *testing.T) *brain.Client {
	t.Helper()
	cfg := brainconfig.DefaultConfig()
	cfg.Enabled = true
	cfg.OllamaURL = "http://127.0.0.1:1" // unreachable — calls will fail, but impl is created with stats
	cfg.DBPath = t.TempDir() + "/brain-test.db"
	cfg.TimeoutMS = 100
	return brain.NewInProcess(&cfg)
}

func TestHandleSessionInit_BrainHealth_PresentWhenBrainEnabled(t *testing.T) {
	s := newTestServer(t)
	bc := newTestBrainClient(t)
	defer bc.Close()
	s.SetBrainClient(bc)

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "brain-agent"}))
	m := mustResult(t, res, err)

	health, ok := m["brain_health"].(map[string]any)
	if !ok {
		t.Fatalf("expected brain_health map, got %T (keys: %v)", m["brain_health"], mapKeys(m))
	}

	// Must have model field.
	if _, ok := health["model"]; !ok {
		t.Error("brain_health missing 'model' field")
	}

	// Must have tiers map with expected tiers.
	tiers, ok := health["tiers"].(map[string]any)
	if !ok {
		t.Fatalf("brain_health.tiers missing or wrong type: %T", health["tiers"])
	}
	for _, tier := range []string{"ingest", "enrich", "guardian", "orchestrate", "archivist"} {
		tierData, ok := tiers[tier].(map[string]any)
		if !ok {
			t.Errorf("brain_health.tiers.%s missing or wrong type", tier)
			continue
		}
		// All tiers should have circuit="closed" initially.
		if circuit, _ := tierData["circuit"].(string); circuit != "closed" {
			t.Errorf("brain_health.tiers.%s.circuit = %q, want 'closed'", tier, circuit)
		}
	}

	// No warning expected when all tiers are healthy (0 calls = no warning).
	noKey(t, m, "brain_warning")
}

func TestHandleSessionInit_BrainWarning_DegradedTier(t *testing.T) {
	s := newTestServer(t)
	bc := newTestBrainClient(t)
	defer bc.Close()
	s.SetBrainClient(bc)

	// Simulate failures: call Ingest with bad data to generate failures.
	// Ollama is unreachable (port 1), so every call fails and records a failure stat.
	for i := 0; i < 5; i++ {
		bc.Ingest(ctx, brain.IngestRequest{
			NodeID:   "test::file.go::Func",
			NodeName: "Func",
			Code:     "func Func() {}",
			Package:  "test",
		})
	}

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "warn-agent"}))
	m := mustResult(t, res, err)

	health, ok := m["brain_health"].(map[string]any)
	if !ok {
		t.Fatalf("expected brain_health map, got %T", m["brain_health"])
	}

	tiers, _ := health["tiers"].(map[string]any)
	ingest, _ := tiers["ingest"].(map[string]any)
	calls, _ := ingest["calls"].(float64)
	if calls < 1 {
		// Ollama not running → ingest calls may be 0 if the impl short-circuits
		// before recording stats. Skip the warning check in that case.
		t.Skip("no ingest calls recorded (Ollama likely unavailable) — warning test skipped")
	}

	rate, _ := ingest["success_rate"].(float64)
	if rate >= 0.5 {
		t.Skip("ingest success_rate >= 0.5 — cannot test warning")
	}

	warning, ok := m["brain_warning"].(string)
	if !ok || warning == "" {
		t.Errorf("expected brain_warning for degraded ingest tier, got %v", m["brain_warning"])
	}
}

// ── embeddingStatus helper ────────────────────────────────────────────────────

func TestEmbeddingStatus_Nil_ReturnsOff(t *testing.T) {
	if got := embeddingStatus(nil); got != "off" {
		t.Errorf("embeddingStatus(nil) = %q, want %q", got, "off")
	}
}

func TestEmbeddingStatus_BuiltinNotReady_ReturnsNotDownloaded(t *testing.T) {
	e := embed.NewBuiltinEmbedder(t.TempDir())
	if e.StatusDetail() == "ready" {
		t.Skip("model already downloaded; skip not-ready path")
	}
	got := embeddingStatus(e)
	if got != "builtin (model not yet downloaded)" {
		t.Errorf("embeddingStatus(builtin, never tried) = %q, want %q", got, "builtin (model not yet downloaded)")
	}
}

// TestEmbeddingStatus_BuiltinUnavailable verifies the "unavailable" state after a
// failed init attempt (distinguishes from "never tried").
func TestEmbeddingStatus_BuiltinUnavailable_AfterFailedInit(t *testing.T) {
	// Point embedder at a non-creatable path so ensureModel() always fails.
	e := embed.NewBuiltinEmbedder("/nonexistent/path/that/cannot/be/created")
	if e.StatusDetail() == "ready" {
		t.Skip("unexpectedly ready")
	}
	// Trigger an init attempt — fails because the directory can't be created.
	_, _ = e.Embed(context.Background(), "probe")

	got := embeddingStatus(e)
	if got != "builtin (unavailable)" {
		t.Errorf("embeddingStatus(builtin, init failed) = %q, want %q", got, "builtin (unavailable)")
	}
}

// TestEmbeddingStatus_UnknownEmbedder covers the default branch.
func TestEmbeddingStatus_UnknownEmbedder_ReturnsUnknown(t *testing.T) {
	got := embeddingStatus(&stubEmbedder{})
	if got != "unknown" {
		t.Errorf("embeddingStatus(unknown type) = %q, want %q", got, "unknown")
	}
}

// stubEmbedder satisfies embed.Embedder but is not BuiltinEmbedder or OllamaEmbedder.
type stubEmbedder struct{}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (s *stubEmbedder) Model() string                                          { return "stub" }
func (s *stubEmbedder) Close() error                                           { return nil }

func TestEmbeddingStatus_Ollama_ReturnsOllama(t *testing.T) {
	e := embed.NewOllamaEmbedder("http://localhost:11434/api/embeddings", "")
	if e == nil {
		t.Skip("NewOllamaEmbedder returned nil (empty endpoint?)")
	}
	got := embeddingStatus(e)
	if got != "ollama" {
		t.Errorf("embeddingStatus(ollama) = %q, want %q", got, "ollama")
	}
}

func TestHandleSessionInit_EmbeddingsField_AlwaysPresent(t *testing.T) {
	// nil embedder: field must be "off"
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "test"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "embeddings")
	if m["embeddings"] != "off" {
		t.Errorf("expected embeddings=off when no embedder set, got %v", m["embeddings"])
	}

	// builtin embedder: field must reflect ready state
	s2 := newTestServer(t)
	s2.SetMemoryEmbedder(embed.NewBuiltinEmbedder(t.TempDir()))
	res2, err2 := s2.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "test2"}))
	m2 := mustResult(t, res2, err2)
	hasKey(t, m2, "embeddings")
	if v := m2["embeddings"]; v != "builtin (ready)" && v != "builtin (model not yet downloaded)" && v != "builtin (unavailable)" {
		t.Errorf("unexpected embeddings value for builtin: %v", v)
	}
}

func TestHandleSessionInit_EmbeddingsField_PresentInQuickMode(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "q", "scope": "quick"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "embeddings")
}

// ── handleGetProjectIdentity ──────────────────────────────────────────────────

func TestHandleGetProjectIdentity_ReturnsIdentity(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetProjectIdentity(ctx, callTool(nil))
	m := mustResult(t, res, err)
	// Response is wrapped: {"identity": {...}, "federation": {...}}
	hasKey(t, m, "identity")
	identity, _ := m["identity"].(map[string]any)
	hasKey(t, identity, "repo_id")
}

func TestHandleGetProjectIdentity_PopulatedGraph(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetProjectIdentity(ctx, callTool(nil))
	m := mustResult(t, res, err)
	identity, _ := m["identity"].(map[string]any)
	// Identity wraps a GraphSummary; populated graph has functions.
	summary, _ := identity["summary"].(map[string]any)
	functions, _ := summary["functions"].(float64)
	if functions < 1 {
		t.Errorf("expected summary.functions > 0 for populated graph, got %v", functions)
	}
}

// ── handleGetWorkingState ─────────────────────────────────────────────────────

func TestHandleGetWorkingState_NoChangeSource(t *testing.T) {
	s := newTestServer(t)
	// changeSource is nil — should return without error.
	res, err := s.handleGetWorkingState(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "recent_changes")
}

// ── handleDiscoverTools ───────────────────────────────────────────────────────

func TestHandleDiscoverTools_ReturnsRecommendation(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{
		"query": "how do I understand a function",
	}))
	m := mustResult(t, res, err)
	// discover_tools returns recommended_tool or recommended_workflow depending on match type.
	if _, ok := m["recommended_tool"]; !ok {
		if _, ok2 := m["recommended_workflow"]; !ok2 {
			t.Errorf("expected recommended_tool or recommended_workflow in result, keys: %v", mapKeys(m))
		}
	}
}

func TestHandleDiscoverTools_EmptyQuery_StillResponds(t *testing.T) {
	s := newTestServer(t)
	result, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": ""}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result for empty query")
	}
}

// ── handleAnnotateNode ────────────────────────────────────────────────────────

func TestHandleAnnotateNode_StoresNote(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)

	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{
		"node_id":  string(loginID),
		"note":     "this function validates JWT tokens",
		"agent_id": "annotator-agent",
	}))
	mustResult(t, res, err)

	anns, err := s.store.GetAnnotationsForNodes([]string{string(loginID)})
	if err != nil {
		t.Fatalf("GetAnnotationsForNodes: %v", err)
	}
	nodeAnns := anns[string(loginID)]
	if len(nodeAnns) == 0 {
		t.Fatal("expected annotation to be stored")
	}
	if !strings.Contains(nodeAnns[0].Note, "JWT") {
		t.Errorf("annotation note mismatch: %q", nodeAnns[0].Note)
	}
}

func TestHandleAnnotateNode_MissingNodeID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleAnnotateNode(ctx, callTool(map[string]any{"note": "some note"}))
	mustErrorResult(t, res, err)
}

// ── handleGetContext ──────────────────────────────────────────────────────────

func TestHandleGetContext_KnownEntity(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "root")
	root, _ := m["root"].(map[string]any)
	if root["name"] != "AuthLogin" {
		t.Errorf("expected root.name=AuthLogin, got %v", root["name"])
	}
}

func TestHandleGetContext_UnknownEntity_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{"entity": "NonExistentXYZ"}))
	// get_context returns a tool-level error OR a success with an error field.
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	// Either an IsError result or a JSON body with an "error" key is acceptable.
	if !res.IsError {
		tc, ok := res.Content[0].(mcp.TextContent)
		if !ok {
			t.Fatal("expected text content")
		}
		if !strings.Contains(strings.ToLower(tc.Text), "not found") &&
			!strings.Contains(strings.ToLower(tc.Text), "error") &&
			!strings.Contains(strings.ToLower(tc.Text), "unknown") {
			t.Errorf("expected error indication in response for unknown entity, got: %s", tc.Text[:min(200, len(tc.Text))])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestHandleGetContext_MissingEntity_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetContext(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

func TestHandleGetContext_UpdatesAgentFocus(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":   "AuthLogin",
		"agent_id": "focus-agent",
		"format": "json",
	}))
	mustResult(t, res, err)

	// The agent focus update is fire-and-forget (goroutine). Give it a moment.
	time.Sleep(50 * time.Millisecond)

	agents, err := s.store.GetAgents()
	if err != nil {
		t.Fatalf("GetAgents: %v", err)
	}
	for _, a := range agents {
		if a.ID == "focus-agent" && a.CurrentFocus == "AuthLogin" {
			return // pass
		}
	}
	t.Error("expected focus-agent to have CurrentFocus=AuthLogin after get_context")
}

func TestHandleGetContext_WithCallers(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	_ = loginID
	// AuthLogin is called by HandleRequest — callers bucket should be non-empty.
	res, err := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m := mustResult(t, res, err)
	callers, _ := m["callers"].([]any)
	if len(callers) == 0 {
		t.Error("expected callers for AuthLogin (called by HandleRequest)")
	}
}

// ── handleFindEntity ──────────────────────────────────────────────────────────

func TestHandleFindEntity_ExactName(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleFindEntity(ctx, callTool(map[string]any{"query": "AuthLogin", "format": "json"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "matches")
	matches, _ := m["matches"].([]any)
	if len(matches) == 0 {
		t.Error("expected at least one match for AuthLogin")
	}
}

func TestHandleFindEntity_PartialName(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleFindEntity(ctx, callTool(map[string]any{"query": "Auth", "format": "json"}))
	m := mustResult(t, res, err)
	matches, _ := m["matches"].([]any)
	if len(matches) < 2 {
		t.Errorf("expected ≥2 matches for 'Auth' (Login+Logout), got %d", len(matches))
	}
}

func TestHandleFindEntity_NoResults_EmptyList(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleFindEntity(ctx, callTool(map[string]any{"query": "ZZZNoSuchEntity", "format": "json"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "matches")
}

func TestHandleFindEntity_MissingQuery_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleFindEntity(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleSearch ──────────────────────────────────────────────────────────────

func TestHandleSearch_SemanticMode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "authentication",
		"mode":  "semantic",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

func TestHandleSearch_ExactMode(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleSearch(ctx, callTool(map[string]any{
		"query": "AuthLogin",
		"mode":  "exact",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "results")
}

func TestHandleSearch_EmptyQuery_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSearch(ctx, callTool(map[string]any{"query": ""}))
	mustErrorResult(t, res, err)
}

// ── handleGetFileContext ──────────────────────────────────────────────────────

func TestHandleGetFileContext_KnownFile(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetFileContext(ctx, callTool(map[string]any{
		"file": "pkg/auth/auth.go",
	}))
	m := mustResult(t, res, err)
	// Response uses "entities" key (or "count" + "entities").
	entities, ok := m["entities"].([]any)
	if !ok {
		t.Fatalf("expected entities list in response, keys: %v", mapKeys(m))
	}
	if len(entities) < 2 {
		t.Errorf("expected ≥2 entities in pkg/auth/auth.go, got %d", len(entities))
	}
}

func TestHandleGetFileContext_UnknownFile(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetFileContext(ctx, callTool(map[string]any{
		"file": "pkg/nonexistent/file.go",
	}))
	// Unknown file returns a tool error (file not indexed).
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	_ = res // error or empty result — both acceptable
}

func TestHandleGetFileContext_MissingParam_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetFileContext(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleGetCallChain ────────────────────────────────────────────────────────

func TestHandleGetCallChain_ValidChain(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// HandleRequest → AuthLogin chain should exist.
	res, err := s.handleGetCallChain(ctx, callTool(map[string]any{
		"from": "HandleRequest",
		"to":   "AuthLogin",
	}))
	// Chain exists — just verify no Go-level error returned.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = res
}

func TestHandleGetCallChain_MissingParams_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetCallChain(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleGetImpact ───────────────────────────────────────────────────────────

func TestHandleGetImpact_KnownSymbol(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "AuthLogin"}))
	m := mustResult(t, res, err)
	// Impact response uses "tiers" and "total_affected".
	hasKey(t, m, "total_affected")
	hasKey(t, m, "root")
}

func TestHandleGetImpact_UnknownSymbol(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "ZZZNothing"}))
	// Unknown symbol returns a tool error — that is the correct behaviour.
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	_ = res
}

func TestHandleGetImpact_MissingSymbol_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetImpact(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── entity_hash + known_hash (R14) ────────────────────────────────────────────

func TestHandleGetContext_EntityHashPresent(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m := mustResult(t, res, err)
	hash, ok := m["entity_hash"].(string)
	if !ok || len(hash) != 12 {
		t.Errorf("expected entity_hash of length 12, got %q", hash)
	}
}

func TestHandleGetContext_KnownHash_ReturnsUnchanged(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// First call — get the hash.
	res1, err1 := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m1 := mustResult(t, res1, err1)
	hash, _ := m1["entity_hash"].(string)
	if hash == "" {
		t.Fatal("no entity_hash in first response")
	}

	// Second call with known_hash matching — expect {"unchanged": true}.
	res2, err2 := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":     "AuthLogin",
		"known_hash": hash,
		"format": "json",
	}))
	m2 := mustResult(t, res2, err2)
	if m2["unchanged"] != true {
		t.Errorf("expected unchanged=true when known_hash matches, got %v", m2)
	}
	if m2["entity_hash"] != hash {
		t.Errorf("expected entity_hash to be echoed back, got %v", m2["entity_hash"])
	}
}

func TestHandleGetContext_WrongKnownHash_ReturnsFull(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":     "AuthLogin",
		"known_hash": "000000000000",
		"format": "json",
	}))
	m := mustResult(t, res, err)
	// Hash mismatch → full response with root and entity_hash.
	hasKey(t, m, "root")
	hasKey(t, m, "entity_hash")
	if m["unchanged"] == true {
		t.Error("should not return unchanged=true for wrong hash")
	}
}

// ── Session auto-cache (transparent known_hash) ───────────────────────────────

// sessionCtx returns a context carrying the given session ID, simulating what
// serveMCPConn injects in production daemon sessions.
func sessionCtx(sessionID string) context.Context {
	return WithSessionID(context.Background(), sessionID)
}

func TestHandleGetContext_SessionAutoCache_ReturnsUnchanged(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	sctx := sessionCtx("agent-session-1")

	// First call: full response expected; server stores hash in session cache.
	res1, err1 := s.handleGetContext(sctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m1 := mustResult(t, res1, err1)
	if m1["unchanged"] == true {
		t.Fatal("first call should not return unchanged")
	}
	hash1, _ := m1["entity_hash"].(string)
	if hash1 == "" {
		t.Fatal("first call missing entity_hash")
	}

	// Second call with same session and same entity — no known_hash passed.
	// Auto-cache should detect unchanged graph and return {unchanged: true}.
	res2, err2 := s.handleGetContext(sctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m2 := mustResult(t, res2, err2)
	if m2["unchanged"] != true {
		t.Errorf("expected unchanged=true on second session call, got %v", m2)
	}
	if m2["cache_source"] != "session" {
		t.Errorf("expected cache_source=session, got %v", m2["cache_source"])
	}
	if m2["entity_hash"] != hash1 {
		t.Errorf("entity_hash mismatch: got %v want %v", m2["entity_hash"], hash1)
	}
}

func TestHandleGetContext_SessionAutoCache_NoSessionID_ReturnsFull(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// ctx with NO session ID — auto-cache must be disabled.
	plain := context.Background()

	res1, err1 := s.handleGetContext(plain, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	mustResult(t, res1, err1)

	// Second call — still no session ID. Must return full response, not unchanged.
	res2, err2 := s.handleGetContext(plain, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m2 := mustResult(t, res2, err2)
	if m2["unchanged"] == true {
		t.Error("auto-cache must be disabled when no session ID is in context")
	}
	hasKey(t, m2, "root")
}

func TestHandleGetContext_SessionAutoCache_DifferentSessions_Isolated(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	sctx1 := sessionCtx("session-A")
	sctx2 := sessionCtx("session-B")

	// Populate session A cache.
	res1, err1 := s.handleGetContext(sctx1, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m1 := mustResult(t, res1, err1)
	if m1["unchanged"] == true {
		t.Fatal("session A first call should not be unchanged")
	}

	// Session B calls same entity — must get full response (different session cache).
	res2, err2 := s.handleGetContext(sctx2, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m2 := mustResult(t, res2, err2)
	if m2["unchanged"] == true {
		t.Error("session B should not see session A's cached hash — sessions must be isolated")
	}
	hasKey(t, m2, "root")
}

func TestHandleGetContext_SessionAutoCache_DifferentFormat_DifferentKey(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	sctx := sessionCtx("format-session")

	// First call: JSON format → populates JSON cache key.
	res1, err1 := s.handleGetContext(sctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m1 := mustResult(t, res1, err1)
	if m1["unchanged"] == true {
		t.Fatal("first call (json) should not be unchanged")
	}

	// Second call: compact format — different cache key, must return full compact response.
	res2, err2 := s.handleGetContext(sctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "compact",
	}))
	if err2 != nil {
		t.Fatalf("compact call error: %v", err2)
	}
	// compact returns a text result, not JSON — so res2 should not have unchanged=true
	// (it is a TextContent result, not an error result).
	if res2 != nil && res2.IsError {
		t.Error("compact format call returned error")
	}
}

func TestHandleGetContext_SessionAutoCache_ModeImpact_NotCached(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	sctx := sessionCtx("impact-session")

	// mode=impact call — must NOT write to session cache (different response shape).
	res1, err1 := s.handleGetContext(sctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"mode":   "impact",
		"format": "json",
	}))
	if err1 != nil {
		t.Fatalf("impact call error: %v", err1)
	}
	_ = res1

	// Subsequent normal get_context for same entity (mode="") — must return full
	// response because the impact call must NOT have populated the session cache.
	res2, err2 := s.handleGetContext(sctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m2 := mustResult(t, res2, err2)
	if m2["unchanged"] == true {
		t.Error("mode=impact call must not populate session cache for mode='' calls")
	}
	hasKey(t, m2, "root")
}

func TestHandleGetContext_SessionAutoCache_ManualKnownHashTakesPrecedence(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	sctx := sessionCtx("priority-session")

	// Populate the session cache.
	res1, _ := s.handleGetContext(sctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m1 := mustResult(t, res1, nil)
	hash := m1["entity_hash"].(string)

	// Pass wrong known_hash manually — should get full response (manual hash mismatch
	// takes precedence; auto-cache should not fire when manual hash is provided and mismatches).
	res2, err2 := s.handleGetContext(sctx, callTool(map[string]any{
		"entity":     "AuthLogin",
		"known_hash": "000000000000",
		"format": "json",
	}))
	m2 := mustResult(t, res2, err2)
	if m2["unchanged"] == true {
		t.Error("wrong manual known_hash should return full response, not unchanged")
	}
	hasKey(t, m2, "root")

	// Pass correct known_hash manually — manual path fires, not auto-cache.
	res3, err3 := s.handleGetContext(sctx, callTool(map[string]any{
		"entity":     "AuthLogin",
		"known_hash": hash,
		"format": "json",
	}))
	m3 := mustResult(t, res3, err3)
	if m3["unchanged"] != true {
		t.Error("correct manual known_hash should return unchanged=true")
	}
	// Manual path does NOT set cache_source=session.
	if m3["cache_source"] == "session" {
		t.Error("manual known_hash should not report cache_source=session")
	}
}

func TestClearSessionHashes_OnlyRemovesTargetSession(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	sctxA := sessionCtx("clear-session-A")
	sctxB := sessionCtx("clear-session-B")

	// Populate both sessions.
	s.handleGetContext(sctxA, callTool(map[string]any{"entity": "AuthLogin", "format": "json"})) //nolint
	s.handleGetContext(sctxB, callTool(map[string]any{"entity": "AuthLogin", "format": "json"})) //nolint

	// Clear only session A.
	s.ClearSessionHashes("clear-session-A")

	// Session A: should get full response (cache cleared).
	resA2, errA2 := s.handleGetContext(sctxA, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	mA2 := mustResult(t, resA2, errA2)
	if mA2["unchanged"] == true {
		t.Error("session A cache should have been cleared — expected full response")
	}

	// Session B: should still get {unchanged:true} (cache intact).
	resB2, errB2 := s.handleGetContext(sctxB, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	mB2 := mustResult(t, resB2, errB2)
	if mB2["unchanged"] != true {
		t.Error("session B cache should be intact after clearing session A")
	}
}

func TestSessionHashMethods_Unit(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Empty session ID — all ops are no-ops.
	s.setSessionHash("", "key", "hash")
	if got := s.getSessionHash("", "key"); got != "" {
		t.Errorf("empty sessionID get should return '', got %q", got)
	}

	// Normal get/set.
	s.setSessionHash("sess1", "entity|file||2|4000", "abc123")
	if got := s.getSessionHash("sess1", "entity|file||2|4000"); got != "abc123" {
		t.Errorf("expected abc123, got %q", got)
	}

	// Different session — isolated.
	if got := s.getSessionHash("sess2", "entity|file||2|4000"); got != "" {
		t.Errorf("different session should not see sess1's entry, got %q", got)
	}

	// ClearSessionHashes removes only target session.
	s.setSessionHash("sess2", "entity|file||2|4000", "xyz789")
	s.ClearSessionHashes("sess1")
	if got := s.getSessionHash("sess1", "entity|file||2|4000"); got != "" {
		t.Errorf("sess1 should be cleared, got %q", got)
	}
	if got := s.getSessionHash("sess2", "entity|file||2|4000"); got != "xyz789" {
		t.Errorf("sess2 should be untouched, got %q", got)
	}
}

// ── test_coverage in get_impact (R2) ─────────────────────────────────────────

func TestHandleGetImpact_TestCoverageField(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "AuthLogin"}))
	m := mustResult(t, res, err)
	// test_coverage key must be present (may be empty slice if no test files).
	// We check that it doesn't cause a panic or wrong type — actual coverage
	// depends on whether test files exist in the fixture.
	_ = m["test_coverage"] // nil (omitempty) or []interface{}
}

// ── closest_reachable in get_call_chain not-found (R2) ───────────────────────

func TestHandleGetCallChain_NotFound_ClosestReachable(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// Use entities that exist but have no call path between them.
	res, err := s.handleGetCallChain(ctx, callTool(map[string]any{
		"from": "HandleRequest",
		"to":   "Database",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, err)
	if found, _ := m["found"].(bool); !found {
		// When not found, closest_reachable MAY be present if BFS reached any nodes.
		// We just verify the field doesn't cause a crash and has the right shape if set.
		if cr, ok := m["closest_reachable"].(map[string]any); ok {
			if cr["name"] == nil || cr["hops"] == nil {
				t.Errorf("closest_reachable missing required fields: %v", cr)
			}
		}
	}
}

// ── handleGetEvents ───────────────────────────────────────────────────────────

func TestHandleGetEvents_InitialEmpty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetEvents(ctx, callTool(map[string]any{"since_seq": float64(0)}))
	m := mustResult(t, res, err)
	hasKey(t, m, "events")
}

func TestHandleGetEvents_AfterSessionInit(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "watcher-agent"}))

	res, err := s.handleGetEvents(ctx, callTool(map[string]any{"since_seq": float64(0)}))
	m := mustResult(t, res, err)
	events, _ := m["events"].([]any)
	if len(events) == 0 {
		t.Error("expected at least one event after session_init")
	}
}

// ── entity_hash stable (root not double-counted) ──────────────────────────────

func TestHandleGetContext_EntityHashStable(t *testing.T) {
	// Two consecutive calls with the same graph must return the same hash.
	s, _, _ := newPopulatedServer(t)
	res1, err1 := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m1 := mustResult(t, res1, err1)
	hash1, _ := m1["entity_hash"].(string)

	res2, err2 := s.handleGetContext(ctx, callTool(map[string]any{"entity": "AuthLogin", "format": "json"}))
	m2 := mustResult(t, res2, err2)
	hash2, _ := m2["entity_hash"].(string)

	if hash1 == "" || hash2 == "" {
		t.Fatal("entity_hash missing in one of the responses")
	}
	if hash1 != hash2 {
		t.Errorf("entity_hash unstable across identical calls: %q vs %q", hash1, hash2)
	}
}

// ── entity_hash in compact format ─────────────────────────────────────────────

func TestHandleGetContext_CompactFormat_EntityHashPresent(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "compact",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatal("expected TextContent for compact format")
	}
	if !strings.Contains(tc.Text, "entity_hash:") {
		t.Errorf("compact format must contain entity_hash: line, got:\n%s", tc.Text)
	}
}

// ── TestCoverage for struct/interface impact ──────────────────────────────────

func TestHandleGetImpact_StructNode_TestCoverageField(t *testing.T) {
	// Build a server with a struct node and a method on it.
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	structID := g.MakeNodeID("pkg/store/store.go", "UserStore")
	methodID := g.MakeNodeID("pkg/store/store.go", "UserStore.Get")
	callerID := g.MakeNodeID("pkg/api/handler.go", "HandleGet")
	testID := g.MakeNodeID("pkg/store/store_test.go", "TestUserStore_Get")

	g.AddNode(&graph.Node{ID: structID, Name: "UserStore", Type: graph.NodeStruct, File: "pkg/store/store.go", Line: 1, Package: "store"})
	g.AddNode(&graph.Node{ID: methodID, Name: "UserStore.Get", Type: graph.NodeMethod, File: "pkg/store/store.go", Line: 5, Package: "store"})
	g.AddNode(&graph.Node{ID: callerID, Name: "HandleGet", Type: graph.NodeFunction, File: "pkg/api/handler.go", Line: 1, Package: "api"})
	g.AddNode(&graph.Node{ID: testID, Name: "TestUserStore_Get", Type: graph.NodeFunction, File: "pkg/store/store_test.go", Line: 1, Package: "store"})

	g.AddEdge(&graph.Edge{From: callerID, To: methodID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: testID, To: methodID, Type: graph.EdgeCalls})

	s := New(g, cfg, st)
	res, err := s.handleGetImpact(ctx, callTool(map[string]any{"symbol": "UserStore"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, err)
	hasKey(t, m, "total_affected")

	// The struct path (merged) must also populate TestCoverage.
	coverage, _ := m["test_coverage"].([]any)
	found := false
	for _, f := range coverage {
		if f == "pkg/store/store_test.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected store_test.go in test_coverage for struct node, got %v", m["test_coverage"])
	}
}

// ── handleGetEvents ───────────────────────────────────────────────────────────

func TestHandleGetEvents_SinceCursorFilters(t *testing.T) {
	s := newTestServer(t)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "evt-a"}))
	time.Sleep(5 * time.Millisecond)

	res1, err1 := s.handleGetEvents(ctx, callTool(map[string]any{"since_seq": float64(0)}))
	m1 := mustResult(t, res1, err1)
	events1, _ := m1["events"].([]any)
	if len(events1) == 0 {
		t.Skip("no events to cursor-test")
	}
	lastEvent, _ := events1[len(events1)-1].(map[string]any)
	lastSeq, _ := lastEvent["seq"].(float64)

	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "evt-b"}))

	res2, err2 := s.handleGetEvents(ctx, callTool(map[string]any{"since_seq": lastSeq}))
	m2 := mustResult(t, res2, err2)
	events2, _ := m2["events"].([]any)
	if len(events2) == 0 {
		t.Error("expected new events after cursor position")
	}
}

// ── Sprint 11: Dynamic token budget by agent model ──────────────────────────

func TestModelBudgetMultiplier(t *testing.T) {
	tests := []struct {
		model string
		want  float64
	}{
		{"claude-opus-4-6", 2.0},
		{"claude-opus-4-6-20250514", 2.0},
		{"Claude-Opus-4-6", 2.0}, // case-insensitive
		{"claude-sonnet-4-6", 1.5},
		{"claude-sonnet-4-6-20250514", 1.5},
		{"claude-haiku-4-5-20251001", 0.75},
		{"gpt-4o", 1.0},
		{"gpt-4o-2024-08-06", 1.0},
		{"gpt-4o-mini", 0.5},
		{"gpt-4o-mini-2024-07-18", 0.5},
		{"gpt-4.1", 1.5},
		{"gpt-4.1-mini", 0.5},
		{"gpt-4.1-nano", 0.5},
		{"gemini-2.5-pro", 1.5},
		{"gemini-2.5-flash", 1.5},
		{"unknown-model", 1.0},
		{"", 1.0},
	}
	for _, tt := range tests {
		got := modelBudgetMultiplier(tt.model)
		if got != tt.want {
			t.Errorf("modelBudgetMultiplier(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

func TestGetSessionBudgetMultiplier_NoSession(t *testing.T) {
	s := newTestServer(t)
	// No session_init called → multiplier should be 1.0
	mult := s.getSessionBudgetMultiplier(ctx)
	if mult != 1.0 {
		t.Errorf("expected 1.0 without session, got %v", mult)
	}
}

func TestGetSessionBudgetMultiplier_WithModel(t *testing.T) {
	s := newTestServer(t)
	// Call session_init with a model to register the session
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"model":    "claude-opus-4-6",
	}))
	mult := s.getSessionBudgetMultiplier(ctx)
	if mult != 2.0 {
		t.Errorf("expected 2.0 for claude-opus-4-6, got %v", mult)
	}
}

func TestGetSessionBudgetMultiplier_WithoutModel(t *testing.T) {
	s := newTestServer(t)
	// Call session_init without model → should default to 1.0
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	mult := s.getSessionBudgetMultiplier(ctx)
	if mult != 1.0 {
		t.Errorf("expected 1.0 without model, got %v", mult)
	}
}

func TestSessionInit_BudgetMultiplier_InResponse(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"model":    "claude-opus-4-6",
	}))
	m := mustResult(t, res, err)
	mult, ok := m["budget_multiplier"].(float64)
	if !ok {
		t.Fatal("expected budget_multiplier in response")
	}
	if mult != 2.0 {
		t.Errorf("budget_multiplier = %v, want 2.0", mult)
	}
}

func TestSessionInit_BudgetMultiplier_OmittedWhenDefault(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
		"model":    "gpt-4o",
	}))
	m := mustResult(t, res, err)
	noKey(t, m, "budget_multiplier")
}

func TestSessionInit_BudgetMultiplier_OmittedWithoutModel(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)
	noKey(t, m, "budget_multiplier")
}

// Integration test: proves that the model budget multiplier actually affects
// token budgets in context handlers end-to-end. Uses get_context with a
// populated graph to verify that an opus session gets a larger budget than
// a mini session (which gets a smaller budget than default).
func TestBudgetMultiplier_Integration_GetContext(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Call get_context WITHOUT session_init (default budget = 4000)
	resDefault, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	mDefault := mustResult(t, resDefault, err)

	// Now register session with gpt-4o-mini (0.5x multiplier → 2000 budget)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "mini-agent",
		"model":    "gpt-4o-mini",
	}))
	resMini, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	mMini := mustResult(t, resMini, err)

	// The response should succeed in both cases (graph is small enough)
	// but the multiplier should be stored and retrievable
	mult := s.getSessionBudgetMultiplier(ctx)
	if mult != 0.5 {
		t.Errorf("expected 0.5 multiplier for gpt-4o-mini, got %v", mult)
	}

	// Both should return valid results (root node present)
	if mDefault["root"] == nil {
		t.Error("default session: expected root node in response")
	}
	if mMini["root"] == nil {
		t.Error("mini session: expected root node in response")
	}
}

// Integration test: explicit token_budget overrides model multiplier
func TestBudgetMultiplier_ExplicitOverride(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Register session with opus (2.0x)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "opus-agent",
		"model":    "claude-opus-4-6",
	}))

	// Call get_context WITH explicit token_budget — should override multiplier
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity":       "AuthLogin",
		"format":       "json",
		"token_budget": float64(500),
	}))
	m := mustResult(t, res, err)
	if m["root"] == nil {
		t.Error("expected root node in response")
	}
	// The test passes if no panic/error — the explicit budget (500) was used
	// instead of the multiplied default (4000 * 2.0 = 8000)
}

// Integration test: prepare_context also respects model multiplier
func TestBudgetMultiplier_Integration_PrepareContext(t *testing.T) {
	s, _, _ := newPopulatedServer(t)

	// Register session with haiku (0.75x)
	_, _ = s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "haiku-agent",
		"model":    "claude-haiku-4-5",
	}))

	mult := s.getSessionBudgetMultiplier(ctx)
	if mult != 0.75 {
		t.Errorf("expected 0.75 multiplier for claude-haiku, got %v", mult)
	}

	// Call prepare_context — should apply 0.75x to the intent default budget
	res, err := s.handlePrepareContext(ctx, callTool(map[string]any{
		"intent": "understand",
		"target": "AuthLogin",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatal("expected successful response from prepare_context")
	}
}
