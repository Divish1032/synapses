package federation_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Manifest reading tests ──────────────────────────────────────────────────

func TestBuildModuleIndex_GoMod(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")

	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: dir, Alias: "sib"}},
		newResolver(nil),
	)
	if d.ModuleCount() != 1 {
		t.Fatalf("expected 1 module, got %d", d.ModuleCount())
	}
	if d.Modules()[0].Prefix != "github.com/user/sibling" {
		t.Errorf("expected module prefix github.com/user/sibling, got %q", d.Modules()[0].Prefix)
	}
}

func TestBuildModuleIndex_PackageJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name": "@synapses/core", "version": "1.0.0"}`)

	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: dir, Alias: "core"}},
		newResolver(nil),
	)
	found := false
	for _, m := range d.Modules() {
		if m.Prefix == "@synapses/core" && m.Lang == "typescript" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected @synapses/core typescript module, got %+v", d.Modules())
	}
}

func TestBuildModuleIndex_CargoToml(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\nname = \"synapses-core\"\nversion = \"0.1.0\"\n")

	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: dir, Alias: "core"}},
		newResolver(nil),
	)
	found := false
	for _, m := range d.Modules() {
		if m.Prefix == "synapses-core" && m.Lang == "rust" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected synapses-core rust module, got %+v", d.Modules())
	}
}

func TestBuildModuleIndex_MultipleManifests(t *testing.T) {
	dir := t.TempDir()
	// Project has both go.mod and package.json (monorepo with Go + TS).
	writeFile(t, filepath.Join(dir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "package.json"), `{"name": "@sibling/core"}`)

	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: dir, Alias: "sib"}},
		newResolver(nil),
	)
	if d.ModuleCount() != 2 {
		t.Fatalf("expected 2 modules (Go + TS), got %d", d.ModuleCount())
	}
}

func TestBuildModuleIndex_MissingManifest(t *testing.T) {
	dir := t.TempDir() // empty — no manifests

	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: dir, Alias: "empty"}},
		newResolver(nil),
	)
	if d.ModuleCount() != 0 {
		t.Errorf("expected 0 modules for empty dir, got %d", d.ModuleCount())
	}
}

func TestBuildModuleIndex_MalformedGoMod(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "this is not a valid go.mod\n")

	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: dir, Alias: "bad"}},
		newResolver(nil),
	)
	// Should not crash, just skip.
	if d.ModuleCount() != 0 {
		t.Errorf("expected 0 modules for malformed go.mod, got %d", d.ModuleCount())
	}
}

// ── Go import extraction tests ──────────────────────────────────────────────

func TestDetectDeps_Go_SingleImport(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	// Create sibling store with the entity.
	createSiblingWithDefaultPath(t, sibDir, "sib",
		sampleNodeWithSig("sib", "AuthService", "pkg/auth/service.go", "func AuthService() *Service"))

	// Create local Go file that imports from sibling.
	localDir := t.TempDir()
	goFile := filepath.Join(localDir, "handler.go")
	writeFile(t, goFile, `package main

import (
	"fmt"
	"github.com/user/sibling/pkg/auth"
)

func Handler() {
	fmt.Println("hello")
	svc := auth.AuthService()
	_ = svc
}
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	deps := d.DetectDeps(bg(), goFile, nil)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d: %+v", len(deps), deps)
	}
	if deps[0].ToProject != "sib" {
		t.Errorf("expected project 'sib', got %q", deps[0].ToProject)
	}
	if deps[0].ToEntity != "AuthService" {
		t.Errorf("expected entity 'AuthService', got %q", deps[0].ToEntity)
	}
	if deps[0].VerifiedSignature == "" {
		t.Error("expected non-empty VerifiedSignature from graph resolution")
	}
	if deps[0].ToFile == "" {
		t.Error("expected non-empty ToFile from graph resolution")
	}
	if deps[0].DetectionTier != "tier1" {
		t.Errorf("expected tier1, got %q", deps[0].DetectionTier)
	}
}

func TestDetectDeps_Go_MultipleEntities(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	nodes := []*graph.Node{
		{ID: "sib::pkg/auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			File: "pkg/auth.go", Line: 1, Exported: true,
			Metadata: map[string]string{"signature": "func Validate(token string) bool"}},
		{ID: "sib::pkg/auth.go::Login", Name: "Login", Type: graph.NodeFunction,
			File: "pkg/auth.go", Line: 20, Exported: true,
			Metadata: map[string]string{"signature": "func Login(user, pass string) error"}},
	}
	createSiblingWithDefaultPath(t, sibDir, "sib", nodes)

	localDir := t.TempDir()
	goFile := filepath.Join(localDir, "handler.go")
	writeFile(t, goFile, `package main

import "github.com/user/sibling/pkg/auth"

func Handle() {
	auth.Validate("token")
	auth.Login("user", "pass")
}
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	deps := d.DetectDeps(bg(), goFile, nil)
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d: %+v", len(deps), deps)
	}
	entities := map[string]bool{}
	for _, dep := range deps {
		entities[dep.ToEntity] = true
	}
	if !entities["Validate"] || !entities["Login"] {
		t.Errorf("expected Validate and Login, got %v", entities)
	}
}

