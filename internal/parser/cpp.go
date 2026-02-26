package parser

import (
	"path/filepath"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/cpp"

	"github.com/synapses/synapses/internal/graph"
)

// CppParser parses C++ (.cpp, .cc, .cxx, .hpp, .hh, .hxx) source files.
type CppParser struct {
	language *sitter.Language
}

// NewCppParser creates a ready-to-use CppParser.
func NewCppParser() *CppParser {
	return &CppParser{language: cpp.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *CppParser) Extensions() []string {
	return []string{".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".mm"}
}

// Parse extracts code entities from a single C++ file and merges them into the graph.
func (p *CppParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// --- #include <system> directives ---
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
	funcQuery := `(function_definition declarator: (function_declarator declarator: (identifier) @func_name))`
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
			Exported: true,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- class specifiers ---
	classQuery := `(class_specifier name: (type_identifier) @class_name)`
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

	// --- struct specifiers ---
	structQuery := `(struct_specifier name: (type_identifier) @struct_name)`
	if err := runQuery(lang, root, src, structQuery, func(captures map[string]string, startLine int) {
		name := captures["struct_name"]
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
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- namespace definitions ---
	nsQuery := `(namespace_definition name: (namespace_identifier) @ns_name)`
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
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}
