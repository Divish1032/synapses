package parser

import (
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

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *GraphQLParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// fieldTypeRef records a deferred field→returnType edge to resolve after all
// type definitions in the file have been added to the graph.
// GraphQL schemas can reference types in any order; we must not drop edges
// simply because the target type was defined after the referencing field.
type fieldTypeRef struct {
	fieldNodeID graph.NodeID
	baseType    string // stripped of []/! wrappers, e.g. "User" from "[User!]!"
	filePath    string
}

// Parse extracts code entities from a single GraphQL file and merges them into the graph.
func (p *GraphQLParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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
		ID:     fileNodeID,
		Type:   graph.NodeFile,
		Name:   filepath.Base(filePath),
		File:   filePath,
		Line:   1,
		Domain: graph.DomainAPI,
	})

	// First pass: walk all definitions, collecting deferred field→type edges.
	// Deferred resolution handles forward references: type B may appear after
	// the field in type A that references it. Without deferral, forward-referenced
	// types produce no edge — silently broken graph for real-world schemas.
	var deferred []fieldTypeRef
	p.walkDefinitions(g, root, src, filePath, fileNodeID, &deferred)

	// Second pass: resolve all deferred field→type edges now that every type
	// definition in this file has been added to the graph.
	for _, d := range deferred {
		typeNodeID := g.MakeNodeID(d.filePath, d.baseType)
		if g.GetNode(typeNodeID) != nil {
			g.AddEdge(&graph.Edge{From: d.fieldNodeID, To: typeNodeID, Type: graph.EdgeDependsOn})
		}
	}

	return nil
}

// walkDefinitions recursively unwraps the grammar's nested wrapper nodes
// (document, definition, type_system_definition, executable_definition,
// type_definition) to find the actual definition nodes and dispatch them.
// deferred accumulates field→type edges that must be resolved after all type
// nodes exist (handles forward references).
func (p *GraphQLParser) walkDefinitions(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
	deferred *[]fieldTypeRef,
) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "object_type_definition":
			p.extractObjectType(g, child, src, filePath, fileNodeID, "type", deferred)
		case "input_object_type_definition":
			p.extractObjectType(g, child, src, filePath, fileNodeID, "input", deferred)
		case "interface_type_definition":
			p.extractObjectType(g, child, src, filePath, fileNodeID, "interface", deferred)
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
			p.walkDefinitions(g, child, src, filePath, fileNodeID, deferred)
		}
	}
}

// extractObjectType handles object_type_definition, input_object_type_definition,
// and interface_type_definition. All three share the same structure: a name child
// and an optional fields_definition containing field_definition children.
func (p *GraphQLParser) extractObjectType(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, kind string,
	deferred *[]fieldTypeRef,
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
		Domain:   graph.DomainAPI,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

	// Extract implements_interfaces → EdgeImplements edges.
	p.extractImplements(g, n, src, filePath, nodeID)

	// Query, Mutation, and Subscription root types: fields are API operations (NodeRoute).
	isOperation := name == "Query" || name == "Mutation" || name == "Subscription"
	// Extract fields from fields_definition.
	p.extractFields(g, n, src, filePath, fileNodeID, nodeID, name, isOperation, deferred)
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
		Domain:   graph.DomainAPI,
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
	// The grammar represents union_member_types recursively:
	//   union_member_types → union_member_types "|" named_type
	// so we recurse to collect all members regardless of nesting depth.
	var members []string
	var collectMembers func(node sitter.Node)
	collectMembers = func(node sitter.Node) {
		for i := uint32(0); i < node.ChildCount(); i++ {
			child := node.Child(i)
			if child.IsNull() {
				continue
			}
			switch child.Type() {
			case "union_member_types":
				collectMembers(child)
			case "named_type":
				if name := graphqlExtractTypeName(child, src); name != "" {
					members = append(members, name)
				}
			}
		}
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "union_member_types" {
			collectMembers(child)
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
		Domain:   graph.DomainAPI,
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
		Domain:   graph.DomainAPI,
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
		Domain:   graph.DomainAPI,
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
	var fragmentOnType string
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "type_condition" {
			continue
		}
		typeName := graphqlExtractTypeName(child, src)
		if typeName != "" {
			meta["on"] = typeName
			fragmentOnType = typeName
		}
		break
	}

	// Add the fragment node first so AddEdge (which requires both endpoints)
	// can succeed when creating the DependsOn edge below.
	nodeID := g.MakeNodeID(filePath, name)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Domain:   graph.DomainAPI,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

	// Create EdgeDependsOn to the target type (after the fragment node exists).
	if fragmentOnType != "" {
		targetID := g.MakeNodeID(filePath, fragmentOnType)
		if g.GetNode(targetID) != nil {
			g.AddEdge(&graph.Edge{From: nodeID, To: targetID, Type: graph.EdgeDependsOn})
		}
	}
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
		Domain:   graph.DomainAPI,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractFields walks a type definition node looking for fields_definition
// children, then emits NodeFunction (or NodeRoute for operation types) nodes
// for each field_definition.
//
// isOperation should be true for Query/Mutation/Subscription types — their fields
// are API operations and are emitted as NodeRoute so that get_impact and BFS/PPR
// treat them as API surface rather than internal implementation details.
func (p *GraphQLParser) extractFields(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
	parentNodeID graph.NodeID, parentName string,
	isOperation bool,
	deferred *[]fieldTypeRef,
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

			// Query/Mutation/Subscription fields are API operations → NodeRoute.
			nodeType := graph.NodeFunction
			if isOperation {
				nodeType = graph.NodeRoute
			}

			fieldNodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       fieldNodeID,
				Type:     nodeType,
				Name:     qualName,
				File:     filePath,
				Line:     int(field.StartPoint().Row) + 1,
				Exported: true,
				Domain:   graph.DomainAPI,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
			g.AddEdge(&graph.Edge{From: parentNodeID, To: fieldNodeID, Type: graph.EdgeDefines})

			// Defer field→returnType EdgeDependsOn resolution so that forward-referenced
			// types (defined after this field in the same file) are also resolved.
			// The second pass in Parse() creates the actual edges once all nodes exist.
			if fieldType != "" {
				baseType := graphqlBaseTypeName(fieldType)
				if !graphqlIsBuiltinScalar(baseType) {
					*deferred = append(*deferred, fieldTypeRef{
						fieldNodeID: fieldNodeID,
						baseType:    baseType,
						filePath:    filePath,
					})
				}
			}
		}
	}
}

