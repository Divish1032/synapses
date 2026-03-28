package parser

import (
	"path/filepath"
	"strings"

	zigg "github.com/alexaandru/go-sitter-forest/zig"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ZigParser parses Zig (.zig) source files.
type ZigParser struct {
	language *sitter.Language
}

// NewZigParser creates a ready-to-use ZigParser.
func NewZigParser() *ZigParser {
	return &ZigParser{language: sitter.NewLanguage(zigg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *ZigParser) Extensions() []string {
	return []string{".zig"}
}

// TSLanguageForFile returns the tree-sitter language for this parser.
func (p *ZigParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// isZigPub returns true if the node has a direct child with type "pub".
func isZigPub(n sitter.Node) bool {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "pub" {
			return true
		}
	}
	return false
}

// zigReturnType extracts the return type text from a function_declaration node.
// After the parameters node, we look for a type node (builtin_type, identifier,
// optional_type, error_union, etc.) and return its text. Punctuation is skipped.
func zigReturnType(n sitter.Node, src []byte) string {
	sawParams := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		if ct == "parameters" {
			sawParams = true
			continue
		}
		if !sawParams {
			continue
		}
		// Skip punctuation / keywords that appear between params and return type.
		switch ct {
		case "fn", "pub", "identifier", "block", "!", "?", ":", ";", ",", ")", "(":
			// "identifier" before params is the function name — skip
		}
		// Skip purely syntactic tokens (single char operators).
		if len(ct) == 1 {
			continue
		}
		switch ct {
		case "fn", "pub", "block", "parameters":
			continue
		case "builtin_type", "identifier", "optional_type", "error_union",
			"pointer_type", "array_type", "anyframe_type", "error_type",
			"type_expr", "var_type":
			text := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			if text != "" {
				return text
			}
		}
	}
	return ""
}

// zigParamsText extracts raw text of the parameters node, truncated to 80 chars.
func zigParamsText(n sitter.Node, src []byte) string {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "parameters" {
			text := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			if len(text) > 80 {
				text = text[:80]
			}
			return text
		}
	}
	return ""
}

// zigEnumValues collects container_field names from an enum_declaration node.
func zigEnumValues(enumDecl sitter.Node, src []byte) string {
	var names []string
	for i := uint32(0); i < enumDecl.ChildCount(); i++ {
		child := enumDecl.Child(i)
		if child.IsNull() || child.Type() != "container_field" {
			continue
		}
		// The identifier is the first identifier child of container_field.
		for j := uint32(0); j < child.ChildCount(); j++ {
			subchild := child.Child(j)
			if !subchild.IsNull() && subchild.Type() == "identifier" {
				names = append(names, string(src[subchild.StartByte():subchild.EndByte()]))
				break
			}
		}
	}
	result := strings.Join(names, ", ")
	if len(result) > 80 {
		result = result[:80]
	}
	return result
}

// zigErrorSetValues collects identifier children from an error_set_declaration node.
func zigErrorSetValues(errorDecl sitter.Node, src []byte) string {
	var names []string
	for i := uint32(0); i < errorDecl.ChildCount(); i++ {
		child := errorDecl.Child(i)
		if child.IsNull() || child.Type() != "identifier" {
			continue
		}
		names = append(names, string(src[child.StartByte():child.EndByte()]))
	}
	result := strings.Join(names, ", ")
	if len(result) > 80 {
		result = result[:80]
	}
	return result
}

// extractZigStructMethods walks a struct_declaration and extracts function_declaration
// children as methods qualified as StructName.methodName.
func extractZigStructMethods(
	g *graph.Graph,
	structDecl sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	structName string,
	structNodeID graph.NodeID,
) {
	for i := uint32(0); i < structDecl.ChildCount(); i++ {
		child := structDecl.Child(i)
		if child.IsNull() || child.Type() != "function_declaration" {
			continue
		}
		// Get method name.
		nameNode := sitter.Node{}
		for j := uint32(0); j < child.ChildCount(); j++ {
			sub := child.Child(j)
			if !sub.IsNull() && sub.Type() == "identifier" {
				nameNode = sub
				break
			}
		}
		if nameNode.IsNull() {
			continue
		}
		methodName := string(src[nameNode.StartByte():nameNode.EndByte()])
		qualName := structName + "." + methodName

		params := zigParamsText(child, src)
		retType := zigReturnType(child, src)
		meta := map[string]string{}
		if params != "" {
			meta["params"] = params
		}
		if retType != "" {
			meta["return_type"] = retType
		}

		methodNodeID := g.MakeNodeID(filePath, qualName)
		g.AddNode(&graph.Node{
			ID:       methodNodeID,
			Type:     graph.NodeMethod,
			Name:     qualName,
			File:     filePath,
			Line:     int(child.StartPoint().Row) + 1,
			Exported: isZigPub(child),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: methodNodeID, Type: graph.EdgeDefines})
		g.AddEdge(&graph.Edge{From: structNodeID, To: methodNodeID, Type: graph.EdgeDefines})
	}
}

// Parse extracts code entities from a single Zig file and merges them into the graph.
func (p *ZigParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree != nil {
		defer tree.Close()
	}
	if tree == nil {
		return nil
	}
	root := tree.RootNode()

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	// Walk the root source_file children.
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "function_declaration":
			// Top-level function.
			nameNode := sitter.Node{}
			for j := uint32(0); j < child.ChildCount(); j++ {
				sub := child.Child(j)
				if !sub.IsNull() && sub.Type() == "identifier" {
					nameNode = sub
					break
				}
			}
			if nameNode.IsNull() {
				continue
			}
			funcName := string(src[nameNode.StartByte():nameNode.EndByte()])
			params := zigParamsText(child, src)
			retType := zigReturnType(child, src)
			meta := map[string]string{"kind": "function"}
			if params != "" {
				meta["params"] = params
			}
			if retType != "" {
				meta["return_type"] = retType
			}
			nodeID := g.MakeNodeID(filePath, funcName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     funcName,
				File:     filePath,
				Line:     int(child.StartPoint().Row) + 1,
				Exported: isZigPub(child),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "variable_declaration":
			// const/var declarations — could be struct, enum, error_set, or import.
			p.extractZigVarDecl(g, child, src, filePath, fileNodeID)

		case "test_declaration":
			// test "name" { ... }
			testName := ""
			for j := uint32(0); j < child.ChildCount(); j++ {
				sub := child.Child(j)
				if sub.IsNull() {
					continue
				}
				if sub.Type() == "string" {
					raw := string(src[sub.StartByte():sub.EndByte()])
					// Strip surrounding quotes.
					testName = strings.Trim(raw, `"`)
					break
				}
			}
			if testName == "" {
				testName = "anonymous"
			}
			qualName := "test_" + testName
			meta := map[string]string{"kind": "test"}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     qualName,
				File:     filePath,
				Line:     int(child.StartPoint().Row) + 1,
				Exported: false,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	return nil
}

// extractZigVarDecl handles variable_declaration nodes at the root level.
func (p *ZigParser) extractZigVarDecl(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// Find the identifier (the binding name).
	nameNode := sitter.Node{}
	isPub := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "pub" {
			isPub = true
		}
		if child.Type() == "identifier" {
			nameNode = child
			break
		}
	}
	if nameNode.IsNull() {
		return
	}
	bindingName := string(src[nameNode.StartByte():nameNode.EndByte()])

	// Find the value node (after "=").
	// In Zig AST, value is the node that follows "=" in a var decl.
	// We look for the first non-trivial node that is a value expression.
	// The children are typically: [pub] [const/var] identifier [: type] = value ;
	sawEquals := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		if ct == "=" {
			sawEquals = true
			continue
		}
		if !sawEquals {
			continue
		}
		// child is the value node.
		switch ct {
		case "struct_declaration":
			nodeID := g.MakeNodeID(filePath, bindingName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     bindingName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isPub,
				Metadata: map[string]string{"kind": "struct"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Extract methods from struct body.
			extractZigStructMethods(g, child, src, filePath, fileNodeID, bindingName, nodeID)
			return

		case "enum_declaration":
			values := zigEnumValues(child, src)
			meta := map[string]string{"kind": "enum"}
			if values != "" {
				meta["values"] = values
			}
			nodeID := g.MakeNodeID(filePath, bindingName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     bindingName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isPub,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			extractZigStructMethods(g, child, src, filePath, fileNodeID, bindingName, nodeID)
			return

		case "union_declaration":
			nodeID := g.MakeNodeID(filePath, bindingName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     bindingName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isPub,
				Metadata: map[string]string{"kind": "union"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			extractZigStructMethods(g, child, src, filePath, fileNodeID, bindingName, nodeID)
			return

		case "error_set_declaration":
			errors := zigErrorSetValues(child, src)
			meta := map[string]string{"kind": "error_set"}
			if errors != "" {
				meta["errors"] = errors
			}
			nodeID := g.MakeNodeID(filePath, bindingName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     bindingName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isPub,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			return

		case "builtin_function":
			// @import("path") → import.
			// Check if the builtin function starts with @import.
			builtinText := string(src[child.StartByte():child.EndByte()])
			if !strings.HasPrefix(builtinText, "@import") {
				return
			}
			// Extract the string argument to @import.
			importPath := ""
			for j := uint32(0); j < child.ChildCount(); j++ {
				sub := child.Child(j)
				if sub.IsNull() {
					continue
				}
				st := sub.Type()
				// Try common string node type names across grammar versions.
				if st == "string" || st == "string_literal" || st == "string_literal_single" {
					raw := string(src[sub.StartByte():sub.EndByte()])
					importPath = strings.Trim(raw, `"`)
					break
				}
			}
			if importPath == "" {
				// Fallback: extract path from @import("...") text.
				if idx := strings.Index(builtinText, `"`); idx >= 0 {
					end := strings.LastIndex(builtinText, `"`)
					if end > idx {
						importPath = builtinText[idx+1 : end]
					}
				}
			}
			if importPath == "" {
				importPath = bindingName
			}
			meta := map[string]string{"path": importPath}
			importNodeID := g.MakeNodeID(filePath, bindingName)
			g.AddNode(&graph.Node{
				ID:       importNodeID,
				Type:     graph.NodePackage,
				Name:     bindingName,
				Package:  importPath,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeDependsOn})
			return

		default:
			// Plain variable/constant (literal, type alias, comptime block, field access, etc.)
			nodeID := g.MakeNodeID(filePath, bindingName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     bindingName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isPub,
				Metadata: map[string]string{"kind": "variable"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			return
		}
	}
}
