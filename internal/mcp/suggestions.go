package mcp

import (
	"sort"
	"strings"
)

// ToolSuggestion is returned in session_init's suggested_tools section.
// It recommends a tool the agent may want to use based on their declared intent.
type ToolSuggestion struct {
	Tool    string `json:"tool"`
	Reason  string `json:"reason"`
	Example string `json:"example"`
}

// intentKeywords maps keywords found in the agent's intent string to tool
// suggestions. Multiple keywords can match — results are deduplicated by
// tool name. Ordering within each slice determines display priority.
var intentKeywords = map[string][]ToolSuggestion{
	"implement": {
		{Tool: "get_impact", Reason: "Check what depends on entities you'll modify", Example: `get_impact(symbol="AuthService")`},
		{Tool: "get_file_context", Reason: "See all entities in files you'll change", Example: `get_file_context(file="internal/auth/service.go")`},
		{Tool: "claim_work", Reason: "Prevent conflicts with other agents", Example: `claim_work(agent_id="...", scope="internal/auth")`},
	},
	"build": {
		{Tool: "get_impact", Reason: "Check what depends on entities you'll modify", Example: `get_impact(symbol="AuthService")`},
		{Tool: "get_file_context", Reason: "See all entities in files you'll change", Example: `get_file_context(file="internal/auth/service.go")`},
		{Tool: "claim_work", Reason: "Prevent conflicts with other agents", Example: `claim_work(agent_id="...", scope="internal/auth")`},
	},
	"add": {
		{Tool: "get_impact", Reason: "Check what depends on entities you'll modify", Example: `get_impact(symbol="AuthService")`},
		{Tool: "get_file_context", Reason: "See all entities in files you'll change", Example: `get_file_context(file="internal/auth/service.go")`},
		{Tool: "claim_work", Reason: "Prevent conflicts with other agents", Example: `claim_work(agent_id="...", scope="internal/auth")`},
	},
	"debug": {
		{Tool: "get_call_chain", Reason: "Trace execution path to find the bug", Example: `get_call_chain(from="Handler", to="Repository")`},
		{Tool: "get_file_context", Reason: "Understand full file structure", Example: `get_file_context(file="internal/auth/service.go")`},
	},
	"fix": {
		{Tool: "get_call_chain", Reason: "Trace execution path to find the bug", Example: `get_call_chain(from="Handler", to="Repository")`},
		{Tool: "get_file_context", Reason: "Understand full file structure", Example: `get_file_context(file="internal/auth/service.go")`},
	},
	"investigate": {
		{Tool: "get_call_chain", Reason: "Trace execution path to find the bug", Example: `get_call_chain(from="Handler", to="Repository")`},
		{Tool: "get_file_context", Reason: "Understand full file structure", Example: `get_file_context(file="internal/auth/service.go")`},
	},
	"review": {
		{Tool: "get_violations", Reason: "Check architecture rule compliance", Example: `get_violations()`},
		{Tool: "get_gaps", Reason: "View known quality issues", Example: `get_gaps()`},
		{Tool: "get_adrs", Reason: "See past architectural decisions", Example: `get_adrs()`},
	},
	"audit": {
		{Tool: "get_violations", Reason: "Check architecture rule compliance", Example: `get_violations()`},
		{Tool: "get_gaps", Reason: "View known quality issues", Example: `get_gaps()`},
		{Tool: "get_adrs", Reason: "See past architectural decisions", Example: `get_adrs()`},
	},
	"refactor": {
		{Tool: "get_impact", Reason: "Blast radius analysis before refactoring", Example: `get_impact(symbol="AuthService")`},
		{Tool: "get_file_context", Reason: "See all entities in scope", Example: `get_file_context(file="internal/auth/service.go")`},
		{Tool: "claim_work", Reason: "Prevent conflicts during refactor", Example: `claim_work(agent_id="...", scope="internal/auth")`},
	},
	"explore": {
		{Tool: "get_repo_map", Reason: "Navigate the codebase by package and layer", Example: `get_repo_map(detail="compact")`},
		{Tool: "explain_codebase", Reason: "Architectural overview", Example: `explain_codebase()`},
	},
	"understand": {
		{Tool: "get_repo_map", Reason: "Navigate the codebase by package and layer", Example: `get_repo_map(detail="compact")`},
		{Tool: "explain_codebase", Reason: "Architectural overview", Example: `explain_codebase()`},
	},
	"plan": {
		{Tool: "get_adrs", Reason: "Review past architectural decisions", Example: `get_adrs()`},
		{Tool: "get_violations", Reason: "Current rule state", Example: `get_violations()`},
		{Tool: "get_plans", Reason: "See existing plans", Example: `get_plans()`},
	},
	"design": {
		{Tool: "get_adrs", Reason: "Review past architectural decisions", Example: `get_adrs()`},
		{Tool: "get_violations", Reason: "Current rule state", Example: `get_violations()`},
		{Tool: "get_plans", Reason: "See existing plans", Example: `get_plans()`},
	},
}

