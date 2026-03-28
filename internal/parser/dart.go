package parser

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// DartParser parses Dart (.dart) source files.
// Used in Flutter mobile apps, server-side Dart, and CLI tools.
// Uses regex-based extraction since Dart is not in go-tree-sitter.
type DartParser struct{}

// NewDartParser creates a ready-to-use DartParser.
func NewDartParser() *DartParser { return &DartParser{} }

// Extensions returns the file extensions handled by this parser.
func (p *DartParser) Extensions() []string {
	return []string{".dart"}
}

// Dart-specific regex patterns.
var (
	// import 'package:flutter/material.dart' or import 'dart:async' or import 'relative/path.dart'
	// Optionally followed by: as alias, show X, hide X
	reDartImport = regexp.MustCompile(`(?m)^[ \t]*import\s+['"]([^'"]+)['"]`)

	// export 'src/widget.dart' or export 'src/widget.dart' show X
	reDartExport = regexp.MustCompile(`(?m)^[ \t]*export\s+['"]([^'"]+)['"]`)

	// class Foo { or abstract class Bar { or class Foo extends Bar implements Baz, Qux {
	// Captures: (abstract|sealed|base|final|interface|mixin)? class Name
	reDartClass = regexp.MustCompile(`(?m)^[ \t]*((?:abstract|sealed|base|final|interface)\s+)?class\s+(\w+)`)

	// mixin Logging on Widget { or mixin Logging {
	reDartMixin = regexp.MustCompile(`(?m)^[ \t]*mixin\s+(\w+)`)

	// extension StringExt on String { or extension on String {
	// Named extension: captures the name; anonymous extension: name is ""
	reDartExtension = regexp.MustCompile(`(?m)^[ \t]*extension\s+(?:(\w+)\s+)?on\s+`)

	// enum Color { red, green, blue }
	reDartEnum = regexp.MustCompile(`(?m)^[ \t]*enum\s+(\w+)`)

	// Top-level function: optional return type, name, params
	// Matches: void main() {, Future<void> fetchData() async {, String greet(String name) {
	// We detect functions NOT inside a class body (top-level).
	// Pattern: optional modifiers + optional return-type + name + (
	reDartTopLevelFunc = regexp.MustCompile(`(?m)^(?:[ \t]*)(?:(?:async\s+)?(?:static\s+)?)?(?:[\w<>\[\]?,\s]+\s+)?(\w+)\s*\(`)

	// Method inside a class body — same structure but indented (at least 2 spaces or 1 tab)
	reDartMethod = regexp.MustCompile(`(?m)^[ \t]{2,}(?:(?:static|async|external|factory|get|set|operator)\s+)*(?:[\w<>\[\]?,\s]*\s+)?(\w+)\s*\(`)

	// Doc comment: /// lines
	reDartTripleSlash = regexp.MustCompile(`(?m)^[ \t]*///[ \t]?(.*)$`)

	// Block doc comment: /** ... */
	reDartBlockDocStart = regexp.MustCompile(`(?m)^[ \t]*/\*\*`)

	// New-style typedef: typedef CallbackType = void Function(int x)
	// Group 1: the typedef name
	// Using [^;{]* for the generic constraint to handle nested generics like Map<String, List<T>>.
	reDartTypedefNew = regexp.MustCompile(`(?m)^[ \t]*typedef\s+(\w+)\s*(?:<[^;{]*>)?\s*=`)

	// Old-style typedef: typedef void Callback(int x)
	// Pattern: typedef + return-type + name + (
	// Group 1: the typedef name (the last \w+ before the opening paren)
	reDartTypedefOld = regexp.MustCompile(`(?m)^[ \t]*typedef\s+(?:[\w<>\[\]?,\s]+?\s+)(\w+)\s*\(`)

	// part of 'filename.dart' (quoted path, group 1) or
	// part of library_name (unquoted identifier, group 2).
	reDartPartOf = regexp.MustCompile(`(?m)^part\s+of\s+['"]([^'"]+)['"]|^part\s+of\s+([\w.]+)\s*;`)
)

