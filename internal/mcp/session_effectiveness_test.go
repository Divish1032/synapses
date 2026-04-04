package mcp

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/pulse"
	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
	"github.com/SynapsesOS/synapses/internal/store"
)

// floatPtr is a test helper that creates a *float64 from a literal.
func floatPtr(v float64) *float64 { return &v }

// ── buildEffectivenessMessage ──────────────────────────────────────────────

func TestBuildEffectivenessMessage_NoDeliveries(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries: 0,
		ToolCalls:       12,
		DurationMs:      30_000,
	}
	msg := buildEffectivenessMessage(r)
	if !strings.Contains(msg, "No context deliveries") {
		t.Errorf("expected 'No context deliveries' in message, got: %q", msg)
	}
	if !strings.Contains(msg, "12 tool calls") {
		t.Errorf("expected '12 tool calls' in message, got: %q", msg)
	}
}

func TestBuildEffectivenessMessage_WithDeliveries(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries:    16,
		FirstFetchRight:    14,
		ContextHitRate:     0.85,
		TaskCompletionRate: floatPtr(0.92),
		ToolCalls:          47,
		TokensSaved:        3000,
		DurationMs:         240_000,
	}
	msg := buildEffectivenessMessage(r)
	// Must contain the first-fetch fraction (new wording: "X/Y deliveries required no correction").
	if !strings.Contains(msg, "14/16") {
		t.Errorf("expected '14/16' in message, got: %q", msg)
	}
	// Must contain the hit rate percentage.
	if !strings.Contains(msg, "85%") {
		t.Errorf("expected '85%%' in message, got: %q", msg)
	}
	// Must mention tool calls.
	if !strings.Contains(msg, "47 tool calls") {
		t.Errorf("expected '47 tool calls' in message, got: %q", msg)
	}
	// Must include token savings.
	if !strings.Contains(msg, "3000 tokens saved") {
		t.Errorf("expected token savings in message, got: %q", msg)
	}
}

func TestBuildEffectivenessMessage_PerfectFirstFetch(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries: 5,
		FirstFetchRight: 5,
		ContextHitRate:  1.0,
		ToolCalls:       20,
		DurationMs:      60_000,
	}
	msg := buildEffectivenessMessage(r)
	if !strings.Contains(msg, "5/5") {
		t.Errorf("expected '5/5' in message, got: %q", msg)
	}
	if !strings.Contains(msg, "100%") {
		t.Errorf("expected '100%%' in message, got: %q", msg)
	}
}

func TestBuildEffectivenessMessage_ZeroTokensSaved_NoSavingsLine(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries: 2,
		FirstFetchRight: 1,
		ContextHitRate:  0.5,
		ToolCalls:       5,
		TokensSaved:     0,
		DurationMs:      10_000,
	}
	msg := buildEffectivenessMessage(r)
	if strings.Contains(msg, "tokens saved") {
		t.Errorf("expected no 'tokens saved' when TokensSaved=0, got: %q", msg)
	}
}

// ── EffectivenessReport struct zero-value safety ──────────────────────────

func TestBuildEffectivenessMessage_AllZero(t *testing.T) {
	// Zero-value report must not panic.
	r := &EffectivenessReport{}
	msg := buildEffectivenessMessage(r)
	if msg == "" {
		t.Error("expected non-empty message for zero-value report")
	}
}

// ── TaskCompletionRate nil vs zero distinction ────────────────────────────

func TestEffectivenessReport_TaskCompletionRateNilWhenNoRetro(t *testing.T) {
	// When retro is nil (no tool-call data), TaskCompletionRate must be nil
	// so JSON consumers see omitted/null rather than false "0% success".
	r := &EffectivenessReport{
		ContextHitRate:     0.9,
		TaskCompletionRate: nil, // explicitly nil = no data
		ToolCalls:          0,
		TotalDeliveries:    3,
		FirstFetchRight:    3,
		DurationMs:         5_000,
	}
	if r.TaskCompletionRate != nil {
		t.Errorf("expected nil TaskCompletionRate when no retro, got %v", *r.TaskCompletionRate)
	}
}

