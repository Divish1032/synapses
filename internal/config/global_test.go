package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGlobalConfig_NotExists(t *testing.T) {
	// Point HOME to an empty temp dir so no global config is found.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	gc, err := LoadGlobalConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gc != nil {
		t.Fatalf("expected nil, got %+v", gc)
	}
}

func TestLoadGlobalConfig_Valid(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir := filepath.Join(tmp, ".synapses")
	os.MkdirAll(dir, 0o755)

	gc := &GlobalConfig{
		Version:    "1",
		Embeddings: "builtin",
		Brain:      BrainConfig{Enabled: true, OllamaURL: "http://localhost:11434"},
		Pulse:      PulseConfig{URL: "https://pulse.example.com", TimeoutSec: 2},
		Session:    SessionConfig{AutoEndThresholdCalls: 80, ReconnectWindowSecs: 300},
	}
	data, _ := json.Marshal(gc)
	os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644)

	loaded, err := LoadGlobalConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil global config")
	}
	if loaded.Embeddings != "builtin" {
		t.Errorf("Embeddings = %q, want %q", loaded.Embeddings, "builtin")
	}
	if !loaded.Brain.Enabled {
		t.Error("Brain.Enabled = false, want true")
	}
	if loaded.Pulse.URL != "https://pulse.example.com" {
		t.Errorf("Pulse.URL = %q, want %q", loaded.Pulse.URL, "https://pulse.example.com")
	}
	if loaded.Session.AutoEndThresholdCalls != 80 {
		t.Errorf("Session.AutoEndThresholdCalls = %d, want 80", loaded.Session.AutoEndThresholdCalls)
	}
}

func TestLoadGlobalConfig_MalformedJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir := filepath.Join(tmp, ".synapses")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{bad json"), 0o644)

	_, err := LoadGlobalConfig()
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestSaveGlobalConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	gc := &GlobalConfig{
		Version:    "1",
		Embeddings: "ollama",
		Brain:      BrainConfig{Enabled: true, Model: "qwen3.5:2b"},
	}
	if err := SaveGlobalConfig(gc); err != nil {
		t.Fatalf("SaveGlobalConfig: %v", err)
	}

	loaded, err := LoadGlobalConfig()
	if err != nil {
		t.Fatalf("LoadGlobalConfig: %v", err)
	}
	if loaded.Embeddings != "ollama" {
		t.Errorf("Embeddings = %q, want %q", loaded.Embeddings, "ollama")
	}
	if loaded.Brain.Model != "qwen3.5:2b" {
		t.Errorf("Brain.Model = %q, want %q", loaded.Brain.Model, "qwen3.5:2b")
	}
}

func TestMergeGlobalConfig_NoProjectKeys(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir := filepath.Join(tmp, ".synapses")
	os.MkdirAll(dir, 0o755)

	globalData := `{
		"embeddings": "builtin",
		"brain": {"enabled": true, "model": "qwen3.5:2b"},
		"pulse": {"url": "https://pulse.test"},
		"session": {"auto_end_threshold_calls": 80},
		"rate_limits": {"write_ops_per_minute": 20},
		"content_safety": {"mode": "warn"}
	}`
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(globalData), 0o644)

	cfg := defaultConfig()
	mergeGlobalConfig(cfg, nil) // nil = no project config

	if cfg.Embeddings != "builtin" {
		t.Errorf("Embeddings = %q, want %q", cfg.Embeddings, "builtin")
	}
	if !cfg.Brain.Enabled {
		t.Error("Brain.Enabled = false, want true")
	}
	if cfg.Pulse.URL != "https://pulse.test" {
		t.Errorf("Pulse.URL = %q, want %q", cfg.Pulse.URL, "https://pulse.test")
	}
	if cfg.Session.AutoEndThresholdCalls != 80 {
		t.Errorf("Session.AutoEndThresholdCalls = %d, want 80", cfg.Session.AutoEndThresholdCalls)
	}
	if cfg.RateLimits.WriteOpsPerMinute != 20 {
		t.Errorf("RateLimits.WriteOpsPerMinute = %d, want 20", cfg.RateLimits.WriteOpsPerMinute)
	}
}

