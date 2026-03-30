package parser

import (
	"path/filepath"
	"strings"

	"github.com/alexaandru/go-sitter-forest/elm"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ElmParser parses Elm (.elm) source files.
type ElmParser struct {
	language *sitter.Language
}

// NewElmParser creates a ready-to-use ElmParser.
func NewElmParser() *ElmParser {
	return &ElmParser{language: sitter.NewLanguage(elm.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *ElmParser) Extensions() []string {
	return []string{".elm"}
}

// TSLanguageForFile returns the tree-sitter language for this parser.
func (p *ElmParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// extractElmDeclInfo does a pre-pass to collect metadata (doc comments,
// type annotations, line counts) for Elm declarations.
func extractElmDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	// First pass: collect type annotations so we can pair them with value_declarations.
	annotations := make(map[string]string)  // name → signature text
	annotationLines := make(map[string]int) // name → 1-indexed start line of type_annotation

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		switch nodeType(child) {
		case "type_annotation":
			name, sig := extractElmTypeAnnotation(child, src)
			if name != "" {
				annotations[name] = sig
				annotationLines[name] = int(child.StartPoint().Row) + 1
			}

		case "value_declaration":
			name := extractElmValueDeclName(child, src)
			if name == "" {
				continue
			}
			// If there's a type annotation, look for doc comments above it
			// (not above the value_declaration which follows it).
			docLine := int(child.StartPoint().Row) + 1
			if annLine, ok := annotationLines[name]; ok {
				docLine = annLine
			}
			dm := declMeta{
				Doc:       extractElmDoc(lines, docLine),
				LineCount: int(child.EndPoint().Row) - int(child.StartPoint().Row) + 1,
			}
			if sig, ok := annotations[name]; ok {
				dm.Signature = sig
			}
			result[name] = dm

		case "type_alias_declaration":
			name := extractElmTypeName(child, src)
			if name == "" {
				continue
			}
			sl := int(child.StartPoint().Row) + 1
			result[name] = declMeta{
				Doc:       extractElmDoc(lines, sl),
				LineCount: int(child.EndPoint().Row) - int(child.StartPoint().Row) + 1,
			}

		case "type_declaration":
			name := extractElmTypeName(child, src)
			if name == "" {
				continue
			}
			sl := int(child.StartPoint().Row) + 1
			result[name] = declMeta{
				Doc:       extractElmDoc(lines, sl),
				LineCount: int(child.EndPoint().Row) - int(child.StartPoint().Row) + 1,
			}

		case "port_annotation":
			name := extractElmPortName(child, src)
			if name == "" {
				continue
			}
			sl := int(child.StartPoint().Row) + 1
			dm := declMeta{
				Doc:       extractElmDoc(lines, sl),
				LineCount: int(child.EndPoint().Row) - int(child.StartPoint().Row) + 1,
			}
			// Port annotations are also type annotations, extract signature.
			sig := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			dm.Signature = sig
			result[name] = dm
		}
	}
	return result
}

// extractElmDoc extracts doc comments preceding a declaration at startLine (1-indexed).
// Elm uses `{- | ... -}` for block doc comments and `-- | ...` or `-- ...` for line comments.
func extractElmDoc(lines []string, startLine int) string {
	if startLine < 2 || len(lines) == 0 {
		return ""
	}

	// Check for block comment {- | ... -} ending before startLine.
	for i := startLine - 2; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		// Check if the line above is the end of a block comment.
		if strings.HasSuffix(trimmed, "-}") {
			return extractElmBlockDoc(lines, i)
		}
		// Check for line doc comments (-- | or plain --)
		if strings.HasPrefix(trimmed, "-- ") || strings.HasPrefix(trimmed, "--\t") || trimmed == "--" {
			return extractLineDoc(lines, startLine, "--")
		}
		break
	}
	return ""
}

// extractElmBlockDoc extracts a block doc comment ending at line endIdx.
// It scans backward to find the opening `{-` and extracts the content.
func extractElmBlockDoc(lines []string, endIdx int) string {
	startIdx := endIdx
	for i := endIdx; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "{-") {
			startIdx = i
			break
		}
	}
	var parts []string
	for i := startIdx; i <= endIdx; i++ {
		trimmed := strings.TrimSpace(lines[i])
		trimmed = strings.TrimPrefix(trimmed, "{-|")
		trimmed = strings.TrimPrefix(trimmed, "{- |")
		trimmed = strings.TrimPrefix(trimmed, "{-")
		trimmed = strings.TrimSuffix(trimmed, "-}")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " ")
}

