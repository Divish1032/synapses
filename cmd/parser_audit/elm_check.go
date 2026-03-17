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

func elmCheck() {
	g := graph.New("elm-audit")
	p := parser.NewElmParser()
	filepath.Walk("/tmp/parser-audit/elm/src", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".elm" {
			return nil
		}
		src, _ := os.ReadFile(path)
		p.Parse(g, path, src)
		return nil
	})
	sites := g.PeekCallSites()
	freq := make(map[string]int)
	for _, s := range sites {
		freq[s.FuncName]++
	}
	type kv struct {
		k string
		v int
	}
	var sorted []kv
	for k, v := range freq {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })
	fmt.Printf("Total call sites: %d\n", len(sites))
	fmt.Println("Top 30 most frequent callee names:")
	for i, kv := range sorted {
		if i >= 30 {
			break
		}
		fmt.Printf("  %-30s %d\n", kv.k, kv.v)
	}
	nodes := g.AllNodes()
	nodeNames := make(map[string]bool)
	for _, n := range nodes {
		nodeNames[strings.ToLower(n.Name)] = true
	}
	resolved := 0
	for _, s := range sites {
		if nodeNames[strings.ToLower(s.FuncName)] {
			resolved++
		}
	}
	fmt.Printf("\nCall sites resolving to known nodes: %d / %d (%.0f%%)\n",
		resolved, len(sites), float64(resolved)/float64(len(sites))*100)
}
