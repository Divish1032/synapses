package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	tomlg "github.com/alexaandru/go-sitter-forest/toml"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TOMLParser parses TOML (.toml) configuration files.
// It extracts sections (tables), array-of-tables, and key-value pairs
// into graph nodes. Special handling is provided for dependency sections
// commonly found in Cargo.toml, pyproject.toml, etc.
type TOMLParser struct {
	language *sitter.Language
}

// NewTOMLParser creates a ready-to-use TOMLParser.
func NewTOMLParser() *TOMLParser {
	return &TOMLParser{language: sitter.NewLanguage(tomlg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *TOMLParser) Extensions() []string {
	return []string{".toml"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *TOMLParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single TOML file and merges them
// into the graph.
//
// Extraction rules:
//   - [section] headers → NodeStruct with kind=table
//   - [[array]] headers → NodeStruct with kind=array_table
//   - Top-level pair nodes (before any section) → NodeField with kind=field
//   - For [dependencies] / [dev-dependencies] / [build-dependencies]: each pair
//     also emits a NodeField with kind=dependency
//   - Dotted keys like [profile.release] → name = "profile.release"
func (p *TOMLParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
	if tree == nil {
		return nil
	}
	root := tree.RootNode()
	if root.IsNull() {
		return nil
	}

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

	// Walk the document's direct children.
	// The TOML grammar root is "document". Its children are:
	//   - "table"              → [section]
	//   - "table_array_element" → [[array_section]]
	//   - "pair"               → top-level key = value
	//   - "comment"            → ignored
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "table":
			p.handleTable(g, child, src, filePath, fileNodeID)
		case "table_array_element":
			p.handleTableArrayElement(g, child, src, filePath, fileNodeID)
		case "pair":
			p.handleTopLevelPair(g, child, src, filePath, fileNodeID)
		}
	}

	return nil
}

// handleTable processes a TOML [section] table header and its contained pairs.
// Emits a NodeStruct with kind=table and connects pairs as NodeField children.
func (p *TOMLParser) handleTable(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	sectionName := tomlExtractTableName(node, src)
	if sectionName == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	tableNodeID := g.MakeNodeID(filePath, "table:"+sectionName)
	g.AddNode(&graph.Node{
		ID:       tableNodeID,
		Type:     graph.NodeStruct,
		Name:     sectionName,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: map[string]string{
			"kind": "table",
		},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: tableNodeID, Type: graph.EdgeDefines})

	// Determine if this is a dependency section.
	isDep := tomlIsDependencySection(sectionName)

	// Extract pair children inside the table body.
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() || child.Type() != "pair" {
			continue
		}
		p.handlePairInSection(g, child, src, filePath, "table:"+sectionName, tableNodeID, isDep)
	}
}

// handleTableArrayElement processes a TOML [[array]] element.
// Emits a NodeStruct with kind=array_table.
func (p *TOMLParser) handleTableArrayElement(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// The array name is inside a "table_array_element_opening" or directly
	// as children with the array name between [[ and ]].
	// We look for the same key types as in table headers.
	arrayName := tomlExtractTableArrayName(node, src)
	if arrayName == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1

	// Use line number for uniqueness since multiple [[array]] entries with the same
	// name are valid TOML (they form an array of tables).
	tableNodeID := g.MakeNodeID(filePath, "array_table:"+arrayName+"@L"+tomlIntStr(startLine))
	g.AddNode(&graph.Node{
		ID:       tableNodeID,
		Type:     graph.NodeStruct,
		Name:     arrayName,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: map[string]string{
			"kind": "array_table",
		},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: tableNodeID, Type: graph.EdgeDefines})

	// Extract pair children inside the array table element.
	sectionKey := "array_table:" + arrayName + "@L" + tomlIntStr(startLine)
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() || child.Type() != "pair" {
			continue
		}
		p.handlePairInSection(g, child, src, filePath, sectionKey, tableNodeID, false)
	}
}

