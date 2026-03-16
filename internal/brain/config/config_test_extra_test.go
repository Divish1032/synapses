package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain/config"
)

// ---------------------------------------------------------------------------
// DefaultConfigPath
// ---------------------------------------------------------------------------

func TestDefaultConfigPath_EnvOverride(t *testing.T) {
	want := "/tmp/custom-brain.json"
	t.Setenv("BRAIN_CONFIG", want)

	got := config.DefaultConfigPath()
	if got != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", got, want)
	}
}

func TestDefaultConfigPath_DefaultLocation(t *testing.T) {
	// Ensure env var is not set.
	t.Setenv("BRAIN_CONFIG", "")

	got := config.DefaultConfigPath()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	want := filepath.Join(home, ".synapses", "brain.json")
	if got != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// SaveFile + LoadFile round-trip
// ---------------------------------------------------------------------------

func TestSaveAndLoadFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "brain.json")

	cfg := config.DefaultConfig()
	cfg.Enabled = true
	cfg.Port = 19999
	cfg.OllamaURL = "http://example.com:11434"

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile() error: %v", err)
	}

	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile() error: %v", err)
	}
	if loaded.Port != 19999 {
		t.Errorf("Port = %d, want 19999", loaded.Port)
	}
	if loaded.OllamaURL != "http://example.com:11434" {
		t.Errorf("OllamaURL = %q, want http://example.com:11434", loaded.OllamaURL)
	}
}

func TestLoadFile_MissingFile_ReturnsError(t *testing.T) {
	_, err := config.LoadFile("/nonexistent/path/brain.json")
	if err == nil {
		t.Error("LoadFile(missing) should return an error")
	}
}

func TestLoadFile_InvalidJSON_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := config.LoadFile(path)
	if err == nil {
		t.Error("LoadFile(invalid JSON) should return an error")
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — tested via partial JSON + LoadFile
// ---------------------------------------------------------------------------

// writePartialConfig writes a minimal JSON config to a temp file and returns its path.
func writePartialConfig(t *testing.T, v map[string]interface{}) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "brain.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestApplyDefaults_EmptyBackendFallsToOllama(t *testing.T) {
	// Write JSON with backend explicitly set to empty string.
	path := writePartialConfig(t, map[string]interface{}{"backend": ""})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Backend != "ollama" {
		t.Errorf("Backend = %q after applyDefaults, want %q", cfg.Backend, "ollama")
	}
}

func TestApplyDefaults_EmptyOllamaURLFallsToDefault(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"ollama_url": ""})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.OllamaURL != "http://localhost:11434" {
		t.Errorf("OllamaURL = %q after applyDefaults, want %q", cfg.OllamaURL, "http://localhost:11434")
	}
}

func TestApplyDefaults_EmptyModelFallsToDefault(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"model": ""})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Model != "qwen3.5:2b" {
		t.Errorf("Model = %q after applyDefaults, want %q", cfg.Model, "qwen3.5:2b")
	}
}

func TestApplyDefaults_ModelIngestFallsToFastModel(t *testing.T) {
	// Set fast_model to a custom value; leave model_ingest unset.
	path := writePartialConfig(t, map[string]interface{}{
		"fast_model":   "customfast:1b",
		"model_ingest": "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// ModelIngest should fall back to FastModel.
	if cfg.ModelIngest != "customfast:1b" {
		t.Errorf("ModelIngest = %q after applyDefaults, want %q", cfg.ModelIngest, "customfast:1b")
	}
}

func TestApplyDefaults_ModelArchivistDefaultsToQwen35(t *testing.T) {
	// Archivist always defaults to qwen3.5:2b independently of ModelOrchestrate.
	// Navigator and Archivist are base-model tiers; they must not inherit FT model tags.
	path := writePartialConfig(t, map[string]interface{}{
		"model_orchestrate": "myorch:3b",
		"model_archivist":   "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.ModelArchivist != "qwen3.5:2b" {
		t.Errorf("ModelArchivist = %q after applyDefaults, want %q", cfg.ModelArchivist, "qwen3.5:2b")
	}
}

func TestApplyDefaults_TimeoutMSZeroFallsToDefault(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"timeout_ms": 0})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.TimeoutMS != 60000 {
		t.Errorf("TimeoutMS = %d after applyDefaults, want 60000", cfg.TimeoutMS)
	}
}

func TestApplyDefaults_PortZeroFallsToDefault(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"port": 0})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Port != 11435 {
		t.Errorf("Port = %d after applyDefaults, want 11435", cfg.Port)
	}
}

