# SynapsesOS — Roadmap V2

*Post-benchmark roadmap. Originally written 2026-03-26. Updated 2026-03-26 after ContextBench architecture analysis revealed core graph gaps. V1 (Sprints 1-18) and V2-A/D1-D2/E1-E2-E5/F are fully shipped. V2 now focuses on three measurement tracks: graph correctness, context retrieval quality, and LLM augmentation delta.*

---

## Current State

- **RepoBench-R:** 59.9% Acc@5 (200 repos, 11,453 samples, Python + Java). Ties BM25. Neural embeddings add zero lift. Hard-difficulty gap is structural — lexical engineering exhausted.
- **ContextBench:** 8.3% F1 (5 tasks, Python). Improved from 4.3% baseline through architecture fixes. Graph structural intelligence is now measurable but weak — most gold context requires semantic understanding the graph doesn't have.
- **Architecture fixes shipped (uncommitted):** Python relative imports, IMPORTS reverse traversal, pickBestNode exact-match tier, provenance weighting in search, ambiguous call guard, struct-level ImpactAnalysis, benchmark `affected_files` parsing.

---

## Three Values — Unchanged

1. **Speed** — No latency added without proportional value
2. **Privacy** — Code never leaves the machine without explicit config
3. **Accuracy** — Wrong context is worse than no context

---

## Sprint Sequence

