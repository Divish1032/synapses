package parser

import (
	"path/filepath"
	"strings"

	objcg "github.com/alexaandru/go-sitter-forest/objc"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ObjCParser parses Objective-C (.m, .h) source files.
//
// Extracted entities:
//   - @interface blocks → NodeStruct (classes) with superclass and protocol metadata
//   - Method declarations inside @interface → NodeMethod with selector and scope metadata
//   - @property declarations inside @interface → NodeMethod (kind=property)
//   - @protocol declarations → NodeInterface
//   - Method declarations inside @protocol → NodeMethod
//   - @implementation blocks → NodeStruct (if not already from @interface) + methods
//   - #import / #include directives → NodePackage with EdgeImports
//
// NS_ENUM / typedef enum are not extracted (complex to parse cleanly from tree-sitter).
type ObjCParser struct {
	language *sitter.Language
}

// NewObjCParser creates a ready-to-use ObjCParser.
func NewObjCParser() *ObjCParser {
	return &ObjCParser{language: sitter.NewLanguage(objcg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *ObjCParser) Extensions() []string {
	return []string{".m", ".h"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *ObjCParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single Objective-C file and merges them into the graph.
func (p *ObjCParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// Walk top-level children of translation_unit.
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "preproc_include":
			p.extractImport(g, child, src, filePath, fileNodeID)
		case "class_interface":
			p.extractClassInterface(g, child, src, filePath, fileNodeID)
		case "protocol_declaration":
			p.extractProtocol(g, child, src, filePath, fileNodeID)
		case "category_interface":
			// @interface ClassName (CategoryName) — extract methods but link to class
			p.extractClassInterface(g, child, src, filePath, fileNodeID)
		case "class_implementation":
			p.extractImplementation(g, child, src, filePath, fileNodeID)
		case "category_implementation":
			p.extractImplementation(g, child, src, filePath, fileNodeID)
		}
	}

	return nil
}

// extractImport handles preproc_include nodes (#import / #include).
// Creates a NodePackage and an EdgeImports from the file to the import.
func (p *ObjCParser) extractImport(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		var importPath string
		switch child.Type() {
		case "system_lib_string":
			// <Foundation/Foundation.h> — strip angle brackets
			raw := string(src[child.StartByte():child.EndByte()])
			importPath = strings.Trim(raw, "<>")
		case "string_literal":
			// "MyClass.h" — strip quotes, collect string_content children
			for j := uint32(0); j < child.ChildCount(); j++ {
				sc := child.Child(j)
				if !sc.IsNull() && sc.Type() == "string_content" {
					importPath = string(src[sc.StartByte():sc.EndByte()])
					break
				}
			}
		}
		if importPath != "" {
			importNodeID := g.MakeNodeID(importPath, importPath)
			g.AddNode(&graph.Node{
				ID:      importNodeID,
				Type:    graph.NodePackage,
				Name:    importPath,
				Package: importPath,
				File:    filePath,
				Line:    int(n.StartPoint().Row) + 1,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
			return
		}
	}
}

// extractClassInterface handles @interface and @interface (Category) nodes.
// For regular @interface: extracts the class as NodeStruct plus methods and properties.
// For category @interface: extracts only the methods, qualified with the class name.
func (p *ObjCParser) extractClassInterface(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Determine the class name and whether this is a category.
	// AST structure:
	//   @interface ClassName : SuperClass <Protocols>  → regular interface
	//   @interface ClassName (CategoryName)             → category
	//
	// The first identifier child is always the class name.
	// A "(" child indicates category. A ":" child indicates superclass.

	var className, superclass string
	var protocols []string
	isCategory := false

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			if className == "" {
				className = string(src[child.StartByte():child.EndByte()])
			} else if superclass == "" && !isCategory {
				// The second identifier after ":" is the superclass.
				// We check that the previous sibling was ":".
				// Simpler: after we see ":", the next identifier is superclass.
				// We handle this by tracking state below.
			}
		case "(":
			isCategory = true
		case ":":
			// Next identifier will be superclass — handled via position tracking.
		case "parameterized_arguments":
			// <NSCopying, NSCoding> — extract protocol names.
			for j := uint32(0); j < child.ChildCount(); j++ {
				pc := child.Child(j)
				if pc.IsNull() {
					continue
				}
				// tree-sitter wraps each in type_name → type_identifier
				if pc.Type() == "type_name" {
					if ti := firstChildOfType(pc, "type_identifier"); !ti.IsNull() {
						protocols = append(protocols, string(src[ti.StartByte():ti.EndByte()]))
					}
				}
			}
		}
	}

	// Re-scan to capture superclass: it's the identifier after ":"
	seenColon := false
	seenClassName := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			if !seenClassName {
				seenClassName = true
				// This is the class name — already captured.
			} else if seenColon && superclass == "" {
				superclass = string(src[child.StartByte():child.EndByte()])
			}
		case ":":
			seenColon = true
		case "(":
			// Stop scanning for superclass in category declarations.
			seenColon = false
		}
	}

	if className == "" {
		return
	}

	// Emit the class node only for non-category @interface.
	if !isCategory {
		meta := make(map[string]string, 3)
		if superclass != "" {
			meta["superclass"] = superclass
		}
		if len(protocols) > 0 {
			meta["protocols"] = strings.Join(protocols, ", ")
		}
		classNodeID := g.MakeNodeID(filePath, className)
		g.AddNode(&graph.Node{
			ID:       classNodeID,
			Type:     graph.NodeStruct,
			Name:     className,
			File:     filePath,
			Line:     int(n.StartPoint().Row) + 1,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: classNodeID, Type: graph.EdgeDefines})
	}

	// Walk children to extract method_declaration and property_declaration nodes.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "method_declaration":
			p.extractMethod(g, child, src, filePath, fileNodeID, className)
		case "property_declaration":
			p.extractProperty(g, child, src, filePath, fileNodeID, className)
		}
	}
}

