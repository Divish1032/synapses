# Changelog

All notable changes to Synapses are documented here. This project adheres to [Semantic Versioning](https://semver.org/).

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
- **18-Language Parser** — Tree-sitter-based parser suite (Go, TypeScript, Rust, Java, and 14 others).
- **Core MCP Tools** — `get_context`, `find_entity`, `search`, `get_call_chain`, `get_impact`, `annotate_node`.
- **SQLite Persistence** — modernc.org/sqlite (pure Go, no CGo).
- **Graph Index** — Columnar adjacency representation for fast BFS.

---

[0.7.0]: https://github.com/SynapsesOS/synapses/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/SynapsesOS/synapses/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/SynapsesOS/synapses/compare/v0.5.2...v0.6.0
[0.5.2]: https://github.com/SynapsesOS/synapses/compare/v0.5.1...v0.5.2
[0.5.1]: https://github.com/SynapsesOS/synapses/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/SynapsesOS/synapses/releases/tag/v0.5.0
