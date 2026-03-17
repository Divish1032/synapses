// progress.go — per-project indexing progress tracking.
//
// IndexingState is created per project (not a global singleton) and registered
// in activeIndexes for the duration of a full reindex. The /api/admin/health
// endpoint calls ActiveSnapshot() to surface live progress to callers.
package main

import (
	"sync"
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
}

// Start records the real total file count (known after Phase 1 filesystem scan)
// and transitions the state to active. Must be called before SetDone.
func (s *IndexingState) Start(total int64) {
	s.mu.Lock()
	s.phase = indexingPhaseActive
	s.filesTotal = total
	s.filesDone = 0
	s.mu.Unlock()
}

// SetDone updates the count of files parsed so far.
func (s *IndexingState) SetDone(done int64) {
	s.mu.Lock()
	s.filesDone = done
	s.mu.Unlock()
}

// Done transitions the state to ready (indexing complete).
func (s *IndexingState) Done() {
	s.mu.Lock()
	s.phase = indexingPhaseReady
	s.mu.Unlock()
}

// Snapshot returns a consistent, point-in-time view of all fields.
// All three values are read under the same lock, so callers never observe
// a mixed state (e.g. phase=ready but filesDone < filesTotal).
func (s *IndexingState) Snapshot() IndexingSnapshot {
	s.mu.Lock()
	phase := s.phase
	done := s.filesDone
	total := s.filesTotal
	s.mu.Unlock()

	var pct int64
	if total > 0 {
		pct = done * 100 / total
	}
	stateStr := "idle"
	switch phase {
	case indexingPhaseActive:
		stateStr = "indexing"
	case indexingPhaseReady:
		stateStr = "ready"
	}
	return IndexingSnapshot{
		State: stateStr,
		Done:  done,
		Total: total,
		Pct:   pct,
	}
}

// IndexingSnapshot is the JSON-serialisable view returned by the health endpoint.
type IndexingSnapshot struct {
	State string `json:"state"`       // "idle" | "indexing" | "ready"
	Done  int64  `json:"files_done"`
	Total int64  `json:"files_total"`
	Pct   int64  `json:"pct"` // 0–100
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

// UnregisterIndexing removes the IndexingState for absPath from the registry.
func UnregisterIndexing(absPath string) {
	activeIndexes.Delete(absPath)
}

// ActiveSnapshot returns the IndexingSnapshot for the first project currently
// in an active indexing pass, or an idle snapshot if none are indexing.
// For the common solo-dev case of one project at a time this is exact.
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
	return found
}
