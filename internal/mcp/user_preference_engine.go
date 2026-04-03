package mcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
)

// MinOccurrencesForUserPref is the minimum number of user_preference session
// observations that must share the same normalized key before it is promoted
// to a UserPreference. Two occurrences means the preference was observed in at
// least 2 distinct memory saves (across sessions or the same session), giving
// a reliable signal that this is a genuine stable preference.
const MinOccurrencesForUserPref = 2

// maxPrefKeyRunes is the maximum rune length of a normalized preference key.
// Long enough to be meaningful but short enough to be a stable dictionary key.
const maxPrefKeyRunes = 60

// prefSignalPatterns is the ordered list of compiled regexes for detecting
// preference signal phrases in memory content. Each pattern must match text
// that starts with a user-preference indicator and captures the preference
// description that follows.
//
// Capture group 1 is the raw preference description (before normalization).
// The capture group matches up to 150 chars and stops at a sentence boundary
// (period or newline) to avoid capturing multiple sentences in one match.
var prefSignalPatterns = []*regexp.Regexp{
	// "User prefers X" / "User prefer X"
	regexp.MustCompile(`(?i)\buser prefer[s]?\s+([^.\n]{10,150})`),
	// "User wants X" / "User want X"
	regexp.MustCompile(`(?i)\buser want[s]?\s+([^.\n]{10,150})`),
	// "User likes X" / "User like X"
	regexp.MustCompile(`(?i)\buser like[s]?\s+([^.\n]{10,150})`),
	// "User chose X"
	regexp.MustCompile(`(?i)\buser chose\s+([^.\n]{10,150})`),
	// "User confirmed X" / "User confirms X"
	regexp.MustCompile(`(?i)\buser confirm[s]?(?:ed)?\s+([^.\n]{10,150})`),
	// "User corrected: X" / "User corrects X" / "User corrected X"
	regexp.MustCompile(`(?i)\buser correct[s]?(?:ed)?:?\s+([^.\n]{10,150})`),
	// "User asked for X" / "User asks for X"
	regexp.MustCompile(`(?i)\buser ask[s]?(?:ed)? for\s+([^.\n]{10,150})`),
}

