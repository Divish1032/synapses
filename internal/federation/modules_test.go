package federation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseGoMod_Basic(t *testing.T) {
	dir := t.TempDir()
	goModPath := filepath.Join(dir, "go.mod")
	content := `module github.com/example/myproject

go 1.21

require (
	github.com/foo/bar v1.2.3
	github.com/baz/qux v0.5.0
)
`
	if err := os.WriteFile(goModPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	mod, err := ParseGoMod(goModPath)
	if err != nil {
		t.Fatal(err)
	}

	if mod.ModulePath != "github.com/example/myproject" {
		t.Errorf("module path: got %q", mod.ModulePath)
	}
	if mod.GoVersion != "1.21" {
		t.Errorf("go version: got %q", mod.GoVersion)
	}
	if len(mod.Require) != 2 {
		t.Fatalf("expected 2 requires, got %d", len(mod.Require))
	}
	if mod.Require[0].Path != "github.com/foo/bar" || mod.Require[0].Version != "v1.2.3" {
		t.Errorf("require[0]: %+v", mod.Require[0])
	}
}

func TestParseGoMod_WithReplace(t *testing.T) {
	dir := t.TempDir()
	goModPath := filepath.Join(dir, "go.mod")
	content := `module github.com/example/myproject

go 1.21

require github.com/foo/bar v1.2.3

replace github.com/foo/bar => ../local-bar
`
	if err := os.WriteFile(goModPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	mod, err := ParseGoMod(goModPath)
	if err != nil {
		t.Fatal(err)
	}

	if len(mod.Replace) != 1 {
		t.Fatalf("expected 1 replace, got %d", len(mod.Replace))
	}
	if mod.Replace["github.com/foo/bar"] != "../local-bar" {
		t.Errorf("replace: got %q", mod.Replace["github.com/foo/bar"])
	}
}

func TestGoModule_MatchesSibling(t *testing.T) {
	mod := &GoModule{
		ModulePath: "github.com/org/main",
		Replace:    map[string]string{},
	}

	siblings := map[string]string{
		"github.com/org/auth":  "auth",
		"github.com/org/utils": "utils",
	}

	alias, matched := mod.MatchesSibling("github.com/org/auth/middleware", siblings)
	if !matched || alias != "auth" {
		t.Errorf("expected match to 'auth', got alias=%q matched=%v", alias, matched)
	}

	alias, matched = mod.MatchesSibling("github.com/other/pkg", siblings)
	if matched {
		t.Error("expected no match for unrelated import")
	}
	_ = alias
}

func TestGoModule_MatchesSibling_WithReplace(t *testing.T) {
	mod := &GoModule{
		ModulePath: "github.com/org/main",
		Replace: map[string]string{
			"github.com/org/auth": "github.com/org/auth-v2",
		},
	}

	siblings := map[string]string{
		"github.com/org/auth-v2": "auth",
	}

	alias, matched := mod.MatchesSibling("github.com/org/auth/pkg", siblings)
	if !matched || alias != "auth" {
		t.Errorf("expected match via replace, got alias=%q matched=%v", alias, matched)
	}
}

func TestFindGoMod(t *testing.T) {
	dir := t.TempDir()
	goModPath := filepath.Join(dir, "go.mod")
	os.WriteFile(goModPath, []byte("module example.com/test\n"), 0644)

	subDir := filepath.Join(dir, "pkg", "sub")
	os.MkdirAll(subDir, 0755)

	found := FindGoMod(subDir)
	if found != goModPath {
		t.Errorf("expected %q, got %q", goModPath, found)
	}
}
