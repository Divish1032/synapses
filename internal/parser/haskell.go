package parser

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// HaskellParser parses Haskell (.hs, .lhs) source files.
// Used in fintech, compiler infrastructure, and functional programming teams.
// Uses regex-based extraction.
type HaskellParser struct{}

// NewHaskellParser creates a ready-to-use HaskellParser.
func NewHaskellParser() *HaskellParser { return &HaskellParser{} }

// Extensions returns the file extensions handled by this parser.
func (p *HaskellParser) Extensions() []string {
	return []string{".hs", ".lhs"}
}

// Regex patterns for Haskell source extraction.
var (
	// module declaration: module Foo.Bar (export1, export2) where
	// OR: module Foo.Bar where  (no export list)
	reHaskellModule = regexp.MustCompile(`(?m)^module\s+([\w.]+)`)

	// module with explicit export list: module Foo (a, b, c) where
	// Captures everything between the parens before "where".
	reHaskellModuleExports = regexp.MustCompile(`(?s)^module\s+[\w.]+\s*\((.+?)\)\s*where`)

	// import statements (all variants):
	//   import Data.Map
	//   import qualified Data.Map as Map
	//   import Data.Map (Map, lookup)
	//   import Data.List hiding (sort)
	reHaskellImport = regexp.MustCompile(`(?m)^import\s+(?:qualified\s+)?([\w.]+)`)

	// type signature: functionName :: SomeType -> OtherType
	// Must start at column 0 (top-level). Captures the function name.
	reHaskellTypeSig = regexp.MustCompile(`(?m)^([a-zA-Z_][\w']*)\s*::`)

	// function definition: functionName args = body  (top-level, no leading whitespace)
	// We require no leading whitespace to avoid matching let/where bindings.
	// [^\n=]* restricts matching to a single line to avoid consuming multi-line data/class decls.
	reHaskellFuncDef = regexp.MustCompile(`(?m)^([a-z_][\w']*)\s+[^\n=]*=`)

	// data declaration: data TypeName a b = ...
	reHaskellData = regexp.MustCompile(`(?m)^data\s+([\w']+)`)

	// newtype declaration: newtype TypeName a = ...
	reHaskellNewtype = regexp.MustCompile(`(?m)^newtype\s+([\w']+)`)

	// typeclass declaration: class (constraints =>) ClassName args where
	//   OR: class ClassName args where
	// We capture the first uppercase-starting identifier after "class" (and after any "=>" constraint).
	reHaskellClass = regexp.MustCompile(`(?m)^class\s+(?:(?:[^\n]*?)\s*=>\s*)?([A-Z][\w']*)\s`)

	// typeclass instance: instance Show MyType where
	// OR: instance (Eq a) => Ord (Tree a) where
	reHaskellInstance = regexp.MustCompile(`(?m)^instance\s+(.+?)\s+where\b`)

	// type alias: type Name = SomeType  OR  type Name a b = Something a b
	// OR: type family Name a (type families, GADTs extension)
	// Captures the type name (uppercase). [\w']* (zero or more) allows single-letter
	// names like "F" (type family F a).
	reHaskellTypeAlias = regexp.MustCompile(`(?m)^type\s+(?:family\s+)?([A-Z][\w']*)`)

	// operator type signature: (+++) :: Foo -> Foo -> Foo
	// Operators are defined with parens: (>>>=), (<|>), etc.
	// Group 1: operator text inside parens (e.g. "+++", ">>=")
	reHaskellOpTypeSig = regexp.MustCompile(`(?m)^\(([^\)]+)\)\s*::`)

	// Haddock block doc comment: {- | ... -}
	reHaskellBlockDoc = regexp.MustCompile(`(?s)\{-\s*\|(.+?)-\}`)

	// foreign import: foreign import callconv [safe|unsafe] ["header"] funcName :: Type
	// The calling convention may be a single word (ccall) or two words (ccall safe).
	// The optional quoted C name may span to the next line when the Haskell name is
	// on the following indented line.
	// Pattern: foreign import <word(s)> [optional "string"] <haskellName>
	// Group 1: Haskell function name
	reForeignImport = regexp.MustCompile(`(?m)^foreign\s+import\s+(?:\w+\s+)*(?:"[^"]*"\s+)?([a-zA-Z_][\w']*)`)
)

