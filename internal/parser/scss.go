package parser

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// SCSSParser parses SCSS and Sass (.scss, .sass) source files using regex.
//
// Extracts:
//   - @mixin name(params) { } → NodeFunction with kind="mixin"
//   - @function name(params) { } → NodeFunction with kind="function"
//   - $variable: value; → NodeVariable with kind="variable"
//   - %placeholder { } → NodeStruct with kind="placeholder"
//   - Top-level .class, #id, and %placeholder selectors → NodeStruct with kind="selector"
//   - Multiple selectors on one line (.a, .b { }) → each extracted separately
//   - @use / @forward / @import → EdgeImports to NodePackage nodes
//   - @include mixin-name → AddCallSite for cross-file resolution + direct EdgeCalls for same-file mixins
//   - @extend .selector / %placeholder → EdgeCalls to extended node
type SCSSParser struct{}

// NewSCSSParser creates a ready-to-use SCSSParser.
func NewSCSSParser() *SCSSParser {
	return &SCSSParser{}
}

// Extensions returns the file extensions handled by this parser.
func (p *SCSSParser) Extensions() []string {
	return []string{".scss", ".sass"}
}

// Regex patterns for SCSS/Sass constructs.
var (
	// @mixin name or @mixin name($arg, ...)
	reScssM = regexp.MustCompile(`(?m)^\s*@mixin\s+([\w-]+)`)

	// @function name($args)
	reScssFunc = regexp.MustCompile(`(?m)^\s*@function\s+([\w-]+)`)

	// @use 'path', @forward 'path', @import 'path'
	reScssUse = regexp.MustCompile(`(?m)^\s*@(?:use|forward|import)\s+['"]([^'"]+)['"]`)

	// @include mixin-name or @include mixin-name($args)
	reScssInclude = regexp.MustCompile(`(?m)^\s*@include\s+([\w-]+)`)

	// @extend .selector, #id, or %placeholder
	reScssExtend = regexp.MustCompile(`(?m)^\s*@extend\s+((?:\.[\w-]+|#[\w-]+|%[\w-]+))`)

	// Selector line: one or more class/id/placeholder selectors before an opening brace.
	// Handles: .a { }, #b { }, %c { }, .a, .b { }, .a, #b, %c { }
	// Anchored to line start to avoid matching nested selectors inside mixin/function bodies.
	reScssSelector = regexp.MustCompile(`(?m)^((?:[\.#%][\w-]+)(?:\s*,\s*[\.#%][\w-]+)*)\s*\{`)
)

