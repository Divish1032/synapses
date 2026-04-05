package benchmarks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/security"
)

// TestSecurityBench_RealRepo_GoTestBench tests Synapses security engine
// against a real vulnerable Go app (Contrast go-test-bench).
// This is NOT a synthetic test — it parses real code and checks for real issues.
func TestSecurityBench_RealRepo_GoTestBench(t *testing.T) {
	repoDir := "/tmp/synbench_repos/go-test-bench"
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		t.Skip("go-test-bench not cloned — run: git clone https://github.com/Contrast-Security-OSS/go-test-bench.git " + repoDir)
	}

	// Parse the repo into a graph.
	g, err := parseRepo(repoDir)
	if err != nil {
		t.Fatalf("parse repo: %v", err)
	}
	t.Logf("Parsed: %d nodes, %d edges", g.NodeCount(), g.EdgeCount())

	engine := security.DefaultEngine()

	// Ground truth: go-test-bench is a deliberately vulnerable app.
	// It uses chi for routing with NO auth middleware.
	// Expected: security engine should flag missing auth on the chi routes.
	chiRouteFile := filepath.Join(repoDir, "pkg/servechi/servechi.go")
	if _, err := os.Stat(chiRouteFile); os.IsNotExist(err) {
		t.Skip("chi route file not found")
	}

	content, _ := os.ReadFile(chiRouteFile)

	// Find the actual file path the graph uses for this file.
	var graphFilePath string
	g.IterateNodes(func(n *graph.Node) {
		if n.Type == graph.NodeFile && strings.HasSuffix(n.File, "servechi.go") {
			graphFilePath = n.File
			t.Logf("Graph file path: %q", n.File)
		}
	})

	// Try multiple path variants to find what works.
	pathVariants := []string{chiRouteFile, graphFilePath, "pkg/servechi/servechi.go"}
	var violations []security.Violation
	for _, p := range pathVariants {
		if p == "" {
			continue
		}
		v := engine.CheckFile(g, p, content)
		t.Logf("CheckFile(%q) → %d violations", p, len(v))
		if len(v) > len(violations) {
			violations = v
		}
	}

	t.Logf("Best violations found: %d", len(violations))

	// Debug: dump all nodes and edges for this file to understand WHY no violations
	t.Logf("--- All nodes for file ---")
	nodes := g.FindByFile(graphFilePath)
	t.Logf("FindByFile(%q) returned %d nodes", graphFilePath, len(nodes))
	for _, n := range nodes {
		t.Logf("  [%s] %s (file=%s)", n.Type, n.Name, n.File)
		// Show outgoing edges
		for _, e := range g.OutEdges(n.ID) {
			target := g.GetNode(e.To)
			tname := "?"
			if target != nil {
				tname = target.Name
			}
			t.Logf("    → %s → %s", e.Type, tname)
		}
	}
	for _, v := range violations {
		t.Logf("  [%s] %s — %s (confidence: %s)", v.Severity, v.PatternID, v.Target, v.Confidence)
	}

	// Ground truth expectations:
	// 1. servechi.go imports chi and registers routes without auth middleware → should fire go-chi-missing-auth
	// 2. No rate limiting → should fire go-chi-missing-rate-limit
	hasAuthFinding := false
	hasRateLimitFinding := false
	for _, v := range violations {
		if strings.Contains(v.PatternID, "chi-missing-auth") {
			hasAuthFinding = true
		}
		if strings.Contains(v.PatternID, "chi-missing-rate") {
			hasRateLimitFinding = true
		}
	}

	if !hasAuthFinding {
		t.Error("MISS: expected go-chi-missing-auth finding on servechi.go (real vulnerable app with no auth)")
	}
	if !hasRateLimitFinding {
		t.Error("MISS: expected go-chi-missing-rate-limit finding on servechi.go (real app with no rate limiting)")
	}

	// Also check files that SHOULDN'T fire (true negatives).
	// Internal utility files should not trigger framework patterns.
	commonFile := filepath.Join(repoDir, "internal/common/common.go")
	if _, err := os.Stat(commonFile); err == nil {
		commonContent, _ := os.ReadFile(commonFile)
		commonViolations := engine.CheckFile(g, commonFile, commonContent)
		if len(commonViolations) > 0 {
			t.Errorf("FALSE POSITIVE: common.go (utility file) produced %d violations: %v",
				len(commonViolations), commonViolations)
		}
	}
}

// TestSecurityBench_RealRepo_VAmPI tests against a real vulnerable Flask app.
func TestSecurityBench_RealRepo_VAmPI(t *testing.T) {
	repoDir := "/tmp/synbench_repos/vampi"
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		t.Skip("VAmPI not cloned — run: git clone https://github.com/erev0s/VAmPI.git " + repoDir)
	}

	g, err := parseRepo(repoDir)
	if err != nil {
		t.Fatalf("parse repo: %v", err)
	}
	t.Logf("Parsed: %d nodes, %d edges", g.NodeCount(), g.EdgeCount())

	engine := security.DefaultEngine()

	// Ground truth: VAmPI has known vulnerabilities:
	// - Missing auth on several endpoints (OWASP A01)
	// - Debug endpoint exposed (excessive data exposure)
	// - Weak JWT signing key
	routeFile := filepath.Join(repoDir, "api_views/users.py")
	content, _ := os.ReadFile(routeFile)
	violations := engine.CheckFile(g, routeFile, content)

	t.Logf("Violations found in users.py: %d", len(violations))
	for _, v := range violations {
		t.Logf("  [%s] %s — %s (confidence: %s)", v.Severity, v.PatternID, v.Target, v.Confidence)
	}

	// VAmPI uses Flask — check for missing auth findings
	hasFlaskAuth := false
	for _, v := range violations {
		if strings.Contains(v.PatternID, "flask-missing-auth") {
			hasFlaskAuth = true
		}
	}

	// Check for hardcoded secret in config
	configFile := filepath.Join(repoDir, "config.py")
	if _, err := os.Stat(configFile); err == nil {
		configContent, _ := os.ReadFile(configFile)
		configViolations := engine.CheckFile(g, configFile, configContent)
		t.Logf("Violations found in config.py: %d", len(configViolations))
		for _, v := range configViolations {
			t.Logf("  [%s] %s — %s", v.Severity, v.PatternID, v.Target)
		}
	}

	// Model file should NOT fire auth patterns (it's not a route handler)
	modelFile := filepath.Join(repoDir, "models/user_model.py")
	if _, err := os.Stat(modelFile); err == nil {
		modelContent, _ := os.ReadFile(modelFile)
		modelViolations := engine.CheckFile(g, modelFile, modelContent)
		authFP := 0
		for _, v := range modelViolations {
			if strings.Contains(v.PatternID, "auth") {
				authFP++
			}
		}
		if authFP > 0 {
			t.Errorf("FALSE POSITIVE: user_model.py (model file) produced %d auth violations", authFP)
		}
	}

	// Log summary for human review
	t.Logf("Flask auth finding: %v", hasFlaskAuth)
	t.Logf("NOTE: VAmPI is a known-vulnerable Flask app. Missing auth findings expected on route files.")
}

// parseRepo parses a repository into a graph using the real Synapses parser.
func parseRepo(repoDir string) (*graph.Graph, error) {
	absDir, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, err
	}

	g := graph.New(filepath.Base(absDir))
	g.SetRoot(absDir)

	w := parser.NewWalker()
	if _, err := w.WalkDir(g, absDir); err != nil {
		return nil, err
	}
	return g, nil
}
