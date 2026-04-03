package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestFailurePatternID_deterministic verifies the ID derivation is stable.
func TestFailurePatternID_deterministic(t *testing.T) {
	id := store.FailurePatternID("proj-1", "jwt-go")
	if id != "proj-1::jwt-go" {
		t.Errorf("got %q, want %q", id, "proj-1::jwt-go")
	}
}

// TestUpsertFailurePattern_roundtrip verifies insert + retrieval.
func TestUpsertFailurePattern_roundtrip(t *testing.T) {
	s := openTestStoreObs(t)

	fp := store.FailurePattern{
		ID:              store.FailurePatternID("proj-fp", "jwt-go"),
		ProjectID:       "proj-fp",
		Keyword:         "jwt-go",
		PatternType:     "library",
		OccurrenceCount: 3,
		SampleApproach:  "Using jwt-go v3 for JWT authentication",
		SampleReason:    "incompatible with existing middleware",
		Confidence:      0.75,
		Text:            "'jwt-go' was tried 3 times and abandoned: incompatible with existing middleware.",
	}

	if err := s.UpsertFailurePattern(fp); err != nil {
		t.Fatalf("UpsertFailurePattern: %v", err)
	}

	patterns, err := s.GetProjectFailurePatterns("proj-fp", 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}
	if len(patterns) != 1 {
		t.Fatalf("want 1 pattern, got %d", len(patterns))
	}
	got := patterns[0]
	if got.Keyword != "jwt-go" {
		t.Errorf("keyword: got %q, want %q", got.Keyword, "jwt-go")
	}
	if got.OccurrenceCount != 3 {
		t.Errorf("occurrence_count: got %d, want 3", got.OccurrenceCount)
	}
	if got.Text == "" {
		t.Error("text should not be empty")
	}
	if got.CreatedAt == 0 {
		t.Error("created_at should be set")
	}
	if got.UpdatedAt == 0 {
		t.Error("updated_at should be set")
	}
}

// TestUpsertFailurePattern_upsertUpdatesFields verifies that ON CONFLICT updates
// occurrence_count, confidence, text, and updated_at but preserves created_at.
func TestUpsertFailurePattern_upsertUpdatesFields(t *testing.T) {
	s := openTestStoreObs(t)

	fp := store.FailurePattern{
		ID:              store.FailurePatternID("proj-up", "fasthttp"),
		ProjectID:       "proj-up",
		Keyword:         "fasthttp",
		PatternType:     "library",
		OccurrenceCount: 2,
		SampleApproach:  "Adding fasthttp",
		SampleReason:    "too low-level",
		Confidence:      0.60,
		Text:            "'fasthttp' was tried 2 times and abandoned: too low-level.",
	}

	if err := s.UpsertFailurePattern(fp); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Read to capture created_at.
	before, err := s.GetProjectFailurePatterns("proj-up", 0.0)
	if err != nil || len(before) != 1 {
		t.Fatalf("GetProjectFailurePatterns after first upsert: %v, len=%d", err, len(before))
	}
	firstCreatedAt := before[0].CreatedAt

	// Update: higher count and confidence.
	fp.OccurrenceCount = 4
	fp.Confidence = 0.85
	fp.Text = "'fasthttp' was tried 4 times and abandoned: too low-level."
	if err := s.UpsertFailurePattern(fp); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	after, err := s.GetProjectFailurePatterns("proj-up", 0.0)
	if err != nil || len(after) != 1 {
		t.Fatalf("GetProjectFailurePatterns after second upsert: %v, len=%d", err, len(after))
	}
	got := after[0]
	if got.OccurrenceCount != 4 {
		t.Errorf("occurrence_count: got %d, want 4", got.OccurrenceCount)
	}
	if got.CreatedAt != firstCreatedAt {
		t.Errorf("created_at changed: was %d, now %d", firstCreatedAt, got.CreatedAt)
	}
}

