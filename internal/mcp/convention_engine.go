package mcp

import (
	"fmt"

	"github.com/SynapsesOS/synapses/internal/store"
)

// MinSessionsForConvention is the minimum number of distinct sessions in which
// an observation key must appear before it is promoted to an ExtractedConvention.
// Research basis: a pattern seen in 3+ independent sessions is unlikely to be
// noise — it reflects a genuine project property.
const MinSessionsForConvention = 3

// categoriesToPromote is the set of observation categories from which
// conventions are extracted. approach_outcome and tool_usage are excluded
// because they describe agent-session behavior (productive vs read-only),
// not intrinsic project properties.
var categoriesToPromote = []string{
	store.ObsCategoryTestingPattern,
	store.ObsCategoryLibraryUsage,
	store.ObsCategoryFilePattern,
}

// conventionBaseTexts maps an observation key to its human-readable base
// convention string (before appending the session count). Keys are sourced
// from wellKnownLibraries in session_observations.go and the testing/file
// pattern constants. Any key not in this map is silently skipped — unknown
// keys produce no convention until they are explicitly mapped here.
var conventionBaseTexts = map[string]string{
	// ── Library usage — Go ────────────────────────────────────────────────
	"uses_testify":     "This project uses testify for testing assertions",
	"uses_gomock":      "This project uses gomock for mocking in tests",
	"uses_mockery":     "This project uses mockery for test mocking",
	"uses_chi_router":  "This project uses chi router for HTTP routing",
	"uses_gin_router":  "This project uses Gin for HTTP routing",
	"uses_echo_router": "This project uses Echo for HTTP routing",
	"uses_gorilla_mux": "This project uses gorilla/mux for HTTP routing",
	"uses_fasthttp":    "This project uses fasthttp for high-performance HTTP",
	"uses_golang_jwt":  "This project uses golang-jwt for JWT authentication",
	"uses_jwt_go":      "This project uses jwt-go for JWT authentication",

	// ── Library usage — Python ────────────────────────────────────────────
	"uses_pytest":    "This project uses pytest for testing",
	"uses_unittest":  "This project uses Python unittest for testing",
	"uses_fastapi":   "This project uses FastAPI as the HTTP framework",
	"uses_flask":     "This project uses Flask as the HTTP framework",
	"uses_django":    "This project uses Django as the HTTP framework",

	// ── Library usage — TypeScript/JavaScript ────────────────────────────
	"uses_jest":     "This project uses Jest for testing",
	"uses_vitest":   "This project uses Vitest for testing",
	"uses_mocha":    "This project uses Mocha for testing",
	"uses_express":  "This project uses Express for HTTP routing",
	"uses_fastify":  "This project uses Fastify for HTTP routing",
	"uses_nextjs":   "This project uses Next.js",

	// ── Library usage — Java ─────────────────────────────────────────────
	"uses_spring":  "This project uses Spring Boot",
	"uses_junit":   "This project uses JUnit for testing",
	"uses_mockito": "This project uses Mockito for mocking in tests",

	// ── Library usage — Rust ─────────────────────────────────────────────
	"uses_actix_web": "This project uses Actix-web for HTTP routing",
	"uses_axum":      "This project uses Axum for HTTP routing",
	"uses_tokio":     "This project uses Tokio for async runtime",

	// ── Testing patterns ──────────────────────────────────────────────────
	"go_test_files_touched":   "This project actively writes Go tests",
	"ts_test_files_touched":   "This project actively writes TypeScript/JavaScript tests",
	"py_test_files_touched":   "This project actively writes Python tests",
	"java_test_files_touched": "This project actively writes Java tests",
	"rust_test_files_touched": "This project actively writes Rust tests",

	// ── File / architectural patterns ─────────────────────────────────────
	"layered_architecture_touched": "This project follows layered architecture (handler/service/repository)",
	"handler_service_touched":      "This project separates handler and service layers",
	"handler_repository_touched":   "This project separates handler and repository layers",
	"middleware_files_touched":      "This project uses middleware components",
}

