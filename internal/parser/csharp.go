package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/csharp"

	"github.com/synapses/synapses/internal/graph"
)

// CSharpParser parses C# (.cs) source files.
type CSharpParser struct {
	language *sitter.Language
}

// NewCSharpParser creates a ready-to-use CSharpParser.
func NewCSharpParser() *CSharpParser {
	return &CSharpParser{language: csharp.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *CSharpParser) Extensions() []string {
	return []string{".cs"}
}

// extractCSharpDeclInfo performs a pre-pass over the AST to collect
// signature, doc comment, and line count for each named declaration.
func extractCSharpDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "method_declaration":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration", "interface_declaration", "namespace_declaration":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
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

// Parse extracts code entities from a single C# file and merges them into the graph.
func (p *CSharpParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractCSharpDeclInfo(root, src)

	// --- using directives ---
	usingQuery := `(using_directive (identifier) @using_name)`
	if err := runQuery(lang, root, src, usingQuery, func(captures map[string]string, _ int) {
		name := captures["using_name"]
		if name == "" {
			return
		}
		importNodeID := g.MakeNodeID(name, name)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    name,
			Package: name,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- namespace declarations ---
	nsQuery := `(namespace_declaration name: (identifier) @ns_name)`
	if err := runQuery(lang, root, src, nsQuery, func(captures map[string]string, startLine int) {
		name := captures["ns_name"]
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

	// --- class declarations ---
	classQuery := `(class_declaration name: (identifier) @class_name)`
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
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- interface declarations ---
	ifaceQuery := `(interface_declaration name: (identifier) @iface_name)`
	if err := runQuery(lang, root, src, ifaceQuery, func(captures map[string]string, startLine int) {
		name := captures["iface_name"]
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
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- method declarations ---
	methodQuery := `(method_declaration name: (identifier) @method_name)`
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
			Exported: isExported(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}
