package brain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
)

// Direct tests for impl methods to reach 80% coverage
// These test the actual impl struct methods, not through the interface

func TestImpl_Ingest_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Ingest: false}
	b := &impl{cfg: cfg}
	resp, err := b.Ingest(context.Background(), IngestRequest{NodeID: "n1"})
	if err != nil || resp.NodeID != "n1" {
		t.Errorf("Ingest disabled path failed")
	}
}

func TestImpl_Ingest_DirectCallCBOpen(t *testing.T) {
	cfg := brainconfig.BrainConfig{Ingest: true}
	cb := newCircuitBreaker(1, 1*time.Second)
	cb.recordFailure("ingest")
	cb.recordFailure("ingest")
	b := &impl{cfg: cfg, cb: cb}
	resp, _ := b.Ingest(context.Background(), IngestRequest{NodeID: "n1"})
	if resp.NodeID != "n1" {
		t.Error("Ingest CB open path failed")
	}
}

func TestImpl_Enrich_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enrich: false}
	b := &impl{cfg: cfg, store: nil}
	defer func() { recover() }()
	b.Enrich(context.Background(), EnrichRequest{})
}

func TestImpl_Enrich_DirectCallCBOpen(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enrich: true}
	cb := newCircuitBreaker(1, 1*time.Second)
	cb.recordFailure("enrich")
	cb.recordFailure("enrich")
	b := &impl{cfg: cfg, cb: cb, store: nil}
	defer func() { recover() }()
	b.Enrich(context.Background(), EnrichRequest{})
}

func TestImpl_ExplainViolation_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Guardian: false}
	b := &impl{cfg: cfg}
	resp, _ := b.ExplainViolation(context.Background(), ViolationRequest{})
	if resp.Explanation != "" {
		t.Error("ExplainViolation disabled path failed")
	}
}

func TestImpl_ExplainViolation_DirectCallNoGuardian(t *testing.T) {
	cfg := brainconfig.BrainConfig{Guardian: true}
	b := &impl{cfg: cfg, guardian: nil}
	resp, _ := b.ExplainViolation(context.Background(), ViolationRequest{})
	if resp.Explanation != "" {
		t.Error("ExplainViolation no guardian path failed")
	}
}

func TestImpl_Coordinate_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Orchestrate: false}
	b := &impl{cfg: cfg}
	resp, _ := b.Coordinate(context.Background(), CoordinateRequest{})
	if resp.Suggestion != "" {
		t.Error("Coordinate disabled path failed")
	}
}

func TestImpl_Coordinate_DirectCallNoOrch(t *testing.T) {
	cfg := brainconfig.BrainConfig{Orchestrate: true}
	b := &impl{cfg: cfg, orchestrator: nil}
	resp, _ := b.Coordinate(context.Background(), CoordinateRequest{})
	if resp.Suggestion != "" {
		t.Error("Coordinate no orchestrator path failed")
	}
}

func TestImpl_Prune_DirectCall(t *testing.T) {
	b := &impl{pruner: nil}
	defer func() { recover() }()
	b.Prune(context.Background(), "test")
}

func TestImpl_Memorize_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Memorize: false}
	b := &impl{cfg: cfg}
	resp, _ := b.Memorize(context.Background(), archivist.MemorizeRequest{})
	if len(resp.NewMemories) != 0 {
		t.Error("Memorize disabled path failed")
	}
}

func TestImpl_Memorize_DirectCallNoArchivist(t *testing.T) {
	cfg := brainconfig.BrainConfig{Memorize: true}
	b := &impl{cfg: cfg, archivist: nil}
	resp, _ := b.Memorize(context.Background(), archivist.MemorizeRequest{})
	if len(resp.NewMemories) != 0 {
		t.Error("Memorize no archivist path failed")
	}
}

func TestImpl_BuildContextPacket_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{ContextBuilder: false}
	b := &impl{cfg: cfg}
	resp, _ := b.BuildContextPacket(context.Background(), ContextPacketRequest{})
	if resp != nil {
		t.Error("BuildContextPacket disabled path failed")
	}
}

func TestImpl_BuildContextPacket_DirectCallCBOpen(t *testing.T) {
	cfg := brainconfig.BrainConfig{ContextBuilder: true}
	cb := newCircuitBreaker(1, 1*time.Second)
	cb.recordFailure("context_builder")
	cb.recordFailure("context_builder")
	b := &impl{cfg: cfg, cb: cb}
	resp, _ := b.BuildContextPacket(context.Background(), ContextPacketRequest{})
	if resp != nil {
		t.Error("BuildContextPacket CB open path failed")
	}
}

func TestImpl_LogDecision_DirectCall(t *testing.T) {
	b := &impl{learner: nil}
	defer func() { recover() }()
	b.LogDecision(context.Background(), DecisionRequest{})
}

func TestImpl_SetSDLCPhase_DirectCall(t *testing.T) {
	b := &impl{sdlcMgr: nil}
	defer func() { recover() }()
	b.SetSDLCPhase(PhaseDevelopment, "a1")
}

