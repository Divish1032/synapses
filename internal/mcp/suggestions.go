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
	"test": {
		{Tool: "verify_implementation", Reason: "Check files against architectural rules before testing", Example: `verify_implementation(files_written=["file.go"])`},
		{Tool: "get_gaps", Reason: "View known quality issues to prioritize test coverage", Example: `get_gaps()`},
		{Tool: "get_impact", Reason: "Find what depends on the code under test", Example: `get_impact(symbol="FunctionName")`},
	},
	"document": {
		{Tool: "get_context", Reason: "Get full context for the entity you're documenting", Example: `get_context(entity="FunctionName")`},
		{Tool: "get_adrs", Reason: "Review past architectural decisions to document rationale", Example: `get_adrs()`},
		{Tool: "explain_codebase", Reason: "Generate architectural overview for documentation", Example: `explain_codebase()`},
	},
	"optimize": {
		{Tool: "get_impact", Reason: "Understand blast radius before optimizing", Example: `get_impact(symbol="SlowFunction")`},
		{Tool: "get_file_context", Reason: "See all entities in files you're optimizing", Example: `get_file_context(file="internal/hot/path.go")`},
		{Tool: "get_gaps", Reason: "View known performance issues", Example: `get_gaps()`},
	},
	"profile": {
		{Tool: "get_impact", Reason: "Identify high-fanin hot paths worth profiling", Example: `get_impact(symbol="HotFunction")`},
		{Tool: "get_file_context", Reason: "See all entities in the hot path", Example: `get_file_context(file="internal/hot/path.go")`},
		{Tool: "get_gaps", Reason: "View known performance issues", Example: `get_gaps()`},
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
