package parser

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/hcl"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// hclRefPattern matches HCL reference expressions: var.name, local.name,
// module.name.*, data.type.name.*
var hclRefPattern = regexp.MustCompile(`\b(var|local|module|data)\.([\w-]+)(?:\.([\w-]+))?`)

// HCLParser parses HCL/Terraform (.tf, .tfvars, .hcl) source files.
// It extracts resource, data, module, variable, output, locals, provider,
// and terraform blocks into graph nodes with appropriate types and metadata.
type HCLParser struct {
	language *sitter.Language
}

// NewHCLParser creates a ready-to-use HCLParser.
func NewHCLParser() *HCLParser {
	return &HCLParser{language: hcl.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *HCLParser) Extensions() []string {
	return []string{".tf", ".tfvars", ".hcl"}
}

// Parse extracts code entities from a single HCL/Terraform file.
func (p *HCLParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseCtx(context.Background(), nil, src)
	if tree == nil {
		return nil
	}
	root := tree.RootNode()
	if root == nil {
		return nil
	}

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lines := strings.Split(string(src), "\n")

	// Walk root → body → children looking for block nodes.
	var walkBody func(n *sitter.Node)
	walkBody = func(n *sitter.Node) {
		if n == nil {
			return
		}
		for i := 0; i < int(n.ChildCount()); i++ {
			child := n.Child(i)
			if child == nil {
				continue
			}
			switch child.Type() {
			case "block":
				p.handleBlock(g, child, src, filePath, fileNodeID, lines)
			case "body":
				walkBody(child)
			case "attribute":
				// Top-level attributes appear in .tfvars files: key = value
				p.handleTopLevelAttribute(g, child, src, filePath, fileNodeID, lines)
			}
		}
	}
	walkBody(root)

	return nil
}

// handleBlock processes a single HCL block node and emits the appropriate
// graph node(s) based on the block type (resource, data, module, etc.).
func (p *HCLParser) handleBlock(
	g *graph.Graph,
	block *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	// Collect identifiers and string_lit children to determine block type and labels.
	var identifiers []string
	var labels []string
	var bodyNode *sitter.Node

	for i := 0; i < int(block.ChildCount()); i++ {
		child := block.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "identifier":
			identifiers = append(identifiers, childText(child, src))
		case "string_lit":
			raw := childText(child, src)
			labels = append(labels, hclStripQuotes(raw))
		case "body":
			bodyNode = child
		}
	}

	if len(identifiers) == 0 {
		return
	}

	blockType := identifiers[0]
	startLine := int(block.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")
	if doc == "" {
		doc = extractLineDoc(lines, startLine, "//")
	}

	switch blockType {
	case "resource":
		// resource "aws_instance" "web" { ... } → NodeStruct name="aws_instance.web"
		if len(labels) >= 2 {
			name := labels[0] + "." + labels[1]
			nodeID := g.MakeNodeID(filePath, name)
			meta := map[string]string{"kind": "resource"}
			if doc != "" {
				meta["doc"] = doc
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: startLine, Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Extract references to var.x, local.x, module.x etc.
			if bodyNode != nil {
				extractHCLReferences(g, bodyNode, src, filePath, nodeID)
			}
		}

	case "data":
		// data "aws_ami" "ubuntu" { ... } → NodeStruct name="data.aws_ami.ubuntu"
		if len(labels) >= 2 {
			name := "data." + labels[0] + "." + labels[1]
			nodeID := g.MakeNodeID(filePath, name)
			meta := map[string]string{"kind": "data"}
			if doc != "" {
				meta["doc"] = doc
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: startLine, Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Extract references to var.x, local.x, module.x etc.
			if bodyNode != nil {
				extractHCLReferences(g, bodyNode, src, filePath, nodeID)
			}
		}

	case "module":
		// module "vpc" { source = "..." } → NodeStruct name="module.vpc"
		if len(labels) >= 1 {
			name := "module." + labels[0]
			nodeID := g.MakeNodeID(filePath, name)
			meta := map[string]string{"kind": "module"}
			if doc != "" {
				meta["doc"] = doc
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: startLine, Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

			// Scan body for source attribute → EdgeImports.
			if bodyNode != nil {
				if source := hclFindAttribute(bodyNode, src, "source"); source != "" {
					importNodeID := g.MakeNodeID(source, source)
					g.AddNode(&graph.Node{
						ID: importNodeID, Type: graph.NodePackage, Name: source,
						Package: source, File: filePath,
					})
					g.AddEdge(&graph.Edge{From: nodeID, To: importNodeID, Type: graph.EdgeImports})
				}
				// Extract references to var.x, local.x, module.x etc.
				extractHCLReferences(g, bodyNode, src, filePath, nodeID)
			}
		}

	case "variable":
		// variable "region" { ... } → NodeVariable name="var.region"
		if len(labels) >= 1 {
			name := "var." + labels[0]
			nodeID := g.MakeNodeID(filePath, name)
			meta := map[string]string{"kind": "variable"}
			if doc != "" {
				meta["doc"] = doc
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeVariable, Name: name, File: filePath,
				Line: startLine, Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}

	case "output":
		// output "ip" { ... } → NodeVariable name="output.ip"
		if len(labels) >= 1 {
			name := "output." + labels[0]
			nodeID := g.MakeNodeID(filePath, name)
			meta := map[string]string{"kind": "output"}
			if doc != "" {
				meta["doc"] = doc
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeVariable, Name: name, File: filePath,
				Line: startLine, Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Extract references to var.x, local.x, module.x etc.
			if bodyNode != nil {
				extractHCLReferences(g, bodyNode, src, filePath, nodeID)
			}
		}

	case "locals":
		// locals { name = value ... } → one NodeVariable per key inside body.
		if bodyNode != nil {
			p.extractLocals(g, bodyNode, src, filePath, fileNodeID, lines)
		}

	case "provider":
		// provider "aws" { ... } → NodeStruct name="provider.aws"
		if len(labels) >= 1 {
			name := "provider." + labels[0]
			nodeID := g.MakeNodeID(filePath, name)
			meta := map[string]string{"kind": "provider"}
			if doc != "" {
				meta["doc"] = doc
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: startLine, Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}

	case "terraform":
		// terraform { required_providers { ... } } → metadata only, no graph node.
		// We intentionally skip terraform blocks as they are configuration metadata.
	}
}

// extractLocals scans a locals block body for attribute nodes and creates
// a NodeVariable for each key.
func (p *HCLParser) extractLocals(
	g *graph.Graph,
	bodyNode *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	for i := 0; i < int(bodyNode.ChildCount()); i++ {
		child := bodyNode.Child(i)
		if child == nil {
			continue
		}
		if child.Type() == "attribute" {
			// attribute: identifier "=" expression
			ident := firstChildOfType(child, "identifier")
			if ident == nil {
				continue
			}
			key := childText(ident, src)
			if key == "" {
				continue
			}
			name := "local." + key
			startLine := int(child.StartPoint().Row) + 1
			doc := extractLineDoc(lines, startLine, "#")
			if doc == "" {
				doc = extractLineDoc(lines, startLine, "//")
			}
			nodeID := g.MakeNodeID(filePath, name)
			meta := map[string]string{"kind": "local"}
			if doc != "" {
				meta["doc"] = doc
			}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeVariable, Name: name, File: filePath,
				Line: startLine, Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}
}

// handleTopLevelAttribute handles bare key = value assignments in .tfvars files.
// Each top-level attribute becomes a NodeVariable with kind="tfvar".
func (p *HCLParser) handleTopLevelAttribute(
	g *graph.Graph,
	attr *sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	ident := firstChildOfType(attr, "identifier")
	if ident == nil {
		return
	}
	key := childText(ident, src)
	if key == "" {
		return
	}
	startLine := int(attr.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")
	if doc == "" {
		doc = extractLineDoc(lines, startLine, "//")
	}
	nodeID := g.MakeNodeID(filePath, key)
	if g.GetNode(nodeID) != nil {
		return
	}
	meta := map[string]string{"kind": "tfvar"}
	if doc != "" {
		meta["doc"] = doc
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeVariable,
		Name:     key,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// hclStripQuotes removes surrounding double quotes from an HCL string literal.
func hclStripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// hclFindAttribute scans a body node for an attribute with the given key name
// and returns the attribute value as a stripped string. Returns "" if not found.
func hclFindAttribute(bodyNode *sitter.Node, src []byte, key string) string {
	for i := 0; i < int(bodyNode.ChildCount()); i++ {
		child := bodyNode.Child(i)
		if child == nil || child.Type() != "attribute" {
			continue
		}
		ident := firstChildOfType(child, "identifier")
		if ident == nil || childText(ident, src) != key {
			continue
		}
		// The value is in an expression child. Extract the full text and strip quotes.
		// Walk children looking for the expression or template_expr or literal_value.
		for j := 0; j < int(child.ChildCount()); j++ {
			valChild := child.Child(j)
			if valChild == nil {
				continue
			}
			vt := valChild.Type()
			if vt == "identifier" || vt == "=" {
				continue
			}
			// This is the value part. Extract text and strip quotes.
			raw := childText(valChild, src)
			return hclStripQuotes(strings.TrimSpace(raw))
		}
	}
	return ""
}

// extractHCLReferences scans an HCL body node's attribute values for
// reference expressions (var.x, local.x, module.x, data.type.x) and emits
// EdgeCalls from fromNodeID to the referenced entity within the same file.
func extractHCLReferences(
	g *graph.Graph,
	bodyNode *sitter.Node,
	src []byte,
	filePath string,
	fromNodeID graph.NodeID,
) {
	if bodyNode == nil {
		return
	}
	bodyText := childText(bodyNode, src)

	seen := make(map[string]bool)
	for _, m := range hclRefPattern.FindAllStringSubmatch(bodyText, -1) {
		refType := m[1]
		name1 := m[2]
		name2 := m[3] // may be empty

		// Build the canonical node name for the referenced entity.
		var refName string
		switch refType {
		case "var":
			refName = "var." + name1
		case "local":
			refName = "local." + name1
		case "module":
			refName = "module." + name1
		case "data":
			if name2 != "" {
				refName = "data." + name1 + "." + name2
			} else {
				refName = "data." + name1
			}
		default:
			continue
		}

		if seen[refName] || refName == "" {
			continue
		}
		seen[refName] = true

		// Look up the referenced node within the same file.
		refNodeID := g.MakeNodeID(filePath, refName)
		if g.GetNode(refNodeID) == nil {
			continue // Referenced entity not in this file — skip
		}

		// Avoid self-references.
		if refNodeID == fromNodeID {
			continue
		}

		g.AddEdge(&graph.Edge{From: fromNodeID, To: refNodeID, Type: graph.EdgeCalls})
	}
}
