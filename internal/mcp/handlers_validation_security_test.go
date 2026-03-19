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
		// Happy paths.
		{"file inside root", "/proj", "/proj/internal/auth.go", true},
		{"file at root boundary", "/proj", "/proj", true},
		{"nested deep", "/proj", "/proj/a/b/c/d.go", true},

		// Traversal attacks.
		{"one level up", "/proj", "/etc/passwd", false},
		{"traversal via join", "/proj", "/etc", false},
		{"sibling dir", "/proj", "/projother/file.go", false},
		{"parent of root", "/proj/sub", "/proj/other.go", false},

		// Empty root: always allowed (no boundary to enforce).
		{"empty root allows anything", "", "/etc/passwd", true},
		{"empty root allows local", "", "relative/file.go", true},

		// Prefix collision guard: "/proj" must not match "/projfoo".
		{"prefix collision", "/proj", "/projfoo/file.go", false},
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

// TestValidatePlanPathTraversal_FreshnessCheck proves the path traversal attack
// is closed for the freshness check in handleValidatePlan.
//
// Attack: pass file="../../outside/secret.txt" in the changes array.
// Before fix: absFile resolves outside root → os.Stat is called → freshness
// warning leaks metadata about the external file.
// After fix: pathWithinRoot rejects it → no freshness warning in response.
func TestValidatePlanPathTraversal_FreshnessCheck(t *testing.T) {
	s, root := newServerWithRoot(t)

	// Create a file OUTSIDE the root — recently modified so it would trigger
	// a freshness warning if os.Stat were called on it.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("sensitive"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	// Craft a relative path that traverses from root to the outside file.
	// filepath.Rel gives us the relative path from root to outsideFile.
	rel, err := filepath.Rel(root, outsideFile)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	changesJSON, _ := json.Marshal([]map[string]string{{"file": rel}})
	res, err := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": string(changesJSON),
	}))
	m := mustResult(t, res, err)

	// There must be NO freshness_warnings mentioning the outside file.
	if fw, ok := m["freshness_warnings"]; ok {
		warnings, _ := fw.([]any)
		for _, w := range warnings {
			ws, _ := w.(string)
			if filepath.Base(outsideFile) != "" && pathContains(ws, filepath.Base(outsideFile)) {
				t.Errorf("freshness warning leaked external file metadata: %q", ws)
			}
		}
	}
}

// TestValidatePlanPathTraversal_LogicChecks proves the path traversal attack
// is closed for the logic-check (os.ReadFile) path in handleValidatePlan.
//
// Attack: pass file="../../../../etc/passwd" → before fix, os.ReadFile reads it.
// After fix: pathWithinRoot rejects it → file is never opened.
func TestValidatePlanPathTraversal_LogicChecks(t *testing.T) {
	s, _ := newServerWithRoot(t)

	// A classic traversal path targeting /etc/passwd.
	changesJSON := `[{"file":"../../../../etc/passwd"}]`
	res, err := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": changesJSON,
	}))
	// The handler must succeed (not panic) but return empty logic_warnings
	// because the file was never read.
	m := mustResult(t, res, err)

	if lw, ok := m["logic_warnings"]; ok {
		warnings, _ := lw.([]any)
		if len(warnings) > 0 {
			t.Errorf("expected no logic_warnings for traversal path, got %v", warnings)
		}
	}
}

// TestVerifyImplementationPathTraversal proves the path traversal attack is
// closed for the freshness check in handleVerifyImplementation.
func TestVerifyImplementationPathTraversal_FreshnessCheck(t *testing.T) {
	s, root := newServerWithRoot(t)

	// File outside the root, recently written.
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
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesJSON),
	}))
	m := mustResult(t, res, err)

	// Verify no freshness_warning for the traversal path appears in any file report.
	files, _ := m["files"].([]any)
	for _, f := range files {
		fm, _ := f.(map[string]any)
		if fw, ok := fm["freshness_warning"]; ok && fw != nil && fw != "" {
			t.Errorf("freshness_warning leaked for external file: %v", fw)
		}
	}
}

// TestValidatePlanPathTraversal_AbsolutePath proves that an absolute path
// outside the root is also rejected (not just relative traversal paths).
func TestValidatePlanPathTraversal_AbsolutePath(t *testing.T) {
	s, _ := newServerWithRoot(t)

	// Pass an absolute path directly to a sensitive location.
	changesJSON := `[{"file":"/etc/passwd"}]`
	res, err := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": changesJSON,
	}))
	m := mustResult(t, res, err)

	// No freshness_warnings for /etc/passwd.
	if fw, ok := m["freshness_warnings"]; ok {
		warnings, _ := fw.([]any)
		for _, w := range warnings {
			if pathContains(w.(string), "passwd") {
				t.Errorf("freshness warning leaked for absolute external path: %v", w)
			}
		}
	}
}

// pathContains reports whether s contains the given substring.
func pathContains(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
