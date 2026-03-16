package parser

import (
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

// declMeta holds the enriched metadata extracted for any declaration node.
// It is language-agnostic and used by all non-Go parsers.
type declMeta struct {
	Signature string
	Doc       string
	LineCount int
}

// langCallSite is the language-agnostic equivalent of rawCallSite used by
// non-Go parsers to collect call sites for post-parse resolution.
type langCallSite struct {
	pkgAlias string
	funcName string
}

// extractSigToBody extracts the declaration signature by finding the body block
// and returning source text from the declaration start up to (but not including)
// the body. Handles "block" (Python, Rust, Java) and "statement_block" (TypeScript).
// Falls back to the full declaration text if no body block is found.
func extractSigToBody(declNode *sitter.Node, src []byte) string {
	for i := 0; i < int(declNode.ChildCount()); i++ {
		child := declNode.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "block" || child.Type() == "statement_block" {
			sig := string(src[declNode.StartByte():child.StartByte()])
			return strings.TrimSpace(strings.Join(strings.Fields(sig), " "))
		}
	}
	return strings.TrimSpace(string(src[declNode.StartByte():declNode.EndByte()]))
}

// extractLineDoc scans backwards from startLine (1-indexed) collecting contiguous
// line comments immediately preceding the declaration. prefix is the comment
// prefix for the language (e.g. "//", "#", "///").
func extractLineDoc(lines []string, startLine int, prefix string) string {
	if startLine < 2 {
		return ""
	}
	var parts []string
	for i := startLine - 2; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, prefix) {
			text := strings.TrimPrefix(trimmed, prefix)
			text = strings.TrimPrefix(text, " ")
			parts = append([]string{text}, parts...)
		} else {
			break
		}
	}
	return strings.Join(parts, " ")
}

// extractBlockDoc scans backwards from startLine (1-indexed) collecting a
// block comment (/** ... */ or /* ... */) immediately preceding the declaration.
// Used for Javadoc, JSDoc, PHPDoc, C/C++ block comments.
func extractBlockDoc(lines []string, startLine int) string {
	if startLine < 2 {
		return ""
	}
	// Find the end of the block comment (line ending with */ before the decl).
	endIdx := -1
	for i := startLine - 2; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if strings.HasSuffix(trimmed, "*/") {
			endIdx = i
		}
		break
	}
	if endIdx < 0 {
		return ""
	}
	// Scan backwards for the start of the block comment (line with /*).
	startIdx := endIdx
	for i := endIdx; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "/*") {
			startIdx = i
			break
		}
		// Stop if we hit a non-comment line.
		if !strings.HasPrefix(trimmed, "*") && !strings.HasPrefix(trimmed, "/*") {
			return ""
		}
	}
	// Extract comment text, stripping comment markers.
	var parts []string
	for i := startIdx; i <= endIdx; i++ {
		trimmed := strings.TrimSpace(lines[i])
		trimmed = strings.TrimPrefix(trimmed, "/**")
		trimmed = strings.TrimPrefix(trimmed, "/*")
		trimmed = strings.TrimSuffix(trimmed, "*/")
		trimmed = strings.TrimPrefix(trimmed, "* ")
		trimmed = strings.TrimPrefix(trimmed, "*")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " ")
}

// extractDocMulti tries line comments first, then falls back to block comments.
// This covers both // and /** */ styles for languages that use both (Java, C#, etc.).
func extractDocMulti(lines []string, startLine int, linePrefix string) string {
	if doc := extractLineDoc(lines, startLine, linePrefix); doc != "" {
		return doc
	}
	return extractBlockDoc(lines, startLine)
}

