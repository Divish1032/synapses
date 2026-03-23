package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/hcl"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TerraformParser parses HashiCorp Configuration Language (.tf) files.
// It extracts Terraform resources, data sources, modules, variables, outputs,
// and providers as graph nodes, and emits DEPENDS_ON edges for within-file
// resource references discovered in expressions.
type TerraformParser struct {
	language *sitter.Language
}

// NewTerraformParser creates a ready-to-use TerraformParser.
func NewTerraformParser() *TerraformParser {
	return &TerraformParser{language: sitter.NewLanguage(hcl.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *TerraformParser) Extensions() []string {
	return []string{".tf"}
}

// TSLanguageForFile implements TreeSitterLanguageProvider so the watcher can
// pre-check .tf files for parse errors during active editing.
func (p *TerraformParser) TSLanguageForFile(_ string) *sitter.Language {
	return p.language
}

// tfResourceInfo holds per-resource data needed for the DEPENDS_ON second pass.
type tfResourceInfo struct {
	nodeID   graph.NodeID
	bodyNode sitter.Node
}

// Parse extracts Terraform entities from a single .tf file and merges them into g.
//
// Node mapping:
//   - resource  → NodeStruct   (name: "resource_type.resource_name")
//   - data      → NodeStruct   (name: "data.resource_type.resource_name")
//   - module    → NodePackage  (name: "module.label")
//   - variable  → NodeVariable (name: "var.label")
//   - output    → NodeVariable (name: "output.label")
//   - provider  → NodeStruct   (name: "provider.label")
//
// All nodes carry metadata: domain="terraform", kind=<block type>.
// DEPENDS_ON edges are emitted for within-file resource references found in
// attribute expressions (both explicit depends_on lists and inline references).
func (p *TerraformParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()

	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree == nil {
		return nil
	}
	root := tree.RootNode()

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:     fileNodeID,
		Type:   graph.NodeFile,
		Name:   filepath.Base(filePath),
		File:   filePath,
		Line:   1,
		Domain: graph.DomainInfra,
	})

	// Locate the top-level body node (config_file → body).
	body := tfFindChildNode(root, "body")
	if body.IsNull() {
		return nil
	}

	// knownResources maps "type.name" (for resources) and "data.type.name" (for data)
	// to their graph node ID and body AST node. Used in the second pass for
	// within-file DEPENDS_ON edge resolution.
	knownResources := make(map[string]tfResourceInfo)

	// First pass: create all nodes.
	for i := uint32(0); i < body.ChildCount(); i++ {
		block := body.Child(i)
		if block.IsNull() || block.Type() != "block" {
			continue
		}

		// Child 0 of a block is always the block-type identifier.
		blockTypeNode := block.Child(0)
		if blockTypeNode.IsNull() || blockTypeNode.Type() != "identifier" {
			continue
		}
		blockType := string(src[blockTypeNode.StartByte():blockTypeNode.EndByte()])
		line := int(blockTypeNode.StartPoint().Row) + 1

		switch blockType {
		case "resource":
			// resource "TYPE" "NAME" { ... }
			// Children: identifier(0) string_lit(1) string_lit(2) block_start(3) body(4) block_end(5)
			rType := tfExtractLabelNode(src, block, 1)
			rName := tfExtractLabelNode(src, block, 2)
			if rType == "" || rName == "" {
				continue
			}
			name := rType + "." + rName
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				Package:  "terraform",
				File:     filePath,
				Line:     line,
				Exported: true,
				Domain:   graph.DomainInfra,
				Metadata: map[string]string{
					"domain":        "terraform",
					"kind":          "resource",
					"resource_type": rType,
					"resource_name": rName,
				},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if bodyNode := tfFindChildNode(block, "body"); !bodyNode.IsNull() {
				knownResources[name] = tfResourceInfo{nodeID: nodeID, bodyNode: bodyNode}
			}

		case "data":
			// data "TYPE" "NAME" { ... }
			rType := tfExtractLabelNode(src, block, 1)
			rName := tfExtractLabelNode(src, block, 2)
			if rType == "" || rName == "" {
				continue
			}
			name := "data." + rType + "." + rName
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				Package:  "terraform",
				File:     filePath,
				Line:     line,
				Exported: true,
				Domain:   graph.DomainInfra,
				Metadata: map[string]string{
					"domain":        "terraform",
					"kind":          "data",
					"resource_type": rType,
					"resource_name": rName,
				},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if bodyNode := tfFindChildNode(block, "body"); !bodyNode.IsNull() {
				knownResources[name] = tfResourceInfo{nodeID: nodeID, bodyNode: bodyNode}
			}

		case "module":
			// module "NAME" { ... }
			label := tfExtractLabelNode(src, block, 1)
			if label == "" {
				continue
			}
			name := "module." + label
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodePackage,
				Name:     name,
				Package:  "terraform",
				File:     filePath,
				Line:     line,
				Exported: true,
				Domain:   graph.DomainInfra,
				Metadata: map[string]string{"domain": "terraform", "kind": "module"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Modules can reference resources; include in knownResources.
			if bodyNode := tfFindChildNode(block, "body"); !bodyNode.IsNull() {
				knownResources[name] = tfResourceInfo{nodeID: nodeID, bodyNode: bodyNode}
			}

		case "variable":
			// variable "NAME" { ... }
			label := tfExtractLabelNode(src, block, 1)
			if label == "" {
				continue
			}
			name := "var." + label
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     name,
				Package:  "terraform",
				File:     filePath,
				Line:     line,
				Exported: true,
				Domain:   graph.DomainInfra,
				Metadata: map[string]string{"domain": "terraform", "kind": "variable"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "output":
			// output "NAME" { ... }
			label := tfExtractLabelNode(src, block, 1)
			if label == "" {
				continue
			}
			name := "output." + label
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     name,
				Package:  "terraform",
				File:     filePath,
				Line:     line,
				Exported: true,
				Domain:   graph.DomainInfra,
				Metadata: map[string]string{"domain": "terraform", "kind": "output"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "provider":
			// provider "NAME" { ... }
			label := tfExtractLabelNode(src, block, 1)
			if label == "" {
				continue
			}
			name := "provider." + label
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				Package:  "terraform",
				File:     filePath,
				Line:     line,
				Exported: true,
				Domain:   graph.DomainInfra,
				Metadata: map[string]string{"domain": "terraform", "kind": "provider"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	// Second pass: emit DEPENDS_ON edges from within-file resource references.
	// This covers both explicit `depends_on = [...]` and inline attribute references
	// like `subnet_id = aws_subnet.main.id`.
	for _, info := range knownResources {
		refs := tfCollectRefs(info.bodyNode, src)
		for ref := range refs {
			target, ok := knownResources[ref]
			if !ok || target.nodeID == info.nodeID {
				continue
			}
			g.AddEdge(&graph.Edge{
				From: info.nodeID,
				To:   target.nodeID,
				Type: graph.EdgeDependsOn,
			})
		}
	}

	return nil
}

// tfFindChildNode returns the first direct child of node whose type matches typ,
// or a null node if none is found.
func tfFindChildNode(node sitter.Node, typ string) sitter.Node {
	for i := uint32(0); i < node.ChildCount(); i++ {
		child := node.Child(i)
		if !child.IsNull() && child.Type() == typ {
			return child
		}
	}
	return sitter.Node{}
}

// tfExtractLabelNode extracts and unquotes the text of the child at childIdx
// within a block node. Block labels are string_lit nodes whose source text
// includes surrounding double quotes (e.g. `"aws_instance"`). Returns "" if
// the child is absent or produces an empty string after unquoting.
func tfExtractLabelNode(src []byte, block sitter.Node, childIdx uint32) string {
	if childIdx >= block.ChildCount() {
		return ""
	}
	child := block.Child(childIdx)
	if child.IsNull() {
		return ""
	}
	text := string(src[child.StartByte():child.EndByte()])
	text = strings.TrimSpace(text)
	text = strings.Trim(text, `"`)
	return text
}

// tfCollectRefs walks an AST subtree and returns all within-file resource
// references found in expressions. It understands:
//
//   - Regular resource refs: `aws_instance.web` (variable_expr + first get_attr sibling)
//   - Data source refs:      `data.aws_ami.ubuntu` (variable_expr("data") + 2 get_attr siblings)
//   - Module refs:           `module.vpc` captured as "module.vpc"
//
// It ignores `var.*`, `local.*`, `path.*`, `self.*`, and `terraform.*` because
// those are not resource node references.
//
// HCL grammar structure (critical): attribute access chains are FLAT siblings
// under `expression`, NOT nested get_attr nodes. For example:
//
//	aws_instance.web.public_ip → expression{ variable_expr, get_attr(.web), get_attr(.public_ip) }
func tfCollectRefs(node sitter.Node, src []byte) map[string]bool {
	refs := make(map[string]bool)
	tfWalkRefs(node, src, refs)
	return refs
}

// tfWalkRefs recursively walks the AST emitting resource references into refs.
func tfWalkRefs(node sitter.Node, src []byte, refs map[string]bool) {
	if node.IsNull() {
		return
	}

	if node.Type() == "expression" {
		// Scan direct children for the flat get_attr chain pattern.
		// Structure: variable_expr, get_attr, get_attr, ...
		var varExpr sitter.Node
		var getAttrs []sitter.Node

		for i := uint32(0); i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child.IsNull() {
				continue
			}
			switch child.Type() {
			case "variable_expr":
				varExpr = child
			case "get_attr":
				getAttrs = append(getAttrs, child)
			}
		}

		if !varExpr.IsNull() && len(getAttrs) >= 1 {
			baseText := string(src[varExpr.StartByte():varExpr.EndByte()])
			firstAttr := tfGetAttrNameNode(src, getAttrs[0])

			if baseText == "data" && len(getAttrs) >= 2 {
				// data.TYPE.NAME — need two get_attr levels
				secondAttr := tfGetAttrNameNode(src, getAttrs[1])
				if firstAttr != "" && secondAttr != "" {
					refs["data."+firstAttr+"."+secondAttr] = true
				}
			} else if !tfIsBuiltinNamespace(baseText) && firstAttr != "" {
				// resource_type.resource_name or module.name
				refs[baseText+"."+firstAttr] = true
			}
		}
		// Always descend — sub-expressions (collection_value, tuple, etc.) may
		// contain additional resource references that are their own expression nodes.
	}

	for i := uint32(0); i < node.ChildCount(); i++ {
		tfWalkRefs(node.Child(i), src, refs)
	}
}

// tfGetAttrNameNode extracts the identifier name from a get_attr node.
// In HCL grammar, get_attr has children: "." and identifier.
// Returns "" if no identifier child is found.
func tfGetAttrNameNode(src []byte, getAttr sitter.Node) string {
	for i := uint32(0); i < getAttr.ChildCount(); i++ {
		child := getAttr.Child(i)
		if !child.IsNull() && child.Type() == "identifier" {
			return string(src[child.StartByte():child.EndByte()])
		}
	}
	return ""
}

// tfIsBuiltinNamespace returns true for HCL namespaces that are not resource
// references. These appear as the root identifier in attribute access chains
// but do not correspond to graph nodes.
func tfIsBuiltinNamespace(name string) bool {
	switch name {
	case "var", "local", "locals", "path", "self", "terraform", "each", "count":
		return true
	}
	return false
}
