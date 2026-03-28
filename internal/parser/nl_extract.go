// Package parser — nl_extract.go implements Tier 0 NL-to-graph entity extraction.
//
// Tier 0 is always-on: pure Go, zero dependencies, zero cost. Given markdown
// body text it returns named entity candidates and relationship signals without
// any LLM or embedding call. Quality alone is ~50% of full NL extraction —
// already better than no extraction at all.
//
// Pipeline position:
//
//	Tier 0 (this file) → Tier 1 (resolver/nl_entities.go, name-match resolution)
//	                   → Tier 2 (brain/client.go, P1 LLM classification)
package parser

import (
	"regexp"
	"strings"
	"unicode"
)

// EntityCandidate is a named entity extracted from natural-language text.
// Name is the raw extracted token; Context is the surrounding sentence
// for Tier 2 LLM classification. Confidence is a Tier 0 heuristic score.
type EntityCandidate struct {
	Name       string  // extracted token (normalised, no backticks/quotes)
	Context    string  // surrounding sentence (≤200 chars) for Tier 2
	SourceLine int     // 1-based line in the original markdown body
	Confidence float64 // heuristic confidence: 0.6–0.95
}

// RelationshipSignal is a keyword-based relationship hint extracted from text.
// From and To are candidate names; Signal is the relationship keyword found.
// These are upgraded to typed EdgeType values by Tier 1/2 resolution.
type RelationshipSignal struct {
	From       string
	To         string
	Signal     string // e.g. "depends on", "implements", "uses"
	SourceLine int
}

// ── Compiled patterns ────────────────────────────────────────────────────────

// backtickRe matches `code spans` (CommonMark inline code).
var backtickRe = regexp.MustCompile("`([^`]+)`")

// camelCaseRe matches CamelCase or PascalCase tokens (e.g. "TokenBucket").
// Requires at least one lowercase letter followed by an uppercase letter
// to avoid matching ALL_CAPS constants or plain uppercase words.
var camelCaseRe = regexp.MustCompile(`\b([A-Z][a-z]+(?:[A-Z][a-z]+)+)\b`)

// quotedTermRe matches "quoted terms" — double-quoted phrases of 1–4 words.
var quotedTermRe = regexp.MustCompile(`"([A-Za-z][A-Za-z0-9 \-]{1,40})"`)

// capitalizedPhraseRe matches Capitalized Multi-Word Phrases (title-case).
// Requires 2-4 capitalised words in sequence (avoids sentence starts).
var capitalizedPhraseRe = regexp.MustCompile(`\b([A-Z][a-z]+(?:\s+[A-Z][a-z]+){1,3})\b`)

// relationshipKeywordRe detects explicit relationship signals in text.
// Named capture groups "from", "rel", "to" are used for extraction.
var relationshipKeywordRe = regexp.MustCompile(
	`(?i)\b(` +
		`(?:depends\s+on|depends_on)|` +
		`(?:implements|implemented\s+by)|` +
		`(?:uses|used\s+by)|` +
		`(?:extends|extended\s+by)|` +
		`(?:caused\s+by|causes)|` +
		`(?:instance\s+of|is\s+a\s+type\s+of)|` +
		`(?:see\s+also|related\s+to|relates\s+to)|` +
		`(?:contradicts|conflicts\s+with)` +
		`)\b`,
)

// ── Stop words ───────────────────────────────────────────────────────────────

