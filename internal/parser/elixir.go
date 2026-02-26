package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/elixir"

	"github.com/synapses/synapses/internal/graph"
)

// ElixirParser parses Elixir (.ex, .exs) source files.
type ElixirParser struct {
	language *sitter.Language
}

// NewElixirParser creates a ready-to-use ElixirParser.
func NewElixirParser() *ElixirParser {
	return &ElixirParser{language: elixir.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *ElixirParser) Extensions() []string {
	return []string{".ex", ".exs"}
}

// extractElixirDeclInfo performs a pre-pass over the AST to collect
// doc comments and line counts for defmodule, def, and defp nodes.
// Elixir uses # for line comments; signatures are the call text up to the do block.
func extractElixirDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		// Elixir grammar: call nodes with target identifier "defmodule"/"def"/"defp"
		if n.Type() == "call" {
			targetNode := n.ChildByFieldName("target")
			if targetNode != nil {
				keyword := string(src[targetNode.StartByte():targetNode.EndByte()])
				switch keyword {
				case "def", "defp":
					// arguments: first argument is the function call with name
					if argsNode := n.ChildByFieldName("arguments"); argsNode != nil {
						if callNode := firstChildOfType(argsNode, "call"); callNode != nil {
							if nameNode := callNode.ChildByFieldName("target"); nameNode != nil {
								name := string(src[nameNode.StartByte():nameNode.EndByte()])
								sl := int(n.StartPoint().Row) + 1
								result[name] = declMeta{
									Doc:       extractLineDoc(lines, sl, "#"),
									LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
								}
							}
						} else if nameNode := firstChildOfType(argsNode, "identifier"); nameNode != nil {
							name := string(src[nameNode.StartByte():nameNode.EndByte()])
							sl := int(n.StartPoint().Row) + 1
							result[name] = declMeta{
								Doc:       extractLineDoc(lines, sl, "#"),
								LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
							}
						}
					}
				case "defmodule":
					if argsNode := n.ChildByFieldName("arguments"); argsNode != nil {
						if aliasNode := firstChildOfType(argsNode, "alias"); aliasNode != nil {
							name := string(src[aliasNode.StartByte():aliasNode.EndByte()])
							sl := int(n.StartPoint().Row) + 1
							result[name] = declMeta{
								Doc:       extractLineDoc(lines, sl, "#"),
								LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
							}
						}
					}
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

// Parse extracts code entities from a single Elixir file and merges them into the graph.
// Captured constructs:
//   - defmodule → NodeStruct (modules are the primary organisational unit)
//   - def / defp → NodeFunction (public and private functions)
func (p *ElixirParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree := parser.Parse(nil, src)
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

	// --- defmodule MyApp.Foo do … end ---
	moduleQuery := `
(call
  target: (identifier) @keyword
  (arguments (alias) @module_name)
  (#eq? @keyword "defmodule")
)`
	if err := runQuery(lang, root, src, moduleQuery, func(captures map[string]string, startLine int) {
		name := captures["module_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
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

	// --- def / defp function_name(…) do … end ---
	funcQuery := `
(call
  target: (identifier) @keyword
  (arguments (identifier) @func_name)
  (#match? @keyword "^def(p)?$")
)`
	if err := runQuery(lang, root, src, funcQuery, func(captures map[string]string, startLine int) {
		name := captures["func_name"]
		keyword := captures["keyword"]
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
			Exported: keyword == "def",
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}
