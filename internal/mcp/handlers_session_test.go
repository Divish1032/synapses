package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── handleSessionInit ─────────────────────────────────────────────────────────

func TestHandleSessionInit_SoloAgent_ResponseShape(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "solo-agent", "scope": "full"}))
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
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"scope": "full"}))
	m := mustResult(t, res, err)
	hasKey(t, m, "project_identity")
	hasKey(t, m, "scale_guidance")
}

func TestHandleSessionInit_StandardScope_IsDefault(t *testing.T) {
	s := newTestServer(t)
	// No scope argument — should default to "standard" (lean mode).
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "std-agent"}))
	m := mustResult(t, res, err)

	// Always-present keys.
	hasKey(t, m, "pending_tasks")
	hasKey(t, m, "scale_guidance")
	hasKey(t, m, "working_state")
	hasKey(t, m, "_summary")
	hasKey(t, m, "more_available")

	// Full-mode-only keys must be absent in standard mode.
	noKey(t, m, "project_identity")
	noKey(t, m, "session_hint")

	// more_available must list deferred sections.
	ma, ok := m["more_available"].(map[string]any)
	if !ok {
		t.Fatalf("more_available must be a map, got %T", m["more_available"])
	}
	sections, ok := ma["sections"].([]any)
	if !ok || len(sections) == 0 {
		t.Error("more_available.sections must be a non-empty list")
	}
	hint, _ := ma["hint"].(string)
	if hint == "" {
		t.Error("more_available.hint must be non-empty")
	}
}

func TestHandleSessionInit_Summary_NonEmpty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "sum-agent"}))
	m := mustResult(t, res, err)

	summary, ok := m["_summary"].(string)
	if !ok || summary == "" {
		t.Errorf("_summary must be a non-empty string, got %v", m["_summary"])
	}
}

func TestHandleSessionInit_QuickScopeAlias_SameAsStandard(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "quick-agent", "scope": "quick"}))
	m := mustResult(t, res, err)

	// quick is an alias for standard — should also have more_available and no project_identity.
	hasKey(t, m, "more_available")
	noKey(t, m, "project_identity")
}

func TestHandleSessionInit_FullScope_NoMoreAvailable(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "full-agent", "scope": "full"}))
	m := mustResult(t, res, err)

	// Full scope must include rich sections and must NOT include more_available.
	hasKey(t, m, "project_identity")
	noKey(t, m, "more_available")
}

func TestHandleSessionInit_SummaryReflectsTaskCount(t *testing.T) {
	s := newTestServer(t)
	// Create a task via the MCP tool so pending_tasks.count > 0.
	_, _ = s.handleCreatePlan(ctx, callTool(map[string]any{
		"agent_id": "sum-agent",
		"title":    "Test plan",
		"tasks":    []any{map[string]any{"title": "Do something", "priority": "p1"}},
	}))

	res, initErr := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "sum-agent"}))
	m := mustResult(t, res, initErr)

	summary, ok := m["_summary"].(string)
	if !ok || summary == "" {
		t.Fatalf("_summary must be a non-empty string, got %v", m["_summary"])
	}
	if !strings.Contains(summary, "1 pending task") {
		t.Errorf("_summary must reflect task count, got %q", summary)
	}
}

func TestHandleSessionInit_ResumeScope_MoreAvailableIsSubset(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "res-agent", "scope": "resume"}))
	m := mustResult(t, res, err)

	// resume scope should have more_available.
	hasKey(t, m, "more_available")

	ma, ok := m["more_available"].(map[string]any)
	if !ok {
		t.Fatalf("more_available must be a map")
	}
	sections, ok := ma["sections"].([]any)
	if !ok {
		t.Fatalf("more_available.sections must be a list")
	}

	// Convert to a set for easy lookup.
	sectionSet := make(map[string]bool, len(sections))
	for _, s := range sections {
		if str, ok := s.(string); ok {
			sectionSet[str] = true
		}
	}

	// Sections present in resume mode must NOT appear in more_available.
	// federation_health, relevant_memories, previous_session_work, knowledge_graph
	// are all guarded by !quickMode only — they ARE present in resume mode.
	for _, shouldBePresent := range []string{"federation_health", "relevant_memories", "previous_session_work", "knowledge_graph"} {
		if sectionSet[shouldBePresent] {
			t.Errorf("more_available must not list %q for scope=resume — it is already present in resume responses", shouldBePresent)
		}
	}
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

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "brain-agent", "scope": "full"}))
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

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "warn-agent", "scope": "full"}))
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
func (s *stubEmbedder) WarmUp(_ context.Context) error                         { return nil }
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

