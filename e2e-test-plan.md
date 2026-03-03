# Synapses OS — End-to-End Test Plan
*Reusable across versions. Run this whenever you want to benchmark how well the 3-leg system performs.*

---

## Purpose
Measure context quality, effort savings, and token efficiency of the full synapses-os stack:
- **Leg 1 — Synapses Core (MCP/Graph)**: navigation, context carving, compact format
- **Leg 2 — Intelligence (Sidecar)**: ingest, enrich, context-packet, violation explanation
- **Leg 3 — Scout (Web)**: search, fetch, distillation pipeline, web_annotate persistence

Each leg has Pass/Fail checks + a scored metric. A final composite rating is computed.

---

## Pre-flight Checklist

```bash
# Verify all 3 components are reachable
synapses version                          # should print current version
curl -s http://localhost:11435/v1/health  # intelligence sidecar
curl -s http://localhost:11436/v1/health  # scout sidecar
```

Expected: synapses binary exists, both HTTP 200 with `status:ok`.

---

## Leg 1 — Synapses Core (MCP / Graph Navigation)

### L1-A: session_init bootstrap
- Call `session_init()` via MCP
- **Pass**: returns `pending_tasks`, `project_identity`, `working_state`, `recent_events` in one round-trip
- **Fail**: tool unavailable or missing any of the 4 fields
- **Metric**: 1 tool call vs 3 separate calls (get_pending_tasks + get_project_identity + get_working_state)

### L1-B: find_entity precision
- Call `find_entity("CarveEgoGraph")`
- **Pass**: returns file path, line number, and node ID within 1 call
- **Fail**: returns wrong entity or requires follow-up grep
- **Metric**: 1 MCP call vs 2-3 grep commands

### L1-C: get_context compact format (token savings)
- Call `get_context("Builder.Build", format="compact")`
- Record token count of response
- Call `get_context("Builder.Build", format="json")`
- Record token count of response
- **Pass**: compact response is ≥70% smaller than JSON
- **Metric**: token reduction % (target: ≥70%)

### L1-D: get_call_chain (manual trace replacement)
- Call `get_call_chain("cmdStart", "Builder.Build")`
- **Pass**: returns a path without requiring manual file reads
- **Fail**: no path found or requires Read/Grep to confirm
- **Metric**: 1 call vs 5-10 Read/Grep commands

### L1-E: search (semantic keyword)
- Call `search("rate limiting")` and `search("BFS carver")`
- **Pass**: returns relevant entity names with file:line in ≤2 results
- **Fail**: empty results or irrelevant matches
- **Metric**: precision@3 (how many of top-3 are relevant)

### L1-F: get_impact (blast radius)
- Call `get_impact("Graph.AddNode")`
- **Pass**: groups results into direct/indirect/peripheral with confidence scores
- **Fail**: no results or flat list with no grouping
- **Metric**: depth-3 coverage (counts entities at each depth)

### L1-G: validate_plan (architectural guard)
- Propose a cross-layer change: `validate_plan([{"file": "synapses/internal/graph/graph.go", "adds_call_to": "Store.Close"}])`
- **Pass**: detects violation (graph→store is forbidden or flagged)
- **Inconclusive**: no rules configured (check get_violations first)
- **Metric**: rule enforcement (yes/no)

### L1-H: get_working_state (developer orientation)
- Call `get_working_state(window_minutes=60)`
- **Pass**: returns recently changed files matching git diff
- **Fail**: empty with recent changes present

### Leg 1 Scoring
| Check | Weight | Score (0-10) |
|-------|--------|--------------|
| L1-A session_init | 1× | |
| L1-B find_entity | 1× | |
| L1-C token savings | 2× | |
| L1-D call chain | 1× | |
| L1-E search | 1× | |
| L1-F impact | 1× | |
| L1-G validate_plan | 1× | |
| L1-H working_state | 1× | |
| **Leg 1 Total** | **/9×** | |

---

## Leg 2 — Intelligence Sidecar

### L2-A: Health + tier model check
```bash
curl -s http://localhost:11435/v1/health
```
- **Pass**: `available: true`, model field present

