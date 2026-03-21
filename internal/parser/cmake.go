package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	cmakeg "github.com/alexaandru/go-sitter-forest/cmake"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// CMakeParser parses CMake (.cmake, CMakeLists.txt) source files.
// It extracts functions, macros, targets (add_executable/add_library),
// variables (set/option), and includes (include/find_package).
type CMakeParser struct {
	language *sitter.Language
}

// NewCMakeParser creates a ready-to-use CMakeParser.
func NewCMakeParser() *CMakeParser {
	return &CMakeParser{language: sitter.NewLanguage(cmakeg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *CMakeParser) Extensions() []string {
	return []string{".cmake"}
}

func (p *CMakeParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Filenames returns the exact base filenames handled by this parser.
func (p *CMakeParser) Filenames() []string {
	return []string{"CMakeLists.txt"}
}

// Parse extracts code entities from a single CMake file and merges them into
// the graph.
//
// Extracts:
//   - File node (NodeFile)
//   - function() definitions -> NodeFunction
//   - macro() definitions -> NodeFunction with kind="macro"
//   - add_executable/add_library targets -> NodeStruct with kind="target"
//   - set/option variables -> NodeVariable
//   - include/find_package -> EdgeImports
func (p *CMakeParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
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

	// Walk all top-level and nested normal_command nodes.
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}

		if n.Type() == "normal_command" {
			p.handleCommand(g, n, src, filePath, fileNodeID, lines)
		}

		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)

	return nil
}

// handleCommand dispatches a normal_command node based on the command identifier.
func (p *CMakeParser) handleCommand(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	cmdName := p.extractCommandName(n, src)
	if cmdName == "" {
		return
	}

	lower := strings.ToLower(cmdName)
	switch lower {
	case "function":
		p.handleFunction(g, n, src, filePath, fileNodeID, lines)
	case "macro":
		p.handleMacro(g, n, src, filePath, fileNodeID, lines)
	case "add_executable", "add_library":
		p.handleTarget(g, n, src, filePath, fileNodeID, lines, lower)
	case "set", "option":
		p.handleVariable(g, n, src, filePath, fileNodeID, lines, lower)
	case "include":
		p.handleInclude(g, n, src, filePath, fileNodeID)
	case "find_package":
		p.handleFindPackage(g, n, src, filePath, fileNodeID)
	case "foreach":
		p.handleForeach(g, n, src, filePath, fileNodeID, lines)
	case "if", "elseif", "else", "endif",
		"endforeach", "while", "endwhile",
		"break", "continue", "return":
		// Control flow commands — bodies are already walked by the recursive walker.
		// No entity to extract.
	}
}

// extractCommandName returns the command identifier from a normal_command node.
func (p *CMakeParser) extractCommandName(n sitter.Node, src []byte) string {
	idNode := firstChildOfType(n, "identifier")
	if idNode.IsNull() {
		return ""
	}
	return childText(idNode, src)
}

// extractArguments extracts the text of each argument from the argument_list
// of a normal_command node. Each argument may contain an unquoted_argument
// or quoted_argument child.
func (p *CMakeParser) extractArguments(n sitter.Node, src []byte) []string {
	argList := firstChildOfType(n, "argument_list")
	if argList.IsNull() {
		return nil
	}

	var args []string
	for i := uint32(0); i < argList.ChildCount(); i++ {
		child := argList.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "argument":
			text := p.extractArgumentText(child, src)
			if text != "" {
				args = append(args, text)
			}
		case "unquoted_argument":
			text := strings.TrimSpace(childText(child, src))
			if text != "" {
				args = append(args, text)
			}
		case "quoted_argument":
			text := p.extractQuotedText(child, src)
			if text != "" {
				args = append(args, text)
			}
		}
	}
	return args
}

// extractArgumentText extracts the text from an argument node, which may
// contain an unquoted_argument or quoted_argument child.
func (p *CMakeParser) extractArgumentText(n sitter.Node, src []byte) string {
	// Try unquoted_argument child first.
	if uq := firstChildOfType(n, "unquoted_argument"); !uq.IsNull() {
		return strings.TrimSpace(childText(uq, src))
	}
	// Try quoted_argument child.
	if qa := firstChildOfType(n, "quoted_argument"); !qa.IsNull() {
		return p.extractQuotedText(qa, src)
	}
	// Fallback: use the argument node text directly.
	text := strings.TrimSpace(childText(n, src))
	return text
}

