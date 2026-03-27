package resolver

import (
	"encoding/json"
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
	fileIdx := buildFileIndex(g)
	return linkSections(g, sections, codeNames) + linkDocFiles(g, files, codeNames) + linkSectionsToFiles(g, sections, fileIdx) + linkCodeBlocks(g, sections, codeNames)
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
	fileIdx := buildFileIndex(g)
	return linkSections(g, sections, codeNames) + linkDocFiles(g, files, codeNames) + linkSectionsToFiles(g, sections, fileIdx) + linkCodeBlocks(g, sections, codeNames)
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
		if isTestFile(n.File) {
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
	// Ambiguity cap: remove names that map to >3 targets.
	for name, targets := range codeNames {
		if len(targets) > 3 {
			delete(codeNames, name)
		}
	}
	return codeNames
}

// isTestFile returns true if the file path looks like a test file.
func isTestFile(path string) bool {
	return strings.HasSuffix(path, "_test.go") ||
		strings.HasSuffix(path, ".test.ts") || strings.HasSuffix(path, ".test.js") ||
		strings.HasSuffix(path, ".test.tsx") || strings.HasSuffix(path, ".test.jsx") ||
		strings.HasSuffix(path, "_test.py") || strings.HasSuffix(path, "_spec.rb") ||
		strings.Contains(path, "/test/") || strings.Contains(path, "/tests/") ||
		strings.Contains(path, "/__tests__/") || strings.Contains(path, "/testdata/")
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
		secCreated := 0

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
				secCreated++
			}
		}
		if secCreated > 0 {
			sec.Metadata["doc_link_source"] = "name_match"
		}
		created += secCreated
	}
	return created
}

// ── File-level doc→code linking ───────────────────────────────────────────────

// filePathRe matches file paths in text: at least one directory separator with a
// common source extension. Designed for backtick spans and prose references.
var filePathRe = regexp.MustCompile(`[A-Za-z0-9_.]+/[A-Za-z0-9_./]+\.[a-z]{1,4}`)

// buildFileIndex builds a lookup from basename and suffix paths to file nodes.
// E.g., for "/repo/src/app/main.go": indexes "main.go", "app/main.go", "src/app/main.go".
func buildFileIndex(g *graph.Graph) map[string][]*graph.Node {
	idx := make(map[string][]*graph.Node)
	for _, n := range g.FindByType(graph.NodeFile) {
		if n.Domain == graph.DomainDocs {
			continue // skip doc files — we link doc sections TO code files
		}
		rel := n.File
		// Index under progressively longer suffixes.
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i := len(parts) - 1; i >= 0; i-- {
			suffix := strings.Join(parts[i:], "/")
			idx[suffix] = append(idx[suffix], n)
		}
	}
	return idx
}

