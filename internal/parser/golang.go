package parser

import (
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	gositter "github.com/alexaandru/go-sitter-forest/go"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
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
		if child.IsNull() || nodeType(child) != "type_declaration" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			spec := child.Child(j)
			if spec.IsNull() || nodeType(spec) != "type_spec" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			typeNode := spec.ChildByFieldName("type")
			if nameNode.IsNull() || typeNode.IsNull() || nodeType(typeNode) != "struct_type" {
				continue
			}
			structName := string(src[nameNode.StartByte():nameNode.EndByte()])
			var fields []string
			// Walk struct_type children to find field_declaration_list.
			for k := uint32(0); k < typeNode.ChildCount(); k++ {
				listNode := typeNode.Child(k)
				if listNode.IsNull() || nodeType(listNode) != "field_declaration_list" {
					continue
				}
				for l := uint32(0); l < listNode.ChildCount(); l++ {
					if len(fields) >= 15 {
						break
					}
					fieldDecl := listNode.Child(l)
					if fieldDecl.IsNull() || nodeType(fieldDecl) != "field_declaration" {
						continue
					}
					// Collect all field_identifier children (the names).
					var names []string
					for m := uint32(0); m < fieldDecl.ChildCount(); m++ {
						fc := fieldDecl.Child(m)
						if !fc.IsNull() && nodeType(fc) == "field_identifier" {
							names = append(names, string(src[fc.StartByte():fc.EndByte()]))
						}
					}
					// Use the named "type" field for reliable type extraction — avoids
					// guessing by position and correctly handles pointer, slice, map,
					// chan, and func types whose child ordering varies.
					var typeText string
					if tn := fieldDecl.ChildByFieldName("type"); !tn.IsNull() {
						typeText = strings.Join(strings.Fields(string(src[tn.StartByte():tn.EndByte()])), " ")
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
	// First pass: collect all declared method names per interface (own methods only).
	// Second pass: resolve embedded interfaces and merge their methods in.
	ownMethods := make(map[string][]string)
	embeds := make(map[string][]string) // ifaceName → list of embedded interface names

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() || nodeType(child) != "type_declaration" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			spec := child.Child(j)
			if spec.IsNull() || nodeType(spec) != "type_spec" {
				continue
			}
			nameNode := spec.ChildByFieldName("name")
			typeNode := spec.ChildByFieldName("type")
			if nameNode.IsNull() || typeNode.IsNull() || nodeType(typeNode) != "interface_type" {
				continue
			}
			ifaceName := string(src[nameNode.StartByte():nameNode.EndByte()])
			var methods []string
			for k := uint32(0); k < typeNode.ChildCount(); k++ {
				elem := typeNode.Child(k)
				if elem.IsNull() {
					continue
				}
				switch nodeType(elem) {
				case "method_elem":
					// Directly declared method: first field_identifier is the name.
					for l := uint32(0); l < elem.ChildCount(); l++ {
						nc := elem.Child(l)
						if !nc.IsNull() && nodeType(nc) == "field_identifier" {
							if m := string(src[nc.StartByte():nc.EndByte()]); m != "" {
								methods = append(methods, m)
							}
							break
						}
					}
				case "type_elem":
					// Embedded interface (Go 1.18+ union elements also use type_elem).
					// Find a type_identifier child — that is the embedded interface name.
					for l := uint32(0); l < elem.ChildCount(); l++ {
						nc := elem.Child(l)
						if nc.IsNull() {
							continue
						}
						if nodeType(nc) == "type_identifier" {
							embedded := string(src[nc.StartByte():nc.EndByte()])
							if embedded != "" && embedded != ifaceName {
								embeds[ifaceName] = append(embeds[ifaceName], embedded)
							}
						}
					}
				}
			}
			ownMethods[ifaceName] = methods
		}
	}

	// Merge embedded methods (one level deep; avoids circular-embed infinite loops
	// which are illegal in Go anyway).
	result := make(map[string][]string, len(ownMethods))
	for name, methods := range ownMethods {
		merged := make([]string, 0, len(methods)+4)
		merged = append(merged, methods...)
		seen := make(map[string]bool, len(methods))
		for _, m := range methods {
			seen[m] = true
		}
		for _, embeddedName := range embeds[name] {
			for _, m := range ownMethods[embeddedName] {
				if !seen[m] {
					seen[m] = true
					merged = append(merged, m)
				}
			}
		}
		if len(merged) > 0 {
			result[name] = merged
		}
	}
	return result
}

// extractGoDeclarationInfo walks the top-level children of the source_file node
// and collects signature, doc comment, and line-count for every function,
// method, and struct/interface declaration.  The returned map is keyed by the
// unqualified name (for functions/types) or "ReceiverType.MethodName" (for
// methods) — matching the name strings used when creating graph nodes.
func extractGoDeclarationInfo(root sitter.Node, src []byte, lines []string) map[string]goFuncInfo {
	result := make(map[string]goFuncInfo)

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}

		switch nodeType(child) {
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
			// Handles struct, interface, and type alias declarations.
			typeSpec := child.Child(0)
			if typeSpec.IsNull() || nodeType(typeSpec) != "type_spec" {
				// Multi-spec: type ( Foo struct{} Bar interface{} )
				// Walk children to find type_spec nodes.
				for j := uint32(0); j < child.ChildCount(); j++ {
					spec := child.Child(j)
					if !spec.IsNull() && nodeType(spec) == "type_spec" {
						nameNode := spec.ChildByFieldName("name")
						if nameNode.IsNull() {
							continue
						}
						name := string(src[nameNode.StartByte():nameNode.EndByte()])
						startLine := int(child.StartPoint().Row) + 1
						endLine := int(child.EndPoint().Row) + 1
						result[name] = goFuncInfo{
							signature: goTypeSignature(spec, src),
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
				signature: goTypeSignature(typeSpec, src),
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
		if !child.IsNull() && nodeType(child) == "block" {
			sig := string(src[declNode.StartByte():child.StartByte()])
			// Collapse newlines / excess whitespace in multi-line signatures.
			sig = strings.Join(strings.Fields(sig), " ")
			return strings.TrimSpace(sig)
		}
	}
	// No body (e.g. external function declaration) — return full text.
	return strings.TrimSpace(string(src[declNode.StartByte():declNode.EndByte()]))
}

// goTypeSignature builds a concise signature for a Go type_spec node.
//
//	type User struct { ... }          → "type User struct"
//	type IService interface { ... }   → "type IService interface"
//	type Node[K any] struct { ... }   → "type Node[K any] struct"
//	type Handler func(w http.ResponseWriter, r *http.Request)  → full normalized text
//	type UserID int64                 → "type UserID int64"
//
// For struct/interface, only the declaration header is returned (no body/fields).
// For all other types (aliases, named types, function types), the full normalized
// spec text is returned — these have no body, so the full text IS the signature.
func goTypeSignature(spec sitter.Node, src []byte) string {
	nameNode := spec.ChildByFieldName("name")
	typeNode := spec.ChildByFieldName("type")
	if nameNode.IsNull() || typeNode.IsNull() {
		return ""
	}
	switch nodeType(typeNode) {
	case "struct_type", "interface_type":
		// Header only: "type Name [type_params] struct|interface"
		// Extract text from name identifier up to (not including) the struct/interface body.
		mid := strings.TrimSpace(strings.Join(strings.Fields(
			string(src[nameNode.StartByte():typeNode.StartByte()])), " "))
		typKw := "struct"
		if nodeType(typeNode) == "interface_type" {
			typKw = "interface"
		}
		return "type " + mid + " " + typKw
	default:
		// Type alias, named type, function type — the spec IS its own signature.
		return "type " + strings.TrimSpace(strings.Join(strings.Fields(
			string(src[spec.StartByte():spec.EndByte()])), " "))
	}
}

// extractReceiverType returns the base type name from a method receiver
// parameter list, e.g. "(g *Graph)" → "Graph".
func extractReceiverType(receiverNode sitter.Node, src []byte) string {
	if receiverNode.IsNull() {
		return ""
	}
	for i := uint32(0); i < receiverNode.ChildCount(); i++ {
		param := receiverNode.Child(i)
		if param.IsNull() || nodeType(param) != "parameter_declaration" {
			continue
		}
		typeNode := param.ChildByFieldName("type")
		if typeNode.IsNull() {
			continue
		}
		switch nodeType(typeNode) {
		case "pointer_type":
			// *T or *T[E] — find the inner type_identifier (may be inside generic_type)
			for j := uint32(0); j < typeNode.ChildCount(); j++ {
				inner := typeNode.Child(j)
				if inner.IsNull() {
					continue
				}
				if nodeType(inner) == "type_identifier" {
					return string(src[inner.StartByte():inner.EndByte()])
				}
				if nodeType(inner) == "generic_type" {
					// *T[E] — first type_identifier child of generic_type is the base name
					for k := uint32(0); k < inner.ChildCount(); k++ {
						gc := inner.Child(k)
						if !gc.IsNull() && nodeType(gc) == "type_identifier" {
							return string(src[gc.StartByte():gc.EndByte()])
						}
					}
				}
			}
		case "type_identifier":
			return string(src[typeNode.StartByte():typeNode.EndByte()])
		case "generic_type":
			// T[E] — first type_identifier child is the base name
			for j := uint32(0); j < typeNode.ChildCount(); j++ {
				inner := typeNode.Child(j)
				if !inner.IsNull() && nodeType(inner) == "type_identifier" {
					return string(src[inner.StartByte():inner.EndByte()])
				}
			}
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
		if nodeType(n) == "call_expression" {
			fn := n.ChildByFieldName("function")
			if !fn.IsNull() {
				switch nodeType(fn) {
				case "selector_expression":
					operand := fn.ChildByFieldName("operand")
					field := fn.ChildByFieldName("field")
					if !operand.IsNull() && !field.IsNull() {
						name := string(src[field.StartByte():field.EndByte()])
						switch nodeType(operand) {
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

// TSLanguageForFile returns the tree-sitter language for this parser.
func (p *GoParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

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
		if nodeType(child) == "package_clause" {
			for j := uint32(0); j < child.ChildCount(); j++ {
				ident := child.Child(j)
				if !ident.IsNull() && nodeType(ident) == "package_identifier" {
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

	// Split source into lines once and pass to helpers to avoid repeated
	// O(n) allocations inside each declaration extraction helper.
	lines := strings.Split(string(src), "\n")

	// Pre-build enriched metadata (signatures, doc comments, line counts) for
	// all top-level declarations in one AST walk, so the query loops below
	// can attach it without additional passes.
	declInfo := extractGoDeclarationInfo(root, src, lines)

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

	// Capture explicit import aliases (e.g., `import alias "pkg/path"`).
	// Go allows renaming imports so the call-site qualifier differs from
	// path.Base(importPath). A separate query avoids relying on optional-
	// capture semantics which vary across tree-sitter binding versions.
	aliasQuery := `(import_spec name: (package_identifier) @alias path: (interpreted_string_literal) @path)`
	_ = runQuery(lang, root, src, aliasQuery, func(captures map[string]string, _ int) {
		alias := captures["alias"]
		raw := strings.Trim(captures["path"], `"`)
		if alias != "" && alias != "_" && alias != "." && raw != "" {
			g.AddImportAlias(filePath, alias, raw)
		}
	})

	// Pre-allocate a batch for call sites collected across all functions/methods
	// in this file. BulkAddCallSites is called once at the end to amortise lock overhead.
	var callSiteBatch []graph.CallSite

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
		// Collect call sites for bulk registration.
		for _, cs := range info.callSites {
			callSiteBatch = append(callSiteBatch, graph.CallSite{
				CallerID:   nodeID,
				CallerFile: filePath,
				PkgAlias:   cs.pkgAlias,
				FuncName:   cs.funcName,
			})
		}
	}); err != nil {
		return err
	}

	// --- Struct type declarations (BEFORE methods so struct→method DEFINES edges work) ---
	// Must be processed before method declarations. The method query below checks
	// g.GetNode(structID) to add struct→method DEFINES edges. If struct nodes are
	// added AFTER methods (original order), GetNode always returns nil and no
	// DEFINES edges are emitted — breaking impact_analysis for struct types.
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

	// --- Interface type declarations (BEFORE methods so interface→method DEFINES edges work) ---
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

	// --- Method declarations ---
	// receiver type can be *T, T, *T[E] (generic pointer), or T[E] (generic value).
	// The generic forms use a generic_type node wrapping the type_identifier.
	methodQuery := `
(method_declaration
  receiver: (parameter_list
    (parameter_declaration
      type: [
        (pointer_type (type_identifier) @receiver_type)
        (pointer_type (generic_type (type_identifier) @receiver_type))
        (type_identifier) @receiver_type
        (generic_type (type_identifier) @receiver_type)
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
		// Collect call sites for bulk registration.
		for _, cs := range info.callSites {
			callSiteBatch = append(callSiteBatch, graph.CallSite{
				CallerID:   nodeID,
				CallerFile: filePath,
				PkgAlias:   cs.pkgAlias,
				FuncName:   cs.funcName,
			})
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
			if nodeType(child) == "identifier" {
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
		switch nodeType(n) {
		case "function_declaration", "method_declaration", "func_literal":
			return
		}
		if nodeType(n) == "const_declaration" {
			for i := uint32(0); i < n.ChildCount(); i++ {
				spec := n.Child(i)
				if spec.IsNull() {
					continue
				}
				switch nodeType(spec) {
				case "const_spec":
					emitConst(spec)
				case "const_spec_list":
					// Grouped: const ( A = 1; B = 2 ) — specs are children of the list.
					for k := uint32(0); k < spec.ChildCount(); k++ {
						inner := spec.Child(k)
						if !inner.IsNull() && nodeType(inner) == "const_spec" {
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
			if nodeType(child) == "identifier" {
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
		switch nodeType(n) {
		case "function_declaration", "method_declaration", "func_literal":
			return
		}
		if nodeType(n) == "var_declaration" {
			for i := uint32(0); i < n.ChildCount(); i++ {
				spec := n.Child(i)
				if spec.IsNull() {
					continue
				}
				switch nodeType(spec) {
				case "var_spec":
					emitVar(spec)
				case "var_spec_list":
					// Grouped: var ( A = 1; B = 2 ) — specs are children of the list.
					for k := uint32(0); k < spec.ChildCount(); k++ {
						inner := spec.Child(k)
						if !inner.IsNull() && nodeType(inner) == "var_spec" {
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
		if nodeType(n) == "type_alias" {
			// Structure varies by grammar version; look for the first type_identifier.
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if nodeType(child) == "type_identifier" {
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

	// Flush all collected call sites in one bulk operation.
	g.BulkAddCallSites(callSiteBatch)

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
		logutil.Warn("synapses: tree-sitter query degraded (query compile error): %v\n", qerr)
		return nil
	}

	qc := sitter.NewQueryCursor()
	iter := qc.Matches(q, root, src)

	// Reuse a single captures map across matches to reduce allocations.
	// The map is cleared between matches instead of reallocated.
	captures := make(map[string]string, 8)

	for {
		m := iter.Next()
		if m == nil {
			break
		}
		if len(m.Captures) == 0 {
			continue
		}
		// Clear map for reuse (faster than reallocating).
		for k := range captures {
			delete(captures, k)
		}
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
	if info.signature == "" && info.doc == "" && info.lineCount == 0 && info.complexity == 0 {
		return nil
	}
	// Count non-empty fields to right-size the map and avoid over-allocation.
	n := 0
	if info.signature != "" {
		n++
	}
	if info.doc != "" {
		n++
	}
	if info.lineCount > 0 {
		n++
	}
	if info.complexity > 0 {
		n++
	}
	m := make(map[string]string, n)
	if info.signature != "" {
		m["signature"] = info.signature
	}
	if info.doc != "" {
		m["doc"] = info.doc
	}
	if info.lineCount > 0 {
		m["line_count"] = strconv.Itoa(info.lineCount)
	}
	if info.complexity > 0 {
		m["complexity"] = strconv.Itoa(info.complexity)
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
	switch nodeType(node) {
	case "if_statement", "for_statement":
		count = 1
	case "expression_case", "type_case", "communication_case":
		// Each case clause in a switch or select adds one branch.
		count = 1
	case "binary_expression":
		// Short-circuit operators introduce an implicit branch.
		for i := uint32(0); i < node.ChildCount(); i++ {
			ch := node.Child(i)
			if !ch.IsNull() && (nodeType(ch) == "&&" || nodeType(ch) == "||") {
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
