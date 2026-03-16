package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain/config"
)

// ---------------------------------------------------------------------------
// ProbeAndDowngradeModels
// ---------------------------------------------------------------------------

func TestProbeAndDowngradeModels_NilListFn(t *testing.T) {
	cfg := config.DefaultConfig()
	err := cfg.ProbeAndDowngradeModels(context.Background(), "http://localhost:11434", nil)
	if err != nil {
		t.Fatalf("expected nil error with nil listFn, got %v", err)
	}
}

func TestProbeAndDowngradeModels_ListFnError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IntelligenceMode = config.ModeStandard
	cfg.AutoConfigureModels(0)

	listFn := func(_ context.Context, _ string) ([]string, error) {
		return nil, context.DeadlineExceeded
	}
	err := cfg.ProbeAndDowngradeModels(context.Background(), "http://localhost:11434", listFn)
	if err != nil {
		t.Fatalf("expected nil error when Ollama unreachable, got %v", err)
	}
	// Models should remain unchanged.
	if cfg.ModelGuardian != "synapses/critic" {
		t.Errorf("ModelGuardian = %q, should be unchanged", cfg.ModelGuardian)
	}
}

func TestProbeAndDowngradeModels_DowngradesMissingModels(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IntelligenceMode = config.ModeStandard
	cfg.AutoConfigureModels(0)

	// Only sentry and navigator are available.
	listFn := func(_ context.Context, _ string) ([]string, error) {
		return []string{"synapses/sentry:latest", "synapses/navigator:latest"}, nil
	}
	err := cfg.ProbeAndDowngradeModels(context.Background(), "http://localhost:11434", listFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Guardian (critic), Enrich (librarian), Archivist should be downgraded to ingest model.
	base := cfg.ModelIngest
	if cfg.ModelGuardian != base {
		t.Errorf("ModelGuardian = %q, want %q (downgraded)", cfg.ModelGuardian, base)
	}
	if cfg.ModelEnrich != base {
		t.Errorf("ModelEnrich = %q, want %q (downgraded)", cfg.ModelEnrich, base)
	}
	if cfg.ModelArchivist != base {
		t.Errorf("ModelArchivist = %q, want %q (downgraded)", cfg.ModelArchivist, base)
	}
}

func TestProbeAndDowngradeModels_AllPresent(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.IntelligenceMode = config.ModeStandard
	cfg.AutoConfigureModels(0)

	listFn := func(_ context.Context, _ string) ([]string, error) {
		return []string{
			"synapses/sentry:latest",
			"synapses/critic:latest",
			"synapses/librarian:latest",
			"synapses/navigator:latest",
			"synapses/archivist:latest",
		}, nil
	}
	err := cfg.ProbeAndDowngradeModels(context.Background(), "http://localhost:11434", listFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Nothing should be downgraded.
	if cfg.ModelGuardian != "synapses/critic" {
		t.Errorf("ModelGuardian = %q, want synapses/critic", cfg.ModelGuardian)
	}
	if cfg.ModelEnrich != "synapses/librarian" {
		t.Errorf("ModelEnrich = %q, want synapses/librarian", cfg.ModelEnrich)
	}
}

func TestProbeAndDowngradeModels_LatestSuffixNormalization(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ModelIngest = "mymodel"
	cfg.ModelGuardian = "mymodel" // same as ingest, no tag
	cfg.ModelEnrich = "othermodel"
	cfg.ModelOrchestrate = "mymodel"
	cfg.ModelArchivist = "mymodel"

	listFn := func(_ context.Context, _ string) ([]string, error) {
		// Ollama returns with :latest suffix.
		return []string{"mymodel:latest"}, nil
	}
	err := cfg.ProbeAndDowngradeModels(context.Background(), "http://localhost:11434", listFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// "othermodel" is missing, should be downgraded to "mymodel".
	if cfg.ModelEnrich != "mymodel" {
		t.Errorf("ModelEnrich = %q, want mymodel (downgraded)", cfg.ModelEnrich)
	}
}

// ---------------------------------------------------------------------------
// SaveFile — creates nested directories
// ---------------------------------------------------------------------------

func TestSaveFile_CreatesNestedDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "brain.json")
	err := config.SaveFile(path, config.DefaultConfig())
	if err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file to exist at %s", path)
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — tilde expansion in GGUFPath and ModelDir
// ---------------------------------------------------------------------------

func TestApplyDefaults_TildeExpansionInGGUFPath(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"backend":   "local",
		"gguf_path": "~/models/test.gguf",
		"model_dir": "/tmp/models",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "models", "test.gguf")
	if cfg.GGUFPath != want {
		t.Errorf("GGUFPath = %q, want %q", cfg.GGUFPath, want)
	}
}

func TestApplyDefaults_TildeExpansionInModelDir(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"model_dir": "~/custom-models",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "custom-models")
	if cfg.ModelDir != want {
		t.Errorf("ModelDir = %q, want %q", cfg.ModelDir, want)
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — FastModel falls back to Model
// ---------------------------------------------------------------------------

func TestApplyDefaults_FastModelFallsToModel(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"model":      "custom:7b",
		"fast_model": "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.FastModel != "custom:7b" {
		t.Errorf("FastModel = %q, want custom:7b", cfg.FastModel)
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — negative port/timeout treated as zero
// ---------------------------------------------------------------------------

func TestApplyDefaults_NegativePort(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"port": -1})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Port != 11435 {
		t.Errorf("Port = %d, want 11435", cfg.Port)
	}
}

func TestApplyDefaults_NegativeTimeout(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"timeout_ms": -100})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.TimeoutMS != 60000 {
		t.Errorf("TimeoutMS = %d, want 60000", cfg.TimeoutMS)
	}
}

func TestApplyDefaults_NegativeEmbedPort(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"embed_port": -5})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.EmbedPort != 11437 {
		t.Errorf("EmbedPort = %d, want 11437", cfg.EmbedPort)
	}
}

// ---------------------------------------------------------------------------
// ModelsToInstall — empty model fields excluded
// ---------------------------------------------------------------------------

func TestModelsToInstall_EmptyFieldsExcluded(t *testing.T) {
	cfg := config.BrainConfig{
		ModelIngest:      "a",
		ModelGuardian:    "",
		ModelEnrich:      "a",
		ModelOrchestrate: "",
		ModelArchivist:   "",
	}
	models := cfg.ModelsToInstall()
	if len(models) != 1 || models[0] != "a" {
		t.Errorf("ModelsToInstall = %v, want [a]", models)
	}
}

// ---------------------------------------------------------------------------
// AutoConfigureModels — boundary RAM values
// ---------------------------------------------------------------------------

func TestAutoConfigureModels_ExactlyAt16GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(16)
	if cfg.ModelEnrich != "qwen3.5:2b" {
		t.Errorf("16GB ModelEnrich = %q, want qwen3.5:2b", cfg.ModelEnrich)
	}
}

func TestAutoConfigureModels_ExactlyAt24GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(24)
	// 24 <= 24, so still mid tier.
	if cfg.ModelEnrich != "qwen3.5:4b" {
		t.Errorf("24GB ModelEnrich = %q, want qwen3.5:4b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:4b" {
		t.Errorf("24GB ModelOrchestrate = %q, want qwen3.5:4b", cfg.ModelOrchestrate)
	}
}

func TestAutoConfigureModels_ExactlyAt25GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(25)
	// > 24, so high tier.
	if cfg.ModelOrchestrate != "qwen3.5:9b" {
		t.Errorf("25GB ModelOrchestrate = %q, want qwen3.5:9b", cfg.ModelOrchestrate)
	}
}
