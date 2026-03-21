package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/php"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// PHPParser parses PHP (.php) source files.
type PHPParser struct {
	language *sitter.Language
}

// NewPHPParser creates a ready-to-use PHPParser.
func NewPHPParser() *PHPParser {
	return &PHPParser{language: sitter.NewLanguage(php.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *PHPParser) Extensions() []string {
	return []string{".php"}
}

func (p *PHPParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// extractPHPDeclInfo collects metadata for PHP declarations.
func extractPHPDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "method_declaration", "function_definition":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"compound_statement"}),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration", "interface_declaration", "trait_declaration", "enum_declaration":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
				if body := n.ChildByFieldName("body"); !body.IsNull() {
					for i := uint32(0); i < body.ChildCount(); i++ {
						walk(body.Child(i), name)
					}
				}
				return
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
	return result
}

// Parse extracts code entities from a single PHP file.
func (p *PHPParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
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
	declInfo := extractPHPDeclInfo(root, src)

	// --- namespace use declarations ---
	useQuery := `(namespace_use_declaration (namespace_use_clause (qualified_name) @use_path))`
	_ = runQuery(lang, root, src, useQuery, func(captures map[string]string, _ int) {
		usePath := captures["use_path"]
		if usePath == "" {
			return
		}
		importNodeID := g.MakeNodeID(usePath, usePath)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: usePath, Package: usePath, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- namespace definition ---
	nsQuery := `(namespace_definition name: (namespace_name) @ns_name)`
	_ = runQuery(lang, root, src, nsQuery, func(captures map[string]string, startLine int) {
		name := captures["ns_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodePackage, Name: name, File: filePath,
			Line: startLine, Exported: true,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	})

	// --- All declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectPHPCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks PHP AST with class qualification.
func (p *PHPParser) extractAllDeclarations(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "class_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			// Detect abstract/final class modifiers.
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "class_modifier" || child.Type() == "abstract_modifier" {
					text := string(src[child.StartByte():child.EndByte()])
					if text == "abstract" || text == "final" {
						if meta == nil {
							meta = make(map[string]string, 1)
						}
						meta["kind"] = text
						break
					}
				}
				// Stop before the class name.
				if child.Type() == "name" {
					break
				}
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "interface_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeInterface, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "trait_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "trait"
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "enum_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "enum"
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "enum_case":
			// PHP 8.1 enum cases: case Active; case Inactive;
			// AST: enum_case → name child
			if enclosingClass == "" {
				break
			}
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				// Fallback: find first "name" type child.
				nameNode = firstChildOfType(n, "name")
			}
			if nameNode.IsNull() {
				break
			}
			caseName := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := enclosingClass + "." + caseName
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break
			}
			caseMeta := map[string]string{"kind": "enum_case"}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: true, Metadata: caseMeta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			enumID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(enumID) != nil {
				g.AddEdge(&graph.Edge{From: enumID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "function_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "method_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if enclosingClass != "" {
				qualName = enclosingClass + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "class_const_declaration":
			// PHP class constants: const VERSION = '1.0';
			if enclosingClass == "" {
				break
			}
			// Walk children to find const_element nodes with name
			for k := uint32(0); k < n.ChildCount(); k++ {
				elem := n.Child(k)
				if elem.IsNull() {
					continue
				}
				// const_element has a name child (identifier) and value
				var constName string
				for m := uint32(0); m < elem.ChildCount(); m++ {
					c := elem.Child(m)
					if c.IsNull() {
						continue
					}
					if c.Type() == "name" || c.Type() == "identifier" {
						constName = string(src[c.StartByte():c.EndByte()])
						break
					}
				}
				if constName == "" {
					// Try the element text if it's a simple identifier
					if elem.Type() == "name" || elem.Type() == "identifier" {
						constName = string(src[elem.StartByte():elem.EndByte()])
					}
				}
				if constName == "" {
					continue
				}
				qualName := enclosingClass + "." + constName
				nodeID := g.MakeNodeID(filePath, qualName)
				if g.GetNode(nodeID) != nil {
					continue
				}
				meta := map[string]string{"kind": "const"}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeStruct, Name: qualName, File: filePath,
					Line:     int(n.StartPoint().Row) + 1,
					Exported: isExported(constName),
					Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "property_declaration":
			// PHP class properties: public string $name;
			if enclosingClass == "" {
				break
			}
			// property_declaration contains property_element children with variable_name
			for k := uint32(0); k < n.ChildCount(); k++ {
				elem := n.Child(k)
				if elem.IsNull() {
					continue
				}
				if elem.Type() == "property_element" || elem.Type() == "variable_name" {
					// Variable name may be direct child or nested
					var propName string
					if elem.Type() == "variable_name" {
						// Strip leading $
						propName = strings.TrimPrefix(string(src[elem.StartByte():elem.EndByte()]), "$")
					} else {
						for m := uint32(0); m < elem.ChildCount(); m++ {
							c := elem.Child(m)
							if !c.IsNull() && c.Type() == "variable_name" {
								propName = strings.TrimPrefix(string(src[c.StartByte():c.EndByte()]), "$")
								break
							}
						}
					}
					if propName == "" {
						continue
					}
					qualName := enclosingClass + "." + propName
					nodeID := g.MakeNodeID(filePath, qualName)
					if g.GetNode(nodeID) != nil {
						continue
					}
					meta := map[string]string{"kind": "property"}
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeStruct, Name: qualName, File: filePath,
						Line:     int(n.StartPoint().Row) + 1,
						Exported: isExported(propName),
						Metadata: meta,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectPHPCallSites collects call sites.
func collectPHPCallSites(g *graph.Graph, lang *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Function calls: foo(...)
	callQuery := `(function_call_expression function: (name) @callee)`
	_ = runQuery(lang, root, src, callQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
	})

	// Method calls: $obj->method(...)
	memberCallQuery := `(member_call_expression name: (name) @callee)`
	_ = runQuery(lang, root, src, memberCallQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
	})

	// Static calls: ClassName::method(...)
	scopedCallQuery := `(scoped_call_expression name: (name) @callee)`
	_ = runQuery(lang, root, src, scopedCallQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
	})

	// Object creation: new ClassName(...)
	objectCreationQuery := `(object_creation_expression (name) @type_name)`
	_ = runQuery(lang, root, src, objectCreationQuery, func(captures map[string]string, _ int) {
		typeName := captures["type_name"]
		if typeName == "" {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: typeName})
	})
}
