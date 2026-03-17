package parser

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	nixg "github.com/alexaandru/go-sitter-forest/nix"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// NixParser parses Nix (.nix) expression files.
// It extracts let bindings, attribute set keys, function parameters, and
// import expressions into graph nodes. Nix is a pure functional language
// for the Nix package manager — its main entities are bindings and derivations
// rather than traditional functions/classes.
type NixParser struct {
	language *sitter.Language
}

// NewNixParser creates a ready-to-use NixParser.
func NewNixParser() *NixParser {
	return &NixParser{language: sitter.NewLanguage(nixg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *NixParser) Extensions() []string {
	return []string{".nix"}
}

// Parse extracts code entities from a single Nix file.
func (p *NixParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// Walk the root's children to extract top-level entities.
	// A Nix file typically has a single top-level expression which may be:
	// - a function_expression (e.g. { pkgs, ... }: body)
	// - a let_expression (let ... in ...)
	// - an attrset_expression ({ key = val; ... })
	// - an apply_expression (function application)
	// - a with_expression (with pkgs; ...)
	// We walk recursively into the top-level structure to find all bindings.
	p.walkNode(g, root, src, filePath, fileNodeID, lines, true)

	return nil
}

// walkNode recursively walks the AST extracting Nix entities.
// topLevel indicates whether we are at a structural level where bindings
// should be extracted (root, or inside top-level let/attrset bodies).
func (p *NixParser) walkNode(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	topLevel bool,
) {
	if node.IsNull() {
		return
	}

	switch node.Type() {
	case "function_expression":
		p.handleFunctionExpression(g, node, src, filePath, fileNodeID, lines)

	case "let_expression":
		p.handleLetExpression(g, node, src, filePath, fileNodeID, lines)

	case "attrset_expression":
		if topLevel {
			p.handleAttrsetExpression(g, node, src, filePath, fileNodeID, lines)
		}

	case "with_expression":
		// with_expression: "with" expr ";" body — recurse into the body.
		p.walkChildren(g, node, src, filePath, fileNodeID, lines, topLevel)

	case "apply_expression":
		// Check for import expressions first.
		p.handleApplyExpression(g, node, src, filePath, fileNodeID)
		// Also recurse into children for nested structures.
		p.walkChildren(g, node, src, filePath, fileNodeID, lines, topLevel)

	default:
		// For other node types at the root level, recurse into children
		// to find nested function/let/attrset expressions.
		if topLevel {
			p.walkChildren(g, node, src, filePath, fileNodeID, lines, topLevel)
		}
	}
}

// walkChildren recurses into all children of a node.
func (p *NixParser) walkChildren(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	topLevel bool,
) {
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		p.walkNode(g, child, src, filePath, fileNodeID, lines, topLevel)
	}
}

// handleFunctionExpression extracts parameters from a function_expression.
// Nix functions: `{ pkgs ? import <nixpkgs> {}, lib, ... }: body`
// The formals node contains formal parameters. We store them as metadata
// on the file node and recurse into the function body.
func (p *NixParser) handleFunctionExpression(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	// Extract formal parameters.
	var params []string
	formals := firstChildOfType(node, "formals")
	if !formals.IsNull() {
		for i := uint32(0); i < formals.ChildCount(); i++ {
			child := formals.Child(i)
			if child.IsNull() {
				continue
			}
			if child.Type() == "formal" {
				ident := firstChildOfType(child, "identifier")
				if !ident.IsNull() {
					name := childText(ident, src)
					if name != "" {
						params = append(params, name)
					}
				}
			}
		}
	} else {
		// Simple lambda: `x: body` — the identifier is a direct child.
		ident := firstChildOfType(node, "identifier")
		if !ident.IsNull() {
			name := childText(ident, src)
			if name != "" {
				params = append(params, name)
			}
		}
	}

	if len(params) > 0 {
		fileNode := g.GetNode(fileNodeID)
		if fileNode != nil {
			if fileNode.Metadata == nil {
				fileNode.Metadata = make(map[string]string)
			}
			fileNode.Metadata["params"] = strings.Join(params, ", ")
		}
	}

	// Recurse into the function body (all children that are not formals/identifier).
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		if ct == "formals" || ct == "identifier" {
			continue
		}
		p.walkNode(g, child, src, filePath, fileNodeID, lines, true)
	}
}

// handleLetExpression extracts bindings from a let_expression.
// Nix: `let name = value; ... in body`
// Each binding in the binding_set is extracted as a NodeVariable.
func (p *NixParser) handleLetExpression(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	// Find binding_set child and extract bindings from it.
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "binding_set":
			p.extractBindings(g, child, src, filePath, fileNodeID, lines, "let")

		case "binding":
			// Some grammars put bindings directly under let_expression.
			p.extractSingleBinding(g, child, src, filePath, fileNodeID, lines, "let")

		default:
			// Recurse into the body of the let expression (the "in" part).
			p.walkNode(g, child, src, filePath, fileNodeID, lines, true)
		}
	}
}

// handleAttrsetExpression extracts keys from a top-level attribute set.
// Nix: `{ pname = "hello"; version = "1.0"; ... }`
func (p *NixParser) handleAttrsetExpression(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	// An attrset_expression's children include bindings or a binding_set.
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "binding_set":
			p.extractBindings(g, child, src, filePath, fileNodeID, lines, "attr")

		case "binding":
			p.extractSingleBinding(g, child, src, filePath, fileNodeID, lines, "attr")

		case "inherit":
			p.handleInherit(g, child, src, filePath, fileNodeID, lines)

		case "inherit_from":
			p.handleInherit(g, child, src, filePath, fileNodeID, lines)
		}
	}
}

