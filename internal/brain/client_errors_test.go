package brain_test

// Tests covering error paths in brain.Client:
// HTTP non-2xx responses, JSON decode failures, put() error path.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain"
)

// newBrainServerWithStatus returns a server that always responds with statusCode.
func newBrainServerWithStatus(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(`{"error":"intentional test error"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newBrainServerWithBody returns a server that writes the given body for any request.
func newBrainServerWithBody(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ── HealthCheck error paths ───────────────────────────────────────────────────

func TestHealthCheck_Non200(t *testing.T) {
	srv := newBrainServerWithStatus(t, http.StatusServiceUnavailable)
	c := brain.NewClient(srv.URL, 5)
	_, err := c.HealthCheck(ctx)
	if err == nil {
		t.Error("expected error for non-200 health check")
	}
}

func TestHealthCheck_InvalidJSON(t *testing.T) {
	// Server returns 200 but with invalid JSON → returns "unknown", nil.
	srv := newBrainServerWithBody(t, http.StatusOK, "not-json")
	c := brain.NewClient(srv.URL, 5)
	model, err := c.HealthCheck(ctx)
	if err != nil {
		t.Fatalf("unexpected error for invalid JSON: %v", err)
	}
	if model != "unknown" {
		t.Errorf("expected 'unknown' for invalid JSON, got %q", model)
	}
}

// ── GetSDLC error path ────────────────────────────────────────────────────────

func TestGetSDLC_Non200(t *testing.T) {
	// Server returns 500 → should fall back to defaults.
	srv := newBrainServerWithStatus(t, http.StatusInternalServerError)
	c := brain.NewClient(srv.URL, 5)
	phase, mode := c.GetSDLC(ctx)
	if phase != "development" || mode != "standard" {
		t.Errorf("expected defaults on error, got phase=%q mode=%q", phase, mode)
	}
}

// ── GetSummary error path ─────────────────────────────────────────────────────

func TestGetSummary_Non200(t *testing.T) {
	srv := newBrainServerWithStatus(t, http.StatusNotFound)
	c := brain.NewClient(srv.URL, 5)
	s := c.GetSummary(ctx, "some::node::id")
	if s != "" {
		t.Errorf("expected empty string for 404, got %q", s)
	}
}

// ── UpsertADR error path ──────────────────────────────────────────────────────

func TestUpsertADR_Non200(t *testing.T) {
	srv := newBrainServerWithStatus(t, http.StatusBadRequest)
	c := brain.NewClient(srv.URL, 5)
	_, err := c.UpsertADR(ctx, brain.ADRRequest{Title: "bad"})
	if err == nil {
		t.Error("expected error for 400 UpsertADR")
	}
}

// ── GetADR error path ─────────────────────────────────────────────────────────

func TestGetADR_ServerError(t *testing.T) {
	srv := newBrainServerWithStatus(t, http.StatusInternalServerError)
	c := brain.NewClient(srv.URL, 5)
	_, err := c.GetADR(ctx, "adr-1")
	if err == nil {
		t.Error("expected error for 500 GetADR")
	}
}

// ── GetADRs error path ────────────────────────────────────────────────────────

func TestGetADRs_ServerError(t *testing.T) {
	srv := newBrainServerWithStatus(t, http.StatusInternalServerError)
	c := brain.NewClient(srv.URL, 5)
	_, err := c.GetADRs(ctx, "")
	if err == nil {
		t.Error("expected error for 500 GetADRs")
	}
}

// ── SetPhase uses put() ───────────────────────────────────────────────────────

func TestSetPhase_Non200(t *testing.T) {
	srv := newBrainServerWithStatus(t, http.StatusBadRequest)
	c := brain.NewClient(srv.URL, 5)
	_, err := c.SetPhase(ctx, brain.SetPhaseRequest{Phase: "staging"})
	if err == nil {
		t.Error("expected error for 400 SetPhase")
	}
}

func TestSetPhase_InvalidJSON(t *testing.T) {
	// Server returns 200 but with invalid JSON → decode error.
	srv := newBrainServerWithBody(t, http.StatusOK, "not-valid-json{{{")
	c := brain.NewClient(srv.URL, 5)
	_, err := c.SetPhase(ctx, brain.SetPhaseRequest{Phase: "staging"})
	if err == nil {
		t.Error("expected decode error for invalid JSON in SetPhase")
	}
}

// ── post() HTTP error body coverage ──────────────────────────────────────────

func TestBuildContextPacket_Non200(t *testing.T) {
	// post() returns error on non-2xx → BuildContextPacket returns nil.
	srv := newBrainServerWithStatus(t, http.StatusInternalServerError)
	c := brain.NewClient(srv.URL, 5)
	pkt := c.BuildContextPacket(ctx, brain.ContextPacketRequest{AgentID: "x"})
	if pkt != nil {
		t.Error("expected nil packet for 500 response")
	}
}

func TestExplainViolation_Non200(t *testing.T) {
	srv := newBrainServerWithStatus(t, http.StatusInternalServerError)
	c := brain.NewClient(srv.URL, 5)
	exp, fix := c.ExplainViolation(ctx, brain.ViolationRequest{})
	if exp != "" || fix != "" {
		t.Error("expected empty strings for 500 ExplainViolation")
	}
}

func TestCoordinate_Non200(t *testing.T) {
	srv := newBrainServerWithStatus(t, http.StatusInternalServerError)
	c := brain.NewClient(srv.URL, 5)
	s := c.Coordinate(ctx, brain.CoordinateRequest{})
	if s != "" {
		t.Errorf("expected empty string for 500 Coordinate, got %q", s)
	}
}