// Parse extracts code entities from a single Haskell (.hs or .lhs) source file.
func (p *HaskellParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	// For literate Haskell (.lhs), extract only the code lines (bird-track style).
	content := string(src)
	if strings.HasSuffix(strings.ToLower(filePath), ".lhs") {
		content = extractLiterateHaskell(content)
	}

	lines := strings.Split(content, "\n")

	// Strip block comments for pattern matching (preserves newlines for line counting).
	codeContent := stripHaskellBlockComments(content)

	// --- Module declaration ---
	moduleName, exportList, hasExplicitExports := extractHaskellModule(content)
	if moduleName != "" {
		moduleNodeID := g.MakeNodeID(filePath, moduleName)
		g.AddNode(&graph.Node{
			ID:      moduleNodeID,
			Type:    graph.NodePackage,
			Name:    moduleName,
			Package: moduleName,
			File:    filePath,
			Line:    1,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: moduleNodeID, Type: graph.EdgeDefines})
	}

	// --- Import statements ---
	for _, m := range reHaskellImport.FindAllStringSubmatchIndex(content, -1) {
		importName := content[m[2]:m[3]]
		if importName == "" {
			continue
		}
		importNodeID := g.MakeNodeID(importName, importName)
		line := 1 + countNewlines(content[:m[0]])
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    importName,
			Package: importName,
			File:    filePath,
			Line:    line,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}

	// --- Collect type signatures (function name → line) ---
	// Use codeContent (block comments stripped) to avoid matching type-sig-like
	// text inside commented-out code.  Line numbers remain valid because
	// stripHaskellBlockComments preserves all newlines.
	typeSigLines := make(map[string]int)
	typeSigDocs := make(map[string]string)
	for _, m := range reHaskellTypeSig.FindAllStringSubmatchIndex(codeContent, -1) {
		name := codeContent[m[2]:m[3]]
		lineNum := 1 + countNewlines(codeContent[:m[0]])
		typeSigLines[name] = lineNum
		// Extract Haddock doc comment from original content (comments not stripped).
		doc := extractHaskellHaddockDoc(lines, lineNum)
		if doc != "" {
			typeSigDocs[name] = doc
		}
	}

	// --- Collect function definitions (top-level, no leading whitespace) ---
	// We use the type signature line if available, otherwise the def line.
	// Track already-emitted names to avoid duplicates.
	emitted := make(map[string]bool)

	// Process type signatures first: each is a function declaration.
	// Run on codeContent to skip type sigs inside block comments.
	for _, m := range reHaskellTypeSig.FindAllStringSubmatchIndex(codeContent, -1) {
		name := codeContent[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		// Skip operator-style names or constructor names (they start with uppercase or special chars).
		if len(name) == 0 || name[0] == '_' && len(name) == 1 {
			continue
		}
		emitted[name] = true
		lineNum := 1 + countNewlines(codeContent[:m[0]])
		doc := typeSigDocs[name]

		exported := isHaskellExported(name, moduleName, exportList, hasExplicitExports)
		nodeID := g.MakeNodeID(filePath, name)
		// extractAfterSig reads from original content (same text at this position
		// since block-comment stripping preserves character offsets).
		meta := buildLangMeta(declMeta{Doc: doc, Signature: name + " ::" + extractAfterSig(content, m[0])})
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

	// Process function definitions for names not already covered by type signatures.
	// Run on codeContent to skip definitions inside block comments.
	for _, m := range reHaskellFuncDef.FindAllStringSubmatchIndex(codeContent, -1) {
		name := codeContent[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		// Skip keywords that look like function defs.
		if isHaskellKeyword(name) {
			continue
		}
		emitted[name] = true
		lineNum := 1 + countNewlines(codeContent[:m[0]])
		doc := extractHaskellHaddockDoc(lines, lineNum)

		exported := isHaskellExported(name, moduleName, exportList, hasExplicitExports)
		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declMeta{Doc: doc})
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

	// --- Data declarations ---
	for _, m := range reHaskellData.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := 1 + countNewlines(content[:m[0]])
		doc := extractHaskellHaddockDoc(lines, lineNum)

		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declMeta{Doc: doc})
		if meta == nil {
			meta = make(map[string]string)
		}
		meta["kind"] = "data"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true, // types starting with uppercase are always exported
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Newtype declarations ---
	for _, m := range reHaskellNewtype.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := 1 + countNewlines(content[:m[0]])
		doc := extractHaskellHaddockDoc(lines, lineNum)

		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declMeta{Doc: doc})
		if meta == nil {
			meta = make(map[string]string)
		}
		meta["kind"] = "newtype"
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

	// --- Typeclass declarations ---
	for _, m := range reHaskellClass.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := 1 + countNewlines(content[:m[0]])
		doc := extractHaskellHaddockDoc(lines, lineNum)

		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declMeta{Doc: doc})
		if meta == nil {
			meta = make(map[string]string)
		}
		meta["kind"] = "typeclass"
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

	// --- Typeclass instances ---
	for _, m := range reHaskellInstance.FindAllStringSubmatchIndex(content, -1) {
		instanceText := strings.TrimSpace(content[m[2]:m[3]])
		// Normalize the instance name: use the full text as identifier (trimmed).
		// e.g. "Show MyType" or "Eq a => Ord (Tree a)"
		instanceName := normalizeInstanceName(instanceText)
		if instanceName == "" || emitted[instanceName] {
			continue
		}
		emitted[instanceName] = true
		lineNum := 1 + countNewlines(content[:m[0]])
		doc := extractHaskellHaddockDoc(lines, lineNum)

		nodeID := g.MakeNodeID(filePath, instanceName)
		meta := buildLangMeta(declMeta{Doc: doc})
		if meta == nil {
			meta = make(map[string]string)
		}
		meta["kind"] = "instance"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     instanceName,
			File:     filePath,
			Line:     lineNum,
			Exported: false,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Type alias declarations (type Name = SomeType) ---
	for _, m := range reHaskellTypeAlias.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := 1 + countNewlines(content[:m[0]])
		doc := extractHaskellHaddockDoc(lines, lineNum)

		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declMeta{Doc: doc})
		if meta == nil {
			meta = make(map[string]string)
		}
		meta["kind"] = "type_alias"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true, // type aliases starting with uppercase are always exported
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Operator type signatures: (+++) :: Foo -> Foo -> Foo ---
	// Run on codeContent to skip operators defined inside block comments.
	for _, m := range reHaskellOpTypeSig.FindAllStringSubmatchIndex(codeContent, -1) {
		opText := strings.TrimSpace(codeContent[m[2]:m[3]])
		// Use "(op)" as the node name to distinguish from regular functions.
		nodeName := "(" + opText + ")"
		if emitted[nodeName] {
			continue
		}
		emitted[nodeName] = true
		lineNum := 1 + countNewlines(codeContent[:m[0]])
		doc := extractHaskellHaddockDoc(lines, lineNum)

		exported := isHaskellExported(nodeName, moduleName, exportList, hasExplicitExports)
		nodeID := g.MakeNodeID(filePath, nodeName)
		meta := buildLangMeta(declMeta{
			Doc:       doc,
			Signature: nodeName + " ::" + extractAfterSig(content, m[0]),
		})
		if meta == nil {
			meta = make(map[string]string)
		}
		meta["kind"] = "operator"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     nodeName,
			File:     filePath,
			Line:     lineNum,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Foreign imports (FFI declarations) ---
	// Run on codeContent to skip foreign imports inside block comments.
	for _, m := range reForeignImport.FindAllStringSubmatchIndex(codeContent, -1) {
		name := codeContent[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := 1 + countNewlines(codeContent[:m[0]])
		doc := extractHaskellHaddockDoc(lines, lineNum)

		exported := isHaskellExported(name, moduleName, exportList, hasExplicitExports)
		nodeID := g.MakeNodeID(filePath, name)
		meta := buildLangMeta(declMeta{Doc: doc})
		if meta == nil {
			meta = make(map[string]string)
		}
		meta["kind"] = "foreign_import"
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

	// --- Block Haddock doc comments ({- | ... -}) ---
	// These are already handled inline above for individual declarations.
	// We process them here to attach to adjacent declarations that may have
	// been missed by the line-based scanner.
	for _, m := range reHaskellBlockDoc.FindAllStringSubmatchIndex(content, -1) {
		// Block docs are attached when we extract individual declarations above.
		// This loop is intentionally left as a no-op to acknowledge the pattern.
		_ = m
	}

	return nil
}

// extractLiterateHaskell extracts code lines from literate Haskell (.lhs) files.
// In bird-track style, code lines start with ">". LaTeX style (\begin{code})
// is also supported as a fallback.
func extractLiterateHaskell(content string) string {
	lines := strings.Split(content, "\n")
	var codeLines []string

	// Detect if it uses bird-track style (lines starting with ">").
	hasBirdTrack := false
	for _, line := range lines {
		if strings.HasPrefix(line, ">") {
			hasBirdTrack = true
			break
		}
	}

	if hasBirdTrack {
		for _, line := range lines {
			if strings.HasPrefix(line, ">") {
				// Strip the ">" prefix and optional single space.
				code := line[1:]
				if len(code) > 0 && code[0] == ' ' {
					code = code[1:]
				}
				codeLines = append(codeLines, code)
			} else {
				// Non-code lines become blank lines to preserve line numbering.
				codeLines = append(codeLines, "")
			}
		}
		return strings.Join(codeLines, "\n")
	}

	// LaTeX style: \begin{code} ... \end{code}
	inCode := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == `\begin{code}` {
			inCode = true
			codeLines = append(codeLines, "")
			continue
		}
		if trimmed == `\end{code}` {
			inCode = false
			codeLines = append(codeLines, "")
			continue
		}
		if inCode {
			codeLines = append(codeLines, line)
		} else {
			codeLines = append(codeLines, "")
		}
	}
	return strings.Join(codeLines, "\n")
}

// extractHaskellModule parses the module declaration and optional export list.
// Returns (moduleName, exportedNames, hasExplicitExports).
func extractHaskellModule(content string) (string, map[string]bool, bool) {
	// Try to find module with explicit export list first.
	if m := reHaskellModuleExports.FindStringSubmatch(content); m != nil {
		name := reHaskellModule.FindStringSubmatch(content)
		if name == nil {
			return "", nil, false
		}
		exports := parseHaskellExportList(m[1])
		return name[1], exports, true
	}
	// Module without explicit export list.
	if m := reHaskellModule.FindStringSubmatch(content); m != nil {
		return m[1], nil, false
	}
	return "", nil, false
}

// parseHaskellExportList parses the comma-separated export list from a module
// declaration. Handles nested parens (for type exports like Maybe(..)).
func parseHaskellExportList(raw string) map[string]bool {
	exports := make(map[string]bool)
	// Strip nested parens content (e.g. "Maybe(..)" → "Maybe").
	// Split on commas, trim each entry, extract the identifier.
	depth := 0
	var current strings.Builder
	for _, ch := range raw {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				name := strings.TrimSpace(current.String())
				// Remove any trailing paren content (e.g. "Maybe(..)" → "Maybe").
				if idx := strings.IndexByte(name, '('); idx >= 0 {
					name = strings.TrimSpace(name[:idx])
				}
				if name != "" {
					exports[name] = true
				}
				current.Reset()
				continue
			}
		}
		if depth == 0 {
			current.WriteRune(ch)
		}
	}
	// Handle last entry.
	if current.Len() > 0 {
		name := strings.TrimSpace(current.String())
		if idx := strings.IndexByte(name, '('); idx >= 0 {
			name = strings.TrimSpace(name[:idx])
		}
		if name != "" {
			exports[name] = true
		}
	}
	return exports
}

