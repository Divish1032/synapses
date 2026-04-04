package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// ── extractPrefSignals ────────────────────────────────────────────────────────

func TestExtractPrefSignals_BasicPatterns(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantKey string // expected normalized key prefix (first 20 chars)
	}{
		{
			name:    "user prefers",
			content: "User prefers verbose commit messages when multiple files changed.",
			wantKey: "verbose commit messa",
		},
		{
			name:    "user wants",
			content: "User wants single bundled PRs for refactors",
			wantKey: "single bundled prs f",
		},
		{
			name:    "user likes",
			content: "User likes detailed inline comments on complex logic.",
			wantKey: "detailed inline comm",
		},
		{
			name:    "user confirmed",
			content: "User confirmed that bundled commits are preferred.",
			wantKey: "that bundled commits",
		},
		{
			name:    "user corrected",
			content: "User corrected: always split unrelated changes into separate PRs.",
			wantKey: "always split unrelat",
		},
		{
			name:    "user asked for",
			content: "User asked for verbose output when running tests.",
			wantKey: "verbose output when ",
		},
		{
			name:    "user chose",
			content: "User chose to keep tests in the same file as the implementation.",
			wantKey: "to keep tests in the",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPrefSignals(tt.content)
			if len(got) == 0 {
				t.Fatalf("extractPrefSignals: got no signals for %q", tt.content)
			}
			if !strings.HasPrefix(got[0], tt.wantKey) {
				t.Errorf("extractPrefSignals: got %q, want prefix %q", got[0], tt.wantKey)
			}
		})
	}
}

func TestExtractPrefSignals_EmptyContent(t *testing.T) {
	if got := extractPrefSignals(""); got != nil {
		t.Errorf("expected nil for empty content, got %v", got)
	}
}

func TestExtractPrefSignals_NoSignals(t *testing.T) {
	content := "The authentication service uses JWT tokens for session management."
	if got := extractPrefSignals(content); len(got) != 0 {
		t.Errorf("expected no signals, got %v", got)
	}
}

func TestExtractPrefSignals_DeduplicatesWithinContent(t *testing.T) {
	// Two different preference phrases → two distinct keys.
	// Neither key should appear more than once.
	content := "User prefers bundled PRs for refactors. Also, user wants verbose commit messages for clarity."
	got := extractPrefSignals(content)
	seen := make(map[string]bool)
	for _, k := range got {
		if seen[k] {
			t.Errorf("duplicate key %q returned by extractPrefSignals", k)
		}
		seen[k] = true
	}
}

func TestExtractPrefSignals_TooShortPhraseIgnored(t *testing.T) {
	// Phrase < 10 chars after signal word must not match.
	content := "User prefers X."
	got := extractPrefSignals(content)
	if len(got) != 0 {
		t.Errorf("expected no signals for too-short phrase, got %v", got)
	}
}

// ── normalizeUserPrefKey ──────────────────────────────────────────────────────

func TestNormalizeUserPrefKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{
			input: "  Verbose Commit Messages  ",
			want:  "verbose commit messages",
		},
		{
			input: "Single  Bundled   PRs",
			want:  "single bundled prs",
		},
		{
			input: "",
			want:  "",
		},
		{
			// Longer than maxPrefKeyRunes — must be truncated.
			input: strings.Repeat("abcdefghij", 10), // 100 chars
			want:  strings.Repeat("abcdefghij", 6),  // 60 chars
		},
	}

	for _, tt := range tests {
		got := normalizeUserPrefKey(tt.input)
		if got != tt.want {
			t.Errorf("normalizeUserPrefKey(%q): got %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ── userPrefText ──────────────────────────────────────────────────────────────

func TestUserPrefText(t *testing.T) {
	text := userPrefText("prefers bundled prs for refactors", 3)
	if !strings.Contains(text, "Prefers") {
		t.Errorf("expected capitalized phrase, got %q", text)
	}
	if !strings.Contains(text, "3") {
		t.Errorf("expected count in text, got %q", text)
	}
	if !strings.Contains(text, "times") {
		t.Errorf("expected 'times', got %q", text)
	}

	// Singular for count=1.
	single := userPrefText("prefers verbose commits", 1)
	if !strings.Contains(single, "time") || strings.Contains(single, "times") {
		t.Errorf("expected 'time' (not 'times') for count=1, got %q", single)
	}
}

// ── userPrefConfidence ────────────────────────────────────────────────────────

func TestUserPrefConfidence(t *testing.T) {
	// Confidence must increase monotonically.
	prev := 0.0
	for _, count := range []int{2, 3, 4, 5, 6, 8} {
		c := userPrefConfidence(count)
		if c <= prev {
			t.Errorf("confidence not monotonically increasing at count=%d: %.2f <= %.2f", count, c, prev)
		}
		if c > 1.0 {
			t.Errorf("confidence > 1.0 at count=%d: %.2f", count, c)
		}
		prev = c
	}
	// Minimum threshold (2) must return >= 0.5.
	if c := userPrefConfidence(MinOccurrencesForUserPref); c < 0.5 {
		t.Errorf("minimum threshold confidence < 0.5: %.2f", c)
	}
}

// ── runUserPrefExtraction ─────────────────────────────────────────────────────

// insertPrefObs is a helper that inserts a user_preference session observation
// directly into the store (bypassing maybeRecordUserPrefObs, which requires a
// running server). Used to test runUserPrefExtraction in isolation.
func insertPrefObs(t *testing.T, st *store.Store, projectID, sessionID, key string) {
	t.Helper()
	obs := store.SessionObservation{
		SessionID:  sessionID,
		ProjectID:  projectID,
		AgentID:    "implementer",
		Category:   store.ObsCategoryUserPref,
		Key:        key,
		Value:      "",
		Confidence: 0.5,
		CreatedAt:  time.Now().UTC().Unix(),
	}
	if _, err := st.InsertSessionObservation(obs); err != nil {
		t.Fatalf("insertPrefObs: %v", err)
	}
}

func TestRunUserPrefExtraction_EmptyProject(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	n, err := runUserPrefExtraction(srv.store, "")
	if err != nil {
		t.Fatalf("runUserPrefExtraction empty projectID: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 prefs for empty projectID, got %d", n)
	}
}

func TestRunUserPrefExtraction_NilStore(t *testing.T) {
	n, err := runUserPrefExtraction(nil, "proj-1")
	if err != nil {
		t.Fatalf("runUserPrefExtraction nil store: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 for nil store, got %d", n)
	}
}

func TestRunUserPrefExtraction_PromotesAfterThreshold(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	projID := "proj-pref-test"
	key := "prefers verbose commit messages for large changes"

	// Insert MinOccurrencesForUserPref-1 observations — must NOT promote.
	for i := 0; i < MinOccurrencesForUserPref-1; i++ {
		insertPrefObs(t, srv.store, projID, "sess-"+string(rune('A'+i)), key)
	}
	n, err := runUserPrefExtraction(srv.store, projID)
	if err != nil {
		t.Fatalf("runUserPrefExtraction: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 promotions below threshold, got %d", n)
	}

	// Add one more observation to cross the threshold.
	insertPrefObs(t, srv.store, projID, "sess-C", key)

	n, err = runUserPrefExtraction(srv.store, projID)
	if err != nil {
		t.Fatalf("runUserPrefExtraction after threshold: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 promotion at threshold, got %d", n)
	}

	// Verify the preference is stored and queryable.
	prefs, err := srv.store.GetProjectUserPreferences(projID, 0)
	if err != nil {
		t.Fatalf("GetProjectUserPreferences: %v", err)
	}
	if len(prefs) != 1 {
		t.Fatalf("expected 1 preference, got %d", len(prefs))
	}
	if prefs[0].OccurrenceCount != MinOccurrencesForUserPref {
		t.Errorf("expected occurrence_count=%d, got %d", MinOccurrencesForUserPref, prefs[0].OccurrenceCount)
	}
}

func TestRunUserPrefExtraction_MultiplePrefs(t *testing.T) {
	srv, _, _ := newPopulatedServer(t)
	projID := "proj-multi-pref"
	keyA := "prefers verbose commit messages for every change"
	keyB := "wants single bundled prs for all refactors this sprint"

	// Insert enough observations for both preferences.
	for i := 0; i < MinOccurrencesForUserPref; i++ {
		sess := "sess-multi-" + string(rune('A'+i))
		insertPrefObs(t, srv.store, projID, sess+"-a", keyA)
		insertPrefObs(t, srv.store, projID, sess+"-b", keyB)
	}

	n, err := runUserPrefExtraction(srv.store, projID)
	if err != nil {
		t.Fatalf("runUserPrefExtraction multi: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 promotions, got %d", n)
	}

	prefs, err := srv.store.GetProjectUserPreferences(projID, 0)
	if err != nil {
		t.Fatalf("GetProjectUserPreferences: %v", err)
	}
	if len(prefs) != 2 {
		t.Errorf("expected 2 preferences stored, got %d", len(prefs))
	}
}

func TestRunUserPrefExtraction_IsIdempotent(t *testing.T) {
	// Running extraction twice must not double-count or create extra rows.
	srv, _, _ := newPopulatedServer(t)
	projID := "proj-idem-pref"
	key := "wants single pr per feature branch"

	for i := 0; i < MinOccurrencesForUserPref+1; i++ {
		insertPrefObs(t, srv.store, projID, "sess-idem-"+string(rune('A'+i)), key)
	}

	// First run.
	if _, err := runUserPrefExtraction(srv.store, projID); err != nil {
		t.Fatalf("first extraction: %v", err)
	}
	// Second run — same result, no new rows.
	n2, err := runUserPrefExtraction(srv.store, projID)
	if err != nil {
		t.Fatalf("second extraction: %v", err)
	}
	if n2 != 1 {
		t.Errorf("expected 1 preference after second run (idempotent), got %d", n2)
	}

	prefs, err := srv.store.GetProjectUserPreferences(projID, 0)
	if err != nil {
		t.Fatalf("GetProjectUserPreferences: %v", err)
	}
	if len(prefs) != 1 {
		t.Errorf("expected exactly 1 preference after two runs, got %d", len(prefs))
	}
}

// ── maybeRecordUserPrefObs integration ────────────────────────────────────────

// TestMaybeRecordUserPrefObs_WritesToStore verifies that a memory(action=save)
// call whose decision text contains a preference signal creates a
// user_preference session observation in the store.
//
// This test exercises the full wiring path:
//
//	handleMemoryDispatch(action=save) → maybeRecordUserPrefObs → InsertSessionObservation
//
// It calls handleSessionInit first so that a Synapses session UUID is
// registered under the "stdio" key — required by InsertSessionObservation.
func TestMaybeRecordUserPrefObs_WritesToStore(t *testing.T) {
	srv := newTestServer(t)
	const projID = "proj-maybe-pref"
	srv.SetProjectID(projID)

	// Register a Synapses session so getSynapseSessionID returns a non-empty ID.
	_, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "implementer",
	}))
	if err != nil {
		t.Fatalf("handleSessionInit: %v", err)
	}

	// Save a memory whose decision text contains a preference signal.
	res, err := srv.handleMemoryDispatch(ctx, callTool(map[string]any{
		"action":   "save",
		"agent_id": "implementer",
		"decision": "User prefers single bundled PRs for all refactors in this sprint.",
		"outcome":  "success",
	}))
	if err != nil {
		t.Fatalf("handleMemoryDispatch save: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("handleMemoryDispatch save returned tool error: %v", res.Content)
	}

	// Verify the observation was recorded.
	counts, err := srv.store.GetObservationKeyCounts(projID, store.ObsCategoryUserPref)
	if err != nil {
		t.Fatalf("GetObservationKeyCounts: %v", err)
	}
	if len(counts) == 0 {
		t.Fatal("expected at least 1 user_preference observation, got 0")
	}

	// Confirm the normalized key contains the expected phrase prefix.
	found := false
	for k := range counts {
		if strings.Contains(k, "single bundled prs") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected observation key containing 'single bundled prs', got keys: %v", counts)
	}
}
