# SynapsesOS Benchmark Strategy

**Goal:** Establish where SynapsesOS stands relative to competitors using globally recognized benchmarks. Prove the value of a graph+memory+retrieval context engine with numbers the industry respects.

**Date authored:** 2026-03-26

---

## Context: Why We Benchmark Now

All prior sprints (11–17) have shipped the intelligence stack:
- Sprint 12: HNSW vector search, nomic-embed embeddings
- Sprint 13: Personalized PageRank (PPR) context carving
- Sprint 14: RTA call graph refinement, type propagation, parallel parsing, OpenAPI/GraphQL parsers
- Sprint 15: First-session highlights, outcome signals, architectural rule engine
- Sprint 16: Learned edge weights, name matching, manual edges
- Sprint 17: HyDE retrieval, query expansion
- Sprint 18 #1-5: Session health, knowledge export, binary/model integrity, token savings

Benchmarking earlier would have measured an incomplete system. Benchmarking now measures the full intelligence stack.

---

## Competitive Landscape

### Direct Competitors

| Company | Approach | Benchmark Claims |
|---|---|---|
| **Augment Code** | Semantic code graph + embeddings + commit history. Now an MCP server. 400K+ file indexing. | Topped SWE-bench Pro. +71% quality (Cursor+Opus 4.5), +80% (Claude Code+Opus 4.5) on 300 real Elasticsearch PRs. |
| **Sourcegraph (Cody)** | SCIP protocol (precise indexing), search, embeddings, code graph. | Essential Recall, Concision, Helpfulness metrics. No public leaderboard. |
| **Cursor** | Local RAG + self-summarization context compression. Composer 2 RL-trained. | CursorBench (private). SWE-bench Multilingual: 73.7% (Composer 2). Terminal-Bench 2.0: 61.7. |
| **Greptile** | Codebase indexing for AI code review. | 82% bug catch rate (vs Cursor 41%) on 50 real bugs. |
| **Aider** | Repo-map (tree-sitter AST tags), git-aware, open-source. | Fully transparent Polyglot leaderboard (225 Exercism exercises, 6 languages). |
| **Continue.dev** | Open-source IDE extension, configurable embeddings and retrieval. | No benchmark claims. |

### The Scaffolding Gap Finding (Key Insight)

SWE-bench Pro (Scale AI, 2025) studied the same Claude Opus 4.5 model across different agent scaffolds and found a 5+ percentage point spread (50.2% to 55.4%) attributable *purely to how context is managed*. This is the strongest published evidence that context engine quality directly translates to task success rate.

### Benchmark Gap Map

| Capability | Benchmark Exists? | Notes |
|---|---|---|
| Code task completion (end-to-end) | Yes — SWE-bench Pro, SWE-bench Verified | The standard, but measures agent+model+context combined |
| Code retrieval precision/recall | Partial — RepoBench-R, CodeRAG-Bench | Retrieval only, no graph-based systems tested |
| Context retrieval quality (Context F1) | Yes — ContextBench (Feb 2026) | Best fit for Synapses — measures precision+recall of retrieved context vs gold |
| Impact analysis accuracy | **No** | Gap — nothing benchmarks blast-radius analysis |
| Dependency graph completeness | **No** | Gap — empirical studies show ~27% error in GitHub dep graph |
| Code-specific memory retention | **No** | LongMemEval exists but not code-specific |
| Cross-domain linking accuracy | **No** | Gap — no benchmark for code→infra→config→docs traversal |
| Token efficiency / context compression | Partial — Factory.ai published one eval | Not standardized |

**Strategic insight:** The gaps in impact analysis, dependency graph completeness, and code memory retention are where Synapses has unique strengths and no competitors have published numbers. These are covered in Tier 2 (docs/BENCHMARK_TIER2.md, TBD).

---

## Synapses Capabilities Being Evaluated

### What Makes Synapses Unique vs Competitors

