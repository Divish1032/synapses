# SynapsesOS — Roadmap V2

*Post-benchmark roadmap. Originally written 2026-03-26. Updated 2026-03-27 after deep analysis revealed ContextBench is the wrong primary benchmark for Synapses — it measures NL→Code fault localization, but Synapses' value is structural code intelligence. V2 now prioritizes benchmarks that measure what Synapses actually does, then adds complementary retrieval to prove delta value.*

---

## Current State

- **RepoBench-R:** 59.9% Acc@5 (200 repos, 11,453 samples, Python + Java). Ties BM25. Neural embeddings add zero lift. Hard-difficulty gap is structural — lexical engineering exhausted.
- **GraphBench:** 21.8% F1 (50 queries, 5 repos, 3 languages). Callers=42.9%, imports=weak, JS/TS near-zero. This is the benchmark that tests what Synapses actually does.
- **ContextBench:** 4.7% F1 (20 tasks, Python, deterministic). Most failures are NL→Code vocabulary gap — the graph can't bridge "bug description in English" → "buggy function in code" without text retrieval. This benchmark does NOT test Synapses' core value.

---

## Three Values — Unchanged

1. **Speed** — No latency added without proportional value
2. **Privacy** — Code never leaves the machine without explicit config
3. **Accuracy** — Wrong context is worse than no context

---

## Key Insight (2026-03-27)

**ContextBench measures fault localization (NL→Code retrieval). Synapses is a structural code intelligence tool (Code→Code graph).** These are fundamentally different problems:

- **Fault localization** needs: BM25/embedding search over source text, NL-to-code vocabulary bridging, semantic understanding. State-of-the-art systems (Agentless, Meta-RAG) use LLMs and dense embeddings for this — achieving 75-85% file-level accuracy.
- **Structural intelligence** needs: accurate call graphs, import chains, type hierarchies, impact analysis. No BM25 or embedding can answer "what breaks if I change this function?"

Chasing ContextBench F1 by bolting on BM25 source search would make Synapses score higher on a benchmark that doesn't test its value. Instead:

1. **First**: Build the right benchmarks that test structural intelligence (GraphBench improvements + LLM augmentation delta)
2. **Then**: Add complementary retrieval (BM25 source search) and measure the *delta* — does Synapses graph + BM25 beat BM25 alone?

---

## Sprint Sequence

```
  V1 Sprints 1-18 ...................... ALL SHIPPED
  V2-A: Retrieval Quality .............. ALL DONE
  V2-D1-D2: Java Benchmark ............ DONE (59.0% Acc@5)
  V2-E1-E2-E5: Embedding/Reranking .... DONE
  V2-F: Production Hardening ........... ALL DONE
  V2-B1-B2: Benchmark Infra ........... DONE
  V2-C1-C2: Tool Surface .............. DONE

  ── COMPLETED ──

  Sprint 19: Graph Architecture Fixes .. 6 tasks — DONE
  Sprint 20: ContextBench Analysis ..... Harness improvements + deep diagnosis — DONE (revealed paradigm mismatch)
  Sprint 21: GraphBench v1 ............. 5 tasks — DONE (21.8% F1 baseline)

  ── ACTIVE / UPCOMING ──

  Sprint 22: GraphBench v2 ............. 7 tasks — Push structural accuracy to 60%+
  Sprint 23: LLM Augmentation Bench .... 5 tasks — Prove Synapses product value (delta measurement)
  Sprint 24: BM25 Source Search ........ 5 tasks — Add complementary text retrieval to Synapses core
  Sprint 25: ContextBench Honest ....... 5 tasks — Measure BM25-only vs BM25+Graph delta
  Sprint 26: Tool & Retrieval Polish ... 5 tasks — Production quality pass
```

---

## Sprint 19: Graph Architecture Fixes ✅

**Goal:** Fix core graph gaps revealed by ContextBench analysis.

**Status:** DONE — committed on `release/0.8.0` (5d04e45, 5032247)

