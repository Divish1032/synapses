package parser

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// PowerShellParser parses PowerShell (.ps1, .psm1, .psd1) source files.
// Uses regex-based extraction since PowerShell is not in go-tree-sitter.
type PowerShellParser struct{}

// NewPowerShellParser creates a ready-to-use PowerShellParser.
func NewPowerShellParser() *PowerShellParser { return &PowerShellParser{} }

// Extensions returns the file extensions handled by this parser.
func (p *PowerShellParser) Extensions() []string {
	return []string{".ps1", ".psm1", ".psd1"}
}

// PowerShell regex patterns.
var (
	// function Verb-Noun { ... } and function Verb-Noun([params]) { ... }
	// Handles optional [CmdletBinding()] attribute on the preceding line — checked separately.
	rePSFunction = regexp.MustCompile(`(?im)^[ \t]*function\s+([\w-]+)\s*(?:\([^)]*\))?\s*\{`)

	// class Foo { ... } and class Foo : BaseClass { ... }
	rePSClass = regexp.MustCompile(`(?im)^[ \t]*class\s+(\w+)(?:\s*:\s*\w+)?\s*\{`)

	// enum Color { ... }
	rePSEnum = regexp.MustCompile(`(?im)^[ \t]*enum\s+(\w+)\s*\{`)

	// [CmdletBinding()] attribute immediately before a function keyword.
	// We scan lines above each function to detect this.
	rePSCmdletBinding = regexp.MustCompile(`(?i)\[CmdletBinding\(`)

	// Method inside a class: [returntype] MethodName([params]) { ... }
	// or just MethodName([params]) { ... }
	rePSMethod = regexp.MustCompile(`(?im)^[ \t]*(?:\[[^\]]+\]\s+)?(\w+)\s*\([^)]*\)\s*\{`)

	// #Requires -Modules ModuleName (also handles -Modules ModA,ModB)
	rePSRequiresModules = regexp.MustCompile(`(?im)^[ \t]*#Requires\s+-Modules?\s+(.+)$`)

	// Import-Module ModuleName or Import-Module "path/to/module"
	rePSImportModule = regexp.MustCompile(`(?im)^[ \t]*Import-Module\s+([^\s#;\r]+)`)

	// using module ./path or using module ModuleName
	rePSUsingModule = regexp.MustCompile(`(?im)^[ \t]*using\s+module\s+([^\s#;\r]+)`)

	// DSC configuration block: configuration MyDSCConfig { ... }
	rePSConfiguration = regexp.MustCompile(`(?im)^[ \t]*configuration\s+(\w+)\s*\{`)

	// DSC workflow: workflow WorkflowName { ... }
	// Workflows are PowerShell Workflows (WMF 5, used in Azure DevOps / Azure Automation).
	// Uses [\w-]+ to capture Verb-Noun names (hyphens allowed in PS workflow names).
	// Group 1: workflow name
	rePSWorkflow = regexp.MustCompile(`(?im)^[ \t]*workflow\s+([\w-]+)\s*(?:\{|$)`)
)

// Parse extracts code entities from a single PowerShell file and merges them into the graph.
func (p *PowerShellParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// Normalize Windows CRLF to LF so that regex patterns matching end-of-line
	// (e.g. `\s*\{` at line end) work correctly on files from Windows machines.
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	// Also strip lone \r in case of classic Mac CR-only line endings.
	src = bytes.ReplaceAll(src, []byte("\r"), []byte("\n"))

	content := string(src)
	lines := strings.Split(content, "\n")
	isModule := strings.EqualFold(filepath.Ext(filePath), ".psm1")

	// --- Module imports ---
	p.extractRequiresModules(g, lines, filePath, fileNodeID)
	p.extractImportModules(g, lines, filePath, fileNodeID)
	p.extractUsingModules(g, lines, filePath, fileNodeID)

	// --- DSC Configurations ---
	p.extractConfigurations(g, lines, filePath, fileNodeID)

	// --- PowerShell Workflows ---
	emitted := make(map[string]bool)
	p.extractWorkflows(g, filePath, fileNodeID, content, emitted)

	// --- Classes and enums ---
	// We track class line ranges so we can detect methods inside them.
	classRanges := p.extractClasses(g, lines, filePath, fileNodeID)
	p.extractEnums(g, lines, filePath, fileNodeID)

	// --- Functions (top-level) ---
	p.extractFunctions(g, lines, filePath, fileNodeID, isModule, classRanges)

	// --- Methods inside classes ---
	p.extractMethods(g, lines, filePath, fileNodeID, classRanges)

	return nil
}

