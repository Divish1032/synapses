package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestConvention_UpsertAndGet verifies round-trip insert + retrieval.
func TestConvention_UpsertAndGet(t *testing.T) {
	s := openTestStoreObs(t)

	c := store.ExtractedConvention{
		ID:           store.ConventionID("proj-1", store.ObsCategoryLibraryUsage, "uses_testify"),
		ProjectID:    "proj-1",
		Category:     store.ObsCategoryLibraryUsage,
		Key:          "uses_testify",
		SessionCount: 4,
		Confidence:   0.70,
		Text:         "This project uses testify for testing assertions (observed across 4 sessions).",
	}

	if err := s.UpsertConvention(c); err != nil {
		t.Fatalf("UpsertConvention: %v", err)
	}

	convs, err := s.GetProjectConventions("proj-1", 0.0)
	if err != nil {
		t.Fatalf("GetProjectConventions: %v", err)
	}
	if len(convs) != 1 {
		t.Fatalf("expected 1 convention, got %d", len(convs))
	}

	got := convs[0]
	if got.Key != "uses_testify" {
		t.Errorf("key mismatch: %q", got.Key)
	}
	if got.SessionCount != 4 {
		t.Errorf("session_count mismatch: %d", got.SessionCount)
	}
	if got.Confidence != 0.70 {
		t.Errorf("confidence mismatch: %v", got.Confidence)
	}
	if got.CreatedAt == 0 {
		t.Error("created_at must be set")
	}
	if got.UpdatedAt == 0 {
		t.Error("updated_at must be set")
	}
}

