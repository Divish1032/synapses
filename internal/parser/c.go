package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/c"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// extractCDeclInfo walks the C AST collecting metadata for function definitions,
// structs, enums, unions, and typedefs.
func extractCDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "function_definition":
			if name := extractCFuncName(n, src); name != "" {
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature:  extractSigToBodyMulti(n, src, []string{"compound_statement"}),
					Doc:        extractDocMulti(lines, sl, "//"),
					LineCount:  int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
					IfdefGuard: extractCIfdefGuard(n, src),
				}
			}
		case "struct_specifier", "enum_specifier", "union_specifier":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:        extractDocMulti(lines, sl, "//"),
					LineCount:  int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
					IfdefGuard: extractCIfdefGuard(n, src),
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

// extractCFuncName extracts the function name from a C function definition.
func extractCFuncName(n sitter.Node, src []byte) string {
	declarator := n.ChildByFieldName("declarator")
	if declarator.IsNull() {
		return ""
	}
	// function_declarator → declarator (identifier)
	if declarator.Type() == "function_declarator" {
		inner := declarator.ChildByFieldName("declarator")
		if !inner.IsNull() && inner.Type() == "identifier" {
			return string(src[inner.StartByte():inner.EndByte()])
		}
		// Could be pointer_declarator wrapping function_declarator
		if !inner.IsNull() && inner.Type() == "parenthesized_declarator" {
			for i := uint32(0); i < inner.ChildCount(); i++ {
				child := inner.Child(i)
				if !child.IsNull() && child.Type() == "pointer_declarator" {
					for j := uint32(0); j < child.ChildCount(); j++ {
						grandchild := child.Child(j)
						if !grandchild.IsNull() && grandchild.Type() == "identifier" {
							return string(src[grandchild.StartByte():grandchild.EndByte()])
						}
					}
				}
			}
		}
	}
	return ""
}

// extractCIfdefGuard walks up the AST from node n and returns the preprocessor
// condition text if n is nested inside a preproc_ifdef, preproc_if, preproc_else,
// or preproc_elif block. Returns "" when the node is unconditionally compiled.
// This lets callers annotate symbols with meta["ifdef"] = "WIN32" etc.
func extractCIfdefGuard(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	cur := n.Parent()
	for !cur.IsNull() {
		switch cur.Type() {
		case "preproc_ifdef":
			// #ifdef FOO  — the condition identifier is the 2nd child.
			for i := uint32(0); i < cur.ChildCount(); i++ {
				child := cur.Child(i)
				if !child.IsNull() && child.Type() == "identifier" {
					return string(src[child.StartByte():child.EndByte()])
				}
			}
		case "preproc_if":
			// #if EXPR — grab the raw condition text.
			for i := uint32(0); i < cur.ChildCount(); i++ {
				child := cur.Child(i)
				if !child.IsNull() && child.Type() == "preproc_expression" {
					return string(src[child.StartByte():child.EndByte()])
				}
			}
			// Fallback: second child is usually the expression.
			if cur.ChildCount() >= 2 {
				if c2 := cur.Child(1); !c2.IsNull() {
					cond := strings.TrimSpace(string(src[c2.StartByte():c2.EndByte()]))
					if cond != "" {
						return cond
					}
				}
			}
		case "preproc_else":
			// #else branch — negate the parent preproc_ifdef/preproc_if condition.
			if parent := cur.Parent(); !parent.IsNull() {
				switch parent.Type() {
				case "preproc_ifdef":
					for i := uint32(0); i < parent.ChildCount(); i++ {
						child := parent.Child(i)
						if !child.IsNull() && child.Type() == "identifier" {
							return "!" + string(src[child.StartByte():child.EndByte()])
						}
					}
				case "preproc_if":
					for i := uint32(0); i < parent.ChildCount(); i++ {
						child := parent.Child(i)
						if !child.IsNull() && child.Type() == "preproc_expression" {
							return "!(" + strings.TrimSpace(string(src[child.StartByte():child.EndByte()])) + ")"
						}
					}
				}
			}
			return "else"
		case "preproc_elif":
			for i := uint32(0); i < cur.ChildCount(); i++ {
				child := cur.Child(i)
				if !child.IsNull() && child.Type() == "preproc_expression" {
					return string(src[child.StartByte():child.EndByte()])
				}
			}
		}
		cur = cur.Parent()
	}
	return ""
}

