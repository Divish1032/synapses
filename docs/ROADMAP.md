# SynapsesOS — Roadmap

*Last updated: 2026-03-21 — Research Council integration. 45 findings from 6 domain-expert researchers (Graph Algorithms, IR & Embeddings, Performance, Program Analysis, Memory Systems, Context Delivery) merged with existing roadmap. Forward sprints restructured by value impact: performance foundations first, then retrieval revolution, graph intelligence, analysis depth, self-refining intelligence, cross-domain knowledge, skills, and observability. Every item maps to Speed, Accuracy, or Privacy.*

---

## Vision

**Now:** SynapsesOS is the intelligence substrate for AI coding agents — a graph-based code intelligence layer that gives agents deep semantic understanding of codebases, persistent cross-session memory, architectural rule enforcement, and cross-project knowledge sharing.

**Next:** The graph, memory, and coordination primitives are domain-agnostic. Code is the first domain. Synapses will expand to the universal knowledge substrate for ALL AI agents — infrastructure (Terraform, k8s), APIs (OpenAPI, GraphQL), documents, and any domain where agents need structured, persistent, cross-session knowledge. Every architecture decision supports this pivot without a rewrite.

**The moat:** Local-first + typed graph edges + entity-anchored memory with auto-staleness + quad-channel hybrid retrieval (graph + BM25 + semantic + temporal) + cross-domain knowledge sharing + self-refining context delivery. No competitor combines all six.

**The insight:** The AST-derived code graph is not just a code intelligence feature — it's a memory retrieval weapon. Multi-hop queries ("what does the auth service's dependency know about the migration?") traverse REAL typed relationships (CALLS, IMPORTS, IMPLEMENTS), not LLM-extracted guesses. This gives Synapses a structural advantage on memory benchmarks that no embedding-only competitor can match.

**The Research Council insight (2026-03-21):** Six domain researchers independently converged on the same gap — the graph and embedding systems are well-built but operate in isolation. Bridging them (semantic-structural hybrid scoring, graph-augmented query expansion, spreading activation for recall) is where the biggest accuracy gains live. The council also identified that cognitive science models (ACT-R power-law decay, spreading activation) outperform the current simple heuristics with minimal implementation effort.

---

## Three Values — Never Compromise

1. **Speed** — No latency added to hot paths without proportional value
2. **Privacy** — Code never leaves the machine without explicit user config
3. **Accuracy** — Wrong context is worse than no context

---

## What's Shipped

- Phase 0: Bug fixes (2026-03-14)
- Phase 1: Session continuity — RX1, R14C (shipped)
- Phase 1B: Anchored memory — AM-1->AM-5 (2026-03-17)
- Phase 2: Graph accuracy — FIX-PARSER-2, R4 (cross-file Python/Java), IMP-EVAL-1 (49 parsers) (2026-03-17)
- UX-1: Indexing progress feedback (2026-03-17)
- Proactive Context Engine: Phase 1-6 — federation, drift detection, dependency tracking, brain enhancement, tool discoverability, component pipeline (2026-03-18/19)
- Context delivery improvements: IMP-IMPL-1, IMP-IMPL-2, BUG-EVAL-20 (2026-03-18)
- Sprint 1: 12 critical bug fixes (2026-03-19)
- Sprint 2: Stability & robustness hardening (2026-03-19)
- Sprint 3: Incremental graph persistence, RWMutex cache, pattern bounds, tool pre-tokenization (2026-03-19)
- Sprint 4: Proactive rule alerts, brain hint in prepare_context (2026-03-19)
- Sprint 5: Commit-to-task linking, branch-aware context, session work summary (2026-03-19)
- Sprint 6: REST/HTTP API, bearer token auth, resolver refactor, handler split, edge type catalog, dual-DB split, context instrumentation, vector storage, local embeddings (2026-03-19)
- Sprint 7: Security & Safety — 15 items (path traversal, FTS5 sanitization, CORS, input limits, plugin opt-in, goroutine lifecycle, rate limiting, federation ACLs) (2026-03-20)
- Sprint 8: Developer Experience & Reliability — 21 items (tool surface reduction, config example, structured logging, SQLite corruption detection, scheduled pruning) (2026-03-20)
- Sprint 9: Test Coverage & Code Quality — 8 items (REST API tests, dual-DB migration tests, embedder smoke test, graceful shutdown tests) (2026-03-20)
- Sprint 10: Temporal Knowledge & Quad-Retrieval — 10 items (memory versioning, knowledge decay scoring, fact lifecycle events, entity history timeline, temporal cross-domain queries, quad-channel recall engine, graph-anchored embedding invalidation, multi-hop knowledge traversal, vector search scaling, embedding pipeline concurrency) (2026-03-21)
- Sprint 11: Performance & Accuracy Foundations — 11 items (SQLite reader/writer pool, performance pragmas, SIMD cosine, subgraph cache tuning, ACT-R decay, differential tier decay, sandwich ordering, semantic dedup, tree-sitter error recovery, heritage clause extraction, dynamic token budget) (2026-03-22)
- Sprint 12: Embedding & Retrieval Revolution — 8 items (nomic-embed spike + upgrade, ANN spike + HNSW, semantic tool discovery, spreading activation, memory admission control, score-aware fusion) (2026-03-22)

**Alpha launch: Monday 2026-03-24.** Sprints 7-9 (Security + DX + Tests) are the gate — all shipped.

---

## Sprint Sequence

```
  Sprint 11: Performance & Accuracy Foundations   ✅ SHIPPED (2026-03-22)
  Sprint 12: Embedding & Retrieval Revolution     ✅ SHIPPED (2026-03-22)
  Sprint 13-pre: Operational Fixes ............. ✅ SHIPPED (2026-03-23)
  Sprint 13: Graph Intelligence ............... ✅ SHIPPED (2026-03-24)
  Sprint 14: Analysis Depth & Domain Expansion .. 8 tasks — Accuracy + Speed (better graph inputs)
  Sprint 15: Self-Refining Intelligence ......... 9 tasks — Accuracy (feedback loop)
  Sprint 16: Cross-Domain Knowledge Graph ....... 7 tasks — Accuracy (killer feature)
  Sprint 17: Skills, Workflows & Intelligence ... 6 tasks — Speed + Accuracy (compound operations)
  Sprint 18: Observability, Proof & Polish ...... 8 tasks — Speed + Privacy (demonstrate value)
                                                  ─────
                                                  38 tasks remaining → 8 parallel waves
```