| # | Task | File | Status |
|---|------|------|--------|
| 19.1 | **pickBestNode exact struct/class match** — Tier 0 for exact case-sensitive struct/interface name matches. Prevents `Table` resolving to `Row.table` method. | `internal/mcp/handlers_util.go` | ✅ Done |
| 19.2 | **Provenance weighting in keyword search** — Vendored (-5), external (-8), generated (-3) score penalties. Prevents vendor/external code outranking user code in search results. | `internal/mcp/handlers_graph.go` | ✅ Done |
| 19.3 | **IMPORTS reverse traversal in ImpactAnalysis** — Supplementary pass after BFS: finds files that IMPORT the root's package. Adds importers at maxDepth (peripheral). Critical for Python where classes are used as types without method calls. | `internal/graph/traverse.go` | ✅ Done |
| 19.4 | **Struct-level ImpactAnalysis call** — For struct/interface nodes, run ImpactAnalysis on the struct node itself (not just per-method). Picks up IMPORTS-based dependents that method-only BFS misses. | `internal/mcp/handlers_context.go` | ✅ Done |
| 19.5 | **Ambiguous method name guard** — Block unqualified call resolution for 50+ common names (`write`, `read`, `get`, `set`, `close`, etc.). Prevents `file.write()` → `HTML.write` false positives. | `internal/resolver/resolver.go` | ✅ Done |
| 19.6 | **Python relative import parsing** — Tree-sitter query for `(relative_import (dotted_name))` AST nodes. Creates NodePackage + IMPORTS edge for `from .foo import Bar`. Previously only absolute imports were parsed. | `internal/parser/python.go` | ✅ Done |

**Key learning:** Daemon caches parsed graphs in `~/.synapses/cache/`. After parser changes, stale caches must be cleared or the fixes are invisible.

---

## Sprint 20: ContextBench Analysis ✅ (Scope Changed)

**Original goal:** Push ContextBench F1 from 8.3% to 15%+.

**Actual outcome:** Deep analysis revealed ContextBench is testing NL→Code fault localization, not structural intelligence. Harness-level fixes (multi-block retrieval, filename boosting, deterministic sorting) yielded marginal improvements (4.3% → 4.7% deterministic baseline). 14/20 tasks score 0% because the tools never find the right file — this is a vocabulary gap, not a graph gap.

**Key findings:**
- Earlier reported F1 scores (8.3%) were inflated by Go map iteration randomness — results weren't reproducible
- True deterministic baseline: 4.7% F1 (20 tasks, Python)
- Root cause: Synapses searches graph node names, not source code text. Bug reports use natural language; code uses identifiers. No keyword overlap = no retrieval.
- State-of-the-art fault localization (Agentless, Meta-RAG) uses LLM-based localization + dense embeddings — fundamentally different from structural graph analysis
- **Conclusion:** ContextBench F1 should be improved by adding BM25 source search as a complementary capability, then measuring the delta that Synapses graph adds on top

**Harness improvements shipped (committed):**
- Deterministic sorting (filename tiebreaker + block start-line tiebreaker)
- Multi-block per-file retrieval with density-sorted windows
- Filename-entity boost (CamelCase→snake_case matching)
- Extern/vendor file penalization
- Debug diagnostics for gold file mention analysis

---

## Sprint 21: GraphBench v1 ✅

**Goal:** Build a benchmark that directly tests Synapses' structural analysis correctness.

**Status:** DONE — 21.8% F1 baseline established.

| # | Task | What | Status |
|---|------|------|--------|
| 21.1 | **Define test format and gold standard** | JSONL format with query types: find_callers, find_callees, find_imports, impact_analysis, find_implementations. | ✅ Done |
| 21.2 | **Generate test cases from real repos** | 50 test cases across 5 repos (Flask, requests, gin, fzf, express), 3 languages. | ✅ Done |
| 21.3 | **Build benchmark runner** | `cmd/benchmark/benchmarks/graphbench.go` with set P/R/F1 scoring. | ✅ Done |
| 21.4 | **Parser gap analysis** | Baseline F1=19.4%. Python best (39%), Go weak (14%), TS near-zero (2%). | ✅ Done |
| 21.5 | **Fix top-3 parser gaps** | find_callers fix (8.3%→42.9%), find_imports multi-symbol strategy. Overall 19.4%→21.8%. | ✅ Done |

**Key gaps identified:** (1) find_imports F1=5% — no file-level entities, (2) JS/TS near-zero — require()/module.exports barely parsed, (3) Go imports weak — package-level resolution incomplete.

---

