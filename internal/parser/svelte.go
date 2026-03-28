package parser

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// SvelteParser parses Svelte (.svelte) single-file components.
//
// Extracts:
//   - Script block imports (import X from 'module') → EdgeImports
//   - Component imports (import Comp from './Comp.svelte') → EdgeImports
//   - Exported variables (export let propName) → NodeVariable with kind="prop"
//   - Reactive declarations ($: computed = ...) → NodeVariable with kind="reactive"
//   - Function declarations (function name() {}) → NodeFunction
//   - Script-level const/let variables → NodeVariable
//   - Svelte component usage (<Component />) → EdgeCalls
type SvelteParser struct{}

// NewSvelteParser creates a ready-to-use SvelteParser.
func NewSvelteParser() *SvelteParser {
	return &SvelteParser{}
}

// Extensions returns the file extensions handled by this parser.
func (p *SvelteParser) Extensions() []string {
	return []string{".svelte"}
}

// Regex patterns for extracting constructs from the script block.
var (
	// Match <script> or <script context="module"> blocks and capture content.
	reScriptBlock = regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>(.+?)</script>`)

	// import { foo } from 'module'  OR  import foo from 'module'  OR  import 'module'
	// (?s) enables dot-matches-newline so multi-line imports (brace spanning several lines) are matched.
	reSvelteImport = regexp.MustCompile(`(?s)import\s+(?:[^'"]+?\s+from\s+)?['"]([^'"]+)['"]`)

	// export let name  OR  export let name = value
	reSvelteExportLet = regexp.MustCompile(`(?m)^\s*export\s+let\s+(\w+)`)

	// export const name = value
	reSvelteExportConst = regexp.MustCompile(`(?m)^\s*export\s+const\s+(\w+)`)

	// $: name = ...  (reactive declaration)
	reSvelteReactive = regexp.MustCompile(`(?m)^\s*\$:\s+(\w+)\s*=`)

	// function name(...) { ... }  OR  async function name(...) { ... }
	reSvelteFunction = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*\(`)

	// const/let name = (params) => ...  OR  const/let name = async (params) => ...
	// Matches the opening paren of an arrow function — sufficient heuristic for Svelte patterns.
	reSvelteArrowFunc = regexp.MustCompile(`(?m)^\s*(?:export\s+)?(?:const|let)\s+(\w+)\s*=\s*(?:async\s+)?\(`)

	// const name = ...  OR  let name = ...  (non-export, script-level)
	reSvelteVariable = regexp.MustCompile(`(?m)^\s*(const|let)\s+(\w+)\s*=`)

	// Component usage in template: <ComponentName or <ComponentName>
	// Must start with uppercase letter (Svelte convention).
	reSvelteComponent = regexp.MustCompile(`<([A-Z]\w+)[\s/>]`)

	// ── Svelte 5 runes ──────────────────────────────────────────────────────
	// const { x, y, z } = $props()  — destructured component props
	// Captures the full content between the braces.
	reSvelteProps5 = regexp.MustCompile(`(?m)^\s*(?:const|let)\s+\{([^}]+)\}\s*=\s*\$props\b`)

	// let x = $state(...)  OR  let x = $state.raw(...)
	reSvelteState5 = regexp.MustCompile(`(?m)^\s*let\s+(\w+)\s*=\s*\$state\b`)

	// const x = $derived(...)  OR  const x = $derived.by(...)
	reSvelteDerived5 = regexp.MustCompile(`(?m)^\s*const\s+(\w+)\s*=\s*\$derived\b`)

	// const x = $bindable(...)  — bindable prop (Svelte 5)
	reSvelteBindable5 = regexp.MustCompile(`(?m)^\s*(?:const|let)\s+(\w+)\s*=\s*\$bindable\b`)
)

// Parse extracts code entities from a single Svelte file and merges them into the graph.
func (p *SvelteParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	content := string(src)

	// Extract all script blocks (both <script> and <script context="module">).
	scriptMatches := reScriptBlock.FindAllStringSubmatch(content, -1)
	var scriptContent strings.Builder
	for _, m := range scriptMatches {
		scriptContent.WriteString(m[1])
		scriptContent.WriteString("\n")
	}
	script := scriptContent.String()

	// Track names already emitted to avoid duplicates (export let vs let).
	emitted := make(map[string]bool)

	// Compute line offsets for the first script block to get approximate line numbers.
	scriptStartLine := 1
	if idx := strings.Index(content, "<script"); idx >= 0 {
		scriptStartLine = strings.Count(content[:idx], "\n") + 1
	}
	scriptLines := strings.Split(script, "\n")

	// --- Imports ---
	for _, m := range reSvelteImport.FindAllStringSubmatch(script, -1) {
		importPath := m[1]
		if importPath == "" {
			continue
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
	}

	// --- Exported let variables (props) ---
	for _, m := range reSvelteExportLet.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		line := scriptStartLine + countNewlines(script[:m[0]])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "prop"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Exported const variables ---
	for _, m := range reSvelteExportConst.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		line := scriptStartLine + countNewlines(script[:m[0]])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "prop"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Reactive declarations ($: name = ...) ---
	for _, m := range reSvelteReactive.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		line := scriptStartLine + countNewlines(script[:m[0]])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: false,
			Metadata: map[string]string{"kind": "reactive"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Function declarations ---
	for _, m := range reSvelteFunction.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		scriptLineIdx := countNewlines(script[:m[0]]) // 0-based index in scriptLines
		line := scriptStartLine + scriptLineIdx       // 1-based file line
		// Check if this function line is an export.
		lineText := getLineAt(scriptLines, scriptLineIdx)
		exported := strings.Contains(lineText, "export ")
		// extractLineDoc expects 1-based line number within the lines array.
		doc := extractLineDoc(scriptLines, scriptLineIdx+1, "//")
		nodeID := g.MakeNodeID(filePath, name)
		nodeMeta := buildLangMeta(declMeta{Doc: doc})
		if nodeMeta == nil {
			nodeMeta = map[string]string{}
		}
		nodeMeta["kind"] = "function"
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: nodeMeta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Arrow function declarations ---
	// const/let name = (params) => ... must become NodeFunction, not NodeVariable.
	for _, m := range reSvelteArrowFunc.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		lineText := strings.TrimSpace(script[m[0]:m[1]])
		exported := strings.Contains(lineText, "export ")
		if emitted[name] {
			// If already emitted as NodeVariable (e.g. by export const path), upgrade to NodeFunction.
			nodeID := g.MakeNodeID(filePath, name)
			if existing := g.GetNode(nodeID); existing != nil && existing.Type == graph.NodeVariable {
				existing.Type = graph.NodeFunction
				existing.Exported = exported
				if existing.Metadata == nil {
					existing.Metadata = map[string]string{}
				}
				existing.Metadata["kind"] = "function"
			}
			continue
		}
		emitted[name] = true
		line := scriptStartLine + countNewlines(script[:m[0]])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: exported,
			Metadata: map[string]string{"kind": "function"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Script-level const/let variables (non-exported) ---
	for _, m := range reSvelteVariable.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[4]:m[5]]
		if emitted[name] {
			continue
		}
		// Skip lines that start with "export" — already handled above.
		lineText := strings.TrimSpace(script[m[0]:m[1]])
		if strings.HasPrefix(lineText, "export") {
			continue
		}
		// Skip Svelte 5 rune assignments — handled by dedicated blocks below.
		// m[1] is the position right after "="; check what follows it.
		afterEq := strings.TrimSpace(script[m[1]:])
		if strings.HasPrefix(afterEq, "$state") || strings.HasPrefix(afterEq, "$derived") ||
			strings.HasPrefix(afterEq, "$bindable") || strings.HasPrefix(afterEq, "$props") {
			continue
		}
		emitted[name] = true
		line := scriptStartLine + countNewlines(script[:m[0]])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: false,
			Metadata: map[string]string{"kind": "variable"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Svelte 5: $props() destructuring ---
	// const { x, y, z = default } = $props()
	for _, m := range reSvelteProps5.FindAllStringSubmatchIndex(script, -1) {
		bracesContent := script[m[2]:m[3]]
		line := scriptStartLine + countNewlines(script[:m[0]])
		for _, ident := range extractDestructuredNames(bracesContent) {
			if emitted[ident] {
				continue
			}
			emitted[ident] = true
			nodeID := g.MakeNodeID(filePath, ident)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     ident,
				File:     filePath,
				Line:     line,
				Exported: true, // props are externally visible
				Metadata: map[string]string{"kind": "prop"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	// --- Svelte 5: $state() reactive state ---
	// let x = $state(initialValue)
	for _, m := range reSvelteState5.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		line := scriptStartLine + countNewlines(script[:m[0]])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: false,
			Metadata: map[string]string{"kind": "state"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Svelte 5: $derived() computed value ---
	// const x = $derived(expression)
	for _, m := range reSvelteDerived5.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		line := scriptStartLine + countNewlines(script[:m[0]])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: false,
			Metadata: map[string]string{"kind": "derived"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Svelte 5: $bindable() prop ---
	// const x = $bindable(defaultValue)
	for _, m := range reSvelteBindable5.FindAllStringSubmatchIndex(script, -1) {
		name := script[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		line := scriptStartLine + countNewlines(script[:m[0]])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "prop"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- Component usage in template ---
	// Collect imported Pascal-case names to cross-reference with component usage.
	importedComponents := make(map[string]bool)
	for _, m := range reSvelteImport.FindAllStringSubmatch(script, -1) {
		for _, name := range extractSvelteComponentNames(m[0]) {
			importedComponents[name] = true
		}
	}

	// Find <Component ...> tags in the template (outside <script> and <style>).
	templateContent := stripScriptAndStyle(content)
	seen := make(map[string]bool)
	for _, m := range reSvelteComponent.FindAllStringSubmatch(templateContent, -1) {
		compName := m[1]
		if seen[compName] {
			continue
		}
		seen[compName] = true
		// Only emit CALLS edge if the component was imported.
		if importedComponents[compName] {
			compNodeID := g.MakeNodeID(filePath, compName)
			g.AddCallSite(graph.CallSite{
				CallerID:   fileNodeID,
				CallerFile: filePath,
				FuncName:   compName,
			})
			_ = compNodeID // call site uses FuncName resolution
		}
	}

	return nil
}

// countNewlines counts the number of newline characters in s.
func countNewlines(s string) int {
	return strings.Count(s, "\n")
}

// getLineAt returns the line at a 0-based index from a lines slice, or "".
func getLineAt(lines []string, idx int) string {
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	return lines[idx]
}

// stripScriptAndStyle removes <script>...</script> and <style>...</style>
// blocks from Svelte content, leaving only the template portion.
func stripScriptAndStyle(content string) string {
	reScript := regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>.*?</script>`)
	reStyle := regexp.MustCompile(`(?s)<style(?:\s[^>]*)?>.*?</style>`)
	result := reScript.ReplaceAllString(content, "")
	return reStyle.ReplaceAllString(result, "")
}

// extractSvelteComponentNames extracts all imported Pascal-case identifiers
// from a single import statement match. Handles:
//   - import Button from './Button'
//   - import { Button, Modal } from './ui'
//   - import DefaultComp, { Named } from './comp'
//   - import type { Foo } from './types'  (skipped)
func extractSvelteComponentNames(importMatch string) []string {
	// Skip type-only imports.
	trimmed := strings.TrimSpace(importMatch)
	// Strip "import " prefix.
	after := strings.TrimPrefix(trimmed, "import ")
	after = strings.TrimSpace(after)
	if strings.HasPrefix(after, "type ") {
		return nil // TypeScript type import — no runtime component
	}

	// Find the boundary before 'from' or end of string.
	fromIdx := strings.LastIndex(after, " from ")
	specifier := after
	if fromIdx >= 0 {
		specifier = after[:fromIdx]
	}

	// Normalize: remove braces, asterisks, "as" aliases, commas.
	specifier = strings.NewReplacer(
		"{", " ",
		"}", " ",
		"*", " ",
		",", " ",
	).Replace(specifier)

	var components []string
	for _, word := range strings.Fields(specifier) {
		// Skip the 'as' keyword itself and its alias.
		if word == "as" {
			continue
		}
		// Only Pascal-case identifiers are Svelte components.
		if len(word) > 0 && word[0] >= 'A' && word[0] <= 'Z' {
			components = append(components, word)
		}
	}
	return components
}

// extractDestructuredNames parses the content between braces of a destructuring
// pattern and returns each bound identifier. Handles:
//   - { x, y, z }
//   - { x = defaultVal, y }
//   - { longName: alias, other }
//   - { ...rest }  (rest element — skipped, it's not a simple identifier)
func extractDestructuredNames(braces string) []string {
	var names []string
	// Split on commas; each element is one binding.
	parts := strings.Split(braces, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "...") {
			continue // skip rest/spread elements
		}
		// "key: alias" — take the alias (right side) as the local name.
		if colonIdx := strings.Index(part, ":"); colonIdx >= 0 {
			part = strings.TrimSpace(part[colonIdx+1:])
		}
		// "name = default" — take the name (left side).
		if eqIdx := strings.Index(part, "="); eqIdx >= 0 {
			part = strings.TrimSpace(part[:eqIdx])
		}
		// Must be a valid JS identifier.
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		valid := true
		for i, ch := range part {
			if i == 0 && (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && ch != '_' && ch != '$' {
				valid = false
				break
			}
			if i > 0 && (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '_' && ch != '$' {
				valid = false
				break
			}
		}
		if valid {
			names = append(names, part)
		}
	}
	return names
}
