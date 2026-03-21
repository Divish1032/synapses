package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/css"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// CSSParser parses CSS (.css) source files using tree-sitter.
type CSSParser struct {
	language *sitter.Language
}

// NewCSSParser creates a ready-to-use CSSParser.
func NewCSSParser() *CSSParser {
	return &CSSParser{language: sitter.NewLanguage(css.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *CSSParser) Extensions() []string {
	return []string{".css"}
}

func (p *CSSParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single CSS file and merges them into the graph.
//
// Extracts:
//   - @import rules → EdgeImports to NodePackage nodes
//   - Rule-set selectors (.class, #id) → NodeStruct with kind="selector"
//   - @keyframes definitions → NodeFunction with kind="keyframes"
//   - CSS custom property definitions (--var) → NodeVariable with kind="custom-property"
//   - @font-face font-family names → NodeVariable with kind="font-face"
//   - animation / animation-name references → EdgeCalls to keyframes nodes
//   - var(--name) references → EdgeCalls to custom-property nodes
//   - Selectors inside @media, @supports, @layer are fully handled (recursive walk)
func (p *CSSParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// Walk the entire AST. The recursive walk handles nesting inside
	// @media, @supports, @layer and any other at-rule blocks automatically.
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}

		switch n.Type() {
		case "import_statement":
			extractCSSImport(g, n, src, filePath, fileNodeID)

		case "rule_set":
			extractCSSRuleSet(g, n, src, filePath, fileNodeID)

		case "keyframes_statement":
			extractCSSKeyframes(g, n, src, filePath, fileNodeID)

		case "declaration":
			extractCSSDeclaration(g, n, src, filePath, fileNodeID)

		case "at_rule":
			extractCSSAtRule(g, n, src, filePath, fileNodeID)
		}

		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)

	return nil
}

// extractCSSRuleSet handles a rule_set node, extracting .class and #id selectors.
// Multiple selectors on one line (.a, .b { }) are both extracted.
// Selectors nested inside @media, @supports, and @layer blocks are found
// automatically by the recursive walk.
func extractCSSRuleSet(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	selectorsNode := firstChildOfType(n, "selectors")
	if selectorsNode.IsNull() {
		return
	}
	line := int(n.StartPoint().Row) + 1

	for i := uint32(0); i < selectorsNode.ChildCount(); i++ {
		child := selectorsNode.Child(i)
		if child.IsNull() {
			continue
		}
		var name string
		switch child.Type() {
		case "class_selector":
			// Full text includes the leading dot: ".button"
			name = strings.TrimSpace(childText(child, src))
		case "id_selector":
			// Full text includes the leading hash: "#header"
			name = strings.TrimSpace(childText(child, src))
		default:
			// Skip pseudo_class_selector, attribute_selector, tag_name (element noise).
			continue
		}
		if name == "" {
			continue
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
			Metadata: map[string]string{"kind": "selector"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// extractCSSImport handles an import_statement node, creating an import edge.
// Supports both @import "file.css" and @import url("file.css").
func extractCSSImport(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var importPath string

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "string_value":
			importPath = stripCSSQuotes(childText(child, src))
		case "call_expression":
			importPath = extractCSSURLArg(child, src)
		}
	}

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
}

// extractCSSKeyframes handles a keyframes_statement node, creating a NodeFunction.
func extractCSSKeyframes(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var name string

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "keyframes_name", "plain_value", "identifier":
			name = strings.TrimSpace(childText(child, src))
		}
	}

	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: map[string]string{"kind": "keyframes"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractCSSDeclaration handles a declaration node. It covers three cases:
//  1. Custom property definition (--var: value) → NodeVariable kind="custom-property"
//  2. animation / animation-name reference → EdgeCalls to keyframes node
//  3. var(--name) usage in any property value → EdgeCalls to custom-property node
func extractCSSDeclaration(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var propName string
	propLine := int(n.StartPoint().Row) + 1

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "property_name" {
			propName = strings.TrimSpace(childText(child, src))
			break
		}
	}

	// 1. Custom property definition.
	if strings.HasPrefix(propName, "--") {
		nodeID := g.MakeNodeID(filePath, propName)
		if g.GetNode(nodeID) == nil {
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     propName,
				File:     filePath,
				Line:     propLine,
				Exported: true,
				Metadata: map[string]string{"kind": "custom-property"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
		// Still fall through to check for var() in the value side of custom properties.
	}

	// 2. Animation reference: the first plain_value after ":" is the keyframes name.
	propLower := strings.ToLower(propName)
	if propLower == "animation" || propLower == "animation-name" {
		seenColon := false
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if child.IsNull() {
				continue
			}
			if child.Type() == ":" {
				seenColon = true
				continue
			}
			if seenColon && child.Type() == "plain_value" {
				animName := strings.TrimSpace(childText(child, src))
				if animName != "" && !isCSSKeyword(animName) {
					kfID := g.MakeNodeID(filePath, animName)
					if g.GetNode(kfID) != nil {
						g.AddEdge(&graph.Edge{From: fileNodeID, To: kfID, Type: graph.EdgeCalls})
					}
				}
				break // only the first plain_value is the animation name
			}
		}
	}

	// 3. var(--name) usages anywhere in the declaration value.
	extractCSSVarRefs(g, n, src, filePath, fileNodeID)
}

// extractCSSVarRefs walks a node's children looking for call_expression nodes
// where function_name == "var" and creates EdgeCalls to the referenced custom property.
func extractCSSVarRefs(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	if n.IsNull() {
		return
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() != "call_expression" {
			continue
		}
		fnNode := firstChildOfType(child, "function_name")
		if fnNode.IsNull() {
			continue
		}
		if strings.ToLower(strings.TrimSpace(childText(fnNode, src))) != "var" {
			continue
		}
		argsNode := firstChildOfType(child, "arguments")
		if argsNode.IsNull() {
			continue
		}
		varRef := strings.Trim(strings.TrimSpace(childText(argsNode, src)), "()")
		varRef = strings.TrimSpace(varRef)
		// var(--name, fallback) → take only the part before the comma.
		if idx := strings.Index(varRef, ","); idx >= 0 {
			varRef = strings.TrimSpace(varRef[:idx])
		}
		if !strings.HasPrefix(varRef, "--") {
			continue
		}
		varNodeID := g.MakeNodeID(filePath, varRef)
		if g.GetNode(varNodeID) != nil {
			g.AddEdge(&graph.Edge{From: fileNodeID, To: varNodeID, Type: graph.EdgeCalls})
		}
	}
}

// extractCSSAtRule handles generic @-rules.
// Currently handles @font-face (extracts font-family name as NodeVariable).
func extractCSSAtRule(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	var atKeyword string
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "at_keyword" {
			atKeyword = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(childText(child, src)), "@"))
			break
		}
	}

	if atKeyword != "font-face" {
		return
	}

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() != "block" && child.Type() != "stylesheet" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			decl := child.Child(j)
			if decl.IsNull() || decl.Type() != "declaration" {
				continue
			}
			propName := ""
			var propValue string
			for k := uint32(0); k < decl.ChildCount(); k++ {
				dChild := decl.Child(k)
				if dChild.IsNull() {
					continue
				}
				switch dChild.Type() {
				case "property_name":
					propName = strings.ToLower(strings.TrimSpace(childText(dChild, src)))
				case "string_value", "plain_value":
					if propValue == "" {
						propValue = stripCSSQuotes(childText(dChild, src))
					}
				}
			}
			if propName == "font-family" && propValue != "" {
				nodeID := g.MakeNodeID(filePath, propValue)
				if g.GetNode(nodeID) != nil {
					return
				}
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeVariable,
					Name:     propValue,
					File:     filePath,
					Line:     int(n.StartPoint().Row) + 1,
					Exported: true,
					Metadata: map[string]string{"kind": "font-face"},
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				return
			}
		}
	}
}

