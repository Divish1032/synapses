package brain_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain"
)

// newBrainServer returns a test server that serves simple JSON for all endpoints.
func newBrainServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"model": "test-model"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/context-packet", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.ContextPacket{EntityName: "Login"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/ingest", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/explain-violation", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.ViolationExplanation{Explanation: "why", Fix: "fix"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/coordinate", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.CoordinateResponse{Suggestion: "use mutex"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/sdlc", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.SDLCConfig{Phase: "production", QualityMode: "strict"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/sdlc/phase", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.SDLCConfig{Phase: "production", QualityMode: "strict"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/adr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string][]brain.ADR{"adrs": {}}) //nolint:errcheck
		} else {
			json.NewEncoder(w).Encode(brain.ADR{ID: "adr-1", Title: "Use SQLite"}) //nolint:errcheck
		}
	})
	mux.HandleFunc("/v1/adr/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(brain.ADR{ID: "adr-1", Title: "Use SQLite"}) //nolint:errcheck
	})
	mux.HandleFunc("/v1/decision", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/summary/", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"summary": "handles auth"}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var ctx = context.Background()

func TestNewClient_DefaultTimeout(t *testing.T) {
	c := brain.NewClient("http://localhost:11435", 0)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_CustomTimeout(t *testing.T) {
	c := brain.NewClient("http://localhost:11435", 10)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestHealthCheck_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	model, err := c.HealthCheck(ctx)
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if model != "test-model" {
		t.Errorf("got model %q, want %q", model, "test-model")
	}
}

func TestHealthCheck_Unreachable(t *testing.T) {
	c := brain.NewClient("http://127.0.0.1:19999", 1)
	_, err := c.HealthCheck(ctx)
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

func TestBuildContextPacket_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	pkt := c.BuildContextPacket(ctx, brain.ContextPacketRequest{
		AgentID: "agent-a",
	})
	if pkt == nil {
		t.Fatal("expected non-nil context packet")
	}
}

func TestBuildContextPacket_Unreachable(t *testing.T) {
	c := brain.NewClient("http://127.0.0.1:19999", 1)
	pkt := c.BuildContextPacket(ctx, brain.ContextPacketRequest{AgentID: "x"})
	if pkt != nil {
		t.Error("expected nil for unreachable brain")
	}
}

func TestIngest_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	// Fire-and-forget — just verify no panic.
	c.Ingest(ctx, brain.IngestRequest{NodeID: "x", NodeName: "Login", NodeType: "function"})
}

func TestBulkIngest_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	nodes := []brain.IngestRequest{
		{NodeID: "x", NodeName: "Login"},
		{NodeID: "y", NodeName: "Logout"},
	}
	c.BulkIngest(ctx, nodes) // must not panic
}

func TestExplainViolation_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	exp, fix := c.ExplainViolation(ctx, brain.ViolationRequest{RuleID: "no-db-in-mcp"})
	if exp == "" {
		t.Error("expected non-empty explanation")
	}
	if fix == "" {
		t.Error("expected non-empty fix")
	}
}

func TestExplainViolation_Unreachable(t *testing.T) {
	c := brain.NewClient("http://127.0.0.1:19999", 1)
	exp, fix := c.ExplainViolation(ctx, brain.ViolationRequest{})
	if exp != "" || fix != "" {
		t.Error("expected empty strings for unreachable brain")
	}
}

func TestCoordinate_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	suggestion := c.Coordinate(ctx, brain.CoordinateRequest{NewAgentID: "a", NewScope: "pkg/auth"})
	if suggestion == "" {
		t.Error("expected non-empty suggestion")
	}
}

func TestCoordinate_Unreachable(t *testing.T) {
	c := brain.NewClient("http://127.0.0.1:19999", 1)
	if s := c.Coordinate(ctx, brain.CoordinateRequest{NewAgentID: "x"}); s != "" {
		t.Error("expected empty string for unreachable brain")
	}
}

func TestGetSummary_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	summary := c.GetSummary(ctx, "repo::auth.go::Login")
	if summary == "" {
		t.Error("expected non-empty summary")
	}
}

func TestGetSummary_Unreachable(t *testing.T) {
	c := brain.NewClient("http://127.0.0.1:19999", 1)
	if s := c.GetSummary(ctx, "x"); s != "" {
		t.Error("expected empty string for unreachable brain")
	}
}

func TestLogDecision_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	c.LogDecision(ctx, brain.DecisionRequest{AgentID: "a", Action: "use Redis"})
}

func TestGetSDLC_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	phase, mode := c.GetSDLC(ctx)
	if phase != "production" {
		t.Errorf("got phase %q, want %q", phase, "production")
	}
	if mode != "strict" {
		t.Errorf("got mode %q, want %q", mode, "strict")
	}
}

func TestGetSDLC_Unreachable(t *testing.T) {
	c := brain.NewClient("http://127.0.0.1:19999", 1)
	phase, mode := c.GetSDLC(ctx)
	if phase != "development" || mode != "standard" {
		t.Errorf("expected default values, got phase=%q mode=%q", phase, mode)
	}
}

func TestSetPhase_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	cfg, err := c.SetPhase(ctx, brain.SetPhaseRequest{Phase: "production"})
	if err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil SDLCConfig")
	}
}

func TestSetPhase_Unreachable(t *testing.T) {
	c := brain.NewClient("http://127.0.0.1:19999", 1)
	_, err := c.SetPhase(ctx, brain.SetPhaseRequest{Phase: "x"})
	if err == nil {
		t.Error("expected error for unreachable brain")
	}
}

func TestUpsertADR_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	adr, err := c.UpsertADR(ctx, brain.ADRRequest{Title: "Use SQLite", Status: "accepted"})
	if err != nil {
		t.Fatalf("UpsertADR: %v", err)
	}
	if adr == nil {
		t.Fatal("expected non-nil ADR")
	}
}

func TestGetADR_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	adr, err := c.GetADR(ctx, "adr-1")
	if err != nil {
		t.Fatalf("GetADR: %v", err)
	}
	if adr == nil {
		t.Fatal("expected non-nil ADR")
	}
}

func TestGetADR_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/adr/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := brain.NewClient(srv.URL, 5)
	_, err := c.GetADR(ctx, "missing")
	if err == nil {
		t.Error("expected error for 404")
	}
}

func TestGetADRs_Success(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	adrs, err := c.GetADRs(ctx, "")
	if err != nil {
		t.Fatalf("GetADRs: %v", err)
	}
	_ = adrs
}

func TestGetADRs_WithFileFilter(t *testing.T) {
	srv := newBrainServer(t)
	c := brain.NewClient(srv.URL, 5)
	adrs, err := c.GetADRs(ctx, "internal/auth/auth.go")
	if err != nil {
		t.Fatalf("GetADRs: %v", err)
	}
	_ = adrs
}
