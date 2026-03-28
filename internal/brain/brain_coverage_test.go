package brain

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
	"github.com/SynapsesOS/synapses/internal/brain/contextbuilder"
	"github.com/SynapsesOS/synapses/internal/brain/store"
)

// --- NullBrain tests ---

func TestNullBrain_Ingest(t *testing.T) {
	nb := &NullBrain{}
	resp, err := nb.Ingest(context.Background(), IngestRequest{NodeID: "test-node"})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if resp.NodeID != "test-node" {
		t.Errorf("NodeID: got %q, want test-node", resp.NodeID)
	}
	if resp.Summary != "" {
		t.Error("expected empty summary")
	}
}

func TestNullBrain_Enrich(t *testing.T) {
	nb := &NullBrain{}
	resp, err := nb.Enrich(context.Background(), EnrichRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Summaries == nil {
		t.Error("expected non-nil summaries map")
	}
	if resp.Insight != "" {
		t.Error("expected empty insight")
	}
}

func TestNullBrain_ExplainViolation(t *testing.T) {
	nb := &NullBrain{}
	resp, err := nb.ExplainViolation(context.Background(), ViolationRequest{RuleID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Explanation != "" || resp.Fix != "" {
		t.Error("expected empty violation response")
	}
}

func TestNullBrain_Coordinate(t *testing.T) {
	nb := &NullBrain{}
	resp, err := nb.Coordinate(context.Background(), CoordinateRequest{NewAgentID: "agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Suggestion != "" {
		t.Error("expected empty suggestion")
	}
}

func TestNullBrain_Summary(t *testing.T) {
	nb := &NullBrain{}
	if nb.Summary("proj", "node") != "" {
		t.Error("expected empty summary")
	}
}

func TestNullBrain_Available(t *testing.T) {
	nb := &NullBrain{}
	if nb.Available() {
		t.Error("NullBrain should not be available")
	}
}

func TestNullBrain_ModelName(t *testing.T) {
	nb := &NullBrain{}
	if nb.ModelName() != "" {
		t.Error("expected empty model name")
	}
}

func TestNullBrain_EnsureModel(t *testing.T) {
	nb := &NullBrain{}
	if err := nb.EnsureModel(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestNullBrain_BuildContextPacket(t *testing.T) {
	nb := &NullBrain{}
	pkt, err := nb.BuildContextPacket(context.Background(), ContextPacketRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if pkt != nil {
		t.Error("expected nil packet")
	}
}

func TestNullBrain_LogDecision(t *testing.T) {
	nb := &NullBrain{}
	if err := nb.LogDecision(context.Background(), DecisionRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestNullBrain_SetSDLCPhase(t *testing.T) {
	nb := &NullBrain{}
	if err := nb.SetSDLCPhase(PhaseDevelopment, "agent-1"); err != nil {
		t.Fatal(err)
	}
}

func TestNullBrain_SetQualityMode(t *testing.T) {
	nb := &NullBrain{}
	if err := nb.SetQualityMode(QualityStandard, "agent-1"); err != nil {
		t.Fatal(err)
	}
}

func TestNullBrain_GetSDLCConfig(t *testing.T) {
	nb := &NullBrain{}
	cfg := nb.GetSDLCConfig()
	if cfg.Phase != PhaseDevelopment {
		t.Errorf("Phase: got %q, want development", cfg.Phase)
	}
	if cfg.QualityMode != QualityStandard {
		t.Errorf("QualityMode: got %q, want standard", cfg.QualityMode)
	}
}

func TestNullBrain_GetPatterns(t *testing.T) {
	nb := &NullBrain{}
	if nb.GetPatterns("", 10) != nil {
		t.Error("expected nil patterns")
	}
}

func TestNullBrain_Prune(t *testing.T) {
	nb := &NullBrain{}
	content, err := nb.Prune(context.Background(), "test content")
	if err != nil {
		t.Fatal(err)
	}
	if content != "test content" {
		t.Errorf("expected content unchanged, got %q", content)
	}
}

func TestNullBrain_UpsertADR(t *testing.T) {
	nb := &NullBrain{}
	if err := nb.UpsertADR(ADRRequest{ID: "adr-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestNullBrain_GetADR(t *testing.T) {
	nb := &NullBrain{}
	adr, err := nb.GetADR("adr-1")
	if err != nil {
		t.Fatal(err)
	}
	if adr.ID != "" {
		t.Error("expected empty ADR")
	}
}

func TestNullBrain_AllADRs(t *testing.T) {
	nb := &NullBrain{}
	adrs, err := nb.AllADRs()
	if err != nil {
		t.Fatal(err)
	}
	if adrs != nil {
		t.Error("expected nil ADRs")
	}
}

func TestNullBrain_GetADRsForFile(t *testing.T) {
	nb := &NullBrain{}
	adrs, err := nb.GetADRsForFile("test.go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if adrs != nil {
		t.Error("expected nil ADRs")
	}
}

func TestNullBrain_Memorize(t *testing.T) {
	nb := &NullBrain{}
	resp, err := nb.Memorize(context.Background(), archivist.MemorizeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.NewMemories) != 0 {
		t.Error("expected no memories")
	}
}

// --- Client tests (with NullBrain) ---

func TestNewClient_Deprecated(t *testing.T) {
	c := NewClient("http://unused", 5000)
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	status, err := c.HealthCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != "" {
		t.Errorf("expected empty status, got %q", status)
	}
}

func TestNewInProcess_Nil(t *testing.T) {
	c := NewInProcess(nil)
	if c == nil {
		t.Fatal("NewInProcess(nil) returned nil")
	}
	status, err := c.HealthCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != "" {
		t.Errorf("expected empty status, got %q", status)
	}
}

func TestNewInProcess_Disabled(t *testing.T) {
	cfg := &brainconfig.BrainConfig{Enabled: false}
	c := NewInProcess(cfg)
	if c == nil {
		t.Fatal("NewInProcess returned nil")
	}
	status, err := c.HealthCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != "" {
		t.Errorf("expected empty status, got %q", status)
	}
}

func TestNewInProcess_Enabled_UnknownBackend(t *testing.T) {
	cfg := &brainconfig.BrainConfig{
		Enabled: true,
		Backend: "nonexistent",
	}
	c := NewInProcess(cfg)
	if c == nil {
		t.Fatal("NewInProcess returned nil")
	}
	// With unknown backend, should fall back to NullBrain
	status, err := c.HealthCheck(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != "" {
		t.Errorf("expected empty status from NullBrain fallback, got %q", status)
	}
}

func TestClient_Ingest(t *testing.T) {
	c := NewClient("", 0)
	c.Ingest(context.Background(), IngestRequest{NodeID: "n1"})
}

func TestClient_ExplainViolation(t *testing.T) {
	c := NewClient("", 0)
	exp, fix := c.ExplainViolation(context.Background(), ViolationRequest{})
	if exp != "" || fix != "" {
		t.Error("expected empty response from NullBrain")
	}
}

func TestClient_GetSummary(t *testing.T) {
	c := NewClient("", 0)
	if c.GetSummary(context.Background(), "node-1") != "" {
		t.Error("expected empty summary")
	}
}

func TestClient_LogDecision(t *testing.T) {
	c := NewClient("", 0)
	c.LogDecision(context.Background(), DecisionRequest{})
}

func TestClient_BuildContextPacket(t *testing.T) {
	c := NewClient("", 0)
	pkt := c.BuildContextPacket(context.Background(), ContextPacketRequest{})
	if pkt != nil {
		t.Error("expected nil packet from NullBrain")
	}
}

func TestClient_SetPhase(t *testing.T) {
	c := NewClient("", 0)
	cfg, err := c.SetPhase(context.Background(), SetPhaseRequest{Phase: "testing"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Phase != PhaseDevelopment {
		t.Errorf("phase: got %q", cfg.Phase)
	}
}

func TestClient_UpsertADR(t *testing.T) {
	c := NewClient("", 0)
	_, err := c.UpsertADR(context.Background(), ADRRequest{ID: "adr-1", Title: "Test"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClient_GetADR(t *testing.T) {
	c := NewClient("", 0)
	adr, err := c.GetADR(context.Background(), "adr-1")
	if err != nil {
		t.Fatal(err)
	}
	if adr.ID != "" {
		t.Error("expected empty ADR from NullBrain")
	}
}

func TestClient_GetADRs(t *testing.T) {
	c := NewClient("", 0)
	adrs, err := c.GetADRs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if adrs != nil {
		t.Error("expected nil")
	}

	adrs2, err := c.GetADRs(context.Background(), "test.go")
	if err != nil {
		t.Fatal(err)
	}
	if adrs2 != nil {
		t.Error("expected nil")
	}
}

func TestClient_Close(t *testing.T) {
	c := NewClient("", 0)
	c.Close()
}

func TestClient_Close_WithEnabledBrain(t *testing.T) {
	cfg := &brainconfig.BrainConfig{Enabled: false}
	c := NewInProcess(cfg)
	if c == nil {
		t.Fatal("NewInProcess returned nil")
	}
	c.Close()
	// Should not panic even though NullBrain.Close() may do nothing
}

func TestClient_BuildContextPacket_WithEnabledBrain(t *testing.T) {
	cfg := &brainconfig.BrainConfig{Enabled: false}
	c := NewInProcess(cfg)
	pkt := c.BuildContextPacket(context.Background(), ContextPacketRequest{})
	if pkt != nil {
		t.Error("expected nil packet from disabled brain")
	}
}

// --- Mock Brain for testing BrainStatsProvider ---

type mockBrainWithStats struct {
	*NullBrain
	stats map[string]interface{}
}

func (m *mockBrainWithStats) BrainStats() map[string]interface{} {
	return m.stats
}

type mockBrainWithTierStatus struct {
	*NullBrain
	stats     map[string]interface{}
	tierState map[string]TierState
}

func (m *mockBrainWithTierStatus) BrainStats() map[string]interface{} {
	return m.stats
}

func (m *mockBrainWithTierStatus) TierStatus() map[string]TierState {
	return m.tierState
}

// --- Client.BrainHealth() tests ---

func TestClient_BrainHealth_WithoutStats(t *testing.T) {
	c := NewClient("", 0)
	health := c.BrainHealth()
	if health != nil {
		t.Error("expected nil health for NullBrain (no BrainStatsProvider)")
	}
}

func TestClient_BrainHealth_WithStats(t *testing.T) {
	mockBrain := &mockBrainWithStats{
		NullBrain: &NullBrain{},
		stats: map[string]interface{}{
			"ingest_calls":      int64(10),
			"ingest_success":    int64(8),
			"ingest_avg_ms":     int64(50),
			"enrich_calls":      int64(5),
			"enrich_success":    int64(5),
			"enrich_avg_ms":     int64(100),
			"guardian_calls":    int64(0),
			"orchestrate_calls": int64(0),
			"archivist_calls":   int64(0),
			"context_builder_calls": int64(0),
		},
	}
	c := &Client{brain: mockBrain}
	health := c.BrainHealth()

	if health == nil {
		t.Fatal("expected non-nil health")
	}
	if model, ok := health["model"]; !ok {
		t.Error("missing 'model' key in health response")
	} else if _, isString := model.(string); !isString {
		t.Error("model should be a string")
	}
	if tiers, ok := health["tiers"].(map[string]interface{}); !ok {
		t.Fatal("expected 'tiers' map in health response")
	} else {
		if ingest, ok := tiers["ingest"].(map[string]interface{}); ok {
			if calls, _ := ingest["calls"].(int64); calls != 10 {
				t.Errorf("ingest calls: got %d, want 10", calls)
			}
			if rate, _ := ingest["success_rate"].(float64); rate != 0.8 {
				t.Errorf("ingest success_rate: got %f, want 0.8", rate)
			}
		}
	}
}

func TestClient_BrainHealth_WithTierStatus(t *testing.T) {
	mockBrain := &mockBrainWithTierStatus{
		NullBrain: &NullBrain{},
		stats: map[string]interface{}{
			"ingest_calls":      int64(1),
			"ingest_success":    int64(0),
			"ingest_avg_ms":     int64(0),
			"enrich_calls":      int64(0),
			"guardian_calls":    int64(0),
			"orchestrate_calls": int64(0),
			"archivist_calls":   int64(0),
			"context_builder_calls": int64(0),
		},
		tierState: map[string]TierState{
			"ingest": {Open: true, Failures: 3, CooldownRemaining: 10000},
		},
	}
	c := &Client{brain: mockBrain}
	health := c.BrainHealth()

	if health == nil {
		t.Fatal("expected non-nil health")
	}
	if tiers, ok := health["tiers"].(map[string]interface{}); ok {
		if ingest, ok := tiers["ingest"].(map[string]interface{}); ok {
			if circuit, _ := ingest["circuit"].(string); circuit != "open" {
				t.Errorf("ingest circuit: got %q, want open", circuit)
			}
		}
	}
}

// --- Circuit breaker tests ---

func TestCircuitBreaker_InitialState(t *testing.T) {
	cb := newCircuitBreaker(3, 5*time.Minute)
	if cb.isOpen("ingest") {
		t.Error("circuit should be closed initially")
	}
}

func TestCircuitBreaker_TripsAfterMaxFailures(t *testing.T) {
	cb := newCircuitBreaker(3, 5*time.Minute)
	cb.recordFailure("ingest")
	cb.recordFailure("ingest")
	if cb.isOpen("ingest") {
		t.Error("should not trip after 2 failures")
	}
	cb.recordFailure("ingest")
	if !cb.isOpen("ingest") {
		t.Error("should trip after 3 failures")
	}
}

func TestCircuitBreaker_ResetOnSuccess(t *testing.T) {
	cb := newCircuitBreaker(3, 5*time.Minute)
	cb.recordFailure("ingest")
	cb.recordFailure("ingest")
	cb.recordSuccess("ingest")
	cb.recordFailure("ingest")
	cb.recordFailure("ingest")
	if cb.isOpen("ingest") {
		t.Error("should not trip — success reset the counter")
	}
}

func TestCircuitBreaker_CooldownExpiry(t *testing.T) {
	cb := newCircuitBreaker(1, 10*time.Millisecond)
	cb.recordFailure("ingest")
	if !cb.isOpen("ingest") {
		t.Error("should be open after failure")
	}
	time.Sleep(15 * time.Millisecond)
	if cb.isOpen("ingest") {
		t.Error("should be closed after cooldown")
	}
}

func TestCircuitBreaker_IndependentTiers(t *testing.T) {
	cb := newCircuitBreaker(1, 5*time.Minute)
	cb.recordFailure("ingest")
	if !cb.isOpen("ingest") {
		t.Error("ingest should be open")
	}
	if cb.isOpen("enrich") {
		t.Error("enrich should still be closed")
	}
}

func TestCircuitBreaker_Status(t *testing.T) {
	cb := newCircuitBreaker(1, 5*time.Minute)
	cb.recordFailure("ingest")
	status := cb.status()
	if !status["ingest"].Open {
		t.Error("ingest should be open")
	}
	if status["ingest"].Failures != 1 {
		t.Errorf("failures: got %d, want 1", status["ingest"].Failures)
	}
	if status["ingest"].CooldownRemaining <= 0 {
		t.Error("expected positive cooldown remaining")
	}
	if status["enrich"].Open {
		t.Error("enrich should be closed")
	}
}

// --- brainStats tests ---

func TestBrainStats_Record(t *testing.T) {
	s := &brainStats{}
	s.record("ingest", true, 100)
	s.record("ingest", false, 50)
	s.record("enrich", true, 200)
	s.record("guardian", true, 75)
	s.record("orchestrate", false, 300)
	s.record("archivist", true, 150)

	snap := s.snapshot()
	if snap["ingest_calls"].(int64) != 2 {
		t.Errorf("ingest_calls: got %v", snap["ingest_calls"])
	}
	if snap["ingest_success"].(int64) != 1 {
		t.Errorf("ingest_success: got %v", snap["ingest_success"])
	}
	if snap["enrich_calls"].(int64) != 1 {
		t.Errorf("enrich_calls: got %v", snap["enrich_calls"])
	}
	if snap["guardian_success"].(int64) != 1 {
		t.Errorf("guardian_success: got %v", snap["guardian_success"])
	}
	if snap["orchestrate_calls"].(int64) != 1 {
		t.Errorf("orchestrate_calls: got %v", snap["orchestrate_calls"])
	}
	if snap["archivist_calls"].(int64) != 1 {
		t.Errorf("archivist_calls: got %v", snap["archivist_calls"])
	}
}

func TestBrainStats_AvgLatency(t *testing.T) {
	s := &brainStats{}
	s.record("ingest", true, 100)
	s.record("ingest", true, 200)

	snap := s.snapshot()
	avg := snap["ingest_avg_ms"].(int64)
	if avg != 150 {
		t.Errorf("avg ingest latency: got %d, want 150", avg)
	}
}

func TestBrainStats_ZeroCalls(t *testing.T) {
	s := &brainStats{}
	snap := s.snapshot()
	if snap["ingest_avg_ms"].(int64) != 0 {
		t.Error("expected 0 avg with no calls")
	}
}

func TestBrainStats_ContextBuilderTier(t *testing.T) {
	s := &brainStats{}
	s.record("context_builder", true, 120)
	s.record("context_builder", false, 80)
	s.record("context_builder", true, 100)

	snap := s.snapshot()
	if snap["context_builder_calls"].(int64) != 3 {
		t.Errorf("context_builder_calls: got %v, want 3", snap["context_builder_calls"])
	}
	if snap["context_builder_success"].(int64) != 2 {
		t.Errorf("context_builder_success: got %v, want 2", snap["context_builder_success"])
	}
}

func TestBrainStats_UnknownTier(t *testing.T) {
	s := &brainStats{}
	s.record("unknown_tier", true, 100)
	snap := s.snapshot()
	// Unknown tier should be silently ignored — no panic
	if snap["unknown_tier_calls"] != nil {
		t.Error("unknown tier should not be recorded")
	}
}

// --- New() with disabled config ---

func TestNew_Disabled(t *testing.T) {
	b := New(brainconfig.BrainConfig{Enabled: false})
	_, ok := b.(*NullBrain)
	if !ok {
		t.Error("disabled config should return NullBrain")
	}
}

// --- New() with unknown backend ---

func TestNew_UnknownBackend(t *testing.T) {
	b := New(brainconfig.BrainConfig{
		Enabled: true,
		Backend: "nonexistent",
	})
	// No LLM client configured → NullBrain
	_, ok := b.(*NullBrain)
	if !ok {
		t.Error("expected NullBrain when backend is unknown")
	}
}

// --- Conversion helpers ---

func TestToBuilderRules(t *testing.T) {
	rules := []RuleInput{
		{RuleID: "r1", Severity: "error", Description: "no imports"},
	}
	out := toBuilderRules(rules)
	if len(out) != 1 || out[0].RuleID != "r1" {
		t.Error("toBuilderRules failed")
	}
}

func TestToBuilderClaims(t *testing.T) {
	claims := []ClaimInput{
		{AgentID: "a1", Scope: "pkg/auth", ScopeType: "package"},
	}
	out := toBuilderClaims(claims)
	if len(out) != 1 || out[0].AgentID != "a1" {
		t.Error("toBuilderClaims failed")
	}
}

// --- impl struct tests ---

func TestImpl_Available(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	if b.Available() {
		t.Error("expected brain to be unavailable when disabled")
	}
}

func TestImpl_ModelName(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	name := b.ModelName()
	// ModelName may be empty or set based on config
	_ = name
}

func TestImpl_Summary(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	summary := b.Summary("test-project", "test-node")
	// Summary should be empty for disabled brain
	if summary != "" {
		t.Errorf("expected empty summary for disabled brain, got %q", summary)
	}
}

func TestImpl_Ingest_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	resp, err := b.Ingest(ctx, IngestRequest{NodeID: "test"})
	if err != nil {
		t.Fatalf("Ingest failed: %v", err)
	}
	if resp.NodeID != "test" {
		t.Errorf("NodeID mismatch: got %q, want test", resp.NodeID)
	}
}

func TestImpl_Enrich_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	resp, err := b.Enrich(ctx, EnrichRequest{})
	if err != nil {
		t.Fatalf("Enrich failed: %v", err)
	}
	if resp.Summaries == nil {
		t.Error("expected non-nil summaries map")
	}
}

func TestImpl_ExplainViolation_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	resp, err := b.ExplainViolation(ctx, ViolationRequest{RuleID: "test"})
	if err != nil {
		t.Fatalf("ExplainViolation failed: %v", err)
	}
	// Disabled brain should return empty response
	if resp.Explanation != "" || resp.Fix != "" {
		t.Error("expected empty explanation and fix for disabled brain")
	}
}

func TestImpl_Coordinate_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	resp, err := b.Coordinate(ctx, CoordinateRequest{NewAgentID: "agent-1"})
	if err != nil {
		t.Fatalf("Coordinate failed: %v", err)
	}
	// Disabled brain should return empty suggestion
	if resp.Suggestion != "" {
		t.Error("expected empty suggestion for disabled brain")
	}
}

func TestImpl_Prune_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	content := "test content"
	result, err := b.Prune(ctx, content)
	if err != nil {
		t.Fatalf("Prune failed: %v", err)
	}
	if result != content {
		t.Errorf("Prune should return unchanged content, got %q", result)
	}
}

func TestImpl_GetSDLCConfig_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	sdlc := b.GetSDLCConfig()
	if sdlc.Phase != PhaseDevelopment {
		t.Errorf("expected PhaseDevelopment, got %q", sdlc.Phase)
	}
	if sdlc.QualityMode != QualityStandard {
		t.Errorf("expected QualityStandard, got %q", sdlc.QualityMode)
	}
}

func TestImpl_SetSDLCPhase_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	err := b.SetSDLCPhase(PhaseReview, "agent-1")
	if err != nil {
		t.Fatalf("SetSDLCPhase failed: %v", err)
	}
}

func TestImpl_SetQualityMode_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	err := b.SetQualityMode(QualityEnterprise, "agent-1")
	if err != nil {
		t.Fatalf("SetQualityMode failed: %v", err)
	}
}

func TestImpl_GetPatterns_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	patterns := b.GetPatterns("test-trigger", 10)
	if patterns != nil {
		t.Error("expected nil patterns for disabled brain")
	}
}

func TestImpl_LogDecision_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	err := b.LogDecision(ctx, DecisionRequest{AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("LogDecision failed: %v", err)
	}
}

func TestImpl_EnsureModel_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	// Use os.Stdout as the writer
	err := b.EnsureModel(ctx, os.Stdout)
	if err != nil {
		t.Fatalf("EnsureModel failed: %v", err)
	}
}

func TestImpl_BuildContextPacket_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	pkt, err := b.BuildContextPacket(ctx, ContextPacketRequest{})
	if err != nil {
		t.Fatalf("BuildContextPacket failed: %v", err)
	}
	if pkt != nil {
		t.Error("expected nil packet for disabled brain")
	}
}

func TestImpl_Memorize_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	resp, err := b.Memorize(ctx, archivist.MemorizeRequest{})
	if err != nil {
		t.Fatalf("Memorize failed: %v", err)
	}
	if len(resp.NewMemories) != 0 {
		t.Error("expected no memories for disabled brain")
	}
}

func TestImpl_UpsertADR_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	err := b.UpsertADR(ADRRequest{ID: "adr-1", Title: "Test ADR"})
	if err != nil {
		t.Fatalf("UpsertADR failed: %v", err)
	}
}

func TestImpl_GetADR_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	adr, err := b.GetADR("adr-1")
	if err != nil {
		t.Fatalf("GetADR failed: %v", err)
	}
	if adr.ID != "" {
		t.Error("expected empty ADR for disabled brain")
	}
}

func TestImpl_AllADRs_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	adrs, err := b.AllADRs()
	if err != nil {
		t.Fatalf("AllADRs failed: %v", err)
	}
	if adrs != nil {
		t.Error("expected nil ADRs for disabled brain")
	}
}

func TestImpl_GetADRsForFile_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	adrs, err := b.GetADRsForFile("test.go", 10)
	if err != nil {
		t.Fatalf("GetADRsForFile failed: %v", err)
	}
	if adrs != nil {
		t.Error("expected nil ADRs for disabled brain")
	}
}

// --- Validation function tests ---

func TestValidateResponse_Empty(t *testing.T) {
	if validateResponse("", 10) {
		t.Error("empty response should fail validation")
	}
}

func TestValidateResponse_TooShort(t *testing.T) {
	if validateResponse("short", 10) {
		t.Error("response below min length should fail")
	}
}

func TestValidateResponse_ValidLength(t *testing.T) {
	if !validateResponse("this is a valid response that meets minimum length", 10) {
		t.Error("valid response should pass")
	}
}

func TestValidateResponse_RepetitiveOutput(t *testing.T) {
	// Response with single word repeated >50% of output
	resp := "test test test test test test test test test test other"
	if validateResponse(resp, 5) {
		t.Error("highly repetitive response should fail validation")
	}
}

func TestValidateResponse_NonRepetitiveOutput(t *testing.T) {
	resp := "this is a good response with various different words and ideas"
	if !validateResponse(resp, 5) {
		t.Error("non-repetitive response should pass")
	}
}

func TestValidateResponse_Whitespace(t *testing.T) {
	resp := "   valid content with spaces   "
	if !validateResponse(resp, 5) {
		t.Error("response with whitespace should pass after trimming")
	}
}

// --- TierStatus and BrainStats tests for impl ---

func TestImpl_TierStatus_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)

	// Type assert to TierStatusProvider
	provider, ok := b.(TierStatusProvider)
	if !ok {
		// NullBrain doesn't implement TierStatusProvider, which is expected
		return
	}

	status := provider.TierStatus()
	if status == nil {
		t.Error("expected non-nil status map")
	}
}