1. **Graph + Memory + Retrieval unified** — memories are anchored to graph nodes with automatic staleness invalidation when the underlying code changes. No competitor does this.
2. **Intent-based context carving** — the same codebase produces structurally different subgraphs depending on whether you're modifying, debugging, reviewing, or planning. Edge weights and traversal direction change per intent.
3. **Cross-domain graph** — a single graph spans code (45+ languages), infrastructure (Terraform/Bicep/HCL), API specs (OpenAPI/GraphQL), config (YAML/TOML/JSON), and docs (Markdown) with typed cross-domain edges (DEPLOYS, CONSUMES, CONFIGURED_BY, DOCUMENTS, EXPLAINS).
4. **Multi-channel fusion** — RRF (Reciprocal Rank Fusion) and ConvexMerge blend FTS5 (BM25), HNSW vector search, graph-anchor retrieval, and recency signals into a single ranked result.
5. **Self-improving feedback loop** — outcome signals from agent sessions train learned edge weights and quality scores that rerank future context deliveries. Failure episodes surface as safety warnings.
6. **ACT-R memory decay** — cognitive-science-based (Anderson & Lebiere, 1998) power-law decay with frequency-boosted half-life. Not simple exponential decay.
7. **45+ language parsers** in a unified graph representation — broader than any competitor's published parser coverage.
8. **HyDE (Hypothetical Document Embeddings)** — generates a hypothetical code definition, embeds it, searches HNSW. Falls back to raw query embedding.
9. **PPR (Personalized PageRank)** — multi-path importance scoring as an alternative to BFS, togglable per request.

### Existing Internal Benchmark Harness

Synapses already has a self-validating benchmark tool (`benchmark` MCP tool, `internal/benchmark/`). This is **not** an external benchmark — it validates internal correctness using the graph's own topology as ground truth. It runs 6 scenarios:

1. `context-completeness` — CarveEgoGraph F1 vs direct callers (threshold: 0.6)
2. `search-accuracy` — FTS top-5 for exact function name (threshold: 0.8)
3. `impact-coverage` — ImpactAnalysis vs direct callers (threshold: 0.5)
4. `graph-reachability` — BFS reachability within 3 hops (threshold: 0.9)
5. `fts-ranking` — FTS rank-1 for exact name match (threshold: 0.7)
6. `memory-recall` — FTS recall of inserted test memories (threshold: 0.7)

This is a health check, not a competitive benchmark. The Tier 1 plan below uses separate external evaluation infrastructure.

---

## Tier 1: External Benchmark Plan

### Overview

Three globally recognized benchmarks, ordered by strategic priority:

| Priority | Benchmark | What It Measures | Leaderboard? | Effort |
|---|---|---|---|---|
| 1 | **ContextBench** | Context F1 (precision + recall vs gold context annotations) | Yes — contextbench.github.io | High |
| 2 | **RepoBench-R** | Acc@k for code retrieval in isolation | No | Low |

---

### Benchmark 1: ContextBench

**Paper:** arxiv.org/abs/2602.05892 (Feb 2026)
**Leaderboard:** contextbench.github.io
**GitHub:** github.com/EuniAI/ContextBench
**Dataset:** huggingface.co/datasets/Contextbench/ContextBench

#### What It Is

1,136 software engineering tasks drawn from SWE-bench Verified, Multi-SWE-bench, SWE-PolyBench PB500, and SWE-bench Pro. Covers 66 repos across 8 languages (Python, Go, TypeScript, Java, Rust, C++, Ruby, PHP). Each task has **human-annotated gold contexts** — 522,115 lines, 23,116 classes/functions, 4,548 files.

**Why this is the best fit for Synapses:** ContextBench is the only publicly recognized benchmark that measures *context retrieval quality* independently of model reasoning. The benchmark found that most systems show high recall but poor precision — agents explore too broadly. Synapses' intent-aware BFS carving should deliver superior precision.

#### Metrics

- **Pass@1** — did the agent solve the task?
- **Context F1** — `2 * (Context Precision * Context Recall) / (Context Precision + Context Recall)` against gold annotations. This is the unique metric no competitor benchmark offers.
- **Context Precision** — fraction of retrieved context lines that appeared in gold
- **Context Recall** — fraction of gold context lines that were retrieved
- **Avg. Cost (USD)** — token efficiency measurement

#### Key Finding from the Paper

> "Sophisticated scaffolding does not consistently improve context retrieval. LLMs prioritize recall over precision. There is a large gap between context explored and context actually used."

Synapses counters this by constraining exploration via token budgets, intent-based edge weights, and quality-score reranking.

#### Setup Plan

**Step 1 — Agent scaffold.** Use `mini-swe-agent` (github.com/SWE-agent/mini-swe-agent) as the base agent. It is ~100 lines, uses bash + file editing tools, achieves >74% on SWE-bench Verified with standardized scaffolding. Minimal surface area means Synapses is the only context variable.

