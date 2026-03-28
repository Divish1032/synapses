package parser

import (
	"path/filepath"
	"strings"

	thriftg "github.com/alexaandru/go-sitter-forest/thrift"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ThriftParser parses Apache Thrift IDL (.thrift) source files.
//
// Extracted entities:
//   - namespace declarations → NodePackage (kind=namespace, metadata lang=<scope>)
//   - typedef definitions    → NodeMethod (kind=typedef, metadata alias=<original_type>)
//   - enum definitions       → NodeStruct (kind=enum) + enum value identifiers → NodeMethod
//   - struct definitions     → NodeStruct (kind=struct) + fields → NodeMethod (kind=field)
//   - exception definitions  → NodeStruct (kind=exception) + fields → NodeMethod (kind=field)
//   - union definitions      → NodeStruct (kind=union) + fields → NodeMethod (kind=field)
//   - const definitions      → NodeMethod (kind=const, metadata type=<type>)
//   - service definitions    → NodeInterface (kind=service) + function_definition → NodeMethod
//
// All top-level Thrift definitions are Exported=true (IDL is a public interface by definition).
//
// AST node types verified via TestProbeThrift:
//
//	document → top-level container
//	namespace_declaration: [namespace][namespace_scope][namespace]
//	typedef_definition: [typedef][definition_type][typedef_identifier]
//	enum_definition: [enum][identifier]{[identifier][=][number][,]...}
//	struct_definition: [struct][identifier]{[field]...}
//	exception_definition: [exception][identifier]{[field]...}
//	service_definition: [service][identifier]{[function_definition]...}
//	union_definition: [union][identifier]{[field]...}
//	const_definition: [const][definition_type][identifier][=][literal]
//	field: [field_id][field_modifier?][type][identifier][,?]
//	function_definition: [type][identifier][parameters][throws?][,?]
type ThriftParser struct {
	language *sitter.Language
}

// NewThriftParser creates a ready-to-use ThriftParser.
func NewThriftParser() *ThriftParser {
	return &ThriftParser{language: sitter.NewLanguage(thriftg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *ThriftParser) Extensions() []string {
	return []string{".thrift"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *ThriftParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single Thrift IDL file and merges them into the graph.
func (p *ThriftParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// Walk top-level children of document.
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "namespace_declaration":
			p.extractNamespace(g, child, src, filePath, fileNodeID)
		case "typedef_definition":
			p.extractTypedef(g, child, src, filePath, fileNodeID)
		case "enum_definition":
			p.extractEnum(g, child, src, filePath, fileNodeID)
		case "struct_definition":
			p.extractStructLike(g, child, src, filePath, fileNodeID, "struct")
		case "exception_definition":
			p.extractStructLike(g, child, src, filePath, fileNodeID, "exception")
		case "union_definition":
			p.extractStructLike(g, child, src, filePath, fileNodeID, "union")
		case "const_definition":
			p.extractConst(g, child, src, filePath, fileNodeID)
		case "service_definition":
			p.extractService(g, child, src, filePath, fileNodeID)
		case "include_statement":
			p.extractInclude(g, child, src, filePath, fileNodeID)
		}
	}

	return nil
}

// extractNamespace handles namespace_declaration nodes.
// AST (simple): [namespace(kw)][namespace_scope][namespace(name)]
//
//	e.g. "namespace go myservice" →
//	  namespace_scope=[go], then one namespace token = "myservice"
//
// AST (dotted): "namespace java com.example.myservice" →
//
//	namespace_scope=[java], then [namespace]="com", then
//	[namespace][.][identifier][.][identifier] = ".example.myservice"
//
// We collect all namespace children after the namespace_scope to reconstruct
// the full qualified name by concatenating their text.
//
// The emitted node name is "<lang>.<fullyQualifiedName>".
// Metadata: lang=<lang>.
func (p *ThriftParser) extractNamespace(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	lang := ""
	nameParts := []string{}
	pastScope := false

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "namespace_scope":
			lang = strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			pastScope = true
		case "namespace":
			if !pastScope {
				// This is the "namespace" keyword token — skip.
				continue
			}
			// Collect the full text of this namespace node (may include dots + identifiers).
			part := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			if part != "" {
				nameParts = append(nameParts, part)
			}
		}
	}

	if len(nameParts) == 0 {
		return
	}

	// Join all name parts (handles both simple "myservice" and dotted "com.example.svc").
	name := strings.Join(nameParts, "")
	nodeName := name
	if lang != "" {
		nodeName = lang + "." + name
	}

	nodeID := g.MakeNodeID(filePath, nodeName)
	meta := map[string]string{"kind": "namespace"}
	if lang != "" {
		meta["lang"] = lang
	}

	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodePackage,
		Name:     nodeName,
		Package:  name,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractTypedef handles typedef_definition nodes.
// AST: [typedef][definition_type][typedef_identifier]
//
// Emits a NodeMethod with kind=typedef and metadata alias=<original_type>.
func (p *ThriftParser) extractTypedef(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// Find typedef_identifier (the new name) and definition_type (the original type).
	newName := ""
	originalType := ""

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "typedef_identifier":
			newName = string(src[child.StartByte():child.EndByte()])
		case "definition_type":
			originalType = strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
		}
	}

	if newName == "" {
		return
	}

	meta := map[string]string{"kind": "typedef"}
	if originalType != "" {
		meta["alias"] = originalType
	}

	nodeID := g.MakeNodeID(filePath, newName)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeMethod,
		Name:     newName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractEnum handles enum_definition nodes.
// AST: [enum][identifier]{[identifier][=][number][,]...}
//
// Emits a NodeStruct (kind=enum) and NodeMethod children for each enum value.
func (p *ThriftParser) extractEnum(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// First identifier child = enum name.
	enumName := ""
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "identifier" {
			enumName = string(src[child.StartByte():child.EndByte()])
			break
		}
	}
	if enumName == "" {
		return
	}

	enumNodeID := g.MakeNodeID(filePath, enumName)
	g.AddNode(&graph.Node{
		ID:       enumNodeID,
		Type:     graph.NodeStruct,
		Name:     enumName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: map[string]string{"kind": "enum"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: enumNodeID, Type: graph.EdgeDefines})

	// Extract enum values: identifiers after the opening "{" that are not "=", numbers, or ",".
	// In the AST, enum values appear as direct identifier children (not inside a field node).
	// We scan after the opening "{".
	inBody := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "{" {
			inBody = true
			continue
		}
		if child.Type() == "}" {
			break
		}
		if !inBody || child.Type() != "identifier" {
			continue
		}
		valueName := string(src[child.StartByte():child.EndByte()])
		if valueName == "" || valueName == enumName {
			continue
		}
		qualName := enumName + "." + valueName
		valueNodeID := g.MakeNodeID(filePath, qualName)
		if g.GetNode(valueNodeID) != nil {
			continue
		}
		g.AddNode(&graph.Node{
			ID:       valueNodeID,
			Type:     graph.NodeMethod,
			Name:     qualName,
			File:     filePath,
			Line:     int(child.StartPoint().Row) + 1,
			Exported: true,
			Metadata: map[string]string{"kind": "enum_value"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: valueNodeID, Type: graph.EdgeDefines})
		g.AddEdge(&graph.Edge{From: enumNodeID, To: valueNodeID, Type: graph.EdgeDefines})
	}
}

// extractStructLike handles struct_definition, exception_definition, and union_definition.
// All three have the same shape: keyword + identifier + { field* }.
// AST: [struct|exception|union][identifier]{[field]...}
//
// Emits a NodeStruct with the given kind, plus NodeMethod children for each field.
// Field AST: [field_id][field_modifier?][type][identifier][,?]
func (p *ThriftParser) extractStructLike(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, kind string) {
	// First identifier child = struct/exception/union name.
	structName := ""
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "identifier" {
			structName = string(src[child.StartByte():child.EndByte()])
			break
		}
	}
	if structName == "" {
		return
	}

	structNodeID := g.MakeNodeID(filePath, structName)
	g.AddNode(&graph.Node{
		ID:       structNodeID,
		Type:     graph.NodeStruct,
		Name:     structName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: map[string]string{"kind": kind},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: structNodeID, Type: graph.EdgeDefines})

	// Extract fields.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "field" {
			continue
		}
		p.extractField(g, child, src, filePath, fileNodeID, structNodeID, structName)
	}
}

