// progress.go — daemon-wide indexing progress state.
//
// IndexingState holds atomic counters updated by buildGraph during a full
// reindex. The /api/admin/health endpoint reads these to expose live progress
// to callers (synapses status, Tauri app after B29).
package main

import (
	"sync/atomic"
)

// indexingPhase is the current state of the daemon's indexing pass.
type indexingPhase int32

const (
	indexingPhaseIdle    indexingPhase = 0
	indexingPhaseActive  indexingPhase = 1
	indexingPhaseReady   indexingPhase = 2
)

// IndexingState is a lightweight, lock-free progress tracker for the initial
// full-index pass. It is written by the indexing goroutine and read by the
// HTTP health handler — all fields use atomic operations so no mutex is needed.
type IndexingState struct {
	phase      atomic.Int32 // indexingPhase
	filesDone  atomic.Int64
	filesTotal atomic.Int64
}

// globalProgress is the singleton shared between buildGraph and the HTTP
// health handler. Safe for concurrent access.
var globalProgress IndexingState

// Reset clears all counters and sets phase to idle.
func (s *IndexingState) Reset() {
	s.phase.Store(int32(indexingPhaseIdle))
	s.filesDone.Store(0)
	s.filesTotal.Store(0)
}

// Start marks the beginning of an indexing pass with total files to process.
func (s *IndexingState) Start(total int) {
	s.filesDone.Store(0)
	s.filesTotal.Store(int64(total))
	s.phase.Store(int32(indexingPhaseActive))
}

// Inc increments the done counter by one.
func (s *IndexingState) Inc() {
	s.filesDone.Add(1)
}

// Done marks the pass as complete.
func (s *IndexingState) Done() {
	s.phase.Store(int32(indexingPhaseReady))
}

// Snapshot returns a point-in-time view of the current state.
// Safe to call from any goroutine.
func (s *IndexingState) Snapshot() IndexingSnapshot {
	phase := indexingPhase(s.phase.Load())
	done := s.filesDone.Load()
	total := s.filesTotal.Load()
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

// IndexingSnapshot is the JSON-serialisable view of IndexingState.
type IndexingSnapshot struct {
	State string `json:"state"`           // "idle" | "indexing" | "ready"
	Done  int64  `json:"files_done"`
	Total int64  `json:"files_total"`
	Pct   int64  `json:"pct"`             // 0–100
}
