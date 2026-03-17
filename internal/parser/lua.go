package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/lua"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// LuaParser parses Lua (.lua) source files.
type LuaParser struct {
	language *sitter.Language
}

// NewLuaParser creates a ready-to-use LuaParser.
func NewLuaParser() *LuaParser {
	return &LuaParser{language: sitter.NewLanguage(lua.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *LuaParser) Extensions() []string {
	return []string{".lua"}
}

// extractLuaCATS does a line-by-line pre-pass scanning for LuaCATS annotations
// (from lua-language-server) and emits graph nodes for classes and their fields.
//
// Supported annotations:
//
//	---@class ClassName [:ParentClass]   → NodeStruct with kind="class"
//	---@field [public|protected|private] fieldName type [description]
//	                                     → NodeMethod with kind="field" on the last seen class
//	---@alias AliasName type             → NodeStruct with kind="alias"
//	---@type type (on local x = ...)     → stored as metadata on next local assignment
func extractLuaCATS(g *graph.Graph, src []byte, filePath string, fileNodeID graph.NodeID) {
	lines := strings.Split(string(src), "\n")
	var currentClass string

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		// ---@class ClassName or ---@class ClassName : Parent
		if strings.HasPrefix(trimmed, "---@class ") {
			rest := strings.TrimPrefix(trimmed, "---@class ")
			rest = strings.TrimSpace(rest)
			// Strip any parent class annotation "ClassName : Parent"
			parts := strings.SplitN(rest, ":", 2)
			className := strings.TrimSpace(parts[0])
			// Also handle space-separated parent: "ClassName Parent"
			if idx := strings.IndexByte(className, ' '); idx != -1 {
				className = className[:idx]
			}
			if className == "" {
				continue
			}
			currentClass = className
			nodeID := g.MakeNodeID(filePath, className)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "class", "source": "luacats"}
				if len(parts) == 2 {
					meta["extends"] = strings.TrimSpace(parts[1])
				}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeStruct,
					Name:     className,
					File:     filePath,
					Line:     lineNum,
					Exported: isExported(className),
					Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
			continue
		}

		// ---@field [public|protected|private] fieldName type
		if strings.HasPrefix(trimmed, "---@field ") && currentClass != "" {
			rest := strings.TrimPrefix(trimmed, "---@field ")
			rest = strings.TrimSpace(rest)
			// Strip optional visibility modifier
			for _, vis := range []string{"public ", "protected ", "private "} {
				if strings.HasPrefix(rest, vis) {
					rest = strings.TrimPrefix(rest, vis)
					break
				}
			}
			// Field name is the first token
			tokens := strings.Fields(rest)
			if len(tokens) == 0 {
				continue
			}
			fieldName := tokens[0]
			qualName := currentClass + "." + fieldName
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "field", "source": "luacats"}
				if len(tokens) > 1 {
					meta["field_type"] = tokens[1]
				}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeMethod,
					Name:     qualName,
					File:     filePath,
					Line:     lineNum,
					Exported: true,
					Metadata: meta,
				})
				classNodeID := g.MakeNodeID(filePath, currentClass)
				if g.GetNode(classNodeID) != nil {
					g.AddEdge(&graph.Edge{From: classNodeID, To: nodeID, Type: graph.EdgeDefines})
				} else {
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
			continue
		}

		// ---@alias AliasName type
		if strings.HasPrefix(trimmed, "---@alias ") {
			rest := strings.TrimPrefix(trimmed, "---@alias ")
			rest = strings.TrimSpace(rest)
			tokens := strings.Fields(rest)
			if len(tokens) == 0 {
				continue
			}
			aliasName := tokens[0]
			nodeID := g.MakeNodeID(filePath, aliasName)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "alias", "source": "luacats"}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeStruct,
					Name:     aliasName,
					File:     filePath,
					Line:     lineNum,
					Exported: isExported(aliasName),
					Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
			// Reset current class context — alias breaks field association
			currentClass = ""
			continue
		}

		// Reset class context on non-annotation, non-empty lines (a blank or
		// non-comment line means the @class block ended).
		if trimmed != "" && !strings.HasPrefix(trimmed, "---") && !strings.HasPrefix(trimmed, "--") {
			// If the next real code line is not a table/local assignment that
			// follows the annotation block, clear class context.
			// We keep it for table field assignments that follow the annotation.
			if !strings.HasPrefix(trimmed, "local ") &&
				!strings.HasPrefix(trimmed, "M.") &&
				!strings.HasPrefix(trimmed, "return ") {
				currentClass = ""
			}
		}
	}
}

