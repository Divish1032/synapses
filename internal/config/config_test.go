package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// writeConfig writes a synapses.json into a temp directory and returns the dir path.
func writeConfig(t *testing.T, cfg config.Config) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}

func TestLoad_MissingFile_ReturnsDefaults(t *testing.T) {
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load() on empty dir returned error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
	// Defaults must be applied.
	cc := cfg.CarveConfig()
	if cc.MaxDepth <= 0 {
		t.Errorf("default MaxDepth should be > 0, got %d", cc.MaxDepth)
	}
	if cc.TokenBudget <= 0 {
		t.Errorf("default TokenBudget should be > 0, got %d", cc.TokenBudget)
	}
}

func TestLoad_ValidFile(t *testing.T) {
	dir := writeConfig(t, config.Config{
		Version: "1",
		Rules: []config.Rule{
			{
				ID:          "no-sql-in-view",
				Description: "test rule",
				ForbiddenEdge: config.ForbiddenEdge{
					FromFilePattern: "*.tsx",
					EdgeType:        graph.EdgeCalls,
				},
				Severity: "error",
			},
		},
	})

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(cfg.Rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(cfg.Rules))
	}
	if cfg.Rules[0].ID != "no-sql-in-view" {
		t.Errorf("rule ID = %q, want 'no-sql-in-view'", cfg.Rules[0].ID)
	}
}

func TestLoad_InvalidSeverity_ReturnsError(t *testing.T) {
	dir := writeConfig(t, config.Config{
		Rules: []config.Rule{
			{ID: "bad", Description: "x", Severity: "critical"}, // not error/warning
		},
	})
	_, err := config.Load(dir)
	if err == nil {
		t.Error("Load() with invalid severity should return error")
	}
}

func TestLoad_MissingRuleID_ReturnsError(t *testing.T) {
	dir := writeConfig(t, config.Config{
		Rules: []config.Rule{
			{ID: "", Description: "no id", Severity: "error"},
		},
	})
	_, err := config.Load(dir)
	if err == nil {
		t.Error("Load() with empty rule ID should return error")
	}
}

func TestFindConfigDir_ParentDir(t *testing.T) {
	// Setup: create /repo/synapses.json and a sub-directory /repo/a/b/c
	baseDir := writeConfig(t, config.Config{Version: "1"})
	subDir := filepath.Join(baseDir, "a", "b", "c")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Should walk up and find it in baseDir
	foundDir, ok := config.FindConfigDir(subDir)
	if !ok {
		t.Fatal("FindConfigDir returned false")
	}
	if foundDir != baseDir {
		t.Errorf("FindConfigDir returned %q, want %q", foundDir, baseDir)
	}
}

func TestFindConfigDir_NotFound(t *testing.T) {
	dir := t.TempDir()
	// No synapses.json created

	subDir := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	foundDir, ok := config.FindConfigDir(subDir)
	if ok {
		t.Errorf("FindConfigDir returned true with dir %q, want false", foundDir)
	}
	if foundDir != subDir {
		t.Errorf("FindConfigDir returned %q, want %q when ok=false", foundDir, subDir)
	}
}

func TestCarveConfig_OverridesApplied(t *testing.T) {
	dir := writeConfig(t, config.Config{
		Rules: []config.Rule{},
		ContextCarve: config.ContextCarveConfig{
			DefaultDepth: 4,
			DecayFactor:  0.3,
			TokenBudget:  8000,
		},
	})

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	cc := cfg.CarveConfig()
	if cc.MaxDepth != 4 {
		t.Errorf("MaxDepth = %d, want 4", cc.MaxDepth)
	}
	if cc.TokenBudget != 8000 {
		t.Errorf("TokenBudget = %d, want 8000", cc.TokenBudget)
	}
	if cc.DecayFactor != 0.3 {
		t.Errorf("DecayFactor = %f, want 0.3", cc.DecayFactor)
	}
}