## Sprint 22: GraphBench v2 — Structural Accuracy Push 🔥 NEXT

**Goal:** Push GraphBench F1 from 21.8% to 60%+. This is Synapses' core product — if the graph is wrong, nothing else matters.

**Why this is #1 priority:** GraphBench directly tests what Synapses promises: accurate structural understanding. Every point of improvement here means better prepare_context, better get_impact, better search results. This is the foundation everything else builds on.

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 22.1 | **File-level entity support** | Add NodeFile entities to the graph so find_imports queries can resolve to actual files. Currently imports create NodePackage edges but no file-level nodes for file-to-file import queries. Parser change across Python, Go, TS. | High | Critical |
| 22.2 | **find_callers precision improvement** | get_context callers field returns too many results (includes transitive callers). Add depth-1-only mode or direct-callers filter. Measure: current callers F1=42.9%, target ≥70%. | Medium | High |
| 22.3 | **JS/TS require() and module.exports parsing** | Express.js scores ~0% because `require('express')` and `module.exports = router` aren't parsed as imports/exports. Add tree-sitter queries for CommonJS patterns. | High | High |
| 22.4 | **Go package-level import resolution** | Go imports resolve to packages but not to specific symbols. `import "net/http"` should create edges to `http.Handler`, `http.ListenAndServe`, etc. when those symbols are used. | Medium | Medium |
| 22.5 | **Expand test cases to 100** | Add 50 more test cases: 20 Python (focus on class hierarchies, decorators), 15 Go (interfaces, goroutine patterns), 15 TS (class inheritance, async/await chains). More diverse repos. | Medium | High |
| 22.6 | **find_implementations query type** | Test interface→concrete type resolution. Python ABCs, Go interfaces, TS abstract classes. Currently untested — could be a strength or a gap. | Medium | Medium |
| 22.7 | **Cross-file data flow tracking** | Variables assigned in one file and used in another (via imports) should create DATA_FLOWS edges. Currently only intra-file data flow is tracked. Critical for understanding how values propagate. | High | Medium |

**Success criteria:** GraphBench F1 ≥ 60% overall. find_callers ≥ 70%, find_imports ≥ 50%, JS/TS ≥ 20%.

---

## Sprint 23: LLM Augmentation Benchmark — Product Value Proof

**Goal:** Answer THE question: *Does giving an LLM access to Synapses tools make it better at solving real coding tasks?* This is the number that justifies Synapses' existence.

**Design:** Run Claude on SWE-Bench-Verified tasks in two conditions:
- **Control:** Claude + repo access (read files, grep, run tests) — no Synapses
- **Treatment:** Claude + repo access + Synapses MCP tools (search, get_context, get_impact, prepare_context)
- **Metric:** Pass@1 delta (treatment minus control)

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 23.1 | **Agent scaffold** | Minimal agent loop in `cmd/benchmark/benchmarks/swebench.go`: issue text → tool calls → patch. Two modes: `baseline` (file read/grep only) and `synapses` (+ MCP tools). Claude API with tool_use, temperature=0. | High | Critical |
| 23.2 | **SWE-Bench-Verified dataset integration** | Download dataset (500 tasks, 12 repos). Build repo checkout + pre-indexing pipeline. Cache indexed repos by `repo@commit`. | Medium | Critical |
| 23.3 | **Evaluation harness** | Run agent on N tasks in both conditions. Produce prediction JSONL. Use SWE-bench eval Docker images. Output: per-task pass/fail, aggregate Pass@1, delta with bootstrap CI. | High | Critical |
| 23.4 | **Pilot run (20 tasks)** | Run on 20 diverse tasks. Measure delta. If delta < +2pp, analyze why — are tools not being called? Are tool results not helpful? Is the agent scaffold too simple? | Medium | High |
| 23.5 | **Tool usage analysis** | Log which Synapses tools were called per task. Compute "tool contribution rate" — fraction of successful patches where a Synapses tool provided the key insight. | Medium | Medium |

**Success criteria:** Pass@1 delta ≥ +3pp on 20-task pilot. Tool contribution rate ≥ 50%.

**Key insight:** If this benchmark shows no delta, that's valuable information — it means Synapses' tools aren't surfacing information the LLM can't find on its own. If it shows +5pp or more, that's the pitch deck number.

---

