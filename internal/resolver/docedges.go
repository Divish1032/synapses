package resolver

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ResolveDocEdges scans ALL Section nodes and markdown file nodes in the graph
// and creates EXPLAINS (doc→code) and DOCUMENTED_BY (code→doc) edges for
// identifiers found in section body text, section titles, and frontmatter titles.
//
// Must be called after all files are parsed so code entity nodes exist.
// Returns the number of EXPLAINS edges created.
//
// Use ResolveDocEdgesForFile for incremental updates when only a single
// markdown file changed (avoids rescanning the entire graph).
func ResolveDocEdges(g *graph.Graph) int {
	sections := g.FindByType(graph.NodeSection)
	files := g.FindByType(graph.NodeFile)
	if len(sections) == 0 && len(files) == 0 {
		return 0
	}
	codeNames := buildCodeNames(g)
	return linkSections(g, sections, codeNames) + linkDocFiles(g, files, codeNames)
}

// ResolveDocEdgesForFile resolves doc edges only for Section nodes and the
// file node that belong to filePath. All other sections' edges are left intact.
//
// Use this in the watcher when a single markdown file is reparsed:
// code entities are unchanged so only the new file's sections need linking.
// Returns the number of EXPLAINS edges created.
func ResolveDocEdgesForFile(g *graph.Graph, filePath string) int {
	abs := filepath.Clean(filePath)

	// Filter sections to this file only.
	var sections []*graph.Node
	for _, s := range g.FindByType(graph.NodeSection) {
		if filepath.Clean(s.File) == abs {
			sections = append(sections, s)
		}
	}

	// Filter file nodes to this file only (for frontmatter title).
	var files []*graph.Node
	for _, f := range g.FindByType(graph.NodeFile) {
		if filepath.Clean(f.File) == abs {
			files = append(files, f)
		}
	}

	if len(sections) == 0 && len(files) == 0 {
		return 0
	}
	codeNames := buildCodeNames(g)
	return linkSections(g, sections, codeNames) + linkDocFiles(g, files, codeNames)
}

// linkDocFiles creates EXPLAINS and DOCUMENTED_BY edges from markdown file nodes
// that declare their topic via a frontmatter title field. Handles:
//
//	YAML: title: "FlatGraph Architecture"
//	TOML: title = "FlatGraph Architecture"
//
// This is the highest-confidence doc-code signal when no in-body references exist.
func linkDocFiles(g *graph.Graph, files []*graph.Node, codeNames map[string][]*graph.Node) int {
	var created int
	for _, f := range files {
		if f.Domain != graph.DomainDocs {
			continue
		}
		fmTitle := f.Metadata["frontmatter_title"]
		if fmTitle == "" {
			continue
		}
		refs := extractEntityRefs(fmTitle)
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
				g.AddEdge(&graph.Edge{From: f.ID, To: target.ID, Type: graph.EdgeExplains})
				g.AddEdge(&graph.Edge{From: target.ID, To: f.ID, Type: graph.EdgeDocumentedBy})
				created++
			}
		}
	}
	return created
}

// buildCodeNames builds a lookup map from identifier name → code nodes.
// Only includes names ≥ 4 chars to avoid false positives on short names
// like "New", "Run", "Get", "err". Qualified names like "Store.Close"
// are indexed under both the full name and the method suffix "Close".
func buildCodeNames(g *graph.Graph) map[string][]*graph.Node {
	allNodes := g.AllNodes()
	codeNames := make(map[string][]*graph.Node, len(allNodes)/2)
	for _, n := range allNodes {
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage || n.Type == graph.NodeSection {
			continue
		}
		name := n.Name
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
	return codeNames
}

// linkSections creates EXPLAINS and DOCUMENTED_BY edges for the given sections.
// Scans both the section title (primary signal) and body text (secondary signal).
// Research shows heading text is the highest-confidence doc-code link signal:
// `## FlatGraph Architecture` → FlatGraph entity even with no body text.
func linkSections(g *graph.Graph, sections []*graph.Node, codeNames map[string][]*graph.Node) int {
	var created int
	for _, sec := range sections {
		title := sec.Metadata["title"]
		body := sec.Metadata["body"]
		text := strings.TrimSpace(title + " " + body)
		if text == "" {
			continue
		}

		refs := extractEntityRefs(text)
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

				g.AddEdge(&graph.Edge{From: sec.ID, To: target.ID, Type: graph.EdgeExplains})
				g.AddEdge(&graph.Edge{From: target.ID, To: sec.ID, Type: graph.EdgeDocumentedBy})
				created++
			}
		}
	}
	return created
}

