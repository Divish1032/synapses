package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/c_sharp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// CSharpParser parses C# (.cs) source files.
type CSharpParser struct {
	language *sitter.Language
}

// NewCSharpParser creates a ready-to-use CSharpParser.
func NewCSharpParser() *CSharpParser {
	return &CSharpParser{language: sitter.NewLanguage(c_sharp.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *CSharpParser) Extensions() []string {
	return []string{".cs"}
}

// extractCSharpDeclInfo performs a pre-pass over the AST to collect metadata.
func extractCSharpDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "method_declaration":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"block"}),
					Doc:       extractDocMulti(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "constructor_declaration":
			qualName := "constructor"
			if enclosingClass != "" {
				qualName = enclosingClass + ".constructor"
			}
			sl := int(n.StartPoint().Row) + 1
			result[qualName] = declMeta{
				Signature: extractSigToBodyMulti(n, src, []string{"block"}),
				Doc:       extractDocMulti(lines, sl, "///"),
				LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
			}
		case "delegate_declaration":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractDocMulti(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_declaration", "struct_declaration", "interface_declaration",
			"enum_declaration", "record_declaration", "namespace_declaration":
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[className] = declMeta{
					Doc:       extractDocMulti(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), className)
				}
			}
			return
		case "property_declaration":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Doc:       extractDocMulti(lines, sl, "///"),
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

// isCSharpPublic checks if a declaration node has the "public" modifier.
func isCSharpPublic(n sitter.Node, src []byte) bool {
	if n.IsNull() {
		return false
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "modifier" {
			text := string(src[child.StartByte():child.EndByte()])
			if text == "public" || text == "internal" {
				return true
			}
		}
	}
	return true // Default (no modifier) is internal, treat as exported
}

