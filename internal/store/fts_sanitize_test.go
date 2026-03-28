package store

import (
	"strings"
	"testing"
)

// TestSanitizeFTSQuery verifies that sanitizeFTSQuery produces valid FTS5
// expressions and never passes operator keywords through unquoted.
func TestSanitizeFTSQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input    string
		wantNone bool // true if expected to return ""
		// wantContains is checked when wantNone=false
		wantContains []string
		// wantNotContains ensures dangerous tokens aren't bare operators
		wantNotContains []string
	}{
		{
			input:    "",
			wantNone: true,
		},
		{
			input:    "   ",
			wantNone: true,
		},
		{
			// All special chars only — should reduce to empty
			input:    `* " ( ) : - / .`,
			wantNone: true,
		},
		{
			input:        "auth",
			wantContains: []string{`"auth"`},
		},
		{
			input:        "auth token",
			wantContains: []string{`"auth"*`, `"token"*`, "OR"},
		},
		{
			// FTS5 keyword: NOT must not appear as bare unquoted operator
			input:           "NOT *",
			wantNotContains: []string{"NOT *", "NOT OR"},
			// After stripping '*': "NOT" → quoted → "NOT"* OR pattern
		},
		{
			// FTS5 keywords: OR AND NEAR must be quoted
			input:           "OR AND NEAR",
			wantNotContains: []string{"OR OR ", "AND OR ", "NEAR OR "},
		},
		{
			// Short words (≤2) get no prefix wildcard
			input:        "go is",
			wantContains: []string{`"go"`, `"is"`},
			// Must NOT contain "go*" or "is*"
			wantNotContains: []string{`"go"*`, `"is"*`},
		},
		{
			// Backslash injection attempt
			input:           `\x00null`,
			wantNotContains: []string{`\`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := sanitizeFTSQuery(tc.input)

			if tc.wantNone {
				if got != "" {
					t.Errorf("sanitizeFTSQuery(%q) = %q, want empty", tc.input, got)
				}
				return
			}

			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("sanitizeFTSQuery(%q) = %q, want to contain %q", tc.input, got, want)
				}
			}
			for _, notWant := range tc.wantNotContains {
				if strings.Contains(got, notWant) {
					t.Errorf("sanitizeFTSQuery(%q) = %q, must NOT contain %q", tc.input, got, notWant)
				}
			}
		})
	}
}

// TestBuildORQuery_ShortWordFallback is the critical regression test.
// When ALL query words are ≤3 chars, buildORQuery falls back to including them.
// Before the fix, this fallback included "OR" separator tokens from
// sanitizeFTSQuery's output, producing invalid FTS5 like `"go" OR OR OR "is"`.
func TestBuildORQuery_ShortWordFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		// wantValid: if true, the output must be usable as FTS5 MATCH without error
		wantValid bool
		wantEmpty bool
	}{
		{"go is it", true, false},   // all ≤2 chars: fallback triggered
		{"the a if", true, false},   // mix of short words
		{"OR AND NOT", true, false}, // FTS5 keywords as query terms — must not produce `OR OR AND OR NOT`
		{"", false, true},
		{"change auth token logic", true, false}, // normal long words — no fallback
		{"abc", true, false},                     // 3-char word: len("abc")=3, not > 3, fallback triggered
		{"abcd", true, false},                    // 4-char word: kept normally
	}

	st := openMemTestStore(t)
	// Seed one episode so CheckPlanSafety can actually run the FTS5 query.
	_, _ = st.RememberEpisode(Episode{
		AgentID:     "test",
		ProjectID:   "proj",
		EpisodeType: "failure",
		Outcome:     "failed",
		Trigger:     "auth service broke after token validation change",
		Decision:    "rollback auth changes",
		Tags:        "[]",
	})

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := buildORQuery(tc.input)

			if tc.wantEmpty {
				if got != "" {
					t.Errorf("buildORQuery(%q) = %q, want empty", tc.input, got)
				}
				return
			}

			if tc.wantValid {
				if got == "" {
					// Empty is acceptable for edge cases, skip FTS5 validity check
					return
				}
				// The key assertion: no double-OR patterns which indicate broken output
				if strings.Contains(got, "OR OR") {
					t.Errorf("buildORQuery(%q) = %q, contains 'OR OR' — invalid FTS5 (fallback bug)", tc.input, got)
				}
				// Verify it's actually valid FTS5 by running it through CheckPlanSafety.
				// If CheckPlanSafety returns an error, the query was invalid FTS5.
				_, err := st.CheckPlanSafety(tc.input, "proj")
				if err != nil {
					t.Errorf("CheckPlanSafety(%q) returned error (invalid FTS5 from buildORQuery): %v", tc.input, err)
				}
			}
		})
	}
}
