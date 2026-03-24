package mcp

import (
	"sync"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

// ── classifyRefetchSignal ─────────────────────────────────────────────────────

func TestClassifyRefetchSignal_ImmediateCorrection(t *testing.T) {
	// < 5 min → strong negative, emit=true
	for _, secs := range []float64{0, 1, 60, 4*60 + 59} {
		d := time.Duration(secs) * time.Second
		sigType, weight, emit := classifyRefetchSignal(d)
		if !emit {
			t.Errorf("sinceLast=%v: expected emit=true", d)
		}
		if sigType != "correction" {
			t.Errorf("sinceLast=%v: expected sigType=correction, got %q", d, sigType)
		}
		if weight != pulsetypes.SignalWeightCorrectionImmediate {
			t.Errorf("sinceLast=%v: expected weight=%v, got %v", d, pulsetypes.SignalWeightCorrectionImmediate, weight)
		}
	}
}

func TestClassifyRefetchSignal_DelayedCorrection(t *testing.T) {
	// 5–30 min → mild negative, emit=true
	for _, secs := range []float64{5 * 60, 10 * 60, 29*60 + 59} {
		d := time.Duration(secs) * time.Second
		sigType, weight, emit := classifyRefetchSignal(d)
		if !emit {
			t.Errorf("sinceLast=%v: expected emit=true", d)
		}
		if sigType != "correction" {
			t.Errorf("sinceLast=%v: expected sigType=correction, got %q", d, sigType)
		}
		if weight != pulsetypes.SignalWeightCorrectionDelayed {
			t.Errorf("sinceLast=%v: expected weight=%v, got %v", d, pulsetypes.SignalWeightCorrectionDelayed, weight)
		}
	}
}

func TestClassifyRefetchSignal_NeutralAfter30Min(t *testing.T) {
	// ≥ 30 min → neutral, emit=false (new subtask)
	for _, secs := range []float64{30 * 60, 31 * 60, 120 * 60} {
		d := time.Duration(secs) * time.Second
		_, _, emit := classifyRefetchSignal(d)
		if emit {
			t.Errorf("sinceLast=%v: expected emit=false (new subtask), got emit=true", d)
		}
	}
}

func TestClassifyRefetchSignal_Boundary_ExactlyFiveMin(t *testing.T) {
	// Exactly 5 min is the delayed bucket (secs < 30*60, not < 5*60)
	d := 5 * time.Minute
	_, weight, emit := classifyRefetchSignal(d)
	if !emit {
		t.Errorf("exactly 5 min: expected emit=true (delayed bucket)")
	}
	if weight != pulsetypes.SignalWeightCorrectionDelayed {
		t.Errorf("exactly 5 min: expected delayed weight, got %v", weight)
	}
}

func TestClassifyRefetchSignal_Boundary_ExactlyThirtyMin(t *testing.T) {
	// Exactly 30 min is the neutral bucket
	d := 30 * time.Minute
	_, _, emit := classifyRefetchSignal(d)
	if emit {
		t.Errorf("exactly 30 min: expected emit=false (neutral/new-subtask)")
	}
}

// ── GetSessionContextEntities ─────────────────────────────────────────────────

func TestGetSessionContextEntities_ReturnsDistinctEntities(t *testing.T) {
	st := openMCPTestStore(t)

	// Insert two deliveries for distinct entities, same session, task_outcome=''.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-1",
		ToolName:  "get_context",
		Entity:    "AuthLogin",
	})
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-1",
		ToolName:  "get_context",
		Entity:    "AuthLogout",
	})
	// Duplicate entity — should appear only once.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-1",
		ToolName:  "get_context",
		Entity:    "AuthLogin",
		Refetched: true,
	})

	entities := st.GetSessionContextEntities("sess-1")
	if len(entities) != 2 {
		t.Fatalf("expected 2 distinct entities, got %d: %v", len(entities), entities)
	}
	entitySet := make(map[string]bool)
	for _, e := range entities {
		entitySet[e] = true
	}
	if !entitySet["AuthLogin"] {
		t.Error("expected AuthLogin in entities")
	}
	if !entitySet["AuthLogout"] {
		t.Error("expected AuthLogout in entities")
	}
}