// extractField handles field nodes inside struct/exception/union.
// AST: [field_id][field_modifier?][type][identifier][,?]
//
// Emits a NodeMethod qualified as StructName.fieldName with kind=field.
// Metadata includes field_id, required/optional modifier, and type.
func (p *ThriftParser) extractField(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, parentNodeID graph.NodeID, parentName string) {
	fieldID := ""
	modifier := ""
	fieldType := ""
	fieldName := ""

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "field_id":
			// [number][:] — the field tag number.
			if num := firstChildOfType(child, "number"); !num.IsNull() {
				fieldID = string(src[num.StartByte():num.EndByte()])
			}
		case "field_modifier":
			modifier = strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
		case "type":
			fieldType = strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
		case "identifier":
			if fieldName == "" {
				fieldName = string(src[child.StartByte():child.EndByte()])
			}
		}
	}

	if fieldName == "" {
		return
	}

	qualName := parentName + "." + fieldName
	meta := map[string]string{"kind": "field"}
	if fieldID != "" {
		meta["field_id"] = fieldID
	}
	if modifier != "" {
		meta["modifier"] = modifier
	}
	if fieldType != "" {
		meta["type"] = fieldType
	}

	fieldNodeID := g.MakeNodeID(filePath, qualName)
	if g.GetNode(fieldNodeID) != nil {
		return
	}
	g.AddNode(&graph.Node{
		ID:       fieldNodeID,
		Type:     graph.NodeMethod,
		Name:     qualName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: parentNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
}

// extractConst handles const_definition nodes.
// AST: [const][definition_type][identifier][=][literal]
//
// Emits a NodeMethod with kind=const.
func (p *ThriftParser) extractConst(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	constName := ""
	constType := ""

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "definition_type":
			constType = strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
		case "identifier":
			if constName == "" {
				constName = string(src[child.StartByte():child.EndByte()])
			}
		}
	}

	if constName == "" {
		return
	}

	meta := map[string]string{"kind": "const"}
	if constType != "" {
		meta["type"] = constType
	}

	nodeID := g.MakeNodeID(filePath, constName)
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeMethod,
		Name:     constName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractService handles service_definition nodes.