// graphqlBaseTypeName strips list wrappers and non-null markers from a GraphQL
// type string to return the base named type.
// "[User!]!" → "User", "String!" → "String", "User" → "User".
func graphqlBaseTypeName(typStr string) string {
	// Strip all [ ] ! characters.
	result := strings.NewReplacer("[", "", "]", "", "!", "").Replace(typStr)
	return strings.TrimSpace(result)
}

// graphqlIsBuiltinScalar returns true for the five built-in GraphQL scalar types.
// These are not emitted as graph nodes so EdgeDependsOn cannot point to them.
func graphqlIsBuiltinScalar(name string) bool {
	switch name {
	case "String", "Int", "Float", "Boolean", "ID":
		return true
	}
	return false
}

// extractImplements looks for implements_interfaces children in a type definition
// and creates EdgeImplements edges to the referenced interface types.
//
// The grammar uses left-recursive nesting for multiple interfaces:
//
//	implements_interfaces (implements Node & Auditable)
//	  implements_interfaces (implements Node)   ← recursive child
//	    implements
//	    named_type (Node)
//	  &
//	  named_type (Auditable)
//
// So we recurse into child implements_interfaces nodes and only extract
// named_type children directly.
func (p *GraphQLParser) extractImplements(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, nodeID graph.NodeID,
) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "implements_interfaces" {
			continue
		}
		p.collectImplementsInterfaces(g, child, src, filePath, nodeID)
	}
}

// collectImplementsInterfaces recursively walks implements_interfaces nodes,
// extracting named_type children and creating EdgeImplements edges.
func (p *GraphQLParser) collectImplementsInterfaces(
	g *graph.Graph, n sitter.Node, src []byte,
	filePath string, nodeID graph.NodeID,
) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "implements_interfaces":
			// Recurse into nested implements_interfaces (left-recursive grammar).
			p.collectImplementsInterfaces(g, child, src, filePath, nodeID)
		case "named_type":
			ifaceName := graphqlExtractTypeName(child, src)
			if ifaceName == "" {
				continue
			}
			ifaceID := g.MakeNodeID(filePath, ifaceName)
			// Create a stub node if the interface hasn't been parsed yet (forward reference).
			if g.GetNode(ifaceID) == nil {
				g.AddNode(&graph.Node{
					ID:       ifaceID,
					Type:     graph.NodeInterface,
					Name:     ifaceName,
					File:     filePath,
					Exported: true,
				})
			}
			g.AddEdge(&graph.Edge{From: nodeID, To: ifaceID, Type: graph.EdgeImplements})
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