func TestApplyDefaults_TildeExpansionInDBPath(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{"db_path": "~/test/brain.sqlite"})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if strings.HasPrefix(cfg.DBPath, "~/") {
		t.Errorf("DBPath = %q — tilde was not expanded", cfg.DBPath)
	}
	home, _ := os.UserHomeDir()
	wantPrefix := filepath.Join(home, "test")
	if !strings.HasPrefix(cfg.DBPath, wantPrefix) {
		t.Errorf("DBPath = %q, want prefix %q", cfg.DBPath, wantPrefix)
	}
}

func TestApplyDefaults_LocalBackendAutoComputesGGUFPath(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"backend":      "local",
		"gguf_path":    "",
		"model_dir":    "/tmp/models",
		"hf_filename":  "sil-coder.gguf",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := "/tmp/models/sil-coder.gguf"
	if cfg.GGUFPath != want {
		t.Errorf("GGUFPath = %q after applyDefaults, want %q", cfg.GGUFPath, want)
	}
}

func TestApplyDefaults_EmbeddingEnabledAutoComputesEmbedModelPath(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"embedding_enabled": true,
		"embed_model_path":  "",
		"model_dir":         "/tmp/models",
		"embed_hf_filename": "nomic-embed.gguf",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	want := "/tmp/models/nomic-embed.gguf"
	if cfg.EmbedModelPath != want {
		t.Errorf("EmbedModelPath = %q after applyDefaults, want %q", cfg.EmbedModelPath, want)
	}
}

// ---------------------------------------------------------------------------
// AutoConfigureModels — additional coverage beyond config_test.go
// ---------------------------------------------------------------------------

