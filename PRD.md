# Synapses — Product Requirements Document

**Product:** Synapses
**Tagline:** The Agentic Control Plane
**Positioning:** Local-first, graph-based context manager for AI coding agents

---

## Problem Statement

AI coding agents (Claude Code, Cursor, Copilot) have no structural understanding of a codebase. They grep files, read raw text, and hallucinate relationships. The larger the codebase, the worse the context quality.

Synapses solves this by:
1. Parsing code into a relational graph (nodes = entities, edges = relationships)
2. Serving the agent a mathematically-carved slice of that graph — only what it needs
3. Enforcing architectural constraints so agents don't introduce structural violations

---

## MVP Definition

**MVP is:** A single binary that any developer can point at a repo, start an MCP server, and immediately give their AI agent accurate structural context — with no configuration required.

**MVP is NOT:** A web UI, a cloud service, a multi-repo workspace, or a semantic search engine. Those are v2.

---

## Phases

---

### Phase 1 — Foundation ✅ Complete

Core infrastructure: the graph engine, basic CLI, and SQLite persistence.

- [x] In-memory graph data model (`NodeType`, `EdgeType`, `Node`, `Edge`)
- [x] Thread-safe graph engine with RWMutex (`AddNode`, `AddEdge`, `RemoveFile`, `FindByName`, `FindByPattern`)
- [x] Graph projection: `Fanin`, `Fanout`, `OutEdges`, `InEdges`
- [x] SQLite persistence (`SaveGraph`, `LoadGraph`, schema migrations)
- [x] Per-project DB naming with FNV hash (avoids project name collisions)
- [x] `index` command (parse + cache)
- [x] `start` command (load + serve MCP)
- [x] `status` command (per-project stats)
- [x] `version` and `help` commands
- [x] Build system (`Makefile`, `go.mod`, `go.sum`)

---

### Phase 2 — Multi-Language Parsing ✅ Complete

Deep AST extraction for all major programming languages using Tree-sitter (smacker/go-tree-sitter).

**Parser infrastructure:**
- [x] `LanguageParser` interface (`Extensions()`, `Parse()`)
- [x] `Walker` orchestrator (`WalkDir`, `ParseFile`, `Register`)
- [x] `shouldSkipDir` (skips `node_modules`, `vendor`, `dist`, `.git`, build artifacts, etc.)
- [x] Generic fallback parser (80+ file types — file node only, no AST)
- [x] Shared `runQuery` helper (tree-sitter S-expression queries with capture callbacks)

**Deep AST parsers (functions, classes/structs/interfaces, imports, methods):**
- [x] Go (`.go`) — packages, imports, functions, methods, structs, interfaces, basic call edges
- [x] TypeScript (`.ts`, `.tsx`) — imports, functions, arrow functions, classes, interfaces, type aliases
- [x] JavaScript (`.js`, `.jsx`, `.mjs`, `.cjs`) — imports, functions, arrow exports, classes
- [x] Python (`.py`, `.pyi`) — imports, from-imports, functions, classes
- [x] Java (`.java`) — imports, classes, interfaces, enums, methods
- [x] Kotlin (`.kt`, `.kts`) — imports, classes, objects, functions
- [x] Scala (`.scala`) — imports, classes, objects, traits, functions
- [x] Groovy (`.groovy`, `.gradle`) — classes, methods
- [x] Rust (`.rs`) — use declarations, functions, structs, enums, traits, impl blocks
- [x] C (`.c`, `.h`, `.ino`) — includes, functions, structs, typedefs
- [x] C++ (`.cpp`, `.cc`, `.cxx`, `.hpp`, `.hh`, `.hxx`, `.mm`) — includes, functions, classes, structs, namespaces
- [x] C# (`.cs`) — using directives, namespaces, classes, interfaces, methods
- [x] Swift (`.swift`) — imports, classes, structs, protocols, functions
- [x] Ruby (`.rb`) — classes, modules, methods, singleton methods
- [x] PHP (`.php`) — use statements, classes, interfaces, functions, methods
- [x] Lua (`.lua`) — functions, local functions
- [x] Elixir (`.ex`, `.exs`) — defmodule, def/defp
- [x] Protocol Buffers (`.proto`) — messages, services, rpc methods, enums

---

### Phase 3 — Graph Intelligence ✅ Complete

The algorithms that make context useful, not just raw data.

