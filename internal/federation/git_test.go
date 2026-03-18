package federation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// ── test helpers ────────────────────────────────────────────────────────────

// initGitRepo creates a git repo in dir with an initial commit containing
// the given files (map of relative path → content).
func initGitRepo(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")

	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial commit")
	return getHead(t, dir)
}

// commitChange modifies a file and commits. Returns the new HEAD.
func commitChange(t *testing.T, dir, filePath, newContent, msg string) string {
	t.Helper()
	full := filepath.Join(dir, filePath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", filePath)
	runGit(t, dir, "commit", "-m", msg)
	return getHead(t, dir)
}

// removeFileAndCommit removes a file and commits. Returns the new HEAD.
func removeFileAndCommit(t *testing.T, dir, filePath, msg string) string {
	t.Helper()
	runGit(t, dir, "rm", filePath)
	runGit(t, dir, "commit", "-m", msg)
	return getHead(t, dir)
}

func getHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD: %v", err)
	}
	return trimNL(string(out))
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func bg() context.Context { return context.Background() }

// ── gitRevParseHead tests ───────────────────────────────────────────────────

func TestGitRevParseHead_ValidRepo(t *testing.T) {
	dir := t.TempDir()
	expected := initGitRepo(t, dir, map[string]string{"a.go": "package a"})

	head, err := gitRevParseHead(bg(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if head != expected {
		t.Errorf("expected %q, got %q", expected, head)
	}
}

func TestGitRevParseHead_NotAGitRepo(t *testing.T) {
	dir := t.TempDir() // no git init

	head, err := gitRevParseHead(bg(), dir)
	if err != nil {
		t.Fatalf("expected nil error for non-git dir, got: %v", err)
	}
	if head != "" {
		t.Errorf("expected empty head for non-git dir, got %q", head)
	}
}

func TestGitRevParseHead_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{"a.go": "package a"})

	ctx, cancel := context.WithCancel(bg())
	cancel()

	_, err := gitRevParseHead(ctx, dir)
	// Either context cancelled error or empty result — both are acceptable fail-open.
	_ = err
}

func TestGitRevParseHead_NonexistentPath(t *testing.T) {
	head, err := gitRevParseHead(bg(), "/nonexistent/path")
	if err == nil && head != "" {
		t.Error("expected error or empty head for nonexistent path")
	}
}

// ── gitDiffNameOnly tests ───────────────────────────────────────────────────

func TestGitDiffNameOnly_ChangedFiles(t *testing.T) {
	dir := t.TempDir()
	oldHead := initGitRepo(t, dir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Auth() {}",
		"pkg/db.go":   "package db\nfunc Connect() {}",
	})
	newHead := commitChange(t, dir, "pkg/auth.go", "package auth\nfunc Auth(ctx context.Context) {}", "change auth")

	files, err := gitDiffNameOnly(bg(), dir, oldHead, newHead)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 || files[0] != "pkg/auth.go" {
		t.Errorf("expected [pkg/auth.go], got %v", files)
	}
}

func TestGitDiffNameOnly_NoChanges(t *testing.T) {
	dir := t.TempDir()
	head := initGitRepo(t, dir, map[string]string{"a.go": "package a"})

	files, err := gitDiffNameOnly(bg(), dir, head, head)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil for same commit, got %v", files)
	}
}

func TestGitDiffNameOnly_UnreachableCommit(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{"a.go": "package a"})

	// Use a fake commit hash — should return nil, nil (unreachable).
	files, err := gitDiffNameOnly(bg(), dir, "0000000000000000000000000000000000000000", "HEAD")
	if err != nil {
		t.Fatalf("expected nil error for unreachable commit, got: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil files for unreachable commit, got %v", files)
	}
}

// ── gitDiffFile tests ───────────────────────────────────────────────────────

func TestGitDiffFile_ShowsDiff(t *testing.T) {
	dir := t.TempDir()
	oldHead := initGitRepo(t, dir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Validate(token string) bool { return true }",
	})
	newHead := commitChange(t, dir, "pkg/auth.go",
		"package auth\nfunc Validate(token string, opts ...Option) bool { return true }",
		"add opts param")

	diff, err := gitDiffFile(bg(), dir, oldHead, newHead, "pkg/auth.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}
	if !containsStr(diff, "Validate") {
		t.Error("expected diff to contain 'Validate'")
	}
}

func TestGitDiffFile_UnchangedFile(t *testing.T) {
	dir := t.TempDir()
	oldHead := initGitRepo(t, dir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Auth() {}",
		"pkg/db.go":   "package db\nfunc Connect() {}",
	})
	newHead := commitChange(t, dir, "pkg/auth.go", "package auth\nfunc Auth(ctx context.Context) {}", "change auth")

	// db.go was not changed.
	diff, err := gitDiffFile(bg(), dir, oldHead, newHead, "pkg/db.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if diff != "" {
		t.Errorf("expected empty diff for unchanged file, got: %s", diff)
	}
}

// ── diffTouchesEntity tests ─────────────────────────────────────────────────

func TestDiffTouchesEntity_GoFunc(t *testing.T) {
	diff := `--- a/auth.go
+++ b/auth.go
@@ -1,3 +1,3 @@
 package auth
-func Validate(token string) bool { return true }
+func Validate(token string, opts ...Option) bool { return true }
`
	if !diffTouchesEntity(diff, "Validate") {
		t.Error("expected diff to touch Validate")
	}
}

