package parser

import (
	"path/filepath"
	"strings"

	"github.com/alexaandru/go-sitter-forest/kotlin"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// KotlinParser parses Kotlin (.kt, .kts) source files.
type KotlinParser struct {
	language *sitter.Language
}

// NewKotlinParser creates a ready-to-use KotlinParser.
func NewKotlinParser() *KotlinParser {
	return &KotlinParser{language: sitter.NewLanguage(kotlin.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *KotlinParser) Extensions() []string {
	return []string{".kt", ".kts"}
}

func (p *KotlinParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// extractKotlinDeclInfo performs a pre-pass to collect metadata.
func extractKotlinDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "function_declaration":
			if nameNode := firstChildOfType(n, "simple_identifier"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				// Check for extension function receiver type.
				if recv := extractKotlinReceiverType(n, src); recv != "" {
					qualName = recv + "." + name
				} else if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"function_body", "block"}),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration", "object_declaration":
			className := ""
			if nameNode := firstChildOfType(n, "type_identifier"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			if className == "" {
				if nameNode := firstChildOfType(n, "simple_identifier"); !nameNode.IsNull() {
					className = string(src[nameNode.StartByte():nameNode.EndByte()])
				}
			}
			if className != "" {
				sl := int(n.StartPoint().Row) + 1
				result[className] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			// Walk body.
			if body := firstChildOfType(n, "class_body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), className)
				}
			}
			return
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
	return result
}

// isKotlinInterface checks if a class_declaration is actually an interface
// by looking for the "interface" keyword token among its children.
func isKotlinInterface(n sitter.Node, src []byte) bool {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		text := string(src[child.StartByte():child.EndByte()])
		if text == "interface" {
			return true
		}
		// Stop early if we hit the class name or body.
		if child.Type() == "type_identifier" || child.Type() == "class_body" {
			break
		}
	}
	return false
}

// isKotlinEnum checks for "enum" keyword.
func isKotlinEnum(n sitter.Node, src []byte) bool {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		text := string(src[child.StartByte():child.EndByte()])
		if text == "enum" {
			return true
		}
		if child.Type() == "type_identifier" || child.Type() == "class_body" {
			break
		}
	}
	return false
}

// isKotlinData checks for "data" keyword.
func isKotlinData(n sitter.Node, src []byte) bool {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		text := string(src[child.StartByte():child.EndByte()])
		if text == "data" {
			return true
		}
		if child.Type() == "type_identifier" || child.Type() == "class_body" {
			break
		}
	}
	return false
}

// isKotlinSealed checks for "sealed" keyword.
func isKotlinSealed(n sitter.Node, src []byte) bool {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		text := string(src[child.StartByte():child.EndByte()])
		if text == "sealed" {
			return true
		}
		if child.Type() == "type_identifier" || child.Type() == "class_body" {
			break
		}
	}
	return false
}

// extractKotlinReceiverType returns the receiver type name for a Kotlin extension
// function declaration (e.g. `fun String.foo()` → "String"), or "" if none.
// In the Kotlin tree-sitter grammar, the receiver type is a child of function_declaration
// whose type is "type_reference" and appears before the "simple_identifier" name node.
func extractKotlinReceiverType(n sitter.Node, src []byte) string {
	// Walk children of function_declaration in order.
	// If we see a receiver type BEFORE the simple_identifier,
	// it is the receiver type.
	//
	// Old grammar: type_reference / nullable_type / user_type appear directly.
	// New grammar (go-sitter-forest): a dedicated "receiver_type" wrapper node
	// contains the user_type / nullable_type child.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "receiver_type":
			// New grammar: unwrap the receiver_type node.
			return extractSimpleTypeName(child, src)
		case "type_reference", "nullable_type", "user_type":
			// Old grammar: receiver type appears directly.
			return extractSimpleTypeName(child, src)
		case "simple_identifier":
			// Reached the function name without seeing a receiver type.
			return ""
		}
	}
	return ""
}

// extractSimpleTypeName extracts the base type name from a type node.
func extractSimpleTypeName(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	// For user_type: contains type_identifier children.
	if ident := firstChildOfType(n, "type_identifier"); !ident.IsNull() {
		return string(src[ident.StartByte():ident.EndByte()])
	}
	// For simple type_reference: the text itself may be the identifier.
	text := strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
	// Strip generic parameters: String<T> → String
	if idx := strings.IndexByte(text, '<'); idx >= 0 {
		text = text[:idx]
	}
	// Strip nullable: String? → String
	text = strings.TrimSuffix(text, "?")
	return text
}

