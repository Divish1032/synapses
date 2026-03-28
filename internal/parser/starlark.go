package parser

import (
	"path/filepath"
	"strings"

	starlarkg "github.com/alexaandru/go-sitter-forest/starlark"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// knownBazelRules is the set of Bazel/Starlark rule function names that
// should be recognized as build targets when invoked at the top level.
var knownBazelRules = map[string]bool{
	// Go
	"go_binary":        true,
	"go_library":       true,
	"go_test":          true,
	"go_proto_library": true,
	// C/C++
	"cc_binary":        true,
	"cc_library":       true,
	"cc_test":          true,
	"cc_proto_library": true,
	// Java
	"java_binary":        true,
	"java_library":       true,
	"java_test":          true,
	"java_proto_library": true,
	// Python
	"py_binary":  true,
	"py_library": true,
	"py_test":    true,
	"py_wheel":   true,
	// Rust
	"rust_binary":  true,
	"rust_library": true,
	"rust_test":    true,
	// Shell
	"sh_binary":  true,
	"sh_library": true,
	"sh_test":    true,
	// TypeScript / JavaScript
	"ts_project":     true,
	"ts_library":     true,
	"js_library":     true,
	"npm_package":    true,
	"web_test_suite": true,
	// Proto
	"proto_library": true,
	// OCI / containers
	"oci_image":       true,
	"oci_push":        true,
	"container_image": true,
	"container_push":  true,
	// Repository rules (WORKSPACE)
	"http_archive":         true,
	"http_file":            true,
	"git_repository":       true,
	"local_repository":     true,
	"new_local_repository": true,
	// Toolchain / platform
	"alias":               true,
	"register_toolchains": true,
	"platform":            true,
	"constraint_value":    true,
	"constraint_setting":  true,
	"toolchain":           true,
	// Config
	"config_setting": true,
	"bool_flag":      true,
	"string_flag":    true,
	"string_setting": true,
	// Misc
	"genrule":       true,
	"filegroup":     true,
	"test_suite":    true,
	"exports_files": true,
}

// StarlarkParser parses Starlark/Bazel (.bzl, .star, BUILD, WORKSPACE) files.
// It extracts function definitions, build targets, load() imports, and
// top-level variable assignments.
type StarlarkParser struct {
	language *sitter.Language
}

// NewStarlarkParser creates a ready-to-use StarlarkParser.
func NewStarlarkParser() *StarlarkParser {
	return &StarlarkParser{language: sitter.NewLanguage(starlarkg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *StarlarkParser) Extensions() []string {
	return []string{".bzl", ".star"}
}

func (p *StarlarkParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Filenames returns the exact base filenames handled by this parser.
func (p *StarlarkParser) Filenames() []string {
	return []string{"BUILD", "BUILD.bazel", "WORKSPACE", "WORKSPACE.bazel"}
}

// Parse extracts code entities from a single Starlark/Bazel file and merges
// them into the graph.
//
// Extracts:
//   - File node (NodeFile)
//   - function_definition -> NodeFunction
//   - Top-level call to known rule function -> NodeStruct with kind="target"
//   - load() calls -> EdgeImports for each loaded symbol
//   - Top-level assignments -> NodeVariable
func (p *StarlarkParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	root := tree.RootNode()
	if root.IsNull() {
		return nil
	}

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lines := strings.Split(string(src), "\n")

	// Walk top-level children of the module.
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "function_definition":
			p.handleFunctionDef(g, child, src, filePath, fileNodeID, lines)

		case "expression_statement":
			// Top-level expression statements may contain:
			// - call expressions (targets or load())
			// - assignment expressions (variables)
			p.handleExpressionStatement(g, child, src, filePath, fileNodeID, lines)
		}
	}

	return nil
}

// handleFunctionDef processes a function_definition node, creating a NodeFunction.
func (p *StarlarkParser) handleFunctionDef(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	// The function name is the first identifier child.
	name := p.extractIdentifierChild(n, src)
	if name == "" {
		return
	}

	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}

	meta := make(map[string]string)
	if doc != "" {
		meta["doc"] = doc
	}

	sig := extractSigToBody(n, src)
	if sig != "" {
		meta["signature"] = sig
	}

	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: !strings.HasPrefix(name, "_"),
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleExpressionStatement dispatches top-level expression statements to the
// appropriate handler based on content: load() calls, known rule calls
// (targets), or assignment statements.
func (p *StarlarkParser) handleExpressionStatement(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "call":
			callee := p.extractCalleeIdentifier(child, src)
			if callee == "load" {
				p.handleLoad(g, child, src, filePath, fileNodeID)
			} else if knownBazelRules[callee] {
				p.handleTarget(g, child, src, filePath, fileNodeID, lines, callee)
			}

		case "assignment":
			p.handleAssignment(g, child, src, filePath, fileNodeID, lines)
		}
	}
}

