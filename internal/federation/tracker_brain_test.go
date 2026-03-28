package federation_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── BrainDetector tests ─────────────────────────────────────────────────────

func TestBrainDetector_ParsesCorrectly(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Validate(token string) error"}, File: "auth.go"},
		{ID: "core-repo::auth.go::AuthService", Name: "AuthService", Type: graph.NodeStruct,
			File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return `{"target_project": "core", "target_entity": "Validate", "import_path": "github.com/user/core/auth", "confidence": 0.9}
{"target_project": "core", "target_entity": "AuthService", "import_path": "github.com/user/core/auth", "confidence": 0.85}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	deps := bd.DetectDeps(context.Background(), "import auth\nauth.Validate(token)", 2000)

	if len(deps) != 2 {
		t.Fatalf("expected 2 validated deps, got %d", len(deps))
	}
	if deps[0].ToEntity != "Validate" {
		t.Errorf("expected first dep entity 'Validate', got %q", deps[0].ToEntity)
	}
	if deps[1].ToEntity != "AuthService" {
		t.Errorf("expected second dep entity 'AuthService', got %q", deps[1].ToEntity)
	}
}

func TestBrainDetector_HallucinationFiltered(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	// Sibling only has Validate, NOT FakeEntity.
	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Validate(token string) error"}, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return `{"target_project": "core", "target_entity": "Validate", "import_path": "auth", "confidence": 0.9}
{"target_project": "core", "target_entity": "FakeEntity", "import_path": "auth", "confidence": 0.95}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	// File content includes an import referencing "auth" so Gate 1 passes.
	// Gate 2 (entity existence) then filters FakeEntity.
	deps := bd.DetectDeps(context.Background(), "from auth import validate\nvalidate(token)", 2000)

	// FakeEntity should be filtered out (anti-hallucination).
	if len(deps) != 1 {
		t.Fatalf("expected 1 validated dep (FakeEntity filtered), got %d", len(deps))
	}
	if deps[0].ToEntity != "Validate" {
		t.Errorf("expected 'Validate', got %q", deps[0].ToEntity)
	}
}

func TestBrainDetector_LowConfidenceFiltered(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return `{"target_project": "core", "target_entity": "Validate", "import_path": "auth", "confidence": 0.5}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	deps := bd.DetectDeps(context.Background(), "from auth import validate", 2000)

	// Low confidence → filtered before reaching Gate 1 or Gate 2.
	if len(deps) != 0 {
		t.Fatalf("expected 0 deps (low confidence filtered), got %d", len(deps))
	}
}

func TestBrainDetector_BrainUnavailable(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return "", errors.New("LLM connection failed")
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	deps := bd.DetectDeps(context.Background(), "some code", 2000)

	// Brain error → fail-open, nil deps.
	if deps != nil {
		t.Errorf("expected nil deps on brain error, got %d", len(deps))
	}
}

func TestBrainDetector_NilGenerate(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()

	bd := federation.NewBrainDetector(nil, r, []string{"core"})
	deps := bd.DetectDeps(context.Background(), "some code", 2000)

	if deps != nil {
		t.Errorf("expected nil deps with nil generate, got %d", len(deps))
	}
}

func TestBrainDetector_EmptyCode(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		t.Fatal("generate should not be called for empty code")
		return "", nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	deps := bd.DetectDeps(context.Background(), "", 2000)

	if deps != nil {
		t.Errorf("expected nil deps for empty code, got %d", len(deps))
	}
}

func TestBrainDetector_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return `not json at all
{"broken json
{"target_project": "core", "target_entity": "Validate", "import_path": "auth", "confidence": 0.9}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	// File has an import referencing "auth" so Gate 1 passes for the valid line.
	deps := bd.DetectDeps(context.Background(), "from auth import validate", 2000)

	// Only the valid line should produce a dep.
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep from partially valid JSON, got %d", len(deps))
	}
}

func TestBrainDetector_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		t.Fatal("generate should not be called with cancelled context")
		return "", nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deps := bd.DetectDeps(ctx, "some code", 2000)

	if deps != nil {
		t.Errorf("expected nil deps with cancelled context, got %d", len(deps))
	}
}

// ── Import cross-validation tests (Gate 1 — prompt injection defense) ───────