func TestGetSessionContextEntities_IgnoresCorrelatedRows(t *testing.T) {
	st := openMCPTestStore(t)

	// One correlated row (task_outcome != '') — should be excluded.
	_, _ = st.CorrelateSessionOutcome("sess-correlated", "success")
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID:   "sess-correlated",
		ToolName:    "get_context",
		Entity:      "SomeFunc",
		TaskOutcome: "success",
	})
	// Insert a row without task_outcome set and correlate it.
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-2",
		ToolName:  "get_context",
		Entity:    "OtherFunc",
	})
	_, _ = st.CorrelateSessionOutcome("sess-2", "success")

	// Both sessions should return no uncorrelated entities.
	if entities := st.GetSessionContextEntities("sess-correlated"); len(entities) != 0 {
		t.Errorf("expected 0 entities for correlated session, got %v", entities)
	}
	if entities := st.GetSessionContextEntities("sess-2"); len(entities) != 0 {
		t.Errorf("expected 0 entities after CorrelateSessionOutcome, got %v", entities)
	}
}

func TestGetSessionContextEntities_EmptySessionID(t *testing.T) {
	st := openMCPTestStore(t)
	if entities := st.GetSessionContextEntities(""); entities != nil {
		t.Errorf("expected nil for empty sessionID, got %v", entities)
	}
}

func TestGetSessionContextEntities_NilStore(t *testing.T) {
	var st *store.Store
	if entities := st.GetSessionContextEntities("sess-any"); entities != nil {
		t.Errorf("expected nil for nil store, got %v", entities)
	}
}

func TestGetSessionContextEntities_WrongSession(t *testing.T) {
	st := openMCPTestStore(t)
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-a",
		ToolName:  "get_context",
		Entity:    "Foo",
	})
	// Different session — should return nothing.
	if entities := st.GetSessionContextEntities("sess-b"); len(entities) != 0 {
		t.Errorf("expected 0 entities for non-matching session, got %v", entities)
	}
}

// ── trackContextCall GC semantics ────────────────────────────────────────────

// TestTrackContextCall_GCUsesLastAt verifies that the entry GC prunes based on
// inactivity since the *last* call (lastAt), not since the *first* call (firstAt).
//
// The bug this covers: using firstAt meant entries in long sessions (>30min)
// were pruned even while actively being refetched, silently dropping signals.
func TestTrackContextCall_GCUsesLastAt(t *testing.T) {
	srv := newTestServer(t)

	// Force the GC sweep window to expire so the next call triggers GC.
	// We do this by backdating ctxCallLastGC to a time > 5 minutes ago.
	srv.ctxCallMu.Lock()
	srv.ctxCallLastGC = time.Now().Add(-10 * time.Minute)
	// Manually insert an entry whose firstAt is >30min ago but lastAt is recent.
	if srv.ctxCalls == nil {
		srv.ctxCalls = make(map[string]*ctxCallEntry)
	}
	srv.ctxCalls["agent-1\x00EntityX"] = &ctxCallEntry{
		count:   2,
		firstAt: time.Now().Add(-35 * time.Minute), // first call was 35min ago
		lastAt:  time.Now().Add(-1 * time.Minute),  // last call was 1min ago (active)
	}
	srv.ctxCallMu.Unlock()

	// Call trackContextCall — this triggers the GC sweep.
	// With lastAt GC: entry is NOT pruned (lastAt = 1min ago < 30min).
	// With firstAt GC (the bug): entry WOULD be pruned (firstAt = 35min ago > 30min).
	count, sinceLast := srv.trackContextCall("agent-1", "EntityX")

	if count != 3 {
		t.Errorf("expected count=3 (entry preserved by GC), got %d — GC may be using firstAt instead of lastAt", count)
	}
	if sinceLast < 30*time.Second {
		t.Errorf("sinceLast should reflect ~1 minute gap, got %v", sinceLast)
	}
}

// TestTrackContextCall_GCPrunesInactiveEntries verifies that genuinely inactive
// entries (lastAt > 30min) are pruned by the GC, resetting count to 1.
func TestTrackContextCall_GCPrunesInactiveEntries(t *testing.T) {
	srv := newTestServer(t)

	srv.ctxCallMu.Lock()
	srv.ctxCallLastGC = time.Now().Add(-10 * time.Minute)
	if srv.ctxCalls == nil {
		srv.ctxCalls = make(map[string]*ctxCallEntry)
	}
	srv.ctxCalls["agent-1\x00InactiveEntity"] = &ctxCallEntry{
		count:   5,
		firstAt: time.Now().Add(-60 * time.Minute),
		lastAt:  time.Now().Add(-35 * time.Minute), // genuinely inactive >30min
	}
	srv.ctxCallMu.Unlock()

	count, sinceLast := srv.trackContextCall("agent-1", "InactiveEntity")

	if count != 1 {
		t.Errorf("expected count=1 (entry pruned — inactive 35min), got %d", count)
	}
	if sinceLast != 0 {
		t.Errorf("expected sinceLast=0 for new entry after GC prune, got %v", sinceLast)
	}
}

