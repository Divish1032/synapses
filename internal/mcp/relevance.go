// relevance.go scores graph nodes against a free-text problem statement.
// Used by the investigate handler to rank code blocks by relevance to the
// user's stated problem, not just structural distance in the call graph.
package mcp

import (
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ProblemScorer scores nodes by keyword overlap with a problem statement.
// Fast path — no LLM or embeddings required.
type ProblemScorer struct {
	keywords   []string          // stemmed keywords from the problem
	keywordSet map[string]bool   // for O(1) lookup
	rawWords   map[string]bool   // unstemmed lowercase words for exact matching
}

// NewProblemScorer tokenizes and stems a problem statement for scoring.
func NewProblemScorer(problem string) *ProblemScorer {
	if problem == "" {
		return &ProblemScorer{keywordSet: make(map[string]bool), rawWords: make(map[string]bool)}
	}

	// Tokenize: split on whitespace and punctuation.
	words := strings.FieldsFunc(problem, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_')
	})

	keywordSet := make(map[string]bool)
	rawWords := make(map[string]bool)
	var keywords []string

	for _, w := range words {
		low := strings.ToLower(w)
		if len(low) < 3 || problemStopWords[low] {
			continue
		}
		rawWords[low] = true
		stemmed := stemWord(low) // from porterstemmer.go
		if !keywordSet[stemmed] {
			keywordSet[stemmed] = true
			keywords = append(keywords, stemmed)
		}
	}

	return &ProblemScorer{
		keywords:   keywords,
		keywordSet: keywordSet,
		rawWords:   rawWords,
	}
}

// Score returns a relevance score [0, 1] for a node against the problem.
// Higher = more relevant. Uses weighted keyword overlap across node fields.
func (s *ProblemScorer) Score(n *graph.Node) float64 {
	if len(s.keywords) == 0 || n == nil {
		return 0
	}

	// Score each field with different weights.
	nameScore := s.fieldScore(n.Name, 3.0)
	docScore := s.fieldScore(n.Metadata["doc"], 2.0)
	sigScore := s.fieldScore(n.Metadata["signature"], 1.5)
	fileScore := s.filePathScore(n.File, 1.0)

	// Sum weighted scores, normalize by max possible.
	total := nameScore + docScore + sigScore + fileScore
	maxPossible := 3.0 + 2.0 + 1.5 + 1.0 // all fields fully matching

	score := total / maxPossible
	if score > 1.0 {
		score = 1.0
	}
	return score
}

// fieldScore computes keyword overlap for a text field.
// Returns weight * (matched keywords / total keywords).
func (s *ProblemScorer) fieldScore(text string, weight float64) float64 {
	if text == "" || len(s.keywords) == 0 {
		return 0
	}
	low := strings.ToLower(text)

	// Check both stemmed and raw matches.
	matches := 0
	for _, kw := range s.keywords {
		if strings.Contains(low, kw) {
			matches++
		}
	}
	// Also check raw (unstemmed) words for exact substring matches.
	for raw := range s.rawWords {
		if len(raw) >= 4 && strings.Contains(low, raw) {
			matches++
		}
	}

	// Normalize: fraction of keywords found, scaled by weight.
	fraction := float64(matches) / float64(len(s.keywords)+len(s.rawWords))
	return weight * fraction
}

// filePathScore checks if the file path contains problem keywords.
func (s *ProblemScorer) filePathScore(filePath string, weight float64) float64 {
	if filePath == "" {
		return 0
	}
	// Use just the base name and parent directory for matching.
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	stem := strings.ToLower(strings.TrimSuffix(base, ext))
	dirName := strings.ToLower(filepath.Base(dir))

	matches := 0
	for raw := range s.rawWords {
		if len(raw) >= 4 {
			if strings.Contains(stem, raw) || strings.Contains(dirName, raw) {
				matches++
			}
		}
	}
	if matches == 0 {
		return 0
	}
	fraction := float64(matches) / float64(len(s.rawWords))
	if fraction > 1.0 {
		fraction = 1.0
	}
	return weight * fraction
}

// CombinedScore blends structural relevance (from BFS/PPR) with problem
// keyword relevance into a single ranking score.
func CombinedScore(structural, keyword float64) float64 {
	// Structural relevance ensures graph-connected nodes rank high.
	// Keyword relevance ensures problem-specific nodes get a boost.
	return 0.5*structural + 0.5*keyword
}

// problemStopWords are common English words excluded from problem scoring.
var problemStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "not": true,
	"this": true, "that": true, "with": true, "from": true, "but": true,
	"have": true, "has": true, "had": true, "will": true, "can": true,
	"was": true, "were": true, "been": true, "being": true, "does": true,
	"did": true, "should": true, "would": true, "could": true, "may": true,
	"might": true, "must": true, "shall": true,
	"each": true, "every": true, "all": true, "any": true, "some": true,
	"when": true, "where": true, "how": true, "what": true, "which": true,
	"who": true, "whom": true, "whose": true, "why": true,
	"into": true, "about": true, "between": true, "through": true, "during": true,
	"before": true, "after": true, "above": true, "below": true,
	"also": true, "then": true, "than": true, "other": true,
	"its": true, "our": true, "their": true, "your": true,
	"current": true, "currently": true,
	"issue": true, "problem": true, "error": true, "bug": true,
	"see": true, "use": true, "using": true, "used": true,
	"leading": true, "leaves": true, "instead": true, "across": true,
}
