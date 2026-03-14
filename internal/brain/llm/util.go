package llm

import (
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

// Truncate shortens s to at most n bytes for use in error messages.
// Appends "..." when truncation occurs.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
