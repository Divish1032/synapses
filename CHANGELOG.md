# Changelog

All notable changes to Synapses are documented in this file.
Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

---

## [Unreleased] — v1.0.3

### Added

**Dynamic Rule Engine:**
- **`upsert_rule` MCP tool**: AI agents can now create or update architectural constraints at runtime. Rules are persisted to a new `dynamic_rules` SQLite table and take effect immediately — no daemon restart required. Subsequent `validate_plan` and `get_violations` calls enforce them. Use the `rule_id` to update an existing rule.
- **Pattern suggestion in `get_project_identity`**: The graph now detects de-facto structural coupling patterns and surfaces them as `suggested_rules`. A suggestion appears when ≥85% of nodes in one directory call into another directory (minimum 3 samples). Each suggestion includes the confidence level, sample count, and ready-to-use `from_dir_pattern` / `to_dir_pattern` values for direct use in `upsert_rule`.
- **`dynamic_rules` SQLite table**: New persistence table for agent-defined rules. `CREATE TABLE IF NOT EXISTS` semantics mean existing databases pick it up automatically on next start — no migration command needed.

**Concurrency safety:**
- `rulesMu sync.RWMutex` added to `Server`: guards `s.config.Rules` during concurrent `upsert_rule` writes while allowing parallel `validate_plan` / `get_violations` reads.

**API surface discovery:**
- **`get_api_contract` MCP tool**: Scans the graph for HTTP/gRPC endpoints using framework-convention detection (net/http, Gin, Echo, Fiber, gRPC stubs, Protocol Buffers RPC) and optional `api_entries` patterns from `synapses.json`. For each endpoint returns signature, detected framework, direct callers (route registrations), and direct callees (service/repository dependencies). Answers: "what is the public API surface?"
- **`api_entries` config field**: New `synapses.json` array for custom endpoint patterns — each entry has `name_pattern` (substring), `file_pattern` (glob or directory path), and optional `node_type`. AND semantics. Useful for TypeScript NestJS decorators, Python FastAPI conventions, or project-specific naming schemes not auto-detected by conventions.

**Violation audit log (Guardian Log):**
- **`get_violation_log` MCP tool** (19th tool): Returns the audit trail of rule violations detected by `get_violations`. Same violation (same rule + from-node + to-node + edge) deduplicates by a stable SHA-1 ID — re-detection updates `last_seen` and increments `occurrences` instead of creating a new row. Optional `rule_id` filter and configurable `limit` (default 50). Violations are returned newest-first.
- **`violation_log` SQLite table**: Auto-logged whenever `get_violations` detects violations. `CREATE TABLE IF NOT EXISTS` — existing databases pick it up automatically. Indexed by `rule_id` and `last_seen` for fast filtered queries.
- **`LogViolations()`** / **`GetViolationLog()`** added to `store.Store`.

**Directional relevance weighting in `get_context`:**
- **`DirectionBoost float64`** added to `graph.CarveConfig` (default 0.2, configurable in `synapses.json` as `context_carve.direction_boost`). During BFS traversal, outgoing CALLS edges (the callee / forward direction) receive a 20% relevance multiplier. This causes the token-budget pruner to prefer call dependencies over call-sites when both are equally distant, yielding more actionable context.

**Enhanced `synapses status` output:**
- Edge type breakdown: CALLS, IMPLEMENTS, IMPORTS, EMBEDS, DEFINES counts shown separately.
- Rule stats: active rule count split by static vs dynamic, plus current violation count.
- Index metadata: indexed file count and cache parameters.
- New `graph.EdgeCountsByType()` method and `store.CountIndexedFiles()` query.

---

## [1.0.2] — 2026-02-24

### Added

