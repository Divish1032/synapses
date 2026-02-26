package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/ruby"

	"github.com/synapses/synapses/internal/graph"
)

// RubyParser parses Ruby (.rb) source files.
type RubyParser struct {
	language *sitter.Language
}

// NewRubyParser creates a ready-to-use RubyParser.
func NewRubyParser() *RubyParser {
	return &RubyParser{language: ruby.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *RubyParser) Extensions() []string {
	return []string{".rb"}
}

// extractRubyDeclInfo performs a pre-pass over the AST to collect
// doc comment and line count for each named declaration.
func extractRubyDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "method", "singleton_method":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class", "module":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
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

// Parse extracts code entities from a single Ruby file and merges them into the graph.
func (p *RubyParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractRubyDeclInfo(root, src)

	// --- class declarations ---
	classQuery := `(class name: (constant) @class_name)`
	if err := runQuery(lang, root, src, classQuery, func(captures map[string]string, startLine int) {
		name := captures["class_name"]
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
			Exported: isRubyPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- module declarations ---
	moduleQuery := `(module name: (constant) @module_name)`
	if err := runQuery(lang, root, src, moduleQuery, func(captures map[string]string, startLine int) {
		name := captures["module_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeInterface,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isRubyPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- method definitions ---
	methodQuery := `(method name: (identifier) @method_name)`
	if err := runQuery(lang, root, src, methodQuery, func(captures map[string]string, startLine int) {
		name := captures["method_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeMethod,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isRubyPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- singleton methods (def self.foo) ---
	singletonQuery := `(singleton_method name: (identifier) @method_name)`
	if err := runQuery(lang, root, src, singletonQuery, func(captures map[string]string, startLine int) {
		name := captures["method_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeMethod,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: isRubyPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}

// isRubyPublic returns true for names that are not private by convention.
// Ruby private methods typically start with _ or are declared inside private blocks.
// Constants (uppercase) are always considered public.
func isRubyPublic(name string) bool {
	return !strings.HasPrefix(name, "_")
}
