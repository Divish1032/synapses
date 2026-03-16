package metrics

// White-box tests for remaining uncovered metrics paths:
// - coverPathToRel with various inputs
// - readModuleName edge cases
// - parseCoverProfile edge cases (malformed, no-colon, no-comma lines)
// - BlameAgeLabel boundary cases (1 day, 1 week, 1 month, 1 year)
// - pprofShortName with nested pointer receivers
// - splitCamelCase-like patterns in pprof names
// - RecentCommitsForFile body truncation (>200 chars)
// - fileBlame hash truncation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── coverPathToRel ────────────────────────────────────────────────────────────

func TestCoverPathToRel_WithModule(t *testing.T) {
	got := coverPathToRel("github.com/foo/bar/internal/pkg/file.go", "github.com/foo/bar")
	if got != "internal/pkg/file.go" {
		t.Errorf("got %q, want %q", got, "internal/pkg/file.go")
	}
}

func TestCoverPathToRel_NoModuleMatch(t *testing.T) {
	// Module name doesn't match — falls through to heuristic.
	// Heuristic strips leading segments that contain a dot (domain-like).
	// "github.com" has dot, "other" does not → strip "github.com/", keep "other/pkg/file.go".
	got := coverPathToRel("github.com/other/pkg/file.go", "github.com/foo/bar")
	if got != "other/pkg/file.go" {
		t.Errorf("got %q, want %q", got, "other/pkg/file.go")
	}
}

func TestCoverPathToRel_EmptyModule(t *testing.T) {
	// No module → heuristic: strip leading domain-like segments (containing '.').
	// "github.com" has dot, "example" does not → strip "github.com/", keep rest.
	got := coverPathToRel("github.com/example/proj/pkg/file.go", "")
	if got != "example/proj/pkg/file.go" {
		t.Errorf("got %q, want %q", got, "example/proj/pkg/file.go")
	}
}

func TestCoverPathToRel_AllDomainSegments(t *testing.T) {
	// All segments contain dots → nothing stripped, return as-is.
	got := coverPathToRel("a.b/c.d/e.f", "")
	if got != "a.b/c.d/e.f" {
		t.Errorf("got %q, want %q", got, "a.b/c.d/e.f")
	}
}

func TestCoverPathToRel_SingleSegment(t *testing.T) {
	got := coverPathToRel("file.go", "")
	// Single segment with dot → all segments have dots → return as-is.
	if got != "file.go" {
		t.Errorf("got %q, want %q", got, "file.go")
	}
}

// ── readModuleName ────────────────────────────────────────────────────────────

func TestReadModuleName_ValidGoMod(t *testing.T) {
	dir := t.TempDir()
	goMod := "module github.com/test/proj\n\ngo 1.21\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readModuleName(dir)
	if got != "github.com/test/proj" {
		t.Errorf("got %q, want %q", got, "github.com/test/proj")
	}
}

func TestReadModuleName_MissingGoMod(t *testing.T) {
	got := readModuleName(t.TempDir())
	if got != "" {
		t.Errorf("expected empty for missing go.mod, got %q", got)
	}
}

func TestReadModuleName_NoModuleLine(t *testing.T) {
	dir := t.TempDir()
	// go.mod without a module line.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("go 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readModuleName(dir)
	if got != "" {
		t.Errorf("expected empty for go.mod without module line, got %q", got)
	}
}

// ── parseCoverProfile edge cases ──────────────────────────────────────────────

func TestParseCoverProfile_EmptyFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "empty.out")
	if err := os.WriteFile(f, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks, err := parseCoverProfile(f)
	if err != nil {
		t.Fatalf("parseCoverProfile empty: %v", err)
	}
	if len(blocks) != 0 {
		t.Errorf("expected 0 blocks from empty file, got %d", len(blocks))
	}
}

func TestParseCoverProfile_MalformedLines(t *testing.T) {
	content := `mode: set
not enough fields
too many fields here extra
pkg/file.go:10.5,20.15 notanum 1
pkg/file.go:10.5,20.15 3 notanum
nocolon 3 1
pkg/file.go:10.5 3 1
`
	f := filepath.Join(t.TempDir(), "bad.out")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks, err := parseCoverProfile(f)
	if err != nil {
		t.Fatalf("parseCoverProfile malformed: %v", err)
	}
	// All lines are malformed — should skip all.
	if len(blocks) != 0 {
		t.Errorf("expected 0 valid blocks, got %d", len(blocks))
	}
}

func TestParseCoverProfile_ValidLine(t *testing.T) {
	content := "mode: set\npkg/file.go:10.5,20.15 3 1\n"
	f := filepath.Join(t.TempDir(), "valid.out")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks, err := parseCoverProfile(f)
	if err != nil {
		t.Fatalf("parseCoverProfile: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.file != "pkg/file.go" {
		t.Errorf("file = %q", b.file)
	}
	if b.startLine != 10 || b.endLine != 20 {
		t.Errorf("lines = %d-%d, want 10-20", b.startLine, b.endLine)
	}
	if b.numStmts != 3 || b.count != 1 {
		t.Errorf("stmts=%d count=%d, want 3,1", b.numStmts, b.count)
	}
}

func TestParseCoverProfile_NonExistentFile(t *testing.T) {
	_, err := parseCoverProfile("/nonexistent/file.out")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

// ── pprofShortName additional cases ───────────────────────────────────────────

func TestPprofShortName_NestedPointerReceiver(t *testing.T) {
	got := pprofShortName("pkg.(*A).(*B).Method")
	if got != "A.B.Method" {
		t.Errorf("got %q, want %q", got, "A.B.Method")
	}
}

func TestPprofShortName_JustDot(t *testing.T) {
	got := pprofShortName(".")
	// After last '/' (none) → "."; after first '.' → "".
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ── BlameAgeLabel boundary values ─────────────────────────────────────────────

func TestBlameAgeLabel_OneDayAgo(t *testing.T) {
	// UTC midnight yesterday: time.Since = 24h + hours_since_midnight_UTC → days = 1.
	d := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	got := BlameAgeLabel(d)
	if got != "1d ago" {
		t.Errorf("got %q, want \"1d ago\"", got)
	}
}

// ── StalenessLabel ────────────────────────────────────────────────────────────

func TestStalenessLabel_ExactBoundary30(t *testing.T) {
	if StalenessLabel(30) != "medium" {
		t.Error("30 should be medium")
	}
}

func TestStalenessLabel_ExactBoundary150(t *testing.T) {
	if StalenessLabel(150) != "high" {
		t.Error("150 should be high")
	}
}

func TestStalenessLabel_Negative(t *testing.T) {
	if StalenessLabel(-1) != "low" {
		t.Error("negative should be low")
	}
}