// extractPythonDocstring extracts a Python docstring from immediately inside a
// function or class body. It looks for triple-quoted strings on the line(s) after
// the def/class declaration.
func extractPythonDocstring(lines []string, startLine int) string {
	if startLine < 1 || startLine >= len(lines) {
		return ""
	}
	// Look for a triple-quoted string starting within 2 lines of the declaration.
	for i := startLine; i < len(lines) && i < startLine+3; i++ {
		trimmed := strings.TrimSpace(lines[i])
		for _, q := range []string{`"""`, `'''`} {
			if strings.HasPrefix(trimmed, q) {
				// Single-line docstring: """text"""
				rest := strings.TrimPrefix(trimmed, q)
				if endIdx := strings.Index(rest, q); endIdx >= 0 {
					return strings.TrimSpace(rest[:endIdx])
				}
				// Multi-line docstring: collect until closing quotes.
				var parts []string
				firstLine := strings.TrimSpace(rest)
				if firstLine != "" {
					parts = append(parts, firstLine)
				}
				for j := i + 1; j < len(lines); j++ {
					line := lines[j]
					trimmedLine := strings.TrimSpace(line)
					if endIdx := strings.Index(trimmedLine, q); endIdx >= 0 {
						before := strings.TrimSpace(trimmedLine[:endIdx])
						if before != "" {
							parts = append(parts, before)
						}
						return strings.Join(parts, " ")
					}
					if trimmedLine != "" {
						parts = append(parts, trimmedLine)
					}
				}
			}
		}
	}
	return ""
}

// childText returns the text content of a node, or "" if the node is nil.
func childText(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	return string(src[n.StartByte():n.EndByte()])
}

// hasChildToken checks if a node has a direct child whose source text matches token.
func hasChildToken(n *sitter.Node, src []byte, token string) bool {
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child != nil && string(src[child.StartByte():child.EndByte()]) == token {
			return true
		}
	}
	return false
}

// findEnclosingClass walks the AST tree from a method node upward to find the
// enclosing class/struct name. This is used for class-qualifying method names.
// classTypes is the set of node types that represent classes (e.g. "class_declaration",
// "class_definition", "struct_specifier").
func findEnclosingClass(n *sitter.Node, src []byte, classTypes map[string]bool) string {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if classTypes[p.Type()] {
			nameNode := p.ChildByFieldName("name")
			if nameNode != nil {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			// Some grammars use unnamed children — try first type_identifier or identifier.
			for _, typ := range []string{"type_identifier", "identifier", "constant", "name"} {
				if ch := firstChildOfType(p, typ); ch != nil {
					return string(src[ch.StartByte():ch.EndByte()])
				}
			}
		}
	}
	return ""
}

// extractSigToBodyMulti tries multiple body block type names when extracting
// signatures. Used for languages with varied body block types.
func extractSigToBodyMulti(declNode *sitter.Node, src []byte, bodyTypes []string) string {
	bodySet := make(map[string]bool, len(bodyTypes))
	for _, bt := range bodyTypes {
		bodySet[bt] = true
	}
	for i := 0; i < int(declNode.ChildCount()); i++ {
		child := declNode.Child(i)
		if child == nil {
			continue
		}
		if bodySet[child.Type()] {
			sig := string(src[declNode.StartByte():child.StartByte()])
			return strings.TrimSpace(strings.Join(strings.Fields(sig), " "))
		}
	}
	return strings.TrimSpace(string(src[declNode.StartByte():declNode.EndByte()]))
}

// firstChildOfType returns the first direct child of n whose type matches typ,
// or nil if none is found. Used by language parsers that lack named fields.
func firstChildOfType(n *sitter.Node, typ string) *sitter.Node {
	for i := 0; i < int(n.ChildCount()); i++ {
		child := n.Child(i)
		if child != nil && child.Type() == typ {
			return child
		}
	}
	return nil
}

// buildLangMeta converts a declMeta into a Node.Metadata map.
// Returns nil if all fields are zero/empty (avoids empty map allocations).
func buildLangMeta(m declMeta) map[string]string {
	if m.Signature == "" && m.Doc == "" && m.LineCount == 0 {
		return nil
	}
	result := make(map[string]string, 3)
	if m.Signature != "" {
		result["signature"] = m.Signature
	}
	if m.Doc != "" {
		result["doc"] = m.Doc
	}
	if m.LineCount > 0 {
		result["line_count"] = strconv.Itoa(m.LineCount)
	}
	return result
}
