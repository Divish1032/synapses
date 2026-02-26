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
