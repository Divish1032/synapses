package parser

import (
	"context"
	"path/filepath"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/lua"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// LuaParser parses Lua (.lua) source files.
type LuaParser struct {
	language *sitter.Language
}

// NewLuaParser creates a ready-to-use LuaParser.
func NewLuaParser() *LuaParser {
	return &LuaParser{language: lua.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *LuaParser) Extensions() []string {
	return []string{".lua"}
}

// Parse extracts code entities from a single Lua file and merges them into the graph.
func (p *LuaParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	lang := p.language

	// In the bundled Lua grammar, both global and local functions are represented
	// as function_statement nodes (no separate function_declaration or local_function
	// node types). Global functions have their name inside a function_name subtree;
	// local functions have an identifier as a direct child after the local keyword.

	// --- global function declarations: function Foo() end ---
	globalFuncQuery := `(function_statement (function_name (identifier) @func_name))`
	if err := runQuery(lang, root, src, globalFuncQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isExported(name),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- local function declarations: local function foo() end ---
	// The local keyword is a direct sibling of the identifier; this query matches
	// function_statement nodes that have both a (local) child and an (identifier)
	// child (the latter being the function name).
	localFuncQuery := `(function_statement (local) (identifier) @func_name)`
	if err := runQuery(lang, root, src, localFuncQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: false, // local functions are file-scoped
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}