func TestImpl_BrainStats_Disabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)

	// Type assert to BrainStatsProvider
	provider, ok := b.(BrainStatsProvider)
	if !ok {
		// NullBrain doesn't implement BrainStatsProvider, which is expected
		return
	}

	stats := provider.BrainStats()
	if stats == nil {
		t.Error("expected non-nil stats map")
	}
}

// --- toContextPacket conversion tests ---

func TestToContextPacket_Empty(t *testing.T) {
	pkt := toContextPacket(&contextbuilder.Packet{})
	if pkt == nil {
		t.Fatal("toContextPacket returned nil")
	}
	if pkt.EntityName != "" {
		t.Errorf("expected empty entity name, got %q", pkt.EntityName)
	}
}

func TestToContextPacket_WithData(t *testing.T) {
	src := &contextbuilder.Packet{
		AgentID:     "agent-1",
		EntityName:  "TestFunc",
		EntityType:  "function",
		GeneratedAt: "2024-01-01T12:00:00Z",
		Phase:       "development",
		QualityMode: "standard",
		RootSummary: "A test function",
		Insight:     "This is insightful",
	}
	pkt := toContextPacket(src)
	if pkt == nil {
		t.Fatal("toContextPacket returned nil")
	}
	if pkt.AgentID != "agent-1" {
		t.Errorf("AgentID: got %q, want agent-1", pkt.AgentID)
	}
	if pkt.EntityName != "TestFunc" {
		t.Errorf("EntityName: got %q, want TestFunc", pkt.EntityName)
	}
	if pkt.RootSummary != "A test function" {
		t.Errorf("RootSummary: got %q", pkt.RootSummary)
	}
}