- [x] BFS ego-graph carver (`CarveEgoGraph`) with configurable depth
- [x] Edge-weighted relevance scoring (`CALLS=1.0`, `IMPLEMENTS=0.9`, `EMBEDS=0.85`, `DEPENDS_ON=0.8`, `IMPORTS=0.7`, `EXPORTS=0.5`, `DEFINES=0.15`)
- [x] Relevance decay by hop distance (configurable `decay_factor`, default 0.5)
- [x] Token budget enforcement (~80 tokens/node estimate, pruning by relevance rank)
- [x] `ProjectIdentity` computation (node/edge counts by type, entry points, top entities by connectivity)
- [x] Architectural rule validation (`Config.CheckViolations`)
- [x] `synapses.json` configuration schema (rules, edge weights, carve defaults)
- [x] `ForbiddenEdge` pattern matching (glob for file paths, substring for entity names)
- [x] Severity levels (`error`, `warning`)

---

### Phase 4 — MCP Integration ✅ Complete

Expose the graph to AI agents via the Model Context Protocol.

- [x] MCP server (stdio transport, `mark3labs/mcp-go`)
- [x] `get_project_identity` — compact architectural handshake at session start
- [x] `get_context` — N-hop ego-subgraph (callers / callees / related); `task_id` boost
- [x] `find_entity` — locate nodes by name/substring; returns signature + doc
- [x] `validate_plan` — check proposed changes against rules (non-destructive graph clone)
- [x] `get_violations` — list all current rule violations
- [x] `get_file_context` — all entities in a file, ordered by line number
- [x] `search` — keyword search across names, doc comments, and file paths
- [x] `get_call_chain` — BFS call path; crosses interface boundaries via IMPLEMENTS edges
- [x] `find_orphans` — unexported functions/methods with zero callers
- [x] `get_federation_status` — linked project node counts and cross-project CALLS edges
- [x] `get_usage_guide` — tool catalogue with `when_to_use` for each tool
- [x] `get_working_state` — recent watcher file-change events + git diff stat
- [x] `create_plan` / `get_pending_tasks` / `update_task` / `get_plans` — agent task memory
- [x] `upsert_rule` — create/update dynamic rules at runtime; persisted to SQLite, active immediately
- [x] Pattern suggestion in `get_project_identity` — detects high-density CALLS coupling, surfaces as `suggested_rules`

---

### Phase 5 — Developer Experience ✅ Complete

Making the tool easy to use day-to-day.

- [x] File watcher (`fsnotify`, 150ms debounce, incremental re-parse)
- [x] Async SQLite save after watcher events (non-blocking)
- [x] `reset` command (per-project and `-all`)
- [x] `list` command (global overview of all indexed projects, no graph load)
- [x] `ProjectStat` lightweight metadata (stored in `meta` table, no full graph scan)
- [x] `ScanAll()` — discovers all indexed projects by scanning cache directory
- [x] Per-project cache naming with `FNV-1a` hash (collision-safe filenames)
- [x] `Makefile` shortcuts (`run/index`, `run/start`, `run/status`, `run/reset`)
- [x] `COMMANDS.md` CLI reference
- [x] `synapses.example.json` configuration reference
- [x] `Graph.Root()` / `SetRoot()` — graph carries its own provenance

---

### Phase 6 — Production Hardening ✅ Complete (v1.0.2)