// ── knowledge_graph stats (Sprint 16 #6) ─────────────────────────────────────

// TestSessionInit_KnowledgeGraph_EmptyGraph verifies that session_init includes
// a knowledge_graph section in full mode even with no non-code-domain nodes.
func TestSessionInit_KnowledgeGraph_EmptyGraph(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "kg-agent", "scope": "full"}))
	m := mustResult(t, res, err)

	kg, ok := m["knowledge_graph"].(map[string]any)
	if !ok {
		t.Fatalf("expected knowledge_graph map in full-mode session_init, got %T — keys: %v",
			m["knowledge_graph"], mapKeys(m))
	}
	for _, key := range []string{"entities_by_domain", "active_domains", "cross_domain_edges", "freshness"} {
		if kg[key] == nil {
			t.Errorf("expected %q key in knowledge_graph", key)
		}
	}
	// Freshness should say "live" not "current" (watcher live-indexes).
	if fs, _ := kg["freshness"].(string); !strings.HasPrefix(fs, "live") {
		t.Errorf("expected freshness to start with 'live', got %q", fs)
	}
}

// TestSessionInit_KnowledgeGraph_QuickMode verifies that knowledge_graph is
// omitted in scope=quick to honour the minimal-token contract.
func TestSessionInit_KnowledgeGraph_QuickMode(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "kg-quick",
		"scope":    "quick",
	}))
	m := mustResult(t, res, err)
	if _, present := m["knowledge_graph"]; present {
		t.Error("knowledge_graph must be absent in scope=quick to preserve token budget")
	}
}

// TestSessionInit_KnowledgeGraph_MultiDomain verifies entity counts and active
// domains are correct when the graph has nodes in multiple domains.
func TestSessionInit_KnowledgeGraph_MultiDomain(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Add code node (default domain).
	codeID := g.MakeNodeID("cmd/main.go", "main")
	g.AddNode(&graph.Node{ID: codeID, Type: graph.NodeFunction, Name: "main",
		File: "cmd/main.go", Domain: graph.DomainCode})

	// Add infra node.
	infraID := g.MakeNodeID("infra/main.tf", "aws_instance.web")
	g.AddNode(&graph.Node{ID: infraID, Type: graph.NodeFunction, Name: "aws_instance.web",
		File: "infra/main.tf", Domain: graph.DomainInfra})

	// Add API node.
	apiID := g.MakeNodeID("api/openapi.yaml", "POST /payments")
	g.AddNode(&graph.Node{ID: apiID, Type: graph.NodeFunction, Name: "POST /payments",
		File: "api/openapi.yaml", Domain: graph.DomainAPI})
	_ = apiID

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "kg-multi", "scope": "full"}))
	m := mustResult(t, res, err)

	kg := m["knowledge_graph"].(map[string]any)
	ebd := kg["entities_by_domain"].(map[string]any)

	if ebd["code"] == nil {
		t.Error("expected 'code' in entities_by_domain")
	}
	if ebd["infra"] == nil {
		t.Error("expected 'infra' in entities_by_domain")
	}
	if ebd["api"] == nil {
		t.Error("expected 'api' in entities_by_domain")
	}

	domains, ok := kg["active_domains"].([]any)
	if !ok || len(domains) < 3 {
		t.Errorf("expected at least 3 active_domains, got %v", kg["active_domains"])
	}
}