// extractQuotedText extracts the inner text from a quoted_argument node,
// stripping the surrounding quotes.
func (p *CMakeParser) extractQuotedText(n sitter.Node, src []byte) string {
	text := childText(n, src)
	text = strings.TrimPrefix(text, `"`)
	text = strings.TrimSuffix(text, `"`)
	return strings.TrimSpace(text)
}

// handleFunction processes a function() command, creating a NodeFunction.
func (p *CMakeParser) handleFunction(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	args := p.extractArguments(n, src)
	if len(args) == 0 {
		return
	}

	name := args[0]
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}

	meta := map[string]string{"kind": "function"}
	if doc != "" {
		meta["doc"] = doc
	}
	if len(args) > 1 {
		meta["signature"] = name + "(" + strings.Join(args[1:], " ") + ")"
	}

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
}

// handleMacro processes a macro() command, creating a NodeFunction with kind="macro".
func (p *CMakeParser) handleMacro(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	args := p.extractArguments(n, src)
	if len(args) == 0 {
		return
	}

	name := args[0]
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}

	meta := map[string]string{"kind": "macro"}
	if doc != "" {
		meta["doc"] = doc
	}
	if len(args) > 1 {
		meta["signature"] = name + "(" + strings.Join(args[1:], " ") + ")"
	}

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
}

// handleTarget processes add_executable() or add_library() commands,
// creating a NodeStruct with kind="target".
func (p *CMakeParser) handleTarget(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	cmdType string,
) {
	args := p.extractArguments(n, src)
	if len(args) == 0 {
		return
	}

	name := args[0]
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}

	targetKind := "executable"
	if cmdType == "add_library" {
		targetKind = "library"
	}

	meta := map[string]string{
		"kind":    "target",
		"subkind": targetKind,
	}
	if doc != "" {
		meta["doc"] = doc
	}

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

// handleVariable processes set() or option() commands,
// creating a NodeVariable.
func (p *CMakeParser) handleVariable(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	cmdType string,
) {
	args := p.extractArguments(n, src)
	if len(args) == 0 {
		return
	}

	name := args[0]
	// Skip variable references like ${VAR} — they are dereferences, not definitions.
	if strings.HasPrefix(name, "${") {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}

	meta := map[string]string{"kind": cmdType}
	if doc != "" {
		meta["doc"] = doc
	}
	// For option(), the description is typically the second argument.
	if cmdType == "option" && len(args) > 1 {
		meta["description"] = args[1]
	}

	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeVariable,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleInclude processes include() commands, creating an import edge.
func (p *CMakeParser) handleInclude(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	args := p.extractArguments(n, src)
	if len(args) == 0 {
		return
	}

	moduleName := args[0]
	importNodeID := g.MakeNodeID(moduleName, moduleName)
	g.AddNode(&graph.Node{
		ID:      importNodeID,
		Type:    graph.NodePackage,
		Name:    moduleName,
		Package: moduleName,
		File:    filePath,
		Line:    int(n.StartPoint().Row) + 1,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
}

// handleForeach processes foreach() commands, extracting the loop variable
// as a NodeVariable so that iteration variables are queryable in the graph.
// Syntax: foreach(VAR IN LISTS ...) or foreach(VAR range) or foreach(VAR item1 item2 ...)
func (p *CMakeParser) handleForeach(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	args := p.extractArguments(n, src)
	if len(args) == 0 {
		return
	}

	loopVar := args[0]
	// Skip variable references — the loop variable must be a plain identifier.
	if strings.HasPrefix(loopVar, "${") || loopVar == "" {
		return
	}

	startLine := int(n.StartPoint().Row) + 1

	nodeID := g.MakeNodeID(filePath, "foreach:"+loopVar)
	if g.GetNode(nodeID) != nil {
		return
	}

	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeVariable,
		Name:     loopVar,
		File:     filePath,
		Line:     startLine,
		Metadata: map[string]string{"kind": "loop_var"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleFindPackage processes find_package() commands, creating an import edge.
func (p *CMakeParser) handleFindPackage(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	args := p.extractArguments(n, src)
	if len(args) == 0 {
		return
	}

	pkgName := args[0]
	importNodeID := g.MakeNodeID(pkgName, pkgName)
	g.AddNode(&graph.Node{
		ID:      importNodeID,
		Type:    graph.NodePackage,
		Name:    pkgName,
		Package: pkgName,
		File:    filePath,
		Line:    int(n.StartPoint().Row) + 1,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
}
