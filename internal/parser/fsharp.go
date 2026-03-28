package parser

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// FSharpParser parses F# (.fs, .fsi, .fsx) source files.
// Used heavily in fintech (.NET ecosystem), financial modeling, and functional teams.
// Uses regex-based extraction — F# is not in go-tree-sitter.
//
// Extracts:
//   - Let bindings (functions and values)   → NodeFunction
//   - Type definitions (records, DUs, aliases, interfaces, classes) → NodeStruct
//   - Module declarations                   → NodePackage
//   - open statements (imports)             → NodePackage + EdgeImports
//   - Member methods inside types           → NodeFunction (qualified)
//   - Active patterns                       → NodeFunction with kind=active_pattern
type FSharpParser struct{}

// NewFSharpParser creates a ready-to-use FSharpParser.
func NewFSharpParser() *FSharpParser { return &FSharpParser{} }

// Extensions returns the file extensions handled by this parser.
func (p *FSharpParser) Extensions() []string {
	return []string{".fs", ".fsi", ".fsx"}
}

// ── Compiled regex patterns ───────────────────────────────────────────────────

var (
	// Let bindings at module level. F# module members are indented by exactly
	// 4 spaces under the enclosing namespace/module declaration; top-level
	// scripts may also have unindented (column-0) let bindings. We match
	// either form (0 or 4 leading spaces) to capture module-level functions
	// while excluding deeply-nested local let bindings (8+ spaces).
	//
	// Group 1: optional "rec "
	// Group 2: optional "inline "
	// Group 3: optional access modifier ("private " | "internal " | "public ")
	// Group 4: function/value name
	reFSharpLet = regexp.MustCompile(`(?m)^(?:    )?let\s+(rec\s+)?(inline\s+)?(private\s+|internal\s+|public\s+)?(\w+)`)

	// Active pattern: let (|Even|Odd|) n =
	// Also matches module-indented active patterns (4 leading spaces).
	// Group 1: full active pattern name including pipes and parens
	reFSharpActivePattern = regexp.MustCompile(`(?m)^(?:    )?let\s+(\(\|[^|)][^)]*\|[^)]*\))`)

	// F# instance/static member: member [this|self|qualifier.]Name
	// Group 1: optional "static " modifier (include trailing space)
	// Group 2: member name (after optional qualifier like "this.")
	// Note: "override" members use reFSharpOverride below — no "member" keyword.
	// The optional access modifier (private|internal|public) may appear between
	// "member" and the self-identifier/name, so we skip it explicitly.
	reFSharpMember = regexp.MustCompile(`(?m)^[ \t]+(static\s+)?member\s+(?:(?:private|internal|public)\s+)?(?:\w+\.)?(\w+)`)

	// F# override without "member" keyword: override this.Name or override this.Name()
	// Group 1: member name (after qualifier like "this.")
	reFSharpOverride = regexp.MustCompile(`(?m)^[ \t]+override\s+\w+\.(\w+)`)

	// F# abstract member: abstract [member] Name
	// Captures abstract declarations in type definitions.
	// Group 1: member name
	reFSharpAbstractMember = regexp.MustCompile(`(?m)^[ \t]+abstract(?:\s+member)?\s+(\w+)`)

	// F# default implementation: default this.Name
	// Group 1: member name (after "this." or similar qualifier)
	reFSharpDefaultMember = regexp.MustCompile(`(?m)^[ \t]+default\s+\w+\.(\w+)`)

	// Type definitions.
	// Group 1: optional access modifier
	// Group 2: type name (no generics consumed here)
	// Group 3: full remainder of the declaration line (after name + optional generics)
	// This captures from the optional generic up to end of line so we can inspect
	// constructor params "(params) =" AND post-"=" body on the same line.
	reFSharpType = regexp.MustCompile(`(?m)^type\s+(private\s+|internal\s+|public\s+)?(\w+)(?:\s*<[^>]*>)?(.*)\n?`)

	// Detect record type body: contains "{ ... }" on same line or starts with "{"
	reFSharpRecordBody = regexp.MustCompile(`\{`)

	// Detect class: constructor params "(param: type)" before "="
	// Matches "(anything) =" or "(anything)=" at start of rest
	reFSharpClassCtor = regexp.MustCompile(`\([^)]*\)\s*=`)

	// Detect interface: "=" followed by "abstract member" or "inherit"
	// (checked by scanning lines after the type decl)

	// Module declaration.
	// Group 1: optional "rec "
	// Group 2: module name (possibly dotted)
	reFSharpModule = regexp.MustCompile(`(?m)^module\s+(rec\s+)?(\w[\w.]*)\b`)

	// open statement.
	// Group 1: module path (dotted)
	reFSharpOpen = regexp.MustCompile(`(?m)^open\s+([\w][\w.]*)`)

	// Triple-slash doc comment: /// text
	reFSharpTripleDoc = regexp.MustCompile(`(?m)^\s*///\s?(.*)`)

	// Block doc comment: (** ... *)  — may span multiple lines but we extract
	// only the immediately preceding block.

	// Discriminated union case: | CaseName of Type  OR  | CaseName  (unit case)
	// Must be indented (inside a type body). Captures the case name.
	reFSharpDUCase = regexp.MustCompile(`(?m)^[ \t]+\|\s+([A-Z]\w*)(?:\s+of\s+|\s*$|\s*\n)`)

	// F# script package/assembly reference: #r "nuget:PackageName" or #r "/path/to/dll"
	// Only meaningful in .fsx script files.
	// Group 1: the reference path/name
	reFSharpScriptRef = regexp.MustCompile(`(?m)^#r\s+["']([^"']+)["']`)
)

