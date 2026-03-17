package main

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// TestBicepParserBasic verifies basic Bicep parsing functionality.
func TestBicepParserBasic(t *testing.T) {
	g := graph.New("test")
	p := parser.NewBicepParser()

	// Minimal Bicep file with a simple variable declaration
	src := []byte(`
param location string = 'eastus'
param vmName string = 'myvm'

resource vm 'Microsoft.Compute/virtualMachines@2021-03-01' = {
  name: vmName
  location: location
}
`)

	if err := p.Parse(g, "test.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected at least one node after parsing Bicep file")
	}
}

// TestBicepParserEmpty verifies handling of empty Bicep file.
func TestBicepParserEmpty(t *testing.T) {
	g := graph.New("test")
	p := parser.NewBicepParser()

	src := []byte("")

	if err := p.Parse(g, "empty.bicep", src); err != nil {
		t.Fatalf("Parse failed on empty file: %v", err)
	}
	// Empty file should parse without error
}

// TestBicepParserWithVariables verifies parsing variables.
func TestBicepParserWithVariables(t *testing.T) {
	g := graph.New("test")
	p := parser.NewBicepParser()

	src := []byte(`
var prefix = 'dev'
var environment = 'development'
var location = 'westus'

output myPrefix string = prefix
`)

	if err := p.Parse(g, "vars.bicep", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected nodes from variable declarations")
	}
}
