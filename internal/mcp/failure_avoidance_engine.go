package mcp

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// MinOccurrencesForFailurePattern is the minimum number of rejected_approach
// records that must share the same keyword before a FailurePattern is promoted.
// A pattern tried and abandoned twice is a genuine signal — not a one-off.
const MinOccurrencesForFailurePattern = 2

// maxRejectedApproachesToScan is the number of project-wide rejected_approach
// records read by the extractor. Keeps extraction O(N) with a bounded N.
const maxRejectedApproachesToScan = 200

// ─────────────────────────────────────────────────────────────────────────────
// Strategy 1: Hyphenated package/library names
// ─────────────────────────────────────────────────────────────────────────────

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

// ─────────────────────────────────────────────────────────────────────────────
// Strategy 2: Backtick-quoted identifiers (function/method/type names)
// ─────────────────────────────────────────────────────────────────────────────

// backtickIdentRE extracts identifiers wrapped in backticks (e.g. `AuthMiddleware`,
// `parseUserID`). Backtick quotes are an explicit code signal — the author is
// specifically marking a code entity — so precision is high and false positives
// are rare. Minimum 4 characters to exclude noise like `ok`, `err`, `id`.
// No spaces or punctuation allowed inside — prevents matching shell commands like
// `go build ./...` or `git commit -m`.
var backtickIdentRE = regexp.MustCompile("`([A-Za-z][A-Za-z0-9_]{3,})`")

// backtickStopList filters identifiers that appear in backticks but are language
// keywords or ubiquitous builtins, not project-specific function or type names.
// Stored and checked in lowercase — keyword extraction normalises to lowercase.
var backtickStopList = map[string]bool{
	// Go builtins / keywords
	"error": true, "string": true, "bool": true, "byte": true, "rune": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
	"uintptr": true, "make": true, "append": true, "delete": true, "copy": true,
	"close": true, "panic": true, "recover": true, "print": true, "println": true,
	"true": true, "false": true, "nil": true, "iota": true,
	"struct": true, "interface": true, "func": true, "type": true, "chan": true,
	"range": true, "select": true, "switch": true, "return": true, "break": true,
	"continue": true, "goto": true, "defer": true, "import": true, "package": true,
	"const": true, "var": true, "fallthrough": true, "default": true, "case": true,
	"else": true, "for": true, "if": true, "map": true, "slice": true, "new": true,
	// Common Go stdlib identifiers too generic to signal a project pattern
	"context": true, "http": true, "json": true, "sync": true, "time": true,
	"bytes": true, "bufio": true, "fmt": true, "log": true, "math": true,
	"sort": true, "path": true, "strconv": true, "strings": true, "unicode": true,
	// Common test identifiers
	"testing": true, "testify": true, "assert": true, "require": true,
	// Python builtins / keywords
	"self": true, "none": true, "class": true, "lambda": true, "yield": true,
	"async": true, "await": true, "pass": true, "with": true, "from": true,
	// JS/TS keywords
	"this": true, "then": true, "catch": true, "finally": true, "typeof": true,
	"instanceof": true, "void": true, "function": true, "prototype": true,
}

// ─────────────────────────────────────────────────────────────────────────────
// Strategy 3: Known runtime/library error patterns
// ─────────────────────────────────────────────────────────────────────────────

// knownErrorPatterns maps recurring runtime error substrings to stable
// hyphenated keywords. When a Blocker field contains the error phrase
// (case-insensitive), the corresponding keyword is promoted as an
// "error_pattern" FailurePattern. Normalized to hyphenated form so patterns
// accumulate reliably across sessions even when the full error message varies
// (e.g., different file paths, line numbers, or goroutine IDs are stripped).
//
// Ordered: more specific entries first (a blocker may contain both "nil pointer
// dereference" and "runtime error" — we want the more specific match).
var knownErrorPatterns = []struct {
	contains string // substring to search (matched case-insensitively in blocker text)
	keyword  string // stable hyphenated keyword stored as FailurePattern.Keyword
}{
	// Go runtime panics — specific first
	{"nil pointer dereference", "nil-pointer-dereference"},
	{"index out of range", "index-out-of-range"},
	{"concurrent map read", "concurrent-map-access"},
	{"send on closed channel", "send-on-closed-channel"},
	{"all goroutines are asleep", "goroutine-deadlock"},
	{"deadlock detected", "goroutine-deadlock"},  // alternate phrasing → same keyword
	{"slice bounds out of range", "slice-bounds-out-of-range"},
	{"interface conversion", "interface-type-assertion-failed"},
	// Go context / network
	{"context deadline exceeded", "context-deadline-exceeded"},
	{"context canceled", "context-canceled"},
	{"connection refused", "connection-refused"},
	{"connection reset by peer", "connection-reset"},
	{"no such host", "dns-resolution-failed"},
	{"tls: certificate signed by unknown authority", "tls-cert-unknown-authority"},
	{"tls handshake timeout", "tls-handshake-timeout"},
	// Go filesystem
	{"permission denied", "permission-denied"},
	{"no such file or directory", "file-not-found"},
	{"too many open files", "too-many-open-files"},
	// Go SQL / DB
	{"sql: no rows in result set", "sql-no-rows"},
	{"sql: transaction has already been committed or rolled back", "sql-txn-closed"},
	{"driver: bad connection", "db-bad-connection"},
	// Generic runtime
	{"stack overflow", "stack-overflow"},
	{"out of memory", "out-of-memory"},
	{"runtime error", "go-runtime-error"}, // catch-all — least specific, last
}

