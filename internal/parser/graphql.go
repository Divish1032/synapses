package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	graphqlg "github.com/alexaandru/go-sitter-forest/graphql"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// GraphQLParser parses GraphQL (.graphql, .gql) schema and operation files
// using tree-sitter. Extracts type definitions, fields, enums, interfaces,
// unions, scalars, fragments, and directive definitions.
type GraphQLParser struct {
	language *sitter.Language
}

// NewGraphQLParser creates a ready-to-use GraphQLParser.
func NewGraphQLParser() *GraphQLParser {
	return &GraphQLParser{language: sitter.NewLanguage(graphqlg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *GraphQLParser) Extensions() []string {
	return []string{".graphql", ".gql"}
}

// Parse extracts code entities from a single GraphQL file and merges them into the graph.
func (p *GraphQLParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
	defer tree.Close()
	root := tree.RootNode()

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	// The go-sitter-forest GraphQL grammar wraps definitions:
	//   root → document → definition → type_system_definition → type_definition → actual_node
	// We recursively unwrap to find the actual definition nodes.
	p.walkDefinitions(g, root, src, filePath, fileNodeID)

	return nil
}

// walkDefinitions recursively unwraps the grammar's nested wrapper nodes
// (document, definition, type_system_definition, executable_definition,
// type_definition) to find the actual definition nodes and dispatch them.
func (p *GraphQLParser) walkDefinitions(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "object_type_definition":
			p.extractObjectType(g, child, src, filePath, fileNodeID, "type")
		case "input_object_type_definition":
			p.extractObjectType(g, child, src, filePath, fileNodeID, "input")
		case "interface_type_definition":
			p.extractObjectType(g, child, src, filePath, fileNodeID, "interface")
		case "enum_type_definition":
			p.extractEnumType(g, child, src, filePath, fileNodeID)
		case "union_type_definition":
			p.extractUnionType(g, child, src, filePath, fileNodeID)
		case "scalar_type_definition":
			p.extractScalarType(g, child, src, filePath, fileNodeID)
		case "schema_definition":
			p.extractSchemaDefinition(g, child, src, filePath, fileNodeID)
		case "fragment_definition":
			p.extractFragment(g, child, src, filePath, fileNodeID)
		case "directive_definition":
			p.extractDirectiveDefinition(g, child, src, filePath, fileNodeID)
		case "document", "definition", "type_system_definition",
			"executable_definition", "type_definition":
			// Wrapper nodes — recurse to find actual definitions.
			p.walkDefinitions(g, child, src, filePath, fileNodeID)
		}
	}
}

// extractObjectType handles object_type_definition, input_object_type_definition,
// and interface_type_definition. All three share the same structure: a name child
// and an optional fields_definition containing field_definition children.
func (p *GraphQLParser) extractObjectType(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, kind string,
) {
	name := graphqlNodeName(n, src)
	if name == "" {
		return
	}

	nodeType := graph.NodeStruct
	if kind == "interface" {
		nodeType = graph.NodeInterface
	}

	meta := map[string]string{"kind": kind}
	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     nodeType,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

	// Extract implements_interfaces → EdgeImplements edges.
	p.extractImplements(g, n, src, filePath, nodeID)

	// Extract fields from fields_definition.
	p.extractFields(g, n, src, filePath, fileNodeID, nodeID, name)
}

// extractEnumType handles enum_type_definition nodes.
func (p *GraphQLParser) extractEnumType(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	name := graphqlNodeName(n, src)
	if name == "" {
		return
	}

	// Collect enum values for metadata.
	var values []string
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "enum_values_definition" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			ev := child.Child(j)
			if ev.IsNull() || ev.Type() != "enum_value_definition" {
				continue
			}
			evName := graphqlNodeName(ev, src)
			if evName == "" {
				// Enum value names may be direct text children.
				evName = graphqlFirstIdentifierText(ev, src)
			}
			if evName != "" {
				values = append(values, evName)
			}
		}
	}

	meta := map[string]string{"kind": "enum"}
	if len(values) > 0 {
		meta["values"] = strings.Join(values, ", ")
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractUnionType handles union_type_definition nodes.
func (p *GraphQLParser) extractUnionType(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	name := graphqlNodeName(n, src)
	if name == "" {
		return
	}

	// Collect member type names for metadata.
	var members []string
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "union_member_types" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			mt := child.Child(j)
			if mt.IsNull() {
				continue
			}
			// Union members are named_type or type nodes.
			mtName := graphqlExtractTypeName(mt, src)
			if mtName != "" {
				members = append(members, mtName)
			}
		}
	}

	meta := map[string]string{"kind": "union"}
	if len(members) > 0 {
		meta["members"] = strings.Join(members, " | ")
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

	// Create EdgeDependsOn edges to member types (resolved by name).
	for _, member := range members {
		memberID := g.MakeNodeID(filePath, member)
		if g.GetNode(memberID) != nil {
			g.AddEdge(&graph.Edge{From: nodeID, To: memberID, Type: graph.EdgeDependsOn})
		}
	}
}

