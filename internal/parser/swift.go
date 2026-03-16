package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/swift"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// SwiftParser parses Swift (.swift) source files.
type SwiftParser struct {
	language *sitter.Language
}

// NewSwiftParser creates a ready-to-use SwiftParser.
func NewSwiftParser() *SwiftParser {
	return &SwiftParser{language: swift.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *SwiftParser) Extensions() []string {
	return []string{".swift"}
}

// extractSwiftDeclInfo performs a pre-pass over the AST to collect metadata.
func extractSwiftDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n *sitter.Node, enclosingClass string)
	walk = func(n *sitter.Node, enclosingClass string) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "function_declaration":
			if nameNode := firstChildOfType(n, "simple_identifier"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"function_body", "code_block"}),
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration":
			name := extractSwiftTypeName(n, src)
			if name != "" {
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
				// Walk body with class context.
				for i := 0; i < int(n.ChildCount()); i++ {
					child := n.Child(i)
					if child != nil && (child.Type() == "class_body" || child.Type() == "enum_class_body") {
						for j := 0; j < int(child.ChildCount()); j++ {
							walk(child.Child(j), name)
						}
					}
				}
				return
			}
		case "protocol_declaration":
			if nameNode := firstChildOfType(n, "type_identifier"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
	return result
}

// swiftDeclKind determines the kind of a Swift class_declaration by inspecting
// its keyword child token. In the Swift tree-sitter grammar, structs, enums,
// classes, actors, and extensions are all represented as class_declaration nodes.
func swiftDeclKind(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		text := string(src[child.StartByte():child.EndByte()])
		switch text {
		case "struct", "class", "enum", "actor", "extension":
			return text
		}
	}
	return "class"
}

// Parse extracts code entities from a single Swift file and merges them into the graph.
func (p *SwiftParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractSwiftDeclInfo(root, src)

	// --- import declarations ---
	importQuery := `(import_declaration (identifier) @import_name)`
	_ = runQuery(lang, root, src, importQuery, func(captures map[string]string, _ int) {
		name := captures["import_name"]
		if name == "" {
			return
		}
		importNodeID := g.MakeNodeID(name, name)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: name, Package: name, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- All declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectSwiftCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractSwiftTypeName extracts the type name from a Swift class_declaration.
// For regular struct/class/enum/actor: the type_identifier is a direct child.
// For extension declarations: the type_identifier is wrapped in user_type.
func extractSwiftTypeName(n *sitter.Node, src []byte) string {
	if nameNode := firstChildOfType(n, "type_identifier"); nameNode != nil {
		return string(src[nameNode.StartByte():nameNode.EndByte()])
	}
	// Extensions use: user_type → type_identifier
	if utNode := firstChildOfType(n, "user_type"); utNode != nil {
		if nameNode := firstChildOfType(utNode, "type_identifier"); nameNode != nil {
			return string(src[nameNode.StartByte():nameNode.EndByte()])
		}
	}
	return ""
}

// extractAllDeclarations walks the Swift AST for all declarations.
func (p *SwiftParser) extractAllDeclarations(
	g *graph.Graph, root *sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta,
) {
	var walk func(n *sitter.Node, enclosingClass string)
	walk = func(n *sitter.Node, enclosingClass string) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "class_declaration":
			name := extractSwiftTypeName(n, src)
			if name == "" {
				break
			}
			kind := swiftDeclKind(n, src)

			nodeType := graph.NodeStruct
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = kind

			if kind == "extension" {
				// Extensions add methods to existing types — don't create a new type node,
				// but walk body with the extended type as context.
				for i := 0; i < int(n.ChildCount()); i++ {
					child := n.Child(i)
					if child != nil && (child.Type() == "class_body" || child.Type() == "enum_class_body") {
						for j := 0; j < int(child.ChildCount()); j++ {
							walk(child.Child(j), name)
						}
					}
				}
				return
			}

			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: nodeType, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Walk body for methods.
			for i := 0; i < int(n.ChildCount()); i++ {
				child := n.Child(i)
				if child != nil && (child.Type() == "class_body" || child.Type() == "enum_class_body") {
					for j := 0; j < int(child.ChildCount()); j++ {
						walk(child.Child(j), name)
					}
				}
			}
			return

		case "protocol_declaration":
			nameNode := firstChildOfType(n, "type_identifier")
			if nameNode == nil {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeInterface, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "typealias_declaration":
			// typealias MyType = SomeType — emit as NodeInterface with kind="typealias"
			nameNode := firstChildOfType(n, "type_identifier")
			if nameNode == nil {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) != nil {
				break
			}
			meta := map[string]string{"kind": "typealias"}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeInterface, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "function_declaration":
			nameNode := firstChildOfType(n, "simple_identifier")
			if nameNode == nil {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			nodeType := graph.NodeFunction
			if enclosingClass != "" {
				qualName = enclosingClass + "." + name
				nodeType = graph.NodeMethod
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: nodeType, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "init_declaration":
			if enclosingClass == "" {
				break
			}
			qualName := enclosingClass + ".init"
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: true,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "deinit_declaration":
			if enclosingClass == "" {
				break
			}
			qualName := enclosingClass + ".deinit"
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: false,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "subscript_declaration":
			// subscript(index: Int) -> T { ... }
			if enclosingClass == "" {
				break
			}
			qualName := enclosingClass + ".subscript"
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break // only one subscript node per type
			}
			meta := map[string]string{"kind": "subscript"}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(enclosingClass),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "computed_property":
			// var myProp: Int { return 42 } — computed property shorthand
			if enclosingClass == "" {
				break
			}
			// The pattern (name) is a child; try value_binding_pattern → pattern
			var propName string
			for i := 0; i < int(n.ChildCount()); i++ {
				child := n.Child(i)
				if child == nil {
					continue
				}
				if child.Type() == "pattern" || child.Type() == "value_binding_pattern" {
					// Dig into the pattern for the identifier
					for j := 0; j < int(child.ChildCount()); j++ {
						c := child.Child(j)
						if c == nil {
							continue
						}
						if c.Type() == "simple_identifier" || c.Type() == "identifier" {
							propName = string(src[c.StartByte():c.EndByte()])
							break
						}
					}
				} else if child.Type() == "simple_identifier" || child.Type() == "identifier" {
					propName = string(src[child.StartByte():child.EndByte()])
				}
				if propName != "" {
					break
				}
			}
			if propName == "" {
				break
			}
			qualName := enclosingClass + "." + propName
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break
			}
			meta := map[string]string{"kind": "property"}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(propName),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
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

// collectSwiftCallSites collects call sites from Swift source.
func collectSwiftCallSites(g *graph.Graph, lang *sitter.Language, root *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Direct calls: foo(...)
	callQuery := `(call_expression (simple_identifier) @callee)`
	_ = runQuery(lang, root, src, callQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || isSwiftBuiltin(callee) {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
	})
}

func isSwiftBuiltin(name string) bool {
	switch name {
	case "print", "debugPrint", "dump", "fatalError", "precondition", "assert",
		"min", "max", "abs", "stride", "zip", "type", "unsafeBitCast",
		"withUnsafePointer", "withUnsafeMutablePointer",
		"String", "Int", "Double", "Float", "Bool", "Array", "Dictionary", "Set",
		"Optional", "Result", "Error", "Codable", "Hashable", "Equatable":
		return true
	}
	return false
}
