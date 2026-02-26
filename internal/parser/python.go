package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"

	"github.com/synapses/synapses/internal/graph"
)

// extractPythonDeclInfo walks the Python AST and builds a name→declMeta map
// for function_definition and class_definition nodes at any nesting depth.
func extractPythonDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n *sitter.Node, depth int)
	walk = func(n *sitter.Node, depth int) {
		if n == nil || depth > 6 {
			return
		}
		switch n.Type() {
		case "function_definition":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_definition":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "decorated_definition":
			// @decorator\ndef foo(): ... — use decorator start for doc lookup
			for j := 0; j < int(n.ChildCount()); j++ {
				inner := n.Child(j)
				if inner == nil {
					continue
				}
				if inner.Type() == "function_definition" || inner.Type() == "class_definition" {
					if nameNode := inner.ChildByFieldName("name"); nameNode != nil {
						name := string(src[nameNode.StartByte():nameNode.EndByte()])
						sl := int(n.StartPoint().Row) + 1
						result[name] = declMeta{
							Signature: extractSigToBody(inner, src),
							Doc:       extractLineDoc(lines, sl, "#"),
							LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
						}
					}
				}
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), depth+1)
		}
	}
	walk(root, 0)
	return result
}

// PythonParser parses Python (.py, .pyi) source files.
type PythonParser struct {
	language *sitter.Language
}

// NewPythonParser creates a ready-to-use PythonParser.
func NewPythonParser() *PythonParser {
	return &PythonParser{language: python.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *PythonParser) Extensions() []string {
	return []string{".py", ".pyi"}
}

// Parse extracts code entities from a single Python file and merges them
// into the provided graph. The following constructs are captured:
//
//   - import statements        → IMPORTS edges
//   - from X import Y          → IMPORTS edges
//   - function definitions     → NodeFunction
//   - class definitions        → NodeStruct
func (p *PythonParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractPythonDeclInfo(root, src)

	// --- import X / import X.Y ---
	importQuery := `(import_statement name: (dotted_name) @import_path)`
	if err := runQuery(lang, root, src, importQuery, func(captures map[string]string, _ int) {
		importPath := captures["import_path"]
		if importPath == "" {
			return
		}
		importNodeID := g.MakeNodeID(importPath, importPath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    importPath,
			Package: importPath,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- from X import Y ---
	fromImportQuery := `(import_from_statement module_name: (dotted_name) @import_path)`
	if err := runQuery(lang, root, src, fromImportQuery, func(captures map[string]string, _ int) {
		importPath := captures["import_path"]
		if importPath == "" {
			return
		}
		importNodeID := g.MakeNodeID(importPath, importPath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    importPath,
			Package: importPath,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- Function definitions ---
	funcQuery := `(function_definition name: (identifier) @func_name)`
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
			Exported: isPythonPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Class definitions ---
	classQuery := `(class_definition name: (identifier) @class_name)`
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
			Exported: isPythonPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}

// isPythonPublic returns true if the name is not prefixed with an underscore.
// In Python, _name and __name are private/dunder by convention.
func isPythonPublic(name string) bool {
	return !strings.HasPrefix(name, "_")
}
