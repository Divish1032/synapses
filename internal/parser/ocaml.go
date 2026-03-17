package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/ocaml"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// OCamlParser parses OCaml (.ml, .mli) source files.
type OCamlParser struct {
	language *sitter.Language
}

// NewOCamlParser creates a ready-to-use OCamlParser.
func NewOCamlParser() *OCamlParser {
	return &OCamlParser{language: ocaml.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *OCamlParser) Extensions() []string {
	return []string{".ml", ".mli"}
}

// extractOCamlDocComment scans backwards from startLine (1-indexed) looking for
// OCaml doc comments: (** ... *). Returns the extracted doc text or "".
func extractOCamlDocComment(lines []string, startLine int) string {
	if startLine < 2 || len(lines) == 0 {
		return ""
	}
	// Scan backwards skipping blank lines to find a doc comment.
	for i := startLine - 2; i >= 0 && i >= startLine-15; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		// Single-line doc comment: (** text *)
		if strings.HasPrefix(trimmed, "(**") && strings.HasSuffix(trimmed, "*)") {
			inner := trimmed[3 : len(trimmed)-2]
			return strings.TrimSpace(inner)
		}
		// Multi-line doc comment ending with *)
		if strings.HasSuffix(trimmed, "*)") {
			// Scan backwards for the opening (**)
			var parts []string
			for j := i; j >= 0 && j >= i-20; j-- {
				line := strings.TrimSpace(lines[j])
				if strings.HasPrefix(line, "(**") {
					// Opening line — strip (** prefix
					opening := strings.TrimPrefix(line, "(**")
					opening = strings.TrimSuffix(opening, "*)")
					opening = strings.TrimSpace(opening)
					if opening != "" {
						parts = append([]string{opening}, parts...)
					}
					return strings.Join(parts, " ")
				}
				// Strip leading * and trailing *)
				cleaned := line
				cleaned = strings.TrimSuffix(cleaned, "*)")
				cleaned = strings.TrimPrefix(cleaned, "*")
				cleaned = strings.TrimSpace(cleaned)
				if cleaned != "" {
					parts = append([]string{cleaned}, parts...)
				}
			}
		}
		// Not a doc comment — stop.
		break
	}
	return ""
}

// extractOCamlDeclInfo does a pre-pass collecting metadata for OCaml declarations.
func extractOCamlDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "value_specification":
			// .mli interface: `val name : type`
			name := extractOCamlValSpecName(n, src)
			if name != "" {
				sl := int(n.StartPoint().Row) + 1
				lc := int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractOCamlDocComment(lines, sl),
					LineCount: lc,
				}
			}
		case "value_definition":
			names := extractOCamlLetNames(n, src)
			sl := int(n.StartPoint().Row) + 1
			lc := int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1
			doc := extractOCamlDocComment(lines, sl)
			for _, name := range names {
				result[name] = declMeta{Doc: doc, LineCount: lc}
			}
		case "type_definition":
			names := extractOCamlTypeNames(n, src)
			sl := int(n.StartPoint().Row) + 1
			lc := int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1
			doc := extractOCamlDocComment(lines, sl)
			for _, name := range names {
				result[name] = declMeta{Doc: doc, LineCount: lc}
			}
		case "module_definition", "module_type_definition":
			name := extractOCamlModuleName(n, src)
			if name != "" {
				sl := int(n.StartPoint().Row) + 1
				lc := int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractOCamlDocComment(lines, sl),
					LineCount: lc,
				}
			}
		case "class_definition":
			name := extractOCamlClassName(n, src)
			if name != "" {
				sl := int(n.StartPoint().Row) + 1
				lc := int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractOCamlDocComment(lines, sl),
					LineCount: lc,
				}
			}
		case "exception_definition":
			name := extractOCamlExceptionName(n, src)
			if name != "" {
				sl := int(n.StartPoint().Row) + 1
				lc := int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractOCamlDocComment(lines, sl),
					LineCount: lc,
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

// Parse extracts code entities from a single OCaml file.
func (p *OCamlParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	declInfo := extractOCamlDeclInfo(root, src)

	// Walk top-level children of the root node.
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "value_definition":
			extractOCamlValueDef(g, n, src, filePath, fileNodeID, declInfo)
			return
		case "value_specification":
			// .mli interface files use `val name : type` → NodeFunction or NodeVariable.
			extractOCamlValSpec(g, n, src, filePath, fileNodeID, declInfo)
			return
		case "type_definition":
			extractOCamlTypeDef(g, n, src, filePath, fileNodeID, declInfo)
			return
		case "module_definition":
			extractOCamlModuleDef(g, n, src, filePath, fileNodeID, declInfo)
			return
		case "module_type_definition":
			extractOCamlModuleTypeDef(g, n, src, filePath, fileNodeID, declInfo)
			return
		case "class_definition":
			extractOCamlClassDef(g, n, src, filePath, fileNodeID, declInfo)
			return
		case "open_statement", "open_module":
			extractOCamlOpen(g, n, src, filePath, fileNodeID)
			return
		case "include_module":
			extractOCamlInclude(g, n, src, filePath, fileNodeID)
			return
		case "exception_definition":
			extractOCamlExceptionDef(g, n, src, filePath, fileNodeID, declInfo)
			return
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)

	// Call sites: OCaml function application is juxtaposition (f x).
	collectOCamlCallSites(g, root, src, filePath, fileNodeID)

	return nil
}

