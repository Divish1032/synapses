package security

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// builtinPatternFiles contains the built-in security pattern JSON files embedded
// at compile time. Files are located in the builtin/ subdirectory.
//
//go:embed builtin/*.json
var builtinPatternFiles embed.FS

// patternFile is the on-disk JSON format. A single file may contain one pattern
// or an array of patterns.
type patternFile struct {
	// Single pattern (if the root JSON object is a pattern).
	single *SecurityPattern
	// Multiple patterns (if the root JSON object is an array or {"patterns": [...]}).
	multi []SecurityPattern
}

// patternFileMulti is the envelope format for a JSON file containing multiple patterns.
type patternFileMulti struct {
	Patterns []SecurityPattern `json:"patterns"`
}

// loadPatternFile parses a JSON byte slice into zero or more SecurityPatterns.
// Supports three formats:
//  1. A single SecurityPattern object: {"id": "...", "name": "...", ...}
//  2. An array of SecurityPattern objects: [{"id": "..."}, ...]
//  3. An envelope object: {"patterns": [{"id": "..."}, ...]}
func loadPatternFile(data []byte, source string) ([]SecurityPattern, error) {
	if len(data) == 0 {
		return nil, nil
	}

	// Detect format by the first non-whitespace character.
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) == 0 {
		return nil, nil
	}

	switch trimmed[0] {
	case '[':
		// Array format.
		var patterns []SecurityPattern
		if err := json.Unmarshal(data, &patterns); err != nil {
			return nil, fmt.Errorf("parse %s (array format): %w", source, err)
		}
		return patterns, nil

	case '{':
		// Could be a single pattern or an envelope {"patterns": [...]}.
		// Peek for the "patterns" key.
		var envelope patternFileMulti
		if err := json.Unmarshal(data, &envelope); err == nil && len(envelope.Patterns) > 0 {
			return envelope.Patterns, nil
		}
		// Try single-pattern format.
		var p SecurityPattern
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("parse %s (single-pattern format): %w", source, err)
		}
		if p.ID == "" {
			return nil, fmt.Errorf("parse %s: JSON object has no 'id' field — not a valid SecurityPattern or envelope", source)
		}
		return []SecurityPattern{p}, nil

	default:
		return nil, fmt.Errorf("parse %s: unexpected JSON start character %q", source, trimmed[0])
	}
}

// validateAndFilter validates each pattern in patterns.
// Returns error on first invalid pattern (missing required fields, bad enum values).
// Disabled patterns (enabled:false) are retained — filtering by enabled state is
// the responsibility of PatternSet query methods (All, ForLanguage, etc.).
func validateAndFilter(patterns []SecurityPattern, source string) ([]SecurityPattern, error) {
	out := make([]SecurityPattern, 0, len(patterns))
	for i, p := range patterns {
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("%s pattern[%d]: %w", source, i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// LoadBuiltin loads all patterns embedded in the binary at compile time.
// These are the built-in patterns shipped with Synapses. Returns an error
// only if an embedded file is malformed — this should never happen in a
// correctly built binary.
func LoadBuiltin() (*PatternSet, error) {
	var allPatterns []SecurityPattern

	err := fs.WalkDir(builtinPatternFiles, "builtin", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".json") {
			return nil
		}

		data, readErr := builtinPatternFiles.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read embedded %s: %w", path, readErr)
		}

		patterns, parseErr := loadPatternFile(data, path)
		if parseErr != nil {
			return parseErr
		}

		validated, validErr := validateAndFilter(patterns, path)
		if validErr != nil {
			return validErr
		}

		allPatterns = append(allPatterns, validated...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load built-in patterns: %w", err)
	}

	return newPatternSet(allPatterns), nil
}

// LoadDir loads all JSON pattern files from a directory on disk.
// Returns an empty PatternSet (not error) if the directory does not exist.
// Returns an error if the directory exists but a file is malformed or invalid.
func LoadDir(dir string) (*PatternSet, error) {
	if dir == "" {
		return newPatternSet(nil), nil
	}

	// Non-existent dir is not an error — it just means no user patterns.
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return newPatternSet(nil), nil
	}

	var allPatterns []SecurityPattern

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".json") {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}

		patterns, parseErr := loadPatternFile(data, path)
		if parseErr != nil {
			return parseErr
		}

		validated, validErr := validateAndFilter(patterns, path)
		if validErr != nil {
			return validErr
		}

		allPatterns = append(allPatterns, validated...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load patterns from %s: %w", dir, err)
	}

	return newPatternSet(allPatterns), nil
}

// LoadAll loads built-in patterns and merges patterns from each extraDir.
// User patterns from extraDirs override built-in patterns when they share an ID.
// The override model: last pattern with a given ID wins (extraDirs applied in order).
//
// This means a user can:
//   - Disable a built-in pattern: add a file with {"id": "go-chi-missing-auth", "enabled": false, ...}
//   - Override severity: add a file with the same ID and a different severity
//   - Add new patterns: add files with unique IDs
//
// Empty strings in extraDirs are silently skipped.
func LoadAll(extraDirs ...string) (*PatternSet, error) {
	builtin, err := LoadBuiltin()
	if err != nil {
		return nil, err
	}

	// Start with built-in patterns, indexed by ID for override resolution.
	byID := make(map[string]SecurityPattern, builtin.Len())
	// Track insertion order to preserve deterministic output.
	order := make([]string, 0, builtin.Len())
	for _, p := range builtin.patterns {
		if _, exists := byID[p.ID]; !exists {
			order = append(order, p.ID)
		}
		byID[p.ID] = p
	}

	// Apply user patterns from each extra directory.
	for _, dir := range extraDirs {
		if dir == "" {
			continue
		}
		userSet, loadErr := LoadDir(dir)
		if loadErr != nil {
			return nil, loadErr
		}
		for _, p := range userSet.patterns {
			if _, exists := byID[p.ID]; !exists {
				order = append(order, p.ID)
			}
			byID[p.ID] = p // override / add
		}
	}

	// Reconstruct ordered slice.
	merged := make([]SecurityPattern, 0, len(order))
	for _, id := range order {
		merged = append(merged, byID[id])
	}

	return newPatternSet(merged), nil
}
