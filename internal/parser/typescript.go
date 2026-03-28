package parser

import (
	"path/filepath"
	"strings"

	tsxsitter "github.com/alexaandru/go-sitter-forest/tsx"
	tssitter "github.com/alexaandru/go-sitter-forest/typescript"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// extractTSDeclInfo walks the TypeScript/TSX AST and builds a name→declMeta map
// for all function, class, interface, type alias, and enum declarations.
func extractTSDeclInfo(root sitter.Node, src []byte, lines []string) map[string]declMeta {
	result := make(map[string]declMeta)

	var walk func(n sitter.Node, enclosingClass string, depth int)
	walk = func(n sitter.Node, enclosingClass string, depth int) {
		if n.IsNull() || depth > 8 {
			return
		}
		switch n.Type() {
		case "function_declaration", "function_expression", "function_signature":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"statement_block", "block"}),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "method_definition":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"statement_block", "block"}),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration", "abstract_class_declaration":
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[className] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), className, depth+1)
			}
			return
		case "interface_declaration", "type_alias_declaration", "enum_declaration":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "variable_declarator":
			// export const foo = () => {}
			nameNode := n.ChildByFieldName("name")
			valueNode := n.ChildByFieldName("value")
			if !nameNode.IsNull() && !valueNode.IsNull() {
				vt := valueNode.Type()
				if vt == "arrow_function" || vt == "function_expression" {
					name := string(src[nameNode.StartByte():nameNode.EndByte()])
					sl := int(n.StartPoint().Row) + 1
					result[name] = declMeta{
						Doc:       extractDocMulti(lines, sl, "//"),
						LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass, depth+1)
		}
	}
	walk(root, "", 0)
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
		tsLang:  sitter.NewLanguage(tssitter.GetLanguage()),
		tsxLang: sitter.NewLanguage(tsxsitter.GetLanguage()),
	}
}

// Extensions returns the file extensions handled by this parser.
func (p *TypeScriptParser) Extensions() []string {
	return []string{".ts", ".tsx"}
}

// TSLanguageForFile returns the tree-sitter language for this parser.
func (p *TypeScriptParser) TSLanguageForFile(filePath string) *sitter.Language {
	return p.langForFile(filePath)
}