// classRange records the line range of a class body so method extraction can
// distinguish class methods from top-level functions.
type classRange struct {
	name      string
	startLine int // 1-based line of the class declaration
	endLine   int // 1-based last line of the class body (estimated by brace counting)
	nodeID    graph.NodeID
}

// extractClasses finds `class Foo { ... }` and `class Foo : Base { ... }` declarations.
func (p *PowerShellParser) extractClasses(
	g *graph.Graph,
	lines []string,
	filePath string,
	fileNodeID graph.NodeID,
) []classRange {
	content := strings.Join(lines, "\n")
	var ranges []classRange

	for _, m := range rePSClass.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		lineNum := strings.Count(content[:m[0]], "\n") + 1

		doc := extractPSDoc(lines, lineNum)
		meta := map[string]string{}
		if doc != "" {
			meta["doc"] = doc
		}
		meta["kind"] = "class"

		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true, // PS classes are always public
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		// Estimate end line by counting braces from the opening brace.
		endLine := estimatePSBlockEnd(lines, lineNum-1)
		ranges = append(ranges, classRange{
			name:      name,
			startLine: lineNum,
			endLine:   endLine,
			nodeID:    nodeID,
		})
	}
	return ranges
}

// extractEnums finds `enum Color { Red; Green; Blue }` declarations.
func (p *PowerShellParser) extractEnums(
	g *graph.Graph,
	lines []string,
	filePath string,
	fileNodeID graph.NodeID,
) {
	content := strings.Join(lines, "\n")
	for _, m := range rePSEnum.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		lineNum := strings.Count(content[:m[0]], "\n") + 1

		doc := extractPSDoc(lines, lineNum)
		meta := map[string]string{"kind": "enum"}
		if doc != "" {
			meta["doc"] = doc
		}

		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// extractFunctions finds all top-level function declarations.
// It skips functions that live inside a class body (those are handled by extractMethods).
func (p *PowerShellParser) extractFunctions(
	g *graph.Graph,
	lines []string,
	filePath string,
	fileNodeID graph.NodeID,
	isModule bool,
	classRanges []classRange,
) {
	content := strings.Join(lines, "\n")
	emitted := make(map[string]bool)

	for _, m := range rePSFunction.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		lineNum := strings.Count(content[:m[0]], "\n") + 1

		// Skip if this function is inside a class body (it's a method).
		if insideAnyClass(lineNum, classRanges) {
			continue
		}
		if emitted[name] {
			continue
		}
		emitted[name] = true

		doc := extractPSDoc(lines, lineNum)
		exported := isPSFunctionExported(name, isModule)

		// Detect cmdlet binding: scan up to 3 lines above for [CmdletBinding(].
		kind := ""
		for i := lineNum - 2; i >= 0 && i >= lineNum-4; i-- {
			if rePSCmdletBinding.MatchString(lines[i]) {
				kind = "cmdlet"
				break
			}
		}

		meta := map[string]string{}
		if doc != "" {
			meta["doc"] = doc
		}
		if kind != "" {
			meta["kind"] = kind
		}

		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// extractMethods finds method definitions inside class bodies.
// A method matches: optional [ReturnType] MethodName([params]) {
// but NOT keywords like if/while/for/foreach/switch/try/catch/do.
func (p *PowerShellParser) extractMethods(
	g *graph.Graph,
	lines []string,
	filePath string,
	fileNodeID graph.NodeID,
	classRanges []classRange,
) {
	if len(classRanges) == 0 {
		return
	}

	content := strings.Join(lines, "\n")
	psKeywords := map[string]bool{
		"if": true, "else": true, "elseif": true, "while": true, "for": true,
		"foreach": true, "switch": true, "try": true, "catch": true, "finally": true,
		"do": true, "until": true, "return": true, "function": true,
	}

	emitted := make(map[string]bool)

	for _, m := range rePSMethod.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		lineNum := strings.Count(content[:m[0]], "\n") + 1

		// Skip keywords.
		if psKeywords[strings.ToLower(name)] {
			continue
		}

		// Only process if inside a class body.
		cr := findEnclosingPSClass(lineNum, classRanges)
		if cr == nil {
			continue
		}

		dedupKey := cr.name + "." + name
		if emitted[dedupKey] {
			continue
		}
		emitted[dedupKey] = true

		qualName := cr.name + "." + name
		doc := extractPSDoc(lines, lineNum)
		meta := map[string]string{"kind": "method"}
		if doc != "" {
			meta["doc"] = doc
		}

		nodeID := g.MakeNodeID(filePath, qualName)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     qualName,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: meta,
		})
		// Method is defined by the class node.
		g.AddEdge(&graph.Edge{From: cr.nodeID, To: nodeID, Type: graph.EdgeDefines})
		// Also link from file.
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// extractRequiresModules handles: #Requires -Modules ModuleName, -Modules Mod1,Mod2
func (p *PowerShellParser) extractRequiresModules(
	g *graph.Graph,
	lines []string,
	filePath string,
	fileNodeID graph.NodeID,
) {
	content := strings.Join(lines, "\n")
	for _, m := range rePSRequiresModules.FindAllStringSubmatchIndex(content, -1) {
		raw := strings.TrimSpace(content[m[2]:m[3]])
		// May be comma-separated.
		for _, mod := range strings.Split(raw, ",") {
			mod = strings.TrimSpace(mod)
			mod = strings.Trim(mod, `'"`)
			if mod == "" {
				continue
			}
			importNodeID := g.MakeNodeID(mod, mod)
			g.AddNode(&graph.Node{
				ID:      importNodeID,
				Type:    graph.NodePackage,
				Name:    mod,
				Package: mod,
				File:    filePath,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
		}
	}
}

// extractImportModules handles: Import-Module ModuleName
func (p *PowerShellParser) extractImportModules(
	g *graph.Graph,
	lines []string,
	filePath string,
	fileNodeID graph.NodeID,
) {
	content := strings.Join(lines, "\n")
	for _, m := range rePSImportModule.FindAllStringSubmatchIndex(content, -1) {
		mod := strings.TrimSpace(content[m[2]:m[3]])
		mod = strings.Trim(mod, `'"`)
		if mod == "" {
			continue
		}
		importNodeID := g.MakeNodeID(mod, mod)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    mod,
			Package: mod,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}
}

// extractUsingModules handles: using module ./path or using module ModuleName
func (p *PowerShellParser) extractUsingModules(
	g *graph.Graph,
	lines []string,
	filePath string,
	fileNodeID graph.NodeID,
) {
	content := strings.Join(lines, "\n")
	for _, m := range rePSUsingModule.FindAllStringSubmatchIndex(content, -1) {
		mod := strings.TrimSpace(content[m[2]:m[3]])
		mod = strings.Trim(mod, `'"`)
		if mod == "" {
			continue
		}
		importNodeID := g.MakeNodeID(mod, mod)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    mod,
			Package: mod,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}
}

// isPSFunctionExported determines export visibility of a PowerShell function.
// Rules:
//   - Functions in .psm1 modules are exported unless prefixed with "_" or "private".
//   - Functions with a capital first letter → exported.
//   - Functions with a dash (Verb-Noun convention) → exported.
//   - Lowercase helpers without a dash → private.
func isPSFunctionExported(name string, isModule bool) bool {
	if strings.HasPrefix(name, "_") || strings.HasPrefix(strings.ToLower(name), "private") {
		return false
	}
	if isModule {
		return true
	}
	if strings.Contains(name, "-") {
		return true
	}
	if len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' {
		return true
	}
	return false
}

// extractPSDoc extracts the doc comment immediately above lineNum (1-based).
// Handles both `# line comment` style and `<# ... #>` block comment style.
func extractPSDoc(lines []string, lineNum int) string {
	if lineNum < 2 {
		return ""
	}
	idx := lineNum - 2 // 0-based index of the line immediately above
	if idx < 0 || idx >= len(lines) {
		return ""
	}

	// Check for <# ... #> block comment ending at or just above the function.
	if blockDoc := extractPSBlockDoc(lines, idx); blockDoc != "" {
		return blockDoc
	}

	// Fallback: collect contiguous # line comments above the declaration.
	return extractLineDoc(lines, lineNum, "#")
}

// extractPSBlockDoc looks for a <# ... #> block comment ending at or near lineIdx (0-based).
func extractPSBlockDoc(lines []string, lineIdx int) string {
	// Find the line ending with #> at or just above lineIdx.
	endIdx := -1
	for i := lineIdx; i >= 0 && i >= lineIdx-5; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if strings.HasSuffix(trimmed, "#>") {
			endIdx = i
			break
		}
		// If we hit a non-blank, non-comment line (not starting with # or <#), stop.
		if !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "<#") {
			break
		}
	}
	if endIdx < 0 {
		return ""
	}

	// Scan backwards for the opening <#.
	startIdx := -1
	for i := endIdx; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "<#") {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return ""
	}

	// Extract comment text, stripping <# and #> markers.
	var parts []string
	for i := startIdx; i <= endIdx; i++ {
		line := strings.TrimSpace(lines[i])
		line = strings.TrimPrefix(line, "<#")
		line = strings.TrimSuffix(line, "#>")
		line = strings.TrimSpace(line)
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.Join(parts, " ")
}