**Graph intelligence:**
- **IMPLEMENTS edges**: Go parser now extracts interface method lists from AST; `internal/resolver/implements.go` runs a post-parse structural heuristic — if a struct defines every method declared in a same-package interface, a `Struct --IMPLEMENTS--> Interface` edge is added. Verified: 19 edges on the Synapses codebase itself.
- **Interface-aware `get_call_chain`**: BFS now follows `IMPLEMENTS` edges in both directions (concrete→interface and interface→concrete) when tracing call paths. Each step reports `"via": "implements"` and the response includes `"via_interface": true` when an interface boundary is crossed.
- **Cross-project CALLS edges**: Call sites are persisted to SQLite (`call_sites` table) and re-resolved after linked project graphs are merged via `MergeFrom`, so cross-repo function calls appear as `CALLS` edges in `get_context` output.
- **Federation in `cmdIndex`**: `synapses index` now loads `synapses.json`, merges linked project graphs, and re-resolves cross-project CALLS — the same federation behaviour as `synapses start`.
- **Parser metadata for 7 more languages**: `signature`, `doc`, and `line_count` metadata is now extracted for declarations in Kotlin, Scala, Groovy, Elixir, C#, Swift, and Ruby — consistent with the Go, TypeScript, Python, Rust, and Java parsers from v1.0.1.

**New MCP tools (6):**
- `get_usage_guide` — structured quick-start sequence, per-tool `when_to_use` catalogue, live entry points, and top-5 key entities. Call this at session start if unsure which tool to use.
- `get_working_state` — returns the 50 most recent file-change events recorded by the watcher (within a configurable time window) plus a best-effort `git diff --stat HEAD`. Answers "what was the developer just working on?"
- `find_orphans` — returns unexported functions and methods with fanin=0 (no callers). Useful for dead-code detection. Exported symbols and Go runtime entry points (`main`, `init`) are excluded.
- `get_federation_status` — reports which linked projects are merged into the graph, node counts per project, and the number of cross-project CALLS edges. Returns `is_federated: false` for single-project setups.

**Context quality:**
- `task_id` parameter on `get_context`: pass a task ID from `get_pending_tasks` to give linked nodes a 1.5× relevance boost in the carved subgraph. Safe to use — the boost is applied post-cache so the shared subgraph cache is not invalidated.
- `ExcludeTestFiles` in `CarveConfig` (default `true`): test file nodes are BFS-traversed (so their edges are discovered) but never emitted. Prevents test helpers from crowding the output on well-tested codebases. Configurable via `exclude_test_files` in `synapses.json`.

**Developer ergonomics:**
- `find_entity` now returns `doc` and `signature` fields in each match result — no need to follow up with `get_context` for a quick signature check.
- `search` now returns `signature` alongside each result; also scores file-path suffix matches (score=8) so `search("store/tasks.go")` finds the right file without needing the full path.
- Relative paths in `get_project_identity`: `entry_points` and `key_entities` now use repo-relative paths instead of absolute paths, saving token budget and reducing noise.

**Reliability:**
- `RemoveCallSitesForFile(file)` on `Graph`: watcher now purges stale call sites for a file before re-parsing it, preventing ghost CALLS edges from deleted functions.
- `UpsertFileMtime` in `store.go`: `persistAsync(changedFile)` performs a single-row upsert after each watcher event so `smartReindex` correctly skips already-up-to-date files on the next startup.
- Watcher integration tests (`internal/watcher/watcher_test.go`): three end-to-end integration tests covering file create, modify, and delete cycles against a real `fsnotify` watcher and graph.

**Internal:**
- `firstChildOfType(n, typ)` helper in `metadata.go` for parsers whose grammars lack named fields.
- Watcher change log: 50-entry circular buffer (`ChangeEvent` struct) records file, timestamp, nodes added/removed, and edges added per watcher event; feeds `get_working_state`.

---

## [1.0.1] — 2026-02-23

### Fixed
- **Entity resolution** (`get_context`, `find_entity`): Added `pickBestNode()` tier logic — prefers
  non-test functions/methods over structs, test helpers, and generic nodes. Eliminates the random
  resolution that previously picked test functions over implementations.
- **Package/file noise in context output**: `ExcludeTypes {NodePackage, NodeFile}` added to default
  `CarveConfig`; stdlib packages no longer consume 50%+ of the token budget.