// nlStopWords is a set of common English words and markdown artefacts that
// should not become knowledge graph nodes even if they look like candidates.
var nlStopWords = func() map[string]bool {
	words := []string{
		// Articles / prepositions
		"the", "a", "an", "of", "in", "on", "at", "to", "for", "from",
		"with", "by", "as", "is", "are", "was", "were", "be", "been",
		"have", "has", "had", "do", "does", "did", "will", "would",
		"can", "could", "should", "may", "might", "shall",
		// Common single-word doc constructs
		"this", "that", "these", "those", "it", "its", "they", "their",
		"we", "our", "you", "your", "he", "she", "his", "her",
		"and", "or", "but", "not", "if", "else", "when", "where",
		"how", "what", "which", "who", "why", "all", "any", "each",
		"more", "most", "some", "other", "than", "then", "so", "also",
		// Technical junk
		"true", "false", "null", "nil", "none", "undefined",
		"new", "return", "func", "function", "var", "const", "let",
		"type", "struct", "interface", "class", "method", "field",
		"value", "string", "int", "float", "bool", "byte", "error",
		"note", "example", "example:", "note:", "see", "ref",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()

// ── Public API ───────────────────────────────────────────────────────────────

// ExtractEntityCandidates scans markdown body text and returns entity candidates.
// Deduplicates by name (keeps highest confidence). Filters stop words and
// tokens shorter than 3 characters. Safe for concurrent use (no global state).
func ExtractEntityCandidates(text string) []EntityCandidate {
	if text == "" {
		return nil
	}

	lines := strings.Split(text, "\n")
	seen := make(map[string]EntityCandidate) // name → best candidate

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		sentence := truncateSentence(line, 200)

		// 1. Backtick spans — highest confidence (~0.9)
		for _, m := range backtickRe.FindAllStringSubmatch(line, -1) {
			name := strings.TrimSpace(m[1])
			if isValidCandidate(name) {
				add(seen, EntityCandidate{
					Name: name, Context: sentence,
					SourceLine: lineNum, Confidence: 0.9,
				})
			}
		}

		// Strip backtick spans before further processing to avoid double-counting.
		stripped := backtickRe.ReplaceAllString(line, " ")

		// 2. CamelCase/PascalCase tokens — medium-high confidence (~0.75)
		for _, m := range camelCaseRe.FindAllString(stripped, -1) {
			if isValidCandidate(m) {
				add(seen, EntityCandidate{
					Name: m, Context: sentence,
					SourceLine: lineNum, Confidence: 0.75,
				})
			}
		}

		// 3. Quoted terms — medium confidence (~0.75)
		for _, m := range quotedTermRe.FindAllStringSubmatch(stripped, -1) {
			name := strings.TrimSpace(m[1])
			if isValidCandidate(name) {
				add(seen, EntityCandidate{
					Name: name, Context: sentence,
					SourceLine: lineNum, Confidence: 0.75,
				})
			}
		}

		// 4. Capitalized multi-word phrases — lower confidence (~0.65)
		// Only apply when line doesn't start a sentence (reduces false positives).
		if !isSentenceStart(stripped) {
			for _, m := range capitalizedPhraseRe.FindAllString(stripped, -1) {
				if isValidCandidate(m) {
					add(seen, EntityCandidate{
						Name: m, Context: sentence,
						SourceLine: lineNum, Confidence: 0.65,
					})
				}
			}
		}
	}

	// Convert map to slice, sorted by source line then name for stability.
	result := make([]EntityCandidate, 0, len(seen))
	for _, c := range seen {
		result = append(result, c)
	}
	sortCandidates(result)
	return result
}

// ExtractRelationshipSignals scans text for explicit relationship keywords
// and returns relationship signals between adjacent entity candidates.
// Best-effort: only fires when a relationship keyword appears between
// two recognisable entity-like tokens on the same line.
func ExtractRelationshipSignals(text string, candidates []EntityCandidate) []RelationshipSignal {
	if text == "" || len(candidates) == 0 {
		return nil
	}

	// Build a name→true lookup for quick membership test.
	names := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		names[strings.ToLower(c.Name)] = true
	}

	lines := strings.Split(text, "\n")
	var signals []RelationshipSignal

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		lowerLine := strings.ToLower(line)

		// Find all relationship keyword positions on this line.
		kwMatches := relationshipKeywordRe.FindAllStringIndex(lowerLine, -1)
		if len(kwMatches) == 0 {
			continue
		}

		// For each keyword, look for entity names on either side within 80 chars.
		for _, kwPos := range kwMatches {
			kw := strings.TrimSpace(lowerLine[kwPos[0]:kwPos[1]])
			before := lowerLine[:kwPos[0]]
			after := lowerLine[kwPos[1]:]

			fromName := findNearestEntity(before, names, true)
			toName := findNearestEntity(after, names, false)

			if fromName != "" && toName != "" && fromName != toName {
				signals = append(signals, RelationshipSignal{
					From:       fromName,
					To:         toName,
					Signal:     kw,
					SourceLine: lineNum,
				})
			}
		}
	}

	return signals
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// isValidCandidate returns true when name is worthy of becoming a graph node.
// Rejects: too short, stop words, pure numbers, markdown artefacts.
func isValidCandidate(name string) bool {
	if len(name) < 3 {
		return false
	}
	// Reject if it's only whitespace or punctuation.
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	// Reject if all characters are digits or punctuation.
	allNonAlpha := true
	for _, r := range trimmed {
		if unicode.IsLetter(r) {
			allNonAlpha = false
			break
		}
	}
	if allNonAlpha {
		return false
	}
	// Reject stop words (case-insensitive single-word check).
	if !strings.ContainsRune(trimmed, ' ') {
		if nlStopWords[strings.ToLower(trimmed)] {
			return false
		}
	}
	// Reject if the name is just markdown heading chars or code fence.
	if strings.TrimLeft(trimmed, "#~`* ") == "" {
		return false
	}
	return true
}