**Sequencing rationale:** Sprint 11 tunes the engine — every subsequent sprint benefits from faster SQLite, smarter decay, and better cache hit rates. Sprint 12 upgrades the embedding model and vector search — the two single-point improvements with the highest absolute impact. Sprint 13 transforms graph traversal with PPR and hybrid scoring — this is the algorithmic leap. Sprint 14 improves what goes INTO the graph (type propagation, incremental reanalysis) and expands to new domains. Sprint 15 closes the feedback loop so the graph and retrieval self-improve. Sprints 16-18 build on all prior intelligence to deliver cross-domain knowledge, workflow automation, and provable value.

---

## Parallel Execution Plan (2 Laptops)

**How it works:** Each wave runs 2 Claude Code sessions on 2 laptops, each on a separate git branch. Tasks within a wave touch **different files** — no merge conflicts. When both finish, merge both branches into `main`, pull on both laptops, start next wave.

**45 remaining tasks** across 9 waves. Each wave maximizes parallelism while respecting dependencies — later waves build on merged code from earlier waves.

```
Wave 1 ──┬── Laptop A: 14.1 + 14.2 (type propagation + RTA refinement)
          └── Laptop B: 13.4 + 13.5 (eigenvector centrality + adaptive decay)
          ── merge → main ──

Wave 2 ──┬── Laptop A: 13.1 + 13.2 (PPR spike + PPR implementation)
          └── Laptop B: 14.3 + 14.4 (dep-aware reanalysis + parallel parsing)
          ── merge → main ──

Wave 3 ──┬── Laptop A: 13.3 + 13.7 (hybrid scoring + interface implementor)
          └── Laptop B: 14.6 + 14.7 (Terraform + OpenAPI parsers)
          ── merge → main ──

Wave 4 ──┬── Laptop A: 13.6 (context-weighted recall)
          └── Laptop B: 14.5 + 14.8 + 15.9 (manual linker + federation parallel + typed interfaces)
          ── merge → main ──

Wave 5 ──┬── Laptop A: 15.1 + 15.2 (outcome signals + quality score)
          └── Laptop B: 15.7 + 16.1 (path-pattern rules + cross-domain edge types)
          ── merge → main ──

Wave 6 ──┬── Laptop A: 15.3 + 15.4 (weight refinement + channel weight learning)
          └── Laptop B: 16.2 + 16.3 (cross-domain name matching + edge confirmation)
          ── merge → main ──

Wave 7 ──┬── Laptop A: 15.5 + 15.6 + 15.8 (session report + confidence scoring + benchmark harness)
          └── Laptop B: 16.4 + 16.5 + 16.6 (multi-domain BFS/PPR + cross-domain impact + KG stats)
          ── merge → main ──

Wave 8 ──┬── Laptop A: 17.1 + 17.2 + 17.3 (docs graph + skills phase 1 + code review flow)
          └── Laptop B: 16.7 + 17.4 + 17.5 + 17.6 (raw graph query + HyDE search + progressive disclosure + compression)
          ── merge → main ──

Wave 9 ──┬── Laptop A: 18.1 + 18.2 + 18.3 + 18.4 (dashboard + wow moment + token savings + health endpoint)
          └── Laptop B: 18.5 + 18.6 + 18.7 + 18.8 (knowledge export + benchmarks + ONNX integrity + brain integrity)
          ── merge → main ──
```

### Wave dependency chain

| Wave | Depends on | Why |
|------|-----------|-----|
| 1 | — | Foundation: better graph inputs (A) + graph scoring improvements (B). Independent subsystems. |
| 2 | Wave 1 | PPR builds on centrality scores (13.4). Parallel parsing touches watcher — needs 14.1/14.2 merged. |
| 3 | Wave 2 | Hybrid scoring needs PPR (13.2) + embeddings (Sprint 12 shipped). New parsers are independent. |
| 4 | Wave 3 | Context-weighted recall uses PPR scores. Manual linker needs edge types from parsers. |
| 5 | Wave 4 | Outcome signals need full intelligence stack in place. Path-pattern rules are independent. |
| 6 | Wave 5 | Weight refinement needs outcome signals (15.1). Cross-domain matching needs parsers (14.6/14.7). |
| 7 | Wave 6 | Reports need quality scores (15.2). Multi-domain BFS needs edge types (16.1) + PPR (13.2). |
| 8 | Wave 7 | Skills/docs compose existing tools. HyDE needs embeddings. Progressive disclosure needs stable session_init. |
| 9 | Wave 8 | Dashboard needs Sprint 15 data. Benchmarks need full intelligence stack. |

### File separation per wave (no merge conflicts)

| Wave | Laptop A files | Laptop B files |
|------|---------------|----------------|
| 1 | `internal/parser/python.go`, `internal/parser/java.go`, `internal/resolver/*` | `internal/graph/index.go`, `internal/graph/traverse.go` |
| 2 | `internal/graph/traverse.go` (PPR), `internal/graph/ppr.go` | `internal/watcher/*.go`, `internal/resolver/*` |
| 3 | `internal/graph/carve*.go`, `internal/graph/traverse.go` | `internal/parser/terraform.go`, `internal/parser/openapi.go` |
| 4 | `internal/store/recall_engine.go` | `internal/mcp/tools.go` (linker), `internal/federation/*`, `internal/mcp/server.go` |
| 5 | `internal/mcp/context_signal*.go` (new) | `internal/store/rules.go`, `internal/graph/edge_types.go` |
| 6 | `internal/graph/weights.go`, `internal/store/recall_engine.go` | `internal/graph/cross_domain*.go` (new) |
| 7 | `internal/mcp/session_report.go`, `cmd/synapses/benchmark*` | `internal/graph/traverse.go` (cross-domain), `internal/mcp/tools.go` |
| 8 | `internal/parser/markdown.go`, `internal/mcp/skills*` | `internal/store/search.go`, `internal/mcp/tools.go` (session_init) |
| 9 | `internal/mcp/dashboard*.go`, `cmd/synapses/health.go` | `internal/store/export.go`, `cmd/synapses/benchmark*` |

---

## Sprint 13-pre: Operational Fixes

**Goal:** Fix 4 logic bugs discovered during health-check on 2026-03-23 that affect agent UX and correctness. All Low effort. Must ship before Sprint 13 algorithmic work begins.