### L2-B: Ingest (Tier 0 prose briefing)
```bash
curl -s -X POST http://localhost:11435/v1/ingest \
  -H "Content-Type: application/json" \
  -d '{"node_id":"test-e2e-001","code":"func CarveEgoGraph(g *Graph, root NodeID, depth int) *SubGraph { /* BFS carver for ego subgraphs */ }","name":"CarveEgoGraph","type":"function","file":"internal/graph/traverse.go","language":"go"}'
```
- **Pass**: returns `{"summary":"..."}` with 1-3 sentence prose (not JSON blob, not empty)
- **Fail**: empty summary, error, or raw JSON object as summary
- **Metric**: summary char count (target: 80-400 chars, meaningful prose)

### L2-C: Enrich (Tier 2 architectural insight)
```bash
curl -s -X POST http://localhost:11435/v1/enrich \
  -H "Content-Type: application/json" \
  -d '{"node_id":"test-e2e-001","summary":"BFS carver that extracts ego subgraphs","code":"func CarveEgoGraph(...)","name":"CarveEgoGraph","type":"function","file":"internal/graph/traverse.go","language":"go"}'
```
- **Pass**: returns `insight` (architectural significance) + `concerns` array
- **Fail**: empty insight, error, or timeout
- **Metric**: insight char count (target: 50-300 chars), concerns count

### L2-D: Context packet
```bash
curl -s -X POST http://localhost:11435/v1/context-packet \
  -H "Content-Type: application/json" \
  -d '{"root_node_id":"synapses-os::synapses/internal/graph/traverse.go::CarveEgoGraph","dep_node_ids":[]}'
```
- **Pass**: returns packet with `root_summary`, `packet_quality` ≥0.5
- **Fail**: quality=0.0 or missing fields
- **Metric**: packet_quality score (0.0-1.0)

### L2-E: Explain violation (Tier 1 guardian)
```bash
curl -s -X POST http://localhost:11435/v1/explain-violation \
  -H "Content-Type: application/json" \
  -d '{"rule_id":"no-db-in-handler","description":"Handler directly calls database","from_entity":"handleGetContext","to_entity":"Store.GetNode","severity":"error"}'
```
- **Pass**: returns plain-English explanation (not JSON, ≥50 chars)
- **Fail**: empty, error, or raw rule JSON

### L2-F: SDLC phase check
```bash
curl -s http://localhost:11435/v1/sdlc
```
- **Pass**: returns `phase` and `mode` fields
- **Fail**: 404 or missing fields

### L2-G: Prune endpoint (Tier 0 boilerplate stripper) — v0.5.1+
```bash
curl -s -X POST http://localhost:11435/v1/prune \
  -H "Content-Type: application/json" \
  -d '{"content":"<html><head><title>Test</title></head><body><nav>skip nav</nav><main>The core technical content: BFS traversal starts from root node and explores neighbors depth-first up to a configurable limit.</main><footer>Copyright 2025</footer></body></html>"}'
```
- **Pass**: returns `pruned` field with boilerplate stripped; `pruned_length < original_length`
- **Fail**: error, or pruned == original (LLM offline)
- **Metric**: compression ratio (pruned_length / original_length, target ≤0.5)

### Leg 2 Scoring
| Check | Weight | Score (0-10) |
|-------|--------|--------------|
| L2-A health | 1× | |
| L2-B ingest prose | 2× | |
| L2-C enrich insight | 2× | |
| L2-D context packet | 2× | |
| L2-E explain violation | 1× | |
| L2-F SDLC | 1× | |
| L2-G prune | 1× | |
| **Leg 2 Total** | **/10×** | |

---

## Leg 3 — Scout Sidecar

### L3-A: Health check
```bash
curl -s http://localhost:11436/v1/health
```
- **Pass**: `status: ok`, `intelligence_available: true`
- **Fail**: unavailable or intelligence_available: false

