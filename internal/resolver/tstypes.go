package resolver

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/synapses/synapses/internal/graph"
)

//go:embed tsresolver.js
var tsResolverScript []byte

// maxTSEdges caps the number of DATA_FLOWS edges accepted from the TypeScript
// resolver to prevent unbounded memory growth on very large projects.
const maxTSEdges = 10_000

// tsEdge is the JSON shape emitted by tsresolver.js on stdout.
type tsEdge struct {
	FromFile string `json:"from_file"`
	FromLine int    `json:"from_line"`
	ToFile   string `json:"to_file"`
	ToLine   int    `json:"to_line"`
}

// ResolveTSTypesCallEdges performs a type-checked CALLS resolution pass for
// TypeScript (.ts / .tsx) files by spawning a Node.js subprocess that runs the
// embedded tsresolver.js script against the project at root.
//
// The resolver uses the TypeScript compiler API (typescript npm package) to
// resolve cross-file call targets that tree-sitter cannot see. It supplements
// — never replaces — the tree-sitter CALLS edges already in g.
//
// Returns the number of new CALLS edges added. Requires:
//   - Node.js available on PATH
//   - "typescript" package in <root>/node_modules or installed globally
//
// On any failure the error is returned and the caller should log it and
// continue (the graph is still usable with tree-sitter-only edges).
func ResolveTSTypesCallEdges(g *graph.Graph, root string) (int, error) {
	// Write the embedded script to a temp file.
	tmp, err := os.CreateTemp("", "synapses-ts-*.js")
	if err != nil {
		return 0, fmt.Errorf("create temp script: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(tsResolverScript); err != nil {
		tmp.Close()
		return 0, fmt.Errorf("write temp script: %w", err)
	}
	tmp.Close()

	// Spawn: node <script> <root>
	cmd := exec.Command("node", tmpPath, root) //nolint:gosec
	cmd.Dir = root
	cmd.Stderr = os.Stderr // resolver warnings go directly to stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn node: %w", err)
	}

	// Build a (cleanAbsFile, 1-based line) → NodeID lookup from all
	// function and method nodes already in the graph.
	type posKey struct {
		file string
		line int
	}
	posToNode := make(map[posKey]graph.NodeID, g.NodeCount())
	for _, n := range g.AllNodes() {
		switch n.Type {
		case graph.NodeFunction, graph.NodeMethod:
			posToNode[posKey{filepath.Clean(n.File), n.Line}] = n.ID
		}
	}

	// Pre-populate dedup set from existing CALLS edges so we never create
	// duplicates of what the tree-sitter resolver already emitted.
	type edgeKey struct{ from, to graph.NodeID }
	seen := make(map[edgeKey]bool, g.EdgeCount())
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls {
			seen[edgeKey{e.From, e.To}] = true
		}
	}

	added := 0
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if added >= maxTSEdges {
			fmt.Fprintf(os.Stderr,
				"synapses/ts-types: edge cap (%d) reached in Go reader — draining remaining output\n",
				maxTSEdges)
			// Drain remaining stdout so the subprocess can exit cleanly.
			for scanner.Scan() {
			}
			break
		}

		var edge tsEdge
		if err := json.Unmarshal(scanner.Bytes(), &edge); err != nil {
			continue // skip malformed lines (shouldn't happen)
		}

		callerID := posToNode[posKey{filepath.Clean(edge.FromFile), edge.FromLine}]
		calleeID := posToNode[posKey{filepath.Clean(edge.ToFile), edge.ToLine}]
		if callerID == "" || calleeID == "" {
			continue // one endpoint not in graph (e.g. filtered file, vendor, etc.)
		}

		k := edgeKey{callerID, calleeID}
		if !seen[k] {
			seen[k] = true
			g.AddEdge(&graph.Edge{
				From: callerID,
				To:   calleeID,
				Type: graph.EdgeCalls,
			})
			added++
		}
	}

	// Wait for the subprocess to exit. Non-zero exit is non-fatal — the
	// resolver already wrote its error to stderr.
	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "synapses/ts-types: node exited with error: %v\n", err)
	}

	return added, nil
}