| # | Item | What | Source | Effort | Value |
|---|------|------|--------|--------|-------|
| ✅ 1 | **validate_plan safety timeout** | Fixed: capture `timedOut` via `errors.Is(DeadlineExceeded)` BEFORE `cancel()`. Added `HasNoFailureEpisodes()` fast EXISTS check. Status values: `clear`, `clear_no_data`, `timeout`, `warning`. Commit: `5537de9`. | Health check 2026-03-23 | Low | Speed |
| ✅ 2 | **File-change event edge count misleading** | Fixed: `edges_added` now uses `OutEdgesForFile(path)` (file-scoped) instead of global `EdgeCount()` delta. Commit: `52982aa`. | Health check 2026-03-23 | Low | Accuracy |
| ✅ 3 | **explain_codebase classifies docs/ as [core logic]** | Fixed: added 8 new path-keyword cases to `detectLayerLabel()` covering `[documentation]`, `[tooling/infra]`, `[data model]`, `[utilities]`, `[security]`, `[caching]`, `[parser]`. Commit: `52982aa`. | Health check 2026-03-23 | Low | Accuracy |
| ✅ 4 | **Git blame stale after commit** | Fixed: watcher now watches `.git/` dir; on `COMMIT_EDITMSG` write, calls `refreshBlameAfterCommit()` which re-enriches blame for all files changed in last 5 min. Commit: `52982aa`. | Health check 2026-03-23 | Low | Accuracy |

---

## Sprint 13: Graph Intelligence

**Goal:** Transform `get_context()` from "good BFS" to "the most relevant code context on earth." Personalized PageRank replaces the fundamental traversal strategy — capturing multi-path importance that BFS structurally cannot represent. Semantic-structural hybrid scoring bridges the graph and embedding systems that currently operate in isolation. Eigenvector centrality gives architecturally important nodes a persistent boost. After this sprint, Synapses delivers context that is both structurally and semantically optimal.

**Research Council context:** LEGO-GraphRAG (VLDB 2025) established PPR as "the current optimal solution for structure-based extraction." CodexGraph (NAACL 2025) showed structural-only retrieval misses semantically related code beyond the hop boundary. These are the two most-cited findings across the council.

| # | Item | What | Source | Effort | Value |
|---|------|------|--------|--------|-------|
| ~~1~~ | ~~**Spike: PPR vs BFS benchmark**~~ | ~~Pick 20 entities across 3 real codebases. Compare PPR vs tuned BFS context quality via LLM judge (Claude evaluates which context is more useful for a task). Validate that PPR's multi-path scoring outperforms BFS's max-score heuristic on hub-heavy graphs.~~ | ~~RC: Graph #1~~ | ~~Low~~ | ~~—~~ |
| ~~2~~ | ~~**Personalized PageRank**~~ | ~~Add `PersonalizedPageRank()` method in `traverse.go`. Power iteration: initialize `rank[root]=1.0`, iterate `rank[n] = α×teleport[n] + (1-α)×Σ(rank[neighbor]×edgeWeight/outDegree)` until convergence. ~30 lines. Reuses existing CSR arrays and edge weights. Add `cfg.UsePPR` flag. `DirectionBoost` maps naturally to transition probability bias.~~ | ~~RC: Graph #1~~ | ~~Medium~~ | ~~Accuracy~~ |
| ~~3~~ | ~~**Semantic-structural hybrid scoring**~~ | ~~Blend structural BFS/PPR scores with node embedding cosine similarity: `finalScore = (1-λ)×structural + λ×cosineSim(embed(root), embed(n))`. The `node_embeddings` table already exists but is NEVER used during ego-graph carving — this bridges that gap. `CarveConfig.EmbeddingLookup func(NodeID) []float32`. Batch-load via `IN(...)` SQL. Lambda=0 fallback when embeddings unavailable.~~ | ~~RC: Graph #2~~ | ~~Medium~~ | ~~Accuracy~~ |
| ~~4~~ | ~~**Precomputed eigenvector centrality**~~ | ~~Compute during `GraphIndex.Build()` using power iteration on adjacency matrix (~40 lines). Store as `EigenvectorCentrality []float64` in GraphIndex. Multiply into traversal scores: `relevance × (1 + 0.2 × centrality)`. Architecturally important nodes (connected to other important nodes) get a persistent boost. O(iterations × edges) at build time — <10ms for 16K edges.~~ | ~~RC: Graph #4~~ | ~~Low~~ | ~~Accuracy~~ |
| ~~5~~ | ~~**Adaptive decay by local graph density**~~ | ~~`localDecay = decay × (1/(1+log2(outDegree+1)))`. High-degree hub nodes (like `Store.Close` with 96 callers) decay children faster, preventing hub explosion. Low-degree nodes in narrow call chains decay slower, allowing deeper traversal. Inspired by GCN degree-normalized message passing (Kipf & Welling, ICLR 2017). 5-line change in BFS inner loop.~~ | ~~RC: Graph #6~~ | ~~Low~~ | ~~Accuracy~~ |
| ~~6~~ | ~~**Context-weighted recall**~~ | ~~When `agent_id` is provided in `recall()`, fetch current session state: declared intent, active task title, recent files. Construct enriched query for BM25/semantic channels: `original_query + intent_keywords + task_title`. Graph channel uses original query (operates on structure, not text). An agent working on "auth" who asks "how does caching work?" sees auth-caching memories boosted. Encoding specificity principle from cognitive science. [Brain-enhanced when available]~~ | ~~RC: Memory #9~~ | ~~Medium~~ | ~~Accuracy~~ |
| ~~7~~ | ~~**Multi-seed interface implementor expansion**~~ | ~~When root is an interface, seed BFS/PPR with concrete implementors (reverse IMPLEMENTS edges) at relevance 0.85. Currently, querying an interface like `Store` does NOT show which types implement it unless they happen to be within 2 hops via other edges. Also: replace O(N) linear scan for struct methods with `GraphIndex.nameIndex` O(1) lookup.~~ | ~~RC: Graph #5~~ | ~~Low~~ | ~~Accuracy~~ |

**End state after Sprint 13:** Context delivery uses PPR (multi-path importance) + semantic-structural hybrid scoring + eigenvector centrality priors + adaptive density decay + interface implementor expansion. This is a generational leap from the current max-score BFS heuristic.

---

## Sprint 14: Analysis Depth & Domain Expansion

**Goal:** Improve what goes INTO the graph — more resolved edges, faster incremental updates, new domains. Type propagation for Python/Java closes the accuracy gap with Go/TypeScript. Dependency-aware reanalysis makes file saves 10-50x faster. Parallel parsing handles branch switches without blocking agents. New domain parsers (Terraform, OpenAPI) create the entities that Sprint 16's cross-domain features build on.