// Parse extracts code entities from a single SCSS/Sass file and merges them into the graph.
func (p *SCSSParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	content := string(src)

	// --- @mixin definitions ---
	for _, m := range reScssM.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			continue
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "mixin"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- @function definitions ---
	for _, m := range reScssFunc.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			continue
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "function"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- $variable declarations (top-level only, depth==0) ---
	for _, v := range scssExtractTopLevelVars(content) {
		name := "$" + v.name
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			continue
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     v.line,
			Exported: false,
			Metadata: map[string]string{"kind": "variable"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// --- @use / @forward / @import ---
	for _, m := range reScssUse.FindAllStringSubmatch(content, -1) {
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

	// --- @include calls → call sites for cross-file resolution ---
	for _, m := range reScssInclude.FindAllStringSubmatch(content, -1) {
		mixinName := m[1]
		if mixinName == "" {
			continue
		}
		// Register as a call site so the resolver can find cross-file mixin definitions.
		g.AddCallSite(graph.CallSite{
			CallerID:   fileNodeID,
			CallerFile: filePath,
			FuncName:   mixinName,
		})
		// If the mixin is defined in THIS file, also create a direct edge immediately.
		mixinNodeID := g.MakeNodeID(filePath, mixinName)
		if g.GetNode(mixinNodeID) != nil {
			g.AddEdge(&graph.Edge{From: fileNodeID, To: mixinNodeID, Type: graph.EdgeCalls})
		}
	}

	// --- Selectors: .class, #id, %placeholder (including multi-selector lines) ---
	emittedSelectors := make(map[string]bool)
	for _, m := range reScssSelector.FindAllStringSubmatchIndex(content, -1) {
		selectorGroup := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1

		// Split on commas to handle ".a, .b, %c { }" correctly.
		for _, sel := range strings.Split(selectorGroup, ",") {
			name := strings.TrimSpace(sel)
			if name == "" || emittedSelectors[name] {
				continue
			}
			emittedSelectors[name] = true

			// Determine kind: %placeholder vs selector.
			kind := "selector"
			if strings.HasPrefix(name, "%") {
				kind = "placeholder"
			}

			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) != nil {
				continue
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     line,
				Exported: true,
				Metadata: map[string]string{"kind": kind},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	// --- @extend edges: @extend .selector or @extend %placeholder ---
	for _, m := range reScssExtend.FindAllStringSubmatch(content, -1) {
		targetName := m[1]
		if targetName == "" {
			continue
		}
		// Create EdgeCalls from file to the extended selector/placeholder.
		// If the target is defined in this file, the edge resolves immediately.
		// If it's from an imported file, it will be resolved by the call-site resolver.
		targetID := g.MakeNodeID(filePath, targetName)
		if g.GetNode(targetID) != nil {
			g.AddEdge(&graph.Edge{From: fileNodeID, To: targetID, Type: graph.EdgeCalls})
		} else {
			// Queue for cross-file resolution.
			g.AddCallSite(graph.CallSite{
				CallerID:   fileNodeID,
				CallerFile: filePath,
				FuncName:   targetName,
			})
		}
	}

	return nil
}

// scssVarResult holds a top-level SCSS variable name and its line number.
type scssVarResult struct {
	name string
	line int
}

// scssExtractTopLevelVars scans SCSS/Sass content and returns only the
// $variable declarations that are at brace-depth 0 (top-level scope).
// Variables inside @mixin, @function, or selector bodies are excluded.
func scssExtractTopLevelVars(content string) []scssVarResult {
	var results []scssVarResult
	depth := 0      // curly-brace depth
	parenDepth := 0 // parenthesis depth — skip $params inside @mixin/@function signatures
	i := 0
	n := len(content)
	lineNum := 1

	for i < n {
		ch := content[i]
		switch ch {
		case '\n':
			lineNum++
			i++
		case '(':
			parenDepth++
			i++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
			i++
		case '{':
			depth++
			i++
		case '}':
			if depth > 0 {
				depth--
			}
			i++
		case '/':
			// Skip line comments (//) and block comments (/* ... */).
			if i+1 < n && content[i+1] == '/' {
				for i < n && content[i] != '\n' {
					i++
				}
			} else if i+1 < n && content[i+1] == '*' {
				i += 2
				for i+1 < n {
					if content[i] == '*' && content[i+1] == '/' {
						i += 2
						break
					}
					if content[i] == '\n' {
						lineNum++
					}
					i++
				}
			} else {
				i++
			}
		case '"', '\'':
			quote := ch
			i++
			for i < n && content[i] != quote {
				if content[i] == '\\' {
					i++
				}
				if content[i] == '\n' {
					lineNum++
				}
				i++
			}
			i++ // closing quote
		case '$':
			if depth == 0 && parenDepth == 0 {
				j := i + 1
				for j < n && isScssNameChar(content[j]) {
					j++
				}
				if j > i+1 {
					k := j
					for k < n && (content[k] == ' ' || content[k] == '\t') {
						k++
					}
					if k < n && content[k] == ':' {
						if k+1 >= n || content[k+1] != ':' {
							varName := content[i+1 : j]
							results = append(results, scssVarResult{name: varName, line: lineNum})
						}
					}
				}
			}
			i++
		default:
			i++
		}
	}
	return results
}

// isScssNameChar returns true for characters valid in SCSS variable names.
func isScssNameChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') ||
		(ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') ||
		ch == '_' || ch == '-'
}
