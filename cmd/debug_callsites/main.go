package main

import (
	"fmt"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

func main() {
	g := graph.New("beta-worker")
	w := parser.NewWalker()
	if _, err := w.WalkDir(g, "/home/ubuntu/work/synapses-os/test-workspace/beta-worker"); err != nil {
		panic(err)
	}

	sites := g.PeekCallSites()
	fmt.Printf("Python call sites before resolve: %d\n", len(sites))
	for _, s := range sites {
		fmt.Printf("  caller=%-55s pkgAlias=%q funcName=%q callerFile=%q\n", s.CallerID, s.PkgAlias, s.FuncName, s.CallerFile)
	}

	n := resolver.ResolveCallEdges(g)
	fmt.Printf("\nResolved CALLS edges: %d\n", n)

	fmt.Printf("\nFunctions/Methods/Structs in graph:\n")
	for _, nd := range g.AllNodes() {
		if nd.Type == graph.NodeFunction || nd.Type == graph.NodeMethod || nd.Type == graph.NodeStruct {
			fmt.Printf("  [%s] pkg=%-15q name=%q\n", nd.Type, nd.Package, nd.Name)
		}
	}

	fmt.Printf("\nIMPORTS edges:\n")
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeImports {
			from := g.GetNode(e.From)
			to := g.GetNode(e.To)
			if from != nil && to != nil {
				fmt.Printf("  %s -> %s\n", from.Name, to.Name)
			}
		}
	}
}