// AST: [service][identifier]{[function_definition]...}
//
// Emits a NodeInterface (kind=service) and function_definition children as NodeMethod.
//
// function_definition AST:
//
//	[type][identifier][parameters][throws?][,?]
//	where type is the return type (or [void]).
func (p *ThriftParser) extractService(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	// First identifier child = service name. If "extends" keyword appears,
	// the next identifier is the parent service.
	serviceName := ""
	parentService := ""
	seenExtends := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			if serviceName == "" {
				serviceName = string(src[child.StartByte():child.EndByte()])
			} else if seenExtends && parentService == "" {
				parentService = string(src[child.StartByte():child.EndByte()])
			}
		case "extends":
			seenExtends = true
		}
	}
	if serviceName == "" {
		return
	}

	meta := map[string]string{"kind": "service"}
	if parentService != "" {
		meta["extends"] = parentService
	}

	serviceNodeID := g.MakeNodeID(filePath, serviceName)
	g.AddNode(&graph.Node{
		ID:       serviceNodeID,
		Type:     graph.NodeInterface,
		Name:     serviceName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: serviceNodeID, Type: graph.EdgeDefines})

	// Create extends edge if parent service is known.
	if parentService != "" {
		parentNodeID := g.MakeNodeID(filePath, parentService)
		if g.GetNode(parentNodeID) == nil {
			// Forward reference — create a placeholder node.
			g.AddNode(&graph.Node{
				ID:       parentNodeID,
				Type:     graph.NodeInterface,
				Name:     parentService,
				File:     filePath,
				Exported: true,
				Metadata: map[string]string{"kind": "service"},
			})
		}
		g.AddEdge(&graph.Edge{From: serviceNodeID, To: parentNodeID, Type: graph.EdgeImplements})
	}

	// Extract function_definitions.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "function_definition" {
			continue
		}
		p.extractServiceFunction(g, child, src, filePath, fileNodeID, serviceNodeID, serviceName)
	}
}

