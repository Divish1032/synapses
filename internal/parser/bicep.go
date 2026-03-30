package parser

import (
	"path/filepath"
	"strings"

	bicepg "github.com/alexaandru/go-sitter-forest/bicep"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// BicepParser parses Azure Bicep (.bicep) infrastructure-as-code files.
// It extracts:
//   - Parameters (param name type = default) → NodeVariable (kind=parameter)
//   - Variables (var name = value) → NodeVariable (kind=variable)
//   - Resources (resource symbolicName 'Type@api' = {...}) → NodeStruct (kind=resource)
//   - Modules (module symbolicName 'path' = {...}) → NodeStruct (kind=module)
//   - Outputs (output name type = value) → NodeVariable (kind=output)
//
// Decorators (@description, @allowed) preceding declarations are collected
// and stored in metadata.
type BicepParser struct {
	language *sitter.Language
}

// NewBicepParser creates a ready-to-use BicepParser.
func NewBicepParser() *BicepParser {
	return &BicepParser{language: sitter.NewLanguage(bicepg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *BicepParser) Extensions() []string {
	return []string{".bicep"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *BicepParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single Bicep file.
//
// The Bicep grammar root node is "infrastructure". Its direct children are:
//   - decorators: one or more @decorator nodes preceding a declaration
//   - parameter_declaration, variable_declaration, resource_declaration,
//     module_declaration, output_declaration
//
// Decorators are collected as they appear and applied to the immediately
// following declaration node.
func (p *BicepParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	// Walk infrastructure children, carrying pending decorator text forward.
	var pendingDecorators []string

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}

		switch nodeType(child) {
		case "decorators":
			pendingDecorators = bicepExtractDecorators(child, src)

		case "parameter_declaration":
			p.handleParam(g, child, src, filePath, fileNodeID, pendingDecorators)
			pendingDecorators = nil

		case "variable_declaration":
			p.handleVar(g, child, src, filePath, fileNodeID)
			pendingDecorators = nil

		case "resource_declaration":
			p.handleResource(g, child, src, filePath, fileNodeID, pendingDecorators)
			pendingDecorators = nil

		case "module_declaration":
			p.handleModule(g, child, src, filePath, fileNodeID)
			pendingDecorators = nil

		case "output_declaration":
			p.handleOutput(g, child, src, filePath, fileNodeID)
			pendingDecorators = nil

		case "type_declaration":
			p.handleTypeDecl(g, child, src, filePath, fileNodeID, pendingDecorators)
			pendingDecorators = nil

		case "function_declaration":
			p.handleFuncDecl(g, child, src, filePath, fileNodeID, pendingDecorators)
			pendingDecorators = nil

		case "target_scope_assignment":
			p.handleTargetScope(g, child, src, filePath, fileNodeID)
			pendingDecorators = nil

		case "metadata_declaration":
			p.handleMetadata(g, child, src, filePath, fileNodeID)
			pendingDecorators = nil

		default:
			// Unknown node type — reset decorator accumulator.
			pendingDecorators = nil
		}
	}

	return nil
}

// ─── Declaration handlers ─────────────────────────────────────────────────────

// handleParam processes: param storageAccountName string = 'mystorage'
// AST: parameter_declaration → param + identifier + type + [= + value]
func (p *BicepParser) handleParam(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	decorators []string,
) {
	name, typeStr, defaultVal := bicepExtractParamParts(n, src)
	if name == "" {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	meta := map[string]string{"kind": "parameter"}
	if typeStr != "" {
		meta["type"] = typeStr
	}
	if defaultVal != "" {
		meta["default"] = defaultVal
	}
	if len(decorators) > 0 {
		meta["decorators"] = strings.Join(decorators, "; ")
	}
	nodeID := g.MakeNodeID(filePath, "param_"+name)
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeVariable, Name: name,
		File: filePath, Line: startLine,
		Exported: true, Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleVar processes: var location = resourceGroup().location
// AST: variable_declaration → var + identifier + = + expr
func (p *BicepParser) handleVar(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	idNode := firstChildOfType(n, "identifier")
	if idNode.IsNull() {
		return
	}
	name := childText(idNode, src)
	if name == "" {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	nodeID := g.MakeNodeID(filePath, "var_"+name)
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeVariable, Name: name,
		File: filePath, Line: startLine,
		Exported: false,
		Metadata: map[string]string{"kind": "variable"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleResource processes:
//
//	resource storageAccount 'Microsoft.Storage/storageAccounts@2021-06-01' = { ... }
//
// AST: resource_declaration → resource + identifier + string + = + object
func (p *BicepParser) handleResource(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	decorators []string,
) {
	idNode := firstChildOfType(n, "identifier")
	if idNode.IsNull() {
		return
	}
	symbolicName := childText(idNode, src)
	if symbolicName == "" {
		return
	}

	// The string node holds 'Provider/Type@apiVersion'.
	typeStr, apiVersion := bicepExtractResourceType(n, src)
	startLine := int(n.StartPoint().Row) + 1

	meta := map[string]string{"kind": "resource"}
	if typeStr != "" {
		meta["type"] = typeStr
	}
	if apiVersion != "" {
		meta["api_version"] = apiVersion
	}
	if len(decorators) > 0 {
		meta["decorators"] = strings.Join(decorators, "; ")
	}

	// Detect `existing` keyword.
	for j := uint32(0); j < n.ChildCount(); j++ {
		if c := n.Child(j); !c.IsNull() && nodeType(c) == "existing" {
			meta["existing"] = "true"
			break
		}
	}

	// Extract name and location from object body.
	objNode := bicepFindObjectBody(n, src)
	if !objNode.IsNull() {
		if name := bicepGetObjectProp(objNode, "name", src); name != "" {
			meta["resource_name"] = name
		}
		if location := bicepGetObjectProp(objNode, "location", src); location != "" {
			meta["location"] = location
		}
	}

	nodeID := g.MakeNodeID(filePath, "resource_"+symbolicName)
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeStruct, Name: symbolicName,
		File: filePath, Line: startLine,
		Exported: true, Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleModule processes:
//
//	module webApp 'modules/webapp.bicep' = { ... }
//
// AST: module_declaration → module + identifier + string + = + object
func (p *BicepParser) handleModule(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	idNode := firstChildOfType(n, "identifier")
	if idNode.IsNull() {
		return
	}
	symbolicName := childText(idNode, src)
	if symbolicName == "" {
		return
	}

	modulePath := bicepExtractStringValue(n, src)
	startLine := int(n.StartPoint().Row) + 1

	meta := map[string]string{"kind": "module"}
	if modulePath != "" {
		meta["path"] = modulePath
	}

	nodeID := g.MakeNodeID(filePath, "module_"+symbolicName)
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeStruct, Name: symbolicName,
		File: filePath, Line: startLine,
		Exported: true, Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

	// Add EdgeImports for the module path.
	if modulePath != "" {
		importNodeID := g.MakeNodeID(modulePath, modulePath)
		g.AddNode(&graph.Node{
			ID: importNodeID, Type: graph.NodeFile, Name: filepath.Base(modulePath),
			File: modulePath, Line: 1,
		})
		g.AddEdge(&graph.Edge{From: nodeID, To: importNodeID, Type: graph.EdgeImports})
	}
}

// handleOutput processes: output storageEndpoint string = storageAccount.properties...
// AST: output_declaration → output + identifier + type + = + expr
func (p *BicepParser) handleOutput(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	idNode := firstChildOfType(n, "identifier")
	if idNode.IsNull() {
		return
	}
	name := childText(idNode, src)
	if name == "" {
		return
	}

	typeStr := bicepExtractTypeStr(n, src)
	startLine := int(n.StartPoint().Row) + 1
	meta := map[string]string{"kind": "output"}
	if typeStr != "" {
		meta["type"] = typeStr
	}

	nodeID := g.MakeNodeID(filePath, "output_"+name)
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeVariable, Name: name,
		File: filePath, Line: startLine,
		Exported: true, Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleTypeDecl processes: type fizz = string
// AST: type_declaration → type + identifier + = + type_expression
func (p *BicepParser) handleTypeDecl(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	decorators []string,
) {
	idNode := firstChildOfType(n, "identifier")
	if idNode.IsNull() {
		return
	}
	name := childText(idNode, src)
	if name == "" {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	meta := map[string]string{"kind": "type"}
	if len(decorators) > 0 {
		meta["decorators"] = strings.Join(decorators, "; ")
	}
	nodeID := g.MakeNodeID(filePath, "type_"+name)
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeStruct, Name: name,
		File: filePath, Line: startLine,
		Exported: true, Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleFuncDecl processes: func sayHello(name string) string => '...'
// AST: function_declaration → func + identifier + typed_lambda_expression
func (p *BicepParser) handleFuncDecl(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	decorators []string,
) {
	idNode := firstChildOfType(n, "identifier")
	if idNode.IsNull() {
		return
	}
	name := childText(idNode, src)
	if name == "" {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	meta := map[string]string{"kind": "function"}
	if len(decorators) > 0 {
		meta["decorators"] = strings.Join(decorators, "; ")
	}
	nodeID := g.MakeNodeID(filePath, "func_"+name)
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeFunction, Name: name,
		File: filePath, Line: startLine,
		Exported: true, Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleTargetScope processes: targetScope = 'subscription'
func (p *BicepParser) handleTargetScope(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	scopeVal := bicepExtractStringValue(n, src)
	if scopeVal == "" {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	nodeID := g.MakeNodeID(filePath, "targetScope")
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeVariable, Name: "targetScope",
		File: filePath, Line: startLine,
		Exported: true,
		Metadata: map[string]string{"kind": "targetScope", "value": scopeVal},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleMetadata processes: metadata name = 'value'
func (p *BicepParser) handleMetadata(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	idNode := firstChildOfType(n, "identifier")
	if idNode.IsNull() {
		return
	}
	name := childText(idNode, src)
	if name == "" {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	meta := map[string]string{"kind": "metadata"}
	// Try to extract value from string child.
	if val := bicepExtractStringValue(n, src); val != "" {
		meta["value"] = val
	}
	nodeID := g.MakeNodeID(filePath, "metadata_"+name)
	g.AddNode(&graph.Node{
		ID: nodeID, Type: graph.NodeVariable, Name: name,
		File: filePath, Line: startLine,
		Exported: false, Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// ─── Bicep AST helper functions ───────────────────────────────────────────────

// bicepExtractDecorators collects decorator texts from a decorators node.
// e.g. "@description('Storage account name')" → ["description('Storage account name')"]
func bicepExtractDecorators(decoratorsNode sitter.Node, src []byte) []string {
	var result []string
	for i := uint32(0); i < decoratorsNode.ChildCount(); i++ {
		child := decoratorsNode.Child(i)
		if child.IsNull() || nodeType(child) != "decorator" {
			continue
		}
		// decorator: @ + call_expression or identifier
		for j := uint32(0); j < child.ChildCount(); j++ {
			gc := child.Child(j)
			if gc.IsNull() || nodeType(gc) == "@" {
				continue
			}
			result = append(result, strings.TrimSpace(childText(gc, src)))
		}
	}
	return result
}

// bicepExtractParamParts extracts (name, type, defaultValue) from a parameter_declaration.
// AST: parameter_declaration → param + identifier + type + [= + value]
func bicepExtractParamParts(n sitter.Node, src []byte) (name, typeStr, defaultVal string) {
	idNode := firstChildOfType(n, "identifier")
	if !idNode.IsNull() {
		name = childText(idNode, src)
	}
	typeStr = bicepExtractTypeStr(n, src)

	// Find default value (the node after "=").
	seenEq := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if nodeType(child) == "=" {
			seenEq = true
			continue
		}
		if seenEq && nodeType(child) != "param" {
			defaultVal = strings.TrimSpace(childText(child, src))
			// Strip quotes from string defaults.
			if len(defaultVal) >= 2 && defaultVal[0] == '\'' && defaultVal[len(defaultVal)-1] == '\'' {
				defaultVal = defaultVal[1 : len(defaultVal)-1]
			}
			break
		}
	}
	return
}

// bicepExtractTypeStr extracts the type string from a declaration node.
// Looks for a "type" child node, then extracts the primitive_type text.
func bicepExtractTypeStr(n sitter.Node, src []byte) string {
	typeNode := firstChildOfType(n, "type")
	if typeNode.IsNull() {
		return ""
	}
	// type → primitive_type → string (keyword like "string", "int", "bool")
	for i := uint32(0); i < typeNode.ChildCount(); i++ {
		child := typeNode.Child(i)
		if child.IsNull() {
			continue
		}
		if nodeType(child) == "primitive_type" {
			for j := uint32(0); j < child.ChildCount(); j++ {
				gc := child.Child(j)
				if !gc.IsNull() {
					return childText(gc, src)
				}
			}
		}
	}
	return strings.TrimSpace(childText(typeNode, src))
}

// bicepExtractResourceType extracts (resourceType, apiVersion) from a resource_declaration.
// The type+version are stored in a string like 'Microsoft.Storage/storageAccounts@2021-06-01'.
func bicepExtractResourceType(n sitter.Node, src []byte) (resourceType, apiVersion string) {
	raw := bicepExtractStringValue(n, src)
	if raw == "" {
		return
	}
	if idx := strings.Index(raw, "@"); idx >= 0 {
		resourceType = raw[:idx]
		apiVersion = raw[idx+1:]
	} else {
		resourceType = raw
	}
	return
}

// bicepExtractStringValue finds the first string node in a declaration and returns
// its content (without surrounding quotes).
func bicepExtractStringValue(n sitter.Node, src []byte) string {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || nodeType(child) != "string" {
			continue
		}
		// string: ' + string_content + '
		for j := uint32(0); j < child.ChildCount(); j++ {
			gc := child.Child(j)
			if !gc.IsNull() && nodeType(gc) == "string_content" {
				return childText(gc, src)
			}
		}
		// Fallback: strip quotes from whole string text.
		raw := childText(child, src)
		return strings.Trim(raw, "'\"")
	}
	return ""
}

// bicepFindObjectBody finds the object node in a resource or module declaration.
// The object may be a direct child, or nested inside an if_statement (conditional
// resource) or for_statement (loop resource).
func bicepFindObjectBody(n sitter.Node, src []byte) sitter.Node {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if nodeType(child) == "object" {
			return child
		}
		if nodeType(child) == "if_statement" || nodeType(child) == "for_statement" {
			for j := uint32(0); j < child.ChildCount(); j++ {
				gc := child.Child(j)
				if !gc.IsNull() && nodeType(gc) == "object" {
					return gc
				}
			}
		}
	}
	return sitter.Node{}
}

// bicepGetObjectProp finds an object_property by key name and returns its value text.
// object: { + object_property* + }
// object_property: identifier + : + value
func bicepGetObjectProp(objNode sitter.Node, key string, src []byte) string {
	if objNode.IsNull() {
		return ""
	}
	for i := uint32(0); i < objNode.ChildCount(); i++ {
		prop := objNode.Child(i)
		if prop.IsNull() || nodeType(prop) != "object_property" {
			continue
		}
		// First identifier child is the key.
		keyNode := firstChildOfType(prop, "identifier")
		if keyNode.IsNull() || childText(keyNode, src) != key {
			continue
		}
		// Value is after the ":".
		seenColon := false
		for j := uint32(0); j < prop.ChildCount(); j++ {
			child := prop.Child(j)
			if child.IsNull() {
				continue
			}
			if nodeType(child) == ":" {
				seenColon = true
				continue
			}
			if seenColon {
				val := strings.TrimSpace(childText(child, src))
				// Strip quotes from string values.
				if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
					return val[1 : len(val)-1]
				}
				return val
			}
		}
	}
	return ""
}