// extractScalarType handles scalar_type_definition nodes.
func (p *GraphQLParser) extractScalarType(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	name := graphqlNodeName(n, src)
	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: map[string]string{"kind": "scalar"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractSchemaDefinition handles schema_definition nodes.
func (p *GraphQLParser) extractSchemaDefinition(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	nodeID := g.MakeNodeID(filePath, "schema")
	meta := map[string]string{"kind": "schema"}

	// Extract root operation types (query, mutation, subscription).
	// Grammar structure: root_operation_type_definition → operation_type + named_type
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "root_operation_type_definition" {
			continue
		}
		var opName, typeName string
		for j := uint32(0); j < child.ChildCount(); j++ {
			gc := child.Child(j)
			if gc.IsNull() {
				continue
			}
			switch gc.Type() {
			case "operation_type":
				opName = strings.TrimSpace(string(src[gc.StartByte():gc.EndByte()]))
			case "named_type":
				typeName = graphqlNodeName(gc, src)
				if typeName == "" {
					typeName = strings.TrimSpace(string(src[gc.StartByte():gc.EndByte()]))
				}
			}
		}
		if opName != "" && typeName != "" {
			meta[opName] = typeName
		}
	}

	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeStruct,
		Name:     "schema",
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractFragment handles fragment_definition nodes.
func (p *GraphQLParser) extractFragment(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	name := graphqlNodeName(n, src)
	if name == "" {
		return
	}

	meta := map[string]string{"kind": "fragment"}

	// Extract "on TypeName" — the type_condition.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "type_condition" {
			continue
		}
		typeName := graphqlExtractTypeName(child, src)
		if typeName != "" {
			meta["on"] = typeName

			// Create EdgeDependsOn to the target type.
			targetID := g.MakeNodeID(filePath, typeName)
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(targetID) != nil {
				g.AddEdge(&graph.Edge{From: nodeID, To: targetID, Type: graph.EdgeDependsOn})
			}
		}
		break
	}

	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractDirectiveDefinition handles directive_definition nodes.
func (p *GraphQLParser) extractDirectiveDefinition(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	name := graphqlNodeName(n, src)
	if name == "" {
		return
	}

	// Prefix with @ for clarity in the graph.
	qualName := "@" + name

	// Extract locations (e.g. FIELD_DEFINITION, OBJECT).
	// directive_locations can be recursive in this grammar.
	var locations []string
	var collectLocations func(node sitter.Node)
	collectLocations = func(node sitter.Node) {
		for i := uint32(0); i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child.IsNull() {
				continue
			}
			switch child.Type() {
			case "directive_locations":
				collectLocations(child)
			case "directive_location":
				locText := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
				if locText != "" && locText != "|" {
					locations = append(locations, locText)
				}
			}
		}
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "directive_locations" {
			collectLocations(child)
		}
	}

	meta := map[string]string{"kind": "directive"}
	if len(locations) > 0 {
		meta["locations"] = strings.Join(locations, ", ")
	}

	nodeID := g.MakeNodeID(filePath, qualName)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     qualName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractFields walks a type definition node looking for fields_definition
// children, then emits NodeFunction nodes for each field_definition.
func (p *GraphQLParser) extractFields(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
	parentNodeID graph.NodeID, parentName string,
) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		if ct != "fields_definition" && ct != "input_fields_definition" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			field := child.Child(j)
			if field.IsNull() {
				continue
			}
			ft := field.Type()
			if ft != "field_definition" && ft != "input_value_definition" {
				continue
			}
			fieldName := graphqlNodeName(field, src)
			if fieldName == "" {
				continue
			}
			qualName := parentName + "." + fieldName

			// Extract field return type.
			meta := map[string]string{"kind": "field"}
			fieldType := graphqlFieldType(field, src)
			if fieldType != "" {
				meta["type"] = fieldType
			}

			fieldNodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       fieldNodeID,
				Type:     graph.NodeFunction,
				Name:     qualName,
				File:     filePath,
				Line:     int(field.StartPoint().Row) + 1,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
			g.AddEdge(&graph.Edge{From: parentNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
		}
	}
}