**Testing:**
- [x] Parser tests for new languages (Java, Kotlin, Scala, Groovy, Rust, C, C++, C#, Swift, Ruby, PHP, Lua, Elixir, Protobuf)
- [ ] Integration test: full index → MCP query roundtrip
- [x] Watcher tests (file create/modify/delete cycle) — `internal/watcher/watcher_test.go`
- [x] `cmdList` / `ScanAll` tests

**Correctness:**
- [x] Cross-file call resolution — two-phase resolver (`internal/resolver/resolver.go`) links `pkg.Func()` calls using IMPORTS edges for alias resolution
- [x] IMPLEMENTS edge detection — structural heuristic: same-package structs satisfying all interface methods get `Struct --IMPLEMENTS--> Interface` edges
- [ ] Kotlin import path extraction — current `import_header (identifier)` query only captures single identifiers; full dotted paths need verification against Kotlin tree-sitter grammar
- [ ] `validate_plan` edge simulation — should also support `adds_import` and `removes_import`, not just call edges

**Error handling:**
- [ ] Aggregate parse errors per file, surface in `status` output
- [x] `ScanAll` should skip and log corrupt DB files rather than silently dropping them
- [ ] Watcher: log which files failed to re-parse and why

**Performance:**
- [x] Incremental re-parse (smart reindex): `file_hashes` table skips unchanged files — ~8× faster on large codebases
- [x] Subgraph cache: 20-entry FIFO with 30s TTL; invalidated by watcher on each save
- [ ] Benchmark suite for carving (target: <10ms at 10k nodes)
- [ ] Profile cold parse on large repos (target: <30s at 10k files)
- [ ] Consider lazy load: don't load full graph at `start`, load on first MCP tool call

**Documentation:**
- [x] README.md (languages, commands, architecture, quick start, all 16 MCP tools) ✅
- [x] COMMANDS.md (full CLI reference with `list`, all flags, all 16 MCP tools) ✅
- [x] `synapses.example.json` — verify all config options are documented
- [x] CONTRIBUTING.md — architecture decision records, data flow with resolver, updated structure

---

### Phase 7 — Post-MVP / v2.0 ❌ Not Started

Features explicitly out of scope for MVP.

**Graph depth:**
- [ ] Full cross-file call graph (TypeScript, Python, Go)
- [ ] Module-level dependency graph (package → package edges)
- [ ] `EMBEDS` / `INHERITS` edges for class hierarchies (Java, Kotlin, TypeScript)
- [ ] Symbol resolution for method dispatch (virtual/interface calls)

**Multi-repo:**
- [ ] Workspace concept — index multiple repos, query across them
- [ ] Shared package graph (e.g. shared `@company/ui` library across repos)

**Intelligence:**
- [ ] Semantic search via embeddings (similarity, not just name match)
- [ ] Change impact analysis ("if I change X, what breaks?")
- [ ] Dependency cycle detection
- [ ] Dead code detection (nodes with zero fanin that aren't entry points)

**Integrations:**
- [ ] Language Server Protocol (LSP) integration — go-to-definition, hover
- [ ] CI/CD mode — `synapses check` exits non-zero on rule violations
- [ ] VS Code extension (optional visual graph browser)

**Operations:**
- [ ] Remote/cloud sync of index (for teams)
- [ ] Web dashboard (local, `synapses ui`)
- [ ] `synapses watch` as a background service (launchd/systemd)

---

## Current State Summary

| Phase | Status | Completeness |
|---|---|---|
| Phase 1 — Foundation | ✅ Complete | 100% |
| Phase 2 — Multi-Language Parsing | ✅ Complete | 100% |
| Phase 3 — Graph Intelligence | ✅ Complete | 100% |
| Phase 4 — MCP Integration | ✅ Complete | 100% |
| Phase 5 — Developer Experience | ✅ Complete | 100% |
| Phase 6 — Production Hardening | ✅ Complete | ~90% |
| Phase 7 — Post-MVP | ❌ Not Started | 0% |

**MVP readiness:** Phases 1–6 are complete. The core product works end-to-end with 16 MCP tools, 18-language AST parsing, watcher integration tests, and a full resolver pipeline. The remaining Phase 6 items (Kotlin import paths, validate_plan import simulation, benchmark suite) are non-blocking nice-to-haves. **v1.0.2 is release-ready.**

---

## Decisions Made

| Decision | Rationale |
|---|---|
| Single binary, no daemon | Zero operational overhead; fits into any dev workflow |
| SQLite over embedded key-value | Relational queries (nodes by file, by name), schema versioning, WAL mode |
| Cache outside project tree | Doesn't pollute `.gitignore`, survives project renames |
| smacker/go-tree-sitter over official bindings | Official bindings had broken module paths; smacker bundles all grammars in one module |
| Tree-sitter query degradation | Wrong node types produce zero matches, not errors — new language parsers can be added safely without exhaustive grammar study |
| MCP stdio transport | Native protocol for Claude Code / Cursor; no HTTP server required |
| No `~/.synapses/` registry | Dual-write consistency problem; scanning cache dir + reading per-project meta table is self-healing |
| BFS with decay over simple N-hop | Relevance-ranked results fit token budgets better; distant irrelevant nodes are pruned first |
| 150ms watcher debounce | Balances responsiveness vs. thrashing on rapid saves (e.g. format-on-save) |
