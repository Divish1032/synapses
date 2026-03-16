package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// extractJavaDeclInfo walks the Java AST collecting metadata for method,
// constructor, class, interface, enum, and record declarations.
// Method names are class-qualified (ClassName.methodName).
func extractJavaDeclInfo(root *sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n *sitter.Node, enclosingClass string, depth int)
	walk = func(n *sitter.Node, enclosingClass string, depth int) {
		if n == nil || depth > 8 {
			return
		}
		switch n.Type() {
		case "method_declaration":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"block"}),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "constructor_declaration":
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + ".constructor"
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"constructor_body", "block"}),
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
				_ = name // suppress unused
			}
		case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration",
			"annotation_type_declaration":
			className := ""
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[className] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			// Walk body with class context.
			if body := n.ChildByFieldName("body"); body != nil {
				for i := 0; i < int(body.ChildCount()); i++ {
					walk(body.Child(i), className, depth+1)
				}
			}
			return
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), enclosingClass, depth+1)
		}
	}
	walk(root, "", 0)
	return result
}

// JavaParser parses Java (.java) source files.
type JavaParser struct {
	language *sitter.Language
}

// NewJavaParser creates a ready-to-use JavaParser.
func NewJavaParser() *JavaParser {
	return &JavaParser{language: java.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *JavaParser) Extensions() []string {
	return []string{".java"}
}

// isJavaPublicNode checks if a declaration node has the "public" modifier
// by inspecting its modifier children. Falls back to true for top-level
// declarations (Java default package-private is still accessible within package).
func isJavaPublicNode(n *sitter.Node, src []byte) bool {
	if n == nil {
		return false
	}
	// Check for modifiers child.
	modifiers := n.ChildByFieldName("modifiers")
	if modifiers == nil {
		// Try finding a "modifiers" node among children.
		for i := 0; i < int(n.ChildCount()); i++ {
			child := n.Child(i)
			if child != nil && child.Type() == "modifiers" {
				modifiers = child
				break
			}
		}
	}
	if modifiers != nil {
		for i := 0; i < int(modifiers.ChildCount()); i++ {
			mod := modifiers.Child(i)
			if mod != nil {
				text := string(src[mod.StartByte():mod.EndByte()])
				if text == "public" {
					return true
				}
			}
		}
		return false
	}
	// No modifiers — default is package-private; treat as exported for graph purposes.
	return true
}

// Parse extracts code entities from a single Java file and merges them into the graph.
func (p *JavaParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractJavaDeclInfo(root, src)

	// --- package declaration ---
	pkgQuery := `(package_declaration (scoped_identifier) @pkg_name)`
	_ = runQuery(lang, root, src, pkgQuery, func(captures map[string]string, _ int) {
		pkgName := captures["pkg_name"]
		if pkgName == "" {
			return
		}
		// Set the file node's package.
		if fn := g.GetNode(fileNodeID); fn != nil {
			fn.Package = pkgName
		}
	})

	// --- import declarations ---
	importQuery := `(import_declaration (scoped_identifier) @import_path)`
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

	// --- All type and method declarations via AST walk (class-qualified) ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectJavaCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks the AST to extract classes, interfaces, enums,
// records, methods, and constructors with proper class-qualification.
func (p *JavaParser) extractAllDeclarations(
	g *graph.Graph,
	root *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
) {
	var walk func(n *sitter.Node, enclosingClass string)
	walk = func(n *sitter.Node, enclosingClass string) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "class_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			// Detect sealed modifier — check modifiers child (field or direct).
			var mods *sitter.Node
			mods = n.ChildByFieldName("modifiers")
			if mods == nil {
				for k := 0; k < int(n.ChildCount()); k++ {
					child := n.Child(k)
					if child != nil && child.Type() == "modifiers" {
						mods = child
						break
					}
				}
			}
			if mods != nil {
				for k := 0; k < int(mods.ChildCount()); k++ {
					mod := mods.Child(k)
					if mod != nil && string(src[mod.StartByte():mod.EndByte()]) == "sealed" {
						if meta == nil {
							meta = make(map[string]string, 1)
						}
						meta["kind"] = "sealed"
						break
					}
				}
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isJavaPublicNode(n, src),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Extract permitted subclasses from "permits" clause.
			for k := 0; k < int(n.ChildCount()); k++ {
				child := n.Child(k)
				if child == nil || child.Type() != "permits" {
					continue
				}
				if tl := firstChildOfType(child, "type_list"); tl != nil {
					for j := 0; j < int(tl.ChildCount()); j++ {
						ti := tl.Child(j)
						if ti == nil || ti.Type() != "type_identifier" {
							continue
						}
						subName := string(src[ti.StartByte():ti.EndByte()])
						subID := g.MakeNodeID(filePath, subName)
						if g.GetNode(subID) == nil {
							subMeta := map[string]string{"kind": "permitted"}
							g.AddNode(&graph.Node{
								ID: subID, Type: graph.NodeStruct, Name: subName, File: filePath,
								Line: int(ti.StartPoint().Row) + 1, Exported: true, Metadata: subMeta,
							})
							g.AddEdge(&graph.Edge{From: fileNodeID, To: subID, Type: graph.EdgeDefines})
						}
					}
				}
			}
			// Walk body with class context.
			if body := n.ChildByFieldName("body"); body != nil {
				for i := 0; i < int(body.ChildCount()); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "interface_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeInterface,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isJavaPublicNode(n, src),
				Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); body != nil {
				for i := 0; i < int(body.ChildCount()); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "enum_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
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
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isJavaPublicNode(n, src),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); body != nil {
				for i := 0; i < int(body.ChildCount()); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "record_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
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
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isJavaPublicNode(n, src),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := n.ChildByFieldName("body"); body != nil {
				for i := 0; i < int(body.ChildCount()); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "annotation_type_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "annotation"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeInterface,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isJavaPublicNode(n, src),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "method_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if enclosingClass != "" {
				qualName = enclosingClass + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isJavaPublicNode(n, src),
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Link class → method.
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "constructor_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode == nil {
				break
			}
			qualName := "constructor"
			if enclosingClass != "" {
				qualName = enclosingClass + ".constructor"
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isJavaPublicNode(n, src),
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
		case "enum_constant":
			// Java enum constant: enum Status { ACTIVE, INACTIVE, PENDING }
			if enclosingClass == "" {
				break
			}
			nameNode := firstChildOfType(n, "identifier")
			if nameNode == nil {
				break
			}
			constName := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := enclosingClass + "." + constName
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) != nil {
				break
			}
			enumMeta := map[string]string{"kind": "enum_constant"}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true, // enum constants are always public
				Metadata: enumMeta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectJavaCallSites performs a depth-first AST walk to collect call sites
// with function-level caller resolution.
func collectJavaCallSites(g *graph.Graph, _ *sitter.Language, root *sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{
			"class_declaration":          true,
			"interface_declaration":      true,
			"enum_declaration":           true,
			"record_declaration":         true,
			"annotation_type_declaration": true,
		},
		FuncTypes: map[string]bool{
			"method_declaration":      true,
			"constructor_declaration": true,
		},
		CallTypes: map[string]bool{
			"method_invocation":        true,
			"object_creation_expression": true,
		},
		NameExtractor: func(n *sitter.Node, src []byte) string {
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		CalleeExtractor: func(n *sitter.Node, src []byte) string {
			switch n.Type() {
			case "method_invocation":
				if nameNode := n.ChildByFieldName("name"); nameNode != nil {
					return string(src[nameNode.StartByte():nameNode.EndByte()])
				}
			case "object_creation_expression":
				if typeNode := n.ChildByFieldName("type"); typeNode != nil {
					return string(src[typeNode.StartByte():typeNode.EndByte()])
				}
			}
			return ""
		},
		IsBuiltin: isJavaBuiltin,
	})
}

// isJavaBuiltin returns true for common Java stdlib types/methods that should
// not generate CALLS edges.
func isJavaBuiltin(name string) bool {
	switch name {
	case "toString", "equals", "hashCode", "getClass", "notify", "notifyAll", "wait",
		"clone", "finalize", "compareTo", "iterator", "size", "get", "put",
		"add", "remove", "contains", "isEmpty", "clear", "toArray",
		"println", "print", "printf", "format",
		"valueOf", "parseInt", "parseDouble", "parseLong", "parseFloat",
		"String", "Integer", "Long", "Double", "Float", "Boolean", "Byte",
		"Character", "Short", "Object", "System", "Math", "Arrays", "Collections",
		"List", "Map", "Set", "Optional", "Stream":
		return true
	}
	return false
}