**Research Council context:** The Program Analysis Researcher found that Python/Java rely entirely on name-based call resolution — extracting function parameter types and `self.attr` types from tree-sitter ASTs is low-hanging fruit that improves CALLS edge resolution by 15-30%.

| # | Item | What | Source | Effort | Value |
|---|------|------|--------|--------|-------|
| ~~1~~ | ~~**Lightweight type propagation**~~ | ~~Extend `collectPythonVarTypes` with function parameter type annotations (`def process(repo: Repository)` → record `repo`→`Repository`). Add `collectSelfAttrTypes`: walk `__init__` for `self.attr = ClassName(...)`. For Java/Kotlin: extract method return types and propagate to call sites. Key by `(file, scope, varname)`. Estimated 15-30% more resolved CALLS edges for typed Python/Java codebases.~~ | ~~RC: Prog Analysis #3~~ | ~~Medium~~ | ~~Accuracy~~ |
| ~~2~~ | ~~**RTA-style call graph refinement**~~ | ~~Collect constructor calls / `new` expressions during parsing into `instantiatedTypes` set. In `findInPackage`, prefer candidates whose receiver type is instantiated. For interface dispatch, only emit IMPLEMENTS edges for instantiated types. Reduces false-positive CALLS edges by 10-40% in Java/TypeScript with deep class hierarchies.~~ | ~~RC: Prog Analysis #4~~ | ~~Medium~~ | ~~Accuracy~~ |
| **[IN PROGRESS]** 3 | **Dependency-aware incremental reanalysis** | Add `import_edges` table tracking (file → imported_package). On file change, compute invalidation set: changed file + transitive importers. Only drain/re-resolve call sites for files in invalidation set. Add `file_edge_index map[string][]*Edge` for O(edges_touching_file) violation checks instead of O(all_edges). 10-50x reduction in per-save resolver work. | RC: Prog Analysis #1 | Medium | Speed |
| 4 | **Parallel file parsing** | Split `reparseFile` into `parseFile` (parallelizable, CPU-bound) and `mergeAndResolve` (serialized). `reparseMu` protects only the merge step. 4-8 files parsed concurrently. Batch coalescing: if >3 files pending simultaneously, wait 50ms to merge into single resolve pass. Branch switch of 20 files: 1.8s → ~0.5s. | RC: Performance #5 | Medium | Speed |
| 5 | **Manual entity linker** (OF-H6) | `link_entities(a, b, relation, domain="code-to-infra")` — user/agent creates custom cross-domain edges. `domain` field tracks explicit vs discovered. Highest strategic leverage — enables cross-domain edges immediately without automatic parsers. | Original Sprint 11 | Low | Accuracy |
| ✅ 6 | **Terraform/k8s parser** | Declarative infrastructure parser. Resources as nodes, DEPENDS_ON edges. Closest to code — explicit references, well-defined HCL schema. | Original Sprint 11 | Medium | Accuracy |
| **[IN PROGRESS]** 7 | **OpenAPI/GraphQL parser** | API schema parser. Endpoints/operations as nodes, request/response type edges. Schema IS a graph — natural fit. | Original Sprint 11 | Medium | Accuracy |
| 8 | **Federation parallel sibling queries** | Replace sequential iteration in `FindEntities` and `CheckDrift` with bounded `errgroup.SetLimit(8)`. At 20+ federated projects, 2-5s → ~500ms. | Original Sprint 11 | Low | Speed |

**End state after Sprint 14:** Graph edges are 15-40% more accurate for Python/Java. File saves resolve 10-50x faster. Branch switches parse in parallel. Terraform and OpenAPI entities exist as first-class graph citizens.

---

## Sprint 15: Self-Refining Intelligence

**Goal:** Close the feedback loop. Measure whether context delivery actually helps agents. Refine graph weights and retrieval channel weights based on what works. After this sprint, the graph reshapes itself based on agent task outcomes — not just usage patterns (Cognee) but actual success signals.

**Data foundation:** Context instrumentation (Sprint 6) and fact lifecycle events (Sprint 11) have been collecting data for weeks. The feedback loop starts with real signal.

| # | Item | What | Source | Effort | Value |
|---|------|------|--------|--------|-------|
| 1 | **Context outcome signal analysis** | Correlate context deliveries with task outcomes. Signal quality discipline: task completion = weak positive. Same-entity re-fetch within 5 min = moderate negative. Task abandoned = strong negative. Re-fetch after 30+ min = neutral (new subtask). | Original Sprint 12 | Medium | Accuracy |
| 2 | **Per-entity context quality score** | Aggregate weighted signals into per-entity quality score. High-quality entities surface higher in BFS/PPR. Low-quality entities flagged for review. Self-improving. | Original Sprint 12 | Medium | Accuracy |
| 3 | **BFS/PPR weight refinement** | Edges traversed during successful completions get +0.1 weight boost (cap 2.0x). Abandoned tasks get -0.05 penalty (floor 0.3x). Edges untouched 30+ days get `dormant=true`. Runs after `end_session`. **The graph reshapes itself based on what helps agents.** | Original Sprint 12 | Medium | Accuracy |
| 4 | **Recall channel weight learning** | Track which of 4 channels contributed the top result per successful `recall()`. Per-project weights diverge from default (0.25 each). Code-heavy projects: graph=0.4, semantic=0.3. Knowledge-mode: semantic=0.5, temporal=0.3. Self-tuning retrieval. | Original Sprint 12 | Low | Accuracy |
| 5 | **Session effectiveness report** | At `end_session`: context hit rate, task completion rate, total tokens, comparison to previous sessions. Store for Tauri dashboard. "Your agents completed 14/16 tasks with first-fetch context." | Original Sprint 12 | Low | Speed |
| 6 | **Context confidence scoring** | `get_context` includes `confidence: 0.87` based on quality score + staleness + session history. Low confidence adds hint. **Constraint**: confidence < 0.5 adds hint but does NOT suppress result. Agents decide, Synapses informs. | Original Sprint 12 | Low | Accuracy |
| 7 | **Path-pattern architectural rules** | Extend `ForbiddenEdge` with `path_pattern []EdgeType` field. When present, run bounded BFS from from-matching nodes and check if to-matching nodes are reachable via the specified edge sequence. Enables multi-hop constraints: "no handler→database path without service layer." Covers 80% of architectural rule expressiveness without a full Datalog engine. Reuses existing BFS infrastructure. | RC: Prog Analysis #2 | Medium | Accuracy |
| 8 | **Benchmark harness** | New tool: `benchmark(scenario="auth-refactor")`. Pre-defined scenarios measuring completeness, precision, latency. Ships with 3-5 built-in scenarios. Focus on measurable structural properties. | Original Sprint 12 | High | Accuracy |
| 9 | **Typed interfaces for Server fields** | Replace `interface{}` fields (`brainClient`, `pulseClient`, etc.) with narrow typed interfaces. Compile-time safety. Natural home here since we're modifying server struct behavior for items 1-7. | Original Sprint 12 | Medium | — |