// TestConvention_UpsertUpdatesExisting verifies that upserting a convention
// with the same ID updates session_count, confidence, and text while preserving
// the original created_at.
func TestConvention_UpsertUpdatesExisting(t *testing.T) {
	s := openTestStoreObs(t)

	id := store.ConventionID("proj-2", store.ObsCategoryTestingPattern, "go_test_files_touched")
	first := store.ExtractedConvention{
		ID:           id,
		ProjectID:    "proj-2",
		Category:     store.ObsCategoryTestingPattern,
		Key:          "go_test_files_touched",
		SessionCount: 3,
		Confidence:   0.60,
		Text:         "first text",
	}
	if err := s.UpsertConvention(first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	got1, _ := s.GetProjectConventions("proj-2", 0.0)
	if len(got1) == 0 {
		t.Fatal("expected convention after first upsert")
	}
	createdAt := got1[0].CreatedAt

	// Upsert again with higher count.
	second := store.ExtractedConvention{
		ID:           id,
		ProjectID:    "proj-2",
		Category:     store.ObsCategoryTestingPattern,
		Key:          "go_test_files_touched",
		SessionCount: 7,
		Confidence:   0.90,
		Text:         "updated text",
	}
	if err := s.UpsertConvention(second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got2, _ := s.GetProjectConventions("proj-2", 0.0)
	if len(got2) != 1 {
		t.Fatalf("expected exactly 1 convention after second upsert, got %d", len(got2))
	}
	c := got2[0]
	if c.SessionCount != 7 {
		t.Errorf("session_count not updated: want 7, got %d", c.SessionCount)
	}
	if c.Confidence != 0.90 {
		t.Errorf("confidence not updated: want 0.90, got %v", c.Confidence)
	}
	if c.Text != "updated text" {
		t.Errorf("text not updated: %q", c.Text)
	}
	// created_at must be preserved from the first insert.
	if c.CreatedAt != createdAt {
		t.Errorf("created_at changed: was %d, now %d", createdAt, c.CreatedAt)
	}
}

// TestConvention_GetFiltersByConfidence verifies that GetProjectConventions
// honours the minConfidence threshold.
func TestConvention_GetFiltersByConfidence(t *testing.T) {
	s := openTestStoreObs(t)

	low := store.ExtractedConvention{
		ID:        store.ConventionID("proj-3", store.ObsCategoryLibraryUsage, "uses_flask"),
		ProjectID: "proj-3", Category: store.ObsCategoryLibraryUsage,
		Key: "uses_flask", SessionCount: 3, Confidence: 0.60, Text: "low",
	}
	high := store.ExtractedConvention{
		ID:        store.ConventionID("proj-3", store.ObsCategoryLibraryUsage, "uses_django"),
		ProjectID: "proj-3", Category: store.ObsCategoryLibraryUsage,
		Key: "uses_django", SessionCount: 10, Confidence: 0.95, Text: "high",
	}
	if err := s.UpsertConvention(low); err != nil {
		t.Fatalf("upsert low: %v", err)
	}
	if err := s.UpsertConvention(high); err != nil {
		t.Fatalf("upsert high: %v", err)
	}

	// minConfidence = 0.7 should return only the high-confidence one.
	filtered, err := s.GetProjectConventions("proj-3", 0.7)
	if err != nil {
		t.Fatalf("GetProjectConventions: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 convention above 0.7, got %d", len(filtered))
	}
	if filtered[0].Key != "uses_django" {
		t.Errorf("unexpected key: %q", filtered[0].Key)
	}
}

// TestConvention_GetOrdersByConfidenceDesc verifies that multiple conventions
// are returned highest-confidence first.
func TestConvention_GetOrdersByConfidenceDesc(t *testing.T) {
	s := openTestStoreObs(t)

	for _, c := range []store.ExtractedConvention{
		{
			ID:        store.ConventionID("proj-4", store.ObsCategoryLibraryUsage, "uses_jest"),
			ProjectID: "proj-4", Category: store.ObsCategoryLibraryUsage,
			Key: "uses_jest", SessionCount: 3, Confidence: 0.60, Text: "jest",
		},
		{
			ID:        store.ConventionID("proj-4", store.ObsCategoryLibraryUsage, "uses_express"),
			ProjectID: "proj-4", Category: store.ObsCategoryLibraryUsage,
			Key: "uses_express", SessionCount: 7, Confidence: 0.90, Text: "express",
		},
		{
			ID:        store.ConventionID("proj-4", store.ObsCategoryLibraryUsage, "uses_vitest"),
			ProjectID: "proj-4", Category: store.ObsCategoryLibraryUsage,
			Key: "uses_vitest", SessionCount: 5, Confidence: 0.80, Text: "vitest",
		},
	} {
		if err := s.UpsertConvention(c); err != nil {
			t.Fatalf("upsert %s: %v", c.Key, err)
		}
	}

	convs, err := s.GetProjectConventions("proj-4", 0.0)
	if err != nil {
		t.Fatalf("GetProjectConventions: %v", err)
	}
	if len(convs) != 3 {
		t.Fatalf("expected 3, got %d", len(convs))
	}
	// Must be: express (0.90) → vitest (0.80) → jest (0.60)
	order := []string{"uses_express", "uses_vitest", "uses_jest"}
	for i, want := range order {
		if convs[i].Key != want {
			t.Errorf("position %d: want %q, got %q", i, want, convs[i].Key)
		}
	}
}

// TestConvention_RequiredFields verifies validation on missing required fields.
func TestConvention_RequiredFields(t *testing.T) {
	s := openTestStoreObs(t)

	// Missing ID.
	err := s.UpsertConvention(store.ExtractedConvention{
		ProjectID: "p", Category: "c", Key: "k",
	})
	if err == nil {
		t.Error("expected error for missing ID")
	}

	// Missing ProjectID.
	err = s.UpsertConvention(store.ExtractedConvention{
		ID: "p::c::k", Category: "c", Key: "k",
	})
	if err == nil {
		t.Error("expected error for missing project_id")
	}
}

// TestConvention_CrossProjectIsolation verifies that conventions for one project
// are not returned for another project.
func TestConvention_CrossProjectIsolation(t *testing.T) {
	s := openTestStoreObs(t)

	for _, proj := range []string{"proj-a", "proj-b"} {
		if err := s.UpsertConvention(store.ExtractedConvention{
			ID:           store.ConventionID(proj, store.ObsCategoryLibraryUsage, "uses_testify"),
			ProjectID:    proj,
			Category:     store.ObsCategoryLibraryUsage,
			Key:          "uses_testify",
			SessionCount: 4,
			Confidence:   0.70,
			Text:         "text for " + proj,
		}); err != nil {
			t.Fatalf("upsert %s: %v", proj, err)
		}
	}

	convs, _ := s.GetProjectConventions("proj-a", 0.0)
	if len(convs) != 1 {
		t.Fatalf("proj-a: expected 1, got %d", len(convs))
	}
	if convs[0].Text != "text for proj-a" {
		t.Errorf("wrong project: %q", convs[0].Text)
	}
}