// extractElmTypeAnnotation extracts the name and full signature from a type_annotation node.
// Returns (name, signature) where signature is the full annotation text.
func extractElmTypeAnnotation(n sitter.Node, src []byte) (string, string) {
	sig := strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
	// The name is the first lower_case_identifier child.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if nodeType(child) == "lower_case_identifier" {
			name := string(src[child.StartByte():child.EndByte()])
			return name, sig
		}
	}
	return "", sig
}

// extractElmValueDeclName extracts the function name from a value_declaration node.
// The name is the first lower_case_identifier in the function_declaration_left child.
func extractElmValueDeclName(n sitter.Node, src []byte) string {
	// Try function_declaration_left first.
	if fdl := firstChildOfType(n, "function_declaration_left"); !fdl.IsNull() {
		if ident := firstChildOfType(fdl, "lower_case_identifier"); !ident.IsNull() {
			return string(src[ident.StartByte():ident.EndByte()])
		}
	}
	// Fallback: direct lower_case_identifier (simple value binding).
	if ident := firstChildOfType(n, "lower_case_identifier"); !ident.IsNull() {
		return string(src[ident.StartByte():ident.EndByte()])
	}
	return ""
}

// extractElmTypeName extracts the type name (upper_case_identifier) from a
// type_alias_declaration or type_declaration node.
func extractElmTypeName(n sitter.Node, src []byte) string {
	if ident := firstChildOfType(n, "upper_case_identifier"); !ident.IsNull() {
		return string(src[ident.StartByte():ident.EndByte()])
	}
	return ""
}

// extractElmPortName extracts the port name from a port_annotation node.
func extractElmPortName(n sitter.Node, src []byte) string {
	if ident := firstChildOfType(n, "lower_case_identifier"); !ident.IsNull() {
		return string(src[ident.StartByte():ident.EndByte()])
	}
	return ""
}

// extractElmModuleName extracts the module name from a module_declaration node.
// Elm module names can be dotted: "module Html.Attributes exposing (...)".
func extractElmModuleName(n sitter.Node, src []byte) string {
	// The module name is in an upper_case_qid child (qualified identifier).
	if qid := firstChildOfType(n, "upper_case_qid"); !qid.IsNull() {
		return string(src[qid.StartByte():qid.EndByte()])
	}
	// Fallback: try upper_case_identifier.
	if ident := firstChildOfType(n, "upper_case_identifier"); !ident.IsNull() {
		return string(src[ident.StartByte():ident.EndByte()])
	}
	return ""
}

// extractElmImportName extracts the module name from an import_clause node.
func extractElmImportName(n sitter.Node, src []byte) string {
	// import_clause has an upper_case_qid for the module name.
	if qid := firstChildOfType(n, "upper_case_qid"); !qid.IsNull() {
		return string(src[qid.StartByte():qid.EndByte()])
	}
	if ident := firstChildOfType(n, "upper_case_identifier"); !ident.IsNull() {
		return string(src[ident.StartByte():ident.EndByte()])
	}
	return ""
}