// extractOCamlLetNames extracts binding names from a value_definition node.
// Handles `let name`, `let rec name`, and `let ... and ...` forms.
func extractOCamlLetNames(n *sitter.Node, src []byte) []string {
	var names []string
	var walkBindings func(node *sitter.Node)
	walkBindings = func(node *sitter.Node) {
		if node == nil {
			return
		}
		nt := node.Type()
		// let_binding contains the actual name and body.
		if nt == "let_binding" {
			if nameNode := node.ChildByFieldName("pattern"); nameNode != nil {
				name := extractOCamlPatternName(nameNode, src)
				if name != "" {
					names = append(names, name)
				}
			}
			// Fallback: try to find a value_name or value_pattern child.
			if len(names) == 0 || names[len(names)-1] == "" {
				for i := 0; i < int(node.ChildCount()); i++ {
					child := node.Child(i)
					if child == nil {
						continue
					}
					ct := child.Type()
					if ct == "value_name" || ct == "value_pattern" {
						name := childText(child, src)
						if name != "" {
							names = append(names, name)
						}
						break
					}
				}
			}
			return
		}
		for i := 0; i < int(node.ChildCount()); i++ {
			walkBindings(node.Child(i))
		}
	}
	walkBindings(n)

	// Fallback: if no let_binding found, scan for identifiers directly.
	if len(names) == 0 {
		names = extractOCamlIdentifiers(n, src)
	}
	return names
}

// extractOCamlPatternName extracts the name from a pattern node.
func extractOCamlPatternName(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case "value_name", "value_pattern":
		return childText(n, src)
	case "parenthesized_pattern":
		// Recurse into the inner pattern.
		for i := 0; i < int(n.ChildCount()); i++ {
			child := n.Child(i)
			if child != nil && child.Type() != "(" && child.Type() != ")" {
				return extractOCamlPatternName(child, src)
			}
		}
	}
	// For simple identifiers.
	text := childText(n, src)
	// Only return if it looks like a valid OCaml identifier (no operators, no complex patterns).
	if text != "" && !strings.ContainsAny(text, " \t\n(){}[];,") {
		return text
	}
	return ""
}

// extractOCamlIdentifiers extracts all identifier-like names from a node's children.
func extractOCamlIdentifiers(n *sitter.Node, src []byte) []string {
	var names []string
	isRec := false
	seenLet := false
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		text := childText(child, src)
		if text == "let" {
			seenLet = true
			continue
		}
		if text == "rec" {
			isRec = true
			continue
		}
		if text == "and" {
			continue
		}
		ct := child.Type()
		if seenLet || isRec {
			if ct == "value_name" || ct == "value_pattern" ||
				(ct != "=" && ct != "let" && ct != "rec" && ct != "and" &&
					!strings.ContainsAny(text, " \t\n(){}[];,=") && text != "") {
				names = append(names, text)
				seenLet = false
				isRec = false
			}
		}
	}
	return names
}