// estimatePSBlockEnd estimates the line number where a block starting at startLineIdx
// (0-based) ends by counting matching braces. Returns a 1-based end line number.
func estimatePSBlockEnd(lines []string, startLineIdx int) int {
	depth := 0
	for i := startLineIdx; i < len(lines); i++ {
		line := stripPSStringLiterals(lines[i])
		for _, ch := range line {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return i + 1 // 1-based
				}
			}
		}
	}
	return len(lines) // fallback: rest of file
}

// stripPSStringLiterals replaces string literal content with spaces to prevent
// braces inside strings from confusing the brace counter.
// Handles single-quoted 'strings' and double-quoted "strings".
func stripPSStringLiterals(line string) string {
	var b strings.Builder
	inSingle := false
	inDouble := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
			b.WriteByte(' ')
		case ch == '"' && !inSingle:
			inDouble = !inDouble
			b.WriteByte(' ')
		case ch == '#' && !inSingle && !inDouble:
			// Rest of line is a comment — skip it.
			return b.String()
		default:
			if inSingle || inDouble {
				b.WriteByte(' ')
			} else {
				b.WriteByte(ch)
			}
		}
	}
	return b.String()
}

// insideAnyClass returns true if lineNum (1-based) falls within any of the given class ranges.
func insideAnyClass(lineNum int, ranges []classRange) bool {
	for _, cr := range ranges {
		if lineNum > cr.startLine && lineNum <= cr.endLine {
			return true
		}
	}
	return false
}

