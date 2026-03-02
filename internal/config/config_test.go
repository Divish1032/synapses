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

// --- Peer config tests ---

func TestPeerConfig_DefaultTrustLevel(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":"1","peers":[{"name":"backend","url":"http://localhost:8767"}]}`
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(cfg.Peers))
	}
	if cfg.Peers[0].TrustLevel != "read_only" {
		t.Errorf("TrustLevel = %q, want %q", cfg.Peers[0].TrustLevel, "read_only")
	}
}

func TestPeerConfig_EnvVarOverridesToken(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":"1","peers":[{"name":"my-svc","url":"http://localhost:8767","token":"json-token"}]}`
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNAPSES_PEER_TOKEN_MY_SVC", "env-token")

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Peers[0].Token != "env-token" {
		t.Errorf("Token = %q, want %q (env var should win)", cfg.Peers[0].Token, "env-token")
	}
}

func TestPeerConfig_PeerAPITokenFromEnv(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":"1","peer_api_port":8766}`
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNAPSES_PEER_API_TOKEN", "incoming-token")

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PeerAPIToken != "incoming-token" {
		t.Errorf("PeerAPIToken = %q, want %q", cfg.PeerAPIToken, "incoming-token")
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