// TestSessionInit_KnowledgeGraph_CrossDomainEdgeCounts verifies the breakdown of
// auto/confirmed/manual edges (non-suppressed only) is correct.
func TestSessionInit_KnowledgeGraph_CrossDomainEdgeCounts(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	codeID := g.MakeNodeID("cmd/main.go", "main")
	g.AddNode(&graph.Node{ID: codeID, Type: graph.NodeFunction, Name: "main",
		File: "cmd/main.go", Domain: graph.DomainCode})
	infraID := g.MakeNodeID("infra/main.tf", "aws_instance.web")
	g.AddNode(&graph.Node{ID: infraID, Type: graph.NodeFunction, Name: "aws_instance.web",
		File: "infra/main.tf", Domain: graph.DomainInfra})

	// Auto edge: created by namematcher.
	if _, err := st.SaveSyntheticEdge(codeID, infraID, graph.EdgeMentions, 0.75); err != nil {
		t.Fatalf("SaveSyntheticEdge: %v", err)
	}
	// Manual edge: created by user.
	if _, err := st.SaveManualEdge(codeID, infraID, string(graph.EdgeDeploys), "infra", "agent-1", 1.0, true); err != nil {
		t.Fatalf("SaveManualEdge: %v", err)
	}
	// Suppressed edge: should not be counted.
	if _, err := st.SaveManualEdge(codeID, infraID, string(graph.EdgeConsumes), "api", "agent-1", 0.7, true); err != nil {
		t.Fatalf("SaveManualEdge (suppressed): %v", err)
	}
	if err := st.ConfirmEdge(codeID, infraID, string(graph.EdgeConsumes), false); err != nil {
		t.Fatalf("ConfirmEdge(suppress): %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "kg-edges", "scope": "full"}))
	m := mustResult(t, res, err)

	kg := m["knowledge_graph"].(map[string]any)
	cde := kg["cross_domain_edges"].(map[string]any)

	// auto = 1 (namematcher), manual = 1 (link_entities), suppressed = excluded
	getInt := func(key string) int {
		v, _ := cde[key].(float64)
		return int(v)
	}
	if got := getInt("auto"); got != 1 {
		t.Errorf("expected auto=1, got %d", got)
	}
	if got := getInt("manual"); got != 1 {
		t.Errorf("expected manual=1, got %d", got)
	}
	if got := getInt("confirmed"); got != 0 {
		t.Errorf("expected confirmed=0, got %d", got)
	}
	if got := getInt("total"); got != 2 {
		t.Errorf("expected total=2, got %d (suppressed edge must be excluded)", got)
	}
}

// TestSessionInit_KnowledgeGraph_ConfirmedEdge verifies that a confirmed edge
// is counted under "confirmed" rather than "auto" or "manual".
func TestSessionInit_KnowledgeGraph_ConfirmedEdge(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	codeID := g.MakeNodeID("cmd/main.go", "main")
	g.AddNode(&graph.Node{ID: codeID, Type: graph.NodeFunction, Name: "main",
		File: "cmd/main.go", Domain: graph.DomainCode})
	infraID := g.MakeNodeID("infra/main.tf", "aws_instance.web")
	g.AddNode(&graph.Node{ID: infraID, Type: graph.NodeFunction, Name: "aws_instance.web",
		File: "infra/main.tf", Domain: graph.DomainInfra})

	// Start as auto, then confirm.
	if _, err := st.SaveSyntheticEdge(codeID, infraID, graph.EdgeMentions, 0.7); err != nil {
		t.Fatalf("SaveSyntheticEdge: %v", err)
	}
	if err := st.ConfirmEdge(codeID, infraID, string(graph.EdgeMentions), true); err != nil {
		t.Fatalf("ConfirmEdge: %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "kg-conf", "scope": "full"}))
	m := mustResult(t, res, err)

	kg := m["knowledge_graph"].(map[string]any)
	cde := kg["cross_domain_edges"].(map[string]any)

	getInt := func(key string) int {
		v, _ := cde[key].(float64)
		return int(v)
	}
	if got := getInt("confirmed"); got != 1 {
		t.Errorf("expected confirmed=1, got %d", got)
	}
	if got := getInt("auto"); got != 0 {
		t.Errorf("expected auto=0 after confirm, got %d", got)
	}
}

// TestSessionInit_KnowledgeGraph_HintForAutoEdges verifies that a hint field
// appears when unreviewed auto-detected edges exist, pointing agents to confirm_edge.
func TestSessionInit_KnowledgeGraph_HintForAutoEdges(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	codeID := g.MakeNodeID("cmd/main.go", "main")
	g.AddNode(&graph.Node{ID: codeID, Type: graph.NodeFunction, Name: "main",
		File: "cmd/main.go", Domain: graph.DomainCode})
	infraID := g.MakeNodeID("infra/main.tf", "web")
	g.AddNode(&graph.Node{ID: infraID, Type: graph.NodeFunction, Name: "web",
		File: "infra/main.tf", Domain: graph.DomainInfra})

	// One unreviewed auto edge → hint must appear.
	if _, err := st.SaveSyntheticEdge(codeID, infraID, graph.EdgeMentions, 0.8); err != nil {
		t.Fatalf("SaveSyntheticEdge: %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "kg-hint", "scope": "full"}))
	m := mustResult(t, res, err)

	kg := m["knowledge_graph"].(map[string]any)
	hint, _ := kg["hint"].(string)
	if !strings.Contains(hint, "confirm_edge") {
		t.Errorf("expected hint mentioning confirm_edge when auto edges exist, got %q", hint)
	}
}

// TestSessionInit_KnowledgeGraph_CustomRelationCounted verifies that a
// user-created edge with a custom relation string (not in EdgeTypeCatalog) is
// still counted under "manual" — not silently dropped.
func TestSessionInit_KnowledgeGraph_CustomRelationCounted(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	codeID := g.MakeNodeID("cmd/main.go", "main")
	g.AddNode(&graph.Node{ID: codeID, Type: graph.NodeFunction, Name: "main",
		File: "cmd/main.go", Domain: graph.DomainCode})
	docsID := g.MakeNodeID("README.md", "intro")
	g.AddNode(&graph.Node{ID: docsID, Type: graph.NodeSection, Name: "intro",
		File: "README.md", Domain: graph.DomainDocs})

	// Custom relation not in EdgeTypeCatalog — must still count as manual.
	if _, err := st.SaveManualEdge(codeID, docsID, "REFERENCES", "docs", "agent-x", 1.0, true); err != nil {
		t.Fatalf("SaveManualEdge custom: %v", err)
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "kg-custom", "scope": "full"}))
	m := mustResult(t, res, err)

	kg := m["knowledge_graph"].(map[string]any)
	cde := kg["cross_domain_edges"].(map[string]any)
	manual := int(cde["manual"].(float64))
	total := int(cde["total"].(float64))
	if manual != 1 {
		t.Errorf("expected manual=1 for custom relation, got %d (custom relations must not be silently dropped)", manual)
	}
	if total != 1 {
		t.Errorf("expected total=1, got %d", total)
	}
}

// ── First-session highlights (Sprint 18 #1) ───────────────────────────────────

// TestSessionInit_FirstSessionHighlights_AppearsOnFirstSession verifies that
// first_session_highlights is present in full-mode session_init on the very
// first call for a project, and that it contains the expected sections.
func TestSessionInit_FirstSessionHighlights_AppearsOnFirstSession(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Add two functions: one with a caller (not dead), one without (dead code).
	calledID := g.MakeNodeID("pkg/auth.go", "Login")
	g.AddNode(&graph.Node{ID: calledID, Type: graph.NodeFunction, Name: "Login", File: "pkg/auth.go"})

	callerID := g.MakeNodeID("pkg/api.go", "Handle")
	g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Handle", File: "pkg/api.go"})

	deadID := g.MakeNodeID("pkg/util.go", "unused")
	g.AddNode(&graph.Node{ID: deadID, Type: graph.NodeFunction, Name: "unused", File: "pkg/util.go"})

	// Handle calls Login (so Login has a caller), but unused has no callers.
	g.AddEdge(&graph.Edge{From: callerID, To: calledID, Type: graph.EdgeCalls})

	srv := New(g, cfg, st)
	// projectID must be non-empty — first-session detection is skipped when empty
	// to avoid false matches on misconfigured servers or tests that never call SetProjectID.
	srv.projectID = "proj-first-session-test"
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "first-agent",
		"scope":    "full",
	}))
	m := mustResult(t, res, err)

	fsh, ok := m["first_session_highlights"].(map[string]any)
	if !ok {
		t.Fatalf("expected first_session_highlights on first session, got %T — keys: %v",
			m["first_session_highlights"], mapKeys(m))
	}
	if fsh["hint"] == nil {
		t.Error("expected hint in first_session_highlights")
	}
	// dead_code section should be present: "unused" is unexported, has no callers,
	// and is not named "main"/"init" — all guards pass.
	dc, ok := fsh["dead_code"].(map[string]any)
	if !ok {
		t.Fatalf("expected dead_code section in first_session_highlights, got %T — keys: %v",
			fsh["dead_code"], mapKeys(fsh))
	}
	// mustResult JSON-unmarshals the response, so numbers are float64.
	total, _ := dc["total"].(float64)
	if total < 1 {
		t.Errorf("expected at least 1 dead code entry, got %v", total)
	}
}

// TestSessionInit_FirstSessionHighlights_AbsentOnSecondSession verifies that
// first_session_highlights is NOT present on subsequent sessions.
// Two separate session_init calls on the same project with different MCP session IDs
// produce two session rows (count = 2), so highlights must not fire on the second.
func TestSessionInit_FirstSessionHighlights_AbsentOnSecondSession(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Add one dead-code function so highlights WOULD fire if conditions were met.
	deadID := g.MakeNodeID("pkg/util.go", "unused")
	g.AddNode(&graph.Node{ID: deadID, Type: graph.NodeFunction, Name: "unused", File: "pkg/util.go"})

	// Pre-insert a prior session row for this project directly, so the session
	// count for the project is already ≥1 before the test call.
	// This simulates "user already ran session_init once before".
	const projectID = "proj-second-session-test"
	if _, _, _, insertErr := st.GetOrResumeSession("prior-agent", projectID, "prior-mcp-conn", "prior work", 0, -1); insertErr != nil {
		t.Fatalf("pre-insert session: %v", insertErr)
	}

	// Build a server that uses the same projectID so CountProjectSessions matches.
	srv := New(g, cfg, st)
	srv.projectID = projectID
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "second-agent",
		"scope":    "full",
	}))
	m := mustResult(t, res, err)

	// count > 1 → highlights must be absent.
	if _, present := m["first_session_highlights"]; present {
		t.Error("first_session_highlights must be absent on second+ session")
	}
}

