# Synapses OS — Improvement Plan v0.6
*Generated after E2E test run (2026-03-03) + analysis of "Codified Context" paper (arxiv 2602.20478)*

---

## Executive Summary

The E2E test (composite 7.0/10) revealed two categories of gaps:

1. **Operational bugs** — model selection for CPU, output overflow, cache race conditions
2. **Architectural gaps** — synapses has excellent graph mechanics but is missing the *context curation* layer described in the Codified Context paper

The Codified Context paper studied 283 sessions on a 108k-line codebase and proved that
**curated, structured context beats a bigger context window**. Synapses already embodies
this principle via compact BFS carving. The paper adds three missing concepts:

- **Tier 1 (Hot Constitution)**: Project rules that are *always injected*, machine-readable
- **Tier 2 (Domain Personas)**: Context shifts by *what area* you're editing, not just *what entity*
- **Tier 3 (Cold Memory / ADRs)**: Permanent storage of *why* decisions were made — the anti-goldfish layer

Each section below maps a gap to a concrete change.

---

## Priority Levels
- **P0** — Breaks existing functionality. Fix immediately.
- **P1** — Significant UX degradation. Fix in next release (v0.5.2).
- **P2** — Quality improvement. Target v0.6.0.
- **P3** — New capability. Target v0.6.x / v0.7.0.

---

## P0 — Critical Operational Bugs

### P0-A: CPU Model Selection (BUG-NEW-02)
**Problem:** `brain.json` defaults to `qwen3.5:0.8b` for Tier 0. On CPU-only hardware this
model takes >60s per inference — exceeding all configured timeouts. Every LLM endpoint fails
silently. Meanwhile `qwen2.5-coder:7b` (also installed) does ingest in 12s.

**Fix:**
- Add `brain setup` auto-detection: if no GPU detected, recommend `qwen2.5-coder:1.5b` for
  Tier 0 and Tier 1, keep `qwen3.5:4b` for Tier 2/3 (run only when explicitly triggered)
- Add a `"cpu_mode": true` flag in brain.json that swaps all 4 tier models to CPU-safe defaults:
  `qwen2.5-coder:1.5b` for all tiers (safe, <15s on any CPU)
- Update `brain setup` to probe Ollama for actual inference latency (30-token test prompt)
  before writing brain.json, picking the fastest model that fits within `timeout_ms/2`
- Document CPU vs GPU model recommendations prominently in README

**Files:** `synapses-intelligence/cmd/brain/main.go`, `config/config.go`

---

### P0-B: `get_impact` Output Overflow (BUG-NEW-03)
**Problem:** High-fanin nodes (e.g. `Graph.AddNode` with 55 callers) produce 85k+ character
output — exceeding Claude's tool result limit. The tool crashes instead of returning a capped result.

**Fix:**
- Add `max_nodes int` parameter to `get_impact` (default 50)
- When result exceeds cap: serialize only depth-1 results in full, summarize depth-2+ as counts
  e.g. `"indirect_count": 340, "peripheral_count": 1200`
