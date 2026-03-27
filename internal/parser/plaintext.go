package parser

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// PlaintextParser extracts structural sections from plain text (.txt) and
// reStructuredText (.rst) files. Like MarkdownParser, each detected heading
// becomes a Section node with CONTAINS edges forming a tree.
//
// RST heading detection follows the docutils spec: a text line followed by an
// underline of repeated punctuation characters (=, -, ~, ^, ", #, +, *, .)
// where the underline is at least as long as the text. An optional overline
// (same character) promotes the heading. Heading depth is assigned by the
// order in which underline characters first appear in the file.
//
// TXT heading detection uses heuristics:
//   - ALL-CAPS lines of 3+ characters → depth 1
//   - Lines ending with ":" preceded by a blank line → depth 2
//   - Fallback: double-blank-line separated paragraphs → depth 1
type PlaintextParser struct{}

// NewPlaintextParser returns a parser for plaintext and RST documentation files.
func NewPlaintextParser() *PlaintextParser { return &PlaintextParser{} }

func (p *PlaintextParser) Extensions() []string {
	return []string{".txt", ".rst"}
}

// Parse extracts heading-based sections from a plaintext or RST file.
func (p *PlaintextParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:     fileNodeID,
		Type:   graph.NodeFile,
		Name:   filepath.Base(filePath),
		File:   filePath,
		Line:   1,
		Domain: graph.DomainDocs,
	})

	ext := strings.ToLower(filepath.Ext(filePath))
	var sections []section
	switch ext {
	case ".rst":
		sections = extractRSTSections(src)
	default:
		sections = extractTxtSections(src)
	}

	if len(sections) == 0 {
		return nil
	}

	// Create Section nodes and CONTAINS edges (same pattern as MarkdownParser).
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
		if len(sec.CodeBlocks) > 0 {
			if cbJSON, err := json.Marshal(sec.CodeBlocks); err == nil {
				meta["code_blocks"] = string(cbJSON)
			}
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

	return nil
}

// ── RST Section Extraction ──────────────────────────────────────────────────

// rstUnderlineChars is the set of characters that can form RST heading underlines.
const rstUnderlineChars = "=-~^\"#+*."

// isRSTUnderline checks if a line is a valid RST underline (all same punctuation char,
// at least 3 characters long).
func isRSTUnderline(s string) (byte, bool) {
	trimmed := strings.TrimRight(s, " \t\r")
	if len(trimmed) < 3 {
		return 0, false
	}
	ch := trimmed[0]
	if !strings.ContainsRune(rstUnderlineChars, rune(ch)) {
		return 0, false
	}
	for i := 1; i < len(trimmed); i++ {
		if trimmed[i] != ch {
			return 0, false
		}
	}
	return ch, true
}

// extractRSTSections parses RST heading patterns and collects body text.
// RST heading hierarchy is determined by order of first appearance of the
// underline character (optionally with overline). A heading with both overline
// and underline using the same character is considered a distinct (higher) style
// than underline-only with that character.
func extractRSTSections(src []byte) []section {
	lines := strings.Split(string(src), "\n")
	if len(lines) == 0 {
		return nil
	}

	// styleKey encodes the heading decoration style for depth ordering.
	// hasOverline distinguishes "===\nTitle\n===" from "Title\n===".
	type styleKey struct {
		char        byte
		hasOverline bool
	}
	var styleOrder []styleKey
	depthOf := func(sk styleKey) int {
		for i, s := range styleOrder {
			if s == sk {
				return i + 1
			}
		}
		styleOrder = append(styleOrder, sk)
		return len(styleOrder)
	}

	var sections []section

	for i := 0; i < len(lines); i++ {
		// Skip blank lines.
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}

		// Case 1: overline + title + underline.
		// Current line is an underline char, next line is text, line after is same underline.
		if overCh, ok := isRSTUnderline(lines[i]); ok {
			overLen := len(strings.TrimRight(lines[i], " \t\r"))
			if i+2 < len(lines) {
				titleLine := strings.TrimSpace(lines[i+1])
				if titleLine != "" && len(titleLine) <= overLen {
					if underCh, ok2 := isRSTUnderline(lines[i+2]); ok2 && underCh == overCh {
						sk := styleKey{char: overCh, hasOverline: true}
						depth := depthOf(sk)
						sections = append(sections, section{
							Title:        titleLine,
							Depth:        depth,
							Line:         i + 2, // 1-based, the title line
							bodyStartIdx: i + 3, // after the underline
						})
						i += 2 // skip title and underline
						continue
					}
				}
			}
		}

		// Case 2: title + underline (no overline).
		// Current line is non-empty text, next line is an underline at least as long.
		if i+1 < len(lines) && trimmed != "" {
			underCh, ok := isRSTUnderline(lines[i+1])
			if ok {
				underLen := len(strings.TrimRight(lines[i+1], " \t\r"))
				if underLen >= len(trimmed) {
					sk := styleKey{char: underCh, hasOverline: false}
					depth := depthOf(sk)
					sections = append(sections, section{
						Title:        trimmed,
						Depth:        depth,
						Line:         i + 1, // 1-based, the title line
						bodyStartIdx: i + 2, // after the underline
					})
					i++ // skip underline
					continue
				}
			}
		}
	}

	// Disambiguate duplicate titles.
	titleCount := make(map[string]int, len(sections))
	for i := range sections {
		base := sections[i].Title
		titleCount[base]++
		if titleCount[base] > 1 {
			sections[i].Title = fmt.Sprintf("%s (%d)", base, titleCount[base])
		}
	}

	// Fill body text.
	fillSectionBodies(lines, sections)

	// Extract RST code-block directives and attach to sections.
	extractRSTCodeBlocks(lines, sections)

	return sections
}

