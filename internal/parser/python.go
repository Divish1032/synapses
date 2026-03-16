package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// extractPythonDeclInfo walks the Python AST and builds a name→declMeta map
// for function_definition and class_definition nodes at any nesting depth.
// Method names are class-qualified (ClassName.method_name).
func extractPythonDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n *sitter.Node, enclosingClass string, depth int)
	walk = func(n *sitter.Node, enclosingClass string, depth int) {
		if n == nil || depth > 8 {
			return
		}
		switch n.Type() {
		case "function_definition":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				// Try docstring first, then # comments.
				doc := extractPythonDocstring(lines, sl)
				if doc == "" {
					doc = extractLineDoc(lines, sl, "#")
				}
				result[qualName] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       doc,
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_definition":
			className := ""
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				doc := extractPythonDocstring(lines, sl)
				if doc == "" {
					doc = extractLineDoc(lines, sl, "#")
				}
				result[className] = declMeta{
					Doc:       doc,
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			// Walk children with class context.
			body := n.ChildByFieldName("body")
			if body != nil {
				for i := 0; i < int(body.ChildCount()); i++ {
					walk(body.Child(i), className, depth+1)
				}
			}
			return
		case "decorated_definition":
			for j := 0; j < int(n.ChildCount()); j++ {
				inner := n.Child(j)
				if inner == nil {
					continue
				}
				if inner.Type() == "function_definition" || inner.Type() == "class_definition" {
					walk(inner, enclosingClass, depth+1)
				}
			}
			return
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), enclosingClass, depth+1)
		}
	}
	walk(root, "", 0)
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
// into the provided graph.
func (p *PythonParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractPythonDeclInfo(root, src)

	// (class→method mapping is handled directly by extractFunctionsAndMethods AST walk)

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

	// --- Function / method definitions (class-qualified via AST walk) ---
	p.extractFunctionsAndMethods(g, root, src, filePath, moduleName, fileNodeID, declInfo)

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

	// --- Module-level ALL_CAPS constants (e.g. MAX_RETRIES = 3, DEFAULT_TIMEOUT = 30) ---
	// Walk top-level assignment nodes; if the name is ALL_CAPS, emit a const node.
	var walkPyConst func(n *sitter.Node)
	walkPyConst = func(n *sitter.Node) {
		if n == nil {
			return
		}
		// Only look at top-level: children of the module root.
		if n.Type() == "expression_statement" {
			for i := 0; i < int(n.ChildCount()); i++ {
				child := n.Child(i)
				if child == nil || child.Type() != "assignment" {
					continue
				}
				lhs := child.ChildByFieldName("left")
				if lhs == nil {
					continue
				}
				var name string
				if lhs.Type() == "identifier" {
					name = string(src[lhs.StartByte():lhs.EndByte()])
				}
				if name == "" || !isPythonAllCaps(name) {
					continue
				}
				nodeID := g.MakeNodeID(filePath, name)
				if g.GetNode(nodeID) != nil {
					continue
				}
				meta := map[string]string{"kind": "const"}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeStruct,
					Name:     name,
					Package:  moduleName,
					File:     filePath,
					Line:     int(lhs.StartPoint().Row) + 1,
					Exported: isPythonPublic(name),
					Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
	}
	// Only walk direct children of the module root (depth=1 only).
	for i := 0; i < int(root.ChildCount()); i++ {
		walkPyConst(root.Child(i))
	}

	// --- Call sites ---
	collectPythonCallSites(g, lang, root, src, filePath, moduleName)

	return nil
}

// extractFunctionsAndMethods walks the AST to emit function and method nodes
// with class-qualified names for methods inside class bodies.
func (p *PythonParser) extractFunctionsAndMethods(
	g *graph.Graph,
	root *sitter.Node,
	src []byte,
	filePath, moduleName string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
) {
	// emitFunc creates a function/method node for a function_definition node,
	// applying any decorator metadata (property, classmethod, staticmethod).
	emitFunc := func(n *sitter.Node, enclosingClass string, decorators []string) {
		nameNode := n.ChildByFieldName("name")
		if nameNode == nil {
			return
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])
		startLine := int(n.StartPoint().Row) + 1

		// Determine kind from decorators.
		decoratorKind := ""
		for _, d := range decorators {
			switch d {
			case "property":
				decoratorKind = "property"
			case "classmethod":
				decoratorKind = "classmethod"
			case "staticmethod":
				decoratorKind = "staticmethod"
			}
		}

		if enclosingClass != "" {
			qualName := enclosingClass + "." + name
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := buildLangMeta(declInfo[qualName])
			if decoratorKind != "" {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = decoratorKind
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				Package:  moduleName,
				File:     filePath,
				Line:     startLine,
				Exported: isPythonPublic(name),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}
		} else {
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if decoratorKind != "" {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = decoratorKind
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     name,
				Package:  moduleName,
				File:     filePath,
				Line:     startLine,
				Exported: isPythonPublic(name),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	var walk func(n *sitter.Node, enclosingClass string)
	walk = func(n *sitter.Node, enclosingClass string) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "class_definition":
			className := ""
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			body := n.ChildByFieldName("body")
			if body != nil {
				for i := 0; i < int(body.ChildCount()); i++ {
					walk(body.Child(i), className)
				}
			}
			return
		case "expression_statement":
			// Type-annotated class fields: name: Type (= value)?
			// AST: expression_statement → assignment → identifier, ":", type[, "=", value]
			if enclosingClass != "" {
				for i := 0; i < int(n.ChildCount()); i++ {
					assign := n.Child(i)
					if assign == nil || assign.Type() != "assignment" {
						continue
					}
					// Must have a "type" child to be a type-annotated field.
					if firstChildOfType(assign, "type") == nil {
						continue
					}
					nameNode := firstChildOfType(assign, "identifier")
					if nameNode == nil {
						continue
					}
					fieldName := string(src[nameNode.StartByte():nameNode.EndByte()])
					qualName := enclosingClass + "." + fieldName
					nodeID := g.MakeNodeID(filePath, qualName)
					if g.GetNode(nodeID) != nil {
						continue
					}
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
						Line:     int(assign.StartPoint().Row) + 1,
						Exported: isPythonPublic(fieldName),
						Metadata: map[string]string{"kind": "field"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					classID := g.MakeNodeID(filePath, enclosingClass)
					if g.GetNode(classID) != nil {
						g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
					}
				}
			}
			// Fall through to recurse (may contain nested function defs).
		case "decorated_definition":
			// Collect decorator names then dispatch to the inner definition.
			var decorators []string
			for j := 0; j < int(n.ChildCount()); j++ {
				child := n.Child(j)
				if child == nil {
					continue
				}
				if child.Type() == "decorator" {
					decText := strings.TrimPrefix(strings.TrimSpace(string(src[child.StartByte():child.EndByte()])), "@")
					if idx := strings.IndexByte(decText, '('); idx >= 0 {
						decText = decText[:idx]
					}
					decorators = append(decorators, strings.TrimSpace(decText))
				}
			}
			for j := 0; j < int(n.ChildCount()); j++ {
				child := n.Child(j)
				if child == nil {
					continue
				}
				if child.Type() == "function_definition" {
					emitFunc(child, enclosingClass, decorators)
				} else if child.Type() == "class_definition" {
					walk(child, enclosingClass)
				}
			}
			return
		case "function_definition":
			emitFunc(n, enclosingClass, nil)
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectPythonCallSites performs a depth-first AST walk to collect call sites
// with function-level caller resolution.
func collectPythonCallSites(g *graph.Graph, _ *sitter.Language, root *sitter.Node, src []byte, filePath, _ string) {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{"class_definition": true},
		FuncTypes: map[string]bool{
			"function_definition":  true,
			"decorated_definition": false, // walk through decorators, not into them
		},
		CallTypes: map[string]bool{"call": true},
		NameExtractor: func(n *sitter.Node, src []byte) string {
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		CalleeExtractor: func(n *sitter.Node, src []byte) string {
			fn := n.ChildByFieldName("function")
			if fn == nil {
				return ""
			}
			switch fn.Type() {
			case "identifier":
				return string(src[fn.StartByte():fn.EndByte()])
			case "attribute":
				attr := fn.ChildByFieldName("attribute")
				if attr != nil {
					return string(src[attr.StartByte():attr.EndByte()])
				}
			}
			return ""
		},
		IsBuiltin: isBuiltinPython,
	})
}

// isPythonPublic returns true if the name is not prefixed with an underscore.
func isPythonPublic(name string) bool {
	return !strings.HasPrefix(name, "_")
}

// isPythonAllCaps returns true if name is a conventional Python constant
// (all uppercase letters, digits, and underscores, at least one letter).
func isPythonAllCaps(name string) bool {
	if name == "" {
		return false
	}
	hasLetter := false
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			hasLetter = true
		} else if r == '_' || (r >= '0' && r <= '9') {
			// allowed
		} else {
			return false
		}
	}
	return hasLetter
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
		"RuntimeError", "StopIteration", "NotImplementedError", "IOError",
		"input", "format", "round", "ord", "chr", "hex", "oct", "bin",
		"reversed", "frozenset", "memoryview", "bytearray", "complex",
		"delattr", "compile", "eval", "exec", "globals", "locals",
		"breakpoint", "help", "ascii", "pow", "divmod", "slice",
		"AttributeError", "FileNotFoundError", "PermissionError",
		"OSError", "ImportError", "ModuleNotFoundError":
		return true
	}
	return false
}