func TestToContextPacket_WithConstraints(t *testing.T) {
	src := &contextbuilder.Packet{
		ActiveConstraints: []contextbuilder.ConstraintItem{
			{RuleID: "r1", Severity: "error", Description: "test constraint", Hint: "test hint"},
		},
	}
	pkt := toContextPacket(src)
	if pkt == nil {
		t.Fatal("toContextPacket returned nil")
	}
	if len(pkt.ActiveConstraints) != 1 {
		t.Errorf("expected 1 constraint, got %d", len(pkt.ActiveConstraints))
	}
	if pkt.ActiveConstraints[0].RuleID != "r1" {
		t.Errorf("RuleID: got %q", pkt.ActiveConstraints[0].RuleID)
	}
}

func TestToContextPacket_WithTeamStatus(t *testing.T) {
	src := &contextbuilder.Packet{
		TeamStatus: []contextbuilder.AgentItem{
			{AgentID: "agent-1", Scope: "pkg/auth", ScopeType: "package", ExpiresIn: 3600},
		},
	}
	pkt := toContextPacket(src)
	if pkt == nil {
		t.Fatal("toContextPacket returned nil")
	}
	if len(pkt.TeamStatus) != 1 {
		t.Errorf("expected 1 team status, got %d", len(pkt.TeamStatus))
	}
	if pkt.TeamStatus[0].AgentID != "agent-1" {
		t.Errorf("AgentID: got %q", pkt.TeamStatus[0].AgentID)
	}
}

