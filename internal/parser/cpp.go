package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/cpp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// extractCppDeclInfo walks the C++ AST collecting metadata for all declarations.
func extractCppDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "function_definition":
			if name := extractCppFuncName(n, src); name != "" {
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature:  extractSigToBodyMulti(n, src, []string{"compound_statement", "field_initializer_list"}),
					Doc:        extractDocMulti(lines, sl, "//"),
					LineCount:  int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
					IfdefGuard: extractCIfdefGuard(n, src),
				}
			}
		case "class_specifier", "struct_specifier":
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[className] = declMeta{
					Doc:        extractDocMulti(lines, sl, "//"),
					LineCount:  int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
					IfdefGuard: extractCIfdefGuard(n, src),
				}
			}
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), className)
				}
			}
			return
		case "enum_specifier":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:        extractDocMulti(lines, sl, "//"),
					LineCount:  int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
					IfdefGuard: extractCIfdefGuard(n, src),
				}
			}
		case "namespace_definition":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
	return result
}

// extractCppFuncName extracts the function name from a C++ function definition,
// handling scope-qualified names (Foo::bar) and regular names.
func extractCppFuncName(n sitter.Node, src []byte) string {
	declarator := n.ChildByFieldName("declarator")
	if declarator.IsNull() {
		return ""
	}
	// Walk through layers: function_declarator wrapping identifier or qualified_identifier.
	return extractNameFromDeclarator(declarator, src)
}

// extractNameFromDeclarator recursively extracts the identifier name from a C/C++ declarator.
func extractNameFromDeclarator(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	switch n.Type() {
	case "identifier", "field_identifier", "destructor_name", "operator_name":
		return string(src[n.StartByte():n.EndByte()])
	case "qualified_identifier":
		// Foo::bar → return "bar" (the name part).
		nameNode := n.ChildByFieldName("name")
		if !nameNode.IsNull() {
			return string(src[nameNode.StartByte():nameNode.EndByte()])
		}
	case "function_declarator":
		inner := n.ChildByFieldName("declarator")
		return extractNameFromDeclarator(inner, src)
	case "reference_declarator", "pointer_declarator":
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if !child.IsNull() && child.Type() != "*" && child.Type() != "&" {
				if name := extractNameFromDeclarator(child, src); name != "" {
					return name
				}
			}
		}
	case "template_function":
		nameNode := n.ChildByFieldName("name")
		return extractNameFromDeclarator(nameNode, src)
	}
	return ""
}

// extractCppScopeQualifier extracts the scope qualifier (class/namespace) from a
// qualified function definition like Foo::bar().
func extractCppScopeQualifier(n sitter.Node, src []byte) string {
	declarator := n.ChildByFieldName("declarator")
	if declarator.IsNull() {
		return ""
	}
	if declarator.Type() == "function_declarator" {
		inner := declarator.ChildByFieldName("declarator")
		if !inner.IsNull() && inner.Type() == "qualified_identifier" {
			scope := inner.ChildByFieldName("scope")
			if !scope.IsNull() {
				text := string(src[scope.StartByte():scope.EndByte()])
				// Remove trailing ::
				text = strings.TrimSuffix(text, "::")
				return text
			}
		}
	}
	return ""
}

// CppParser parses C++ source files.
type CppParser struct {
	language *sitter.Language
}

