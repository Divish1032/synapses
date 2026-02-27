package parser

import (
	"context"
	"path/filepath"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/c"

	"github.com/Divish1032/synapses/internal/graph"
)

// CParser parses C (.c, .h) source files.
type CParser struct {
	language *sitter.Language
}

// NewCParser creates a ready-to-use CParser.
func NewCParser() *CParser {
	return &CParser{language: c.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *CParser) Extensions() []string {
	return []string{".c", ".h", ".ino"}
}

// Parse extracts code entities from a single C file and merges them into the graph.
func (p *CParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// --- #include directives ---
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

	// --- system includes: #include <stdio.h> ---
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
	// C function_definition → declarator → function_declarator → declarator (identifier)
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
			Exported: true, // C doesn't have access modifiers; static functions are file-local
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- struct specifiers with tag names ---
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

	// --- typedef declarations (typedef struct Foo Foo; typedef void (*FnPtr)(...)) ---
	typedefQuery := `(type_definition declarator: (type_identifier) @typedef_name)`
	if err := runQuery(lang, root, src, typedefQuery, func(captures map[string]string, startLine int) {
		name := captures["typedef_name"]
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
