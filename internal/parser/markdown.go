package parser

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// MarkdownParser extracts structural sections from Markdown files (.md, .markdown, .mdx).
// Each ATX or setext heading becomes a Section node with CONTAINS edges forming a tree.
// Cross-document [text](path.md) links become LINKS_TO edges.
// Entity linking (EXPLAINS/DOCUMENTED_BY) is deferred to resolver.ResolveDocEdges
// which runs after all files are parsed, ensuring code entities are available.
//
// Handles:
//   - ATX headings (# H1 through ###### H6) per CommonMark
//   - Setext headings (underline-style === and ---) per CommonMark
//   - YAML/TOML frontmatter (--- block at top of file) is skipped
//   - Headings inside fenced code blocks (``` or ~~~) are skipped
//   - Duplicate heading titles disambiguated with counter suffix
type MarkdownParser struct{}

// NewMarkdownParser returns a parser for Markdown documentation files.
func NewMarkdownParser() *MarkdownParser { return &MarkdownParser{} }

func (p *MarkdownParser) Extensions() []string {
	return []string{".md", ".markdown", ".mdx"}
}

// Parse extracts heading-based sections from a Markdown file.
// Creates: file node, Section nodes (one per heading), CONTAINS edges (hierarchy),
// and LINKS_TO edges for relative markdown links.
func (p *MarkdownParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	// File node. Frontmatter title (if present) is stored in metadata so that
	// resolver.ResolveDocEdges can create EXPLAINS edges from the file to the
	// entities it documents (highest-confidence doc-code signal per arxiv:2506.16440).
	fileNodeID := g.MakeNodeID(filePath, filePath)
	sections, fmTitle := extractSections(src)
	var fileMeta map[string]string
	if fmTitle != "" {
		fileMeta = map[string]string{"frontmatter_title": fmTitle}
	}
	g.AddNode(&graph.Node{
		ID:       fileNodeID,
		Type:     graph.NodeFile,
		Name:     filepath.Base(filePath),
		File:     filePath,
		Line:     1,
		Domain:   graph.DomainDocs,
		Metadata: fileMeta,
	})

	if len(sections) == 0 {
		return nil
	}

	// Create Section nodes and CONTAINS edges.
	// parentStack tracks the most recent section at each depth level
	// so we can wire CONTAINS from parent heading to child heading.
	type stackEntry struct {
		depth  int
		nodeID graph.NodeID
	}
	var parentStack []stackEntry

	for _, sec := range sections {
		sectionName := fmt.Sprintf("%s § %s", filepath.Base(filePath), sec.Title)
		sectionNodeID := g.MakeNodeID(filePath, sectionName)

		meta := map[string]string{
			"title": sec.Title,
			"depth": fmt.Sprintf("%d", sec.Depth),
		}
		if sec.BodyPreview != "" {
			meta["body_preview"] = sec.BodyPreview
		}
		if sec.Body != "" {
			meta["body"] = sec.Body
		}

		g.AddNode(&graph.Node{
			ID:       sectionNodeID,
			Type:     graph.NodeSection,
			Name:     sectionName,
			File:     filePath,
			Line:     sec.Line,
			Domain:   graph.DomainDocs,
			Metadata: meta,
		})

		// Wire CONTAINS edge: find the nearest ancestor heading with a
		// shallower depth. If none, the parent is the file node.
		for len(parentStack) > 0 && parentStack[len(parentStack)-1].depth >= sec.Depth {
			parentStack = parentStack[:len(parentStack)-1]
		}

		parentID := fileNodeID
		if len(parentStack) > 0 {
			parentID = parentStack[len(parentStack)-1].nodeID
		}
		g.AddEdge(&graph.Edge{
			From: parentID,
			To:   sectionNodeID,
			Type: graph.EdgeContains,
		})

		parentStack = append(parentStack, stackEntry{depth: sec.Depth, nodeID: sectionNodeID})
	}

	// Extract and create LINKS_TO edges for relative markdown links.
	links := extractMarkdownLinks(src)
	dir := filepath.Dir(filePath)
	for _, link := range links {
		if isExternalLink(link) {
			continue
		}
		// Strip fragment (#section-name) for file resolution.
		target := link
		if idx := strings.IndexByte(target, '#'); idx >= 0 {
			target = target[:idx]
		}
		if target == "" {
			continue // self-referencing anchor link
		}
		targetPath := filepath.Join(dir, target)
		targetPath = filepath.Clean(targetPath)
		targetNodeID := g.MakeNodeID(targetPath, targetPath)
		g.AddEdge(&graph.Edge{
			From: fileNodeID,
			To:   targetNodeID,
			Type: graph.EdgeLinksTo,
		})
	}

	return nil
}

// section represents a parsed heading section from a markdown file.
type section struct {
	Title        string // heading text (without # prefix)
	Depth        int    // heading level (1-6)
	Line         int    // 1-based line number of heading text
	bodyStartIdx int    // 0-based index into lines[] where body begins (after heading + underline)
	Body         string // full body text between this heading and the next (max 2000 chars)
	BodyPreview  string // first 200 chars of body
}

