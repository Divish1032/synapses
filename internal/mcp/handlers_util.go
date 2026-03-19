package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

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
func pickBestNode(nodes []*graph.Node, g *graph.Graph) *graph.Node {
	tierOf := func(n *graph.Node) int {
		isTest := strings.HasSuffix(n.File, "_test.go")
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
func jsonResult(v interface{}) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}
