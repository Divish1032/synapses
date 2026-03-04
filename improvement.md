# synapses improvement log

## v0.5.1 — Universal Pre-Enrichment + Prose Context Format (2026-03-03)

### Changes

#### P4 — Universal Async Pre-Enrichment (Remove 100-Node Cap)
`cmd/synapses/main.go` `bulkIngestToBrain()`:
- Removed `const maxIngest = 100` cap — all non-package/non-file nodes are now ingested
- Dropped the "high-value only" filter (fanin>3 / exported+used / entry point)
- Increased concurrency from 4 → 8 workers (0.8B handles more parallel requests)
- Sort by fanin (most-connected first) preserved
**Effect:** A 500-node codebase that previously got partial coverage (~100 nodes) now
gets full prose briefings for all nodes in ~25min background process. get_context
then reads from brain.sqlite cache instantly.

#### P5 — Prose Briefing Prompt in Ingestor
`synapses-intelligence/internal/ingestor/ingestor.go` `promptTemplate`:
Updated from 1-sentence summary to 2-3 sentence technical briefing format covering:
what it does, its role in the system, and important patterns/concerns.
**Effect:** richer prose summaries that give Claude natural-language context instead
of raw code/doc snippets.

#### P6 — Compact Go Serializer + get_context `format` Parameter
New file `internal/mcp/digest.go`: `serializeCompact(dc *directionalContext) string`
- Root entity: header line + prose summary (brain briefing or AST doc fallback)
- Calls/Called-by sections with entity names in natural language
- Warnings from GraphWarnings + Concerns
- Architectural insight from brain enrichment
- Callee detail blocks for high-relevance (≥0.6) nodes
`internal/mcp/tools.go`: reads `format` param in `handleGetContext()`; if `format="compact"`,
returns `serializeCompact(dc)` as plain text instead of JSON blob.
`internal/mcp/server.go`: added `format` parameter to `get_context` tool registration.

| Format | Tokens | Use case |
|--------|--------|----------|
| `json` (default) | 2000–3800 | backward compat, machine parsing |
| `compact` | 400–600 | Claude context — 80% token reduction |

---

## v0.5.0 — E2E Test Run (2026-03-03)

---

### BUGS (must-fix)

#### BUG-S01 — validate_plan file-path matching broken on combined root index
**Severity:** High
**Observed:** `validate_plan` returns "no matching node in graph" for paths like
`synapses/internal/mcp/tools.go`. The tool uses repo-relative paths but the graph
stores IDs as `<repo>::<path>`. When the MCP server serves a combined
`synapses-os` root index, the file-path strip logic doesn't handle the project
subdirectory prefix correctly.
**Fix:** Normalise incoming file paths against the graph's repo prefix before
matching. Try `<repoRoot>::<path>` → strip `<repoRoot>/` from incoming path.

#### BUG-S02 — get_impact returns `tiers: null` instead of empty array
**Severity:** Low
**Observed:** Querying `get_impact` on `Server.handleGetContext` (a tool handler
registered by function pointer, not called by Go code) returns `tiers: null`.
Callers should get `tiers: []` for a clearly empty result, not null, which
breaks JSON consumers that do `tiers.length`.
**Fix:** Initialise `tiers` to `[]` (empty slice) before returning, never nil.

#### BUG-S03 — MCP server does not pick up synapses.json changes without restart
**Severity:** Medium
**Observed:** After adding `brain.url` + `scout.url` to `synapses.json`, the
running MCP server (managed by Claude Code) continued with no brain/scout
client. Config is read once at startup.
**Fix:** Add a SIGHUP handler or `config.Watch()` loop that re-reads
`synapses.json` every 60s and calls `server.SetBrainClient()` /
`server.SetScoutClient()` if the URL has changed. Alternatively add a
`session_reload_config` MCP tool.

#### BUG-S04 — Brain/scout not configured by default — silent failure in get_context
**Severity:** Medium
**Observed:** A freshly-installed `synapses` binary has no `synapses.json`.
`get_context` returns graph-only results with zero indication that brain
enrichment is missing. No `brain_unavailable: true` field or warning.
**Fix:** In `handleGetContext`, if `getBrainClient()` returns nil, add a
`"brain": "not configured — add brain.url to synapses.json"` hint to the
response JSON. Same for scout tools returning "scout unavailable".

---

### USABILITY ISSUES

#### UX-S01 — get_impact requires exact method syntax (`Server.X`), find_entity accepts substrings
**Observed:** `get_impact("handleGetContext")` → "entity not found".
`find_entity("handleGetContext")` → finds it. Two tools, two lookup behaviours.
**Improvement:** `get_impact` should fall back to `Graph.FindByName` (same
fuzzy lookup as `find_entity`) when the exact ID isn't found. Return a "did you
mean: Server.handleGetContext?" hint if zero results.