func TestToContextPacket_WithPatternHints(t *testing.T) {
	src := &contextbuilder.Packet{
		PatternHints: []contextbuilder.PatternItem{
			{Trigger: "file_change", CoChange: "test_update", Reason: "correlated", Confidence: 0.85},
		},
	}
	pkt := toContextPacket(src)
	if pkt == nil {
		t.Fatal("toContextPacket returned nil")
	}
	if len(pkt.PatternHints) != 1 {
		t.Errorf("expected 1 pattern hint, got %d", len(pkt.PatternHints))
	}
	if pkt.PatternHints[0].Trigger != "file_change" {
		t.Errorf("Trigger: got %q", pkt.PatternHints[0].Trigger)
	}
}

// --- storeADRtoBrain conversion tests ---

func TestStoreADRtoBrain_Empty(t *testing.T) {
	src := store.ADR{}
	adr := storeADRtoBrain(src)
	if adr.ID != "" {
		t.Errorf("expected empty ID, got %q", adr.ID)
	}
}

func TestStoreADRtoBrain_WithData(t *testing.T) {
	now := time.Now()
	src := store.ADR{
		ID:           "adr-1",
		Title:        "Use PostgreSQL",
		Status:       "accepted",
		ContextText:  "Need a reliable database",
		Decision:     "Use PostgreSQL over MongoDB",
		Consequences: "Learning curve, but better for our use case",
		LinkedFiles:  []string{"internal/db/postgres.go"},
		CreatedAt:    now.Format(time.RFC3339),
		UpdatedAt:    now.Format(time.RFC3339),
	}
	adr := storeADRtoBrain(src)

	if adr.ID != "adr-1" {
		t.Errorf("ID: got %q, want adr-1", adr.ID)
	}
	if adr.Title != "Use PostgreSQL" {
		t.Errorf("Title: got %q", adr.Title)
	}
	if adr.Status != "accepted" {
		t.Errorf("Status: got %q", adr.Status)
	}
	if len(adr.LinkedFiles) != 1 {
		t.Errorf("expected 1 linked file, got %d", len(adr.LinkedFiles))
	}
}

