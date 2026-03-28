package store

import (
	"os"
	"path/filepath"
	"testing"
)

// --- Open error paths ---

func TestOpen_InvalidPath(t *testing.T) {
	// Path to a directory that can't be created (file in the way).
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	// Create a file where a directory is expected.
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(filepath.Join(blocker, "sub", "brain.sqlite"))
	if err == nil {
		t.Fatal("expected error when parent path is a file")
	}
}

// --- GetSummary fallback to legacy (empty projectID) ---

func TestGetSummary_FallbackToLegacy(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Insert with empty projectID (legacy).
	if err := s.UpsertSummary("", "node-legacy", "LegacyFunc", "legacy summary", nil); err != nil {
		t.Fatalf("UpsertSummary: %v", err)
	}

	// Query with a different projectID — should fall back to legacy row.
	got := s.GetSummary("some-project", "node-legacy")
	if got != "legacy summary" {
		t.Errorf("GetSummary fallback = %q, want 'legacy summary'", got)
	}
}

// --- GetSummaries with empty input ---

func TestGetSummaries_EmptyIDs(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	result := s.GetSummaries("proj", []string{})
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

// --- Violation cache upsert (update existing) ---

func TestUpsertViolationExplanation_Update(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertViolationExplanation("r1", "f.go", "old", "old-fix")
	_ = s.UpsertViolationExplanation("r1", "f.go", "new", "new-fix")

	expl, fix, ok := s.GetViolationExplanation("r1", "f.go")
	if !ok {
		t.Fatal("expected hit")
	}
	if expl != "new" {
		t.Errorf("explanation = %q, want 'new'", expl)
	}
	if fix != "new-fix" {
		t.Errorf("fix = %q, want 'new-fix'", fix)
	}
}

// --- Insight cache update ---

func TestUpsertInsightCache_Update(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertInsightCache("n1", "dev", "old insight", []string{"old"})
	_ = s.UpsertInsightCache("n1", "dev", "new insight", []string{"new1", "new2"})

	entry, ok := s.GetInsightCache("n1", "dev")
	if !ok {
		t.Fatal("expected hit")
	}
	if entry.Insight != "new insight" {
		t.Errorf("Insight = %q, want 'new insight'", entry.Insight)
	}
	if len(entry.Concerns) != 2 {
		t.Errorf("Concerns = %v, want 2 items", entry.Concerns)
	}
}

// --- SDLC config update (upsert over existing) ---

func TestUpsertSDLCConfig_OverwritesExisting(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertSDLCConfig("dev", "quick", "agent1")
	_ = s.UpsertSDLCConfig("staging", "enterprise", "agent2")

	cfg := s.GetSDLCConfig()
	if cfg.Phase != "staging" {
		t.Errorf("Phase = %q, want staging", cfg.Phase)
	}
	if cfg.QualityMode != "enterprise" {
		t.Errorf("QualityMode = %q, want enterprise", cfg.QualityMode)
	}
	if cfg.UpdatedBy != "agent2" {
		t.Errorf("UpdatedBy = %q, want agent2", cfg.UpdatedBy)
	}
}

// --- AllPatterns on empty DB ---

func TestAllPatterns_Empty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	all, err := s.AllPatterns()
	if err != nil {
		t.Fatalf("AllPatterns: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 patterns, got %d", len(all))
	}
}

// --- GetADRsForFile with limit=0 (no limit) ---

func TestGetADRsForFile_ZeroLimitReturnsAll(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	for i := 0; i < 3; i++ {
		_ = s.UpsertADR(ADR{
			ID:          string(rune('a' + i)),
			Title:       "T",
			Status:      "accepted",
			Decision:    "D",
			LinkedFiles: []string{"shared/"},
		})
	}

	adrs, err := s.GetADRsForFile("shared/file.go", 0)
	if err != nil {
		t.Fatalf("GetADRsForFile: %v", err)
	}
	if len(adrs) != 3 {
		t.Errorf("expected 3 (no limit), got %d", len(adrs))
	}
}

// --- ADR with nil LinkedFiles ---

func TestUpsertADR_NilLinkedFiles(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	err := s.UpsertADR(ADR{
		ID:       "adr-nil",
		Title:    "No files",
		Status:   "proposed",
		Decision: "TBD",
	})
	if err != nil {
		t.Fatalf("UpsertADR: %v", err)
	}

	got, err := s.GetADR("adr-nil")
	if err != nil {
		t.Fatalf("GetADR: %v", err)
	}
	if len(got.LinkedFiles) != 0 {
		t.Errorf("LinkedFiles = %v, want nil or empty", got.LinkedFiles)
	}
}

// --- ADR with CreatedAt pre-set ---

func TestUpsertADR_PresetCreatedAt(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	err := s.UpsertADR(ADR{
		ID:        "adr-ts",
		Title:     "With timestamp",
		Status:    "accepted",
		Decision:  "yes",
		CreatedAt: "2025-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("UpsertADR: %v", err)
	}

	got, err := s.GetADR("adr-ts")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != "2025-01-01T00:00:00Z" {
		t.Errorf("CreatedAt = %q, want 2025-01-01T00:00:00Z", got.CreatedAt)
	}
}

// --- Multiple patterns for same trigger ---

func TestGetPatternsForTriggers_MultipleTriggers(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_ = s.UpsertPattern("A", "X", "r1")
	_ = s.UpsertPattern("B", "Y", "r2")
	_ = s.UpsertPattern("C", "Z", "r3")

	patterns := s.GetPatternsForTriggers([]string{"A", "B"}, 10)
	if len(patterns) != 2 {
		t.Errorf("expected 2 patterns, got %d", len(patterns))
	}
}