// ocamlLetHasParams returns true if a let binding has function parameters
// (i.e. it's a function, not a simple value binding).
func ocamlLetHasParams(n *sitter.Node, src []byte) bool {
	// Look for let_binding children.
	var checkBinding func(node *sitter.Node) bool
	checkBinding = func(node *sitter.Node) bool {
		if node == nil {
			return false
		}
		if node.Type() == "let_binding" {
			// A function binding has parameter nodes after the name but before '='.
			foundName := false
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				if child == nil {
					continue
				}
				text := childText(child, src)
				ct := child.Type()
				if ct == "value_name" || ct == "value_pattern" {
					foundName = true
					continue
				}
				if ct == "pattern" && child.ChildByFieldName("pattern") != nil {
					foundName = true
					continue
				}
				if text == "=" {
					break
				}
				// If we found the name and the next child is a parameter pattern, it's a function.
				if foundName && ct != "=" && ct != ":" && ct != "type_constraint" {
					if ct == "parameter" || ct == "parenthesized_pattern" ||
						ct == "value_name" || ct == "value_pattern" ||
						ct == "unit" || ct == "tuple_pattern" || ct == "typed_pattern" ||
						ct == "label" || ct == "labeled_argument" {
						return true
					}
				}
			}
			return false
		}
		for i := 0; i < int(node.ChildCount()); i++ {
			if checkBinding(node.Child(i)) {
				return true
			}
		}
		return false
	}
	return checkBinding(n)
}

// ocamlIsRec returns true if a value_definition contains the "rec" keyword.
func ocamlIsRec(n *sitter.Node, src []byte) bool {
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child != nil && childText(child, src) == "rec" {
			return true
		}
	}
	return false
}

// extractOCamlValueDef processes a value_definition (let binding) node.
func extractOCamlValueDef(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta) {
	names := extractOCamlLetNames(n, src)
	isRec := ocamlIsRec(n, src)
	hasParams := ocamlLetHasParams(n, src)
	startLine := int(n.StartPoint().Row) + 1

	for _, name := range names {
		if name == "" {
			continue
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			continue
		}
		nodeType := graph.NodeVariable
		if isRec || hasParams {
			nodeType = graph.NodeFunction
		}
		meta := buildLangMeta(declInfo[name])
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     nodeType,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// extractOCamlValSpecName extracts the name from a value_specification node.
// .mli interface files use `val name : type` which produces value_specification.
func extractOCamlValSpecName(n *sitter.Node, src []byte) string {
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		if ct == "value_name" || ct == "value_pattern" {
			return childText(child, src)
		}
		// After "val" keyword, next identifier is the name.
		text := childText(child, src)
		if text == "val" || text == ":" {
			continue
		}
		// Operator name wrapped in parentheses: (>>=), (|>), (+), etc.
		if strings.HasPrefix(text, "(") && strings.HasSuffix(text, ")") && len(text) > 2 {
			inner := text[1 : len(text)-1]
			if inner != "" {
				return text
			}
		}
		if ct != "val" && !strings.ContainsAny(text, " \t\n():[]{}") && text != "" {
			return text
		}
	}
	return ""
}

// extractOCamlValSpec processes a value_specification node (.mli `val name : type`).
// It determines whether the spec is a function type (contains "->") or a plain value.
func extractOCamlValSpec(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta) {
	name := extractOCamlValSpecName(n, src)
	if name == "" {
		return
	}
	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}
	// If the type expression contains "->", it's a function type.
	fullText := childText(n, src)
	nodeType := graph.NodeVariable
	if strings.Contains(fullText, "->") {
		nodeType = graph.NodeFunction
	}
	meta := buildLangMeta(declInfo[name])
	if meta == nil {
		meta = make(map[string]string)
	}
	meta["kind"] = "val"
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     nodeType,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractOCamlTypeNames extracts type names from a type_definition node.
// Handles `type t = ...` and `type t = ... and u = ...`.
func extractOCamlTypeNames(n *sitter.Node, src []byte) []string {
	var names []string
	var walk func(node *sitter.Node)
	walk = func(node *sitter.Node) {
		if node == nil {
			return
		}
		if node.Type() == "type_binding" {
			if nameNode := node.ChildByFieldName("name"); nameNode != nil {
				name := childText(nameNode, src)
				if name != "" {
					names = append(names, name)
					return
				}
			}
			// Fallback: scan children for type_constructor_path or value_name.
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				if child == nil {
					continue
				}
				ct := child.Type()
				if ct == "type_constructor_path" || ct == "type_constructor" {
					name := childText(child, src)
					if name != "" {
						names = append(names, name)
						return
					}
				}
			}
		}
		for i := 0; i < int(node.ChildCount()); i++ {
			walk(node.Child(i))
		}
	}
	walk(n)

	// Fallback: look for identifiers after "type" keyword.
	if len(names) == 0 {
		seenType := false
		for i := 0; i < int(n.ChildCount()); i++ {
			child := n.Child(i)
			if child == nil {
				continue
			}
			text := childText(child, src)
			if text == "type" {
				seenType = true
				continue
			}
			if text == "and" {
				seenType = true
				continue
			}
			if seenType && text != "=" && !strings.ContainsAny(text, " \t\n(){}[];,") && text != "" {
				names = append(names, text)
				seenType = false
			}
		}
	}
	return names
}

