package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// pluginTimeout caps the time a single plugin invocation may take.
const pluginTimeout = 10 * time.Second

// pluginOutput is the JSON schema emitted by external parser plugins on stdout.
// Plugins should write one JSON object and exit 0 on success.
//
// Protocol summary:
//
//	stdin:  <abs_file_path>\n<raw_file_content>
//	stdout: {"nodes":[...],"edges":[...]}
//	stderr: free-form warnings (printed to synapses stderr)
//	exit 0: success (empty output is valid — file has no symbols)
//	exit ≠0: error (logged; file is skipped gracefully)
type pluginOutput struct {
	Nodes []pluginNode `json:"nodes"`
	Edges []pluginEdge `json:"edges"`
}

// pluginNode describes a single code symbol emitted by a plugin.
type pluginNode struct {
	// Name is the symbol name (required).
	Name string `json:"name"`
	// Type is one of: function, method, struct, class, model, interface, package, module.
	// Unknown values fall back to "function".
	Type string `json:"type"`
	// Line is the 1-based source line where the symbol is declared (required).
	Line int `json:"line"`
	// Exported marks the symbol as public / exported.
	Exported bool `json:"exported,omitempty"`
	// Doc is an optional documentation string.
	Doc string `json:"doc,omitempty"`
	// Signature is an optional human-readable type signature.
	Signature string `json:"signature,omitempty"`
}

// pluginEdge describes a relationship between two symbols in the same file.
// Both From and To must be names of nodes declared in the same plugin output.
// The Type field is an EdgeType string (e.g. "CALLS", "IMPORTS").
type pluginEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// PluginParser implements LanguageParser by spawning an external process.
// One process is spawned per file; the process lifetime is bounded by pluginTimeout.
type PluginParser struct {
	extensions []string
	command    string
	args       []string
}

// newPluginParser builds a PluginParser from a raw command string and extension list.
// The command string is split on whitespace into binary + args.
func newPluginParser(extensions []string, command string) *PluginParser {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return &PluginParser{extensions: extensions}
	}
	return &PluginParser{
		extensions: extensions,
		command:    parts[0],
		args:       parts[1:],
	}
}

// Extensions implements LanguageParser.
func (p *PluginParser) Extensions() []string { return p.extensions }

// Parse implements LanguageParser. It spawns the plugin process, writes the
// file path + content to stdin, reads JSON from stdout, and merges the
// resulting nodes and edges into g.
func (p *PluginParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	if p.command == "" {
		return fmt.Errorf("plugin: empty command for extensions %v", p.extensions)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pluginTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.command, p.args...) //nolint:gosec // command is guarded by PluginChecker allowlist at registration time
	// stdin: file path on first line, then raw file content.
	cmd.Stdin = bytes.NewReader(append([]byte(filePath+"\n"), src...))
	cmd.Stderr = os.Stderr // plugin warnings/errors flow to synapses stderr

	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("plugin %q: %w", p.command, err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil // plugin produced no output — valid for empty/unsupported files
	}

	var result pluginOutput
	if err := json.Unmarshal(out, &result); err != nil {
		return fmt.Errorf("plugin %q: parse output: %w", p.command, err)
	}

	// Always add a file node so the graph has an anchor for DEFINES edges.
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	// Register declared symbols.
	for _, n := range result.Nodes {
		if n.Name == "" || n.Line <= 0 {
			continue
		}
		nodeID := g.MakeNodeID(filePath, n.Name)
		var meta map[string]string
		if n.Doc != "" || n.Signature != "" {
			meta = make(map[string]string, 2)
			if n.Doc != "" {
				meta["doc"] = n.Doc
			}
			if n.Signature != "" {
				meta["signature"] = n.Signature
			}
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     pluginNodeType(n.Type),
			Name:     n.Name,
			File:     filePath,
			Line:     n.Line,
			Exported: n.Exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// Register declared edges between symbols in this file.
	for _, e := range result.Edges {
		if e.From == "" || e.To == "" || e.Type == "" {
			continue
		}
		fromID := g.MakeNodeID(filePath, e.From)
		toID := g.MakeNodeID(filePath, e.To)
		// Skip if either endpoint was not declared in this parse round.
		if g.GetNode(fromID) == nil || g.GetNode(toID) == nil {
			continue
		}
		edgeType := graph.EdgeType(strings.ToUpper(e.Type))
		// Validate against known edge types to prevent arbitrary values
		// from untrusted plugin subprocess output.
		if _, known := graph.DefaultEdgeWeights[edgeType]; !known {
			continue
		}
		g.AddEdge(&graph.Edge{
			From: fromID,
			To:   toID,
			Type: edgeType,
		})
	}

	return nil
}

// pluginNodeType maps a plugin-supplied type string to a graph.NodeType.
// Unknown values fall back to NodeFunction.
func pluginNodeType(s string) graph.NodeType {
	switch strings.ToLower(s) {
	case "function", "func":
		return graph.NodeFunction
	case "method":
		return graph.NodeMethod
	case "struct", "class", "model", "type":
		return graph.NodeStruct
	case "interface", "trait", "protocol":
		return graph.NodeInterface
	case "package", "module", "namespace":
		return graph.NodePackage
	default:
		return graph.NodeFunction
	}
}