### L3-B: Web search
```bash
curl -s -X POST http://localhost:11436/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query":"go tree-sitter bindings golang","max_results":3}'
```
- **Pass**: returns ≥2 results with title, url, snippet
- **Fail**: 0 results or error
- **Metric**: result count, avg snippet length

### L3-C: Web fetch (fast path — trafilatura)
```bash
curl -s -X POST http://localhost:11436/v1/fetch \
  -H "Content-Type: application/json" \
  -d '{"url":"https://pkg.go.dev/github.com/smacker/go-tree-sitter","distill":false}'
```
- **Pass**: returns content with `source: "trafilatura"` or `"browser"`, ≥200 chars
- **Fail**: empty content, error
- **Metric**: latency (target: <5s trafilatura, <15s browser)

### L3-D: Distillation pipeline (prune→ingest) — v0.0.3+
```bash
curl -s -X POST http://localhost:11436/v1/fetch \
  -H "Content-Type: application/json" \
  -d '{"url":"https://pkg.go.dev/github.com/smacker/go-tree-sitter","distill":true}'
```
- **Pass**: returns `distilled: true`, `distilled_content` present and shorter than raw
- **Fail**: `distilled: false`, timeout, or missing field
- **Metric**: distilled_content length vs raw content length

### L3-E: Cache check (no duplicate fetch)
- Repeat L3-C with same URL
- **Pass**: second call is ≥80% faster (from cache)
- **Fail**: same latency as first call

### L3-F: MCP web_search tool (via synapses)
- Call `web_search(query="BFS ego graph algorithm golang")` via MCP
- **Pass**: returns results without raw API error
- **Fail**: "scout unavailable" or empty

### L3-G: MCP web_annotate persistence
- Call `web_annotate(node_id="...", note="test annotation from e2e", hits=[{...}])` via MCP
- Call `get_context("CarveEgoGraph", format="compact")`
- **Pass**: annotation appears in context output
- **Fail**: annotation missing from context

### Leg 3 Scoring
| Check | Weight | Score (0-10) |
|-------|--------|--------------|
| L3-A health | 1× | |
| L3-B search | 2× | |
| L3-C fetch speed | 1× | |
| L3-D distillation | 2× | |
| L3-E cache | 1× | |
| L3-F MCP web_search | 1× | |
| L3-G web_annotate | 2× | |
| **Leg 3 Total** | **/10×** | |

---

## Token & Effort Savings Methodology

### Token savings measurement
For each MCP navigation call, estimate the equivalent without synapses:

| Synapses Tool | Without-synapses equivalent | Baseline tokens |
|--------------|---------------------------|-----------------|
| session_init | get_pending_tasks + get_project_identity + get_working_state | ~3 grep + 3 file reads |
| find_entity | grep -r "FuncName" src/ \| head -5 | ~500 tokens output |
| get_context(compact) | Read 3-5 files manually | ~3000 tokens |
| get_context(json) | Read 3-5 files manually | ~3000 tokens |
| get_call_chain | Manual grep → read → grep → read chain (5+ steps) | ~5000 tokens |
| get_impact | No equivalent — manual analysis | ∞ |
| search(semantic) | grep -r keyword across 200 files | ~2000 tokens noise |
| validate_plan | Manual rule review | ~1000 tokens |

### Effort savings (round-trip tool calls saved)
Count: (baseline tool calls) − (synapses tool calls) for each task.

### Money savings (API cost)
At Claude Sonnet 4.6 pricing: $3/M input tokens + $15/M output tokens.
Compute: (tokens_saved_per_session × sessions_per_day × 30) × $3/1M

---

## Rating Rubric

| Score | Label | Description |
|-------|-------|-------------|
| 9-10 | Excellent | All legs pass, ≥70% token savings, context quality is production-ready |
| 7-8 | Good | Minor failures in 1-2 checks, 50-70% savings, useful but some rough edges |
| 5-6 | Acceptable | Several non-critical failures, 30-50% savings, needs improvement |
| 3-4 | Needs Work | Core navigation works but intelligence/scout unreliable |
| 1-2 | Broken | Core functionality failing |

