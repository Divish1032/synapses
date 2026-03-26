package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/python"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// extractPythonDeclInfo walks the Python AST and builds a name→declMeta map
// for function_definition and class_definition nodes at any nesting depth.
// Method names are class-qualified (ClassName.method_name).
func extractPythonDeclInfo(root sitter.Node, src []byte, lines []string) map[string]declMeta {
	result := make(map[string]declMeta)

	var walk func(n sitter.Node, enclosingClass string, depth int)
	walk = func(n sitter.Node, enclosingClass string, depth int) {
		if n.IsNull() || depth > 8 {
			return
		}
		switch n.Type() {
		case "function_definition":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				// Try docstring first, then # comments.
				doc := extractPythonDocstring(lines, sl)
				if doc == "" {
					doc = extractLineDoc(lines, sl, "#")
				}
				result[qualName] = declMeta{
					Signature: extractSigToBody(n, src),
					Doc:       doc,
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class_definition":
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				doc := extractPythonDocstring(lines, sl)
				if doc == "" {
					doc = extractLineDoc(lines, sl, "#")
				}
				result[className] = declMeta{
					Doc:       doc,
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
			// Walk children with class context.
			body := n.ChildByFieldName("body")
			if !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), className, depth+1)
				}
			}
			return
		case "decorated_definition":
			for j := uint32(0); j < n.ChildCount(); j++ {
				inner := n.Child(j)
				if inner.IsNull() {
					continue
				}
				if inner.Type() == "function_definition" || inner.Type() == "class_definition" {
					walk(inner, enclosingClass, depth+1)
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

// PythonParser parses Python (.py, .pyi) source files.
type PythonParser struct {
	language *sitter.Language
}

// NewPythonParser creates a ready-to-use PythonParser.
func NewPythonParser() *PythonParser {
	return &PythonParser{language: sitter.NewLanguage(python.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *PythonParser) Extensions() []string {
	return []string{".py", ".pyi"}
}

func (p *PythonParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single Python file and merges them
// into the provided graph.
func (p *PythonParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	moduleName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:      fileNodeID,
		Type:    graph.NodeFile,
		Name:    filepath.Base(filePath),
		File:    filePath,
		Line:    1,
		Package: moduleName,
	})

	lang := p.language
	lines := strings.Split(string(src), "\n")
	declInfo := extractPythonDeclInfo(root, src, lines)

	// (class→method mapping is handled directly by extractFunctionsAndMethods AST walk)

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

	// --- from X import Y (absolute: from astropy.table import NdarrayMixin) ---
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

	// --- from .X import Y (relative: from .ndarray_mixin import NdarrayMixin) ---
	// Relative imports use a (relative_import) AST node containing a dotted_name.
	// Resolve the relative path to an absolute module name using the importer's
	// directory so the IMPORTS edge connects to the same package node that the
	// target module's entities use as their Package field.
	relImportQuery := `(import_from_statement module_name: (relative_import (dotted_name) @rel_path))`
	if err := runQuery(lang, root, src, relImportQuery, func(captures map[string]string, _ int) {
		relPath := captures["rel_path"]
		if relPath == "" {
			return
		}
		// Resolve relative import to the target module name and file path.
		// For "from .cli import X" in src/flask/testing.py:
		//   relPath = "cli" → target file = "src/flask/cli.py"
		importNodeID := g.MakeNodeID(relPath, relPath)
		// Compute target file: sibling in same directory.
		targetFile := filepath.Join(filepath.Dir(filePath), relPath+".py")
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    relPath,
			Package: relPath,
			File:    targetFile,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- Function / method definitions (class-qualified via AST walk) ---
	p.extractFunctionsAndMethods(g, root, src, filePath, moduleName, fileNodeID, declInfo)

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
			Package:  moduleName,
			File:     filePath,
			Line:     startLine,
			Exported: isPythonPublic(name),
			Metadata: buildLangMeta(declInfo[name]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- Module-level ALL_CAPS constants (e.g. MAX_RETRIES = 3, DEFAULT_TIMEOUT = 30) ---
	// Walk top-level assignment nodes; if the name is ALL_CAPS, emit a const node.
	var walkPyConst func(n sitter.Node)
	walkPyConst = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		// Only look at top-level: children of the module root.
		if n.Type() == "expression_statement" {
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() || child.Type() != "assignment" {
					continue
				}
				lhs := child.ChildByFieldName("left")
				if lhs.IsNull() {
					continue
				}
				var name string
				if lhs.Type() == "identifier" {
					name = string(src[lhs.StartByte():lhs.EndByte()])
				}
				if name == "" || !isPythonAllCaps(name) {
					continue
				}
				nodeID := g.MakeNodeID(filePath, name)
				if g.GetNode(nodeID) != nil {
					continue
				}
				meta := map[string]string{"kind": "const"}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeStruct,
					Name:     name,
					Package:  moduleName,
					File:     filePath,
					Line:     int(lhs.StartPoint().Row) + 1,
					Exported: isPythonPublic(name),
					Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
	}
	// Only walk direct children of the module root (depth=1 only).
	for i := uint32(0); i < root.ChildCount(); i++ {
		walkPyConst(root.Child(i))
	}

	// --- Call sites ---
	collectPythonCallSites(g, lang, root, src, filePath, moduleName)

	return nil
}

// extractFunctionsAndMethods walks the AST to emit function and method nodes
// with class-qualified names for methods inside class bodies.
func (p *PythonParser) extractFunctionsAndMethods(
	g *graph.Graph,
	root sitter.Node,
	src []byte,
	filePath, moduleName string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
) {
	// emitFunc creates a function/method node for a function_definition node,
	// applying any decorator metadata (property, classmethod, staticmethod).
	emitFunc := func(n sitter.Node, enclosingClass string, decorators []string) {
		nameNode := n.ChildByFieldName("name")
		if nameNode.IsNull() {
			return
		}
		name := string(src[nameNode.StartByte():nameNode.EndByte()])
		startLine := int(n.StartPoint().Row) + 1

		// Determine kind from decorators.
		decoratorKind := ""
		for _, d := range decorators {
			switch d {
			case "property":
				decoratorKind = "property"
			case "classmethod":
				decoratorKind = "classmethod"
			case "staticmethod":
				decoratorKind = "staticmethod"
			}
		}

		if enclosingClass != "" {
			qualName := enclosingClass + "." + name
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := buildLangMeta(declInfo[qualName])
			if decoratorKind != "" {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = decoratorKind
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				Package:  moduleName,
				File:     filePath,
				Line:     startLine,
				Exported: isPythonPublic(name),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			classID := g.MakeNodeID(filePath, enclosingClass)
			if g.GetNode(classID) != nil {
				g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
			}
		} else {
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if decoratorKind != "" {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = decoratorKind
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     name,
				Package:  moduleName,
				File:     filePath,
				Line:     startLine,
				Exported: isPythonPublic(name),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "class_definition":
			className := ""
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				className = string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			body := n.ChildByFieldName("body")
			if !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), className)
				}
			}
			return
		case "expression_statement":
			// Type-annotated class fields: name: Type (= value)?
			// AST: expression_statement → assignment → identifier, ":", type[, "=", value]
			if enclosingClass != "" {
				for i := uint32(0); i < n.ChildCount(); i++ {
					assign := n.Child(i)
					if assign.IsNull() || assign.Type() != "assignment" {
						continue
					}
					// Must have a "type" child to be a type-annotated field.
					if firstChildOfType(assign, "type").IsNull() {
						continue
					}
					nameNode := firstChildOfType(assign, "identifier")
					if nameNode.IsNull() {
						continue
					}
					fieldName := string(src[nameNode.StartByte():nameNode.EndByte()])
					qualName := enclosingClass + "." + fieldName
					nodeID := g.MakeNodeID(filePath, qualName)
					if g.GetNode(nodeID) != nil {
						continue
					}
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
						Line:     int(assign.StartPoint().Row) + 1,
						Exported: isPythonPublic(fieldName),
						Metadata: map[string]string{"kind": "field"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					classID := g.MakeNodeID(filePath, enclosingClass)
					if g.GetNode(classID) != nil {
						g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
					}
				}
			}
			// Fall through to recurse (may contain nested function defs).
		case "decorated_definition":
			// Collect decorator names then dispatch to the inner definition.
			var decorators []string
			for j := uint32(0); j < n.ChildCount(); j++ {
				child := n.Child(j)
				if child.IsNull() {
					continue
				}
				if child.Type() == "decorator" {
					decText := strings.TrimPrefix(strings.TrimSpace(string(src[child.StartByte():child.EndByte()])), "@")
					if idx := strings.IndexByte(decText, '('); idx >= 0 {
						decText = decText[:idx]
					}
					decorators = append(decorators, strings.TrimSpace(decText))
				}
			}
			for j := uint32(0); j < n.ChildCount(); j++ {
				child := n.Child(j)
				if child.IsNull() {
					continue
				}
				if child.Type() == "function_definition" {
					emitFunc(child, enclosingClass, decorators)
				} else if child.Type() == "class_definition" {
					walk(child, enclosingClass)
				}
			}
			return
		case "function_definition":
			emitFunc(n, enclosingClass, nil)
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectPythonCallSites performs a depth-first AST walk to collect call sites
// with function-level caller resolution.
func collectPythonCallSites(g *graph.Graph, lang *sitter.Language, root sitter.Node, src []byte, filePath, _ string) {
	// Collect variable type annotations for cross-file obj.method() resolution.
	collectPythonVarTypes(g, lang, root, src, filePath)

	fileNodeID := g.MakeNodeID(filePath, filePath)
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{"class_definition": true},
		FuncTypes: map[string]bool{
			"function_definition":  true,
			"decorated_definition": false, // walk through decorators, not into them
		},
		CallTypes: map[string]bool{"call": true},
		NameExtractor: func(n sitter.Node, src []byte) string {
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		// AliasedCalleeExtractor extracts (object, method) so the resolver can
		// distinguish `repo.save()` (PkgAlias="repo") from bare `save()` (PkgAlias="").
		AliasedCalleeExtractor: func(n sitter.Node, src []byte) (alias, name string) {
			fn := n.ChildByFieldName("function")
			if fn.IsNull() {
				return "", ""
			}
			switch fn.Type() {
			case "identifier":
				return "", string(src[fn.StartByte():fn.EndByte()])
			case "attribute":
				obj := fn.ChildByFieldName("object")
				attr := fn.ChildByFieldName("attribute")
				if !attr.IsNull() {
					var objName string
					if !obj.IsNull() {
						objName = string(src[obj.StartByte():obj.EndByte()])
					}
					return objName, string(src[attr.StartByte():attr.EndByte()])
				}
			}
			return "", ""
		},
		IsBuiltin: isBuiltinPython,
	})
}

// collectPythonVarTypes walks the AST to extract variable → type mappings for
// cross-file call resolution. Records four patterns:
//   - Annotated assignments:        obj: ClassName = ...
//   - Constructor assignments:      obj = ClassName(...)
//   - Function parameter types:     def f(repo: Repository, ...) → repo → Repository
//   - Self-attribute constructors:  self.attr = ClassName(...) → "self.attr" → ClassName
func collectPythonVarTypes(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "assignment":
			left := n.ChildByFieldName("left")
			right := n.ChildByFieldName("right")
			typeAnnot := n.ChildByFieldName("type")

			if left.IsNull() {
				goto recurse
			}

			if left.Type() == "identifier" {
				varName := string(src[left.StartByte():left.EndByte()])
				if varName == "self" || varName == "cls" {
					goto recurse
				}

				// Pattern 1: obj: TypeName = ...
				if !typeAnnot.IsNull() {
					typeName := extractPythonTypeName(typeAnnot, src)
					if typeName != "" {
						g.AddVarType(filePath, varName, typeName)
						goto recurse
					}
				}

				// Pattern 2: obj = TypeName(...) — constructor call
				if !right.IsNull() && right.Type() == "call" {
					fn := right.ChildByFieldName("function")
					if !fn.IsNull() && fn.Type() == "identifier" {
						typeName := string(src[fn.StartByte():fn.EndByte()])
						// Only record if it looks like a class name (starts with uppercase).
						if typeName != "" && typeName[0] >= 'A' && typeName[0] <= 'Z' {
							g.AddVarType(filePath, varName, typeName)
						}
					}
				}

			} else if left.Type() == "attribute" {
				// Pattern 3: self.attr = ClassName(...) — store "self.attr" → ClassName.
				// The call-site extractor produces PkgAlias="self.attr" for self.attr.method(),
				// so keying by the full attribute text enables cross-file resolution.
				obj := left.ChildByFieldName("object")
				if obj.IsNull() || obj.Type() != "identifier" {
					goto recurse
				}
				if string(src[obj.StartByte():obj.EndByte()]) != "self" {
					goto recurse
				}
				if right.IsNull() || right.Type() != "call" {
					goto recurse
				}
				fn := right.ChildByFieldName("function")
				if fn.IsNull() || fn.Type() != "identifier" {
					goto recurse
				}
				typeName := string(src[fn.StartByte():fn.EndByte()])
				if typeName != "" && typeName[0] >= 'A' && typeName[0] <= 'Z' {
					attrKey := string(src[left.StartByte():left.EndByte()])
					g.AddVarType(filePath, attrKey, typeName)
				}
			}

		case "function_definition":
			// Pattern 4: typed function parameters.
			// def process(repo: Repository, size: int = 0) → repo → Repository
			params := n.ChildByFieldName("parameters")
			if !params.IsNull() {
				collectPythonParamTypes(g, params, src, filePath)
			}
		}

	recurse:
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// collectPythonParamTypes extracts typed parameter annotations from a Python
// parameters node and records them in the graph's varTypes store.
// Handles typed_parameter and typed_default_parameter nodes:
//   - typed_parameter:         (identifier) (type (identifier))
//   - typed_default_parameter: (identifier) (type (identifier)) (default)
func collectPythonParamTypes(g *graph.Graph, params sitter.Node, src []byte, filePath string) {
	for i := uint32(0); i < params.ChildCount(); i++ {
		param := params.Child(i)
		if param.IsNull() {
			continue
		}
		if param.Type() != "typed_parameter" && param.Type() != "typed_default_parameter" {
			continue
		}
		// The type annotation is in the "type" field.
		typeAnnot := param.ChildByFieldName("type")
		if typeAnnot.IsNull() {
			continue
		}
		typeName := extractPythonTypeName(typeAnnot, src)
		if typeName == "" {
			continue
		}
		// The parameter name is the first identifier child.
		for j := uint32(0); j < param.ChildCount(); j++ {
			child := param.Child(j)
			if child.IsNull() {
				continue
			}
			if child.Type() == "identifier" {
				varName := string(src[child.StartByte():child.EndByte()])
				if varName != "self" && varName != "cls" && varName != "" {
					g.AddVarType(filePath, varName, typeName)
				}
				break
			}
		}
	}
}

// extractPythonTypeName extracts the concrete class name from a Python type
// annotation node (the "type" field of assignment/typed_parameter).
//
// Tree-sitter Python wraps the actual type expression inside a "type" node.
// This function walks the children of that wrapper to find the real type:
//
//   - identifier:    Repository → "Repository"
//   - generic_type:  Optional[X] / Union[X,None] / List[X] / ClassVar[X]
//   - binary_operator: X | None (PEP 604)
//
// For Optional/Union/ClassVar/Final/Annotated, the inner type is unwrapped.
// For container types (List, Dict, Set), the outer name is returned and the
// caller may filter via isBuiltin.
func extractPythonTypeName(typeNode sitter.Node, src []byte) string {
	if typeNode.IsNull() {
		return ""
	}
	// Walk immediate children — "type" is a thin wrapper around the expression.
	for i := uint32(0); i < typeNode.ChildCount(); i++ {
		child := typeNode.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			return string(src[child.StartByte():child.EndByte()])

		case "generic_type":
			// generic_type: Optional[X], Union[X,None], List[X], etc.
			// Structure: generic_type → identifier("Optional") + type_parameter → type → identifier
			outerName := extractGenericOuterName(child, src)
			switch outerName {
			case "Optional", "ClassVar", "Final":
				// Single-arg generics: unwrap to the inner type.
				if inner := extractGenericNthType(child, src, 0); inner != "" {
					return inner
				}
				return "" // fallback: wrapper type (Optional etc.) is not useful for resolution
			case "Union":
				// Union[Service, None] → first non-None uppercase type
				if inner := extractUnionInnerType(child, src); inner != "" {
					return inner
				}
				return "" // Union with no extractable concrete type
			case "Annotated":
				// Annotated[X, metadata] → first type arg is the real type
				if inner := extractGenericNthType(child, src, 0); inner != "" {
					return inner
				}
			default:
				// List[X], Dict[K,V], etc. — extract inner type; container name
				// alone would produce incorrect type resolution.
				if inner := extractGenericNthType(child, src, 0); inner != "" {
					return inner
				}
				return ""
			}

		case "binary_operator":
			// PEP 604: Service | None → "Service"
			if name := extractPEP604Type(child, src); name != "" {
				return name
			}
		}
	}
	return ""
}

// extractGenericOuterName returns the base type name from a generic_type node.
// e.g. generic_type for Optional[Repository] → "Optional"
func extractGenericOuterName(genericNode sitter.Node, src []byte) string {
	for i := uint32(0); i < genericNode.ChildCount(); i++ {
		child := genericNode.Child(i)
		if !child.IsNull() && child.Type() == "identifier" {
			return string(src[child.StartByte():child.EndByte()])
		}
	}
	return ""
}

// extractGenericNthType extracts the Nth type argument from a generic_type node.
// The structure is: generic_type → type_parameter → (type children, commas, brackets)
// Optional[Repository] → idx=0 → "Repository"
// Annotated[X, meta]  → idx=0 → "X"
func extractGenericNthType(genericNode sitter.Node, src []byte, idx int) string {
	for i := uint32(0); i < genericNode.ChildCount(); i++ {
		tp := genericNode.Child(i)
		if tp.IsNull() || tp.Type() != "type_parameter" {
			continue
		}
		count := 0
		for j := uint32(0); j < tp.ChildCount(); j++ {
			typeChild := tp.Child(j)
			if typeChild.IsNull() || typeChild.Type() != "type" {
				continue
			}
			if count == idx {
				// Extract identifier from this type node.
				for k := uint32(0); k < typeChild.ChildCount(); k++ {
					id := typeChild.Child(k)
					if id.IsNull() || id.Type() != "identifier" {
						continue
					}
					name := string(src[id.StartByte():id.EndByte()])
					if name != "" && name[0] >= 'A' && name[0] <= 'Z' {
						return name
					}
				}
			}
			count++
		}
	}
	return ""
}

// extractUnionInnerType returns the first non-None uppercase type from
// a Union[...] generic_type node.
// Union[AuthService, None] → "AuthService"
func extractUnionInnerType(genericNode sitter.Node, src []byte) string {
	for i := uint32(0); i < genericNode.ChildCount(); i++ {
		tp := genericNode.Child(i)
		if tp.IsNull() || tp.Type() != "type_parameter" {
			continue
		}
		for j := uint32(0); j < tp.ChildCount(); j++ {
			typeChild := tp.Child(j)
			if typeChild.IsNull() || typeChild.Type() != "type" {
				continue
			}
			for k := uint32(0); k < typeChild.ChildCount(); k++ {
				id := typeChild.Child(k)
				if id.IsNull() || id.Type() != "identifier" {
					continue
				}
				name := string(src[id.StartByte():id.EndByte()])
				if name != "" && name != "None" && name[0] >= 'A' && name[0] <= 'Z' {
					return name
				}
			}
		}
	}
	return ""
}

// extractPEP604Type extracts the concrete type from PEP 604 union syntax:
// Repository | None → "Repository", None | Service → "Service".
// Returns the first non-None uppercase identifier operand.
func extractPEP604Type(binop sitter.Node, src []byte) string {
	// Binary operator children: left operand, operator, right operand.
	// We scan for identifier children, skip "None".
	for k := uint32(0); k < binop.ChildCount(); k++ {
		child := binop.Child(k)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			name := string(src[child.StartByte():child.EndByte()])
			if name != "None" && name != "" && name[0] >= 'A' && name[0] <= 'Z' {
				return name
			}
		case "binary_operator":
			// Nested: A | B | None — recurse
			if name := extractPEP604Type(child, src); name != "" {
				return name
			}
		}
	}
	return ""
}

// isPythonPublic returns true if the name is not prefixed with an underscore.
func isPythonPublic(name string) bool {
	return !strings.HasPrefix(name, "_")
}

// isPythonAllCaps returns true if name is a conventional Python constant
// (all uppercase letters, digits, and underscores, at least one letter).
func isPythonAllCaps(name string) bool {
	if name == "" {
		return false
	}
	hasLetter := false
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			hasLetter = true
		} else if r == '_' || (r >= '0' && r <= '9') {
			// allowed
		} else {
			return false
		}
	}
	return hasLetter
}

// isBuiltinPython returns true for Python built-in functions and common stdlib
// calls that should never generate CALLS edges.
func isBuiltinPython(name string) bool {
	switch name {
	case "print", "len", "range", "enumerate", "zip", "map", "filter", "sorted",
		"list", "dict", "set", "tuple", "str", "int", "float", "bool", "bytes",
		"type", "isinstance", "issubclass", "hasattr", "getattr", "setattr",
		"open", "super", "property", "staticmethod", "classmethod",
		"abs", "max", "min", "sum", "any", "all", "next", "iter",
		"repr", "hash", "id", "callable", "vars", "dir", "object",
		"Exception", "ValueError", "TypeError", "KeyError", "IndexError",
		"RuntimeError", "StopIteration", "NotImplementedError", "IOError",
		"input", "format", "round", "ord", "chr", "hex", "oct", "bin",
		"reversed", "frozenset", "memoryview", "bytearray", "complex",
		"delattr", "compile", "eval", "exec", "globals", "locals",
		"breakpoint", "help", "ascii", "pow", "divmod", "slice",
		"AttributeError", "FileNotFoundError", "PermissionError",
		"OSError", "ImportError", "ModuleNotFoundError":
		return true
	}
	return false
}
