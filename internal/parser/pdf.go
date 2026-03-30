// Package parser — Sprint 27.4: PDF document parser.
//
// Extracts text from PDF files and creates Section nodes in the graph,
// following the same pattern as MarkdownParser and PlaintextParser.
//
// Text extraction strategy:
//   1. Primary: shell out to `pdftotext` (poppler-utils, widely available)
//   2. Fallback: skip file with a log message (no Go PDF library dependency)
//
// Guards: skip files >10MB, cap at 50 pages, 5s timeout on extraction.
// Section splitting: form-feed page boundaries, then heading detection within pages.
package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/SynapsesOS/synapses/internal/graph"
)

const (
	pdfMaxFileSize = 10 * 1024 * 1024 // 10 MB
	pdfMaxPages    = 50
	pdfExtractTimeout = 5 * time.Second
)

// PDFParser extracts text from PDF files and creates Section nodes.
type PDFParser struct{}

// NewPDFParser returns a parser for PDF documents.
func NewPDFParser() *PDFParser { return &PDFParser{} }

// Extensions returns the file extensions handled by PDFParser.
func (p *PDFParser) Extensions() []string {
	return []string{".pdf"}
}

// Parse extracts sections from a PDF file. The src parameter is ignored —
// PDF binary content must be read from the file directly by pdftotext.
func (p *PDFParser) Parse(g *graph.Graph, filePath string, _ []byte) error {
	// Guard: check file size before attempting extraction.
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil // file disappeared — skip silently
	}
	if fi.Size() > pdfMaxFileSize {
		return nil // too large — skip silently
	}

	text, err := extractPDFText(filePath)
	if err != nil {
		// Log once so users know PDFs aren't being indexed.
		// Common cause: pdftotext not installed (poppler-utils).
		fmt.Fprintf(os.Stderr, "synapses: skipping PDF %s: %v\n", filepath.Base(filePath), err)
		return nil
	}
	if strings.TrimSpace(text) == "" {
		return nil // empty PDF — skip silently
	}

	// Create file node.
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:     fileNodeID,
		Type:   graph.NodeFile,
		Name:   filepath.Base(filePath),
		File:   filePath,
		Line:   1,
		Domain: graph.DomainDocs,
	})

	// Split into sections.
	sections := extractPDFSections(text)
	if len(sections) == 0 {
		return nil
	}

	// Create Section nodes and CONTAINS edges (same pattern as PlaintextParser).
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

// extractPDFText shells out to pdftotext to extract text from a PDF file.
// Returns empty string + error if pdftotext is not available.
func extractPDFText(filePath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pdfExtractTimeout)
	defer cancel()

	// pdftotext args: -layout preserves formatting, -l N limits pages, - means stdout
	args := []string{"-layout"}
	if pdfMaxPages > 0 {
		args = append(args, "-l", fmt.Sprintf("%d", pdfMaxPages))
	}
	args = append(args, filePath, "-")

	cmd := exec.CommandContext(ctx, "pdftotext", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.String(), nil
}

// extractPDFSections splits extracted PDF text into sections.
// Uses form-feed characters as page boundaries, then heading detection within pages.
func extractPDFSections(text string) []section {
	// Split by form-feed (page boundaries from pdftotext).
	pages := strings.Split(text, "\f")
	if len(pages) == 0 {
		return nil
	}

	var sections []section
	lineNum := 1

	for pageIdx, page := range pages {
		_ = pageIdx // used for "Page N" fallback titles
		rawLineCount := strings.Count(page, "\n") + 1
		page = strings.TrimSpace(page)
		if page == "" {
			lineNum += rawLineCount
			continue
		}

		lines := strings.Split(page, "\n")
		var currentTitle string
		var currentBody strings.Builder
		currentDepth := 2 // default: page-level sections
		sectionStartLine := lineNum

		for _, line := range lines {
			trimmed := strings.TrimSpace(line)

			// Heading detection: ALL-CAPS lines of 4+ chars → depth 1
			if len(trimmed) >= 4 && isAllCaps(trimmed) && !isNumber(trimmed) {
				// Save previous section.
				if currentTitle != "" {
					body := currentBody.String()
					sections = append(sections, makeSection(currentTitle, currentDepth, sectionStartLine, body))
				}
				currentTitle = trimmed
				currentDepth = 1
				currentBody.Reset()
				sectionStartLine = lineNum
				lineNum++
				continue
			}

			// Numbered headings: "1.2.3 Title" pattern → depth based on number depth
			if d, title := parseNumberedHeading(trimmed); d > 0 {
				if currentTitle != "" {
					body := currentBody.String()
					sections = append(sections, makeSection(currentTitle, currentDepth, sectionStartLine, body))
				}
				currentTitle = title
				currentDepth = d
				currentBody.Reset()
				sectionStartLine = lineNum
				lineNum++
				continue
			}

			// Regular body text.
			if currentTitle == "" {
				// First page content before any heading → use page as section.
				currentTitle = fmt.Sprintf("Page %d", pageIdx+1)
				sectionStartLine = lineNum
			}
			currentBody.WriteString(trimmed)
			currentBody.WriteByte('\n')
			lineNum++
		}

		// Flush last section of this page.
		if currentTitle != "" {
			body := currentBody.String()
			sections = append(sections, makeSection(currentTitle, currentDepth, sectionStartLine, body))
		}
	}

	return sections
}

func makeSection(title string, depth, line int, body string) section {
	body = strings.TrimSpace(body)
	preview := body
	if len(preview) > 200 {
		preview = preview[:200]
	}
	if len(body) > 2000 {
		body = body[:2000]
	}
	return section{
		Title:       title,
		Depth:       depth,
		Line:        line,
		Body:        body,
		BodyPreview: preview,
	}
}

// isAllCaps returns true if s contains only uppercase letters, digits, spaces, and punctuation.
func isAllCaps(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if !unicode.IsUpper(r) {
				return false
			}
			hasLetter = true
		}
	}
	return hasLetter
}

// isNumber returns true if s is all digits/dots/spaces (e.g., a page number).
func isNumber(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) && r != '.' && r != ' ' {
			return false
		}
	}
	return true
}

// parseNumberedHeading detects patterns like "1.2.3 Title" and returns depth + title.
func parseNumberedHeading(s string) (int, string) {
	if len(s) < 3 {
		return 0, ""
	}
	// Must start with a digit.
	if s[0] < '0' || s[0] > '9' {
		return 0, ""
	}
	// Find end of numbering.
	i := 0
	dots := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		if s[i] == '.' {
			dots++
		}
		i++
	}
	if i >= len(s) || s[i] != ' ' {
		return 0, ""
	}
	title := strings.TrimSpace(s[i+1:])
	if len(title) < 2 {
		return 0, ""
	}
	// Depth = number of dots + 1 (e.g., "1.2.3" has 2 dots → depth 3).
	depth := dots + 1
	if depth > 6 {
		depth = 6
	}
	return depth, fmt.Sprintf("%s %s", s[:i], title)
}

// IsPDFToolAvailable checks if pdftotext is installed.
func IsPDFToolAvailable() bool {
	_, err := exec.LookPath("pdftotext")
	return err == nil
}