func TestMergeGlobalConfig_ProjectOverrides(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir := filepath.Join(tmp, ".synapses")
	os.MkdirAll(dir, 0o755)

	globalData := `{
		"embeddings": "builtin",
		"brain": {"enabled": true, "model": "qwen3.5:2b"},
		"pulse": {"url": "https://global-pulse"}
	}`
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(globalData), 0o644)

	cfg := defaultConfig()
	cfg.Embeddings = "off"
	cfg.Brain = BrainConfig{Enabled: false}

	// Project explicitly set "embeddings" and "brain" but not "pulse"
	projectKeys := map[string]bool{
		"embeddings": true,
		"brain":      true,
	}
	mergeGlobalConfig(cfg, projectKeys)

	// Project values should win
	if cfg.Embeddings != "off" {
		t.Errorf("Embeddings = %q, want %q (project should win)", cfg.Embeddings, "off")
	}
	if cfg.Brain.Enabled {
		t.Error("Brain.Enabled = true, want false (project should win)")
	}
	// Global should fill in pulse (not in project)
	if cfg.Pulse.URL != "https://global-pulse" {
		t.Errorf("Pulse.URL = %q, want %q (global should fill)", cfg.Pulse.URL, "https://global-pulse")
	}
}

func TestMergeGlobalConfig_EmptyGlobal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	dir := filepath.Join(tmp, ".synapses")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644)

	cfg := defaultConfig()
	cfg.Embeddings = "ollama"
	mergeGlobalConfig(cfg, map[string]bool{"embeddings": true})

	if cfg.Embeddings != "ollama" {
		t.Errorf("Embeddings = %q, want %q", cfg.Embeddings, "ollama")
	}
}

func TestMergeGlobalConfig_NoGlobalFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cfg := defaultConfig()
	mergeGlobalConfig(cfg, nil) // should not panic
}

func TestExtractRawKeys(t *testing.T) {
	data := []byte(`{"brain": {}, "pulse": {"url": "x"}, "version": "1"}`)
	keys := extractRawKeys(data)
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
	for _, k := range []string{"brain", "pulse", "version"} {
		if !keys[k] {
			t.Errorf("missing key %q", k)
		}
	}
}

func TestExtractRawKeys_Invalid(t *testing.T) {
	keys := extractRawKeys([]byte("not json"))
	if keys != nil {
		t.Errorf("expected nil for invalid JSON, got %v", keys)
	}
}

func TestLoadWithGlobalConfig_Integration(t *testing.T) {
	// Set up global config
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".synapses")
	os.MkdirAll(globalDir, 0o755)
	globalData := `{
		"brain": {"enabled": true, "ollama_url": "http://localhost:11434", "model": "qwen3.5:2b"},
		"session": {"auto_end_threshold_calls": 100}
	}`
	os.WriteFile(filepath.Join(globalDir, "config.json"), []byte(globalData), 0o644)

	// Set up project config that only sets embeddings
	projDir := t.TempDir()
	projData := `{"version": "1", "embeddings": "off"}`
	os.WriteFile(filepath.Join(projDir, "synapses.json"), []byte(projData), 0o644)

	cfg, err := Load(projDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Project should win for embeddings
	if cfg.Embeddings != "off" {
		t.Errorf("Embeddings = %q, want %q", cfg.Embeddings, "off")
	}
	// Global should fill in brain (not in project)
	if !cfg.Brain.Enabled {
		t.Error("Brain.Enabled = false, want true (from global)")
	}
	if cfg.Brain.OllamaURL != "http://localhost:11434" {
		t.Errorf("Brain.OllamaURL = %q, want from global", cfg.Brain.OllamaURL)
	}
	// Global should fill in session
	if cfg.Session.AutoEndThresholdCalls != 100 {
		t.Errorf("Session.AutoEndThresholdCalls = %d, want 100 (from global)", cfg.Session.AutoEndThresholdCalls)
	}
}

func TestLoadWithGlobalConfig_ConflictingKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".synapses")
	os.MkdirAll(globalDir, 0o755)
	globalData := `{"brain": {"enabled": true, "model": "global-model"}}`
	os.WriteFile(filepath.Join(globalDir, "config.json"), []byte(globalData), 0o644)

	projDir := t.TempDir()
	projData := `{"version": "1", "brain": {"enabled": false}}`
	os.WriteFile(filepath.Join(projDir, "synapses.json"), []byte(projData), 0o644)

	cfg, err := Load(projDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Project explicitly set brain → project wins entirely
	if cfg.Brain.Enabled {
		t.Error("Brain.Enabled = true, want false (project should win)")
	}
}

func TestLoadNoProjectConfig_GlobalApplies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".synapses")
	os.MkdirAll(globalDir, 0o755)
	globalData := `{"embeddings": "builtin", "brain": {"enabled": true}}`
	os.WriteFile(filepath.Join(globalDir, "config.json"), []byte(globalData), 0o644)

	// Load from a dir with no synapses.json
	projDir := t.TempDir()
	cfg, err := Load(projDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Embeddings != "builtin" {
		t.Errorf("Embeddings = %q, want %q (from global)", cfg.Embeddings, "builtin")
	}
	if !cfg.Brain.Enabled {
		t.Error("Brain.Enabled = false, want true (from global)")
	}
}
