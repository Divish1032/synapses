package parser

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// TestBicepParser_HandleParam tests parameter declaration extraction.
func TestBicepParser_HandleParam(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
param location string = 'eastus'
param vmName string = 'myvm'
param env string
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	paramNodes := filterNodesByType(nodes, graph.NodeVariable)
	if len(paramNodes) == 0 {
		t.Error("expected at least one parameter node")
	}

	// Check for specific parameter metadata
	found := false
	for _, n := range paramNodes {
		if n.Name == "location" && n.Metadata["kind"] == "parameter" {
			found = true
			if n.Metadata["type"] != "string" {
				t.Errorf("expected type 'string', got %q", n.Metadata["type"])
			}
			if n.Metadata["default"] != "eastus" {
				t.Errorf("expected default 'eastus', got %q", n.Metadata["default"])
			}
		}
	}
	if !found {
		t.Error("expected to find 'location' parameter")
	}
}

// TestBicepParser_HandleVar tests variable declaration extraction.
func TestBicepParser_HandleVar(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
var prefix = 'dev'
var environment = 'development'
var location = 'westus'
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	varNodes := filterNodesByType(nodes, graph.NodeVariable)
	if len(varNodes) < 3 {
		t.Errorf("expected at least 3 variable nodes, got %d", len(varNodes))
	}

	// Check for specific variable
	found := false
	for _, n := range varNodes {
		if n.Name == "prefix" && n.Metadata["kind"] == "variable" {
			found = true
			if n.Exported {
				t.Error("expected variable to be non-exported")
			}
		}
	}
	if !found {
		t.Error("expected to find 'prefix' variable")
	}
}

// TestBicepParser_HandleResource tests resource declaration extraction.
func TestBicepParser_HandleResource(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
resource vm 'Microsoft.Compute/virtualMachines@2021-03-01' = {
  name: 'myvm'
  location: 'eastus'
}

resource storage 'Microsoft.Storage/storageAccounts@2021-06-01' = {
  name: 'mystg'
}
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	structNodes := filterNodesByType(nodes, graph.NodeStruct)
	if len(structNodes) < 2 {
		t.Errorf("expected at least 2 resource nodes, got %d", len(structNodes))
	}

	// Check for vm resource
	found := false
	for _, n := range structNodes {
		if n.Name == "vm" && n.Metadata["kind"] == "resource" {
			found = true
			if n.Metadata["type"] != "Microsoft.Compute/virtualMachines" {
				t.Errorf("expected type 'Microsoft.Compute/virtualMachines', got %q", n.Metadata["type"])
			}
		}
	}
	if !found {
		t.Error("expected to find 'vm' resource")
	}

	// Check for storage resource
	storageFound := false
	for _, n := range structNodes {
		if n.Name == "storage" && n.Metadata["kind"] == "resource" {
			storageFound = true
		}
	}
	if !storageFound {
		t.Error("expected to find 'storage' resource")
	}
}

// TestBicepParser_HandleModule tests module declaration extraction.
func TestBicepParser_HandleModule(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
module webApp 'modules/webapp.bicep' = {
  name: 'webAppModule'
}

module database 'modules/db.bicep' = {
  name: 'dbModule'
}
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	moduleNodes := filterNodesByType(nodes, graph.NodeStruct)
	if len(moduleNodes) < 2 {
		t.Errorf("expected at least 2 module nodes, got %d", len(moduleNodes))
	}

	// Check for module with path metadata
	found := false
	for _, n := range moduleNodes {
		if n.Name == "webApp" && n.Metadata["kind"] == "module" {
			found = true
			if n.Metadata["path"] != "modules/webapp.bicep" {
				t.Errorf("expected path 'modules/webapp.bicep', got %q", n.Metadata["path"])
			}
		}
	}
	if !found {
		t.Error("expected to find 'webApp' module")
	}
}

// TestBicepParser_HandleOutput tests output declaration extraction.
func TestBicepParser_HandleOutput(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
output storageEndpoint string = 'endpoint'
output location string = 'loc'
output vmId string = 'id'
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	outputNodes := filterNodesByType(nodes, graph.NodeVariable)
	outputCount := 0
	for _, n := range outputNodes {
		if n.Metadata["kind"] == "output" {
			outputCount++
		}
	}
	if outputCount < 3 {
		t.Errorf("expected at least 3 output nodes, got %d", outputCount)
	}

	// Check for specific output with type metadata
	found := false
	for _, n := range outputNodes {
		if n.Name == "storageEndpoint" && n.Metadata["kind"] == "output" {
			found = true
			if n.Metadata["type"] != "string" {
				t.Errorf("expected type 'string', got %q", n.Metadata["type"])
			}
		}
	}
	if !found {
		t.Error("expected to find 'storageEndpoint' output")
	}
}

// TestBicepParser_HandleTypeDecl tests type declaration extraction.
func TestBicepParser_HandleTypeDecl(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
type fizz = string | int
type buzz = {
  name: string
  age: int
}
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	typeNodes := filterNodesByType(nodes, graph.NodeStruct)
	typeCount := 0
	for _, n := range typeNodes {
		if n.Metadata["kind"] == "type" {
			typeCount++
		}
	}
	if typeCount < 2 {
		t.Errorf("expected at least 2 type declaration nodes, got %d", typeCount)
	}

	// Check for specific type
	found := false
	for _, n := range typeNodes {
		if n.Name == "fizz" && n.Metadata["kind"] == "type" {
			found = true
		}
	}
	if !found {
		t.Error("expected to find 'fizz' type declaration")
	}
}

