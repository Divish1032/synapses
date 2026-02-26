package mcp

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/synapses/synapses/internal/graph"
)

// handleExportGraph exports a subgraph (around an entity) or the full graph
// as DOT, Mermaid, or GraphML for visualization and documentation.
func (s *Server) handleExportGraph(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	entityName := stringArg(req, "entity")
	format := stringArg(req, "format")
	if format == "" {
		format = "dot"
	}
	if format != "dot" && format != "mermaid" && format != "graphml" {
		return mcp.NewToolResultError("format must be 'dot', 'mermaid', or 'graphml'"), nil
	}

	depth := 2
	if v, ok := req.Params.Arguments["depth"].(float64); ok && v > 0 {
		depth = int(v)
	}
	includeMeta, _ := req.Params.Arguments["include_metadata"].(bool)

	var nodes []*graph.Node
	var edges []*graph.Edge
	scope := "full"
	entityLabel := ""

	if entityName != "" {
		// Ego-subgraph export centred on the named entity.
		candidates := s.graph.FindByName(entityName)
		if len(candidates) == 0 {
			candidates = s.graph.FindByPattern(entityName)
		}
		if len(candidates) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("entity %q not found", entityName)), nil
		}
		root := pickBestNode(candidates, s.graph)
		entityLabel = root.Name
		scope = "entity"

		cfg := graph.DefaultCarveConfig()
		cfg.MaxDepth = depth
		cfg.TokenBudget = 500000 // high budget — exporting, not summarising
		cfg.MinRelevance = 0     // include everything reachable

		sg, err := s.graph.CarveEgoGraph(root.ID, cfg)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// Deduplicate: CarvedNodes may repeat the root.
		seen := make(map[graph.NodeID]bool, len(sg.Nodes))
		for _, cn := range sg.Nodes {
			if !seen[cn.Node.ID] {
				nodes = append(nodes, cn.Node)
				seen[cn.Node.ID] = true
			}
		}
		edges = sg.Edges
	} else {
		// Full-graph export: exclude file/package hub-nodes for cleaner output.
		all := s.graph.AllNodes()
		nodes = make([]*graph.Node, 0, len(all))
		for _, n := range all {
			if n.Type != graph.NodeFile && n.Type != graph.NodePackage {
				nodes = append(nodes, n)
			}
		}
		edges = s.graph.AllEdges()
	}

	repoRoot := s.graph.Root()

	var output string
	switch format {
	case "dot":
		output = exportDOT(nodes, edges, repoRoot, includeMeta)
	case "mermaid":
		output = exportMermaid(nodes, edges, repoRoot, includeMeta)
	case "graphml":
		output = exportGraphML(nodes, edges, repoRoot)
	}

	result := map[string]interface{}{
		"status": "ok",
		"format": format,
		"scope":  scope,
		"graph":  output,
		"stats": map[string]int{
			"nodes": len(nodes),
			"edges": len(edges),
		},
	}
	if entityLabel != "" {
		result["entity"] = entityLabel
	}
	return jsonResult(result)
}

// ── DOT (Graphviz) ───────────────────────────────────────────────────────────

func exportDOT(nodes []*graph.Node, edges []*graph.Edge, repoRoot string, includeMeta bool) string {
	var b strings.Builder
	prefix := ""
	if repoRoot != "" {
		prefix = repoRoot + "/"
	}

	nodeSet := make(map[graph.NodeID]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}

	b.WriteString("digraph G {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [fontname=\"Helvetica\" fontsize=10];\n\n")

	for _, n := range nodes {
		id := dotNodeID(n.ID)
		shape := dotShape(n.Type)
		color := dotNodeColor(n.Type)
		file := strings.TrimPrefix(n.File, prefix)

		label := fmt.Sprintf("%s\\n(%s)", n.Name, string(n.Type))
		if includeMeta && n.Metadata != nil {
			if sig := n.Metadata["signature"]; sig != "" {
				if len(sig) > 60 {
					sig = sig[:60] + "..."
				}
				label += "\\n" + dotEscape(sig)
			}
		}

		fmt.Fprintf(&b, "  %s [label=%q shape=%s color=%s tooltip=%q];\n",
			id, label, shape, color, file)
	}

	b.WriteString("\n")

	for _, e := range edges {
		if !nodeSet[e.From] || !nodeSet[e.To] {
			continue
		}
		fmt.Fprintf(&b, "  %s -> %s [label=%q color=%s];\n",
			dotNodeID(e.From), dotNodeID(e.To),
			string(e.Type), dotEdgeColor(e.Type))
	}

	b.WriteString("}\n")
	return b.String()
}