// TestCarveConfig_HybridLambda_PointerSemantics verifies the *float64 behaviour:
//   - nil (unset in JSON)           → CarveConfig.HybridLambda == 0 (handler applies default)
//   - explicit 0.0 in JSON          → CarveConfig.HybridLambda == 0 (disable hybrid)
//   - explicit 0.5 in JSON          → CarveConfig.HybridLambda == 0.5
//
// The *float64 type is critical: without it, hybrid_lambda: 0 and omitted lambda
// are indistinguishable, so users cannot disable hybrid scoring via synapses.json.
func TestCarveConfig_HybridLambda_PointerSemantics(t *testing.T) {
	zero := 0.0
	half := 0.5

	cases := []struct {
		name   string
		lambda *float64
		want   float64
	}{
		{"nil (unset)", nil, 0},
		{"explicit 0.0", &zero, 0},
		{"explicit 0.5", &half, 0.5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeConfig(t, config.Config{
				ContextCarve: config.ContextCarveConfig{HybridLambda: tc.lambda},
			})
			cfg, err := config.Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			cc := cfg.CarveConfig()
			if cc.HybridLambda != tc.want {
				t.Errorf("HybridLambda = %v, want %v", cc.HybridLambda, tc.want)
			}
			// Verify the pointer itself is correctly preserved for nil/non-nil check
			// in handlers_context.go (the key distinction between unset and explicit-0).
			if tc.lambda == nil && cfg.ContextCarve.HybridLambda != nil {
				t.Error("expected ContextCarve.HybridLambda to be nil when unset")
			}
			if tc.lambda != nil && cfg.ContextCarve.HybridLambda == nil {
				t.Errorf("expected ContextCarve.HybridLambda to be non-nil for value %v", *tc.lambda)
			}
		})
	}
}

// FIX-RESOLVER-1: use_go_types should default to true when go.mod is present.
func TestLoad_UseGoTypes_DefaultsTrueWhenGoModPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.UseGoTypes {
		t.Error("expected UseGoTypes=true when go.mod is present, got false")
	}
}

func TestLoad_UseGoTypes_FalseWhenNoGoMod(t *testing.T) {
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UseGoTypes {
		t.Error("expected UseGoTypes=false when no go.mod, got true")
	}
}

func TestLoad_UseGoTypes_ExplicitFalseRespected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// synapses.json with explicit use_go_types: false — should override the go.mod default.
	cfgJSON := []byte(`{"use_go_types": false}`)
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), cfgJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UseGoTypes {
		t.Error("explicit use_go_types=false in synapses.json should not be overridden by go.mod detection")
	}
}

func TestCheckViolations_NoRules(t *testing.T) {
	cfg, _ := config.Load(t.TempDir()) // empty dir → no rules
	g := buildViolationGraph(t)
	violations := cfg.CheckViolations(g)
	if len(violations) != 0 {
		t.Errorf("expected 0 violations with no rules, got %d", len(violations))
	}
}

func TestCheckViolations_EdgeTypeMatch(t *testing.T) {
	dir := writeConfig(t, config.Config{
		Rules: []config.Rule{{
			ID:       "no-calls",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				EdgeType: graph.EdgeCalls,
			},
		}},
	})
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	g := buildViolationGraph(t) // contains a CALLS edge

	violations := cfg.CheckViolations(g)
	if len(violations) == 0 {
		t.Error("expected at least 1 violation for CALLS edge rule")
	}
	if violations[0].RuleID != "no-calls" {
		t.Errorf("violation RuleID = %q, want 'no-calls'", violations[0].RuleID)
	}
	if violations[0].Severity != "error" {
		t.Errorf("violation Severity = %q, want 'error'", violations[0].Severity)
	}
}

func TestCheckViolations_FilePatternMatch(t *testing.T) {
	dir := writeConfig(t, config.Config{
		Rules: []config.Rule{{
			ID:       "no-tsx-calls",
			Severity: "warning",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*.tsx",
				EdgeType:        graph.EdgeCalls,
			},
		}},
	})
	cfg, _ := config.Load(dir)
	g := buildViolationGraph(t) // handler.tsx calls a function

	violations := cfg.CheckViolations(g)
	if len(violations) == 0 {
		t.Error("expected violation for tsx CALLS edge")
	}
}

func TestCheckViolations_FilePatternNoMatch(t *testing.T) {
	dir := writeConfig(t, config.Config{
		Rules: []config.Rule{{
			ID:       "no-go-in-tsx",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*.tsx",
				EdgeType:        graph.EdgeCalls,
				ToFilePattern:   "*.go", // target must be a .go file
			},
		}},
	})
	cfg, _ := config.Load(dir)

	// Graph has tsx→tsx CALLS — ToFilePattern=*.go should NOT match.
	g := graph.New("test")
	handlerID := graph.NodeID("test::handler.tsx::handler.tsx")
	loginID := graph.NodeID("test::handler.tsx::LoginForm")
	g.AddNode(&graph.Node{ID: handlerID, Type: graph.NodeFile, Name: "handler.tsx", File: "handler.tsx"})
	g.AddNode(&graph.Node{ID: loginID, Type: graph.NodeFunction, Name: "LoginForm", File: "handler.tsx"})
	g.AddEdge(&graph.Edge{From: handlerID, To: loginID, Type: graph.EdgeCalls})

	violations := cfg.CheckViolations(g)
	if len(violations) != 0 {
		t.Errorf("expected 0 violations (ToFilePattern *.go not matched), got %d", len(violations))
	}
}