// extractProtocol handles @protocol declarations.
// Emits a NodeInterface and extracts method_declaration children as NodeMethod.
func (p *ObjCParser) extractProtocol(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// First identifier child = protocol name.
	protocolName := ""
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "identifier" {
			protocolName = string(src[child.StartByte():child.EndByte()])
			break
		}
	}
	if protocolName == "" {
		return
	}

	// Collect parent protocols from protocol_reference_list.
	var parentProtocols []string
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "protocol_reference_list" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			pc := child.Child(j)
			if !pc.IsNull() && pc.Type() == "identifier" {
				parentProtocols = append(parentProtocols, string(src[pc.StartByte():pc.EndByte()]))
			}
		}
	}

	meta := make(map[string]string, 2)
	meta["kind"] = "protocol"
	if len(parentProtocols) > 0 {
		meta["protocols"] = strings.Join(parentProtocols, ", ")
	}

	protoNodeID := g.MakeNodeID(filePath, protocolName)
	g.AddNode(&graph.Node{
		ID:       protoNodeID,
		Type:     graph.NodeInterface,
		Name:     protocolName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: protoNodeID, Type: graph.EdgeDefines})

	// Extract method declarations — may be direct children or nested inside
	// qualified_protocol_interface_declaration or other container nodes.
	p.extractProtocolMethods(g, n, src, filePath, fileNodeID, protocolName)
}

// extractProtocolMethods recursively walks protocol children to find method_declaration
// and property_declaration nodes, which may be nested inside container nodes like
// qualified_protocol_interface_declaration.
func (p *ObjCParser) extractProtocolMethods(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, protocolName string) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "method_declaration":
			p.extractMethod(g, child, src, filePath, fileNodeID, protocolName)
		case "property_declaration":
			p.extractProperty(g, child, src, filePath, fileNodeID, protocolName)
		default:
			// Recurse into container nodes (e.g., qualified_protocol_interface_declaration,
			// optional, required sections).
			if child.ChildCount() > 0 && child.Type() != "identifier" && child.Type() != "protocol_reference_list" {
				p.extractProtocolMethods(g, child, src, filePath, fileNodeID, protocolName)
			}
		}
	}
}