// extractCSharpHeritage extracts heritage (base_list) from C# class/struct
// declarations. C# uses a single base_list for both extends and implements.
// Since C# classes can extend one class and implement multiple interfaces,
// and we can't easily distinguish them at parse time without type analysis,
// we store all base types in heritage_implements metadata.
func extractCSharpHeritage(n sitter.Node, src []byte, meta map[string]string) map[string]string {
	baseList := n.ChildByFieldName("bases")
	if baseList.IsNull() {
		baseList = firstChildOfType(n, "base_list")
	}
	if baseList.IsNull() {
		return meta
	}
	names := extractTypeIdentifiers(baseList, src)
	if len(names) == 0 {
		return meta
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	meta["heritage_implements"] = strings.Join(names, ",")
	return meta
}

// isCSharpExtensionMethod returns true when the method_declaration's first
// parameter has a "this" modifier — the C# extension method marker.
// AST: method_declaration → parameter_list → parameter → modifier("this")
func isCSharpExtensionMethod(n sitter.Node, src []byte) bool {
	paramList := n.ChildByFieldName("parameters")
	if paramList.IsNull() {
		paramList = firstChildOfType(n, "parameter_list")
	}
	if paramList.IsNull() {
		return false
	}
	// Find the first parameter child.
	for i := uint32(0); i < paramList.ChildCount(); i++ {
		child := paramList.Child(i)
		if child.IsNull() || child.Type() != "parameter" {
			continue
		}
		// Check if this parameter has a modifier child with value "this".
		for j := uint32(0); j < child.ChildCount(); j++ {
			mod := child.Child(j)
			if !mod.IsNull() && mod.Type() == "modifier" &&
				string(src[mod.StartByte():mod.EndByte()]) == "this" {
				return true
			}
		}
		return false // first parameter found, no "this" modifier
	}
	return false
}

// Parse extracts code entities from a single C# file.
func (p *CSharpParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractCSharpDeclInfo(root, src)

	// --- using directives ---
	usingQuery := `(using_directive (identifier) @using_name)`
	_ = runQuery(lang, root, src, usingQuery, func(captures map[string]string, _ int) {
		name := captures["using_name"]
		if name == "" {
			return
		}
		importNodeID := g.MakeNodeID(name, name)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: name, Package: name, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- using directives with qualified name ---
	usingQualQuery := `(using_directive (qualified_name) @using_name)`
	_ = runQuery(lang, root, src, usingQualQuery, func(captures map[string]string, _ int) {
		name := captures["using_name"]
		if name == "" {
			return
		}
		importNodeID := g.MakeNodeID(name, name)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: name, Package: name, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- All declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectCSharpCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks the AST for all C# declarations with class qualification.
func (p *CSharpParser) extractAllDeclarations(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "namespace_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodePackage, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: true, Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "class_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			// Detect abstract/sealed class modifiers.
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "modifier" {
					text := string(src[child.StartByte():child.EndByte()])
					if text == "abstract" || text == "sealed" {
						if meta == nil {
							meta = make(map[string]string, 1)
						}
						meta["kind"] = text
						break
					}
				}
			}
			// Heritage clause extraction: C# uses base_list for both extends and implements.
			meta = extractCSharpHeritage(n, src, meta)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "struct_declaration":
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
			meta["kind"] = "struct"
			meta = extractCSharpHeritage(n, src, meta)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: meta,
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
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: buildLangMeta(declInfo[name]),
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
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "record_declaration":
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
			meta["kind"] = "record"
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

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
			meta := buildLangMeta(declInfo[qualName])
			// Detect extension methods: first parameter has modifier "this".
			if isCSharpExtensionMethod(n, src) {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = "extension"
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "constructor_declaration":
			qualName := "constructor"
			if enclosingClass != "" {
				qualName = enclosingClass + ".constructor"
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "property_declaration":
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
			meta := buildLangMeta(declInfo[qualName])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "property"
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "delegate_declaration":
			// public delegate void Handler(int x);
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) != nil {
				break
			}
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "delegate"
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeFunction, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "event_field_declaration":
			// public event EventHandler OnClick;
			// AST: event_field_declaration → variable_declaration → variable_declarator → identifier
			if enclosingClass == "" {
				break
			}
			for i := uint32(0); i < n.ChildCount(); i++ {
				vd := n.Child(i)
				if vd.IsNull() || vd.Type() != "variable_declaration" {
					continue
				}
				for j := uint32(0); j < vd.ChildCount(); j++ {
					declarator := vd.Child(j)
					if declarator.IsNull() || declarator.Type() != "variable_declarator" {
						continue
					}
					nameNode := firstChildOfType(declarator, "identifier")
					if nameNode.IsNull() {
						continue
					}
					evtName := string(src[nameNode.StartByte():nameNode.EndByte()])
					qualName := enclosingClass + "." + evtName
					nodeID := g.MakeNodeID(filePath, qualName)
					if g.GetNode(nodeID) != nil {
						continue
					}
					evtMeta := map[string]string{"kind": "event"}
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
						Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: evtMeta,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					classID := g.MakeNodeID(filePath, enclosingClass)
					if g.GetNode(classID) != nil {
						g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
					}
				}
			}

		case "indexer_declaration":
			// public T this[int index] { get; set; }
			if enclosingClass == "" {
				break
			}
			qualName := enclosingClass + ".this"
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break // Only one indexer node per type.
			}
			meta := map[string]string{"kind": "indexer"}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isCSharpPublic(n, src), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectCSharpCallSites performs a depth-first AST walk to collect call sites
// with function-level caller resolution.
func collectCSharpCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{
			"class_declaration":     true,
			"struct_declaration":    true,
			"interface_declaration": true,
			"record_declaration":    true,
		},
		FuncTypes: map[string]bool{
			"method_declaration":      true,
			"constructor_declaration": true,
		},
		CallTypes: map[string]bool{
			"invocation_expression":    true,
			"object_creation_expression": true,
		},
		NameExtractor: func(n sitter.Node, src []byte) string {
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		CalleeExtractor: func(n sitter.Node, src []byte) string {
			switch n.Type() {
			case "invocation_expression":
				fn := n.ChildByFieldName("function")
				if fn.IsNull() {
					return ""
				}
				switch fn.Type() {
				case "identifier":
					return string(src[fn.StartByte():fn.EndByte()])
				case "member_access_expression":
					if nameNode := fn.ChildByFieldName("name"); !nameNode.IsNull() {
						return string(src[nameNode.StartByte():nameNode.EndByte()])
					}
				}
			case "object_creation_expression":
				if typeNode := n.ChildByFieldName("type"); !typeNode.IsNull() {
					return string(src[typeNode.StartByte():typeNode.EndByte()])
				}
			}
			return ""
		},
		IsBuiltin: func(name string) bool { return false }, // C# has no builtins to filter
	})
}
