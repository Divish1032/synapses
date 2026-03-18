package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	gositter "github.com/alexaandru/go-sitter-forest/go"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// rawCallSite is an unresolved call captured during AST walk.
type rawCallSite struct {
	pkgAlias string // "" for direct calls; package alias for pkg.Func() calls
	funcName string // the function/method name being called
}

// goBuiltins lists Go built-in functions to exclude from call site collection.
var goBuiltins = map[string]bool{
	"make": true, "new": true, "len": true, "cap": true, "append": true,
	"copy": true, "close": true, "delete": true, "panic": true, "recover": true,
	"print": true, "println": true, "real": true, "imag": true, "complex": true,
}

// goFuncInfo holds the enriched metadata extracted per Go declaration.
type goFuncInfo struct {
	signature  string        // e.g. "func Foo(x int) error"
	doc        string        // leading doc comment, whitespace-trimmed
	lineCount  int           // end_line - start_line + 1
	complexity int           // cyclomatic complexity (1 + decision points); 0 for types
	callSites  []rawCallSite // raw call sites inside the body
}

// extractInterfaceMethods scans the source_file AST for interface type
// declarations and returns a map of interface name → list of declared method names.
// This is stored as metadata on interface nodes so the resolver can later detect
// which structs implement which interfaces without re-parsing the source.
// extractGoStructFields walks the top-level type declarations and collects
// field name + type strings for each struct. Returns a map from struct name
// to a slice of "FieldName type" strings (at most 15 fields per struct to keep
// metadata compact). Embedded fields (anonymous fields) use just the type name
// as the field name. Struct tags are stripped from the output.
func extractGoStructFields(root sitter.Node, src []byte) map[string][]string {
	result := make(map[string][]string)
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() || child.Type() != "type_declaration" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			spec := child.Child(j)
			if spec.IsNull() || spec.Type() != "type_spec" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			typeNode := spec.ChildByFieldName("type")
			if nameNode.IsNull() || typeNode.IsNull() || typeNode.Type() != "struct_type" {
				continue
			}
			structName := string(src[nameNode.StartByte():nameNode.EndByte()])
			var fields []string
			// Walk struct_type children to find field_declaration_list.
			for k := uint32(0); k < typeNode.ChildCount(); k++ {
				listNode := typeNode.Child(k)
				if listNode.IsNull() || listNode.Type() != "field_declaration_list" {
					continue
				}
				for l := uint32(0); l < listNode.ChildCount(); l++ {
					if len(fields) >= 15 {
						break
					}
					fieldDecl := listNode.Child(l)
					if fieldDecl.IsNull() || fieldDecl.Type() != "field_declaration" {
						continue
					}
					// Collect field_identifier children (the names) and the type node.
					var names []string
					var typeText string
					for m := uint32(0); m < fieldDecl.ChildCount(); m++ {
						fc := fieldDecl.Child(m)
						if fc.IsNull() {
							continue
						}
						switch fc.Type() {
						case "field_identifier":
							names = append(names, string(src[fc.StartByte():fc.EndByte()]))
						case "raw_string_literal", "interpreted_string_literal":
							// struct tag — skip
						default:
							if len(names) > 0 && typeText == "" {
								// First non-name, non-tag child after names is the type.
								typeText = strings.Join(strings.Fields(string(src[fc.StartByte():fc.EndByte()])), " ")
							}
						}
					}
					if len(names) == 0 {
						// Embedded/anonymous field: the whole declaration text is the type.
						raw := strings.TrimSpace(string(src[fieldDecl.StartByte():fieldDecl.EndByte()]))
						// Remove trailing struct tag.
						if idx := strings.Index(raw, "`"); idx > 0 {
							raw = strings.TrimSpace(raw[:idx])
						}
						if raw != "" {
							fields = append(fields, raw)
						}
						continue
					}
					for _, n := range names {
						if len(fields) >= 15 {
							break
						}
						if typeText != "" {
							fields = append(fields, n+" "+typeText)
						} else {
							fields = append(fields, n)
						}
					}
				}
			}
			if len(fields) > 0 {
				result[structName] = fields
			}
		}
	}
	return result
}