// linkSectionsToFiles creates EXPLAINS/DOCUMENTED_BY edges when doc sections
// reference file paths (e.g., `src/app/main.go` in backtick spans or prose).
func linkSectionsToFiles(g *graph.Graph, sections []*graph.Node, fileIdx map[string][]*graph.Node) int {
	var created int
	for _, sec := range sections {
		title := sec.Metadata["title"]
		body := sec.Metadata["body"]
		text := strings.TrimSpace(title + " " + body)
		if text == "" {
			continue
		}

		seen := make(map[graph.NodeID]bool)

		// Extract file paths from backtick spans.
		for _, m := range backtickRe.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 {
				continue
			}
			ref := strings.TrimSpace(m[1])
			if !looksLikeFilePath(ref) {
				continue
			}
			for _, target := range resolveFileRef(ref, fileIdx) {
				if target.ID == sec.ID || seen[target.ID] {
					continue
				}
				seen[target.ID] = true
				g.AddEdge(&graph.Edge{From: sec.ID, To: target.ID, Type: graph.EdgeExplains})
				g.AddEdge(&graph.Edge{From: target.ID, To: sec.ID, Type: graph.EdgeDocumentedBy})
				created++
			}
		}

		// Extract file paths from prose text.
		for _, ref := range filePathRe.FindAllString(text, -1) {
			for _, target := range resolveFileRef(ref, fileIdx) {
				if target.ID == sec.ID || seen[target.ID] {
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

// resolveFileRef looks up a file path reference in the file index.
// Tries the full ref first, then progressively shorter suffixes.
func resolveFileRef(ref string, idx map[string][]*graph.Node) []*graph.Node {
	ref = filepath.ToSlash(ref)
	if nodes, ok := idx[ref]; ok {
		return nodes
	}
	// Try basename.
	base := filepath.Base(ref)
	if nodes, ok := idx[base]; ok {
		return nodes
	}
	return nil
}

// looksLikeFilePath returns true if s looks like a file path reference.
func looksLikeFilePath(s string) bool {
	// Must contain a dot (extension) and either a slash or common extension.
	if !strings.Contains(s, ".") {
		return false
	}
	// Must not be a URL.
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return false
	}
	// Should contain a slash or end with a common extension.
	if strings.Contains(s, "/") {
		return true
	}
	ext := filepath.Ext(s)
	switch ext {
	case ".go", ".py", ".js", ".ts", ".jsx", ".tsx", ".java", ".rb", ".rs",
		".c", ".h", ".cpp", ".hpp", ".cs", ".swift", ".kt", ".scala",
		".yaml", ".yml", ".toml", ".json", ".xml", ".html", ".css", ".scss",
		".md", ".rst", ".txt":
		return true
	}
	return false
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

// ── Code block identifier linking (Phase 3) ─────────────────────────────────

// docCodeBlock mirrors parser.codeBlock for JSON deserialization.
type docCodeBlock struct {
	Language string `json:"language"`
	Content  string `json:"content"`
	Line     int    `json:"line"`
}

// linkCodeBlocks parses code blocks from section metadata (Phase 1) and creates
// EXPLAINS edges for identifiers that exactly match entries in codeNames.
func linkCodeBlocks(g *graph.Graph, sections []*graph.Node, codeNames map[string][]*graph.Node) int {
	var created int
	for _, sec := range sections {
		cbJSON := sec.Metadata["code_blocks"]
		if cbJSON == "" {
			continue
		}
		var blocks []docCodeBlock
		if err := json.Unmarshal([]byte(cbJSON), &blocks); err != nil {
			continue
		}

		// Collect existing EXPLAINS targets to dedup.
		existing := make(map[graph.NodeID]bool)
		for _, e := range g.OutEdges(sec.ID) {
			if e.Type == graph.EdgeExplains {
				existing[e.To] = true
			}
		}

		secCreated := 0
		for _, block := range blocks {
			if secCreated >= 5 {
				break
			}
			idents := extractCodeBlockIdentifiers(block.Content, block.Language)
			for _, ident := range idents {
				if secCreated >= 5 {
					break
				}
				targets, ok := codeNames[ident]
				if !ok {
					continue
				}
				for _, target := range targets {
					if existing[target.ID] {
						continue
					}
					existing[target.ID] = true
					g.AddEdge(&graph.Edge{From: sec.ID, To: target.ID, Type: graph.EdgeExplains})
					g.AddEdge(&graph.Edge{From: target.ID, To: sec.ID, Type: graph.EdgeDocumentedBy})
					secCreated++
					if secCreated >= 5 {
						break
					}
				}
			}
		}
		if secCreated > 0 {
			if sec.Metadata["doc_link_source"] == "" {
				sec.Metadata["doc_link_source"] = "code_block"
			}
		}
		created += secCreated
	}
	return created
}

// Regex patterns for code block identifier extraction.
var (
	// Python/JS imports: `from X import Y`, `import X`, `require('X')`
	pyImportRe  = regexp.MustCompile(`(?:from\s+(\w+)\s+import\s+([\w, ]+)|import\s+([\w.]+))`)
	jsRequireRe = regexp.MustCompile(`require\(['"]([^'"]+)['"]\)`)
	jsImportRe  = regexp.MustCompile(`import\s+(?:\{([^}]+)\}|(\w+))\s+from`)
	// Qualified calls: X.method() where X is CamelCase
	qualCallRe = regexp.MustCompile(`([A-Z][A-Za-z0-9]+)\.\w+\(`)
	// Type annotations: : TypeName or -> TypeName
	typeAnnotRe = regexp.MustCompile(`(?::\s*|->)\s*([A-Z][A-Za-z0-9]+)`)
	// Decorators/attributes: @decorator or #[attribute]
	decoratorRe = regexp.MustCompile(`@([A-Za-z_]\w{3,})`)
	rustAttrRe  = regexp.MustCompile(`#\[([A-Za-z_]\w{3,})`)
	// Go/Rust use: use X
	useImportRe = regexp.MustCompile(`use\s+([\w:]+)`)
)

// extractCodeBlockIdentifiers extracts potential code entity names from a code block.
func extractCodeBlockIdentifiers(content, lang string) []string {
	seen := make(map[string]bool)
	var idents []string

	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) < 4 || seen[s] {
			return
		}
		seen[s] = true
		idents = append(idents, s)
	}

	// Python imports
	for _, m := range pyImportRe.FindAllStringSubmatch(content, -1) {
		if m[1] != "" {
			add(m[1]) // from X
			for _, name := range strings.Split(m[2], ",") {
				add(strings.TrimSpace(name))
			}
		}
		if m[3] != "" {
			add(m[3])
		}
	}

	// JS/TS imports
	for _, m := range jsImportRe.FindAllStringSubmatch(content, -1) {
		if m[1] != "" {
			for _, name := range strings.Split(m[1], ",") {
				add(strings.TrimSpace(name))
			}
		}
		if m[2] != "" {
			add(m[2])
		}
	}
	for _, m := range jsRequireRe.FindAllStringSubmatch(content, -1) {
		// Extract basename for require paths
		parts := strings.Split(m[1], "/")
		add(parts[len(parts)-1])
	}

	// Qualified calls: Flask.run() → Flask
	for _, m := range qualCallRe.FindAllStringSubmatch(content, -1) {
		add(m[1])
	}

	// Type annotations
	for _, m := range typeAnnotRe.FindAllStringSubmatch(content, -1) {
		add(m[1])
	}

	// Decorators
	for _, m := range decoratorRe.FindAllStringSubmatch(content, -1) {
		add(m[1])
	}
	for _, m := range rustAttrRe.FindAllStringSubmatch(content, -1) {
		add(m[1])
	}

	// Use imports (Go/Rust)
	for _, m := range useImportRe.FindAllStringSubmatch(content, -1) {
		parts := strings.Split(m[1], "::")
		add(parts[len(parts)-1])
	}

	return idents
}