func TestAutoConfigureModels_8GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(8)
	if cfg.ModelEnrich != "qwen3.5:2b" {
		t.Errorf("8GB ModelEnrich = %q, want qwen3.5:2b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:2b" {
		t.Errorf("8GB ModelOrchestrate = %q, want qwen3.5:2b", cfg.ModelOrchestrate)
	}
}

func TestAutoConfigureModels_16GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(16)
	if cfg.ModelEnrich != "qwen3.5:2b" {
		t.Errorf("16GB ModelEnrich = %q, want qwen3.5:2b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:2b" {
		t.Errorf("16GB ModelOrchestrate = %q, want qwen3.5:2b", cfg.ModelOrchestrate)
	}
}

func TestAutoConfigureModels_20GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(20)
	if cfg.ModelEnrich != "qwen3.5:4b" {
		t.Errorf("20GB ModelEnrich = %q, want qwen3.5:4b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:4b" {
		t.Errorf("20GB ModelOrchestrate = %q, want qwen3.5:4b", cfg.ModelOrchestrate)
	}
}

func TestAutoConfigureModels_30GB(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(30)
	if cfg.ModelEnrich != "qwen3.5:4b" {
		t.Errorf("30GB ModelEnrich = %q, want qwen3.5:4b", cfg.ModelEnrich)
	}
	if cfg.ModelOrchestrate != "qwen3.5:9b" {
		t.Errorf("30GB ModelOrchestrate = %q, want qwen3.5:9b", cfg.ModelOrchestrate)
	}
}

// ---------------------------------------------------------------------------
// ModelsToInstall — deduplication and sorting
// ---------------------------------------------------------------------------

func TestModelsToInstall_Dedup(t *testing.T) {
	cfg := config.DefaultConfig()
	// All tiers at same model — result must have exactly 1 entry.
	cfg.AutoConfigureModels(4)
	models := cfg.ModelsToInstall()
	// With 4GB, all tiers use qwen3.5:2b, so only 1 unique model (archivist defaults to orchestrate).
	if len(models) != 1 {
		t.Errorf("ModelsToInstall() returned %d models (%v), want 1", len(models), models)
	}
	if models[0] != "qwen3.5:2b" {
		t.Errorf("ModelsToInstall()[0] = %q, want qwen3.5:2b", models[0])
	}
}

func TestModelsToInstall_Sorted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AutoConfigureModels(20) // enrich/orch → 4b, rest → 2b
	models := cfg.ModelsToInstall()
	for i := 1; i < len(models); i++ {
		if models[i] < models[i-1] {
			t.Errorf("ModelsToInstall() not sorted: %v", models)
		}
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — "auto" model trigger path
// ---------------------------------------------------------------------------

func TestApplyDefaults_AutoModelTriggersAutoConfigureModels(t *testing.T) {
	// Write a config file with model_ingest="auto" then LoadFile triggers applyDefaults
	// which calls AutoConfigureModels(0) when any tier model is "auto".
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"model_ingest":"auto","backend":"ollama"}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// After AutoConfigureModels(0) with 0 GB RAM, models should no longer be "auto".
	if cfg.ModelIngest == "auto" {
		t.Error("expected ModelIngest to be resolved from 'auto', still 'auto'")
	}
}

// ---------------------------------------------------------------------------
// SaveFile — MkdirAll error path
// ---------------------------------------------------------------------------

func TestSaveFile_MkdirAllError(t *testing.T) {
	// Use a path whose parent is a file (not a directory), so MkdirAll fails.
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "notadir/sub/config.json" — MkdirAll fails because "notadir" is a file
	err := config.SaveFile(filepath.Join(file, "sub", "config.json"), config.BrainConfig{})
	if err == nil {
		t.Fatal("expected error when MkdirAll fails, got nil")
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — comprehensive zero-value overrides (covers remaining branches)
// All fields set to "" via JSON override DefaultConfig() values.
// ---------------------------------------------------------------------------

func TestApplyDefaults_AllZeroOverrides(t *testing.T) {
	// Explicitly zero out fields that DefaultConfig() pre-fills.
	// json.Unmarshal overwrites them → applyDefaults sets defaults.
	path := writePartialConfig(t, map[string]interface{}{
		"backend":            "ollama",
		"fast_model":         "",
		"model_guardian":     "",
		"model_enrich":       "",
		"model_orchestrate":  "",
		"db_path":            "",
		"default_phase":      "",
		"default_mode":       "",
		"model_dir":          "",
		"hf_filename":        "",
		"embed_hf_repo":      "",
		"embed_hf_filename":  "",
		"embed_port":         0,
		"llama_bin_dir":      "",
		"llama_cpp_version":  "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	// All should be filled in by applyDefaults.
	if cfg.FastModel == "" {
		t.Error("expected FastModel to be set")
	}
	if cfg.ModelGuardian == "" {
		t.Error("expected ModelGuardian to be set")
	}
	if cfg.ModelEnrich == "" {
		t.Error("expected ModelEnrich to be set")
	}
	if cfg.ModelOrchestrate == "" {
		t.Error("expected ModelOrchestrate to be set")
	}
	if cfg.DBPath == "" {
		t.Error("expected DBPath to be set")
	}
	if cfg.DefaultPhase == "" {
		t.Error("expected DefaultPhase to be set")
	}
	if cfg.DefaultMode == "" {
		t.Error("expected DefaultMode to be set")
	}
	if cfg.ModelDir == "" {
		t.Error("expected ModelDir to be set")
	}
	if cfg.HFFilename == "" {
		t.Error("expected HFFilename to be set")
	}
	if cfg.EmbedHFRepo == "" {
		t.Error("expected EmbedHFRepo to be set")
	}
	if cfg.EmbedHFFilename == "" {
		t.Error("expected EmbedHFFilename to be set")
	}
	if cfg.EmbedPort == 0 {
		t.Error("expected EmbedPort to be set")
	}
	if cfg.LlamaBinDir == "" {
		t.Error("expected LlamaBinDir to be set")
	}
	if cfg.LlamaCPPVersion == "" {
		t.Error("expected LlamaCPPVersion to be set")
	}
}

// ---------------------------------------------------------------------------
// applyDefaults — local backend with all defaults zeroed → sets HFRepo
// ---------------------------------------------------------------------------

func TestApplyDefaults_LocalBackend_SetsHFRepo(t *testing.T) {
	path := writePartialConfig(t, map[string]interface{}{
		"backend": "local",
		"hf_repo": "",
	})
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.HFRepo == "" {
		t.Error("expected HFRepo to be set for local backend")
	}
}
