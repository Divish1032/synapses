package mcp

// White-box tests for intents.go — targets the pure helpers and main handlers.
// Uses package mcp (not mcp_test) for direct access to unexported symbols.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── tokensUsed / budgetLeft ───────────────────────────────────────────────────

func TestTokensUsed_Empty(t *testing.T) {
	var b strings.Builder
	if n := tokensUsed(&b); n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestTokensUsed_NonEmpty(t *testing.T) {
	var b strings.Builder
	b.WriteString("1234") // 4 chars → 1 token
	if n := tokensUsed(&b); n != 1 {
		t.Errorf("expected 1, got %d", n)
	}
}

func TestBudgetLeft_WithinBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("hello") // 5 chars → 1 token
	left := budgetLeft(&b, 100)
	if left != 99 {
		t.Errorf("expected 99, got %d", left)
	}
}

func TestBudgetLeft_Exceeded(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("x", 1000)) // 1000 chars → 250 tokens
	left := budgetLeft(&b, 10)
	if left != 0 {
		t.Errorf("expected 0 when budget exceeded, got %d", left)
	}
}

// ── intentDefaultBudget ───────────────────────────────────────────────────────

func TestIntentDefaultBudget(t *testing.T) {
	cases := []struct {
		intent string
		want   int
	}{
		{"modify", 3000},
		{"review", 3000},
		{"debug", 3500},
		{"understand", 2000},
		{"add", 2000},
		{"plan", 2000},
		{"unknown", 2000},
		{"", 2000},
	}
	for _, tc := range cases {
		got := intentDefaultBudget(tc.intent)
		if got != tc.want {
			t.Errorf("intentDefaultBudget(%q) = %d, want %d", tc.intent, got, tc.want)
		}
	}
}

// ── formatAge ─────────────────────────────────────────────────────────────────

func TestFormatAge_InvalidTimestamp(t *testing.T) {
	if got := formatAge("not-a-timestamp"); got != "unknown" {
		t.Errorf("expected 'unknown', got %q", got)
	}
}

func TestFormatAge_JustNow(t *testing.T) {
	ts := time.Now().Add(-5 * time.Second).Format(time.RFC3339)
	if got := formatAge(ts); got != "just now" {
		t.Errorf("expected 'just now', got %q", got)
	}
}

func TestFormatAge_MinutesAgo(t *testing.T) {
	ts := time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	got := formatAge(ts)
	if !strings.Contains(got, "m ago") {
		t.Errorf("expected 'm ago', got %q", got)
	}
}

func TestFormatAge_HoursAgo(t *testing.T) {
	ts := time.Now().Add(-3 * time.Hour).Format(time.RFC3339)
	got := formatAge(ts)
	if !strings.Contains(got, "h ago") {
		t.Errorf("expected 'h ago', got %q", got)
	}
}

func TestFormatAge_DaysAgo(t *testing.T) {
	ts := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	got := formatAge(ts)
	if !strings.Contains(got, "d ago") {
		t.Errorf("expected 'd ago', got %q", got)
	}
}

// ── writeWarnings ─────────────────────────────────────────────────────────────

func TestWriteWarnings_NilPacket(t *testing.T) {
	var b strings.Builder
	writeWarnings(&b, nil) // must not panic
	if b.Len() != 0 {
		t.Error("expected no output for nil packet")
	}
}

func TestWriteWarnings_WithWarnings(t *testing.T) {
	var b strings.Builder
	pkt := &brain.ContextPacket{
		GraphWarnings: []string{"too many calls"},
		Concerns:      []string{"missing error handling"},
	}
	writeWarnings(&b, pkt)
	out := b.String()
	if !strings.Contains(out, "## Warnings") {
		t.Error("expected ## Warnings section")
	}
	if !strings.Contains(out, "too many calls") {
		t.Error("expected graph warning in output")
	}
}

func TestWriteWarnings_EmptyPacket(t *testing.T) {
	var b strings.Builder
	writeWarnings(&b, &brain.ContextPacket{})
	if b.Len() != 0 {
		t.Error("expected no output for empty warnings/concerns")
	}
}

// ── writeAnnotations ──────────────────────────────────────────────────────────

func TestWriteAnnotations_Empty(t *testing.T) {
	var b strings.Builder
	writeAnnotations(&b, nil, "root-id")
	if b.Len() != 0 {
		t.Error("expected no output for nil annotation map")
	}
}

func TestWriteAnnotations_WithNotes(t *testing.T) {
	var b strings.Builder
	ts := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	annMap := map[string][]store.Annotation{
		"root-id": {{AgentID: "agent-a", Note: "reviewing auth", CreatedAt: ts}},
		"other":   {{AgentID: "agent-b", Note: "see also logout", CreatedAt: ts}},
	}
	writeAnnotations(&b, annMap, "root-id")
	out := b.String()
	if !strings.Contains(out, "## Agent Notes") {
		t.Error("expected ## Agent Notes section")
	}
	if !strings.Contains(out, "agent-a") {
		t.Error("expected agent-a in output")
	}
}