// extractLuaDeclInfo collects metadata for Lua declarations.
func extractLuaDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "function_declaration" {
			sl := int(n.StartPoint().Row) + 1
			// Try to extract function name from various children.
			if dotIdx := firstChildOfType(n, "dot_index_expression"); !dotIdx.IsNull() {
				name := string(src[dotIdx.StartByte():dotIdx.EndByte()])
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "--"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			} else if methIdx := firstChildOfType(n, "method_index_expression"); !methIdx.IsNull() {
				name := string(src[methIdx.StartByte():methIdx.EndByte()])
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "--"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			} else if ident := firstChildOfType(n, "identifier"); !ident.IsNull() {
				name := string(src[ident.StartByte():ident.EndByte()])
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "--"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return result
}

// Parse extracts code entities from a single Lua file.
func (p *LuaParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// --- LuaCATS annotation pre-pass (---@class, ---@field, ---@alias) ---
	extractLuaCATS(g, src, filePath, fileNodeID)

	lang := p.language
	declInfo := extractLuaDeclInfo(root, src)

	// --- require() calls as imports ---
	// The Lua grammar wraps string args in function_arguments, so we walk
	// function_call nodes directly rather than using a query.
	var walkRequire func(n sitter.Node)
	walkRequire = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "function_call" {
			// Check if the callee identifier is "require".
			isRequire := false
			var reqPath string
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "identifier" {
					if string(src[child.StartByte():child.EndByte()]) == "require" {
						isRequire = true
					}
				}
				if child.Type() == "function_arguments" || child.Type() == "arguments" {
					for j := uint32(0); j < child.ChildCount(); j++ {
						arg := child.Child(j)
						if arg.IsNull() {
							continue
						}
						if arg.Type() == "string" {
							rawStr := string(src[arg.StartByte():arg.EndByte()])
							reqPath = strings.Trim(rawStr, `"'`)
						}
					}
				}
			}
			if isRequire && reqPath != "" {
				importNodeID := g.MakeNodeID(reqPath, reqPath)
				g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: reqPath, Package: reqPath, File: filePath})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walkRequire(n.Child(i))
		}
	}
	walkRequire(root)

	// --- global function declarations: function Foo() end ---
	// NOTE: the query also fires for each identifier component inside a qualified
	// function_name (e.g. "Vector2" and "new" for `function Vector2.new()`).
	// We skip if the node was already added (e.g. by the LuaCATS pre-pass) to
	// avoid overwriting class/alias nodes with empty function nodes.
	globalFuncQuery := `(function_declaration (identifier) @func_name)`
	if err := runQuery(lang, root, src, globalFuncQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return // already added (LuaCATS class/alias or duplicate)
		}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
			Line: startLine, Exported: isExported(name), Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- method-style functions: function Foo.bar() or function Foo:bar() ---
	methodHandler := func(captures map[string]string, startLine int) {
		fullName := captures["full_name"]
		if fullName == "" || !strings.ContainsAny(fullName, ".:") {
			return // Already captured by global func query.
		}
		// Convert Foo.bar or Foo:bar to Foo.bar.
		qualName := strings.ReplaceAll(fullName, ":", ".")
		// Don't re-add if it's a simple name already captured.
		nodeID := g.MakeNodeID(filePath, qualName)
		if g.GetNode(nodeID) != nil {
			return
		}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
			Line: startLine, Exported: true, Metadata: buildLangMeta(declInfo[fullName]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
	_ = runQuery(lang, root, src, `(function_declaration (dot_index_expression) @full_name)`, methodHandler)
	_ = runQuery(lang, root, src, `(function_declaration (method_index_expression) @full_name)`, methodHandler)

	// --- local function declarations: local function foo() end ---
	localFuncQuery := `(function_declaration "local" (identifier) @func_name)`
	if err := runQuery(lang, root, src, localFuncQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
			Line: startLine, Exported: false, Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Table field function assignments: M.foo = function() end ---
	// In the new grammar this is an assignment_statement (possibly inside variable_declaration):
	//   assignment_statement > variable_list > dot_index_expression/bracket_index_expression
	//   assignment_statement > expression_list > function_definition
	var walkTableFuncs func(n sitter.Node)
	walkTableFuncs = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		nt := n.Type()
		if nt == "assignment_statement" {
			// Check if expression_list contains a function_definition.
			exprList := firstChildOfType(n, "expression_list")
			hasFuncVal := false
			if !exprList.IsNull() {
				for i := uint32(0); i < exprList.ChildCount(); i++ {
					c := exprList.Child(i)
					if !c.IsNull() && c.Type() == "function_definition" {
						hasFuncVal = true
						break
					}
				}
			}
			if hasFuncVal {
				varList := firstChildOfType(n, "variable_list")
				if !varList.IsNull() {
					for i := uint32(0); i < varList.ChildCount(); i++ {
						varNode := varList.Child(i)
						if varNode.IsNull() {
							continue
						}
						vt := varNode.Type()
						var qualName string
						if vt == "dot_index_expression" || vt == "bracket_index_expression" {
							qualName = extractLuaTableFieldName(varNode, src)
						}
						if qualName == "" {
							continue
						}
						nodeID := g.MakeNodeID(filePath, qualName)
						if g.GetNode(nodeID) != nil {
							continue
						}
						g.AddNode(&graph.Node{
							ID: nodeID, Type: graph.NodeFunction, Name: qualName, File: filePath,
							Line: int(varNode.StartPoint().Row) + 1, Exported: true,
							Metadata: map[string]string{"kind": "table_func"},
						})
						g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walkTableFuncs(n.Child(i))
		}
	}
	walkTableFuncs(root)

	// --- Call sites ---
	collectLuaCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractLuaTableFieldName extracts a qualified "Table.field" name from a
// variable_declarator node. Handles both dot notation (M.foo) and bracket
// notation with string literals (M["foo"]). Returns "" for plain identifiers
// or non-string bracket keys (e.g. M[i]).
func extractLuaTableFieldName(varNode sitter.Node, src []byte) string {
	if varNode.IsNull() {
		return ""
	}
	// Collect children skipping punctuation (. [ ] ,).
	var parts []string
	hasBracket := false
	for i := uint32(0); i < varNode.ChildCount(); i++ {
		child := varNode.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			text := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			if text != "" {
				parts = append(parts, text)
			}
		case "string":
			hasBracket = true
			// Extract the string_content child (strips quotes).
			for j := uint32(0); j < child.ChildCount(); j++ {
				sc := child.Child(j)
				if !sc.IsNull() && sc.Type() == "string_content" {
					parts = append(parts, string(src[sc.StartByte():sc.EndByte()]))
					break
				}
			}
		}
	}
	if len(parts) < 2 {
		return "" // plain variable, not a table field
	}
	_ = hasBracket
	return strings.Join(parts, ".")
}

// collectLuaCallSites collects call sites.
func collectLuaCallSites(g *graph.Graph, lang *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	callQuery := `(function_call (identifier) @callee)`
	_ = runQuery(lang, root, src, callQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || callee == "require" || callee == "print" || callee == "error" ||
			callee == "type" || callee == "tostring" || callee == "tonumber" ||
			callee == "pairs" || callee == "ipairs" || callee == "next" ||
			callee == "select" || callee == "pcall" || callee == "xpcall" ||
			callee == "assert" || callee == "rawget" || callee == "rawset" ||
			callee == "setmetatable" || callee == "getmetatable" ||
			callee == "unpack" || callee == "table" || callee == "string" ||
			callee == "math" || callee == "io" || callee == "os" || callee == "debug" {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
	})
}
