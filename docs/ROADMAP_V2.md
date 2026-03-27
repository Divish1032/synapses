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

## Sprint 22: GraphBench v2 — Structural Accuracy Push 🔥 IN PROGRESS

**Goal:** Push GraphBench F1 from 24.6% to 60%+. This is Synapses' core product — if the graph is wrong, nothing else matters.

**Current score: 43.9% F1** (up from 24.6% baseline, +78% relative improvement)

**Per-query-type scores:**
| Query Type | Tests | F1 | Status |
|---|---|---|---|
| find_implementations | 1 | 66.7% | Excellent (was 22.2%) |
| find_callers | 7 | 58.3% | Good (was 42.9%) |
| find_callees | 15 | 44.7% | Improved (was 30.4%) |
| find_imports | 18 | 40.7% | Much improved (was 6.4%) |
| impact_analysis | 9 | 39.8% | OK (was 37.6%) |

**Per-language scores:**
| Language | Tests | F1 | Status |
|---|---|---|---|
| Python | 20 | 50.3% | Good (was 40.6%) |
| Go | 20 | 49.5% | Good (was 21.0%) |
| TypeScript | 10 | 22.9% | Improved (was 0.0%) |

| # | Task | What | Effort | Impact | Status |
|---|------|------|--------|--------|--------|
| 22.1 | **File-level imports in get_context** | When get_context receives a file path, include the NodeFile's outgoing IMPORTS edges in a new `imports` JSON field. Enables direct "what does this file import?" queries. | Medium | Critical | ✅ Done |
| 22.1b | **looksLikeFilePath for bare filenames** | Accept bare filenames like `gin.go`, `context.go` (lowercase + code extension) as file path queries, not just paths with `/`. | Low | High | ✅ Done |
| 22.2 | **CommonJS require() in TS parser** | Add tree-sitter query for `require('...')` → IMPORTS edge. JS parser already had this; TS parser now matches. | Low | Medium | ✅ Done |
| 22.3 | **Python relative import file resolution** | Resolve `from .cli import X` → target file `src/flask/cli.py` instead of storing importer's path. Handle dotted paths (`.sansio.blueprints` → `sansio/blueprints.py`). | Low | High | ✅ Done |
| 22.4 | **Precision matching fix** | Fix GraphBench precision calculation to use same partial dot-suffix matching as recall. Was using exact-only lookups. | Low | Medium | ✅ Done |
| 22.5 | **impact_analysis depth tuning** | Reduce BFS depth from 3 to 2. Depth=3 returns too many transitive dependents (27% precision vs 70% recall). | Low | Medium | ✅ Done |
| 22.6 | **JS prototype method parsing** | Express.js `app.METHOD = function() {}` patterns not parsed. Added tree-sitter query for prototype method assignment — captures all Express.js application API methods. | Medium | Critical | ✅ Done |
| 22.7 | **Aliased callee extraction** | `obj.method()` calls now set PkgAlias to the object name, enabling cross-module call resolution. `createApplication → app.init()` now resolves through import map. | Medium | High | ✅ Done |
| 22.8 | **Dotted-name entity resolution** | Prefer code entities (functions/methods) over NL-extracted concepts and test files when resolving dotted names like `app.listen`. | Low | High | ✅ Done |
| 22.9 | **Exported function preference** | pickBestNode now prefers exported functions over non-exported ones. `New` resolves to gin.New (public API) over internal helpers. | Low | Medium | ✅ Done |
| 22.10 | **Test directory detection** | isTestFile now detects test/, tests/, spec/ directories. Express.js test files correctly classified. | Low | Medium | ✅ Done |
| 22.11 | **find_implementations type filter** | Only return struct/class/interface nodes, not their methods. IRouter implementations: 22% → 67% F1. | Low | Medium | ✅ Done |
| 22.12 | **Cross-repo callee resolution** | Tests expect callees like `werkzeug.serving.run_simple` (external package). Requires dependency indexing. | High | Medium | ⏳ Blocked |
| 22.13 | **Expand test cases to 100** | Add 50 more test cases across diverse repos and languages. | Medium | High | ⏳ Remaining |