// extractSections parses ATX and setext headings and collects body text.
//
// extractSections parses ATX and setext headings and collects body text.
// Returns the list of sections and the frontmatter title (if the file starts
// with YAML/TOML frontmatter that contains a `title:` / `title =` field).
func extractSections(src []byte) ([]section, string) {
	lines := strings.Split(string(src), "\n")
	if len(lines) == 0 {
		return nil, ""
	}

	// ── Step 1: skip YAML/TOML frontmatter, extract title field ─────────────
	// Frontmatter is a --- or +++ block at the VERY start of the file.
	startIdx := 0
	var frontmatterTitle string
	if len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		if first == "---" || first == "+++" {
			closer := first
			for i := 1; i < len(lines); i++ {
				if strings.TrimSpace(lines[i]) == closer {
					startIdx = i + 1 // resume after closing delimiter
					break
				}
				// Extract title from YAML (`title: "..."`) or TOML (`title = "..."`).
				line := strings.TrimSpace(lines[i])
				var val string
				if strings.HasPrefix(line, "title:") {
					val = strings.TrimSpace(strings.TrimPrefix(line, "title:"))
				} else if strings.HasPrefix(line, "title =") {
					val = strings.TrimSpace(strings.TrimPrefix(line, "title ="))
				}
				if val != "" {
					frontmatterTitle = strings.TrimSpace(strings.Trim(val, `"'`))
				}
			}
		}
	}

	// ── Step 2: collect headings (ATX + setext), skip fenced blocks ─────────
	var sections []section
	inFence := false
	var fenceChar byte // '`' or '~'

	for i := startIdx; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Detect fenced code block boundaries.
		if !inFence {
			if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
				ch := trimmed[0]
				if trimmed[1] == ch && trimmed[2] == ch {
					inFence = true
					fenceChar = ch
					continue
				}
			}
		} else {
			if len(trimmed) >= 3 && trimmed[0] == fenceChar &&
				trimmed[1] == fenceChar && trimmed[2] == fenceChar {
				inFence = false
				fenceChar = 0
			}
			continue
		}

		// ATX heading: `# Title`
		if depth, title := parseATXHeading(line); depth > 0 {
			sections = append(sections, section{
				Title:        title,
				Depth:        depth,
				Line:         i + 1, // 1-based
				bodyStartIdx: i + 1, // lines[i+1] = line after heading
			})
			continue
		}

		// Setext heading: non-empty text line followed by === (H1) or --- (H2).
		// The NEXT line must be the underline — peek ahead.
		if trimmed != "" && i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if isSetextUnderline(next) {
				depth := 1
				if next[0] == '-' {
					depth = 2
				}
				sections = append(sections, section{
					Title:        strings.TrimSpace(line),
					Depth:        depth,
					Line:         i + 1,     // 1-based: the text line
					bodyStartIdx: i + 2,     // skip both text and underline lines
				})
				i++ // consume the underline line
				continue
			}
		}
	}

	// ── Step 3: disambiguate duplicate titles ────────────────────────────────
	titleCount := make(map[string]int, len(sections))
	for i := range sections {
		base := sections[i].Title
		titleCount[base]++
		if titleCount[base] > 1 {
			sections[i].Title = fmt.Sprintf("%s (%d)", base, titleCount[base])
		}
	}

	// ── Step 4: fill body text for each section ──────────────────────────────
	for i := range sections {
		var endLine int
		if i+1 < len(sections) {
			// End just before the text line of the next heading.
			endLine = sections[i+1].Line - 1
		} else {
			endLine = len(lines)
		}

		var bodyLines []string
		for li := sections[i].bodyStartIdx; li < endLine && li < len(lines); li++ {
			bodyLines = append(bodyLines, lines[li])
		}
		body := strings.TrimSpace(strings.Join(bodyLines, "\n"))

		if len(body) > 2000 {
			sections[i].Body = body[:2000]
		} else {
			sections[i].Body = body
		}
		if len(body) > 200 {
			sections[i].BodyPreview = body[:200]
		} else {
			sections[i].BodyPreview = body
		}
	}

	return sections, frontmatterTitle
}

// isSetextUnderline returns true for a setext heading underline:
// three or more = characters (H1) or - characters (H2), nothing else.
func isSetextUnderline(s string) bool {
	if len(s) < 3 {
		return false
	}
	ch := s[0]
	if ch != '=' && ch != '-' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] != ch {
			return false
		}
	}
	return true
}

// parseATXHeading returns (depth, title) for an ATX heading line.
// Returns (0, "") if the line is not a valid ATX heading.
func parseATXHeading(line string) (int, string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return 0, ""
	}
	depth := 0
	for _, ch := range trimmed {
		if ch == '#' {
			depth++
		} else {
			break
		}
	}
	if depth > 6 {
		return 0, "" // not a valid ATX heading
	}
	// Must have a space after the # characters (CommonMark spec).
	rest := trimmed[depth:]
	if len(rest) > 0 && rest[0] != ' ' {
		return 0, ""
	}
	title := strings.TrimSpace(rest)
	// Strip optional closing #s.
	title = strings.TrimRight(title, "# ")
	return depth, title
}

// mdLinkRe matches markdown links: [text](url)
var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)

// extractMarkdownLinks returns all link targets from [text](url) patterns.
func extractMarkdownLinks(src []byte) []string {
	matches := mdLinkRe.FindAllSubmatch(src, -1)
	var links []string
	for _, m := range matches {
		if len(m) >= 3 {
			links = append(links, string(m[2]))
		}
	}
	return links
}

// isExternalLink returns true for URLs that are not relative file paths.
func isExternalLink(link string) bool {
	return strings.HasPrefix(link, "http://") ||
		strings.HasPrefix(link, "https://") ||
		strings.HasPrefix(link, "mailto:") ||
		strings.HasPrefix(link, "ftp://") ||
		strings.HasPrefix(link, "//")
}