func TestDetectDeps_Go_MixedImports_OnlySiblingDetected(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	createSiblingWithDefaultPath(t, sibDir, "sib",
		sampleNodeWithSig("sib", "Validate", "pkg/auth.go", "func Validate(token string) bool"))

	localDir := t.TempDir()
	goFile := filepath.Join(localDir, "handler.go")
	// 4 imports: only 1 is from sibling.
	writeFile(t, goFile, `package main

import (
	"context"
	"fmt"
	"net/http"
	"github.com/user/sibling/pkg/auth"
)

func Handle(ctx context.Context, w http.ResponseWriter) {
	fmt.Fprintf(w, "%v", auth.Validate("token"))
}
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	deps := d.DetectDeps(bg(), goFile, nil)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep (only sibling), got %d: %+v", len(deps), deps)
	}
	if deps[0].ToEntity != "Validate" {
		t.Errorf("expected Validate, got %q", deps[0].ToEntity)
	}
}

func TestDetectDeps_Go_AliasedImport(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	createSiblingWithDefaultPath(t, sibDir, "sib",
		sampleNodeWithSig("sib", "Validate", "pkg/auth.go", "func Validate(token string) bool"))

	localDir := t.TempDir()
	goFile := filepath.Join(localDir, "handler.go")
	writeFile(t, goFile, `package main

import myauth "github.com/user/sibling/pkg/auth"

func Handle() {
	myauth.Validate("token")
}
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	deps := d.DetectDeps(bg(), goFile, nil)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep (aliased import), got %d: %+v", len(deps), deps)
	}
	if deps[0].ToEntity != "Validate" {
		t.Errorf("expected Validate, got %q", deps[0].ToEntity)
	}
}

func TestDetectDeps_Go_EntityNotInSiblingStore_Skipped(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	// Sibling store has AuthService but NOT Validate.
	createSiblingWithDefaultPath(t, sibDir, "sib",
		sampleNodeWithSig("sib", "AuthService", "pkg/auth.go", "func AuthService()"))

	localDir := t.TempDir()
	goFile := filepath.Join(localDir, "handler.go")
	writeFile(t, goFile, `package main

import "github.com/user/sibling/pkg/auth"

func Handle() {
	auth.Validate("token")
	auth.AuthService()
}
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	deps := d.DetectDeps(bg(), goFile, nil)
	// Only AuthService should be detected (Validate is not in store).
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep (anti-hallucination), got %d: %+v", len(deps), deps)
	}
	if deps[0].ToEntity != "AuthService" {
		t.Errorf("expected AuthService, got %q", deps[0].ToEntity)
	}
}

// ── TypeScript import tests ─────────────────────────────────────────────────

func TestDetectDeps_TS_NamedImport(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "package.json"), `{"name": "@synapses/core"}`)
	createSiblingWithDefaultPath(t, sibDir, "core",
		sampleNodeWithSig("core", "AuthService", "src/auth.ts", "export class AuthService"))

	localDir := t.TempDir()
	tsFile := filepath.Join(localDir, "handler.ts")
	writeFile(t, tsFile, `import { AuthService } from "@synapses/core"

const svc = new AuthService()
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "core"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "core"}}, r,
	)

	deps := d.DetectDeps(bg(), tsFile, nil)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d: %+v", len(deps), deps)
	}
	if deps[0].ToEntity != "AuthService" {
		t.Errorf("expected AuthService, got %q", deps[0].ToEntity)
	}
}