func TestEffectivenessReport_TaskCompletionRateSetWhenRetroPresent(t *testing.T) {
	// When retro is present (even with 0% success), TaskCompletionRate is non-nil.
	rate := 0.0 // 100% error rate → 0% completion
	r := &EffectivenessReport{
		TaskCompletionRate: &rate,
	}
	if r.TaskCompletionRate == nil {
		t.Error("expected non-nil TaskCompletionRate when retro is present")
	}
	if *r.TaskCompletionRate != 0.0 {
		t.Errorf("expected 0.0, got %v", *r.TaskCompletionRate)
	}
}

// ── handleEndSession EffectivenessReport integration ─────────────────────────

// TestHandleEndSession_EffectivenessReport_AbsentWithoutPulse verifies that
// effectiveness_report is absent from the end_session response when no pulse
// client is attached — no zero-value noise for projects without analytics.
func TestHandleEndSession_EffectivenessReport_AbsentWithoutPulse(t *testing.T) {
	srv := newTestServer(t) // no pulse client
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "no-pulse-agent"}))
	res, err := srv.handleEndSession(ctx, callTool(map[string]any{"agent_id": "no-pulse-agent"}))
	m := mustResult(t, res, err)
	noKey(t, m, "effectiveness_report")
}

// TestHandleEndSession_EffectivenessReport_PresentAfterSessionInit verifies
// that effectiveness_report is present whenever a pulse client is configured
// and session_init has been called (so synapseSessionID is non-empty).
// This covers the no-delivery path: message must describe a session with zero
// context calls rather than silently omitting the field.
func TestHandleEndSession_EffectivenessReport_PresentAfterSessionInit(t *testing.T) {
	srv := newTestServer(t)
	pc := newPulseClient(t)
	defer pc.Close()
	srv.SetPulseClient(pc)

	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "eff-agent"}))

	res, err := srv.handleEndSession(ctx, callTool(map[string]any{"agent_id": "eff-agent"}))
	m := mustResult(t, res, err)

	hasKey(t, m, "effectiveness_report")
	report, ok := m["effectiveness_report"].(map[string]any)
	if !ok {
		t.Fatalf("effectiveness_report must be a map, got %T (keys: %v)", m["effectiveness_report"], mapKeys(m))
	}
	msg, _ := report["message"].(string)
	if msg == "" {
		t.Error("effectiveness_report.message must be non-empty")
	}
	// With no context deliveries the count must be present as 0, not missing.
	if _, ok := report["total_deliveries"]; !ok {
		t.Error("effectiveness_report.total_deliveries must be present")
	}
}