// --- Additional impl struct tests (with disabled configs) ---

func TestImpl_GetPatterns_Empty(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	patterns := b.GetPatterns("", 0)
	if patterns != nil {
		t.Error("expected nil patterns from disabled brain")
	}
}

func TestImpl_GetPatterns_WithTrigger(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	patterns := b.GetPatterns("file_change", 10)
	if patterns != nil {
		t.Error("expected nil patterns from disabled brain")
	}
}

func TestImpl_Memorize_WithEvents(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	req := archivist.MemorizeRequest{
		SessionEvents: []archivist.SessionEvent{
			{Tool: "get_context", Entity: "TestFunc", Result: "succeeded"},
			{Tool: "validate_plan", Entity: "changes", Result: "passed"},
		},
	}
	resp, err := b.Memorize(ctx, req)
	if err != nil {
		t.Fatalf("Memorize failed: %v", err)
	}
	if len(resp.NewMemories) != 0 {
		t.Error("expected no memories from disabled brain")
	}
	if len(resp.Annotations) != 0 {
		t.Error("expected no annotations from disabled brain")
	}
}

func TestImpl_Prune_MultilineContent(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	content := "Line 1\nLine 2\nLine 3"
	result, err := b.Prune(ctx, content)
	if err != nil {
		t.Fatalf("Prune failed: %v", err)
	}
	if result != content {
		t.Errorf("expected unchanged content, got %q", result)
	}
}