// add inserts c into seen, keeping the entry with the highest confidence.
func add(seen map[string]EntityCandidate, c EntityCandidate) {
	key := strings.ToLower(c.Name)
	if existing, ok := seen[key]; ok {
		if c.Confidence <= existing.Confidence {
			return
		}
	}
	seen[key] = c
}

// ExtractFrontmatterCandidates creates entity candidates from frontmatter metadata
// (tags and category) stored on file nodes. These have high confidence (0.95) since
// they are explicitly authored by the document owner.
func ExtractFrontmatterCandidates(tags []string, category string) []EntityCandidate {
	var candidates []EntityCandidate
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if isValidCandidate(tag) {
			candidates = append(candidates, EntityCandidate{
				Name:       tag,
				Context:    "frontmatter tag",
				SourceLine: 1,
				Confidence: 0.95,
			})
		}
	}
	if category != "" && isValidCandidate(category) {
		candidates = append(candidates, EntityCandidate{
			Name:       category,
			Context:    "frontmatter category",
			SourceLine: 1,
			Confidence: 0.95,
		})
	}
	return candidates
}

// truncateSentence returns up to maxLen bytes from s, cutting at word boundary.
func truncateSentence(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	cut := s[:maxLen]
	if idx := strings.LastIndexByte(cut, ' '); idx > 0 {
		return cut[:idx]
	}
	return cut
}

// isSentenceStart returns true when the trimmed line looks like the start of
// a new sentence (begins with a capital word, likely a subject noun).
// This heuristic reduces false positives from capitalised sentence-opening words.
func isSentenceStart(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	// If the first character is uppercase but the rest of the first word is lowercase,
	// treat as sentence start (i.e. "This is a sentence" not "TokenBucket is a...").
	words := strings.Fields(trimmed)
	if len(words) == 0 {
		return false
	}
	first := words[0]
	if len(first) < 2 {
		return false
	}
	return unicode.IsUpper(rune(first[0])) && unicode.IsLower(rune(first[1]))
}

// findNearestEntity searches s for the closest known entity name within 80 chars.
// If lookBack is true, searches from the right end of s (before the keyword).
func findNearestEntity(s string, names map[string]bool, lookBack bool) string {
	if len(s) > 80 {
		if lookBack {
			s = s[len(s)-80:]
		} else {
			s = s[:80]
		}
	}
	// Try each word / multi-word token in the window.
	words := strings.Fields(strings.ToLower(s))
	if lookBack {
		// Search from the end (nearest to the keyword).
		for i := len(words) - 1; i >= 0; i-- {
			w := strings.Trim(words[i], ".,;:!?()")
			if names[w] {
				return w
			}
		}
	} else {
		for _, w := range words {
			w = strings.Trim(w, ".,;:!?()")
			if names[w] {
				return w
			}
		}
	}
	return ""
}

// sortCandidates sorts candidates by source line ascending, then name for stability.
func sortCandidates(cs []EntityCandidate) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0; j-- {
			a, b := cs[j-1], cs[j]
			if a.SourceLine > b.SourceLine ||
				(a.SourceLine == b.SourceLine && a.Name > b.Name) {
				cs[j-1], cs[j] = cs[j], cs[j-1]
			} else {
				break
			}
		}
	}
}
