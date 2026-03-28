package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	jsong "github.com/alexaandru/go-sitter-forest/json"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// JSONParser parses JSON (.json) files with filename-aware depth control.
// Full deep parsing is performed for known high-value filenames (package.json,
// tsconfig.json, jsconfig.json, *.schema.json). All other JSON files get
// shallow top-level-key extraction only.
type JSONParser struct {
	language *sitter.Language
}

// NewJSONParser creates a ready-to-use JSONParser.
func NewJSONParser() *JSONParser {
	return &JSONParser{language: sitter.NewLanguage(jsong.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *JSONParser) Extensions() []string {
	return []string{".json"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *JSONParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single JSON file and merges them
// into the graph.
//
// Filename-aware strategy:
//   - package.json: deep parse (name, version, dependencies, scripts)
//   - tsconfig.json / jsconfig.json: compilerOptions and include/exclude
//   - *.schema.json / schema.json: $defs/definitions as NodeStruct, properties as NodeField
//   - all other .json: top-level keys as NodeField (1 level deep only)
func (p *JSONParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	if len(src) == 0 {
		return nil
	}

	// Find the root object node (document → object).
	rootObj := jsonFindRootObject(root)
	if rootObj.IsNull() {
		return nil
	}

	base := filepath.Base(filePath)
	switch {
	case base == "package.json":
		p.parsePackageJSON(g, rootObj, src, filePath, fileNodeID)
	case base == "tsconfig.json" || base == "jsconfig.json":
		p.parseTSConfig(g, rootObj, src, filePath, fileNodeID)
	case base == "schema.json" || strings.HasSuffix(base, ".schema.json"):
		p.parseSchemaJSON(g, rootObj, src, filePath, fileNodeID)
	default:
		// OpenAPI/Swagger JSON: detect by presence of ("openapi" or "swagger") + "paths" keys.
		if p.isOpenAPIJSON(rootObj, src) {
			p.parseOpenAPIJSON(g, rootObj, src, filePath, fileNodeID)
		} else {
			p.parseGenericJSON(g, rootObj, src, filePath, fileNodeID)
		}
	}

	return nil
}

// parsePackageJSON extracts entities from a package.json file.
// Extracts: name and version as file metadata, dependencies/devDependencies/
// peerDependencies as NodeField, scripts as NodeField.
func (p *JSONParser) parsePackageJSON(
	g *graph.Graph,
	obj sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	pairs := jsonExtractObjectPairs(obj, src)

	// First pass: extract name and version as metadata on the file node.
	fileNode := g.GetNode(fileNodeID)
	for _, kv := range pairs {
		switch kv.key {
		case "name", "version":
			if kv.valueType == "string" {
				if fileNode != nil {
					if fileNode.Metadata == nil {
						fileNode.Metadata = make(map[string]string)
					}
					fileNode.Metadata[kv.key] = kv.valueStr
				}
			}
		}
	}

	// Second pass: extract dependencies and scripts.
	for _, kv := range pairs {
		switch kv.key {
		case "dependencies":
			p.extractPackageDepSection(g, kv.valueNode, src, filePath, fileNodeID, "dependency")
		case "devDependencies":
			p.extractPackageDepSection(g, kv.valueNode, src, filePath, fileNodeID, "dev_dependency")
		case "peerDependencies":
			p.extractPackageDepSection(g, kv.valueNode, src, filePath, fileNodeID, "peer_dependency")
		case "scripts":
			p.extractScriptsSection(g, kv.valueNode, src, filePath, fileNodeID)
		case "workspaces":
			startLine := kv.startLine
			if startLine == 0 {
				startLine = 1
			}
			fieldNodeID := g.MakeNodeID(filePath, "config:workspaces")
			g.AddNode(&graph.Node{
				ID:       fieldNodeID,
				Type:     graph.NodeVariable,
				Name:     "workspaces",
				File:     filePath,
				Line:     startLine,
				Exported: true,
				Metadata: map[string]string{"kind": "config"},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
		}
	}
}

// extractPackageDepSection extracts each key from a dependencies object as NodeField.
func (p *JSONParser) extractPackageDepSection(
	g *graph.Graph,
	obj sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	kind string,
) {
	if obj.IsNull() {
		return
	}
	pairs := jsonExtractObjectPairs(obj, src)
	for _, kv := range pairs {
		if kv.key == "" {
			continue
		}
		startLine := kv.startLine
		if startLine == 0 {
			startLine = int(obj.StartPoint().Row) + 1
		}
		meta := map[string]string{"kind": kind}
		if kv.valueStr != "" {
			meta["version"] = kv.valueStr
		}
		fieldNodeID := g.MakeNodeID(filePath, kind+":"+kv.key)
		g.AddNode(&graph.Node{
			ID:       fieldNodeID,
			Type:     graph.NodeVariable,
			Name:     kv.key,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDependsOn})
	}
}

// extractScriptsSection extracts each key from a scripts object as NodeField.
func (p *JSONParser) extractScriptsSection(
	g *graph.Graph,
	obj sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	if obj.IsNull() {
		return
	}
	pairs := jsonExtractObjectPairs(obj, src)
	for _, kv := range pairs {
		if kv.key == "" {
			continue
		}
		startLine := kv.startLine
		if startLine == 0 {
			startLine = int(obj.StartPoint().Row) + 1
		}
		meta := map[string]string{"kind": "script"}
		if kv.valueStr != "" {
			meta["value"] = kv.valueStr
		}
		fieldNodeID := g.MakeNodeID(filePath, "script:"+kv.key)
		g.AddNode(&graph.Node{
			ID:       fieldNodeID,
			Type:     graph.NodeVariable,
			Name:     kv.key,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
	}
}

// parseTSConfig extracts entities from tsconfig.json / jsconfig.json.
// Extracts compilerOptions pairs as NodeField with kind=compiler_option,
// and include/exclude as NodeField with kind=config.
func (p *JSONParser) parseTSConfig(
	g *graph.Graph,
	obj sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	pairs := jsonExtractObjectPairs(obj, src)
	for _, kv := range pairs {
		switch kv.key {
		case "compilerOptions":
			if !kv.valueNode.IsNull() {
				subPairs := jsonExtractObjectPairs(kv.valueNode, src)
				for _, sub := range subPairs {
					if sub.key == "" {
						continue
					}
					startLine := sub.startLine
					if startLine == 0 {
						startLine = int(kv.valueNode.StartPoint().Row) + 1
					}
					meta := map[string]string{"kind": "compiler_option"}
					if sub.valueStr != "" {
						meta["value"] = sub.valueStr
					}
					fieldNodeID := g.MakeNodeID(filePath, "compilerOption:"+sub.key)
					g.AddNode(&graph.Node{
						ID:       fieldNodeID,
						Type:     graph.NodeVariable,
						Name:     sub.key,
						File:     filePath,
						Line:     startLine,
						Exported: true,
						Metadata: meta,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
				}
			}
		case "include", "exclude":
			startLine := int(obj.StartPoint().Row) + 1
			if kv.startLine > 0 {
				startLine = kv.startLine
			}
			meta := map[string]string{"kind": "config"}
			fieldNodeID := g.MakeNodeID(filePath, "config:"+kv.key)
			g.AddNode(&graph.Node{
				ID:       fieldNodeID,
				Type:     graph.NodeVariable,
				Name:     kv.key,
				File:     filePath,
				Line:     startLine,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
		}
	}
}

// parseSchemaJSON extracts entities from JSON Schema files (*.schema.json, schema.json).
// Extracts $defs/definitions keys as NodeStruct, properties keys as NodeField.
func (p *JSONParser) parseSchemaJSON(
	g *graph.Graph,
	obj sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	pairs := jsonExtractObjectPairs(obj, src)
	for _, kv := range pairs {
		switch kv.key {
		case "$defs", "definitions":
			if !kv.valueNode.IsNull() {
				defPairs := jsonExtractObjectPairs(kv.valueNode, src)
				for _, def := range defPairs {
					if def.key == "" {
						continue
					}
					startLine := def.startLine
					if startLine == 0 {
						startLine = int(kv.valueNode.StartPoint().Row) + 1
					}
					structNodeID := g.MakeNodeID(filePath, "def:"+def.key)
					g.AddNode(&graph.Node{
						ID:       structNodeID,
						Type:     graph.NodeStruct,
						Name:     def.key,
						File:     filePath,
						Line:     startLine,
						Exported: true,
						Metadata: map[string]string{"kind": "schema_type"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: structNodeID, Type: graph.EdgeDefines})
				}
			}
		case "properties":
			if !kv.valueNode.IsNull() {
				propPairs := jsonExtractObjectPairs(kv.valueNode, src)
				for _, prop := range propPairs {
					if prop.key == "" {
						continue
					}
					startLine := prop.startLine
					if startLine == 0 {
						startLine = int(kv.valueNode.StartPoint().Row) + 1
					}
					fieldNodeID := g.MakeNodeID(filePath, "prop:"+prop.key)
					g.AddNode(&graph.Node{
						ID:       fieldNodeID,
						Type:     graph.NodeVariable,
						Name:     prop.key,
						File:     filePath,
						Line:     startLine,
						Exported: true,
						Metadata: map[string]string{"kind": "property"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
				}
			}
		}
	}
}

// parseGenericJSON extracts top-level keys from any unknown JSON file.
// Only goes 1 level deep (top-level object keys only).
func (p *JSONParser) parseGenericJSON(
	g *graph.Graph,
	obj sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	pairs := jsonExtractObjectPairs(obj, src)
	for _, kv := range pairs {
		if kv.key == "" {
			continue
		}
		startLine := kv.startLine
		if startLine == 0 {
			startLine = int(obj.StartPoint().Row) + 1
		}
		meta := map[string]string{"kind": "field"}
		if kv.valueStr != "" {
			meta["value"] = kv.valueStr
		}
		fieldNodeID := g.MakeNodeID(filePath, "field:"+kv.key)
		g.AddNode(&graph.Node{
			ID:       fieldNodeID,
			Type:     graph.NodeVariable,
			Name:     kv.key,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: fieldNodeID, Type: graph.EdgeDefines})
	}
}

// jsonKVPair holds a parsed key-value pair from a JSON object.
type jsonKVPair struct {
	key       string      // key name (string_content extracted, quotes stripped)
	valueNode sitter.Node // the raw value node (may be object/array/etc.)
	valueStr  string      // string representation of the value (for scalars)
	valueType string      // "string", "number", "true", "false", "null", "object", "array"
	startLine int         // 1-indexed line of the key node
}

// jsonExtractObjectPairs extracts all key-value pairs from a JSON object node.
// The object grammar is: "{" pair* "}" where each pair is: string ":" value.
func jsonExtractObjectPairs(obj sitter.Node, src []byte) []jsonKVPair {
	if obj.IsNull() || obj.Type() != "object" {
		return nil
	}
	var result []jsonKVPair
	for i := uint32(0); i < obj.ChildCount(); i++ {
		child := obj.Child(i)
		if child.IsNull() || child.Type() != "pair" {
			continue
		}
		kv := jsonExtractPair(child, src)
		if kv.key != "" {
			result = append(result, kv)
		}
	}
	return result
}

// jsonExtractPair extracts a single key-value pair from a JSON pair node.
// pair: string ":" value
// string: '"' string_content '"' — we extract string_content for clean key names.
func jsonExtractPair(pair sitter.Node, src []byte) jsonKVPair {
	var kv jsonKVPair
	// pair children: [0]=string(key), [1]=":", [2]=value
	// Some grammars may vary — we scan all children.
	pastColon := false
	for i := uint32(0); i < pair.ChildCount(); i++ {
		child := pair.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		if ct == ":" {
			pastColon = true
			continue
		}
		if !pastColon {
			// This is the key node.
			if ct == "string" {
				kv.key = jsonExtractStringContent(child, src)
				kv.startLine = int(child.StartPoint().Row) + 1
			}
		} else {
			// This is the value node.
			kv.valueNode = child
			kv.valueType = ct
			switch ct {
			case "string":
				kv.valueStr = jsonExtractStringContent(child, src)
			case "number", "true", "false", "null":
				kv.valueStr = strings.TrimSpace(childText(child, src))
			case "object", "array":
				// For complex types, leave valueStr empty — callers use valueNode.
				kv.valueStr = ""
			}
		}
	}
	return kv
}

// jsonExtractStringContent extracts the text from a JSON string node by finding
// the string_content child. This cleanly strips the surrounding quotes.
// Falls back to stripping quotes manually if string_content child is not found.
func jsonExtractStringContent(strNode sitter.Node, src []byte) string {
	if strNode.IsNull() {
		return ""
	}
	// Look for a string_content child.
	for i := uint32(0); i < strNode.ChildCount(); i++ {
		child := strNode.Child(i)
		if !child.IsNull() && child.Type() == "string_content" {
			return childText(child, src)
		}
	}
	// Fallback: strip surrounding quotes from raw text.
	raw := childText(strNode, src)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

// jsonFindRootObject traverses from the document root to find the top-level object.
// JSON grammar: document → object (or array, but we only handle objects at the top level).
func jsonFindRootObject(root sitter.Node) sitter.Node {
	if root.IsNull() {
		return sitter.Node{}
	}
	// The document node's first non-trivial child should be the object.
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if !child.IsNull() && child.Type() == "object" {
			return child
		}
	}
	// If root itself is an object (some grammar variants).
	if root.Type() == "object" {
		return root
	}
	return sitter.Node{}
}

// isOpenAPIJSON returns true if the JSON root object contains ("openapi" or "swagger")
// AND "paths" keys — the minimal signature of an OpenAPI/Swagger document.
func (p *JSONParser) isOpenAPIJSON(obj sitter.Node, src []byte) bool {
	if obj.IsNull() {
		return false
	}
	var hasSpec, hasPaths bool
	for _, kv := range jsonExtractObjectPairs(obj, src) {
		switch kv.key {
		case "openapi", "swagger":
			hasSpec = true
		case "paths":
			hasPaths = true
		}
		if hasSpec && hasPaths {
			return true
		}
	}
	return false
}

// parseOpenAPIJSON extracts API entities from an OpenAPI/Swagger JSON document.
// Endpoints (paths × methods) are emitted as NodeRoute with Domain DomainAPI.
// Schema definitions (components.schemas / definitions) are emitted as NodeStruct.
// EdgeDependsOn edges are created from each endpoint to any $ref schema types it uses.
func (p *JSONParser) parseOpenAPIJSON(
	g *graph.Graph,
	obj sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	pairs := jsonExtractObjectPairs(obj, src)
	pairMap := make(map[string]jsonKVPair, len(pairs))
	for _, kv := range pairs {
		pairMap[kv.key] = kv
	}

	// Extract info.title / info.version as file node metadata.
	if infoKV, ok := pairMap["info"]; ok && !infoKV.valueNode.IsNull() {
		fileNode := g.GetNode(fileNodeID)
		if fileNode != nil {
			if fileNode.Metadata == nil {
				fileNode.Metadata = make(map[string]string)
			}
			for _, sub := range jsonExtractObjectPairs(infoKV.valueNode, src) {
				if (sub.key == "title" || sub.key == "version") && sub.valueStr != "" {
					fileNode.Metadata[sub.key] = sub.valueStr
				}
			}
		}
	}

	// Extract schema definitions: OpenAPI 3.x components.schemas, Swagger 2.x definitions.
	p.extractJSONOpenAPISchemas(g, src, filePath, fileNodeID, pairMap)

	// Extract paths → endpoint nodes.
	if pathsKV, ok := pairMap["paths"]; ok && !pathsKV.valueNode.IsNull() {
		p.extractJSONOpenAPIPaths(g, src, filePath, fileNodeID, pathsKV.valueNode)
	}
}

// httpMethodSet is the set of HTTP methods recognised in OpenAPI path items.
var httpMethodSet = map[string]bool{
	"get": true, "post": true, "put": true, "delete": true,
	"patch": true, "head": true, "options": true, "trace": true,
}

// extractJSONOpenAPIPaths extracts endpoint nodes from the "paths" object.
func (p *JSONParser) extractJSONOpenAPIPaths(
	g *graph.Graph,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	pathsObj sitter.Node,
) {
	for _, pathKV := range jsonExtractObjectPairs(pathsObj, src) {
		pathStr := pathKV.key // e.g. "/users/{id}"
		if pathStr == "" || pathKV.valueNode.IsNull() {
			continue
		}
		for _, methodKV := range jsonExtractObjectPairs(pathKV.valueNode, src) {
			method := strings.ToLower(methodKV.key)
			if !httpMethodSet[method] {
				continue
			}
			upperMethod := strings.ToUpper(method)

			// Try to find operationId inside the operation object.
			nodeName := upperMethod + " " + pathStr
			line := methodKV.startLine
			if line == 0 {
				line = 1
			}
			if !methodKV.valueNode.IsNull() {
				for _, opKV := range jsonExtractObjectPairs(methodKV.valueNode, src) {
					if opKV.key == "operationId" && opKV.valueStr != "" {
						nodeName = opKV.valueStr
						if opKV.startLine > 0 {
							line = opKV.startLine
						}
						break
					}
				}
			}

			nodeID := g.MakeNodeID(filePath, "endpoint:"+upperMethod+":"+pathStr)
			if g.GetNode(nodeID) == nil {
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeRoute,
					Name:     nodeName,
					File:     filePath,
					Line:     line,
					Exported: true,
					Domain:   graph.DomainAPI,
					Metadata: map[string]string{
						"kind":   "openapi_endpoint",
						"method": upperMethod,
						"path":   pathStr,
					},
				})
			}
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

			// Create EdgeDependsOn from endpoint to referenced schema types.
			if !methodKV.valueNode.IsNull() {
				refs := jsonCollectOpenAPIRefs(methodKV.valueNode, src, nil)
				seen := make(map[string]bool, len(refs))
				for _, ref := range refs {
					name := openAPIRefToSchemaName(ref)
					if name == "" || seen[name] {
						continue
					}
					seen[name] = true
					schemaID := g.MakeNodeID(filePath, "schema:"+name)
					if g.GetNode(schemaID) != nil {
						g.AddEdge(&graph.Edge{From: nodeID, To: schemaID, Type: graph.EdgeDependsOn})
					}
				}
			}
		}
	}
}

// extractJSONOpenAPISchemas extracts schema type nodes from components.schemas (v3)
// or definitions (v2).
func (p *JSONParser) extractJSONOpenAPISchemas(
	g *graph.Graph,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	pairMap map[string]jsonKVPair,
) {
	// OpenAPI 3.x: components.schemas
	if compKV, ok := pairMap["components"]; ok && !compKV.valueNode.IsNull() {
		for _, sub := range jsonExtractObjectPairs(compKV.valueNode, src) {
			if sub.key == "schemas" && !sub.valueNode.IsNull() {
				p.emitJSONSchemaNodes(g, src, filePath, fileNodeID, sub.valueNode)
				break
			}
		}
	}
	// Swagger 2.x: definitions
	if defsKV, ok := pairMap["definitions"]; ok && !defsKV.valueNode.IsNull() {
		p.emitJSONSchemaNodes(g, src, filePath, fileNodeID, defsKV.valueNode)
	}
}

// emitJSONSchemaNodes creates NodeStruct nodes for each key in a schemas/definitions object.
func (p *JSONParser) emitJSONSchemaNodes(
	g *graph.Graph,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	schemasObj sitter.Node,
) {
	for _, kv := range jsonExtractObjectPairs(schemasObj, src) {
		name := kv.key
		if name == "" {
			continue
		}
		line := kv.startLine
		if line == 0 {
			line = 1
		}
		nodeID := g.MakeNodeID(filePath, "schema:"+name)
		if g.GetNode(nodeID) == nil {
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     line,
				Exported: true,
				Domain:   graph.DomainAPI,
				Metadata: map[string]string{"kind": "openapi_schema"},
			})
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// jsonCollectOpenAPIRefs recursively collects all "$ref" string values from a
// JSON object subtree. Used to find schema references in operation bodies.
func jsonCollectOpenAPIRefs(node sitter.Node, src []byte, out []string) []string {
	if node.IsNull() {
		return out
	}
	switch node.Type() {
	case "object":
		for _, kv := range jsonExtractObjectPairs(node, src) {
			if kv.key == "$ref" && kv.valueStr != "" {
				out = append(out, kv.valueStr)
			} else if !kv.valueNode.IsNull() {
				out = jsonCollectOpenAPIRefs(kv.valueNode, src, out)
			}
		}
	case "array":
		for i := uint32(0); i < node.ChildCount(); i++ {
			child := node.Child(i)
			if !child.IsNull() {
				out = jsonCollectOpenAPIRefs(child, src, out)
			}
		}
	}
	return out
}
