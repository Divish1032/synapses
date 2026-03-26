package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// pathStripRe matches absolute Unix paths in error messages.
// Matches any path starting with / followed by a path segment, covering all
// prefixes including /home, /Users, /tmp, /var, container paths (/app, /build),
// CI paths (/runner, /workspace), and others.
var pathStripRe = regexp.MustCompile(`/[a-zA-Z_][a-zA-Z0-9_.-]*/[^\s:,"'\)]+`)

// winPathStripRe matches absolute Windows paths (e.g. C:\Users\foo\...).
var winPathStripRe = regexp.MustCompile(`[A-Z]:\\[^\s:,"'\)]+`)

// MCP input size limits. These prevent unbounded memory growth, SQLite bloat,
// and OOM from embedding oversized strings. The default cap applies to all
// string args; tighter per-field limits apply to content stored in SQLite or
// passed to the embedder.
const (
	maxArgLength           = 64 * 1024 // default cap for all string args
	maxArgLengthDecision   = 4 * 1024  // episodic memory / remember() decision field
	maxArgLengthRationale  = 4 * 1024  // remember() rationale — concatenated with decision before embedding
	maxArgLengthNote       = 8 * 1024  // annotation note field (annotate_node, web_annotate)
	maxArgLengthPayload    = 16 * 1024 // message bus payload field
)

// stringArg extracts a string argument from a CallToolRequest by key.
// Returns "" if the key is absent or not a string.
// Silently truncates to maxArgLength at a valid UTF-8 rune boundary.
func stringArg(req mcp.CallToolRequest, key string) string {
	v, _ := req.GetArguments()[key].(string)
	return truncateUTF8(v, maxArgLength)
}

// truncateUTF8 truncates s to at most maxBytes while preserving valid UTF-8.
// It walks back from the cut point to avoid splitting a multi-byte character.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	// Walk back at most 3 bytes (max UTF-8 continuation bytes in a 4-byte seq)
	// to find the last complete rune boundary.
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// stringArgLimited extracts a string argument and returns an error if it
// exceeds maxLen bytes. Use for fields stored in SQLite or passed to the
// embedder where silent truncation would produce silently corrupt data.
func stringArgLimited(req mcp.CallToolRequest, key string, maxLen int) (string, error) {
	v, _ := req.GetArguments()[key].(string)
	if len(v) > maxLen {
		return "", fmt.Errorf("%s exceeds maximum length of %d bytes (got %d bytes)", key, maxLen, len(v))
	}
	return v, nil
}

// camelWords splits a CamelCase or mixedCase identifier into lowercase words.
//
//	"CarveEgoGraph" → ["carve", "ego", "graph"]
//	"BFSCarver"     → ["bfs", "carver"]   (consecutive capitals = one word)
//	"handleGetNode" → ["handle", "get", "node"]
func camelWords(name string) []string {
	runes := []rune(name)
	var words []string
	var cur strings.Builder
	for i, r := range runes {
		isUp := r >= 'A' && r <= 'Z'
		prevUp := i > 0 && runes[i-1] >= 'A' && runes[i-1] <= 'Z'
		nextLo := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
		// Start a new word at an uppercase letter when it's a standard camelCase
		// boundary (previous was lower, e.g. "Ego" in "CarveEgoGraph") OR when
		// it's the last capital in an acronym run (e.g. "C" in "BFSCarver").
		if isUp && cur.Len() > 0 && (!prevUp || nextLo) {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		words = append(words, strings.ToLower(cur.String()))
	}
	return words
}

// pickBestNode selects the most-relevant node from a candidate list using a
// tiered priority system that avoids test files and structural noise:
//
//	Tier 1: function or method in a non-test file  (exact semantic match)
//	Tier 2: struct or interface in a non-test file
//	Tier 3: function or method in a test file
//	Tier 4: any other type in a non-test file
//	Tier 5: everything else
//
// Within each tier the node with the highest connectivity (fanin+fanout) wins.
func pickBestNode(nodes []*graph.Node, g *graph.Graph, query ...string) *graph.Node {
	tierOf := func(n *graph.Node) int {
		isTest := isTestFile(n.File)
		// Tier 0: exact case-sensitive name match on struct/interface in non-test file.
		// This prevents "Table" resolving to "Row.table" (method) or "HTML" to
		// "html" (function in Makefile) when an exact struct match exists.
		if len(query) > 0 && query[0] != "" && n.Name == query[0] && !isTest {
			if n.Type == graph.NodeStruct || n.Type == graph.NodeInterface {
				return 0
			}
		}
		switch n.Type {
		case graph.NodeFunction, graph.NodeMethod:
			if !isTest {
				return 1
			}
			return 3
		case graph.NodeStruct, graph.NodeInterface:
			if !isTest {
				return 2
			}
			return 4
		default:
			if !isTest {
				return 4
			}
			return 5
		}
	}

	best := nodes[0]
	bestTier := tierOf(best)
	bestScore := g.Fanin(best.ID) + g.Fanout(best.ID)

	for _, n := range nodes[1:] {
		tier := tierOf(n)
		score := g.Fanin(n.ID) + g.Fanout(n.ID)
		if tier < bestTier || (tier == bestTier && score > bestScore) {
			best = n
			bestTier = tier
			bestScore = score
		}
	}
	return best
}

// normalizeSubgraph trims the repo root prefix from all File fields so that
// the LLM sees short relative paths instead of machine-specific absolute ones.
// This meaningfully reduces token consumption in large responses.
func normalizeSubgraph(sg *graph.SubGraph, repoRoot string) *graph.SubGraph {
	if repoRoot == "" {
		return sg
	}
	prefix := repoRoot
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	trimFile := func(path string) string {
		if strings.HasPrefix(path, prefix) {
			return strings.TrimPrefix(path, prefix)
		}
		return path
	}

	// Deep-copy nodes with trimmed File fields to avoid mutating the live graph.
	outNodes := make([]graph.CarvedNode, len(sg.Nodes))
	for i, cn := range sg.Nodes {
		nodeCopy := *cn.Node
		nodeCopy.File = trimFile(nodeCopy.File)
		outNodes[i] = graph.CarvedNode{
			Node:      &nodeCopy,
			Relevance: cn.Relevance,
			Hop:       cn.Hop,
		}
	}
	return &graph.SubGraph{
		Root:  sg.Root,
		Nodes: outNodes,
		Edges: sg.Edges,
	}
}

// handleGetFileContext returns all entities defined in a file, ordered by line.
// Accepts a partial path (e.g. "store/tasks.go" matches the full absolute path).
func isTestFile(filePath string) bool {
	base := filePath
	if i := strings.LastIndex(filePath, "/"); i >= 0 {
		base = filePath[i+1:]
	}
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.ts") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, "_test.py") ||
		strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, "_test.js") ||
		strings.HasSuffix(base, ".test.js")
}

