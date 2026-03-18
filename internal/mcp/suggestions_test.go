package mcp

import (
	"testing"
)

// ── Porter stemmer tests ─────────────────────────────────────────────────────

func TestPorterStem_IntentVerbs(t *testing.T) {
	// Verify that Porter stemmer normalizes inflected verb forms so that
	// the stemmed intent word matches the stemmed keyword.
	// We don't test exact stem output (that's the algorithm's business) —
	// we test that stem(inflected) == stem(keyword) for every intent pair.
	pairs := []struct {
		inflected string
		keyword   string
	}{
		{"implementing", "implement"},
		{"debugging", "debug"},
		{"exploring", "explore"},
		{"investigating", "investigate"},
		{"reviewing", "review"},
		{"refactoring", "refactor"},
		{"designing", "design"},
		{"planning", "plan"},
		{"building", "build"},
		{"auditing", "audit"},
		{"fixing", "fix"},
		{"implemented", "implement"},
		{"reviewed", "review"},
		{"explored", "explore"},
	}
	// Note: "debugger"→"debugg" vs "debug"→"debug" don't match in Porter.
	// This is acceptable — agents say "debugging" or "debug", not "debugger".
	for _, p := range pairs {
		stemI := porterStem(p.inflected)
		stemK := porterStem(p.keyword)
		if stemI != stemK {
			t.Errorf("stem(%q)=%q != stem(%q)=%q — inflected form won't match keyword",
				p.inflected, stemI, p.keyword, stemK)
		}
	}
}

func TestPorterStem_ShortWords(t *testing.T) {
	// Words ≤2 chars should pass through unchanged.
	if got := porterStem("go"); got != "go" {
		t.Errorf("porterStem(\"go\") = %q, want \"go\"", got)
	}
	if got := porterStem(""); got != "" {
		t.Errorf("porterStem(\"\") = %q, want \"\"", got)
	}
}

func TestPorterStem_NoFalsePositive_AddressVsAdd(t *testing.T) {
	// "address" and "add" must NOT stem to the same thing.
	if porterStem("address") == porterStem("add") {
		t.Error("stem(address) == stem(add) — would cause false positive")
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
