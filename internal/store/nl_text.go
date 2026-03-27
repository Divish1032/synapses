// Package store — deterministic NL description generation for code nodes.
//
// Converts code identifiers and signatures into natural-language text so that
// embedding similarity between code nodes and documentation nodes operates in
// the same NL↔NL domain instead of the weaker code-syntax↔NL domain.
package store

import (
	"strings"
	"unicode"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// nameToWords decomposes a code identifier into lowercase space-separated words.
// Handles CamelCase, snake_case, SCREAMING_SNAKE, dotted names, and mixed forms.
//
//	"SendStaticFile"    → "send static file"
//	"send_static_file"  → "send static file"
//	"getHTTPResponse"   → "get http response"
//	"Context.JSON"      → "context json"
//	"HTTP"              → "http"
func nameToWords(name string) string {
	if name == "" {
		return ""
	}
	var words []string
	// Split on dots first (e.g. "Flask.send_static_file")
	for _, part := range strings.Split(name, ".") {
		words = append(words, splitIdentifier(part)...)
	}
	return strings.Join(words, " ")
}

// splitIdentifier splits a single identifier (no dots) into lowercase words.
func splitIdentifier(s string) []string {
	if s == "" {
		return nil
	}
	// Split on underscores/hyphens first
	segments := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-'
	})
	var words []string
	for _, seg := range segments {
		words = append(words, splitCamelCaseWords(seg)...)
	}
	return words
}

// splitCamelCaseWords splits a CamelCase or ALLCAPS segment into lowercase words.
//
//	"SendFile"       → ["send", "file"]
//	"getHTTPResponse" → ["get", "http", "response"]
//	"HTTP"           → ["http"]
//	"HTMLParser"     → ["html", "parser"]
func splitCamelCaseWords(s string) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev := runes[i-1]
		cur := runes[i]
		// Split before: lowercase→uppercase (e.g. "send|File")
		if unicode.IsLower(prev) && unicode.IsUpper(cur) {
			words = append(words, strings.ToLower(string(runes[start:i])))
			start = i
			continue
		}
		// Split before: uppercase→uppercase→lowercase (e.g. "HTT|P|Response" → "HTTP|Response")
		// We split before the last uppercase in a run when followed by lowercase.
		if i >= 2 && unicode.IsUpper(runes[i-2]) && unicode.IsUpper(prev) && unicode.IsLower(cur) {
			words = append(words, strings.ToLower(string(runes[start:i-1])))
			start = i - 1
			continue
		}
	}
	if start < len(runes) {
		w := strings.ToLower(string(runes[start:]))
		if w != "" {
			words = append(words, w)
		}
	}
	return words
}

// nameOwnerAndWords splits a dotted name into owner (first segment) and the rest as words.
// "Flask.send_static_file" → owner="flask", words="send static file"
// "send_static_file" → owner="", words="send static file"
func nameOwnerAndWords(name string) (owner, words string) {
	dot := strings.IndexByte(name, '.')
	if dot < 0 {
		return "", nameToWords(name)
	}
	ownerPart := name[:dot]
	rest := name[dot+1:]
	return nameToWords(ownerPart), nameToWords(rest)
}

// IsCodeNodeType returns true for node types that represent code entities
// (as opposed to documentation, knowledge, or structural nodes).
func IsCodeNodeType(t graph.NodeType) bool {
	switch t {
	case graph.NodeFunction, graph.NodeMethod, graph.NodeStruct,
		graph.NodeInterface, graph.NodeVariable, graph.NodeRoute:
		return true
	}
	return false
}

