package ingestor

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

func TestSummarize_Success(t *testing.T) {
	mock := llm.NewMockClient(`{"summary": "Validates JWT tokens and checks expiry."}`)
	st := newTestStore(t)
	ing := New(mock, st, 3*time.Second)

	resp, err := ing.Summarize(context.Background(), Request{
		ProjectID: "test-project",
		NodeID:    "node:auth:Validate",
		NodeName:  "Validate",
		NodeType:  "method",
		Package:   "auth",
		Code:      "func (s *AuthService) Validate(token string) (Claims, error) { ... }",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Summary != "Validates JWT tokens and checks expiry." {
		t.Errorf("unexpected summary: %q", resp.Summary)
	}
	if resp.NodeID != "node:auth:Validate" {
		t.Errorf("unexpected node ID: %q", resp.NodeID)
	}

	// Verify persisted to store.
	stored := st.GetSummary("test-project", "node:auth:Validate")
	if stored != "Validates JWT tokens and checks expiry." {
		t.Errorf("store not updated, got: %q", stored)
	}
}

func TestSummarize_LLMUnavailable(t *testing.T) {
	mock := &llm.MockClient{Err: os.ErrDeadlineExceeded}
	st := newTestStore(t)
	ing := New(mock, st, 3*time.Second)

	_, err := ing.Summarize(context.Background(), Request{
		ProjectID: "test-project",
		NodeID:    "node:x",
		NodeName:  "X",
		Code:      "func X() {}",
	})
	if err == nil {
		t.Fatal("expected error when LLM unavailable, got nil")
	}

	// Nothing should be written to the store on failure.
	if stored := st.GetSummary("test-project", "node:x"); stored != "" {
		t.Errorf("expected no stored summary on failure, got: %q", stored)
	}
}

func TestSummarize_PlainTextFallback(t *testing.T) {
	// Small models sometimes return plain text instead of JSON.
	// The ingestor should accept the plain text as the summary.
	mock := llm.NewMockClient(`This function performs authentication validation.`)
	st := newTestStore(t)
	ing := New(mock, st, 3*time.Second)

	resp, err := ing.Summarize(context.Background(), Request{
		ProjectID: "test-project",
		NodeID:    "node:x",
		Code:      "func X() {}",
	})
	if err != nil {
		t.Fatalf("expected plain-text fallback to succeed, got error: %v", err)
	}
	if resp.Summary == "" {
		t.Fatal("expected non-empty summary from plain-text fallback")
	}
}

func TestSummarize_MarkdownWrappedJSON(t *testing.T) {
	mock := llm.NewMockClient("```json\n{\"summary\": \"Does auth validation.\"}\n```")
	st := newTestStore(t)
	ing := New(mock, st, 3*time.Second)

	resp, err := ing.Summarize(context.Background(), Request{
		ProjectID: "test-project",
		NodeID:    "node:y",
		NodeName:  "Y",
		Code:      "func Y() {}",
	})
	if err != nil {
		t.Fatalf("unexpected error with markdown-wrapped JSON: %v", err)
	}
	if resp.Summary != "Does auth validation." {
		t.Errorf("unexpected summary: %q", resp.Summary)
	}
}

func TestBuildPrompt_TruncatesLongCode(t *testing.T) {
	ing := &Ingestor{}
	longCode := string(make([]byte, 1000))
	prompt := ing.buildPrompt(Request{
		NodeName: "Foo",
		NodeType: "function",
		Package:  "pkg",
		Code:     longCode,
	})
	if len(prompt) > 2000 {
		t.Errorf("prompt too long: %d chars", len(prompt))
	}
}

func TestSummarize_CodeHallucination_JSONPath(t *testing.T) {
	// LLM returns Go code as the summary value — must be rejected.
	mock := llm.NewMockClient(`{"summary": "func Validate(token string) { x := 5; return x }"}`)
	st := newTestStore(t)
	ing := New(mock, st, 3*time.Second)

	_, err := ing.Summarize(context.Background(), Request{
		ProjectID: "test-project",
		NodeID:    "node:auth:Validate",
		NodeName:  "Validate",
		Code:      "func Validate(token string) error { return nil }",
	})
	if err == nil {
		t.Fatal("expected error when LLM returns code as summary, got nil")
	}

	// Nothing should be written to the store on rejection.
	if stored := st.GetSummary("test-project", "node:auth:Validate"); stored != "" {
		t.Errorf("expected no stored summary when code hallucination rejected, got: %q", stored)
	}
}

func TestSummarize_CodeHallucination_FallbackPath(t *testing.T) {
	// LLM returns raw Go code (not JSON) — fallback path must also reject it.
	mock := llm.NewMockClient("func Validate(token string) {\n\tx := verify(token)\n\treturn x\n}")
	st := newTestStore(t)
	ing := New(mock, st, 3*time.Second)

	_, err := ing.Summarize(context.Background(), Request{
		ProjectID: "test-project",
		NodeID:    "node:auth:Validate",
		NodeName:  "Validate",
		Code:      "func Validate(token string) error { return nil }",
	})
	if err == nil {
		t.Fatal("expected error when LLM returns raw code as fallback, got nil")
	}

	if stored := st.GetSummary("test-project", "node:auth:Validate"); stored != "" {
		t.Errorf("expected no stored summary when code hallucination rejected, got: %q", stored)
	}
}

func TestLooksLikeCode(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		// Clearly code — should be rejected.
		{"func Foo() { x := 5 }", true},
		{"x := compute(y)", true},
		{"func Open(path string) (*Store, error) {\n\tdb, err := sql.Open(...)\n}", true},
		// Natural prose mentioning "function" or "returns" — should NOT be rejected.
		{"This function validates JWT tokens and returns an error on expiry.", false},
		{"Manages the connection pool for the database.", false},
		{"Handles HTTP routing and dispatches requests to registered handlers.", false},
		// Edge: single brace or "func" without both markers — not rejected.
		{"Groups related {items} in the config.", false},
		{"Calls the func defined in the store package.", false},
		// Go struct/interface declarations.
		{"type Config struct { Host string }", true},
		{"type Handler interface { Handle(r *Request) error }", true},
		// Python function definition.
		{"def validate(token, expiry):", true},
		// JS/TS arrow function.
		{"const handler = (req) => { return req.body }", true},
		{"app.get('/health', (req, res) => res.send('ok'))", true},
		// JS/TS function keyword.
		{"function(x) { return x + 1 }", true},
		// Prose that mentions programming terms but is not code.
		{"Defines the interface between the client and server.", false},
		{"Python-style configuration using key-value pairs.", false},
		{"The function type is registered in the handler map.", false},
	}
	for _, tc := range cases {
		got := looksLikeCode(tc.input)
		if got != tc.want {
			t.Errorf("looksLikeCode(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{`{"summary": "hello"}`, `{"summary": "hello"}`},
		{"```json\n{\"summary\": \"hello\"}\n```", `{"summary": "hello"}`},
		{"Here is the answer: {\"summary\": \"hello\"} done.", `{"summary": "hello"}`},
		{" \n{\"summary\": \"hello\"}\n", `{"summary": "hello"}`},
	}
	for _, tc := range cases {
		got := llm.ExtractJSON(tc.input)
		if got != tc.want {
			t.Errorf("ExtractJSON(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