// conventionText returns the natural-language convention string for a given
// observation category+key. When fileCount > 0, it includes file-level evidence
// so the agent understands how universal the pattern is across the codebase
// (e.g., "detected in 14+ files" signals a dominant pattern vs "1 file" which
// suggests it's rare). Returns "" for any key not found in conventionBaseTexts.
func conventionText(_, key string, fileCount, sessionCount int) string {
	base, ok := conventionBaseTexts[key]
	if !ok {
		return ""
	}
	if fileCount > 0 {
		// "14+" is intentionally conservative: fileCount is the max seen in a
		// single session's touched files (capped at maxLibraryFileScan=20), not
		// the total distinct project files. The "+" signals a lower bound.
		return fmt.Sprintf("%s (detected in %d+ files, confirmed across %d sessions).", base, fileCount, sessionCount)
	}
	return fmt.Sprintf("%s (confirmed across %d sessions).", base, sessionCount)
}

// conventionConfidence maps a session count to a confidence score.
// Confidence increases monotonically: each additional confirming session
// raises the score. The minimum qualifying count is MinSessionsForConvention.
//
// TODO(29.6): Add a violation penalty path — if the user explicitly corrects
// a convention or the pattern stops appearing in recent sessions, decrease
// confidence. This requires Sprint 29.6 (user preference tracking).
func conventionConfidence(sessionCount int) float64 {
	switch {
	case sessionCount >= 10:
		return 0.95
	case sessionCount >= 7:
		return 0.90
	case sessionCount >= 5:
		return 0.80
	case sessionCount >= 4:
		return 0.70
	default: // 3 — minimum threshold
		return 0.60
	}
}

// runConventionExtraction aggregates session_observations for the project
// across all promotable categories, identifies keys that appear in ≥
// MinSessionsForConvention distinct sessions, and upserts them as
// ExtractedConvention records in the store.
//
// Returns the number of conventions upserted (new + updated). Runs
// synchronously at end_session as a Tier 1 auto-capture operation: no agent
// action is required. The cost is O(observation_rows_for_project) in SQL
// aggregates — no graph traversal, no LLM calls.
//
// Empty projectID is a no-op and returns (0, nil). Store unavailability
// (nil knowledgeDB) returns (0, nil) via the store nil-guard in UpsertConvention.
func runConventionExtraction(st *store.Store, projectID string) (int, error) {
	if st == nil || projectID == "" {
		return 0, nil
	}

	total := 0
	for _, cat := range categoriesToPromote {
		counts, err := st.GetObservationKeyCounts(projectID, cat)
		if err != nil {
			return total, fmt.Errorf("convention extraction: get key counts (%s): %w", cat, err)
		}

		// Fetch per-key file counts stored in the Value field by the session
		// observation pipeline. A missing or empty result means this category
		// doesn't track file counts (testing/file-pattern observations don't) —
		// in that case fileCount falls back to 0 and the text omits the file clause.
		fileCounts, _ := st.GetObservationKeyMaxValue(projectID, cat)

		for key, count := range counts {
			if count < MinSessionsForConvention {
				continue
			}
			text := conventionText(cat, key, fileCounts[key], count)
			if text == "" {
				// Unknown key — no convention mapped for it yet. Skip silently.
				continue
			}
			conv := store.ExtractedConvention{
				ID:           store.ConventionID(projectID, cat, key),
				ProjectID:    projectID,
				Category:     cat,
				Key:          key,
				SessionCount: count,
				Confidence:   conventionConfidence(count),
				Text:         text,
			}
			if err := st.UpsertConvention(conv); err != nil {
				return total, fmt.Errorf("convention extraction: upsert %q: %w", key, err)
			}
			total++
		}
	}
	return total, nil
}
