package enricher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
	"github.com/SynapsesOS/synapses/internal/brain/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "brain.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestEnrich_Success(t *testing.T) {
	mock := llm.NewMockClient(`{"insight": "AuthService is the central authentication hub. It validates tokens and enforces rate limits.", "concerns": ["handles JWTs", "rate limit boundary"]}`)
	st := newTestStore(t)
	e := New(mock, st, 3*time.Second)

	resp, err := e.Enrich(context.Background(), Request{
		RootName:    "AuthService",
		RootType:    "struct",
		CalleeNames: []string{"TokenValidator", "RateLimiter"},
		CallerNames: []string{"LoginHandler"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Insight == "" {
		t.Error("expected non-empty insight")
	}
	if len(resp.Concerns) == 0 {
		t.Error("expected at least one concern")
	}
}

func TestEnrich_LLMFailure_ReturnsDeterministicFields(t *testing.T) {
	// Enrich is fail-silent on LLM errors. The deterministic pass always runs
	// and returns Phase + ComplexityScore. The heuristic fallback also populates
	// Insight so get_context always delivers a non-empty Insight to agents.
	mock := &llm.MockClient{Err: os.ErrDeadlineExceeded}
	st := newTestStore(t)
	e := New(mock, st, 3*time.Second)

	resp, err := e.Enrich(context.Background(), Request{
		RootName:    "X",
		RootFile:    "internal/store/store.go",
		CalleeNames: []string{"A", "B"},
		FanIn:       5,
	})
	if err != nil {
		t.Fatalf("expected nil error (fail-silent), got: %v", err)
	}
	// Heuristic fallback: Insight must be non-empty even when LLM fails.
	if resp.Insight == "" {
		t.Error("expected non-empty heuristic insight when LLM fails")
	}
	if !resp.DeterministicHit {
		t.Error("expected DeterministicHit=true even when LLM fails")
	}
	if resp.Phase != "persistence" {
		t.Errorf("expected Phase=persistence for internal/store/ path, got %q", resp.Phase)
	}
	// ComplexityScore = (5+2) * (1 + 2/10.0) = 7 * 1.2 = 8.4
	if resp.ComplexityScore == 0 {
		t.Error("expected non-zero ComplexityScore when FanIn>0")
	}
}

func TestEnrich_EmptyCallers(t *testing.T) {
	mock := llm.NewMockClient(`{"insight": "A standalone utility with no dependencies.", "concerns": []}`)
	st := newTestStore(t)
	e := New(mock, st, 3*time.Second)

	resp, err := e.Enrich(context.Background(), Request{
		RootName: "HashUtil",
		RootType: "function",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Insight == "" {
		t.Error("expected non-empty insight")
	}
}

func TestEnrich_DeterministicAlwaysPopulated(t *testing.T) {
	// Deterministic fields must be populated even when LLM succeeds.
	mock := llm.NewMockClient(`{"insight": "Handles persistence.", "concerns": ["SQL correctness"]}`)
	st := newTestStore(t)
	e := New(mock, st, 3*time.Second)

	resp, err := e.Enrich(context.Background(), Request{
		RootName:    "Store",
		RootFile:    "internal/store/store.go",
		CalleeNames: []string{"db.Exec", "db.Query"},
		FanIn:       10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.DeterministicHit {
		t.Error("DeterministicHit must be true even when LLM succeeds")
	}
	if resp.Phase != "persistence" {
		t.Errorf("expected Phase=persistence, got %q", resp.Phase)
	}
	if resp.ComplexityScore == 0 {
		t.Error("expected non-zero ComplexityScore")
	}
	if resp.Insight == "" {
		t.Error("expected LLM insight when LLM succeeds")
	}
}

func TestDeterministicPhase(t *testing.T) {
	cases := []struct {
		file  string
		phase string
	}{
		{"internal/store/store.go", "persistence"},
		{"internal/db/migrations.go", "persistence"},
		{"internal/mcp/tools.go", "api"},
		{"cmd/synapses/main.go", "entry_point"},
		{"main.go", "entry_point"},
		{"internal/graph/graph_test.go", "test"},
		{"auth_test.go", "test"},
		{"internal/graph/traverse.go", "core"},
		{"pkg/brain/client.go", "core"},
		{"router/v1.go", "api"},
		{"handler/users.go", "api"},
		{"server.go", "api"},
		{"", ""},
		{"vendor/github.com/foo/bar.go", ""}, // no matching pattern — vendor paths return empty
	}
	for _, tc := range cases {
		got := deterministicPhase(tc.file)
		if got != tc.phase {
			t.Errorf("deterministicPhase(%q) = %q, want %q", tc.file, got, tc.phase)
		}
	}
}

func TestDeterministicComplexity(t *testing.T) {
	cases := []struct {
		fanIn  int
		fanOut int
		want   float64
	}{
		{0, 0, 0.0},
		{1, 0, 1.0},   // (1+0) * (1 + 0/10) = 1.0
		{0, 10, 20.0}, // (0+10) * (1 + 10/10) = 10*2 = 20
		{5, 2, 8.4},   // (5+2) * (1 + 2/10) = 7 * 1.2 = 8.4
		{10, 10, 40.0}, // (10+10) * (1+10/10) = 20*2 = 40
	}
	const eps = 1e-9
	for _, tc := range cases {
		got := deterministicComplexity(tc.fanIn, tc.fanOut)
		diff := got - tc.want
		if diff < -eps || diff > eps {
			t.Errorf("deterministicComplexity(%d,%d) = %v, want %v", tc.fanIn, tc.fanOut, got, tc.want)
		}
	}
}

func TestEnricherStats(t *testing.T) {
	mock := llm.NewMockClient(`{"insight": "ok", "concerns": []}`)
	st := newTestStore(t)
	e := New(mock, st, 3*time.Second)

	// Zero before any calls.
	s := e.Stats()
	if s.DeterministicHits != 0 || s.OllamaCalls != 0 {
		t.Errorf("expected zero stats, got %+v", s)
	}

	_, _ = e.Enrich(context.Background(), Request{RootName: "X"})
	s = e.Stats()
	if s.DeterministicHits != 1 {
		t.Errorf("expected DeterministicHits=1, got %d", s.DeterministicHits)
	}
	if s.OllamaCalls != 1 {
		t.Errorf("expected OllamaCalls=1, got %d", s.OllamaCalls)
	}

	// LLM failure: deterministic hits but no ollama call increment.
	failMock := &llm.MockClient{Err: os.ErrDeadlineExceeded}
	e2 := New(failMock, st, 3*time.Second)
	_, _ = e2.Enrich(context.Background(), Request{RootName: "Y"})
	s2 := e2.Stats()
	if s2.DeterministicHits != 1 {
		t.Errorf("expected DeterministicHits=1, got %d", s2.DeterministicHits)
	}
	if s2.OllamaCalls != 0 {
		t.Errorf("expected OllamaCalls=0 on LLM failure, got %d", s2.OllamaCalls)
	}
}

func TestJoinNames(t *testing.T) {
	cases := []struct {
		names []string
		n     int
		want  string
	}{
		{[]string{"A", "B", "C"}, 5, "A, B, C"},
		{[]string{"A", "B", "C", "D", "E", "F"}, 5, "A, B, C, D, E"},
		{nil, 5, ""},
	}
	for _, tc := range cases {
		got := joinNames(tc.names, tc.n)
		if got != tc.want {
			t.Errorf("joinNames(%v, %d) = %q, want %q", tc.names, tc.n, got, tc.want)
		}
	}
}
