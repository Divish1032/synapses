package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/groovy"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// GroovyParser parses Groovy (.groovy, .gradle) source files.
type GroovyParser struct {
	language *sitter.Language
}

// NewGroovyParser creates a ready-to-use GroovyParser.
func NewGroovyParser() *GroovyParser {
	return &GroovyParser{language: sitter.NewLanguage(groovy.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *GroovyParser) Extensions() []string {
	return []string{".groovy", ".gradle"}
}

// extractGroovyDeclInfo performs a pre-pass for metadata.
func extractGroovyDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
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
		case "class_definition":
			if nameNode := firstChildOfType(n, "identifier"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractDocMulti(lines, sl, "//"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
				// Walk all children with class context.
				for i := uint32(0); i < n.ChildCount(); i++ {
					walk(n.Child(i), name)
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

// Parse extracts code entities from a single Groovy file.
func (p *GroovyParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
	declInfo := extractGroovyDeclInfo(root, src)

	// --- package declaration ---
	var walkGroovyPkg func(n sitter.Node)
	walkGroovyPkg = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "groovy_package" {
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if child.IsNull() {
					continue
				}
				if child.Type() == "qualified_name" {
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
			walkGroovyPkg(n.Child(i))
		}
	}
	walkGroovyPkg(root)

	// --- import declarations ---
	// The Groovy tree-sitter grammar uses "juxt_expression" for imports, not
	// "import_statement". Try both to handle grammar variations gracefully.
	for _, importQueryStr := range []string{
		`(juxt_expression (identifier) @import_path)`,
		`(import_statement (identifier) @import_path)`,
	} {
		_ = runQuery(lang, root, src, importQueryStr, func(captures map[string]string, _ int) {
			importPath := captures["import_path"]
			if importPath == "" {
				return
			}
			importNodeID := g.MakeNodeID(importPath, importPath)
			g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: importPath, Package: importPath, File: filePath})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
		})
	}

	// --- All declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Gradle DSL patterns (tasks, plugins, dependencies) ---
	extractGradleDSL(g, root, src, filePath, fileNodeID)

	// --- Call sites ---
	collectGroovyCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks Groovy AST.
func (p *GroovyParser) extractAllDeclarations(
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

			// Check if it's an interface or trait by looking at keyword tokens.
			nodeType := graph.NodeStruct
			meta := buildLangMeta(declInfo[name])
			if hasChildToken(n, src, "interface") {
				nodeType = graph.NodeInterface
			} else if hasChildToken(n, src, "trait") {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = "trait"
			} else if hasChildToken(n, src, "enum") {
				if meta == nil {
					meta = make(map[string]string, 1)
				}
				meta["kind"] = "enum"
			}

			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: nodeType, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Walk all children with class context — Groovy grammar may not have
			// a named body node, so walk everything.
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), name)
			}
			return

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
			g.AddNode(&graph.Node{
				ID: nodeID, Type: nodeType, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isExported(name), Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// extractGradleDSL detects Gradle DSL patterns in .gradle files and emits
// graph nodes for tasks, plugin IDs, and dependency coordinates.
//
// Supported patterns (line-scan based, grammar-independent):
//
//	task myTask(type: Jar) { ... }        → NodeFunction kind="gradle_task"
//	task myTask { ... }                   → NodeFunction kind="gradle_task"
//	task("myTask") { ... }                → NodeFunction kind="gradle_task"
//	id 'com.foo.plugin'                   → NodePackage  kind="gradle_plugin"
//	implementation 'com.foo:bar:1.0'      → NodePackage  kind="gradle_dep"
//	api / compile / runtimeOnly / etc.
func extractGradleDSL(g *graph.Graph, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	lines := strings.Split(string(src), "\n")

	// Gradle dependency configuration keywords.
	depKeywords := map[string]bool{
		"implementation": true, "api": true, "compile": true,
		"runtimeOnly": true, "compileOnly": true, "testImplementation": true,
		"testCompile": true, "testRuntimeOnly": true, "annotationProcessor": true,
		"kapt": true, "classpath": true,
	}

	// Plugin DSL keyword.
	pluginKeywords := map[string]bool{"id": true, "alias": true}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}

		// --- task declarations ---
		// Forms: "task myTask", "task myTask(type: Jar)", "task('myTask')", `task("myTask")`
		if strings.HasPrefix(trimmed, "task ") || strings.HasPrefix(trimmed, "task(") {
			taskName := extractGradleTaskName(trimmed)
			if taskName != "" {
				nodeID := g.MakeNodeID(filePath, taskName)
				if g.GetNode(nodeID) == nil {
					meta := map[string]string{"kind": "gradle_task"}
					// Capture type if present: task myTask(type: Jar)
					if idx := strings.Index(trimmed, "type:"); idx != -1 {
						rest := strings.TrimSpace(trimmed[idx+5:])
						typeName := strings.FieldsFunc(rest, func(r rune) bool {
							return r == ')' || r == ',' || r == ' ' || r == '\t'
						})
						if len(typeName) > 0 {
							meta["task_type"] = typeName[0]
						}
					}
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeFunction, Name: taskName,
						File: filePath, Line: lineNum, Exported: true, Metadata: meta,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
			continue
		}

		// --- plugin id declarations (inside plugins { } block) ---
		// Forms: id 'com.foo.plugin'  or  id("com.foo.plugin")
		// Handle both `keyword 'arg'` and `keyword("arg")` call forms.
		rawFirst := strings.Fields(trimmed)[0]
		firstToken := rawFirst
		if idx := strings.IndexByte(rawFirst, '('); idx != -1 {
			firstToken = rawFirst[:idx]
		}
		if pluginKeywords[firstToken] {
			pluginID := extractGradleStringArg(trimmed)
			if pluginID != "" {
				nodeID := g.MakeNodeID(pluginID, pluginID)
				if g.GetNode(nodeID) == nil {
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodePackage, Name: pluginID,
						Package: pluginID, File: filePath, Line: lineNum,
						Metadata: map[string]string{"kind": "gradle_plugin"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeImports})
				}
			}
			continue
		}

		// --- dependency declarations ---
		// Forms: implementation 'com.foo:bar:1.0'  or  implementation("com.foo:bar:1.0")
		if depKeywords[firstToken] {
			dep := extractGradleStringArg(trimmed)
			if dep != "" && strings.ContainsRune(dep, ':') {
				// Strip version: com.foo:bar:1.0 → com.foo:bar
				parts := strings.SplitN(dep, ":", 3)
				depName := parts[0] + ":" + parts[1]
				nodeID := g.MakeNodeID(depName, depName)
				if g.GetNode(nodeID) == nil {
					meta := map[string]string{
						"kind":          "gradle_dep",
						"configuration": firstToken,
					}
					if len(parts) == 3 {
						meta["version"] = parts[2]
					}
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodePackage, Name: depName,
						Package: depName, File: filePath, Line: lineNum, Metadata: meta,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeImports})
				}
			}
		}
	}
	_ = root // pre-pass is line-based; root kept for future AST-based enhancements
}

// extractGradleTaskName parses the task name from a Gradle task declaration line.
// Handles: "task myTask", "task myTask(type: Jar)", "task('myTask')", `task("myTask")`.
func extractGradleTaskName(line string) string {
	// Strip "task" prefix.
	rest := strings.TrimSpace(strings.TrimPrefix(line, "task"))
	if rest == "" {
		return ""
	}
	// Method-call form: task('name') or task("name")
	if rest[0] == '(' {
		return strings.Trim(strings.FieldsFunc(rest, func(r rune) bool {
			return r == '(' || r == ')' || r == '\'' || r == '"' || r == ','
		})[0], " \t")
	}
	// Named form: "myTask" or "myTask(type: ...)"
	name := strings.FieldsFunc(rest, func(r rune) bool {
		return r == '(' || r == ' ' || r == '\t' || r == '{'
	})
	if len(name) > 0 {
		return name[0]
	}
	return ""
}

// extractGradleStringArg extracts a single string argument from a Gradle DSL
// call line. Handles both quoted ('foo') and method-call ("foo") forms.
func extractGradleStringArg(line string) string {
	// Find first quote character.
	for _, q := range []byte{'"', '\''} {
		start := strings.IndexByte(line, q)
		if start == -1 {
			continue
		}
		rest := line[start+1:]
		end := strings.IndexByte(rest, q)
		if end == -1 {
			continue
		}
		return rest[:end]
	}
	return ""
}

// collectGroovyCallSites collects call sites. The Groovy grammar may not have
// standard call_expression nodes, so we try multiple query patterns.
func collectGroovyCallSites(g *graph.Graph, lang *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	for _, queryStr := range []string{
		`(call_expression (identifier) @callee)`,
		`(juxt_expression (identifier) @callee)`,
	} {
		_ = runQuery(lang, root, src, queryStr, func(captures map[string]string, _ int) {
			callee := captures["callee"]
			if callee == "" || callee == "println" || callee == "print" ||
				callee == "class" || callee == "import" || callee == "package" {
				return
			}
			g.AddCallSite(graph.CallSite{CallerID: fileNodeID, CallerFile: filePath, FuncName: callee})
		})
	}
}
