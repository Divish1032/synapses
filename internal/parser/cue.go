package parser

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/cue"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// CUEParser parses CUE (.cue) configuration files.
// It extracts package declarations, imports, definitions (#Name), top-level
// fields, and let bindings into graph nodes with appropriate types and metadata.
type CUEParser struct {
	language *sitter.Language
}

// NewCUEParser creates a ready-to-use CUEParser.
func NewCUEParser() *CUEParser {
	return &CUEParser{language: sitter.NewLanguage(cue.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *CUEParser) Extensions() []string {
	return []string{".cue"}
}

// Parse extracts code entities from a single CUE file.
func (p *CUEParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
	if tree == nil {
		return nil
	}
	root := tree.RootNode()
	if root.IsNull() {
		return nil
	}

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lines := strings.Split(string(src), "\n")

	// Walk root children.
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "package_clause":
			p.handlePackageClause(g, child, src, fileNodeID)

		case "import_declaration":
			p.handleImportDecl(g, child, src, filePath, fileNodeID)

		case "field":
			p.handleTopLevelField(g, child, src, filePath, fileNodeID, lines)

		case "let_clause":
			p.handleLetClause(g, child, src, filePath, fileNodeID, lines)
		}
	}

	return nil
}

// handlePackageClause extracts the package name and stores it as metadata on
// the file node. CUE: `package mypackage`
func (p *CUEParser) handlePackageClause(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	fileNodeID graph.NodeID,
) {
	// package_clause has an identifier child containing the package name.
	ident := firstChildOfType(node, "identifier")
	if ident.IsNull() {
		// Fallback: try the second child (first is "package" keyword).
		if node.ChildCount() >= 2 {
			ident = node.Child(1)
		}
	}
	if ident.IsNull() {
		return
	}
	pkgName := childText(ident, src)
	if pkgName == "" || pkgName == "package" {
		return
	}

	fileNode := g.GetNode(fileNodeID)
	if fileNode != nil {
		if fileNode.Metadata == nil {
			fileNode.Metadata = make(map[string]string)
		}
		fileNode.Metadata["package"] = pkgName
	}
}

// handleImportDecl processes an import_declaration node. It handles both single
// imports (`import "path"`) and grouped imports (`import ( "a" \n "b" )`).
func (p *CUEParser) handleImportDecl(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// Walk children looking for import_spec nodes.
	var walkImports func(n sitter.Node)
	walkImports = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if child.IsNull() {
				continue
			}
			switch child.Type() {
			case "import_spec":
				p.handleImportSpec(g, child, src, filePath, fileNodeID)
			case "import_spec_list":
				walkImports(child)
			default:
				// For single imports, the string might be a direct child.
				if child.Type() == "string" || child.Type() == "simple_string_lit" {
					importPath := cueStripQuotes(childText(child, src))
					if importPath != "" {
						p.addImportNode(g, importPath, filePath, fileNodeID)
					}
				}
			}
		}
	}
	walkImports(node)
}

// handleImportSpec processes a single import_spec node.
func (p *CUEParser) handleImportSpec(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// import_spec may contain: optional alias identifier + string path.
	// Walk children to find the string literal.
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		text := childText(child, src)
		if len(text) >= 2 && text[0] == '"' {
			importPath := cueStripQuotes(text)
			if importPath != "" {
				p.addImportNode(g, importPath, filePath, fileNodeID)
			}
			return
		}
	}
}

// addImportNode creates an import package node and IMPORTS edge.
func (p *CUEParser) addImportNode(
	g *graph.Graph,
	importPath string,
	filePath string,
	fileNodeID graph.NodeID,
) {
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

// handleTopLevelField processes a top-level field node. If the label starts with
// `#` it is a CUE definition (NodeStruct), otherwise it is a regular field (NodeVariable).
func (p *CUEParser) handleTopLevelField(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	name := cueExtractFieldName(node, src)
	if name == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "//")

	meta := make(map[string]string)
	if doc != "" {
		meta["doc"] = doc
	}

	lineCount := int(node.EndPoint().Row) - int(node.StartPoint().Row) + 1
	if lineCount > 0 {
		meta["line_count"] = strconv.Itoa(lineCount)
	}

	// Determine node type based on whether the name starts with # or _#.
	isDefinition := strings.HasPrefix(name, "#") || strings.HasPrefix(name, "_#")
	if isDefinition {
		meta["kind"] = "definition"
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	} else {
		meta["kind"] = "field"
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// handleLetClause processes a let_clause: `let x = expr`.
func (p *CUEParser) handleLetClause(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	// let_clause children: "let" keyword, identifier, "=", expression.
	ident := firstChildOfType(node, "identifier")
	if ident.IsNull() {
		return
	}
	name := childText(ident, src)
	if name == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "//")

	meta := map[string]string{"kind": "let"}
	if doc != "" {
		meta["doc"] = doc
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeVariable,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// cueExtractFieldName extracts the field name from a field node.
// CUE fields have a label child that may be an identifier, a string, or a
// definition identifier (starting with #).
func cueExtractFieldName(fieldNode sitter.Node, src []byte) string {
	// The first child of a field is the label.
	label := firstChildOfType(fieldNode, "label")
	if label.IsNull() {
		// Fallback: check direct children.
		for i := uint32(0); i < fieldNode.ChildCount(); i++ {
			child := fieldNode.Child(i)
			if child.IsNull() {
				continue
			}
			ct := child.Type()
			if ct == "identifier" || ct == "string" || ct == "simple_string_lit" {
				text := childText(child, src)
				return cueStripQuotes(text)
			}
		}
		return ""
	}

	// Walk label children to find the actual name.
	for i := uint32(0); i < label.ChildCount(); i++ {
		child := label.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		switch ct {
		case "identifier":
			text := childText(child, src)
			// Check if the label text in source starts with # or _#
			// by looking at the full label text.
			labelText := childText(label, src)
			if strings.HasPrefix(labelText, "#") || strings.HasPrefix(labelText, "_#") {
				return cueCleanLabel(labelText)
			}
			return text
		case "simple_string_lit", "string":
			return cueStripQuotes(childText(child, src))
		}
	}

	// Fallback: use the full label text.
	labelText := childText(label, src)
	if labelText != "" {
		return cueCleanLabel(labelText)
	}
	return ""
}

// cueCleanLabel extracts the identifier part from a CUE label, stripping
// any trailing colon or whitespace. E.g. "#Database:" → "#Database".
func cueCleanLabel(s string) string {
	s = strings.TrimSuffix(s, ":")
	s = strings.TrimSpace(s)
	// If it contains an alias (F=f), take the first part before =.
	if idx := strings.Index(s, "="); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	return s
}

// cueStripQuotes removes surrounding double quotes from a CUE string literal.
func cueStripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
