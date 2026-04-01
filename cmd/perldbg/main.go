package main

import (
	"fmt"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

func main() {
	src := []byte(`package File::Copy;
sub copy;
sub move;
sub copy {
    my ($from) = @_;
    return _copy($from);
}
sub _copy {
    move($from);
    return 1;
}
sub move { return 1; }
`)
	g := graph.New("test-perl")
	g.SetRoot("/tmp/testperllib")
	p := parser.NewPerlParser()
	filePath := "/tmp/testperllib/lib/File/Copy.pm"
	p.Parse(g, filePath, src)

	fmt.Println("=== NODES ===")
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeFunction {
			fmt.Printf("  FUNC: Name=%q Package=%q\n", n.Name, n.Package)
		}
	}

	sites := g.PeekCallSites()
	fmt.Printf("\n=== CALL SITES (%d) ===\n", len(sites))
	for _, s := range sites {
		callerNode := g.GetNode(s.CallerID)
		callerName := "<file>"
		if callerNode != nil { callerName = callerNode.Name }
		fmt.Printf("  %s -> pkg=%q func=%q\n", callerName, s.PkgAlias, s.FuncName)
	}

	n := resolver.ResolveCallEdges(g)
	fmt.Printf("\n=== RESOLVED %d CALL EDGES ===\n", n)
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls {
			from := g.GetNode(e.From)
			to := g.GetNode(e.To)
			fn, tn := "<nil>", "<nil>"
			if from != nil { fn = from.Name }
			if to != nil { tn = to.Name }
			fmt.Printf("  %s -> %s\n", fn, tn)
		}
	}
}