// ─────────────────────────────────────────────────────────────────────────────
// Core extraction engine
// ─────────────────────────────────────────────────────────────────────────────

// runFailurePatternExtraction reads all rejected_approach records for the project
// and promotes recurring keyword patterns to ExtractedFailurePattern records.
//
// Three extraction strategies run against each record:
//
//  1. Hyphenated library/package names (e.g. "jwt-go", "chi-router", "gin-gonic")
//     — matched by regex on lowercased approach+blocker text.
//  2. Backtick-quoted identifiers (e.g. `AuthMiddleware`, `parseUserID`)
//     — matched by backtick regex on original approach text. High precision:
//     backticks are explicit code markers, not prose.
//  3. Known runtime error normalization (e.g. "nil pointer dereference" →
//     "nil-pointer-dereference") — matched against the Blocker field to produce
//     stable hyphenated keywords that accumulate cross-session.
//
// A pattern is promoted when the same keyword appears in
// ≥ MinOccurrencesForFailurePattern rejected_approach records.
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
		// seen deduplicates keywords within a single record so one record cannot
		// inflate the occurrence count for any keyword more than once.
		seen := make(map[string]bool)

		// ── Strategy 1: Hyphenated library/package names ─────────────────────
		// Lowercased combined text — library names are case-insensitive.
		lowText := strings.ToLower(r.Approach + " " + r.Blocker)
		for _, tok := range hyphenatedPkgRE.FindAllString(lowText, -1) {
			if hyphenatedStopList[tok] || seen[tok] {
				continue
			}
			seen[tok] = true
			addMatch(tok, classifyLibraryType(tok), r)
		}

		// ── Strategy 2: Backtick-quoted identifiers ───────────────────────────
		// Applied to ORIGINAL (non-lowercased) text — backtick content is
		// case-sensitive code. Keyword stored lowercase for consistent accumulation
		// across sessions (Go PascalCase, Python snake_case, JS camelCase are all
		// reduced to the same key regardless of how they were formatted).
		origText := r.Approach + " " + r.Blocker
		for _, sub := range backtickIdentRE.FindAllStringSubmatch(origText, -1) {
			lower := strings.ToLower(sub[1])
			if backtickStopList[lower] || seen[lower] {
				continue
			}
			seen[lower] = true
			addMatch(lower, "function", r)
		}

		// ── Strategy 3: Known runtime error normalization ─────────────────────
		// Scans the Blocker field (which holds specific error/exception text)
		// for known multi-word error phrases and maps them to stable hyphenated
		// keywords. Ordered most-specific-first so "nil pointer dereference"
		// wins over the catch-all "runtime error".
		lowerBlocker := strings.ToLower(r.Blocker)
		if lowerBlocker != "" {
			for _, ep := range knownErrorPatterns {
				if strings.Contains(lowerBlocker, ep.contains) && !seen[ep.keyword] {
					seen[ep.keyword] = true
					addMatch(ep.keyword, "error_pattern", r)
					// First match wins per record — prevents one blocker from
					// adding both "nil-pointer-dereference" and the catch-all
					// "go-runtime-error" to the same record's keyword set.
					break
				}
			}
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

		// Collect all unique failure reasons across contributing records.
		// Agents benefit from seeing the full spread of failure modes, not
		// just one sample — especially when a keyword appears 3-5 times with
		// different root causes.
		reasonsSeen := make(map[string]bool)
		var allReasons []string
		for _, rec := range m.records {
			r := strings.TrimSpace(rec.FailureReason)
			if r == "" || reasonsSeen[r] {
				continue
			}
			reasonsSeen[r] = true
			allReasons = append(allReasons, r)
		}

		text := failurePatternText(keyword, m.patternType, count, allReasons)
		fp := store.FailurePattern{
			ID:                  store.FailurePatternID(projectID, keyword),
			ProjectID:           projectID,
			Keyword:             keyword,
			PatternType:         m.patternType,
			OccurrenceCount:     count,
			SampleApproach:      sampleApproach,
			SampleReason:        sampleReason,
			Confidence:          failurePatternConfidence(count),
			Text:                text,
			LastRecordCreatedAt: sample.CreatedAt,
		}
		if err := st.UpsertFailurePattern(fp); err != nil {
			return total, fmt.Errorf("failure pattern extraction: upsert %q: %w", keyword, err)
		}
		total++
	}
	return total, nil
}

