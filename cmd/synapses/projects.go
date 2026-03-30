// projects.go — persist and reload known project paths across daemon restarts.
//
// Stores project info in ~/.synapses/projects.json. Supports two formats:
//   - v1 (legacy): plain JSON array of absolute paths ["path1", "path2"]
//   - v2 (current): {version: 2, projects: [{path, state, hibernated_at}]}
//
// On daemon startup, known projects are warmed eagerly (if state="warm") or
// loaded as tombstones (if state="hibernated") so the first MCP request
// doesn't block on graph indexing.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// knownProjectsMu guards concurrent writes to projects.json.
var knownProjectsMu sync.Mutex

// knownProjectsPath returns the path to ~/.synapses/projects.json.
func knownProjectsPath() (string, error) {
	home, err := synapsesHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "projects.json"), nil
}

// projectEntry represents one project in the v2 projects.json format.
type projectEntry struct {
	Path         string `json:"path"`
	State        string `json:"state"`                    // "warm" or "hibernated"
	HibernatedAt string `json:"hibernated_at,omitempty"`  // RFC3339, set when hibernated
}

// projectsFile is the v2 projects.json format.
type projectsFile struct {
	Version  int            `json:"version"`
	Projects []projectEntry `json:"projects"`
}

// loadKnownProjects reads the list of previously registered project paths.
// Returns nil on any error (file missing, corrupt JSON, etc.).
// Backward-compatible: reads both v1 (array) and v2 (object) formats.
func loadKnownProjects() []string {
	entries := loadProjectEntries()
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return paths
}

// loadProjectEntries reads the v2 project entries, auto-migrating from v1.
func loadProjectEntries() []projectEntry {
	p, err := knownProjectsPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}

	// Try v2 format first.
	var v2 projectsFile
	if err := json.Unmarshal(data, &v2); err == nil && v2.Version >= 2 {
		return v2.Projects
	}

	// Fall back to v1 format (plain array).
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil
	}
	// Auto-migrate: all v1 projects are "warm" by default.
	entries := make([]projectEntry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, projectEntry{Path: path, State: "warm"})
	}
	// Persist migration.
	writeProjectEntries(entries)
	return entries
}

// loadKnownProjectsWithState returns entries with their hibernate state,
// used by the daemon startup to decide warm init vs tombstone creation.
func loadKnownProjectsWithState() []projectEntry {
	return loadProjectEntries()
}

// maxKnownProjects caps the persisted project list to prevent unbounded growth.
const maxKnownProjects = 100

// saveKnownProject appends a project path to the persisted list (idempotent).
func saveKnownProject(absPath string) {
	knownProjectsMu.Lock()
	defer knownProjectsMu.Unlock()

	existing := loadProjectEntries()
	for _, e := range existing {
		if e.Path == absPath {
			return // already known
		}
	}
	existing = append(existing, projectEntry{Path: absPath, State: "warm"})
	// Evict oldest entries if over cap.
	if len(existing) > maxKnownProjects {
		existing = existing[len(existing)-maxKnownProjects:]
	}
	writeProjectEntries(existing)
}

// removeKnownProject removes a project path from the persisted list.
func removeKnownProject(absPath string) {
	knownProjectsMu.Lock()
	defer knownProjectsMu.Unlock()

	existing := loadProjectEntries()
	filtered := existing[:0]
	for _, e := range existing {
		if e.Path != absPath {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) != len(existing) {
		writeProjectEntries(filtered)
	}
}

// updateKnownProjectState updates the hibernate state for a project.
// state should be "warm" or "hibernated".
func updateKnownProjectState(absPath, state string) {
	knownProjectsMu.Lock()
	defer knownProjectsMu.Unlock()

	existing := loadProjectEntries()
	found := false
	for i := range existing {
		if existing[i].Path == absPath {
			existing[i].State = state
			if state == "hibernated" {
				existing[i].HibernatedAt = time.Now().UTC().Format(time.RFC3339)
			} else {
				existing[i].HibernatedAt = ""
			}
			found = true
			break
		}
	}
	if !found {
		entry := projectEntry{Path: absPath, State: state}
		if state == "hibernated" {
			entry.HibernatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		existing = append(existing, entry)
	}
	writeProjectEntries(existing)
}

// writeProjectEntries writes the full project list in v2 format atomically.
func writeProjectEntries(entries []projectEntry) {
	p, err := knownProjectsPath()
	if err != nil {
		return
	}
	v2 := projectsFile{
		Version:  2,
		Projects: entries,
	}
	data, err := json.MarshalIndent(v2, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	dir := filepath.Dir(p)
	f, err := os.CreateTemp(dir, ".projects-*.tmp")
	if err != nil {
		logutil.Warn("projects.json: create temp failed: %v\n", err)
		return
	}
	tmp := f.Name()
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		logutil.Warn("projects.json: write failed: %v %v\n", writeErr, closeErr)
		os.Remove(tmp)
		return
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		logutil.Warn("projects.json: rename failed: %v\n", err)
		os.Remove(tmp)
	}
}

// writeKnownProjects is kept for any legacy callers but now writes v2 format.
func writeKnownProjects(paths []string) {
	entries := make([]projectEntry, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, projectEntry{Path: p, State: "warm"})
	}
	writeProjectEntries(entries)
}
