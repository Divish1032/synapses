package mcp

import (
	"time"

	"github.com/SynapsesOS/synapses/internal/pulse"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

// classifyRefetchSignal returns the OutcomeSignalEvent fields appropriate for
// a same-entity re-fetch based on how much time elapsed since the previous
// delivery. Implements Sprint 15 #1 signal quality discipline:
//
//   - sinceLast < 5 min → "correction", SignalWeightCorrectionImmediate (-0.5)
//     The agent immediately needed more context — initial delivery was insufficient.
//   - sinceLast in [5, 30) min → "correction", SignalWeightCorrectionDelayed (-0.2)
//     May be a different angle on the same entity; mild negative.
//   - sinceLast ≥ 30 min → neutral, no signal emitted (new subtask context).
//
// Returns emit=false when no signal should be fired (30+ min case).
func classifyRefetchSignal(sinceLast time.Duration) (signalType string, weight float64, emit bool) {
	secs := sinceLast.Seconds()
	switch {
	case secs < float64(pulsetypes.RefetchImmediateThreshold): // < 5 min
		return "correction", pulsetypes.SignalWeightCorrectionImmediate, true
	case secs < 30*60: // 5–30 min
		return "correction", pulsetypes.SignalWeightCorrectionDelayed, true
	default: // ≥ 30 min — treat as new subtask, neutral
		return "", 0, false
	}
}

// emitAbandonedContextSignals fires a "task_abandoned" OutcomeSignalEvent for
// every entity that received context in the session but was never correlated
// with a task completion. This represents a strong negative signal (the agent
// worked with this context but the session ended without a success outcome).
//
// Must be called BEFORE CorrelateSessionOutcome so it can query rows with
// task_outcome='' — after correlation those rows become "unknown" and are
// indistinguishable from sessions that simply had no tasks.
//
// Errors are silently swallowed: instrumentation must never block session close.
func (s *Server) emitAbandonedContextSignals(sessionID, agentID, projectID string) {
	pc := s.getPulseClient()
	if pc == nil || s.store == nil || sessionID == "" {
		return
	}
	entities := s.store.GetSessionContextEntities(sessionID)
	if len(entities) == 0 {
		return
	}
	for _, entity := range entities {
		e := entity // capture for goroutine
		s.goBackground(func() {
			pc.RecordOutcomeSignal(pulse.OutcomeSignalEvent{
				ProjectID:    projectID,
				AgentID:      agentID,
				Entity:       e,
				SignalType:   "task_abandoned",
				Count:        1,
				SessionID:    sessionID,
				SignalWeight: pulsetypes.SignalWeightTaskAbandoned,
			})
			// NOTE: quality score recomputation (Sprint 15 #2) is handled by
			// the pulse collector after InsertOutcomeSignalTx — calling it here
			// would run before the signal is flushed to the DB.
		})
	}
}
