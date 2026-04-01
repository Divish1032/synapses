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
// Sprint 23.9: all suggestions use the consolidated 8-tool names.
var intentKeywords = map[string][]ToolSuggestion{
	"implement": {
		{Tool: "get_impact", Reason: "Check what depends on entities you'll modify", Example: `get_impact(symbol="AuthService")`},
		{Tool: "search", Reason: "Find all entities in files you'll change", Example: `search(query="auth", mode="exact")`},
	},
	"build": {
		{Tool: "get_impact", Reason: "Check what depends on entities you'll modify", Example: `get_impact(symbol="AuthService")`},
		{Tool: "search", Reason: "Find all entities in files you'll change", Example: `search(query="auth", mode="exact")`},
	},
	"add": {
		{Tool: "get_impact", Reason: "Check what depends on entities you'll modify", Example: `get_impact(symbol="AuthService")`},
		{Tool: "search", Reason: "Find all entities in files you'll change", Example: `search(query="auth", mode="exact")`},
	},
	"debug": {
		{Tool: "get_context", Reason: "Trace execution path to find the bug", Example: `get_context(mode="path", from="Handler", to="Repository")`},
		{Tool: "search", Reason: "Find entities by name or concept", Example: `search(query="auth", mode="keyword")`},
	},
	"fix": {
		{Tool: "get_context", Reason: "Trace execution path to find the bug", Example: `get_context(mode="path", from="Handler", to="Repository")`},
		{Tool: "search", Reason: "Find entities by name or concept", Example: `search(query="auth", mode="keyword")`},
	},
	"investigate": {
		{Tool: "get_context", Reason: "Trace execution path to find the bug", Example: `get_context(mode="path", from="Handler", to="Repository")`},
		{Tool: "search", Reason: "Find entities by name or concept", Example: `search(query="auth", mode="keyword")`},
	},
	"review": {
		{Tool: "validate", Reason: "Check architecture rule compliance", Example: `validate(phase="list")`},
		{Tool: "memory", Reason: "View known quality issues", Example: `memory(action="list_gaps")`},
		{Tool: "validate", Reason: "See past architectural decisions", Example: `validate(phase="list_adrs")`},
	},
	"audit": {
		{Tool: "validate", Reason: "Check architecture rule compliance", Example: `validate(phase="list")`},
		{Tool: "memory", Reason: "View known quality issues", Example: `memory(action="list_gaps")`},
		{Tool: "validate", Reason: "See past architectural decisions", Example: `validate(phase="list_adrs")`},
	},
	"refactor": {
		{Tool: "get_impact", Reason: "Blast radius analysis before refactoring", Example: `get_impact(symbol="AuthService")`},
		{Tool: "search", Reason: "Find all entities in scope", Example: `search(query="auth", mode="exact")`},
	},
	"explore": {
		{Tool: "get_context", Reason: "Navigate codebase structure", Example: `get_context(entity="main", intent="understand")`},
		{Tool: "search", Reason: "Find entities by name or concept", Example: `search(query="auth", mode="keyword")`},
	},
	"understand": {
		{Tool: "get_context", Reason: "Navigate codebase structure", Example: `get_context(entity="main", intent="understand")`},
		{Tool: "search", Reason: "Find entities by name or concept", Example: `search(query="auth", mode="keyword")`},
	},
	"plan": {
		{Tool: "validate", Reason: "Review past architectural decisions", Example: `validate(phase="list_adrs")`},
		{Tool: "validate", Reason: "Current rule state", Example: `validate(phase="list")`},
		{Tool: "tasks", Reason: "See existing plans", Example: `tasks(action="list_plans")`},
	},
	"design": {
		{Tool: "validate", Reason: "Review past architectural decisions", Example: `validate(phase="list_adrs")`},
		{Tool: "validate", Reason: "Current rule state", Example: `validate(phase="list")`},
		{Tool: "tasks", Reason: "See existing plans", Example: `tasks(action="list_plans")`},
	},
	"test": {
		{Tool: "validate", Reason: "Check files against architectural rules", Example: `validate(phase="post", files_written=["file.go"])`},
		{Tool: "memory", Reason: "View known quality issues to prioritize test coverage", Example: `memory(action="list_gaps")`},
		{Tool: "get_impact", Reason: "Find what depends on the code under test", Example: `get_impact(symbol="FunctionName")`},
	},
	"document": {
		{Tool: "get_context", Reason: "Get full context for the entity you're documenting", Example: `get_context(entity="FunctionName")`},
		{Tool: "validate", Reason: "Review past architectural decisions to document rationale", Example: `validate(phase="list_adrs")`},
	},
	"optimize": {
		{Tool: "get_impact", Reason: "Understand blast radius before optimizing", Example: `get_impact(symbol="SlowFunction")`},
		{Tool: "search", Reason: "Find all entities in files you're optimizing", Example: `search(query="SlowFunction", mode="exact")`},
		{Tool: "memory", Reason: "View known performance issues", Example: `memory(action="list_gaps")`},
	},
	"profile": {
		{Tool: "get_impact", Reason: "Identify high-fanin hot paths worth profiling", Example: `get_impact(symbol="HotFunction")`},
		{Tool: "search", Reason: "Find all entities in the hot path", Example: `search(query="HotFunction", mode="exact")`},
		{Tool: "memory", Reason: "View known performance issues", Example: `memory(action="list_gaps")`},
	},
}

// intentKeywordKeys is a sorted slice of intentKeywords map keys.
// Deterministic iteration order guarantees reproducible suggestion output.
var intentKeywordKeys []string

// stemmedKeywordSuggestions maps pre-computed Porter stems of intent keywords
// to their tool suggestions. Built once at init to avoid redundant stemWord()
// calls during suggestToolsForIntent's Pass 2. If two keywords share the same
// stem, the last one (by sorted key order) wins — collisions are rare and benign.
var stemmedKeywordSuggestions map[string][]ToolSuggestion

func init() {
	intentKeywordKeys = make([]string, 0, len(intentKeywords))
	for k := range intentKeywords {
		intentKeywordKeys = append(intentKeywordKeys, k)
	}
	sort.Strings(intentKeywordKeys)

	// Pre-compute Porter stems for all intent keywords.
	stemmedKeywordSuggestions = make(map[string][]ToolSuggestion, len(intentKeywords))
	for _, k := range intentKeywordKeys {
		stemmedKeywordSuggestions[porterStem(k)] = intentKeywords[k]
	}
}

// stemWord applies the Porter stemming algorithm to normalize an inflected
// word to its base form. Uses the inlined Porter stemmer in porterstemmer.go.
// Handles all English inflections: implementing→implement, exploring→explor,
// debugging→debug, investigation→investig, reviewed→review, etc.
func stemWord(word string) string {
	return porterStem(word)
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

	// Pass 2: Porter stem matching — stem each intent word and look it up in
	// the pre-computed stemmedKeywordSuggestions map. Porter stemmer normalizes
	// inflected forms: "implementing"→"implement", "exploring"→"explor".
	// Pre-computed stems avoid redundant stemWord() calls on keywords per call.
	if len(result) < 5 {
		for _, word := range words {
			if _, exact := intentKeywords[word]; exact {
				continue // already handled in exact-match pass above
			}
			suggestions, ok := stemmedKeywordSuggestions[stemWord(word)]
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
	}

	return result
}
