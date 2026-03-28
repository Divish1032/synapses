package scout

// White-box tests for techstack.go — tests unexported parsers directly
// using package scout (not scout_test) for symbol access.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ── moduleShortName ───────────────────────────────────────────────────────────

func TestModuleShortName_Long(t *testing.T) {
	got := moduleShortName("github.com/mark3labs/mcp-go")
	if got != "mcp-go" {
		t.Errorf("got %q, want %q", got, "mcp-go")
	}
}

func TestModuleShortName_Short(t *testing.T) {
	got := moduleShortName("context")
	if got != "context" {
		t.Errorf("got %q, want %q", got, "context")
	}
}

func TestModuleShortName_GoOrg(t *testing.T) {
	got := moduleShortName("golang.org/x/tools")
	if got != "tools" {
		t.Errorf("got %q, want %q", got, "tools")
	}
}

// ── parseGoMod ────────────────────────────────────────────────────────────────

func TestParseGoMod_NoFile(t *testing.T) {
	entries := parseGoMod(t.TempDir())
	if entries != nil {
		t.Error("expected nil for missing go.mod")
	}
}

func TestParseGoMod_BlockRequire(t *testing.T) {
	dir := t.TempDir()
	gomod := `module example.com/test

go 1.21

require (
	github.com/foo/bar v1.2.3
	github.com/baz/qux v0.1.0
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseGoMod(dir)
	if len(entries) == 0 {
		t.Fatal("expected entries from block require")
	}
	// Entries are sorted by short name; bar < qux alphabetically
	if entries[0].Name != "bar" {
		t.Errorf("expected bar first (sorted), got %q", entries[0].Name)
	}
	if entries[0].Ecosystem != "go" {
		t.Errorf("expected ecosystem %q, got %q", "go", entries[0].Ecosystem)
	}
}

func TestParseGoMod_SingleLineRequire(t *testing.T) {
	dir := t.TempDir()
	gomod := `module example.com/test

go 1.21

require github.com/foo/bar v1.0.0
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseGoMod(dir)
	if len(entries) == 0 {
		t.Fatal("expected entries from single-line require")
	}
	if entries[0].Name != "bar" {
		t.Errorf("expected %q, got %q", "bar", entries[0].Name)
	}
}