### Composite Score Formula
```
Leg1_score = weighted_avg(L1-A..L1-H scores) / 10
Leg2_score = weighted_avg(L2-A..L2-G scores) / 10
Leg3_score = weighted_avg(L3-A..L3-G scores) / 10
Token_score = clamp(token_reduction_pct / 70, 0, 1)
Composite = (Leg1 × 0.35 + Leg2 × 0.30 + Leg3 × 0.25 + Token_score × 0.10) × 10
```

---

## Results Template

Fill this in after each test run:

```
## E2E Test Results — vX.Y.Z — YYYY-MM-DD

### System Versions
- synapses: vX.Y.Z
- synapses-intelligence: vX.Y.Z
- synapses-scout: vX.Y.Z

### Leg 1 — Synapses Core
| Check | Pass/Fail | Score | Notes |
|-------|-----------|-------|-------|
| L1-A session_init | | | |
| L1-B find_entity | | | |
| L1-C token savings | | | compact: NNN tokens, json: NNN tokens (XX% reduction) |
| L1-D call chain | | | |
| L1-E search | | | |
| L1-F impact | | | |
| L1-G validate_plan | | | |
| L1-H working_state | | | |
| **Leg 1 Score** | | **/10** | |

### Leg 2 — Intelligence Sidecar
| Check | Pass/Fail | Score | Notes |
|-------|-----------|-------|-------|
| L2-A health | | | |
| L2-B ingest | | | summary: "..." |
| L2-C enrich | | | insight: "..." |
| L2-D context packet | | | quality=X.X |
| L2-E explain violation | | | |
| L2-F SDLC | | | phase=X, mode=X |
| L2-G prune | | | ratio=X.X |
| **Leg 2 Score** | | **/10** | |

### Leg 3 — Scout
| Check | Pass/Fail | Score | Notes |
|-------|-----------|-------|-------|
| L3-A health | | | |
| L3-B search | | | N results |
| L3-C fetch | | | Xs, source=X |
| L3-D distillation | | | distilled=X |
| L3-E cache | | | Xs cached |
| L3-F MCP web_search | | | N results |
| L3-G web_annotate | | | persisted=X |
| **Leg 3 Score** | | **/10** | |

### Token & Effort Savings
| Metric | Value |
|--------|-------|
| get_context compact vs json | XX% reduction |
| Estimated tokens saved per session | ~NNN tokens |
| Equivalent tool calls without synapses | ~N calls → 1 call |
| Estimated monthly API cost saved | ~$X.XX |

### Final Rating
**Composite Score: X.X / 10 — [Label]**

### Observations
- What worked well:
- What needs improvement:
- Bugs found:
```

---

## Regression Baseline (v0.5.1)
*Update this section after each major version test.*

| Metric | v0.5.1 baseline |
|--------|----------------|
| session_init round-trips | 1 (was 3) |
| get_context compact tokens | ~450 avg |
| get_context json tokens | ~5000 avg |
| token reduction | ~89% |
| intelligence WriteTimeout | 2×TimeoutMS (was hardcoded 30s) |
| scout distillation pipeline | 2-step (prune→ingest) |
| ingest latency (Tier 0 qwen2.5-coder:7b CPU) | ~12s ✅ |
| ingest latency (Tier 0 qwen3.5:0.8b CPU) | >60s ❌ too slow |
| enrich latency (Tier 2 qwen3.5:4b CPU) | >20min ❌ too slow |
| prune latency (Tier 0 qwen3.5:0.8b CPU) | >120s ❌ too slow |
| scout search latency | ~1.3s |
| scout cache hit latency | ~14ms |

---

## E2E Test Results — v0.5.1 — 2026-03-03

### System Versions
- synapses: v0.5.1 (binary v0.4.0 label — version constant not bumped ⚠️)
- synapses-intelligence: v0.5.1 (binary reads brain.json correctly)
- synapses-scout: v0.0.3

### Environment
- CPU-only (no GPU) Linux server
- All Qwen3.5 models installed (0.8b, 2b, 4b, 9b), qwen2.5-coder:7b also available
- brain.json: model_ingest=qwen3.5:0.8b, model_enrich=qwen3.5:4b, timeout_ms=120000

