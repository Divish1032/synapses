package store_test

import (
	"fmt"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── UpsertGap ─────────────────────────────────────────────────────────────────

func TestUpsertGap_Create(t *testing.T) {
	st := openTestStore(t)

	g, err := st.UpsertGap(store.QualityGap{
		NodeID:      "parser.go:DetectProvenance",
		GapID:       "dist-relative-path",
		Description: "dist/ relative path not matched",
		Severity:    "medium",
		Status:      "open",
		FoundBy:     "agent-1",
	})
	if err != nil {
		t.Fatalf("UpsertGap() error: %v", err)
	}
	if g.ID == "" {
		t.Error("expected non-empty ID")
	}
	if g.FoundAt == "" {
		t.Error("expected non-empty FoundAt")
	}
	if g.NodeID != "parser.go:DetectProvenance" {
		t.Errorf("NodeID mismatch: got %q", g.NodeID)
	}
	if g.Status != "open" {
		t.Errorf("Status mismatch: got %q", g.Status)
	}
}

func TestUpsertGap_Idempotent(t *testing.T) {
	st := openTestStore(t)

	first, err := st.UpsertGap(store.QualityGap{
		NodeID:      "parser.go:DetectProvenance",
		GapID:       "dist-relative-path",
		Description: "original description",
		Severity:    "low",
		Status:      "open",
	})
	if err != nil {
		t.Fatalf("first UpsertGap() error: %v", err)
	}

	second, err := st.UpsertGap(store.QualityGap{
		NodeID:      "parser.go:DetectProvenance",
		GapID:       "dist-relative-path",
		Description: "updated description",
		Severity:    "high",
		Status:      "open",
	})
	if err != nil {
		t.Fatalf("second UpsertGap() error: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("expected same ID on upsert: first=%q second=%q", first.ID, second.ID)
	}
	if second.Description != "updated description" {
		t.Errorf("Description not updated: got %q", second.Description)
	}
	if second.Severity != "high" {
		t.Errorf("Severity not updated: got %q", second.Severity)
	}
}

func TestUpsertGap_FixedLifecycle(t *testing.T) {
	st := openTestStore(t)

	_, err := st.UpsertGap(store.QualityGap{
		NodeID:      "tools.go:handleGetContext",
		GapID:       "entity-disambig",
		Description: "bare entity name in signals",
		Severity:    "low",
		Status:      "open",
	})
	if err != nil {
		t.Fatalf("UpsertGap open: %v", err)
	}

	fixed, err := st.UpsertGap(store.QualityGap{
		NodeID:    "tools.go:handleGetContext",
		GapID:     "entity-disambig",
		Description: "bare entity name in signals",
		Severity:  "low",
		Status:    "fixed",
		FixNotes:  "now uses entityWithPath helper",
	})
	if err != nil {
		t.Fatalf("UpsertGap fixed: %v", err)
	}
	if fixed.Status != "fixed" {
		t.Errorf("expected status=fixed, got %q", fixed.Status)
	}
	if fixed.FixNotes != "now uses entityWithPath helper" {
		t.Errorf("fix_notes not persisted: got %q", fixed.FixNotes)
	}
}

// ── GetGaps ───────────────────────────────────────────────────────────────────

func TestGetGaps_DefaultOpenOnly(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.UpsertGap(store.QualityGap{NodeID: "n1", GapID: "g1", Description: "open gap", Severity: "medium", Status: "open"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "n2", GapID: "g2", Description: "fixed gap", Severity: "low", Status: "fixed"})

	gaps, err := st.GetGaps(store.GapFilter{Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps() error: %v", err)
	}
	if len(gaps) != 1 {
		t.Errorf("expected 1 open gap, got %d", len(gaps))
	}
	if gaps[0].GapID != "g1" {
		t.Errorf("expected gap g1, got %q", gaps[0].GapID)
	}
}

func TestGetGaps_FilterByNodeID(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.UpsertGap(store.QualityGap{NodeID: "node-a", GapID: "gap-1", Description: "d", Severity: "medium", Status: "open"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "node-b", GapID: "gap-2", Description: "d", Severity: "medium", Status: "open"})

	gaps, err := st.GetGaps(store.GapFilter{NodeID: "node-a", Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps() error: %v", err)
	}
	if len(gaps) != 1 {
		t.Errorf("expected 1 gap for node-a, got %d", len(gaps))
	}
}

func TestGetGaps_FilterBySeverity(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.UpsertGap(store.QualityGap{NodeID: "n1", GapID: "g1", Description: "d", Severity: "high", Status: "open"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "n2", GapID: "g2", Description: "d", Severity: "low", Status: "open"})

	gaps, err := st.GetGaps(store.GapFilter{Severity: "high", Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps() error: %v", err)
	}
	if len(gaps) != 1 || gaps[0].GapID != "g1" {
		t.Errorf("expected 1 high-severity gap, got %d", len(gaps))
	}
}

func TestGetGaps_AllStatuses(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.UpsertGap(store.QualityGap{NodeID: "n1", GapID: "g1", Description: "d", Severity: "medium", Status: "open"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "n2", GapID: "g2", Description: "d", Severity: "low", Status: "fixed"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "n3", GapID: "g3", Description: "d", Severity: "low", Status: "wontfix"})

	gaps, err := st.GetGaps(store.GapFilter{Status: "all"})
	if err != nil {
		t.Fatalf("GetGaps(all) error: %v", err)
	}
	if len(gaps) != 3 {
		t.Errorf("expected 3 gaps with status=all, got %d", len(gaps))
	}
}

func TestGetGaps_EmptyResult(t *testing.T) {
	st := openTestStore(t)

	gaps, err := st.GetGaps(store.GapFilter{Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps() error: %v", err)
	}
	if len(gaps) != 0 {
		t.Errorf("expected 0 gaps in empty store, got %d", len(gaps))
	}
}

// TestGetGaps_SortOrderIsBySeverity verifies that GetGaps returns gaps in
// semantic severity order (critical → high → medium → low), not lexicographic
// order. Lexicographic DESC would yield medium → low → high → critical (wrong).
func TestGetGaps_SortOrderIsBySeverity(t *testing.T) {
	st := openTestStore(t)

	severities := []string{"low", "medium", "critical", "high"}
	for i, sev := range severities {
		_, err := st.UpsertGap(store.QualityGap{
			NodeID:      fmt.Sprintf("node-%d", i),
			GapID:       fmt.Sprintf("gap-%s", sev),
			Description: "d",
			Severity:    sev,
			Status:      "open",
		})
		if err != nil {
			t.Fatalf("UpsertGap(%s): %v", sev, err)
		}
	}

	gaps, err := st.GetGaps(store.GapFilter{Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps() error: %v", err)
	}
	if len(gaps) != 4 {
		t.Fatalf("expected 4 gaps, got %d", len(gaps))
	}

	want := []string{"critical", "high", "medium", "low"}
	for i, g := range gaps {
		if g.Severity != want[i] {
			t.Errorf("gaps[%d]: expected severity %q, got %q (full order: %v)",
				i, want[i], g.Severity, severitiesOf(gaps))
		}
	}
}

func severitiesOf(gaps []store.QualityGap) []string {
	out := make([]string, len(gaps))
	for i, g := range gaps {
		out[i] = g.Severity
	}
	return out
}

// TestGetGaps_FileFilterAnchored verifies that get_gaps(file="auth.go") does
// NOT match "unauth.go" — the old LIKE '%auth.go%' pattern was too broad.
func TestGetGaps_FileFilterAnchored(t *testing.T) {
	st := openTestStore(t)

	// Gap on the target file (node_id contains ::auth.go::)
	_, _ = st.UpsertGap(store.QualityGap{
		NodeID: "repo::pkg/auth.go::ValidateToken", GapID: "g1",
		Description: "d", Severity: "medium", Status: "open",
	})
	// Gap on a different file whose name contains "auth.go" as substring
	_, _ = st.UpsertGap(store.QualityGap{
		NodeID: "repo::pkg/unauth.go::Bypass", GapID: "g2",
		Description: "d", Severity: "medium", Status: "open",
	})

	gaps, err := st.GetGaps(store.GapFilter{File: "auth.go", Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps() error: %v", err)
	}
	if len(gaps) != 1 {
		t.Errorf("expected 1 gap for auth.go, got %d (gaps: %v)", len(gaps), gapIDs(gaps))
	}
	if len(gaps) == 1 && gaps[0].GapID != "g1" {
		t.Errorf("expected gap g1, got %q", gaps[0].GapID)
	}
}

func gapIDs(gaps []store.QualityGap) []string {
	out := make([]string, len(gaps))
	for i, g := range gaps {
		out[i] = g.GapID
	}
	return out
}

// ── Compound filters ──────────────────────────────────────────────────────────

// TestGetGaps_CompoundNodeIDAndSeverity verifies that NodeID + Severity filters
// are both applied. The old switch fell through to the NodeID-only case and
// silently ignored the Severity constraint.
func TestGetGaps_CompoundNodeIDAndSeverity(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.UpsertGap(store.QualityGap{NodeID: "node-a", GapID: "g1", Description: "d", Severity: "high", Status: "open"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "node-a", GapID: "g2", Description: "d", Severity: "low", Status: "open"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "node-b", GapID: "g3", Description: "d", Severity: "high", Status: "open"})

	gaps, err := st.GetGaps(store.GapFilter{NodeID: "node-a", Severity: "high", Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps() error: %v", err)
	}
	if len(gaps) != 1 {
		t.Errorf("expected 1 gap (node-a + high), got %d (gaps: %v)", len(gaps), gapIDs(gaps))
	}
	if len(gaps) == 1 && gaps[0].GapID != "g1" {
		t.Errorf("expected gap g1, got %q", gaps[0].GapID)
	}
}

// TestGetGaps_CompoundFileAndSeverity verifies that File + Severity filters
// are both applied. The old switch fell through to the File-only case.
func TestGetGaps_CompoundFileAndSeverity(t *testing.T) {
	st := openTestStore(t)

	_, _ = st.UpsertGap(store.QualityGap{NodeID: "repo::pkg/auth.go::Login", GapID: "g1", Description: "d", Severity: "critical", Status: "open"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "repo::pkg/auth.go::Logout", GapID: "g2", Description: "d", Severity: "low", Status: "open"})
	_, _ = st.UpsertGap(store.QualityGap{NodeID: "repo::other.go::Func", GapID: "g3", Description: "d", Severity: "critical", Status: "open"})

	gaps, err := st.GetGaps(store.GapFilter{File: "auth.go", Severity: "critical", Status: "open"})
	if err != nil {
		t.Fatalf("GetGaps() error: %v", err)
	}
	if len(gaps) != 1 {
		t.Errorf("expected 1 gap (auth.go + critical), got %d (gaps: %v)", len(gaps), gapIDs(gaps))
	}
	if len(gaps) == 1 && gaps[0].GapID != "g1" {
		t.Errorf("expected gap g1, got %q", gaps[0].GapID)
	}
}
