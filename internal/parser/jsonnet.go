package parser

import (
	"path/filepath"
	"strings"

	jsonnetg "github.com/alexaandru/go-sitter-forest/jsonnet"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// JsonnetParser parses Jsonnet (.jsonnet, .libsonnet) source files.
// It extracts:
//   - Imports (local x = import 'path') → NodePackage + EdgeImports
//   - Local variables (local x = value) at file level → NodeVariable
//   - Object fields (field: value) → NodeVariable (exported)
//   - Hidden fields (field:: value) → NodeVariable (unexported)
//   - Function fields (field(params):: body) → NodeFunction
//   - Local functions inside objects (local f(x) = body) → NodeFunction
type JsonnetParser struct {
	language *sitter.Language
}

// NewJsonnetParser creates a ready-to-use JsonnetParser.
func NewJsonnetParser() *JsonnetParser {
	return &JsonnetParser{language: sitter.NewLanguage(jsonnetg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *JsonnetParser) Extensions() []string {
	return []string{".jsonnet", ".libsonnet"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *JsonnetParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single Jsonnet file.
//
// The Jsonnet grammar structures files as a chain of local_bind nodes:
//
//	local_bind → local + bind + ; + (next local_bind | object | expr)
//
// The final expression in the chain is typically an object literal containing
// the exported fields and functions.
func (p *JsonnetParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.IsNull() {
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

	// Walk the local_bind chain to collect all top-level bindings and find the
	// final expression (usually a top-level object).
	p.walkLocalBindChain(g, root, src, filePath, fileNodeID)

	return nil
}

// walkLocalBindChain traverses the chain of local_bind nodes that form the
// top-level of a Jsonnet file.
func (p *JsonnetParser) walkLocalBindChain(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if n.IsNull() {
		return
	}

	switch nodeType(n) {
	case "document":
		// document has a single child — the expression (usually a local_bind or object).
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if !child.IsNull() {
				p.walkLocalBindChain(g, child, src, filePath, fileNodeID)
			}
		}

	case "local_bind":
		// local_bind: local + bind + ; + body
		// body can be another local_bind or the final object.
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if child.IsNull() {
				continue
			}
			switch nodeType(child) {
			case "bind":
				p.handleBind(g, child, src, filePath, fileNodeID)
			case "local_bind":
				p.walkLocalBindChain(g, child, src, filePath, fileNodeID)
			case "object":
				p.extractObjectMembers(g, child, src, filePath, fileNodeID)
			case "function", "anonymous_function":
				// function(params) { fields... } — extract the object body inside.
				p.extractFunctionBody(g, child, src, filePath, fileNodeID)
			}
		}

	case "object":
		p.extractObjectMembers(g, n, src, filePath, fileNodeID)

	case "function", "anonymous_function":
		// Top-level function(params) { fields... } pattern.
		p.extractFunctionBody(g, n, src, filePath, fileNodeID)
	}
}

// handleBind processes a bind node at the top level of a local_bind.
// It emits NodePackage for imports and NodeVariable for other bindings.
func (p *JsonnetParser) handleBind(
	g *graph.Graph,
	bind sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if bind.IsNull() {
		return
	}

	// bind: id + = + value
	idNode := firstChildOfType(bind, "id")
	if idNode.IsNull() {
		return
	}
	name := childText(idNode, src)
	if name == "" {
		return
	}

	startLine := int(bind.StartPoint().Row) + 1

	// Find the value (everything after the = sign).
	foundEq := false
	for i := uint32(0); i < bind.ChildCount(); i++ {
		child := bind.Child(i)
		if child.IsNull() {
			continue
		}
		if nodeType(child) == "=" {
			foundEq = true
			continue
		}
		if !foundEq {
			continue
		}
		// This is the value node.
		if nodeType(child) == "import" {
			// local x = import 'path'
			importPath := jsonnetExtractImportPath(child, src)
			if importPath == "" {
				importPath = name
			}
			importNodeID := g.MakeNodeID(importPath, importPath)
			g.AddNode(&graph.Node{
				ID: importNodeID, Type: graph.NodePackage, Name: importPath,
				Package: importPath, File: filePath, Line: startLine,
				Metadata: map[string]string{"path": importPath, "alias": name},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
		} else {
			// Check if this bind has params (i.e., local function).
			hasParams := false
			for j := uint32(0); j < bind.ChildCount(); j++ {
				if bc := bind.Child(j); !bc.IsNull() && nodeType(bc) == "params" {
					hasParams = true
					break
				}
			}

			if hasParams {
				// local f(x) = expr — emit as NodeFunction.
				fnNodeID := g.MakeNodeID(filePath, "fn_"+name)
				if g.GetNode(fnNodeID) == nil {
					g.AddNode(&graph.Node{
						ID: fnNodeID, Type: graph.NodeFunction, Name: name,
						File: filePath, Line: startLine,
						Exported: false,
						Metadata: map[string]string{"kind": "local_function"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: fnNodeID, Type: graph.EdgeDefines})
				}
			} else {
				// local x = value — emit as NodeVariable.
				nodeID := g.MakeNodeID(filePath, name)
				if g.GetNode(nodeID) == nil {
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeVariable, Name: name,
						File: filePath, Line: startLine,
						Exported: false,
						Metadata: map[string]string{"kind": "local"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
		}
		break
	}
}

// extractObjectMembers processes the members of a top-level Jsonnet object.
// Emits NodeVariable for fields, NodeFunction for function fields.
func (p *JsonnetParser) extractObjectMembers(
	g *graph.Graph,
	obj sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if obj.IsNull() {
		return
	}

	for i := uint32(0); i < obj.ChildCount(); i++ {
		member := obj.Child(i)
		if member.IsNull() || nodeType(member) != "member" {
			continue
		}
		// member can contain: objlocal (local helper), or field.
		for j := uint32(0); j < member.ChildCount(); j++ {
			child := member.Child(j)
			if child.IsNull() {
				continue
			}
			switch nodeType(child) {
			case "objlocal":
				p.handleObjLocal(g, child, src, filePath, fileNodeID)
			case "field":
				p.handleField(g, child, src, filePath, fileNodeID)
			}
		}
	}
}

// handleObjLocal processes a `local f(x) = body` inside an object.
// Emits NodeFunction.
func (p *JsonnetParser) handleObjLocal(
	g *graph.Graph,
	objlocal sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if objlocal.IsNull() {
		return
	}
	bindNode := firstChildOfType(objlocal, "bind")
	if bindNode.IsNull() {
		return
	}
	idNode := firstChildOfType(bindNode, "id")
	if idNode.IsNull() {
		return
	}
	name := childText(idNode, src)
	if name == "" {
		return
	}
	startLine := int(objlocal.StartPoint().Row) + 1
	nodeID := g.MakeNodeID(filePath, "local_fn_"+name)
	if g.GetNode(nodeID) == nil {
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name,
			File: filePath, Line: startLine,
			Exported: false,
			Metadata: map[string]string{"kind": "local_function"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// handleField processes a `field: value` or `field(params):: body` member.
func (p *JsonnetParser) handleField(
	g *graph.Graph,
	field sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if field.IsNull() {
		return
	}

	// Extract fieldname.
	fieldnameNode := firstChildOfType(field, "fieldname")
	if fieldnameNode.IsNull() {
		return
	}
	name := strings.TrimSpace(childText(fieldnameNode, src))
	if name == "" {
		return
	}

	startLine := int(field.StartPoint().Row) + 1

	// Determine if this is a function field (has params: `field(params):: body`)
	// and whether it is hidden (:: vs :).
	isFunction := false
	isHidden := false
	for i := uint32(0); i < field.ChildCount(); i++ {
		child := field.Child(i)
		if child.IsNull() {
			continue
		}
		switch nodeType(child) {
		case "params":
			isFunction = true
		case "::":
			isHidden = true
		}
	}

	uniqueName := name
	if isFunction {
		uniqueName = "fn_" + name
	}

	nodeID := g.MakeNodeID(filePath, uniqueName)
	if g.GetNode(nodeID) != nil {
		return
	}

	if isFunction {
		kind := "function"
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeFunction, Name: name,
			File: filePath, Line: startLine,
			Exported: !isHidden,
			Metadata: map[string]string{"kind": kind},
		})
	} else {
		kind := "field"
		if isHidden {
			kind = "hidden_field"
		}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeVariable, Name: name,
			File: filePath, Line: startLine,
			Exported: !isHidden,
			Metadata: map[string]string{"kind": kind},
		})
	}
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractFunctionBody handles top-level `function(params) body` expressions.
// The body is typically an object whose members should be extracted as exports.
// It also extracts the function's parameters as NodeVariable nodes.
func (p *JsonnetParser) extractFunctionBody(
	g *graph.Graph,
	fn sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if fn.IsNull() {
		return
	}
	// Extract parameters from the params node.
	for i := uint32(0); i < fn.ChildCount(); i++ {
		child := fn.Child(i)
		if child.IsNull() {
			continue
		}
		switch nodeType(child) {
		case "params":
			p.extractFunctionParams(g, child, src, filePath, fileNodeID)
		case "object":
			p.extractObjectMembers(g, child, src, filePath, fileNodeID)
		case "local_bind":
			p.walkLocalBindChain(g, child, src, filePath, fileNodeID)
		}
	}
}

// extractFunctionParams extracts parameter names from a params node as NodeVariable.
func (p *JsonnetParser) extractFunctionParams(
	g *graph.Graph,
	params sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if params.IsNull() {
		return
	}
	for i := uint32(0); i < params.ChildCount(); i++ {
		child := params.Child(i)
		if child.IsNull() {
			continue
		}
		// param nodes contain an id child.
		if nodeType(child) == "param" {
			idNode := firstChildOfType(child, "id")
			if idNode.IsNull() {
				continue
			}
			name := childText(idNode, src)
			if name == "" {
				continue
			}
			startLine := int(child.StartPoint().Row) + 1
			nodeID := g.MakeNodeID(filePath, "param_"+name)
			if g.GetNode(nodeID) == nil {
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeVariable, Name: name,
					File: filePath, Line: startLine,
					Exported: true,
					Metadata: map[string]string{"kind": "parameter"},
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
		}
	}
}

// jsonnetExtractImportPath returns the import path string from an import node.
// import: 'import' keyword + string node → string_content child
func jsonnetExtractImportPath(importNode sitter.Node, src []byte) string {
	if importNode.IsNull() {
		return ""
	}
	strNode := firstChildOfType(importNode, "string")
	if strNode.IsNull() {
		return strings.Trim(childText(importNode, src), "'\"")
	}
	// string: string_start + string_content + string_end
	for i := uint32(0); i < strNode.ChildCount(); i++ {
		child := strNode.Child(i)
		if !child.IsNull() && nodeType(child) == "string_content" {
			return childText(child, src)
		}
	}
	// Fallback: strip quotes from the whole string node text.
	raw := childText(strNode, src)
	return strings.Trim(raw, "'\"")
}
