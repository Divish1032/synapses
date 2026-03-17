package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func TestJuliaParserAudit(t *testing.T) {
	src := []byte(`
module MyPackage

using LinearAlgebra: norm, dot
import JSON

struct Point{T <: Number}
    x::T
    y::T
end

mutable struct MyStruct <: AbstractVector{Float64}
    data::Vector{Float64}
end

abstract type Transform end

distance(a::Point, b::Point) = sqrt((a.x - b.x)^2 + (a.y - b.y)^2)

function process(x::T) where {T <: Number}
    return x * 2
end

Base.show(io::IO, s::MyStruct) = print(io, "MyStruct($(length(s.data)))")

const DEFAULT_SIZE = 100

end
`)

	g := graph.New("julia-test")
	p := parser.NewJuliaParser()
	if err := p.Parse(g, "/tmp/test.jl", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	nodeExists := func(name string, nodeType graph.NodeType) bool {
		for _, n := range g.AllNodes() {
			if n.Name == name && n.Type == nodeType {
				return true
			}
		}
		return false
	}

	tests := []struct {
		name     string
		nodeType graph.NodeType
	}{
		{"test.jl", graph.NodeFile},
		{"MyPackage", graph.NodeStruct},
		{"Point", graph.NodeStruct},
		{"MyStruct", graph.NodeStruct},
		{"Transform", graph.NodeInterface},
		{"distance", graph.NodeFunction},
		{"process", graph.NodeFunction},
		{"Base.show", graph.NodeFunction},
		{"DEFAULT_SIZE", graph.NodeVariable},
		{"LinearAlgebra", graph.NodePackage},
		{"JSON", graph.NodePackage},
	}

	for _, tt := range tests {
		if !nodeExists(tt.name, tt.nodeType) {
			t.Errorf("expected node %q (type=%s) not found", tt.name, tt.nodeType)
		}
	}

	// Verify MyStruct has supertype metadata.
	for _, n := range g.AllNodes() {
		if n.Name == "MyStruct" && n.Type == graph.NodeStruct {
			if n.Metadata["supertype"] != "AbstractVector" {
				t.Errorf("MyStruct supertype: expected AbstractVector, got %q", n.Metadata["supertype"])
			}
		}
	}
}