**Step 2 — Wire Synapses MCP.** Add Synapses as an additional MCP tool in the agent loop. The agent may call:
- `prepare_context(entity, intent)` — primary context delivery
- `search(query)` — code search
- `get_impact(entity)` — blast-radius context
- `recall(query)` — memory retrieval

Synapses daemon must be running and have the repo indexed before tasks for that repo are run.

**Step 3 — Pre-index all repos.** For each of the 66 ContextBench repos:
```bash
synapses index --repo <path> --project <name>
```
Index must complete before benchmark tasks for that repo run. Indexing is a one-time cost per repo snapshot.

**Step 4 — Run the benchmark.**
```bash
# Load dataset from HuggingFace
python contextbench_runner.py \
  --agent mini-swe-agent \
  --mcp-endpoint http://localhost:8844 \
  --output results/contextbench_synapses.json
```

**Step 5 — Compute Context F1.** Compare retrieved context (files + line ranges the agent accessed via Synapses) against gold annotations. The ContextBench evaluation script handles this.

**Step 6 — Submit to leaderboard.** Follow submission instructions at contextbench.github.io.

#### Control vs Treatment

Run each task twice on a representative subset (100-200 tasks):
- **Control:** mini-swe-agent with no context engine (bash grep/find only)
- **Treatment:** mini-swe-agent + Synapses MCP

Report both Context F1 and Pass@1 for both conditions. The delta is the headline number.

#### Expected Outcome

Most leaderboard entries show Context Precision < 0.4. Synapses' token-budget-bounded carving and intent-based traversal should achieve Precision > 0.5. F1 improvement over bare agents is the publishable claim.

---

### Benchmark 2: RepoBench-R

**Paper:** arxiv.org/abs/2306.03091 (ICLR 2024)
**Dataset:** huggingface.co/datasets/tianyang/repobench-r

#### What It Is

Given a code completion point and a list of candidate snippets from other files in the same repo, rank the most relevant snippet highest. Pure retrieval task — no agent, no code generation.

- **Languages:** Python, Java
- **Scenarios:** XF-F (cross-file first — nearest relevant cross-file snippet), XF-R (cross-file random)
- **Difficulty:** Easy (5-9 candidates), Hard (10+ candidates)
- **Scale:** 12,000 samples per scenario/difficulty combination

**Why this matters:** Isolates retrieval quality from model reasoning noise. Lets you directly compare Synapses' hybrid retrieval (FTS + HNSW + graph-anchor + RRF) against BM25 and dense-only baselines. Also enables internal ablation: which retrieval channel contributes most.

#### Metrics

- **Accuracy@1** — gold snippet is rank 1
- **Accuracy@3** — gold snippet is in top 3
- **Accuracy@5** — gold snippet is in top 5
- **Accuracy@10** — gold snippet is in top 10

#### Dataset Loading

```python
from datasets import load_dataset

configs = [
    "python_cff",  # Python, cross-file-first
    "python_cfr",  # Python, cross-file-random
    "java_cff",    # Java, cross-file-first
    "java_cfr",    # Java, cross-file-random
]

for config in configs:
    dataset = load_dataset("tianyang/repobench-r", config, split="test_easy")
    dataset_hard = load_dataset("tianyang/repobench-r", config, split="test_hard")
```

Each record contains:
- `context` — code up to the completion point (the query)
- `import_statement` — imports at file top
- `gold_snippet_index` — index of the correct answer in `candidate_code`
- `candidate_code` — list of snippet strings to rank

#### Setup Plan

**Step 1 — Retrieve candidates using Synapses.**

For each record, format the query as `context[-500:]` (last 500 chars of context — the code just before the completion point). Run Synapses' `search` tool over the candidate pool. This requires the candidates to be indexed.

Since candidates are raw snippets (not from a live repo), two approaches:
- **Approach A (accurate):** Use the actual repo snapshots the candidates came from, index them, then query.
- **Approach B (fast):** Embed the query and all candidates using Synapses' built-in embedder, rank by cosine similarity. This tests the embedding quality directly.

Recommend Approach B for RepoBench-R since the benchmark's structure (pre-extracted candidates) maps naturally to embedding-based ranking.

**Step 2 — Run all configurations.**
```bash
python repobench_runner.py \
  --configs python_cff python_cfr java_cff java_cfr \
  --difficulty easy hard \
  --retrieval-mode hybrid  # or: fts-only, vector-only, graph-anchor
  --mcp-endpoint http://localhost:8844 \
  --output results/repobench_synapses.json
```

