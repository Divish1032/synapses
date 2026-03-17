package main

import (
	"fmt"
	"os"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func main() {
	g := graph.New("audit")
	p := parser.NewBicepParser()
	src, _ := os.ReadFile("/tmp/parser-audit/bicep-synthetic/comprehensive.bicep")
	p.Parse(g, "/tmp/parser-audit/bicep-synthetic/comprehensive.bicep", src)

	fmt.Println("=== ALL edges ===")
	for _, e := range g.AllEdges() {
		fmt.Printf("  [%s] %s -> %s\n", e.Type, e.From, e.To)
	}
}
