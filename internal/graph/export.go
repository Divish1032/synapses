package graph

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// ExportDOT serialises nodes and edges as a Graphviz DOT digraph.
// repoRoot is stripped from file paths for readability; pass "" to skip.
// includeMeta adds signature metadata to node labels when present.
func ExportDOT(nodes []*Node, edges []*Edge, repoRoot string, includeMeta bool) string {
	var b strings.Builder
	prefix := ""
	if repoRoot != "" {
		prefix = repoRoot + "/"
	}

	nodeSet := make(map[NodeID]bool, len(nodes))
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

// ExportMermaid serialises nodes and edges as a Mermaid LR flowchart.
func ExportMermaid(nodes []*Node, edges []*Edge, repoRoot string, includeMeta bool) string {
	var b strings.Builder

	nodeSet := make(map[NodeID]bool, len(nodes))
	for _, n := range nodes {
		nodeSet[n.ID] = true
	}
	usedTypes := make(map[NodeType]bool)

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

	b.WriteString("\n")
	if usedTypes[NodeFunction] || usedTypes[NodeMethod] {
		b.WriteString("  classDef funcStyle fill:#ddf,stroke:#339,color:#000\n")
	}
	if usedTypes[NodeStruct] {
		b.WriteString("  classDef structStyle fill:#dfd,stroke:#393,color:#000\n")
	}
	if usedTypes[NodeInterface] {
		b.WriteString("  classDef ifaceStyle fill:#fdf,stroke:#939,color:#000\n")
	}
	if usedTypes[NodeVariable] {
		b.WriteString("  classDef varStyle fill:#ffd,stroke:#993,color:#000\n")
	}

	return b.String()
}

// ExportGraphML serialises nodes and edges as GraphML XML.
func ExportGraphML(nodes []*Node, edges []*Edge, repoRoot string) string {
	prefix := ""
	if repoRoot != "" {
		prefix = repoRoot + "/"
	}

	nodeSet := make(map[NodeID]bool, len(nodes))
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

// ── DOT helpers ──────────────────────────────────────────────────────────────

func dotNodeID(id NodeID) string {
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

func dotShape(t NodeType) string {
	switch t {
	case NodeFunction, NodeMethod:
		return "ellipse"
	case NodeStruct:
		return "box"
	case NodeInterface:
		return "diamond"
	case NodeVariable:
		return "note"
	default:
		return "box"
	}
}

func dotNodeColor(t NodeType) string {
	switch t {
	case NodeFunction:
		return "navy"
	case NodeMethod:
		return "blue"
	case NodeStruct:
		return "darkgreen"
	case NodeInterface:
		return "purple"
	case NodeVariable:
		return "gray"
	default:
		return "black"
	}
}

func dotEdgeColor(t EdgeType) string {
	switch t {
	case EdgeCalls:
		return "navy"
	case EdgeImplements:
		return "purple"
	case EdgeEmbeds:
		return "darkgreen"
	case EdgeImports:
		return "gray"
	case EdgeDependsOn:
		return "orange"
	default:
		return "black"
	}
}

// ── Mermaid helpers ───────────────────────────────────────────────────────────

func mermaidID(id NodeID) string {
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

func mermaidClass(t NodeType) string {
	switch t {
	case NodeFunction, NodeMethod:
		return "funcStyle"
	case NodeStruct:
		return "structStyle"
	case NodeInterface:
		return "ifaceStyle"
	case NodeVariable:
		return "varStyle"
	default:
		return "funcStyle"
	}
}

// ── GraphML types ─────────────────────────────────────────────────────────────

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