**Step 3 — Compute Acc@k.**
```python
def accuracy_at_k(results, k):
    correct = sum(1 for r in results if r['gold_rank'] <= k)
    return correct / len(results)
```

#### Ablation Matrix (Internal Use)

Run all 4 retrieval modes across all 4 configs × 2 difficulty levels:

| Mode | Description |
|---|---|
| `fts-only` | FTS5 BM25 only (`SemanticSearch`) |
| `vector-only` | HNSW cosine similarity only |
| `hybrid-rrf` | RRF merge of FTS + vector |
| `hybrid-convex` | ConvexMerge of FTS + vector + recency |
| `hybrid-anchor` | RRF + graph-anchor channel |

This ablation answers: "Which channel contributes most to retrieval accuracy?" Results are internal engineering data but also support the Tier 3 ablation study in the ROADMAP.

#### Actual Results (2026-03-26) — 100 repos, 8,349 samples

Full report: [docs/benchmarks/REPOBENCH_R.md](benchmarks/REPOBENCH_R.md)

| Mode | Acc@1 | Acc@3 | Acc@5 | Acc@10 |
|------|------:|------:|------:|-------:|
| `fts-only` (BM25) | 13.7% | 38.3% | 60.1% | 85.7% |
| `vector-only` | 15.1% | 38.9% | 60.7% | 85.8% |
| `hybrid-rrf` | 14.3% | 39.0% | 60.7% | 86.2% |
| `hybrid-convex` ⭐ | **14.7%** | 38.9% | **60.9%** | **86.4%** |
| `hybrid-anchor` | 11.9% | 33.8% | 55.7% | 83.0% |
| `synapses-embed` (Ollama) | 14.3% | 39.0% | 60.7% | 86.2% |
| `synapses-search` | 14.2% | 38.9% | 60.7% | 86.2% |

**vs BM25 paper baseline (~60% Acc@5):** `hybrid-convex` beats it by +0.9pp.
**vs ada-002 (~65% Acc@5):** -4.1pp gap — concentrated in hard-difficulty retrieval.
**Original target (>70% Acc@5):** Not reached. Hard-difficulty is the bottleneck (32–49% Acc@5 vs 74–81% for easy).

Key finding: Neural embeddings (nomic-embed via Ollama) provide zero measurable lift over TF-IDF cosine at scale. The gap to ada-002 requires better query construction, not a larger embedding model.

---

## New Code Benchmark to Be Aware Of: SWE Context Bench

**Paper:** arxiv.org/abs/2602.08316 (Feb 2026)

Built on SWE-bench Lite (300 tasks) augmented with 99 related tasks sharing context via real dependency/reference relationships. Explicitly measures **experience reuse** — whether an agent can carry context from a resolved issue to a related downstream issue. Directly relevant to Synapses' memory system (episodic memory, anchored memories, pattern recognition). Not yet implemented in the Tier 1 plan but worth tracking for Tier 2.

---

## Implementation Plan

### What to Build

The existing `internal/benchmark/` harness is **not** the right foundation — it is a self-validating health check. Tier 1 requires a separate standalone evaluation binary.

**New package: `cmd/benchmark/`**

```
cmd/benchmark/
├── main.go                    # CLI entry: --benchmark=contextbench|swe-verified|repobench
├── agent/
│   ├── mini_swe_agent.go      # mini-swe-agent loop with Synapses MCP client
│   └── mcp_client.go          # HTTP client for Synapses MCP tools
├── benchmarks/
│   ├── contextbench.go        # Dataset loader, task runner, Context F1 calculator
│   ├── swe_verified.go        # Dataset loader, patch formatter, JSONL output
│   └── repobench.go           # HuggingFace dataset loader, Acc@k scorer
├── indexer/
│   └── repo_indexer.go        # Pre-index repos via Synapses API before benchmark runs
└── reporter/
    └── reporter.go            # JSON + markdown output, leaderboard submission format
```

**Key design principles:**
- The benchmark binary calls Synapses via existing MCP HTTP transport — **no changes to Synapses daemon required**
- Agent loop is injectable: same runner works with or without Synapses MCP (control vs treatment)
- Indexing is a separate pre-flight step, cached to disk to avoid re-indexing same repo snapshot twice
- All results stored as JSON for post-processing and submission

### MCP Client (Core Integration Point)