func TestImpl_Summary_Empty(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	summary := b.Summary("", "")
	if summary != "" {
		t.Errorf("expected empty summary for empty IDs, got %q", summary)
	}
}

func TestImpl_GetSDLCConfig_PersistentDefaults(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)

	// First call
	cfg1 := b.GetSDLCConfig()
	if cfg1.Phase != PhaseDevelopment {
		t.Errorf("expected phase development, got %q", cfg1.Phase)
	}

	// Second call should return same defaults
	cfg2 := b.GetSDLCConfig()
	if cfg2.Phase != cfg1.Phase {
		t.Error("SDLC config should be consistent")
	}
}

func TestImpl_Coordinate_Empty(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	resp, err := b.Coordinate(ctx, CoordinateRequest{})
	if err != nil {
		t.Fatalf("Coordinate failed: %v", err)
	}
	if resp.Suggestion != "" {
		t.Error("expected empty suggestion from disabled brain")
	}
}

func TestImpl_Coordinate_WithClaims(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	req := CoordinateRequest{
		NewAgentID: "agent-2",
		NewScope:   "internal/auth",
		ConflictingClaims: []WorkClaim{
			{AgentID: "agent-1", Scope: "internal/auth", ScopeType: "package"},
		},
	}
	resp, err := b.Coordinate(ctx, req)
	if err != nil {
		t.Fatalf("Coordinate failed: %v", err)
	}
	if resp.Suggestion != "" {
		t.Error("expected empty suggestion from disabled brain")
	}
}

