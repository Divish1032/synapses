// Package federation provides cross-project dependency tracking and drift detection.
package federation

import (
	"os"
	"path/filepath"
	"time"
)

// DiscoveredSibling represents a potential federation entry found by scanning
// the parent directory for other Synapses-indexed projects.
type DiscoveredSibling struct {
	Path string `json:"path"`
	Name string `json:"name"` // directory basename
	Hint string `json:"hint"` // how it was detected
}

// DiscoverSiblings scans the parent directory of projectRoot for other directories
// that appear to be Synapses-indexed projects (contain synapses.json or .synapses/).
// Returns suggestions only — never auto-adds to federation config.
// Skips the current project. Timeout: 500ms max. Depth: 1 level only.
func DiscoverSiblings(projectRoot string) []DiscoveredSibling {
	parent := filepath.Dir(projectRoot)
	if parent == projectRoot {
		return nil // at filesystem root
	}
	currentName := filepath.Base(projectRoot)

	deadline := time.Now().Add(500 * time.Millisecond)

	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}

	var siblings []DiscoveredSibling
	for _, e := range entries {
		if time.Now().After(deadline) {
			break
		}
		if !e.IsDir() || e.Name() == currentName {
			continue
		}
		// Skip hidden directories.
		if e.Name()[0] == '.' {
			continue
		}

		dirPath := filepath.Join(parent, e.Name())
		hint := ""

		// Check for synapses.json.
		if _, err := os.Stat(filepath.Join(dirPath, "synapses.json")); err == nil {
			hint = "has synapses.json"
		}
		// Check for .synapses/ cache directory.
		if hint == "" {
			if info, err := os.Stat(filepath.Join(dirPath, ".synapses")); err == nil && info.IsDir() {
				hint = "has .synapses/ cache"
			}
		}

		if hint != "" {
			siblings = append(siblings, DiscoveredSibling{
				Path: dirPath,
				Name: e.Name(),
				Hint: hint,
			})
		}
	}
	return siblings
}
