package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/elixir"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ElixirParser parses Elixir (.ex, .exs) source files.
type ElixirParser struct {
	language *sitter.Language
}

// NewElixirParser creates a ready-to-use ElixirParser.
func NewElixirParser() *ElixirParser {
	return &ElixirParser{language: sitter.NewLanguage(elixir.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *ElixirParser) Extensions() []string {
	return []string{".ex", ".exs"}
}

func (p *ElixirParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// extractElixirDeclInfo performs a pre-pass for metadata.
func extractElixirDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "call" {
			targetNode := n.ChildByFieldName("target")
			if !targetNode.IsNull() {
				keyword := string(src[targetNode.StartByte():targetNode.EndByte()])
				switch keyword {
				case "def", "defp", "defmacro", "defmacrop":
					if argsNode := firstChildOfType(n, "arguments"); !argsNode.IsNull() {
						funcName := extractElixirFuncName(argsNode, src)
						if funcName != "" {
							sl := int(n.StartPoint().Row) + 1
							result[funcName] = declMeta{
								Doc:       extractElixirAtDoc(lines, sl),
								LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
							}
						}
					}
				case "defmodule":
					if argsNode := firstChildOfType(n, "arguments"); !argsNode.IsNull() {
						if aliasNode := firstChildOfType(argsNode, "alias"); !aliasNode.IsNull() {
							name := string(src[aliasNode.StartByte():aliasNode.EndByte()])
							sl := int(n.StartPoint().Row) + 1
							result[name] = declMeta{
								Doc:       extractElixirAtDoc(lines, sl),
								LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
							}
						}
					}
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

// extractElixirAtDoc scans backward from startLine (1-indexed) to find an
// @doc attribute preceding the function definition and returns its text.
// It handles:
//   - @doc "single line"
//   - @doc """
//     multi-line heredoc
//     """
//
// Blank lines and @spec / @type / @callback / @impl / @deprecated annotations
// that commonly appear between @doc and def are skipped. Returns "" if no @doc
// is found within 30 lines.
func extractElixirAtDoc(lines []string, startLine int) string {
	if startLine <= 1 || len(lines) == 0 {
		return ""
	}
	// When scanning backward we may hit `"""` which is the *closing* delimiter
	// of a heredoc block. Once we've seen that, we must continue scanning
	// backward through the heredoc *content* until we find the `@doc """` opening.
	seenClosingHeredoc := false

	for i := startLine - 2; i >= 0 && i >= startLine-30; i-- {
		line := strings.TrimSpace(lines[i])

		// Standalone `"""` — the closing delimiter of a preceding heredoc block.
		if line == `"""` {
			seenClosingHeredoc = true
			continue
		}

		if seenClosingHeredoc {
			// We are scanning backward through heredoc content.  Keep going until
			// we find the opening `@doc """` line.
			if strings.HasPrefix(line, "@doc") {
				isHeredoc := strings.HasPrefix(line, `@doc """`) ||
					strings.HasPrefix(line, `@doc ~S"""`)
				if !isHeredoc {
					return "" // @doc false or unexpected
				}
				// Collect heredoc content between i+1 and the closing `"""`.
				var parts []string
				for j := i + 1; j < len(lines); j++ {
					if strings.TrimSpace(lines[j]) == `"""` {
						break
					}
					if t := strings.TrimSpace(lines[j]); t != "" {
						parts = append(parts, t)
					}
				}
				return strings.Join(parts, " ")
			}
			// Still inside heredoc content — keep scanning backward.
			continue
		}

		// --- Not in heredoc mode ---

		// Skip blank lines and annotations that sit between @doc and def.
		if line == "" ||
			strings.HasPrefix(line, "@spec") ||
			strings.HasPrefix(line, "@type") ||
			strings.HasPrefix(line, "@callback") ||
			strings.HasPrefix(line, "@impl") ||
			strings.HasPrefix(line, "@deprecated") {
			continue
		}

		if strings.HasPrefix(line, "@doc ") {
			// Could be a single-line or the opening of a heredoc (rare — content
			// on same line as `@doc """`).
			rest := strings.TrimSpace(line[5:])
			if strings.HasPrefix(rest, `"""`) {
				// Opening + content may be on subsequent lines — reuse heredoc
				// extraction by scanning forward from i+1.
				var parts []string
				for j := i + 1; j < len(lines); j++ {
					if strings.TrimSpace(lines[j]) == `"""` {
						break
					}
					if t := strings.TrimSpace(lines[j]); t != "" {
						parts = append(parts, t)
					}
				}
				return strings.Join(parts, " ")
			}
			// Single-line string: @doc "text"
			doc := strings.Trim(rest, `"`)
			return strings.TrimSpace(doc)
		}

		// @doc false / @moduledoc — no doc.
		if strings.HasPrefix(line, "@doc") {
			return ""
		}

		// Hit something unrelated — stop.
		break
	}
	return ""
}

// extractElixirFuncName gets the function name from def/defp arguments.
func extractElixirFuncName(argsNode sitter.Node, src []byte) string {
	// First try: call child (def foo(args) do ... end)
	if callNode := firstChildOfType(argsNode, "call"); !callNode.IsNull() {
		if nameNode := callNode.ChildByFieldName("target"); !nameNode.IsNull() {
			return string(src[nameNode.StartByte():nameNode.EndByte()])
		}
	}
	// Second try: identifier child (def foo do ... end)
	if nameNode := firstChildOfType(argsNode, "identifier"); !nameNode.IsNull() {
		return string(src[nameNode.StartByte():nameNode.EndByte()])
	}
	return ""
}

// Parse extracts code entities from a single Elixir file.
func (p *ElixirParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractElixirDeclInfo(root, src)

	// --- defmodule MyApp.Foo do ... end ---
	moduleQuery := `
(call
  target: (identifier) @keyword
  (arguments (alias) @module_name)
)`
	if err := runQuery(lang, root, src, moduleQuery, func(captures map[string]string, startLine int) {
		name := captures["module_name"]
		keyword := captures["keyword"]
		if name == "" || keyword != "defmodule" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
			Line: startLine, Exported: true, Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- def / defp function_name(...) do ... end ---
	funcQuery := `
(call
  target: (identifier) @keyword
  (arguments (identifier) @func_name)
)`
	if err := runQuery(lang, root, src, funcQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		keyword := captures["keyword"]
		if name == "" || (keyword != "def" && keyword != "defp") {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
			Line: startLine, Exported: keyword == "def", Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- def/defp with arguments: def foo(a, b) do ... end ---
	funcWithArgsQuery := `
(call
  target: (identifier) @keyword
  (arguments (call target: (identifier) @func_name))
)`
	_ = runQuery(lang, root, src, funcWithArgsQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		keyword := captures["keyword"]
		if name == "" || (keyword != "def" && keyword != "defp") {
			return
		}
		// Don't duplicate.
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
			Line: startLine, Exported: keyword == "def", Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- defmacro / defmacrop ---
	macroQuery := `
(call
  target: (identifier) @keyword
  (arguments (identifier) @macro_name)
)`
	_ = runQuery(lang, root, src, macroQuery, func(captures map[string]string, startLine int) {
		name := captures["macro_name"]
		keyword := captures["keyword"]
		if name == "" || (keyword != "defmacro" && keyword != "defmacrop") {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declInfo[name])
		if meta == nil {
			meta = make(map[string]string, 1)
		}
		meta["kind"] = "macro"
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
			Line: startLine, Exported: keyword == "defmacro", Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- defmacro with arguments ---
	macroWithArgsQuery := `
(call
  target: (identifier) @keyword
  (arguments (call target: (identifier) @macro_name))
)`
	_ = runQuery(lang, root, src, macroWithArgsQuery, func(captures map[string]string, startLine int) {
		name := captures["macro_name"]
		keyword := captures["keyword"]
		if name == "" || (keyword != "defmacro" && keyword != "defmacrop") {
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
		meta["kind"] = "macro"
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
			Line: startLine, Exported: keyword == "defmacro", Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- use/import/alias as imports ---
	// Note: #match? predicates may not be evaluated by go-tree-sitter, so we
	// filter the keyword in the callback to avoid matching defmodule, def, etc.
	elixirImportQuery := `
(call
  target: (identifier) @keyword
  (arguments (alias) @module_name)
)`
	_ = runQuery(lang, root, src, elixirImportQuery, func(captures map[string]string, _ int) {
		keyword := captures["keyword"]
		moduleName := captures["module_name"]
		if moduleName == "" {
			return
		}
		// Only capture use/import/alias calls as imports.
		if keyword != "use" && keyword != "import" && keyword != "alias" {
			return
		}
		importNodeID := g.MakeNodeID(moduleName, moduleName)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: moduleName, Package: moduleName, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- defprotocol MyProtocol do ... end ---
	protoQuery := `
(call
  target: (identifier) @keyword
  (arguments (alias) @proto_name)
)`
	_ = runQuery(lang, root, src, protoQuery, func(captures map[string]string, startLine int) {
		keyword := captures["keyword"]
		name := captures["proto_name"]
		if name == "" || keyword != "defprotocol" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeInterface, Name: name, File: filePath,
			Line: startLine, Exported: true,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- defimpl MyProtocol, for: SomeType do ... end ---
	implQuery := `
(call
  target: (identifier) @keyword
  (arguments (alias) @proto_name)
)`
	_ = runQuery(lang, root, src, implQuery, func(captures map[string]string, startLine int) {
		keyword := captures["keyword"]
		name := captures["proto_name"]
		if name == "" || keyword != "defimpl" {
			return
		}
		implNodeID := g.MakeNodeID(filePath, name+"_impl_"+filePath)
		meta := map[string]string{"kind": "impl", "protocol": name}
		g.AddNode(&graph.Node{
			ID: implNodeID, Type: graph.NodeStruct, Name: name, File: filePath,
			Line: startLine, Exported: true, Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: implNodeID, Type: graph.EdgeDefines})
	})

	// --- defdelegate func_name(args), to: Module ---
	// AST: call(target="defdelegate") → arguments → call(target=identifier("func_name"))
	delegateQuery := `
(call
  target: (identifier) @keyword
  (arguments (call target: (identifier) @func_name))
)`
	_ = runQuery(lang, root, src, delegateQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		keyword := captures["keyword"]
		if name == "" || keyword != "defdelegate" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			return
		}
		meta := map[string]string{"kind": "delegate"}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
			Line: startLine, Exported: true, Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- defstruct field names (qualified with enclosing module) ---
	extractElixirStructFields(g, root, src, filePath, fileNodeID)

	// --- defguard / defguardp ---
	// AST: call(target=identifier("defguard"), arguments(binary_operator(call(identifier("name")), when, ...)))
	extractElixirGuards(g, root, src, filePath, fileNodeID)

	// --- OTP/Phoenix behaviour callbacks (use GenServer, use Plug, etc.) ---
	injectElixirBehaviourCallbacks(g, lang, root, src, filePath, fileNodeID)

	// --- Call sites ---
	collectElixirCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// collectElixirCallSites collects call sites.
func collectElixirCallSites(g *graph.Graph, lang *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	callQuery := `(call target: (identifier) @callee)`
	_ = runQuery(lang, root, src, callQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || isElixirBuiltin(callee) {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
	})
}

// extractElixirStructFields walks the AST for defstruct calls and emits
// individual field names as NodeMethod nodes qualified with the enclosing module.
// AST: call(target="defstruct") → arguments → keywords → pair → keyword("name: ")
// The walk tracks the enclosing defmodule so field names are qualified as
// "ModuleName.field_name" instead of bare "field_name".
func extractElixirStructFields(g *graph.Graph, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var walk func(n sitter.Node, moduleName string)
	walk = func(n sitter.Node, moduleName string) {
		if n.IsNull() {
			return
		}
		if n.Type() == "call" {
			targetNode := n.ChildByFieldName("target")
			if targetNode.IsNull() {
				targetNode = firstChildOfType(n, "identifier")
			}
			if !targetNode.IsNull() {
				keyword := string(src[targetNode.StartByte():targetNode.EndByte()])
				switch keyword {
				case "defmodule":
					// Track the current module name for child nodes.
					argsNode := n.ChildByFieldName("arguments")
					if argsNode.IsNull() {
						argsNode = firstChildOfType(n, "arguments")
					}
					newModule := moduleName
					if !argsNode.IsNull() {
						if aliasNode := firstChildOfType(argsNode, "alias"); !aliasNode.IsNull() {
							newModule = string(src[aliasNode.StartByte():aliasNode.EndByte()])
						}
					}
					for i := uint32(0); i < n.ChildCount(); i++ {
						walk(n.Child(i), newModule)
					}
					return
				case "defstruct":
					argsNode := n.ChildByFieldName("arguments")
					if argsNode.IsNull() {
						argsNode = firstChildOfType(n, "arguments")
					}
					if !argsNode.IsNull() {
						extractStructFieldsFromArgs(g, argsNode, src, filePath, fileNodeID, moduleName)
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), moduleName)
		}
	}
	walk(root, "")
}

// extractStructFieldsFromArgs extracts field names from defstruct keyword arguments
// and qualifies them with the enclosing module name (e.g. "Plug.Conn.status").
func extractStructFieldsFromArgs(g *graph.Graph, argsNode sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, moduleName string) {
	var walkArgs func(n sitter.Node)
	walkArgs = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "pair" {
			// First child is keyword (e.g. "name: ")
			kw := firstChildOfType(n, "keyword")
			if !kw.IsNull() {
				fieldName := strings.TrimSpace(string(src[kw.StartByte():kw.EndByte()]))
				fieldName = strings.TrimSuffix(fieldName, ":")
				fieldName = strings.TrimSpace(fieldName)
				if fieldName != "" {
					// Qualify with enclosing module name to avoid bare field nodes.
					qualName := fieldName
					if moduleName != "" {
						qualName = moduleName + "." + fieldName
					}
					nodeID := g.MakeNodeID(filePath, qualName)
					if g.GetNode(nodeID) == nil {
						meta := map[string]string{"kind": "field"}
						g.AddNode(&graph.Node{
							ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
							Line: int(kw.StartPoint().Row) + 1, Exported: true, Metadata: meta,
						})
						g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walkArgs(n.Child(i))
		}
	}
	walkArgs(argsNode)
}

// extractElixirGuards walks the AST for defguard/defguardp declarations.
// AST: call(target="defguard", arguments(binary_operator(call(identifier("name")), when, ...)))
// Also handles the simpler form: defguard name(args)
func extractElixirGuards(g *graph.Graph, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "call" {
			targetNode := n.ChildByFieldName("target")
			if !targetNode.IsNull() {
				keyword := string(src[targetNode.StartByte():targetNode.EndByte()])
				if keyword == "defguard" || keyword == "defguardp" {
					name := extractElixirGuardName(n, src)
					if name != "" {
						nodeID := g.MakeNodeID(filePath, name)
						if g.GetNode(nodeID) == nil {
							meta := map[string]string{"kind": "guard"}
							g.AddNode(&graph.Node{
								ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
								Line: int(n.StartPoint().Row) + 1, Exported: keyword == "defguard", Metadata: meta,
							})
							g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
						}
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// extractElixirGuardName extracts the guard function name from a defguard call.
// It handles two forms:
//   - defguard is_positive(x) when x > 0  → arguments → binary_operator → call → identifier
//   - defguard is_positive(x)              → arguments → call → identifier (target)
func extractElixirGuardName(callNode sitter.Node, src []byte) string {
	// Try field name first, then fallback to firstChildOfType.
	argsNode := callNode.ChildByFieldName("arguments")
	if argsNode.IsNull() {
		argsNode = firstChildOfType(callNode, "arguments")
	}
	if argsNode.IsNull() {
		return ""
	}
	// Try binary_operator form first (has "when" clause).
	if binOp := firstChildOfType(argsNode, "binary_operator"); !binOp.IsNull() {
		if innerCall := firstChildOfType(binOp, "call"); !innerCall.IsNull() {
			// Try field name first, then fall back to first identifier child.
			if target := innerCall.ChildByFieldName("target"); !target.IsNull() {
				return string(src[target.StartByte():target.EndByte()])
			}
			if ident := firstChildOfType(innerCall, "identifier"); !ident.IsNull() {
				return string(src[ident.StartByte():ident.EndByte()])
			}
		}
		// Fallback: binary_operator → call (without field) → identifier
		if ident := firstChildOfType(binOp, "identifier"); !ident.IsNull() {
			return string(src[ident.StartByte():ident.EndByte()])
		}
	}
	// Try direct call form (no when clause).
	if innerCall := firstChildOfType(argsNode, "call"); !innerCall.IsNull() {
		if target := innerCall.ChildByFieldName("target"); !target.IsNull() {
			return string(src[target.StartByte():target.EndByte()])
		}
		if ident := firstChildOfType(innerCall, "identifier"); !ident.IsNull() {
			return string(src[ident.StartByte():ident.EndByte()])
		}
	}
	return ""
}

// otpBehaviourCallbacks maps known OTP/Phoenix behaviour module names to their
// standard callback function names. When a module calls `use GenServer` (etc.),
// we inject virtual nodes for any callbacks not already explicitly defined.
var otpBehaviourCallbacks = map[string][]string{
	"GenServer": {
		"init", "handle_call", "handle_cast", "handle_info",
		"handle_continue", "terminate", "code_change", "format_status",
	},
	"GenEvent": {
		"init", "handle_event", "handle_call", "handle_info",
		"terminate", "code_change",
	},
	"GenStateMachine": {
		"init", "callback_mode", "handle_event", "terminate", "code_change",
	},
	"Supervisor": {
		"init",
	},
	"Application": {
		"start", "stop",
	},
	"Plug": {
		"init", "call",
	},
	"Plug.Router": {
		"init", "call",
	},
	"Phoenix.Channel": {
		"join", "handle_in", "handle_out", "handle_info", "terminate",
	},
	"Phoenix.LiveView": {
		"mount", "render", "handle_event", "handle_info",
		"handle_params", "update", "terminate",
	},
	"Phoenix.LiveComponent": {
		"mount", "render", "update", "handle_event",
	},
}

// injectElixirBehaviourCallbacks scans the AST for `use BehaviourModule` calls
// and injects virtual callback nodes for any callbacks not already defined.
// The module node gets meta["behaviours"] = comma-separated behaviour list.
func injectElixirBehaviourCallbacks(
	g *graph.Graph, lang *sitter.Language, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	// First pass: collect which behaviours are used and the module name.
	var usedBehaviours []string
	var moduleName string

	// Find defmodule name.
	moduleQuery := `(call target: (identifier) @keyword (arguments (alias) @module_name))`
	_ = runQuery(lang, root, src, moduleQuery, func(captures map[string]string, _ int) {
		if captures["keyword"] == "defmodule" && moduleName == "" {
			moduleName = captures["module_name"]
		}
	})

	// Find `use SomeBehaviour` calls (alias as argument).
	useQuery := `(call target: (identifier) @keyword (arguments (alias) @behaviour_name))`
	_ = runQuery(lang, root, src, useQuery, func(captures map[string]string, _ int) {
		if captures["keyword"] != "use" {
			return
		}
		bname := captures["behaviour_name"]
		if _, known := otpBehaviourCallbacks[bname]; known {
			usedBehaviours = append(usedBehaviours, bname)
		}
	})

	if len(usedBehaviours) == 0 {
		return
	}

	// Tag the module struct node with its behaviours.
	if moduleName != "" {
		moduleNodeID := g.MakeNodeID(filePath, moduleName)
		if modNode := g.GetNode(moduleNodeID); modNode != nil {
			if modNode.Metadata == nil {
				modNode.Metadata = make(map[string]string)
			}
			modNode.Metadata["behaviours"] = strings.Join(usedBehaviours, ",")
		}
	}

	// Inject virtual callback nodes for each behaviour.
	for _, bname := range usedBehaviours {
		callbacks, ok := otpBehaviourCallbacks[bname]
		if !ok {
			continue
		}
		for _, cb := range callbacks {
			// Check whether the callback is already explicitly defined (bare name).
			bareID := g.MakeNodeID(filePath, cb)
			if existing := g.GetNode(bareID); existing != nil {
				// Explicitly defined — annotate in place, don't create a duplicate.
				if existing.Metadata == nil {
					existing.Metadata = make(map[string]string)
				}
				existing.Metadata["behaviour"] = bname
				existing.Metadata["kind"] = "behaviour_callback"
				continue
			}
			// Build a module-qualified name for the virtual node so it doesn't
			// collide with same-named callbacks from other modules in the same file.
			qualName := cb
			if moduleName != "" {
				qualName = moduleName + "." + cb
			}
			qualID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(qualID) != nil {
				continue
			}
			meta := map[string]string{
				"kind":     "behaviour_callback",
				"behaviour": bname,
				"virtual":  "true",
			}
			g.AddNode(&graph.Node{
				ID:       qualID,
				Type:     graph.NodeFunction,
				Name:     qualName,
				File:     filePath,
				Line:     0,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: qualID, Type: graph.EdgeDefines})
		}
	}
}

func isElixirBuiltin(name string) bool {
	switch name {
	case "def", "defp", "defmodule", "defmacro", "defmacrop", "defstruct",
		"defprotocol", "defimpl", "defdelegate", "defguard", "defguardp", "defexception", "defoverridable",
		"use", "import", "alias", "require",
		"if", "unless", "cond", "case", "with", "for", "raise", "reraise", "throw",
		"try", "rescue", "catch", "after",
		"IO", "Enum", "Map", "List", "String", "Kernel", "Agent", "Task", "GenServer",
		"is_nil", "is_atom", "is_binary", "is_integer", "is_float", "is_boolean",
		"is_list", "is_map", "is_tuple", "is_function",
		"inspect", "to_string", "length", "elem", "put_elem", "hd", "tl",
		"send", "receive", "spawn", "spawn_link", "self":
		return true
	}
	return false
}
