package brain

import (
	"context"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
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

func TestClient_Ingest(t *testing.T) {
	c := NewClient("", 0)
	c.Ingest(context.Background(), IngestRequest{NodeID: "n1"})
}

func TestClient_BulkIngest(t *testing.T) {
	c := NewClient("", 0)
	c.BulkIngest(context.Background(), []IngestRequest{
		{NodeID: "n1"},
		{NodeID: "n2"},
	})
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

func TestClient_GetSDLC(t *testing.T) {
	c := NewClient("", 0)
	phase, mode := c.GetSDLC(context.Background())
	if phase != "development" {
		t.Errorf("phase: got %q, want development", phase)
	}
	if mode != "standard" {
		t.Errorf("mode: got %q, want standard", mode)
	}
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