func extractInterfaceMethods(root sitter.Node, src []byte) map[string][]string {
	result := make(map[string][]string)
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() || child.Type() != "type_declaration" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			spec := child.Child(j)
			if spec.IsNull() || spec.Type() != "type_spec" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			typeNode := spec.ChildByFieldName("type")
			if nameNode.IsNull() || typeNode.IsNull() || typeNode.Type() != "interface_type" {
				continue
			}
			ifaceName := string(src[nameNode.StartByte():nameNode.EndByte()])
			var methods []string
			for k := uint32(0); k < typeNode.ChildCount(); k++ {
				// Interface method declarations are "method_elem" nodes in the
				// tree-sitter Go grammar. The method name is the first
				// field_identifier child of the method_elem.
				methodElem := typeNode.Child(k)
				if methodElem.IsNull() || methodElem.Type() != "method_elem" {
					continue
				}
				for l := uint32(0); l < methodElem.ChildCount(); l++ {
					nameNode := methodElem.Child(l)
					if !nameNode.IsNull() && nameNode.Type() == "field_identifier" {
						if m := string(src[nameNode.StartByte():nameNode.EndByte()]); m != "" {
							methods = append(methods, m)
						}
						break
					}
				}
			}
			if len(methods) > 0 {
				result[ifaceName] = methods
			}
		}
	}
	return result
}

// extractGoDeclarationInfo walks the top-level children of the source_file node
// and collects signature, doc comment, and line-count for every function,
// method, and struct/interface declaration.  The returned map is keyed by the
// unqualified name (for functions/types) or "ReceiverType.MethodName" (for
// methods) — matching the name strings used when creating graph nodes.
func extractGoDeclarationInfo(root sitter.Node, src []byte) map[string]goFuncInfo {
	result := make(map[string]goFuncInfo)
	lines := strings.Split(string(src), "\n")

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "function_declaration":
			nameNode := child.ChildByFieldName("name")
			if nameNode.IsNull() {
				continue
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			startLine := int(child.StartPoint().Row) + 1
			endLine := int(child.EndPoint().Row) + 1
			var calls []rawCallSite
			var complexity int
			if body := child.ChildByFieldName("body"); !body.IsNull() {
				calls = extractCallSites(body, src)
				complexity = 1 + countComplexity(body)
			}
			result[name] = goFuncInfo{
				signature:  extractSignature(child, src),
				doc:        extractDocComment(lines, startLine),
				lineCount:  endLine - startLine + 1,
				complexity: complexity,
				callSites:  calls,
			}

		case "method_declaration":
			nameNode := child.ChildByFieldName("name")
			receiverNode := child.ChildByFieldName("receiver")
			if nameNode.IsNull() {
				continue
			}
			methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
			receiverType := extractReceiverType(receiverNode, src)
			qualifiedName := receiverType + "." + methodName
			startLine := int(child.StartPoint().Row) + 1
			endLine := int(child.EndPoint().Row) + 1
			var calls []rawCallSite
			var complexity int
			if body := child.ChildByFieldName("body"); !body.IsNull() {
				calls = extractCallSites(body, src)
				complexity = 1 + countComplexity(body)
			}
			result[qualifiedName] = goFuncInfo{
				signature:  extractSignature(child, src),
				doc:        extractDocComment(lines, startLine),
				lineCount:  endLine - startLine + 1,
				complexity: complexity,
				callSites:  calls,
			}

		case "type_declaration":
			// Handles struct and interface type declarations.
			typeSpec := child.Child(0)
			if typeSpec.IsNull() || typeSpec.Type() != "type_spec" {
				// Multi-spec: (type_declaration (type_spec) (type_spec)...)
				// Walk children to find type_spec nodes.
				for j := uint32(0); j < child.ChildCount(); j++ {
					spec := child.Child(j)
					if !spec.IsNull() && spec.Type() == "type_spec" {
						nameNode := spec.ChildByFieldName("name")
						if nameNode.IsNull() {
							continue
						}
						name := string(src[nameNode.StartByte():nameNode.EndByte()])
						startLine := int(child.StartPoint().Row) + 1
						endLine := int(child.EndPoint().Row) + 1
						result[name] = goFuncInfo{
							doc:       extractDocComment(lines, startLine),
							lineCount: endLine - startLine + 1,
						}
					}
				}
				continue
			}
			nameNode := typeSpec.ChildByFieldName("name")
			if nameNode.IsNull() {
				continue
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			startLine := int(child.StartPoint().Row) + 1
			endLine := int(child.EndPoint().Row) + 1
			result[name] = goFuncInfo{
				doc:       extractDocComment(lines, startLine),
				lineCount: endLine - startLine + 1,
			}
		}
	}
	return result
}

