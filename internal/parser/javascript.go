package parser

import (
	"path/filepath"
	"strings"

	"github.com/alexaandru/go-sitter-forest/javascript"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// JavaScriptParser parses JavaScript (.js, .jsx, .mjs, .cjs) source files.
type JavaScriptParser struct {
	language *sitter.Language
}

// NewJavaScriptParser creates a ready-to-use JavaScriptParser.
func NewJavaScriptParser() *JavaScriptParser {
	return &JavaScriptParser{language: sitter.NewLanguage(javascript.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *JavaScriptParser) Extensions() []string {
	return []string{".js", ".jsx", ".mjs", ".cjs"}
}

func (p *JavaScriptParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// extractJSDeclInfo walks the JavaScript AST and builds a name→declMeta map
// for all function, class, and method declarations.
func extractJSDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n sitter.Node, enclosingClass string, depth int)
	walk = func(n sitter.Node, enclosingClass string, depth int) {
		if n.IsNull() || depth > 8 {
			return
		}
		switch n.Type() {
		case "function_declaration", "function_expression":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"statement_block"}),
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
					Signature: extractSigToBodyMulti(n, src, []string{"statement_block"}),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration":
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[className] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			// Walk children with class context for method qualification.
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), className, depth+1)
			}
			return
		case "variable_declarator":
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

