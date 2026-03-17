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

type auditResult struct {
	lang         string
	dir          string
	filesScanned int
	counts       map[string]int
	emptyNames   int
	zeroLines    int
	noMeta       int
	totalNodes   int
	sampleNodes  []string
	edgeCounts   map[string]int
	callSites    int
}

func auditParser(lang, dir string, p parser.LanguageParser) auditResult {
	g := graph.New("audit-" + lang)

	res := auditResult{
		lang:       lang,
		dir:        dir,
		counts:     make(map[string]int),
		edgeCounts: make(map[string]int),
	}

	exts := make(map[string]bool)
	for _, e := range p.Extensions() {
		exts[strings.ToLower(e)] = true
	}

	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !exts[ext] {
			return nil
		}
		res.filesScanned++
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		p.Parse(g, path, src)
		return nil
	})

	nodes := g.AllNodes()
	res.totalNodes = len(nodes)

	for _, n := range nodes {
		res.counts[string(n.Type)]++
		if n.Name == "" {
			res.emptyNames++
		}
		if n.Line == 0 && n.Type != graph.NodeFile && n.Type != graph.NodePackage {
			res.zeroLines++
		}
		if len(n.Metadata) == 0 && n.Type != graph.NodeFile && n.Type != graph.NodePackage {
			res.noMeta++
		}
	}

	count := 0
	for _, n := range nodes {
		if count >= 30 {
			break
		}
		if n.Type == graph.NodeFile || n.Type == graph.NodePackage {
			continue
		}
		metaStr := ""
		for k, v := range n.Metadata {
			metaStr += k + "=" + v + " "
		}
		res.sampleNodes = append(res.sampleNodes, fmt.Sprintf(
			"  [%-10s] %-45s line=%-4d exported=%-5v meta={%s}",
			n.Type, n.Name, n.Line, n.Exported, strings.TrimSpace(metaStr),
		))
		count++
	}

	for _, e := range g.AllEdges() {
		res.edgeCounts[string(e.Type)]++
	}
	res.callSites = len(g.PeekCallSites())

	return res
}

func printResult(r auditResult) {
	fmt.Printf("\n══════════════════════════════════════════════════════\n")
	fmt.Printf("  LANG: %-10s  DIR: %s\n", r.lang, r.dir)
	fmt.Printf("══════════════════════════════════════════════════════\n")
	fmt.Printf("  Files scanned: %d  |  Total nodes: %d\n", r.filesScanned, r.totalNodes)

	nonFile := r.totalNodes - r.counts["file"] - r.counts["package"]
	fmt.Printf("  Structural nodes (non-file/pkg): %d\n", nonFile)
	fmt.Printf("  Empty names: %d  |  Zero lines: %d  |  No metadata: %d\n",
		r.emptyNames, r.zeroLines, r.noMeta)
	fmt.Println()

	fmt.Println("  Node type breakdown:")
	types := make([]string, 0, len(r.counts))
	for k := range r.counts {
		types = append(types, k)
	}
	sort.Strings(types)
	for _, t := range types {
		fmt.Printf("    %-20s %d\n", t, r.counts[t])
	}
	fmt.Println()

	fmt.Println("  Edge type breakdown:")
	etypes := make([]string, 0, len(r.edgeCounts))
	for k := range r.edgeCounts {
		etypes = append(etypes, k)
	}
	sort.Strings(etypes)
	for _, t := range etypes {
		fmt.Printf("    %-20s %d\n", t, r.edgeCounts[t])
	}
	fmt.Printf("  Pending call sites: %d\n", r.callSites)
	fmt.Println()

	fmt.Println("  Sample structural nodes (first 30):")
	for _, s := range r.sampleNodes {
		fmt.Println(s)
	}
}

func main() {
	type entry struct {
		lang string
		dir  string
		p    parser.LanguageParser
	}

	audits := []entry{
		{"bash",   "/tmp/parser-audit/bash",                  parser.NewBashParser()},
		{"sql",    "/tmp/parser-audit/sql",                   parser.NewSQLParser()},
		{"css",    "/tmp/parser-audit/css",                   parser.NewCSSParser()},
		{"scss",   "/tmp/parser-audit/scss-bootstrap/scss",   parser.NewSCSSParser()},
		{"ocaml",  "/tmp/parser-audit/ocaml",                 parser.NewOCamlParser()},
		{"elm",    "/tmp/parser-audit/elm/src",               parser.NewElmParser()},
		{"hcl",    "/tmp/parser-audit/hcl",                   parser.NewHCLParser()},
		{"svelte", "/tmp/parser-audit/svelte/src",            parser.NewSvelteParser()},
		{"cue",    "/tmp/parser-audit/cue-repo",              parser.NewCUEParser()},
	}

	for _, a := range audits {
		if _, err := os.Stat(a.dir); os.IsNotExist(err) {
			fmt.Printf("\n[SKIP] %s — dir not found: %s\n", a.lang, a.dir)
			continue
		}
		r := auditParser(a.lang, a.dir, a.p)
		printResult(r)
	}

	// Elm call site noise analysis
	fmt.Println("\n═══ ELM CALL SITE NOISE ANALYSIS ═══")
	elmCheck()

	// CSS feature verification
	fmt.Println("\n═══ CSS FEATURE VERIFICATION ═══")
	cssCheck()

	// Svelte 5 verification
	fmt.Println("\n═══ SVELTE 5 RUNES VERIFICATION ═══")
	svelte5Check()

}
