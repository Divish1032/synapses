package parser

import (
	"strconv"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// declMeta holds the enriched metadata extracted for any declaration node.
// It is language-agnostic and used by all non-Go parsers.
type declMeta struct {
	Signature   string
	Doc         string
	LineCount   int
	IfdefGuard  string // non-empty when symbol is inside a #ifdef/#if/#else block
}

// langCallSite is the language-agnostic equivalent of rawCallSite used by
// non-Go parsers to collect call sites for post-parse resolution.
type langCallSite struct {
	pkgAlias string
	funcName string
}

// callSiteConfig tells the generic call-site collector which AST node types
// represent class-like and function-like declarations, and which node types
// are call expressions. This allows a single AST walk to resolve function-level
// callers for any language.
type callSiteConfig struct {
	// ClassTypes are node types that establish a class context (e.g. "class_declaration").
	ClassTypes map[string]bool
	// FuncTypes are node types that establish a function context (e.g. "function_declaration").
	FuncTypes map[string]bool
	// CallTypes are node types that represent call expressions (e.g. "call_expression").
	CallTypes map[string]bool
	// NameExtractor returns the name from a class or function node.
	// For class: returns "ClassName". For function: returns "funcName".
	NameExtractor func(n sitter.Node, src []byte) string
	// CalleeExtractor returns the callee name from a call expression node, or "" to skip.
	// Mutually exclusive with AliasedCalleeExtractor — only one should be set.
	CalleeExtractor func(n sitter.Node, src []byte) string
	// AliasedCalleeExtractor returns both the receiver/object alias and the callee name
	// from a call expression node. Use this instead of CalleeExtractor for languages
	// that distinguish obj.method() from bare function() calls. Returns ("", "") to skip.
	// The alias is stored as PkgAlias in the CallSite for import-guided resolution.
	AliasedCalleeExtractor func(n sitter.Node, src []byte) (alias, name string)
	// IsBuiltin returns true if a callee name should be filtered out.
	IsBuiltin func(name string) bool
}

// extractSigToBody extracts the declaration signature by finding the body block
// and returning source text from the declaration start up to (but not including)
// the body. Handles "block" (Python, Rust, Java) and "statement_block" (TypeScript).
// Falls back to the full declaration text if no body block is found.
func extractSigToBody(declNode sitter.Node, src []byte) string {
	for i := uint32(0); i < declNode.ChildCount(); i++ {
		child := declNode.Child(i)
		if child.IsNull() {
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
					if j-i > 200 {
						break
					}
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

// childText returns the text content of a node, or "" if the node is null.
func childText(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	return string(src[n.StartByte():n.EndByte()])
}

// hasChildToken checks if a node has a direct child whose source text matches token.
func hasChildToken(n sitter.Node, src []byte, token string) bool {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && string(src[child.StartByte():child.EndByte()]) == token {
			return true
		}
	}
	return false
}

// findEnclosingClass walks the AST tree from a method node upward to find the
// enclosing class/struct name. This is used for class-qualifying method names.
// classTypes is the set of node types that represent classes (e.g. "class_declaration",
// "class_definition", "struct_specifier").
func findEnclosingClass(n sitter.Node, src []byte, classTypes map[string]bool) string {
	for p := n.Parent(); !p.IsNull(); p = p.Parent() {
		if classTypes[p.Type()] {
			nameNode := p.ChildByFieldName("name")
			if !nameNode.IsNull() {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			// Some grammars use unnamed children — try first type_identifier or identifier.
			for _, typ := range []string{"type_identifier", "identifier", "constant", "name"} {
				if ch := firstChildOfType(p, typ); !ch.IsNull() {
					return string(src[ch.StartByte():ch.EndByte()])
				}
			}
		}
	}
	return ""
}

// extractSigToBodyMulti tries multiple body block type names when extracting
// signatures. Used for languages with varied body block types.
func extractSigToBodyMulti(declNode sitter.Node, src []byte, bodyTypes []string) string {
	bodySet := make(map[string]bool, len(bodyTypes))
	for _, bt := range bodyTypes {
		bodySet[bt] = true
	}
	for i := uint32(0); i < declNode.ChildCount(); i++ {
		child := declNode.Child(i)
		if child.IsNull() {
			continue
		}
		if bodySet[child.Type()] {
			sig := string(src[declNode.StartByte():child.StartByte()])
			return strings.TrimSpace(strings.Join(strings.Fields(sig), " "))
		}
	}
	return strings.TrimSpace(string(src[declNode.StartByte():declNode.EndByte()]))
}

// collectCallSitesWalk performs a depth-first AST walk to collect call sites
// with function-level caller resolution. Instead of attributing all calls to the
// file node, each call is attributed to its enclosing function/method.
//
// The walk tracks (enclosingClass, enclosingFunc) context. When a call expression
// is found, the caller is resolved to the graph node matching the enclosing function.
// If no enclosing function exists (module-level call), fileNodeID is used.
//
// This produces the same quality of CALLS edges as the Go parser, enabling
// accurate get_call_chain and get_impact for all supported languages.
func collectCallSitesWalk(
	g *graph.Graph,
	root sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	cfg callSiteConfig,
) {
	var walk func(n sitter.Node, enclosingClass, enclosingFunc string)
	walk = func(n sitter.Node, enclosingClass, enclosingFunc string) {
		if n.IsNull() {
			return
		}
		nt := n.Type()

		// Track class context.
		if cfg.ClassTypes[nt] {
			className := cfg.NameExtractor(n, src)
			if className != "" {
				for i := uint32(0); i < n.ChildCount(); i++ {
					walk(n.Child(i), className, enclosingFunc)
				}
				return
			}
		}

		// Track function context.
		if cfg.FuncTypes[nt] {
			funcName := cfg.NameExtractor(n, src)
			if funcName != "" {
				qualFunc := funcName
				if enclosingClass != "" {
					qualFunc = enclosingClass + "." + funcName
				}
				for i := uint32(0); i < n.ChildCount(); i++ {
					walk(n.Child(i), enclosingClass, qualFunc)
				}
				return
			}
		}

		// Collect call site.
		if cfg.CallTypes[nt] {
			var alias, callee string
			if cfg.AliasedCalleeExtractor != nil {
				alias, callee = cfg.AliasedCalleeExtractor(n, src)
			} else if cfg.CalleeExtractor != nil {
				callee = cfg.CalleeExtractor(n, src)
			}
			if callee != "" && !cfg.IsBuiltin(callee) {
				// Resolve caller: use enclosing function node if available, else file.
				callerID := fileNodeID
				if enclosingFunc != "" {
					funcNodeID := g.MakeNodeID(filePath, enclosingFunc)
					if g.GetNode(funcNodeID) != nil {
						callerID = funcNodeID
					}
				}
				g.AddCallSite(graph.CallSite{
					CallerID:   callerID,
					CallerFile: filePath,
					PkgAlias:   alias,
					FuncName:   callee,
				})
			}
		}

		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass, enclosingFunc)
		}
	}
	walk(root, "", "")
}

// extractTypeIdentifiers extracts type names from a heritage clause AST node.
// It walks direct children looking for type_identifier nodes (simple names)
// and generic_type nodes (extracts the name child). Returns deduplicated names.
func extractTypeIdentifiers(n sitter.Node, src []byte) []string {
	if n.IsNull() {
		return nil
	}
	var names []string
	seen := make(map[string]bool)
	var walk func(child sitter.Node)
	walk = func(child sitter.Node) {
		if child.IsNull() {
			return
		}
		switch child.Type() {
		case "type_identifier":
			name := string(src[child.StartByte():child.EndByte()])
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			return
		case "generic_type":
			// generic_type has a name child (type_identifier) + type_arguments.
			// Extract only the name, not the type arguments.
			if ti := firstChildOfType(child, "type_identifier"); !ti.IsNull() {
				name := string(src[ti.StartByte():ti.EndByte()])
				if name != "" && !seen[name] {
					seen[name] = true
					names = append(names, name)
				}
			}
			return
		case "user_type":
			// Kotlin user_type: simple_identifier + optional type_arguments.
			// Extract only the identifier, not the type arguments.
			for _, idType := range []string{"simple_identifier", "type_identifier"} {
				if id := firstChildOfType(child, idType); !id.IsNull() {
					name := string(src[id.StartByte():id.EndByte()])
					if name != "" && !seen[name] {
						seen[name] = true
						names = append(names, name)
					}
					return
				}
			}
			return
		case "identifier", "simple_identifier":
			name := string(src[child.StartByte():child.EndByte()])
			if name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			return
		}
		for i := uint32(0); i < child.ChildCount(); i++ {
			walk(child.Child(i))
		}
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		walk(n.Child(i))
	}
	return names
}

// firstChildOfType returns the first direct child of n whose type matches typ,
// or nil if none is found. Used by language parsers that lack named fields.
func firstChildOfType(n sitter.Node, typ string) sitter.Node {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == typ {
			return child
		}
	}
	return sitter.Node{}
}

// buildLangMeta converts a declMeta into a Node.Metadata map.
// Returns nil if all fields are zero/empty (avoids empty map allocations).
func buildLangMeta(m declMeta) map[string]string {
	if m.Signature == "" && m.Doc == "" && m.LineCount == 0 && m.IfdefGuard == "" {
		return nil
	}
	result := make(map[string]string, 4)
	if m.Signature != "" {
		result["signature"] = m.Signature
	}
	if m.Doc != "" {
		result["doc"] = m.Doc
	}
	if m.LineCount > 0 {
		result["line_count"] = strconv.Itoa(m.LineCount)
	}
	if m.IfdefGuard != "" {
		result["ifdef"] = m.IfdefGuard
	}
	return result
}
