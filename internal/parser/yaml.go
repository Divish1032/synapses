package parser

import (
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// YAMLParser parses YAML (.yaml, .yml) files.
// Covers Kubernetes manifests, Docker Compose, GitHub Actions, Ansible,
// Helm, OpenAPI, and general config.
// Uses gopkg.in/yaml.v3 for proper AST-level parsing (not regex).
type YAMLParser struct{}

// NewYAMLParser creates a ready-to-use YAMLParser.
func NewYAMLParser() *YAMLParser { return &YAMLParser{} }

// Extensions returns the file extensions handled by this parser.
func (p *YAMLParser) Extensions() []string {
	return []string{".yaml", ".yml"}
}

// Parse extracts code entities from a single YAML file and merges them
// into the graph.
//
// It handles multi-document YAML (--- separated) and is schema-aware for:
//   - Kubernetes manifests (kind + metadata.name)
//   - Docker Compose (services)
//   - GitHub Actions (jobs, on triggers)
//   - Ansible playbooks (top-level tasks with name field)
//
// General extraction:
//   - Top-level keys → NodeVariable with kind=key
//   - YAML anchors (&name) → NodeVariable with kind=anchor
//   - String values matching *.yaml or *.yml → EdgeImports
func (p *YAMLParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	// Always emit a file node, even on parse failure.
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

	// Decode into a raw yaml.Node to get full AST including line numbers and anchors.
	// Note: Helm chart templates with unquoted {{ .Values.xxx }} will fail yaml.Unmarshal
	// below and fall through to the graceful nil return — no special guard needed.
	// (Quoted {{ item }} in e.g. Ansible YAML is valid YAML and parses correctly.)
	var docs yaml.Node
	if err := yaml.Unmarshal(src, &docs); err != nil {
		// Malformed YAML — return nil (file node already emitted).
		return nil
	}

	if docs.Kind == 0 {
		// Empty document — nothing to extract.
		return nil
	}

	// docs.Kind == yaml.DocumentNode: top-level wrapper.
	// For multi-document YAML, yaml.Unmarshal into a yaml.Node puts all documents
	// under a single DocumentNode whose Content[0] is a SequenceNode of documents
	// only if there are multiple. More precisely: if unmarshalling into *yaml.Node,
	// it gives one DocumentNode. For multiple docs, we need to decode manually.
	// Use a Decoder to iterate documents.
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	for {
		var docNode yaml.Node
		if err := dec.Decode(&docNode); err != nil {
			break // EOF or error — stop
		}
		if docNode.Kind == 0 {
			continue
		}
		// docNode.Kind should be yaml.DocumentNode.
		var root *yaml.Node
		if docNode.Kind == yaml.DocumentNode && len(docNode.Content) > 0 {
			root = docNode.Content[0]
		} else {
			root = &docNode
		}
		if root == nil {
			continue
		}
		p.processDocument(g, filePath, fileNodeID, root)
	}

	return nil
}

// processDocument extracts entities from a single YAML document root node.
func (p *YAMLParser) processDocument(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	root *yaml.Node,
) {
	if root == nil {
		return
	}

	switch root.Kind {
	case yaml.MappingNode:
		p.processMappingDocument(g, filePath, fileNodeID, root)
	case yaml.SequenceNode:
		// Top-level sequence — Ansible playbooks are like this.
		p.processSequenceDocument(g, filePath, fileNodeID, root)
	}
}

// processMappingDocument handles YAML documents whose root is a mapping node.
func (p *YAMLParser) processMappingDocument(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	root *yaml.Node,
) {
	if root == nil || root.Kind != yaml.MappingNode {
		return
	}

	// Collect top-level key-value pairs.
	topKeys := p.extractMappingKV(root)

	// Walk all top-level keys — emit NodeVariable for each.
	for _, kv := range topKeys {
		key := kv.key
		valNode := kv.val
		line := kv.line
		if key == "" {
			continue
		}

		nodeID := g.MakeNodeID(filePath, key)
		if g.GetNode(nodeID) == nil {
			g.AddNode(&graph.Node{
				ID:   nodeID,
				Type: graph.NodeVariable,
				Name: key,
				File: filePath,
				Line: line,
				Metadata: map[string]string{
					"kind": "key",
				},
			})
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		// Scan anchors and file references within values.
		if valNode != nil {
			p.walkForAnchors(g, filePath, fileNodeID, valNode)
			p.walkForFileRefs(g, filePath, fileNodeID, valNode)
		}
	}

	// Schema-aware extraction.
	kvMap := make(map[string]*yaml.Node, len(topKeys))
	for _, kv := range topKeys {
		kvMap[kv.key] = kv.val
	}

	// Kubernetes: has "kind" key.
	if _, hasKind := kvMap["kind"]; hasKind {
		p.extractK8sResource(g, filePath, fileNodeID, kvMap)
		return
	}

	// Docker Compose: has "services" key.
	if servicesNode, ok := kvMap["services"]; ok {
		p.extractDockerComposeServices(g, filePath, fileNodeID, servicesNode)
	}

	// GitHub Actions: has "jobs" key.
	if jobsNode, ok := kvMap["jobs"]; ok {
		p.extractGHAJobs(g, filePath, fileNodeID, jobsNode)
	}
	// GitHub Actions: "on" key for triggers.
	if onNode, ok := kvMap["on"]; ok {
		p.extractGHATriggers(g, filePath, fileNodeID, onNode)
	}

	// OpenAPI / Swagger: has "openapi" or "swagger" key AND "paths" key.
	if _, hasOpenAPI := kvMap["openapi"]; hasOpenAPI {
		if pathsNode, ok := kvMap["paths"]; ok {
			p.extractOpenAPIPaths(g, filePath, fileNodeID, pathsNode)
		}
	}
	if _, hasSwagger := kvMap["swagger"]; hasSwagger {
		if pathsNode, ok := kvMap["paths"]; ok {
			p.extractOpenAPIPaths(g, filePath, fileNodeID, pathsNode)
		}
	}
}

// processSequenceDocument handles YAML documents whose root is a sequence node.
// This covers Ansible playbooks where the file is a list of plays/tasks.
func (p *YAMLParser) processSequenceDocument(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	root *yaml.Node,
) {
	if root == nil || root.Kind != yaml.SequenceNode {
		return
	}

	// Check if this looks like an Ansible task list: items that have a "name" field.
	for _, item := range root.Content {
		if item == nil || item.Kind != yaml.MappingNode {
			continue
		}
		kvs := p.extractMappingKV(item)
		kvMap := make(map[string]*yaml.Node)
		for _, kv := range kvs {
			kvMap[kv.key] = kv.val
		}
		if nameNode, ok := kvMap["name"]; ok && nameNode != nil && nameNode.Kind == yaml.ScalarNode {
			taskName := nameNode.Value
			if taskName == "" {
				continue
			}
			nodeID := g.MakeNodeID(filePath, "task:"+taskName)
			if g.GetNode(nodeID) == nil {
				g.AddNode(&graph.Node{
					ID:   nodeID,
					Type: graph.NodeFunction,
					Name: taskName,
					File: filePath,
					Line: nameNode.Line,
					Metadata: map[string]string{
						"kind": "task",
					},
				})
			}
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}

		// Scan for anchors and file refs inside sequence items.
		p.walkForAnchors(g, filePath, fileNodeID, item)
		p.walkForFileRefs(g, filePath, fileNodeID, item)
	}
}

// extractK8sResource extracts a Kubernetes resource node from the top-level mapping.
// Looks for metadata.name and metadata.namespace.
func (p *YAMLParser) extractK8sResource(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	kvMap map[string]*yaml.Node,
) {
	kindNode := kvMap["kind"]
	if kindNode == nil || kindNode.Kind != yaml.ScalarNode {
		return
	}
	resourceKind := kindNode.Value

	// Extract metadata.name and metadata.namespace.
	var resourceName, namespace string
	var nameLine int
	if metaNode, ok := kvMap["metadata"]; ok && metaNode != nil && metaNode.Kind == yaml.MappingNode {
		metaKVs := p.extractMappingKV(metaNode)
		for _, kv := range metaKVs {
			switch kv.key {
			case "name":
				if kv.val != nil && kv.val.Kind == yaml.ScalarNode {
					resourceName = kv.val.Value
					nameLine = kv.val.Line
				}
			case "namespace":
				if kv.val != nil && kv.val.Kind == yaml.ScalarNode {
					namespace = kv.val.Value
				}
			}
		}
	}

	if resourceName == "" {
		// No metadata.name — use the kind as a fallback name.
		resourceName = resourceKind
		if kindNode != nil {
			nameLine = kindNode.Line
		}
	}

	nodeName := resourceKind + "/" + resourceName
	meta := map[string]string{
		"kind":          "k8s_resource",
		"k8s_kind":      resourceKind,
		"resource_name": resourceName,
	}
	if namespace != "" {
		meta["namespace"] = namespace
	}

	nodeID := g.MakeNodeID(filePath, nodeName)
	if g.GetNode(nodeID) == nil {
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     nodeName,
			File:     filePath,
			Line:     nameLine,
			Exported: true,
			Metadata: meta,
		})
	}
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// extractDockerComposeServices extracts each service under a "services:" key.
func (p *YAMLParser) extractDockerComposeServices(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	servicesNode *yaml.Node,
) {
	if servicesNode == nil || servicesNode.Kind != yaml.MappingNode {
		return
	}

	serviceKVs := p.extractMappingKV(servicesNode)
	for _, kv := range serviceKVs {
		serviceName := kv.key
		if serviceName == "" {
			continue
		}
		line := kv.line

		nodeID := g.MakeNodeID(filePath, "service:"+serviceName)
		if g.GetNode(nodeID) == nil {
			g.AddNode(&graph.Node{
				ID:   nodeID,
				Type: graph.NodeStruct,
				Name: serviceName,
				File: filePath,
				Line: line,
				Metadata: map[string]string{
					"kind": "service",
				},
			})
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		// Scan service body for anchors and file refs.
		if kv.val != nil {
			p.walkForAnchors(g, filePath, fileNodeID, kv.val)
			p.walkForFileRefs(g, filePath, fileNodeID, kv.val)
		}
	}
}

// extractGHAJobs extracts job names from a GitHub Actions workflow's "jobs:" key.
func (p *YAMLParser) extractGHAJobs(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	jobsNode *yaml.Node,
) {
	if jobsNode == nil || jobsNode.Kind != yaml.MappingNode {
		return
	}

	jobKVs := p.extractMappingKV(jobsNode)
	for _, kv := range jobKVs {
		jobName := kv.key
		if jobName == "" {
			continue
		}
		line := kv.line

		nodeID := g.MakeNodeID(filePath, "job:"+jobName)
		if g.GetNode(nodeID) == nil {
			g.AddNode(&graph.Node{
				ID:   nodeID,
				Type: graph.NodeFunction,
				Name: jobName,
				File: filePath,
				Line: line,
				Metadata: map[string]string{
					"kind": "job",
				},
			})
		}
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// extractGHATriggers extracts trigger events from a GitHub Actions workflow's "on:" key.
func (p *YAMLParser) extractGHATriggers(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	onNode *yaml.Node,
) {
	if onNode == nil {
		return
	}

	switch onNode.Kind {
	case yaml.ScalarNode:
		// on: push
		eventName := onNode.Value
		if eventName != "" {
			nodeID := g.MakeNodeID(filePath, "trigger:"+eventName)
			if g.GetNode(nodeID) == nil {
				g.AddNode(&graph.Node{
					ID:   nodeID,
					Type: graph.NodeVariable,
					Name: eventName,
					File: filePath,
					Line: onNode.Line,
					Metadata: map[string]string{
						"kind": "trigger",
					},
				})
			}
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}

	case yaml.SequenceNode:
		// on: [push, pull_request]
		for _, item := range onNode.Content {
			if item == nil || item.Kind != yaml.ScalarNode {
				continue
			}
			eventName := item.Value
			if eventName == "" {
				continue
			}
			nodeID := g.MakeNodeID(filePath, "trigger:"+eventName)
			if g.GetNode(nodeID) == nil {
				g.AddNode(&graph.Node{
					ID:   nodeID,
					Type: graph.NodeVariable,
					Name: eventName,
					File: filePath,
					Line: item.Line,
					Metadata: map[string]string{
						"kind": "trigger",
					},
				})
			}
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}

	case yaml.MappingNode:
		// on:
		//   push:
		//     branches: [main]
		//   pull_request: ...
		triggerKVs := p.extractMappingKV(onNode)
		for _, kv := range triggerKVs {
			eventName := kv.key
			if eventName == "" {
				continue
			}
			nodeID := g.MakeNodeID(filePath, "trigger:"+eventName)
			if g.GetNode(nodeID) == nil {
				g.AddNode(&graph.Node{
					ID:   nodeID,
					Type: graph.NodeVariable,
					Name: eventName,
					File: filePath,
					Line: kv.line,
					Metadata: map[string]string{
						"kind": "trigger",
					},
				})
			}
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}
}

// extractOpenAPIPaths extracts API endpoints from an OpenAPI/Swagger spec's "paths:" section.
// Each HTTP method under each path is emitted as a NodeFunction with kind=openapi_endpoint.
// Uses operationId as the node name if present, otherwise "METHOD /path".
func (p *YAMLParser) extractOpenAPIPaths(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	pathsNode *yaml.Node,
) {
	if pathsNode == nil || pathsNode.Kind != yaml.MappingNode {
		return
	}
	// HTTP methods recognised in OpenAPI.
	httpMethods := map[string]bool{
		"get": true, "post": true, "put": true, "delete": true,
		"patch": true, "head": true, "options": true, "trace": true,
	}
	pathKVs := p.extractMappingKV(pathsNode)
	for _, pathKV := range pathKVs {
		pathStr := pathKV.key // e.g. "/users/{id}"
		pathItem := pathKV.val
		if pathItem == nil || pathItem.Kind != yaml.MappingNode {
			continue
		}
		methodKVs := p.extractMappingKV(pathItem)
		for _, methodKV := range methodKVs {
			method := methodKV.key
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			opNode := methodKV.val
			line := methodKV.line

			// Try to get operationId.
			nodeName := strings.ToUpper(method) + " " + pathStr
			if opNode != nil && opNode.Kind == yaml.MappingNode {
				opKVs := p.extractMappingKV(opNode)
				for _, opKV := range opKVs {
					if opKV.key == "operationId" && opKV.val != nil && opKV.val.Kind == yaml.ScalarNode && opKV.val.Value != "" {
						nodeName = opKV.val.Value
						line = opKV.val.Line
						break
					}
				}
			}

			nodeID := g.MakeNodeID(filePath, "endpoint:"+strings.ToUpper(method)+":"+pathStr)
			if g.GetNode(nodeID) == nil {
				g.AddNode(&graph.Node{
					ID:       nodeID,
					Type:     graph.NodeFunction,
					Name:     nodeName,
					File:     filePath,
					Line:     line,
					Exported: true, // OpenAPI endpoints are public API surface
					Metadata: map[string]string{
						"kind":   "openapi_endpoint",
						"method": strings.ToUpper(method),
						"path":   pathStr,
					},
				})
			}
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}
}

// walkForAnchors recursively walks a yaml.Node looking for anchor definitions
// (node.Anchor != "") and emits NodeVariable with kind=anchor.
func (p *YAMLParser) walkForAnchors(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	node *yaml.Node,
) {
	if node == nil {
		return
	}

	if node.Anchor != "" {
		anchorName := node.Anchor
		nodeID := g.MakeNodeID(filePath, "&"+anchorName)
		if g.GetNode(nodeID) == nil {
			g.AddNode(&graph.Node{
				ID:   nodeID,
				Type: graph.NodeVariable,
				Name: "&" + anchorName,
				File: filePath,
				Line: node.Line,
				Metadata: map[string]string{
					"kind": "anchor",
				},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}

	// Skip alias nodes to avoid infinite recursion (they point back to anchored nodes).
	if node.Kind == yaml.AliasNode {
		return
	}

	for _, child := range node.Content {
		p.walkForAnchors(g, filePath, fileNodeID, child)
	}
}

// walkForFileRefs recursively walks a yaml.Node looking for scalar string values
// that reference other YAML files (*.yaml or *.yml). Emits EdgeImports.
func (p *YAMLParser) walkForFileRefs(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	node *yaml.Node,
) {
	if node == nil {
		return
	}

	if node.Kind == yaml.ScalarNode {
		val := node.Value
		if yamlIsFileRef(val) {
			refNodeID := g.MakeNodeID(val, val)
			if g.GetNode(refNodeID) == nil {
				g.AddNode(&graph.Node{
					ID:   refNodeID,
					Type: graph.NodeFile,
					Name: val,
					File: val,
					Line: 1,
				})
			}
			g.AddEdge(&graph.Edge{From: fileNodeID, To: refNodeID, Type: graph.EdgeImports})
		}
		return
	}

	// Skip alias nodes.
	if node.Kind == yaml.AliasNode {
		return
	}

	for _, child := range node.Content {
		p.walkForFileRefs(g, filePath, fileNodeID, child)
	}
}

// kvPair holds a decoded mapping key-value pair with the key's line number.
type kvPair struct {
	key  string
	val  *yaml.Node
	line int
}

// extractMappingKV extracts all key-value pairs from a mapping node.
// Keys must be scalar nodes; values can be anything.
func (p *YAMLParser) extractMappingKV(node *yaml.Node) []kvPair {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}

	var pairs []kvPair
	// Mapping content is alternating key, value nodes.
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		if keyNode == nil || keyNode.Kind != yaml.ScalarNode {
			continue
		}
		key := keyNode.Value
		if key == "" {
			continue
		}
		pairs = append(pairs, kvPair{
			key:  key,
			val:  valNode,
			line: keyNode.Line,
		})
	}
	return pairs
}

// yamlIsFileRef returns true if the value looks like a YAML file path reference.
// Matches strings ending in .yaml or .yml that don't start with a schema prefix.
func yamlIsFileRef(s string) bool {
	if s == "" {
		return false
	}
	// Must end in .yaml or .yml (case-insensitive).
	lower := strings.ToLower(s)
	if !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
		return false
	}
	// Exclude k8s apiVersion values like "apps/v1" (no YAML suffix, but be safe).
	// Exclude strings with spaces (not file paths).
	if strings.ContainsAny(s, " \t\n") {
		return false
	}
	// Must look like a path: contains / or just a filename.
	// Exclude values that are purely a version string like "v1.yaml".
	// We accept any .yaml/.yml value as a potential file reference.
	return true
}
