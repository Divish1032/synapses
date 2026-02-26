package parser

import (
	"path/filepath"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/php"

	"github.com/synapses/synapses/internal/graph"
)

// PHPParser parses PHP (.php) source files.
type PHPParser struct {
	language *sitter.Language
}

// NewPHPParser creates a ready-to-use PHPParser.
func NewPHPParser() *PHPParser {
	return &PHPParser{language: php.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *PHPParser) Extensions() []string {
	return []string{".php"}
}

// Parse extracts code entities from a single PHP file and merges them into the graph.
func (p *PHPParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// --- namespace use declarations ---
	useQuery := `(namespace_use_declaration (namespace_use_clause (qualified_name) @use_path))`
	if err := runQuery(lang, root, src, useQuery, func(captures map[string]string, _ int) {
		usePath := captures["use_path"]
		if usePath == "" {
			return
		}
		importNodeID := g.MakeNodeID(usePath, usePath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    usePath,
			Package: usePath,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- class declarations ---
	classQuery := `(class_declaration name: (name) @class_name)`
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
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- interface declarations ---
	ifaceQuery := `(interface_declaration name: (name) @iface_name)`
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
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- function definitions ---
	funcQuery := `(function_definition name: (name) @func_name)`
	if err := runQuery(lang, root, src, funcQuery, func(captures map[string]string, startLine int) {
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

	// --- method declarations ---
	methodQuery := `(method_declaration name: (name) @method_name)`
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
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}