// ── resolveTarget ─────────────────────────────────────────────────────────────

func TestResolveTarget_ExactMatch(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	_ = loginID
	rt := s.resolveTarget("AuthLogin", "")
	if rt.isConcept {
		t.Error("expected exact match, not concept fallback")
	}
	if rt.bestNode == nil {
		t.Error("expected non-nil bestNode")
	}
	if rt.bestNode.Name != "AuthLogin" {
		t.Errorf("expected AuthLogin, got %q", rt.bestNode.Name)
	}
}

func TestResolveTarget_WithFileHint(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	rt := s.resolveTarget("AuthLogin", "pkg/auth/auth.go")
	if rt.isConcept {
		t.Error("expected match with file hint")
	}
}

func TestResolveTarget_NoMatch(t *testing.T) {
	s := newTestServer(t)
	rt := s.resolveTarget("NonExistentSymbol", "")
	if !rt.isConcept {
		t.Error("expected concept fallback for unknown symbol")
	}
}

func TestResolveTarget_FilePath(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// Should find nodes via FindByFile
	rt := s.resolveTarget("pkg/auth/auth.go", "")
	if rt.isConcept {
		t.Error("expected file-path match")
	}
	if !rt.isFile {
		t.Error("expected isFile=true for file path target")
	}
}

func TestResolveTarget_PatternMatch(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// "Auth" should match AuthLogin and AuthLogout via FindByPattern
	rt := s.resolveTarget("Auth", "")
	if rt.isConcept {
		t.Error("expected pattern match for 'Auth'")
	}
}

// ── choiceMapHeader ───────────────────────────────────────────────────────────

func TestChoiceMapHeader_NilResolved(t *testing.T) {
	s := newTestServer(t)
	if h := s.choiceMapHeader(nil, "Foo"); h != "" {
		t.Error("expected empty for nil resolved")
	}
}

func TestChoiceMapHeader_SingleCandidate(t *testing.T) {
	s := newTestServer(t)
	node := &graph.Node{ID: "n1", Name: "Foo", File: "foo.go"}
	rt := &resolvedTarget{bestNode: node, candidates: []*graph.Node{node}}
	if h := s.choiceMapHeader(rt, "Foo"); h != "" {
		t.Error("expected empty for single candidate")
	}
}

func TestChoiceMapHeader_MultipleCandidates(t *testing.T) {
	s := newTestServer(t)
	n1 := &graph.Node{ID: "n1", Name: "Foo", File: "a/foo.go", Line: 1, Type: graph.NodeFunction}
	n2 := &graph.Node{ID: "n2", Name: "Foo", File: "b/foo.go", Line: 1, Type: graph.NodeFunction}
	rt := &resolvedTarget{
		bestNode:   n1,
		candidates: []*graph.Node{n1, n2},
	}
	h := s.choiceMapHeader(rt, "Foo")
	if h == "" {
		t.Error("expected non-empty header for multiple candidates")
	}
	if !strings.Contains(h, "Ambiguous Target") {
		t.Error("expected 'Ambiguous Target' in header")
	}
}

// ── handlePrepareContext ──────────────────────────────────────────────────────

func TestHandlePrepareContext_NoTarget(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handlePrepareContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result when target is missing")
	}
}

func TestHandlePrepareContext_ConceptFallback(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{
		"intent": "understand",
		"target": "auth rate limiting",
	})
	result, err := s.handlePrepareContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestHandlePrepareContext_WithEntity(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"intent": "modify",
		"target": "AuthLogin",
	})
	result, err := s.handlePrepareContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestHandlePrepareContext_AllIntents(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	intents := []string{"modify", "understand", "review", "debug", "add", "plan"}
	for _, intent := range intents {
		req := callTool(map[string]any{
			"intent": intent,
			"target": "AuthLogin",
		})
		result, err := s.handlePrepareContext(ctx, req)
		if err != nil {
			t.Fatalf("intent=%q: unexpected error: %v", intent, err)
		}
		if result == nil {
			t.Fatalf("intent=%q: expected non-nil result", intent)
		}
	}
}

func TestHandlePrepareContext_CustomBudget(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"intent":       "understand",
		"target":       "AuthLogin",
		"token_budget": float64(500),
	})
	result, err := s.handlePrepareContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// ── handlePlanContext ─────────────────────────────────────────────────────────

func TestHandlePlanContext_NoTarget(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handlePlanContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result when target is missing")
	}
}