// GenerateNLDescription produces a deterministic natural-language description
// of a code node from its name, type, signature, docstring, and call edges.
// Zero LLM dependency — pure string manipulation.
//
// Hard cap: 400 characters. Returns "" for non-code node types.
func GenerateNLDescription(name string, nodeType graph.NodeType, sig, doc string, callees, callers []string) string {
	if !IsCodeNodeType(nodeType) {
		return ""
	}

	owner, words := nameOwnerAndWords(name)

	// Short-circuit: trivial names with no doc and no edges (e.g. "main", "init")
	if len(name) <= 5 && doc == "" && sig == "" && len(callees) == 0 && len(callers) == 0 {
		if owner != "" {
			return owner + ": " + words
		}
		return words
	}

	var sb strings.Builder
	sb.Grow(400)

	// Owner prefix: "flask: " or ""
	if owner != "" {
		sb.WriteString(owner)
		sb.WriteString(": ")
	}

	switch nodeType {
	case graph.NodeFunction, graph.NodeMethod:
		// "{words}{params clause}. {doc sentence}. {callees clause}"
		sb.WriteString(words)
		if params := extractParams(sig); params != "" {
			sb.WriteString(", given ")
			sb.WriteString(params)
		}
		if docSent := firstDocSentence(doc); docSent != "" {
			if !strings.HasSuffix(sb.String(), ".") {
				sb.WriteByte('.')
			}
			sb.WriteByte(' ')
			sb.WriteString(docSent)
		}
		if ret := extractReturnHint(sig); ret != "" {
			if !strings.HasSuffix(sb.String(), ".") {
				sb.WriteByte('.')
			}
			sb.WriteString(" returns ")
			sb.WriteString(ret)
		}
		if narrative := calleeNarrative(callees); narrative != "" {
			if !strings.HasSuffix(sb.String(), ".") {
				sb.WriteByte('.')
			}
			sb.WriteString(" involves ")
			sb.WriteString(narrative)
		}
		if callerNarr := callerNarrative(callers); callerNarr != "" {
			if !strings.HasSuffix(sb.String(), ".") {
				sb.WriteByte('.')
			}
			sb.WriteString(" called by ")
			sb.WriteString(callerNarr)
		}

	case graph.NodeStruct:
		// "{words}. {doc sentence}."
		sb.WriteString(words)
		if docSent := firstDocSentence(doc); docSent != "" {
			sb.WriteString(". ")
			sb.WriteString(docSent)
		}

	case graph.NodeInterface:
		sb.WriteString(words)
		sb.WriteString(": interface")
		if docSent := firstDocSentence(doc); docSent != "" {
			sb.WriteString(". ")
			sb.WriteString(docSent)
		}

	case graph.NodeVariable:
		sb.WriteString(words)
		if docSent := firstDocSentence(doc); docSent != "" {
			sb.WriteString(". ")
			sb.WriteString(docSent)
		}

	case graph.NodeRoute:
		sb.WriteString(words)
		if docSent := firstDocSentence(doc); docSent != "" {
			sb.WriteString(". ")
			sb.WriteString(docSent)
		}
	}

	result := sb.String()
	if len(result) > 400 {
		// Truncate at rune boundary to avoid breaking multibyte UTF-8.
		runes := []rune(result)
		if len(runes) > 400 {
			result = string(runes[:400])
		}
	}
	return result
}

// extractParams extracts parameter names from a function signature.
// Works across Go, Python, Java, JS/TS signatures by finding content between parens.
// Strips type annotations, keeping only parameter names.
//
//	"func send(filename string, ctx context.Context)" → "filename, ctx"
//	"def send(self, filename, content_type=None)"     → "filename, content type"
//	"(self, request, *args, **kwargs)"                → "request"
func extractParams(sig string) string {
	if sig == "" {
		return ""
	}
	// Find first opening paren
	open := strings.IndexByte(sig, '(')
	if open < 0 {
		return ""
	}
	// Find matching closing paren
	close := strings.IndexByte(sig[open:], ')')
	if close < 0 {
		return ""
	}
	inner := sig[open+1 : open+close]
	if strings.TrimSpace(inner) == "" {
		return ""
	}

	// Split on commas, respecting angle-bracket nesting (Java/TS generics).
	parts := splitRespectingBrackets(inner)
	var names []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Skip self/this/cls
		lower := strings.ToLower(p)
		if lower == "self" || lower == "this" || lower == "cls" {
			continue
		}
		// Skip *args, **kwargs
		if strings.HasPrefix(p, "*") {
			continue
		}
		// Extract just the param name:
		// Go: "filename string" → "filename"
		// Python: "filename: str = None" → "filename"
		// Java: "String filename" → "filename" (last token before any =)
		name := extractParamName(p)
		if name == "" || name == "self" || name == "this" || name == "cls" {
			continue
		}
		names = append(names, nameToWords(name))
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) > 5 {
		names = names[:5]
	}
	return strings.Join(names, ", ")
}

// extractParamName gets the parameter name from a single param declaration.
func extractParamName(param string) string {
	// Remove default values: "x = 5" → "x"
	if eq := strings.IndexByte(param, '='); eq >= 0 {
		param = strings.TrimSpace(param[:eq])
	}
	// Remove type annotations after colon: "x: int" → "x"
	if colon := strings.IndexByte(param, ':'); colon >= 0 {
		param = strings.TrimSpace(param[:colon])
	}

	// Now we have either "name type" (Go/Java) or just "name" (Python/JS)
	fields := strings.Fields(param)
	if len(fields) == 0 {
		return ""
	}

	// Strip Java annotations like @NotNull
	for len(fields) > 1 && strings.HasPrefix(fields[0], "@") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return ""
	}

	// Single token: if it's a type name, skip it; otherwise it's the param name.
	if len(fields) == 1 {
		if isTypeName(fields[0]) {
			return ""
		}
		return fields[0]
	}

	// Two+ tokens: find the param name (the non-type token).
	// Go: "filename string" → first is name
	// Java: "String filename" → last is name
	first := fields[0]
	if unicode.IsUpper(rune(first[0])) || isTypeName(first) {
		// Type-first: "String filename", "context.Context ctx"
		return fields[len(fields)-1]
	}
	// Name-first: "filename string", "ctx context.Context"
	return fields[0]
}

