package config_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// buildScopedViolationGraph creates:
//
//	auth.go:  handler --CALLS--> Login (auth.go itself)
//	auth.go:  Login   --CALLS--> DB    (Login in auth.go, DB in db.go)
func buildScopedViolationGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("repo")

	handlerID := g.MakeNodeID("auth.go", "handler")
	loginID := g.MakeNodeID("auth.go", "Login")
	dbID := g.MakeNodeID("db.go", "DB")

	g.AddNode(&graph.Node{ID: handlerID, Type: graph.NodeFunction, Name: "handler", File: "auth.go", Package: "handler"})
	g.AddNode(&graph.Node{ID: loginID, Type: graph.NodeFunction, Name: "Login", File: "auth.go", Package: "auth"})
	g.AddNode(&graph.Node{ID: dbID, Type: graph.NodeFunction, Name: "DB", File: "db.go", Package: "db"})

	// Within auth.go
	g.AddEdge(&graph.Edge{From: handlerID, To: loginID, Type: graph.EdgeCalls})
	// auth.go → db.go
	g.AddEdge(&graph.Edge{From: loginID, To: dbID, Type: graph.EdgeCalls})

	return g
}

func TestCheckViolationsForFile_FindsViolationsTouchingFile(t *testing.T) {
	g := buildScopedViolationGraph(t)

	// Rule: no CALLS edges where the target is named "DB"
	cfg := &config.Config{
		Rules: []config.Rule{
			{
				ID:       "no-db-calls",
				Severity: "error",
				ForbiddenEdge: config.ForbiddenEdge{
					EdgeType:      graph.EdgeCalls,
					ToNamePattern: "DB",
				},
			},
		},
	}

	// auth.go contains Login which calls DB → should find violation
	// (Login is an endpoint of the edge, so the edge touches auth.go)
	violations := cfg.CheckViolationsForFile(g, "auth.go")
	found := false
	for _, v := range violations {
		if v.RuleID == "no-db-calls" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'no-db-calls' violation for auth.go (Login→DB edge), got %v", violations)
	}
}

func TestCheckViolationsForFile_DoesNotFindEdgesUnrelatedToFile(t *testing.T) {
	g := buildScopedViolationGraph(t)

	// Rule: no CALLS edges where the target is named "Login"
	cfg := &config.Config{
		Rules: []config.Rule{
			{
				ID:       "no-login-calls",
				Severity: "error",
				ForbiddenEdge: config.ForbiddenEdge{
					EdgeType:      graph.EdgeCalls,
					ToNamePattern: "Login",
				},
			},
		},
	}

	// db.go has no edges touching it that call Login → no violations
	violations := cfg.CheckViolationsForFile(g, "db.go")
	for _, v := range violations {
		if v.RuleID == "no-login-calls" {
			t.Errorf("unexpected violation for db.go: db.go has no edges to Login")
		}
	}
}

func TestCheckViolationsForFile_UnknownFileNoViolations(t *testing.T) {
	g := buildScopedViolationGraph(t)

	cfg := &config.Config{
		Rules: []config.Rule{
			{
				ID:       "any-calls",
				Severity: "error",
				ForbiddenEdge: config.ForbiddenEdge{
					EdgeType: graph.EdgeCalls,
				},
			},
		},
	}

	violations := cfg.CheckViolationsForFile(g, "nonexistent.go")
	if len(violations) != 0 {
		t.Errorf("want 0 violations for unknown file, got %d", len(violations))
	}
}

func TestCheckViolationsForFile_NoRulesReturnsNil(t *testing.T) {
	g := buildScopedViolationGraph(t)
	cfg := &config.Config{}

	violations := cfg.CheckViolationsForFile(g, "auth.go")
	if violations != nil {
		t.Errorf("want nil with no rules, got %v", violations)
	}
}
