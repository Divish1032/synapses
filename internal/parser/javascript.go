package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/javascript"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// JavaScriptParser parses JavaScript (.js, .jsx, .mjs, .cjs) source files.
type JavaScriptParser struct {
	language *sitter.Language
}

// NewJavaScriptParser creates a ready-to-use JavaScriptParser.
func NewJavaScriptParser() *JavaScriptParser {
	return &JavaScriptParser{language: javascript.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *JavaScriptParser) Extensions() []string {
	return []string{".js", ".jsx", ".mjs", ".cjs"}
}

// extractJSDeclInfo walks the JavaScript AST and builds a name→declMeta map
// for all function, class, and method declarations.
func extractJSDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n *sitter.Node, enclosingClass string, depth int)
	walk = func(n *sitter.Node, enclosingClass string, depth int) {
		if n == nil || depth > 8 {
			return
		}
		switch n.Type() {
		case "function_declaration", "function_expression":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"statement_block"}),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "method_definition":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
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
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[className] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			// Walk children with class context for method qualification.
			for i := 0; i < int(n.ChildCount()); i++ {
				walk(n.Child(i), className, depth+1)
			}
			return
		case "variable_declarator":
			nameNode := n.ChildByFieldName("name")
			valueNode := n.ChildByFieldName("value")
			if nameNode != nil && valueNode != nil {
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
		for i := 0; i < int(n.ChildCount()); i++ {
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

	tree, _ := parser.ParseCtx(context.Background(), nil, src)
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

	// --- Class declarations ---
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

	// --- Method definitions (inside class bodies) --- class-qualified names.
	p.extractClassMethods(g, root, src, filePath, moduleName, fileNodeID, declInfo)

	// --- Call sites ---
	collectJSCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractClassMethods walks the AST to find method definitions inside class bodies
// and creates class-qualified method nodes (ClassName.methodName).
func (p *JavaScriptParser) extractClassMethods(
	g *graph.Graph,
	root *sitter.Node,
	src []byte,
	filePath, moduleName string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
) {
	var walk func(n *sitter.Node, enclosingClass string)
	walk = func(n *sitter.Node, enclosingClass string) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "class_declaration":
			className := ""
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			for i := 0; i < int(n.ChildCount()); i++ {
				walk(n.Child(i), className)
			}
			return
		case "method_definition":
			if enclosingClass == "" {
				break
			}
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
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
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectJSCallSites walks the AST and adds call sites for function and method
// calls so the cross-file resolver can link them as CALLS edges.
func collectJSCallSites(g *graph.Graph, lang *sitter.Language, root *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Direct calls: foo(...)
	directQuery := `(call_expression function: (identifier) @callee)`
	_ = runQuery(lang, root, src, directQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || isTSBuiltin(callee) || callee == "require" {
			return
		}
		g.AddCallSite(graph.CallSite{
			CallerID:   fileNodeID,
			CallerFile: filePath,
			FuncName:   callee,
			PkgAlias:   "",
		})
	})

	// Method calls: obj.method(...)
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
