package brain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain"
)

// TestNewInProcess_Disabled verifies that a nil config returns a non-nil NullBrain client.
func TestNewInProcess_Disabled(t *testing.T) {
	c := brain.NewInProcess(nil)
	if c == nil {
		t.Fatal("expected non-nil client for nil config")
	}
}

// TestNewClient_BackwardsCompat verifies the deprecated constructor doesn't panic.
func TestNewClient_BackwardsCompat(t *testing.T) {
	c := brain.NewClient("http://ignored", 5)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

// TestHealthCheck_Disabled verifies HealthCheck on a NullBrain client is safe.
func TestHealthCheck_Disabled(t *testing.T) {
	c := brain.NewInProcess(nil)
	_, err := c.HealthCheck(context.Background())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestBuildContextPacket_NullBrain verifies BuildContextPacket returns nil on NullBrain.
func TestBuildContextPacket_NullBrain(t *testing.T) {
	c := brain.NewInProcess(nil)
	pkt := c.BuildContextPacket(context.Background(), brain.ContextPacketRequest{AgentID: "test"})
	if pkt != nil {
		t.Errorf("expected nil packet from NullBrain, got %+v", pkt)
	}
}

// TestIngest_NilSafe verifies Ingest doesn't panic on NullBrain.
func TestIngest_NilSafe(t *testing.T) {
	c := brain.NewInProcess(nil)
	c.Ingest(context.Background(), brain.IngestRequest{NodeID: "x", Code: "func Foo() {}"})
}

// TestExplainViolation_NullBrain verifies ExplainViolation returns empty strings.
func TestExplainViolation_NullBrain(t *testing.T) {
	c := brain.NewInProcess(nil)
	exp, fix := c.ExplainViolation(context.Background(), brain.ViolationRequest{RuleID: "R01"})
	if exp != "" || fix != "" {
		t.Errorf("expected empty strings from NullBrain, got (%q, %q)", exp, fix)
	}
}

// TestGetSummary_NullBrain verifies GetSummary returns "" on NullBrain.
func TestGetSummary_NullBrain(t *testing.T) {
	c := brain.NewInProcess(nil)
	s := c.GetSummary(context.Background(), "some-node-id")
	if s != "" {
		t.Errorf("expected empty summary, got %q", s)
	}
}

// TestGetADRs_NullBrain verifies GetADRs returns empty slice without error.
func TestGetADRs_NullBrain(t *testing.T) {
	c := brain.NewInProcess(nil)
	adrs, err := c.GetADRs(context.Background(), "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(adrs) != 0 {
		t.Errorf("expected empty ADRs, got %v", adrs)
	}
}

// TestClose_NullBrain verifies Close doesn't panic.
func TestClose_NullBrain(t *testing.T) {
	c := brain.NewInProcess(nil)
	c.Close() // must not panic
}

// ── GenerateHypothetical ──────────────────────────────────────────────────────

// TestGenerateHypothetical_NullBrain verifies that GenerateHypothetical returns
// "" when the brain is disabled (NullBrain path).
func TestGenerateHypothetical_NullBrain(t *testing.T) {
	c := brain.NewInProcess(nil)
	hyp := c.GenerateHypothetical(context.Background(), "rate limiter")
	if hyp != "" {
		t.Errorf("expected empty hypothesis from NullBrain, got %q", hyp)
	}
}

// TestGenerateHypothetical_CancelledContext verifies that a cancelled context
// causes GenerateHypothetical to return "" (timeout/error path → raw query fallback).
func TestGenerateHypothetical_CancelledContext(t *testing.T) {
	c := brain.NewInProcess(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled
	hyp := c.GenerateHypothetical(ctx, "authentication middleware")
	// NullBrain returns "" regardless; what matters is it doesn't panic.
	if hyp != "" {
		t.Errorf("expected empty hypothesis on cancelled context, got %q", hyp)
	}
}

// TestGenerateHypothetical_QueryWithQuotes verifies that a query containing
// embedded double-quotes does not panic and returns "" from NullBrain.
// (Production path: fmt.Sprintf %q safely escapes the query in the prompt.)
func TestGenerateHypothetical_QueryWithQuotes(t *testing.T) {
	c := brain.NewInProcess(nil)
	hyp := c.GenerateHypothetical(context.Background(), `rate "limiting" strategy`)
	if hyp != "" {
		t.Errorf("expected empty hypothesis from NullBrain, got %q", hyp)
	}
}

// TestGenerateHypothetical_VeryLongQuery verifies that a very long query (>150 runes)
// does not panic and is handled gracefully (NullBrain returns "").
func TestGenerateHypothetical_VeryLongQuery(t *testing.T) {
	c := brain.NewInProcess(nil)
	longQuery := strings.Repeat("authentication middleware rate limiting circuit breaker ", 20)
	hyp := c.GenerateHypothetical(context.Background(), longQuery)
	if hyp != "" {
		t.Errorf("expected empty hypothesis from NullBrain, got %q", hyp)
	}
}
