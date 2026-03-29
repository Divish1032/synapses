package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// GlobalConfig represents the subset of Config fields that can be set globally
// in ~/.synapses/config.json. Project-level synapses.json always wins for any
// key that is explicitly present.
type GlobalConfig struct {
	Version string `json:"version,omitempty"`

	// Embedding fields
	EmbeddingEndpoint string `json:"embedding_endpoint,omitempty"`
	Embeddings        string `json:"embeddings,omitempty"`
	EmbeddingModel    string `json:"embedding_model,omitempty"`
	EmbedPoolSize     int    `json:"embed_pool_size,omitempty"`

	// Sub-configs that can have global defaults
	Brain         BrainConfig         `json:"brain,omitempty"`
	Pulse         PulseConfig         `json:"pulse,omitempty"`
	Session       SessionConfig       `json:"session,omitempty"`
	RateLimits    RateLimitConfig     `json:"rate_limits,omitempty"`
	ContentSafety ContentSafetyConfig `json:"content_safety,omitempty"`
	Recall        RecallConfig        `json:"recall,omitempty"`
}

// GlobalConfigPath returns the path to the global config file.
func GlobalConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, ".synapses", "config.json"), nil
}

// LoadGlobalConfig reads and parses ~/.synapses/config.json.
// Returns nil with no error if the file does not exist.
func LoadGlobalConfig() (*GlobalConfig, error) {
	path, err := GlobalConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read global config %s: %w", path, err)
	}
	var gc GlobalConfig
	if err := json.Unmarshal(data, &gc); err != nil {
		return nil, fmt.Errorf("parse global config %s: %w — fix the JSON or remove the file", path, err)
	}
	return &gc, nil
}

// SaveGlobalConfig writes the global config to ~/.synapses/config.json.
// Creates the directory if it does not exist.
func SaveGlobalConfig(gc *GlobalConfig) error {
	path, err := GlobalConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(gc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal global config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write global config %s: %w", path, err)
	}
	return nil
}

// globalConfigKeys are the top-level JSON keys that can appear in the global config.
var globalConfigKeys = map[string]bool{
	"version":            true,
	"embedding_endpoint": true,
	"embeddings":         true,
	"embedding_model":    true,
	"embed_pool_size":    true,
	"brain":              true,
	"pulse":              true,
	"session":            true,
	"rate_limits":        true,
	"content_safety":     true,
	"recall":             true,
}

// mergeGlobalConfig applies global config defaults to cfg for any globalizable
// key not explicitly present in the project's raw JSON. projectRawKeys is the
// set of top-level keys found in the project's synapses.json (nil when no
// project config file exists, meaning all global values should apply).
func mergeGlobalConfig(cfg *Config, projectRawKeys map[string]bool) {
	gc, err := LoadGlobalConfig()
	if err != nil {
		logutil.Warn("global config: %v", err)
		return
	}
	if gc == nil {
		return // no global config file
	}

	// Helper: returns true if the project explicitly set this key.
	has := func(key string) bool {
		if projectRawKeys == nil {
			return false // no project config → global applies to everything
		}
		return projectRawKeys[key]
	}

	// Embedding fields
	if !has("embedding_endpoint") && gc.EmbeddingEndpoint != "" {
		cfg.EmbeddingEndpoint = gc.EmbeddingEndpoint
	}
	if !has("embeddings") && gc.Embeddings != "" {
		cfg.Embeddings = gc.Embeddings
	}
	if !has("embedding_model") && gc.EmbeddingModel != "" {
		cfg.EmbeddingModel = gc.EmbeddingModel
	}
	if !has("embed_pool_size") && gc.EmbedPoolSize != 0 {
		cfg.EmbedPoolSize = gc.EmbedPoolSize
	}

	// Brain
	if !has("brain") {
		cfg.Brain = gc.Brain
	}

	// Pulse
	if !has("pulse") {
		cfg.Pulse = gc.Pulse
	}

	// Session
	if !has("session") {
		cfg.Session = gc.Session
	}

	// RateLimits
	if !has("rate_limits") {
		cfg.RateLimits = gc.RateLimits
	}

	// ContentSafety
	if !has("content_safety") {
		cfg.ContentSafety = gc.ContentSafety
	}

	// Recall
	if !has("recall") {
		cfg.Recall = gc.Recall
	}
}

// extractRawKeys parses raw JSON and returns the set of top-level keys present.
func extractRawKeys(data []byte) map[string]bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	keys := make(map[string]bool, len(raw))
	for k := range raw {
		keys[k] = true
	}
	return keys
}