func TestDetectDeps_TS_MultipleNamedImports(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "package.json"), `{"name": "@synapses/core"}`)
	nodes := []*graph.Node{
		{ID: "core::src/auth.ts::AuthService", Name: "AuthService", Type: graph.NodeFunction,
			File: "src/auth.ts", Line: 1, Exported: true,
			Metadata: map[string]string{"signature": "export class AuthService"}},
		{ID: "core::src/auth.ts::Validate", Name: "Validate", Type: graph.NodeFunction,
			File: "src/auth.ts", Line: 20, Exported: true,
			Metadata: map[string]string{"signature": "export function Validate(token: string): boolean"}},
	}
	createSiblingWithDefaultPath(t, sibDir, "core", nodes)

	localDir := t.TempDir()
	tsFile := filepath.Join(localDir, "handler.ts")
	writeFile(t, tsFile, `import { AuthService, Validate } from "@synapses/core"

const svc = new AuthService()
const ok = Validate("token")
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "core"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "core"}}, r,
	)

	deps := d.DetectDeps(bg(), tsFile, nil)
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d: %+v", len(deps), deps)
	}
}

func TestDetectDeps_TS_AliasedImport(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "package.json"), `{"name": "@synapses/core"}`)
	createSiblingWithDefaultPath(t, sibDir, "core",
		sampleNodeWithSig("core", "AuthService", "src/auth.ts", "export class AuthService"))

	localDir := t.TempDir()
	tsFile := filepath.Join(localDir, "handler.ts")
	writeFile(t, tsFile, `import { AuthService as Auth } from "@synapses/core"

const svc = new Auth()
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "core"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "core"}}, r,
	)

	deps := d.DetectDeps(bg(), tsFile, nil)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep (original name from alias), got %d: %+v", len(deps), deps)
	}
	// Should detect original name (AuthService), not the alias (Auth).
	if deps[0].ToEntity != "AuthService" {
		t.Errorf("expected AuthService, got %q", deps[0].ToEntity)
	}
}

// ── Rust import tests ───────────────────────────────────────────────────────

func TestDetectDeps_Rust_UseStatement(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "Cargo.toml"), "[package]\nname = \"synapses-core\"\nversion = \"0.1.0\"\n")
	createSiblingWithDefaultPath(t, sibDir, "core",
		sampleNodeWithSig("core", "AuthService", "src/auth.rs", "pub struct AuthService"))

	localDir := t.TempDir()
	rsFile := filepath.Join(localDir, "handler.rs")
	writeFile(t, rsFile, `use synapses_core::auth::AuthService;

fn handle() {
    let svc = AuthService::new();
}
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "core"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "core"}}, r,
	)

	deps := d.DetectDeps(bg(), rsFile, nil)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d: %+v", len(deps), deps)
	}
	if deps[0].ToEntity != "AuthService" {
		t.Errorf("expected AuthService, got %q", deps[0].ToEntity)
	}
}

func TestDetectDeps_Rust_GroupedUse(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "Cargo.toml"), "[package]\nname = \"synapses-core\"\nversion = \"0.1.0\"\n")
	nodes := []*graph.Node{
		{ID: "core::src/auth.rs::Validate", Name: "Validate", Type: graph.NodeFunction,
			File: "src/auth.rs", Line: 1, Exported: true,
			Metadata: map[string]string{"signature": "pub fn Validate(token: &str) -> bool"}},
		{ID: "core::src/auth.rs::Login", Name: "Login", Type: graph.NodeFunction,
			File: "src/auth.rs", Line: 20, Exported: true,
			Metadata: map[string]string{"signature": "pub fn Login(user: &str) -> Result<Token, Error>"}},
	}
	createSiblingWithDefaultPath(t, sibDir, "core", nodes)

	localDir := t.TempDir()
	rsFile := filepath.Join(localDir, "handler.rs")
	writeFile(t, rsFile, `use synapses_core::auth::{Validate, Login};

fn handle() {
    Validate("token");
    Login("user");
}
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "core"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "core"}}, r,
	)

	deps := d.DetectDeps(bg(), rsFile, nil)
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d: %+v", len(deps), deps)
	}
}

