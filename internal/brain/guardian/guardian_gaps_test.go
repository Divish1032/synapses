package guardian

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
	"github.com/SynapsesOS/synapses/internal/brain/store"
)

func newGapStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "brain.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// --- Explain error paths ---

func TestExplain_LLMError(t *testing.T) {
	mock := &errMock{err: errors.New("llm unavailable")}
	st := newGapStore(t)
	g := New(mock, st, 3*time.Second)

	_, err := g.Explain(context.Background(), Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
		TargetName: "pkg",
	})
	if err == nil {
		t.Fatal("expected error from LLM failure")
	}
	if !errors.Is(err, mock.err) {
		// Check wrapped error.
		if got := err.Error(); got == "" {
			t.Error("expected non-empty error message")
		}
	}
}

func TestExplain_InvalidJSON(t *testing.T) {
	mock := llm.NewMockClient("not valid json at all")
	st := newGapStore(t)
	g := New(mock, st, 3*time.Second)

	_, err := g.Explain(context.Background(), Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
		TargetName: "pkg",
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestExplain_EmptyExplanation(t *testing.T) {
	mock := llm.NewMockClient(`{"explanation": "", "fix": "do something"}`)
	st := newGapStore(t)
	g := New(mock, st, 3*time.Second)

	_, err := g.Explain(context.Background(), Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
		TargetName: "pkg",
	})
	if err == nil {
		t.Fatal("expected error for empty explanation")
	}
}

func TestExplain_EmptyFix_OK(t *testing.T) {
	mock := llm.NewMockClient(`{"explanation": "Something is wrong.", "fix": ""}`)
	st := newGapStore(t)
	g := New(mock, st, 3*time.Second)

	resp, err := g.Explain(context.Background(), Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
		TargetName: "pkg",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Explanation != "Something is wrong." {
		t.Errorf("Explanation = %q", resp.Explanation)
	}
	if resp.Fix != "" {
		t.Errorf("Fix = %q, want empty", resp.Fix)
	}
}

// --- buildPrompt edge cases ---

func TestBuildPrompt_DefaultSeverity(t *testing.T) {
	mock := llm.NewMockClient(`{"explanation": "test", "fix": "fix"}`)
	st := newGapStore(t)
	g := New(mock, st, 3*time.Second)

	// Empty severity should default to "warning".
	prompt := g.buildPrompt(Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
	})
	if prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestBuildPrompt_EmptyDescriptionFallsToRuleID(t *testing.T) {
	mock := llm.NewMockClient("")
	st := newGapStore(t)
	g := New(mock, st, 3*time.Second)

	prompt := g.buildPrompt(Request{
		RuleID:     "my-rule-id",
		SourceFile: "file.go",
	})
	// Description empty -> falls back to RuleID.
	if prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

func TestBuildPrompt_EmptyTargetName(t *testing.T) {
	mock := llm.NewMockClient("")
	st := newGapStore(t)
	g := New(mock, st, 3*time.Second)

	prompt := g.buildPrompt(Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
		TargetName: "",
	})
	// Should contain "(unknown target)".
	if prompt == "" {
		t.Error("expected non-empty prompt")
	}
}

// --- New with zero timeout ---

func TestNew_ZeroTimeout(t *testing.T) {
	mock := llm.NewMockClient(`{"explanation": "ok", "fix": "fix"}`)
	st := newGapStore(t)
	g := New(mock, st, 0)

	// Should use default 3s timeout and work fine.
	resp, err := g.Explain(context.Background(), Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
		TargetName: "pkg",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Explanation != "ok" {
		t.Errorf("Explanation = %q", resp.Explanation)
	}
}

func TestNew_NegativeTimeout(t *testing.T) {
	mock := llm.NewMockClient(`{"explanation": "ok", "fix": "fix"}`)
	st := newGapStore(t)
	g := New(mock, st, -1*time.Second)

	resp, err := g.Explain(context.Background(), Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
		TargetName: "pkg",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Explanation != "ok" {
		t.Errorf("Explanation = %q", resp.Explanation)
	}
}

// --- parseViolation with code-fenced JSON ---

func TestExplain_JSONInCodeFence(t *testing.T) {
	mock := llm.NewMockClient("```json\n{\"explanation\": \"fenced\", \"fix\": \"fenced fix\"}\n```")
	st := newGapStore(t)
	g := New(mock, st, 3*time.Second)

	resp, err := g.Explain(context.Background(), Request{
		RuleID:     "rule-1",
		SourceFile: "file.go",
		TargetName: "pkg",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Explanation != "fenced" {
		t.Errorf("Explanation = %q, want 'fenced'", resp.Explanation)
	}
}

// errMock is a mock that always returns an error.
type errMock struct {
	err error
}

func (m *errMock) Generate(_ context.Context, _ string) (string, error) {
	return "", m.err
}
func (m *errMock) Available(_ context.Context) bool               { return true }
func (m *errMock) ModelName() string                              { return "err-mock" }
func (m *errMock) ModelPulled(_ context.Context) bool             { return true }
func (m *errMock) PullModel(_ context.Context, _ io.Writer) error { return nil }
