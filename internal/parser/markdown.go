package parser

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// MarkdownParser extracts structural sections from Markdown files (.md, .markdown, .mdx).
// Each ATX heading becomes a Section node with CONTAINS edges forming a tree.
// Cross-document [text](path.md) links become LINKS_TO edges.
// Entity linking (EXPLAINS/DOCUMENTED_BY) is deferred to resolver.ResolveDocEdges
// which runs after all files are parsed, ensuring code entities are available.
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
	// File node (same as genericParser).
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:     fileNodeID,
		Type:   graph.NodeFile,
		Name:   filepath.Base(filePath),
		File:   filePath,
		Line:   1,
		Domain: graph.DomainDocs,
	})

	sections := extractSections(src)
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
	Title       string // heading text (without # prefix)
	Depth       int    // heading level (1-6)
	Line        int    // 1-based line number
	Body        string // full body text between this heading and the next (max 2000 chars)
	BodyPreview string // first 200 chars of body
}

// extractSections parses ATX headings and collects body text between them.
// Headings inside fenced code blocks (``` or ~~~) are skipped per CommonMark.
// Duplicate heading names within a file are disambiguated by appending a
// counter suffix so each Section gets a stable, unique node ID.
func extractSections(src []byte) []section {
	lines := strings.Split(string(src), "\n")
	var sections []section

	inFence := false
	var fenceChar byte // '`' or '~'
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Detect fenced code block open/close (``` or ~~~, 3+ chars).
		if !inFence {
			if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
				allSame := true
				ch := trimmed[0]
				for j := 1; j < 3; j++ {
					if trimmed[j] != ch {
						allSame = false
						break
					}
				}
				if allSame {
					inFence = true
					fenceChar = ch
					continue
				}
			}
		} else {
			// Inside a fence: look for the closing delimiter (same char, 3+).
			if len(trimmed) >= 3 && trimmed[0] == fenceChar {
				allSame := true
				for j := 1; j < 3; j++ {
					if trimmed[j] != fenceChar {
						allSame = false
						break
					}
				}
				if allSame {
					inFence = false
					fenceChar = 0
				}
			}
			continue // skip all lines inside a fence
		}
		depth, title := parseATXHeading(line)
		if depth == 0 {
			continue
		}
		sections = append(sections, section{
			Title: title,
			Depth: depth,
			Line:  i + 1, // 1-based
		})
	}

	// Disambiguate duplicate heading titles within the file so each section
	// gets a unique name (and thus a unique node ID).
	titleCount := make(map[string]int, len(sections))
	for i := range sections {
		base := sections[i].Title
		titleCount[base]++
		if titleCount[base] > 1 {
			sections[i].Title = fmt.Sprintf("%s (%d)", base, titleCount[base])
		}
	}

	// Fill in body text for each section: everything between this heading
	// and the next heading (or end of file).
	for i := range sections {
		startLine := sections[i].Line // heading line (1-based)
		var endLine int
		if i+1 < len(sections) {
			endLine = sections[i+1].Line - 1
		} else {
			endLine = len(lines)
		}

		// Body starts on the line after the heading.
		var bodyLines []string
		for li := startLine; li < endLine && li < len(lines); li++ {
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

	return sections
}

// parseATXHeading returns (depth, title) for an ATX heading line.
// Returns (0, "") if the line is not a heading.
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
