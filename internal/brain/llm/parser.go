package llm

import (
	"strings"
)

// ParseSILResponse parses the labeled output format produced by the fine-tuned SIL model.
//
// Expected output (after optional <think>...</think> block):
//
//	ROOT_SUMMARY: One sentence about the root node.
//	INSIGHT: One sentence about architectural role.
//	CONCERNS: concern1, concern2, concern3
//
// Falls back to raw text as insight for backward compatibility with standard
// Ollama models that emit plain text or JSON.
//
// Returns empty strings/nil slice for any field not found in the response.
func ParseSILResponse(raw string) (rootSummary, insight string, concerns []string) {
	// Strip <think>...</think> blocks first.
	text := stripThinkBlocks(raw)
	text = strings.TrimSpace(text)

	if text == "" {
		return
	}

	// Labeled format detection: at least one of the three labels must be present.
	if strings.Contains(strings.ToUpper(text), "ROOT_SUMMARY:") ||
		strings.Contains(strings.ToUpper(text), "INSIGHT:") {

		rootSummary = extractSILLabel(text, "ROOT_SUMMARY")
		insight = extractSILLabel(text, "INSIGHT")

		concernsRaw := extractSILLabel(text, "CONCERNS")
		if concernsRaw != "" && !strings.EqualFold(strings.TrimSpace(concernsRaw), "none") {
			for _, c := range strings.Split(concernsRaw, ",") {
				c = strings.TrimSpace(c)
				if c != "" {
					concerns = append(concerns, c)
				}
			}
		}
		return
	}

	// Not labeled — return empty so parseInsight's own fallback chain handles it:
	// JSON format first, then raw text. Setting insight here causes JSON responses
	// to bypass the JSON parser and get stored verbatim.
	return
}

// extractSILLabel extracts the value after "LABEL:" up to the next known label line.
// Handles both "LABEL: value on one line" and multi-word values.
// Returns "" if the label is not found.
func extractSILLabel(text, label string) string {
	upperText := strings.ToUpper(text)
	prefix := label + ":"
	idx := strings.Index(upperText, prefix)
	if idx < 0 {
		return ""
	}

	// Skip past the colon.
	start := idx + len(prefix)
	rest := text[start:]

	// Collect all characters until the next labeled line.
	var parts []string
	for _, line := range strings.Split(rest, "\n") {
		trimmed := strings.TrimSpace(line)
		// Stop when we hit the next known SIL label.
		if isSILLabelLine(strings.ToUpper(trimmed)) && len(parts) > 0 {
			break
		}
		parts = append(parts, trimmed)
	}

	return strings.TrimSpace(strings.Join(parts, " "))
}

// isSILLabelLine returns true if the line starts with a known SIL output label.
func isSILLabelLine(upperLine string) bool {
	return strings.HasPrefix(upperLine, "ROOT_SUMMARY:") ||
		strings.HasPrefix(upperLine, "INSIGHT:") ||
		strings.HasPrefix(upperLine, "CONCERNS:")
}