**End user impact:** "Synapses saved you 47,000 tokens this session. Context accuracy: 87%. 14/16 tasks completed with first-fetch context. Recall precision improved 12% since last week."

**Why stronger than Cognee:** Cognee refines after ingestion (Memify step). Synapses refines after USAGE. Cognee's refinement is structural heuristics. Synapses' refinement is actual agent task outcomes. Outcome-driven > usage-driven.

---

## Sprint 16: Cross-Domain Knowledge Graph

**Goal:** Connect entities across domains. Traverse from a Go function to its Terraform deployment to its API specification in a single BFS/PPR query. The killer feature no competitor has. Enhanced by Sprint 13's PPR (multi-path traversal across domain boundaries) and Sprint 14's domain parsers (Terraform, OpenAPI entities).

| # | Item | What | Source | Effort | Value |
|---|------|------|--------|--------|-------|
| 1 | **Cross-domain edge types** | MENTIONS (name match), DEPLOYS (code→infra), CONSUMES (code→API), DOCUMENTS (docs→code), CONFIGURED_BY (code→config). Register in EdgeTypeDescriptor catalog. | Original Sprint 14 | Low | Accuracy |
| 2 | **Context-aware name matching** | Background process: after reindex, scan for entity name matches across domains. Context-aware: candidates must share context signal (same directory, co-occurring in config, referenced in same task/memory). Confidence score 0.0-1.0, minimum 0.6 for auto-creation. Case normalization (kebab/camel/snake equivalence). **Accuracy over recall** — false positive edges erode trust faster than missing edges. [Brain-enhanced when available — LLM validates match semantics] | Original Sprint 14 | Medium | Accuracy |
| 3 | **Cross-domain edge confirmation** | `confirm_edge(edge_id, confirmed=true/false)`. Confirmed → confidence 1.0. Rejected → suppressed permanently. Feeds back into matching heuristics. Human-in-the-loop safety net. | Original Sprint 14 | Low | Accuracy |
| 4 | **Multi-domain BFS/PPR in get_context** | Cross-domain edges traversed with configurable decay (default 0.5x). Return cross-domain neighbors in separate `cross_domain` bucket. PPR naturally handles cross-domain traversal — no special handling needed beyond edge weight configuration. | Original Sprint 14 | Medium | Accuracy |
| 5 | **Cross-domain impact analysis** | `get_impact` follows cross-domain edges. "What breaks if I change PaymentService?" → code callers + Terraform resources + API specs + docs. **The killer feature.** Only follows confidence >= 0.6 or confirmed. | Original Sprint 14 | Medium | Accuracy |
| 6 | **Knowledge graph stats in session_init** | `knowledge_graph` section: total entities by domain, cross-domain edges (auto/confirmed/manual), active domains, freshness. Agents self-calibrate. | Original Sprint 14 | Low | Speed |
| 7 | **Raw graph query tool** (B31) | Constrained DSL: `NODES WHERE package="auth" AND fanin > 5`. Read-only, 1000 node cap, 500ms timeout. Power user tool for cross-domain exploration. | Original Sprint 11 | Medium | Speed |

**Precision safeguard:** False positive edges in item 2 amplify into false blast-radius results in item 5. Context-aware matching (2), edge confirmation (3), and confidence threshold (5) are defense-in-depth.

---

## Sprint 17: Skills, Workflows & Intelligence

**Goal:** Compound operations over existing tools. Pre-built flows for common tasks. Brain-enhanced search. Progressive disclosure to reduce boot tokens. This sprint turns the intelligence built in Sprints 11-16 into one-call solutions that save agents 3-5 tool calls per workflow.

**Synapses Intelligence integration:** This is where the local brain (llama-server) earns its keep beyond simple enrichment. HyDE uses the brain to generate hypothetical code answers for dramatically better semantic search on abstract queries. Progressive disclosure reduces boot context by 4-6x, making room for the intelligence that matters.

| # | Item | What | Source | Effort | Value |
|---|------|------|--------|--------|-------|
| 1 | **Documentation graph flow** (OF-F4) | Markdown headings as nodes, cross-references as edges, code entity MENTIONS links. Makes docs queryable via `get_context` and `search`. Enables Sprint 16's cross-domain BFS to traverse into documentation. **Most defensible item** — every AI tool generates docs, none make them queryable through a knowledge graph. | Original Sprint 13 | Medium | Accuracy |
| 2 | **Skills Phase 1 — Context recipes** (R18) | Named context bundles: `skill: "auth-flow"` returns pre-defined entity set. Recipe schema supports `projects: "*"` for cross-domain recipes like `skill: "weekly-summary"`. One call instead of 5. | Original Sprint 13 | Medium | Speed |
| 3 | **Code review flow** (OF-F1) | Automated review: `get_context` → `get_impact` → `get_violations` → structured review report. High-value code-domain skill demonstrating the skills engine. | Original Sprint 13 | Medium | Speed |
| 4 | **HyDE-enhanced semantic search** | When `brainClient` is available and `mode="semantic"`: brain generates a hypothetical function/type definition matching the query intent (~200 tokens, 500ms timeout). Embed the hypothesis, search against that instead of raw query. Bridges abstract intent queries ("how does rate limiting work") to code-style entities. 49% retrieval accuracy improvement per Anthropic research. `hyde=false` parameter for exact-name queries. [Brain-enhanced] | RC: Context #4 | Medium | Accuracy |
| 5 | **Progressive disclosure in session_init** | Change default `scope` from `"full"` to `"standard"` (new tier). Standard includes: pending tasks, working state, scale guidance, safety alerts (~500 tokens). Novel items (project identity when changed, events when count > 0) included automatically. Full sidecar/brain/constitution only in `scope="full"`. `more_available` key lists deferred items. Backward-compatible. | RC: Context #6 | Low | Speed |
| 6 | **Tool result compression** | Add `_summary` field (50-100 tokens) to `session_init`, `validate_plan`, `verify_implementation`, `search` responses. Template-based — no LLM needed. Example: `"_summary": "Plan OK: 0 violations, 0 logic warnings. Safety check: clear."` Enables agent/framework context compaction. | RC: Context #7 | Low | Speed |

