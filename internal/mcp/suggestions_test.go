package mcp

import (
	"testing"
)

// ── stripSuffix tests ────────────────────────────────────────────────────────

func TestStripSuffix(t *testing.T) {
	cases := []struct{ input, want string }{
		{"implementing", "implement"},
		{"debugging", "debug"},
		{"planning", "plan"},
		{"running", "run"},
		{"reviewed", "review"},
		{"explored", "explor"}, // -ed strips, result is "explor"
		{"exploring", "explor"},
		{"debugger", "debug"},
		{"investigation", "investig"},
		{"refactoring", "refactor"},
		{"auditing", "audit"},
		{"designing", "design"},
		{"building", "build"},
		{"plan", "plan"},       // no suffix to strip
		{"add", "add"},         // too short after any strip
		{"fix", "fix"},         // no matching suffix
		{"go", "go"},           // too short
		{"", ""},               // empty
		{"carefully", "care"},  // -ful then -ly? no — strips -ely first: "car"? no len<3. Strips -ly: "careful". Then no more.
	}
	// Correct expected values based on actual suffix stripping rules:
	// "carefully" → strips "-ely" suffix → "car" has len 3 ≥ 3 → "car"?
	// Actually let me trace: "carefully" ends with "ely"? "carefully" = c-a-r-e-f-u-l-l-y
	// Suffixes checked: ation, tion, sion, ment, ness, ious, eous, ous, ize, ise, ful, ing, ely, ly, ed, er
	// "carefully" ends with "ly" → stem "careful" (len 7 ≥ 3) → "careful"
	// Re-set expected:
	cases[len(cases)-1] = struct{ input, want string }{"carefully", "careful"}

	for _, tc := range cases {
		got := stripSuffix(tc.input)
		if got != tc.want {
			t.Errorf("stripSuffix(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ── suggestToolsForIntent tests ──────────────────────────────────────────────

func TestSuggestTools_ImplementIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("implementing auth refactor")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'implementing' intent")
	}
	assertContains(t, toolNames(suggestions), "get_impact")
}

func TestSuggestTools_DebugIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("debugging login flow")
	assertContains(t, toolNames(suggestions), "get_call_chain")
}

func TestSuggestTools_ReviewIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("reviewing code quality")
	names := toolNames(suggestions)
	assertContains(t, names, "get_violations")
	assertContains(t, names, "get_gaps")
}

func TestSuggestTools_ExploreIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("exploring the codebase")
	assertContains(t, toolNames(suggestions), "get_repo_map")
}

func TestSuggestTools_PlanIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("planning the architecture")
	assertContains(t, toolNames(suggestions), "get_adrs")
}

func TestSuggestTools_RefactorIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("refactoring auth module")
	assertContains(t, toolNames(suggestions), "get_impact")
}

func TestSuggestTools_FixIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("fix the login bug")
	assertContains(t, toolNames(suggestions), "get_call_chain")
}

func TestSuggestTools_NoIntent(t *testing.T) {
	if suggestions := suggestToolsForIntent(""); suggestions != nil {
		t.Fatalf("expected nil for empty intent, got %d", len(suggestions))
	}
}

func TestSuggestTools_UnknownIntent(t *testing.T) {
	if suggestions := suggestToolsForIntent("something random and unknown"); len(suggestions) != 0 {
		t.Fatalf("expected no suggestions for unknown intent, got %d", len(suggestions))
	}
}

func TestSuggestTools_Deduplication(t *testing.T) {
	suggestions := suggestToolsForIntent("implement and build new feature")
	seen := make(map[string]bool)
	for _, s := range suggestions {
		if seen[s.Tool] {
			t.Fatalf("duplicate tool suggestion: %s", s.Tool)
		}
		seen[s.Tool] = true
	}
}

func TestSuggestTools_MaxFive(t *testing.T) {
	suggestions := suggestToolsForIntent("implement build add debug fix investigate review audit refactor explore understand plan design")
	if len(suggestions) > 5 {
		t.Fatalf("expected max 5 suggestions, got %d", len(suggestions))
	}
}

func TestSuggestTools_StemMatch(t *testing.T) {
	cases := []struct {
		intent       string
		expectedTool string
	}{
		{"implementing auth", "get_impact"},
		{"exploring codebase", "get_repo_map"},
		{"investigated the bug", "get_call_chain"},
		{"reviewed the PR", "get_violations"},
		{"refactoring module", "get_impact"},
		{"designing system", "get_adrs"},
	}
	for _, tc := range cases {
		suggestions := suggestToolsForIntent(tc.intent)
		if len(suggestions) == 0 {
			t.Errorf("intent %q: no suggestions", tc.intent)
			continue
		}
		assertContains(t, toolNames(suggestions), tc.expectedTool)
	}
}

func TestSuggestTools_Deterministic(t *testing.T) {
	// Run 100 times — result must be identical every time.
	first := suggestToolsForIntent("implementing and exploring")
	for i := 0; i < 100; i++ {
		got := suggestToolsForIntent("implementing and exploring")
		if len(got) != len(first) {
			t.Fatalf("iteration %d: length %d != %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j].Tool != first[j].Tool {
				t.Fatalf("iteration %d: tool[%d] = %s, want %s", i, j, got[j].Tool, first[j].Tool)
			}
		}
	}
}

func TestSuggestTools_NoFalsePositive_Address(t *testing.T) {
	// "address" must NOT match "add" — old stemMatch 4-char prefix would match.
	// stripSuffix("address") = "address" (no matching suffix), so it won't match "add".
	suggestions := suggestToolsForIntent("address the concern")
	if len(suggestions) != 0 {
		t.Errorf("expected no suggestions for 'address', got %v", toolNames(suggestions))
	}
}

func TestSuggestTools_ReasonAndExample(t *testing.T) {
	suggestions := suggestToolsForIntent("debug issue")
	for _, s := range suggestions {
		if s.Reason == "" {
			t.Errorf("suggestion %s has empty reason", s.Tool)
		}
		if s.Example == "" {
			t.Errorf("suggestion %s has empty example", s.Tool)
		}
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func toolNames(suggestions []ToolSuggestion) []string {
	names := make([]string, len(suggestions))
	for i, s := range suggestions {
		names[i] = s.Tool
	}
	return names
}

func assertContains(t *testing.T, slice []string, item string) {
	t.Helper()
	for _, s := range slice {
		if s == item {
			return
		}
	}
	t.Errorf("expected %v to contain %q", slice, item)
}