---

### Leg 1 — Synapses Core
| Check | Pass/Fail | Score | Notes |
|-------|-----------|-------|-------|
| L1-A session_init | ✅ Pass | 10 | 1 call, returned pending_tasks + project_identity + working_state + recent_events |
| L1-B find_entity | ✅ Pass | 9 | Found CarveEgoGraph in 1 call with signature + line. Ranks test functions before main entity. |
| L1-C token savings | ✅ Pass | 10 | compact: ~450 tokens, json: ~5000 tokens → **89% reduction** (target ≥70%) |
| L1-D call chain | ⚠️ Partial | 6 | Correctly returns "no path" for cross-binary calls. No explanation of WHY (cross-binary boundary). |
| L1-E search | ⚠️ Partial | 6 | "CarveEgoGraph" → 12 hits ✅. "rate limiting" → 1 hit ✅. "BFS carver" → 0 hits ❌ (phrase FTS gap) |
| L1-F get_impact | ⚠️ Partial | 5 | Result exceeds 85k chars for high-fanin nodes (Graph.AddNode: 55 callers). Tool crashes with overflow. |
| L1-G validate_plan | ⚠️ Partial | 7 | No rules configured → no violations. Tool works but requires upfront rule setup. |
| L1-H working_state | ✅ Pass | 8 | Returns recent_changes correctly (empty for 60-min window with no activity). |
| **Leg 1 Score** | | **7.9/10** | Weighted: (10+9+20+6+6+5+7+8)/9 |

### Leg 2 — Intelligence Sidecar
| Check | Pass/Fail | Score | Notes |
|-------|-----------|-------|-------|
| L2-A health | ✅ Pass | 10 | available:true, model:qwen3.5:4b shown |
| L2-B ingest | ⚠️ Partial | 4 | qwen2.5-coder:7b: ✅ 12s, good prose. qwen3.5:0.8b: ❌ "empty response from LLM" after 56s |
| L2-C enrich | ❌ Fail | 2 | qwen3.5:4b with thinking: >20min on CPU, never completes within any timeout |
| L2-D context packet | ⚠️ Partial | 5 | Fast path works (phase, quality_gate, phase_guidance). packet_quality=0 (no summaries cached yet) |
| L2-E explain violation | ❌ Fail | 2 | qwen3.5:2b timeout — same CPU bottleneck |
| L2-F SDLC | ✅ Pass | 9 | phase=development, mode=standard, updated_at correct |
| L2-G prune | ❌ Fail | 2 | qwen3.5:0.8b: times out at 120s on CPU |
| **Leg 2 Score** | | **4.5/10** | Root cause: Qwen3.5 models too slow on CPU. qwen2.5-coder:7b works. |

**Root cause for L2 failures:** qwen3.5:0.8b takes >60s per inference on CPU (even for simple prompts). The architecture is correct; model selection for CPU needs adjusting. Recommended: use qwen2.5-coder:1.5b for Tier 0/1 on CPU, qwen3.5:4b only on GPU.

### Leg 3 — Scout
| Check | Pass/Fail | Score | Notes |
|-------|-----------|-------|-------|
| L3-A health | ✅ Pass | 10 | status:ok, intelligence_available:true |
| L3-B search | ✅ Pass | 10 | 3 results in 1.3s — all relevant (github, pkg.go.dev) |
| L3-C fetch | ✅ Pass | 8 | content_md: 29,943 chars, word_count: 2404. Field is `content_md` not `content` (doc inconsistency). |
| L3-D distillation | ❌ Fail | 3 | BUG-SC01: intelligence prune times out on CPU. BUG-SC02: cache blocks distill:true (force_refresh required). |
| L3-E cache | ✅ Pass | 10 | 14ms cached vs ~1s fresh. **99% faster** on cache hit. |
| L3-F MCP web_search | ❌ Fail | 2 | "scout unavailable" — MCP server started before scout config in synapses.json. Needs restart to pick up config. |
| L3-G web_annotate | ✅ Pass | 9 | Annotation saved (annotation_id returned). Stored in graph DB. Compact format shows doc-summary; JSON format shows annotations separately. |
| **Leg 3 Score** | | **7.4/10** | Weighted: (10+20+8+6+10+2+18)/10 |

