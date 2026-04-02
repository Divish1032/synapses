package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectFormatterConventions_NoFiles(t *testing.T) {
	dir := t.TempDir()
	got := detectFormatterConventions(dir)
	if len(got) != 0 {
		t.Fatalf("expected no conventions for empty dir, got %v", got)
	}
}

func TestDetectFormatterConventions_EmptyRoot(t *testing.T) {
	got := detectFormatterConventions("")
	if got != nil {
		t.Fatalf("expected nil for empty root, got %v", got)
	}
}

func TestDetectFormatterConventions_Prettier(t *testing.T) {
	for _, filename := range []string{
		".prettierrc",
		".prettierrc.json",
		"prettier.config.js",
		".prettierrc.yml",
	} {
		t.Run(filename, func(t *testing.T) {
			dir := t.TempDir()
			touchFile(t, dir, filename)
			got := detectFormatterConventions(dir)
			if len(got) != 1 {
				t.Fatalf("expected 1 convention, got %d: %v", len(got), got)
			}
			if !strings.Contains(got[0], "Prettier") {
				t.Errorf("expected Prettier in convention, got: %q", got[0])
			}
			assertFormatterMsg(t, got[0])
		})
	}
}

func TestDetectFormatterConventions_Biome(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, dir, "biome.json")
	got := detectFormatterConventions(dir)
	if len(got) != 1 {
		t.Fatalf("expected 1 convention, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "Biome") {
		t.Errorf("expected Biome in convention, got: %q", got[0])
	}
}

func TestDetectFormatterConventions_Rustfmt(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, dir, "rustfmt.toml")
	got := detectFormatterConventions(dir)
	if len(got) != 1 {
		t.Fatalf("expected 1 convention, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "rustfmt") {
		t.Errorf("expected rustfmt in convention, got: %q", got[0])
	}
}

func TestDetectFormatterConventions_GolangCI(t *testing.T) {
	for _, filename := range []string{".golangci.yml", ".golangci.yaml"} {
		t.Run(filename, func(t *testing.T) {
			dir := t.TempDir()
			touchFile(t, dir, filename)
			got := detectFormatterConventions(dir)
			if len(got) != 1 {
				t.Fatalf("expected 1 convention, got %d: %v", len(got), got)
			}
			if !strings.Contains(got[0], "gofmt") {
				t.Errorf("expected gofmt in convention, got: %q", got[0])
			}
		})
	}
}

func TestDetectFormatterConventions_EditorConfig(t *testing.T) {
	dir := t.TempDir()
	touchFile(t, dir, ".editorconfig")
	got := detectFormatterConventions(dir)
	if len(got) != 1 {
		t.Fatalf("expected 1 convention, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "EditorConfig") {
		t.Errorf("expected EditorConfig in convention, got: %q", got[0])
	}
}

func TestDetectFormatterConventions_PyprojectBlack(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", "[tool.black]\nline-length = 88\n")
	got := detectFormatterConventions(dir)
	if len(got) != 1 {
		t.Fatalf("expected 1 convention, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "black") {
		t.Errorf("expected black in convention, got: %q", got[0])
	}
}

func TestDetectFormatterConventions_PyprojectRuff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", "[tool.ruff]\nline-length = 88\n[tool.ruff.format]\nquote-style = \"double\"\n")
	got := detectFormatterConventions(dir)
	if len(got) != 1 {
		t.Fatalf("expected 1 convention, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "ruff") {
		t.Errorf("expected ruff in convention, got: %q", got[0])
	}
}

func TestDetectFormatterConventions_PyprojectNoFormatter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pyproject.toml", "[build-system]\nrequires = [\"setuptools\"]\n")
	got := detectFormatterConventions(dir)
	if len(got) != 0 {
		t.Fatalf("expected no conventions for pyproject without formatter, got %v", got)
	}
}

func TestDetectFormatterConventions_MultipleFormatters(t *testing.T) {
	// A project with both Prettier (JS side) and EditorConfig.
	dir := t.TempDir()
	touchFile(t, dir, ".prettierrc")
	touchFile(t, dir, ".editorconfig")
	got := detectFormatterConventions(dir)
	if len(got) != 2 {
		t.Fatalf("expected 2 conventions, got %d: %v", len(got), got)
	}
}

func TestDetectFormatterConventions_OnlyOnePerFormatter(t *testing.T) {
	// Multiple Prettier config files should produce only one convention.
	dir := t.TempDir()
	touchFile(t, dir, ".prettierrc")
	touchFile(t, dir, ".prettierrc.json")
	got := detectFormatterConventions(dir)
	prettierCount := 0
	for _, c := range got {
		if strings.Contains(c, "Prettier") {
			prettierCount++
		}
	}
	if prettierCount != 1 {
		t.Fatalf("expected exactly 1 Prettier convention, got %d: %v", prettierCount, got)
	}
}

func TestFormatterConventionMessage(t *testing.T) {
	// The message must always contain the re-read reminder.
	msg := formatConvention("Prettier")
	if !strings.Contains(msg, "re-read files after writing") {
		t.Errorf("message missing re-read reminder: %q", msg)
	}
	if !strings.Contains(msg, "Prettier") {
		t.Errorf("message missing formatter name: %q", msg)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func touchFile(t *testing.T, dir, name string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("touchFile %s: %v", name, err)
	}
	f.Close()
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writeFile %s: %v", name, err)
	}
}

func assertFormatterMsg(t *testing.T, msg string) {
	t.Helper()
	if !strings.Contains(msg, "re-read files after writing") {
		t.Errorf("convention missing re-read reminder: %q", msg)
	}
}
