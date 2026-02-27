package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/rust"

	"github.com/Divish1032/synapses/internal/graph"
)

// extractRustDeclInfo walks the Rust AST collecting metadata for function,
// struct, enum, and trait declarations. Rust doc comments use "///".
func extractRustDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		if n == nil || depth > 6 {
			return
		}
		switch n.Type() {
		case "function_item":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "struct_item", "enum_item", "trait_item":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
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

// RustParser parses Rust (.rs) source files.
type RustParser struct {
	language *sitter.Language
}

// NewRustParser creates a ready-to-use RustParser.
func NewRustParser() *RustParser {
	return &RustParser{language: rust.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *RustParser) Extensions() []string {
	return []string{".rs"}
}

// Parse extracts code entities from a single Rust file and merges them into the graph.
func (p *RustParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractRustDeclInfo(root, src)

	// --- use declarations ---
	useQuery := `(use_declaration argument: (scoped_identifier) @use_path)`
	if err := runQuery(lang, root, src, useQuery, func(captures map[string]string, _ int) {
		usePath := captures["use_path"]
		if usePath == "" {
			return
		}
		importNodeID := g.MakeNodeID(usePath, usePath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    usePath,
			Package: usePath,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- function items ---
	funcQuery := `(function_item name: (identifier) @func_name)`
	if err := runQuery(lang, root, src, funcQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- struct items ---
	structQuery := `(struct_item name: (type_identifier) @struct_name)`
	if err := runQuery(lang, root, src, structQuery, func(captures map[string]string, startLine int) {
		name := captures["struct_name"]
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
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- enum items ---
	enumQuery := `(enum_item name: (type_identifier) @enum_name)`
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
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- trait items ---
	traitQuery := `(trait_item name: (type_identifier) @trait_name)`
	if err := runQuery(lang, root, src, traitQuery, func(captures map[string]string, startLine int) {
		name := captures["trait_name"]
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
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- impl blocks: record the type being implemented ---
	implQuery := `(impl_item type: (type_identifier) @impl_type)`
	if err := runQuery(lang, root, src, implQuery, func(captures map[string]string, startLine int) {
		name := captures["impl_type"]
		if name == "" {
			return
		}
		// Only register if not already in graph; impl blocks reference a type.
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}