// failurePatternMatchesEntity returns true when a failure pattern is relevant
// to the entity currently being queried via get_context. Used to surface
// targeted warnings at the exact moment an agent investigates a potentially
// problematic entity, rather than relying solely on session_init bulk delivery.
//
// Matching rules (applied in priority order):
//  1. Exact match: fp.Keyword == entityNameLower — always relevant.
//  2. Contains match: entityNameLower contains the keyword — catches import
//     paths like "github.com/dgrijalva/jwt-go" matching keyword "jwt-go".
//     Keyword must be ≥ 6 chars to prevent short tokens like "gin" matching
//     unrelated entity names like "origin" or "engine".
//
// error_pattern entries are excluded — nobody queries "nil-pointer-dereference"
// as a get_context entity, and noise would harm signal quality.
func failurePatternMatchesEntity(fp store.FailurePattern, entityNameLower string) bool {
	if fp.PatternType == "error_pattern" {
		return false
	}
	if fp.Keyword == entityNameLower {
		return true
	}
	// Contains match: only for longer keywords to avoid false positives.
	if len(fp.Keyword) >= 6 && strings.Contains(entityNameLower, fp.Keyword) {
		return true
	}
	return false
}

// relativeAge returns a human-readable string describing how long ago a Unix
// timestamp occurred (e.g. "last seen 3 days ago"). Returns "" for zero/negative
// timestamps. Used to add recency context to failure pattern warnings at
// delivery time — not stored in the pattern's Text field to avoid staleness.
func relativeAge(unixSec int64) string {
	if unixSec <= 0 {
		return ""
	}
	d := time.Since(time.Unix(unixSec, 0))
	switch {
	case d < 2*24*time.Hour:
		return "last seen today"
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "last seen 1 day ago"
		}
		return fmt.Sprintf("last seen %d days ago", days)
	case d < 30*24*time.Hour:
		weeks := int(d.Hours() / 24 / 7)
		if weeks == 1 {
			return "last seen 1 week ago"
		}
		return fmt.Sprintf("last seen %d weeks ago", weeks)
	case d < 365*24*time.Hour:
		months := int(d.Hours() / 24 / 30)
		if months == 1 {
			return "last seen 1 month ago"
		}
		return fmt.Sprintf("last seen %d months ago", months)
	default:
		return "last seen over a year ago"
	}
}

// classifyLibraryType returns "library" when tok matches a well-known library
// entry, "package" otherwise. Used for the PatternType field — both types
// produce the same NL warning but the distinction may inform future confidence
// tuning.
func classifyLibraryType(tok string) string {
	for _, lib := range wellKnownLibraries {
		if strings.Contains(lib.contains, tok) || strings.Contains(tok, strings.TrimLeft(lib.contains, "/")) {
			return "library"
		}
	}
	return "package"
}

// failurePatternText returns the natural-language warning string for a failure
// pattern. The phrasing is adapted to the pattern type so each category reads
// naturally:
//
//   - library / package / function: "'jwt-go' was tried 3 times and abandoned: [reason A; reason B]."
//   - error_pattern:                "'nil-pointer-dereference' caused 3 approaches to fail: reason."
//
// reasons is the full set of distinct failure reasons across all contributing
// records. Multiple reasons are joined with "; " so agents see the full spread
// of failure modes rather than a single sample.
func failurePatternText(keyword, patternType string, count int, reasons []string) string {
	times := "times"
	if count == 1 {
		times = "time"
	}

	// Build reason string: trim trailing punctuation, join multiples.
	var reasonStr string
	if len(reasons) == 1 {
		reasonStr = strings.TrimRight(strings.TrimSpace(reasons[0]), ".")
	} else if len(reasons) > 1 {
		parts := make([]string, 0, len(reasons))
		for _, r := range reasons {
			r = strings.TrimRight(strings.TrimSpace(r), ".")
			if r != "" {
				parts = append(parts, r)
			}
		}
		if len(parts) > 0 {
			reasonStr = "[" + strings.Join(parts, "; ") + "]"
		}
	}

	if patternType == "error_pattern" {
		// "Was tried and abandoned" is semantically wrong for runtime errors —
		// agents don't try nil pointer dereferences, they encounter them.
		if reasonStr == "" {
			return fmt.Sprintf("'%s' caused %d %s to fail.", keyword, count, times)
		}
		return fmt.Sprintf("'%s' caused %d approach(es) to fail: %s.", keyword, count, reasonStr)
	}

	// library, package, function — all represent something the agent actively tried.
	if reasonStr == "" {
		return fmt.Sprintf("'%s' was tried %d %s and abandoned.", keyword, count, times)
	}
	return fmt.Sprintf("'%s' was tried %d %s and abandoned: %s.", keyword, count, times, reasonStr)
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