func TestBrainDetector_NoImport_Rejected(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	// Entity exists in sibling — Gate 2 would pass.
	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return `{"target_project": "core", "target_entity": "Validate", "import_path": "core/auth", "confidence": 0.95}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	// File has NO import statements — a prompt injection told the LLM to
	// claim a dep that doesn't correspond to any actual import.
	deps := bd.DetectDeps(context.Background(), "print('hello world')\n# no imports here", 2000)

	if len(deps) != 0 {
		t.Fatalf("expected 0 deps (no matching import in file), got %d", len(deps))
	}
}

func TestBrainDetector_WrongImport_Rejected(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		// LLM claims dep on "core" but the file only imports "utils".
		return `{"target_project": "core", "target_entity": "Validate", "import_path": "core/auth", "confidence": 0.95}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	// File imports "utils" not "core" — the injected dep should be rejected.
	deps := bd.DetectDeps(context.Background(), "import utils\nutils.do_thing()", 2000)

	if len(deps) != 0 {
		t.Fatalf("expected 0 deps (import doesn't match target project), got %d", len(deps))
	}
}

func TestBrainDetector_MatchingImport_Accepted(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return `{"target_project": "core", "target_entity": "Validate", "import_path": "core.auth", "confidence": 0.95}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	// File has a Python import that references "core.auth" — both gates should pass.
	deps := bd.DetectDeps(context.Background(), "from core.auth import validate\nvalidate(token)", 2000)

	if len(deps) != 1 {
		t.Fatalf("expected 1 dep (matching import + entity exists), got %d", len(deps))
	}
	if deps[0].ToEntity != "Validate" {
		t.Errorf("expected 'Validate', got %q", deps[0].ToEntity)
	}
}

func TestBrainDetector_JavaImport_Accepted(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::Auth.java::AuthService", Name: "AuthService", Type: graph.NodeStruct, File: "Auth.java"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return `{"target_project": "core", "target_entity": "AuthService", "import_path": "com.core.auth.AuthService", "confidence": 0.9}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	deps := bd.DetectDeps(context.Background(), "import com.core.auth.AuthService;\n\npublic class Main {}", 2000)

	if len(deps) != 1 {
		t.Fatalf("expected 1 dep (Java import matches), got %d", len(deps))
	}
}

func TestBrainDetector_RubyRequire_Accepted(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.rb::AuthService", Name: "AuthService", Type: graph.NodeStruct, File: "auth.rb"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	generate := func(ctx context.Context, prompt string) (string, error) {
		return `{"target_project": "core", "target_entity": "AuthService", "import_path": "core/auth", "confidence": 0.9}`, nil
	}

	bd := federation.NewBrainDetector(generate, r, []string{"core"})
	deps := bd.DetectDeps(context.Background(), "require 'core/auth'\n\nAuthService.new", 2000)

	if len(deps) != 1 {
		t.Fatalf("expected 1 dep (Ruby require matches), got %d", len(deps))
	}
}

// ── ResolveBrainDeps tests ──────────────────────────────────────────────────

func TestResolveBrainDeps_DeduplicatesAgainstTier1(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Validate(token string) error"}, File: "auth.go"},
		{ID: "core-repo::auth.go::Login", Name: "Login", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Login(user string) error"}, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	det := federation.NewDeterministicDetector([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, r)

	// Tier 1 already found Validate.
	tier1Deps := []store.CrossProjectDep{
		{ToProject: "core", ToEntity: "Validate"},
	}

	brainDeps := []federation.RawCrossDep{
		{ToProject: "core", ToEntity: "Validate"}, // duplicate
		{ToProject: "core", ToEntity: "Login"},    // new
	}

	resolved := det.ResolveBrainDeps(context.Background(), brainDeps, tier1Deps, "/test/file.py")
	if len(resolved) != 1 {
		t.Fatalf("expected 1 dep (Validate deduplicated), got %d", len(resolved))
	}
	if resolved[0].ToEntity != "Login" {
		t.Errorf("expected 'Login', got %q", resolved[0].ToEntity)
	}
	if resolved[0].DetectionTier != "tier2" {
		t.Errorf("expected tier2, got %q", resolved[0].DetectionTier)
	}
}
