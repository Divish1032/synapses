package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/java"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// extractJavaDeclInfo walks the Java AST collecting metadata for method,
// constructor, class, interface, enum, and record declarations.
// Method names are class-qualified (ClassName.methodName).
func extractJavaDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n sitter.Node, enclosingClass string, depth int)
	walk = func(n sitter.Node, enclosingClass string, depth int) {
		if n.IsNull() || depth > 8 {
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
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "constructor_declaration":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
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
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[className] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			// Walk body with class context.
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), className, depth+1)
				}
			}
			return
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
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
	return &JavaParser{language: sitter.NewLanguage(java.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *JavaParser) Extensions() []string {
	return []string{".java"}
}

func (p *JavaParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// isJavaPublicNode checks if a declaration node has the "public" modifier
// by inspecting its modifier children. Falls back to true for top-level
// declarations (Java default package-private is still accessible within package).
func isJavaPublicNode(n sitter.Node, src []byte) bool {
	if n.IsNull() {
		return false
	}
	// Check for modifiers child.
	modifiers := n.ChildByFieldName("modifiers")
	if modifiers.IsNull() {
		// Try finding a "modifiers" node among children.
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if !child.IsNull() && child.Type() == "modifiers" {
				modifiers = child
				break
			}
		}
	}
	if !modifiers.IsNull() {
		for i := uint32(0); i < modifiers.ChildCount(); i++ {
			mod := modifiers.Child(i)
			if !mod.IsNull() {
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
	root sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
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
			// Detect sealed modifier — check modifiers child (field or direct).
			var mods sitter.Node
			mods = n.ChildByFieldName("modifiers")
			if mods.IsNull() {
				for k := uint32(0); k < n.ChildCount(); k++ {
					child := n.Child(k)
					if !child.IsNull() && child.Type() == "modifiers" {
						mods = child
						break
					}
				}
			}
			if !mods.IsNull() {
				for k := uint32(0); k < mods.ChildCount(); k++ {
					mod := mods.Child(k)
					if !mod.IsNull() && string(src[mod.StartByte():mod.EndByte()]) == "sealed" {
						if meta == nil {
							meta = make(map[string]string, 1)
						}
						meta["kind"] = "sealed"
						break
					}
				}
			}
			// Heritage clause extraction: implements/extends.
			meta = extractJavaHeritage(n, src, meta)
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
			for k := uint32(0); k < n.ChildCount(); k++ {
				child := n.Child(k)
				if child.IsNull() || child.Type() != "permits" {
					continue
				}
				if tl := firstChildOfType(child, "type_list"); !tl.IsNull() {
					for j := uint32(0); j < tl.ChildCount(); j++ {
						ti := tl.Child(j)
						if ti.IsNull() || ti.Type() != "type_identifier" {
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
				ID:       nodeID,
				Type:     graph.NodeInterface,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isJavaPublicNode(n, src),
				Metadata: buildLangMeta(declInfo[name]),
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
			meta = extractJavaHeritage(n, src, meta)
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
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

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
			meta = extractJavaHeritage(n, src, meta)
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
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "annotation_type_declaration":
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
			if nameNode.IsNull() {
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
			if nameNode.IsNull() {
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
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectJavaCallSites performs a depth-first AST walk to collect call sites
// with function-level caller resolution.
func collectJavaCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Collect variable type declarations for cross-file obj.method() resolution.
	collectJavaVarTypes(g, root, src, filePath)

	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{
			"class_declaration":           true,
			"interface_declaration":       true,
			"enum_declaration":            true,
			"record_declaration":          true,
			"annotation_type_declaration": true,
		},
		FuncTypes: map[string]bool{
			"method_declaration":      true,
			"constructor_declaration": true,
		},
		CallTypes: map[string]bool{
			"method_invocation":          true,
			"object_creation_expression": true,
		},
		NameExtractor: func(n sitter.Node, src []byte) string {
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		// AliasedCalleeExtractor extracts (object, method) for method_invocation nodes
		// so the resolver can use the object name to narrow cross-file type resolution.
		AliasedCalleeExtractor: func(n sitter.Node, src []byte) (alias, name string) {
			switch n.Type() {
			case "method_invocation":
				nameNode := n.ChildByFieldName("name")
				if nameNode.IsNull() {
					return "", ""
				}
				methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
				objNode := n.ChildByFieldName("object")
				var objName string
				if !objNode.IsNull() {
					// Use only simple identifiers as alias — skip chained or complex expressions.
					if objNode.Type() == "identifier" {
						objName = string(src[objNode.StartByte():objNode.EndByte()])
					}
				}
				return objName, methodName
			case "object_creation_expression":
				typeNode := n.ChildByFieldName("type")
				if typeNode.IsNull() {
					return "", ""
				}
				return "", string(src[typeNode.StartByte():typeNode.EndByte()])
			}
			return "", ""
		},
		IsBuiltin: isJavaBuiltin,
	})
}

// collectJavaVarTypes walks the AST to extract variable → type mappings for
// cross-file call resolution. Records field declarations and local variable declarations.
func collectJavaVarTypes(g *graph.Graph, root sitter.Node, src []byte, filePath string) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "field_declaration", "local_variable_declaration":
			typeNode := n.ChildByFieldName("type")
			if typeNode.IsNull() {
				break
			}
			typeName := string(src[typeNode.StartByte():typeNode.EndByte()])
			// Skip primitive types and common stdlib generics.
			if typeName == "" || isJavaBuiltin(typeName) {
				break
			}
			// Walk declarators to get variable names.
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() || child.Type() != "variable_declarator" {
					continue
				}
				nameNode := child.ChildByFieldName("name")
				if nameNode.IsNull() {
					continue
				}
				varName := string(src[nameNode.StartByte():nameNode.EndByte()])
				if varName != "" {
					g.AddVarType(filePath, varName, typeName)
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// extractJavaHeritage extracts implements/extends clauses from a Java class,
// enum, or record declaration and stores them in metadata.
func extractJavaHeritage(n sitter.Node, src []byte, meta map[string]string) map[string]string {
	// Java class_declaration has "interfaces" field (type_list) and "superclass" field.
	var implementsNames, extendsNames []string

	if ifaces := n.ChildByFieldName("interfaces"); !ifaces.IsNull() {
		implementsNames = extractTypeIdentifiers(ifaces, src)
	}
	if super := n.ChildByFieldName("superclass"); !super.IsNull() {
		extendsNames = extractTypeIdentifiers(super, src)
	}

	if len(implementsNames) == 0 && len(extendsNames) == 0 {
		return meta
	}

	if meta == nil {
		meta = make(map[string]string)
	}
	if len(implementsNames) > 0 {
		meta["heritage_implements"] = strings.Join(implementsNames, ",")
	}
	if len(extendsNames) > 0 {
		meta["heritage_extends"] = strings.Join(extendsNames, ",")
	}
	return meta
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