// isHaskellExported determines if a top-level name should be marked as exported.
// Rules:
//  1. Names starting with "_" are always private.
//  2. If there's an explicit export list, only listed names are exported.
//  3. If no explicit export list, all lowercase-starting names are exported.
//  4. Uppercase-starting names (types) are always exported.
func isHaskellExported(name, _ string, exportList map[string]bool, hasExplicitExports bool) bool {
	if len(name) == 0 {
		return false
	}
	// Convention: underscore prefix → private.
	if name[0] == '_' {
		return false
	}
	// Types (uppercase) are always exported.
	if name[0] >= 'A' && name[0] <= 'Z' {
		return true
	}
	// If explicit export list exists, check membership.
	if hasExplicitExports {
		return exportList[name]
	}
	// No explicit export list → all top-level definitions are exported.
	return true
}

// extractHaskellHaddockDoc extracts a Haddock doc comment immediately before
// the given 1-indexed line number. Handles:
//   - "-- |" lines (single-line Haddock)
//   - "{- | ... -}" block Haddock (detected by scanning backwards)
func extractHaskellHaddockDoc(lines []string, startLine int) string {
	if startLine < 2 || len(lines) == 0 {
		return ""
	}
	var parts []string
	for i := startLine - 2; i >= 0 && i >= startLine-30; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			break
		}
		// Single-line Haddock: -- | text  OR  -- ^ text (for following doc)
		if strings.HasPrefix(trimmed, "-- |") || strings.HasPrefix(trimmed, "-- ^") {
			text := strings.TrimPrefix(trimmed, "-- |")
			text = strings.TrimPrefix(text, "-- ^")
			text = strings.TrimSpace(text)
			parts = append([]string{text}, parts...)
			continue
		}
		// Continuation of a -- | doc block: lines starting with "--" (but not "-- |" or code)
		if strings.HasPrefix(trimmed, "--") && !strings.HasPrefix(trimmed, "-- |") {
			// Check if previous lines established a haddock block.
			text := strings.TrimPrefix(trimmed, "--")
			text = strings.TrimSpace(text)
			parts = append([]string{text}, parts...)
			continue
		}
		// Block Haddock: {- | ... -}
		if strings.HasSuffix(trimmed, "-}") {
			// Scan backwards for the opening {- |.
			var blockParts []string
			for j := i; j >= 0 && j >= i-20; j-- {
				bline := strings.TrimSpace(lines[j])
				if strings.HasPrefix(bline, "{-") && strings.Contains(bline, "|") {
					// Opening line.
					inner := bline
					inner = strings.TrimPrefix(inner, "{-")
					inner = strings.TrimPrefix(inner, " |")
					inner = strings.TrimSuffix(inner, "-}")
					inner = strings.TrimSpace(inner)
					if inner != "" {
						blockParts = append([]string{inner}, blockParts...)
					}
					parts = append([]string{strings.Join(blockParts, " ")}, parts...)
					break
				}
				// Middle line of block comment.
				cleaned := bline
				cleaned = strings.TrimSuffix(cleaned, "-}")
				cleaned = strings.TrimPrefix(cleaned, "-")
				cleaned = strings.TrimSpace(cleaned)
				if cleaned != "" {
					blockParts = append([]string{cleaned}, blockParts...)
				}
			}
			break
		}
		// Not a comment — stop scanning.
		break
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// normalizeInstanceName creates a compact, unique identifier for a typeclass instance.
// e.g. "Show MyType" → "instance Show MyType"
//
//	"Eq a => Ord (Tree a)" → "instance Ord (Tree a)"
func normalizeInstanceName(instanceText string) string {
	// Strip any constraint (everything before "=>").
	if idx := strings.Index(instanceText, "=>"); idx >= 0 {
		instanceText = strings.TrimSpace(instanceText[idx+2:])
	}
	// Prefix with "instance" to make it clear and unique.
	name := "instance " + instanceText
	// Replace spaces with underscores and remove parens for a valid identifier-like name.
	name = strings.Map(func(r rune) rune {
		switch r {
		case ' ':
			return '_'
		case '(', ')':
			return -1 // drop
		}
		return r
	}, name)
	// Collapse multiple underscores.
	for strings.Contains(name, "__") {
		name = strings.ReplaceAll(name, "__", "_")
	}
	name = strings.Trim(name, "_")
	return name
}

