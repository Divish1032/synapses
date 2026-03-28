package parser

import (
	"path/filepath"
	"strings"

	"github.com/alexaandru/go-sitter-forest/scala"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ScalaParser parses Scala (.scala) source files.
type ScalaParser struct {
	language *sitter.Language
}

// NewScalaParser creates a ready-to-use ScalaParser.
func NewScalaParser() *ScalaParser {
	return &ScalaParser{language: sitter.NewLanguage(scala.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *ScalaParser) Extensions() []string {
	return []string{".scala"}
}

func (p *ScalaParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// extractScalaDeclInfo performs a pre-pass for metadata.
func extractScalaDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "function_definition":
			if nameNode := firstChildOfType(n, "identifier"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_definition", "object_definition", "trait_definition":
			if nameNode := firstChildOfType(n, "identifier"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
				// Walk body with class context.
				if body := firstChildOfType(n, "template_body"); !body.IsNull() {
					for i := uint32(0); i < body.ChildCount(); i++ {
						walk(body.Child(i), name)
					}
				}
				return
			}
		case "type_definition":
			if nameNode := firstChildOfType(n, "identifier"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "given_definition":
			if nameNode := firstChildOfType(n, "identifier"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "enum_definition":
			// Scala 3 enum: enum Color: case Red, case Green, ...
			if nameNode := firstChildOfType(n, "identifier"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
	return result
}

// Parse extracts code entities from a single Scala file.
func (p *ScalaParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree != nil {
		defer tree.Close()
	}
	if tree == nil {
		return nil
	}
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
	declInfo := extractScalaDeclInfo(root, src)

	// --- package declaration ---
	var walkPackage func(n sitter.Node)
	walkPackage = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "package_clause" {
			// package_clause → package_identifier (full dotted name)
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "package_identifier" {
					pkgName := string(src[child.StartByte():child.EndByte()])
					if pkgName != "" {
						pkgID := g.MakeNodeID(pkgName, pkgName)
						g.AddNode(&graph.Node{
							ID: pkgID, Type: graph.NodePackage, Name: pkgName, Package: pkgName, File: filePath,
							Line: int(n.StartPoint().Row) + 1, Exported: true,
						})
						g.AddEdge(&graph.Edge{From: fileNodeID, To: pkgID, Type: graph.EdgeImports})
					}
					break
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walkPackage(n.Child(i))
		}
	}
	walkPackage(root)

	// --- import declarations ---
	importQuery := `(import_declaration (identifier) @import_path)`
	_ = runQuery(lang, root, src, importQuery, func(captures map[string]string, _ int) {
		importPath := captures["import_path"]
		if importPath == "" {
			return
		}
		importNodeID := g.MakeNodeID(importPath, importPath)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: importPath, Package: importPath, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- All declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectScalaCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks Scala AST with class qualification.
func (p *ScalaParser) extractAllDeclarations(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "class_definition":
			nameNode := firstChildOfType(n, "identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			// Check for case class or implicit class.
			if hasChildToken(n, src, "case") {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = "case_class"
			} else if hasScalaModifier(n, src, "implicit") {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = "implicit"
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := firstChildOfType(n, "template_body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "object_definition":
			nameNode := firstChildOfType(n, "identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "object"
			if hasChildToken(n, src, "case") {
				meta["kind"] = "case_object"
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := firstChildOfType(n, "template_body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "trait_definition":
			nameNode := firstChildOfType(n, "identifier")
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
			if body := firstChildOfType(n, "template_body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "type_definition":
			nameNode := firstChildOfType(n, "identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "type_alias"
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeInterface, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "function_definition":
			nameNode := firstChildOfType(n, "identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			nodeType := graph.NodeFunction
			if enclosingClass != "" {
				qualName = enclosingClass + "." + name
				nodeType = graph.NodeMethod
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := buildLangMeta(declInfo[qualName])
			if hasScalaModifier(n, src, "implicit") {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = "implicit"
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: nodeType, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "given_definition":
			// Scala 3 given instances: given intOrdering: Ordering[Int] = ...
			nameNode := firstChildOfType(n, "identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			nodeType := graph.NodeFunction
			if enclosingClass != "" {
				qualName = enclosingClass + "." + name
				nodeType = graph.NodeMethod
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := buildLangMeta(declInfo[qualName])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "given"
			g.AddNode(&graph.Node{
				ID: nodeID, Type: nodeType, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "enum_definition":
			// Scala 3 enum: enum Color: case Red \n case Green
			// AST: enum_definition → identifier (name) + enum_body → enum_case_definitions → simple_enum_case → identifier
			nameNode := firstChildOfType(n, "identifier")
			if nameNode.IsNull() {
				break
			}
			enumName := string(src[nameNode.StartByte():nameNode.EndByte()])
			enumNodeID := g.MakeNodeID(filePath, enumName)
			meta := buildLangMeta(declInfo[enumName])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "enum"
			g.AddNode(&graph.Node{
				ID: enumNodeID, Type: graph.NodeStruct, Name: enumName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(enumName), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: enumNodeID, Type: graph.EdgeDefines})
			// Walk enum_body → enum_case_definitions → simple_enum_case → identifier
			if body := firstChildOfType(n, "enum_body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					caseDefs := body.Child(i)
					if caseDefs.IsNull() || caseDefs.Type() != "enum_case_definitions" {
						continue
					}
					for j := uint32(0); j < caseDefs.ChildCount(); j++ {
						sc := caseDefs.Child(j)
						if sc.IsNull() || sc.Type() != "simple_enum_case" {
							continue
						}
						caseNameNode := firstChildOfType(sc, "identifier")
						if caseNameNode.IsNull() {
							continue
						}
						caseName := string(src[caseNameNode.StartByte():caseNameNode.EndByte()])
						qualCase := enumName + "." + caseName
						caseID := g.MakeNodeID(filePath, qualCase)
						g.AddNode(&graph.Node{
							ID: caseID, Type: graph.NodeMethod, Name: qualCase, File: filePath,
							Line: int(sc.StartPoint().Row) + 1, Exported: true,
							Metadata: map[string]string{"kind": "enum_case"},
						})
						g.AddEdge(&graph.Edge{From: fileNodeID, To: caseID, Type: graph.EdgeDefines})
						g.AddEdge(&graph.Edge{From: enumNodeID, To: caseID, Type: graph.EdgeDefines})
					}
				}
			}
			return

		case "val_definition", "var_definition":
			// Capture val/var declarations as function nodes if they're in a class.
			nameNode := firstChildOfType(n, "identifier")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			if enclosingClass == "" {
				break // Skip module-level vals.
			}
			qualName := enclosingClass + "." + name
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := make(map[string]string, 1)
			meta["kind"] = "val"
			if n.Type() == "var_definition" {
				meta["kind"] = "var"
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectScalaCallSites collects call sites.
func collectScalaCallSites(g *graph.Graph, lang *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	callQuery := `(call_expression function: (identifier) @callee)`
	_ = runQuery(lang, root, src, callQuery, func(captures map[string]string, _ int) {
		callee := captures["callee"]
		if callee == "" || isScalaBuiltin(callee) {
			return
		}
		g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
	})
}

// hasScalaModifier checks if a Scala declaration node has a specific modifier
// (e.g. "implicit", "override", "lazy") in its modifiers child.
func hasScalaModifier(n sitter.Node, src []byte, modifier string) bool {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "modifiers" {
			for j := uint32(0); j < child.ChildCount(); j++ {
				mod := child.Child(j)
				if !mod.IsNull() && string(src[mod.StartByte():mod.EndByte()]) == modifier {
					return true
				}
			}
			return false
		}
		// Stop before the def/val/class keyword.
		if child.Type() == "identifier" || child.Type() == "def" || child.Type() == "class" {
			break
		}
	}
	return false
}

func isScalaBuiltin(name string) bool {
	switch name {
	case "println", "print", "printf",
		"require", "assert", "assume",
		"Some", "None", "Left", "Right", "Nil",
		"List", "Map", "Set", "Seq", "Vector", "Array", "Tuple",
		"Option", "Either", "Try", "Future", "Promise",
		"classOf", "isInstanceOf", "asInstanceOf",
		"toString", "hashCode", "equals", "copy",
		"head", "tail", "map", "flatMap", "filter", "foreach", "fold",
		"foldLeft", "foldRight", "reduce", "collect", "find",
		"isEmpty", "nonEmpty", "size", "length", "contains",
		"getOrElse", "orElse", "toList", "toSeq", "toMap", "toSet":
		return true
	}
	return false
}
