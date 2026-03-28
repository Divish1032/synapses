package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── pathWithinRoot unit tests ─────────────────────────────────────────────────

func TestPathWithinRoot(t *testing.T) {
	cases := []struct {
		name string
		root string
		path string
		want bool
	}{
		// Happy paths: file inside root.
		{"file inside root", "/proj", "/proj/internal/auth.go", true},
		{"file at root boundary", "/proj", "/proj", true},
		{"nested deep", "/proj", "/proj/a/b/c/d.go", true},

		// Traversal attacks: must be blocked.
		{"absolute outside root", "/proj", "/etc/passwd", false},
		{"sibling dir", "/proj", "/projother/file.go", false},
		{"parent of root", "/proj/sub", "/proj/other.go", false},

		// Prefix collision guard: "/proj" must NOT match "/projfoo".
		{"prefix collision", "/proj", "/projfoo/file.go", false},

		// Empty root: fail-closed — no boundary known → block all FS access.
		{"empty root blocks absolute", "", "/etc/passwd", false},
		{"empty root blocks relative", "", "relative/file.go", false},
		{"empty root blocks root itself", "", "/", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pathWithinRoot(tc.root, tc.path)
			if got != tc.want {
				t.Errorf("pathWithinRoot(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
			}
		})
	}
}

// ── Attack vector tests ───────────────────────────────────────────────────────

// newServerWithRoot creates a server whose graph root is set to a temp directory.
// Files placed in that directory are within the project root; files placed
// elsewhere are outside it — any attempt to access them via path traversal
// must be blocked.
func newServerWithRoot(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()

	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	g.SetRoot(root)

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return New(g, cfg, st), root
}

// TestValidatePlanPathTraversal_FreshnessCheck proves the freshness-stat attack
// is closed in handleValidatePlan.
//
// Attack: agent passes a recently-modified file via traversal
//
//	(e.g. "../../outside/secret.txt"). Before fix: os.Stat is called and
//	"graph_freshness" appears in the response, leaking that the external file
//	was modified < 10s ago. After fix: pathWithinRoot rejects it → "graph_freshness"
//	is absent entirely.
func TestValidatePlanPathTraversal_FreshnessCheck(t *testing.T) {
	s, root := newServerWithRoot(t)

	// Create a file OUTSIDE the root, just written (< 10s ago → would trigger
	// a freshness warning if os.Stat were called on it).
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("sensitive content"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	// Compute the relative traversal path from root into outsideFile.
	rel, err := filepath.Rel(root, outsideFile)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	changesJSON, _ := json.Marshal([]map[string]string{{"file": rel}})
	res, callErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": string(changesJSON),
	}))
	m := mustResult(t, res, callErr)

	// "graph_freshness" must be absent: the traversal file was never stat'd.
	// If it were present, it would prove the boundary check did not fire.
	if _, ok := m["graph_freshness"]; ok {
		t.Errorf("graph_freshness appeared in response — os.Stat was called on external file: %v", m["graph_freshness"])
	}
}

// TestValidatePlanPathTraversal_LogicChecks proves the os.ReadFile attack is
// closed in handleValidatePlan's logic-check path.
//
// Attack: agent passes a .go file via traversal. Before fix: os.ReadFile reads
//
//	the file and parser.RunLogicChecks produces warnings based on its content.
//	After fix: pathWithinRoot rejects it → the file is never read → no
//	logic_warnings appear.
//
// The outside file contains a known tilde-path pattern ("~/data") which
// RunLogicChecks detects in Go source. If the guard fires, no warnings appear.
// If the guard is missing, warnings appear — proving the file was read.
func TestValidatePlanPathTraversal_LogicChecks(t *testing.T) {
	s, root := newServerWithRoot(t)

	// Create a .go file OUTSIDE the root with a Go logic warning pattern.
	// RunLogicChecks detects tilde paths in os.Open / os.ReadFile calls.
	outsideDir := t.TempDir()
	outsideGo := filepath.Join(outsideDir, "malicious.go")
	goSrc := []byte(`package main

import "os"

func bad() {
	os.ReadFile("~/sensitive/data")
}
`)
	if err := os.WriteFile(outsideGo, goSrc, 0o644); err != nil {
		t.Fatalf("write outside go file: %v", err)
	}

	// Compute relative traversal path. filepath.Rel produces ".." segments.
	rel, err := filepath.Rel(root, outsideGo)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	changesJSON, _ := json.Marshal([]map[string]string{{"file": rel}})
	res, callErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": string(changesJSON),
	}))
	m := mustResult(t, res, callErr)

	// "logic_warnings" must be absent — the outside .go file was never read.
	// If the guard were missing, RunLogicChecks would detect the tilde path and
	// "logic_warnings" would be present with at least one entry.
	if lw, ok := m["logic_warnings"]; ok {
		t.Errorf("logic_warnings appeared in response — os.ReadFile was called on external file: %v", lw)
	}
}

