// projects.go — persist and reload known project paths across daemon restarts.
//
// Stores a simple JSON array of absolute paths in ~/.synapses/projects.json.
// On daemon startup, known projects are warmed eagerly in the background so
// the first MCP request doesn't block on graph indexing.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

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

// loadKnownProjects reads the list of previously registered project paths.
// Returns nil on any error (file missing, corrupt JSON, etc.).
func loadKnownProjects() []string {
	p, err := knownProjectsPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil
	}
	return paths
}

// saveKnownProject appends a project path to the persisted list (idempotent).
// maxKnownProjects caps the persisted project list to prevent unbounded growth.
const maxKnownProjects = 100

func saveKnownProject(absPath string) {
	knownProjectsMu.Lock()
	defer knownProjectsMu.Unlock()

	existing := loadKnownProjects()
	for _, p := range existing {
		if p == absPath {
			return // already known
		}
	}
	existing = append(existing, absPath)
	// Evict oldest entries if over cap.
	if len(existing) > maxKnownProjects {
		existing = existing[len(existing)-maxKnownProjects:]
	}
	writeKnownProjects(existing)
}

// removeKnownProject removes a project path from the persisted list.
func removeKnownProject(absPath string) {
	knownProjectsMu.Lock()
	defer knownProjectsMu.Unlock()

	existing := loadKnownProjects()
	filtered := existing[:0]
	for _, p := range existing {
		if p != absPath {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) != len(existing) {
		writeKnownProjects(filtered)
	}
}

// writeKnownProjects writes the full list atomically.
func writeKnownProjects(paths []string) {
	p, err := knownProjectsPath()
	if err != nil {
		return
	}
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	// Write to an unpredictable temp file (os.CreateTemp) then rename for
	// atomicity. Using a predictable name like p+".tmp" would allow a local
	// attacker to pre-create a symlink and redirect the write.
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
