package llm

import (
	"encoding/json"
	"regexp"
	"strings"
)

// thinkTagRe strips <think>...</think> blocks from LLM output.
// Used by both OllamaClient and LocalClient as a safety net.
var thinkTagRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThinkBlocks removes all <think>...</think> sections from s.
func stripThinkBlocks(s string) string {
	return strings.TrimSpace(thinkTagRe.ReplaceAllString(s, ""))
}

// ExtractJSON strips markdown code fences and extracts the JSON object from raw LLM output.
// Many small models wrap JSON responses in ```json ... ``` blocks despite instructions.
// This function handles that gracefully so callers always get raw JSON to unmarshal.
func ExtractJSON(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "```"); idx >= 0 {
		s = s[idx:]
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		if end := strings.Index(s, "```"); end >= 0 {
			s = s[:end]
		}
	}
	if start := strings.Index(s, "{"); start >= 0 {
		s = s[start:]
	}
	if end := strings.LastIndex(s, "}"); end >= 0 {
		s = s[:end+1]
	}
	return strings.TrimSpace(s)
}

// RepairJSON attempts to fix common JSON bracket mismatches produced by
// Qwen3.5 models. The most frequent issue is nested arrays-of-objects where
// the model writes "]]" instead of "]}]" (dropping the closing "}" of the
// inner object before the outer array bracket).
//
// Only modifies the input when it fails json.Unmarshal AND the fix produces
// valid JSON — never corrupts already-valid output.
func RepairJSON(s string) string {
	s = strings.TrimSpace(s)
	var probe json.RawMessage
	if json.Unmarshal([]byte(s), &probe) == nil {
		return s // already valid
	}
	// Fix 1: ]] → ]}] — missing object-close before outer array-close.
	fixed := strings.ReplaceAll(s, "]]", "]}]")
	if json.Unmarshal([]byte(fixed), &probe) == nil {
		return fixed
	}
	return s // couldn't fix
}

// Truncate shortens s to at most n runes for use in error messages.
// Appends "..." when truncation occurs. Uses rune-aware slicing to
// avoid cutting multi-byte UTF-8 characters mid-sequence.
func Truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}
