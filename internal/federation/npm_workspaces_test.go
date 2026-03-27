package federation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNPMWorkspace_PackageJSON(t *testing.T) {
	dir := t.TempDir()

	// Root package.json with workspaces.
	rootPkg := `{"name": "monorepo", "workspaces": ["packages/*"]}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(rootPkg), 0644)

	// Create workspace packages.
	for _, pkg := range []struct{ name, dir string }{
		{"@org/auth", "packages/auth"},
		{"@org/utils", "packages/utils"},
	} {
		pkgDir := filepath.Join(dir, pkg.dir)
		os.MkdirAll(pkgDir, 0755)
		pkgJSON := `{"name": "` + pkg.name + `"}`
		os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(pkgJSON), 0644)
	}

	ws := ParseNPMWorkspace(dir)
	if ws == nil {
		t.Fatal("expected non-nil workspace")
	}
	if len(ws.Packages) != 2 {
		t.Fatalf("expected 2 packages, got %d", len(ws.Packages))
	}
	if ws.Packages["@org/auth"] != "packages/auth" {
		t.Errorf("auth package: %q", ws.Packages["@org/auth"])
	}
}

func TestParseNPMWorkspace_PnpmWorkspace(t *testing.T) {
	dir := t.TempDir()

	// pnpm-workspace.yaml
	yaml := "packages:\n  - 'apps/*'\n  - 'libs/*'\n"
	os.WriteFile(filepath.Join(dir, "pnpm-workspace.yaml"), []byte(yaml), 0644)

	// Create a package.
	appDir := filepath.Join(dir, "apps", "web")
	os.MkdirAll(appDir, 0755)
	os.WriteFile(filepath.Join(appDir, "package.json"), []byte(`{"name": "@org/web"}`), 0644)

	ws := ParseNPMWorkspace(dir)
	if ws == nil {
		t.Fatal("expected non-nil workspace")
	}
	if ws.Packages["@org/web"] != filepath.Join("apps", "web") {
		t.Errorf("web package dir: %q", ws.Packages["@org/web"])
	}
}

func TestNPMWorkspace_MatchesWorkspacePackage(t *testing.T) {
	ws := &NPMWorkspace{
		RootDir: "/project",
		Packages: map[string]string{
			"@org/auth":  "packages/auth",
			"@org/utils": "packages/utils",
		},
	}

	dir, matched := ws.MatchesWorkspacePackage("@org/auth")
	if !matched || dir != "packages/auth" {
		t.Errorf("direct match: dir=%q matched=%v", dir, matched)
	}

	dir, matched = ws.MatchesWorkspacePackage("@org/auth/middleware")
	if !matched || dir != "packages/auth" {
		t.Errorf("prefix match: dir=%q matched=%v", dir, matched)
	}

	_, matched = ws.MatchesWorkspacePackage("react")
	if matched {
		t.Error("expected no match for external package")
	}
}

func TestNPMWorkspace_Nil(t *testing.T) {
	var ws *NPMWorkspace
	_, matched := ws.MatchesWorkspacePackage("@org/auth")
	if matched {
		t.Error("expected no match on nil workspace")
	}
}

func TestParseNPMWorkspace_NoWorkspace(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name": "simple-app"}`), 0644)

	ws := ParseNPMWorkspace(dir)
	if ws != nil {
		t.Error("expected nil for non-workspace project")
	}
}
