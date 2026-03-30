package config_test

// Additional tests for uncovered config functions:
// IsAgentRule, CheckViolationsForFile, globContains (via matchesForbidden),
// applyDefaults (brain/constitution), matchesForbidden edge cases,
// suggestFix branch coverage.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func makeRule(id, severity string, fe config.ForbiddenEdge) config.Rule {
	return config.Rule{
		ID:            id,
		Severity:      severity,
		Description:   "test rule",
		ForbiddenEdge: fe,
	}
}

func writeCoverageConfig(t *testing.T, cfg interface{}) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// buildCoverageGraph returns a graph where handlerFile calls dbFile.
func buildCoverageGraph() (*graph.Graph, graph.NodeID, graph.NodeID) {
	g := graph.New("/repo")
	fromID := g.MakeNodeID("/repo/api/handler.go", "HandleLogin")
	toID := g.MakeNodeID("/repo/db/query.go", "QueryUser")
	g.AddNode(&graph.Node{
		ID: fromID, Name: "HandleLogin", Type: graph.NodeFunction,
		File: "/repo/api/handler.go", Package: "api",
	})
	g.AddNode(&graph.Node{
		ID: toID, Name: "QueryUser", Type: graph.NodeFunction,
		File: "/repo/db/query.go", Package: "db",
	})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeCalls})
	return g, fromID, toID
}

// ── IsAgentRule ───────────────────────────────────────────────────────────────

func TestIsAgentRule_Agent(t *testing.T) {
	r := config.Rule{ID: "r1", Severity: "warning", RuleType: "agent"}
	if !r.IsAgentRule() {
		t.Error("expected IsAgentRule()=true for rule_type=agent")
	}
}

func TestIsAgentRule_Structural(t *testing.T) {
	r := config.Rule{ID: "r1", Severity: "warning", RuleType: "structural"}
	if r.IsAgentRule() {
		t.Error("expected IsAgentRule()=false for rule_type=structural")
	}
}

func TestIsAgentRule_Empty(t *testing.T) {
	r := config.Rule{ID: "r1", Severity: "warning"}
	if r.IsAgentRule() {
		t.Error("expected IsAgentRule()=false for empty rule_type")
	}
}

// ── CheckViolationsForFile ────────────────────────────────────────────────────

func TestCheckViolationsForFile_NoRules(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	cfg := &config.Config{}
	vs := cfg.CheckViolationsForFile(g, "/repo/api/handler.go")
	if len(vs) != 0 {
		t.Errorf("expected 0 violations with no rules, got %d", len(vs))
	}
}

