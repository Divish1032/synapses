package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// --- parsePromptFile ---

func TestParsePromptFile_FullFrontmatter(t *testing.T) {
	data := []byte(`---
id: my-prompt
description: A test prompt
file_pattern: "**/*.go"
entity_pattern: "Service"
module_pattern: "internal/*"
auto_load: true
---
Body line 1.
Body line 2.`)

	pt := parsePromptFile(data, "user")

	if pt.ID != "my-prompt" {
		t.Errorf("ID: got %q, want %q", pt.ID, "my-prompt")
	}
	if pt.Description != "A test prompt" {
		t.Errorf("Description: got %q", pt.Description)
	}
	if pt.FilePattern != "**/*.go" {
		t.Errorf("FilePattern: got %q", pt.FilePattern)
	}
	if pt.EntityPattern != "Service" {
		t.Errorf("EntityPattern: got %q", pt.EntityPattern)
	}
	if pt.ModulePattern != "internal/*" {
		t.Errorf("ModulePattern: got %q", pt.ModulePattern)
	}
	if !pt.AutoLoad {
		t.Error("AutoLoad: expected true")
	}
	if pt.Source != "user" {
		t.Errorf("Source: got %q", pt.Source)
	}
	if pt.Body != "Body line 1.\nBody line 2." {
		t.Errorf("Body: got %q", pt.Body)
	}
}

func TestParsePromptFile_NoFrontmatter(t *testing.T) {
	data := []byte("Just plain body text.")
	pt := parsePromptFile(data, "builtin")
	if pt.Body != "Just plain body text." {
		t.Errorf("Body: got %q", pt.Body)
	}
	if pt.ID != "" {
		t.Errorf("ID should be empty, got %q", pt.ID)
	}
}

func TestParsePromptFile_MalformedFrontmatter(t *testing.T) {
	// No closing --- delimiter
	data := []byte("---\nid: broken\nno closing delimiter")
	pt := parsePromptFile(data, "builtin")
	// Should fall back to treating whole content as body
	if pt.Body == "" {
		t.Error("Body should not be empty for malformed frontmatter")
	}
}

func TestParsePromptFile_QuotedValues(t *testing.T) {
	data := []byte("---\nid: 'quoted-id'\ndescription: \"quoted desc\"\n---\nbody")
	pt := parsePromptFile(data, "project")
	if pt.ID != "quoted-id" {
		t.Errorf("ID: got %q, want %q", pt.ID, "quoted-id")
	}
	if pt.Description != "quoted desc" {
		t.Errorf("Description: got %q", pt.Description)
	}
}

// --- matchGlob ---

