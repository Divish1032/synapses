package store

import (
	"strconv"
	"testing"
	"time"
)

// ── DecayedImportanceScore ────────────────────────────────────────────────────

func TestDecayedImportanceScore_Pinned(t *testing.T) {
	t.Parallel()
	// Pinned memory from 10 years ago — must still score 1.0.
	m := Memory{
		Importance:     ImportancePinned,
		LastAccessedAt: time.Now().Add(-10 * 365 * 24 * time.Hour).Format(time.RFC3339),
		CreatedAt:      time.Now().Add(-10 * 365 * 24 * time.Hour).Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score != 1.0 {
		t.Errorf("pinned memory score = %f, want 1.0", score)
	}
}

func TestDecayedImportanceScore_DefaultImportance_JustAccessed(t *testing.T) {
	t.Parallel()
	// importance "1.0", accessed right now — score should be ~1.0.
	m := Memory{
		Importance:     "1.0",
		LastAccessedAt: time.Now().Format(time.RFC3339),
		CreatedAt:      time.Now().Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score < 0.99 {
		t.Errorf("fresh default-importance score = %f, want ≥0.99", score)
	}
}

func TestDecayedImportanceScore_DefaultImportance_OneHalfLife(t *testing.T) {
	t.Parallel()
	// importance "1.0", last accessed 1 week ago — should be ~0.5.
	m := Memory{
		Importance:     "1.0",
		LastAccessedAt: time.Now().Add(-168 * time.Hour).Format(time.RFC3339),
		CreatedAt:      time.Now().Add(-168 * time.Hour).Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score < 0.49 || score > 0.51 {
		t.Errorf("one-half-life score = %f, want ~0.5", score)
	}
}

func TestDecayedImportanceScore_HighImportanceWeight(t *testing.T) {
	t.Parallel()
	// importance "2.0", 1 week old — score = 2.0 * 0.5 = 1.0.
	m := Memory{
		Importance:     "2.0",
		LastAccessedAt: time.Now().Add(-168 * time.Hour).Format(time.RFC3339),
		CreatedAt:      time.Now().Add(-168 * time.Hour).Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score < 0.99 || score > 1.01 {
		t.Errorf("high-importance 1-halflife score = %f, want ~1.0", score)
	}
}

func TestDecayedImportanceScore_LowImportanceWeight(t *testing.T) {
	t.Parallel()
	// importance "0.5", just accessed — score = 0.5 * ~1.0 = ~0.5.
	m := Memory{
		Importance:     "0.5",
		LastAccessedAt: time.Now().Format(time.RFC3339),
		CreatedAt:      time.Now().Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score < 0.49 || score > 0.51 {
		t.Errorf("low-importance fresh score = %f, want ~0.5", score)
	}
}

func TestDecayedImportanceScore_EmptyImportance_TreatedAsOne(t *testing.T) {
	t.Parallel()
	// Empty importance defaults to weight 1.0.
	m := Memory{
		Importance:     "",
		LastAccessedAt: time.Now().Format(time.RFC3339),
		CreatedAt:      time.Now().Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score < 0.99 {
		t.Errorf("empty importance fresh score = %f, want ≥0.99", score)
	}
}

func TestDecayedImportanceScore_InvalidImportance_TreatedAsOne(t *testing.T) {
	t.Parallel()
	// Invalid importance string — should treat as weight 1.0, not panic.
	m := Memory{
		Importance:     "not-a-number",
		LastAccessedAt: time.Now().Format(time.RFC3339),
		CreatedAt:      time.Now().Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score < 0.99 {
		t.Errorf("invalid importance fresh score = %f, want ≥0.99", score)
	}
}

func TestDecayedImportanceScore_BelowVisibilityThreshold(t *testing.T) {
	t.Parallel()
	// importance "1.0", last accessed very long ago — should fall below threshold.
	// At halfLife=168h, threshold=0.05: age = halfLife * (1/0.05 - 1) = 168*19 = 3192h ≈ 133 days.
	m := Memory{
		Importance:     "1.0",
		LastAccessedAt: time.Now().Add(-3200 * time.Hour).Format(time.RFC3339),
		CreatedAt:      time.Now().Add(-3200 * time.Hour).Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score >= DecayVisibilityThreshold {
		t.Errorf("very-old memory score = %f, want < %f (visibility threshold)", score, DecayVisibilityThreshold)
	}
}

func TestDecayedImportanceScore_PinnedNeverBelowThreshold(t *testing.T) {
	t.Parallel()
	// Pinned memory, even very old, must score above threshold.
	m := Memory{
		Importance:     ImportancePinned,
		LastAccessedAt: time.Now().Add(-10000 * time.Hour).Format(time.RFC3339),
		CreatedAt:      time.Now().Add(-10000 * time.Hour).Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score < DecayVisibilityThreshold {
		t.Errorf("pinned old memory score = %f, should be ≥ threshold %f", score, DecayVisibilityThreshold)
	}
}

func TestDecayedImportanceScore_UsesLastAccessedAt_NotCreatedAt(t *testing.T) {
	t.Parallel()
	// Memory created long ago but accessed recently — should have high score.
	// This verifies we use last_accessed_at for the decay signal, not created_at.
	m := Memory{
		Importance:     "1.0",
		LastAccessedAt: time.Now().Format(time.RFC3339),                              // accessed now
		CreatedAt:      time.Now().Add(-3200 * time.Hour).Format(time.RFC3339),       // created long ago
	}
	score := DecayedImportanceScore(m, 168)
	if score < 0.99 {
		t.Errorf("recently-accessed old memory score = %f, want ≥0.99 (decay uses last_accessed_at)", score)
	}
}

func TestDecayedImportanceScore_InvalidTimestamp_FallsBackToCreatedAt(t *testing.T) {
	t.Parallel()
	// Invalid last_accessed_at — should fall back to created_at.
	m := Memory{
		Importance:     "1.0",
		LastAccessedAt: "not-a-timestamp",
		CreatedAt:      time.Now().Format(time.RFC3339),
	}
	score := DecayedImportanceScore(m, 168)
	if score < 0.99 {
		t.Errorf("invalid last_accessed_at fallback score = %f, want ≥0.99", score)
	}
}

// ── Importance field persistence ──────────────────────────────────────────────

func TestInsertMemory_ImportanceRoundTrip(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:       TierProject,
		Content:    "Security config: use TLS 1.3 minimum for all connections.",
		AgentID:    "agent-1",
		Source:     SourceManual,
		Tags:       `[]`,
		Importance: ImportancePinned,
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	mems, err := st.QueryMemories(TierProject, "", "agent-1", 10)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted memory not found")
	}
	if found.Importance != ImportancePinned {
		t.Errorf("importance = %q, want %q", found.Importance, ImportancePinned)
	}
}

func TestInsertMemory_ImportanceDefaultsToOnePointZero(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "Default importance memory for testing decay defaults.",
		AgentID: "agent-2",
		Source:  SourceManual,
		Tags:    `[]`,
		// Importance not set — should default to "1.0".
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	mems, err := st.QueryMemories(TierProject, "", "agent-2", 10)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted memory not found")
	}
	if found.Importance != "1.0" {
		t.Errorf("default importance = %q, want %q", found.Importance, "1.0")
	}
}

func TestInsertMemory_ImportanceFloatWeight(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:       TierProject,
		Content:    "High-importance but non-pinned memory for testing float importance weights.",
		AgentID:    "agent-3",
		Source:     SourceManual,
		Tags:       `[]`,
		Importance: "0.8",
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	mems, err := st.QueryMemories(TierProject, "", "agent-3", 10)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted memory not found")
	}
	if found.Importance != "0.8" {
		t.Errorf("importance = %q, want \"0.8\"", found.Importance)
	}
}

// ── Importance clamping (Fix 3: footgun prevention) ───────────────────────────

func TestInsertMemory_ImportanceZeroClampedToThreshold(t *testing.T) {
	t.Parallel()
	// importance "0.0" would make any memory permanently invisible (score=0 < threshold).
	// prepareMemory must clamp it up to DecayVisibilityThreshold so fresh memories remain visible.
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:       TierProject,
		Content:    "Memory with zero importance — should be clamped to minimum threshold.",
		AgentID:    "agent-clamp-1",
		Source:     SourceManual,
		Tags:       `[]`,
		Importance: "0.0",
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	mems, err := st.QueryMemories(TierProject, "", "agent-clamp-1", 10)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted memory not found")
	}
	// Must be clamped to the minimum floor (2× threshold = 0.10), not left at zero.
	const minFloor = DecayVisibilityThreshold * 2
	if w, err := strconv.ParseFloat(found.Importance, 64); err != nil || w < minFloor {
		t.Errorf("importance %q should have been clamped to >= %v, got %v", found.Importance, minFloor, w)
	}
	// A freshly-inserted memory at the floor must always score above the visibility threshold.
	score := DecayedImportanceScore(*found, 168)
	if score < DecayVisibilityThreshold {
		t.Errorf("clamped importance score = %f, want >= %f (threshold)", score, DecayVisibilityThreshold)
	}
}

func TestInsertMemory_ImportanceBelowThresholdClamped(t *testing.T) {
	t.Parallel()
	// importance "0.01" (below threshold of 0.05) — should be clamped.
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:       TierProject,
		Content:    "Memory with sub-threshold importance — should be clamped to minimum visible.",
		AgentID:    "agent-clamp-2",
		Source:     SourceManual,
		Tags:       `[]`,
		Importance: "0.01",
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	mems, err := st.QueryMemories(TierProject, "", "agent-clamp-2", 10)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted memory not found")
	}
	// The clamped weight must be at or above the minimum floor (0.10).
	const minFloor = DecayVisibilityThreshold * 2
	if w, err := strconv.ParseFloat(found.Importance, 64); err != nil || w < minFloor {
		t.Errorf("importance %q should have been clamped to >= %v, got %v", found.Importance, minFloor, w)
	}
	// A freshly-inserted memory at the floor must score above the visibility threshold.
	score := DecayedImportanceScore(*found, 168)
	if score < DecayVisibilityThreshold {
		t.Errorf("fresh memory with clamped importance score = %f, want >= %f", score, DecayVisibilityThreshold)
	}
}

func TestInsertMemory_ImportanceInvalidStringDefaultsToOne(t *testing.T) {
	t.Parallel()
	// Invalid numeric string falls back to "1.0" (normal decay).
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:       TierProject,
		Content:    "Memory with invalid importance string — should default to 1.0.",
		AgentID:    "agent-clamp-3",
		Source:     SourceManual,
		Tags:       `[]`,
		Importance: "ultra-high",
	})
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	mems, err := st.QueryMemories(TierProject, "", "agent-clamp-3", 10)
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}
	var found *Memory
	for i := range mems {
		if mems[i].ID == id {
			found = &mems[i]
			break
		}
	}
	if found == nil {
		t.Fatal("inserted memory not found")
	}
	if found.Importance != "1.0" {
		t.Errorf("invalid importance defaulted to %q, want \"1.0\"", found.Importance)
	}
}
