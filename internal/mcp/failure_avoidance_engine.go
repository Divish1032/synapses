package mcp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/store"
)

// MinOccurrencesForFailurePattern is the minimum number of rejected_approach
// records that must share the same keyword before a FailurePattern is promoted.
// A pattern tried and abandoned twice is a genuine signal — not a one-off.
const MinOccurrencesForFailurePattern = 2

// maxRejectedApproachesToScan is the number of project-wide rejected_approach
// records read by the extractor. Keeps extraction O(N) with a bounded N.
const maxRejectedApproachesToScan = 200

// hyphenatedPkgRE matches hyphenated package/library names: lowercase letter,
// followed by lowercase alphanumerics, a hyphen, then at least one lowercase
// letter (prevents matching version strings like "v3" or plain adjectives).
// Anchored with \b to avoid partial matches inside longer identifiers.
var hyphenatedPkgRE = regexp.MustCompile(`\b[a-z][a-z0-9]*-[a-z][a-z0-9]*(?:-[a-z][a-z0-9]*)?\b`)

// hyphenatedStopList is a set of common hyphenated words that are definitely
// not library or package names. Prevents false positives from approach text
// like "read-only", "long-term", "step-by-step".
var hyphenatedStopList = map[string]bool{
	"read-only":      true,
	"long-term":      true,
	"step-by-step":   true,
	"up-to-date":     true,
	"well-known":     true,
	"built-in":       true,
	"trade-off":      true,
	"end-to-end":     true,
	"follow-up":      true,
	"out-of-the-box": true,
	"in-memory":      true,
	"out-of-date":    true,
	"day-to-day":     true,
	"roll-back":      true,
	"fall-back":      true,
	"set-up":         true,
}

// runFailurePatternExtraction reads all rejected_approach records for the project
// and promotes recurring keyword patterns to ExtractedFailurePattern records.
//
// A pattern is promoted when the same keyword (library name or hyphenated package
// name) appears in ≥ MinOccurrencesForFailurePattern rejected_approach records.
//
// Returns the number of patterns upserted (new + updated). Runs synchronously at
// end_session as a Tier 1 auto-capture operation — no agent action required. Cost
// is O(rejected_approach rows for project), capped at maxRejectedApproachesToScan.
//
// Empty projectID is a no-op and returns (0, nil).
func runFailurePatternExtraction(st *store.Store, projectID string) (int, error) {
	if st == nil || projectID == "" {
		return 0, nil
	}

	// Read project-wide rejected approaches (all agents, up to cap).
	approaches, err := st.SearchRejectedApproaches("", projectID, "", maxRejectedApproachesToScan)
	if err != nil {
		return 0, fmt.Errorf("failure pattern extraction: read rejected approaches: %w", err)
	}
	if len(approaches) == 0 {
		return 0, nil
	}

	// keyword → matched records accumulator.
	type match struct {
		keyword     string
		patternType string
		records     []store.RejectedApproach // for sample text selection
	}
	matches := make(map[string]*match)

	addMatch := func(keyword, ptype string, r store.RejectedApproach) {
		if m, ok := matches[keyword]; ok {
			m.records = append(m.records, r)
		} else {
			matches[keyword] = &match{
				keyword:     keyword,
				patternType: ptype,
				records:     []store.RejectedApproach{r},
			}
		}
	}

	for _, r := range approaches {
		// Combine approach + blocker for keyword scanning (lowercased for
		// case-insensitive token extraction).
		text := strings.ToLower(r.Approach + " " + r.Blocker)

		// Extract hyphenated package/library names (e.g., "jwt-go", "chi-router",
		// "gin-gonic"). The regex matches names directly from the approach text
		// so the keyword is human-readable ("jwt-go" not "uses_jwt_go").
		//
		// Pattern: starts with a lowercase letter, followed by lowercase
		// alphanumerics, a hyphen, at least one more lowercase letter segment.
		// This naturally catches library names like "jwt-go", "gin-gonic",
		// "gorilla-mux", "react-router" while filtering version strings ("v3")
		// and generic adjectives ("well-known" → in stop list).
		seen := make(map[string]bool)
		for _, tok := range hyphenatedPkgRE.FindAllString(text, -1) {
			if hyphenatedStopList[tok] || seen[tok] {
				continue
			}
			seen[tok] = true
			// Classify as "library" when the token appears in wellKnownLibraries,
			// otherwise as "package". This distinction informs the confidence level
			// in a future enhancement but both generate the same NL warning for now.
			ptype := "package"
			for _, lib := range wellKnownLibraries {
				if strings.Contains(lib.contains, tok) || strings.Contains(tok, strings.TrimLeft(lib.contains, "/")) {
					ptype = "library"
					break
				}
			}
			addMatch(tok, ptype, r)
		}
	}

	total := 0
	for keyword, m := range matches {
		count := len(m.records)
		if count < MinOccurrencesForFailurePattern {
			continue
		}

		// Pick the most recent record (records come from the store ordered
		// by created_at DESC, so records[0] is most recent).
		sample := m.records[0]
		sampleApproach := truncate(sample.Approach, 200)
		sampleReason := truncate(sample.FailureReason, 200)

		text := failurePatternText(keyword, count, sampleReason)
		fp := store.FailurePattern{
			ID:              store.FailurePatternID(projectID, keyword),
			ProjectID:       projectID,
			Keyword:         keyword,
			PatternType:     m.patternType,
			OccurrenceCount: count,
			SampleApproach:  sampleApproach,
			SampleReason:    sampleReason,
			Confidence:      failurePatternConfidence(count),
			Text:            text,
		}
		if err := st.UpsertFailurePattern(fp); err != nil {
			return total, fmt.Errorf("failure pattern extraction: upsert %q: %w", keyword, err)
		}
		total++
	}
	return total, nil
}

// failurePatternText returns the natural-language warning string for a failure
// pattern. Format: "'<keyword>' was tried <N> time(s) and abandoned: <reason>."
func failurePatternText(keyword string, count int, reason string) string {
	times := "times"
	if count == 1 {
		times = "time"
	}
	r := strings.TrimRight(strings.TrimSpace(reason), ".")
	if r == "" {
		return fmt.Sprintf("'%s' was tried %d %s and abandoned.", keyword, count, times)
	}
	return fmt.Sprintf("'%s' was tried %d %s and abandoned: %s.", keyword, count, times, r)
}

// failurePatternConfidence maps occurrence count to a confidence score.
// Two occurrences is the threshold — each additional reinforces the pattern.
func failurePatternConfidence(count int) float64 {
	switch {
	case count >= 6:
		return 0.95
	case count >= 5:
		return 0.90
	case count >= 4:
		return 0.80
	case count >= 3:
		return 0.70
	default: // 2 — minimum threshold
		return 0.60
	}
}