// handleTopLevelPair processes a pair at the document root level (before any section).
// Emits a NodeField with kind=field.
func (p *TOMLParser) handleTopLevelPair(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	keyName := tomlExtractPairKey(node, src)
	if keyName == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	valStr := tomlExtractPairValue(node, src)

	meta := map[string]string{"kind": "field"}
	if valStr != "" {
		meta["value"] = valStr
	}

	fieldNodeID := g.MakeNodeID(filePath, "field:"+keyName)
	g.AddNode(&graph.Node{
		ID:       fieldNodeID,
		Type:     graph.NodeVariable,
		Name:     keyName,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
}

// handlePairInSection processes a pair inside a table section.
// sectionKey is the human-readable section name used for constructing unique node IDs.
// sectionNodeID is the graph NodeID of the parent table for edge creation.
// Emits a NodeField with kind=field or kind=dependency (for dep sections).
func (p *TOMLParser) handlePairInSection(
	g *graph.Graph,
	node sitter.Node,
	src []byte,
	filePath string,
	sectionKey string,
	sectionNodeID graph.NodeID,
	isDep bool,
) {
	keyName := tomlExtractPairKey(node, src)
	if keyName == "" {
		return
	}

	startLine := int(node.StartPoint().Row) + 1
	valStr := tomlExtractPairValue(node, src)

	kind := "field"
	if isDep {
		kind = "dependency"
	}

	meta := map[string]string{"kind": kind}
	if valStr != "" {
		meta["value"] = valStr
	}

	// Use sectionKey prefix for uniqueness within the file.
	fieldNodeID := g.MakeNodeID(filePath, sectionKey+":pair:"+keyName)
	g.AddNode(&graph.Node{
		ID:       fieldNodeID,
		Type:     graph.NodeVariable,
		Name:     keyName,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: sectionNodeID, To: fieldNodeID, Type: graph.EdgeContains})
}

// tomlExtractTableName extracts the section name from a TOML table node.
// A table node has children: "[", key, "]"
// The key can be bare_key, quoted_key, or dotted_key.
func tomlExtractTableName(node sitter.Node, src []byte) string {
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "bare_key":
			return childText(child, src)
		case "quoted_key":
			return tomlStripQuotes(childText(child, src))
		case "dotted_key":
			return tomlExtractDottedKey(child, src)
		}
	}
	return ""
}

// tomlExtractTableArrayName extracts the array name from a table_array_element node.
// Similar to table name extraction — the key appears between [[ and ]].
func tomlExtractTableArrayName(node sitter.Node, src []byte) string {
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "bare_key":
			return childText(child, src)
		case "quoted_key":
			return tomlStripQuotes(childText(child, src))
		case "dotted_key":
			return tomlExtractDottedKey(child, src)
		}
	}
	return ""
}

// tomlExtractPairKey extracts the key name from a TOML pair node.
// A pair has: key "=" value, where key is bare_key, quoted_key, or dotted_key.
func tomlExtractPairKey(node sitter.Node, src []byte) string {
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "bare_key":
			return childText(child, src)
		case "quoted_key":
			return tomlStripQuotes(childText(child, src))
		case "dotted_key":
			return tomlExtractDottedKey(child, src)
		}
	}
	return ""
}

// tomlExtractPairValue extracts the value from a TOML pair node as a string.
// Used for metadata storage. Truncates large values (arrays, inline tables) to 60 chars.
func tomlExtractPairValue(node sitter.Node, src []byte) string {
	// Walk children looking for the value (skip key nodes and "=").
	pastEquals := false
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		if ct == "bare_key" || ct == "quoted_key" || ct == "dotted_key" {
			continue
		}
		if ct == "=" {
			pastEquals = true
			continue
		}
		if !pastEquals {
			continue
		}
		// This is the value node.
		raw := strings.TrimSpace(childText(child, src))
		switch ct {
		case "string":
			return tomlStripQuotes(raw)
		case "integer", "float", "boolean":
			return raw
		case "array":
			if len(raw) > 60 {
				return raw[:60] + "..."
			}
			return raw
		case "inline_table":
			if len(raw) > 60 {
				return raw[:60] + "..."
			}
			return raw
		default:
			if len(raw) > 60 {
				return raw[:60] + "..."
			}
			return raw
		}
	}
	return ""
}

// tomlExtractDottedKey joins all bare_key/quoted_key children of a dotted_key node with ".".
// Example: dotted_key "profile.release" → "profile.release"
func tomlExtractDottedKey(node sitter.Node, src []byte) string {
	var parts []string
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "bare_key":
			parts = append(parts, childText(child, src))
		case "quoted_key":
			parts = append(parts, tomlStripQuotes(childText(child, src)))
		case "dotted_key":
			// Recurse into nested dotted_key (3+ part keys nest left-recursively).
			parts = append(parts, tomlExtractDottedKey(child, src))
		}
	}
	return strings.Join(parts, ".")
}

// tomlStripQuotes removes surrounding single or double quotes from a TOML string value.
func tomlStripQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return s
	}
	if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// tomlIsDependencySection returns true if a section name represents a dependency table.
// Matches [dependencies], [dev-dependencies], [build-dependencies], and similar patterns.
func tomlIsDependencySection(name string) bool {
	lower := strings.ToLower(name)
	return lower == "dependencies" ||
		lower == "dev-dependencies" ||
		lower == "build-dependencies" ||
		lower == "dev_dependencies" ||
		lower == "build_dependencies" ||
		strings.HasSuffix(lower, ".dependencies") ||
		strings.HasSuffix(lower, ".dev-dependencies") ||
		strings.HasSuffix(lower, ".build-dependencies")
}

// tomlIntStr converts an int to a string without importing strconv in this file.
// Uses the strconv-like approach inline.
func tomlIntStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
