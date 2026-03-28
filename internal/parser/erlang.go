package parser

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ErlangParser parses Erlang (.erl, .hrl) source files.
// Used in telecom, real-time distributed systems, and high-availability platforms.
// Uses regex-based extraction.
type ErlangParser struct{}

// NewErlangParser creates a ready-to-use ErlangParser.
func NewErlangParser() *ErlangParser { return &ErlangParser{} }

// Extensions returns the file extensions handled by this parser.
func (p *ErlangParser) Extensions() []string {
	return []string{".erl", ".hrl"}
}

// Erlang regex patterns.
var (
	// -module(mymodule).
	reErlModule = regexp.MustCompile(`(?m)^-module\((\w+)\)\.`)

	// -behaviour(gen_server).  or  -behavior(gen_server).
	reErlBehaviour = regexp.MustCompile(`(?m)^-behavi(?:ou)?r\((\w+)\)\.`)

	// -export([func/arity, ...]).  — may span multiple lines
	// We capture the full bracket content with a lazy (?s) match.
	reErlExport = regexp.MustCompile(`(?s)-export\(\[([^\]]*)\]\)`)

	// Function name AND arity from export list entry: funcName/arity
	// Group 1: function name, Group 2: arity (as string)
	reErlExportEntry = regexp.MustCompile(`(\w+)/(\d+)`)

	// Function clause at start of line: funcName(Args...) ->
	// Must start with a lowercase letter (Erlang convention).
	reErlFunction = regexp.MustCompile(`(?m)^([a-z_]\w*)\s*\(`)

	// -record(person, {name, age}).
	reErlRecord = regexp.MustCompile(`(?m)^-record\((\w+)\s*,`)

	// -type mytype() :: ...
	reErlType = regexp.MustCompile(`(?m)^-type\s+(\w+)\s*\(`)

	// -opaque handle() :: ...
	reErlOpaque = regexp.MustCompile(`(?m)^-opaque\s+(\w+)\s*\(`)

	// -import(lists, [map/2, filter/2]).
	reErlImport = regexp.MustCompile(`(?s)-import\((\w+)\s*,\s*\[([^\]]*)\]\)`)

	// -include("header.hrl").
	reErlInclude = regexp.MustCompile(`(?m)^-include\("([^"]+)"\)`)

	// -include_lib("stdlib/include/assert.hrl").
	reErlIncludeLib = regexp.MustCompile(`(?m)^-include_lib\("([^"]+)"\)`)

	// -spec funcName(ArgType1, ArgType2) -> RetType.
	// Group 1: function name, Group 2: full spec text after name (one line)
	reErlSpec = regexp.MustCompile(`(?m)^-spec\s+(\w+)\s*\(([^\n]*)`)

	// -callback name(Args) -> RetType.
	// Group 1: callback name, Group 2: arity
	reErlCallback = regexp.MustCompile(`(?m)^-callback\s+(\w+)\s*/(\d+)\.`)

	// -callback name(ArgTypes) -> RetType. (alternative form with type signatures)
	reErlCallbackSig = regexp.MustCompile(`(?m)^-callback\s+(\w+)\s*\(([^\n]*)`)
)