## Sprint 24: BM25 Source Code Search — Complementary Retrieval

**Goal:** Add full-text search over actual source code to Synapses. This is NOT replacing the graph — it's adding a retrieval channel that the graph alone can't provide. The graph answers "what connects to X?" while BM25 answers "where does this term appear?"

**Why now (not earlier):** We need GraphBench and LLM benchmarks established first so we can measure the *delta* that BM25 adds vs graph-only, and the delta that graph adds vs BM25-only. Without baselines, we can't tell if improvements come from BM25 or from the graph.

**Architecture decisions:**
- **Chunking:** Function-level (AST-aware). The parser already knows function boundaries — use them as natural chunk boundaries. Research shows function-level chunks (32-64 lines) work best at small token budgets.
- **Index:** BM25 with word-level splitting (research: best quality-latency tradeoff for code). Use `crawlab-team/bm25` Go library or implement from scratch (~200 lines for inverted index + TF-IDF scoring).
- **Storage:** Extend SQLite graph DB with an FTS5 virtual table over source code chunks. Indexed per-function with file path + line range metadata.
- **Privacy:** 100% local. No embeddings, no network calls. Pure algorithmic text matching.
- **Speed:** BM25 queries are O(1) with inverted index, <10ms. Index build during existing parse phase adds ~20% time.

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 24.1 | **Function-level source chunking** | During parsing, extract function/method bodies as text chunks with metadata (file, start_line, end_line, function_name). Store in graph DB alongside AST nodes. Reuse existing parser infrastructure — extend `NodeFunction` with a `Body` field or create a `chunks` table. | Medium | Critical |
| 24.2 | **BM25 inverted index** | Build FTS5 virtual table over source chunks. Index function name + body text. Query returns ranked chunks with file + line range. Evaluate: FTS5 built-in BM25 vs custom implementation. FTS5 is simpler and already used for memories. | Medium | Critical |
| 24.3 | **`search` tool: `source` mode** | Add `mode=source` to the search handler. Queries BM25 index over source code. Returns file + function + line range + snippet. Complements existing `keyword` (node names), `fulltext` (memory FTS), and `semantic` (vector) modes. | Low | High |
| 24.4 | **Hybrid fusion in search** | When no mode is specified, run both `keyword` (graph nodes) and `source` (BM25 code) in parallel. Fuse results with RRF (already implemented in recall_engine.go). Graph results surface structurally relevant code; BM25 surfaces lexically matching code. | Medium | High |
| 24.5 | **Function-aware retrieval windows** | When returning code context, snap to function boundaries using AST metadata. Instead of "lines 80-166 of file.py" (arbitrary window), return "function `_separability_matrix` at lines 207-317" (meaningful unit). | Medium | Medium |

**Success criteria:** `search mode=source` returns the correct file in top-5 results for ≥60% of ContextBench tasks. Hybrid search (graph+BM25) outperforms either channel alone.

**Key design principle:** BM25 source search is a *tool* the LLM can use, not a replacement for graph traversal. The LLM decides when to search text vs when to traverse the graph. Synapses provides both capabilities.

---

## Sprint 25: ContextBench Honest — Delta Measurement

**Goal:** Re-run ContextBench in a way that honestly measures Synapses' contribution. Instead of "can Synapses alone find the bug?" (unfair), measure "does adding Synapses graph on top of BM25 text search improve retrieval?"

**Design:** Three conditions:
- **BM25-only:** Source code search only, no graph traversal
- **Graph-only:** Current Synapses tools only (search nodes, prepare_context, get_impact)
- **BM25+Graph (hybrid):** Both channels, RRF fusion

**Metric:** F1 for each condition. The number that matters is `hybrid F1 - BM25-only F1` = Synapses' structural contribution.

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 25.1 | **Refactor ContextBench harness for multi-condition runs** | Run each task in all 3 conditions. Produce per-condition F1 and delta columns in output. Deterministic (seeded) entity extraction and file scoring. | Medium | Critical |
| 25.2 | **BM25-only baseline** | ContextBench using only `search mode=source` tool calls. No prepare_context, no get_impact. Measures what pure text retrieval achieves. Expected: 15-25% F1 (competitive with BM25 baselines in literature). | Low | Critical |
| 25.3 | **Hybrid condition** | ContextBench using BM25 source search + graph tools. Entity → BM25 search for file candidates → prepare_context on top candidates → merge. Expected: 20-35% F1 if graph adds value. | Medium | High |
| 25.4 | **Delta analysis** | Per-task breakdown: which tasks improved from graph addition? Which got worse? Categorize: (a) graph helped — found structurally connected files BM25 missed, (b) graph hurt — BFS noise diluted BM25 signal, (c) neutral. | Low | High |
| 25.5 | **Expand to 66 tasks** | Run all 66 ContextBench tasks (not just --limit=20). Multi-language if dataset supports it. Statistical significance test on delta. | Medium | Medium |