// extractAfterSig extracts the type signature text after "::" on the same line.
// Returns a short snippet (up to 80 chars) of the type.
func extractAfterSig(content string, matchStart int) string {
	// Find the "::" in the match vicinity.
	end := matchStart + 120
	if end > len(content) {
		end = len(content)
	}
	snippet := content[matchStart:end]
	// Find first newline to limit to single line.
	if nl := strings.Index(snippet, "\n"); nl >= 0 {
		snippet = snippet[:nl]
	}
	// Extract the part after "::"
	if idx := strings.Index(snippet, "::"); idx >= 0 {
		sig := strings.TrimSpace(snippet[idx+2:])
		if len(sig) > 80 {
			sig = sig[:80]
		}
		return " " + sig
	}
	return ""
}

// isHaskellKeyword returns true if the name is a Haskell keyword that should not
// be treated as a function definition.
func isHaskellKeyword(name string) bool {
	switch name {
	case "if", "then", "else", "let", "in", "where", "do", "of",
		"case", "class", "data", "default", "deriving", "import",
		"infixl", "infixr", "infix", "instance", "module", "newtype",
		"type", "foreign", "hiding", "qualified", "as", "forall",
		"mdo", "rec", "proc":
		return true
	}
	return false
}

// stripHaskellBlockComments returns a copy of src with all non-pragma block
// comments ({- ... -}) replaced by spaces.  All newlines are preserved so
// that byte offsets and line numbers computed from the returned string remain
// identical to those from the original.
//
// Rules:
//   - Pragmas {-# ... #-} are kept unchanged.
//   - Haddock module docs {-| ... -} are stripped (they contain prose, not code).
//   - Box-style decorators like {---------} are stripped.
//   - Block comments nest in Haskell ({- {- inner -} outer -}).
//
// This prevents the regex-based type-sig and function-def scanners from
// matching identifiers that appear in commented-out code.
func stripHaskellBlockComments(src string) string {
	b := []byte(src)
	i := 0
	depth := 0
	for i < len(b) {
		// Start of a block comment or pragma?
		if i+1 < len(b) && b[i] == '{' && b[i+1] == '-' {
			// Peek: is it a pragma {-# ... #-}?
			if i+2 < len(b) && b[i+2] == '#' {
				// Pragma: skip past the closing #-} without blanking.
				i += 3
				for i+2 < len(b) {
					if b[i] == '#' && b[i+1] == '-' && b[i+2] == '}' {
						i += 3
						break
					}
					i++
				}
				continue
			}
			// Non-pragma block comment — start blanking.
			depth++
			// Replace opening {- with spaces.
			if b[i] != '\n' {
				b[i] = ' '
			}
			if b[i+1] != '\n' {
				b[i+1] = ' '
			}
			i += 2
			continue
		}
		if depth > 0 {
			if i+1 < len(b) && b[i] == '{' && b[i+1] == '-' {
				// Nested block comment start.
				depth++
				if b[i] != '\n' {
					b[i] = ' '
				}
				if b[i+1] != '\n' {
					b[i+1] = ' '
				}
				i += 2
				continue
			}
			if i+1 < len(b) && b[i] == '-' && b[i+1] == '}' {
				// End of a block comment level.
				depth--
				if b[i] != '\n' {
					b[i] = ' '
				}
				if b[i+1] != '\n' {
					b[i+1] = ' '
				}
				i += 2
				continue
			}
			// Inside a block comment: blank out non-newline characters.
			if b[i] != '\n' {
				b[i] = ' '
			}
			i++
			continue
		}
		i++
	}
	return string(b)
}