// Parse extracts code entities from a single Erlang file and merges them into g.
func (p *ErlangParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// ── Pass 1: collect all exported function names ──────────────────────────
	// exported maps "funcName/arity" → true for all exported function/arity pairs.
	exported := make(map[string]bool)
	// exportedNames maps just the function name → true (for backward-compat checks).
	exportedNames := make(map[string]bool)
	for _, m := range reErlExport.FindAllStringSubmatch(content, -1) {
		list := m[1]
		for _, entry := range reErlExportEntry.FindAllStringSubmatch(list, -1) {
			name := entry[1]
			arity := entry[2]
			exported[name+"/"+arity] = true
			exportedNames[name] = true
		}
	}

	// ── Module attribute ─────────────────────────────────────────────────────
	var moduleName string
	var behaviours []string

	if m := reErlModule.FindStringSubmatchIndex(content); m != nil {
		name := content[m[2]:m[3]]
		moduleName = name
		line := strings.Count(content[:m[0]], "\n") + 1
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:      nodeID,
			Type:    graph.NodePackage,
			Name:    name,
			Package: name,
			File:    filePath,
			Line:    line,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── Behaviour attribute ──────────────────────────────────────────────────
	for _, m := range reErlBehaviour.FindAllStringSubmatchIndex(content, -1) {
		behaviourName := content[m[2]:m[3]]
		behaviours = append(behaviours, behaviourName)
		// Also emit a NodeVariable for the behaviour so agents can find it by name.
		behLine := strings.Count(content[:m[0]], "\n") + 1
		behNodeID := g.MakeNodeID(filePath, "behaviour:"+behaviourName)
		if g.GetNode(behNodeID) == nil {
			g.AddNode(&graph.Node{
				ID:       behNodeID,
				Type:     graph.NodeVariable,
				Name:     behaviourName,
				File:     filePath,
				Line:     behLine,
				Metadata: map[string]string{"kind": "otp_behaviour"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: behNodeID, Type: graph.EdgeDefines})
		}
	}

	// If we found behaviours, annotate the module package node.
	if len(behaviours) > 0 && moduleName != "" {
		moduleNodeID := g.MakeNodeID(filePath, moduleName)
		if modNode := g.GetNode(moduleNodeID); modNode != nil {
			if modNode.Metadata == nil {
				modNode.Metadata = make(map[string]string)
			}
			modNode.Metadata["behaviours"] = strings.Join(behaviours, ",")
		}
	}

	// ── Function clauses ─────────────────────────────────────────────────────
	seen := make(map[string]bool)
	for _, m := range reErlFunction.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		// Filter out attribute keywords that match the function regex.
		if isErlangKeyword(name) {
			continue
		}
		lineStart := strings.LastIndex(content[:m[0]], "\n") + 1
		lineText := content[lineStart:m[1]]
		_ = lineText

		// Compute arity from the argument list immediately following the match.
		// m[1] is the position right after the opening '(' of the arg list.
		arity := erlangArgArity(content, m[1])
		var nodeName string
		if arity >= 0 {
			nodeName = fmt.Sprintf("%s/%d", name, arity)
		} else {
			nodeName = name
		}

		if seen[nodeName] {
			continue
		}
		seen[nodeName] = true

		line := strings.Count(content[:m[0]], "\n") + 1
		doc := extractErlangDoc(lines, line)

		isExported := exported[nodeName] || (arity < 0 && exportedNames[name])

		nodeID := g.MakeNodeID(filePath, nodeName)
		meta := declMeta{Doc: doc}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     nodeName,
			File:     filePath,
			Line:     line,
			Exported: isExported,
			Metadata: buildLangMeta(meta),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── Type specs (-spec) ───────────────────────────────────────────────────
	// Collect -spec attributes and attach them as metadata to existing function nodes.
	// -spec are processed AFTER function nodes are created so we can find the node.
	for _, m := range reErlSpec.FindAllStringSubmatchIndex(content, -1) {
		funcName := content[m[2]:m[3]]
		specText := strings.TrimSpace(content[m[4]:m[5]])
		// The spec applies to all arities of funcName — find any matching node.
		// Try common arities 0-9 first, then fall back to name-only.
		attached := false
		for a := 0; a <= 9; a++ {
			nodeID := g.MakeNodeID(filePath, fmt.Sprintf("%s/%d", funcName, a))
			if node := g.GetNode(nodeID); node != nil {
				if node.Metadata == nil {
					node.Metadata = make(map[string]string)
				}
				node.Metadata["spec"] = funcName + "(" + specText
				attached = true
				break
			}
		}
		// Fallback: try name-only node (for unexported functions where arity wasn't determined).
		if !attached {
			nodeID := g.MakeNodeID(filePath, funcName)
			if node := g.GetNode(nodeID); node != nil {
				if node.Metadata == nil {
					node.Metadata = make(map[string]string)
				}
				node.Metadata["spec"] = funcName + "(" + specText
			}
		}
	}

	// ── Callback declarations (-callback) ────────────────────────────────────
	// -callback defines functions that OTP behaviour implementors must provide.
	// Emit as NodeFunction with kind=callback; these are always "exported" in the
	// sense that implementing modules must export them.
	callbackSeen := make(map[string]bool)
	for _, m := range reErlCallbackSig.FindAllStringSubmatchIndex(content, -1) {
		cbName := content[m[2]:m[3]]
		sigText := strings.TrimSpace(content[m[4]:m[5]])
		// Count arity from the sig text (same arg-counting logic).
		// sigText starts with the args, so prepend a '(' to reuse erlangArgArity.
		arity := erlangArgArity("("+sigText, 1)
		var nodeName string
		if arity >= 0 {
			nodeName = fmt.Sprintf("%s/%d", cbName, arity)
		} else {
			nodeName = cbName
		}
		if callbackSeen[nodeName] {
			continue
		}
		callbackSeen[nodeName] = true
		line := strings.Count(content[:m[0]], "\n") + 1
		nodeID := g.MakeNodeID(filePath, nodeName)
		if g.GetNode(nodeID) != nil {
			// Function node already exists (e.g. the module implements its own callback).
			// Just annotate it.
			if node := g.GetNode(nodeID); node != nil && node.Metadata != nil {
				node.Metadata["kind"] = "callback"
			}
			continue
		}
		g.AddNode(&graph.Node{
			ID:   nodeID,
			Type: graph.NodeFunction,
			Name: nodeName,
			File: filePath,
			Line: line,
			// Callbacks are always exported — implementing modules must export them.
			Exported: true,
			Metadata: map[string]string{
				"kind": "callback",
				"spec": cbName + "(" + sigText,
			},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── Record definitions ───────────────────────────────────────────────────
	for _, m := range reErlRecord.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "record"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── Type definitions ─────────────────────────────────────────────────────
	for _, m := range reErlType.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1
		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			continue // already defined (e.g. collision with record of same name)
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: true,
			Metadata: map[string]string{"kind": "type"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── Opaque type definitions ──────────────────────────────────────────────
	for _, m := range reErlOpaque.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1
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
			Metadata: map[string]string{"kind": "opaque"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── Import from other modules ─────────────────────────────────────────────
	for _, m := range reErlImport.FindAllStringSubmatchIndex(content, -1) {
		modName := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1
		importNodeID := g.MakeNodeID(filePath, "import:"+modName)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    modName,
			Package: modName,
			File:    filePath,
			Line:    line,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}

	// ── Include files ────────────────────────────────────────────────────────
	for _, m := range reErlInclude.FindAllStringSubmatchIndex(content, -1) {
		path := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1
		includeNodeID := g.MakeNodeID(path, path)
		g.AddNode(&graph.Node{
			ID:      includeNodeID,
			Type:    graph.NodePackage,
			Name:    path,
			Package: path,
			File:    filePath,
			Line:    line,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: includeNodeID, Type: graph.EdgeImports})
	}

	// ── Include lib files ────────────────────────────────────────────────────
	for _, m := range reErlIncludeLib.FindAllStringSubmatchIndex(content, -1) {
		path := content[m[2]:m[3]]
		line := strings.Count(content[:m[0]], "\n") + 1
		includeNodeID := g.MakeNodeID(path, path)
		g.AddNode(&graph.Node{
			ID:      includeNodeID,
			Type:    graph.NodePackage,
			Name:    path,
			Package: path,
			File:    filePath,
			Line:    line,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: includeNodeID, Type: graph.EdgeImports})
	}

	return nil
}

// extractErlangDoc collects contiguous %% comment lines immediately preceding
// the function at the given 1-indexed line number. It prioritises lines that
// contain @doc (EDoc format) but falls back to any %% lines.
func extractErlangDoc(lines []string, funcLine int) string {
	if funcLine < 2 {
		return ""
	}
	var parts []string
	for i := funcLine - 2; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "%%") {
			text := strings.TrimPrefix(trimmed, "%%")
			text = strings.TrimSpace(text)
			parts = append([]string{text}, parts...)
		} else {
			break
		}
	}
	return strings.Join(parts, " ")
}

// erlangArgArity counts the number of arguments in an Erlang function clause
// by scanning the content starting at the position just after the opening paren
// of the function clause. Returns -1 if the arity cannot be determined.
func erlangArgArity(content string, afterOpenParen int) int {
	if afterOpenParen >= len(content) {
		return -1
	}
	// We are inside the arg list (after the first '(').
	// Count commas at depth 0 to determine arity.
	// Handle all bracket types: (), {}, [] can nest inside args.
	depth := 0
	hasContent := false
	commas := 0
	for i := afterOpenParen; i < len(content); i++ {
		ch := content[i]
		switch ch {
		case '(', '{', '[':
			depth++
			hasContent = true
		case ')', '}', ']':
			if depth == 0 && ch == ')' {
				// Closing the top-level arg list.
				if !hasContent {
					return 0 // zero-arity
				}
				return commas + 1
			}
			depth--
			hasContent = true
		case ',':
			if depth == 0 {
				commas++
				hasContent = true
			}
		case '%':
			// Erlang comment — skip to end of line.
			for i < len(content) && content[i] != '\n' {
				i++
			}
		case '\n':
			// Multi-line function heads are valid; continue scanning.
		case ' ', '\t':
			// Whitespace — ignore.
		default:
			if depth == 0 {
				hasContent = true
			}
		}
	}
	return -1
}

// isErlangKeyword returns true for Erlang reserved words that can appear at
// the beginning of a line but are not function definitions.
func isErlangKeyword(name string) bool {
	switch name {
	case "if", "case", "receive", "begin", "end", "when", "after",
		"fun", "try", "catch", "of", "query":
		return true
	}
	return false
}