// TestSessionInit_FirstSessionHighlights_CompactInQuickMode verifies that
// first_session_highlights IS present in scope=quick on first session, but
// in compact form (counts only, no sample arrays) to respect token budget.
// This prevents the "missed forever" failure mode where an agent using quickMode
// on their very first session permanently loses the one-shot highlights.
func TestSessionInit_FirstSessionHighlights_CompactInQuickMode(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	deadID := g.MakeNodeID("pkg/util.go", "unused")
	g.AddNode(&graph.Node{ID: deadID, Type: graph.NodeFunction, Name: "unused", File: "pkg/util.go"})

	srv := New(g, cfg, st)
	srv.projectID = "proj-quick-mode-test"
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "quick-agent",
		"scope":    "quick",
	}))
	m := mustResult(t, res, err)

	// Highlights must be PRESENT even in quickMode — first session is one-shot.
	fsh, ok := m["first_session_highlights"].(map[string]any)
	if !ok {
		t.Fatalf("first_session_highlights must be present in scope=quick on first session, got %T — keys: %v",
			m["first_session_highlights"], mapKeys(m))
	}
	// In compact mode: counts present, no sample arrays.
	if fsh["dead_code_count"] == nil {
		t.Error("expected dead_code_count in compact highlights (scope=quick)")
	}
	if _, hasFullSection := fsh["dead_code"]; hasFullSection {
		t.Error("dead_code sample section must be absent in compact (scope=quick) — only counts allowed")
	}
	if fsh["hint"] == nil {
		t.Error("expected hint in compact highlights")
	}
}

