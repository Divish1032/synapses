package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain/config"
)

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
