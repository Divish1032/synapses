package enricher

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/llm"
	"github.com/SynapsesOS/synapses/internal/brain/store"
)

// Ensure io is used — referenced by captureMockClient.PullModel below.
var _ io.Writer

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "brain.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// ---------------------------------------------------------------------------
// languageFromFile
// ---------------------------------------------------------------------------

func TestLanguageFromFile(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"internal/auth/service.go", "go"},
		{"scripts/ingest.py", "python"},
		{"src/App.ts", "typescript"},
		{"src/App.tsx", "typescript"},
		{"src/index.js", "javascript"},
		{"src/index.jsx", "javascript"},
		{"src/main.rs", "rust"},
		{"src/Main.java", "java"},
		{"lib/helper.rb", "ruby"},
		{"README.md", "unknown"},
		{"", "unknown"},
		{"no_extension", "unknown"},
	}
	for _, tc := range cases {
		got := languageFromFile(tc.path)
		if got != tc.want {
			t.Errorf("languageFromFile(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// packageFromFile
// ---------------------------------------------------------------------------

func TestPackageFromFile(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"internal/auth/service.go", "auth"},
		{"cmd/brain/main.go", "brain"},
		{"service.go", "."},
	}
	for _, tc := range cases {
		got := packageFromFile(tc.path)
		if got != tc.want {
			t.Errorf("packageFromFile(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// nodeTypeOrDefault
// ---------------------------------------------------------------------------

func TestNodeTypeOrDefault(t *testing.T) {
	if got := nodeTypeOrDefault(""); got != "function" {
		t.Errorf("nodeTypeOrDefault(%q) = %q, want %q", "", got, "function")
	}
	if got := nodeTypeOrDefault("struct"); got != "struct" {
		t.Errorf("nodeTypeOrDefault(%q) = %q, want %q", "struct", got, "struct")
	}
}

// ---------------------------------------------------------------------------
// detectDomain
// ---------------------------------------------------------------------------

func TestDetectDomain(t *testing.T) {
	cases := []struct {
		path        string
		wantNonEmpty bool
		wantContains string
	}{
		{"internal/parser/go.go", true, "AST"},
		{"internal/graph/bfs.go", true, "BFS"},
		{"cmd/brain/main.go", false, ""},
	}
	for _, tc := range cases {
		got := detectDomain(tc.path)
		if tc.wantNonEmpty {
			if got == "" {
				t.Errorf("detectDomain(%q): expected non-empty string, got empty", tc.path)
				continue
			}
			if tc.wantContains != "" && !strings.Contains(got, tc.wantContains) {
				t.Errorf("detectDomain(%q) = %q, want it to contain %q", tc.path, got, tc.wantContains)
			}
		} else {
			if got != "" {
				t.Errorf("detectDomain(%q) = %q, want empty string", tc.path, got)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// WithSILMode
// ---------------------------------------------------------------------------

func TestWithSILMode_SetsSILMode(t *testing.T) {
	client := llm.NewMockClient("ROOT_SUMMARY: The root summary.\nINSIGHT: The insight.\nCONCERNS: c1, c2")
	st := openTestStore(t)
	e := New(client, st, 0)
	e2 := e.WithSILMode()
	if e2 != e {
		t.Error("expected same enricher returned")
	}
	// Verify the SIL prompt path is exercised (Enrich should succeed).
	resp, err := e2.Enrich(context.Background(), Request{
		RootName: "MyFunc",
		RootType: "function",
		RootFile: "internal/graph/bfs.go",
	})
	if err != nil {
		t.Fatalf("Enrich with SIL mode failed: %v", err)
	}
	if resp.Insight == "" {
		t.Error("expected non-empty insight from SIL mode Enrich")
	}
}

// ---------------------------------------------------------------------------
// parseInsight
// ---------------------------------------------------------------------------

func TestParseInsight_SILFormat(t *testing.T) {
	raw := "ROOT_SUMMARY: The root summary.\nINSIGHT: The insight.\nCONCERNS: c1, c2"
	resp, err := parseInsight(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.RootSummary == "" {
		t.Error("expected RootSummary to be set")
	}
	if resp.Insight == "" {
		t.Error("expected Insight to be set")
	}
	if !resp.LLMUsed {
		t.Error("expected LLMUsed=true")
	}
}

func TestParseInsight_JSONFallback(t *testing.T) {
	raw := `{"insight": "some insight", "concerns": ["c1"]}`
	resp, err := parseInsight(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Insight == "" {
		t.Error("expected Insight to be set from JSON")
	}
	if !resp.LLMUsed {
		t.Error("expected LLMUsed=true")
	}
}

func TestParseInsight_RawTextFallback(t *testing.T) {
	raw := "just plain text"
	resp, err := parseInsight(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Insight != "just plain text" {
		t.Errorf("expected Insight=%q, got %q", "just plain text", resp.Insight)
	}
}

func TestParseInsight_EmptyString(t *testing.T) {
	_, err := parseInsight("")
	if err == nil {
		t.Fatal("expected error for empty response, got nil")
	}
	if !strings.Contains(err.Error(), "empty response from LLM") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestParseInsight_LongRawTextTruncated(t *testing.T) {
	// Build a string longer than 400 characters that is not valid JSON or SIL format.
	raw := strings.Repeat("a", 401)
	resp, err := parseInsight(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The insight should be truncated with the unicode ellipsis.
	if !strings.HasSuffix(resp.Insight, "…") {
		t.Errorf("expected insight to end with '…', got: %q", resp.Insight[max(0, len(resp.Insight)-10):])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// buildSILPrompt (tested via Enrich with silMode=true)
// ---------------------------------------------------------------------------

func TestBuildSILPrompt_ViaEnrich(t *testing.T) {
	var capturedPrompt string
	// Use a MockClient that captures the prompt.
	capturingClient := &captureMockClient{
		resp: "ROOT_SUMMARY: root.\nINSIGHT: insight.\nCONCERNS: none",
	}
	st := openTestStore(t)
	e := New(capturingClient, st, 3*time.Second)
	e.WithSILMode()

	_, err := e.Enrich(context.Background(), Request{
		RootName:    "GraphSearch",
		RootType:    "function",
		RootFile:    "internal/graph/search.go",
		CalleeNames: []string{"BFS", "DFS"},
		CallerNames: []string{"QueryHandler"},
	})
	if err != nil {
		t.Fatalf("Enrich failed: %v", err)
	}
	capturedPrompt = capturingClient.lastPrompt
	if !strings.HasPrefix(capturedPrompt, "Graph: {") {
		t.Errorf("expected SIL prompt to start with 'Graph: {', got: %q", capturedPrompt[:min(len(capturedPrompt), 40)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// captureMockClient is a test LLM client that records the last prompt it received.
type captureMockClient struct {
	resp       string
	lastPrompt string
}

func (c *captureMockClient) Generate(_ context.Context, prompt string) (string, error) {
	c.lastPrompt = prompt
	return c.resp, nil
}

func (c *captureMockClient) Available(_ context.Context) bool         { return true }
func (c *captureMockClient) ModelName() string                        { return "capture:test" }
func (c *captureMockClient) ModelPulled(_ context.Context) bool       { return true }
func (c *captureMockClient) PullModel(_ context.Context, _ io.Writer) error { return nil }

// TestHeuristicInsight_Content verifies topology-based heuristic text for all
// caller/callee combinations, including the FanIn > len(CallerNames) case.
func TestHeuristicInsight_Content(t *testing.T) {
	cases := []struct {
		name     string
		req      Request
		contains []string
	}{
		{
			name:     "both callers and callees",
			req:      Request{RootName: "Store", RootType: "struct", CallerNames: []string{"A", "B"}, CalleeNames: []string{"X"}},
			contains: []string{"Store", "struct", "2", "1"},
		},
		{
			name:     "FanIn overrides len(CallerNames)",
			req:      Request{RootName: "Router", RootType: "struct", CallerNames: []string{"A"}, FanIn: 42, CalleeNames: []string{"X"}},
			contains: []string{"Router", "42"},
		},
		{
			name:     "only callees",
			req:      Request{RootName: "Root", RootType: "function", CalleeNames: []string{"A", "B", "C"}},
			contains: []string{"Root", "function", "3"},
		},
		{
			name:     "isolated node",
			req:      Request{RootName: "Util", RootType: "function"},
			contains: []string{"Util", "no recorded"},
		},
		{
			name:     "empty name defaults",
			req:      Request{},
			contains: []string{"this entity", "entity"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := heuristicInsight(tc.req)
			if got == "" {
				t.Fatal("heuristicInsight returned empty string")
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("heuristicInsight %q missing %q", got, want)
				}
			}
		})
	}
}

// TestEnrich_LLMFailure_HeuristicInsight_FanIn verifies that FanIn is used in
// the heuristic when the LLM fails, so the reported caller count is accurate
// even when CallerNames is capped by the maxNamesInPrompt limit.
func TestEnrich_LLMFailure_HeuristicInsight_FanIn(t *testing.T) {
	mock := &llm.MockClient{Err: os.ErrDeadlineExceeded}
	st := openTestStore(t)
	e := New(mock, st, 3*time.Second)

	resp, err := e.Enrich(context.Background(), Request{
		RootName:    "HotPath",
		RootType:    "function",
		CallerNames: []string{"A", "B"}, // capped subset
		FanIn:       99,                 // true total
		CalleeNames: []string{"X"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Insight, "99") {
		t.Errorf("heuristic must use FanIn=99, got: %q", resp.Insight)
	}
}