// intentKeywordKeys is a sorted slice of intentKeywords map keys.
// Deterministic iteration order guarantees reproducible suggestion output.
var intentKeywordKeys []string

func init() {
	intentKeywordKeys = make([]string, 0, len(intentKeywords))
	for k := range intentKeywords {
		intentKeywordKeys = append(intentKeywordKeys, k)
	}
	sort.Strings(intentKeywordKeys)
}

// stripSuffix removes common English verb inflection suffixes to normalize
// an inflected word back to (approximately) its base form. Handles:
//
//	implementing→implement, debugging→debug, reviewed→review,
//	exploring→explor, investigated→investigat, refactoring→refactor
//
// Not a full Porter stemmer — just the suffixes needed for intent matching.
// Minimum 3-char stem to avoid stripping too aggressively on short words.
func stripSuffix(word string) string {
	// Ordered longest-first so "ation" matches before "tion" before "ion".
	suffixes := []string{
		"ation", "tion", "sion",
		"ment", "ness", "ious", "eous", "ous",
		"ize", "ise", "ful",
		"ing", "ely", "ly",
		"ed", "er",
	}
	for _, sfx := range suffixes {
		if strings.HasSuffix(word, sfx) && len(word)-len(sfx) >= 3 {
			stem := word[:len(word)-len(sfx)]
			// Handle doubled consonant from suffix removal:
			// "debugging"→"debugg"→"debug", "debugger"→"debugg"→"debug",
			// "planned"→"plann"→"plan", "planning"→"plann"→"plan"
			if (sfx == "ing" || sfx == "er" || sfx == "ed") &&
				len(stem) >= 2 && stem[len(stem)-1] == stem[len(stem)-2] {
				stem = stem[:len(stem)-1]
			}
			return stem
		}
	}
	return word
}

// suggestToolsForIntent parses the intent string for keywords and returns
// matching tool suggestions. Deduplicates by tool name. Max 5 suggestions.
// Uses deterministic iteration order for reproducible output.
func suggestToolsForIntent(intent string) []ToolSuggestion {
	if intent == "" {
		return nil
	}

	lower := strings.ToLower(intent)
	words := strings.Fields(lower)

	seen := make(map[string]bool)
	var result []ToolSuggestion

	// Pass 1: exact word match against keyword map (O(1) lookups, deterministic).
	for _, word := range words {
		suggestions, ok := intentKeywords[word]
		if !ok {
			continue
		}
		for _, s := range suggestions {
			if seen[s.Tool] {
				continue
			}
			seen[s.Tool] = true
			result = append(result, s)
			if len(result) >= 5 {
				return result
			}
		}
	}

	// Pass 2: stem matching — strip suffixes from intent words, then match.
	// Uses sorted keys for deterministic iteration order.
	if len(result) < 5 {
		for _, keyword := range intentKeywordKeys {
			suggestions := intentKeywords[keyword]
			matched := false
			for _, w := range words {
				if w == keyword {
					// Already handled in exact-match pass above.
					matched = false
					break
				}
				stemW := stripSuffix(w)
				if stemW == keyword {
					matched = true
					break
				}
				// Handle partial stem matches: "explor" (from "exploring") vs "explore".
				shorter, longer := stemW, keyword
				if len(shorter) > len(longer) {
					shorter, longer = longer, shorter
				}
				if len(shorter) >= 4 && strings.HasPrefix(longer, shorter) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			for _, s := range suggestions {
				if seen[s.Tool] {
					continue
				}
				seen[s.Tool] = true
				result = append(result, s)
				if len(result) >= 5 {
					return result
				}
			}
		}
	}

	return result
}