```
  V1 Sprints 1-18 ...................... ALL SHIPPED
  V2-A: Retrieval Quality .............. ALL DONE (10 tasks — 4 shipped, 6 invalidated)
  V2-D1-D2: Java Benchmark ............ DONE (59.0% Acc@5, within 2pp of Python)
  V2-E1-E2-E5: Embedding/Reranking .... DONE (cross-encoder, code embeddings, adaptive fusion)
  V2-F: Production Hardening ........... ALL DONE (startup, RAM, graceful degradation, crash recovery)
  V2-B1-B2: Benchmark Infra ........... DONE (ContextBench runner, context access logger)
  V2-C1-C2: Tool Surface .............. DONE (tool tiering, first-session highlights)

  ── ACTIVE / UPCOMING ──

  Sprint 19: Graph Architecture Fixes .. 6 tasks — DONE, needs commit
  Sprint 20: ContextBench F1 Push ...... 6 tasks — Find and fix remaining gaps
  Sprint 21: Graph Accuracy Benchmark .. 5 tasks — Benchmark A (structural correctness)
  Sprint 22: LLM Augmentation Benchmark  5 tasks — Benchmark B (Synapses + LLM delta)
  Sprint 23: Tool & Retrieval Polish ... 5 tasks — Remaining V2-C/E/D tasks
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

**Key learning:** Daemon caches parsed graphs in `~/.synapses/cache/`. After parser changes, stale caches must be cleared or the fixes are invisible. This blocked diagnosis for hours.

---

## Sprint 20: ContextBench F1 Push

**Goal:** Improve ContextBench F1 from 8.3% toward 15%+ by finding and fixing the remaining retrieval gaps. Not over-engineering — find the highest-leverage gaps through task-by-task failure analysis.

**Current scores (5-task pilot, Python):**

| Task | F1 | Issue |
|------|-----|-------|
| Task 1 (separable.py) | 0.0% | Right file found, but retrieval window (lines 80-166) doesn't reach gold (lines 207-317). Window positioning problem. |
| Task 2 (NdarrayMixin→table.py) | 12.9% | Fixed by Sprint 19. `table.py` now found via IMPORTS. |
| Task 3 (coordinates) | 16.9% | Partial — some gold files found, but `attributes.py` missed. |
| Task 4 (html.py) | 0.0% | Right file is top-mentioned (18 mentions), but retrieval starts at line 42. Gold is at lines 345-421. Deep-in-file problem. |
| Task 5 (sliced_wcs.py) | 11.8% | Partial — file found but line coverage misaligned. |

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 20.1 | **Task-by-task failure analysis (20 tasks)** | Run full 20-task ContextBench. For each task scoring <5% F1, diagnose: (a) is the right file found? (b) is the right region found? (c) what tool call would have found it? Categorize failures into: file-miss, region-miss, entity-miss, tool-gap. | Medium | Critical |
| 20.2 | **Retrieval window tuning** | Tasks 1 and 4 find the right file but retrieve the wrong region (top of file instead of gold region deeper in). Investigate: use line mentions from tools to center windows on mentioned lines rather than starting from line 1. Density-based window placement. | Medium | High |
| 20.3 | **Multi-window per file** | Currently retrieves one contiguous window per file. Gold context often spans multiple disjoint regions (e.g., imports at top + implementation at line 300). Allow 2-3 windows per file when mention density shows multiple clusters. | Medium | High |
| 20.4 | **Entity extraction improvement** | The harness extracts entities from the problem statement using regex. Missed entities = missed tool calls = missed files. Audit entity extraction on all 20 tasks. Consider: CamelCase splitting, backtick-quoted identifiers, function signatures in issue text. | Low | Medium |
| 20.5 | **Benchmark `affected_files` path normalization** | `affected_files` returns absolute paths (`/private/tmp/cb_repos/...`). The `addMention` function normalizes to repo-relative, but verify this works for all path formats (symlinks, `/private/tmp` vs `/tmp` on macOS). | Low | Medium |
| 20.6 | **Run 20-task benchmark with all fixes** | Full 20-task run after 20.2-20.5 fixes. Compare against 8.3% baseline. Target: 12%+ F1. | Low | Critical |

**Success criteria:** F1 ≥ 12% on 20-task Python ContextBench. At least 10/20 tasks score >0% F1 (file found).

---

## Sprint 21: Graph Accuracy Benchmark (Benchmark A)

**Goal:** Build a benchmark that directly tests Synapses' structural analysis correctness — does the graph accurately represent code relationships? This is what Synapses *actually does*, unlike ContextBench which tests retrieval.

**Why:** ContextBench conflates graph quality with retrieval strategy. Benchmark A isolates graph quality. If the graph is wrong, no retrieval strategy can fix it. If the graph is right, retrieval improvements have a solid foundation.

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 21.1 | **Define test format and gold standard** | Design JSONL format: `{repo, commit, language, tests: [{query_type, query, expected}]}`. Query types: `find_callers(fn)`, `find_callees(fn)`, `find_imports(file)`, `impact_analysis(symbol)`, `find_implementations(interface)`. Expected = exact set of node names/files. | Low | Critical |
| 21.2 | **Generate test cases from real repos** | For 5 repos per language (Python, Java, Go, TypeScript), manually create 10 test cases each using IDE cross-references as ground truth. Total: 200 test cases. Use popular, well-structured repos (Flask, Spring Boot, Gin, Next.js). Automate where possible using LSP/gopls/pyright. | High | Critical |
| 21.3 | **Build benchmark runner** | `cmd/benchmark/benchmarks/graphbench.go`. For each test case: index repo, run query via Synapses API, compare result set against expected. Metrics: Precision, Recall, F1 per query type and per language. | Medium | Critical |
| 21.4 | **Parser gap analysis** | Run Benchmark A, identify systematic failures. Categorize: (a) parser didn't create the node, (b) parser didn't create the edge, (c) resolver created wrong edge, (d) node resolution picked wrong target. Each category maps to a specific fix location. | Medium | High |
| 21.5 | **Fix top-3 parser gaps** | Based on 21.4 analysis, fix the three highest-impact parser/resolver issues. Each fix should improve multiple test cases. Re-run benchmark to verify. | Medium | High |

**Success criteria:** Graph F1 ≥ 80% on `find_callers` and `find_imports` queries. ≥ 60% on `impact_analysis` (harder due to transitive closure). All 4 languages tested.

---

## Sprint 22: LLM Augmentation Benchmark (Benchmark B)

**Goal:** Measure Synapses' actual product value — does giving an LLM access to Synapses tools make it better at solving real coding tasks? This is the number that matters for users.

**Design:** Run Claude on SWE-Bench-Verified tasks in two conditions:
- **Control:** Claude + repo access (read files, grep, run tests) — no Synapses
- **Treatment:** Claude + repo access + Synapses MCP tools (search, get_context, get_impact, prepare_context)
- **Metric:** Pass@1 delta (treatment minus control)

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 22.1 | **Agent scaffold** | Build a minimal agent loop in `cmd/benchmark/benchmarks/swebench.go`: takes issue text → calls tools → produces patch. Two modes: `baseline` (file read/grep only) and `synapses` (+ MCP tools). Use Claude API with tool_use. Deterministic (temperature=0). | High | Critical |
| 22.2 | **SWE-Bench-Verified dataset integration** | Download SWE-Bench-Verified dataset (500 tasks, 12 repos). Build repo checkout + pre-indexing pipeline. Cache indexed repos by `repo@commit`. | Medium | Critical |
| 22.3 | **Evaluation harness** | Run agent on N tasks in both conditions. Produce prediction JSONL. Use SWE-bench evaluation Docker images to score. Output: per-task pass/fail for both conditions, aggregate Pass@1, delta with bootstrap CI. | High | Critical |
| 22.4 | **Pilot run (20 tasks)** | Run on 20 diverse tasks (mix of Python repos, difficulty levels). Measure delta. If delta < +2pp, analyze why — are tools not being called? Are tool results not helpful? Is the agent scaffold too simple? | Medium | High |
| 22.5 | **Tool usage analysis** | For each task, log which Synapses tools were called and whether the tool response contained information that appeared in the final patch. Compute "tool contribution rate" — fraction of successful patches where a Synapses tool provided the key insight. | Medium | Medium |

**Success criteria:** Pass@1 delta ≥ +3pp on 20-task pilot. Tool contribution rate ≥ 50% (Synapses tools contributed to at least half of successful patches).

---

## Sprint 23: Tool & Retrieval Polish

**Goal:** Remaining high-value tasks from V2 streams that don't fit the benchmark sprints but improve the product.

| # | Task | What | Effort | Impact |
|---|------|------|--------|--------|
| 23.1 | **Session-init token budget** (was V2-C3) | Enforce 800-token cap on `session_init` default scope. Move detailed sections behind `scope="full"`. | Low | Medium |
| 23.2 | **Tool documentation quality pass** (was V2-C4) | Review all 35+ tool descriptions for clarity, examples, when-to-use guidance. Tool catalog embedding quality depends on this. | Medium | Medium |
| 23.3 | **LLM query expansion** (was V2-E3) | When brain is available, use lightweight prompt to extract likely completion targets. Add to query for `synapses-search` and `synapses-embed` modes. 300ms timeout. | Medium | Medium |
| 23.4 | **TypeScript/Go retrieval evaluation** (was V2-D3) | Custom evaluation set: 10 repos per language, cross-file completion samples. Validates parser depth for TS/Go where Synapses has deepest type resolution. | High | Medium |
| 23.5 | **Benchmark CI integration** (was V2-B6) | GitHub Actions: run RepoBench-R (5-repo quick mode) on PRs to `release/*`. Fail if >2pp regression. Baseline in `docs/benchmarks/baseline.json`. | Medium | Medium |

---

## Priority Order

| Priority | Sprint | Why |
|----------|--------|-----|
| 1 | Sprint 19: Architecture Fixes | Done. Commit and ship. Unblocks everything. |
| 2 | Sprint 20: ContextBench F1 Push | Directly improves measurable quality. Reveals more gaps. |
| 3 | Sprint 21: Graph Accuracy Benchmark | Tests what Synapses actually does. Foundational measurement. |
| 4 | Sprint 22: LLM Augmentation Benchmark | The product-value proof. Harder to build but most convincing. |
| 5 | Sprint 23: Tool & Retrieval Polish | Quality-of-life. Do after measurement infrastructure exists. |

---

## What V2 Does NOT Include

- **New language parsers.** 61-parser coverage is sufficient. Fix quality before adding quantity.
- **New MCP tools.** 35+ tools is enough. Add only if benchmarks reveal a missing capability.
- **Cloud infrastructure.** Synapses remains local-first.
- **UI/frontend work.** Tauri app is a separate workstream.
- **Learned sparse retrieval (was V2-E4).** Deferred — requires training infrastructure and the ROI is unclear given lexical engineering exhaustion.

---

## The Narrative Arc

```
Today:       "Synapses ties BM25 on retrieval. Graph analysis works but isn't well-measured.
              ContextBench F1 = 8.3% after architecture fixes."

After S20:   "ContextBench F1 ≥ 12%. We know exactly where the graph helps and where it doesn't."

After S21:   "Graph accuracy is ≥ 80% on callers/imports. Parser gaps are identified and fixed.
              We have a regression test suite for structural correctness."

After S22:   "Claude + Synapses solves +3pp more SWE-Bench tasks than Claude alone.
              The product value is measurable and provable."

After S23:   "Tool surface is polished, CI prevents regressions, TS/Go validated.
              Ready for broader adoption."
```

The goal is not to ship features. The goal is to produce numbers that prove the architecture is right — and then let those numbers speak.