// extractServiceFunction handles function_definition nodes inside a service.
// AST: [type][identifier][parameters][throws?][,?]
//
// Emits a NodeMethod qualified as ServiceName.funcName.
// Metadata includes return_type and throws information.
func (p *ThriftParser) extractServiceFunction(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID, serviceNodeID graph.NodeID, serviceName string) {
	funcName := ""
	returnType := ""
	modifier := ""
	var throwsTypes []string

	// First collect the type (return type) and the identifier (function name).
	// The type child comes before the identifier.
	typeSeenBeforeName := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "function_modifier":
			modifier = strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
		case "type":
			returnType = strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			typeSeenBeforeName = true
		case "void":
			// void return type appears as a bare token
			returnType = "void"
			typeSeenBeforeName = true
		case "identifier":
			if typeSeenBeforeName && funcName == "" {
				funcName = string(src[child.StartByte():child.EndByte()])
			}
		case "throws":
			// throws AST: [throws][parameters] — extract exception type names.
			for j := uint32(0); j < child.ChildCount(); j++ {
				tc := child.Child(j)
				if tc.IsNull() || tc.Type() != "parameters" {
					continue
				}
				for k := uint32(0); k < tc.ChildCount(); k++ {
					param := tc.Child(k)
					if param.IsNull() || param.Type() != "parameter" {
						continue
					}
					// parameter: [field_id][type][identifier]
					for l := uint32(0); l < param.ChildCount(); l++ {
						pc := param.Child(l)
						if !pc.IsNull() && pc.Type() == "type" {
							throwsTypes = append(throwsTypes, strings.TrimSpace(string(src[pc.StartByte():pc.EndByte()])))
						}
					}
				}
			}
		}
	}

	if funcName == "" {
		return
	}

	qualName := serviceName + "." + funcName
	meta := map[string]string{"kind": "function"}
	if returnType != "" {
		meta["return_type"] = returnType
	}
	if modifier != "" {
		meta["modifier"] = modifier
	}
	if len(throwsTypes) > 0 {
		meta["throws"] = strings.Join(throwsTypes, ", ")
	}

	funcNodeID := g.MakeNodeID(filePath, qualName)
	g.AddNode(&graph.Node{
		ID:       funcNodeID,
		Type:     graph.NodeMethod,
		Name:     qualName,
		File:     filePath,
		Line:     int(n.StartPoint().Row) + 1,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: funcNodeID, Type: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: serviceNodeID, To: funcNodeID, Type: graph.EdgeDefines})
}

// extractInclude handles include_statement nodes.
// AST: [include]["path/to/file.thrift"]
// Emits a NodePackage with EdgeImports from the file.
func (p *ThriftParser) extractInclude(g *graph.Graph, n sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "string" || child.Type() == "string_literal" {
			raw := string(src[child.StartByte():child.EndByte()])
			// Strip quotes.
			raw = strings.Trim(raw, `"'`)
			if raw == "" {
				continue
			}
			includeNodeID := g.MakeNodeID(raw, raw)
			if g.GetNode(includeNodeID) == nil {
				g.AddNode(&graph.Node{
					ID:      includeNodeID,
					Type:    graph.NodePackage,
					Name:    raw,
					Package: raw,
					File:    filePath,
					Line:    int(n.StartPoint().Row) + 1,
				})
			}
			g.AddEdge(&graph.Edge{From: fileNodeID, To: includeNodeID, Type: graph.EdgeImports})
			return
		}
	}
}