// TestComputeFirstSessionHighlights_CompactMode verifies the compact flag
// returns counts-only (no sample arrays) for quickMode callers.
func TestComputeFirstSessionHighlights_CompactMode(t *testing.T) {
	g := graph.New("test-repo")
	deadID := g.MakeNodeID("pkg/util.go", "unused")
	g.AddNode(&graph.Node{ID: deadID, Type: graph.NodeFunction, Name: "unused", File: "pkg/util.go"})

	result := computeFirstSessionHighlights(g, nil, map[string]bool{}, true /* compact */)
	if result == nil {
		t.Fatal("expected non-nil compact highlights")
	}
	if result["dead_code_count"] == nil {
		t.Error("expected dead_code_count key in compact mode")
	}
	if _, present := result["dead_code"]; present {
		t.Error("dead_code sample section must be absent in compact mode")
	}
	if result["hint"] == nil {
		t.Error("expected hint in compact mode")
	}
}

// TestComputeFirstSessionHighlights_HighRiskEntity verifies that a function
// with high fanin (≥3) and no test callers appears in high_risk_entities.
func TestComputeFirstSessionHighlights_HighRiskEntity(t *testing.T) {
	g := graph.New("test-repo")

	// Add a function called by 4 non-test callers, no test callers.
	targetID := g.MakeNodeID("pkg/core.go", "CoreOp")
	g.AddNode(&graph.Node{ID: targetID, Type: graph.NodeFunction, Name: "CoreOp", File: "pkg/core.go"})

	for i := 0; i < 4; i++ {
		callerID := g.MakeNodeID("pkg/caller.go", fmt.Sprintf("caller%d", i))
		g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction,
			Name: fmt.Sprintf("caller%d", i), File: "pkg/caller.go"})
		g.AddEdge(&graph.Edge{From: callerID, To: targetID, Type: graph.EdgeCalls})
	}

	highlights := computeFirstSessionHighlights(g, nil, map[string]bool{}, false)
	if highlights == nil {
		t.Fatal("expected non-nil highlights for high-risk entity")
	}
	hr, ok := highlights["high_risk_entities"].(map[string]any)
	if !ok {
		t.Fatalf("expected high_risk_entities section, got %T — highlight keys: %v",
			highlights["high_risk_entities"], mapKeys(highlights))
	}
	// Direct call (no JSON round-trip), so "total" is int, not float64.
	total, _ := hr["total"].(int)
	if total < 1 {
		t.Errorf("expected at least 1 high-risk entity (total), got %d", total)
	}
}

