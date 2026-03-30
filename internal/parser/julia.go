package parser

import (
	"path/filepath"
	"strings"

	juliag "github.com/alexaandru/go-sitter-forest/julia"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// JuliaParser parses Julia (.jl) source files.
type JuliaParser struct {
	language *sitter.Language
}

// NewJuliaParser creates a ready-to-use JuliaParser.
func NewJuliaParser() *JuliaParser {
	return &JuliaParser{language: sitter.NewLanguage(juliag.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *JuliaParser) Extensions() []string {
	return []string{".jl"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *JuliaParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// juliaIsUppercase returns true if name starts with an uppercase letter.
// In Julia, convention is that uppercase-named symbols are exported/public.
func juliaIsUppercase(name string) bool {
	if len(name) == 0 {
		return false
	}
	return strings.ToUpper(name[:1]) == name[:1] && name[:1] != strings.ToLower(name[:1])
}

// juliaExtractTypeHeadName extracts the struct/type name from a type_head node.
// type_head can contain:
//   - identifier → simple name
//   - binary_expression → "Name <: SuperType" (left = identifier = name)
func juliaExtractTypeHeadName(typeHead sitter.Node, src []byte) (name string, supertype string) {
	if typeHead.IsNull() {
		return "", ""
	}
	for i := uint32(0); i < typeHead.ChildCount(); i++ {
		child := typeHead.Child(i)
		if child.IsNull() {
			continue
		}
		switch nodeType(child) {
		case "identifier":
			name = string(src[child.StartByte():child.EndByte()])
			return name, ""
		case "parametrized_type_expression":
			// Point{T} — first identifier child is the name.
			name = juliaFirstIdentifier(child, src)
			return name, ""
		case "binary_expression":
			// Name <: SuperType — left is name (identifier or parametrized_type_expression),
			// right is supertype (identifier or parametrized_type_expression).
			for j := uint32(0); j < child.ChildCount(); j++ {
				sub := child.Child(j)
				if sub.IsNull() {
					continue
				}
				switch nodeType(sub) {
				case "identifier":
					if name == "" {
						name = string(src[sub.StartByte():sub.EndByte()])
					} else if supertype == "" {
						supertype = string(src[sub.StartByte():sub.EndByte()])
					}
				case "parametrized_type_expression":
					ident := juliaFirstIdentifier(sub, src)
					if name == "" {
						name = ident
					} else if supertype == "" {
						supertype = ident
					}
				}
			}
			return name, supertype
		}
	}
	return "", ""
}

// juliaFirstIdentifier returns the first identifier text in a node's children.
func juliaFirstIdentifier(n sitter.Node, src []byte) string {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && nodeType(child) == "identifier" {
			return string(src[child.StartByte():child.EndByte()])
		}
	}
	return ""
}

// juliaExtractFuncName extracts the function name from a signature node.
// signature can contain call_expression or typed_expression wrapping call_expression.
// call_expression → identifier (first identifier child) + argument_list.
func juliaExtractFuncName(sig sitter.Node, src []byte) string {
	if sig.IsNull() {
		return ""
	}
	// Walk down: signature → (typed_expression / where_expression →) call_expression → identifier
	for i := uint32(0); i < sig.ChildCount(); i++ {
		child := sig.Child(i)
		if child.IsNull() {
			continue
		}
		switch nodeType(child) {
		case "call_expression":
			return juliaCallExprName(child, src)
		case "typed_expression":
			// typed_expression wraps call_expression with "::" return type.
			for j := uint32(0); j < child.ChildCount(); j++ {
				sub := child.Child(j)
				if sub.IsNull() {
					continue
				}
				if nodeType(sub) == "call_expression" {
					return juliaCallExprName(sub, src)
				}
				if nodeType(sub) == "where_expression" {
					if name := juliaExtractFromWhere(sub, src); name != "" {
						return name
					}
				}
			}
		case "where_expression":
			// function transform(x::T) where {T <: Number} ... end
			if name := juliaExtractFromWhere(child, src); name != "" {
				return name
			}
		case "identifier":
			return string(src[child.StartByte():child.EndByte()])
		}
	}
	return ""
}

// juliaCallExprName extracts the function name from a call_expression node.
// Handles both plain identifiers and field_expression (Base.show).
func juliaCallExprName(n sitter.Node, src []byte) string {
	for j := uint32(0); j < n.ChildCount(); j++ {
		sub := n.Child(j)
		if sub.IsNull() {
			continue
		}
		switch nodeType(sub) {
		case "identifier":
			return string(src[sub.StartByte():sub.EndByte()])
		case "field_expression":
			// Base.show → extract the full qualified name.
			return string(src[sub.StartByte():sub.EndByte()])
		}
	}
	return ""
}

// juliaExtractFromWhere extracts function name from a where_expression.
// where_expression contains a call_expression and the where clause.
func juliaExtractFromWhere(n sitter.Node, src []byte) string {
	for j := uint32(0); j < n.ChildCount(); j++ {
		sub := n.Child(j)
		if sub.IsNull() {
			continue
		}
		if nodeType(sub) == "call_expression" {
			return juliaCallExprName(sub, src)
		}
		if nodeType(sub) == "typed_expression" {
			// Recurse into typed_expression inside where.
			for k := uint32(0); k < sub.ChildCount(); k++ {
				sub2 := sub.Child(k)
				if !sub2.IsNull() && nodeType(sub2) == "call_expression" {
					return juliaCallExprName(sub2, src)
				}
			}
		}
	}
	return ""
}

// Parse extracts code entities from a single Julia file and merges them into the graph.
func (p *JuliaParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	extractJuliaBody(g, root, src, filePath, fileNodeID, fileNodeID)

	return nil
}

// extractJuliaBody recursively processes Julia AST nodes and adds them to the graph.
// parentID is the node ID of the enclosing container (file or module).
func extractJuliaBody(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	parentID graph.NodeID,
) {
	if node.IsNull() {
		return
	}

	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}

		switch nodeType(child) {
		case "module_definition":
			extractJuliaModule(g, child, src, filePath, fileNodeID)

		case "struct_definition":
			extractJuliaStruct(g, child, src, filePath, fileNodeID)

		case "abstract_definition":
			extractJuliaAbstract(g, child, src, filePath, fileNodeID)

		case "function_definition":
			extractJuliaFunction(g, child, src, filePath, fileNodeID)

		case "macro_definition":
			extractJuliaMacro(g, child, src, filePath, fileNodeID)

		case "const_statement":
			extractJuliaConst(g, child, src, filePath, fileNodeID)

		case "assignment":
			// Short-form function: f(x) = expr — LHS is call_expression.
			extractJuliaShortFunc(g, child, src, filePath, fileNodeID)

		case "using_statement", "import_statement":
			extractJuliaImport(g, child, src, filePath, fileNodeID)

		default:
			// Recurse into other container nodes (e.g. if/begin/let blocks).
			extractJuliaBody(g, child, src, filePath, fileNodeID, parentID)
		}
	}
}

// extractJuliaModule handles module_definition nodes.
func extractJuliaModule(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// Find the module name: identifier child after "module" keyword.
	name := ""
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if nodeType(child) == "identifier" {
			name = string(src[child.StartByte():child.EndByte()])
			break
		}
	}
	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: map[string]string{"kind": "module"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

	// Recursively process module body children.
	extractJuliaBody(g, n, src, filePath, fileNodeID, nodeID)
}

// extractJuliaStruct handles struct_definition nodes.
func extractJuliaStruct(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// Check for mutable keyword.
	isMutable := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && nodeType(child) == "mutable" {
			isMutable = true
			break
		}
	}

	// Find type_head child for the name.
	typeHead := sitter.Node{}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && nodeType(child) == "type_head" {
			typeHead = child
			break
		}
	}
	if typeHead.IsNull() {
		return
	}

	name, supertype := juliaExtractTypeHeadName(typeHead, src)
	if name == "" {
		return
	}

	meta := map[string]string{"kind": "struct"}
	if isMutable {
		meta["mutable"] = "true"
	}
	if supertype != "" {
		meta["supertype"] = supertype
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: juliaIsUppercase(name),
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractJuliaAbstract handles abstract_definition nodes.
func extractJuliaAbstract(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	typeHead := sitter.Node{}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && nodeType(child) == "type_head" {
			typeHead = child
			break
		}
	}
	if typeHead.IsNull() {
		return
	}

	name, _ := juliaExtractTypeHeadName(typeHead, src)
	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeInterface,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: juliaIsUppercase(name),
		Metadata: map[string]string{"kind": "abstract"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractJuliaFunction handles function_definition nodes.
func extractJuliaFunction(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// Find the signature node.
	sig := sitter.Node{}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && nodeType(child) == "signature" {
			sig = child
			break
		}
	}
	if sig.IsNull() {
		return
	}

	name := juliaExtractFuncName(sig, src)
	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: juliaIsUppercase(name),
		Metadata: map[string]string{"kind": "function"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractJuliaMacro handles macro_definition nodes.
func extractJuliaMacro(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	sig := sitter.Node{}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && nodeType(child) == "signature" {
			sig = child
			break
		}
	}
	if sig.IsNull() {
		return
	}

	name := juliaExtractFuncName(sig, src)
	if name == "" {
		return
	}

	// Prefix macro names with "@" per Julia convention.
	macroName := "@" + name

	nodeID := g.MakeNodeID(filePath, macroName)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     macroName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: juliaIsUppercase(name),
		Metadata: map[string]string{"kind": "macro"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractJuliaConst handles const_statement nodes.
// const X = value → NodeField with kind=const.
func extractJuliaConst(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// const_statement → const + assignment
	// assignment → identifier + "=" + value
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || nodeType(child) != "assignment" {
			continue
		}
		// First identifier in assignment is the constant name.
		for j := uint32(0); j < child.ChildCount(); j++ {
			sub := child.Child(j)
			if sub.IsNull() {
				continue
			}
			if nodeType(sub) == "identifier" {
				name := string(src[sub.StartByte():sub.EndByte()])
				nodeID := g.MakeNodeID(filePath, name)
				if g.GetNode(nodeID) != nil {
					break
				}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeVariable,
					Name:     name,
					File:     filePath,
					Line:     int(n.StartPoint().Row) + 1,
					Exported: juliaIsUppercase(name),
					Metadata: map[string]string{"kind": "const"},
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				break
			}
		}
		break
	}
}

// extractJuliaShortFunc handles assignment nodes that represent short-form functions.
// f(x) = expr → the LHS is a call_expression.
func extractJuliaShortFunc(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if n.ChildCount() == 0 {
		return
	}
	// First child must be a call_expression (LHS of assignment).
	lhs := n.Child(0)
	if lhs.IsNull() {
		return
	}

	var name string
	switch nodeType(lhs) {
	case "call_expression":
		name = juliaCallExprName(lhs, src)
	case "where_expression":
		// f(x::T) where T = expr
		name = juliaExtractFromWhere(lhs, src)
	case "typed_expression":
		// f(x)::Int = expr
		for i := uint32(0); i < lhs.ChildCount(); i++ {
			sub := lhs.Child(i)
			if !sub.IsNull() && nodeType(sub) == "call_expression" {
				name = juliaCallExprName(sub, src)
				break
			}
		}
	default:
		return // Not a function assignment (e.g., plain x = 5).
	}

	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return // Already extracted (e.g., from function_definition).
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: juliaIsUppercase(name),
		Metadata: map[string]string{"kind": "function"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractJuliaImport handles using_statement and import_statement nodes.
// using LinearAlgebra: norm, dot → creates import nodes.
// import JSON → creates import node.
func extractJuliaImport(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// Walk children to find module names (identifiers or selected_import nodes).
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		var modName string
		switch nodeType(child) {
		case "identifier":
			modName = string(src[child.StartByte():child.EndByte()])
		case "selected_import":
			// selected_import: ModuleName : symbol1, symbol2
			modName = juliaFirstIdentifier(child, src)
		case "scoped_identifier":
			// Pkg.SubModule
			modName = string(src[child.StartByte():child.EndByte()])
		}
		if modName == "" || modName == "using" || modName == "import" {
			continue
		}
		importNodeID := g.MakeNodeID(modName, modName)
		if g.GetNode(importNodeID) == nil {
			g.AddNode(&graph.Node{
				ID:      importNodeID,
				Type:    graph.NodePackage,
				Name:    modName,
				Package: modName,
				File:    filePath,
				Line:    int(n.StartPoint().Row) + 1,
			})
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}
}