// extractPrefSignals returns a slice of normalized preference keys extracted
// from memory content. Each element is the result of applying normalizeUserPrefKey
// to a match captured by one of the prefSignalPatterns.
//
// Returns nil when no signals are detected (not an error).
func extractPrefSignals(content string) []string {
	if content == "" {
		return nil
	}
	// Use a set to avoid returning the same key twice from the same memory
	// (e.g., a memory that mentions the same preference in two sentences).
	seen := make(map[string]bool)
	var keys []string
	for _, re := range prefSignalPatterns {
		// FindAllStringSubmatch captures every occurrence of the pattern in the
		// content — a single memory may mention the same preference type multiple
		// times ("User prefers X. Also user prefers Y.").
		all := re.FindAllStringSubmatch(content, -1)
		for _, m := range all {
			if len(m) < 2 {
				continue
			}
			key := normalizeUserPrefKey(m[1])
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

// normalizeUserPrefKey converts a raw preference phrase into a stable,
// lowercase key of at most maxPrefKeyRunes runes. The normalization collapses
// leading/trailing whitespace and control characters, then lowercases the
// string, and finally truncates to the key length limit.
//
// Returns "" if the result would be empty after stripping.
func normalizeUserPrefKey(phrase string) string {
	phrase = strings.TrimFunc(phrase, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
	if phrase == "" {
		return ""
	}
	// Collapse internal whitespace runs to single spaces for stability.
	phrase = strings.Join(strings.Fields(phrase), " ")
	// Lowercase for case-insensitive matching across sessions.
	phrase = strings.ToLower(phrase)
	// Truncate to maxPrefKeyRunes runes — keys must be stable identifiers.
	runes := []rune(phrase)
	if len(runes) > maxPrefKeyRunes {
		phrase = string(runes[:maxPrefKeyRunes])
	}
	return phrase
}

// userPrefText returns the natural-language preference string for session_init
// delivery. Includes the occurrence count so the agent understands signal strength.
func userPrefText(prefKey string, count int) string {
	times := "times"
	if count == 1 {
		times = "time"
	}
	// Capitalize first letter of key for readability in the briefing.
	text := prefKey
	if len(prefKey) > 0 {
		runes := []rune(prefKey)
		runes[0] = unicode.ToUpper(runes[0])
		text = string(runes)
	}
	return fmt.Sprintf("User %s (observed %d %s).", text, count, times)
}

// userPrefConfidence maps an occurrence count to a confidence score.
// Confidence increases monotonically with each additional confirming observation.
func userPrefConfidence(count int) float64 {
	switch {
	case count >= 8:
		return 0.95
	case count >= 6:
		return 0.90
	case count >= 5:
		return 0.85
	case count >= 4:
		return 0.80
	case count >= 3:
		return 0.70
	default: // 2 — minimum threshold
		return 0.60
	}
}

// maybeRecordUserPrefObs checks the decision text of a memory(action=save)
// call for user preference signals. For each signal detected, it inserts a
// user_preference session observation so the cross-session extraction engine
// can aggregate preference counts without relying on memory row counts (which
// are affected by dedup merging).
//
// This is called from handleMemoryDispatch immediately after a successful
// memory save. It is a Tier 1 server-side auto-capture: the agent has no
// awareness that this happens. Uses fire-and-forget semantics — errors are
// silently dropped to avoid interfering with the memory save result.
func (s *Server) maybeRecordUserPrefObs(ctx context.Context, req mcp.CallToolRequest) {
	if s.store == nil || s.projectID == "" {
		return
	}

	// The decision parameter carries the main content of a memory save.
	decision := stringArg(req, "decision")
	if decision == "" {
		return
	}

	keys := extractPrefSignals(decision)
	if len(keys) == 0 {
		return
	}

	agentID := stringArg(req, "agent_id")
	if agentID == "" {
		agentID = "unknown"
	}

	// Get the current Synapses session ID so observations are properly session-scoped.
	mcpSessID := SessionIDFromContext(ctx)
	sessID := s.getSynapseSessionID(mcpSessID)
	now := time.Now().UTC().Unix()

	for _, key := range keys {
		obs := store.SessionObservation{
			SessionID: sessID,
			ProjectID: s.projectID,
			AgentID:   agentID,
			Category:  store.ObsCategoryUserPref,
			Key:       key,
			Value:     "",
			// Confidence starts at 0.5 per individual observation; aggregation
			// across sessions raises the effective confidence.
			Confidence: 0.5,
			CreatedAt:  now,
		}
		// Ignore errors — this is best-effort enrichment. The memory save
		// already succeeded; a missing observation is not an error.
		_, _ = s.store.InsertSessionObservation(obs)
	}
}

// runUserPrefExtraction aggregates user_preference session_observations for
// the project and promotes recurring preference keys (≥ MinOccurrencesForUserPref
// observations) to UserPreference records.
//
// Each memory(action=save) call containing a preference signal generates one
// observation via maybeRecordUserPrefObs. Multiple saves across sessions
// accumulate multiple observations for the same key. This approach is immune
// to memory dedup — observations are separate from memory rows.
//
// Returns the number of preferences upserted (new + updated). Runs
// synchronously at end_session as a Tier 1 auto-capture operation — no agent
// action beyond saving memories is required.
//
// Empty projectID is a no-op and returns (0, nil).
func runUserPrefExtraction(st *store.Store, projectID string) (int, error) {
	if st == nil || projectID == "" {
		return 0, nil
	}

	counts, err := st.GetObservationKeyCounts(projectID, store.ObsCategoryUserPref)
	if err != nil {
		return 0, fmt.Errorf("user pref extraction: get observation counts: %w", err)
	}
	if len(counts) == 0 {
		return 0, nil
	}

	total := 0
	for key, count := range counts {
		if count < MinOccurrencesForUserPref {
			continue
		}
		text := userPrefText(key, count)
		up := store.UserPreference{
			ID:              store.UserPreferenceID(projectID, key),
			ProjectID:       projectID,
			PrefKey:         key,
			Text:            text,
			OccurrenceCount: count,
			Confidence:      userPrefConfidence(count),
		}
		if err := st.UpsertUserPreference(up); err != nil {
			return total, fmt.Errorf("user pref extraction: upsert %q: %w", key, err)
		}
		total++
	}
	return total, nil
}
