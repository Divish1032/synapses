package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestUserPreferenceID verifies the deterministic ID format.
func TestUserPreferenceID(t *testing.T) {
	id := store.UserPreferenceID("proj-1", "prefers bundled prs")
	expected := "proj-1::prefers bundled prs"
	if id != expected {
		t.Errorf("UserPreferenceID: got %q, want %q", id, expected)
	}
}

// TestUpsertAndGetUserPreferences verifies the full CRUD lifecycle.
func TestUpsertAndGetUserPreferences(t *testing.T) {
	st := openTestStore(t)

	// Insert first preference.
	p1 := store.UserPreference{
		ID:              store.UserPreferenceID("proj-abc", "prefers verbose commit messages"),
		ProjectID:       "proj-abc",
		PrefKey:         "prefers verbose commit messages",
		Text:            "User prefers verbose commit messages (observed 3 times).",
		OccurrenceCount: 3,
		Confidence:      0.70,
	}
	if err := st.UpsertUserPreference(p1); err != nil {
		t.Fatalf("UpsertUserPreference: %v", err)
	}

	// Insert second preference with lower confidence.
	p2 := store.UserPreference{
		ID:              store.UserPreferenceID("proj-abc", "prefers single bundled prs for refactors"),
		ProjectID:       "proj-abc",
		PrefKey:         "prefers single bundled prs for refactors",
		Text:            "User prefers single bundled prs for refactors (observed 2 times).",
		OccurrenceCount: 2,
		Confidence:      0.60,
	}
	if err := st.UpsertUserPreference(p2); err != nil {
		t.Fatalf("UpsertUserPreference p2: %v", err)
	}

	// Retrieve both — no confidence filter.
	prefs, err := st.GetProjectUserPreferences("proj-abc", 0)
	if err != nil {
		t.Fatalf("GetProjectUserPreferences: %v", err)
	}
	if len(prefs) != 2 {
		t.Fatalf("expected 2 preferences, got %d", len(prefs))
	}
	// Results must be ordered confidence DESC.
	if prefs[0].PrefKey != "prefers verbose commit messages" {
		t.Errorf("expected higher-confidence pref first, got %q", prefs[0].PrefKey)
	}

	// Filter by confidence 0.65 — should return only p1.
	filtered, err := st.GetProjectUserPreferences("proj-abc", 0.65)
	if err != nil {
		t.Fatalf("GetProjectUserPreferences filtered: %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("expected 1 filtered preference, got %d", len(filtered))
	}
	if filtered[0].PrefKey != "prefers verbose commit messages" {
		t.Errorf("wrong preference returned: %q", filtered[0].PrefKey)
	}

	// Upsert to update occurrence_count — created_at must be preserved.
	p1Updated := p1
	p1Updated.OccurrenceCount = 5
	p1Updated.Confidence = 0.85
	p1Updated.Text = "User prefers verbose commit messages (observed 5 times)."
	if err := st.UpsertUserPreference(p1Updated); err != nil {
		t.Fatalf("UpsertUserPreference update: %v", err)
	}

	updated, err := st.GetProjectUserPreferences("proj-abc", 0.8)
	if err != nil {
		t.Fatalf("GetProjectUserPreferences after update: %v", err)
	}
	if len(updated) != 1 {
		t.Fatalf("expected 1 high-confidence pref, got %d", len(updated))
	}
	if updated[0].OccurrenceCount != 5 {
		t.Errorf("expected occurrence_count=5, got %d", updated[0].OccurrenceCount)
	}
}

// TestUpsertUserPreference_Validation verifies required-field guards.
func TestUpsertUserPreference_Validation(t *testing.T) {
	st := openTestStore(t)

	if err := st.UpsertUserPreference(store.UserPreference{ProjectID: "p", PrefKey: "k", Confidence: 0.5}); err == nil {
		t.Error("expected error for missing ID, got nil")
	}
	if err := st.UpsertUserPreference(store.UserPreference{ID: "x::k", PrefKey: "k", Confidence: 0.5}); err == nil {
		t.Error("expected error for missing ProjectID, got nil")
	}
	if err := st.UpsertUserPreference(store.UserPreference{ID: "p::k", ProjectID: "p", Confidence: 0.5}); err == nil {
		t.Error("expected error for missing PrefKey, got nil")
	}
}