// ── Regex patterns ────────────────────────────────────────────────────────────

// backtickRe matches the full content of a backtick span: `...`
// We capture everything inside and extract the leading identifier separately,
// so that `Store.Close(ctx)` yields "Store.Close" rather than being rejected.
var backtickRe = regexp.MustCompile("`([^`]+)`")

// htmlCodeRe matches <code>identifier</code> and <code class="...">identifier</code>.
// Only captures simple single-word content (no nested tags) to stay high-precision.
var htmlCodeRe = regexp.MustCompile(`<code[^>]*>([A-Za-z_][A-Za-z0-9_.(<[^<]*>]*?)</code>`)

// extractEntityRefs extracts potential code entity names from doc body text.
//
// Three strategies in descending confidence:
//  1. Backtick spans `Store.Close(ctx)` → extracts leading identifier "Store.Close"
//  2. HTML <code>FunctionName</code> → extracts the identifier directly
//  3. CamelCase/PascalCase tokens in prose: FlatGraph, AddNode (moderate confidence)
//
// Returns a deduplicated list of candidate names, all ≥ 4 chars.
func extractEntityRefs(body string) []string {
	seen := make(map[string]bool)
	var refs []string

	addRef := func(ident string) {
		ident = strings.TrimRight(ident, "._,;:")
		if len(ident) < 4 || seen[ident] {
			return
		}
		seen[ident] = true
		refs = append(refs, ident)
		// Also index the last dot-segment for qualified names like "Store.Close".
		if dot := strings.LastIndexByte(ident, '.'); dot >= 0 {
			short := strings.TrimRight(ident[dot+1:], "._,;:")
			if len(short) >= 4 && !seen[short] {
				seen[short] = true
				refs = append(refs, short)
			}
		}
	}

	// ── Strategy 1: backtick spans ──────────────────────────────────────────
	// Capture the full span content, then extract the leading identifier
	// before any `(`, `[`, ` `, `<` so that `AddNode(n *Node)` → "AddNode".
	for _, m := range backtickRe.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		content := strings.TrimSpace(m[1])
		// Extract leading identifier (stop at first non-identifier character).
		ident := leadingIdentifier(content)
		addRef(ident)
	}

	// ── Strategy 2: HTML <code> tags ───────────────────────────────────────
	for _, m := range htmlCodeRe.FindAllStringSubmatch(body, -1) {
		if len(m) < 2 {
			continue
		}
		addRef(leadingIdentifier(strings.TrimSpace(m[1])))
	}

	// ── Strategy 3: CamelCase/PascalCase prose tokens ──────────────────────
	// Strip backtick and HTML-code spans first to avoid double-counting.
	stripped := backtickRe.ReplaceAllString(body, " ")
	stripped = htmlCodeRe.ReplaceAllString(stripped, " ")
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

// leadingIdentifier extracts the identifier prefix from a string by stopping
// at the first character that cannot appear in a Go/multi-language identifier.
// Examples:
//
//	"Store.Close(ctx)" → "Store.Close"
//	"AddNode(n)"       → "AddNode"
//	"graph.New"        → "graph.New"
//	"err"              → "err"
func leadingIdentifier(s string) string {
	end := len(s)
	for i, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' {
			end = i
			break
		}
	}
	return strings.TrimRight(s[:end], "._")
}

// splitWords splits text into words on whitespace and common punctuation.
// '*' and '_' are included as delimiters so that Markdown bold (**Name**)
// and italic (*Name* / _Name_) markers are stripped before CamelCase matching.
func splitWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';' || r == ':' ||
			r == '(' || r == ')' || r == '[' || r == ']' || r == '{' || r == '}' ||
			r == '"' || r == '\'' || r == '/' || r == '|' || r == '=' ||
			r == '<' || r == '>' || r == '!' || r == '?' ||
			r == '*' || r == '_' // bold/italic markdown markers
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