// findEnclosingPSClass returns the classRange that contains lineNum (1-based), or nil.
func findEnclosingPSClass(lineNum int, ranges []classRange) *classRange {
	for i := range ranges {
		if lineNum > ranges[i].startLine && lineNum <= ranges[i].endLine {
			return &ranges[i]
		}
	}
	return nil
}

// extractConfigurations finds DSC `configuration Name { ... }` blocks.
// DSC configurations are used for Windows server configuration management.
// They are modelled as NodeStruct with kind=configuration since they define
// a named declarative configuration schema, not imperative logic.
func (p *PowerShellParser) extractConfigurations(
	g *graph.Graph,
	lines []string,
	filePath string,
	fileNodeID graph.NodeID,
) {
	content := strings.Join(lines, "\n")
	for _, m := range rePSConfiguration.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		lineNum := strings.Count(content[:m[0]], "\n") + 1

		doc := extractPSDoc(lines, lineNum)
		meta := map[string]string{"kind": "configuration"}
		if doc != "" {
			meta["doc"] = doc
		}

		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// extractWorkflows extracts PowerShell Workflow blocks.
func (p *PowerShellParser) extractWorkflows(g *graph.Graph, filePath string, fileNodeID graph.NodeID, content string, emitted map[string]bool) {
	for _, m := range rePSWorkflow.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:   nodeID,
			Type: graph.NodeStruct,
			Name: name,
			File: filePath,
			Line: line,
			Metadata: map[string]string{"kind": "workflow"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}