// handleTarget processes a top-level call to a known Bazel rule function,
// creating a NodeStruct with kind="target".
func (p *StarlarkParser) handleTarget(
	g *graph.Graph,
	callNode sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	ruleName string,
) {
	targetName := p.extractKeywordArgValue(callNode, src, "name")
	if targetName == "" {
		return
	}

	startLine := int(callNode.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	nodeID := g.MakeNodeID(filePath, targetName)
	if g.GetNode(nodeID) != nil {
		return
	}

	meta := map[string]string{
		"kind": "target",
		"rule": ruleName,
	}
	if doc != "" {
		meta["doc"] = doc
	}

	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     targetName,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleLoad processes a load() call, creating import edges for each loaded symbol.
// Syntax: load("@rules_go//go:def.bzl", "go_binary", "go_test")
// The first argument is the source label; remaining string args are imported symbols.
func (p *StarlarkParser) handleLoad(
	g *graph.Graph,
	callNode sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	argList := p.findChildByType(callNode, "argument_list")
	if argList.IsNull() {
		return
	}

	startLine := int(callNode.StartPoint().Row) + 1

	// Collect all string arguments from the argument_list.
	var stringArgs []string
	for i := uint32(0); i < argList.ChildCount(); i++ {
		arg := argList.Child(i)
		if arg.IsNull() {
			continue
		}

		// Direct string arguments.
		if arg.Type() == "string" {
			text := p.unquoteString(arg, src)
			if text != "" {
				stringArgs = append(stringArgs, text)
			}
			continue
		}

		// keyword_argument: the value might be a string (e.g. load aliasing).
		// We skip keyword arguments for load — they are alias mappings
		// like symbol = "original_name", and we handle them by taking
		// the key as the local name.
		if arg.Type() == "keyword_argument" {
			// For aliased loads like: load("...", my_alias = "go_binary")
			// the key is the local alias, the value is the original symbol name.
			// We create the import for the original symbol name.
			valNode := p.findKeywordValue(arg, src)
			if !valNode.IsNull() && valNode.Type() == "string" {
				text := p.unquoteString(valNode, src)
				if text != "" {
					stringArgs = append(stringArgs, text)
				}
			}
		}
	}

	if len(stringArgs) < 2 {
		return
	}

	// First string is the label (source).
	label := stringArgs[0]
	importNodeID := g.MakeNodeID(label, label)
	g.AddNode(&graph.Node{
		ID:      importNodeID,
		Type:    graph.NodePackage,
		Name:    label,
		Package: label,
		File:    filePath,
		Line:    startLine,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})

	// Remaining strings are imported symbols.
	for _, sym := range stringArgs[1:] {
		symNodeID := g.MakeNodeID(label, sym)
		g.AddNode(&graph.Node{
			ID:       symNodeID,
			Type:     graph.NodeFunction,
			Name:     sym,
			Package:  label,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: map[string]string{
				"source":    label,
				"kind":      "load",
				"signature": sym + "(...)",
			},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: symNodeID, Type: graph.EdgeImports})
	}
}

// handleAssignment processes a top-level assignment statement, creating a
// NodeVariable for the left-hand side.
func (p *StarlarkParser) handleAssignment(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	// The left-hand side is the first identifier child.
	lhs := p.findChildByType(n, "identifier")
	if lhs.IsNull() {
		return
	}

	name := childText(lhs, src)
	if name == "" {
		return
	}

	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}

	meta := make(map[string]string)
	if doc != "" {
		meta["doc"] = doc
	}

	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeVariable,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: !strings.HasPrefix(name, "_"),
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractIdentifierChild returns the text of the first identifier child of n.
func (p *StarlarkParser) extractIdentifierChild(n sitter.Node, src []byte) string {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "identifier" {
			return childText(child, src)
		}
	}
	return ""
}

// extractCalleeIdentifier returns the callee name from a call node.
// For simple calls like go_library(...), the first child is an identifier.
func (p *StarlarkParser) extractCalleeIdentifier(callNode sitter.Node, src []byte) string {
	for i := uint32(0); i < callNode.ChildCount(); i++ {
		child := callNode.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "identifier" {
			return childText(child, src)
		}
		// For attribute access like foo.bar(...), extract the last identifier.
		if child.Type() == "attribute" {
			return p.extractAttributeName(child, src)
		}
	}
	return ""
}

// extractAttributeName extracts the attribute name from an attribute node
// (e.g. "bar" from "foo.bar").
func (p *StarlarkParser) extractAttributeName(n sitter.Node, src []byte) string {
	// The attribute node has children: object, ".", identifier.
	// We want the last identifier.
	for i := int(n.ChildCount()) - 1; i >= 0; i-- {
		child := n.Child(uint32(i))
		if !child.IsNull() && child.Type() == "identifier" {
			return childText(child, src)
		}
	}
	return ""
}

// extractKeywordArgValue looks through the argument_list of a call node
// to find a keyword_argument where the key matches the given name,
// and returns the unquoted string value.
func (p *StarlarkParser) extractKeywordArgValue(callNode sitter.Node, src []byte, key string) string {
	argList := p.findChildByType(callNode, "argument_list")
	if argList.IsNull() {
		return ""
	}

	for i := uint32(0); i < argList.ChildCount(); i++ {
		kwArg := argList.Child(i)
		if kwArg.IsNull() || kwArg.Type() != "keyword_argument" {
			continue
		}

		// keyword_argument children: identifier (key), "=", value
		keyNode := p.findChildByType(kwArg, "identifier")
		if keyNode.IsNull() {
			continue
		}
		if childText(keyNode, src) != key {
			continue
		}

		// Find the value (string node).
		valNode := p.findKeywordValue(kwArg, src)
		if !valNode.IsNull() && valNode.Type() == "string" {
			return p.unquoteString(valNode, src)
		}
	}
	return ""
}

// findChildByType returns the first child of n with the given type.
func (p *StarlarkParser) findChildByType(n sitter.Node, typ string) sitter.Node {
	return firstChildOfType(n, typ)
}

// findKeywordValue returns the value node from a keyword_argument.
// The keyword_argument has children: identifier, "=", value.
// We return the last non-operator child that is not the key identifier.
func (p *StarlarkParser) findKeywordValue(kwArg sitter.Node, src []byte) sitter.Node {
	_ = src
	foundEquals := false
	for i := uint32(0); i < kwArg.ChildCount(); i++ {
		child := kwArg.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "=" {
			foundEquals = true
			continue
		}
		if foundEquals {
			return child
		}
	}
	// Fallback: return the last child if it's not the identifier.
	if kwArg.ChildCount() >= 3 {
		return kwArg.Child(kwArg.ChildCount() - 1)
	}
	return sitter.Node{}
}

// unquoteString strips surrounding quotes from a string node's text.
func (p *StarlarkParser) unquoteString(n sitter.Node, src []byte) string {
	text := childText(n, src)
	// Strip triple quotes first, then single/double quotes.
	for _, q := range []string{`"""`, `'''`, `"`, `'`} {
		if strings.HasPrefix(text, q) && strings.HasSuffix(text, q) && len(text) >= 2*len(q) {
			return text[len(q) : len(text)-len(q)]
		}
	}
	return text
}