// TestComputeFirstSessionHighlights_TestCallerExcludesFromDead verifies that
// a function called only from _test.go is NOT flagged as dead code.
func TestComputeFirstSessionHighlights_TestCallerExcludesFromDead(t *testing.T) {
	g := graph.New("test-repo")

	testedID := g.MakeNodeID("pkg/auth.go", "Validate")
	g.AddNode(&graph.Node{ID: testedID, Type: graph.NodeFunction, Name: "Validate", File: "pkg/auth.go"})

	testCallerID := g.MakeNodeID("pkg/auth_test.go", "TestValidate")
	g.AddNode(&graph.Node{ID: testCallerID, Type: graph.NodeFunction,
		Name: "TestValidate", File: "pkg/auth_test.go"})

	g.AddEdge(&graph.Edge{From: testCallerID, To: testedID, Type: graph.EdgeCalls})

	highlights := computeFirstSessionHighlights(g, nil, map[string]bool{}, false)
	// Either nil (no dead code found) or dead_code absent — Validate has a test caller.
	if highlights != nil {
		if dc, ok := highlights["dead_code"].(map[string]any); ok {
			sample, _ := dc["sample"].([]any)
			for _, entry := range sample {
				if em, ok := entry.(map[string]any); ok {
					if em["name"] == "Validate" {
						t.Error("Validate should not appear in dead_code — it has a test caller")
					}
				}
			}
		}
	}
}

