package resolver

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ResolveDocEdges scans all Section nodes in the graph and creates
// EXPLAINS (section→code) and DOCUMENTED_BY (code→section) edges
// for identifiers found in section body text.
//
// Must be called after all files are parsed so code entity nodes exist.
// Returns the number of EXPLAINS edges created.
func ResolveDocEdges(g *graph.Graph) int {
	sections := g.FindByType(graph.NodeSection)
	if len(sections) == 0 {
		return 0
	}

	// Build a set of candidate code entity names for fast lookup.
	// Only include names ≥ 4 chars to avoid false positives on
	// common short identifiers ("New", "Run", "Get", "Set", "err").
	allNodes := g.AllNodes()
	codeNames := make(map[string][]*graph.Node, len(allNodes)/2)
	for _, n := range allNodes {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage || n.Type == graph.NodeSection {
			continue
		}
		name := n.Name
		// For qualified method names like "Store.Close", index both
		// the full name and the method-only part.
		if len(name) < 4 {
			continue
		}
		codeNames[name] = append(codeNames[name], n)
		if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
			short := name[dot+1:]
			if len(short) >= 4 {
				codeNames[short] = append(codeNames[short], n)
			}
		}
	}

	var created int
	for _, sec := range sections {
		body := sec.Metadata["body"]
		if body == "" {
			continue
		}

		// Extract identifiers from the section body.
		refs := extractEntityRefs(body)

		// Deduplicate edges per section: one EXPLAINS edge per unique target.
		seen := make(map[graph.NodeID]bool)

		for _, ref := range refs {
			targets, ok := codeNames[ref]
			if !ok {
				continue
			}
			for _, target := range targets {
				if seen[target.ID] {
					continue
				}
				seen[target.ID] = true

				// EXPLAINS: section → code entity
				g.AddEdge(&graph.Edge{
					From: sec.ID,
					To:   target.ID,
					Type: graph.EdgeExplains,
				})
				// DOCUMENTED_BY: code entity → section
				g.AddEdge(&graph.Edge{
					From: target.ID,
					To:   sec.ID,
					Type: graph.EdgeDocumentedBy,
				})
				created++
			}
		}
	}

	return created
}

// backtickRe matches backtick-wrapped identifiers: `FunctionName`
var backtickRe = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_.]*)`")

// extractEntityRefs extracts potential code entity names from doc text.
// Two strategies (per research — explicit mentions have 3x higher precision):
//  1. Backtick-wrapped identifiers: `Store.Close` (highest confidence)
//  2. CamelCase/PascalCase tokens in prose: FlatGraph, AddNode (moderate confidence)
//
// Returns deduplicated list of candidate names, all ≥ 4 chars.
func extractEntityRefs(body string) []string {
	seen := make(map[string]bool)
	var refs []string

	// Strategy 1: backtick-wrapped identifiers.
	for _, m := range backtickRe.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		ident := m[1]
		if len(ident) < 4 {
			continue
		}
		if !seen[ident] {
			seen[ident] = true
			refs = append(refs, ident)
		}
		// Also add the last dot-segment for qualified names.
		if dot := strings.LastIndexByte(ident, '.'); dot >= 0 {
			short := ident[dot+1:]
			if len(short) >= 4 && !seen[short] {
				seen[short] = true
				refs = append(refs, short)
			}
		}
	}

	// Strategy 2: CamelCase/PascalCase words in prose (not inside backticks).
	// Strip backtick spans first to avoid double-counting.
	stripped := backtickRe.ReplaceAllString(body, " ")
	for _, word := range splitWords(stripped) {
		if len(word) < 4 {
			continue
		}
		if !isCamelCase(word) {
			continue
		}
		if seen[word] {
			continue
		}
		seen[word] = true
		refs = append(refs, word)
	}

	return refs
}

// splitWords splits text into words on whitespace and common punctuation.
func splitWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';' || r == ':' ||
			r == '(' || r == ')' || r == '[' || r == ']' || r == '{' || r == '}' ||
			r == '"' || r == '\'' || r == '/' || r == '|' || r == '=' ||
			r == '<' || r == '>' || r == '!' || r == '?'
	})
}

// isCamelCase returns true if the word follows CamelCase/PascalCase conventions:
// starts with an uppercase letter and contains at least one lowercase letter.
// This filters out ALL_CAPS constants and plain lowercase words.
func isCamelCase(word string) bool {
	if len(word) == 0 {
		return false
	}
	runes := []rune(word)
	if !unicode.IsUpper(runes[0]) {
		return false
	}
	hasLower := false
	for _, r := range runes[1:] {
		if unicode.IsLower(r) {
			hasLower = true
		}
		// Allow dots for qualified names like Store.Close
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '_' {
			return false
		}
	}
	return hasLower
}