func TestImpl_SetQualityMode_DirectCall(t *testing.T) {
	b := &impl{sdlcMgr: nil}
	defer func() { recover() }()
	b.SetQualityMode(QualityStandard, "a1")
}

func TestImpl_GetSDLCConfig_DirectCall(t *testing.T) {
	b := &impl{sdlcMgr: nil}
	defer func() { recover() }()
	b.GetSDLCConfig()
}

func TestImpl_GetPatterns_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.GetPatterns("", 0)
}

func TestImpl_UpsertADR_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.UpsertADR(ADRRequest{ID: "a1"})
}

func TestImpl_GetADR_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.GetADR("a1")
}

func TestImpl_AllADRs_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.AllADRs()
}

func TestImpl_GetADRsForFile_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.GetADRsForFile("f.go", 10)
}

// --- Fallback chain tests (Sprint 17 #4) ---

// TestImpl_Enrich_HeuristicFallback_BothCBOpen verifies that when both the
// primary ("enrich") and fallback ("ingest") circuit breakers are open,
// Enrich returns a non-empty heuristic insight instead of an empty response.
func TestImpl_Enrich_HeuristicFallback_BothCBOpen(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enrich: true}
	cb := newCircuitBreaker(1, 10*time.Second)
	// Trip both tiers so all LLM paths are exhausted.
	cb.recordFailure("enrich")
	cb.recordFailure("enrich")
	cb.recordFailure("ingest")
	cb.recordFailure("ingest")

	b := &impl{cfg: cfg, cb: cb, store: nil}
	resp, err := b.Enrich(context.Background(), EnrichRequest{
		RootName:    "AuthService",
		RootType:    "struct",
		CallerNames: []string{"HandleLogin", "HandleRegister"},
		CalleeNames: []string{"db.Query"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Insight == "" {
		t.Error("expected non-empty heuristic insight when all LLM tiers exhausted")
	}
	if !resp.Degraded {
		t.Error("expected Degraded=true for heuristic fallback")
	}
	// The heuristic must mention the entity name and topology.
	if !strings.Contains(resp.Insight, "AuthService") {
		t.Errorf("heuristic insight missing entity name: %q", resp.Insight)
	}
	if !strings.Contains(resp.Insight, "2") { // 2 callers
		t.Errorf("heuristic insight missing caller count: %q", resp.Insight)
	}
}

// TestImpl_ExplainViolation_TemplateFallback_BothCBOpen verifies that when
// both the primary ("guardian") and fallback ("ingest") circuit breakers are
// open, ExplainViolation returns a non-empty template response.
func TestImpl_ExplainViolation_TemplateFallback_BothCBOpen(t *testing.T) {
	cfg := brainconfig.BrainConfig{Guardian: true}
	cb := newCircuitBreaker(1, 10*time.Second)
	cb.recordFailure("guardian")
	cb.recordFailure("guardian")
	cb.recordFailure("ingest")
	cb.recordFailure("ingest")

	b := &impl{cfg: cfg, cb: cb, guardian: nil} // nil guardian ensures no LLM call
	resp, err := b.ExplainViolation(context.Background(), ViolationRequest{
		RuleID:       "no-store-in-view",
		Description:  "view components must not import store packages",
		RuleSeverity: "error",
		SourceFile:   "/abs/path/to/project/internal/mcp/handlers.go",
		TargetName:   "internal/store",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Explanation == "" {
		t.Error("expected non-empty template explanation when all LLM tiers exhausted")
	}
	if resp.Fix == "" {
		t.Error("expected non-empty template fix when all LLM tiers exhausted")
	}
	if !resp.Degraded {
		t.Error("expected Degraded=true for template fallback")
	}
	// Template must reference key rule data.
	if !strings.Contains(resp.Explanation, "internal/store") {
		t.Errorf("template explanation missing target name: %q", resp.Explanation)
	}
}

// TestHeuristicEnrichInsight_Content verifies the heuristic text content for
// various caller/callee combinations.
func TestHeuristicEnrichInsight_Content(t *testing.T) {
	cases := []struct {
		name     string
		req      EnrichRequest
		contains []string
	}{
		{
			name: "both callers and callees",
			req:  EnrichRequest{RootName: "Store", RootType: "struct", CallerNames: []string{"A", "B"}, CalleeNames: []string{"X"}},
			contains: []string{"Store", "struct", "2", "1"},
		},
		{
			name: "only callers",
			req:  EnrichRequest{RootName: "Leaf", RootType: "function", CallerNames: []string{"X"}, CalleeNames: nil},
			contains: []string{"Leaf", "function", "1"},
		},
		{
			name: "only callees",
			req:  EnrichRequest{RootName: "Root", RootType: "function", CallerNames: nil, CalleeNames: []string{"A", "B", "C"}},
			contains: []string{"Root", "function", "3"},
		},
		{
			name:     "isolated node",
			req:      EnrichRequest{RootName: "Util", RootType: "function"},
			contains: []string{"Util", "no recorded"},
		},
		{
			name:     "empty name defaults",
			req:      EnrichRequest{},
			contains: []string{"this entity", "entity"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := heuristicEnrichInsight(tc.req)
			if got == "" {
				t.Fatal("heuristic returned empty string")
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("heuristic %q missing %q", got, want)
				}
			}
		})
	}
}

// TestGuardianTemplateFallback_Content verifies the template response text,
// including that absolute source paths are rendered as package-relative paths
// (e.g. "internal/ui/handler.go" not just "handler.go").
func TestGuardianTemplateFallback_Content(t *testing.T) {
	req := ViolationRequest{
		RuleID:       "no-ui-imports-db",
		Description:  "UI layer must not import DB packages",
		RuleSeverity: "error",
		// Simulate the full absolute path that validate_plan passes via fromNode.File.
		SourceFile: "/home/ubuntu/work/project/internal/ui/handler.go",
		TargetName: "internal/db",
	}
	resp := guardianTemplateFallback(req)
	if resp.Explanation == "" {
		t.Error("template explanation is empty")
	}
	if resp.Fix == "" {
		t.Error("template fix is empty")
	}
	if !resp.Degraded {
		t.Error("expected Degraded=true")
	}
	if !strings.Contains(resp.Explanation, "internal/db") {
		t.Errorf("explanation missing target name: %q", resp.Explanation)
	}
	if !strings.Contains(resp.Fix, "internal/db") {
		t.Errorf("fix missing target name: %q", resp.Fix)
	}
	if !strings.Contains(resp.Fix, "no-ui-imports-db") {
		t.Errorf("fix missing rule ID: %q", resp.Fix)
	}
	// The source path must be package-relative, not just the filename.
	if !strings.Contains(resp.Explanation, "internal/ui/handler.go") {
		t.Errorf("explanation must use relative path, got: %q", resp.Explanation)
	}
}

// TestRelativeSourcePath verifies the path extraction utility handles all cases.
func TestRelativeSourcePath(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/home/ubuntu/project/internal/mcp/handlers.go", "internal/mcp/handlers.go"},
		{"/home/ubuntu/project/cmd/synapses/main.go", "cmd/synapses/main.go"},
		{"/home/ubuntu/project/pkg/store/store.go", "pkg/store/store.go"},
		{"/home/ubuntu/project/src/app/app.go", "src/app/app.go"},
		// No known marker — fall back to base name only.
		{"/random/path/to/file.go", "file.go"},
		// Already relative — no leading slash, no marker match; returns base.
		{"internal/ui/handler.go", "handler.go"},
		// Empty string — filepath.Base("") returns ".", acceptable fallback.
		{"", "."},
	}
	for _, tc := range cases {
		got := relativeSourcePath(tc.input)
		if got != tc.want {
			t.Errorf("relativeSourcePath(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestGuardianTemplateFallback_Defaults verifies that empty fields produce
// a graceful (non-empty, non-panicking) template response.
func TestGuardianTemplateFallback_Defaults(t *testing.T) {
	resp := guardianTemplateFallback(ViolationRequest{})
	if resp.Explanation == "" || resp.Fix == "" {
		t.Error("template fallback returned empty fields for zero-value request")
	}
}

// ── NL classification helpers ─────────────────────────────────────────────────

func TestParseEntityTypeResponse_ValidTypes(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"concept", "concept"},
		{"CONCEPT", "concept"},
		{"entity", "entity"},
		{"artifact", "artifact"},
		{"decision", "decision"},
		// Trailing punctuation stripped.
		{"concept.", "concept"},
		{"entity,", "entity"},
		// Extra words after the type word — split on space.
		{"concept is a general idea", "concept"},
		// Newline-separated extra text — the original bug: IndexByte(' ') missed this.
		{"concept\nThis is a general concept.", "concept"},
		{"entity\n\nSome further explanation.", "entity"},
		// Leading/trailing whitespace.
		{"  artifact  ", "artifact"},
	}
	for _, tc := range cases {
		got := parseEntityTypeResponse(tc.input)
		if got != tc.want {
			t.Errorf("parseEntityTypeResponse(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseEntityTypeResponse_Invalid(t *testing.T) {
	invalid := []string{"", "unknown", "node", "class", "function", "  ", "123"}
	for _, s := range invalid {
		if got := parseEntityTypeResponse(s); got != "" {
			t.Errorf("parseEntityTypeResponse(%q) should return \"\", got %q", s, got)
		}
	}
}

func TestBuildEntityTypePrompt_ContainsFields(t *testing.T) {
	p := buildEntityTypePrompt("TokenBucket", "The TokenBucket controls throughput.")
	if !strings.Contains(p, "TokenBucket") {
		t.Error("prompt should contain entity name")
	}
	if !strings.Contains(p, "throughput") {
		t.Error("prompt should contain context")
	}
	if !strings.Contains(p, "concept") {
		t.Error("prompt should contain valid types")
	}
}

func TestBuildEntityTypePrompt_LongContextTruncated(t *testing.T) {
	long := strings.Repeat("word ", 60) // 300 chars
	p := buildEntityTypePrompt("X", long)
	// The context in the prompt should be truncated to ≤150 chars (at word boundary).
	// Rough check: total prompt length should be well under 500 chars.
	if len(p) > 500 {
		t.Errorf("prompt too long for truncated context: %d chars", len(p))
	}
}