// Parse extracts code entities from a single F# file and merges them into the graph.
func (p *FSharpParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// Deduplicate emitted names to avoid double-adding.
	emitted := make(map[string]bool)

	// ── 1. Module declarations ────────────────────────────────────────────────
	for _, m := range reFSharpModule.FindAllStringSubmatchIndex(content, -1) {
		// m[4], m[5] = module name group
		name := content[m[4]:m[5]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		line := 1 + strings.Count(content[:m[0]], "\n")
		doc := fsharpExtractDoc(lines, line)
		meta := map[string]string{"kind": "module"}
		if doc != "" {
			meta["doc"] = doc
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodePackage,
			Name:     name,
			Package:  name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 2. open statements (imports) ─────────────────────────────────────────
	openSeen := make(map[string]bool)
	for _, m := range reFSharpOpen.FindAllStringSubmatchIndex(content, -1) {
		importPath := content[m[2]:m[3]]
		if openSeen[importPath] {
			continue
		}
		openSeen[importPath] = true
		openLine := 1 + strings.Count(content[:m[0]], "\n")
		importNodeID := g.MakeNodeID(importPath, importPath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    importPath,
			Package: importPath,
			File:    filePath,
			Line:    openLine,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}

	// ── 2b. Script references (#r) — only in .fsx files ──────────────────────
	if strings.HasSuffix(strings.ToLower(filePath), ".fsx") {
		refSeen := make(map[string]bool)
		for _, m := range reFSharpScriptRef.FindAllStringSubmatchIndex(content, -1) {
			ref := content[m[2]:m[3]]
			if ref == "" || refSeen[ref] {
				continue
			}
			refSeen[ref] = true
			// Extract a friendly name from nuget: prefix or dll path.
			refName := ref
			if strings.HasPrefix(ref, "nuget:") {
				refName = strings.TrimPrefix(ref, "nuget:")
				// Strip version if present: "Newtonsoft.Json, 13.0.1" → "Newtonsoft.Json"
				if idx := strings.Index(refName, ","); idx >= 0 {
					refName = strings.TrimSpace(refName[:idx])
				}
			} else {
				// Use the base filename without extension.
				refName = filepath.Base(ref)
				refName = strings.TrimSuffix(refName, ".dll")
				refName = strings.TrimSuffix(refName, ".DLL")
			}
			refLine := 1 + strings.Count(content[:m[0]], "\n")
			importNodeID := g.MakeNodeID(refName, refName)
			g.AddNode(&graph.Node{
				ID:      importNodeID,
				Type:    graph.NodePackage,
				Name:    refName,
				Package: refName,
				File:    filePath,
				Line:    refLine,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
		}
	}

	// ── 3. Type definitions ───────────────────────────────────────────────────
	// We process type definitions before let bindings so member methods can
	// track the current type context.
	typeLines := make(map[int]string) // line 1-based → type name
	for _, m := range reFSharpType.FindAllStringSubmatchIndex(content, -1) {
		// Group indices: [0]=full match, [2][3]=access, [4][5]=name, [6][7]=rest
		typeName := content[m[4]:m[5]]
		accessMod := ""
		if m[2] >= 0 {
			accessMod = strings.TrimSpace(content[m[2]:m[3]])
		}
		rest := ""
		if m[6] >= 0 {
			rest = strings.TrimSpace(content[m[6]:m[7]])
		}
		line := 1 + strings.Count(content[:m[0]], "\n")
		typeLines[line] = typeName
		doc := fsharpExtractDoc(lines, line)

		exported := accessMod != "private" && accessMod != "internal"
		kind := fsharpClassifyType(typeName, rest, lines, line)

		meta := map[string]string{"kind": kind}
		if doc != "" {
			meta["doc"] = doc
		}

		nodeID := g.MakeNodeID(filePath, typeName)
		if emitted[typeName] {
			continue
		}
		emitted[typeName] = true
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     typeName,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 3b. Discriminated union cases ─────────────────────────────────────────
	// For union types, extract individual cases as NodeVariable nodes.
	// e.g. type Shape = | Circle of float | Square of float * float
	// → emits Circle and Square as NodeVariable with kind=union_case
	for _, m := range reFSharpType.FindAllStringSubmatchIndex(content, -1) {
		typeName := content[m[4]:m[5]]
		rest := ""
		if m[6] >= 0 {
			rest = strings.TrimSpace(content[m[6]:m[7]])
		}
		typeLine := 1 + strings.Count(content[:m[0]], "\n")
		kind := fsharpClassifyType(typeName, rest, lines, typeLine)
		if kind != "union" {
			continue
		}
		typeNodeID := g.MakeNodeID(filePath, typeName)
		// Scan the body lines for | Case patterns.
		bodyLines := fsharpBodyLines(lines, typeLine, 50)
		for _, bl := range bodyLines {
			caseMatches := reFSharpDUCase.FindStringSubmatch(bl)
			if caseMatches == nil {
				continue
			}
			caseName := caseMatches[1]
			if caseName == "" || emitted[typeName+"."+caseName] {
				continue
			}
			emitted[typeName+"."+caseName] = true
			caseLine := typeLine // approximate line (body scanning doesn't track exact lines)
			caseNodeID := g.MakeNodeID(filePath, typeName+"."+caseName)
			g.AddNode(&graph.Node{
				ID:       caseNodeID,
				Type:     graph.NodeVariable,
				Name:     typeName + "." + caseName,
				File:     filePath,
				Line:     caseLine,
				Exported: true,
				Metadata: map[string]string{
					"kind":       "union_case",
					"union_type": typeName,
				},
			})
			g.AddEdge(&graph.Edge{From: typeNodeID, To: caseNodeID, Type: graph.EdgeDefines})
		}
	}

	// ── 4. Active patterns ────────────────────────────────────────────────────
	for _, m := range reFSharpActivePattern.FindAllStringSubmatchIndex(content, -1) {
		// Group 1: full active pattern name, e.g. "(|Even|Odd|)"
		patternName := content[m[2]:m[3]]
		if emitted[patternName] {
			continue
		}
		emitted[patternName] = true
		line := 1 + strings.Count(content[:m[0]], "\n")
		doc := fsharpExtractDoc(lines, line)
		meta := map[string]string{"kind": "active_pattern"}
		if doc != "" {
			meta["doc"] = doc
		}
		nodeID := g.MakeNodeID(filePath, patternName)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     patternName,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 5. Let bindings (top-level functions/values) ─────────────────────────
	for _, m := range reFSharpLet.FindAllStringSubmatchIndex(content, -1) {
		// m[8][9] = function name (group 4, now shifted by the new inline group 2)
		name := content[m[8]:m[9]]
		if emitted[name] {
			continue
		}

		// Determine access/export. Group 3 = access modifier (indices 6,7).
		accessMod := ""
		if m[6] >= 0 {
			accessMod = strings.TrimSpace(content[m[6]:m[7]])
		}
		exported := accessMod != "private" && accessMod != "internal"

		// Detect inline modifier (group 2, indices 4,5).
		isInline := m[4] >= 0 && strings.TrimSpace(content[m[4]:m[5]]) == "inline"

		line := 1 + strings.Count(content[:m[0]], "\n")
		doc := fsharpExtractDoc(lines, line)
		meta := buildLangMeta(declMeta{Doc: doc})

		if isInline {
			if meta == nil {
				meta = map[string]string{}
			}
			meta["inline"] = "true"
		}

		emitted[name] = true
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 6. Abstract member declarations ───────────────────────────────────────
	// Process abstract members FIRST so they take priority in deduplication over
	// concrete implementations with the same name in subclasses.
	for _, m := range reFSharpAbstractMember.FindAllStringSubmatchIndex(content, -1) {
		memberName := content[m[2]:m[3]]
		if emitted[memberName] {
			continue
		}
		emitted[memberName] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1

		nodeID := g.MakeNodeID(filePath, memberName)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     memberName,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "abstract_member"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 6b. Override methods (override this.Name) ─────────────────────────────
	// Process overrides before regular members so they take correct kind.
	for _, m := range reFSharpOverride.FindAllStringSubmatchIndex(content, -1) {
		memberName := content[m[2]:m[3]]
		if emitted[memberName] {
			continue
		}
		emitted[memberName] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1

		nodeID := g.MakeNodeID(filePath, memberName)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     memberName,
			File:     filePath,
			Line:     line,
			Exported: !strings.HasPrefix(memberName, "_"),
			Metadata: map[string]string{"kind": "override"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 6c. Instance and static member methods ────────────────────────────────
	for _, m := range reFSharpMember.FindAllStringSubmatchIndex(content, -1) {
		modifierRaw := ""
		if m[2] >= 0 {
			modifierRaw = strings.TrimSpace(content[m[2]:m[3]])
		}
		memberName := content[m[4]:m[5]]
		if emitted[memberName] {
			continue
		}
		emitted[memberName] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1

		kind := "member"
		if strings.HasPrefix(modifierRaw, "static") {
			kind = "static_member"
		}

		meta := map[string]string{"kind": kind}
		nodeID := g.MakeNodeID(filePath, memberName)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     memberName,
			File:     filePath,
			Line:     line,
			Exported: !strings.HasPrefix(memberName, "_"),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 6d. Default member implementations ────────────────────────────────────
	for _, m := range reFSharpDefaultMember.FindAllStringSubmatchIndex(content, -1) {
		memberName := content[m[2]:m[3]]
		nodeKey := "default:" + memberName
		if emitted[nodeKey] {
			continue
		}
		emitted[nodeKey] = true
		// If member already emitted as abstract, just update its kind to default_member.
		// Otherwise emit a new node.
		existingID := g.MakeNodeID(filePath, memberName)
		if existing := g.GetNode(existingID); existing != nil {
			if existing.Metadata == nil {
				existing.Metadata = map[string]string{}
			}
			existing.Metadata["has_default"] = "true"
			continue
		}
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		nodeID := g.MakeNodeID(filePath, memberName)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     memberName,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "default_member"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	return nil
}

// fsharpClassifyType determines the kind of an F# type definition based on its
// name, the full declaration line remainder, and subsequent lines in the file.
//
// rest contains everything on the declaration line after the name (and optional
// generic parameters), including any "= ..." continuation. Examples:
//   - "= {" or "(accountId: int) =" or "=\n" or " = string"
//
// Returns one of: "record", "union", "alias", "interface", "class".
func fsharpClassifyType(name, rest string, lines []string, typeLine int) string {
	restTrimmed := strings.TrimSpace(rest)

	// Class: constructor parameters "(params) =" on the declaration line.
	// e.g. rest = "(accountId: AccountId) ="  or  "(conn: string) ="
	if reFSharpClassCtor.MatchString(restTrimmed) {
		return "class"
	}

	// Scan the body: look at lines following the type declaration (up to 20 lines).
	bodyLines := fsharpBodyLines(lines, typeLine, 20)
	body := strings.Join(bodyLines, "\n")

	// Interface: indented body contains "abstract member" or inherits interface
	if strings.Contains(body, "abstract member") || strings.Contains(body, "inherit ") {
		// Make sure the class ctor check above didn't miss it; DUs won't have "abstract member"
		return "interface"
	}

	// Record: "= {" on the declaration line or first non-empty body line starts with "{"
	if strings.Contains(restTrimmed, "= {") || strings.HasPrefix(restTrimmed, "= {") {
		return "record"
	}
	for _, bl := range bodyLines {
		blt := strings.TrimSpace(bl)
		if blt == "" {
			continue
		}
		if strings.HasPrefix(blt, "{") {
			return "record"
		}
		break
	}

	// Discriminated union: body contains "| Case" pattern
	if strings.Contains(body, "| ") {
		// Confirm it looks like DU cases (uppercase after pipe)
		for _, bl := range bodyLines {
			blt := strings.TrimSpace(bl)
			if strings.HasPrefix(blt, "| ") || blt == "|" {
				return "union"
			}
			// Also: first content line like "= Case1 | Case2"
			if strings.HasPrefix(blt, "= ") && strings.Contains(blt, " | ") {
				return "union"
			}
		}
	}

	// Also check rest itself for inline DU: "= Market | Limit | Stop"
	if strings.Contains(restTrimmed, "= ") && strings.Contains(restTrimmed, " | ") {
		return "union"
	}

	// Record fallback: body contains "{ ... }" (multi-line records)
	if reFSharpRecordBody.MatchString(body) {
		return "record"
	}

	// Default: simple type alias  (type MyAlias = SomeType)
	return "alias"
}

// fsharpBodyLines returns up to maxLines lines after typeLine (1-based), skipping
// blank lines but stopping at the next top-level declaration.
func fsharpBodyLines(lines []string, typeLine, maxLines int) []string {
	var result []string
	for i := typeLine; i < len(lines) && len(result) < maxLines; i++ {
		line := lines[i]
		// Stop at next top-level declaration (another type/let/module at col 0).
		if i > typeLine && len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '/' {
			break
		}
		result = append(result, line)
	}
	return result
}

// fsharpExtractDoc extracts the doc comment immediately preceding a declaration
// at the given 1-based line number. Handles:
//   - /// triple-slash XML doc comments (collected contiguously above)
//   - (** ... *) block doc comments
func fsharpExtractDoc(lines []string, startLine int) string {
	if startLine < 2 {
		return ""
	}

	// Check if immediately above is a triple-slash doc comment.
	prevIdx := startLine - 2 // 0-based
	if prevIdx >= 0 && prevIdx < len(lines) {
		trimmed := strings.TrimSpace(lines[prevIdx])
		if strings.HasPrefix(trimmed, "///") {
			// Collect all contiguous /// lines above.
			return extractLineDoc(lines, startLine, "///")
		}
	}

	// Check for (** ... *) block doc comment ending before this line.
	return fsharpExtractBlockDoc(lines, startLine)
}

// fsharpExtractBlockDoc extracts a (** ... *) block doc comment ending just
// before the declaration at startLine (1-based).
func fsharpExtractBlockDoc(lines []string, startLine int) string {
	if startLine < 2 {
		return ""
	}
	// Find the end of the block comment: a line containing "*)" before startLine.
	endIdx := -1
	for i := startLine - 2; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if strings.HasSuffix(trimmed, "*)") {
			endIdx = i
		}
		break
	}
	if endIdx < 0 {
		return ""
	}
	// Scan backwards for the opening "(**" or "(*".
	startIdx := endIdx
	for i := endIdx; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "(**") || strings.HasPrefix(trimmed, "(*") {
			startIdx = i
			break
		}
		if !strings.HasPrefix(trimmed, "*") {
			return ""
		}
	}
	// Extract comment text.
	var parts []string
	for i := startIdx; i <= endIdx; i++ {
		trimmed := strings.TrimSpace(lines[i])
		trimmed = strings.TrimPrefix(trimmed, "(**")
		trimmed = strings.TrimPrefix(trimmed, "(*")
		trimmed = strings.TrimSuffix(trimmed, "*)")
		trimmed = strings.TrimPrefix(trimmed, "* ")
		trimmed = strings.TrimPrefix(trimmed, "*")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " ")
}

// reFSharpTripleDocLine matches a single /// comment line. Used to ensure
// the regex is compiled once for internal use.
var _ = reFSharpTripleDoc // suppress unused-var lint (used in test via fsharpExtractDoc)
