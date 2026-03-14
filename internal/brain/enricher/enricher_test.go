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

func TestEnrich_LLMFailure_ReturnsError(t *testing.T) {
	mock := &llm.MockClient{Err: os.ErrDeadlineExceeded}
	st := newTestStore(t)
	e := New(mock, st, 3*time.Second)

	_, err := e.Enrich(context.Background(), Request{RootName: "X"})
	if err == nil {
		t.Fatal("expected error when LLM fails, got nil")
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