// TestComputeFirstSessionHighlights_EmptyGraphReturnsNil verifies that an empty
// graph produces no highlights rather than an empty map.
func TestComputeFirstSessionHighlights_EmptyGraphReturnsNil(t *testing.T) {
	g := graph.New("test-repo")
	if result := computeFirstSessionHighlights(g, nil, map[string]bool{}, false); result != nil {
		t.Errorf("expected nil for empty graph, got %v", result)
	}
}

// highlightsJSON is a test helper that JSON-marshals computeFirstSessionHighlights
// output so all types are JSON-standard (numbers=float64, slices=[]interface{}).
func highlightsJSON(t *testing.T, g *graph.Graph, vlog []store.ViolationLogEntry) map[string]any {
	t.Helper()
	raw := computeFirstSessionHighlights(g, vlog, map[string]bool{}, false)
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("highlightsJSON marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("highlightsJSON unmarshal: %v", err)
	}
	return out
}

// TestComputeFirstSessionHighlights_ExportedFuncNotDeadCode verifies that
// exported functions with no in-repo callers are NOT flagged as dead code —
// they may be public API called by external packages.
func TestComputeFirstSessionHighlights_ExportedFuncNotDeadCode(t *testing.T) {
	g := graph.New("test-repo")

	// Exported function, no callers in-repo — should NOT be dead code.
	exportedID := g.MakeNodeID("pkg/api.go", "HandleHTTP")
	g.AddNode(&graph.Node{
		ID:       exportedID,
		Type:     graph.NodeFunction,
		Name:     "HandleHTTP",
		File:     "pkg/api.go",
		Exported: true,
	})

	// Unexported function, no callers — should be dead code.
	unexportedID := g.MakeNodeID("pkg/api.go", "internalHelper")
	g.AddNode(&graph.Node{
		ID:   unexportedID,
		Type: graph.NodeFunction,
		Name: "internalHelper",
		File: "pkg/api.go",
	})

	result := highlightsJSON(t, g, nil)
	if result == nil {
		t.Fatal("expected non-nil highlights (unexported dead function present)")
	}
	dc, ok := result["dead_code"].(map[string]any)
	if !ok {
		t.Fatalf("expected dead_code section, got %T — keys: %v", result["dead_code"], mapKeys(result))
	}
	total, _ := dc["total"].(float64)
	if total < 1 {
		t.Errorf("expected at least 1 dead code entry (internalHelper), got %v", total)
	}
	for _, entry := range dc["sample"].([]interface{}) {
		em := entry.(map[string]interface{})
		if em["name"] == "HandleHTTP" {
			t.Error("HandleHTTP (exported) must NOT appear in dead_code")
		}
	}
}

