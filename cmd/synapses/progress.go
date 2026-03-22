// progress.go — per-project indexing progress tracking.
//
// IndexingState is created per project (not a global singleton) and registered
// in activeIndexes for the duration of a full reindex. The /api/admin/health
// endpoint calls ActiveSnapshot() to surface live progress to callers.
//
// Cross-process visibility: when `synapses index` runs as a CLI subprocess
// (separate from the daemon), its progress is written to
// ~/.synapses/indexing-progress.json. The daemon's ActiveSnapshot() reads
// that file as a fallback when no in-process indexer is active, making the
// daemon's health endpoint accurate even for CLI-initiated indexing.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// indexingPhase is the lifecycle state of one indexing pass.
type indexingPhase int

const (
	indexingPhaseIdle   indexingPhase = 0
	indexingPhaseActive indexingPhase = 1
	indexingPhaseReady  indexingPhase = 2
)

// IndexingState tracks progress for one full reindex pass.
// All methods are safe for concurrent use.
type IndexingState struct {
	mu         sync.Mutex
	phase      indexingPhase
	filesDone  int64
	filesTotal int64
	label      string // optional phase label shown after file parsing (e.g. "Resolving edges…")
}

// Start records the real total file count (known after Phase 1 filesystem scan)
// and transitions the state to active. Must be called before SetDone.
func (s *IndexingState) Start(total int64) {
	s.mu.Lock()
	s.phase = indexingPhaseActive
	s.filesTotal = total
	s.filesDone = 0
	s.mu.Unlock()
	writeProgressFile(s.Snapshot())
}

// SetDone updates the count of files parsed so far.
func (s *IndexingState) SetDone(done int64) {
	s.mu.Lock()
	s.filesDone = done
	s.mu.Unlock()
	writeProgressFile(s.Snapshot())
}

// SetLabel sets a human-readable phase label (e.g. "Resolving edges…") that the
// frontend displays after file parsing is complete but before the state transitions
// to ready. This prevents the progress bar from showing 100% while post-parse
// work (edge resolution, cache saves) is still ongoing.
func (s *IndexingState) SetLabel(label string) {
	s.mu.Lock()
	s.label = label
	s.mu.Unlock()
	writeProgressFile(s.Snapshot())
}

// Done transitions the state to ready (indexing complete).
func (s *IndexingState) Done() {
	s.mu.Lock()
	s.phase = indexingPhaseReady
	s.mu.Unlock()
	writeProgressFile(s.Snapshot())
}

// Snapshot returns a consistent, point-in-time view of all fields.
// All values are read under the same lock, so callers never observe
// a mixed state (e.g. phase=ready but filesDone < filesTotal).
//
// While still in the active phase, pct is capped at 99 so the progress bar
// never shows 100% while post-parse work (edge resolution, cache saves) is
// ongoing. It only reaches 100 when state transitions to "ready".
func (s *IndexingState) Snapshot() IndexingSnapshot {
	s.mu.Lock()
	phase := s.phase
	done := s.filesDone
	total := s.filesTotal
	label := s.label
	s.mu.Unlock()

	var pct int64
	if total > 0 {
		pct = done * 100 / total
	}
	stateStr := "idle"
	switch phase {
	case indexingPhaseActive:
		stateStr = "indexing"
		if pct > 99 {
			pct = 99 // reserve 100% for the ready transition
		}
	case indexingPhaseReady:
		stateStr = "ready"
		pct = 100
	}
	return IndexingSnapshot{
		State: stateStr,
		Done:  done,
		Total: total,
		Pct:   pct,
		Label: label,
	}
}

// IndexingSnapshot is the JSON-serialisable view returned by the health endpoint.
type IndexingSnapshot struct {
	State string `json:"state"`        // "idle" | "indexing" | "ready"
	Done  int64  `json:"files_done"`
	Total int64  `json:"files_total"`
	Pct   int64  `json:"pct"`          // 0–100; capped at 99 while still indexing
	Label string `json:"label,omitempty"` // phase label shown after file parsing (e.g. "Resolving edges…")
}

// activeIndexes maps absPath → *IndexingState for all projects currently
// performing a full reindex. Using sync.Map so reads (health endpoint) never
// block writers (indexing goroutines).
var activeIndexes sync.Map // string → *IndexingState

// RegisterIndexing creates and registers an IndexingState for absPath.
// The caller must call UnregisterIndexing(absPath) when indexing completes
// or fails, typically via defer.
func RegisterIndexing(absPath string) *IndexingState {
	s := &IndexingState{}
	activeIndexes.Store(absPath, s)
	return s
}

// UnregisterIndexing removes the IndexingState for absPath from the registry
// and clears the shared progress file so the daemon stops showing stale data.
func UnregisterIndexing(absPath string) {
	activeIndexes.Delete(absPath)
	clearProgressFile()
}

// ActiveSnapshot returns the IndexingSnapshot for the first project currently
// in an active indexing pass, or an idle snapshot if none are indexing.
// For the common solo-dev case of one project at a time this is exact.
//
// Falls back to reading ~/.synapses/indexing-progress.json so that CLI
// subprocess indexing (synapses index --path …) is visible to the daemon.
func ActiveSnapshot() IndexingSnapshot {
	var found IndexingSnapshot
	activeIndexes.Range(func(_, v interface{}) bool {
		snap := v.(*IndexingState).Snapshot()
		if snap.State == "indexing" {
			found = snap
			return false // stop after first active
		}
		return true
	})
	if found.State == "indexing" {
		return found
	}
	// No in-process indexer — check the shared progress file written by the
	// CLI subprocess when `synapses index` runs outside the daemon process.
	return readProgressFile()
}

// ── Cross-process shared progress file ───────────────────────────────────────

// progressFilePath returns the well-known shared progress file path.
func progressFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".synapses", "indexing-progress.json")
}

// progressFileEntry is the on-disk representation, including the writer's PID
// so stale files from crashed processes are ignored.
type progressFileEntry struct {
	IndexingSnapshot
	PID int `json:"pid"`
}

// lastFileWriteMs tracks the last write time for throttling (global, one indexer at a time).
var lastFileWriteMs atomic.Int64

// writeProgressFile atomically writes snap to the shared progress file.
// Writes are throttled to once per 200 ms unless the state changed to "ready".
func writeProgressFile(snap IndexingSnapshot) {
	now := time.Now().UnixMilli()
	last := lastFileWriteMs.Load()
	if snap.State != "ready" && now-last < 200 {
		return
	}
	if !lastFileWriteMs.CompareAndSwap(last, now) {
		return // another goroutine won the throttle race
	}
	path := progressFilePath()
	if path == "" {
		return
	}
	entry := progressFileEntry{IndexingSnapshot: snap, PID: os.Getpid()}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	os.Rename(tmp, path) //nolint:errcheck
}

// clearProgressFile removes the shared progress file after indexing completes.
func clearProgressFile() {
	path := progressFilePath()
	if path != "" {
		os.Remove(path) //nolint:errcheck
	}
}

// readProgressFile returns the IndexingSnapshot from the shared progress file,
// or an empty snapshot if the file is absent, malformed, or its writer is dead.
func readProgressFile() IndexingSnapshot {
	path := progressFilePath()
	if path == "" {
		return IndexingSnapshot{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return IndexingSnapshot{}
	}
	var entry progressFileEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return IndexingSnapshot{}
	}
	if !processAlive(entry.PID) {
		os.Remove(path) //nolint:errcheck — stale file from crashed process
		return IndexingSnapshot{}
	}
	return entry.IndexingSnapshot
}