// isJavaType checks if a token looks like a Java/Go type (contains dots or angle brackets).
func isJavaType(s string) bool {
	return strings.ContainsAny(s, ".<>[]")
}

// isTypeName returns true if the token looks like a type rather than a param name.
// Covers Go primitives, Java primitives, pointer types, and capitalized types.
func isTypeName(s string) bool {
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "*") || strings.HasPrefix(s, "[]") {
		return true
	}
	switch s {
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"float32", "float64", "float", "double",
		"string", "bool", "boolean", "byte", "char",
		"long", "short", "void", "error", "rune",
		"interface{}", "any":
		return true
	}
	if isJavaType(s) {
		return true
	}
	return false
}

// firstDocSentence extracts the first sentence from a docstring.
// Stops at the first period followed by a space or end-of-string, or at 120 chars.
func firstDocSentence(doc string) string {
	if doc == "" {
		return ""
	}
	// Strip common docstring prefixes
	doc = strings.TrimSpace(doc)
	// Trim leading "Returns ...\n" style — we want the description, not return doc
	// But if the whole doc is "Returns X", that IS the description.

	// Take first 120 runes max (rune-safe truncation)
	if r := []rune(doc); len(r) > 120 {
		doc = string(r[:120])
	}

	// Find sentence boundary
	for i := 0; i < len(doc)-1; i++ {
		if doc[i] == '.' && (doc[i+1] == ' ' || doc[i+1] == '\n') {
			return doc[:i+1]
		}
	}
	// Check if doc ends with period
	if doc[len(doc)-1] == '.' {
		return doc
	}
	return doc
}

// extractReturnHint extracts a return type hint from a signature.
//
//	Go:     "func foo(x int) (string, error)" → "string, error"
//	Python: "def foo(x) -> str"               → "str"
//	Java:   "public String foo(x)"            → "" (type is before name, too complex)
func extractReturnHint(sig string) string {
	if sig == "" {
		return ""
	}
	// Python: -> annotation
	if idx := strings.Index(sig, "->"); idx >= 0 {
		ret := strings.TrimSpace(sig[idx+2:])
		if ret != "" && ret != "None" && ret != "void" {
			return nameToWords(ret)
		}
		return ""
	}
	// Go: find the closing paren of the param list, then check what follows.
	// "func foo(x int) (string, error)" or "func foo(x int) string"
	// We need to find the param-list closing paren, not the return-type closing paren.
	open := strings.IndexByte(sig, '(')
	if open < 0 {
		return ""
	}
	// Find matching close paren for the param list (handle nested parens).
	depth := 0
	paramClose := -1
	for i := open; i < len(sig); i++ {
		switch sig[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				paramClose = i
				goto found
			}
		}
	}
found:
	if paramClose < 0 || paramClose == len(sig)-1 {
		return ""
	}
	rest := strings.TrimSpace(sig[paramClose+1:])
	if rest == "" || rest == "{" {
		return ""
	}
	// Strip outer parens from Go multi-return "(string, error)" → "string, error"
	rest = strings.Trim(rest, "()")
	rest = strings.TrimSpace(rest)
	if rest == "" || rest == "error" {
		return ""
	}
	// Skip pointer/complex types like "*Graph" — not useful as NL
	if strings.HasPrefix(rest, "*") {
		return ""
	}
	// For multi-return, take only the first non-error type
	if idx := strings.IndexByte(rest, ','); idx >= 0 {
		first := strings.TrimSpace(rest[:idx])
		if first == "error" {
			return ""
		}
		return nameToWords(first)
	}
	return nameToWords(rest)
}

// calleeNarrative builds a short narrative of what a function calls.
// Max 5 callees, each converted to words.
//
//	["GetPath", "SendFile"] → "get path, send file"
func calleeNarrative(callees []string) string {
	if len(callees) == 0 {
		return ""
	}
	limit := 5
	if len(callees) < limit {
		limit = len(callees)
	}
	parts := make([]string, limit)
	for i := 0; i < limit; i++ {
		parts[i] = nameToWords(callees[i])
	}
	return strings.Join(parts, ", ")
}

// splitRespectingBrackets splits a string on commas while respecting
// angle-bracket nesting (e.g. "Map<String, List<String>>, int" → ["Map<String, List<String>>", " int"]).
func splitRespectingBrackets(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// callerNarrative builds a short narrative of what calls this function.
// Max 3 callers.
func callerNarrative(callers []string) string {
	if len(callers) == 0 {
		return ""
	}
	limit := 3
	if len(callers) < limit {
		limit = len(callers)
	}
	parts := make([]string, limit)
	for i := 0; i < limit; i++ {
		parts[i] = nameToWords(callers[i])
	}
	return strings.Join(parts, ", ")
}