// extractOCamlTypeDef processes a type_definition node.
func extractOCamlTypeDef(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta) {
	names := extractOCamlTypeNames(n, src)
	startLine := int(n.StartPoint().Row) + 1

	for _, name := range names {
		if name == "" {
			continue
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			continue
		}
		meta := buildLangMeta(declInfo[name])
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// extractOCamlModuleName extracts the module name from a module_definition or module_type_definition node.
// AST: module_definition → module_binding → module_name
func extractOCamlModuleName(n *sitter.Node, src []byte) string {
	if nameNode := n.ChildByFieldName("name"); nameNode != nil {
		return childText(nameNode, src)
	}
	// Navigate into module_binding or similar children to find module_name.
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		if ct == "module_name" {
			return childText(child, src)
		}
		// module_binding contains the module_name.
		if ct == "module_binding" {
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc != nil && gc.Type() == "module_name" {
					return childText(gc, src)
				}
			}
		}
		// module_type_binding may also contain the name.
		if ct == "module_type_binding" {
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc != nil && (gc.Type() == "module_type_name" || gc.Type() == "module_name") {
					return childText(gc, src)
				}
			}
		}
	}
	return ""
}

// extractOCamlModuleDef processes a module_definition node.
func extractOCamlModuleDef(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta) {
	name := extractOCamlModuleName(n, src)
	if name == "" {
		return
	}
	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}
	meta := buildLangMeta(declInfo[name])
	if meta == nil {
		meta = make(map[string]string, 1)
	}
	meta["kind"] = "module"
	startLine := int(n.StartPoint().Row) + 1
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractOCamlModuleTypeDef processes a module_type_definition node.
func extractOCamlModuleTypeDef(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta) {
	name := extractOCamlModuleName(n, src)
	if name == "" {
		return
	}
	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}
	meta := buildLangMeta(declInfo[name])
	startLine := int(n.StartPoint().Row) + 1
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeInterface,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractOCamlClassName extracts the class name from a class_definition node.
// AST: class_definition → class_binding → class_name
func extractOCamlClassName(n *sitter.Node, src []byte) string {
	if nameNode := n.ChildByFieldName("name"); nameNode != nil {
		return childText(nameNode, src)
	}
	// Navigate into class_binding to find class_name.
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		if ct == "class_name" {
			return childText(child, src)
		}
		if ct == "class_binding" {
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc != nil && gc.Type() == "class_name" {
					return childText(gc, src)
				}
			}
		}
	}
	return ""
}