#### UX-S02 — create_plan auto-links nodes with poor relevance
**Observed:** Creating a plan titled "Test synapses core MCP tools" auto-linked
`synapses-scout/src/scout/server.py::search` — completely irrelevant. The
auto-linker used BM25 on task title/description and picked the wrong project.
**Improvement:** Auto-linking should be scoped to the repo the MCP server is
serving. Cross-project node links should require explicit `linked_nodes` from
the caller. Add a `confidence` threshold (e.g. ≥0.7) below which auto-links
are dropped.

#### UX-S03 — `search(mode="semantic")` is misleading — it's BM25, not vector
**Observed:** `search("rate limiting throttle concurrency")` returns 0 results
with hint "try broader terms". No semantic/embedding expansion occurs. The mode
name suggests vector similarity but the implementation is FTS BM25.
**Improvement:** Either:
  a. Rename to `mode="fts"` and `mode="keyword"`.
  b. Or implement actual embedding-based search using a local model
     (sentence-transformers/nomic-embed) for true semantic retrieval.
  c. At minimum, document clearly what "semantic" means in the tool description.

#### UX-S04 — get_file_context("server.go") returns mixed entities from all files with that name
**Observed:** The combined `synapses-os` root index has three `server.go` files
(synapses/mcp, synapses-intelligence/server, synapses-scout/server.py). All
entities are returned mixed together with no file-path grouping or attribution.
**Improvement:** Show the file path for each entity in `get_file_context`
output, and group results by file when multiple files match the suffix. Allow
full-path overrides (e.g. `get_file_context("internal/mcp/server.go")`).

#### UX-S05 — get_working_state ignores config/JSON file changes
**Observed:** After creating `synapses.json` and editing `settings.json`, both
returned zero recent changes. The watcher watches source files only (.go, .py,
.ts etc.) and ignores config files.
**Improvement:** Include `*.json`, `*.yaml`, `*.toml`, `Makefile`,
`Dockerfile` changes in `get_working_state`. These are often the most
significant indicator of configuration changes relevant to the agent.

---

### ARCHITECTURE GAPS

#### ARCH-S01 — No federation: cross-project graph edges don't exist
**Observed:** The `synapses-core` MCP server (serving `synapses/`) has no graph
edges into `synapses-intelligence/` or `synapses-scout/`. The call from
`brain/client.go` to `localhost:11435` is an HTTP boundary the static analyser
cannot cross.
**Future:** Add a `federation` mode where HTTP calls matching known sidecar URLs
(brain, scout) generate synthetic `CALLS_REMOTE` edges. This would let
`get_call_chain("cmdStart", "Enricher.Enrich")` work end-to-end across
projects. Tracked under `is_federated: false` in `get_project_identity`.

#### ARCH-S02 — `session_init` tool exists but is not in CLAUDE.md startup ritual
**Observed:** `handleSessionInit` consolidates `get_pending_tasks` +
`get_project_identity` + `get_working_state` into one call. But CLAUDE.md
still lists the three-step ritual. Agents are paying 3× round-trips at startup.
**Fix:** Update CLAUDE.md to: `session_init()  ← FIRST: single-call bootstrap`.
Remove the three-step ritual. Reduces session start from ~3 MCP calls to 1.

#### ARCH-S03 — No MCP tool for reading a stored brain summary by name (only by node_id)
**Observed:** `GET /v1/summary/{nodeId}` requires the exact stored `node_id`
(e.g. `synapses::cmdStart`). There is no `GET /v1/summary?name=cmdStart` fuzzy
lookup. Agents that don't know the node_id cannot retrieve summaries from the
graph context.
**Improvement:** Add a `search_summary` MCP tool or query param that accepts a
name/fuzzy string, looks up matching node IDs in brain.sqlite, and returns
summaries for all matches.

#### ARCH-S04 — No config hot-reload; synapses.json changes require MCP server restart
See BUG-S03. In a production setup, the MCP server is a long-lived process
managed by Claude Code. There is currently no way to reconfigure brain/scout
URLs without restarting the entire Claude Code session.

---

### PERFORMANCE NOTES

- **Index time:** `synapses index` on 90 files took ~2s — acceptable.
- **get_context latency:** <50ms on warm cache — excellent.
- **get_project_identity:** ~20ms — excellent.
- **search(keyword):** ~5ms — excellent.
- **search(semantic):** ~10ms — excellent (BM25, not vector).
- **Concern:** Adding vector embeddings (ARCH gap above) would add ~100-500ms
  per query unless pre-computed and stored in SQLite.