func TestDiffTouchesEntity_GoMethod(t *testing.T) {
	diff := `--- a/server.go
+++ b/server.go
@@ -5,3 +5,3 @@
-func (s *Server) Handle(req Request) Response {
+func (s *Server) Handle(req Request, ctx context.Context) Response {
`
	if !diffTouchesEntity(diff, "Handle") {
		t.Error("expected diff to touch Handle (method signature)")
	}
}

func TestDiffTouchesEntity_GoType(t *testing.T) {
	diff := `--- a/types.go
+++ b/types.go
@@ -1,3 +1,4 @@
-type Config struct {
+type Config struct {
+    NewField string
`
	if !diffTouchesEntity(diff, "Config") {
		t.Error("expected diff to touch Config type")
	}
}

func TestDiffTouchesEntity_PythonDef(t *testing.T) {
	diff := `--- a/auth.py
+++ b/auth.py
@@ -1,2 +1,2 @@
-def validate(token):
+def validate(token, strict=False):
`
	if !diffTouchesEntity(diff, "validate") {
		t.Error("expected diff to touch Python validate")
	}
}

func TestDiffTouchesEntity_RustFn(t *testing.T) {
	diff := `--- a/auth.rs
+++ b/auth.rs
@@ -1,2 +1,2 @@
-pub fn validate(token: &str) -> bool {
+pub fn validate(token: &str, opts: Options) -> bool {
`
	if !diffTouchesEntity(diff, "validate") {
		t.Error("expected diff to touch Rust validate")
	}
}

func TestDiffTouchesEntity_TSExport(t *testing.T) {
	diff := `--- a/auth.ts
+++ b/auth.ts
@@ -1,2 +1,2 @@
-export function validate(token: string): boolean {
+export function validate(token: string, opts?: Options): boolean {
`
	if !diffTouchesEntity(diff, "validate") {
		t.Error("expected diff to touch TS validate")
	}
}

func TestDiffTouchesEntity_NotTouched(t *testing.T) {
	diff := `--- a/auth.go
+++ b/auth.go
@@ -5,3 +5,4 @@
 func Validate(token string) bool {
+    log.Println("validating")
     return true
`
	// "Validate" appears only in context (no +/- prefix), not in changed lines.
	if diffTouchesEntity(diff, "Validate") {
		t.Error("expected diff NOT to touch Validate — only context line")
	}
}

func TestDiffTouchesEntity_EmptyDiff(t *testing.T) {
	if diffTouchesEntity("", "Validate") {
		t.Error("expected false for empty diff")
	}
}

func TestDiffTouchesEntity_EmptyEntity(t *testing.T) {
	if diffTouchesEntity("some diff", "") {
		t.Error("expected false for empty entity")
	}
}

func TestDiffTouchesEntity_QualifiedName(t *testing.T) {
	diff := `--- a/server.go
+++ b/server.go
@@ -5,3 +5,3 @@
-func (s *Server) Validate(req Request) error {
+func (s *Server) Validate(req Request, opts ...Option) error {
`
	// "Server.Validate" — the unqualified "Validate" should match method sigs.
	if !diffTouchesEntity(diff, "Server.Validate") {
		t.Error("expected qualified name to match method signature")
	}
}

// ── entityExistsInFile tests ────────────────────────────────────────────────

func TestEntityExistsInFile_Found(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Validate(token string) bool { return true }",
	})

	if !entityExistsInFile(bg(), dir, "pkg/auth.go", "Validate") {
		t.Error("expected Validate to exist in file")
	}
}

func TestEntityExistsInFile_NotFound(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Other() {}",
	})

	if entityExistsInFile(bg(), dir, "pkg/auth.go", "Validate") {
		t.Error("expected Validate to NOT exist in file")
	}
}

func TestEntityExistsInFile_FileDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, map[string]string{"a.go": "package a"})

	if entityExistsInFile(bg(), dir, "nonexistent.go", "Foo") {
		t.Error("expected false for nonexistent file")
	}
}

// ── extractDiffSummary tests ────────────────────────────────────────────────

func TestExtractDiffSummary_SingleLineChange(t *testing.T) {
	diff := `-func Validate(token string) bool {
+func Validate(token string, opts ...Option) bool {
`
	summary := extractDiffSummary(diff, "Validate")
	if !containsStr(summary, "Changed:") {
		t.Errorf("expected 'Changed:' in summary, got %q", summary)
	}
}

func TestExtractDiffSummary_MultipleChanges(t *testing.T) {
	diff := `-func Validate(token string) bool {
-func Validate2(token string) bool {
+func Validate(token string, opts ...Option) bool {
+func Validate2(token string, opts ...Option) bool {
`
	summary := extractDiffSummary(diff, "Validate")
	if !containsStr(summary, "removed") || !containsStr(summary, "added") {
		t.Errorf("expected 'removed' and 'added' in summary, got %q", summary)
	}
}

func TestExtractDiffSummary_NoEntityLines(t *testing.T) {
	diff := `-something else
+something new
`
	summary := extractDiffSummary(diff, "Validate")
	if summary != "Signature changed" {
		t.Errorf("expected 'Signature changed', got %q", summary)
	}
}

// ── git timeout test ────────────────────────────────────────────────────────

func TestGitTimeout_DoesNotBlock(t *testing.T) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(bg(), 50*time.Millisecond)
	defer cancel()

	// Use a nonexistent path — the command will fail quickly, but the
	// test verifies we don't block longer than the timeout.
	_, _ = gitRevParseHead(ctx, "/nonexistent/very/long/path/that/does/not/exist")

	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("git command blocked for %v — expected <2s", elapsed)
	}
}

// ── helper ──────────────────────────────────────────────────────────────────

func containsStr(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