// topLevelPackage returns the first path component of filePath (the top-level
// directory name), which typically corresponds to the binary or package root.
// Returns "" if filePath has no directory component.
func topLevelPackage(filePath string) string {
	if filePath == "" {
		return ""
	}
	// Normalize to forward slashes so this works on Windows (where graph paths
	// may use backslashes) and Unix alike.
	clean := filepath.ToSlash(filePath)
	clean = strings.TrimLeft(clean, "/")
	parts := strings.SplitN(clean, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// jsonResult marshals v to JSON and wraps it in a text tool result.
// Responses exceeding 2 MiB are rejected to prevent memory exhaustion.
func jsonResult(v interface{}) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolError("marshal result", err)
	}
	const maxResponseBytes = 2 * 1024 * 1024 // 2 MiB
	if len(b) > maxResponseBytes {
		return mcp.NewToolResultError(fmt.Sprintf("response too large (%d bytes, max %d). Narrow your query.", len(b), maxResponseBytes)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// toolError wraps a raw Go error with a user-facing message and recovery hint.
// Common SQLite and store errors are detected and annotated so agents get
// actionable guidance instead of raw constraint violation strings (BUG-022).
func toolError(operation string, err error) (*mcp.CallToolResult, error) {
	msg := stripInternalPaths(err.Error())
	hint := ""

	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		hint = "A record with this ID already exists. Use a different ID or update the existing record."
	case strings.Contains(msg, "database is locked"):
		hint = "The database is temporarily busy. Retry after a brief pause."
	case strings.Contains(msg, "no such table"):
		hint = "The store schema may be outdated. Try re-running 'synapses index' to rebuild."
	case strings.Contains(msg, "disk I/O error") || strings.Contains(msg, "readonly database"):
		hint = "Database write failed — check disk space and file permissions."
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		hint = "A referenced record does not exist. Verify parent record IDs before creating child records."
	}

	if hint != "" {
		return mcp.NewToolResultError(fmt.Sprintf("%s failed: %s\n\nHint: %s", operation, msg, hint)), nil
	}
	return mcp.NewToolResultError(fmt.Sprintf("%s: %v", operation, msg)), nil
}

// StripInternalPaths removes absolute filesystem paths from error messages to
// prevent leaking internal server paths to AI agents via MCP tool results.
// Replaces patterns like "/Users/foo/.synapses/data/graph.db" with "<internal>".
func StripInternalPaths(msg string) string {
	// Strip Unix absolute paths (e.g., /home/user/.synapses/..., /Users/...)
	// but preserve relative paths and URL paths.
	result := pathStripRe.ReplaceAllString(msg, "<internal>")
	// Also strip Windows-style paths (e.g., C:\Users\...)
	result = winPathStripRe.ReplaceAllString(result, "<internal>")
	return result
}

// stripInternalPaths is an unexported alias for internal callers.
var stripInternalPaths = StripInternalPaths

// looksLikeFilePath returns true when the query string appears to be a file
// path rather than a symbol name — it contains a '/' separator and ends with a
// recognised code extension. Used to gate the FindByFile fallback so that
// ordinary symbol queries don't accidentally match file nodes.
func looksLikeFilePath(q string) bool {
	// Bare filenames (no '/'): only accept if the extension is a code extension
	// AND the stem doesn't look like a qualified symbol name (e.g. "render.JSON").
	// The heuristic: bare filenames must be lowercase (files like "gin.go",
	// "context.go") to avoid false positives on "Engine.Run", "render.JSON".
	if !strings.Contains(q, "/") && q != strings.ToLower(q) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(q))
	switch ext {
	case ".go", ".py", ".js", ".ts", ".jsx", ".tsx", ".java", ".kt", ".rb",
		".rs", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".swift", ".m",
		".scala", ".clj", ".ex", ".exs", ".erl", ".hs", ".ml", ".fs",
		".vue", ".svelte", ".php", ".lua", ".r", ".jl", ".dart", ".zig",
		".sol", ".tf", ".hcl", ".yaml", ".yml", ".json", ".toml", ".xml",
		".graphql", ".gql", ".proto", ".sql", ".sh", ".bash", ".zsh",
		".ps1", ".el", ".vim", ".css", ".scss", ".less", ".html", ".md":
		return true
	}
	return false
}
