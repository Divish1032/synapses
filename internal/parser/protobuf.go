package parser

import (
	"context"
	"path/filepath"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/protobuf"

	"github.com/Divish1032/synapses/internal/graph"
)

// ProtobufParser parses Protocol Buffer (.proto) source files.
type ProtobufParser struct {
	language *sitter.Language
}

// NewProtobufParser creates a ready-to-use ProtobufParser.
func NewProtobufParser() *ProtobufParser {
	return &ProtobufParser{language: protobuf.GetLanguage()}
}

// Extensions returns the file extensions handled by this parser.
func (p *ProtobufParser) Extensions() []string {
	return []string{".proto"}
}

// Parse extracts code entities from a single .proto file and merges them into the graph.
// Captured constructs: message definitions, service definitions, rpc methods, enum definitions.
func (p *ProtobufParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseCtx(context.Background(), nil, src)
	root := tree.RootNode()

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lang := p.language

	// --- message definitions ---
	messageQuery := `(message_name (identifier) @message_name)`
	if err := runQuery(lang, root, src, messageQuery, func(captures map[string]string, startLine int) {
		name := captures["message_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- service definitions ---
	serviceQuery := `(service_name (identifier) @service_name)`
	if err := runQuery(lang, root, src, serviceQuery, func(captures map[string]string, startLine int) {
		name := captures["service_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeInterface,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- rpc methods ---
	rpcQuery := `(rpc_name (identifier) @rpc_name)`
	if err := runQuery(lang, root, src, rpcQuery, func(captures map[string]string, startLine int) {
		name := captures["rpc_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeMethod,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	// --- enum definitions ---
	enumQuery := `(enum_name (identifier) @enum_name)`
	if err := runQuery(lang, root, src, enumQuery, func(captures map[string]string, startLine int) {
		name := captures["enum_name"]
		if name == "" {
			return
		}
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}); err != nil {
		return err
	}

	return nil
}
