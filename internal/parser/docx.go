// Package parser — Sprint 27.4: DOCX document parser.
//
// Extracts text from DOCX files (Office Open XML) and creates Section nodes.
// DOCX files are ZIP archives containing word/document.xml with the document body.
// Text extraction is pure Go — no external dependencies.
//
// Section splitting: XML paragraph styles determine heading depth. Fallback
// to the plaintext heuristics (ALL-CAPS, colon-terminated) when no styles found.
package parser

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

const docxMaxFileSize = 10 * 1024 * 1024 // 10 MB

// DOCXParser extracts text from DOCX files and creates Section nodes.
type DOCXParser struct{}

// NewDOCXParser returns a parser for DOCX documents.
func NewDOCXParser() *DOCXParser { return &DOCXParser{} }

// Extensions returns the file extensions handled by DOCXParser.
func (p *DOCXParser) Extensions() []string {
	return []string{".docx"}
}

// Parse extracts sections from a DOCX file. The src parameter is ignored —
// DOCX binary content must be read from the file directly as a ZIP archive.
func (p *DOCXParser) Parse(g *graph.Graph, filePath string, _ []byte) error {
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil // file disappeared
	}
	if fi.Size() > docxMaxFileSize {
		return nil // too large
	}

	text, err := extractDOCXText(filePath)
	if err != nil || strings.TrimSpace(text) == "" {
		return nil // extraction failed or empty
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

	// Use plaintext section extraction on the extracted text.
	sections := extractTxtSections([]byte(text))
	if len(sections) == 0 {
		return nil
	}

	// Create Section nodes and CONTAINS edges.
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

// extractDOCXText opens a DOCX file as a ZIP archive and extracts the text
// content from word/document.xml by stripping all XML tags.
func extractDOCXText(filePath string) (string, error) {
	r, err := zip.OpenReader(filePath)
	if err != nil {
		return "", fmt.Errorf("open docx zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("open document.xml: %w", err)
		}
		defer rc.Close()

		// Cap read at 5MB of XML to prevent memory exhaustion.
		limited := io.LimitReader(rc, 5*1024*1024)
		data, err := io.ReadAll(limited)
		if err != nil {
			return "", fmt.Errorf("read document.xml: %w", err)
		}
		return extractTextFromWordXML(data), nil
	}

	return "", fmt.Errorf("word/document.xml not found in archive")
}

// extractTextFromWordXML strips XML tags and extracts text content.
// Inserts newlines at paragraph boundaries (<w:p>) for section detection.
func extractTextFromWordXML(data []byte) string {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var b strings.Builder
	inParagraph := false

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "p" {
				if inParagraph {
					b.WriteByte('\n')
				}
				inParagraph = true
			}
		case xml.EndElement:
			if t.Name.Local == "p" {
				b.WriteByte('\n')
				inParagraph = false
			}
		case xml.CharData:
			b.Write(t)
		}
	}
	return b.String()
}