func TestMatchGlob_DoubleStar(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.go", "internal/graph/graph.go", true},
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/graph/graph.ts", false},
		{"**/*_test.go", "internal/mcp/tools_test.go", true},
		{"**/*_test.go", "internal/mcp/tools.go", false},
	}
	for _, tt := range tests {
		got := matchGlob(tt.pattern, tt.path)
		if got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestMatchGlob_DoubleStarSuffix(t *testing.T) {
	if !matchGlob("internal/**", "internal/graph/graph.go") {
		t.Error("internal/** should match internal/graph/graph.go")
	}
	if matchGlob("internal/**", "cmd/main.go") {
		t.Error("internal/** should not match cmd/main.go")
	}
}

func TestMatchGlob_Simple(t *testing.T) {
	if !matchGlob("*.go", "main.go") {
		t.Error("*.go should match main.go")
	}
	if matchGlob("*.go", "main.ts") {
		t.Error("*.go should not match main.ts")
	}
}

// --- matchRegex ---

func TestMatchRegex(t *testing.T) {
	if !matchRegex(".*Service$", "AuthService") {
		t.Error("should match AuthService")
	}
	if matchRegex(".*Service$", "AuthHandler") {
		t.Error("should not match AuthHandler")
	}
	// Invalid regex should return false, not panic.
	if matchRegex("[invalid", "anything") {
		t.Error("invalid regex should return false")
	}
}

// --- MatchPrompts ---

func TestMatchPrompts_FilePattern(t *testing.T) {
	templates := []PromptTemplate{
		{ID: "go-only", FilePattern: "**/*.go", Body: "go body"},
		{ID: "ts-only", FilePattern: "**/*.ts", Body: "ts body"},
	}
	got := MatchPrompts(templates, "internal/graph/graph.go", "Graph", "internal/graph")
	if len(got) != 1 || got[0].ID != "go-only" {
		t.Errorf("expected [go-only], got %v", got)
	}
}

func TestMatchPrompts_EntityPattern(t *testing.T) {
	templates := []PromptTemplate{
		{ID: "service-guide", EntityPattern: ".*Service"},
	}
	got := MatchPrompts(templates, "auth.go", "AuthService", "internal/auth")
	if len(got) != 1 {
		t.Errorf("expected 1 match, got %d", len(got))
	}
	got2 := MatchPrompts(templates, "auth.go", "AuthHandler", "internal/auth")
	if len(got2) != 0 {
		t.Errorf("expected 0 matches, got %d", len(got2))
	}
}

func TestMatchPrompts_NoPatterns_NoMatch(t *testing.T) {
	// Template with no patterns (only auto_load) should NOT appear in MatchPrompts.
	templates := []PromptTemplate{
		{ID: "auto-only", AutoLoad: true, Body: "project-wide"},
	}
	got := MatchPrompts(templates, "main.go", "main", "cmd")
	if len(got) != 0 {
		t.Errorf("expected 0 matches for pattern-less template, got %d", len(got))
	}
}

func TestMatchPrompts_EmptyInputs(t *testing.T) {
	templates := []PromptTemplate{
		{ID: "go", FilePattern: "**/*.go"},
	}
	// Empty file → no match on FilePattern
	got := MatchPrompts(templates, "", "Graph", "internal/graph")
	if len(got) != 0 {
		t.Errorf("empty file should not match, got %d", len(got))
	}
}

// --- AutoLoadPrompts ---

func TestAutoLoadPrompts(t *testing.T) {
	templates := []PromptTemplate{
		{ID: "a", AutoLoad: true},
		{ID: "b", AutoLoad: false},
		{ID: "c", AutoLoad: true},
	}
	got := AutoLoadPrompts(templates)
	if len(got) != 2 {
		t.Errorf("expected 2 auto-load prompts, got %d", len(got))
	}
}

// --- BuiltinPrompts ---

func TestBuiltinPrompts_NotEmpty(t *testing.T) {
	pts := BuiltinPrompts()
	if len(pts) == 0 {
		t.Error("expected at least one built-in prompt")
	}
	for _, pt := range pts {
		if pt.ID == "" {
			t.Error("built-in prompt missing ID")
		}
		if pt.Body == "" {
			t.Errorf("built-in prompt %q has empty body", pt.ID)
		}
		if pt.Source != "builtin" {
			t.Errorf("built-in prompt %q has wrong source: %q", pt.ID, pt.Source)
		}
	}
}

// --- LoadPromptDir ---

func TestLoadPromptDir_NonExistent(t *testing.T) {
	pts, err := LoadPromptDir("/no/such/dir", "project")
	if err != nil {
		t.Errorf("non-existent dir should return nil error, got %v", err)
	}
	if pts != nil {
		t.Errorf("non-existent dir should return nil slice, got %v", pts)
	}
}

func TestLoadPromptDir_LoadsFiles(t *testing.T) {
	dir := t.TempDir()
	content := "---\nid: test-prompt\ndescription: A test\nfile_pattern: \"**/*.go\"\n---\nTest body."
	if err := os.WriteFile(filepath.Join(dir, "test.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-.md file should be ignored
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	pts, err := LoadPromptDir(dir, "project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(pts))
	}
	if pts[0].ID != "test-prompt" {
		t.Errorf("ID: got %q", pts[0].ID)
	}
	if pts[0].Source != "project" {
		t.Errorf("Source: got %q", pts[0].Source)
	}
}

func TestLoadPromptDir_FallbackIDFromFilename(t *testing.T) {
	dir := t.TempDir()
	// File with no frontmatter — ID should fall back to filename stem
	if err := os.WriteFile(filepath.Join(dir, "my-guide.md"), []byte("Plain body."), 0o644); err != nil {
		t.Fatal(err)
	}

	pts, err := LoadPromptDir(dir, "user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(pts))
	}
	if pts[0].ID != "my-guide" {
		t.Errorf("ID fallback: got %q, want %q", pts[0].ID, "my-guide")
	}
}
