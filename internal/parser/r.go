package parser

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// RParser parses R (.r, .R) source files.
// Used extensively in data science, statistics, bioinformatics, finance, and pharma.
// Uses regex-based extraction since R is not in smacker/go-tree-sitter.
//
// Extracts:
//   - Function assignments (<-, =, <<-) → NodeFunction
//   - S3 generic functions (summary.myClass) → NodeFunction with kind=s3generic
//   - S4 setClass() → NodeStruct with kind=s4class
//   - S4 setGeneric() → NodeFunction with kind=s4generic
//   - S4 setMethod() → NodeFunction with kind=s4method
//   - R5/Reference classes (setRefClass) → NodeStruct with kind=r5class
//   - library()/require() imports → NodePackage + EdgeImports
//   - Namespace pkg::func references → EdgeImports
type RParser struct{}

// NewRParser creates a ready-to-use RParser.
func NewRParser() *RParser { return &RParser{} }

// Extensions returns the file extensions handled by this parser.
func (p *RParser) Extensions() []string {
	return []string{".r", ".R"}
}

// Regex patterns for R source code.
var (
	// Standard function assignment: name <- function(...) or name <<- function(...)
	// Also matches name = function(...)
	// Group 1: function name
	reRFuncAssign = regexp.MustCompile(`(?m)^(\.[A-Za-z][A-Za-z0-9._]*|[A-Za-z][A-Za-z0-9._]*)\s*(?:<<-|<-|=)\s*function\s*\(`)

	// S4 setClass("ClassName", ...)
	// Group 1: class name (quoted or unquoted)
	reRSetClass = regexp.MustCompile(`(?m)^setClass\s*\(\s*["']([A-Za-z][A-Za-z0-9._]*)["']`)

	// S4 setGeneric("name", function(...) { ... })
	// Group 1: generic name
	reRSetGeneric = regexp.MustCompile(`(?m)^setGeneric\s*\(\s*["']([A-Za-z][A-Za-z0-9._]*)["']`)

	// S4 setMethod("name", "ClassName", function(...) { ... })
	// Group 1: generic name, Group 2: signature class
	reRSetMethod = regexp.MustCompile(`(?m)^setMethod\s*\(\s*["']([A-Za-z][A-Za-z0-9._]*)["']\s*,\s*["']([A-Za-z][A-Za-z0-9._]*)["']`)

	// R5/Reference class: MyClass <- setRefClass("MyClass", ...)
	// Group 1: variable name (binding), Group 2: class name in setRefClass
	reRSetRefClass = regexp.MustCompile(`(?m)^([A-Za-z][A-Za-z0-9._]*)\s*(?:<-|=)\s*setRefClass\s*\(\s*["']([A-Za-z][A-Za-z0-9._]*)["']`)

	// R6 class (modern OOP): MyClass <- R6::R6Class("MyClass", public=list(...))
	// Also handles: MyClass <- R6Class("ClassName", ...) when R6 is loaded
	// Group 1: variable name (binding), Group 2: class name inside R6Class (optional)
	reRR6Class = regexp.MustCompile(`(?m)^([A-Za-z][A-Za-z0-9._]*)\s*(?:<-|=)\s*(?:R6::)?R6Class\s*\(\s*["']([A-Za-z][A-Za-z0-9._]*)["']`)

	// R 4.1+ lambda shorthand: name <- \(args) expr  or  name = \(args) expr
	// Also handles <<- assignment operator.
	// Group 1: function name
	reRLambdaAssign = regexp.MustCompile(`(?m)^(\.[A-Za-z][A-Za-z0-9._]*|[A-Za-z][A-Za-z0-9._]*)\s*(?:<<-|<-|=)\s*\\\(`)

	// library(pkg) or library("pkg") or library('pkg')
	// Group 1: package name (possibly quoted)
	reRLibrary = regexp.MustCompile(`(?m)(?:^|\s)(?:library|require)\s*\(\s*["']?([A-Za-z][A-Za-z0-9._]*)["']?`)

	// pkg::func namespace references
	// Group 1: package name
	reRNamespace = regexp.MustCompile(`(?m)\b([A-Za-z][A-Za-z0-9._]*):::?[A-Za-z]`)

	// Roxygen2 doc comment: #' text
	reRRoxygen = regexp.MustCompile(`^#'(.*)$`)

	// Plain # comment line
	reRComment = regexp.MustCompile(`^#(.*)$`)
)