// Parse extracts code entities from a single Kotlin file.
func (p *KotlinParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lang := p.language
	declInfo := extractKotlinDeclInfo(root, src)

	// --- import directives ---
	importQuery := `(import_header (identifier) @import_path)`
	_ = runQuery(lang, root, src, importQuery, func(captures map[string]string, _ int) {
		importPath := captures["import_path"]
		if importPath == "" {
			return
		}
		importNodeID := g.MakeNodeID(importPath, importPath)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: importPath, Package: importPath, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- All declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectKotlinCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks the Kotlin AST.
func (p *KotlinParser) extractAllDeclarations(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "class_declaration":
			nameNode := firstChildOfType(n, "type_identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])

			// Determine the actual kind.
			nodeType := graph.NodeStruct
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string)
			}

			if isKotlinInterface(n, src) {
				nodeType = graph.NodeInterface
				meta["kind"] = "interface"
			} else if isKotlinEnum(n, src) {
				meta["kind"] = "enum"
			} else if isKotlinData(n, src) {
				meta["kind"] = "data"
			} else if isKotlinSealed(n, src) {
				meta["kind"] = "sealed"
			}

			// Heritage clause extraction: Kotlin uses delegation_specifier children.
			meta = extractKotlinHeritage(n, src, meta)
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: nodeType, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Walk primary_constructor for val/var constructor parameter properties.
			if ctor := firstChildOfType(n, "primary_constructor"); !ctor.IsNull() {
				for i := uint32(0); i < ctor.ChildCount(); i++ {
					walk(ctor.Child(i), name)
				}
			}
			// Walk body.
			if body := firstChildOfType(n, "class_body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "class_parameter":
			// Constructor parameter properties: class User(val name: String, var age: Int)
			// Only emit a node when the parameter has a val/var binding (it's a property).
			if enclosingClass == "" {
				break
			}
			hasBinding := false
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if !child.IsNull() && child.Type() == "binding_pattern_kind" {
					hasBinding = true
					break
				}
			}
			if !hasBinding {
				break
			}
			nameNode := firstChildOfType(n, "simple_identifier")
			if nameNode.IsNull() {
				break
			}
			propName := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := enclosingClass + "." + propName
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break
			}
			// Detect val vs var from binding_pattern_kind child text.
			kind := "val"
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "binding_pattern_kind" {
					text := string(src[child.StartByte():child.EndByte()])
					if text == "var" {
						kind = "var"
					}
					break
				}
			}
			ctorMeta := map[string]string{"kind": kind}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(propName), Metadata: ctorMeta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			ctorClassID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(ctorClassID) != nil {
				g.AddEdge(&graph.Edge{From: ctorClassID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "companion_object":
			// companion object { fun create(): Foo = Foo() }
			// Walk companion body with enclosing class context so methods
			// are qualified as ClassName.methodName.
			if enclosingClass == "" {
				break
			}
			if body := firstChildOfType(n, "class_body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), enclosingClass)
				}
			}
			return

		case "object_declaration":
			nameNode := firstChildOfType(n, "type_identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "object"
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := firstChildOfType(n, "class_body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "function_declaration":
			nameNode := firstChildOfType(n, "simple_identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			nodeType := graph.NodeFunction
			var meta map[string]string

			// Check for extension function: fun ReceiverType.funcName()
			// In the Kotlin grammar, the receiver type appears as a child before
			// the function name. We look for a "type_reference" or "nullable_type"
			// that precedes the simple_identifier in the function declaration.
			receiverType := extractKotlinReceiverType(n, src)
			if receiverType != "" {
				// Extension function: name it as "ReceiverType.funcName"
				qualName = receiverType + "." + name
				nodeType = graph.NodeFunction
				meta = map[string]string{"kind": "extension"}
			} else if enclosingClass != "" {
				qualName = enclosingClass + "." + name
				nodeType = graph.NodeMethod
			}

			if meta == nil {
				meta = buildLangMeta(declInfo[qualName])
			} else {
				// Merge with declInfo metadata
				if dm := buildLangMeta(declInfo[qualName]); dm != nil {
					for k, v := range dm {
						meta[k] = v
					}
				}
			}

			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: nodeType, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" && receiverType == "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "type_alias":
			// typealias Callback = (String) -> Unit
			nameNode := firstChildOfType(n, "type_identifier")
			if nameNode.IsNull() {
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

		case "property_declaration":
			// Kotlin class val/var properties: val name: String, var status: String
			if enclosingClass == "" {
				break
			}
			// The property name is: variable_declaration → simple_identifier
			var propName string
			if varDecl := firstChildOfType(n, "variable_declaration"); !varDecl.IsNull() {
				if nameNode := firstChildOfType(varDecl, "simple_identifier"); !nameNode.IsNull() {
					propName = string(src[nameNode.StartByte():nameNode.EndByte()])
				}
			}
			// Fallback: direct simple_identifier child (multivar_declaration pattern)
			if propName == "" {
				if nameNode := firstChildOfType(n, "simple_identifier"); !nameNode.IsNull() {
					propName = string(src[nameNode.StartByte():nameNode.EndByte()])
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
			// Determine val vs var from first child keyword.
			kind := "val"
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				text := string(src[child.StartByte():child.EndByte()])
				if text == "var" {
					kind = "var"
					break
				}
				if text == "val" {
					break
				}
			}
			meta := map[string]string{"kind": kind}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isExported(propName),
				Metadata: meta,
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

// isKotlinBuiltinType returns true for Kotlin stdlib types that should not
// generate varType entries (they have no user-defined methods to resolve).
func isKotlinBuiltinType(name string) bool {
	switch name {
	case "String", "Int", "Long", "Boolean", "Float", "Double", "Short", "Byte", "Char",
		"List", "Map", "Set", "Array", "MutableList", "MutableMap", "MutableSet",
		"Any", "Unit", "Nothing", "Pair", "Triple",
		"Number", "Comparable", "Iterable", "Sequence", "Collection",
		"HashMap", "HashSet", "ArrayList", "LinkedHashMap", "LinkedHashSet":
		return true
	}
	return false
}

// extractKotlinTypeName extracts the type name from a Kotlin type annotation node.
// Handles user_type, nullable_type (unwraps), and generic_type (takes base).
func extractKotlinTypeName(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	switch n.Type() {
	case "nullable_type":
		// Unwrap: String? → String, Repository? → Repository
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if !child.IsNull() && child.Type() == "user_type" {
				return extractKotlinTypeName(child, src)
			}
		}
		return ""
	case "user_type":
		// user_type contains type_identifier (or simple_identifier in some grammar versions)
		if ident := firstChildOfType(n, "type_identifier"); !ident.IsNull() {
			return string(src[ident.StartByte():ident.EndByte()])
		}
		if ident := firstChildOfType(n, "simple_identifier"); !ident.IsNull() {
			return string(src[ident.StartByte():ident.EndByte()])
		}
		// Fallback: strip generics and nullable
		text := strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
		if idx := strings.IndexByte(text, '<'); idx >= 0 {
			text = text[:idx]
		}
		text = strings.TrimSuffix(text, "?")
		return text
	default:
		// For type_reference or other wrappers, try to find user_type child
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if !child.IsNull() && (child.Type() == "user_type" || child.Type() == "nullable_type") {
				return extractKotlinTypeName(child, src)
			}
		}
		return ""
	}
}

// collectKotlinVarTypes walks the AST to extract variable → type mappings from:
//   - Function parameters: fun process(repo: Repository) → repo→Repository
//   - Property declarations: val service: AuthService → service→AuthService
//   - Constructor parameters: class Foo(val name: String) → name→String (skips builtins)
func collectKotlinVarTypes(g *graph.Graph, root sitter.Node, src []byte, filePath string) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "function_declaration":
			// Extract typed parameters from function_value_parameters
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() || child.Type() != "function_value_parameters" {
					continue
				}
				for j := uint32(0); j < child.ChildCount(); j++ {
					param := child.Child(j)
					if param.IsNull() {
						continue
					}
					// Grammar: function_value_parameters contains "parameter" children directly
					if param.Type() != "parameter" && param.Type() != "function_value_parameter" {
						continue
					}
					// If function_value_parameter, unwrap to parameter child
					paramNode := param
					if param.Type() == "function_value_parameter" {
						if inner := firstChildOfType(param, "parameter"); !inner.IsNull() {
							paramNode = inner
						}
					}
					nameNode := firstChildOfType(paramNode, "simple_identifier")
					if nameNode.IsNull() {
						continue
					}
					varName := string(src[nameNode.StartByte():nameNode.EndByte()])

					// Find the type annotation (user_type or nullable_type)
					typeName := ""
					for k := uint32(0); k < paramNode.ChildCount(); k++ {
						tc := paramNode.Child(k)
						if tc.IsNull() {
							continue
						}
						if tc.Type() == "user_type" || tc.Type() == "nullable_type" {
							typeName = extractKotlinTypeName(tc, src)
							break
						}
					}
					if typeName == "" || isKotlinBuiltinType(typeName) {
						continue
					}
					g.AddVarType(filePath, varName, typeName)
				}
			}

		case "property_declaration":
			// val service: AuthService or var repo: Repository
			var varName string
			if varDecl := firstChildOfType(n, "variable_declaration"); !varDecl.IsNull() {
				if nameNode := firstChildOfType(varDecl, "simple_identifier"); !nameNode.IsNull() {
					varName = string(src[nameNode.StartByte():nameNode.EndByte()])
				}
				// Type annotation is on variable_declaration
				typeName := ""
				for k := uint32(0); k < varDecl.ChildCount(); k++ {
					tc := varDecl.Child(k)
					if tc.IsNull() {
						continue
					}
					if tc.Type() == "user_type" || tc.Type() == "nullable_type" {
						typeName = extractKotlinTypeName(tc, src)
						break
					}
				}
				if varName != "" && typeName != "" && !isKotlinBuiltinType(typeName) {
					g.AddVarType(filePath, varName, typeName)
				}
			}

		case "class_parameter":
			// Constructor parameter: class Foo(val name: String)
			nameNode := firstChildOfType(n, "simple_identifier")
			if nameNode.IsNull() {
				break
			}
			varName := string(src[nameNode.StartByte():nameNode.EndByte()])
			typeName := ""
			for k := uint32(0); k < n.ChildCount(); k++ {
				tc := n.Child(k)
				if tc.IsNull() {
					continue
				}
				if tc.Type() == "user_type" || tc.Type() == "nullable_type" {
					typeName = extractKotlinTypeName(tc, src)
					break
				}
			}
			if typeName == "" || isKotlinBuiltinType(typeName) {
				break
			}
			g.AddVarType(filePath, varName, typeName)
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// collectKotlinCallSites collects call sites.
func collectKotlinCallSites(g *graph.Graph, lang *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Collect variable type declarations for cross-file obj.method() resolution.
	collectKotlinVarTypes(g, root, src, filePath)
	callQuery := `(call_expression (simple_identifier) @callee)`
	_ = runQuery(lang, root, src, callQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || isKotlinBuiltin(callee) {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
	})
}

// extractKotlinHeritage extracts supertypes from a Kotlin class_declaration.
// Kotlin uses delegation_specifier children (or a delegation_specifier_list)
// for both extends and implements, e.g. `class Foo : Bar(), IBaz`.
// Uses extractTypeIdentifiers to correctly handle constructor invocations
// (Bar() → "Bar", not "Bar()") and generic types (Comparable<T> → "Comparable").
func extractKotlinHeritage(n sitter.Node, src []byte, meta map[string]string) map[string]string {
	var names []string
	seen := make(map[string]bool)

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() != "delegation_specifier" && child.Type() != "delegation_specifier_list" {
			continue
		}
		// extractTypeIdentifiers recursively walks the AST subtree for
		// type_identifier and generic_type nodes, correctly handling
		// constructor_invocation → user_type → type_identifier paths.
		for _, name := range extractTypeIdentifiers(child, src) {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}

	if len(names) == 0 {
		return meta
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	meta["heritage_implements"] = strings.Join(names, ",")
	return meta
}

func isKotlinBuiltin(name string) bool {
	switch name {
	case "println", "print", "readLine", "TODO", "error", "require", "check",
		"listOf", "mutableListOf", "mapOf", "mutableMapOf", "setOf", "mutableSetOf",
		"arrayOf", "intArrayOf", "longArrayOf", "floatArrayOf", "doubleArrayOf",
		"lazy", "run", "let", "also", "apply", "with", "repeat",
		"String", "Int", "Long", "Double", "Float", "Boolean", "Char", "Byte", "Short",
		"Any", "Unit", "Nothing", "Pair", "Triple",
		"emptyList", "emptyMap", "emptySet",
		"maxOf", "minOf", "to", "until", "downTo", "step":
		return true
	}
	return false
}
