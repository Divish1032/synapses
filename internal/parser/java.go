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
func extractJavaDeclInfo(root sitter.Node, src []byte, lines []string) map[string]declMeta {
	result := make(map[string]declMeta)

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
	lines := strings.Split(string(src), "\n")
	declInfo := extractJavaDeclInfo(root, src, lines)

	// --- package declaration ---
	// Java packages: scoped (com.example.app) or single (retrofit2).
	var javaPackage string
	for _, pkgQ := range []string{
		`(package_declaration (scoped_identifier) @pkg_name)`,
		`(package_declaration (identifier) @pkg_name)`,
	} {
		if javaPackage != "" {
			break
		}
		_ = runQuery(lang, root, src, pkgQ, func(captures map[string]string, _ int) {
			if n := captures["pkg_name"]; n != "" {
				javaPackage = n
			}
		})
	}
	// Set file node package.
	if javaPackage != "" {
		if fn := g.GetNode(fileNodeID); fn != nil {
			fn.Package = javaPackage
		}
	}
	// Fallback: use directory name as package.
	if javaPackage == "" {
		dirName := filepath.Base(filepath.Dir(filePath))
		if dirName != "" && dirName != "." {
			javaPackage = dirName
		} else {
			javaPackage = strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
		}
	}

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
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo, javaPackage)

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
	javaPackage string,
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

	// Post-process: set Package on all nodes in this file that don't have it.
	// This ensures same-package call resolution works for Java.
	if javaPackage != "" {
		for _, n := range g.FindByFile(filePath) {
			if n.Package == "" && n.Type != graph.NodeFile && n.Type != graph.NodePackage {
				n.Package = javaPackage
			}
		}
	}
}

// collectJavaCallSites performs a depth-first AST walk to collect call sites
// with function-level caller resolution.
func collectJavaCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Collect variable type declarations for cross-file obj.method() resolution.
	collectJavaVarTypes(g, root, src, filePath)
	// Collect instantiated types for RTA-style call graph refinement.
	collectJavaInstantiatedTypes(g, root, src, filePath)
	// Collect framework-annotated and enum instantiations.
	collectJavaAnnotatedInstantiations(g, root, src, filePath)

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
// cross-file call resolution. Records three patterns:
//   - Field declarations:          Repository repo;
//   - Local variable declarations: Repository repo = factory.get();
//   - Method/constructor params:   void process(Repository repo) → repo → Repository
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
			typeName := extractJavaSimpleTypeName(typeNode, src)
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

		case "method_declaration", "constructor_declaration":
			// Extract typed formal parameters so method-body call resolution works.
			// void process(Repository repo, AuthService auth) → repo→Repository, auth→AuthService
			params := n.ChildByFieldName("parameters")
			if params.IsNull() {
				break
			}
			collectJavaFormalParamTypes(g, params, src, filePath)
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// collectJavaFormalParamTypes extracts type annotations from formal_parameter and
// spread_parameter nodes within a method's parameter list.
func collectJavaFormalParamTypes(g *graph.Graph, params sitter.Node, src []byte, filePath string) {
	for i := uint32(0); i < params.ChildCount(); i++ {
		param := params.Child(i)
		if param.IsNull() {
			continue
		}
		if param.Type() != "formal_parameter" && param.Type() != "spread_parameter" {
			continue
		}
		typeNode := param.ChildByFieldName("type")
		if typeNode.IsNull() {
			continue
		}
		typeName := extractJavaSimpleTypeName(typeNode, src)
		if typeName == "" || isJavaBuiltin(typeName) {
			continue
		}
		nameNode := param.ChildByFieldName("name")
		if nameNode.IsNull() {
			continue
		}
		varName := string(src[nameNode.StartByte():nameNode.EndByte()])
		if varName != "" {
			g.AddVarType(filePath, varName, typeName)
		}
	}
}