// extractMethod handles method_declaration nodes inside @interface or @protocol.
// Constructs the ObjC selector from the declaration and emits a NodeMethod.
//
// ObjC selector construction rules:
//   - Simple method: - (ReturnType)methodName;  → selector = "methodName"
//   - Multi-part:    - (ReturnType)initWithName:(NSString *)n count:(int)c; → selector = "initWithName:count:"
//
// The method_declaration structure is:
//
//	[-/+] [method_type] [identifier] ([identifier] [method_parameter])*  [;]
//
// The selector is built from:
//
//  1. The first identifier child (first part of selector).
//
//  2. For each method_parameter: its leading identifier child (keyword label before ":").
//     Wait — re-reading the AST: the grammar puts the keyword before ":" directly in the
//     method_declaration, NOT inside method_parameter. Let's look at the probe output:
//
//     [method_declaration]
//     [-]
//     [method_type] → (instancetype)
//     [identifier] → "initWithName"    ← first keyword
//     [method_parameter]
//     [:] → ":"
//     [method_type] → (NSString *)
//     [identifier] → "name"          ← param name
//     [identifier] → "count"           ← second keyword (between method_parameters)
//     [method_parameter]
//     [:] → ":"
//     [method_type] → (NSInteger)
//     [identifier] → "count"         ← param name
//
// So the selector parts are identifiers that PRECEDE each method_parameter [:].
// We need to collect: identifiers that come BEFORE a method_parameter in the
// method_declaration children list.
func (p *ObjCParser) extractMethod(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, enclosingClass string) {
	// Determine instance (-) or class (+) method.
	scope := "instance"
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "+" {
			scope = "class"
			break
		}
		if child.Type() == "-" {
			break
		}
	}

	// Extract return type from method_type child.
	returnType := ""
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "method_type" {
			continue
		}
		// method_type contains (type_name) — get all text between parens.
		returnType = strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
		// Strip outer parens: "(ReturnType)" → "ReturnType"
		returnType = strings.TrimPrefix(returnType, "(")
		returnType = strings.TrimSuffix(returnType, ")")
		returnType = strings.TrimSpace(returnType)
		break
	}

	// Build the selector by collecting identifiers before each method_parameter.
	// The pattern in the AST:
	//   identifier  → first keyword (always present)
	//   identifier  → subsequent keyword (appears between method_parameters)
	//   method_parameter → ":"  method_type  identifier(param_name)
	//
	// Selector = first_keyword + (for each method_parameter: keyword + ":").
	// For multi-part selectors we accumulate keywords.
	var selectorParts []string
	pendingIdent := "" // identifier seen since last method_parameter

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			pendingIdent = string(src[child.StartByte():child.EndByte()])
		case "method_parameter":
			// The keyword before this ":" is pendingIdent.
			if pendingIdent != "" {
				selectorParts = append(selectorParts, pendingIdent+":")
				pendingIdent = ""
			}
		}
	}

	// If no method_parameter was found, it's a simple method with no parameters.
	if len(selectorParts) == 0 && pendingIdent != "" {
		selectorParts = []string{pendingIdent}
	}

	if len(selectorParts) == 0 {
		return
	}

	selector := strings.Join(selectorParts, "")
	qualName := enclosingClass + "." + selector

	meta := map[string]string{
		"scope": scope,
	}
	if returnType != "" {
		meta["return_type"] = returnType
	}

	methodNodeID := g.MakeNodeID(filePath, qualName)
	g.AddNode(&graph.Node{
		ID:       methodNodeID,
		Type:     graph.NodeMethod,
		Name:     qualName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: methodNodeID, Type: graph.EdgeDefines})
	// Link class/protocol → method.
	classNodeID := g.MakeNodeID(filePath, enclosingClass)
	if g.GetNode(classNodeID) != nil {
		g.AddEdge(&graph.Edge{From: classNodeID, To: methodNodeID, Type: graph.EdgeDefines})
	}
}

