package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	tssitter "github.com/smacker/go-tree-sitter/typescript/typescript"
	tsxsitter "github.com/smacker/go-tree-sitter/typescript/tsx"

	"github.com/synapses/synapses/internal/graph"
)

// extractTSDeclInfo walks the TypeScript/TSX AST and builds a name→declMeta map
// for all function, class, interface, and type alias declarations.
func extractTSDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		if n == nil || depth > 5 {
			return
		}
		switch n.Type() {
		case "function_declaration", "function_expression":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractLineDoc(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "method_definition":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractLineDoc(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "interface_declaration", "type_alias_declaration":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "variable_declarator":
			// export const foo = () => {}
			nameNode := n.ChildByFieldName("name")
			valueNode := n.ChildByFieldName("value")
			if nameNode != nil && valueNode != nil {
				vt := valueNode.Type()
				if vt == "arrow_function" || vt == "function_expression" {
					name := string(src[nameNode.StartByte():nameNode.EndByte()])
					sl := int(n.StartPoint().Row) + 1
					result[name] = declMeta{
						Doc:       extractLineDoc(lines, sl, "//"),
						LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
					}
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

// TypeScriptParser parses TypeScript (.ts) and TSX (.tsx) source files.
type TypeScriptParser struct {
	tsLang  *sitter.Language
	tsxLang *sitter.Language
}

// NewTypeScriptParser creates a ready-to-use TypeScriptParser.
func NewTypeScriptParser() *TypeScriptParser {
	return &TypeScriptParser{
		tsLang:  tssitter.GetLanguage(),
		tsxLang: tsxsitter.GetLanguage(),
	}
}

// Extensions returns the file extensions handled by this parser.
func (p *TypeScriptParser) Extensions() []string {
	return []string{".ts", ".tsx"}
}

// Parse extracts code entities from a single TypeScript/TSX file and merges
// them into the provided graph. The following constructs are captured:
//
//   - Import declarations     → IMPORTS edges
//   - Function declarations   → NodeFunction
//   - Arrow functions (named) → NodeFunction
//   - Class declarations      → NodeStruct
//   - Interface declarations  → NodeInterface
//   - Type alias declarations → NodeInterface (treated as nominal types)
//   - Call expressions        → CALLS edges
func (p *TypeScriptParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	lang := p.langForFile(filePath)

	tsParser := sitter.NewParser()
	tsParser.SetLanguage(lang)

	tree := tsParser.Parse(nil, src)
	root := tree.RootNode()

	// File node.
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	return p.extractDeclarations(g, lang, root, src, filePath, fileNodeID)
}

// langForFile returns the appropriate tree-sitter language for the extension.
func (p *TypeScriptParser) langForFile(filePath string) *sitter.Language {
	if strings.ToLower(filepath.Ext(filePath)) == ".tsx" {
		return p.tsxLang
	}
	return p.tsLang
}

// extractDeclarations walks top-level TypeScript/TSX declarations.
func (p *TypeScriptParser) extractDeclarations(
	g *graph.Graph,
	lang *sitter.Language,
	root *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) error {
	declInfo := extractTSDeclInfo(root, src)

	// --- Import declarations ---
	// import Foo from 'bar'       → string source
	// import { X } from 'bar'    → string source
	// import * as X from 'bar'   → string source
	importQuery := `(import_statement source: (string (string_fragment) @import_path))`
	if err := runQuery(lang, root, src, importQuery, func(captures map[string]string, _ int) {
		importPath := captures["import_path"]
		if importPath == "" {
			return
		}
		// Use the full import path as Name so FindByPattern can match substrings.
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

	// --- Function declarations ---
	funcQuery := `(function_declaration name: (identifier) @func_name)`
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

	// --- Exported arrow functions: export const foo = () => {} ---
	arrowQuery := `
(export_statement
  declaration: (lexical_declaration
    (variable_declarator
      name: (identifier) @func_name
      value: [(arrow_function) (function_expression)]
    )
  )
)`
	if err := runQuery(lang, root, src, arrowQuery, func(captures map[string]string, startLine int) {
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
			Exported: true,
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Class declarations ---
	classQuery := `(class_declaration name: (type_identifier) @class_name)`
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
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Interface declarations ---
	ifaceQuery := `(interface_declaration name: (type_identifier) @iface_name)`
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
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Type alias declarations (treated as nominal types) ---
	typeQuery := `(type_alias_declaration name: (type_identifier) @type_name)`
	if err := runQuery(lang, root, src, typeQuery, func(captures map[string]string, startLine int) {
		name := captures["type_name"]
		if name == "" {
			return
		}
		meta := buildLangMeta(declInfo[name])
		if meta == nil {
			meta = make(map[string]string, 1)
		}
		meta["kind"] = "type_alias"
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeInterface,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Call expressions: obj.method() pattern ---
	callQuery := `
(call_expression
  function: (member_expression
    object: (identifier) @callee_obj
    property: (property_identifier) @callee_prop
  )
)`
	if err := runQuery(lang, root, src, callQuery, func(captures map[string]string, _ int) {
		obj := captures["callee_obj"]
		prop := captures["callee_prop"]
		if obj == "" || prop == "" {
			return
		}
		qualifiedName := obj + "." + prop
		calleeID := g.MakeNodeID(filePath, qualifiedName)
		if g.GetNode(calleeID) == nil {
			return
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: calleeID, Type: graph.EdgeCalls})
	}); err != nil {
		return err
	}

	return nil
}
