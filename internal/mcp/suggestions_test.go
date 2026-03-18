package mcp

import (
	"testing"
)

// ── suggestToolsForIntent tests ──────────────────────────────────────────────

func TestSuggestTools_ImplementIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("implementing auth refactor")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'implementing' intent")
	}
	names := toolNames(suggestions)
	assertContains(t, names, "get_impact")
	assertContains(t, names, "get_file_context")
	assertContains(t, names, "claim_work")
}

func TestSuggestTools_DebugIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("debugging login flow")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'debug' intent")
	}
	names := toolNames(suggestions)
	assertContains(t, names, "get_call_chain")
	assertContains(t, names, "get_file_context")
}

func TestSuggestTools_ReviewIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("reviewing code quality")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'review' intent")
	}
	names := toolNames(suggestions)
	assertContains(t, names, "get_violations")
	assertContains(t, names, "get_gaps")
	assertContains(t, names, "get_adrs")
}

func TestSuggestTools_ExploreIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("exploring the codebase")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'explore' intent")
	}
	names := toolNames(suggestions)
	assertContains(t, names, "get_repo_map")
	assertContains(t, names, "explain_codebase")
}

func TestSuggestTools_PlanIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("planning the architecture")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'plan' intent")
	}
	names := toolNames(suggestions)
	assertContains(t, names, "get_adrs")
	assertContains(t, names, "get_violations")
	assertContains(t, names, "get_plans")
}

func TestSuggestTools_RefactorIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("refactoring auth module")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'refactor' intent")
	}
	names := toolNames(suggestions)
	assertContains(t, names, "get_impact")
	assertContains(t, names, "get_file_context")
	assertContains(t, names, "claim_work")
}

func TestSuggestTools_NoIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("")
	if len(suggestions) != 0 {
		t.Fatalf("expected no suggestions for empty intent, got %d", len(suggestions))
	}
}

func TestSuggestTools_UnknownIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("something random and unknown")
	if len(suggestions) != 0 {
		t.Fatalf("expected no suggestions for unknown intent, got %d", len(suggestions))
	}
}

func TestSuggestTools_Deduplication(t *testing.T) {
	// "implement" and "build" both suggest the same tools.
	// A combined intent should not produce duplicates.
	suggestions := suggestToolsForIntent("implement and build new feature")
	names := toolNames(suggestions)
	seen := make(map[string]bool)
	for _, n := range names {
		if seen[n] {
			t.Fatalf("duplicate tool suggestion: %s", n)
		}
		seen[n] = true
	}
}

func TestSuggestTools_MaxFive(t *testing.T) {
	// An intent with many matching keywords should still cap at 5.
	suggestions := suggestToolsForIntent("implement build add debug fix investigate review audit refactor explore understand plan design")
	if len(suggestions) > 5 {
		t.Fatalf("expected max 5 suggestions, got %d", len(suggestions))
	}
}

func TestSuggestTools_SubstringMatch(t *testing.T) {
	// "implementing" contains "implement" — should match via substring.
	suggestions := suggestToolsForIntent("implementing auth service")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'implementing' (substring of 'implement')")
	}
	names := toolNames(suggestions)
	assertContains(t, names, "get_impact")
}

func TestSuggestTools_FixIntent(t *testing.T) {
	suggestions := suggestToolsForIntent("fix the login bug")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for 'fix' intent")
	}
	names := toolNames(suggestions)
	assertContains(t, names, "get_call_chain")
}

func TestSuggestTools_ReasonAndExample(t *testing.T) {
	suggestions := suggestToolsForIntent("debug issue")
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions")
	}
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
