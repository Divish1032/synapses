package mcp

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// KVField is a single labeled key-value entry in a KV-format response.
// Key is the label (e.g. "Security", "Blast-radius"). Value is the NL annotation.
// Important fields are rendered first and never truncated.
type KVField struct {
	Key       string
	Value     string
	Important bool // true = appears first and survives truncation
}

// FormatKV renders a structured labeled KV response.
//
// Output format:
//
//	# Header (subtitle)
//	Key: Value
//	Key2: Value2
//
// Important fields are sorted first. The total output is trimmed to budget tokens
// (estimated as chars/4). budget=0 means no limit.
func FormatKV(header, subtitle string, fields []KVField, budget int) string {
	var b strings.Builder

	// Title line
	if subtitle != "" {
		fmt.Fprintf(&b, "# %s (%s)\n", header, subtitle)
	} else {
		fmt.Fprintf(&b, "# %s\n", header)
	}

	// Important fields first, then regular
	for pass := 0; pass < 2; pass++ {
		for _, f := range fields {
			if f.Key == "" || f.Value == "" {
				continue
			}
			if pass == 0 && !f.Important {
				continue
			}
			if pass == 1 && f.Important {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n", f.Key, f.Value)
		}
	}

	result := b.String()
	if budget > 0 {
		result = TrimToTokenBudget(result, budget)
	}
	return result
}

// EstimateTokens approximates token count as chars/4 (standard LLM token estimate).
func EstimateTokens(s string) int {
	return (len(s) + 3) / 4
}

// TrimToTokenBudget truncates text to fit within the token budget.
// Truncation preserves complete lines — it never cuts mid-line.
// When truncated, appends "[truncated — use detail_level=full for complete output]".
func TrimToTokenBudget(s string, budget int) string {
	if budget <= 0 {
		return s
	}
	// Fast path: already fits
	if EstimateTokens(s) <= budget {
		return s
	}
	// Budget in characters (tokens * 4)
	maxChars := budget * 4
	trailer := "\n[truncated — use detail_level=full for complete output]"
	// Reserve chars for trailer
	available := maxChars - len(trailer)
	if available <= 0 {
		return trailer
	}

	// Trim to last complete line within available chars
	cut := s
	if len(cut) > available {
		cut = cut[:available]
		// Walk back to last newline
		if nl := strings.LastIndex(cut, "\n"); nl > 0 {
			cut = cut[:nl]
		}
	}
	return cut + trailer
}

// kvContentHash returns a short hash of content for use as a dedup key.
// Uses first 8 bytes of SHA-256 as a hex string (16 chars).
func kvContentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h[:8])
}