// collectJavaInstantiatedTypes walks the AST for object_creation_expression nodes
// (new Foo(...)) and records the constructed type names via AddInstantiatedType.
// This enables RTA-style call graph refinement in the resolver — when multiple
// methods share the same name, prefer candidates whose receiver type is in the
// instantiated set, reducing false-positive CALLS edges by 10-40% in codebases
// with deep class hierarchies.
func collectJavaInstantiatedTypes(g *graph.Graph, root sitter.Node, src []byte, filePath string) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "object_creation_expression" {
			typeNode := n.ChildByFieldName("type")
			if !typeNode.IsNull() {
				typeName := extractJavaSimpleTypeName(typeNode, src)
				if typeName != "" && !isJavaBuiltin(typeName) {
					g.AddInstantiatedType(filePath, typeName)
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// javadiAnnotations is the set of DI/framework annotations that indicate a class
// is instantiated by a framework container rather than via explicit new.
var javaDIAnnotations = map[string]bool{
	"Component": true, "Service": true, "Repository": true,
	"Controller": true, "RestController": true, "Configuration": true,
	"Bean": true, "Singleton": true, "Managed": true,
	"Entity": true, "MappedSuperclass": true, "Embeddable": true,
	"Named": true, "ApplicationScoped": true, "RequestScoped": true,
	"SessionScoped": true, "Stateless": true, "Stateful": true,
	"MessageDriven": true,
}

// extractJavaModifierAnnotations returns annotation names from a modifiers node.
func extractJavaModifierAnnotations(modifiers sitter.Node, src []byte) []string {
	if modifiers.IsNull() {
		return nil
	}
	var names []string
	for i := uint32(0); i < modifiers.ChildCount(); i++ {
		child := modifiers.Child(i)
		if child.IsNull() {
			continue
		}
		// annotation or marker_annotation
		if child.Type() == "annotation" || child.Type() == "marker_annotation" {
			nameNode := child.ChildByFieldName("name")
			if nameNode.IsNull() {
				// fallback: first identifier child
				nameNode = firstChildOfType(child, "identifier")
			}
			if !nameNode.IsNull() {
				names = append(names, string(src[nameNode.StartByte():nameNode.EndByte()]))
			}
		}
	}
	return names
}

// javaHasModifier checks whether a node's modifiers include a given keyword.
func javaHasModifier(n sitter.Node, src []byte, keyword string) bool {
	modifiers := n.ChildByFieldName("modifiers")
	if modifiers.IsNull() {
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if !child.IsNull() && child.Type() == "modifiers" {
				modifiers = child
				break
			}
		}
	}
	if modifiers.IsNull() {
		return false
	}
	for i := uint32(0); i < modifiers.ChildCount(); i++ {
		child := modifiers.Child(i)
		if !child.IsNull() && string(src[child.StartByte():child.EndByte()]) == keyword {
			return true
		}
	}
	return false
}

// collectJavaAnnotatedInstantiations records class names as instantiated when:
//  1. The class carries a DI annotation (@Service, @Component, etc.)
//  2. A @Bean method in a @Configuration class returns a non-builtin type
//  3. An enum_declaration has at least one enum_constant child
//  4. A static method's return type matches the enclosing class (static factory)
func collectJavaAnnotatedInstantiations(g *graph.Graph, root sitter.Node, src []byte, filePath string) {
	var walk func(n sitter.Node, enclosingClass string, enclosingIsConfig bool)
	walk = func(n sitter.Node, enclosingClass string, enclosingIsConfig bool) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "class_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			className := string(src[nameNode.StartByte():nameNode.EndByte()])

			// Check modifiers for DI annotations.
			modifiers := n.ChildByFieldName("modifiers")
			if modifiers.IsNull() {
				for i := uint32(0); i < n.ChildCount(); i++ {
					child := n.Child(i)
					if !child.IsNull() && child.Type() == "modifiers" {
						modifiers = child
						break
					}
				}
			}
			annotations := extractJavaModifierAnnotations(modifiers, src)
			isConfig := false
			for _, ann := range annotations {
				if javaDIAnnotations[ann] && !isJavaBuiltin(className) {
					g.AddInstantiatedType(filePath, className)
				}
				if ann == "Configuration" {
					isConfig = true
				}
			}

			// Walk body with class context.
			body := n.ChildByFieldName("body")
			if !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), className, isConfig)
				}
			}
			return

		case "enum_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			enumName := string(src[nameNode.StartByte():nameNode.EndByte()])
			// Record if there is at least one enum_constant.
			body := n.ChildByFieldName("body")
			if !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					child := body.Child(i)
					if !child.IsNull() && child.Type() == "enum_constant" {
						if !isJavaBuiltin(enumName) {
							g.AddInstantiatedType(filePath, enumName)
						}
						break
					}
				}
				// Still walk body for nested classes.
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), enumName, false)
				}
			}
			return

		case "method_declaration":
			if enclosingClass == "" {
				break
			}
			// Static factory: static method whose return type equals enclosing class.
			if javaHasModifier(n, src, "static") {
				retNode := n.ChildByFieldName("type")
				if !retNode.IsNull() {
					retType := extractJavaSimpleTypeName(retNode, src)
					if retType == enclosingClass && !isJavaBuiltin(retType) {
						g.AddInstantiatedType(filePath, enclosingClass)
					}
				}
			}
			// @Bean method in @Configuration class: return type is instantiated by Spring.
			if enclosingIsConfig {
				modifiers := n.ChildByFieldName("modifiers")
				if modifiers.IsNull() {
					for i := uint32(0); i < n.ChildCount(); i++ {
						child := n.Child(i)
						if !child.IsNull() && child.Type() == "modifiers" {
							modifiers = child
							break
						}
					}
				}
				anns := extractJavaModifierAnnotations(modifiers, src)
				for _, ann := range anns {
					if ann == "Bean" {
						retNode := n.ChildByFieldName("type")
						if !retNode.IsNull() {
							retType := extractJavaSimpleTypeName(retNode, src)
							if retType != "" && !isJavaBuiltin(retType) {
								g.AddInstantiatedType(filePath, retType)
							}
						}
						break
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass, enclosingIsConfig)
		}
	}
	walk(root, "", false)
}