// extractSignature returns the declaration signature (everything before the
// function body opening brace), normalised to a single line.
func extractSignature(declNode sitter.Node, src []byte) string {
	// Find the body block child — the signature is everything before it.
	for i := uint32(0); i < declNode.ChildCount(); i++ {
		child := declNode.Child(i)
		if !child.IsNull() && child.Type() == "block" {
			sig := string(src[declNode.StartByte():child.StartByte()])
			// Collapse newlines / excess whitespace in multi-line signatures.
			sig = strings.Join(strings.Fields(sig), " ")
			return strings.TrimSpace(sig)
		}
	}
	// No body (e.g. external function declaration) — return full text.
	return strings.TrimSpace(string(src[declNode.StartByte():declNode.EndByte()]))
}

// extractReceiverType returns the base type name from a method receiver
// parameter list, e.g. "(g *Graph)" → "Graph".
func extractReceiverType(receiverNode sitter.Node, src []byte) string {
	if receiverNode.IsNull() {
		return ""
	}
	for i := uint32(0); i < receiverNode.ChildCount(); i++ {
		param := receiverNode.Child(i)
		if param.IsNull() || param.Type() != "parameter_declaration" {
			continue
		}
		typeNode := param.ChildByFieldName("type")
		if typeNode.IsNull() {
			continue
		}
		switch typeNode.Type() {
		case "pointer_type":
			// *T — find the inner type_identifier
			for j := uint32(0); j < typeNode.ChildCount(); j++ {
				inner := typeNode.Child(j)
				if !inner.IsNull() && inner.Type() == "type_identifier" {
					return string(src[inner.StartByte():inner.EndByte()])
				}
			}
		case "type_identifier":
			return string(src[typeNode.StartByte():typeNode.EndByte()])
		}
	}
	return ""
}

// extractDocComment scans backwards from startLine (1-indexed) to collect any
// contiguous // comment lines immediately preceding the declaration.
// Returns the comment text joined into a single string, with the // prefix stripped.
func extractDocComment(lines []string, startLine int) string {
	if startLine < 2 {
		return ""
	}
	var parts []string
	for i := startLine - 2; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "//") {
			text := strings.TrimPrefix(trimmed, "//")
			text = strings.TrimPrefix(text, " ")
			parts = append([]string{text}, parts...)
		} else {
			// Any non-comment line (including blank lines) ends the search.
			break
		}
	}
	return strings.Join(parts, " ")
}

