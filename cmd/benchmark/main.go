// Command benchmark is a standalone binary for running Tier 1 external benchmarks
// against a running Synapses daemon. It is purely external — it calls the daemon
// via the REST HTTP transport (/v1/tools/{tool}?project=...) and does not import
// any internal Synapses packages.
//
// Usage:
//
//	# Local modes (no daemon needed):
//	benchmark --benchmark=repobench --retrieval=hybrid-rrf --no-synapses
//
//	# Full synapses-embed mode (all repos cloned + indexed automatically):
//	benchmark --benchmark=repobench --retrieval=synapses-embed \
//	          --repos-dir=/tmp/repobench_repos --cache-file=/tmp/index_cache.json
//
//	# Index only (pre-flight step):
//	benchmark --benchmark=repobench --index-only \
//	          --repos-dir=/tmp/repobench_repos --cache-file=/tmp/index_cache.json
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/benchmarks"
	"github.com/SynapsesOS/synapses/cmd/benchmark/indexer"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

func main() {
	var (
		benchmarkName = flag.String("benchmark", "repobench", "Benchmark to run: contextbench | swe-verified | repobench")
		endpoint      = flag.String("endpoint", "http://127.0.0.1:11435", "Synapses daemon REST endpoint")
		project       = flag.String("project", "", "Single project path (overrides per-repo routing for synapses-embed)")
		outputDir     = flag.String("output-dir", "results", "Directory to write JSON and markdown results")
		retrieval     = flag.String("retrieval", "hybrid-rrf", "Retrieval mode: fts-only | vector-only | hybrid-rrf | hybrid-convex | hybrid-anchor | synapses-search | synapses-embed | synapses-embed-local")
		configs       = flag.String("configs", "python_cff,python_cfr,java_cff,java_cfr", "Comma-separated RepoBench configs")
		difficulty    = flag.String("difficulty", "easy,hard", "Comma-separated difficulties: easy | hard")
		limit         = flag.Int("limit", 0, "Max samples per config/difficulty (0 = all)")
		noSynapses    = flag.Bool("no-synapses", false, "Control run: disable Synapses MCP")
		reposDir      = flag.String("repos-dir", "/tmp/repobench_repos", "Directory where repos are cloned")
		cacheFile     = flag.String("cache-file", "/tmp/repobench_index_cache.json", "JSON cache of indexed repos")
		indexWorkers  = flag.Int("index-workers", 8, "Parallel workers for cloning+indexing")
		indexOnly     = flag.Bool("index-only", false, "Clone and index repos, then exit")
		skipIndex     = flag.Bool("skip-index", false, "Skip synapses index step (clone only)")
	)
	flag.Parse()

	*benchmarkName = strings.ToLower(strings.TrimSpace(*benchmarkName))

	// ── Index-only mode ──────────────────────────────────────────────────────
	if *indexOnly {
		repos := collectRepos(splitComma(*configs), splitComma(*difficulty))
		fmt.Printf("Indexing %d unique repos with %d workers...\n", len(repos), *indexWorkers)
		results, err := indexer.Run(indexer.Options{
			ReposDir:       *reposDir,
			CacheFile:      *cacheFile,
			Repos:          repos,
			Workers:        *indexWorkers,
			SkipIndex:      *skipIndex,
			TimeoutPerRepo: 0, // use default
			Verbose:        false,
		})
		if err != nil {
			log.Fatalf("indexer: %v", err)
		}
		indexer.Summary(results)
		return
	}

	// ── Build MCP client ────────────────────────────────────────────────────
	var mcpClient *agent.SynapsesClient
	if *noSynapses {
		mcpClient = agent.NewDisabledClient()
	} else {
		mcpClient = agent.NewClient(*endpoint, *project)
	}

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	rep := reporter.New(*outputDir)

	switch *benchmarkName {
	case "repobench", "repobench-r":
		cfgList := splitComma(*configs)
		diffList := splitComma(*difficulty)

		// For synapses-embed, load the index cache so the runner can route
		// each sample to its own project path.
		var repoCache *indexer.Cache
		if *retrieval == "synapses-embed" || *retrieval == "synapses-search" {
			cache, err := indexer.LoadCache(*cacheFile)
			if err != nil {
				log.Printf("warning: could not load index cache %s: %v (falling back to --project flag)", *cacheFile, err)
			} else {
				repoCache = cache
			}
		}

		// For synapses-embed-local, initialise the in-process ONNX embedder.
		var localEmb *benchmarks.LocalEmbedder
		if *retrieval == "synapses-embed-local" {
			log.Printf("loading local nomic-embed model...")
			e, err := benchmarks.NewLocalEmbedder(3)
			if err != nil {
				log.Fatalf("could not load local embedder: %v", err)
			}
			defer e.Close()
			localEmb = e
			log.Printf("local embedder ready")
		}

		opts := benchmarks.RepoBenchOptions{
			Configs:       cfgList,
			Difficulties:  diffList,
			RetrievalMode: *retrieval,
			LimitPerSet:   *limit,
			ReposDir:      *reposDir,
			RepoCache:     repoCache,
			LocalEmbedder: localEmb,
		}
		result, err := benchmarks.RunRepoBench(mcpClient, opts)
		if err != nil {
			log.Fatalf("repobench failed: %v", err)
		}
		if err := rep.WriteRepoBench(result); err != nil {
			log.Fatalf("write results: %v", err)
		}
		rep.PrintRepoBenchSummary(result)

	case "contextbench":
		log.Fatal("contextbench runner not yet implemented (Phase 4)")

	case "swe-verified", "swe_verified":
		log.Fatal("swe-verified runner not yet implemented (Phase 3)")

	default:
		log.Fatalf("unknown benchmark %q", *benchmarkName)
	}
}

// collectRepos reads all unique repo names from the JSONL files.
func collectRepos(configs, difficulties []string) []string {
	seen := make(map[string]bool)
	var repos []string
	for _, cfg := range configs {
		for _, diff := range difficulties {
			path := fmt.Sprintf("repobench_%s_%s.jsonl", cfg, diff)
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				// Fast JSON field extraction without full parse.
				if idx := strings.Index(line, `"repo_name"`); idx >= 0 {
					rest := line[idx+len(`"repo_name"`):]
					if colon := strings.Index(rest, `"`); colon >= 0 {
						rest = rest[colon+1:]
						if end := strings.Index(rest, `"`); end >= 0 {
							repo := rest[:end]
							if !seen[repo] {
								seen[repo] = true
								repos = append(repos, repo)
							}
						}
					}
				}
			}
		}
	}
	return repos
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