func TestCheckViolations_PathComponentPatterns(t *testing.T) {
	tests := []struct {
		name            string
		fromFilePattern string
		toFilePattern   string
		fromFile        string
		toFile          string
		wantViolation   bool
	}{
		{
			name:            "path-component from_file_pattern matches deep path",
			fromFilePattern: "*/mcp/*",
			fromFile:        "synapses/internal/mcp/tools.go",
			toFile:          "synapses/internal/graph/graph.go",
			wantViolation:   true,
		},
		{
			name:          "path-component to_file_pattern matches deep path",
			toFilePattern: "*/parser/*",
			fromFile:      "synapses/internal/mcp/tools.go",
			toFile:        "synapses/internal/parser/golang.go",
			wantViolation: true,
		},
		{
			name:            "from_file_pattern does not match wrong directory",
			fromFilePattern: "*/graph/*",
			fromFile:        "mcp/tools.go",
			toFile:          "other/file.go",
			wantViolation:   false,
		},
		{
			name:            "simple basename glob matches",
			fromFilePattern: "*.go",
			fromFile:        "handler.go",
			toFile:          "db.go",
			wantViolation:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeConfig(t, config.Config{
				Rules: []config.Rule{{
					ID:       "test-rule",
					Severity: "error",
					ForbiddenEdge: config.ForbiddenEdge{
						FromFilePattern: tc.fromFilePattern,
						ToFilePattern:   tc.toFilePattern,
						EdgeType:        graph.EdgeCalls,
					},
				}},
			})

			cfg, err := config.Load(dir)
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}

			g := graph.New("test")
			fromID := graph.NodeID("test::" + tc.fromFile + "::Caller")
			toID := graph.NodeID("test::" + tc.toFile + "::Callee")
			g.AddNode(&graph.Node{ID: fromID, Type: graph.NodeFunction, Name: "Caller", File: tc.fromFile})
			g.AddNode(&graph.Node{ID: toID, Type: graph.NodeFunction, Name: "Callee", File: tc.toFile})
			g.AddEdge(&graph.Edge{From: fromID, To: toID, Type: graph.EdgeCalls})

			violations := cfg.CheckViolations(g)
			if tc.wantViolation && len(violations) == 0 {
				t.Error("expected a violation, got none")
			}
			if !tc.wantViolation && len(violations) != 0 {
				t.Errorf("expected no violation, got %d: %+v", len(violations), violations)
			}
		})
	}
}

func TestCheckViolationsForEdges_MatchesSubset(t *testing.T) {
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-calls",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				EdgeType: graph.EdgeCalls,
			},
		}},
	}
	g := buildViolationGraph(t)
	edges := g.AllEdges() // one CALLS edge: handler.tsx → Login

	violations := cfg.CheckViolationsForEdges(edges, g.GetNode)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation, got %d", len(violations))
	}
	if violations[0].RuleID != "no-calls" {
		t.Errorf("violation RuleID = %q, want 'no-calls'", violations[0].RuleID)
	}
}

func TestCheckViolationsForEdges_NoEdges(t *testing.T) {
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-calls",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				EdgeType: graph.EdgeCalls,
			},
		}},
	}
	violations := cfg.CheckViolationsForEdges(nil, func(_ graph.NodeID) *graph.Node { return nil })
	if len(violations) != 0 {
		t.Errorf("expected no violations for nil edges, got %d", len(violations))
	}
}

func TestCheckViolationsForEdges_NoRules(t *testing.T) {
	cfg := &config.Config{}
	g := buildViolationGraph(t)
	edges := g.AllEdges()
	violations := cfg.CheckViolationsForEdges(edges, g.GetNode)
	if len(violations) != 0 {
		t.Errorf("expected no violations with no rules, got %d", len(violations))
	}
}