// Parse extracts code entities from a single Elm file.
func (p *ElmParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	declInfo := extractElmDeclInfo(root, src)

	// Track type annotations so we can pair them with the next value_declaration.
	pendingAnnotations := make(map[string]string) // name → signature

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}

		switch nodeType(child) {
		case "module_declaration":
			moduleName := extractElmModuleName(child, src)
			if moduleName != "" {
				fileNode := g.GetNode(fileNodeID)
				if fileNode != nil {
					if fileNode.Metadata == nil {
						fileNode.Metadata = make(map[string]string)
					}
					fileNode.Metadata["module"] = moduleName
					// Extract exposing list if present.
					if exposing := firstChildOfType(child, "exposing_list"); !exposing.IsNull() {
						fileNode.Metadata["exposing"] = strings.TrimSpace(string(src[exposing.StartByte():exposing.EndByte()]))
					}
				}
			}

		case "import_clause":
			importName := extractElmImportName(child, src)
			if importName != "" {
				importNodeID := g.MakeNodeID(importName, importName)
				g.AddNode(&graph.Node{
					ID:      importNodeID,
					Type:    graph.NodePackage,
					Name:    importName,
					Package: importName,
					File:    filePath,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
			}

		case "type_annotation":
			name, sig := extractElmTypeAnnotation(child, src)
			if name != "" {
				pendingAnnotations[name] = sig
			}

		case "value_declaration":
			name := extractElmValueDeclName(child, src)
			if name == "" {
				continue
			}
			sl := int(child.StartPoint().Row) + 1
			meta := buildLangMeta(declInfo[name])
			if sig, ok := pendingAnnotations[name]; ok {
				if meta == nil {
					meta = make(map[string]string)
				}
				meta["signature"] = sig
				delete(pendingAnnotations, name)
			}
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     name,
				File:     filePath,
				Line:     sl,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "type_alias_declaration":
			name := extractElmTypeName(child, src)
			if name == "" {
				continue
			}
			sl := int(child.StartPoint().Row) + 1
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string)
			}
			meta["kind"] = "type_alias"
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     sl,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "type_declaration":
			name := extractElmTypeName(child, src)
			if name == "" {
				continue
			}
			sl := int(child.StartPoint().Row) + 1
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string)
			}
			meta["kind"] = "custom_type"
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     sl,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "port_annotation":
			name := extractElmPortName(child, src)
			if name == "" {
				continue
			}
			sl := int(child.StartPoint().Row) + 1
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string)
			}
			meta["kind"] = "port"
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     name,
				File:     filePath,
				Line:     sl,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	// Call sites: track EdgeCalls between Elm functions.
	collectElmCallSites(g, root, src, filePath, fileNodeID)

	return nil
}

// collectElmCallSites walks the AST extracting function call expressions
// and emits EdgeCalls from the enclosing function to the callee.
func collectElmCallSites(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: nil,
		FuncTypes: map[string]bool{
			"value_declaration": true,
		},
		CallTypes: map[string]bool{
			"function_call_expr": true,
		},
		NameExtractor: func(n sitter.Node, s []byte) string {
			return extractElmValueDeclName(n, s)
		},
		CalleeExtractor: func(n sitter.Node, s []byte) string {
			// function_call_expr: first child is the function being called.
			// It may be a value_expr, value_qid, or direct identifier.
			if n.ChildCount() == 0 {
				return ""
			}
			first := n.Child(0)
			if first.IsNull() {
				return ""
			}
			text := strings.TrimSpace(string(s[first.StartByte():first.EndByte()]))
			// Strip module qualification: "Html.div" → "div", "List.map" → "map".
			// But keep the full name if it's a user-defined module function.
			if idx := strings.LastIndex(text, "."); idx >= 0 {
				text = text[idx+1:]
			}
			return text
		},
		IsBuiltin: isElmBuiltin,
	})
}

