package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── ViolationID ───────────────────────────────────────────────────────────────

func TestViolationID_Deterministic(t *testing.T) {
	t.Parallel()
	id1 := store.ViolationID("rule-1", "cmd/main.go:main", "internal/secret.go:Secret", "IMPORTS")
	id2 := store.ViolationID("rule-1", "cmd/main.go:main", "internal/secret.go:Secret", "IMPORTS")
	if id1 != id2 {
		t.Errorf("ViolationID not deterministic: %q != %q", id1, id2)
	}
}

func TestViolationID_DifferentInputsDifferentIDs(t *testing.T) {
	t.Parallel()
	id1 := store.ViolationID("rule-1", "a", "b", "IMPORTS")
	id2 := store.ViolationID("rule-2", "a", "b", "IMPORTS")
	if id1 == id2 {
		t.Error("expected different IDs for different rule IDs")
	}
}

// ── LogViolations ─────────────────────────────────────────────────────────────

func TestLogViolations_InsertsAndDeduplicates(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	vs := []config.Violation{
		{
			RuleID:   "no-cmd-import",
			Severity: "error",
			FromNode: graph.NodeID("cmd/main.go:main"),
			ToNode:   graph.NodeID("internal/secret.go:Secret"),
			EdgeType: graph.EdgeType("IMPORTS"),
		},
	}

	// First log — should insert.
	if err := st.LogViolations(vs); err != nil {
		t.Fatalf("LogViolations first: %v", err)
	}

	// Log same violation again — should update occurrences, not insert new row.
	if err := st.LogViolations(vs); err != nil {
		t.Fatalf("LogViolations second: %v", err)
	}

	entries, err := st.GetViolationLog("", 50)
	if err != nil {
		t.Fatalf("GetViolationLog: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry (deduped), got %d", len(entries))
	}
	if entries[0].Occurrences != 2 {
		t.Errorf("expected occurrences=2, got %d", entries[0].Occurrences)
	}
}

func TestLogViolations_Empty(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Logging zero violations should not error.
	if err := st.LogViolations(nil); err != nil {
		t.Fatalf("LogViolations nil: %v", err)
	}
}

// ── ViolationIDsForFile ───────────────────────────────────────────────────────

func TestViolationIDsForFile_Match(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	vs := []config.Violation{
		{
			RuleID:   "rule-a",
			Severity: "error",
			FromNode: graph.NodeID("cmd/main.go:main"),
			ToNode:   graph.NodeID("internal/auth.go:Auth"),
			EdgeType: graph.EdgeType("IMPORTS"),
			FromFile: "cmd/main.go",
			ToFile:   "internal/auth.go",
		},
	}
	_ = st.LogViolations(vs)

	ids, err := st.ViolationIDsForFile("cmd/main.go")
	if err != nil {
		t.Fatalf("ViolationIDsForFile: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("expected 1 ID for cmd/main.go, got %d", len(ids))
	}
}

func TestViolationIDsForFile_NoMatch(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	ids, err := st.ViolationIDsForFile("nonexistent.go")
	if err != nil {
		t.Fatalf("ViolationIDsForFile: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 IDs, got %d", len(ids))
	}
}

// ── GetViolationLog ───────────────────────────────────────────────────────────

func TestGetViolationLog_FilterByRule(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_ = st.LogViolations([]config.Violation{
		{RuleID: "rule-x", Severity: "error", FromNode: "a", ToNode: "b", EdgeType: "CALLS"},
		{RuleID: "rule-y", Severity: "warning", FromNode: "c", ToNode: "d", EdgeType: "IMPORTS"},
	})

	entries, err := st.GetViolationLog("rule-x", 50)
	if err != nil {
		t.Fatalf("GetViolationLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry for rule-x, got %d", len(entries))
	}
	if entries[0].RuleID != "rule-x" {
		t.Errorf("expected rule-x, got %q", entries[0].RuleID)
	}
}

func TestGetViolationLog_AllRules(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_ = st.LogViolations([]config.Violation{
		{RuleID: "r1", Severity: "error", FromNode: "a", ToNode: "b", EdgeType: "CALLS"},
		{RuleID: "r2", Severity: "error", FromNode: "c", ToNode: "d", EdgeType: "CALLS"},
	})

	entries, err := st.GetViolationLog("", 50)
	if err != nil {
		t.Fatalf("GetViolationLog all: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(entries))
	}
}