// buildViolationGraph creates a small graph with a .tsx file calling a function.
func buildViolationGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("test")

	handlerID := graph.NodeID("test::handler.tsx::handler.tsx")
	loginFnID := graph.NodeID("test::auth.go::Login")

	g.AddNode(&graph.Node{ID: handlerID, Type: graph.NodeFile, Name: "handler.tsx", File: "handler.tsx"})
	g.AddNode(&graph.Node{ID: loginFnID, Type: graph.NodeFunction, Name: "Login", File: "auth.go"})
	g.AddEdge(&graph.Edge{From: handlerID, To: loginFnID, Type: graph.EdgeCalls})

	return g
}

func TestFederationACL_IsAllowed(t *testing.T) {
	tests := []struct {
		name    string
		acl     *config.FederationACLConfig
		project string
		want    bool
	}{
		{"nil ACL denies all", nil, "any-project", false},
		{"empty AllowReadFrom denies all", &config.FederationACLConfig{}, "any", false},
		{"explicit empty slice denies all", &config.FederationACLConfig{AllowReadFrom: []string{}}, "any", false},
		{"wildcard allows all", &config.FederationACLConfig{AllowReadFrom: []string{"*"}}, "anything", true},
		{"exact match allows", &config.FederationACLConfig{AllowReadFrom: []string{"backend"}}, "backend", true},
		{"non-matching denies", &config.FederationACLConfig{AllowReadFrom: []string{"backend"}}, "frontend", false},
		{"multiple entries", &config.FederationACLConfig{AllowReadFrom: []string{"a", "b"}}, "b", true},
		{"multiple entries deny unlisted", &config.FederationACLConfig{AllowReadFrom: []string{"a", "b"}}, "c", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.acl.IsAllowed(tt.project)
			if got != tt.want {
				t.Errorf("IsAllowed(%q) = %v, want %v", tt.project, got, tt.want)
			}
		})
	}
}

func TestFederationACL_JSONRoundTrip(t *testing.T) {
	cfg := config.Config{
		Version: "1",
		FederationACL: &config.FederationACLConfig{
			AllowReadFrom: []string{"project-a", "project-b"},
		},
	}

	dir := writeConfig(t, cfg)
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.FederationACL == nil {
		t.Fatal("FederationACL should not be nil after load")
	}
	if len(loaded.FederationACL.AllowReadFrom) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(loaded.FederationACL.AllowReadFrom))
	}
	if !loaded.FederationACL.IsAllowed("project-a") {
		t.Error("project-a should be allowed")
	}
	if loaded.FederationACL.IsAllowed("project-c") {
		t.Error("project-c should NOT be allowed")
	}
}

// ── Path-Pattern Architectural Rule Tests ────────────────────────────────────

// buildLayeredGraph creates a 3-layer graph: handler → service → db.
// Edges: handler -CALLS-> service -CALLS-> db.
// Also includes a direct handler -CALLS-> db edge (the violation).
// Absolute paths are used so that matchFilePath's progressive-suffix stripping
// can match patterns like "*/handlers/*" against "/repo/handlers/order.go".
func buildLayeredGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("/repo")

	handlerID := g.MakeNodeID("/repo/handlers/order.go", "HandleOrder")
	serviceID := g.MakeNodeID("/repo/service/order.go", "OrderService")
	dbID := g.MakeNodeID("/repo/db/store.go", "Insert")

	g.AddNode(&graph.Node{ID: handlerID, Type: graph.NodeFunction, Name: "HandleOrder", File: "/repo/handlers/order.go"})
	g.AddNode(&graph.Node{ID: serviceID, Type: graph.NodeFunction, Name: "OrderService", File: "/repo/service/order.go"})
	g.AddNode(&graph.Node{ID: dbID, Type: graph.NodeFunction, Name: "Insert", File: "/repo/db/store.go"})

	// Proper path: handler → service → db (two-hop, via service layer).
	g.AddEdge(&graph.Edge{From: handlerID, To: serviceID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: serviceID, To: dbID, Type: graph.EdgeCalls})

	// Direct violation: handler → db (one-hop, skips service layer).
	g.AddEdge(&graph.Edge{From: handlerID, To: dbID, Type: graph.EdgeCalls})

	return g
}

func TestCheckViolations_PathPattern_DirectCall(t *testing.T) {
	// Rule: handler CALLS db directly is forbidden (1-hop path pattern).
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-handler-direct-db",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*/handlers/*",
				ToFilePattern:   "*/db/*",
				PathPattern:     []graph.EdgeType{graph.EdgeCalls},
			},
		}},
	}

	g := buildLayeredGraph(t)
	violations := cfg.CheckViolations(g)

	// Should detect the direct handler→db CALLS edge.
	found := false
	for _, v := range violations {
		if v.RuleID == "no-handler-direct-db" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected violation 'no-handler-direct-db', got %v", violations)
	}
}

