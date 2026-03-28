package parser

import (
	"bytes"
	"path/filepath"
	"strings"

	eproto "github.com/emicklei/proto"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ProtobufParser parses Protocol Buffer (.proto) source files using
// github.com/emicklei/proto — a dedicated proto parser that handles oneof
// blocks, nested messages, and enum values correctly without producing
// [ERROR] nodes (the fundamental limitation of the tree-sitter proto grammar).
type ProtobufParser struct{}

// NewProtobufParser creates a ready-to-use ProtobufParser.
func NewProtobufParser() *ProtobufParser {
	return &ProtobufParser{}
}

// Extensions returns the file extensions handled by this parser.
func (p *ProtobufParser) Extensions() []string {
	return []string{".proto"}
}

// Parse extracts code entities from a single .proto file.
func (p *ProtobufParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	protoParser := eproto.NewParser(bytes.NewReader(src))
	definition, _ := protoParser.Parse()
	if definition == nil {
		return nil // unparseable file — emit nothing rather than crash
	}

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	v := &protoExtractor{
		g:          g,
		filePath:   filePath,
		fileNodeID: fileNodeID,
	}
	v.walkElements(definition.Elements, "")
	return nil
}

// protoExtractor walks proto AST elements and emits graph nodes.
type protoExtractor struct {
	g          *graph.Graph
	filePath   string
	fileNodeID graph.NodeID
}

// walkElements processes a slice of proto Visitee elements.
// enclosing is the dotted qualifier of the parent scope (empty at file level).
func (v *protoExtractor) walkElements(elements []eproto.Visitee, enclosing string) {
	for _, elem := range elements {
		switch e := elem.(type) {

		case *eproto.Package:
			if fn := v.g.GetNode(v.fileNodeID); fn != nil {
				fn.Package = e.Name
			}

		case *eproto.Import:
			path := strings.Trim(e.Filename, `"'`)
			if path == "" {
				continue
			}
			importNodeID := v.g.MakeNodeID(path, path)
			v.g.AddNode(&graph.Node{
				ID: importNodeID, Type: graph.NodePackage, Name: path,
				Package: path, File: v.filePath,
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: importNodeID, Type: graph.EdgeImports})

		case *eproto.Message:
			if e.IsExtend {
				// extend Foo { field ... } — emit extension fields as Foo.field_name nodes.
				v.walkElements(e.Elements, e.Name)
				continue
			}
			qualName := protoQualify(enclosing, e.Name)
			nodeID := v.g.MakeNodeID(v.filePath, qualName)
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: qualName, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: map[string]string{"kind": "message"},
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosing != "" {
				if parentID := v.g.MakeNodeID(v.filePath, enclosing); v.g.GetNode(parentID) != nil {
					v.g.AddEdge(&graph.Edge{From: parentID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
			// Recurse: fields, nested messages, enums, oneofs.
			v.walkElements(e.Elements, qualName)

		case *eproto.NormalField:
			if enclosing == "" {
				continue
			}
			qualName := enclosing + "." + e.Name
			nodeID := v.g.MakeNodeID(v.filePath, qualName)
			if v.g.GetNode(nodeID) != nil {
				continue
			}
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: map[string]string{"kind": "field", "type": e.Type},
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if parentID := v.g.MakeNodeID(v.filePath, enclosing); v.g.GetNode(parentID) != nil {
				v.g.AddEdge(&graph.Edge{From: parentID, To: nodeID, Type: graph.EdgeDefines})
			}

		case *eproto.MapField:
			if enclosing == "" {
				continue
			}
			qualName := enclosing + "." + e.Name
			nodeID := v.g.MakeNodeID(v.filePath, qualName)
			if v.g.GetNode(nodeID) != nil {
				continue
			}
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: map[string]string{"kind": "map_field"},
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if parentID := v.g.MakeNodeID(v.filePath, enclosing); v.g.GetNode(parentID) != nil {
				v.g.AddEdge(&graph.Edge{From: parentID, To: nodeID, Type: graph.EdgeDefines})
			}

		case *eproto.Oneof:
			if enclosing == "" {
				continue
			}
			qualName := enclosing + "." + e.Name
			nodeID := v.g.MakeNodeID(v.filePath, qualName)
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: map[string]string{"kind": "oneof"},
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if parentID := v.g.MakeNodeID(v.filePath, enclosing); v.g.GetNode(parentID) != nil {
				v.g.AddEdge(&graph.Edge{From: parentID, To: nodeID, Type: graph.EdgeDefines})
			}
			// Oneof fields qualify under the containing message (not the oneof).
			v.walkElements(e.Elements, enclosing)

		case *eproto.OneOfField:
			if enclosing == "" {
				continue
			}
			qualName := enclosing + "." + e.Name
			nodeID := v.g.MakeNodeID(v.filePath, qualName)
			if v.g.GetNode(nodeID) != nil {
				continue
			}
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: map[string]string{"kind": "oneof_field", "type": e.Type},
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if parentID := v.g.MakeNodeID(v.filePath, enclosing); v.g.GetNode(parentID) != nil {
				v.g.AddEdge(&graph.Edge{From: parentID, To: nodeID, Type: graph.EdgeDefines})
			}

		case *eproto.Enum:
			qualName := protoQualify(enclosing, e.Name)
			nodeID := v.g.MakeNodeID(v.filePath, qualName)
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: qualName, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: map[string]string{"kind": "enum"},
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosing != "" {
				if parentID := v.g.MakeNodeID(v.filePath, enclosing); v.g.GetNode(parentID) != nil {
					v.g.AddEdge(&graph.Edge{From: parentID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
			v.walkElements(e.Elements, qualName)

		case *eproto.EnumField:
			if enclosing == "" {
				continue
			}
			qualName := enclosing + "." + e.Name
			nodeID := v.g.MakeNodeID(v.filePath, qualName)
			if v.g.GetNode(nodeID) != nil {
				continue
			}
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: map[string]string{"kind": "enum_value"},
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if parentID := v.g.MakeNodeID(v.filePath, enclosing); v.g.GetNode(parentID) != nil {
				v.g.AddEdge(&graph.Edge{From: parentID, To: nodeID, Type: graph.EdgeDefines})
			}

		case *eproto.Service:
			nodeID := v.g.MakeNodeID(v.filePath, e.Name)
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeInterface, Name: e.Name, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: map[string]string{"kind": "service"},
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			v.walkElements(e.Elements, e.Name)

		case *eproto.RPC:
			if enclosing == "" {
				continue
			}
			qualName := enclosing + "." + e.Name
			nodeID := v.g.MakeNodeID(v.filePath, qualName)
			rpcMeta := map[string]string{
				"kind":          "rpc",
				"request_type":  e.RequestType,
				"response_type": e.ReturnsType,
			}
			if e.StreamsRequest {
				rpcMeta["streams_request"] = "true"
			}
			if e.StreamsReturns {
				rpcMeta["streams_response"] = "true"
			}
			v.g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: v.filePath,
				Line: e.Position.Line, Exported: true,
				Metadata: rpcMeta,
			})
			v.g.AddEdge(&graph.Edge{From: v.fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if parentID := v.g.MakeNodeID(v.filePath, enclosing); v.g.GetNode(parentID) != nil {
				v.g.AddEdge(&graph.Edge{From: parentID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
	}
}

// ResolveProtoTypeRefs creates DEPENDS_ON edges from fields to their message/enum
// types within the same proto graph. This enables impact analysis: changing a message
// type surfaces all messages that reference it.
// Must be called after all proto files in the project are parsed.
func ResolveProtoTypeRefs(g *graph.Graph) int {
	resolved := 0
	for _, n := range g.AllNodes() {
		if n.Metadata == nil || n.Metadata["kind"] != "field" && n.Metadata["kind"] != "oneof_field" && n.Metadata["kind"] != "rpc" {
			continue
		}
		// For fields: type is the field's protobuf type (e.g., "OtherMessage", "google.protobuf.Timestamp")
		// For RPCs: request_type and response_type
		var typeRefs []string
		if ft := n.Metadata["type"]; ft != "" {
			typeRefs = append(typeRefs, ft)
		}
		if rt := n.Metadata["request_type"]; rt != "" {
			typeRefs = append(typeRefs, rt)
		}
		if rt := n.Metadata["response_type"]; rt != "" {
			typeRefs = append(typeRefs, rt)
		}

		for _, typeName := range typeRefs {
			// Skip primitive types.
			if isProtoPrimitive(typeName) {
				continue
			}
			// Find the target message/enum node by name.
			targets := g.FindByName(typeName)
			for _, target := range targets {
				if target.Type != graph.NodeStruct && target.Type != graph.NodeInterface {
					continue
				}
				if target.ID == n.ID {
					continue
				}
				if !g.HasEdge(n.ID, target.ID, graph.EdgeDependsOn) {
					g.AddEdge(&graph.Edge{
						From: n.ID,
						To:   target.ID,
						Type: graph.EdgeDependsOn,
					})
					resolved++
				}
				break // take first match
			}
		}
	}
	return resolved
}

// isProtoPrimitive returns true for proto built-in scalar types.
func isProtoPrimitive(t string) bool {
	switch t {
	case "double", "float", "int32", "int64", "uint32", "uint64",
		"sint32", "sint64", "fixed32", "fixed64", "sfixed32", "sfixed64",
		"bool", "string", "bytes":
		return true
	}
	return false
}

// protoQualify returns "parent.child" when inside a scope, or "child" at file level.
func protoQualify(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}