// extractBindings iterates over children of a binding_set, extracting each binding.
func (p *NixParser) extractBindings(
	g *graph.Graph,
	bindingSet sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	kind string,
) {
	for i := uint32(0); i < bindingSet.ChildCount(); i++ {
		child := bindingSet.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "binding":
			p.extractSingleBinding(g, child, src, filePath, fileNodeID, lines, kind)
		case "inherit":
			p.handleInherit(g, child, src, filePath, fileNodeID, lines)
		case "inherit_from":
			p.handleInherit(g, child, src, filePath, fileNodeID, lines)
		}
	}
}

// extractSingleBinding extracts a single binding node as a NodeVariable.
// binding: `attrpath = value;` where attrpath contains one or more identifiers.
func (p *NixParser) extractSingleBinding(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	kind string,
) {
	name := nixExtractBindingName(node, src)
	if name == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	meta := map[string]string{"kind": kind}
	if doc != "" {
		meta["doc"] = doc
	}

	lineCount := int(node.EndPoint().Row) - int(node.StartPoint().Row) + 1
	if lineCount > 0 {
		meta["line_count"] = strconv.Itoa(lineCount)
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

// handleInherit processes `inherit name1 name2;` and `inherit (expr) name1;` nodes.
// Each inherited name is extracted as a NodeVariable.
func (p *NixParser) handleInherit(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	startLine := int(node.StartPoint().Row) + 1

	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		// Extract each identifier in the inherit statement.
		if child.Type() == "identifier" || child.Type() == "attr_identifier" {
			name := childText(child, src)
			if name == "" {
				continue
			}

			meta := map[string]string{"kind": "inherit"}
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
}

// handleApplyExpression checks if an apply_expression is an import call.
// Nix imports: `import ./path.nix`, `import <nixpkgs>`, `import ./path { ... }`
// An import is an apply_expression where the first child is a variable_expression
// with identifier "import", or the first child is itself an apply_expression
// starting with "import" (for chained applications like `import ./path { args }`).
func (p *NixParser) handleApplyExpression(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if node.ChildCount() < 2 {
		return
	}

	first := node.Child(0)
	if first.IsNull() {
		return
	}

	isImport := false
	var pathNode sitter.Node

	switch first.Type() {
	case "variable_expression":
		// Direct import: `import ./path.nix`
		ident := firstChildOfType(first, "identifier")
		if !ident.IsNull() && childText(ident, src) == "import" {
			isImport = true
			pathNode = node.Child(1)
		}

	case "apply_expression":
		// Chained application: `import ./path { args }` parses as
		// apply_expression(apply_expression(import, path), { args })
		// Recurse into the inner apply to handle the import.
		p.handleApplyExpression(g, first, src, filePath, fileNodeID)
		return
	}

	if !isImport || pathNode.IsNull() {
		return
	}

	importPath := nixExtractImportPath(pathNode, src, filePath)
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

// nixExtractBindingName extracts the name from a binding node.
// A binding has an attrpath child containing one or more identifiers separated by dots.
// We extract the full dotted path as the binding name.
func nixExtractBindingName(node sitter.Node, src []byte) string {
	attrpath := firstChildOfType(node, "attrpath")
	if !attrpath.IsNull() {
		var parts []string
		for i := uint32(0); i < attrpath.ChildCount(); i++ {
			child := attrpath.Child(i)
			if child.IsNull() {
				continue
			}
			if child.Type() == "identifier" || child.Type() == "attr_identifier" {
				text := childText(child, src)
				if text != "" {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, ".")
		}
	}

	// Fallback: try a direct identifier child.
	ident := firstChildOfType(node, "identifier")
	if !ident.IsNull() {
		return childText(ident, src)
	}
	return ""
}

// nixExtractImportPath extracts the import path from the argument node of an
// import expression. Handles:
// - path_expression: `./path.nix` or `../lib/default.nix`
// - spath_expression: `<nixpkgs>` (angle-bracket paths)
// - string_expression: `"path/to/file.nix"`
func nixExtractImportPath(node sitter.Node, src []byte, filePath string) string {
	if node.IsNull() {
		return ""
	}

	text := strings.TrimSpace(childText(node, src))

	switch node.Type() {
	case "path_expression", "path_fragment":
		// Resolve relative paths against the file's directory.
		if strings.HasPrefix(text, "./") || strings.HasPrefix(text, "../") {
			dir := filepath.Dir(filePath)
			resolved := filepath.Join(dir, text)
			return filepath.Clean(resolved)
		}
		return text

	case "spath_expression":
		// Angle-bracket paths like <nixpkgs> — return as-is.
		return text

	case "string_expression":
		// Strip quotes.
		if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
			inner := text[1 : len(text)-1]
			if strings.HasPrefix(inner, "./") || strings.HasPrefix(inner, "../") {
				dir := filepath.Dir(filePath)
				return filepath.Clean(filepath.Join(dir, inner))
			}
			return inner
		}
		return text

	default:
		// For other node types (e.g. parenthesized expressions), try the raw text.
		if text != "" && text != "import" {
			return text
		}
		return ""
	}
}