// TestHandleEndSession_EffectivenessReport_CountsDeliveries verifies that
// total_deliveries and first_fetch_right correctly reflect context deliveries
// seeded for the session before end_session is called.
//
// Setup: 3 deliveries — 2 first-fetch (refetched=false) + 1 correction (refetched=true).
// Expected: total_deliveries=3, first_fetch_right=2, message contains "2/3".
func TestHandleEndSession_EffectivenessReport_CountsDeliveries(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)

	// session_init registers the Synapses session UUID under the "stdio" key.
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "del-agent"}))

	// Retrieve the UUID assigned by session_init (same package → unexported OK).
	sessID := srv.getSynapseSessionID("") // "" = stdio
	if sessID == "" {
		t.Fatal("expected non-empty Synapses session ID after session_init")
	}

	// Open a direct pstore connection for synchronous delivery inserts.
	// SQLite WAL allows two concurrent connections; writes are visible on commit.
	st, err := pulsestore.Open(pulsePath)
	if err != nil {
		t.Fatalf("pulsestore.Open: %v", err)
	}

	// 2 first-fetch deliveries (refetched=false) with token savings.
	for i := 0; i < 2; i++ {
		if err := st.InsertContextDeliveryTx(pulsetypes.ContextDeliveryEvent{
			SessionID:      sessID,
			AgentID:        "del-agent",
			Entity:         "FooService",
			BaselineTokens: 1000,
			ResponseTokens: 800,
			EntityFound:    true,
		}); err != nil {
			t.Fatalf("InsertContextDeliveryTx (first-fetch %d): %v", i, err)
		}
	}
	// 1 correction delivery (refetched=true).
	if err := st.InsertContextDeliveryTx(pulsetypes.ContextDeliveryEvent{
		SessionID:      sessID,
		AgentID:        "del-agent",
		Entity:         "FooService",
		BaselineTokens: 1000,
		ResponseTokens: 950,
		Refetched:      true,
		EntityFound:    true,
	}); err != nil {
		t.Fatalf("InsertContextDeliveryTx (refetched): %v", err)
	}
	// Close direct store before end_session reads it — ensures WAL is flushed.
	st.Close()

	res, err := srv.handleEndSession(ctx, callTool(map[string]any{"agent_id": "del-agent"}))
	m := mustResult(t, res, err)

	hasKey(t, m, "effectiveness_report")
	report, ok := m["effectiveness_report"].(map[string]any)
	if !ok {
		t.Fatalf("effectiveness_report must be a map, got %T", m["effectiveness_report"])
	}

	totalDel, _ := report["total_deliveries"].(float64)
	firstFetch, _ := report["first_fetch_right"].(float64)
	tokensSaved, _ := report["tokens_saved"].(float64)
	msg, _ := report["message"].(string)

	if totalDel != 3 {
		t.Errorf("total_deliveries: want 3, got %v", totalDel)
	}
	if firstFetch != 2 {
		t.Errorf("first_fetch_right: want 2, got %v", firstFetch)
	}
	// 2×(1000-800) + 1×(1000-950) = 400+50 = 450 tokens saved.
	if tokensSaved != 450 {
		t.Errorf("tokens_saved: want 450, got %v", tokensSaved)
	}
	// Message uses new wording: "First-fetch context: 2/3 deliveries required no correction"
	if !strings.Contains(msg, "2/3") {
		t.Errorf("message must contain '2/3', got: %q", msg)
	}
	if !strings.Contains(msg, "450 tokens saved") {
		t.Errorf("message must mention token savings, got: %q", msg)
	}
}

// TestHandleEndSession_EffectivenessReport_Prev7d verifies that prev_7d is
// populated when prior session_effectiveness rows exist for the same agent.
// This exercises the critical trend-read-before-insert ordering: the prior
// session must NOT appear in its own Prev7d, and the seeded prior session
// MUST appear in the new session's Prev7d.
func TestHandleEndSession_EffectivenessReport_Prev7d(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	// Seed a prior session effectiveness row directly (synchronous — bypasses collector).
	st, err := pulsestore.Open(pulsePath)
	if err != nil {
		t.Fatalf("pulsestore.Open: %v", err)
	}
	if err := st.InsertSessionEffectiveness(pulsetypes.SessionEffectiveness{
		SessionID:          "prior-session-uuid",
		AgentID:            "prev-agent",
		ProjectID:          "test-project",
		ContextHitRate:     0.8,
		TaskCompletionRate: 0.9,
		TokensSaved:        1200,
		ToolCalls:          30,
		DurationMs:         120_000,
	}); err != nil {
		t.Fatalf("InsertSessionEffectiveness (prior): %v", err)
	}
	st.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)

	// session_init registers the new Synapses session (different from the prior one).
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "prev-agent"}))

	res, err := srv.handleEndSession(ctx, callTool(map[string]any{"agent_id": "prev-agent"}))
	m := mustResult(t, res, err)

	hasKey(t, m, "effectiveness_report")
	report, ok := m["effectiveness_report"].(map[string]any)
	if !ok {
		t.Fatalf("effectiveness_report must be a map, got %T", m["effectiveness_report"])
	}

	// prev_7d must be present because the prior session exists.
	hasKey(t, report, "prev_7d")
	prev7d, ok := report["prev_7d"].(map[string]any)
	if !ok {
		t.Fatalf("prev_7d must be a map, got %T", report["prev_7d"])
	}

	sessions, _ := prev7d["sessions"].(float64)
	if sessions < 1 {
		t.Errorf("prev_7d.sessions: want ≥1 (the seeded prior session), got %v", sessions)
	}
	avgHitRate, _ := prev7d["avg_context_hit_rate"].(float64)
	if avgHitRate <= 0 {
		t.Errorf("prev_7d.avg_context_hit_rate: want >0, got %v", avgHitRate)
	}
	totalTokens, _ := prev7d["total_tokens_saved"].(float64)
	if totalTokens != 1200 {
		t.Errorf("prev_7d.total_tokens_saved: want 1200 (from prior session), got %v", totalTokens)
	}
}