func TestHandlePlanContext_ClearVerdict(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"target": "AuthLogin",
	})
	result, err := s.handlePlanContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, result, nil)
	if verdict, _ := m["verdict"].(string); verdict != "clear" {
		t.Errorf("expected clear verdict, got %q", verdict)
	}
}

func TestHandlePlanContext_WithChanges(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	req := callTool(map[string]any{
		"target":  "AuthLogin",
		"changes": `[{"file": "pkg/auth/auth.go", "adds_call_to": "AuthLogout"}]`,
	})
	result, err := s.handlePlanContext(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// ── aggregatedImpact ──────────────────────────────────────────────────────────

func TestAggregatedImpact_FunctionNode(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	node := s.graph.GetNode(loginID)
	if node == nil {
		t.Fatal("expected login node to exist")
	}
	result := s.aggregatedImpact(node, 2)
	// May return nil if no impact paths, but must not panic.
	_ = result
}

func TestAggregatedImpact_StructNode(t *testing.T) {
	s := newTestServer(t)
	// Add a struct with methods.
	structID := s.graph.MakeNodeID("pkg/svc.go", "Service")
	s.graph.AddNode(&graph.Node{
		ID: structID, Name: "Service", Type: graph.NodeStruct,
		File: "pkg/svc.go", Package: "svc",
	})
	methodID := s.graph.MakeNodeID("pkg/svc.go", "Service.Handle")
	s.graph.AddNode(&graph.Node{
		ID: methodID, Name: "Handle", Type: graph.NodeMethod,
		File: "pkg/svc.go", Package: "svc",
	})
	node := s.graph.GetNode(structID)
	result := s.aggregatedImpact(node, 1)
	_ = result // must not panic
}

// ── buildBrainPacket (nil brain) ──────────────────────────────────────────────

func TestBuildBrainPacket_NilBrain(t *testing.T) {
	s, loginID, _ := newPopulatedServer(t)
	node := s.graph.GetNode(loginID)
	dc := &directionalContext{Root: node}
	pkt := s.buildBrainPacket(ctx, node, dc, "")
	if pkt != nil {
		t.Error("expected nil packet when brain not configured")
	}
}

// ── extra coverage: suggestNextAfterContext ───────────────────────────────────

func TestSuggestNextAfterContext_HasCallees(t *testing.T) {
	dc := &directionalContext{
		Root: &graph.Node{Name: "A", File: "a.go"},
		Callees: []graph.CarvedNode{
			{Node: &graph.Node{Name: "B", File: "b.go"}},
		},
	}
	suggestions := suggestNextAfterContext(dc)
	if len(suggestions) == 0 {
		t.Error("expected at least one suggestion")
	}
}

func TestSuggestNextAfterContext_EmptyDC(t *testing.T) {
	dc := &directionalContext{
		Root: &graph.Node{Name: "X", File: "x.go"},
	}
	// Should not panic with empty callees/callers.
	suggestions := suggestNextAfterContext(dc)
	_ = suggestions
}

// ── topLevelPackage ───────────────────────────────────────────────────────────

func TestTopLevelPackage_Standard(t *testing.T) {
	if p := topLevelPackage("internal/auth"); p != "internal" {
		t.Errorf("expected 'internal', got %q", p)
	}
}

func TestTopLevelPackage_SingleComponent(t *testing.T) {
	if p := topLevelPackage("auth"); p != "auth" {
		t.Errorf("expected 'auth', got %q", p)
	}
}

func TestTopLevelPackage_Empty(t *testing.T) {
	if p := topLevelPackage(""); p != "" {
		t.Errorf("expected empty, got %q", p)
	}
}

// ── camelWords ────────────────────────────────────────────────────────────────

func TestCamelWords_CamelCase(t *testing.T) {
	words := camelWords("AuthLogin")
	if len(words) == 0 {
		t.Error("expected non-empty words for CamelCase")
	}
	// Should split "AuthLogin" into ["auth", "login"] or similar.
	joined := strings.Join(words, " ")
	if !strings.Contains(strings.ToLower(joined), "auth") {
		t.Errorf("expected 'auth' in camelWords output: %v", words)
	}
}

func TestCamelWords_SingleWord(t *testing.T) {
	words := camelWords("login")
	if len(words) == 0 {
		t.Error("expected non-empty for single word")
	}
}

func TestCamelWords_Empty(t *testing.T) {
	words := camelWords("")
	if len(words) != 0 {
		t.Errorf("expected empty for empty input, got %v", words)
	}
}

// prevent unused import errors
var _ = fmt.Sprintf

// ── applyIntentCarveConfig ────────────────────────────────────────────────────

// TestApplyIntentCarveConfig_SetsAllThreeFields verifies that applyIntentCarveConfig
// populates EdgeWeights, DirectionBoost, and IntentID correctly for each intent.
func TestApplyIntentCarveConfig_SetsAllThreeFields(t *testing.T) {
	cases := []struct {
		intent        string
		wantIntentID  string
		wantBoost     float64
		checkEdgeType graph.EdgeType
		wantRelOp     string // "gt" or "lt" relative to DefaultEdgeWeights value
	}{
		// modify: DirectionBoost=0.3, IMPLEMENTS reduced vs default
		{"modify", "modify", 0.3, graph.EdgeImplements, "lt"},
		// debug: DirectionBoost=-0.3, DATA_FLOWS boosted vs default
		{"debug", "debug", -0.3, graph.EdgeDataFlows, "gt"},
		// review: DirectionBoost=0.0, IMPLEMENTS boosted vs default
		{"review", "review", 0.0, graph.EdgeImplements, "gt"},
		// understand: DirectionBoost=0.2, weights equal to default
		{"understand", "understand", 0.2, graph.EdgeCalls, "eq"},
		// plan: DirectionBoost=0.2, IMPLEMENTS boosted vs default
		{"plan", "plan", 0.2, graph.EdgeImplements, "gt"},
		// add: DirectionBoost=0.2, IMPORTS boosted vs default
		{"add", "add", 0.2, graph.EdgeImports, "gt"},
	}

	for _, tc := range cases {
		t.Run(tc.intent, func(t *testing.T) {
			cfg := graph.DefaultCarveConfig()
			applyIntentCarveConfig(&cfg, tc.intent)

			// IntentID must match the intent string.
			if cfg.IntentID != tc.wantIntentID {
				t.Errorf("IntentID = %q, want %q", cfg.IntentID, tc.wantIntentID)
			}

			// DirectionBoost must match expected value.
			if cfg.DirectionBoost != tc.wantBoost {
				t.Errorf("DirectionBoost = %v, want %v", cfg.DirectionBoost, tc.wantBoost)
			}

			// EdgeWeights must be non-nil and contain the spot-checked edge type.
			if cfg.EdgeWeights == nil {
				t.Fatal("EdgeWeights is nil after applyIntentCarveConfig")
			}
			got := cfg.EdgeWeights[tc.checkEdgeType]
			def := graph.DefaultEdgeWeights[tc.checkEdgeType]
			switch tc.wantRelOp {
			case "gt":
				if got <= def {
					t.Errorf("EdgeWeights[%s] = %v, want > default %v", tc.checkEdgeType, got, def)
				}
			case "lt":
				if got >= def {
					t.Errorf("EdgeWeights[%s] = %v, want < default %v", tc.checkEdgeType, got, def)
				}
			case "eq":
				if got != def {
					t.Errorf("EdgeWeights[%s] = %v, want == default %v", tc.checkEdgeType, got, def)
				}
			}
		})
	}
}

// TestApplyIntentCarveConfig_ReplacesPointerNotMutatesDefault verifies that
// applyIntentCarveConfig replaces cfg.EdgeWeights with an intent-specific map
// and does NOT point to DefaultEdgeWeights (which would allow accidental
// mutation of the global).
func TestApplyIntentCarveConfig_ReplacesPointerNotMutatesDefault(t *testing.T) {
	// For every intent, the resulting EdgeWeights pointer must differ from
	// DefaultEdgeWeights for intents that change weights. Specifically, "modify"
	// sets EdgeImplements to a lower value than the default.
	intentsWithDifferentWeights := []string{"modify", "debug", "review", "add", "plan"}
	for _, intent := range intentsWithDifferentWeights {
		cfg := graph.DefaultCarveConfig()
		applyIntentCarveConfig(&cfg, intent)

		// The returned map must not be identical to DefaultEdgeWeights.
		// We check by looking for at least one weight that differs.
		differs := false
		for et, w := range cfg.EdgeWeights {
			if w != graph.DefaultEdgeWeights[et] {
				differs = true
				break
			}
		}
		if !differs {
			t.Errorf("intent=%q: EdgeWeights is identical to DefaultEdgeWeights — intent-specific weights not applied", intent)
		}
	}
}

// TestApplyIntentCarveConfig_UnderstandUsesDefaultWeights verifies that the
// "understand" intent uses default weights (balanced/unchanged).
func TestApplyIntentCarveConfig_UnderstandUsesDefaultWeights(t *testing.T) {
	cfg := graph.DefaultCarveConfig()
	applyIntentCarveConfig(&cfg, "understand")

	for et, def := range graph.DefaultEdgeWeights {
		if cfg.EdgeWeights[et] != def {
			t.Errorf("understand EdgeWeights[%s] = %v, want default %v", et, cfg.EdgeWeights[et], def)
		}
	}
}