**Key architectural changes shipped (11 commits):**
1. **`imports` field in get_context JSON response** — new structural information directly answering file-level import queries
2. **Bare filename resolution** — `gin.go` now resolves as a file path, not just a symbol name
3. **Python relative import → file path mapping** — parser resolves `.cli` to `src/flask/cli.py`, `.sansio.blueprints` to `sansio/blueprints.py`
4. **CommonJS require() → IMPORTS edges** — JS/TS files now have import edges from require() calls
5. **JS prototype method parsing** — `obj.method = function() {}` patterns create NodeMethod entries
6. **Aliased callee extraction** — `obj.method()` sets PkgAlias enabling cross-module call resolution
7. **Dotted-name resolution improvement** — prefers code over concepts/files for `app.listen` style queries
8. **Exported function preference** — public API functions preferred over internal helpers
9. **Test directory detection** — `test/`, `tests/`, `spec/` directories recognized
10. **find_implementations type filtering** — only struct/class types, not their methods
11. **Import deduplication** — prevents duplicate import entries in response

**Remaining gaps to 60% (need +16pp):**
- 10 tests at 0%: 5 cross-package calls (can't fix without dependency indexing), 3 ambiguous names (get/New/Run), 2 JS impact analysis
- find_imports precision 30% — returns internal relative imports alongside expected external ones
- TypeScript non-import tests still mostly 0% — need module.exports tracing for full CommonJS support

**Success criteria:** GraphBench F1 ≥ 60% overall. find_callers ≥ 70%, find_imports ≥ 50%, JS/TS ≥ 20%.

---

## Sprint 22b: GraphBench Language Expansion — Production Readiness ✅ IN PROGRESS

**Goal:** Expand GraphBench from 3 languages to 6. Fix parser/resolver gaps discovered.

**Current score: 40.3% F1** across 80 tests, 6 languages (up from 24.6% on 50 tests, 3 languages)

| Language | Tests | F1 | Status |
|---|---|---|---|
| Python | 20 | 54.9% | Strong |
| Go | 20 | 50.4% | Strong |
| Rust | 10 | 38.3% | Good (new) |
| Ruby | 10 | 36.1% | Good (new, was 0%) |
| TypeScript | 10 | 22.9% | Improved (was 0%) |
| Java | 10 | 14.7% | Working (was 0%) |

**Why now:** We can't ship Sprint 23 (LLM augmentation) without knowing if our parsers work for Java (most popular enterprise language), Rust (fastest growing systems language), and Ruby (critical web framework language). GraphBench is our quality gate — every language we add becomes a regression test. The fixes we made in Sprint 22 (aliased callee extraction, prototype method parsing, dotted-name resolution) may have broken things for untested languages.

**Repos selected:**
- **Java:** OkHttp (square/okhttp) — mid-size HTTP client, clear interceptor/protocol interfaces, cross-module calls
- **Rust:** reqwest (seanmonstar/reqwest) — trait-based architecture, feature gating, layered abstractions
- **Ruby:** Rack (rack/rack) — minimal but rich middleware pattern, clean call chains, module dependencies

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 22b.1 | **Java test cases (OkHttp)** | 10 test cases: find_imports (3), find_callees (3), find_callers (2), impact_analysis (1), find_implementations (1). Focus on interface hierarchy (Interceptor, Call, Connection) and cross-package calls. | Medium | Critical |
| 22b.2 | **Rust test cases (reqwest)** | 10 test cases: find_imports (3), find_callees (3), find_callers (2), impact_analysis (1), find_implementations (1). Focus on trait implementations, async call chains, and module hierarchy. | Medium | Critical |
| 22b.3 | **Ruby test cases (Rack)** | 10 test cases: find_imports (3), find_callees (3), find_callers (2), impact_analysis (1), find_implementations (1). Focus on middleware stack, require patterns, and module mixins. | Medium | Critical |
| 22b.4 | **Fix Java parser gaps** | Run GraphBench on Java, analyze failures, fix any parser issues (import resolution, method call extraction, interface detection). | Variable | High |
| 22b.5 | **Fix Rust parser gaps** | Run GraphBench on Rust, analyze failures, fix any parser issues (use/mod resolution, trait impl detection, associated function calls). | Variable | High |
| 22b.6 | **Fix Ruby parser gaps** | Run GraphBench on Ruby, analyze failures, fix any parser issues (require/require_relative resolution, method_missing, module inclusion). | Variable | High |
| 22b.7 | **Cross-language regression test** | Re-run full 80-test GraphBench (50 existing + 30 new) to verify no regressions. Target: 40%+ F1 per language, 45%+ overall. | Low | Critical |

**Success criteria:** GraphBench F1 ≥ 40% for EACH of the 6 languages. Overall F1 ≥ 45% on 80 tests.

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