// extractCallSites recursively walks an AST subtree (typically a function body)
// and collects all call expressions as raw call sites for post-parse resolution.
// It captures:
//   - pkg.Func()          (selector: identifier operand)       → pkgAlias set
//   - var.Method()        (selector: identifier operand)       → pkgAlias set (resolved later)
//   - a.field.Method()   (selector: inner selector operand)   → pkgAlias = inner field
//   - Func()             (plain identifier)                    → pkgAlias empty
//
// Deeper chains (a.b.c.d()) are skipped.
func extractCallSites(node sitter.Node, src []byte) []rawCallSite {
	var calls []rawCallSite
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "call_expression" {
			fn := n.ChildByFieldName("function")
			if !fn.IsNull() {
				switch fn.Type() {
				case "selector_expression":
					operand := fn.ChildByFieldName("operand")
					field := fn.ChildByFieldName("field")
					if !operand.IsNull() && !field.IsNull() {
						name := string(src[field.StartByte():field.EndByte()])
						switch operand.Type() {
						case "identifier":
							// Simple: pkg.Func() or var.Method()
							pkg := string(src[operand.StartByte():operand.EndByte()])
							calls = append(calls, rawCallSite{pkgAlias: pkg, funcName: name})
						case "selector_expression":
							// 2-level: a.field.Method() — use inner field as alias
							// Handles s.graph.CarveEgoGraph(), self.db.Query(), etc.
							innerField := operand.ChildByFieldName("field")
							if !innerField.IsNull() {
								alias := string(src[innerField.StartByte():innerField.EndByte()])
								calls = append(calls, rawCallSite{pkgAlias: alias, funcName: name})
							}
						}
					}
				case "identifier":
					name := string(src[fn.StartByte():fn.EndByte()])
					if !goBuiltins[name] {
						calls = append(calls, rawCallSite{funcName: name})
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(node)
	return calls
}

// GoParser parses Go source files into graph nodes and edges using Tree-sitter.
type GoParser struct {
	language *sitter.Language
}

// NewGoParser creates a ready-to-use GoParser.
func NewGoParser() *GoParser {
	return &GoParser{
		language: sitter.NewLanguage(gositter.GetLanguage()),
	}
}

// Extensions returns the file extensions handled by this parser.
func (p *GoParser) Extensions() []string {
	return []string{".go"}
}

// Parse extracts code entities from a single Go source file and merges them
// into the provided graph. The following constructs are captured:
//
//   - Package declarations → NodePackage
//   - Import declarations  → NodeFile edges (IMPORTS)
//   - Function declarations → NodeFunction
//   - Method declarations  → NodeMethod
//   - Type declarations (struct) → NodeStruct
//   - Type declarations (interface) → NodeInterface
//   - Type aliases and named types → NodeStruct (metadata kind=type_alias)
//   - Package-level const declarations → NodeVariable (metadata kind=const)
//   - Package-level var declarations → NodeVariable (metadata kind=var)
//   - Function/method call expressions → CALLS edges
//
// Package-level var nodes use the line of the "var" keyword, not the line of
// any inferred type. For slice/map vars (var foo = []BarType{...}), the node
// name is "foo", the type is NodeVariable, and the element type does not affect
// classification (FIX-PARSER-2).
func (p *GoParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
	root := tree.RootNode()

	// First pass: extract package name.
	pkg := extractPackageName(root, src)

	// Ensure a file node exists.
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:      fileNodeID,
		Type:    graph.NodeFile,
		Name:    filepath.Base(filePath),
		Package: pkg,
		File:    filePath,
		Line:    1,
	})

	// Second pass: extract top-level declarations.
	return p.extractDeclarations(g, root, src, filePath, pkg, fileNodeID)
}

// extractPackageName walks child nodes to find the package clause identifier.
func extractPackageName(root sitter.Node, src []byte) string {
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "package_clause" {
			for j := uint32(0); j < child.ChildCount(); j++ {
				ident := child.Child(j)
				if !ident.IsNull() && ident.Type() == "package_identifier" {
					return string(src[ident.StartByte():ident.EndByte()])
				}
			}
		}
	}
	return "unknown"
}

// extractDeclarations walks the top-level statements and emits nodes/edges.
func (p *GoParser) extractDeclarations(
	g *graph.Graph,
	root sitter.Node,
	src []byte,
	filePath, pkg string,
	fileNodeID graph.NodeID,
) error {
	lang := p.language

	// Pre-build enriched metadata (signatures, doc comments, line counts) for
	// all top-level declarations in one AST walk, so the query loops below
	// can attach it without additional passes.
	declInfo := extractGoDeclarationInfo(root, src)

	// Pre-collect interface method names so they can be stored as metadata on
	// interface nodes, enabling ResolveImplementsEdges to detect struct satisfaction.
	ifaceMethods := extractInterfaceMethods(root, src)

	// Pre-collect struct field info for IMP-IMPL-2: compact format shows fields.
	structFields := extractGoStructFields(root, src)

	// --- Import declarations ---
	importQuery := `(import_spec path: (interpreted_string_literal) @import_path)`
	if err := runQuery(lang, root, src, importQuery, func(captures map[string]string, startLine int) {
		raw := captures["import_path"]
		raw = strings.Trim(raw, `"`)
		if raw == "" {
			return
		}
		// Create a node for the imported package and add an IMPORTS edge.
		// Use the full import path as Name so FindByPattern can match substrings.
		importNodeID := g.MakeNodeID(raw, raw)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    raw,
			Package: raw,
			File:    filePath,
			Line:    startLine,
		})
		g.AddEdge(&graph.Edge{
			From: fileNodeID,
			To:   importNodeID,
			Type: graph.EdgeImports,
		})
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
		info := declInfo[name]
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			Package:  pkg,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
			Metadata: buildMeta(info),
		})
		// File DEFINES function.
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		// Register call sites for post-parse resolution.
		for _, cs := range info.callSites {
			g.AddCallSite(graph.CallSite{
				CallerID:   nodeID,
				CallerFile: filePath,
				PkgAlias:   cs.pkgAlias,
				FuncName:   cs.funcName,
			})
		}
	}); err != nil {
		return err
	}

	// --- Method declarations ---
	// receiver type can be *T or T; we capture the type identifier.
	methodQuery := `
(method_declaration
  receiver: (parameter_list
    (parameter_declaration
      type: [
        (pointer_type (type_identifier) @receiver_type)
        (type_identifier) @receiver_type
      ]
    )
  )
  name: (field_identifier) @method_name
)`
	if err := runQuery(lang, root, src, methodQuery, func(captures map[string]string, startLine int) {
		methodName := captures["method_name"]
		receiverType := captures["receiver_type"]
		if methodName == "" {
			return
		}
		qualifiedName := receiverType + "." + methodName
		nodeID := g.MakeNodeID(filePath, qualifiedName)
		info := declInfo[qualifiedName]
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeMethod,
			Name:     qualifiedName,
			Package:  pkg,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(methodName),
			Metadata: buildMeta(info),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		// If the receiver struct is in this graph, add a DEFINES edge from it.
		if receiverType != "" {
			structID := g.MakeNodeID(filePath, receiverType)
			if g.GetNode(structID) != nil {
				g.AddEdge(&graph.Edge{From: structID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
		// Register call sites for post-parse resolution.
		for _, cs := range info.callSites {
			g.AddCallSite(graph.CallSite{
				CallerID:   nodeID,
				CallerFile: filePath,
				PkgAlias:   cs.pkgAlias,
				FuncName:   cs.funcName,
			})
		}
	}); err != nil {
		return err
	}

	// --- Struct type declarations ---
	structQuery := `
(type_declaration
  (type_spec
    name: (type_identifier) @type_name
    type: (struct_type)
  )
)`
	if err := runQuery(lang, root, src, structQuery, func(captures map[string]string, startLine int) {
		name := captures["type_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		meta := buildMeta(declInfo[name])
		if fields, ok := structFields[name]; ok && len(fields) > 0 {
			if meta == nil {
				meta = make(map[string]string)
			}
			meta["fields"] = strings.Join(fields, ",")
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			Package:  pkg,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Interface type declarations ---
	ifaceQuery := `
(type_declaration
  (type_spec
    name: (type_identifier) @type_name
    type: (interface_type)
  )
)`
	if err := runQuery(lang, root, src, ifaceQuery, func(captures map[string]string, startLine int) {
		name := captures["type_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		meta := buildMeta(declInfo[name])
		// Store declared method names so the implements resolver can match structs.
		if methods, ok := ifaceMethods[name]; ok && len(methods) > 0 {
			if meta == nil {
				meta = make(map[string]string)
			}
			meta["methods"] = strings.Join(methods, ",")
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeInterface,
			Name:     name,
			Package:  pkg,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		// Emit NodeMethod for each declared interface method so the graph can
		// traverse interface → method edges (e.g. "what does IAuthService expose?").
		if methods, ok := ifaceMethods[name]; ok {
			for _, methodName := range methods {
				qualName := name + "." + methodName
				methID := g.MakeNodeID(filePath, qualName)
				if g.GetNode(methID) != nil {
					continue
				}
				g.AddNode(&graph.Node{
					ID:       methID,
					Type:     graph.NodeMethod,
					Name:     qualName,
					Package:  pkg,
					File:     filePath,
					Line:     startLine,
					Exported: isExported(methodName),
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: methID, Type: graph.EdgeDefines})
				g.AddEdge(&graph.Edge{From: nodeID, To: methID, Type: graph.EdgeDefines})
			}
		}
	}); err != nil {
		return err
	}

	// --- Type alias and named type declarations (non-struct, non-interface) ---
	// Captures: type Foo = Bar, type ID int64, type Handler func(...), etc.
	// These are type_spec nodes whose type field is NOT struct_type or interface_type.
	// We collect all type_spec names and skip those already added above.
	typeAliasQuery := `
(type_declaration
  (type_spec
    name: (type_identifier) @type_name
  )
)`
	if err := runQuery(lang, root, src, typeAliasQuery, func(captures map[string]string, startLine int) {
		name := captures["type_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return // Already added as struct or interface.
		}
		meta := buildMeta(declInfo[name])
		if meta == nil {
			meta = make(map[string]string, 1)
		}
		meta["kind"] = "type_alias"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			Package:  pkg,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Package-level const declarations ---
	// Handles both single: `const Foo = 1` and grouped: `const ( A = 1; B = 2 )`
	// In grouped form the grammar wraps specs in a const_spec_list container node.
	emitConst := func(spec sitter.Node) {
		for j := uint32(0); j < spec.ChildCount(); j++ {
			child := spec.Child(j)
			if child.IsNull() {
				continue
			}
			if child.Type() == "identifier" {
				name := string(src[child.StartByte():child.EndByte()])
				nodeID := g.MakeNodeID(filePath, name)
				// No existence check: Graph.AddNode overwrites stale nodes from prior parses.
				// An existence guard here would prevent re-indexing from correcting a
				// misclassified node (e.g. a var previously stored as struct).
				meta := map[string]string{"kind": "const"}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeVariable,
					Name:     name,
					Package:  pkg,
					File:     filePath,
					Line:     int(child.StartPoint().Row) + 1,
					Exported: isExported(name),
					Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
	}
	var walkConst func(n sitter.Node)
	walkConst = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		// Don't descend into function/method bodies — local consts are noise.
		switch n.Type() {
		case "function_declaration", "method_declaration", "func_literal":
			return
		}
		if n.Type() == "const_declaration" {
			for i := uint32(0); i < n.ChildCount(); i++ {
				spec := n.Child(i)
				if spec.IsNull() {
					continue
				}
				switch spec.Type() {
				case "const_spec":
					emitConst(spec)
				case "const_spec_list":
					// Grouped: const ( A = 1; B = 2 ) — specs are children of the list.
					for k := uint32(0); k < spec.ChildCount(); k++ {
						inner := spec.Child(k)
						if !inner.IsNull() && inner.Type() == "const_spec" {
							emitConst(inner)
						}
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walkConst(n.Child(i))
		}
	}
	walkConst(root)

	// --- Package-level var declarations ---
	// Handles both single: `var Foo = 1` and grouped: `var ( A = 1; B = 2 )`
	// In grouped form the grammar wraps specs in a var_spec_list container node.
	emitVar := func(spec sitter.Node) {
		for j := uint32(0); j < spec.ChildCount(); j++ {
			child := spec.Child(j)
			if child.IsNull() {
				continue
			}
			if child.Type() == "identifier" {
				name := string(src[child.StartByte():child.EndByte()])
				nodeID := g.MakeNodeID(filePath, name)
				// No existence check: Graph.AddNode overwrites stale nodes from prior parses.
				// An existence guard here would prevent re-indexing from correcting a
				// misclassified node (e.g. a var previously stored as struct — FIX-PARSER-2).
				meta := map[string]string{"kind": "var"}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeVariable,
					Name:     name,
					Package:  pkg,
					File:     filePath,
					Line:     int(child.StartPoint().Row) + 1,
					Exported: isExported(name),
					Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
	}
	var walkVar func(n sitter.Node)
	walkVar = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		// Don't descend into function/method bodies — local vars are noise.
		switch n.Type() {
		case "function_declaration", "method_declaration", "func_literal":
			return
		}
		if n.Type() == "var_declaration" {
			for i := uint32(0); i < n.ChildCount(); i++ {
				spec := n.Child(i)
				if spec.IsNull() {
					continue
				}
				switch spec.Type() {
				case "var_spec":
					emitVar(spec)
				case "var_spec_list":
					// Grouped: var ( A = 1; B = 2 ) — specs are children of the list.
					for k := uint32(0); k < spec.ChildCount(); k++ {
						inner := spec.Child(k)
						if !inner.IsNull() && inner.Type() == "var_spec" {
							emitVar(inner)
						}
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walkVar(n.Child(i))
		}
	}
	walkVar(root)

	// --- True type aliases: type Foo = Bar (uses type_alias node in some grammar versions) ---
	// In certain go-tree-sitter versions, `type X = Y` is a `type_alias` node,
	// not a `type_spec`, so the query above misses them.  Walk the AST directly.
	var walkTypeAlias func(n sitter.Node)
	walkTypeAlias = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "type_alias" {
			// Structure varies by grammar version; look for the first type_identifier.
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "type_identifier" {
					name := string(src[child.StartByte():child.EndByte()])
					nodeID := g.MakeNodeID(filePath, name)
					if g.GetNode(nodeID) == nil {
						meta := buildMeta(declInfo[name])
						if meta == nil {
							meta = make(map[string]string, 1)
						}
						meta["kind"] = "type_alias"
						g.AddNode(&graph.Node{
							ID:       nodeID,
							Type:     graph.NodeStruct,
							Name:     name,
							Package:  pkg,
							File:     filePath,
							Line:     int(child.StartPoint().Row) + 1,
							Exported: isExported(name),
							Metadata: meta,
						})
						g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					}
					break // Only the first type_identifier is the alias name.
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walkTypeAlias(n.Child(i))
		}
	}
	walkTypeAlias(root)

	return nil
}

// runQuery compiles and executes a tree-sitter query, calling fn for every match.
// captures is a map of capture name → matched text. startLine is 1-indexed.
//
// If the query fails to compile (e.g. due to an unrecognised node type in the
// bundled grammar version), runQuery logs the error and returns nil — the parser
// continues without that extraction pass. This "degradation" approach means a
// parser always produces at least a file node, and wrong queries can be fixed
// without breaking every downstream consumer.
func runQuery(
	lang *sitter.Language,
	root sitter.Node,
	src []byte,
	queryStr string,
	fn func(captures map[string]string, startLine int),
) error {
	q, qerr := sitter.NewQuery(lang, []byte(queryStr))
	if qerr != nil {
		// Log to stderr so developers can see which queries need updating, but
		// treat this as a non-fatal degradation — we still produce a file node.
		fmt.Fprintf(os.Stderr, "synapses: tree-sitter query degraded (query compile error): %v\n", qerr)
		return nil
	}

	qc := sitter.NewQueryCursor()
	iter := qc.Matches(q, root, src)

	for {
		m := iter.Next()
		if m == nil {
			break
		}
		if len(m.Captures) == 0 {
			continue
		}
		captures := make(map[string]string, len(m.Captures))
		startLine := 0
		for _, c := range m.Captures {
			name := q.CaptureNameForID(c.Index)
			text := string(src[c.Node.StartByte():c.Node.EndByte()])
			captures[name] = text
			if startLine == 0 {
				startLine = int(c.Node.StartPoint().Row) + 1
			}
		}
		fn(captures, startLine)
	}

	return nil
}

// isExported returns true if the identifier begins with a Unicode uppercase letter.
// This correctly handles non-ASCII identifiers, which Go permits per the spec.
func isExported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return r != utf8.RuneError && unicode.IsUpper(r)
}

// buildMeta converts a goFuncInfo into the map[string]string metadata stored
// on graph nodes.  Only non-empty/non-zero fields are included to keep the
// SQLite blob compact.
func buildMeta(info goFuncInfo) map[string]string {
	if info.signature == "" && info.doc == "" && info.lineCount == 0 {
		return nil
	}
	m := make(map[string]string, 4)
	if info.signature != "" {
		m["signature"] = info.signature
	}
	if info.doc != "" {
		m["doc"] = info.doc
	}
	if info.lineCount > 0 {
		m["line_count"] = fmt.Sprintf("%d", info.lineCount)
	}
	if info.complexity > 0 {
		m["complexity"] = fmt.Sprintf("%d", info.complexity)
	}
	return m
}

// countComplexity counts the cyclomatic decision points in a function/method
// body node. Returns the raw count — callers should add 1 for the base path.
//
// Counts: if, for, expression_case, type_case, communication_case, and binary
// expressions using && or ||. Struct/interface bodies return 0 because they
// contain no control flow.
func countComplexity(node sitter.Node) int {
	if node.IsNull() {
		return 0
	}
	count := 0
	switch node.Type() {
	case "if_statement", "for_statement":
		count = 1
	case "expression_case", "type_case", "communication_case":
		// Each case clause in a switch or select adds one branch.
		count = 1
	case "binary_expression":
		// Short-circuit operators introduce an implicit branch.
		for i := uint32(0); i < node.ChildCount(); i++ {
			ch := node.Child(i)
			if !ch.IsNull() && (ch.Type() == "&&" || ch.Type() == "||") {
				count = 1
				break
			}
		}
	}
	for i := uint32(0); i < node.ChildCount(); i++ {
		count += countComplexity(node.Child(i))
	}
	return count
}