func TestDetectDeps_Rust_HyphenatedCrate(t *testing.T) {
	// Cargo.toml has "synapses-core" but Rust code uses "synapses_core".
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "Cargo.toml"), "[package]\nname = \"synapses-core\"\nversion = \"0.1.0\"\n")
	createSiblingWithDefaultPath(t, sibDir, "core",
		sampleNodeWithSig("core", "Validate", "src/lib.rs", "pub fn Validate() -> bool"))

	localDir := t.TempDir()
	rsFile := filepath.Join(localDir, "handler.rs")
	writeFile(t, rsFile, `use synapses_core::Validate;

fn main() { Validate(); }
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "core"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "core"}}, r,
	)

	deps := d.DetectDeps(bg(), rsFile, nil)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep (hyphen→underscore normalization), got %d: %+v", len(deps), deps)
	}
}

// ── Edge cases ──────────────────────────────────────────────────────────────

func TestDetectDeps_UnsupportedLanguage_NoDeps(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")

	localDir := t.TempDir()
	pyFile := filepath.Join(localDir, "handler.py")
	writeFile(t, pyFile, `import something`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	deps := d.DetectDeps(bg(), pyFile, nil)
	if len(deps) != 0 {
		t.Errorf("expected 0 deps for Python (unsupported), got %d", len(deps))
	}
}

func TestDetectDeps_NoModules_NoDeps(t *testing.T) {
	d := federation.NewDeterministicDetector(nil, newResolver(nil))

	localDir := t.TempDir()
	goFile := filepath.Join(localDir, "handler.go")
	writeFile(t, goFile, `package main
import "fmt"
func main() { fmt.Println("hello") }
`)

	deps := d.DetectDeps(bg(), goFile, nil)
	if len(deps) != 0 {
		t.Errorf("expected 0 deps (no modules), got %d", len(deps))
	}
}

func TestDetectDeps_CancelledContext(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	createSiblingWithDefaultPath(t, sibDir, "sib",
		sampleNodeWithSig("sib", "Validate", "pkg/auth.go", "func Validate(token string) bool"))

	localDir := t.TempDir()
	goFile := filepath.Join(localDir, "handler.go")
	writeFile(t, goFile, `package main
import "github.com/user/sibling/pkg/auth"
func Handle() { auth.Validate("token") }
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	ctx, cancel := context.WithCancel(bg())
	cancel()
	deps := d.DetectDeps(ctx, goFile, nil)
	if len(deps) != 0 {
		t.Errorf("expected 0 deps for cancelled context, got %d", len(deps))
	}
}

func TestDetectDeps_FileDoesNotExist(t *testing.T) {
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: t.TempDir(), Alias: "sib"}},
		newResolver(nil),
	)
	deps := d.DetectDeps(bg(), "/nonexistent/file.go", nil)
	if len(deps) != 0 {
		t.Errorf("expected 0 deps for missing file, got %d", len(deps))
	}
}