// Parse extracts code entities from a single R source file and merges them into g.
func (p *RParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	content := string(src)
	lines := strings.Split(content, "\n")

	// Track emitted names to avoid duplicates.
	emitted := make(map[string]bool)

	// Track imports already emitted to avoid duplicate EdgeImports.
	importedPkgs := make(map[string]bool)

	// Helper: emit a package import node + EdgeImports.
	emitImport := func(pkgName string) {
		if pkgName == "" || importedPkgs[pkgName] {
			return
		}
		importedPkgs[pkgName] = true
		pkgNodeID := g.MakeNodeID(pkgName, pkgName)
		g.AddNode(&graph.Node{
			ID:      pkgNodeID,
			Type:    graph.NodePackage,
			Name:    pkgName,
			Package: pkgName,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: pkgNodeID, Type: graph.EdgeImports})
	}

	// Helper: extract roxygen doc comment above a given 0-based line index.
	// Scans backwards collecting #' lines; also accepts plain # as fallback.
	extractRDoc := func(lineIdx int) string {
		if lineIdx == 0 {
			return ""
		}
		var parts []string
		// First try roxygen (#') lines.
		for i := lineIdx - 1; i >= 0; i-- {
			trimmed := strings.TrimSpace(lines[i])
			if m := reRRoxygen.FindStringSubmatch(trimmed); m != nil {
				text := strings.TrimSpace(m[1])
				parts = append([]string{text}, parts...)
			} else if trimmed == "" {
				// Allow one blank line between doc comment and function.
				continue
			} else {
				break
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
		// Fall back to plain # comments.
		for i := lineIdx - 1; i >= 0; i-- {
			trimmed := strings.TrimSpace(lines[i])
			if m := reRComment.FindStringSubmatch(trimmed); m != nil {
				text := strings.TrimSpace(m[1])
				parts = append([]string{text}, parts...)
			} else if trimmed == "" {
				continue
			} else {
				break
			}
		}
		return strings.Join(parts, " ")
	}

	// -------------------------------------------------------------------------
	// 1a. R6 classes — the dominant modern R OOP pattern (R6 package).
	//     Must be matched before plain function assignments for the same reason
	//     as R5: `MyClass <- R6::R6Class(...)` would otherwise match the generic
	//     function assignment regex.
	// -------------------------------------------------------------------------
	for _, m := range reRR6Class.FindAllStringSubmatchIndex(content, -1) {
		bindingName := content[m[2]:m[3]] // variable the class is bound to
		className := content[m[4]:m[5]]   // name inside R6Class("...")
		nodeName := bindingName
		if emitted[nodeName] {
			continue
		}
		emitted[nodeName] = true
		lineIdx := strings.Count(content[:m[0]], "\n") // 0-based
		line := lineIdx + 1
		nodeID := g.MakeNodeID(filePath, nodeName)
		meta := map[string]string{"kind": "r6class"}
		if className != bindingName {
			meta["class_name"] = className
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     nodeName,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// -------------------------------------------------------------------------
	// 1b. R5/Reference classes — must be matched before plain function assignments
	//     because the pattern `MyClass <- setRefClass(...)` would also match
	//     the generic function assignment regex.
	// -------------------------------------------------------------------------
	for _, m := range reRSetRefClass.FindAllStringSubmatchIndex(content, -1) {
		bindingName := content[m[2]:m[3]] // variable the class is bound to
		className := content[m[4]:m[5]]   // name inside setRefClass("...")
		// Use the binding name as the node name (what user refers to).
		nodeName := bindingName
		if emitted[nodeName] {
			continue
		}
		emitted[nodeName] = true
		lineIdx := strings.Count(content[:m[0]], "\n") // 0-based
		line := lineIdx + 1
		nodeID := g.MakeNodeID(filePath, nodeName)
		meta := map[string]string{"kind": "r5class"}
		if className != bindingName {
			meta["class_name"] = className
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     nodeName,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// -------------------------------------------------------------------------
	// 2. S4 setClass()
	// -------------------------------------------------------------------------
	for _, m := range reRSetClass.FindAllStringSubmatchIndex(content, -1) {
		className := content[m[2]:m[3]]
		if emitted[className] {
			continue
		}
		emitted[className] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		nodeID := g.MakeNodeID(filePath, className)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     className,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "s4class"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// -------------------------------------------------------------------------
	// 3. S4 setGeneric()
	// -------------------------------------------------------------------------
	for _, m := range reRSetGeneric.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		doc := extractRDoc(lineIdx)
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: buildLangMeta(declMeta{Doc: doc, Signature: name + " (S4 generic)"}),
		})
		if md := g.GetNode(nodeID); md != nil && md.Metadata == nil {
			md.Metadata = map[string]string{}
		}
		if node := g.GetNode(nodeID); node != nil {
			if node.Metadata == nil {
				node.Metadata = map[string]string{}
			}
			node.Metadata["kind"] = "s4generic"
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// -------------------------------------------------------------------------
	// 4. S4 setMethod()
	// -------------------------------------------------------------------------
	for _, m := range reRSetMethod.FindAllStringSubmatchIndex(content, -1) {
		genericName := content[m[2]:m[3]]
		sigClass := content[m[4]:m[5]]
		// Node name: "genericName.sigClass" to make it unique.
		nodeName := genericName + "." + sigClass
		if emitted[nodeName] {
			continue
		}
		emitted[nodeName] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		doc := extractRDoc(lineIdx)
		nodeID := g.MakeNodeID(filePath, nodeName)
		meta := buildLangMeta(declMeta{Doc: doc})
		if meta == nil {
			meta = map[string]string{}
		}
		meta["kind"] = "s4method"
		meta["generic"] = genericName
		meta["signature_class"] = sigClass
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     nodeName,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// -------------------------------------------------------------------------
	// 5. Regular function assignments (includes S3 generics like summary.myClass)
	// -------------------------------------------------------------------------
	for _, m := range reRFuncAssign.FindAllStringSubmatchIndex(content, -1) {
		funcName := content[m[2]:m[3]]
		if emitted[funcName] {
			continue
		}
		emitted[funcName] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		doc := extractRDoc(lineIdx)

		// Determine if this is an S3 method: name contains a dot but not a
		// leading dot (which indicates private). Pattern: generic.ClassName.
		// We detect S3 by checking if name contains "." and the part before
		// the first dot is a known or reasonable generic name.
		exported := !strings.HasPrefix(funcName, ".")
		var kind string
		if exported && strings.Contains(funcName, ".") {
			kind = "s3generic"
		}

		meta := buildLangMeta(declMeta{Doc: doc})
		if kind != "" {
			if meta == nil {
				meta = map[string]string{}
			}
			meta["kind"] = kind
		}

		nodeID := g.MakeNodeID(filePath, funcName)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     funcName,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// -------------------------------------------------------------------------
	// 5b. R 4.1+ lambda shorthand: name <- \(args) expr
	//     The backslash-paren \( syntax was introduced in R 4.1.0 (2021) as a
	//     shorthand for function(). Must be matched after reRFuncAssign to allow
	//     the emitted map to prevent duplicates.
	// -------------------------------------------------------------------------
	for _, m := range reRLambdaAssign.FindAllStringSubmatchIndex(content, -1) {
		funcName := content[m[2]:m[3]]
		if emitted[funcName] {
			continue
		}
		emitted[funcName] = true
		lineIdx := strings.Count(content[:m[0]], "\n") // 0-based
		line := lineIdx + 1
		doc := extractRDoc(lineIdx)
		exported := !strings.HasPrefix(funcName, ".")

		meta := buildLangMeta(declMeta{Doc: doc})
		if meta == nil {
			meta = map[string]string{}
		}
		meta["kind"] = "lambda"

		nodeID := g.MakeNodeID(filePath, funcName)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     funcName,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// -------------------------------------------------------------------------
	// 6. library() / require() imports
	// -------------------------------------------------------------------------
	for _, m := range reRLibrary.FindAllStringSubmatchIndex(content, -1) {
		pkgName := content[m[2]:m[3]]
		emitImport(pkgName)
	}

	// -------------------------------------------------------------------------
	// 7. Namespace pkg::func references in top-level code
	//    These are weaker import signals — emit only packages not already imported.
	// -------------------------------------------------------------------------
	for _, m := range reRNamespace.FindAllStringSubmatchIndex(content, -1) {
		pkgName := content[m[2]:m[3]]
		emitImport(pkgName)
	}

	return nil
}
