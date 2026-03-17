package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: parser_audit_harness <dir>")
		os.Exit(1)
	}
	root := os.Args[1]
	g := graph.New("audit")
	w := parser.NewWalker()
	_, _ = w.WalkDir(g, root)
	nodes := g.AllNodes()
	typeCounts := map[string]int{}
	for _, n := range nodes { typeCounts[string(n.Type)]++ }
	types := make([]string, 0, len(typeCounts))
	for k := range typeCounts { types = append(types, k) }
	sort.Slice(types, func(i, j int) bool { return typeCounts[types[i]] > typeCounts[types[j]] })
	fmt.Printf("=== Node type counts (total: %d) ===\n", len(nodes))
	for _, t := range types { fmt.Printf("  %-20s %d\n", t, typeCounts[t]) }
	emptyNames, zeroLines, withMeta, exportedCount := 0, 0, 0, 0
	for _, n := range nodes {
		if n.Name == "" { emptyNames++ }
		if n.Line == 0 { zeroLines++ }
		if len(n.Metadata) > 0 { withMeta++ }
		if n.Exported { exportedCount++ }
	}
	metaPct := 0
	if len(nodes) > 0 { metaPct = withMeta * 100 / len(nodes) }
	fmt.Printf("\n=== Quality stats ===\n  Total: %d  EmptyNames: %d  ZeroLines: %d  Meta: %d (%d%%)  Exported: %d\n",
		len(nodes), emptyNames, zeroLines, withMeta, metaPct, exportedCount)
	fmt.Printf("\n=== Sample nodes (first 20 non-file) ===\n")
	shown := 0
	for _, n := range nodes {
		if shown >= 20 || string(n.Type) == "file" { if string(n.Type) == "file" { continue }; if shown >= 20 { break } }
		rel := strings.TrimPrefix(n.File, root)
		fmt.Printf("  [%-12s] %-42s  ln=%-5d exp=%-5v file=%s\n  meta=%v\n", n.Type, n.Name, n.Line, n.Exported, rel, n.Metadata)
		shown++
	}
	fmt.Printf("\n=== Source files ===\n")
	fc := map[string]int{}
	for _, n := range nodes { if string(n.Type) == "file" { fc[filepath.Ext(n.File)]++ } }
	el := make([]string, 0); for k := range fc { el = append(el, k) }; sort.Strings(el)
	for _, e := range el { fmt.Printf("  %s: %d\n", e, fc[e]) }
}
