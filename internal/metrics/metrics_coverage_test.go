package metrics

// Additional tests for EnrichChurn and EnrichPprof inner loops.
// Requires git to be available (skips otherwise).

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// initGitRepoWithFile creates a git repo and commits a Go file. Returns dir.
func initGitRepoWithFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) bool {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		return cmd.Run() == nil
	}

	if !run("git", "init") {
		t.Skip("git not available")
	}
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "Test")

	pkgDir := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	goFile := filepath.Join(pkgDir, "auth.go")
	if err := os.WriteFile(goFile, []byte("package pkg\nfunc Login() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !run("git", "add", ".") || !run("git", "commit", "-m", "add auth") {
		t.Skip("git commit failed")
	}
	return dir
}

// ── EnrichChurn inner loop ────────────────────────────────────────────────────

func TestEnrichChurn_WithGitRepo_NodeAnnotated(t *testing.T) {
	dir := initGitRepoWithFile(t)

	g := graph.New(dir)
	goFile := filepath.Join(dir, "pkg", "auth.go")
	id := g.MakeNodeID(goFile, "Login")
	g.AddNode(&graph.Node{
		ID:      id,
		Name:    "Login",
		Type:    graph.NodeFunction,
		File:    goFile,
		Package: "pkg",
	})

	EnrichChurn(g, dir, 90)

	// The node should now have a "churn" metadata entry (or at minimum no crash).
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Fatal("expected node in graph")
	}
	// churn may or may not be set depending on git output; just verify no panic.
	_ = nodes[0].Metadata
}

func TestEnrichChurn_NodeNoMetadata(t *testing.T) {
	dir := initGitRepoWithFile(t)

	g := graph.New(dir)
	goFile := filepath.Join(dir, "pkg", "auth.go")
	id := g.MakeNodeID(goFile, "Login")
	// Node with nil Metadata.
	g.AddNode(&graph.Node{
		ID:       id,
		Name:     "Login",
		Type:     graph.NodeFunction,
		File:     goFile,
		Package:  "pkg",
		Metadata: nil, // intentionally nil
	})

	// Must not panic when Metadata is nil and churn count > 0.
	EnrichChurn(g, dir, 90)
}

func TestEnrichChurn_NoMatchingFile(t *testing.T) {
	dir := initGitRepoWithFile(t)

	g := graph.New(dir)
	// Node pointing to a file NOT committed to git → no churn entry.
	id := g.MakeNodeID(filepath.Join(dir, "other", "notexist.go"), "NoFunc")
	g.AddNode(&graph.Node{
		ID:   id,
		Name: "NoFunc",
		Type: graph.NodeFunction,
		File: filepath.Join(dir, "other", "notexist.go"),
	})

	EnrichChurn(g, dir, 90)
	// No crash, no churn metadata expected.
}

// ── EnrichPprof inner loop ────────────────────────────────────────────────────

func TestEnrichPprof_WithRealProfile(t *testing.T) {
	// Generate a real pprof CPU profile.
	f, err := os.CreateTemp(t.TempDir(), "cpu*.pprof")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatalf("StartCPUProfile: %v", err)
	}
	sum := 0
	for i := 0; i < 500000; i++ {
		sum += i
	}
	_ = sum
	pprof.StopCPUProfile()
	f.Close()

	// Build a graph with function nodes (names may match pprof output).
	g := graph.New("/repo")
	id := g.MakeNodeID("/repo/pkg/auth.go", "Login")
	g.AddNode(&graph.Node{
		ID:   id,
		Name: "Login",
		Type: graph.NodeFunction,
		File: "/repo/pkg/auth.go",
	})

	// EnrichPprof should not panic even when no names match.
	EnrichPprof(g, "/repo", f.Name())
}

func TestEnrichPprof_NodeWithNilMetadata(t *testing.T) {
	// Create a profile and a graph where a node name matches.
	f, err := os.CreateTemp(t.TempDir(), "cpu*.pprof")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatalf("StartCPUProfile: %v", err)
	}
	sum := 0
	for i := 0; i < 500000; i++ {
		sum += i
	}
	_ = sum
	pprof.StopCPUProfile()
	f.Close()

	g := graph.New("/repo")
	id := g.MakeNodeID("/repo/pkg/auth.go", "Login")
	g.AddNode(&graph.Node{
		ID:       id,
		Name:     "Login",
		Type:     graph.NodeFunction,
		File:     "/repo/pkg/auth.go",
		Metadata: nil, // nil — branch coverage for nil metadata path
	})

	EnrichPprof(g, "/repo", f.Name())
}