// extractRSTCodeBlocks parses RST `.. code-block:: lang` directives and attaches
// the indented content to the enclosing section.
func extractRSTCodeBlocks(lines []string, sections []section) {
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, ".. code-block::") && !strings.HasPrefix(trimmed, ".. sourcecode::") {
			continue
		}
		// Extract language tag.
		lang := ""
		if idx := strings.Index(trimmed, "::"); idx >= 0 {
			lang = strings.TrimSpace(trimmed[idx+2:])
		}
		directiveLine := i + 1 // 1-based

		// Find the indented content block (skip blank lines, then collect indented).
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		if j >= len(lines) {
			continue
		}
		// Determine indent level from first content line.
		indent := 0
		for _, ch := range lines[j] {
			if ch == ' ' {
				indent++
			} else if ch == '\t' {
				indent += 4
			} else {
				break
			}
		}
		if indent == 0 {
			continue // no indented block
		}

		var contentLines []string
		for j < len(lines) {
			line := lines[j]
			if strings.TrimSpace(line) == "" {
				contentLines = append(contentLines, "")
				j++
				continue
			}
			// Check if still indented.
			lineIndent := 0
			for _, ch := range line {
				if ch == ' ' {
					lineIndent++
				} else if ch == '\t' {
					lineIndent += 4
				} else {
					break
				}
			}
			if lineIndent < indent {
				break
			}
			// Strip the common indent.
			if len(line) > indent {
				contentLines = append(contentLines, line[indent:])
			} else {
				contentLines = append(contentLines, strings.TrimSpace(line))
			}
			j++
		}

		content := strings.TrimSpace(strings.Join(contentLines, "\n"))
		if content == "" {
			continue
		}

		// Find which section owns this directive.
		secIdx := -1
		for si := len(sections) - 1; si >= 0; si-- {
			if sections[si].Line <= directiveLine {
				secIdx = si
				break
			}
		}
		if secIdx < 0 {
			continue
		}
		if len(sections[secIdx].CodeBlocks) >= 5 {
			continue
		}
		if len(content) > 2000 {
			content = content[:2000]
		}
		sections[secIdx].CodeBlocks = append(sections[secIdx].CodeBlocks, codeBlock{
			Language: lang,
			Content:  content,
			Line:     directiveLine,
		})
	}
}

// ── TXT Section Extraction ──────────────────────────────────────────────────

// isAllCapsLine returns true if the line has 3+ characters and no lowercase letters.
func isAllCapsLine(s string) bool {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < 3 {
		return false
	}
	hasUpper := false
	for _, r := range trimmed {
		if unicode.IsLower(r) {
			return false
		}
		if unicode.IsUpper(r) {
			hasUpper = true
		}
	}
	return hasUpper
}

// extractTxtSections uses heuristics to find headings in plain text files.
func extractTxtSections(src []byte) []section {
	lines := strings.Split(string(src), "\n")
	if len(lines) == 0 {
		return nil
	}

	// First pass: look for structured headings (all-caps H1, colon-suffix H2).
	var sections []section
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// ALL-CAPS lines → depth 1 heading.
		if isAllCapsLine(trimmed) {
			sections = append(sections, section{
				Title:        toTitleCase(trimmed),
				Depth:        1,
				Line:         i + 1,
				bodyStartIdx: i + 1,
			})
			continue
		}

		// Line ending with ":" preceded by a blank line → depth 2 heading.
		if strings.HasSuffix(trimmed, ":") && i > 0 && strings.TrimSpace(lines[i-1]) == "" {
			title := strings.TrimSuffix(trimmed, ":")
			if title != "" && len(title) < 80 {
				sections = append(sections, section{
					Title:        title,
					Depth:        2,
					Line:         i + 1,
					bodyStartIdx: i + 1,
				})
			}
		}
	}

	// Fallback: if no headings found, split on double-blank-lines as paragraphs.
	if len(sections) == 0 {
		sections = splitParagraphs(lines)
	}

	// Disambiguate duplicate titles.
	titleCount := make(map[string]int, len(sections))
	for i := range sections {
		base := sections[i].Title
		titleCount[base]++
		if titleCount[base] > 1 {
			sections[i].Title = fmt.Sprintf("%s (%d)", base, titleCount[base])
		}
	}

	fillSectionBodies(lines, sections)
	return sections
}

// splitParagraphs creates sections from double-blank-line-separated paragraphs.
// Returns at most 20 sections for large files.
func splitParagraphs(lines []string) []section {
	var sections []section
	inBlank := true
	paraStart := 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if !inBlank {
				inBlank = true
			}
			continue
		}
		if inBlank {
			// Start of a new paragraph.
			inBlank = false
			paraStart = i

			// Use first line as title (truncate if long).
			title := trimmed
			if len(title) > 60 {
				title = title[:57] + "..."
			}
			sections = append(sections, section{
				Title:        title,
				Depth:        1,
				Line:         paraStart + 1,
				bodyStartIdx: paraStart,
			})
			if len(sections) >= 20 {
				break
			}
		}
	}
	return sections
}

// toTitleCase converts an ALL-CAPS string to Title Case for readability.
func toTitleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
		}
	}
	return strings.Join(words, " ")
}

// ── Shared Helpers ──────────────────────────────────────────────────────────

// fillSectionBodies populates Body and BodyPreview for each section using
// the range between consecutive headings (same logic as MarkdownParser).
func fillSectionBodies(lines []string, sections []section) {
	for i := range sections {
		var endLine int
		if i+1 < len(sections) {
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
}