---

## Sprint 18: Observability, Proof & Polish

**Goal:** Make Synapses' value visible, provable, and secure in its supply chain. Run benchmarks that demonstrate the impact of Sprints 11-17's intelligence improvements. Address remaining integrity verification for model and binary downloads.

| # | Item | What | Source | Effort | Value |
|---|------|------|--------|--------|-------|
| 1 | **Productivity dashboard data** | Aggregate metrics via REST API: sessions/day, task completion rate, context quality trend, token savings, knowledge growth. All data already in SQLite from Sprint 15 instrumentation. | Original Sprint 15 | Medium | Speed |
| 2 | **First-session "wow" moment** | On first `session_init`, include `first_session_highlights`: dead code (0 callers + 0 tests), likely-unintended dependencies, highest-risk entities (high fanin + no tests + recent changes), architectural violations. Surprising findings that demonstrate value immediately. | Original Sprint 15 | Low | Speed |
| 3 | **Token savings estimator** | Compare context delivery tokens to realistic baseline (Grep-based discovery + full file reads, ~3 tokens/line). Report delta in `end_session` + Tauri dashboard. **Speed** value made tangible. | Original Sprint 15 | Low | Speed |
| 4 | **Health endpoint** | `GET /v1/health` — daemon uptime, project count, graph sizes, memory counts, federation status, brain availability, last index time, error count, embedding status (ready/downloading/disabled), subsystem health (watcher alive, embedding queue depth, DB size). | Original Sprint 15 | Low | Speed |
| 5 | **Knowledge export/import** | `export_knowledge(format="json")` and `import_knowledge(path="...")`. Reduces lock-in anxiety. Knowledge backup. Manual team sync. **Privacy** value — user controls data completely. | Original Sprint 15 | Medium | Privacy |
| 6 | **Tiered benchmark strategy** | **Tier 1**: LongMemEval-compatible on 3 OSS repos. Target 85-92% (with Sprint 12's retrieval upgrades). **Tier 2**: CodeMemBench — impact-aware recall, staleness detection, cross-domain traversal. Plays to unique strengths. **Tier 3**: Ablation study — PPR vs BFS, HNSW vs brute-force, nomic vs MiniLM, ACT-R vs simple decay. Each ablation is publishable. | Original Sprint 15 | Medium | Accuracy |
| 7 | **ONNX model integrity verification** | Pin expected SHA-256 hash for embedding model (nomic-embed by this point). Verify after download before loading into ONNX runtime. Reject on mismatch. | Original Sprint 15 | Low | Privacy |
| 8 | **Brain binary integrity verification** | SHA-256 verification for llama-server binaries. Add `io.LimitReader` size cap on download. Verify hash before executing. | Original Sprint 15 | Low | Privacy |

**Why benchmark at Sprint 18:** Requires PPR (Sprint 13), HNSW (Sprint 12), nomic-embed (Sprint 12), outcome signals (Sprint 15), and accumulated data. Running benchmarks earlier would benchmark an incomplete system. Running here benchmarks the full intelligence stack.

---

## Backlog — Build When Needed

### A2A & Multi-Protocol Access [Trigger-Based]

| Item | Trigger |
|------|---------|
| A2A agent card | When A2A adoption reaches critical mass |
| A2A task reception | When agent card generates inbound interest |
| OpenAI-compatible context API (OF-P2) | When non-MCP agent users request access |
| Protocol abstraction layer | When 3+ protocol adapters exist |

### Research Council — Deferred Items

Items evaluated by the Research Council and deliberately deferred. High effort, speculative, or blocked on prior sprint validation.

| Item | Trigger | Source |
|------|---------|--------|
| Full Datalog rule engine (Phase 2) | When path-pattern rules (Sprint 15 #7) prove insufficient | RC: Prog Analysis #2 |
| Cross-language LSP integration | When dedicated type-checked resolvers prove insufficient for Python/Java | RC: Prog Analysis #5 |
| Episodic-to-semantic consolidation pipeline | When memory corpora routinely exceed 5K entries | RC: Memory #4 |
| Causal/temporal memory graph layers (MAGMA) | When simpler improvements (ACT-R, tier decay) are validated | RC: Memory #7 |
| Contextual retrieval with chunk-level descriptions | When HyDE and query expansion (Sprint 17 #4) are validated | RC: Context #9 |
| Community-aware context boundaries | When user complaints about cross-module noise surface | RC: Graph #7 |
| Integer quantization for embeddings | When HNSW decision (Sprint 12) makes this moot or necessary | RC: IR #7 |
| Batch embedding pipeline parallelism | When memory corpus exceeds 10K per project | RC: IR #6 |
| Per-project lazy loading in daemon | When 10+ simultaneous projects is a real user scenario | RC: Performance #7 |
| BFS allocation pooling (sync.Pool) | Nice-to-have during any BFS refactor, not standalone | RC: Performance #6 |
| Workflow recipe server-side execution | When tool-call latency becomes documented user pain point | RC: Context #8 |

### Other Backlog Items

| Item | Trigger |
|------|---------|
| MCP Streamable HTTP (B27) | When spec is finalized (targeting June 2026) |
| Background mutation analysis (MUT-1) | When skills Phase 1 is stable |
| SQL parser | When SQL-heavy project users request it |
| Protobuf parser | When gRPC-heavy project users request it |
| Skills Phase 2 — Context hooks (B14-B22) | When R18 Phase 1 is stable |
| WASM sandbox for skills (OF-S1) | When third-party skills ship |
| Cross-encoder reranker for recall | When Sprint 12's retrieval pipeline is stable (+5-10% precision) |
| Sequential pattern mining → bundles (R30) | When intent alignment has meaningful volume |
| Built-in task scheduler (OF-E1) | When scheduled jobs are needed |
| Token budget tracking (OF-E2) | When team/multi-agent billing matters |
| Agent registry + manifests (OF-E5) | When multi-agent delegation is real |
| Capability-based RBAC (OF-S6) | When multi-tenant scenario materializes |
| Merkle audit trail (OF-S3) | When enterprise compliance is needed |
| MCP auth hardening (B30) | When HTTP transport ships |
| Chunk-level access control (BL-1) | When multi-tenant path is defined |
| Semantic conflict detection (BL-2) | When contradictory agent beliefs are measured |
| External data connectors (OF-H2) | When domain parsers ship |
| Notification channels (OF-P3) | When event push to Slack/Discord is demanded |
| Config-code schema consistency (BRAIN-2) | When domain parser for config files exists |
| R13 (trace_concept) | When quad-retrieval + PPR are stable |
| Server god-object decomposition | When server struct exceeds 50 fields or 15 mutexes |
| Embedding pipeline pooling (advanced) | When concurrent sessions exceed 50 |

---

## Deferred — Won't Build

Items evaluated and deliberately excluded. See `docs/DEFERRED.md` for detailed rationale.

- R25 (ML context budget), R26 (hierarchical summarization), R27 (semantic dedup) — too early
- B1 (agent time travel), B2 (full conversation persistence), B12 (sandbox) — wrong layer
- B5 (adaptive multi-round LLM), B8 (cost-aware context), B9 (agent profiles) — redundant
- B10 (LoRA adapters), B11 (KV cache) — unproven value
- B23 (Windows ARM64), B24 (Roo-Code integration) — no demand
- R17 (structural fingerprint cache) — narrow scenario, overhead offsets benefit
- Sprint 4 #1 (session delta), #2+#3 (capability listing + brain health) — already mostly shipped
- DIAG-1 (diagnose_graph), DIAG-2 (dead code detection) — debugging tools, not daily drivers
- R35 (field-level annotations) — niche, not observed gap
- Sprint 4 #10 (session_init token budget) — scope param already covers this
- Multi-LLM provider routing (OF-E4) — duplicates agent runtime responsibility
- Work claim enforcement — git hook (B6) — solved by Claude Code Agent Teams
- Intent alignment metrics (R29) — absorbed into Sprint 15

---

## The 8 Killer Differentiators

1. **Quad-retrieval with AST graph advantage** — 4 channels (graph + BM25 + semantic + temporal) merged with RRF + score-aware fusion. Graph channel uses spreading activation on REAL typed edges from AST parsing. Benchmark target: 85-92% LongMemEval. (Sprint 12)

2. **Personalized PageRank + semantic-structural hybrid** — PPR captures multi-path importance that no BFS can represent. Hybrid scoring bridges structural proximity with semantic similarity. No competitor uses PPR for code context delivery. (Sprint 13)

3. **Cognitive science-grade memory** — ACT-R frequency-weighted power-law decay, tier-specific half-lives, multi-dimensional admission control. The memory system models how human memory actually works — frequently recalled knowledge stays accessible, session logs fade, architectural decisions persist. (Sprint 11-12)

4. **Entity-anchored temporal knowledge with auto-staleness** — Memory versioning + decay + entity-anchored staleness + embedding invalidation. Nobody auto-invalidates when code changes. (Shipped + Sprint 11)

5. **Self-refining graph and retrieval** — BFS/PPR edge weights adjust based on task outcomes. Recall channel weights learn per-project. Path-pattern rules enforce multi-hop architectural constraints. The graph reshapes itself. (Sprint 15)

6. **Cross-domain impact analysis with precision safeguards** — "What breaks if I change this function?" returns code callers + Terraform + API specs + docs. Context-aware matching + edge confirmation prevent false positives. (Sprint 16)

7. **Local-first cross-domain knowledge substrate** — Privacy-preserving (EU AI Act compliant by architecture). nomic-embed-text-v1.5 runs locally via ONNX. HNSW runs in-process. Brain runs as local llama-server. Zero cloud dependency, ever. (Architecture)

8. **Intelligence-enhanced retrieval** — HyDE uses local brain to bridge intent-style queries to code entities. Context-weighted recall uses session awareness. The brain makes everything smarter when available, everything works without it. (Sprint 17)

---

## How This Maps to the 3 Values

| Value | Sprint Impact |
|-------|--------------|
| **Speed** | Sprint 11 (SQLite 4-8x reads, SIMD 3-5x cosine, cache 2-4x hits, dynamic token budgets), Sprint 12 (HNSW 50-100x vector search at scale, semantic tool discovery), Sprint 13 (PPR convergence faster than deep BFS on hub-heavy graphs), Sprint 14 (incremental reanalysis 10-50x, parallel parsing), Sprint 17 (one-call skills, progressive disclosure 4-6x fewer boot tokens, _summary compression), Sprint 18 (token savings proof, health endpoint) |
| **Privacy** | Architecture: all computation local. Sprint 12: nomic-embed runs local ONNX, HNSW runs in-process. Sprint 17: HyDE uses local brain, not cloud API. Sprint 18: model integrity verification, knowledge export for full data control. EU AI Act makes local-first a regulatory advantage. |
| **Accuracy** | Sprint 11 (ACT-R decay, tier-specific half-lives, sandwich ordering, semantic dedup, error recovery, heritage clauses), Sprint 12 (nomic-embed 32x context, spreading activation, admission control, score-aware fusion), Sprint 13 (PPR multi-path, hybrid scoring, centrality, adaptive density, context-weighted recall), Sprint 14 (type propagation 15-30% more edges, RTA 10-40% fewer false positives), Sprint 15 (outcome-driven weight refinement, path-pattern rules, confidence scoring), Sprint 16 (cross-domain impact with precision safeguards), Sprint 17 (HyDE for abstract queries), Sprint 18 (benchmarked accuracy) |

---

## Synapses Intelligence Integration Map

The local brain (llama-server) enhances the system when available. Every feature degrades gracefully when brain is offline.

| Sprint | Feature | How Brain Helps | Without Brain |
|--------|---------|-----------------|---------------|
| 13 | Context-weighted recall | Brain enriches query understanding with intent inference | Session state keywords used directly |
| 16 | Cross-domain name matching | Brain validates match semantics, reduces false positives | Context-aware heuristic matching only |
| 17 | HyDE semantic search | Brain generates hypothetical code → dramatically better embedding | Raw query embedded directly |
| 17 | Documentation graph | Brain identifies code-doc entity links | Name-matching heuristic only |

---

## Competitive Positioning After Sprint 18

| Capability | Synapses | Best Competitor | Advantage |
|------------|----------|----------------|-----------|
| Code intelligence depth | 49+ AST parsers, PPR ego-slices, semantic-structural hybrid | Augment (embeddings, 400K files) | **Synapses — PPR + hybrid scoring is novel** |
| Memory recall accuracy | Quad-retrieval + HNSW + ACT-R decay + spreading activation, target 85-92% LongMemEval | Hindsight (91.4% LongMemEval) | **Competitive** — graph channel + spreading activation matches cross-encoder reranking |
| Impact analysis | Full blast-radius via PPR, cross-domain (code+infra+API+docs) | Nobody | **Synapses — unclaimed** |
| Temporal knowledge | Memory versioning + ACT-R decay + tier-specific half-lives + entity-anchored staleness | Zep (bi-temporal, edge-level) | Zep deeper on pure temporal; **Synapses unique on frequency-weighted decay + staleness** |
| Self-improvement | PPR weight refinement + recall channel learning + outcome signals + path-pattern rules | Cognee (graph refinement by usage) | **Synapses — outcome-driven > usage-driven** |
| Graph traversal | PPR + eigenvector centrality + adaptive density decay + hybrid scoring | Sourcegraph (basic BFS), Augment (embedding similarity) | **Synapses — no competitor uses PPR for code context** |
| Cross-domain | Code + Terraform + OpenAPI + docs graph + cross-domain PPR | Nobody | **Synapses — unclaimed territory** |
| Embedding quality | nomic-embed-text-v1.5 (8192 tokens, MTEB-leading) + HNSW ANN | Competitors use cloud embedding APIs | **Synapses — local + modern + fast** |
| Local-first | Native, zero cloud, bundled embedding + brain | Cognee (self-hostable, needs Neo4j) | **Synapses — zero external deps** |
| Privacy | Everything local. ONNX embeddings. In-process HNSW. Local brain. | Cloud competitors can't match | **Synapses — architectural advantage** |

**Strategic positioning:** Synapses is complementary to Augment Context Engine, not competing. Augment does code retrieval (finding relevant files). Synapses does everything else (memory, temporal, cross-domain, impact, self-improvement). "Use both" is stronger than "use us instead."

---

## Research Council Summary (2026-03-21)

Six domain-expert researchers audited the codebase for improvement opportunities (not bugs). 45 findings across 6 domains, ranked by (Impact x Confidence) / Effort.

| Researcher | Findings | Resolution |
|------------|----------|------------|
| **Graph Algorithm** | 3 HIGH, 3 MEDIUM, 1 LOW | HIGH → Sprints 13 (PPR, hybrid scoring). MEDIUM → Sprints 11, 13. LOW → Backlog. |
| **IR & Embeddings** | 2 HIGH, 3 MEDIUM, 2 LOW | HIGH → Sprint 12 (nomic-embed, HNSW). MEDIUM → Sprints 11, 12. LOW → Backlog. |
| **Performance** | 2 HIGH, 3 MEDIUM, 2 LOW | HIGH → Sprint 11 (SQLite pools, SIMD). MEDIUM → Sprints 11, 14. LOW → Backlog. |
| **Program Analysis** | 3 HIGH, 2 MEDIUM, 2 LOW | HIGH → Sprints 14, 15 (type propagation, path rules, incremental). MEDIUM → Sprints 11, 14. LOW → Sprint 11. |
| **Memory Systems** | 3 HIGH, 4 MEDIUM, 2 LOW | HIGH → Sprints 11, 12 (ACT-R, admission control, spreading activation). MEDIUM → Sprints 11, 12, 13. |
| **Context Delivery** | 3 HIGH, 4 MEDIUM, 2 LOW | HIGH → Sprints 11, 12, 17 (sandwich ordering, semantic discovery, HyDE). MEDIUM → Sprint 17. LOW → Backlog. |

**Finding counts:** HIGH: 16, MEDIUM: 22, LOW: 7
**Action:** Adopt Now: 8 items (Sprint 11), Investigate: 9 items (Sprints 12-17), Defer: 11 items (Backlog)

Key papers informing the roadmap:
- LEGO-GraphRAG (VLDB 2025) — PPR as optimal subgraph extraction
- CodexGraph (NAACL 2025) — structural + semantic hybrid retrieval
- LocAgent (ACL 2025) — multi-hop traversal for code localization
- Anderson & Lebiere (1998) — ACT-R base-level learning equation
- A-MAC (arXiv:2603.04549, 2026) — multi-dimensional memory admission
- Chroma Context Rot (2025) — LLM attention patterns and output ordering
- Liu et al. (NeurIPS 2023) — Lost in the Middle
- Sourcegraph (2024) — From Slow to SIMD in Go

Full findings: `council-reports/research/report.md`
Run data: `council-reports/research/runs/2026-03-21_023439/`

---

## Notes

- **Research Council integration (2026-03-21)**: 45 findings from 6 domain-expert researchers fully integrated into Sprints 11-18. Every HIGH and MEDIUM finding has a home. LOW findings deferred to backlog with clear triggers.
- **Pre-Launch Council integration (2026-03-20)**: 61 findings from 6 operational auditors (Security, Architect, Go Engineer, QA, Product/DX, SRE) fully resolved in Sprints 7-10. All shipped.
- **Sprint resequencing rationale**: Performance foundations (Sprint 11) before algorithmic improvements (Sprints 12-13) because faster SQLite/cache/SIMD makes every subsequent spike and benchmark run faster. Retrieval revolution (Sprint 12) before graph intelligence (Sprint 13) because better embeddings make semantic-structural hybrid scoring meaningful. Domain expansion (Sprint 14) after graph intelligence because new parsers benefit from PPR + hybrid scoring immediately.
- **Knowledge-substrate pivot**: PLAN-KNOWLEDGE-SUBSTRATE.md (Phases 1-5) shipped before Sprint 6.
- **Within-project coordination is solved**: Claude Code Agent Teams (shipped Feb 2026) handles work claims, peer coordination, task handoff.
- **Competitive landscape (March 2026)**: Mem0 (49% LongMemEval), Zep/Graphiti (63.8% LongMemEval, cloud-only), Hindsight (91.4% LongMemEval, cloud-based), Cognee (14 retrieval modes, open-source), Augment (#1 SWE-Bench Pro 51.8%, $252M raised). MCP: 97M monthly SDK downloads.
- **Benchmark fallback strategy**: Three tiers ensure something is always publishable. LongMemEval → CodeMemBench → ablation study. Each Research Council improvement (PPR, HNSW, nomic-embed, ACT-R) is independently ablatable.
- **Intelligence integration**: Brain (local LLM) enhances 4 features across 3 sprints. Every feature degrades gracefully without brain. Privacy preserved — brain is local llama-server, never cloud.
- **IMPROVEMENT.md items**: IMP-IMPL-1 (parseSignatureParts) is an accepted limitation. IMP-IMPL-2, IMP-IMPL-3, IMP-IMPL-4 are wontfix.
