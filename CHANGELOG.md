# Changelog

All notable changes to Synapses are documented here. This project adheres to [Semantic Versioning](https://semver.org/).

## [0.8.0] - 2026-03-28

### Changed
- **MCP Tool Consolidation (58→12)** — Merged 58 MCP tools into 12 via action/mode/phase dispatcher params. Research shows LLM accuracy drops past 20 tools; this puts Synapses at the optimal range. Tools: `session_init`, `search`, `get_context`, `get_file_context`, `get_impact`, `validate`, `memory`, `end_session`, `tasks`, `rules`, `annotate`, `lookup_docs`.
- **5 Tools → MCP Resources** — Read-only tools (`get_repo_map`, `get_edge_types`, `get_my_analytics`, `get_decision_log`, `query_graph`) moved to MCP Resources (`synapses://repo-map`, `synapses://edge-types`, `synapses://analytics`, `synapses://decision-log`, `synapses://query/{q}`).
- **11 Tools Removed** — `discover_tools` (unnecessary with 12 tools), graph edit tools (`link_entities`, `unlink_entities`, `confirm_edge`), coordination tools (`get_agents`, `get_events`, `send_message`, `get_messages`), internal tools (`rank_candidates`, `benchmark`, `export_knowledge`).

### Added
- **Work Ledger** — Ambient cross-session coordination without explicit tools. Every MCP call passively records entity/file signals to SQLite. Overlap detection (Tier 1: same file/entity, Tier 2: 1-hop graph neighbor) injects alerts into tool responses. Per-session watermark dedup prevents noise. `session_init` returns full cross-session briefing. 24h auto-prune.

### Fixed
- **handleRecall panic** — Pre-existing `index out of range [0]` panic when recall returned zero results (episode_tools.go:736).
- **Knowledge mode validate crash** — `validate(phase=post/list/full)` would panic on nil graph in knowledge mode. Added nil-graph guard in dispatcher.

## [0.8.0-pre] - 2026-03-23

### Added
- **Socket Activation** — Daemon now supports launchd (macOS) and systemd (Linux) socket activation. The OS holds port 11435 during daemon restarts, so AI agents see a brief delay instead of "connection refused". New files: `socket_activation_darwin.go`, `socket_activation_linux.go`, `socket_activation_other.go`.
- **Default Service Install** — `synapses init` automatically registers the daemon as a system service (launchd/systemd) if not already installed. No more forgotten `synapses daemon install` step. Auto-restarts on crash with zero user action.
- **Project Persistence** — Registered projects saved to `~/.synapses/projects.json`. On daemon restart, all known projects are eagerly warmed in the background so the first MCP request is instant instead of blocking for 10-60s on graph indexing.
- **Structured Daemon Lifecycle Events** — JSON events (`daemon_starting`, `daemon_ready`, `daemon_stopping`, `daemon_panic`) written to daemon.log. Diagnostic ladder: empty log = binary never ran, `starting` without `ready` = crash during init.
- **Panic Recovery in MCP Handler** — `defer recover()` wraps the `/mcp` HTTP handler. Panics in tool handlers or project init log full stack traces and return HTTP 500 instead of crashing the daemon. Combined with mcp-go's `WithRecovery()` middleware for tool-level protection.
- **Per-Project Circuit Breaker** — If a project panics 3 times in 5 minutes, it's temporarily disabled to protect other projects. Auto-re-enables after 10 minutes.
- **Suffix Cycle Detection in Loop Guard** — Detects not just repeated identical calls (A,A,A) but alternating patterns (A,B,A,B) and short cycles (A,B,C,A,B,C) of any length up to 10. Window increased from 20 to 30 calls. Prevents agents from gaming the guard by alternating between tools.
- **Agent-Scoped Rate Limiting** — Rate limit buckets keyed by `agent_id:project_id` instead of MCP session ID. Reconnecting no longer resets rate limits.
- **Systemd Socket Unit** — New `synapses.socket` unit for zero-downtime restarts on Linux. The daemon service unit now includes `Requires=synapses.socket`.

### Changed
- **Verified Init Flow** — `synapses init` now fails loudly if the daemon can't start, instead of silently writing `.mcp.json` pointing to a dead server. Prints diagnostics (log tail, port status, binary path) on failure.
- **Loop Guard No Longer Resets on File Save** — File saves are not evidence of agent progress (agents save files as part of their loop). The guard now auto-resets only when the agent calls a different tool (fingerprint change) or after 60s of inactivity.
- **Daemon Health Verification** — Init wizard verifies the MCP endpoint is reachable before writing agent configs and printing "Synapses is ready!".

### Dependencies
- Added `github.com/tprasadtp/go-launchd` — Pure Go launchd socket activation (no cgo)
- Added `github.com/coreos/go-systemd/v22` — systemd socket activation and file descriptor passing

---

## [0.7.2] - 2026-03-22

### Added
- **Web Console** — Built-in management UI served at `http://localhost:11435`. Dashboard shows live index stats, task queues, episodic memory, agent activity, and one-click reindex. Embedded in the binary via `//go:embed`; no separate install.
- **`synapses update` command** — Check for new releases and install the latest binary with a single command. Background check runs on daemon start and surfaces an update hint in `synapses version` and `session_init` if a newer release is available.
- **Disk override for web console** — Drop a custom `index.html` into `~/.synapses/console/` and the daemon serves it instead of the embedded UI. Enables hotfixes and custom dashboards without rebuilding the binary.
- **CSRF protection** — All mutation endpoints on the admin API require a session token. Fail-closed: if token generation fails, mutations are rejected.
- **Update notification in `session_init`** — If a newer Synapses release is cached, AI agents receive an `update_available` hint in the `session_init` response.
- **Admin API endpoints** — `GET /api/admin/update-check`, `POST /api/admin/projects/{path}/reindex`, `GET /api/admin/services`, and supporting routes for the web console.

### Fixed
- **Reindex correctness** — `POST /api/admin/projects/{path}/reindex` now fully tears down and rebuilds the project instance (was previously only rebuilding the FTS index from stale in-memory data).
- **Ollama pull write timeout** — `/api/admin/ollama/pull` excluded from HTTP write deadline so large model downloads do not time out.
- **Dev embed** — `web.ConsoleFS` in dev mode is now `os.DirFS("web")` so `fs.Sub(ConsoleFS, "console/dist")` resolves correctly without a production build.

---

## [0.7.1] - 2026-03-09

### Added
- **`lookup_docs` Scout Integration** — New `LookupDocsRequest`/`LookupDocsResponse` types in scout client for one-shot documentation lookup.

### Fixed
- **CI Lint Fixes** — Resolved 15 golangci-lint errors (goimports formatting, staticcheck S1011) across 8 files.
- **Windows Path Compatibility** — `topLevelPackage()` now uses `filepath.ToSlash()` for correct path normalization on Windows.
- **Release Archives** — Removed stale `COMMANDS.md` from goreleaser archive config.

### Infrastructure
- **Distribution Setup** — Homebrew formulas (`synapses.rb`, `brain.rb`), GitHub issue templates (bug report, feature request), VS Code extension publish workflow.

---

## [0.7.0] - 2026-03-09

### Added
- **Phase 1: Agent Message Bus** — SQLite-based inter-agent communication with `agent_messages` table. New MCP tools: `send_message`, `get_messages`, `mark_read`. Broadcast support (to_agent="") and unread message injection in `session_init`.
- **Phase 2: Episodic Memory** — `episodes` table + `episodes_fts` FTS5 virtual table. New MCP tools: `remember`, `recall`, `get_episodes`, `check_plan_safety`, `get_rule_candidates`. FTS BM25 recall with failure episode filtering.
- **Phase 3: SIL Verification** — `verifier.go` with deterministic claim verification against graph topology. Annotations: [✓] verified, [⚠ UNVERIFIED: actual value]. Rule-based, fast (no LLM required).
- **Phase 4: Vector Embedding Search (Plan B)** — Brute-force Go cosine similarity ANN in `internal/store/embeddings.go`. `node_embeddings` table with `content_hash` column for stale detection. `EmbedBatch` client support (16-node chunks). Batch embeddings for all nodes on index.
- **Phase 5: Cross-Project Propagation** — `notifyCrossProjectImpact` in watcher detects cross-project dependencies, broadcasts notifications via agent message bus. 5 passing tests.
- **llama-server Backend** — No Ollama required. Subprocess-managed llama-server with OpenAI-compatible API. CPU/GPU auto-detect (Metal, CUDA, ROCm). Setup: `brain setup --llama-server`.
- **4-Layer Memory Hierarchy** — Documented in CLAUDE.md: Layer 1 (events), Layer 2 (session_state), Layer 3 (episodes), Layer 4 (session_init injection).
- **New MCP Tool: `prepare_context`** — Intent-based context assembly. Intents: modify/understand/review/debug/add/plan. Returns phase-aware Context Packet.
- **MCP Resources (v0.7.0)** — Push-based notifications for `synapses://active-context`, `synapses://file/{path}`, `synapses://violations`.

### Changed
- **Batch Embedding by Default** — `embedAllNodes` now uses 16-node chunks; per-node fallback on batch failure with non-fatal error handling.
- **Content-Hash Invalidation** — Embeddings invalidated by SHA256(name+sig+doc)[:4] hex, not by time. Stale embeddings detected and re-embedded automatically.
- **Brain Timeout** — `synapses.json` brain.timeout_sec now defaults to 60s (was 5s) to accommodate CPU-only inference.
- **E2E Scoring Framework** — 4-leg validation (core, intelligence, scout, hybrid context) with composite scoring and production vs CLI estimation.

### Fixed
- **BUG-I01**: Qwen3.5 thinking mode now guarded to qwen3.x only. Non-Qwen models no longer timeout with incorrect thinking prefix.
- **BUG-I02**: Brain db_path tilde expansion fixed. Uses absolute paths on all platforms.
- **Cross-Project Edge Handling** — AddEdge no longer silently drops edges whose endpoints don't exist; federation merge order is now correct.

### Deprecated
- **Fine-tuning Pipeline** — SFT/GRPO training (Unsloth/Qwen3.5 VLM Dynamo bug) abandoned in favor of stock Qwen3.5:4b via llama-server.

---

## [0.6.1] - 2026-03-04

### Added
- **GPU Auto-Detection** — `brain setup` probes for Metal (macOS), CUDA, ROCm; writes tiered model assignments.
- **4-Tier Nervous System** — Tier 0 (Reflex), Tier 1 (Sensory), Tier 2 (Specialist), Tier 3 (Architect). Per-tier model config.
- **ADR Support** — `upsert_adr` and `get_adrs` MCP tools. Brain backend stores architectural decisions in brain.sqlite.

### Fixed
- **BUG-S01**: `synapses.json` hot-reload now works correctly. Config changes detected by watcher and applied without restart.
- **CPU Model Selection** — qwen3.5 models too slow on CPU. Switched to qwen2.5-coder:1.5b and 7b for Tiers 0/1/2/3 on CPU.

---

## [0.6.0] - 2026-03-01

### Added
- **Intelligence Sidecar (synapses-intelligence)** — 4-tier LLM system (ingestor, enricher, guardian, orchestrator). HTTP API on :11435.
- **Episodic Memory Schema** — `episodes` and `episodes_fts` tables (Foundation for Phase 2).
- **Vector Embeddings Schema** — `node_embeddings` table (Foundation for Phase 4).
- **Agent Messages Schema** — `agent_messages` table (Foundation for Phase 1).
- **Context Packet Assembly** — `contextbuilder` in synapses-intelligence. Phase-aware packet generation.
- **Scout Integration** — Web search, fetch, distillation via synapses-scout sidecar.
- **Detail Levels in `get_context`** — `summary` (50 tokens), `neighbors` (200 tokens), `full` (400-600 tokens).
- **MCP Resources Framework** — Push-based notifications for context and violations.

### Changed
- **Compact Format** — `get_context(format="compact")` returns prose instead of JSON (80% token reduction).
- **writeTimeout** — Brain timeout changed from hardcoded 30s to 2×TimeoutMS (default 120s).

---

## [0.5.2] - 2026-02-15

### Added
- **Scout Sidecar** — Web search, fetch, distillation. First version (v0.0.5).
- **Brain `setup` Command** — Interactive setup with model probing and latency benchmarking.
- **FTS Prefix Matching** — `sanitizeFTSQuery` with term* suffix for prefix search.
- **Cross-Binary Call Explanations** — `get_call_chain` shows topLevelPackage context.

### Fixed
- **BUG-001**: Brain client hardcoded 2s timeout removed (uses caller timeout).
- **BUG-002**: Intelligence default TimeoutMS increased from 3000→30000.
- **BUG-003**: Task tools.go arrays fixed with type switch for string/[]interface{}.

---

## [0.5.1] - 2026-02-10

### Added
- **Compact Prose Format** — `get_context(format="compact")` → 400-600 tokens (vs 2000-3800 raw).
- **ingestor + enricher** — Tier 0 and Tier 2 working end-to-end.
- **Prose-over-JSON** — 0.8B generates briefings at index time; Claude receives prose not JSON.

### Changed
- **Token Efficiency** — 89% reduction in context size via prose-over-JSON principle.

---

## [0.5.0] - 2026-02-05

### Added
- **Parser Extended** — Added support for JavaScript, Python, Java, C, C++, Rust, Ruby, PHP.
- **FTS5 Semantic Search** — BM25 ranking across all entities.
- **Call-Site Resolver** — Incremental cross-file CALLS edge resolution.
- **Watcher** — File change detection and incremental re-indexing via fsnotify.

---

## [0.4.0] - 2026-01-20

### Added
- **MCP Protocol** — Implemented mark3labs/mcp-go server over stdio.
- **49+ Language Parsers** — Tree-sitter-based parser suite (Go, TypeScript, Rust, Java, and 50+ others).
- **Core MCP Tools** — `get_context`, `find_entity`, `search`, `get_call_chain`, `get_impact`, `annotate_node`.
- **SQLite Persistence** — modernc.org/sqlite (pure Go, no CGo).
- **Graph Index** — Columnar adjacency representation for fast BFS.

---

[0.7.2]: https://github.com/SynapsesOS/synapses/compare/v0.7.1...v0.7.2
[0.7.1]: https://github.com/SynapsesOS/synapses/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/SynapsesOS/synapses/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/SynapsesOS/synapses/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/SynapsesOS/synapses/compare/v0.5.2...v0.6.0
[0.5.2]: https://github.com/SynapsesOS/synapses/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/SynapsesOS/synapses/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/SynapsesOS/synapses/releases/tag/v0.5.0