- Add `"truncated": true` flag in response so caller knows results were capped
- Apply same cap to `get_context` when `token_budget` is hit (already partially there via
  CarveEgoGraph's token pruning, but the impact tool bypasses it)

**Files:** `synapses/internal/mcp/tools.go` (handleGetImpact), `internal/graph/traverse.go`

---

### P0-C: Scout Distillation Timeout (BUG-SC01 still present)
**Problem:** Even with `intelligence_timeout_ms: 60000` in scout.json, the prune step
(qwen3.5:0.8b) takes >120s on CPU. Scout's client times out before intelligence returns.

**Fix:**
- Scout distillation: make prune step *optional and async* — if prune times out, proceed
  with raw content directly to ingest (already fail-silent on prune, but ingest also fails)
- Add `"distill_strategy": "best_effort"` vs `"distill_strategy": "required"` in scout.json
- In `best_effort` mode: if prune OR ingest times out, return the trafilatura-extracted
  content as `content_md` with `distilled: false` rather than failing the whole fetch
- Long term: Scout should do *client-side* distillation using a local rule-based boilerplate
  stripper (remove nav, footer, sidebar) as Tier 0 fallback when intelligence is slow

**Files:** `synapses-scout/src/scout/distiller/client.py`, `synapses-scout/src/scout/router.py`

---

## P1 — Significant UX Improvements (v0.5.2)

### P1-A: `synapses.json` Hot-Reload (BUG-NEW-04)
**Problem:** synapses reads config at startup only. If scout/brain URLs are added after start,
MCP tools like `web_search` return "unavailable" until restart. Users have no feedback loop.

**Fix:**
- Watch `synapses.json` via fsnotify (already wired for source files in `internal/watcher`)
- On change: re-init brain client + scout client without restarting the MCP server
- Add `"config_reload"` event to the events table so session_init picks it up
- For VS Code extension: add status bar indicator showing brain/scout connectivity live

**Files:** `synapses/internal/watcher/watcher.go`, `synapses/internal/config/config.go`,
`synapses/internal/mcp/server.go`

---

### P1-B: Scout Cache Blocks `distill:true` (BUG-SC02)
**Problem:** Once a URL is cached without distillation, `distill:true` on subsequent calls
returns the undistilled cached result. The `force_refresh` workaround is not discoverable.

**Fix:**
- Add a `distilled` boolean column to scout.db cache table
- Cache lookup: if `distill:true` requested but cached entry has `distilled=false`,
  bypass cache and re-fetch + distill
- Return `"cache_miss_reason": "not_distilled"` in response so caller understands
- Long term: cache distilled and undistilled versions separately (different cache keys)

**Files:** `synapses-scout/src/scout/cache.py`, `synapses-scout/src/scout/scout.py`

---

### P1-C: `get_call_chain` Cross-Binary Explanation (BUG-NEW-05)
**Problem:** When asked for a chain between entities in different binaries/processes, the tool
returns `"no call chain exists"` with no explanation. The user assumes the tool is broken.

**Fix:**
- Detect cross-binary boundary: if `from` and `to` are in different repo roots / entry-point
  binaries, return a structured explanation:
  ```json
  {
    "found": false,
    "reason": "cross_binary_boundary",
    "from_binary": "synapses/cmd/synapses",
    "to_binary": "synapses-intelligence/cmd/brain",
    "message": "These entities live in separate processes. Communication happens via HTTP (see brain client at synapses/internal/brain/client.go).",
    "bridge_hint": "synapses-os::synapses/internal/brain/client.go::Client.Ingest"
  }
  ```
- Use entry-point analysis (already tracked) to classify each node to its owning binary
- Surface the HTTP bridge (brain/client.go, scout/client.go) as the suggested path

**Files:** `synapses/internal/mcp/tools.go` (handleGetCallChain), `internal/graph/traverse.go`

---

### P1-D: FTS Phrase Matching Gap (L1-E)
**Problem:** `search("BFS carver")` → 0 results. The FTS5 index doesn't handle multi-word
conceptual phrases that span different tokens in names/docs.

**Fix:**
- At index time: for each node's doc comment, extract 2-gram and 3-gram phrase tuples and
  store in FTS5 as additional content (e.g. "BFS carver", "ego graph", "token budget")
- At query time: if single FTS5 query returns 0 results, split query into individual terms
  and return union results ranked by term overlap count
- Add semantic fallback: if `embedding_endpoint` configured in synapses.json, re-rank by
  cosine similarity (already mentioned in handleSemanticSearch doc)
- Consider storing camelCase splits as additional FTS5 content (splitCamelCase already exists
  in store.go — use it at index time too, not just at search time)

**Files:** `synapses/internal/store/store.go` (FTS5 index construction),
`synapses/internal/mcp/tools.go` (handleSemanticSearch)

---

### P1-E: Test Files Ranked Above Main Entity (L1-B)
**Problem:** `find_entity("CarveEgoGraph")` returns test functions before the main method.
Users want the primary implementation first.

**Fix:**
- In `find_entity` ranking: boost entities in non-test files (file not ending in `_test.go`
  or `test_*.py`)
- Secondary rank: prefer methods/functions over test stubs
- Third rank: prefer exported over unexported
- Return main entity as `matches[0]` consistently

**Files:** `synapses/internal/store/store.go` (entity search), `synapses/internal/mcp/tools.go`

---

## P2 — Quality Improvements (v0.6.0)

### P2-A: "Hot Constitution" — Project Rules as Graph Nodes
*Inspired by Codified Context Tier 1*

**Problem:** synapses.json supports architectural rules (`dynamic_rules` table) but:
1. Rules are violation-detection only (post-hoc)
2. No "always-inject" constitutional principles that appear in every context packet
3. Rules are config-file based, not queryable or enrichable

**The Codified Context insight:** The paper found that having explicit, machine-readable
"laws" (PROHIBITED / REQUIRED patterns) injected every session prevented repeat mistakes
across 283 sessions. The key is they were *always active*, not checked lazily.

**Improvement:**
- Add a `constitution` section to synapses.json (alongside `brain` and `scout`):
  ```json
  {
    "constitution": {
      "principles": [
        "Never use CGo — use modernc/sqlite (pure Go)",
        "All MCP handlers must be fail-silent (return empty result, not error, on LLM timeout)",
        "Parser packages must not import internal/graph — use only internal/parser types"
      ],
      "inject_in_context": true,
      "inject_in_session_init": true
    }
  }
  ```
- Surface principles in `session_init` response (new `constitution` field)
- Surface principles in `get_context` compact output for entities that violate or are near
  constitution rules (based on file pattern matching)
- Store principles as special `NodeType=PRINCIPLE` nodes in the graph, attached via
  `GOVERNS` edges to file patterns they apply to

**Impact:** Claude (or any agent) sees the project's laws every session — no re-explaining
conventions. This directly solves the "Goldfish Memory" problem.

**Files:** `synapses/internal/config/config.go`, `synapses/internal/graph/graph.go` (new node type),
`synapses/internal/mcp/tools.go` (session_init, get_context)

---

### P2-B: Architectural Decision Records (ADRs) as Cold Memory
*Inspired by Codified Context Tier 3*

**Problem:** `decision_log` in brain.sqlite records what an agent *did*, but not *why*
architectural choices exist. The paper's "Cold Memory" includes:
- Why a library was chosen / rejected
- Why a certain pattern was adopted
- Known limitations and their workarounds

Currently this knowledge lives in CLAUDE.md comments and gets stale. There's no way for
the AI to query "why does this use X instead of Y?"

**Improvement:**
- Add `upsert_adr` MCP tool:
  ```json
  {
    "title": "No CGo in core packages",
    "status": "accepted",
    "context": "Deployment targets include ARM and MUSL Linux; CGo breaks cross-compilation",
    "decision": "Use modernc/sqlite (pure Go SQLite driver)",
    "consequences": "No libsqlite3 system dependency; binary is self-contained",
    "linked_nodes": ["synapses/internal/store/store.go"]
  }
  ```
- Store ADRs in brain.sqlite (new `adrs` table)
- Surface relevant ADRs in context packet when working on linked node areas
- Add `get_adrs(filter_by_file)` MCP tool for cold-memory retrieval
- `get_context` compact format: append `[ADR: ...]` lines for nearby ADRs

**Impact:** The "why" of design choices is permanently available. Future agents (or future
versions of Claude) never reverse a deliberate architectural decision.

**Files:** `synapses-intelligence/internal/store/store.go`, `synapses/internal/mcp/brain_tools.go`,
`synapses-intelligence/server/server.go` (new `/v1/adr` endpoint)

---

### P2-C: Domain-Aware Context Enrichment
*Inspired by Codified Context Tier 2 (19 specialized personas)*

**Problem:** The intelligence sidecar uses the same enrichment prompt for every node
regardless of domain. A parser function gets the same analysis prompt as an MCP handler.
This produces generic insights ("this function processes data") instead of domain-specific
ones ("this parser function should not use tree-sitter APIs directly at this layer").

**The Codified Context insight:** 19 specialized agents each had domain-specific instructions
that prevented "hallucination bloat" — the AI not being distracted by irrelevant rules.

**Improvement:**
- Add domain detection to enricher: classify each node by its file path pattern:
  ```
  internal/parser/    → "parser domain" — highlight language-specific quirks, AST handling
  internal/mcp/       → "MCP domain" — highlight tool contract, fail-silent, latency
  internal/graph/     → "graph domain" — highlight complexity, edge cases, BFS correctness
  internal/store/     → "store domain" — highlight SQL correctness, migration safety
  internal/brain/     → "integration domain" — highlight timeout handling, HTTP contracts
  ```
- Each domain gets a specialized enrichment prompt prefix
- Domain-specific concerns: parser domain auto-checks for missing language support; MCP
  domain auto-checks for missing error handling
- Store `domain` as a tag on summaries so context packets can filter by domain context

**Files:** `synapses-intelligence/internal/enricher/enricher.go`,
`synapses-intelligence/internal/ingestor/ingestor.go`

---

### P2-D: Progressive Context Loading
*Inspired by Codified Context's "15-20% buffer reservation" and "architectural summaries first"*

**Problem:** `get_context` currently returns a fixed-depth BFS result. There's no way to
progressively load more context as budget allows. A caller has no way to ask "give me a
summary first, then load neighbors if I have budget."

**Improvement:**
- Add `detail_level` parameter to `get_context`: `summary` | `neighbors` | `full`
  - `summary`: root node only (doc + signature + summary) — ~50 tokens
  - `neighbors`: root + immediate callers/callees — ~200 tokens
  - `full`: current behavior (BFS depth 2) — ~450 tokens compact, ~5000 JSON
- Add `remaining_budget_tokens` to context packet responses — lets the agent know how much
  budget is left and whether to request more context
- `session_init` defaults to `summary` level for all entities; agent can call
  `get_context(entity, detail_level="full")` for entities it needs to edit

**Impact:** Matches the paper's recommendation to prioritize architectural summaries before
implementation details. Reduces session startup token cost by ~60%.

**Files:** `synapses/internal/mcp/tools.go`, `synapses/internal/mcp/digest.go`,
`synapses/internal/graph/traverse.go`

---

### P2-E: Context Packet Quality When Brain Is Cold
**Problem:** `packet_quality: 0.0` when brain.sqlite has no summaries (fresh install or
after `brain reset`). The fast path (SDLC + quality_gate + phase_guidance) works, but
`entity_name` returns empty — meaning the context packet carries no semantic knowledge.

**Improvement:**
- Fall back to the graph's own doc comment as `root_summary` when brain.sqlite has no
  summary for the node (graph nodes already carry doc from parsers)
- This gives `packet_quality ≥ 0.4` immediately (root_summary present) without any LLM
- Update `computeQuality` to check graph annotation as a fallback source
- Display "source: graph_doc" vs "source: brain_summary" in the response so caller knows quality

**Files:** `synapses-intelligence/internal/contextbuilder/builder.go`

---

### P2-F: Search Result Caching in Scout (BUG-SC03)
**Problem:** Search results are never stored in scout.db despite `default_ttl_search_hours`
being configured. This means every search hits DuckDuckGo, adding latency and rate-limit risk.

**Fix:**
- Identify where search results are meant to be cached in `cache.py`
- Add `cache_write` call in `scout.py` search handler after results returned
- Verify TTL-based expiry works for search results (same as web_page entries)
- Add `"search": N` to the health endpoint's `cache.by_type` breakdown

**Files:** `synapses-scout/src/scout/scout.py`, `synapses-scout/src/scout/cache.py`

---

## P3 — New Capabilities (v0.6.x / v0.7.0)

### P3-A: Retrieval Hooks System
*Core concept from Codified Context paper — "trigger-based context injection"*

**Problem:** Currently, when an agent calls `get_context("handleIngest")`, it gets the BFS
neighborhood. But if the agent is about to edit `internal/parser/python.go`, there are
relevant architectural rules it should know about (Python `isPythonPublic` quirk, the fact
that `attrCallQuery` was added as a bug fix, etc.). These don't appear unless the agent
explicitly annotates the node.

**The paper's solution:** Retrieval hooks fire automatically when the agent encounters
specific patterns. This prevents "hallucination bloat" by injecting *only the rules relevant
to the current context*.

**Implementation:**
- New `hooks` section in synapses.json:
  ```json
  {
    "hooks": [
      {
        "trigger_pattern": "internal/parser/**",
        "inject_note": "Parser domain: isPythonPublic() marks all non-_ functions as Exported. attrCallQuery needed for self.method() calls in Python. TypeScript requires collectTSCallSites().",
        "hook_id": "parser-domain-rules"
      },
      {
        "trigger_pattern": "internal/mcp/**",
        "inject_note": "MCP domain: all handlers must be fail-silent. Return empty mcp.CallToolResult on LLM error, not an error response. Timeout must use context.WithTimeout.",
        "hook_id": "mcp-handler-rules"
      }
    ]
  }
  ```
- When `get_context` is called: check if the entity's file matches any hook pattern, and
  append matching hook notes to the response
- When `session_init` is called with a recently-modified file: fire hooks for that file's
  pattern, injecting domain rules into `working_state`
- Store hook templates as `NodeType=HOOK` in the graph for queryability

**Impact:** This is the paper's biggest finding — rule propagation across sessions. Hook
notes attached to file patterns automatically appear whenever Claude touches that area,
eliminating repeat mistakes without requiring Claude to remember.

**Files:** `synapses/internal/config/config.go`, `synapses/internal/mcp/tools.go`,
`synapses/internal/graph/graph.go`

---

### P3-B: Multi-Agent Shared Truth (Codified Context Coordination)
**Problem:** The paper demonstrated 19 agents coordinating by reading the same "source of
truth." Synapses has `claim_work` + `peer` federation, but agents can't easily query "what
did other agents decide about this entity?"

**Improvement:**
- Elevate `decision_log` as a first-class MCP tool with richer querying:
  ```
  get_decisions(entity_name, phase, last_n=20)
  ```
- Add `decision_summary` field to `get_context` compact output: "Last 3 decisions touching
  this entity: ..."
- `session_init` returns `my_recent_decisions` (last 5 decisions by this agent_id) —
  restores agent-specific memory instantly
- Cross-agent pattern: when agent A marks a task done, auto-propagate its `decisions` as
  annotations to the nodes it modified — future agents see the reasoning in context

**Files:** `synapses/internal/mcp/task_tools.go`, `synapses/internal/mcp/brain_tools.go`,
`synapses-intelligence/internal/store/store.go`

---

### P3-C: Intelligent `brain setup` with Latency Probing
**Problem:** Users don't know which model to use for their hardware. The current `brain setup`
picks by RAM, but RAM ≠ inference speed (CPU architecture matters too).

**Improvement:**
- During `brain setup`, run a 30-token benchmark on each candidate model and measure actual
  latency before writing brain.json
- Output: "qwen3.5:0.8b → 87s/inference (❌ too slow), qwen2.5-coder:1.5b → 8s/inference (✅)"
- Set `timeout_ms` automatically to `max(measured_latency × 3, 30000)` — never guess
- Add `brain benchmark` command (standalone, outputs latency table for all installed models)

**Files:** `synapses-intelligence/cmd/brain/main.go`

---

### P3-D: Federated Cold Memory Across Projects
*Paper insight: all 19 agents read the same knowledge base*

**Problem:** If you work on both `alpha-api` and `beta-worker`, architectural decisions made
while editing `alpha-api` (e.g. "don't use Library X") are invisible when editing
`beta-worker` because brain.sqlite is per-project.

**Improvement:**
- Add `"shared_brain_db"` path to synapses.json — a second brain.sqlite that all projects
  can read from (org-level cold memory)
- When building a context packet, merge local brain.sqlite + shared_brain_db results
- Agents writing ADRs can choose `scope: "local"` vs `scope: "shared"` — shared ADRs go
  to shared_brain_db
- This enables the paper's "knowledge traveled across sessions" finding at the project-graph
  level

**Files:** `synapses-intelligence/internal/store/store.go`, `synapses/internal/brain/client.go`,
`synapses-intelligence/internal/contextbuilder/builder.go`

---

### P3-E: Version Lifecycle Automation
**Problem (meta):** During E2E testing, `brain version` reported `v0.4.0` even though all
features were v0.5.1. This required a manual fix. Suggests release process is fragile.

**Improvement:**
- Add a `Makefile` target `make bump-version VERSION=0.5.2` that updates all version
  constants across synapses + synapses-intelligence simultaneously
- Add a pre-commit hook (in `.claude/settings.json`) that checks version constants match
  the latest git tag — fails commit if stale
- Add version to the MCP `session_init` response so agents always know what version they're
  talking to

**Files:** `Makefile`, `.claude/settings.json` (hooks), `synapses/cmd/synapses/main.go`,
`synapses-intelligence/cmd/brain/main.go`

---

## Summary Table

| ID | Priority | Component | Title | Effort |
|----|----------|-----------|-------|--------|
| P0-A | 🔴 P0 | intelligence | CPU model selection + latency probing | Small |
| P0-B | 🔴 P0 | synapses | get_impact output cap (max_nodes) | Small |
| P0-C | 🔴 P0 | scout | Distillation best-effort fallback | Small |
| P1-A | 🟠 P1 | synapses | synapses.json hot-reload | Medium |
| P1-B | 🟠 P1 | scout | Cache distilled vs undistilled separately | Small |
| P1-C | 🟠 P1 | synapses | get_call_chain cross-binary explanation | Small |
| P1-D | 🟠 P1 | synapses | FTS phrase matching + n-gram index | Medium |
| P1-E | 🟠 P1 | synapses | find_entity rank: impl before tests | Tiny |
| P2-A | 🟡 P2 | all | Hot Constitution — project principles always injected | Medium |
| P2-B | 🟡 P2 | intelligence | Architectural Decision Records (ADRs) | Medium |
| P2-C | 🟡 P2 | intelligence | Domain-aware enrichment personas | Medium |
| P2-D | 🟡 P2 | synapses | Progressive context loading (detail_level param) | Medium |
| P2-E | 🟡 P2 | intelligence | Context packet quality from graph doc fallback | Small |
| P2-F | 🟡 P2 | scout | Fix search result caching in scout.db | Small |
| P3-A | 🔵 P3 | synapses | Retrieval hooks system | Large |
| P3-B | 🔵 P3 | synapses + intelligence | Multi-agent shared decision log | Large |
| P3-C | 🔵 P3 | intelligence | brain benchmark command | Medium |
| P3-D | 🔵 P3 | intelligence | Federated shared brain.sqlite | Large |
| P3-E | 🔵 P3 | all | Version lifecycle automation | Small |

---

## The Codified Context Mapping

How the paper's 3-tier framework maps to synapses v0.6 improvements:

| Paper Tier | Paper Concept | Synapses Gap | v0.6 Fix |
|-----------|---------------|--------------|----------|
| Tier 1 (Hot Constitution) | Always-active machine-readable rules | Rules only in CLAUDE.md (not machine-readable) | P2-A: Constitution nodes + P3-A: Retrieval hooks |
| Tier 2 (Domain Agents) | 19 specialized personas per code area | All enrichment uses same prompt | P2-C: Domain-aware enrichment |
| Tier 3 (Cold Memory / ADRs) | Permanent "why" storage, queryable | decision_log exists but no "why" structure | P2-B: ADRs as first-class nodes |
| Cross-tier | Shared truth across all agents | brain.sqlite is per-project | P3-D: Federated cold memory |
| Cross-tier | Retrieval hooks (just-in-time injection) | Manual annotations only | P3-A: Hook system |
| Meta | Context window ≠ understanding | Compact format good (89% reduction) but no budget awareness | P2-D: Progressive loading |

---

## Recommended Release Schedule

### v0.5.2 (small, fast) — P0 + P1
Fix the operational bugs. No architectural changes.
- P0-A: CPU model probing in brain setup
- P0-B: get_impact max_nodes cap
- P0-C: Scout distillation best-effort
- P1-B: Cache distilled separately
- P1-C: Cross-binary call_chain explanation
- P1-E: find_entity ranking fix
- P2-F: Scout search caching fix

### v0.6.0 (medium, architectural) — P1-A + P2-A through P2-E
The "Codified Context" release — introduces the constitution, ADRs, domain personas, and
progressive loading. This is where synapses becomes a full "codified context" system.
- P1-A: synapses.json hot-reload
- P1-D: FTS phrase matching
- P2-A: Hot Constitution
- P2-B: ADRs
- P2-C: Domain enrichment
- P2-D: Progressive context loading
- P2-E: Context packet doc fallback

### v0.7.0 (large, new capabilities) — P3-A through P3-E
- P3-A: Retrieval hooks
- P3-B: Multi-agent shared truth
- P3-C: brain benchmark
- P3-D: Federated cold memory
- P3-E: Version automation

---

## Key Design Principle (from the paper)

> "Codified context transforms how agents navigate complex software systems by bridging the
> gap between unlimited information and limited cognitive capacity."

Synapses already bridges this gap via compact BFS carving (89% token reduction). The v0.6
work bridges the *other* gap: not just *what* context to give, but *which rules govern it*
(constitution), *why decisions were made* (ADRs), and *which domain expertise applies*
(personas).

**The goal:** An agent starting a fresh session on any file in any project should
automatically receive, in ≤500 tokens:
1. The laws that govern that file's area (constitution + hooks)
2. A semantic summary of the entity (brain summary or doc fallback)
3. Any past decisions or known pitfalls (ADRs + decision_log)
4. The SDLC phase and quality gates

Currently, points 2 and 3 require LLM and are unreliable on CPU. The v0.5.2 bug fixes
make point 2 reliable. The v0.6.0 work adds points 1 and 3 as *deterministic, LLM-free*
features.