// TestVerifyImplementationPathTraversal_FreshnessCheck proves the freshness-stat
// attack is closed in handleVerifyImplementation.
//
// Attack: agent passes a recently-modified external file via traversal in
//
//	files_written. Before fix: os.Stat fires → FreshnessWarning leaks metadata.
//	After fix: pathWithinRoot rejects it → FreshnessWarning stays empty.
func TestVerifyImplementationPathTraversal_FreshnessCheck(t *testing.T) {
	s, root := newServerWithRoot(t)

	// File outside the root, just written (< 10s old → freshness trigger).
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("sensitive"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	rel, err := filepath.Rel(root, outsideFile)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	filesJSON, _ := json.Marshal([]string{rel})
	res, callErr := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	// Verify no file report has a non-empty freshness_warning for the traversal path.
	files, _ := m["files"].([]any)
	for _, f := range files {
		fm, _ := f.(map[string]any)
		if fw, ok := fm["freshness_warning"]; ok && fw != nil && fw != "" {
			t.Errorf("freshness_warning appeared for external file — os.Stat was called: %v", fw)
		}
	}
}

// TestValidatePlanPathTraversal_AbsolutePath proves that an absolute path
// pointing directly to a sensitive file is also rejected (not just relative
// traversal paths). This closes the bypass where an agent omits ".." and
// simply provides "/etc/passwd" directly.
func TestValidatePlanPathTraversal_AbsolutePath(t *testing.T) {
	s, _ := newServerWithRoot(t)

	// /etc/passwd exists and has a known modification time. If os.Stat fires
	// and the daemon has been running < 10s (unlikely but possible in tests),
	// a freshness warning could appear. We check both the absence of
	// graph_freshness AND logic_warnings.
	changesJSON := `[{"file":"/etc/passwd"}]`
	res, callErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": changesJSON,
	}))
	m := mustResult(t, res, callErr)

	if _, ok := m["graph_freshness"]; ok {
		t.Errorf("graph_freshness appeared for absolute external path /etc/passwd")
	}
	if _, ok := m["logic_warnings"]; ok {
		t.Errorf("logic_warnings appeared for absolute external path /etc/passwd")
	}
}

// TestValidatePlanPathTraversal_EmptyRoot proves that when the graph has no
// root set, all filesystem access is blocked (fail-closed behavior).
//
// This covers the scenario where the daemon starts without a project path —
// an empty root must not silently allow arbitrary file access.
func TestValidatePlanPathTraversal_EmptyRoot(t *testing.T) {
	// Server with NO root set — root is "".
	s := newTestServer(t)

	// Create a file INSIDE what would be a project dir — but since root is
	// empty, even "safe-looking" paths must not trigger FS access.
	tmpFile := filepath.Join(t.TempDir(), "recent.go")
	if err := os.WriteFile(tmpFile, []byte(`package main

import "os"

func bad() { os.ReadFile("~/data") }
`), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}

	// Pass the absolute path directly (no traversal needed — root is empty).
	changesJSON, _ := json.Marshal([]map[string]string{{"file": tmpFile}})
	res, callErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": string(changesJSON),
	}))
	m := mustResult(t, res, callErr)

	// With fail-closed behavior, neither freshness nor logic warnings appear.
	if _, ok := m["graph_freshness"]; ok {
		t.Errorf("graph_freshness appeared with empty root — FS access was not blocked")
	}
	if _, ok := m["logic_warnings"]; ok {
		t.Errorf("logic_warnings appeared with empty root — os.ReadFile was not blocked")
	}
}