// extractProperty handles @property declarations inside @interface.
// Emits a NodeMethod (kind=property) qualified as ClassName.propertyName.
//
// AST structure for @property:
//
//	[property_declaration]
//	  [@property]
//	  [property_attributes_declaration] → (nonatomic, strong)
//	  [struct_declaration]
//	    [type_identifier] → "NSString"
//	    [struct_declarator]
//	      [pointer_declarator]
//	        [*]
//	        [identifier] → "name"    ← property name
//	    OR
//	    [struct_declarator]
//	      [identifier] → "count"     ← property name (no pointer)
func (p *ObjCParser) extractProperty(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, enclosingClass string) {
	// Collect property attributes text.
	attrText := ""
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "property_attributes_declaration" {
			attrText = string(src[child.StartByte():child.EndByte()])
			break
		}
	}

	// Find type identifier and property name from struct_declaration.
	typeName := ""
	propName := ""

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "struct_declaration" {
			continue
		}
		// Extract type identifier.
		if ti := firstChildOfType(child, "type_identifier"); !ti.IsNull() {
			typeName = string(src[ti.StartByte():ti.EndByte()])
		}
		// Extract property name from struct_declarator.
		for j := uint32(0); j < child.ChildCount(); j++ {
			sd := child.Child(j)
			if sd.IsNull() || sd.Type() != "struct_declarator" {
				continue
			}
			// Name may be directly an identifier (no pointer) or nested in pointer_declarator.
			if pd := firstChildOfType(sd, "pointer_declarator"); !pd.IsNull() {
				// Walk pointer_declarator to find the innermost identifier.
				propName = objcLastIdentifier(pd, src)
			} else if ident := firstChildOfType(sd, "identifier"); !ident.IsNull() {
				propName = string(src[ident.StartByte():ident.EndByte()])
			}
		}
		break
	}

	if propName == "" {
		return
	}

	qualName := enclosingClass + "." + propName
	meta := map[string]string{"kind": "property"}
	if typeName != "" {
		meta["type"] = typeName
	}
	if attrText != "" {
		meta["attributes"] = attrText
	}

	propNodeID := g.MakeNodeID(filePath, qualName)
	if g.GetNode(propNodeID) != nil {
		return // already emitted (e.g., from category)
	}
	g.AddNode(&graph.Node{
		ID:       propNodeID,
		Type:     graph.NodeMethod,
		Name:     qualName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: propNodeID, Type: graph.EdgeDefines})
	classNodeID := g.MakeNodeID(filePath, enclosingClass)
	if g.GetNode(classNodeID) != nil {
		g.AddEdge(&graph.Edge{From: classNodeID, To: propNodeID, Type: graph.EdgeDefines})
	}
}

// extractImplementation handles @implementation blocks (class_implementation, category_implementation).
// Creates a NodeStruct for the class (if not already from @interface) and extracts method_definition
// children as NodeMethod. This ensures .m files without a corresponding .h still produce nodes.
func (p *ObjCParser) extractImplementation(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// First identifier child = class name.
	className := ""
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "identifier" {
			className = string(src[child.StartByte():child.EndByte()])
			break
		}
	}
	if className == "" {
		return
	}

	// Ensure the class node exists (may already exist from @interface in .h).
	classNodeID := g.MakeNodeID(filePath, className)
	if g.GetNode(classNodeID) == nil {
		g.AddNode(&graph.Node{
			ID:       classNodeID,
			Type:     graph.NodeStruct,
			Name:     className,
			File:     filePath,
			Line:     int(n.StartPoint().Row) + 1,
			Exported: true,
			Metadata: map[string]string{"kind": "implementation"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: classNodeID, Type: graph.EdgeDefines})
	}

	// Walk children for method_definition (implementation methods) and function_definition.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "method_definition":
			p.extractMethodDef(g, child, src, filePath, fileNodeID, className)
		case "function_definition":
			p.extractCFunction(g, child, src, filePath, fileNodeID)
		}
	}
}

