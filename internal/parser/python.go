package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"

	"github.com/Divish1032/synapses/internal/graph"
)

// extractPythonDeclInfo walks the Python AST and builds a name→declMeta map
// for function_definition and class_definition nodes at any nesting depth.
func extractPythonDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		if n == nil || depth > 6 {
			return
		}
		switch n.Type() {
		case "function_definition":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_definition":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "decorated_definition":
			// @decorator\ndef foo(): ... — use decorator start for doc lookup
			for j := 0; j < int(n.ChildCount()); j++ {
				inner := n.Child(j)
				if inner == nil {
					continue
				}
				if inner.Type() == "function_definition" || inner.Type() == "class_definition" {
					if nameNode := inner.ChildByFieldName("name"); nameNode != nil {
						name := string(src[nameNode.StartByte():nameNode.EndByte()])
						sl := int(n.StartPoint().Row) + 1
						result[name] = declMeta{
							Signature: extractSigToBody(inner, src),
							Doc:       extractLineDoc(lines, sl, "#"),
							LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
						}
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

// PythonParser parses Python (.py, .pyi) source files.
type PythonParser struct {
	language *sitter.Language
}

// NewPythonParser creates a ready-to-use PythonParser.
func NewPythonParser() *PythonParser {
	return &PythonParser{language: python.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *PythonParser) Extensions() []string {
	return []string{".py", ".pyi"}
}

// Parse extracts code entities from a single Python file and merges them
// into the provided graph. The following constructs are captured:
//
//   - import statements        → IMPORTS edges
//   - from X import Y          → IMPORTS edges
//   - function definitions     → NodeFunction (module-level) or NodeMethod (inside class)
//   - class definitions        → NodeStruct
//   - function call sites      → stored for resolver (produces CALLS edges)
func (p *PythonParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseCtx(context.Background(), nil, src)
	root := tree.RootNode()

	// Module name = basename without extension (e.g. "worker" for worker.py).
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
	declInfo := extractPythonDeclInfo(root, src)

	// Track which names are inside a class body (to classify as methods).
	classBodyFuncs := buildPythonClassMethods(root, src)

	// --- import X / import X.Y ---
	importQuery := `(import_statement name: (dotted_name) @import_path)`
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

	// --- from X import Y ---
	fromImportQuery := `(import_from_statement module_name: (dotted_name) @import_path)`
	if err := runQuery(lang, root, src, fromImportQuery, func(captures map[string]string, _ int) {
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

	// --- Function / method definitions ---
	funcQuery := `(function_definition name: (identifier) @func_name)`
	if err := runQuery(lang, root, src, funcQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		nodeType := graph.NodeFunction
		if classBodyFuncs[name] {
			nodeType = graph.NodeMethod
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     nodeType,
			Name:     name,
			Package:  moduleName,
			File:     filePath,
			Line:     startLine,
			Exported: isPythonPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Class definitions ---
	classQuery := `(class_definition name: (identifier) @class_name)`
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
			Exported: isPythonPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Call sites: direct calls e.g. Worker(config), compute_hash(data) ---
	// These are collected now and resolved into CALLS edges after all files are parsed.
	collectPythonCallSites(g, lang, root, src, filePath, moduleName)

	return nil
}

// buildPythonClassMethods returns a set of method names that appear directly
// inside a class body (i.e. are methods, not module-level functions).
func buildPythonClassMethods(root *sitter.Node, src []byte) map[string]bool {
	result := make(map[string]bool)
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "class_definition" {
			body := n.ChildByFieldName("body")
			if body != nil {
				for i := 0; i < int(body.ChildCount()); i++ {
					child := body.Child(i)
					if child == nil {
						continue
					}
					target := child
					if target.Type() == "decorated_definition" {
						for j := 0; j < int(target.ChildCount()); j++ {
							inner := target.Child(j)
							if inner != nil && (inner.Type() == "function_definition") {
								target = inner
								break
							}
						}
					}
					if target.Type() == "function_definition" {
						if nameNode := target.ChildByFieldName("name"); nameNode != nil {
							result[string(src[nameNode.StartByte():nameNode.EndByte()])] = true
						}
					}
				}
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return result
}

// collectPythonCallSites walks the AST and adds call sites for every direct
// function/class call (identifier calls). These are resolved later by the
// cross-file resolver into CALLS edges.
func collectPythonCallSites(g *graph.Graph, lang *sitter.Language, root *sitter.Node, src []byte, filePath, moduleName string) {
	// Map function names → node IDs for the current file (to find the enclosing caller).
	// Use module-level and method nodes; pick the closest enclosing function as caller.
	funcNodeIDs := make(map[string]graph.NodeID)
	for _, n := range g.AllNodes() {
		if n.File == filePath && (n.Type == graph.NodeFunction || n.Type == graph.NodeMethod) {
			funcNodeIDs[n.Name] = n.ID
		}
	}

	// Use file node as default caller for module-level calls.
	fileNodeID := g.MakeNodeID(filePath, filePath)

	// Query: simple identifier calls — Func(...) or Class(...)
	callQuery := `(call function: (identifier) @callee)`
	_ = runQuery(lang, root, src, callQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || isBuiltinPython(callee) {
			return
		}
		g.AddCallSite(graph.CallSite{
			CallerID:   fileNodeID,
			CallerFile: filePath,
			FuncName:   callee,
			PkgAlias:   "",
		})
	})

	// Query: attribute/method calls — self.method(), obj.func()
	// Captures the method name portion so the resolver can link to the target.
	attrCallQuery := `(call function: (attribute attribute: (identifier) @callee))`
	_ = runQuery(lang, root, src, attrCallQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || isBuiltinPython(callee) {
			return
		}
		g.AddCallSite(graph.CallSite{
			CallerID:   fileNodeID,
			CallerFile: filePath,
			FuncName:   callee,
			PkgAlias:   "",
		})
	})
}

// isPythonPublic returns true if the name is not prefixed with an underscore.
// In Python, _name and __name are private/dunder by convention.
func isPythonPublic(name string) bool {
	return !strings.HasPrefix(name, "_")
}

// isBuiltinPython returns true for Python built-in functions and common stdlib
// calls that should never generate CALLS edges.
func isBuiltinPython(name string) bool {
	switch name {
	case "print", "len", "range", "enumerate", "zip", "map", "filter", "sorted",
		"list", "dict", "set", "tuple", "str", "int", "float", "bool", "bytes",
		"type", "isinstance", "issubclass", "hasattr", "getattr", "setattr",
		"open", "super", "property", "staticmethod", "classmethod",
		"abs", "max", "min", "sum", "any", "all", "next", "iter",
		"repr", "hash", "id", "callable", "vars", "dir", "object",
		"Exception", "ValueError", "TypeError", "KeyError", "IndexError",
		"RuntimeError", "StopIteration", "NotImplementedError", "IOError":
		return true
	}
	return false
}