- **DEFINES edge weight**: Lowered from 0.6 → 0.15 to prevent file-hub nodes from equalising all
  sibling functions at identical relevance.
- **Absolute paths in node IDs**: `MakeNodeID` now uses project-relative paths when `g.root` is
  set; `normalizeSubgraph()` in MCP tools trims `File` fields. Saves ~40% token overhead per node.
- **Zero CALLS edges**: Go parser now collects raw call sites during AST traversal; a post-parse
  resolver pass (`internal/resolver/resolver.go`) links them to target nodes across files using
  IMPORTS edges for package alias resolution.

### Added
- **Go parser metadata** (`extractGoDeclarationInfo`): `signature`, `doc`, `line_count` stored in
  `Node.Metadata` for all Go functions, methods, and structs.
- **Parser metadata for TS, Python, Rust, Java**: Same `extractXxxDeclInfo` pre-pass pattern applied
  to TypeScript, Python, Rust, and Java parsers.
- **Subgraph cache** (`internal/graph/cache.go`): 20-entry FIFO cache with 30-second TTL for carved
  subgraphs; invalidated by the file watcher after each incremental re-parse.
- **Per-node token estimation** (`estimateNodeTokens`): Replaces the flat `80 tokens/node` rate in
  `CarveEgoGraph` with a byte-based estimate summing all string fields and metadata.
- **Directional context** (`get_context`): Response now split into `callers`, `callees`, and
  `related` sections instead of a flat node list.
- **New MCP tools**: `get_file_context` (all entities in a file), `search` (keyword search across
  names and doc comments), `get_call_chain` (BFS path between two entities).
- **Agent task memory**: `plans` and `tasks` tables in SQLite; four new MCP tools —
  `create_plan`, `get_pending_tasks`, `update_task`, `get_plans` — enable session continuity across
  LLM conversations.
- **Incremental reindex**: `file_hashes` table stores parsed file mtimes. `smartReindex()`
  in `main.go` re-parses only changed/new files — ~8× faster on large codebases.
- **Promoted metadata columns**: `doc`, `signature`, `line_count` promoted from the JSON
  blob to first-class SQLite columns on the `nodes` table. `Open()` runs `ALTER TABLE` migrations
  transparently for existing databases.
- **Federation / monorepo support**: `Linked []string` field in `synapses.json`; paths
  resolved relative to the config file. `graph.MergeFrom()` merges linked project nodes additively
  at startup; `SaveGraph` filters out linked-project nodes by `repoID::` prefix so they are never
  persisted to the primary store.
- `MinRelevance: 0.25` default in `CarveConfig` — prunes low-signal nodes before the token budget
  is applied.
- `synapses init` command: one-shot index + `.mcp.json` writer with guided next-step output.

### Changed
- `Server.New()` now accepts `*store.Store` as a third argument so MCP task tools can persist data.
- `WalkDir` now returns `(map[string]int64, error)` — the map contains parsed file mtimes.
- Default `CarveConfig.DecayFactor` = 0.5; `MaxDepth` = 2; `TokenBudget` = 4000.

---

## [1.0.0] — 2026-02-XX *(initial release)*

### Added
- Multi-language AST parsing via tree-sitter: Go, TypeScript, JavaScript, Python, Java, Kotlin,
  Scala, Groovy, Rust, C, C++, C#, Swift, Ruby, PHP, Lua, Elixir, Protobuf.
- In-memory code graph with BFS-based ego-subgraph carving (`CarveEgoGraph`).
- SQLite persistence (`modernc.org/sqlite`, no CGo) with schema migrations.
- MCP server over stdio: `get_project_identity`, `get_context`, `find_entity`,
  `validate_plan`, `get_violations`.
- Architectural rule engine (`synapses.json`) with forbidden-edge patterns.
- File watcher (`fsnotify`) for incremental graph updates on save.
- CLI: `init`, `start`, `index`, `status`, `list`, `reset`, `version`.