// isCSSKeyword returns true for CSS animation timing and global keywords that
// should not be treated as keyframes names.
func isCSSKeyword(s string) bool {
	switch s {
	case "none", "inherit", "initial", "unset", "revert", "revert-layer",
		"ease", "linear", "ease-in", "ease-out", "ease-in-out", "step-start", "step-end",
		"normal", "reverse", "alternate", "alternate-reverse",
		"forwards", "backwards", "both",
		"running", "paused",
		"infinite":
		return true
	}
	return false
}

// stripCSSQuotes removes surrounding single or double quotes from a string.
func stripCSSQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// extractCSSURLArg extracts the path from a url() call expression.
func extractCSSURLArg(callNode sitter.Node, src []byte) string {
	if callNode.IsNull() {
		return ""
	}
	if args := firstChildOfType(callNode, "arguments"); !args.IsNull() {
		for i := uint32(0); i < args.ChildCount(); i++ {
			child := args.Child(i)
			if child.IsNull() {
				continue
			}
			if child.Type() == "string_value" {
				return stripCSSQuotes(childText(child, src))
			}
			if child.Type() == "plain_value" {
				return strings.TrimSpace(childText(child, src))
			}
		}
	}

	for i := uint32(0); i < callNode.ChildCount(); i++ {
		child := callNode.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "string_value" {
			return stripCSSQuotes(childText(child, src))
		}
		if child.Type() == "plain_value" {
			return strings.TrimSpace(childText(child, src))
		}
	}

	raw := childText(callNode, src)
	if idx := strings.Index(raw, "("); idx >= 0 {
		if end := strings.LastIndex(raw, ")"); end > idx {
			inner := strings.TrimSpace(raw[idx+1 : end])
			return stripCSSQuotes(inner)
		}
	}
	return ""
}