func TestDetectDeps_SiblingStoreUnavailable_Skipped(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	// Do NOT create sibling store.

	localDir := t.TempDir()
	goFile := filepath.Join(localDir, "handler.go")
	writeFile(t, goFile, `package main
import "github.com/user/sibling/pkg/auth"
func Handle() { auth.Validate("token") }
`)

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	deps := d.DetectDeps(bg(), goFile, nil)
	if len(deps) != 0 {
		t.Errorf("expected 0 deps (sibling store unavailable), got %d", len(deps))
	}
}

// ── StoreDeps tests ─────────────────────────────────────────────────────────

func TestStoreDeps_PersistsAndCleanup(t *testing.T) {
	sibDir := t.TempDir()
	writeFile(t, filepath.Join(sibDir, "go.mod"), "module github.com/user/sibling\n\ngo 1.21\n")
	createSiblingWithDefaultPath(t, sibDir, "sib",
		sampleNodeWithSig("sib", "Validate", "pkg/auth.go", "func Validate(token string) bool"))

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sib"}})
	defer r.Close()
	d := federation.NewDeterministicDetector(
		[]config.FederationEntry{{Path: sibDir, Alias: "sib"}}, r,
	)

	localStore := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)

	// First round: store 2 deps.
	deps1 := []store.CrossProjectDep{
		{FromEntity: "file:handler.go", ToProject: "sib", ToEntity: "Validate",
			ToFile: "pkg/auth.go", VerifiedCommit: "abc", VerifiedAt: now,
			DetectionTier: "tier1", VerifiedSignature: "func Validate(token string) bool"},
		{FromEntity: "file:handler.go", ToProject: "sib", ToEntity: "Login",
			ToFile: "pkg/auth.go", VerifiedCommit: "abc", VerifiedAt: now,
			DetectionTier: "tier1", VerifiedSignature: "func Login() error"},
	}
	d.StoreDeps(deps1, localStore)

	stored, err := localStore.GetCrossProjectDeps("file:handler.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("expected 2 stored deps, got %d", len(stored))
	}

	// Second round: only 1 dep (Login removed from file). Old Login dep should be cleaned up.
	deps2 := []store.CrossProjectDep{
		{FromEntity: "file:handler.go", ToProject: "sib", ToEntity: "Validate",
			ToFile: "pkg/auth.go", VerifiedCommit: "def", VerifiedAt: now,
			DetectionTier: "tier1", VerifiedSignature: "func Validate(token string) bool"},
	}
	d.StoreDeps(deps2, localStore)

	stored, err = localStore.GetCrossProjectDeps("file:handler.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("expected 1 stored dep after cleanup, got %d", len(stored))
	}
	if stored[0].ToEntity != "Validate" {
		t.Errorf("expected Validate, got %q", stored[0].ToEntity)
	}
}

func TestStoreDeps_NilStore(t *testing.T) {
	d := federation.NewDeterministicDetector(nil, newResolver(nil))
	// Should not panic.
	d.StoreDeps([]store.CrossProjectDep{{FromEntity: "x"}}, nil)
}

// ── fileFromNodeID tests ────────────────────────────────────────────────────

func TestFileFromNodeID(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"repo::pkg/auth.go::Validate", "pkg/auth.go"},
		{"repo::src/main.rs::main", "src/main.rs"},
		{"repo::index.ts::App", "index.ts"},
		{"invalid", ""},
		{"repo::file", ""},
	}
	for _, tt := range tests {
		got := federation.FileFromNodeID(tt.id)
		if got != tt.want {
			t.Errorf("fileFromNodeID(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// ── langFromExt tests ───────────────────────────────────────────────────────

func TestLangFromExt(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"handler.go", "go"},
		{"app.ts", "typescript"},
		{"app.tsx", "typescript"},
		{"app.js", "typescript"},
		{"app.jsx", "typescript"},
		{"lib.rs", "rust"},
		{"main.py", ""},
		{"style.css", ""},
		{"readme.md", ""},
	}
	for _, tt := range tests {
		got := federation.LangFromExt(tt.path)
		if got != tt.want {
			t.Errorf("langFromExt(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// openTestStore is defined in resolver_test.go — shared across test files.
