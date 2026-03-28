package parser

import (
	"path/filepath"
	"strings"

	"github.com/alexaandru/go-sitter-forest/hcl"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TerraformParser parses HashiCorp Configuration Language (.tf) files.
// It extracts Terraform resources, data sources, modules, variables, outputs,
// locals, and providers as graph nodes, and records TerraformRefs for
// cross-file DEPENDS_ON resolution (resolved post-parse by ResolveTerraformRefs).
type TerraformParser struct {
	language *sitter.Language
}

// NewTerraformParser creates a ready-to-use TerraformParser.
func NewTerraformParser() *TerraformParser {
	return &TerraformParser{language: sitter.NewLanguage(hcl.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *TerraformParser) Extensions() []string {
	return []string{".tf", ".tf.json"}
}

// TSLanguageForFile implements TreeSitterLanguageProvider so the watcher can
// pre-check .tf files for parse errors during active editing.
func (p *TerraformParser) TSLanguageForFile(_ string) *sitter.Language {
	return p.language
}

// Parse extracts Terraform entities from a single .tf file and merges them into g.
//
// Node mapping:
//   - resource  → NodeStruct   (name: "resource_type.resource_name")
//   - data      → NodeStruct   (name: "data.resource_type.resource_name")
//   - module    → NodePackage  (name: "module.label")
//   - variable  → NodeVariable (name: "var.label")
//   - output    → NodeVariable (name: "output.label")
//   - locals    → NodeVariable (name: "local.key") per key in the block
//   - provider  → NodeStruct   (name: "provider.label")
//
// All nodes carry Domain=DomainInfra and metadata domain="terraform", kind=<type>.
// Resource references found in attribute expressions are recorded as TerraformRefs
// for cross-file DEPENDS_ON resolution by ResolveTerraformRefs (resolver package).
func (p *TerraformParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()

	tree, err := parser.ParseString(parseCtx, nil, src)
	if err != nil || tree == nil {
		return nil // tree-sitter always returns a tree; nil is a safety guard
	}
	defer tree.Close()

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

	// resourceNodes tracks the nodeIDs of resource/data/module blocks in this
	// file, keyed by their canonical ref name ("type.name", "data.type.name",
	// "module.name"). Used when collecting refs: we emit a TerraformRef for
	// every expression reference, and the global resolver resolves them.
	resourceNodes := make(map[string]graph.NodeID)

	// First pass: create all nodes and record resource nodeIDs.
	for i := uint32(0); i < body.ChildCount(); i++ {
		block := body.Child(i)
		if block.IsNull() || block.Type() != "block" {
			continue
		}

		blockTypeNode := block.Child(0)
		if blockTypeNode.IsNull() || blockTypeNode.Type() != "identifier" {
			continue
		}
		blockType := string(src[blockTypeNode.StartByte():blockTypeNode.EndByte()])
		line := int(blockTypeNode.StartPoint().Row) + 1

		switch blockType {
		case "resource":
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
			resourceNodes[name] = nodeID
			// Collect refs from body for cross-file resolution.
			if bodyNode := tfFindChildNode(block, "body"); !bodyNode.IsNull() {
				p.collectAndEmitRefs(g, filePath, nodeID, bodyNode, src)
			}

		case "data":
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
			resourceNodes[name] = nodeID
			if bodyNode := tfFindChildNode(block, "body"); !bodyNode.IsNull() {
				p.collectAndEmitRefs(g, filePath, nodeID, bodyNode, src)
			}

		case "module":
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
			resourceNodes[name] = nodeID
			if bodyNode := tfFindChildNode(block, "body"); !bodyNode.IsNull() {
				p.collectAndEmitRefs(g, filePath, nodeID, bodyNode, src)
			}

		case "variable":
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

		case "locals":
			// locals { key = value ... } — each key becomes a "local.key" node.
			// Unlike other blocks, locals has no label; its body attributes ARE the locals.
			bodyNode := tfFindChildNode(block, "body")
			if bodyNode.IsNull() {
				continue
			}
			for j := uint32(0); j < bodyNode.ChildCount(); j++ {
				attr := bodyNode.Child(j)
				if attr.IsNull() || attr.Type() != "attribute" {
					continue
				}
				keyNode := attr.Child(0)
				if keyNode.IsNull() || keyNode.Type() != "identifier" {
					continue
				}
				key := string(src[keyNode.StartByte():keyNode.EndByte()])
				if key == "" {
					continue
				}
				name := "local." + key
				nodeID := g.MakeNodeID(filePath, name)
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeVariable,
					Name:     name,
					Package:  "terraform",
					File:     filePath,
					Line:     int(keyNode.StartPoint().Row) + 1,
					Exported: false, // locals are module-scoped, not exported
					Domain:   graph.DomainInfra,
					Metadata: map[string]string{"domain": "terraform", "kind": "local"},
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "provider":
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

	_ = resourceNodes // tracked for potential future use
	return nil
}

// collectAndEmitRefs walks bodyNode collecting all Terraform resource references
// and emits them as TerraformRefs on the graph for cross-file resolution.
// fromID is the node that contains these references (the depending resource).
func (p *TerraformParser) collectAndEmitRefs(
	g *graph.Graph,
	filePath string,
	fromID graph.NodeID,
	bodyNode sitter.Node,
	src []byte,
) {
	refs := tfCollectRefs(bodyNode, src)
	for ref := range refs {
		g.AddTerraformRef(graph.TerraformRef{
			FromID:   fromID,
			FromFile: filePath,
			RefName:  ref,
		})
	}
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
				secondAttr := tfGetAttrNameNode(src, getAttrs[1])
				if firstAttr != "" && secondAttr != "" {
					refs["data."+firstAttr+"."+secondAttr] = true
				}
			} else if !tfIsBuiltinNamespace(baseText) && firstAttr != "" {
				refs[baseText+"."+firstAttr] = true
			}
		}
	}

	for i := uint32(0); i < node.ChildCount(); i++ {
		tfWalkRefs(node.Child(i), src, refs)
	}
}

// tfGetAttrNameNode extracts the identifier name from a get_attr node.
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
// references. "module" is intentionally NOT excluded — module.vpc refs create
// cross-resource edges.
func tfIsBuiltinNamespace(name string) bool {
	switch name {
	case "var", "local", "locals", "path", "self", "terraform", "each", "count":
		return true
	}
	return false
}