// isCStatic checks if a C declaration has the `static` storage class specifier.
func isCStatic(n sitter.Node, src []byte) bool {
	if n.IsNull() {
		return false
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "storage_class_specifier" {
			text := string(src[child.StartByte():child.EndByte()])
			if text == "static" {
				return true
			}
		}
	}
	return false
}

// CParser parses C (.c, .h) source files.
type CParser struct {
	language *sitter.Language
}

// NewCParser creates a ready-to-use CParser.
func NewCParser() *CParser {
	return &CParser{language: sitter.NewLanguage(c.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *CParser) Extensions() []string {
	return []string{".c", ".h", ".ino"}
}

func (p *CParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single C file and merges them into the graph.
func (p *CParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree != nil {
		defer tree.Close()
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
	declInfo := extractCDeclInfo(root, src)

	// --- #include directives (string) ---
	includeQuery := `(preproc_include path: (string_literal) @include_path)`
	if err := runQuery(lang, root, src, includeQuery, func(captures map[string]string, _ int) {
		path := captures["include_path"]
		if path == "" {
			return
		}
		importNodeID := g.MakeNodeID(path, path)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    path,
			Package: path,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- system includes: #include <stdio.h> ---
	sysIncludeQuery := `(preproc_include path: (system_lib_string) @include_path)`
	if err := runQuery(lang, root, src, sysIncludeQuery, func(captures map[string]string, _ int) {
		path := captures["include_path"]
		if path == "" {
			return
		}
		importNodeID := g.MakeNodeID(path, path)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    path,
			Package: path,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- function definitions ---
	// Note: tree-sitter parses ALL branches of #ifdef/#if/#else, so this
	// query finds symbols from every preprocessor branch automatically.
	// The ifdef guard is captured in declInfo[name].IfdefGuard during the
	// extractCDeclInfo pre-pass and surfaced via buildLangMeta as meta["ifdef"].
	funcQuery := `(function_definition declarator: (function_declarator declarator: (identifier) @func_name))`
	if err := runQuery(lang, root, src, funcQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		// Check if this function definition's parent has `static`.
		isStatic := false
		srcLines := strings.Split(string(src), "\n")
		if startLine > 0 && startLine <= len(srcLines) {
			line := strings.TrimSpace(srcLines[startLine-1])
			isStatic = strings.HasPrefix(line, "static ")
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: !isStatic,
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- function prototypes (declarations in .h files) ---
	// These are `declaration` nodes with a function_declarator.
	protoQuery := `(declaration declarator: (function_declarator declarator: (identifier) @func_name))`
	if err := runQuery(lang, root, src, protoQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		// Only add if not already present from a function_definition.
		existingID := g.MakeNodeID(filePath, name)
		if g.GetNode(existingID) != nil {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- struct specifiers ---
	structQuery := `(struct_specifier name: (type_identifier) @struct_name)`
	if err := runQuery(lang, root, src, structQuery, func(captures map[string]string, startLine int) {
		name := captures["struct_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return // Avoid duplicates from forward declarations.
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- enum specifiers ---
	enumQuery := `(enum_specifier name: (type_identifier) @enum_name)`
	if err := runQuery(lang, root, src, enumQuery, func(captures map[string]string, startLine int) {
		name := captures["enum_name"]
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
		meta["kind"] = "enum"
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
	}); err != nil {
		return err
	}

	// --- union specifiers ---
	unionQuery := `(union_specifier name: (type_identifier) @union_name)`
	if err := runQuery(lang, root, src, unionQuery, func(captures map[string]string, startLine int) {
		name := captures["union_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
		meta := make(map[string]string, 1)
		meta["kind"] = "union"
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
	}); err != nil {
		return err
	}

	// --- typedef declarations ---
	// Walk the AST so we can inspect the underlying type (struct/enum/union/other)
	// and set metadata["kind"] accordingly.
	var walkTypedefs func(n sitter.Node)
	walkTypedefs = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "type_definition" {
			// The typedef name is the last type_identifier child (the alias).
			var typedefName string
			var startLine int
			typeKind := ""
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				switch child.Type() {
				case "struct_specifier":
					// Anonymous struct iff no name field: typedef struct { ... } MyType;
					if child.ChildByFieldName("name").IsNull() {
						typeKind = "struct"
					}
				case "enum_specifier":
					// Anonymous enum iff no name field.
					if child.ChildByFieldName("name").IsNull() {
						typeKind = "enum"
					}
				case "union_specifier":
					if child.ChildByFieldName("name").IsNull() {
						typeKind = "union"
					}
				case "function_declarator":
					// typedef int (*callback_fn)(int x, int y);
					// function_declarator → parenthesized_declarator → pointer_declarator → type_identifier
					typeKind = "function_pointer"
					if pd := firstChildOfType(child, "parenthesized_declarator"); !pd.IsNull() {
						if ptr := firstChildOfType(pd, "pointer_declarator"); !ptr.IsNull() {
							if ti := firstChildOfType(ptr, "type_identifier"); !ti.IsNull() {
								typedefName = string(src[ti.StartByte():ti.EndByte()])
								startLine = int(ti.StartPoint().Row) + 1
							}
						}
					}
				case "type_identifier":
					// Last type_identifier is the typedef alias name.
					typedefName = string(src[child.StartByte():child.EndByte()])
					startLine = int(child.StartPoint().Row) + 1
				}
			}
			if typedefName != "" {
				nodeID := g.MakeNodeID(filePath, typedefName)
				if g.GetNode(nodeID) == nil {
					var meta map[string]string
					if typeKind != "" {
						meta = map[string]string{"kind": typeKind}
					}
					g.AddNode(&graph.Node{
						ID:       nodeID,
						Type:     graph.NodeStruct,
						Name:     typedefName,
						File:     filePath,
						Line:     startLine,
						Exported: true,
						Metadata: meta,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walkTypedefs(n.Child(i))
		}
	}
	walkTypedefs(root)

	// --- Preprocessor function-like macros: #define FOO(x) ... ---
	macroFuncQuery := `(preproc_function_def name: (identifier) @macro_name)`
	_ = runQuery(lang, root, src, macroFuncQuery, func(captures map[string]string, startLine int) {
		name := captures["macro_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
		meta := make(map[string]string, 1)
		meta["kind"] = "macro"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- Preprocessor object-like macros: #define FOO 42 ---
	macroObjQuery := `(preproc_def name: (identifier) @macro_name)`
	_ = runQuery(lang, root, src, macroObjQuery, func(captures map[string]string, startLine int) {
		name := captures["macro_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
		meta := make(map[string]string, 1)
		meta["kind"] = "macro"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct, // Object-like macro is a constant/value
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- Call sites (function-level resolution) ---
	collectCCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// collectCCallSites performs function-level call site collection for C.
func collectCCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{}, // C has no classes
		FuncTypes:  map[string]bool{"function_definition": true},
		CallTypes:  map[string]bool{"call_expression": true},
		NameExtractor: func(n sitter.Node, src []byte) string {
			return extractCFuncName(n, src)
		},
		CalleeExtractor: func(n sitter.Node, src []byte) string {
			fn := n.ChildByFieldName("function")
			if !fn.IsNull() && fn.Type() == "identifier" {
				return string(src[fn.StartByte():fn.EndByte()])
			}
			return ""
		},
		IsBuiltin: isCBuiltin,
	})
}

// isCBuiltin returns true for common C stdlib functions.
func isCBuiltin(name string) bool {
	switch name {
	case "printf", "fprintf", "sprintf", "snprintf", "scanf", "sscanf",
		"malloc", "calloc", "realloc", "free",
		"memcpy", "memmove", "memset", "memcmp",
		"strlen", "strcpy", "strncpy", "strcat", "strncat", "strcmp", "strncmp",
		"fopen", "fclose", "fread", "fwrite", "fgets", "fputs", "fseek", "ftell",
		"exit", "abort", "atexit",
		"atoi", "atol", "atof", "strtol", "strtoul", "strtod",
		"assert", "sizeof":
		return true
	}
	return false
}