// ── emitAbandonedContextSignals ───────────────────────────────────────────────

func TestEmitAbandonedContextSignals_NilPulseClient_NoPanic(t *testing.T) {
	// Server has no pulse client (default from newTestServer). Must not panic.
	srv := newTestServer(t)
	srv.store.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-np",
		ToolName:  "get_context",
		Entity:    "SomeEntity",
	})
	// Should return immediately without panic or error.
	srv.emitAbandonedContextSignals("sess-np", "agent-1", "proj-1")
	// Allow background goroutines to drain (none expected, but defensive).
	srv.drainBackground()
}

func TestEmitAbandonedContextSignals_EmptyEntities_NoPanic(t *testing.T) {
	srv := newTestServer(t)
	pulseCli := newPulseClient(t)
	defer pulseCli.Close()
	srv.SetPulseClient(pulseCli)

	// No context deliveries for this session — emitAbandonedContextSignals
	// should return immediately without firing any signal.
	srv.emitAbandonedContextSignals("sess-empty", "agent-1", "proj-1")
	srv.drainBackground()
}

func TestEmitAbandonedContextSignals_EmitsForUncorrelatedEntities(t *testing.T) {
	srv := newTestServer(t)
	pulseCli := newPulseClient(t)
	defer pulseCli.Close()
	srv.SetPulseClient(pulseCli)

	// Seed two uncorrelated entities in context_deliveries.
	srv.store.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-ab",
		ToolName:  "get_context",
		Entity:    "EntityA",
	})
	srv.store.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-ab",
		ToolName:  "get_context",
		Entity:    "EntityB",
	})

	// Should fire without panic. Background goroutines are fire-and-forget so
	// we drain the pool and verify no rows remain uncorrelated after a
	// subsequent CorrelateSessionOutcome call.
	srv.emitAbandonedContextSignals("sess-ab", "agent-1", "proj-1")
	srv.drainBackground()

	// The entities should still have task_outcome='' at this point (abandoned
	// signals are emitted BEFORE correlation). Verify GetSessionContextEntities
	// still sees them (i.e., we haven't accidentally mutated task_outcome).
	entities := srv.store.GetSessionContextEntities("sess-ab")
	if len(entities) != 2 {
		t.Errorf("expected entities still uncorrelated after emit, got %d", len(entities))
	}
}

func TestEmitAbandonedContextSignals_OnlyUnknownOutcome_CalledFromEndSession(t *testing.T) {
	// Verify the ordering invariant: abandoned signals fire before
	// CorrelateSessionOutcome changes task_outcome to 'unknown'.
	srv := newTestServer(t)
	pulseCli := newPulseClient(t)
	defer pulseCli.Close()
	srv.SetPulseClient(pulseCli)

	srv.store.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-order",
		ToolName:  "get_context",
		Entity:    "OrderedEntity",
	})

	// Simulate the end_session unknown path:
	// 1. emitAbandonedContextSignals (queries task_outcome='')
	// 2. CorrelateSessionOutcome (sets task_outcome='unknown')
	srv.emitAbandonedContextSignals("sess-order", "agent-1", "proj-1")
	srv.drainBackground()

	n, err := srv.store.CorrelateSessionOutcome("sess-order", "unknown")
	if err != nil {
		t.Fatalf("CorrelateSessionOutcome: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row correlated, got %d", n)
	}

	// After correlation, GetSessionContextEntities must return nothing
	// (task_outcome != '' now).
	entities := srv.store.GetSessionContextEntities("sess-order")
	if len(entities) != 0 {
		t.Errorf("expected 0 entities after correlation, got %v", entities)
	}
}

// ── drainBackground helper ────────────────────────────────────────────────────

// drainBackground waits for all in-flight goBackground goroutines to complete.
// It does so by submitting a sentinel task and waiting for it.
func (s *Server) drainBackground() {
	var wg sync.WaitGroup
	wg.Add(1)
	s.goBackground(func() { wg.Done() })
	wg.Wait()
}