// extractImplements looks for implements_interfaces children in a type definition
// and creates EdgeImplements edges to the referenced interface types.
func (p *GraphQLParser) extractImplements(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, nodeID graph.NodeID,
) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "implements_interfaces" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			iface := child.Child(j)
			if iface.IsNull() {
				continue
			}
			ifaceName := graphqlExtractTypeName(iface, src)
			if ifaceName == "" || ifaceName == "&" || ifaceName == "implements" {
				continue
			}
			ifaceID := g.MakeNodeID(filePath, ifaceName)
			if g.GetNode(ifaceID) != nil {
				g.AddEdge(&graph.Edge{From: nodeID, To: ifaceID, Type: graph.EdgeImplements})
			}
		}
	}
}

// graphqlNodeName extracts the name from a GraphQL AST node by looking for a
// child of type "name" which itself may contain a text node, or by looking for
// a direct "name" field.
func graphqlNodeName(n sitter.Node, src []byte) string {
	// Try ChildByFieldName first (works when grammar uses named fields).
	if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
		return strings.TrimSpace(string(src[nameNode.StartByte():nameNode.EndByte()]))
	}
	// Walk children looking for a "name" type node or wrapper nodes like "fragment_name".
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		if ct == "name" {
			return strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
		}
		// Handle wrapper nodes like fragment_name that contain a name child.
		if strings.HasSuffix(ct, "_name") {
			if inner := child.ChildByFieldName("name"); !inner.IsNull() {
				return strings.TrimSpace(string(src[inner.StartByte():inner.EndByte()]))
			}
			// Try direct name child.
			for j := uint32(0); j < child.ChildCount(); j++ {
				gc := child.Child(j)
				if !gc.IsNull() && gc.Type() == "name" {
					return strings.TrimSpace(string(src[gc.StartByte():gc.EndByte()]))
				}
			}
		}
	}
	return ""
}

// graphqlFirstIdentifierText returns the text of the first non-punctuation
// child whose text looks like an identifier (alphanumeric/underscore).
func graphqlFirstIdentifierText(n sitter.Node, src []byte) string {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		text := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
		if text == "" || text == "{" || text == "}" || text == "(" || text == ")" {
			continue
		}
		// Skip keywords.
		if text == "enum" || text == "type" || text == "input" || text == "interface" ||
			text == "union" || text == "scalar" || text == "fragment" || text == "on" ||
			text == "directive" || text == "schema" || text == "extend" {
			continue
		}
		return text
	}
	return ""
}

// graphqlExtractTypeName extracts a type name from a named_type, type, or
// type_condition node. Handles wrapping in list_type or non_null_type.
func graphqlExtractTypeName(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	switch n.Type() {
	case "named_type", "type_identifier":
		return strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
	case "type_condition":
		// "on TypeName" — look for named_type child.
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if child.IsNull() {
				continue
			}
			if name := graphqlExtractTypeName(child, src); name != "" && name != "on" {
				return name
			}
		}
	case "list_type", "non_null_type":
		// Unwrap: [Type]! → Type
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if child.IsNull() {
				continue
			}
			if name := graphqlExtractTypeName(child, src); name != "" {
				return name
			}
		}
	default:
		// For generic nodes, try extracting the text if it looks like a type name.
		text := strings.TrimSpace(string(src[n.StartByte():n.EndByte()]))
		// Filter out punctuation and keywords.
		if text != "" && text != "|" && text != "=" && text != "&" &&
			text != "implements" && text != "on" &&
			!strings.ContainsAny(text, "{}()[]!") {
			return text
		}
	}
	return ""
}

// graphqlFieldType extracts the return type of a field_definition node.
// Looks for a "type" child node and returns its text representation.
func graphqlFieldType(n sitter.Node, src []byte) string {
	// Try named field "type" first.
	if typeNode := n.ChildByFieldName("type"); !typeNode.IsNull() {
		return strings.TrimSpace(string(src[typeNode.StartByte():typeNode.EndByte()]))
	}
	// Walk children looking for type-like nodes after the colon.
	pastColon := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		text := string(src[child.StartByte():child.EndByte()])
		if text == ":" {
			pastColon = true
			continue
		}
		if pastColon {
			ct := child.Type()
			if ct == "named_type" || ct == "list_type" || ct == "non_null_type" || ct == "type" {
				return strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			}
		}
	}
	return ""
}
