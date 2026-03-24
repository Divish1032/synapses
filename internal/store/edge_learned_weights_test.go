package store_test

import (
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// edgeKey is a shorthand for building test EdgeWeightKeys.
func edgeKey(from, to, et string) graph.EdgeWeightKey {
	return graph.EdgeWeightKey{
		From: graph.NodeID(from),
		To:   graph.NodeID(to),
		Type: graph.EdgeType(et),
	}
}

// TestLearnedEdgeWeightsVersion_IncrementsOnWrite verifies that the version
// counter starts at 0, increments on each UpsertLearnedEdgeWeights call, and
// that GetLearnedEdgeWeights reflects post-write state (cache invalidation).
func TestLearnedEdgeWeightsVersion_IncrementsOnWrite(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Version must be 0 on a fresh store.
	if v := st.GetLearnedEdgeWeightsVersion(); v != 0 {
		t.Fatalf("expected version 0 on fresh store, got %d", v)
	}

	k1 := edgeKey("pkg::V1", "pkg::V2", "CALLS")
	k2 := edgeKey("pkg::V2", "pkg::V3", "IMPORTS")

	// First write: version 0→1.
	st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{k1}, 0.1)
	v1 := st.GetLearnedEdgeWeightsVersion()
	if v1 != 1 {
		t.Errorf("expected version 1 after first upsert, got %d", v1)
	}

	// Cache must reflect the new weight — not a stale zero.
	m := st.GetLearnedEdgeWeights()
	if m == nil {
		t.Fatal("cache returned nil after first upsert")
	}
	if got := m[k1]; got < 1.09 || got > 1.11 {
		t.Errorf("expected ~1.1 in cache after first upsert, got %f", got)
	}

	// Second write (different key): version 1→2.
	st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{k2}, -0.05)
	v2 := st.GetLearnedEdgeWeightsVersion()
	if v2 != 2 {
		t.Errorf("expected version 2 after second upsert, got %d", v2)
	}

	// Both keys must now be in the cache.
	m2 := st.GetLearnedEdgeWeights()
	if _, ok := m2[k1]; !ok {
		t.Error("k1 missing from cache after second upsert")
	}
	if _, ok := m2[k2]; !ok {
		t.Error("k2 missing from cache after second upsert")
	}
}

// TestLearnedEdgeWeightsVersion_DifferentMapsCollideOnLen demonstrates the
// scenario fixed by the version counter: two weight maps with the same entry
// count but different keys would previously share the same cache key (lew:1).
// With the version counter, each write produces a distinct version, preventing
// any cache collision regardless of map cardinality.
func TestLearnedEdgeWeightsVersion_DifferentMapsCollideOnLen(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Write edge A→B.
	kA := edgeKey("pkg::CollA", "pkg::CollB", "CALLS")
	st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{kA}, 0.5) // weight 1.5, version 1

	vAfterA := st.GetLearnedEdgeWeightsVersion()

	// Write edge C→D (same len=1 map, different key).
	kC := edgeKey("pkg::CollC", "pkg::CollD", "IMPORTS")
	st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{kC}, -0.2) // version 2

	vAfterB := st.GetLearnedEdgeWeightsVersion()

	// Versions must differ — if a cache key used len(map) both would be "lew:1".
	if vAfterA == vAfterB {
		t.Errorf("version did not increment between distinct writes: both %d", vAfterA)
	}

	// The in-memory cache must carry both edges after the second write.
	m := st.GetLearnedEdgeWeights()
	if _, ok := m[kA]; !ok {
		t.Error("kA missing — cache not reloaded after second write")
	}
	if _, ok := m[kC]; !ok {
		t.Error("kC missing — cache not reloaded after second write")
	}
}

// TestGetLearnedEdgeWeights_Empty returns nil before any entries are inserted.
func TestGetLearnedEdgeWeights_Empty(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	result := st.GetLearnedEdgeWeights()
	if result != nil {
		t.Errorf("expected nil map for empty table, got %v", result)
	}
}

// TestUpsertLearnedEdgeWeights_InsertAndBoost verifies a new row is created at
// 1.0+delta and a second call accumulates correctly (capped at 2.0).
func TestUpsertLearnedEdgeWeights_InsertAndBoost(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	k := edgeKey("pkg::A", "pkg::B", "CALLS")

	// First upsert: new row should start at 1.0 + 0.1 = 1.1.
	st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{k}, 0.1)
	m := st.GetLearnedEdgeWeights()
	if m == nil {
		t.Fatal("expected non-nil map after first upsert")
	}
	if got := m[k]; got < 1.09 || got > 1.11 {
		t.Errorf("expected weight ~1.1 after first boost, got %f", got)
	}

	// Second upsert: 1.1 + 0.1 = 1.2.
	st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{k}, 0.1)
	m = st.GetLearnedEdgeWeights()
	if got := m[k]; got < 1.19 || got > 1.21 {
		t.Errorf("expected weight ~1.2 after second boost, got %f", got)
	}
}

