package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	tsxsitter "github.com/smacker/go-tree-sitter/typescript/tsx"
	tssitter "github.com/smacker/go-tree-sitter/typescript/typescript"

	"github.com/SynapsesOS/synapses/internal/graph"
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
//   - Method definitions      → NodeMethod (inside classes)
//   - Class declarations      → NodeStruct
//   - Interface declarations  → NodeInterface
//   - Type alias declarations → NodeInterface (treated as nominal types)
//   - Call expressions        → call sites (resolved to CALLS edges by resolver)
func (p *TypeScriptParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	lang := p.langForFile(filePath)

	tsParser := sitter.NewParser()
	tsParser.SetLanguage(lang)

	tree, _ := tsParser.ParseCtx(context.Background(), nil, src)
	root := tree.RootNode()

	// Module name = basename without extension (e.g. "pipeline" for pipeline.ts).
	moduleName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

	// File node.
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:      fileNodeID,
		Type:    graph.NodeFile,
		Name:    filepath.Base(filePath),
		File:    filePath,
		Line:    1,
		Package: moduleName,
	})

	return p.extractDeclarations(g, lang, root, src, filePath, fileNodeID, moduleName)
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
	moduleName string,
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
			Package:  moduleName,
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
			Package:  moduleName,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Method definitions (inside class bodies) ---
	methodQuery := `(method_definition name: (property_identifier) @method_name)`
	if err := runQuery(lang, root, src, methodQuery, func(captures map[string]string, startLine int) {
		name := captures["method_name"]
		if name == "" || name == "constructor" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeMethod,
			Name:     name,
			Package:  moduleName,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
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
			Package:  moduleName,
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
			Package:  moduleName,
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
			Package:  moduleName,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Call sites: direct calls foo() and method calls obj.method() ---
	// Collected now and resolved into CALLS edges after all files are parsed.
	collectTSCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// collectTSCallSites walks the AST and adds call sites for function and method
// calls so the cross-file resolver can link them as CALLS edges.
func collectTSCallSites(g *graph.Graph, lang *sitter.Language, root *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Direct calls: foo(...)
	directQuery := `(call_expression function: (identifier) @callee)`
	_ = runQuery(lang, root, src, directQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || isTSBuiltin(callee) {
			return
		}
		g.AddCallSite(graph.CallSite{
			CallerID:   fileNodeID,
			CallerFile: filePath,
			FuncName:   callee,
			PkgAlias:   "",
		})
	})

	// Method calls: obj.method(...) — collect the method name for cross-module resolution.
	memberQuery := `(call_expression function: (member_expression property: (property_identifier) @method_name))`
	_ = runQuery(lang, root, src, memberQuery, func(captures map[string]string, _ int) {
		name := captures["method_name"]
		if name == "" || isTSBuiltin(name) {
			return
		}
		g.AddCallSite(graph.CallSite{
			CallerID:   fileNodeID,
			CallerFile: filePath,
			FuncName:   name,
			PkgAlias:   "",
		})
	})
}

// isTSBuiltin returns true for TypeScript/JavaScript built-in methods and
// constructors that should never generate CALLS edges.
func isTSBuiltin(name string) bool {
	switch name {
	case "push", "pop", "shift", "unshift", "splice", "slice", "map", "filter",
		"reduce", "forEach", "find", "findIndex", "some", "every", "includes",
		"indexOf", "join", "reverse", "sort", "concat", "flat", "flatMap",
		"keys", "values", "entries", "assign", "create", "freeze",
		"stringify", "parse", "toString", "valueOf", "hasOwnProperty",
		"addEventListener", "removeEventListener", "dispatchEvent",
		"setTimeout", "setInterval", "clearTimeout", "clearInterval",
		"console", "log", "warn", "error", "info", "debug",
		"Promise", "resolve", "reject", "then", "catch", "finally",
		"JSON", "Math", "Date", "Object", "Array", "String", "Number", "Boolean",
		"Symbol", "RegExp", "Error", "TypeError", "RangeError",
		"parseInt", "parseFloat", "isNaN", "isFinite",
		"now", "from", "of", "call", "apply", "bind":
		return true
	}
	return false
}