```go
// agent/mcp_client.go
type SynapsesClient struct {
    endpoint string
    project  string
}

func (c *SynapsesClient) PrepareContext(entity, intent string) (*ContextResult, error)
func (c *SynapsesClient) Search(query string) (*SearchResult, error)
func (c *SynapsesClient) GetImpact(entity string) (*ImpactResult, error)
func (c *SynapsesClient) Recall(query string) (*RecallResult, error)
```

The agent calls these during its reasoning loop. For the control run, the client returns empty results (effectively disabled) so the comparison is apples-to-apples.

### Context Tracking for Context F1

For ContextBench's Context F1 metric, every file and line range accessed via Synapses must be logged. The MCP client wraps every call and records:

```go
type ContextAccess struct {
    TaskID    string
    File      string
    LineStart int
    LineEnd   int
    Tool      string // "prepare_context", "search", "get_impact", "recall"
    Timestamp time.Time
}
```

These are compared against ContextBench's gold annotations at evaluation time.

### Pre-indexing Strategy

For ContextBench, repos are fixed snapshots. Index once, reuse across all tasks for that repo.

---

## Execution Sequence

### Phase 1: Infrastructure (Week 1-2)

- [ ] Build `cmd/benchmark/main.go` with CLI flags
- [ ] Build `agent/mcp_client.go` — HTTP client for Synapses MCP tools
- [ ] Build `agent/mini_swe_agent.go` — minimal agent loop
- [ ] Build `indexer/repo_indexer.go` — pre-indexing with disk cache
- [ ] Build `reporter/reporter.go` — JSON output + leaderboard format

### Phase 2: RepoBench-R (Week 2, fastest to ship)

- [ ] Build `benchmarks/repobench.go` — HuggingFace dataset loader + Acc@k scorer
- [ ] Run all 4 configs × 2 difficulties × 5 retrieval modes
- [ ] Publish ablation results internally
- [ ] Compare against BM25 and dense baselines from RepoBench paper

### Phase 3: ContextBench (Week 3-4, most complex)

- [ ] Build `benchmarks/contextbench.go` — dataset loader + Context F1 calculator + context access logger
- [ ] Pre-index all 66 ContextBench repos
- [ ] Full run (1,136 tasks)
- [ ] Control vs treatment subset comparison (100-200 tasks)
- [ ] Submit to ContextBench leaderboard

### Phase 4: Publish (Week 4)

- [ ] Write benchmark report (markdown + JSON results)
- [ ] ContextBench: submit to public leaderboard
- [ ] Internal ablation report (RepoBench-R channel analysis)

---

## What Numbers to Publish

| Benchmark | Metric | Comparison Point | Narrative |
|---|---|---|---|
| ContextBench | Context F1 (precision + recall) | Leaderboard position; most entries have Precision < 0.4 | "Synapses delivers higher-precision context than any pure-LLM scaffolding approach" |
| RepoBench-R | Acc@5 hybrid vs BM25 | BM25 ~60%, dense ~65% on hard subset | "Synapses' multi-channel fusion outperforms BM25 by X% on hard cross-file retrieval" |

**The unified narrative:**
> "Synapses delivers higher-precision, intent-aware context than any pure-LLM scaffolding approach. On ContextBench — the only benchmark that directly measures context quality — Synapses achieves [F1], ranking [position]."

---

## Infrastructure Requirements

| Component | Requirement | Notes |
|---|---|---|
| Synapses daemon | Running on same machine or local network | MCP HTTP on port 8844 |
| Python | 3.10+ | For HuggingFace datasets |
| Go | 1.22+ | For benchmark binary |
| Docker | Optional | For isolated daemon benchmarking |
| HuggingFace account | Free tier sufficient | For dataset downloads |

---

## References

- ContextBench paper: arxiv.org/abs/2602.05892
- ContextBench leaderboard: contextbench.github.io
- ContextBench GitHub: github.com/EuniAI/ContextBench
- RepoBench paper: arxiv.org/abs/2306.03091
- RepoBench-R dataset: huggingface.co/datasets/tianyang/repobench-r
- CodeRAG-Bench: github.com/code-rag-bench/code-rag-bench
- SWE Context Bench paper: arxiv.org/abs/2602.08316
- Augment Context Engine MCP: augmentcode.com/context-engine
- GraphRAG-Bench (ICLR 2026): github.com/GraphRAG-Bench/GraphRAG-Benchmark
- MemoryAgentBench (ICLR 2026): github.com/HUST-AI-HYZ/MemoryAgentBench
- LongMemEval (ICLR 2025): arxiv.org/abs/2410.10813
- HAL harness: github.com/princeton-pli/hal-harness