// TestHandleEndSession_EffectivenessReport_AbsentWithoutSessionInit verifies
// that effectiveness_report is absent when pulse is configured but session_init
// was never called for this agent — synapseSessionID is empty so the guard
// `pc != nil && synapseSessionID != ""` blocks report generation.
func TestHandleEndSession_EffectivenessReport_AbsentWithoutSessionInit(t *testing.T) {
	srv := newTestServer(t)
	pc := newPulseClient(t)
	defer pc.Close()
	srv.SetPulseClient(pc)

	// end_session WITHOUT prior session_init — no Synapses session UUID registered.
	res, err := srv.handleEndSession(ctx, callTool(map[string]any{"agent_id": "no-init-agent"}))
	m := mustResult(t, res, err)
	noKey(t, m, "effectiveness_report")
}

// ── Knowledge growth ─────────────────────────────────────────────────────────

func TestBuildEffectivenessMessage_KnowledgeGrowth(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries: 2,
		FirstFetchRight: 2,
		ContextHitRate:  0.5,
		ToolCalls:       10,
		DurationMs:      60_000,
		KnowledgeGrowth: 3,
	}
	msg := buildEffectivenessMessage(r)
	if !strings.Contains(msg, "3 memories created") {
		t.Errorf("message should mention knowledge growth: %q", msg)
	}
}

func TestBuildEffectivenessMessage_ZeroKnowledgeGrowth_NoMention(t *testing.T) {
	r := &EffectivenessReport{
		TotalDeliveries: 1,
		FirstFetchRight: 1,
		ContextHitRate:  1.0,
		ToolCalls:       5,
		DurationMs:      30_000,
		KnowledgeGrowth: 0,
	}
	msg := buildEffectivenessMessage(r)
	if strings.Contains(msg, "memories") {
		t.Errorf("message should not mention memories when growth is 0: %q", msg)
	}
}