func TestImpl_Enrich_WithNodeIDs(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	req := EnrichRequest{
		RootID:      "n1",
		RootName:    "TestFunc",
		RootType:    "function",
		AllNodeIDs:  []string{"n1", "n2", "n3"},
		CalleeNames: []string{"helper1"},
	}
	resp, err := b.Enrich(ctx, req)
	if err != nil {
		t.Fatalf("Enrich failed: %v", err)
	}
	if resp.Insight != "" {
		t.Error("expected empty insight from disabled brain")
	}
	if resp.Summaries == nil {
		t.Error("expected summaries map")
	}
}

func TestImpl_ExplainViolation_WithDetails(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	req := ViolationRequest{
		RuleID:       "no-db-in-handler",
		RuleSeverity: "error",
		Description:  "Handler should not directly access database",
		SourceFile:   "internal/api/handlers.go",
		TargetName:   "FetchUser",
	}
	resp, err := b.ExplainViolation(ctx, req)
	if err != nil {
		t.Fatalf("ExplainViolation failed: %v", err)
	}
	if resp.Explanation != "" || resp.Fix != "" {
		t.Error("expected empty explanation and fix from disabled brain")
	}
}

func TestImpl_Ingest_WithNodeDetails(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	req := IngestRequest{
		ProjectID: "myproject",
		NodeID:    "n-123",
		NodeName:  "ProcessPayment",
		NodeType:  "function",
		Package:   "internal/payments",
		Code:      "func ProcessPayment(amount int) error { ... }",
	}
	resp, err := b.Ingest(ctx, req)
	if err != nil {
		t.Fatalf("Ingest failed: %v", err)
	}
	if resp.NodeID != "n-123" {
		t.Errorf("NodeID: got %q, want n-123", resp.NodeID)
	}
	if resp.Summary != "" {
		t.Error("expected empty summary from disabled brain")
	}
}

func TestImpl_BuildContextPacket_Full(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	req := ContextPacketRequest{
		ProjectID:   "myproject",
		AgentID:     "agent-1",
		Phase:       PhaseDevelopment,
		QualityMode: QualityStandard,
		Snapshot: SynapsesSnapshotInput{
			RootNodeID:   "n-1",
			RootName:     "ProcessOrder",
			RootType:     "function",
			RootFile:     "internal/orders/process.go",
			CalleeNames:  []string{"ValidateOrder", "PaymentGateway"},
			CallerNames:  []string{"HandleOrderRequest"},
			RelatedNames: []string{"OrderService"},
			TaskID:       "task-123",
			HasTests:     true,
			FanIn:        5,
		},
	}
	resp, err := b.BuildContextPacket(ctx, req)
	if err != nil {
		t.Fatalf("BuildContextPacket failed: %v", err)
	}
	if resp != nil {
		t.Error("expected nil packet from disabled brain")
	}
}

func TestImpl_LogDecision_WithOutcome(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enabled: false}
	b := New(cfg)
	ctx := context.Background()
	req := DecisionRequest{
		AgentID:         "agent-1",
		Phase:           string(PhaseDevelopment),
		EntityName:      "ProcessPayment",
		Action:          "refactored",
		RelatedEntities: []string{"PaymentValidator", "TransactionLogger"},
		Outcome:         "improved_code_quality",
		Notes:           "Split payment validation into separate module",
	}
	err := b.LogDecision(ctx, req)
	if err != nil {
		t.Fatalf("LogDecision failed: %v", err)
	}
}

// --- impl with mocked LLM clients ---

type mockLLMClient struct {
	modelName    string
	isAvailable  bool
	modelPulled  bool
	shouldError  bool
}

func (m *mockLLMClient) ModelName() string                              { return m.modelName }
func (m *mockLLMClient) Available(context.Context) bool                { return m.isAvailable }
func (m *mockLLMClient) ModelPulled(context.Context) bool              { return m.modelPulled }
func (m *mockLLMClient) PullModel(context.Context, io.Writer) error {
	if m.shouldError {
		return fmt.Errorf("mock pull error")
	}
	return nil
}
func (m *mockLLMClient) Generate(context.Context, string) (string, error) {
	return "", nil
}

// --- Test impl methods with mock LLM ---

func TestImpl_Available_WithMockLLM(t *testing.T) {
	b := &impl{
		llm: &mockLLMClient{isAvailable: true},
	}
	if !b.Available() {
		t.Error("expected brain to be available")
	}
}

func TestImpl_Available_Unavailable(t *testing.T) {
	b := &impl{
		llm: &mockLLMClient{isAvailable: false},
	}
	if b.Available() {
		t.Error("expected brain to be unavailable")
	}
}

func TestImpl_ModelName_WithMockLLM(t *testing.T) {
	expectedModel := "qwen3.5:2b"
	b := &impl{
		llm: &mockLLMClient{modelName: expectedModel},
	}
	if b.ModelName() != expectedModel {
		t.Errorf("ModelName: got %q, want %q", b.ModelName(), expectedModel)
	}
}

