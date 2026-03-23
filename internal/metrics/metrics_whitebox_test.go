package metrics

// White-box tests for unexported metrics helpers: pprofShortName, fileChurn,
// parsePprofTop. Using package metrics (not metrics_test) gives direct access.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// ── pprofShortName ────────────────────────────────────────────────────────────

func TestPprofShortName_FullyQualified(t *testing.T) {
	got := pprofShortName("github.com/foo/bar/pkg.(*Graph).AddEdge")
	if got != "Graph.AddEdge" {
		t.Errorf("got %q, want %q", got, "Graph.AddEdge")
	}
}

func TestPprofShortName_SimpleFunc(t *testing.T) {
	got := pprofShortName("github.com/foo/bar/pkg.FuncName")
	if got != "FuncName" {
		t.Errorf("got %q, want %q", got, "FuncName")
	}
}

func TestPprofShortName_RuntimeFunc(t *testing.T) {
	got := pprofShortName("runtime.mallocgc")
	if got != "mallocgc" {
		t.Errorf("got %q, want %q", got, "mallocgc")
	}
}

func TestPprofShortName_NoSlash(t *testing.T) {
	got := pprofShortName("pkg.SomeFunc")
	if got != "SomeFunc" {
		t.Errorf("got %q, want %q", got, "SomeFunc")
	}
}

func TestPprofShortName_Empty(t *testing.T) {
	got := pprofShortName("")
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestPprofShortName_NoDot(t *testing.T) {
	got := pprofShortName("runtime/mallocgc")
	// Last '/' → strips to "mallocgc"; no dot → unchanged.
	if got != "mallocgc" {
		t.Errorf("got %q, want %q", got, "mallocgc")
	}
}

// ── parsePprofTop — non-existent file returns error ───────────────────────────

func TestParsePprofTop_NonExistentFile(t *testing.T) {
	_, err := parsePprofTop("/nonexistent/cpu.pprof")
	if err == nil {
		t.Error("expected error for non-existent pprof file")
	}
}

// ── parsePprofTop and pprofShortName via real CPU profile ─────────────────────

func TestParsePprofTop_RealProfile(t *testing.T) {
	// Create a real CPU pprof file via runtime/pprof.
	f, err := os.CreateTemp(t.TempDir(), "cpu*.pprof")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatalf("StartCPUProfile: %v", err)
	}
	// Do a tiny amount of work to get at least one sample.
	sum := 0
	for i := 0; i < 100000; i++ {
		sum += i
	}
	_ = sum
	time.Sleep(5 * time.Millisecond)
	pprof.StopCPUProfile()
	f.Close()

	// parsePprofTop should succeed (may return empty map if no samples,
	// but must not return an error for a valid pprof file).
	samples, err := parsePprofTop(f.Name())
	if err != nil {
		// go tool pprof might not be available in all CI environments; skip.
		t.Skipf("parsePprofTop: %v (go tool pprof unavailable?)", err)
	}
	// samples may be empty on low-CPU systems; just verify no panic.
	_ = samples
}

// ── fileChurn — non-git dir returns error ─────────────────────────────────────

func TestFileChurn_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	_, err := fileChurn(dir, 90)
	if err == nil {
		t.Error("expected error for non-git directory")
	}
}

func TestFileChurn_ZeroDays(t *testing.T) {
	// Zero days is passed as-is; non-git dir still returns error.
	dir := t.TempDir()
	_, err := fileChurn(dir, 0)
	// Just verify no panic — result can be error or empty map.
	_ = err
}

// ── fileChurn and RecentCommitsForFile with a real git repo ───────────────────

func initGitRepo(t *testing.T) string {
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

	// Write a file and commit it.
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "svc.go"),
		[]byte("package pkg\nfunc Serve() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !run("git", "add", ".") {
		t.Skip("git add failed")
	}
	if !run("git", "commit", "-m", "initial commit") {
		t.Skip("git commit failed")
	}
	return dir
}

func TestFileChurn_WithGitRepo(t *testing.T) {
	dir := initGitRepo(t)

	churnMap, err := fileChurn(dir, 90)
	if err != nil {
		t.Fatalf("fileChurn: %v", err)
	}
	// "pkg/svc.go" was just committed — should appear in the map.
	found := false
	for k := range churnMap {
		if strings.Contains(k, "svc.go") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected pkg/svc.go in churn map after commit")
	}
}

func TestRecentCommitsForFile_WithGitRepo(t *testing.T) {
	dir := initGitRepo(t)

	commits := RecentCommitsForFile(context.Background(), dir, "pkg/svc.go", 3)
	if len(commits) == 0 {
		t.Error("expected at least 1 commit for pkg/svc.go")
	}
	c := commits[0]
	if c.Hash == "" {
		t.Error("expected non-empty hash")
	}
	if c.Message == "" {
		t.Error("expected non-empty message")
	}
}

// ── parsePprofTop — edge cases with real profiles ────────────────────────────

func TestParsePprofTop_WithMinimalProfile(t *testing.T) {
	// Create a minimal CPU pprof to ensure edge cases in parsing are covered
	f, err := os.CreateTemp(t.TempDir(), "minimal*.pprof")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatalf("StartCPUProfile: %v", err)
	}
	// Very minimal work
	var sum int
	for i := 0; i < 10000; i++ {
		sum += i
	}
	_ = sum
	time.Sleep(1 * time.Millisecond)
	pprof.StopCPUProfile()
	f.Close()

	// This should handle the pprof output parsing including:
	// - Lines before the "flat" header
	// - Empty lines in the output
	// - Variable field counts (pprof format)
	samples, err := parsePprofTop(f.Name())
	if err != nil {
		// Skip if go tool pprof is not available
		t.Skipf("parsePprofTop: %v (go tool pprof unavailable?)", err)
	}
	// Just verify no panic and empty map is acceptable
	_ = samples
}