**Success criteria:** Hybrid F1 ≥ BM25-only F1 + 5pp. At least 30% of tasks show measurable graph contribution.

**What this proves:** If the delta is positive, Synapses' structural intelligence adds real value on top of text search. If it's zero or negative, we know the graph needs more work (back to Sprint 22) or the tools need better orchestration.

---

## Sprint 26: Tool & Retrieval Polish

**Goal:** Production quality pass on all tools and benchmarks.

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 26.1 | **Session-init token budget** (was V2-C3) | Enforce 800-token cap on `session_init` default scope. Move detailed sections behind `scope="full"`. | Low | Medium |
| 26.2 | **Tool documentation quality pass** (was V2-C4) | Review all tool descriptions for clarity, examples, when-to-use guidance. | Medium | Medium |
| 26.3 | **LLM query expansion** (was V2-E3) | When brain is available, use lightweight prompt to extract likely completion targets. Add to query. 300ms timeout. | Medium | Medium |
| 26.4 | **TypeScript/Go retrieval evaluation** (was V2-D3) | Custom evaluation set: 10 repos per language. Validates parser depth for TS/Go. | High | Medium |
| 26.5 | **Benchmark CI integration** (was V2-B6) | GitHub Actions: run GraphBench + RepoBench-R on PRs to `release/*`. Fail if >2pp regression. | Medium | Medium |

---

## Priority Order

| Priority | Sprint | Why |
|----------|--------|-----|
| 1 | **Sprint 22: GraphBench v2** | Fix the core product. If the graph is wrong, nothing else matters. |
| 2 | **Sprint 23: LLM Augmentation** | Prove product value. The number that justifies Synapses to users. |
| 3 | **Sprint 24: BM25 Source Search** | Add complementary capability. Enables honest ContextBench. |
| 4 | **Sprint 25: ContextBench Honest** | Measure delta. Proves graph adds value over text search alone. |
| 5 | **Sprint 26: Polish** | Production readiness. Do after measurement infrastructure exists. |

---

## What V2 Does NOT Include

- **New language parsers.** 61-parser coverage is sufficient. Fix quality before adding quantity.
- **Cloud embeddings or external APIs for retrieval.** Privacy-first means local-only retrieval.
- **LLM-in-the-loop for localization.** Agentless-style "ask GPT-4 which files are relevant" is effective but violates speed+privacy. Synapses' localization must be algorithmic.
- **UI/frontend work.** Tauri app is a separate workstream.
- **Chasing ContextBench F1 as a standalone metric.** It conflates text retrieval with structural intelligence. Only meaningful as a delta measurement (Sprint 25).

---

## The Narrative Arc

```
Today:       "GraphBench F1 = 21.8%. ContextBench F1 = 4.7% (wrong benchmark for the product).
              Synapses' structural intelligence works but has precision gaps."

After S22:   "GraphBench F1 ≥ 60%. Call graphs, imports, and impact analysis are reliably
              accurate across Python, Go, and TS. The foundation is solid."

After S23:   "Claude + Synapses solves +3pp more SWE-Bench tasks than Claude alone.
              The product value is measurable and provable."

After S24:   "BM25 source search added. Synapses now finds code by text AND by structure.
              Two retrieval channels that complement each other."

After S25:   "ContextBench: BM25-only=20%, Graph-only=5%, Hybrid=28%. The graph adds
              +8pp on top of text search — proof that structural intelligence matters."

After S26:   "Tools polished, CI prevents regressions, multi-language validated.
              Ready for broader adoption."
```

The goal is not to inflate benchmark numbers. The goal is to prove that **structural code intelligence adds value that text search alone cannot provide** — and then ship that value to users.