func TestHandleEndSession_EffectivenessReport_KnowledgeGrowthInPrev7d(t *testing.T) {
	dir := t.TempDir()
	pulsePath := filepath.Join(dir, "pulse.sqlite")

	pc, err := pulse.New(pulsePath)
	if err != nil {
		t.Fatalf("pulse.New: %v", err)
	}
	defer pc.Close()

	// Seed a prior session with knowledge_growth > 0.
	st, err := pulsestore.Open(pulsePath)
	if err != nil {
		t.Fatalf("pulsestore.Open: %v", err)
	}
	if err := st.InsertSessionEffectiveness(pulsetypes.SessionEffectiveness{
		SessionID:       "prior-kg-session",
		AgentID:         "kg-agent",
		ProjectID:       "test-project",
		ContextHitRate:  0.5,
		TokensSaved:     100,
		ToolCalls:       5,
		DurationMs:      30_000,
		KnowledgeGrowth: 7,
	}); err != nil {
		t.Fatalf("InsertSessionEffectiveness: %v", err)
	}
	st.Close()

	srv := newTestServer(t)
	srv.SetPulseClient(pc)

	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "kg-agent"}))
	res, err := srv.handleEndSession(ctx, callTool(map[string]any{"agent_id": "kg-agent"}))
	m := mustResult(t, res, err)

	report, ok := m["effectiveness_report"].(map[string]any)
	if !ok {
		t.Fatalf("effectiveness_report must be a map, got %T", m["effectiveness_report"])
	}
	prev7d, ok := report["prev_7d"].(map[string]any)
	if !ok {
		t.Fatalf("prev_7d must be a map, got %T", report["prev_7d"])
	}
	kg, _ := prev7d["total_knowledge_growth"].(float64)
	if kg != 7 {
		t.Errorf("prev_7d.total_knowledge_growth: want 7 (from seeded session), got %v", kg)
	}
}

// TestDeterministicArchivist_WiresEmbedding verifies Sprint 30.5: the Deterministic
// Archivist must call QueueEmbedMemory after every InsertMemory so that session
// learnings become semantically searchable in future sessions.
// Regression target: if the QueueEmbedMemory call is removed, no embedding is stored.
func TestDeterministicArchivist_WiresEmbedding(t *testing.T) {
	srv := newTestServer(t)
	srv.StartBackground()

	// Wire a fast stub embedder — returns a non-nil vector immediately.
	emb := &testEmbedder{vec: []float32{0.1, 0.2, 0.3}, model: "test-archivist-embed"}
	srv.SetMemoryEmbedder(emb)

	// Register a session so handleEndSession picks up the synapseSessionID.
	_, err := srv.handleSessionInit(ctx, callTool(map[string]any{"agent_id": "archivist-embed-agent"}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}

	sessID := srv.getSynapseSessionID(SessionIDFromContext(ctx))
	if sessID == "" {
		t.Fatal("synapseSessionID must be non-empty after session_init")
	}

	// Seed an exploration log entry — Bucket 4 (get_context) produces a memory
	// when EntityQueried and FindingSummary are non-empty.
	if err := srv.store.AppendExplorationEntry(store.ExplorationEntry{
		SessionID:      sessID,
		ProjectID:      srv.projectID,
		ToolName:       "get_context",
		EntityQueried:  "AuthService",
		FindingSummary: "AuthService handles OAuth2 login and delegates JWT validation to TokenValidator",
	}); err != nil {
		t.Fatalf("AppendExplorationEntry: %v", err)
	}

	// End session — Deterministic Archivist runs, saves the memory, and
	// (Sprint 30.5) calls QueueEmbedMemory on the saved memory ID.
	res, err := srv.handleEndSession(ctx, callTool(map[string]any{"agent_id": "archivist-embed-agent"}))
	if err != nil {
		t.Fatalf("handleEndSession: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleEndSession returned error: %v", res.Content)
	}

	// Drain background workers — embedding is fire-and-forget via goBackground.
	time.Sleep(300 * time.Millisecond)

	// Verify: at least one auto-captured project memory now has an embedding stored.
	mems, err := srv.store.RecentMemoriesCtx(ctx, 20, 7, nil, false)
	if err != nil {
		t.Fatalf("RecentMemoriesCtx: %v", err)
	}

	embeddedCount := 0
	for _, m := range mems {
		if m.Source == store.SourceAuto && m.Tier == store.TierProject {
			vec := srv.store.GetMemoryEmbedding(m.ID)
			if len(vec) > 0 {
				embeddedCount++
			}
		}
	}

	if embeddedCount == 0 {
		t.Error("Sprint 30.5: Deterministic Archivist must call QueueEmbedMemory — no auto-captured memory has an embedding")
	}
}