// TestUpsertLearnedEdgeWeights_PenaltyAndFloor verifies the floor of 0.3.
func TestUpsertLearnedEdgeWeights_PenaltyAndFloor(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	k := edgeKey("pkg::A", "pkg::C", "IMPORTS")

	// Apply enough negative deltas to hit the floor.
	for i := 0; i < 30; i++ {
		st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{k}, -0.05)
	}
	m := st.GetLearnedEdgeWeights()
	if m == nil {
		t.Fatal("expected non-nil map")
	}
	if got := m[k]; got < 0.3-0.001 {
		t.Errorf("weight dropped below floor: %f", got)
	}
}

// TestUpsertLearnedEdgeWeights_CapAt2x verifies the 2.0× cap.
func TestUpsertLearnedEdgeWeights_CapAt2x(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	k := edgeKey("pkg::X", "pkg::Y", "CALLS")

	// Apply enough positive deltas to hit the cap.
	for i := 0; i < 20; i++ {
		st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{k}, 0.1)
	}
	m := st.GetLearnedEdgeWeights()
	if got := m[k]; got > 2.0+0.001 {
		t.Errorf("weight exceeded cap: %f", got)
	}
}

// TestMarkDormantEdges_AppliesPenalty verifies that edges older than the cutoff
// get dormant=1 and a 0.7× weight multiplier on the next GetLearnedEdgeWeights.
func TestMarkDormantEdges_AppliesPenalty(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	k := edgeKey("pkg::Old", "pkg::Node", "CALLS")

	// Insert with a specific weight of 1.2 and last_used set to the past.
	st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{k}, 0.2) // weight_mult = 1.2

	// Mark as dormant using a cutoff far in the future (everything is "old").
	st.MarkDormantEdges(time.Now().UTC().Add(24 * time.Hour))

	m := st.GetLearnedEdgeWeights()
	got, ok := m[k]
	if !ok {
		t.Fatal("edge missing after dormant marking")
	}
	// Expected: 1.2 × 0.7 = 0.84.
	if got < 0.83 || got > 0.85 {
		t.Errorf("expected dormant weight ~0.84 (1.2×0.7), got %f", got)
	}
}

// TestMarkDormantEdges_AlreadyDormantNotDoubled verifies that a second
// MarkDormantEdges call does not re-apply the penalty.
func TestMarkDormantEdges_AlreadyDormantNotDoubled(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	k := edgeKey("pkg::OldA", "pkg::OldB", "IMPLEMENTS")
	st.UpsertLearnedEdgeWeights([]graph.EdgeWeightKey{k}, 0.0) // weight_mult = 1.0

	cutoff := time.Now().UTC().Add(24 * time.Hour)
	st.MarkDormantEdges(cutoff) // weight_mult → 0.7, dormant → 1
	st.MarkDormantEdges(cutoff) // must NOT apply 0.7× again

	m := st.GetLearnedEdgeWeights()
	got := m[k]
	// Should still be 0.7 (not 0.49).
	if got < 0.69 || got > 0.71 {
		t.Errorf("expected 0.7 after double-call, got %f (should not be 0.49)", got)
	}
}

// TestGetSessionAllDeliveredEntities_HappyPath verifies entities are returned
// after correlation (task_outcome is no longer empty).
func TestGetSessionAllDeliveredEntities_HappyPath(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-weight-1",
		AgentID:   "agent",
		ToolName:  "get_context",
		Entity:    "AuthService",
	})
	st.InsertContextDelivery(store.ContextDelivery{
		SessionID: "sess-weight-1",
		AgentID:   "agent",
		ToolName:  "get_context",
		Entity:    "TokenValidator",
	})
	// Correlate → task_outcome no longer empty.
	_, _ = st.CorrelateSessionOutcome("sess-weight-1", "success")

	entities := st.GetSessionAllDeliveredEntities("sess-weight-1")
	if len(entities) != 2 {
		t.Errorf("expected 2 entities, got %d: %v", len(entities), entities)
	}
}

// TestGetSessionAllDeliveredEntities_EmptySession returns nil for unknown session.
func TestGetSessionAllDeliveredEntities_EmptySession(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	entities := st.GetSessionAllDeliveredEntities("no-such-session")
	if entities != nil {
		t.Errorf("expected nil for unknown session, got %v", entities)
	}
}