// isElmBuiltin returns true for Elm standard library function names that
// should not generate call-site edges (too common/noisy).
func isElmBuiltin(name string) bool {
	switch name {
	// Keywords
	case "if", "then", "else", "case", "of", "let", "in", "type", "alias",
		"module", "import", "exposing", "as", "port", "effect":
		return true
	// Core types / constructors
	case "True", "False", "Nothing", "Just", "Ok", "Err":
		return true
	// Basics stdlib
	case "not", "negate", "abs", "sqrt", "ceiling", "floor", "round", "truncate",
		"toString", "toFloat", "toInt", "identity", "always", "never",
		"compare", "max", "min", "modBy", "remainderBy", "isNaN", "isInfinite",
		"xor", "clamp":
		return true
	// Operators
	case "++", "+", "-", "*", "/", "//", "^", "==", "/=", "<", ">", "<=", ">=",
		"&&", "||", "|>", "<|", ">>", "<<":
		return true
	// Debug
	case "Debug", "todo", "crash", "log":
		return true
	// Module names (qualified prefix, stripped by parser)
	case "Html", "Svg", "Http", "Json", "Task", "Platform", "Browser",
		"List", "Dict", "Set", "Array", "Maybe", "Result", "String", "Tuple",
		"Basics", "Cmd", "Sub", "Process", "Random", "Time", "Regex":
		return true
	// Html.* elements — stripping module prefix leaves bare element names
	case "text", "node", "map",
		"div", "span", "p", "a", "button", "input", "textarea", "img",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li", "dl", "dt", "dd",
		"table", "thead", "tbody", "tfoot", "tr", "th", "td", "caption", "colgroup", "col",
		"form", "label", "select", "option", "optgroup", "fieldset", "legend",
		"header", "footer", "main", "nav", "section", "article", "aside",
		"blockquote", "pre", "code", "em", "strong", "small", "s", "cite", "q",
		"sup", "sub", "br", "hr", "figure", "figcaption", "address",
		"canvas", "audio", "video", "source", "track", "embed", "iframe",
		"details", "summary", "menu", "progress", "meter",
		"i", "b", "u":
		return true
	// Html.Attributes.* — stripping module prefix
	case "class", "id", "style", "href", "src", "alt", "title", "type_",
		"value", "placeholder", "name", "action", "method", "target",
		"disabled", "checked", "selected", "readonly", "required",
		"autofocus", "multiple", "width", "height", "rows", "cols",
		"maxlength", "minlength", "step", "pattern",
		"autocomplete", "novalidate", "enctype", "accept", "acceptCharset",
		"attribute", "classList", "property", "tabindex",
		"rel", "media", "lang", "dir", "hidden":
		return true
	// Html.Events.* — stripping module prefix
	case "onClick", "onInput", "onChange", "onSubmit", "onFocus", "onBlur",
		"onMouseOver", "onMouseOut", "onMouseDown", "onMouseUp", "onMouseMove",
		"onKeyDown", "onKeyUp", "onKeyPress", "on", "onWithOptions",
		"onCheck", "onDoubleClick", "preventDefaultOn", "stopPropagationOn":
		return true
	// Json.Decode.* / Json.Decode.Pipeline.*
	case "succeed", "fail", "field", "at", "index", "maybe", "nullable",
		"oneOf", "list", "array", "dict", "keyValuePairs", "value_",
		"string", "bool", "int", "float", "null",
		"object", "encode",
		"andThen", "map2", "map3", "map4", "map5", "map6", "map7", "map8",
		"decodeString", "decodeValue", "errorToString":
		return true
	// Json.Decode.Pipeline.*
	case "decode", "required_", "optional", "hardcoded", "custom", "resolve":
		return true
	// List.* / String.* / Maybe.* commonly stripped
	case "filter", "foldl", "foldr", "sortBy", "sortWith", "sort",
		"head", "tail", "take", "drop", "reverse", "length", "isEmpty",
		"member", "any", "all", "indexedMap", "filterMap", "concatMap", "concat",
		"intersperse", "partition", "unzip", "append", "singleton",
		"trim", "trimLeft", "trimRight", "split", "join", "words", "lines",
		"contains", "startsWith", "endsWith", "indexes", "indices",
		"toUpper", "toLower", "pad", "padLeft", "padRight", "repeat",
		"left", "right", "dropLeft", "dropRight", "slice",
		"fromChar", "toList", "fromList", "cons",
		"fromInt", "fromFloat", "toInt_", "toFloat_":
		return true
	// Http.*
	case "send", "get", "post", "request", "emptyBody", "jsonBody",
		"stringBody", "expectJson", "expectString", "expectStringResponse",
		"multipartBody", "toTask":
		return true
	// Browser.Navigation.*
	case "pushUrl", "replaceUrl", "back", "forward", "load", "reload", "key":
		return true
	// Url.Parser.*
	case "parse", "top", "oneOf_", "map_", "custom_":
		return true
	// Task.*
	case "perform", "attempt", "andThen_", "succeed_", "fail_", "sequence",
		"map_task":
		return true
	// Common app-level but too generic to be useful
	case "update", "view", "init", "subscriptions":
		return true
	}
	return false
}