func TestCheckViolations_PathPattern_TwoHop(t *testing.T) {
	// Rule: handler→X→db (two-hop) is forbidden — detects indirect-but-missing-service paths.
	// This catches handler→service→db where service is NOT a proper service layer.
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-two-hop-handler-db",
			Severity: "warning",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*/handlers/*",
				ToFilePattern:   "*/db/*",
				PathPattern:     []graph.EdgeType{graph.EdgeCalls, graph.EdgeCalls},
			},
		}},
	}

	g := buildLayeredGraph(t)
	violations := cfg.CheckViolations(g)

	found := false
	for _, v := range violations {
		if v.RuleID == "no-two-hop-handler-db" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected violation 'no-two-hop-handler-db' for handler→service→db path, got %v", violations)
	}
}

func TestCheckViolations_PathPattern_NoViolation(t *testing.T) {
	// Rule requires 3-hop path handler→X→Y→db. Graph only has 1-hop and 2-hop.
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-three-hop",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*/handlers/*",
				ToFilePattern:   "*/db/*",
				PathPattern:     []graph.EdgeType{graph.EdgeCalls, graph.EdgeCalls, graph.EdgeCalls},
			},
		}},
	}

	g := buildLayeredGraph(t)
	violations := cfg.CheckViolations(g)

	for _, v := range violations {
		if v.RuleID == "no-three-hop" {
			t.Errorf("unexpected violation 'no-three-hop': graph has no 3-hop path")
		}
	}
}

func TestCheckViolations_PathPattern_WrongEdgeType(t *testing.T) {
	// Rule uses IMPORTS edge type — graph only has CALLS edges. Should find no violations.
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-handler-imports-db",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*/handlers/*",
				ToFilePattern:   "*/db/*",
				PathPattern:     []graph.EdgeType{graph.EdgeImports},
			},
		}},
	}

	g := buildLayeredGraph(t)
	violations := cfg.CheckViolations(g)

	for _, v := range violations {
		if v.RuleID == "no-handler-imports-db" {
			t.Errorf("unexpected violation: graph has no IMPORTS edges")
		}
	}
}

func TestCheckViolations_PathPattern_EmptyGraph(t *testing.T) {
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-handler-db",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				PathPattern: []graph.EdgeType{graph.EdgeCalls},
			},
		}},
	}

	g := graph.New("empty")
	violations := cfg.CheckViolations(g)

	if len(violations) != 0 {
		t.Errorf("expected 0 violations on empty graph, got %d", len(violations))
	}
}

func TestCheckViolations_PathPattern_DoesNotFireForSingleEdgeRules(t *testing.T) {
	// A rule with EdgeType but no PathPattern must NOT be affected by the
	// path-pattern code path. Existing single-edge behaviour unchanged.
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "no-calls",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				EdgeType: graph.EdgeCalls,
			},
		}},
	}

	g := buildLayeredGraph(t)
	violations := cfg.CheckViolations(g)

	// All CALLS edges should be caught by the normal single-edge path.
	if len(violations) == 0 {
		t.Error("expected violations for CALLS rule without path_pattern")
	}
}

func TestCheckViolations_PathPattern_ViolationFields(t *testing.T) {
	// Verify the violation has correct RuleID, Severity, and non-zero node IDs.
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:          "check-fields",
			Severity:    "warning",
			Description: "handler must not call db directly",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*/handlers/*",
				ToFilePattern:   "*/db/*",
				PathPattern:     []graph.EdgeType{graph.EdgeCalls},
			},
		}},
	}

	g := buildLayeredGraph(t)
	violations := cfg.CheckViolations(g)

	var v *config.Violation
	for i := range violations {
		if violations[i].RuleID == "check-fields" {
			v = &violations[i]
			break
		}
	}
	if v == nil {
		t.Fatal("expected violation 'check-fields', got none")
	}
	if v.Severity != "warning" {
		t.Errorf("Severity = %q, want 'warning'", v.Severity)
	}
	if v.FromNode == "" {
		t.Error("FromNode should not be empty")
	}
	if v.ToNode == "" {
		t.Error("ToNode should not be empty")
	}
	if v.EdgeType != graph.EdgeCalls {
		t.Errorf("EdgeType = %q, want CALLS", v.EdgeType)
	}
}

func TestCheckViolationsForFile_PathPattern(t *testing.T) {
	// File-scoped check: only fire when from-node is in the changed file.
	cfg := &config.Config{
		Rules: []config.Rule{{
			ID:       "scoped-handler-db",
			Severity: "error",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*/handlers/*",
				ToFilePattern:   "*/db/*",
				PathPattern:     []graph.EdgeType{graph.EdgeCalls},
			},
		}},
	}

	g := buildLayeredGraph(t)

	// Changed file is the handler — should detect violation.
	violations := cfg.CheckViolationsForFile(g, "/repo/handlers/order.go")
	found := false
	for _, v := range violations {
		if v.RuleID == "scoped-handler-db" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected violation when handler file changed")
	}

	// Changed file is the service — handler is NOT in this file, no violation.
	violations = cfg.CheckViolationsForFile(g, "/repo/service/order.go")
	for _, v := range violations {
		if v.RuleID == "scoped-handler-db" {
			t.Error("unexpected path-pattern violation when non-handler file changed")
		}
	}
}

func TestPathPattern_JSONRoundTrip_SynapsesJSON(t *testing.T) {
	// Verify that path_pattern survives a full synapses.json write → Load cycle.
	// This tests the JSON unmarshaling path that agents and users hit when
	// they write path-pattern rules directly in their synapses.json config.
	cfg := config.Config{
		Version: "1",
		Rules: []config.Rule{{
			ID:          "no-handler-db-json",
			Description: "handlers must not call db directly",
			Severity:    "error",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: "*/handlers/*",
				ToFilePattern:   "*/db/*",
				PathPattern:     []graph.EdgeType{graph.EdgeCalls, graph.EdgeCalls},
			},
		}},
	}

	dir := writeConfig(t, cfg)
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(loaded.Rules))
	}
	r := loaded.Rules[0]
	if r.ID != "no-handler-db-json" {
		t.Errorf("rule ID = %q", r.ID)
	}
	if len(r.ForbiddenEdge.PathPattern) != 2 {
		t.Fatalf("PathPattern len = %d, want 2", len(r.ForbiddenEdge.PathPattern))
	}
	if r.ForbiddenEdge.PathPattern[0] != graph.EdgeCalls {
		t.Errorf("PathPattern[0] = %q, want CALLS", r.ForbiddenEdge.PathPattern[0])
	}
	if r.ForbiddenEdge.PathPattern[1] != graph.EdgeCalls {
		t.Errorf("PathPattern[1] = %q, want CALLS", r.ForbiddenEdge.PathPattern[1])
	}
	if r.ForbiddenEdge.FromFilePattern != "*/handlers/*" {
		t.Errorf("FromFilePattern = %q", r.ForbiddenEdge.FromFilePattern)
	}

	// End-to-end: loaded config must detect violations on the layered graph.
	g := buildLayeredGraph(t)
	violations := loaded.CheckViolations(g)
	found := false
	for _, v := range violations {
		if v.RuleID == "no-handler-db-json" {
			found = true
			break
		}
	}
	if !found {
		t.Error("loaded config did not detect path-pattern violation")
	}
}

func TestPathPattern_MultipleRulesShareNodeSnapshot(t *testing.T) {
	// Two path-pattern rules must both fire correctly — validates that the
	// lazy allNodes snapshot is shared correctly between rules.
	cfg := &config.Config{
		Rules: []config.Rule{
			{
				ID:       "rule-direct",
				Severity: "error",
				ForbiddenEdge: config.ForbiddenEdge{
					FromFilePattern: "*/handlers/*",
					ToFilePattern:   "*/db/*",
					PathPattern:     []graph.EdgeType{graph.EdgeCalls},
				},
			},
			{
				ID:       "rule-twohop",
				Severity: "warning",
				ForbiddenEdge: config.ForbiddenEdge{
					FromFilePattern: "*/handlers/*",
					ToFilePattern:   "*/db/*",
					PathPattern:     []graph.EdgeType{graph.EdgeCalls, graph.EdgeCalls},
				},
			},
		},
	}

	g := buildLayeredGraph(t)
	violations := cfg.CheckViolations(g)

	ruleDirect, ruleTwoHop := false, false
	for _, v := range violations {
		switch v.RuleID {
		case "rule-direct":
			ruleDirect = true
		case "rule-twohop":
			ruleTwoHop = true
		}
	}
	if !ruleDirect {
		t.Error("rule-direct did not fire")
	}
	if !ruleTwoHop {
		t.Error("rule-twohop did not fire")
	}
}