func TestParseGoMod_SkipsIndirect(t *testing.T) {
	dir := t.TempDir()
	// indirect deps are marked with "// indirect" — should be skipped
	gomod := `module example.com/test

go 1.21

require (
	github.com/direct/dep v1.0.0
	github.com/indirect/dep v2.0.0 // indirect
)
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseGoMod(dir)
	for _, e := range entries {
		if e.Name == "dep" && e.Version == "v2.0.0" {
			t.Error("indirect dep should be skipped")
		}
	}
}

func TestParseGoMod_CapsAtMaxDeps(t *testing.T) {
	dir := t.TempDir()
	// Write more than maxDeps (10) entries.
	gomod := "module example.com/big\n\ngo 1.21\n\nrequire (\n"
	for i := 0; i < 15; i++ {
		gomod += "\tgithub.com/pkg/dep" + string(rune('a'+i)) + " v1.0.0\n"
	}
	gomod += ")\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseGoMod(dir)
	if len(entries) > maxDeps {
		t.Errorf("expected at most %d entries, got %d", maxDeps, len(entries))
	}
}

// ── parsePackageJSON ──────────────────────────────────────────────────────────

func TestParsePackageJSON_NoFile(t *testing.T) {
	entries := parsePackageJSON(t.TempDir())
	if entries != nil {
		t.Error("expected nil for missing package.json")
	}
}

func TestParsePackageJSON_WithDeps(t *testing.T) {
	dir := t.TempDir()
	pkg := map[string]interface{}{
		"name":    "my-app",
		"version": "1.0.0",
		"dependencies": map[string]string{
			"react": "^18.2.0",
			"axios": "~1.4.0",
		},
		"devDependencies": map[string]string{
			"typescript": "^5.0.0",
		},
	}
	data, _ := json.Marshal(pkg)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePackageJSON(dir)
	if len(entries) == 0 {
		t.Fatal("expected entries from package.json")
	}
	// All should be "node" ecosystem.
	for _, e := range entries {
		if e.Ecosystem != "node" {
			t.Errorf("expected ecosystem node, got %q", e.Ecosystem)
		}
	}
	// Versions should have ^ and ~ stripped.
	for _, e := range entries {
		if e.Version != "" && (e.Version[0] == '^' || e.Version[0] == '~') {
			t.Errorf("version %q still has prefix char", e.Version)
		}
	}
}

func TestParsePackageJSON_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePackageJSON(dir)
	if entries != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestParsePackageJSON_CapsAtMaxDeps(t *testing.T) {
	dir := t.TempDir()
	deps := make(map[string]string)
	for i := 0; i < 15; i++ {
		deps["pkg-"+string(rune('a'+i))] = "1.0.0"
	}
	pkg := map[string]interface{}{"dependencies": deps}
	data, _ := json.Marshal(pkg)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePackageJSON(dir)
	if len(entries) > maxDeps {
		t.Errorf("expected at most %d entries, got %d", maxDeps, len(entries))
	}
}

// ── parseRequirementsTxt ──────────────────────────────────────────────────────

func TestParseRequirementsTxt_NoFile(t *testing.T) {
	entries := parseRequirementsTxt(t.TempDir())
	if entries != nil {
		t.Error("expected nil for missing requirements.txt")
	}
}

func TestParseRequirementsTxt_WithDeps(t *testing.T) {
	dir := t.TempDir()
	content := `# a comment
requests==2.31.0
flask>=2.0.0
numpy<=1.25.0
scipy~=1.11.0
pandas!=1.5.0
bare-dep
`
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseRequirementsTxt(dir)
	if len(entries) == 0 {
		t.Fatal("expected entries from requirements.txt")
	}
	byName := make(map[string]TechStackEntry)
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e, ok := byName["requests"]; !ok || e.Version != "2.31.0" {
		t.Errorf("requests: want 2.31.0, got %+v", byName["requests"])
	}
	if e, ok := byName["flask"]; !ok || e.Version != "2.0.0" {
		t.Errorf("flask: want 2.0.0, got %+v", byName["flask"])
	}
	if e, ok := byName["bare-dep"]; !ok || e.Version != "" {
		t.Errorf("bare-dep: want empty version, got %+v", byName["bare-dep"])
	}
	for _, e := range entries {
		if e.Ecosystem != "python" {
			t.Errorf("expected ecosystem python, got %q for %q", e.Ecosystem, e.Name)
		}
	}
}

func TestParseRequirementsTxt_SkipsComments(t *testing.T) {
	dir := t.TempDir()
	content := "# comment only\n\n"
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseRequirementsTxt(dir)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for comment-only file, got %d", len(entries))
	}
}

func TestParseRequirementsTxt_CapsAtMaxDeps(t *testing.T) {
	dir := t.TempDir()
	var content string
	for i := 0; i < 15; i++ {
		content += "pkg" + string(rune('a'+i)) + "==1.0.0\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseRequirementsTxt(dir)
	if len(entries) > maxDeps {
		t.Errorf("expected at most %d entries, got %d", maxDeps, len(entries))
	}
}

// ── parseCargoToml ────────────────────────────────────────────────────────────

func TestParseCargoToml_NoFile(t *testing.T) {
	entries := parseCargoToml(t.TempDir())
	if entries != nil {
		t.Error("expected nil for missing Cargo.toml")
	}
}

func TestParseCargoToml_WithDeps(t *testing.T) {
	dir := t.TempDir()
	content := `[package]
name = "my-crate"
version = "0.1.0"

[dependencies]
serde = "1.0"
tokio = {version = "1.0", features = ["full"]}

[dev-dependencies]
criterion = "0.4"
`
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseCargoToml(dir)
	if len(entries) == 0 {
		t.Fatal("expected entries from Cargo.toml")
	}
	for _, e := range entries {
		if e.Ecosystem != "rust" {
			t.Errorf("expected ecosystem rust, got %q", e.Ecosystem)
		}
	}
	byName := make(map[string]TechStackEntry)
	for _, e := range entries {
		byName[e.Name] = e
	}
	if _, ok := byName["serde"]; !ok {
		t.Error("expected serde in entries")
	}
	if _, ok := byName["criterion"]; !ok {
		t.Error("expected criterion (dev-dep) in entries")
	}
}

func TestParseCargoToml_SectionsReset(t *testing.T) {
	dir := t.TempDir()
	// [other-section] should stop collecting deps
	content := `[dependencies]
serde = "1.0"

[other-section]
not-a-dep = "value"
`
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseCargoToml(dir)
	for _, e := range entries {
		if e.Name == "not-a-dep" {
			t.Error("should not include entries from [other-section]")
		}
	}
}

// ── parsePyprojectToml ────────────────────────────────────────────────────────

func TestParsePyprojectToml_NoFile(t *testing.T) {
	entries := parsePyprojectToml(t.TempDir())
	if entries != nil {
		t.Error("expected nil for missing pyproject.toml")
	}
}

func TestParsePyprojectToml_PoetryDeps(t *testing.T) {
	dir := t.TempDir()
	content := `[tool.poetry]
name = "myproject"
version = "0.1.0"

[tool.poetry.dependencies]
python = "^3.11"
requests = "^2.31.0"
fastapi = "^0.100.0"
`
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePyprojectToml(dir)
	if len(entries) == 0 {
		t.Fatal("expected entries from pyproject.toml")
	}
	for _, e := range entries {
		if e.Name == "python" {
			t.Error("python should be excluded from entries")
		}
		if e.Ecosystem != "python" {
			t.Errorf("expected ecosystem python, got %q", e.Ecosystem)
		}
	}
}

func TestParsePyprojectToml_ProjectDeps(t *testing.T) {
	dir := t.TempDir()
	content := `[project]
name = "myproject"
version = "1.0.0"
description = "a project"
requires-python = ">=3.9"
requests = ">=2.0"
httpx = "^0.24"
`
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePyprojectToml(dir)
	// name, version, description are excluded
	for _, e := range entries {
		if e.Name == "name" || e.Name == "version" || e.Name == "description" {
			t.Errorf("metadata field %q should be excluded", e.Name)
		}
	}
}

func TestParsePyprojectToml_SectionReset(t *testing.T) {
	dir := t.TempDir()
	content := `[tool.poetry.dependencies]
requests = "2.31.0"

[build-system]
not-a-dep = "value"
`
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePyprojectToml(dir)
	for _, e := range entries {
		if e.Name == "not-a-dep" {
			t.Error("should not include entries from [build-system]")
		}
	}
}

// ── DetectTechStack ───────────────────────────────────────────────────────────

func TestDetectTechStack_EmptyDir(t *testing.T) {
	entries := DetectTechStack(t.TempDir())
	if len(entries) != 0 {
		t.Errorf("expected empty result for dir with no manifests, got %d", len(entries))
	}
}

func TestDetectTechStack_GoOnly(t *testing.T) {
	dir := t.TempDir()
	gomod := "module example.com/test\n\ngo 1.21\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := DetectTechStack(dir)
	if len(entries) == 0 {
		t.Fatal("expected at least 1 entry")
	}
	if entries[0].Ecosystem != "go" {
		t.Errorf("expected go ecosystem, got %q", entries[0].Ecosystem)
	}
}

func TestDetectTechStack_NodeOnly(t *testing.T) {
	dir := t.TempDir()
	pkg := map[string]interface{}{"dependencies": map[string]string{"express": "4.18.0"}}
	data, _ := json.Marshal(pkg)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	entries := DetectTechStack(dir)
	if len(entries) == 0 {
		t.Fatal("expected at least 1 entry")
	}
	if entries[0].Ecosystem != "node" {
		t.Errorf("expected node ecosystem, got %q", entries[0].Ecosystem)
	}
}

func TestDetectTechStack_TotalCappedAtMaxDeps(t *testing.T) {
	dir := t.TempDir()

	// Go: 8 deps
	var gomod string
	gomod = "module example.com/test\n\ngo 1.21\n\nrequire (\n"
	for i := 0; i < 8; i++ {
		gomod += "\tgithub.com/go/dep" + string(rune('a'+i)) + " v1.0.0\n"
	}
	gomod += ")\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Node: 8 deps (but total should cap at 10)
	nodeDeps := make(map[string]string)
	for i := 0; i < 8; i++ {
		nodeDeps["node-pkg-"+string(rune('a'+i))] = "1.0.0"
	}
	pkg := map[string]interface{}{"dependencies": nodeDeps}
	data, _ := json.Marshal(pkg)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	entries := DetectTechStack(dir)
	if len(entries) > maxDeps {
		t.Errorf("DetectTechStack should cap at %d, got %d", maxDeps, len(entries))
	}
}

func TestDetectTechStack_MultipleManifests(t *testing.T) {
	dir := t.TempDir()

	// Go
	gomod := "module example.com/test\n\ngo 1.21\n\nrequire (\n\tgithub.com/foo/bar v1.0.0\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	// Python
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask==2.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := DetectTechStack(dir)
	ecosystems := make(map[string]bool)
	for _, e := range entries {
		ecosystems[e.Ecosystem] = true
	}
	if !ecosystems["go"] {
		t.Error("expected go ecosystem in results")
	}
	if !ecosystems["python"] {
		t.Error("expected python ecosystem in results")
	}
}

// ── Edge cases for parsing functions ───────────────────────────────────────────

func TestParseCargoToml_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	content := ""
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseCargoToml(dir)
	if len(entries) != 0 {
		t.Errorf("expected empty or nil for empty Cargo.toml, got %d entries", len(entries))
	}
}

func TestParseCargoToml_NoEqualsSign(t *testing.T) {
	dir := t.TempDir()
	content := `[dependencies]
invalid-line-without-equals
another-invalid`
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseCargoToml(dir)
	if len(entries) != 0 {
		t.Errorf("expected no entries for invalid deps, got %d", len(entries))
	}
}

func TestParseCargoToml_MixedSections(t *testing.T) {
	dir := t.TempDir()
	content := `[dependencies]
serde = "1.0"

[dev-dependencies]
test-lib = "0.5"

[build-dependencies]
build-tool = "2.0"
`
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parseCargoToml(dir)
	if len(entries) != 2 {
		t.Errorf("expected 2 entries (serde + test-lib), got %d", len(entries))
	}
}

func TestParsePyprojectToml_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	content := ""
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePyprojectToml(dir)
	if len(entries) != 0 {
		t.Errorf("expected empty or nil for empty pyproject.toml, got %d entries", len(entries))
	}
}

func TestParsePyprojectToml_ProjectDepsWithSkipped(t *testing.T) {
	dir := t.TempDir()
	content := `[project]
name = "myproject"
version = "1.0.0"
description = "A test project"
python = "3.11"
dependencies = [
    "requests==2.31.0"
]
`
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePyprojectToml(dir)
	// Should skip name, version, description, python entries
	for _, e := range entries {
		if e.Name == "name" || e.Name == "version" || e.Name == "description" || e.Name == "python" {
			t.Errorf("should skip %q entry", e.Name)
		}
	}
}

func TestParsePyprojectToml_NoEqualsSign(t *testing.T) {
	dir := t.TempDir()
	content := `[tool.poetry.dependencies]
invalid-line-no-equals
another-invalid
requests = "2.31.0"
`
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := parsePyprojectToml(dir)
	// Should only have requests
	if len(entries) != 1 || entries[0].Name != "requests" {
		t.Errorf("expected only requests entry, got %d entries", len(entries))
	}
}

func TestModuleShortName_EmptyString(t *testing.T) {
	got := moduleShortName("")
	if got != "" {
		t.Errorf("empty string should return empty string, got %q", got)
	}
}

func TestModuleShortName_SingleSlash(t *testing.T) {
	got := moduleShortName("a/b")
	if got != "b" {
		t.Errorf("expected %q, got %q", "b", got)
	}
}