func TestImpl_EnsureModel_AlreadyPulled(t *testing.T) {
	b := &impl{
		llm: &mockLLMClient{modelPulled: true},
	}
	err := b.EnsureModel(context.Background(), os.Stdout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestImpl_EnsureModel_NeedsPull(t *testing.T) {
	b := &impl{
		llm: &mockLLMClient{modelPulled: false, shouldError: false},
	}
	err := b.EnsureModel(context.Background(), os.Stdout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestImpl_EnsureModel_PullFails(t *testing.T) {
	b := &impl{
		llm: &mockLLMClient{modelPulled: false, shouldError: true},
	}
	err := b.EnsureModel(context.Background(), os.Stdout)
	if err == nil {
		t.Error("expected error when pull fails")
	}
}

func TestImpl_Summary_WithNilStore(t *testing.T) {
	b := &impl{
		store: nil,
	}
	summary := b.Summary("proj", "node")
	if summary != "" {
		t.Errorf("expected empty summary, got %q", summary)
	}
}

func TestImpl_CoordinateFallback_Basic(t *testing.T) {
	b := &impl{}
	req := CoordinateRequest{
		NewAgentID: "agent-2",
		NewScope:   "internal/auth",
		ConflictingClaims: []WorkClaim{
			{AgentID: "agent-1", Scope: "internal/auth", ScopeType: "package"},
		},
	}
	resp, err := b.coordinateFallback(context.Background(), req)
	if err != nil {
		t.Fatalf("coordinateFallback: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion from deterministic coordinator")
	}
	if !resp.Degraded {
		t.Error("expected Degraded flag to be true")
	}
}

func TestImpl_CoordinateFallback_MultipleConflicts(t *testing.T) {
	b := &impl{}
	req := CoordinateRequest{
		NewAgentID: "agent-3",
		NewScope:   "internal/auth",
		ConflictingClaims: []WorkClaim{
			{AgentID: "agent-1", Scope: "internal/auth", ScopeType: "package"},
			{AgentID: "agent-2", Scope: "internal/auth", ScopeType: "package"},
		},
	}
	resp, err := b.coordinateFallback(context.Background(), req)
	if err != nil {
		t.Fatalf("coordinateFallback: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion from deterministic coordinator")
	}
}

func TestImpl_CoordinateFallback_EmptyClaims(t *testing.T) {
	b := &impl{}
	req := CoordinateRequest{
		NewAgentID:        "agent-1",
		NewScope:          "internal/auth",
		ConflictingClaims: []WorkClaim{},
	}
	resp, err := b.coordinateFallback(context.Background(), req)
	if err != nil {
		t.Fatalf("coordinateFallback: %v", err)
	}
	if resp.Suggestion == "" {
		t.Error("expected non-empty suggestion even with no conflicts")
	}
}

func TestCircuitBreaker_Snapshot(t *testing.T) {
	cb := newCircuitBreaker(3, 5*time.Minute)
	cb.recordFailure("ingest")
	status := cb.status()

	if status["ingest"].Open {
		t.Error("should not be open after 1 failure")
	}
	if status["ingest"].Failures != 1 {
		t.Errorf("failures: got %d, want 1", status["ingest"].Failures)
	}
}

func TestCircuitBreaker_DifferentTiers(t *testing.T) {
	cb := newCircuitBreaker(2, 5*time.Minute)

	// Trip ingest
	cb.recordFailure("ingest")
	cb.recordFailure("ingest")

	// Enrich should still be fine
	if cb.isOpen("enrich") {
		t.Error("enrich should not be affected by ingest failures")
	}

	// Trip guardian
	cb.recordFailure("guardian")
	cb.recordFailure("guardian")

	if !cb.isOpen("ingest") {
		t.Error("ingest should be open")
	}
	if !cb.isOpen("guardian") {
		t.Error("guardian should be open")
	}
	if cb.isOpen("enrich") {
		t.Error("enrich should not be open")
	}
}

func TestImpl_WarmUpModels_Empty(t *testing.T) {
	// Should not panic with empty client list
	warmUpModels(context.Background())
}

func TestImpl_WarmUpModels_WithNilClients(t *testing.T) {
	// Should not panic with nil clients mixed in
	warmUpModels(context.Background(), nil, nil)
}

func TestImpl_ValidateResponse_EdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		minLen    int
		expected  bool
	}{
		{"empty", "", 10, false},
		{"single word", "test", 5, false},
		{"minimal valid", "hello world", 5, true},
		// Only checks repetition if > 10 words
		{"all same word (>10)", "test test test test test test test test test test test", 5, false},
		{"mostly same word (>10)", "test test test test test test test test test word1 word2", 5, false},
		{"varied", "the quick brown fox jumps over lazy dog and more text", 5, true},
		{"with whitespace", "   test content here and more   ", 5, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validateResponse(tt.response, tt.minLen)
			if result != tt.expected {
				t.Errorf("validateResponse(%q, %d) = %v, want %v",
					tt.response, tt.minLen, result, tt.expected)
			}
		})
	}
}

func TestImpl_New_NilConfig(t *testing.T) {
	// Using empty config should return NullBrain since Enabled defaults to false
	cfg := brainconfig.BrainConfig{}
	b := New(cfg)
	if _, ok := b.(*NullBrain); !ok {
		t.Errorf("expected NullBrain, got %T", b)
	}
}

func TestImpl_New_LocalBackendWithoutGGUFPath(t *testing.T) {
	cfg := brainconfig.BrainConfig{
		Enabled: true,
		Backend: "local",
	}
	b := New(cfg)
	// Should fall back to NullBrain since no GGUF path
	if _, ok := b.(*NullBrain); !ok {
		t.Errorf("expected NullBrain fallback, got %T", b)
	}
}

// --- Additional coverage tests ---

// TestImpl_TierStatus_Integration tests that TierStatus returns circuit breaker state
func TestImpl_TierStatus_Integration(t *testing.T) {
	b := &impl{
		cb: newCircuitBreaker(3, 5*time.Minute),
	}

	status := b.TierStatus()
	if status == nil {
		t.Error("TierStatus should return non-nil map")
	}
}

// TestImpl_BrainStats_Integration tests that BrainStats returns snapshot
func TestImpl_BrainStats_Integration(t *testing.T) {
	b := &impl{}
	b.stats.record("ingest", true, 50)
	b.stats.record("enrich", false, 100)

	result := b.BrainStats()
	if result == nil {
		t.Error("BrainStats should return non-nil map")
	}
	if count, ok := result["ingest_success"]; !ok || count != int64(1) {
		t.Errorf("ingest_success: got %v, want 1", count)
	}
}