// extractJavaSimpleTypeName returns the bare type name from a Java type node.
// Handles type_identifier, generic_type (List<Foo> → "List"), and array_type.
// Returns "" for primitive or void types — no class exists to resolve.
func extractJavaSimpleTypeName(typeNode sitter.Node, src []byte) string {
	if typeNode.IsNull() {
		return ""
	}
	switch typeNode.Type() {
	case "type_identifier":
		return string(src[typeNode.StartByte():typeNode.EndByte()])
	case "generic_type":
		// e.g. List<Repository> — take the base type identifier
		for i := uint32(0); i < typeNode.ChildCount(); i++ {
			child := typeNode.Child(i)
			if !child.IsNull() && child.Type() == "type_identifier" {
				return string(src[child.StartByte():child.EndByte()])
			}
		}
	case "array_type":
		// e.g. Repository[] — take the element type
		elem := typeNode.ChildByFieldName("element")
		if !elem.IsNull() {
			return extractJavaSimpleTypeName(elem, src)
		}
	case "integral_type", "boolean_type", "void_type", "floating_point_type":
		// Primitive/void — no class to look up. Return "" to skip.
		return ""
	default:
		// Unknown type nodes: return the text; caller filters via isJavaBuiltin.
		return string(src[typeNode.StartByte():typeNode.EndByte()])
	}
	return ""
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
		"List", "Map", "Set", "Optional", "Stream",
		// Concrete stdlib collection and utility implementations.
		"ArrayList", "LinkedList", "HashMap", "TreeMap", "LinkedHashMap",
		"HashSet", "TreeSet", "LinkedHashSet", "ArrayDeque", "PriorityQueue",
		"StringBuilder", "StringBuffer", "Scanner", "Random", "Thread",
		"RuntimeException", "Exception", "IllegalArgumentException",
		"IllegalStateException", "NullPointerException", "UnsupportedOperationException":
		return true
	}
	return false
}