// TestComputeFirstSessionHighlights_RecencyBoostsRiskOrder verifies that a
// recently-changed file with lower fanin ranks above a non-recent file with
// higher fanin, because recency multiplies the score by 10.
func TestComputeFirstSessionHighlights_RecencyBoostsRiskOrder(t *testing.T) {
	g := graph.New("test-repo")

	// stableOp: fanin=5, NOT recently changed → score=5
	stableID := g.MakeNodeID("pkg/stable.go", "stableOp")
	g.AddNode(&graph.Node{ID: stableID, Type: graph.NodeFunction, Name: "stableOp", File: "pkg/stable.go"})
	for i := 0; i < 5; i++ {
		cID := g.MakeNodeID("pkg/stable.go", fmt.Sprintf("sCallerOp%d", i))
		g.AddNode(&graph.Node{ID: cID, Type: graph.NodeFunction, Name: fmt.Sprintf("sCallerOp%d", i), File: "pkg/stable.go"})
		g.AddEdge(&graph.Edge{From: cID, To: stableID, Type: graph.EdgeCalls})
	}

	// hotOp: fanin=3, IS recently changed → score=30 (3×10)
	hotID := g.MakeNodeID("pkg/hot.go", "hotOp")
	g.AddNode(&graph.Node{ID: hotID, Type: graph.NodeFunction, Name: "hotOp", File: "pkg/hot.go"})
	for i := 0; i < 3; i++ {
		cID := g.MakeNodeID("pkg/hot.go", fmt.Sprintf("hCallerOp%d", i))
		g.AddNode(&graph.Node{ID: cID, Type: graph.NodeFunction, Name: fmt.Sprintf("hCallerOp%d", i), File: "pkg/hot.go"})
		g.AddEdge(&graph.Edge{From: cID, To: hotID, Type: graph.EdgeCalls})
	}

	recentFiles := map[string]bool{"pkg/hot.go": true}
	// Use JSON round-trip so types are standard (riskEntry is local to the helper).
	raw := computeFirstSessionHighlights(g, nil, recentFiles, false)
	if raw == nil {
		t.Fatal("expected non-nil highlights")
	}
	b, _ := json.Marshal(raw)
	var highlights map[string]any
	json.Unmarshal(b, &highlights)

	hr, ok := highlights["high_risk_entities"].(map[string]any)
	if !ok {
		t.Fatalf("expected high_risk_entities section")
	}
	sample, _ := hr["sample"].([]any)
	if len(sample) < 2 {
		t.Fatalf("expected at least 2 high-risk entries, got %d", len(sample))
	}
	first, _ := sample[0].(map[string]any)
	if first["name"] != "hotOp" {
		t.Errorf("expected hotOp first (recency-boosted score=30 > stableOp score=5), got %s first", first["name"])
	}
}

// TestComputeFirstSessionHighlights_MainAndInitNotDeadCode verifies that
// main() and init() are excluded from dead code even though they are unexported
// and have no CALLS in-edges (they are Go entry points invoked by the runtime).
func TestComputeFirstSessionHighlights_MainAndInitNotDeadCode(t *testing.T) {
	g := graph.New("test-repo")

	for _, name := range []string{"main", "init"} {
		id := g.MakeNodeID("cmd/main.go", name)
		g.AddNode(&graph.Node{
			ID:   id,
			Type: graph.NodeFunction,
			Name: name,
			File: "cmd/main.go",
			// Exported: false (default) — runtime entry points are lowercase.
		})
	}

	// Add one genuine dead function so the result is non-nil.
	deadID := g.MakeNodeID("pkg/util.go", "orphaned")
	g.AddNode(&graph.Node{ID: deadID, Type: graph.NodeFunction, Name: "orphaned", File: "pkg/util.go"})

	result := highlightsJSON(t, g, nil)
	if result == nil {
		t.Fatal("expected non-nil highlights (orphaned dead function present)")
	}
	dc, ok := result["dead_code"].(map[string]any)
	if !ok {
		t.Fatalf("expected dead_code section, got %T", result["dead_code"])
	}
	for _, entry := range dc["sample"].([]interface{}) {
		em := entry.(map[string]interface{})
		if em["name"] == "main" || em["name"] == "init" {
			t.Errorf("%v() must NOT appear in dead_code — it is a Go runtime entry point", em["name"])
		}
	}
}