// NewCppParser creates a ready-to-use CppParser.
func NewCppParser() *CppParser {
	return &CppParser{language: sitter.NewLanguage(cpp.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *CppParser) Extensions() []string {
	return []string{".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".mm"}
}

// Parse extracts code entities from a single C++ file and merges them into the graph.
func (p *CppParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
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
	declInfo := extractCppDeclInfo(root, src)

	// --- #include directives ---
	includeQuery := `(preproc_include path: (string_literal) @include_path)`
	if err := runQuery(lang, root, src, includeQuery, func(captures map[string]string, _ int) {
		path := captures["include_path"]
		if path == "" {
			return
		}
		importNodeID := g.MakeNodeID(path, path)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: path, Package: path, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	sysIncludeQuery := `(preproc_include path: (system_lib_string) @include_path)`
	if err := runQuery(lang, root, src, sysIncludeQuery, func(captures map[string]string, _ int) {
		path := captures["include_path"]
		if path == "" {
			return
		}
		importNodeID := g.MakeNodeID(path, path)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: path, Package: path, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- All declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectCppCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks the C++ AST extracting all declarations.
func (p *CppParser) extractAllDeclarations(
	g *graph.Graph,
	root sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "function_definition":
			name := extractCppFuncName(n, src)
			if name == "" {
				break
			}
			// Check for scope-qualified: Foo::bar()
			scope := extractCppScopeQualifier(n, src)
			qualName := name
			nodeType := graph.NodeFunction
			if enclosingClass != "" {
				qualName = enclosingClass + "." + name
				nodeType = graph.NodeMethod
			} else if scope != "" {
				qualName = scope + "." + name
				nodeType = graph.NodeMethod
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     nodeType,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true,
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Link to enclosing class if present.
			if enclosingClass != "" || scope != "" {
				ownerName := enclosingClass
				if ownerName == "" {
					ownerName = scope
				}
				ownerID := g.MakeNodeID(filePath, ownerName)
				if g.GetNode(ownerID) != nil {
					g.AddEdge(&graph.Edge{From: ownerID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "class_specifier":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isExported(name),
				Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "struct_specifier":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) != nil {
				break
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true,
				Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "enum_specifier":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) != nil {
				break
			}
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "enum"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "namespace_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodePackage,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true,
				Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "type_alias_declaration", "alias_declaration":
			// using MyType = std::vector<int>;
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) != nil {
				break
			}
			meta := map[string]string{"kind": "type_alias"}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeInterface,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isExported(name),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "template_declaration":
			// Walk into the template's body to find the actual declaration.
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if !child.IsNull() && child.Type() != "template_parameter_list" {
					walk(child, enclosingClass)
				}
			}
			return

		case "declaration":
			// Function prototypes: void foo(int x);
			declarator := n.ChildByFieldName("declarator")
			if !declarator.IsNull() && declarator.Type() == "function_declarator" {
				name := extractNameFromDeclarator(declarator, src)
				if name != "" {
					nodeID := g.MakeNodeID(filePath, name)
					if g.GetNode(nodeID) == nil {
						g.AddNode(&graph.Node{
							ID:       nodeID,
							Type:     graph.NodeFunction,
							Name:     name,
							File:     filePath,
							Line:     int(n.StartPoint().Row) + 1,
							Exported: true,
						})
						g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectCppCallSites performs function-level call site collection for C++,
// attributing each call to its enclosing function/method node.
func collectCppCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{
			"class_specifier":  true,
			"struct_specifier": true,
		},
		FuncTypes: map[string]bool{
			"function_definition": true,
		},
		CallTypes: map[string]bool{
			"call_expression": true,
		},
		NameExtractor: func(n sitter.Node, src []byte) string {
			switch n.Type() {
			case "class_specifier", "struct_specifier":
				if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
					return string(src[nameNode.StartByte():nameNode.EndByte()])
				}
			case "function_definition":
				name := extractCppFuncName(n, src)
				// For top-level scope-qualified definitions like Foo::bar(),
				// include the scope so the name matches the graph node "Foo.bar".
				if scope := extractCppScopeQualifier(n, src); scope != "" {
					return scope + "." + name
				}
				return name
			}
			return ""
		},
		CalleeExtractor: func(n sitter.Node, src []byte) string {
			fn := n.ChildByFieldName("function")
			if fn.IsNull() {
				return ""
			}
			switch fn.Type() {
			case "identifier":
				return string(src[fn.StartByte():fn.EndByte()])
			case "field_expression":
				if field := fn.ChildByFieldName("field"); !field.IsNull() {
					return string(src[field.StartByte():field.EndByte()])
				}
			case "qualified_identifier":
				if nameNode := fn.ChildByFieldName("name"); !nameNode.IsNull() {
					return string(src[nameNode.StartByte():nameNode.EndByte()])
				}
			}
			return ""
		},
		IsBuiltin: isCBuiltin,
	})
}