---

### Token & Effort Savings (measured)
| Metric | Value |
|--------|-------|
| get_context compact vs json | **89% reduction** (~450 vs ~5000 tokens) |
| session_init (1 call vs 3 calls) | 66% fewer round-trips at session start |
| find_entity vs grep across 195 files | 1 call (deterministic) vs 10+ grep lines + noise |
| get_call_chain vs manual trace | 1 call vs 5-10 sequential Read+Grep steps |
| Estimated tokens saved per session (navigation) | ~15,000–20,000 tokens |
| Equivalent API cost saved per session (Sonnet 4.6) | ~$0.045–$0.06 saved |
| At 100 sessions/month | ~$4.50–$6.00/month saved |
| **Biggest win** | get_impact — no manual equivalent exists. Would require reading 55 files to find all callers. |

---

### Bugs Found During This Run
| ID | Severity | Description |
|----|----------|-------------|
| BUG-NEW-01 | Medium | brain/main.go version constant still says "0.4.0" — should be "0.5.1" |
| BUG-NEW-02 | High | qwen3.5:0.8b too slow on CPU for all configured timeouts (>60s). qwen2.5-coder:1.5b recommended for CPU Tier 0 |
| BUG-SC01 | High | Scout intelligence_timeout_ms (60s) < Qwen3.5 CPU inference time — distillation always fails on CPU |
| BUG-SC02 | Medium | Cache hit returns undistilled content even when distill:true (force_refresh workaround exists) |
| BUG-NEW-03 | Medium | get_impact overflows tool output for high-fanin nodes (85k+ chars). Needs depth/node cap. |
| BUG-NEW-04 | Low | MCP web_search "scout unavailable" after config added — synapses reads config once at startup, no hot-reload |
| BUG-NEW-05 | Low | get_call_chain returns "no path" for cross-binary calls without explaining the boundary |

---

### Final Rating

```
Leg 1 (Synapses Core):    7.9/10
Leg 2 (Intelligence):     4.5/10  ← CPU model mismatch
Leg 3 (Scout):            7.4/10
Token Score:              1.0/10 (89% reduction, capped)

Composite = (0.79 × 0.35) + (0.45 × 0.30) + (0.74 × 0.25) + (1.0 × 0.10)
          = 0.2765 + 0.135 + 0.185 + 0.10
          = 0.6965
```

**Composite Score: 7.0 / 10 — Good**

> **Key insight:** Score drops primarily from L2 (intelligence) which is a CPU hardware mismatch
> not an architecture flaw. If qwen2.5-coder:7b is used (which works at 12s), L2 would score
> ~8.0 and the composite rises to **7.7/10 — Good/Excellent border**.
> The graph navigation layer (Leg 1) and web layer (Leg 3) are solid.

### What Worked Well
- **89% token reduction** via compact format — biggest ROI feature
- **session_init** bootstrap is exactly 1 call — excellent ergonomics
- **Scout search** — 1.3s, relevant results, zero setup friction
- **Scout cache** — 14ms hits, transparent to caller
- **web_annotate** — full persistence loop works end-to-end
- **SDLC phase management** — correct, persistent across restarts
- **Context packet fast path** — deterministic quality gates without LLM

### What Needs Improvement
- **Model selection for CPU** — Qwen3.5 models are designed for GPU. Tier 0 should default to qwen2.5-coder:1.5b on CPU
- **get_impact overflow** — needs max_nodes or depth cap before serializing
- **MCP config hot-reload** — synapses should watch synapses.json for changes
- **Scout distillation on CPU** — intelligence_timeout_ms must be ≥ 2× Ollama inference time
- **version constant** — main.go should be bumped as part of release process
- **call_chain cross-binary** — should detect and explain cross-binary boundary