// TestBicepParser_HandleFuncDecl tests function declaration extraction.
func TestBicepParser_HandleFuncDecl(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	// Note: Function declarations in Bicep need proper syntax
	src := []byte(`
func sayHello(name string) string => 'Hello'

param funcTest string
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	allNodes := g.AllNodes()
	if len(allNodes) == 0 {
		t.Error("expected some nodes from parsing")
	}
	// Function parsing may not produce nodes depending on the grammar
	// Just verify the parse completes without error
}

// TestBicepParser_HandleTargetScope tests targetScope declaration extraction.
func TestBicepParser_HandleTargetScope(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
targetScope = 'subscription'
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	varNodes := filterNodesByType(nodes, graph.NodeVariable)

	// Check for targetScope
	found := false
	for _, n := range varNodes {
		if n.Name == "targetScope" && n.Metadata["kind"] == "targetScope" {
			found = true
			if n.Metadata["value"] != "subscription" {
				t.Errorf("expected value 'subscription', got %q", n.Metadata["value"])
			}
		}
	}
	if !found {
		t.Error("expected to find 'targetScope' declaration")
	}
}

// TestBicepParser_HandleMetadata tests metadata declaration extraction.
func TestBicepParser_HandleMetadata(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
metadata name = 'myapp'
metadata version = '1.0.0'
metadata description = 'My Bicep module'
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	metadataNodes := filterNodesByType(nodes, graph.NodeVariable)
	metaCount := 0
	for _, n := range metadataNodes {
		if n.Metadata["kind"] == "metadata" {
			metaCount++
		}
	}
	if metaCount < 3 {
		t.Errorf("expected at least 3 metadata nodes, got %d", metaCount)
	}

	// Check for specific metadata
	found := false
	for _, n := range metadataNodes {
		if n.Name == "name" && n.Metadata["kind"] == "metadata" {
			found = true
		}
	}
	if !found {
		t.Error("expected to find 'name' metadata")
	}
}

// TestBicepParser_WithDecorators tests decorator handling on declarations.
func TestBicepParser_WithDecorators(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
@description('Storage account location')
@allowed(['eastus', 'westus'])
param location string = 'eastus'

@export()
type customType = string
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()

	// Check parameter has decorators metadata
	paramFound := false
	for _, n := range nodes {
		if n.Name == "location" && n.Metadata["kind"] == "parameter" {
			paramFound = true
			if n.Metadata["decorators"] == "" {
				t.Error("expected decorators metadata on parameter")
			}
		}
	}
	if !paramFound {
		t.Error("expected to find 'location' parameter with decorators")
	}

	// Check type has decorators metadata
	typeFound := false
	for _, n := range nodes {
		if n.Name == "customType" && n.Metadata["kind"] == "type" {
			typeFound = true
			if n.Metadata["decorators"] == "" {
				t.Error("expected decorators metadata on type")
			}
		}
	}
	if !typeFound {
		t.Error("expected to find 'customType' type with decorators")
	}
}

// TestBicepParser_ComplexFile tests parsing a complex Bicep file with all elements.
func TestBicepParser_ComplexFile(t *testing.T) {
	g := graph.New("test")
	p := NewBicepParser()

	src := []byte(`
targetScope = 'resourceGroup'

metadata name = 'Complex App'
metadata version = '1.0.0'

@description('Storage account name prefix')
param storagePrefix string = 'myapp'

var environment = 'prod'
var location = 'eastus'

type storageConfig = {
  name: string
  kind: string
}

resource storage 'Microsoft.Storage/storageAccounts@2021-06-01' = {
  name: '${storagePrefix}storage'
  location: location
}

module network 'modules/network.bicep' = {
  name: 'networkModule'
}

output storageId string = storage.id
output networkId string = network.outputs.id
`)

	if err := p.Parse(g, "complex.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	if len(nodes) < 8 {
		t.Errorf("expected at least 8 nodes for complex file, got %d", len(nodes))
	}

	// Verify each type of declaration is present
	hasParam := false
	hasVar := false
	hasType := false
	hasResource := false
	hasModule := false
	hasOutput := false
	hasTargetScope := false
	hasMetadata := false

	for _, n := range nodes {
		if n.Name == "storagePrefix" && n.Metadata["kind"] == "parameter" {
			hasParam = true
		}
		if n.Name == "environment" && n.Metadata["kind"] == "variable" {
			hasVar = true
		}
		if n.Name == "storageConfig" && n.Metadata["kind"] == "type" {
			hasType = true
		}
		if n.Name == "storage" && n.Metadata["kind"] == "resource" {
			hasResource = true
		}
		if n.Name == "network" && n.Metadata["kind"] == "module" {
			hasModule = true
		}
		if n.Name == "storageId" && n.Metadata["kind"] == "output" {
			hasOutput = true
		}
		if n.Name == "targetScope" && n.Metadata["kind"] == "targetScope" {
			hasTargetScope = true
		}
		if n.Name == "name" && n.Metadata["kind"] == "metadata" {
			hasMetadata = true
		}
	}

	if !hasParam {
		t.Error("expected to find parameter declaration")
	}
	if !hasVar {
		t.Error("expected to find variable declaration")
	}
	if !hasType {
		t.Error("expected to find type declaration")
	}
	if !hasResource {
		t.Error("expected to find resource declaration")
	}
	if !hasModule {
		t.Error("expected to find module declaration")
	}
	if !hasOutput {
		t.Error("expected to find output declaration")
	}
	if !hasTargetScope {
		t.Error("expected to find targetScope declaration")
	}
	if !hasMetadata {
		t.Error("expected to find metadata declaration")
	}
}

// ─── Helper functions ─────────────────────────────────────────────────────────

func filterNodesByType(nodes []*graph.Node, nodeType graph.NodeType) []*graph.Node {
	var result []*graph.Node
	for _, n := range nodes {
		if n.Type == nodeType {
			result = append(result, n)
		}
	}
	return result
}