// Parse extracts code entities from a single JavaScript file and merges them
// into the provided graph. The following constructs are captured:
//
//   - Import declarations (ESM)     → IMPORTS edges
//   - CommonJS require() calls      → IMPORTS edges
//   - Function declarations         → NodeFunction
//   - Arrow functions (named)       → NodeFunction
//   - Method definitions            → NodeMethod (class-qualified)
//   - Class declarations            → NodeStruct
//   - Call expressions              → call sites (resolved to CALLS edges)
func (p *JavaScriptParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree != nil {
		defer tree.Close()
	}
	if tree == nil {
		return nil
	}
	root := tree.RootNode()

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

	lang := p.language
	declInfo := extractJSDeclInfo(root, src)

	// --- ESM import declarations ---
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

	// --- CommonJS require() calls: const x = require('module') ---
	requireQuery := `(call_expression function: (identifier) @fn_name arguments: (arguments (string (string_fragment) @req_path)) (#eq? @fn_name "require"))`
	if err := runQuery(lang, root, src, requireQuery, func(captures map[string]string, _ int) {
		reqPath := captures["req_path"]
		if reqPath == "" {
			return
		}
		importNodeID := g.MakeNodeID(reqPath, reqPath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    reqPath,
			Package: reqPath,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- CommonJS module.exports = X → mark X as exported ---
	moduleExportsQuery := `
(assignment_expression
  left: (member_expression
    object: (identifier) @obj
    property: (property_identifier) @prop)
  right: (identifier) @rhs
  (#eq? @obj "module")
  (#eq? @prop "exports"))`
	var moduleExportedNames []string
	if err := runQuery(lang, root, src, moduleExportsQuery, func(captures map[string]string, _ int) {
		if name := captures["rhs"]; name != "" {
			moduleExportedNames = append(moduleExportedNames, name)
		}
	}); err != nil {
		return err
	}

	// --- CommonJS module.exports = { key: val, ... } → create exported nodes for each key ---
	// Handles patterns like: module.exports = { Router: Router, json: bodyParser.json }
	moduleExportsObjQuery := `
(assignment_expression
  left: (member_expression
    object: (identifier) @obj
    property: (property_identifier) @prop)
  right: (object
    (pair
      key: [(property_identifier) (string)] @key) @pair)
  (#eq? @obj "module")
  (#eq? @prop "exports"))`
	if err := runQuery(lang, root, src, moduleExportsObjQuery, func(captures map[string]string, startLine int) {
		keyName := captures["key"]
		// Strip quotes from string keys.
		keyName = strings.Trim(keyName, "\"'")
		if keyName == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, keyName)
		if g.GetNode(nodeID) == nil {
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     keyName,
				Package:  moduleName,
				File:     filePath,
				Line:     startLine,
				Exported: true,
				Metadata: buildLangMeta(declInfo[keyName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		} else {
			if n := g.GetNode(nodeID); n != nil {
				n.Exported = true
			}
		}
	}); err != nil {
		return err
	}

	// --- CommonJS exports.foo = expr → EdgeExports from file ---
	exportsPropertyQuery := `
(assignment_expression
  left: (member_expression
    object: (identifier) @obj
    property: (property_identifier) @prop)
  (#eq? @obj "exports"))`
	if err := runQuery(lang, root, src, exportsPropertyQuery, func(captures map[string]string, startLine int) {
		propName := captures["prop"]
		if propName == "" || propName == "exports" {
			return
		}
		// Create an exported node for the property if it doesn't already exist.
		nodeID := g.MakeNodeID(filePath, propName)
		if g.GetNode(nodeID) == nil {
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     propName,
				Package:  moduleName,
				File:     filePath,
				Line:     startLine,
				Exported: true,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		} else {
			// Node already exists (e.g., from a function declaration); mark exported.
			if n := g.GetNode(nodeID); n != nil {
				n.Exported = true
			}
		}
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

	// --- Exported arrow / function expressions: export const foo = () => {} ---
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

	// --- Non-exported arrow/function expressions at module level ---
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

	// --- Prototype method assignments: obj.method = function name() {} ---
	// Captures CommonJS patterns like: app.listen = function listen() { ... }
	// The function_expression's name (if present) becomes the node name.
	// Falls back to the property name if the function is anonymous.
	protoMethodQuery := `
(assignment_expression
  left: (member_expression
    object: (identifier) @obj_name
    property: (property_identifier) @method_name)
  right: [(function_expression) (arrow_function)]
)`
	if err := runQuery(lang, root, src, protoMethodQuery, func(captures map[string]string, startLine int) {
		methodName := captures["method_name"]
		if methodName == "" || methodName == "exports" {
			return
		}
		// Skip if node already exists (from function_declaration or other query).
		nodeID := g.MakeNodeID(filePath, methodName)
		if g.GetNode(nodeID) != nil {
			return
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeMethod,
			Name:     methodName,
			Package:  moduleName,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: buildLangMeta(declInfo[methodName]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Class declarations ---
	classQuery := `(class_declaration name: (identifier) @class_name)`
	if err := runQuery(lang, root, src, classQuery, func(captures map[string]string, startLine int) {
		name := captures["class_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declInfo[name])
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

	// Extract heritage (extends/implements) for class declarations.
	// TypeScript: class Foo extends Bar implements IBaz { ... }
	extractTSClassHeritage(g, root, src, filePath)

	// --- Method definitions (inside class bodies) --- class-qualified names.
	p.extractClassMethods(g, root, src, filePath, moduleName, fileNodeID, declInfo)

	// --- Call sites ---
	collectJSCallSites(g, lang, root, src, filePath, fileNodeID)

	// --- Apply module.exports = X: mark the named function/variable as exported ---
	for _, name := range moduleExportedNames {
		nodeID := g.MakeNodeID(filePath, name)
		if n := g.GetNode(nodeID); n != nil {
			n.Exported = true
		}
	}

	return nil
}

// extractClassMethods walks the AST to find method definitions inside class bodies
// and creates class-qualified method nodes (ClassName.methodName).
func (p *JavaScriptParser) extractClassMethods(
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
		case "class_declaration":
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), className)
			}
			return
		case "method_definition":
			if enclosingClass == "" {
				break
			}
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			if name == "constructor" {
				// Capture constructors too — they're important.
				qualName := enclosingClass + ".constructor"
				nodeID := g.MakeNodeID(filePath, qualName)
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeMethod,
					Name:     qualName,
					Package:  moduleName,
					File:     filePath,
					Line:     int(n.StartPoint().Row) + 1,
					Exported: true,
					Metadata: buildLangMeta(declInfo[qualName]),
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				// Link class → constructor
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
				break
			}
			qualName := enclosingClass + "." + name
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				Package:  moduleName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isExported(name),
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Link class → method
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}
		case "field_definition":
			// Modern JS class field: name = ''; or #privateField = 0;
			if enclosingClass == "" {
				break
			}
			// Name is property_identifier (public) or private_property_identifier (#name).
			var name string
			isPrivate := false
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "property_identifier" {
					name = string(src[child.StartByte():child.EndByte()])
					break
				}
				if child.Type() == "private_property_identifier" {
					raw := string(src[child.StartByte():child.EndByte()])
					name = strings.TrimPrefix(raw, "#")
					isPrivate = true
					break
				}
			}
			if name == "" {
				break
			}
			qualName := enclosingClass + "." + name
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break
			}
			fieldKind := "field"
			if isPrivate {
				fieldKind = "private"
			}
			fieldMeta := map[string]string{"kind": fieldKind}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				Package:  moduleName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: !isPrivate,
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

// collectJSCallSites performs a depth-first AST walk to collect call sites with
// function-level caller resolution, matching the Go parser's accuracy.
func collectJSCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Collect instantiated types for RTA-style call graph refinement.
	collectTSInstantiatedTypes(g, root, src, filePath)
	// Collect decorator-annotated and static-factory instantiations.
	collectTSDecoratorInstantiations(g, root, src, filePath)
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{"class_declaration": true},
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
		AliasedCalleeExtractor: jsAliasedCalleeExtractor,
		IsBuiltin: func(name string) bool {
			return isTSBuiltin(name) || name == "require"
		},
	})
}

// jsCalleeExtractor extracts callee names from JS/TS call expressions.
// Handles: foo(...), obj.method(...), and obj?.method(...).
func jsCalleeExtractor(n sitter.Node, src []byte) string {
	fn := n.ChildByFieldName("function")
	if fn.IsNull() {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return string(src[fn.StartByte():fn.EndByte()])
	case "member_expression":
		prop := fn.ChildByFieldName("property")
		if !prop.IsNull() {
			return string(src[prop.StartByte():prop.EndByte()])
		}
	}
	return ""
}

// jsAliasedCalleeExtractor returns (alias, callee) for qualified calls.
// For obj.method(), alias="obj", callee="method".
// For direct calls foo(), alias="", callee="foo".
func jsAliasedCalleeExtractor(n sitter.Node, src []byte) (string, string) {
	fn := n.ChildByFieldName("function")
	if fn.IsNull() {
		return "", ""
	}
	switch fn.Type() {
	case "identifier":
		return "", string(src[fn.StartByte():fn.EndByte()])
	case "member_expression":
		obj := fn.ChildByFieldName("object")
		prop := fn.ChildByFieldName("property")
		if !prop.IsNull() {
			callee := string(src[prop.StartByte():prop.EndByte()])
			if !obj.IsNull() {
				switch obj.Type() {
				case "identifier":
					return string(src[obj.StartByte():obj.EndByte()]), callee
				case "this":
					return "this", callee
				case "super":
					return "super", callee
				case "member_expression":
					// Chained: a.b.method() → alias="a.b"
					return string(src[obj.StartByte():obj.EndByte()]), callee
				}
			}
			return "", callee
		}
	}
	return "", ""
}

// extractTSClassHeritage walks the AST to find class_declaration nodes with
// heritage clauses (extends/implements) and stores them as metadata on the
// corresponding graph node. This enables inheritance-based method resolution.
//
// TypeScript AST shape:
//
//	(class_declaration
//	  name: (type_identifier)
//	  (class_heritage
//	    (extends_clause (identifier) ...)
//	    (implements_clause (type_identifier) ...)))
//
// JavaScript only has extends (no implements keyword).
func extractTSClassHeritage(g *graph.Graph, root sitter.Node, src []byte, filePath string) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "class_declaration" {
			extractSingleClassHeritage(g, n, src, filePath)
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// extractSingleClassHeritage extracts heritage (extends/implements) from a
// single class_declaration node and stores it as metadata.
func extractSingleClassHeritage(g *graph.Graph, classNode sitter.Node, src []byte, filePath string) {
	nameNode := classNode.ChildByFieldName("name")
	if nameNode.IsNull() {
		return
	}
	className := string(src[nameNode.StartByte():nameNode.EndByte()])
	nodeID := g.MakeNodeID(filePath, className)
	node := g.GetNode(nodeID)
	if node == nil {
		return
	}

	var extendsNames, implementsNames []string
	for i := uint32(0); i < classNode.ChildCount(); i++ {
		child := classNode.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "class_heritage":
			for j := uint32(0); j < child.ChildCount(); j++ {
				clause := child.Child(j)
				if clause.IsNull() {
					continue
				}
				switch clause.Type() {
				case "extends_clause":
					extendsNames = append(extendsNames, extractTSTypeNames(clause, src)...)
				case "implements_clause":
					implementsNames = append(implementsNames, extractTSTypeNames(clause, src)...)
				}
			}
		case "extends_clause":
			extendsNames = append(extendsNames, extractTSTypeNames(child, src)...)
		case "implements_clause":
			implementsNames = append(implementsNames, extractTSTypeNames(child, src)...)
		}
	}

	if len(extendsNames) == 0 && len(implementsNames) == 0 {
		return
	}
	if node.Metadata == nil {
		node.Metadata = make(map[string]string)
	}
	if len(extendsNames) > 0 {
		node.Metadata["heritage_extends"] = strings.Join(extendsNames, ",")
	}
	if len(implementsNames) > 0 {
		node.Metadata["heritage_implements"] = strings.Join(implementsNames, ",")
	}
}

// extractTSTypeNames extracts type identifier names from an extends_clause or
// implements_clause node. Handles both simple identifiers and generic types
// (e.g., Array<string> → "Array").
func extractTSTypeNames(clause sitter.Node, src []byte) []string {
	var names []string
	for i := uint32(0); i < clause.ChildCount(); i++ {
		child := clause.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier", "type_identifier":
			names = append(names, string(src[child.StartByte():child.EndByte()]))
		case "generic_type":
			// Generic type: React.Component<Props> → extract "Component" or just the name part.
			if nameNode := child.ChildByFieldName("name"); !nameNode.IsNull() {
				names = append(names, string(src[nameNode.StartByte():nameNode.EndByte()]))
			} else {
				// Fallback: first identifier child.
				for j := uint32(0); j < child.ChildCount(); j++ {
					gc := child.Child(j)
					if !gc.IsNull() && (gc.Type() == "identifier" || gc.Type() == "type_identifier") {
						names = append(names, string(src[gc.StartByte():gc.EndByte()]))
						break
					}
				}
			}
		case "member_expression", "nested_type_identifier":
			// Qualified: React.Component → extract full text but just last segment.
			text := string(src[child.StartByte():child.EndByte()])
			if dot := strings.LastIndexByte(text, '.'); dot >= 0 {
				names = append(names, text[dot+1:])
			} else {
				names = append(names, text)
			}
		}
	}
	return names
}