// Parse extracts code entities from a single TypeScript/TSX file and merges
// them into the provided graph.
func (p *TypeScriptParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	lang := p.langForFile(filePath)

	tsParser := sitter.NewParser()
	tsParser.SetLanguage(lang)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := tsParser.ParseString(parseCtx, nil, src)
	if tree != nil {
		defer tree.Close()
	}
	if tree == nil {
		return nil
	}
	root := tree.RootNode()

	// Module name = basename without extension.
	moduleName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

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
	root sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	moduleName string,
) error {
	lines := strings.Split(string(src), "\n")
	declInfo := extractTSDeclInfo(root, src, lines)

	// --- Import declarations ---
	importQuery := `(import_statement source: (string (string_fragment) @import_path))`
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

	// --- Re-export statements: export { X } from './bar', export * from './baz' ---
	// These create IMPORTS edges to the source module, enabling the resolver
	// to follow re-export chains through barrel files (index.ts).
	reExportQuery := `(export_statement source: (string (string_fragment) @import_path))`
	if err := runQuery(lang, root, src, reExportQuery, func(captures map[string]string, _ int) {
		importPath := captures["import_path"]
		if importPath == "" {
			return
		}
		importNodeID := g.MakeNodeID(importPath, importPath)
		if g.GetNode(importNodeID) == nil {
			g.AddNode(&graph.Node{
				ID:      importNodeID,
				Type:    graph.NodePackage,
				Name:    importPath,
				Package: importPath,
				File:    filePath,
			})
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- CommonJS require() calls ---
	// Handles: const x = require('module'), var x = require('./router')
	// Creates IMPORTS edges identical to ES6 import statements.
	requireQuery := `(call_expression
		function: (identifier) @fn_name
		arguments: (arguments (string (string_fragment) @import_path))
	)`
	if err := runQuery(lang, root, src, requireQuery, func(captures map[string]string, startLine int) {
		if captures["fn_name"] != "require" {
			return
		}
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
			Line:    startLine,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- Function declarations (regular and ambient/declare) ---
	// function_declaration: function foo() { ... }
	// function_signature:   declare function foo(): void;  (no body, used in .d.ts)
	addFuncNode := func(name string, startLine int) {
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
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
	}
	funcQuery := `(function_declaration name: (identifier) @func_name)`
	if err := runQuery(lang, root, src, funcQuery, func(captures map[string]string, startLine int) {
		if name := captures["func_name"]; name != "" {
			addFuncNode(name, startLine)
		}
	}); err != nil {
		return err
	}
	// Ambient function signatures: declare function foo(): void;
	funcSigQuery := `(function_signature name: (identifier) @func_name)`
	_ = runQuery(lang, root, src, funcSigQuery, func(captures map[string]string, startLine int) {
		if name := captures["func_name"]; name != "" {
			addFuncNode(name, startLine)
		}
	})

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

	// --- Non-exported arrow/function expressions ---
	nonExportArrowQuery := `
(lexical_declaration
  (variable_declarator
    name: (identifier) @func_name
    value: [(arrow_function) (function_expression)]
  )
)`
	if err := runQuery(lang, root, src, nonExportArrowQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		// Skip if already captured by the export query.
		if g.GetNode(g.MakeNodeID(filePath, name)) != nil {
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
			Exported: false,
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Class declarations (including abstract) ---
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

	// --- Abstract class declarations ---
	abstractClassQuery := `(abstract_class_declaration name: (type_identifier) @class_name)`
	if err := runQuery(lang, root, src, abstractClassQuery, func(captures map[string]string, startLine int) {
		name := captures["class_name"]
		if name == "" {
			return
		}
		// Skip if already added by the regular class query.
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
		meta := buildLangMeta(declInfo[name])
		if meta == nil {
			meta = make(map[string]string, 1)
		}
		meta["kind"] = "abstract"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
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

	// --- Type alias declarations ---
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

	// --- Enum declarations ---
	enumQuery := `(enum_declaration name: (identifier) @enum_name)`
	if err := runQuery(lang, root, src, enumQuery, func(captures map[string]string, startLine int) {
		name := captures["enum_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declInfo[name])
		if meta == nil {
			meta = make(map[string]string, 1)
		}
		meta["kind"] = "enum"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
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

	// --- Enum members (individual values like Color.Red) ---
	// TS grammar: enum_declaration → enum_body → property_identifier children.
	extractTSEnumMembers(g, root, src, filePath, fileNodeID)

	// --- Namespace / module declarations: namespace Foo {} or module Foo {} ---
	// The TypeScript grammar represents these as internal_module nodes.
	nsQuery := `(internal_module (identifier) @ns_name)`
	_ = runQuery(lang, root, src, nsQuery, func(captures map[string]string, startLine int) {
		name := captures["ns_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return // already registered as a class/interface
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodePackage,
			Name:     name,
			Package:  moduleName,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- Method definitions (class-qualified) ---
	p.extractClassMethods(g, root, src, filePath, moduleName, fileNodeID, declInfo)

	// --- Call sites ---
	collectTSCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractClassMethods walks the AST to find method definitions inside class bodies
// and creates class-qualified method nodes.
func (p *TypeScriptParser) extractClassMethods(
	g *graph.Graph,
	root sitter.Node,
	src []byte,
	filePath, moduleName string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "export_statement":
			// Exported decorated class: decorator sibling + class_declaration child.
			var decs []string
			var exportedClassName string
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "decorator" {
					if name := tsDecoratorName(child, src); name != "" {
						decs = append(decs, name)
					}
				}
				if child.Type() == "class_declaration" || child.Type() == "abstract_class_declaration" {
					if nameNode := child.ChildByFieldName("name"); !nameNode.IsNull() {
						exportedClassName = string(src[nameNode.StartByte():nameNode.EndByte()])
					}
				}
			}
			if exportedClassName != "" && len(decs) > 0 {
				nodeID := g.MakeNodeID(filePath, exportedClassName)
				if classNode := g.GetNode(nodeID); classNode != nil {
					if classNode.Metadata == nil {
						classNode.Metadata = make(map[string]string)
					}
					classNode.Metadata["decorators"] = strings.Join(decs, ",")
				}
			}
			// Continue recursing into children.

		case "class_declaration", "abstract_class_declaration":
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			// Collect decorators that are direct children (non-exported class).
			if className != "" {
				var decs []string
				for i := uint32(0); i < n.ChildCount(); i++ {
					child := n.Child(i)
					if !child.IsNull() && child.Type() == "decorator" {
						if name := tsDecoratorName(child, src); name != "" {
							decs = append(decs, name)
						}
					}
				}
				if len(decs) > 0 {
					nodeID := g.MakeNodeID(filePath, className)
					if classNode := g.GetNode(nodeID); classNode != nil {
						if classNode.Metadata == nil {
							classNode.Metadata = make(map[string]string)
						}
						classNode.Metadata["decorators"] = strings.Join(decs, ",")
					}
				}
				// Heritage clause extraction: implements/extends.
				extractTSHeritage(g, n, src, filePath, className)
			}
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), className)
			}
			return
		case "interface_declaration":
			// Walk interface body for method_signature and property_signature.
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), className)
			}
			return
		case "method_definition", "method_signature", "abstract_method_signature":
			if enclosingClass == "" {
				break
			}
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := enclosingClass + "." + name
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break // already added (e.g. class + interface share a name)
			}
			meta := buildLangMeta(declInfo[qualName])
			if n.Type() == "abstract_method_signature" {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = "abstract"
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				Package:  moduleName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isExported(name),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Link class/interface → method
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}
		case "property_signature":
			// Interface property signature: e.g. readonly userId: string
			if enclosingClass == "" {
				break
			}
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := enclosingClass + "." + name
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break
			}
			meta := map[string]string{"kind": "property"}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				Package:  moduleName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isExported(name),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "public_field_definition":
			// TypeScript class field: name: string = ''; private age: number;
			if enclosingClass == "" {
				break
			}
			// Name is a property_identifier child (after optional accessibility_modifier / readonly).
			nameNode := firstChildOfType(n, "property_identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := enclosingClass + "." + name
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break
			}
			// Detect accessibility modifier for kind metadata.
			fieldKind := "field"
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "accessibility_modifier" {
					text := string(src[child.StartByte():child.EndByte()])
					if text == "private" || text == "protected" {
						fieldKind = text
					}
					break
				}
				if child.Type() == "readonly" {
					fieldKind = "readonly"
					break
				}
				if child.Type() == "property_identifier" {
					break // no modifier
				}
			}
			fieldMeta := map[string]string{"kind": fieldKind}
			// Determine exported: private/protected fields are not exported.
			exported := fieldKind != "private" && fieldKind != "protected"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				Package:  moduleName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: exported,
				Metadata: fieldMeta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectTSInstantiatedTypes walks the AST for new_expression nodes
// (new Foo(...)) and records the constructed type names via AddInstantiatedType.
// TypeScript's new_expression has a "constructor" field holding the class name.
// This enables RTA-style call graph refinement in the resolver.
func collectTSInstantiatedTypes(g *graph.Graph, root sitter.Node, src []byte, filePath string) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "new_expression" {
			constructor := n.ChildByFieldName("constructor")
			if !constructor.IsNull() {
				// Extract the simple identifier — skip member_expression (e.g. ns.Foo)
				// to avoid recording qualified names that won't match method receiver types.
				var typeName string
				switch constructor.Type() {
				case "identifier":
					typeName = string(src[constructor.StartByte():constructor.EndByte()])
				case "member_expression":
					// Use only the property (rightmost) part: new ns.Foo() → "Foo"
					prop := constructor.ChildByFieldName("property")
					if !prop.IsNull() {
						typeName = string(src[prop.StartByte():prop.EndByte()])
					}
				}
				if typeName != "" && !isTSBuiltin(typeName) {
					g.AddInstantiatedType(filePath, typeName)
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// tsDIDecorators is the set of Angular/NestJS/general DI decorators that indicate
// a class is instantiated by a framework container.
var tsDIDecorators = map[string]bool{
	"Component": true, "Injectable": true, "Directive": true, "Pipe": true,
	"NgModule": true, "Controller": true, "Module": true, "Resolver": true,
	"Guard": true, "Interceptor": true, "Middleware": true,
	"singleton": true, "service": true, "provide": true,
}

// collectTSDecoratorInstantiations records class names as instantiated when the
// class carries a DI decorator, or when a static method's return type matches
// the enclosing class (static factory pattern).
func collectTSDecoratorInstantiations(g *graph.Graph, root sitter.Node, src []byte, filePath string) {
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "class_declaration", "abstract_class_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			className := string(src[nameNode.StartByte():nameNode.EndByte()])

			// Two decorator placements in tree-sitter TypeScript:
			//
			// 1. Non-exported class: decorator is a direct CHILD of class_declaration.
			//    @Injectable() class Foo {} →  class_declaration → [decorator, class, name, body]
			//
			// 2. Exported class: decorator is a sibling inside the parent export_statement.
			//    @Injectable() export class Foo {} → export_statement → [decorator, export, class_declaration]
			//
			// We handle both by: (a) scanning class_declaration's own children, and
			// (b) scanning parent's children that precede this class node.

			// (a) decorator as direct child of this class_declaration node
			if !isTSBuiltin(className) {
				for i := uint32(0); i < n.ChildCount(); i++ {
					child := n.Child(i)
					if child.IsNull() || child.Type() != "decorator" {
						continue
					}
					decName := extractTSDecoratorName(child, src)
					if tsDIDecorators[decName] {
						g.AddInstantiatedType(filePath, className)
						break
					}
				}
			}

			// (b) decorator as sibling in parent node (export_statement case)
			parent := n.Parent()
			if !parent.IsNull() && !isTSBuiltin(className) {
				for i := uint32(0); i < parent.ChildCount(); i++ {
					sib := parent.Child(i)
					if sib.IsNull() {
						continue
					}
					if sib.Equal(n) {
						break // stop scanning once we reach the class itself
					}
					if sib.Type() != "decorator" {
						continue
					}
					decName := extractTSDecoratorName(sib, src)
					if tsDIDecorators[decName] {
						g.AddInstantiatedType(filePath, className)
						break
					}
				}
			}

			// Walk class body with class context for static factory detection.
			body := n.ChildByFieldName("body")
			if !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), className)
				}
			}
			return

		case "method_definition":
			if enclosingClass == "" {
				break
			}
			// Static factory: method has "static" modifier and return type == enclosingClass.
			isStatic := false
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if !child.IsNull() && child.Type() == "static" {
					isStatic = true
					break
				}
			}
			if !isStatic {
				break
			}
			// Find type_annotation child for return type.
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() || child.Type() != "type_annotation" {
					continue
				}
				// type_annotation → ":" type — find type_identifier inside
				retType := extractTSTypeIdentifier(child, src)
				if retType == enclosingClass && !isTSBuiltin(retType) {
					g.AddInstantiatedType(filePath, enclosingClass)
				}
				break
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// extractTSDecoratorName returns the decorator name from a decorator node.
// Handles: @Injectable, @Injectable(), @Component({...})
func extractTSDecoratorName(decorator sitter.Node, src []byte) string {
	// decorator's first non-@ child is identifier or call_expression
	for i := uint32(0); i < decorator.ChildCount(); i++ {
		child := decorator.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			return string(src[child.StartByte():child.EndByte()])
		case "call_expression":
			fn := child.ChildByFieldName("function")
			if !fn.IsNull() && fn.Type() == "identifier" {
				return string(src[fn.StartByte():fn.EndByte()])
			}
		}
	}
	return ""
}

// extractTSTypeIdentifier finds the first type_identifier inside a type_annotation node.
func extractTSTypeIdentifier(typeAnnotation sitter.Node, src []byte) string {
	var find func(n sitter.Node) string
	find = func(n sitter.Node) string {
		if n.IsNull() {
			return ""
		}
		if n.Type() == "type_identifier" {
			return string(src[n.StartByte():n.EndByte()])
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			if r := find(n.Child(i)); r != "" {
				return r
			}
		}
		return ""
	}
	return find(typeAnnotation)
}

// collectTSCallSites performs a depth-first AST walk to collect call sites with
// function-level caller resolution.
func collectTSCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Collect instantiated types for RTA-style call graph refinement.
	collectTSInstantiatedTypes(g, root, src, filePath)
	// Collect decorator-annotated and static-factory instantiations.
	collectTSDecoratorInstantiations(g, root, src, filePath)
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{
			"class_declaration":          true,
			"abstract_class_declaration": true,
		},
		FuncTypes: map[string]bool{
			"function_declaration": true,
			"method_definition":    true,
			"arrow_function":       true,
			"function_expression":  true,
		},
		CallTypes: map[string]bool{"call_expression": true},
		NameExtractor: func(n sitter.Node, src []byte) string {
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		AliasedCalleeExtractor: jsAliasedCalleeExtractor, // TS uses the same call expression structure as JS.
		IsBuiltin:              isTSBuiltin,
	})
}

// extractTSEnumMembers walks the AST for enum_declaration nodes and emits
// individual enum member nodes (e.g. Color.Red, Color.Green).
// TS grammar: enum_declaration → enum_body → property_identifier children.
func extractTSEnumMembers(g *graph.Graph, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "enum_declaration" {
			// Get enum name.
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				return
			}
			enumName := string(src[nameNode.StartByte():nameNode.EndByte()])
			enumNodeID := g.MakeNodeID(filePath, enumName)
			// Walk enum_body for property_identifier children (the member names).
			for i := uint32(0); i < n.ChildCount(); i++ {
				body := n.Child(i)
				if body.IsNull() || body.Type() != "enum_body" {
					continue
				}
				for j := uint32(0); j < body.ChildCount(); j++ {
					member := body.Child(j)
					if member.IsNull() {
						continue
					}
					// Direct property_identifier (no value) or enum_assignment → property_identifier.
					var nameNode sitter.Node
					if member.Type() == "property_identifier" {
						nameNode = member
					} else if member.Type() == "enum_assignment" {
						nameNode = firstChildOfType(member, "property_identifier")
					}
					if nameNode.IsNull() {
						continue
					}
					memberName := string(src[nameNode.StartByte():nameNode.EndByte()])
					qualName := enumName + "." + memberName
					nodeID := g.MakeNodeID(filePath, qualName)
					if g.GetNode(nodeID) != nil {
						continue
					}
					meta := map[string]string{"kind": "enum_member"}
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
						Line: int(member.StartPoint().Row) + 1, Exported: true, Metadata: meta,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					if g.GetNode(enumNodeID) != nil {
						g.AddEdge(&graph.Edge{From: enumNodeID, To: nodeID, Type: graph.EdgeDefines})
					}
				}
			}
			return
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// tsDecoratorName extracts the decorator identifier from a decorator node.
// Handles both @Component() (call_expression) and @Component (bare identifier).
func tsDecoratorName(decorator sitter.Node, src []byte) string {
	if ce := firstChildOfType(decorator, "call_expression"); !ce.IsNull() {
		if ident := firstChildOfType(ce, "identifier"); !ident.IsNull() {
			return string(src[ident.StartByte():ident.EndByte()])
		}
	}
	if ident := firstChildOfType(decorator, "identifier"); !ident.IsNull() {
		return string(src[ident.StartByte():ident.EndByte()])
	}
	return ""
}

// extractTSHeritage walks the children of a TypeScript class_declaration or
// abstract_class_declaration node, finds class_heritage, and extracts
// implements/extends type names into the class node's metadata.
func extractTSHeritage(g *graph.Graph, classNode sitter.Node, src []byte, filePath, className string) {
	nodeID := g.MakeNodeID(filePath, className)
	node := g.GetNode(nodeID)
	if node == nil {
		return
	}

	var implementsNames, extendsNames []string

	for i := uint32(0); i < classNode.ChildCount(); i++ {
		child := classNode.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "class_heritage":
			// class_heritage contains extends_clause and/or implements_clause.
			for j := uint32(0); j < child.ChildCount(); j++ {
				hChild := child.Child(j)
				if hChild.IsNull() {
					continue
				}
				switch hChild.Type() {
				case "extends_clause":
					extendsNames = append(extendsNames, extractTypeIdentifiers(hChild, src)...)
				case "implements_clause":
					implementsNames = append(implementsNames, extractTypeIdentifiers(hChild, src)...)
				}
			}
		}
	}

	if len(implementsNames) == 0 && len(extendsNames) == 0 {
		return
	}

	if node.Metadata == nil {
		node.Metadata = make(map[string]string)
	}
	if len(implementsNames) > 0 {
		node.Metadata["heritage_implements"] = strings.Join(implementsNames, ",")
	}
	if len(extendsNames) > 0 {
		node.Metadata["heritage_extends"] = strings.Join(extendsNames, ",")
	}
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
		// ES6 built-in constructors used with new.
		"Map", "Set", "WeakMap", "WeakSet", "WeakRef",
		"ArrayBuffer", "DataView", "Int8Array", "Uint8Array", "Int16Array",
		"Uint16Array", "Int32Array", "Uint32Array", "Float32Array", "Float64Array",
		"URL", "URLSearchParams", "TextEncoder", "TextDecoder",
		"Worker", "MessageChannel", "BroadcastChannel",
		"parseInt", "parseFloat", "isNaN", "isFinite",
		"now", "from", "of", "call", "apply", "bind",
		"require", "define":
		return true
	}
	return false
}