// TestGetProjectFailurePatterns_minConfidence verifies that low-confidence patterns
// are excluded when a minimum threshold is set.
func TestGetProjectFailurePatterns_minConfidence(t *testing.T) {
	s := openTestStoreObs(t)

	high := store.FailurePattern{
		ID:          store.FailurePatternID("proj-mc", "jwt-go"),
		ProjectID:   "proj-mc",
		Keyword:     "jwt-go",
		PatternType: "library",
		OccurrenceCount: 3,
		Confidence:  0.75,
		Text:        "high confidence",
	}
	low := store.FailurePattern{
		ID:          store.FailurePatternID("proj-mc", "fasthttp"),
		ProjectID:   "proj-mc",
		Keyword:     "fasthttp",
		PatternType: "library",
		OccurrenceCount: 2,
		Confidence:  0.40,
		Text:        "low confidence",
	}
	for _, fp := range []store.FailurePattern{high, low} {
		if err := s.UpsertFailurePattern(fp); err != nil {
			t.Fatalf("UpsertFailurePattern: %v", err)
		}
	}

	// With minConfidence=0.6, only the high pattern should be returned.
	patterns, err := s.GetProjectFailurePatterns("proj-mc", 0.6)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}
	if len(patterns) != 1 {
		t.Fatalf("want 1 pattern with conf ≥ 0.6, got %d", len(patterns))
	}
	if patterns[0].Keyword != "jwt-go" {
		t.Errorf("keyword: got %q, want jwt-go", patterns[0].Keyword)
	}

	// With minConfidence=0, both should appear.
	all, err := s.GetProjectFailurePatterns("proj-mc", 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns(0.0): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("want 2 patterns, got %d", len(all))
	}
}

// TestGetProjectFailurePatterns_ordering verifies confidence DESC ordering.
func TestGetProjectFailurePatterns_ordering(t *testing.T) {
	s := openTestStoreObs(t)

	for _, fp := range []store.FailurePattern{
		{
			ID: store.FailurePatternID("proj-ord", "pkg-b"), ProjectID: "proj-ord",
			Keyword: "pkg-b", PatternType: "package", OccurrenceCount: 2,
			Confidence: 0.60, Text: "b",
		},
		{
			ID: store.FailurePatternID("proj-ord", "pkg-a"), ProjectID: "proj-ord",
			Keyword: "pkg-a", PatternType: "package", OccurrenceCount: 5,
			Confidence: 0.95, Text: "a",
		},
		{
			ID: store.FailurePatternID("proj-ord", "pkg-c"), ProjectID: "proj-ord",
			Keyword: "pkg-c", PatternType: "library", OccurrenceCount: 3,
			Confidence: 0.75, Text: "c",
		},
	} {
		if err := s.UpsertFailurePattern(fp); err != nil {
			t.Fatalf("UpsertFailurePattern: %v", err)
		}
	}

	patterns, err := s.GetProjectFailurePatterns("proj-ord", 0.0)
	if err != nil {
		t.Fatalf("GetProjectFailurePatterns: %v", err)
	}
	if len(patterns) != 3 {
		t.Fatalf("want 3, got %d", len(patterns))
	}
	// Should be ordered: pkg-a (0.95), pkg-c (0.75), pkg-b (0.60).
	want := []string{"pkg-a", "pkg-c", "pkg-b"}
	for i, p := range patterns {
		if p.Keyword != want[i] {
			t.Errorf("position %d: got %q, want %q", i, p.Keyword, want[i])
		}
	}
}

// TestUpsertFailurePattern_validation verifies required field checks.
func TestUpsertFailurePattern_validation(t *testing.T) {
	s := openTestStoreObs(t)

	cases := []struct {
		name string
		fp   store.FailurePattern
	}{
		{"missing ID", store.FailurePattern{ProjectID: "p", Keyword: "k"}},
		{"missing project_id", store.FailurePattern{ID: "x::k", Keyword: "k"}},
		{"missing keyword", store.FailurePattern{ID: "x::k", ProjectID: "p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.UpsertFailurePattern(tc.fp); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}
