package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/css"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// CSSParser parses CSS (.css) source files.
type CSSParser struct {
	language *sitter.Language
}

// NewCSSParser creates a ready-to-use CSSParser.
func NewCSSParser() *CSSParser {
	return &CSSParser{language: css.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *CSSParser) Extensions() []string {
	return []string{".css"}
}

// Parse extracts code entities from a single CSS file and merges them into the graph.
//
// Extracts:
//   - @import rules → EdgeImports edges to NodePackage nodes
//   - @keyframes definitions → NodeFunction with kind="keyframes"
//   - CSS custom property definitions (--var) in :root → NodeVariable with kind="custom-property"
func (p *CSSParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseCtx(context.Background(), nil, src)
	root := tree.RootNode()

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	// Walk the AST once, extracting all entity types.
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}

		switch n.Type() {
		case "import_statement":
			extractCSSImport(g, n, src, filePath, fileNodeID)

		case "keyframes_statement":
			extractCSSKeyframes(g, n, src, filePath, fileNodeID)

		case "declaration":
			// Check if this is a custom property definition (--variable).
			extractCSSCustomProperty(g, n, src, filePath, fileNodeID)

		case "at_rule":
			extractCSSAtRule(g, n, src, filePath, fileNodeID)
		}

		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)

	return nil
}

// extractCSSImport handles an import_statement node, creating an import edge.
// Supports both @import "file.css" and @import url("file.css").
func extractCSSImport(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var importPath string

	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}

		switch child.Type() {
		case "string_value":
			// @import "file.css"
			importPath = stripCSSQuotes(childText(child, src))

		case "call_expression":
			// @import url("file.css")
			// The call_expression has a function_name "url" and arguments with a string.
			importPath = extractCSSURLArg(child, src)
		}
	}

	if importPath == "" {
		return
	}

	importNodeID := g.MakeNodeID(importPath, importPath)
	g.AddNode(&graph.Node{
		ID:      importNodeID,
		Type:    graph.NodePackage,
		Name:    importPath,
		Package: importPath,
		File:    filePath,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
}

// extractCSSKeyframes handles a keyframes_statement node, creating a NodeFunction.
func extractCSSKeyframes(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var name string

	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		// The keyframes name can appear as "keyframes_name" or "plain_value" or "identifier"
		// depending on the grammar version.
		switch child.Type() {
		case "keyframes_name", "plain_value", "identifier":
			name = strings.TrimSpace(childText(child, src))
		}
	}

	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: map[string]string{"kind": "keyframes"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractCSSCustomProperty checks if a declaration node defines a CSS custom property
// (property name starting with "--") and creates a NodeVariable for it.
func extractCSSCustomProperty(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var propName string

	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "property_name" {
			propName = strings.TrimSpace(childText(child, src))
			break
		}
	}

	if !strings.HasPrefix(propName, "--") {
		return
	}

	nodeID := g.MakeNodeID(filePath, propName)
	if g.GetNode(nodeID) != nil {
		return
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeVariable,
		Name:     propName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: map[string]string{"kind": "custom-property"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractCSSAtRule handles @-rules that contain named entities.
// Currently handles @font-face (extracts font-family name as NodeVariable).
func extractCSSAtRule(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Check if this is a @font-face rule by looking for the at-keyword.
	var atKeyword string
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "at_keyword" {
			atKeyword = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(childText(child, src)), "@"))
			break
		}
	}

	if atKeyword != "font-face" {
		return
	}

	// Find the block/rule-set child and look for a font-family declaration.
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "block" || child.Type() == "stylesheet" {
			// Walk declarations inside the block.
			for j := 0; j < int(child.ChildCount()); j++ {
				decl := child.Child(j)
				if decl == nil || decl.Type() != "declaration" {
					continue
				}
				// Check if property is font-family.
				propName := ""
				var propValue string
				for k := 0; k < int(decl.ChildCount()); k++ {
					dChild := decl.Child(k)
					if dChild == nil {
						continue
					}
					switch dChild.Type() {
					case "property_name":
						propName = strings.ToLower(strings.TrimSpace(childText(dChild, src)))
					case "string_value", "plain_value":
						if propValue == "" {
							propValue = stripCSSQuotes(childText(dChild, src))
						}
					}
				}
				if propName == "font-family" && propValue != "" {
					nodeID := g.MakeNodeID(filePath, propValue)
					if g.GetNode(nodeID) != nil {
						return
					}
					g.AddNode(&graph.Node{
						ID:       nodeID,
						Type:     graph.NodeVariable,
						Name:     propValue,
						File:     filePath,
						Line:     int(n.StartPoint().Row) + 1,
						Exported: true,
						Metadata: map[string]string{"kind": "font-face"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					return
				}
			}
		}
	}
}

// stripCSSQuotes removes surrounding single or double quotes from a string.
func stripCSSQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// extractCSSURLArg extracts the path from a url() call expression.
// Handles url("file.css"), url('file.css'), and url(file.css).
func extractCSSURLArg(callNode *sitter.Node, src []byte) string {
	// Walk the call_expression children looking for arguments/string_value.
	if callNode == nil {
		return ""
	}
	// The arguments node contains the actual URL value.
	if args := firstChildOfType(callNode, "arguments"); args != nil {
		for i := 0; i < int(args.ChildCount()); i++ {
			child := args.Child(i)
			if child == nil {
				continue
			}
			if child.Type() == "string_value" {
				return stripCSSQuotes(childText(child, src))
			}
			// Unquoted URL: plain_value inside arguments.
			if child.Type() == "plain_value" {
				return strings.TrimSpace(childText(child, src))
			}
		}
	}

	// Fallback: look for any string_value or plain_value direct child.
	for i := 0; i < int(callNode.ChildCount()); i++ {
		child := callNode.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "string_value" {
			return stripCSSQuotes(childText(child, src))
		}
		if child.Type() == "plain_value" {
			return strings.TrimSpace(childText(child, src))
		}
	}

	// Last resort: extract text between parentheses from the raw source.
	raw := childText(callNode, src)
	if idx := strings.Index(raw, "("); idx >= 0 {
		if end := strings.LastIndex(raw, ")"); end > idx {
			inner := strings.TrimSpace(raw[idx+1 : end])
			return stripCSSQuotes(inner)
		}
	}
	return ""
}