// extractMethodDef handles method_definition nodes inside @implementation.
// Uses the same selector extraction as extractMethod but from method_definition grammar.
func (p *ObjCParser) extractMethodDef(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, enclosingClass string) {
	// Determine instance (-) or class (+) method.
	scope := "instance"
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "+" {
			scope = "class"
			break
		}
		if child.Type() == "-" {
			break
		}
	}

	// Build selector from identifier + method_parameter pattern (same as extractMethod).
	var selectorParts []string
	pendingIdent := ""

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			pendingIdent = string(src[child.StartByte():child.EndByte()])
		case "keyword_argument_list":
			// method_definition may use keyword_argument_list for parameters.
			p.extractSelectorFromKeywordArgs(child, src, &selectorParts)
			pendingIdent = ""
		case "method_parameter":
			if pendingIdent != "" {
				selectorParts = append(selectorParts, pendingIdent+":")
				pendingIdent = ""
			}
		}
	}

	if len(selectorParts) == 0 && pendingIdent != "" {
		selectorParts = []string{pendingIdent}
	}
	if len(selectorParts) == 0 {
		return
	}

	selector := strings.Join(selectorParts, "")
	qualName := enclosingClass + "." + selector

	// Don't duplicate if already extracted from @interface.
	methodNodeID := g.MakeNodeID(filePath, qualName)
	if g.GetNode(methodNodeID) != nil {
		return
	}

	meta := map[string]string{"scope": scope}
	g.AddNode(&graph.Node{
		ID:       methodNodeID,
		Type:     graph.NodeMethod,
		Name:     qualName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: methodNodeID, Type: graph.EdgeDefines})
	classNodeID := g.MakeNodeID(filePath, enclosingClass)
	if g.GetNode(classNodeID) != nil {
		g.AddEdge(&graph.Edge{From: classNodeID, To: methodNodeID, Type: graph.EdgeDefines})
	}
}

// extractSelectorFromKeywordArgs extracts selector parts from a keyword_argument_list node
// used in method_definition grammar.
func (p *ObjCParser) extractSelectorFromKeywordArgs(n sitter.Node, src []byte, parts *[]string) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "keyword_declarator" {
			// keyword_declarator contains: identifier ":" type identifier
			for j := uint32(0); j < child.ChildCount(); j++ {
				kc := child.Child(j)
				if !kc.IsNull() && kc.Type() == "identifier" {
					*parts = append(*parts, string(src[kc.StartByte():kc.EndByte()])+":")
					break // first identifier is the keyword
				}
			}
		}
	}
}

// extractCFunction handles standalone C function_definition nodes inside @implementation.
func (p *ObjCParser) extractCFunction(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Look for the function declarator to get the name.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "function_declarator" {
			if ident := firstChildOfType(child, "identifier"); !ident.IsNull() {
				name := string(src[ident.StartByte():ident.EndByte()])
				funcNodeID := g.MakeNodeID(filePath, name)
				if g.GetNode(funcNodeID) != nil {
					return
				}
				g.AddNode(&graph.Node{
					ID:       funcNodeID,
					Type:     graph.NodeFunction,
					Name:     name,
					File:     filePath,
					Line:     int(n.StartPoint().Row) + 1,
					Exported: true,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: funcNodeID, Type: graph.EdgeDefines})
				return
			}
		}
	}
}

// objcLastIdentifier walks a pointer_declarator chain to find the innermost identifier.
// Pointer declarators can nest: ** pointer → pointer → identifier.
func objcLastIdentifier(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	// If this node is an identifier, return it.
	if n.Type() == "identifier" {
		return string(src[n.StartByte():n.EndByte()])
	}
	// Recurse into children looking for nested pointer_declarator or identifier.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "identifier" {
			return string(src[child.StartByte():child.EndByte()])
		}
		if child.Type() == "pointer_declarator" {
			if name := objcLastIdentifier(child, src); name != "" {
				return name
			}
		}
	}
	return ""
}