// Parse extracts code entities from a single Dart file and merges them into the graph.
func (p *DartParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	if len(src) == 0 {
		return nil
	}

	content := string(src)
	lines := strings.Split(content, "\n")

	// --- Imports ---
	for _, m := range reDartImport.FindAllStringSubmatchIndex(content, -1) {
		importPath := content[m[2]:m[3]]
		if importPath == "" {
			continue
		}
		importLine := countNewlines(content[:m[0]]) + 1
		importNodeID := g.MakeNodeID(importPath, importPath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    importPath,
			Package: importPath,
			File:    filePath,
			Line:    importLine,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}

	// --- Exports ---
	for _, m := range reDartExport.FindAllStringSubmatchIndex(content, -1) {
		exportPath := content[m[2]:m[3]]
		if exportPath == "" {
			continue
		}
		exportLine := countNewlines(content[:m[0]]) + 1
		exportNodeID := g.MakeNodeID(exportPath, exportPath)
		g.AddNode(&graph.Node{
			ID:      exportNodeID,
			Type:    graph.NodePackage,
			Name:    exportPath,
			Package: exportPath,
			File:    filePath,
			Line:    exportLine,
			Metadata: map[string]string{"kind": "export"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: exportNodeID, Type: graph.EdgeImports})
	}

	// --- part of declarations (this file is a part of a library) ---
	for _, m := range reDartPartOf.FindAllStringSubmatchIndex(content, -1) {
		// Group 1 (m[2]:m[3]) = quoted path; Group 2 (m[4]:m[5]) = unquoted library name.
		ref := ""
		if m[2] >= 0 {
			ref = content[m[2]:m[3]]
		} else if m[4] >= 0 {
			ref = content[m[4]:m[5]]
		}
		if ref == "" {
			continue
		}
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		refNodeID := g.MakeNodeID(ref, ref)
		if g.GetNode(refNodeID) == nil {
			g.AddNode(&graph.Node{
				ID:   refNodeID,
				Type: graph.NodeFile,
				Name: ref,
				File: ref,
				Line: line,
			})
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: refNodeID, Type: graph.EdgeImports})
	}

	// --- Classes (including abstract, sealed, etc.) ---
	// We track class ranges so we can attribute methods to them.
	type classRange struct {
		name  string
		start int // byte offset where the class keyword begins
		end   int // byte offset of matching closing brace (approximate)
	}
	var classRanges []classRange

	for _, m := range reDartClass.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[4]:m[5]]
		byteOffset := m[0]
		line := countNewlines(content[:byteOffset]) + 1

		// Determine if abstract
		isAbstract := false
		if m[2] != -1 {
			mod := strings.TrimSpace(content[m[2]:m[3]])
			isAbstract = strings.Contains(mod, "abstract")
		}

		doc := extractDocMulti(lines, line, "///")

		meta := map[string]string{"kind": "class"}
		if isAbstract {
			meta["abstract"] = "true"
		}
		if doc != "" {
			meta["doc"] = doc
		}

		exported := !strings.HasPrefix(name, "_")
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		// Record a rough class range: from this position to end of file
		// (we refine this later via brace counting).
		classRanges = append(classRanges, classRange{name: name, start: byteOffset})
	}

	// Compute approximate end for each class via brace counting.
	for i := range classRanges {
		start := classRanges[i].start
		// Find the opening brace.
		braceStart := strings.Index(content[start:], "{")
		if braceStart < 0 {
			classRanges[i].end = len(content)
			continue
		}
		absStart := start + braceStart
		depth := 0
		end := absStart
		for j := absStart; j < len(content); j++ {
			switch content[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = j
					goto foundEnd
				}
			}
		}
	foundEnd:
		classRanges[i].end = end
	}

	// --- Mixins ---
	for _, m := range reDartMixin.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		byteOffset := m[0]
		line := countNewlines(content[:byteOffset]) + 1
		doc := extractDocMulti(lines, line, "///")
		meta := map[string]string{"kind": "mixin"}
		if doc != "" {
			meta["doc"] = doc
		}
		exported := !strings.HasPrefix(name, "_")
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Extensions ---
	for _, m := range reDartExtension.FindAllStringSubmatchIndex(content, -1) {
		byteOffset := m[0]
		line := countNewlines(content[:byteOffset]) + 1

		// Name may be empty (anonymous extension).
		name := ""
		if m[2] != -1 {
			name = content[m[2]:m[3]]
		}
		if name == "" {
			// Use a synthetic name based on line number for anonymous extensions.
			name = "extension@" + strings.TrimSpace(strings.Split(content[byteOffset:], "\n")[0])
			// Trim to something safe.
			if idx := strings.Index(name, "{"); idx >= 0 {
				name = strings.TrimSpace(name[:idx])
			}
		}

		doc := extractDocMulti(lines, line, "///")
		meta := map[string]string{"kind": "extension"}
		if doc != "" {
			meta["doc"] = doc
		}
		exported := !strings.HasPrefix(name, "_")
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Enums ---
	for _, m := range reDartEnum.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		byteOffset := m[0]
		line := countNewlines(content[:byteOffset]) + 1
		doc := extractDocMulti(lines, line, "///")
		meta := map[string]string{"kind": "enum"}
		if doc != "" {
			meta["doc"] = doc
		}
		exported := !strings.HasPrefix(name, "_")
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Top-level functions and methods ---
	// Strategy: walk line by line. Detect which lines are inside a class body
	// vs at the top level. For class bodies, extract methods. For top-level,
	// extract function declarations.
	//
	// We use a line-based approach with brace-depth tracking to determine context.
	// Top-level functions: depth == 0 when the func keyword / signature appears.
	// Methods: depth == 1 (inside one pair of braces — the class body).

	// Build a sorted list of class bodies by line range.
	type lineRange struct {
		name      string
		startLine int
		endLine   int
	}
	var classBodies []lineRange
	for _, cr := range classRanges {
		if cr.end == 0 {
			continue
		}
		sl := countNewlines(content[:cr.start]) + 1
		el := countNewlines(content[:cr.end]) + 1
		classBodies = append(classBodies, lineRange{cr.name, sl, el})
	}

	// owningClass returns the class name for a given 1-based line number, or "".
	owningClass := func(lineNum int) string {
		for _, lr := range classBodies {
			if lineNum > lr.startLine && lineNum <= lr.endLine {
				return lr.name
			}
		}
		return ""
	}

	// knownClassNames contains all class/mixin/extension names in this file.
	// Used to filter out constructor calls inside class bodies (e.g. const Foo())
	// that would otherwise be misidentified as method declarations.
	knownClassNames := make(map[string]bool)
	for _, cr := range classRanges {
		knownClassNames[cr.name] = true
	}
	for _, m := range reDartMixin.FindAllStringSubmatchIndex(content, -1) {
		if m[2] >= 0 {
			knownClassNames[content[m[2]:m[3]]] = true
		}
	}
	for _, m := range reDartEnum.FindAllStringSubmatchIndex(content, -1) {
		if m[2] >= 0 {
			knownClassNames[content[m[2]:m[3]]] = true
		}
	}

	// Dart function/method declaration pattern applied line-by-line.
	// We look for lines that look like function/method signatures.
	// Return type: any identifier (including user-defined types like Node, Iterable<Node>?, etc.)
	// followed by optional generics, optional nullable ?, optional [].
	// We accept any \w+ as the return type to avoid false negatives from
	// a hardcoded whitelist of known types.
	reFuncLine := regexp.MustCompile(
		`^[ \t]*(?:(?:static|async|external|factory|override|abstract|get|set|operator)\s+)*` +
			`(?:\w[\w<>\[\]?,\s]*\s+)?` +
			`([a-zA-Z_]\w*)\s*\(`,
	)

	// Keywords that are not function names.
	dartKeywords := map[string]bool{
		"if": true, "else": true, "while": true, "for": true, "switch": true,
		"catch": true, "finally": true, "return": true, "throw": true, "assert": true,
		"print": true, "super": true, "this": true, "new": true, "await": true,
		"yield": true, "class": true, "extends": true, "implements": true, "with": true,
		"mixin": true, "enum": true, "import": true, "export": true, "library": true,
		"part": true, "show": true, "hide": true, "as": true, "is": true, "in": true,
		"abstract": true, "static": true, "final": true, "const": true, "var": true,
		"late": true, "required": true, "external": true, "factory": true, "typedef": true,
		"extension": true, "on": true, "operator": true, "get": true, "set": true,
		"async": true, "sync": true, "covariant": true, "sealed": true, "base": true,
		"interface": true, "when": true, "case": true,
		// Common Dart SDK class names that appear frequently as constructor calls
		// inside method bodies and should not be mistaken for method declarations.
		"Function": true, "RegExp": true, "File": true,
	}

	// --- Typedefs ---
	// Dart supports two typedef forms:
	//   New-style: typedef Callback = void Function(int)  (Dart 2.0+)
	//   Old-style: typedef void Callback(int)  (legacy)
	typedefNames := make(map[string]bool) // track to prevent false-positives in func extraction
	{
		seen := make(map[string]bool)
		// New-style typedefs.
		for _, m := range reDartTypedefNew.FindAllStringSubmatchIndex(content, -1) {
			name := content[m[2]:m[3]]
			if name == "" || dartKeywords[name] || seen[name] {
				continue
			}
			seen[name] = true
			typedefNames[name] = true
			byteOffset := m[0]
			line := countNewlines(content[:byteOffset]) + 1
			doc := extractDocMulti(lines, line, "///")
			meta := map[string]string{"kind": "typedef"}
			if doc != "" {
				meta["doc"] = doc
			}
			exported := !strings.HasPrefix(name, "_")
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     line,
				Exported: exported,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
		// Old-style typedefs (legacy: typedef RetType Name(Params)).
		for _, m := range reDartTypedefOld.FindAllStringSubmatchIndex(content, -1) {
			name := content[m[2]:m[3]]
			if name == "" || dartKeywords[name] || seen[name] {
				continue
			}
			seen[name] = true
			typedefNames[name] = true
			byteOffset := m[0]
			line := countNewlines(content[:byteOffset]) + 1
			doc := extractDocMulti(lines, line, "///")
			meta := map[string]string{"kind": "typedef"}
			if doc != "" {
				meta["doc"] = doc
			}
			exported := !strings.HasPrefix(name, "_")
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     line,
				Exported: exported,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	emitted := make(map[string]bool)

	for i, line := range lines {
		lineNum := i + 1 // 1-based
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		// Skip import/export/class/mixin/extension/enum lines (already handled).
		if strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "export ") {
			continue
		}
		if strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(trimmed, "abstract class ") ||
			strings.HasPrefix(trimmed, "mixin ") || strings.HasPrefix(trimmed, "extension ") ||
			strings.HasPrefix(trimmed, "enum ") {
			continue
		}
		// Also skip lines with sealed/base/final/interface class
		if regexp.MustCompile(`^(?:sealed|base|final|interface)\s+class\s`).MatchString(trimmed) {
			continue
		}
		// Skip typedef lines — already handled above, and they can generate
		// false-positive function nodes (e.g. "Function" from typedef Callback = void Function(int)).
		if strings.HasPrefix(trimmed, "typedef ") {
			continue
		}
		// Skip `part` directives (part 'file.dart', part of 'library.dart').
		if strings.HasPrefix(trimmed, "part ") {
			continue
		}

		m := reFuncLine.FindStringSubmatchIndex(line)
		if m == nil {
			continue
		}
		funcName := line[m[2]:m[3]]
		if dartKeywords[funcName] {
			continue
		}
		// Skip names that are actually typedefs — already emitted as NodeStruct.
		if typedefNames[funcName] {
			continue
		}
		// Skip constructor-like patterns: ClassName( where ClassName matches a known class.
		// Those are already captured as class nodes.
		if emitted[funcName] {
			continue
		}

		ownerClass := owningClass(lineNum)
		doc := extractDocMulti(lines, lineNum, "///")

		if ownerClass != "" {
			// Skip constructors: Dart constructors share the class name (e.g. CounterWidget(...)).
			// They are already represented by the class node itself.
			if funcName == ownerClass {
				continue
			}
			// Skip constructor calls of OTHER known classes used as field initializers
			// or list literals (e.g. const OrderedListSyntax() inside another class body).
			// These are call expressions, not method declarations.
			if knownClassNames[funcName] {
				continue
			}
			// Method inside a class.
			qualName := ownerClass + "." + funcName
			if emitted[qualName] {
				continue
			}
			emitted[qualName] = true

			exported := !strings.HasPrefix(funcName, "_")
			meta := map[string]string{"kind": "method"}
			if doc != "" {
				meta["doc"] = doc
			}
			if strings.Contains(line, " async ") || strings.HasSuffix(strings.TrimRight(line, " {"), " async") {
				meta["async"] = "true"
			}
			classNodeID := g.MakeNodeID(filePath, ownerClass)
			methodNodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       methodNodeID,
				Type:     graph.NodeFunction,
				Name:     qualName,
				File:     filePath,
				Line:     lineNum,
				Exported: exported,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: classNodeID, To: methodNodeID, Type: graph.EdgeDefines})
		} else {
			// Top-level function.
			emitted[funcName] = true

			exported := !strings.HasPrefix(funcName, "_")
			meta := map[string]string{"kind": "function"}
			if doc != "" {
				meta["doc"] = doc
			}
			if strings.Contains(line, " async") {
				meta["async"] = "true"
			}
			nodeID := g.MakeNodeID(filePath, funcName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     funcName,
				File:     filePath,
				Line:     lineNum,
				Exported: exported,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	return nil
}