// extractOCamlClassDef processes a class_definition node.
func extractOCamlClassDef(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta) {
	name := extractOCamlClassName(n, src)
	if name == "" {
		return
	}
	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}
	meta := buildLangMeta(declInfo[name])
	if meta == nil {
		meta = make(map[string]string, 1)
	}
	meta["kind"] = "class"
	startLine := int(n.StartPoint().Row) + 1
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractOCamlOpen processes an open_statement node.
func extractOCamlOpen(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Extract the module path from the open statement.
	var moduleName string
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		text := childText(child, src)
		if text == "open" || text == "!" {
			continue
		}
		ct := child.Type()
		if ct == "module_path" || ct == "module_name" || ct == "extended_module_path" ||
			ct == "constructor_path" {
			moduleName = text
			break
		}
		// Uppercase identifier after "open" is the module name.
		if isExported(text) && !strings.ContainsAny(text, " \t\n(){}[];,=") {
			moduleName = text
			break
		}
	}
	if moduleName == "" {
		return
	}
	importNodeID := g.MakeNodeID(moduleName, moduleName)
	g.AddNode(&graph.Node{
		ID:      importNodeID,
		Type:    graph.NodePackage,
		Name:    moduleName,
		Package: moduleName,
		File:    filePath,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
}

// extractOCamlInclude handles `include Module` statements, creating EdgeImports
// from the file to the included module. In OCaml, include brings all definitions
// from a module into scope (stronger than open).
func extractOCamlInclude(
	g *graph.Graph,
	n *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// include_module structure: "include" keyword + module expression
	// The module name is typically in a module_path or module_type_path child.
	var moduleName string

	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		// Skip the "include" keyword itself.
		if ct == "include" {
			continue
		}
		// Module path expressions.
		if ct == "module_path" || ct == "module_type_path" || ct == "module_identifier" {
			moduleName = strings.TrimSpace(childText(child, src))
			break
		}
		// Fallback: any uppercase-starting identifier.
		text := strings.TrimSpace(childText(child, src))
		if text != "" && len(text) > 0 && text[0] >= 'A' && text[0] <= 'Z' {
			moduleName = text
			break
		}
	}

	if moduleName == "" {
		return
	}

	importNodeID := g.MakeNodeID(moduleName, moduleName)
	g.AddNode(&graph.Node{
		ID:      importNodeID,
		Type:    graph.NodePackage,
		Name:    moduleName,
		Package: moduleName,
		File:    filePath,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
}

// extractOCamlExceptionName extracts the exception name from an exception_definition node.
// AST: exception_definition → constructor_declaration → constructor_name
func extractOCamlExceptionName(n *sitter.Node, src []byte) string {
	if nameNode := n.ChildByFieldName("name"); nameNode != nil {
		return childText(nameNode, src)
	}
	// Navigate into constructor_declaration to find constructor_name.
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child == nil {
			continue
		}
		ct := child.Type()
		if ct == "constructor_name" {
			return childText(child, src)
		}
		if ct == "constructor_declaration" {
			for j := 0; j < int(child.ChildCount()); j++ {
				gc := child.Child(j)
				if gc != nil && gc.Type() == "constructor_name" {
					return childText(gc, src)
				}
			}
		}
	}
	return ""
}

// extractOCamlExceptionDef processes an exception_definition node.
func extractOCamlExceptionDef(g *graph.Graph, n *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta) {
	name := extractOCamlExceptionName(n, src)
	if name == "" {
		return
	}
	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}
	meta := buildLangMeta(declInfo[name])
	if meta == nil {
		meta = make(map[string]string, 1)
	}
	meta["kind"] = "exception"
	startLine := int(n.StartPoint().Row) + 1
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// collectOCamlCallSites walks the AST to find function application nodes
// and records them as call sites.
func collectOCamlCallSites(g *graph.Graph, root *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		if n.Type() == "application_expression" {
			// The first child of an application_expression is the function being called
			// (a value_path containing a value_name, or a field_expression for method calls).
			if n.ChildCount() > 0 {
				funcNode := n.Child(0)
				if funcNode != nil {
					// Prefer value_name grandchild (e.g. value_path > value_name).
					var callee string
					if funcNode.Type() == "value_path" || funcNode.Type() == "module_path" {
						if vn := firstChildOfType(funcNode, "value_name"); vn != nil {
							callee = strings.TrimSpace(childText(vn, src))
						} else {
							callee = strings.TrimSpace(childText(funcNode, src))
						}
					} else if funcNode.Type() == "value_name" {
						callee = strings.TrimSpace(childText(funcNode, src))
					}
					// Only record simple identifiers and qualified names as call sites.
					if callee != "" && !isOCamlBuiltin(callee) &&
						!strings.ContainsAny(callee, " \t\n(){}[];,=") {
						g.AddCallSite(graph.CallSite{
							CallerID:   fileNodeID,
							CallerFile: filePath,
							FuncName:   callee,
						})
					}
				}
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// isOCamlBuiltin returns true if the name is a common OCaml builtin/pervasive.
func isOCamlBuiltin(name string) bool {
	switch name {
	case "print_string", "print_endline", "print_int", "print_float",
		"print_char", "print_newline", "prerr_endline", "prerr_string",
		"Printf.printf", "Printf.sprintf", "Printf.fprintf",
		"failwith", "raise", "invalid_arg", "ignore",
		"not", "succ", "pred", "abs", "max", "min",
		"fst", "snd", "ref",
		"List.map", "List.iter", "List.fold_left", "List.fold_right",
		"List.filter", "List.length", "List.rev", "List.hd", "List.tl",
		"Array.make", "Array.length", "Array.get", "Array.set",
		"String.length", "String.sub", "String.concat",
		"Hashtbl.create", "Hashtbl.find", "Hashtbl.add", "Hashtbl.replace",
		"int_of_string", "string_of_int", "float_of_string", "string_of_float",
		"int_of_float", "float_of_int", "char_of_int", "int_of_char",
		"string_of_bool", "bool_of_string":
		return true
	}
	return false
}
