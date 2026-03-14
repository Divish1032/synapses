package guardian

import (
	"context"
	"io"
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

func TestExplain_Success(t *testing.T) {
	mock := llm.NewMockClient(`{"explanation": "You are importing a database package directly from a view component, which breaks the Clean Architecture rule.", "fix": "Create a service layer that mediates between the view and the database."}`)
	st := newTestStore(t)
	g := New(mock, st, 3*time.Second)

	resp, err := g.Explain(context.Background(), Request{
		RuleID:       "no-db-in-view",
		RuleSeverity: "error",
		Description:  "View components must not import database packages",
		SourceFile:   "ui/user_view.go",
		TargetName:   "db/client",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Explanation == "" {
		t.Error("expected non-empty explanation")
	}
	if resp.Fix == "" {
		t.Error("expected non-empty fix")
	}
}

func TestExplain_CachesResult(t *testing.T) {
	callCount := 0
	// Use a custom mock that tracks calls.
	mock := &countingMock{
		response: `{"explanation": "Violation explanation.", "fix": "Fix here."}`,
		count:    &callCount,
	}
	st := newTestStore(t)
	g := New(mock, st, 3*time.Second)

	req := Request{
		RuleID:     "rule-1",
		SourceFile: "some/file.go",
		TargetName: "forbidden/pkg",
	}

	// First call: should invoke LLM.
	_, err := g.Explain(context.Background(), req)
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected 1 LLM call, got %d", callCount)
	}

	// Second call: should use cache, no LLM call.
	_, err = g.Explain(context.Background(), req)
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if callCount != 1 {
		t.Errorf("expected still 1 LLM call after cache hit, got %d", callCount)
	}
}

func TestExplain_DifferentFilesDontShareCache(t *testing.T) {
	callCount := 0
	mock := &countingMock{
		response: `{"explanation": "Explanation.", "fix": "Fix."}`,
		count:    &callCount,
	}
	st := newTestStore(t)
	g := New(mock, st, 3*time.Second)

	req1 := Request{RuleID: "rule-1", SourceFile: "file_a.go", TargetName: "pkg"}
	req2 := Request{RuleID: "rule-1", SourceFile: "file_b.go", TargetName: "pkg"}

	g.Explain(context.Background(), req1)
	g.Explain(context.Background(), req2)

	if callCount != 2 {
		t.Errorf("expected 2 LLM calls for different files, got %d", callCount)
	}
}

// countingMock is a mock LLM client that counts Generate calls.
type countingMock struct {
	response string
	count    *int
}

func (m *countingMock) Generate(_ context.Context, _ string) (string, error) {
	*m.count++
	return m.response, nil
}
func (m *countingMock) Available(_ context.Context) bool               { return true }
func (m *countingMock) ModelName() string                              { return "mock" }
func (m *countingMock) ModelPulled(_ context.Context) bool             { return true }
func (m *countingMock) PullModel(_ context.Context, _ io.Writer) error { return nil }
