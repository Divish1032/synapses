package parser

import (
	"context"
	"path/filepath"
	"strings"
	"unicode"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"

	"github.com/Divish1032/synapses/internal/graph"
)

// extractJavaDeclInfo walks the Java AST collecting metadata for method,
// class, interface, and enum declarations. Java line comments use "//".
func extractJavaDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		if n == nil || depth > 6 {
			return
		}
		switch n.Type() {
		case "method_declaration":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractLineDoc(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration", "interface_declaration", "enum_declaration":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), depth+1)
		}
	}
	walk(root, 0)
	return result
}

// JavaParser parses Java (.java) source files.
type JavaParser struct {
	language *sitter.Language
}

// NewJavaParser creates a ready-to-use JavaParser.
func NewJavaParser() *JavaParser {
	return &JavaParser{language: java.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *JavaParser) Extensions() []string {
	return []string{".java"}
}

// Parse extracts code entities from a single Java file and merges them into the graph.
// Captured constructs: import declarations, class/interface/enum declarations, method declarations.
func (p *JavaParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	lang := p.language
	declInfo := extractJavaDeclInfo(root, src)

	// --- import declarations ---
	importQuery := `(import_declaration (scoped_identifier) @import_path)`
	if err := runQuery(lang, root, src, importQuery, func(captures map[string]string, _ int) {
		importPath := captures["import_path"]
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
	}); err != nil {
		return err
	}

	// --- class declarations ---
	classQuery := `(class_declaration name: (identifier) @class_name)`
	if err := runQuery(lang, root, src, classQuery, func(captures map[string]string, startLine int) {
		name := captures["class_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isJavaPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- interface declarations ---
	ifaceQuery := `(interface_declaration name: (identifier) @iface_name)`
	if err := runQuery(lang, root, src, ifaceQuery, func(captures map[string]string, startLine int) {
		name := captures["iface_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeInterface,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isJavaPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- enum declarations ---
	enumQuery := `(enum_declaration name: (identifier) @enum_name)`
	if err := runQuery(lang, root, src, enumQuery, func(captures map[string]string, startLine int) {
		name := captures["enum_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isJavaPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- method declarations ---
	methodQuery := `(method_declaration name: (identifier) @method_name)`
	if err := runQuery(lang, root, src, methodQuery, func(captures map[string]string, startLine int) {
		name := captures["method_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeMethod,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isJavaPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}

// isJavaPublic returns true if the name starts with an uppercase letter.
// Java's actual visibility is determined by the `public` keyword in the AST, but
// querying modifiers requires a more complex Tree-sitter pattern. As a pragmatic
// approximation: public-API identifiers in Java follow PascalCase (types) or
// camelCase-starting-with-upper (rare), while private/package-private helpers
// use camelCase starting with a lowercase letter.
func isJavaPublic(name string) bool {
	if name == "" {
		return false
	}
	return unicode.IsUpper(rune(name[0]))
}