// dotNodeID converts a NodeID to a valid DOT identifier by replacing
// non-alphanumeric characters with underscores and prepending "n" to
// ensure the result starts with a letter.
func dotNodeID(id graph.NodeID) string {
	var b strings.Builder
	b.WriteByte('n')
	for _, c := range string(id) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func dotEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func dotShape(t graph.NodeType) string {
	switch t {
	case graph.NodeFunction, graph.NodeMethod:
		return "ellipse"
	case graph.NodeStruct:
		return "box"
	case graph.NodeInterface:
		return "diamond"
	case graph.NodeVariable:
		return "note"
	default:
		return "box"
	}
}

func dotNodeColor(t graph.NodeType) string {
	switch t {
	case graph.NodeFunction:
		return "navy"
	case graph.NodeMethod:
		return "blue"
	case graph.NodeStruct:
		return "darkgreen"
	case graph.NodeInterface:
		return "purple"
	case graph.NodeVariable:
		return "gray"
	default:
		return "black"
	}
}

func dotEdgeColor(t graph.EdgeType) string {
	switch t {
	case graph.EdgeCalls:
		return "navy"
	case graph.EdgeImplements:
		return "purple"
	case graph.EdgeEmbeds:
		return "darkgreen"
	case graph.EdgeImports:
		return "gray"
	case graph.EdgeDependsOn:
		return "orange"
	default:
		return "black"
	}
}

// ── Mermaid ──────────────────────────────────────────────────────────────────

func exportMermaid(nodes []*graph.Node, edges []*graph.Edge, repoRoot string, includeMeta bool) string {
	var b strings.Builder

	nodeSet := make(map[graph.NodeID]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}
	usedTypes := make(map[graph.NodeType]bool)

	b.WriteString("graph LR\n")

	for _, n := range nodes {
		id := mermaidID(n.ID)
		usedTypes[n.Type] = true

		label := n.Name
		if includeMeta && n.Metadata != nil {
			if sig := n.Metadata["signature"]; sig != "" && len(sig) <= 40 {
				label += " | " + sig
			}
		}
		// Mermaid doesn't allow unescaped quotes inside node labels.
		label = strings.ReplaceAll(label, `"`, `'`)

		fmt.Fprintf(&b, "  %s[\"%s\"]:::%s\n", id, label, mermaidClass(n.Type))
	}

	b.WriteString("\n")

	for _, e := range edges {
		if !nodeSet[e.From] || !nodeSet[e.To] {
			continue
		}
		fmt.Fprintf(&b, "  %s --> |%s| %s\n",
			mermaidID(e.From), string(e.Type), mermaidID(e.To))
	}

	// Emit only the classDef entries for node types that actually appear.
	b.WriteString("\n")
	if usedTypes[graph.NodeFunction] || usedTypes[graph.NodeMethod] {
		b.WriteString("  classDef funcStyle fill:#ddf,stroke:#339,color:#000\n")
	}
	if usedTypes[graph.NodeStruct] {
		b.WriteString("  classDef structStyle fill:#dfd,stroke:#393,color:#000\n")
	}
	if usedTypes[graph.NodeInterface] {
		b.WriteString("  classDef ifaceStyle fill:#fdf,stroke:#939,color:#000\n")
	}
	if usedTypes[graph.NodeVariable] {
		b.WriteString("  classDef varStyle fill:#ffd,stroke:#993,color:#000\n")
	}

	return b.String()
}

// mermaidID converts a NodeID to a Mermaid-safe identifier (alphanumeric + underscore).
func mermaidID(id graph.NodeID) string {
	var b strings.Builder
	b.WriteByte('n')
	for _, c := range string(id) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func mermaidClass(t graph.NodeType) string {
	switch t {
	case graph.NodeFunction, graph.NodeMethod:
		return "funcStyle"
	case graph.NodeStruct:
		return "structStyle"
	case graph.NodeInterface:
		return "ifaceStyle"
	case graph.NodeVariable:
		return "varStyle"
	default:
		return "funcStyle"
	}
}

// ── GraphML ──────────────────────────────────────────────────────────────────

type graphmlDoc struct {
	XMLName xml.Name     `xml:"graphml"`
	Xmlns   string       `xml:"xmlns,attr"`
	Keys    []graphmlKey `xml:"key"`
	Graph   graphmlGraph `xml:"graph"`
}

type graphmlKey struct {
	ID       string `xml:"id,attr"`
	For      string `xml:"for,attr"`
	AttrName string `xml:"attr.name,attr"`
	AttrType string `xml:"attr.type,attr"`
}

type graphmlGraph struct {
	ID          string        `xml:"id,attr"`
	EdgeDefault string        `xml:"edgedefault,attr"`
	Nodes       []graphmlNode `xml:"node"`
	Edges       []graphmlEdge `xml:"edge"`
}

type graphmlNode struct {
	ID   string        `xml:"id,attr"`
	Data []graphmlData `xml:"data"`
}

type graphmlEdge struct {
	ID     string        `xml:"id,attr"`
	Source string        `xml:"source,attr"`
	Target string        `xml:"target,attr"`
	Data   []graphmlData `xml:"data"`
}

type graphmlData struct {
	Key   string `xml:"key,attr"`
	Value string `xml:",chardata"`
}

func exportGraphML(nodes []*graph.Node, edges []*graph.Edge, repoRoot string) string {
	prefix := ""
	if repoRoot != "" {
		prefix = repoRoot + "/"
	}

	nodeSet := make(map[graph.NodeID]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}

	gmlNodes := make([]graphmlNode, 0, len(nodes))
	for _, n := range nodes {
		file := strings.TrimPrefix(n.File, prefix)
		gmlNodes = append(gmlNodes, graphmlNode{
			ID: string(n.ID),
			Data: []graphmlData{
				{Key: "d0", Value: string(n.Type)},
				{Key: "d1", Value: n.Name},
				{Key: "d2", Value: n.Package},
				{Key: "d3", Value: file},
				{Key: "d4", Value: fmt.Sprintf("%d", n.Line)},
			},
		})
	}

	gmlEdges := make([]graphmlEdge, 0, len(edges))
	for i, e := range edges {
		if !nodeSet[e.From] || !nodeSet[e.To] {
			continue
		}
		gmlEdges = append(gmlEdges, graphmlEdge{
			ID:     fmt.Sprintf("e%d", i),
			Source: string(e.From),
			Target: string(e.To),
			Data:   []graphmlData{{Key: "d5", Value: string(e.Type)}},
		})
	}

	doc := graphmlDoc{
		Xmlns: "http://graphml.graphdrawing.org/graphml",
		Keys: []graphmlKey{
			{ID: "d0", For: "node", AttrName: "type", AttrType: "string"},
			{ID: "d1", For: "node", AttrName: "name", AttrType: "string"},
			{ID: "d2", For: "node", AttrName: "package", AttrType: "string"},
			{ID: "d3", For: "node", AttrName: "file", AttrType: "string"},
			{ID: "d4", For: "node", AttrName: "line", AttrType: "int"},
			{ID: "d5", For: "edge", AttrName: "type", AttrType: "string"},
		},
		Graph: graphmlGraph{
			ID:          "G",
			EdgeDefault: "directed",
			Nodes:       gmlNodes,
			Edges:       gmlEdges,
		},
	}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Sprintf("<!-- error marshalling GraphML: %v -->", err)
	}
	return xml.Header + string(out)
}