func TestCheckViolationsForFile_MatchingFile(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	rule := makeRule("no-handler-db", "error", config.ForbiddenEdge{
		FromFilePattern: "*/api/*",
		ToFilePattern:   "*/db/*",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolationsForFile(g, "/repo/api/handler.go")
	if len(vs) == 0 {
		t.Error("expected at least one violation for matching file")
	}
}

func TestCheckViolationsForFile_UnrelatedFile(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	rule := makeRule("no-handler-db", "error", config.ForbiddenEdge{
		FromFilePattern: "*/api/*",
		ToFilePattern:   "*/db/*",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	// File not involved in any edge → 0 violations.
	vs := cfg.CheckViolationsForFile(g, "/repo/service/auth.go")
	if len(vs) != 0 {
		t.Errorf("expected 0 violations for unrelated file, got %d", len(vs))
	}
}

// ── applyDefaults (brain/scout/constitution) ──────────────────────────────────

func TestApplyDefaults_BrainEnabled(t *testing.T) {
	raw := map[string]interface{}{
		"version": "1",
		"brain":   map[string]interface{}{"enabled": true},
		"rules":   []interface{}{},
	}
	dir := writeCoverageConfig(t, raw)
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Brain.OllamaURL == "" {
		t.Error("expected brain.ollama_url to be defaulted")
	}
	if cfg.Brain.Model == "" {
		t.Error("expected brain.model to be defaulted")
	}
}

func TestApplyDefaults_ConstitutionPrinciples(t *testing.T) {
	raw := map[string]interface{}{
		"version": "1",
		"constitution": map[string]interface{}{
			"principles": []string{"always write tests"},
		},
		"rules": []interface{}{},
	}
	dir := writeCoverageConfig(t, raw)
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Constitution.InjectInContext {
		t.Error("expected InjectInContext=true when principles are set")
	}
	if !cfg.Constitution.InjectInSessionInit {
		t.Error("expected InjectInSessionInit=true when principles are set")
	}
}

// ── matchesForbidden edge cases (via CheckViolations) ─────────────────────────

func TestMatchesForbidden_EdgeTypeFilter(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	// Rule only fires for EdgeImports, but the graph has EdgeCalls.
	rule := makeRule("only-imports", "error", config.ForbiddenEdge{
		EdgeType: graph.EdgeImports,
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) != 0 {
		t.Errorf("expected 0 violations for EdgeImports filter on CALLS graph, got %d", len(vs))
	}
}

func TestMatchesForbidden_FromTypeFilter(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	// Rule only fires for NodeStruct source; graph has NodeFunction.
	rule := makeRule("struct-src", "warning", config.ForbiddenEdge{
		FromType: graph.NodeStruct,
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) != 0 {
		t.Errorf("expected 0 violations for FromType=struct, got %d", len(vs))
	}
}

func TestMatchesForbidden_ToTypeFilter(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	rule := makeRule("struct-dst", "warning", config.ForbiddenEdge{
		ToType: graph.NodeStruct,
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) != 0 {
		t.Errorf("expected 0 violations for ToType=struct, got %d", len(vs))
	}
}

func TestMatchesForbidden_ToNamePattern_Matches(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	// "QueryUser" contains "Query".
	rule := makeRule("no-direct-query", "error", config.ForbiddenEdge{
		ToNamePattern: "Query",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) == 0 {
		t.Error("expected violation for ToNamePattern=Query")
	}
}

func TestMatchesForbidden_ToNamePattern_NoMatch(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	rule := makeRule("no-xyz", "error", config.ForbiddenEdge{
		ToNamePattern: "XyzNeverExists",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) != 0 {
		t.Errorf("expected 0 violations for non-matching ToNamePattern, got %d", len(vs))
	}
}

func TestMatchesForbidden_ToNamePattern_ExactMatch(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	// Exact name match (not substring).
	rule := makeRule("exact-match", "error", config.ForbiddenEdge{
		ToNamePattern: "QueryUser",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) == 0 {
		t.Error("expected violation for exact ToNamePattern=QueryUser")
	}
}

func TestMatchesForbidden_AllEmpty_NoViolations(t *testing.T) {
	g, _, _ := buildCoverageGraph()
	// All-empty ForbiddenEdge should NOT match any edge (agent rule).
	rule := makeRule("agent-rule", "warning", config.ForbiddenEdge{})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) != 0 {
		t.Errorf("expected 0 violations for all-empty ForbiddenEdge, got %d", len(vs))
	}
}

// ── suggestFix via violation (integration path) ───────────────────────────────

func TestSuggestFix_HandlerToDBPattern(t *testing.T) {
	// handler.go → query.go (db access pattern)
	g := graph.New("/repo")
	fromID := g.MakeNodeID("/repo/api/handler.go", "HandleLogin")
	toID := g.MakeNodeID("/repo/db/query.go", "QueryUser")
	g.AddNode(&graph.Node{ID: fromID, Name: "HandleLogin", Type: graph.NodeFunction,
		File: "/repo/api/handler.go", Package: "api"})
	g.AddNode(&graph.Node{ID: toID, Name: "QueryUser", Type: graph.NodeFunction,
		File: "/repo/db/query.go", Package: "db"})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeCalls})

	rule := makeRule("no-handler-db", "error", config.ForbiddenEdge{
		FromFilePattern: "*/api/*",
		ToFilePattern:   "*/db/*",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) == 0 {
		t.Fatal("expected violation")
	}
	if vs[0].SuggestedFix == "" {
		t.Error("expected non-empty SuggestedFix")
	}
}

func TestSuggestFix_HandlerToGenericPattern(t *testing.T) {
	// handler.go calls a non-DB service.
	g := graph.New("/repo")
	fromID := g.MakeNodeID("/repo/api/handler.go", "HandleOrder")
	toID := g.MakeNodeID("/repo/svc/order.go", "PlaceOrder")
	g.AddNode(&graph.Node{ID: fromID, Name: "HandleOrder", Type: graph.NodeFunction,
		File: "/repo/api/handler.go", Package: "api"})
	g.AddNode(&graph.Node{ID: toID, Name: "PlaceOrder", Type: graph.NodeFunction,
		File: "/repo/svc/order.go", Package: "svc"})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeCalls})

	rule := makeRule("no-handler-svc-direct", "warning", config.ForbiddenEdge{
		FromFilePattern: "*/api/*",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) == 0 {
		t.Fatal("expected violation")
	}
	if vs[0].SuggestedFix == "" {
		t.Error("expected non-empty SuggestedFix")
	}
}

func TestSuggestFix_GenericCalls_WithToNamePattern(t *testing.T) {
	g := graph.New("/repo")
	fromID := g.MakeNodeID("/repo/svc/order.go", "PlaceOrder")
	toID := g.MakeNodeID("/repo/db/raw.go", "RawExec")
	g.AddNode(&graph.Node{ID: fromID, Name: "PlaceOrder", Type: graph.NodeFunction,
		File: "/repo/svc/order.go", Package: "svc"})
	g.AddNode(&graph.Node{ID: toID, Name: "RawExec", Type: graph.NodeFunction,
		File: "/repo/db/raw.go", Package: "db"})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeCalls})

	rule := makeRule("no-raw-exec", "error", config.ForbiddenEdge{
		ToNamePattern: "RawExec",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) == 0 {
		t.Fatal("expected violation")
	}
	if vs[0].SuggestedFix == "" {
		t.Error("expected non-empty SuggestedFix")
	}
}

func TestSuggestFix_ImportEdge_Handler(t *testing.T) {
	g := graph.New("/repo")
	fromID := g.MakeNodeID("/repo/api/handler.go", "HandlerPkg")
	toID := g.MakeNodeID("/repo/db/store.go", "StorePkg")
	g.AddNode(&graph.Node{ID: fromID, Name: "HandlerPkg", Type: graph.NodeFunction,
		File: "/repo/api/handler.go", Package: "api"})
	g.AddNode(&graph.Node{ID: toID, Name: "StorePkg", Type: graph.NodeFunction,
		File: "/repo/db/store.go", Package: "db"})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeImports})

	rule := makeRule("no-handler-import-db", "error", config.ForbiddenEdge{
		EdgeType: graph.EdgeImports,
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) == 0 {
		t.Fatal("expected violation")
	}
	if vs[0].SuggestedFix == "" {
		t.Error("expected non-empty SuggestedFix for IMPORTS")
	}
}

func TestSuggestFix_ImportEdge_WithFilePatterns(t *testing.T) {
	g := graph.New("/repo")
	fromID := g.MakeNodeID("/repo/svc/auth.go", "AuthSvc")
	toID := g.MakeNodeID("/repo/db/store.go", "StoreDB")
	g.AddNode(&graph.Node{ID: fromID, Name: "AuthSvc", Type: graph.NodeFunction,
		File: "/repo/svc/auth.go", Package: "svc"})
	g.AddNode(&graph.Node{ID: toID, Name: "StoreDB", Type: graph.NodeFunction,
		File: "/repo/db/store.go", Package: "db"})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeImports})

	rule := makeRule("svc-no-db-import", "warning", config.ForbiddenEdge{
		EdgeType:        graph.EdgeImports,
		FromFilePattern: "*/svc/*",
		ToFilePattern:   "*/db/*",
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) == 0 {
		t.Fatal("expected violation")
	}
	if vs[0].SuggestedFix == "" {
		t.Error("expected non-empty SuggestedFix for IMPORTS with file patterns")
	}
}

func TestSuggestFix_DefaultEdgeType(t *testing.T) {
	// Use EdgeDefines (non-CALLS, non-IMPORTS) to hit the default case.
	g := graph.New("/repo")
	fromID := g.MakeNodeID("/repo/svc/auth.go", "AuthSvc")
	toID := g.MakeNodeID("/repo/db/store.go", "StoreDB")
	g.AddNode(&graph.Node{ID: fromID, Name: "AuthSvc", Type: graph.NodeFunction,
		File: "/repo/svc/auth.go", Package: "svc"})
	g.AddNode(&graph.Node{ID: toID, Name: "StoreDB", Type: graph.NodeFunction,
		File: "/repo/db/store.go", Package: "db"})
	g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeDefines})

	rule := makeRule("no-defines", "warning", config.ForbiddenEdge{
		EdgeType: graph.EdgeDefines,
	})
	cfg := &config.Config{Rules: []config.Rule{rule}}
	vs := cfg.CheckViolations(g)
	if len(vs) == 0 {
		t.Fatal("expected violation for EdgeDefines rule")
	}
	if vs[0].SuggestedFix == "" {
		t.Error("expected non-empty SuggestedFix for default edge type")
	}
}

// ---------------------------------------------------------------------------
// ToBrainConfig — IntelligenceMode passthrough and auto-configuration
// ---------------------------------------------------------------------------

func TestToBrainConfig_PassesIntelligenceMode(t *testing.T) {
	bc := config.BrainConfig{
		Enabled:          true,
		IntelligenceMode: "standard",
		OllamaURL:        "http://localhost:11434",
	}
	got := bc.ToBrainConfig()
	if string(got.IntelligenceMode) != "standard" {
		t.Errorf("IntelligenceMode = %q, want %q", got.IntelligenceMode, "standard")
	}
}

func TestToBrainConfig_StandardMode_AutoConfiguresIdentities(t *testing.T) {
	bc := config.BrainConfig{
		Enabled:          true,
		IntelligenceMode: "standard",
		OllamaURL:        "http://localhost:11434",
	}
	got := bc.ToBrainConfig()
	if got.ModelIngest != "qwen3.5:0.8b" {
		t.Errorf("ModelIngest = %q, want qwen3.5:0.8b", got.ModelIngest)
	}
	if got.ModelGuardian != "qwen3.5:2b" {
		t.Errorf("ModelGuardian = %q, want qwen3.5:2b", got.ModelGuardian)
	}
	if got.ModelEnrich != "qwen3.5:4b" {
		t.Errorf("ModelEnrich = %q, want qwen3.5:4b", got.ModelEnrich)
	}
	if got.ModelOrchestrate != "qwen3.5:2b" {
		t.Errorf("ModelOrchestrate = %q, want qwen3.5:2b", got.ModelOrchestrate)
	}
	if got.ModelArchivist != "qwen3.5:2b" {
		t.Errorf("ModelArchivist = %q, want qwen3.5:2b", got.ModelArchivist)
	}
}

func TestToBrainConfig_OptimalMode_AllTiers2b(t *testing.T) {
	bc := config.BrainConfig{
		Enabled:          true,
		IntelligenceMode: "optimal",
	}
	got := bc.ToBrainConfig()
	if got.ModelGuardian != "qwen3.5:2b" {
		t.Errorf("ModelGuardian = %q, want qwen3.5:2b", got.ModelGuardian)
	}
	if got.ModelIngest != "qwen3.5:0.8b" {
		t.Errorf("ModelIngest = %q, want qwen3.5:0.8b", got.ModelIngest)
	}
}

func TestToBrainConfig_NoMode_LegacyBehavior(t *testing.T) {
	bc := config.BrainConfig{
		Enabled:   true,
		Model:     "qwen3.5:2b",
		OllamaURL: "http://localhost:11434",
	}
	got := bc.ToBrainConfig()
	// Without IntelligenceMode, AutoConfigureModels should NOT be called.
	// Tier models remain empty (to be filled by brain/config.applyDefaults).
	if string(got.IntelligenceMode) != "" {
		t.Errorf("IntelligenceMode = %q, want empty (legacy)", got.IntelligenceMode)
	}
}

func TestToBrainConfig_PassesGuardianAndArchivist(t *testing.T) {
	bc := config.BrainConfig{
		Enabled:        true,
		ModelGuardian:  "custom/guardian",
		ModelArchivist: "custom/archivist",
	}
	got := bc.ToBrainConfig()
	// Without IntelligenceMode, explicit values should pass through.
	if got.ModelGuardian != "custom/guardian" {
		t.Errorf("ModelGuardian = %q, want custom/guardian", got.ModelGuardian)
	}
	if got.ModelArchivist != "custom/archivist" {
		t.Errorf("ModelArchivist = %q, want custom/archivist", got.ModelArchivist)
	}
}

func TestApplyDefaults_BrainModel_ModeAware(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"standard", "qwen3.5:4b"},
		{"full", "qwen3.5:4b"},
		{"optimal", "qwen3.5:2b"},
		{"", "qwen3.5:2b"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		raw := map[string]interface{}{
			"brain": map[string]interface{}{
				"enabled": true,
			},
		}
		if tc.mode != "" {
			raw["brain"].(map[string]interface{})["intelligence_mode"] = tc.mode
		}
		data, _ := json.Marshal(raw)
		os.WriteFile(filepath.Join(dir, "synapses.json"), data, 0o644)
		cfg, err := config.Load(dir)
		if err != nil {
			t.Fatalf("mode=%q: Load failed: %v", tc.mode, err)
		}
		if cfg.Brain.Model != tc.want {
			t.Errorf("mode=%q: Brain.Model = %q, want %q", tc.mode, cfg.Brain.Model, tc.want)
		}
	}
}
