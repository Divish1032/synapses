# Synapses — Code Intelligence for AI Agents

[![Release](https://img.shields.io/github/v/release/SynapsesOS/synapses?style=for-the-badge&color=00ADD8)](https://github.com/SynapsesOS/synapses/releases/latest)
[![License](https://img.shields.io/badge/License-MIT-green?style=for-the-badge)](LICENSE)
[![CI](https://github.com/SynapsesOS/synapses/actions/workflows/ci.yml/badge.svg)](https://github.com/SynapsesOS/synapses/actions)
[![VS Code](https://img.shields.io/visual-studio-marketplace/v/SynapsesOS.synapses?style=for-the-badge&label=VS%20Code&color=007ACC)](https://marketplace.visualstudio.com/items?itemName=SynapsesOS.synapses)

**Synapses** is a graph-based code intelligence server that gives AI coding agents structured understanding of large codebases. Replace ad-hoc grep with typed graph queries. Supports 49+ languages. Works with Claude Code, Cursor, Zed, Windsurf, and any editor via [MCP](https://modelcontextprotocol.io).

```
IDE → MCP Tools → Synapses (Graph+SQLite)
```

---

## What is Synapses?

Synapses solves a core problem in AI-assisted development: **large codebases are too big to fit in context, and grep is too dumb to understand code structure.**

Instead of line-by-line searching, Synapses maintains an in-memory graph of your codebase:
- **Nodes**: functions, methods, structs, classes, interfaces, variables, files, packages
- **Edges**: calls, implements, defines, embeds, imports, depends on, data flows

AI agents query the graph via **48 MCP tools** to answer questions like:
- "Find all callers of auth.Login()"
- "What breaks if I change this function signature?"
- "Architect a context packet for debugging checkout flow"
- "Explain this architectural rule violation in English"

Synapses maintains **episodic memory** (past decisions, failures), an **agent message bus**, **vector embeddings** (semantic search), and **cross-project federation** so agents don't repeat work across sessions.

---

## Features

**48 MCP Tools** — session management, code graph queries, task memory, agent coordination, episodic memory, architecture enforcement, skill automation
**49+ Language Parsers** — Go, TypeScript, Python, Java, Rust, C/C++, C#, Swift, Ruby, PHP, Kotlin, Scala, Dart, Zig, Haskell, Terraform, Nix, and 30+ more with generic fallback
**Episodic Memory** — persist past decisions and failures; future sessions query them to avoid repeating mistakes
**Agent Message Bus** — broadcast work status across agents; unread messages surface on session start
**Vector Embeddings** — built-in all-MiniLM-L6-v2 ONNX model, zero external dependencies. Optional Ollama or OpenAI-compatible endpoint.
**Cross-Project Federation** — query sibling project graphs and memories from a single session
**Intent-Based Context** — context packets adapt to agent intent (understand/review/debug/add/modify/plan)
**Architectural Rules** — enforce constraints (e.g., "handlers cannot call DB directly"); get violations with suggestions
**Quality Gaps** — record and surface agent-discovered edge cases, coverage gaps, and known limitations across sessions
**Single Binary** — one MCP server, works with any IDE. Pre-built for macOS, Linux, Windows.
**Fail-Silent** — brain LLM crashes? graph queries still work. Web cache down? lookup_docs returns a clear message.

---

## Supported Languages

| Language | Tier | Status |
|----------|------|--------|
| Go | 1st | Full support (go/types, build graph, test detection) |
| TypeScript/JavaScript | 1st | Full support (tree-sitter + optional tsserver for types) |
| Python | 1st | Full support (dynamic import detection) |
| Java | 1st | Full support (classpath resolution) |
| Rust | 1st | Full support |
| C/C++ | 1st | Full support (via tree-sitter) |
| C# | 2nd | Supported |
| Swift | 2nd | Supported |
| Ruby | 2nd | Supported |
| PHP | 2nd | Supported |
| Kotlin | 2nd | Supported |
| Scala | 2nd | Supported |
| Lua | 2nd | Supported |
| Elixir | 2nd | Supported |
| Protobuf | 2nd | Supported |
| Groovy | 2nd | Supported |
| Generic (regex-based) | Fallback | Catches basic function defs in any language |

---

## Web Console

When the Synapses daemon is running, a **built-in web console** is available at:

```
http://localhost:11435
```

The console gives you a visual dashboard for all your indexed projects — live index stats, task queues, episodic memory, agent activity, and one-click reindex. No separate install needed; it's embedded in the binary.

**Open it:** after running `synapses init`, just visit `http://localhost:11435` in your browser.

**Desktop app:** if you prefer a native window, the [Synapses desktop app](https://github.com/SynapsesOS/synapses-app) wraps this same UI in a Tauri shell (macOS, Linux, Windows).

**Custom UI (advanced):** drop your own `index.html` into `~/.synapses/console/` and the daemon will serve that instead of the built-in UI. Useful for hotfixes or custom dashboards without rebuilding the binary.

---

## Quick Start

### 1. Install

**macOS / Linux (Homebrew — recommended):**
```bash
brew tap SynapsesOS/tap
brew install synapses
```

**macOS / Linux / Windows (direct binary):**

Download the latest release from [GitHub Releases](https://github.com/SynapsesOS/synapses/releases/latest):

| Platform | File |
|----------|------|
| macOS (Apple Silicon) | `synapses_darwin_arm64.tar.gz` |
| macOS (Intel) | `synapses_darwin_x86_64.tar.gz` |
| Linux (x86_64) | `synapses_linux_x86_64.tar.gz` |
| Linux (ARM64) | `synapses_linux_arm64.tar.gz` |
| Windows | `synapses_windows_x86_64.zip` |

Extract and place the `synapses` binary on your `PATH` (e.g. `/usr/local/bin/synapses`).

**From source (requires Go 1.22+):**
```bash
curl -fsSL https://raw.githubusercontent.com/SynapsesOS/synapses/main/install.sh | sh
```

**VS Code Extension:**
```
ext install SynapsesOS.synapses
```
Or search "Synapses" in the VS Code Extensions panel.

### 2. Initialize Your Project

```bash
cd /path/to/your/repo
synapses init
```

That's it. The `init` wizard handles everything in four steps:

| Step | What it does |
|------|-------------|
| **[1/4] Project Setup** | Detects git, creates `synapses.json` with sensible defaults |
| **[2/4] Indexing** | Parses your codebase and builds the code graph (49+ languages) |
| **[3/4] Starting Engine** | Installs system service (auto-restart on crash), starts daemon, registers project, verifies MCP endpoint |
| **[4/4] Connect Agents** | Auto-detects installed AI agents and writes their MCP configs |

The wizard auto-detects Claude Code, Cursor, VS Code, Windsurf, Zed, and Antigravity. Select which ones to connect and Synapses writes the config files for you.

**Non-interactive mode:**
```bash
synapses init --yes --agents claude,cursor
```

**Connect additional agents later:**
```bash
synapses connect --agent windsurf
```

---

## IDE Integrations

**Synapses works with any editor that supports MCP (Model Context Protocol).**

The easiest way to connect is `synapses init` — it auto-detects installed agents and writes their configs. To connect additional agents later:

- **Claude Code** — `synapses connect --agent claude`
- **Cursor** — `synapses connect --agent cursor`
- **Zed** — `synapses connect --agent zed`
- **Windsurf** — `synapses connect --agent windsurf`
- **VS Code** — `synapses connect --agent vscode`
- **Antigravity** — `synapses connect --agent antigravity`
- **Manual config** — each agent's config points to: `{"command": "synapses", "args": ["start", "--path", "/path/to/repo"]}`

---

## MCP Tools Reference

Synapses registers **40 MCP tools** across 9 categories. All are available in your IDE's tool palette.

### Session Bootstrap
| Tool | Params | Description |
|------|--------|-------------|
| `session_init` | `agent_id`, `intent`, `scope`, `model`, `provider` | Single round-trip session bootstrap. Returns pending tasks, project identity, working state, and recent events. Supports incremental delivery: unchanged sections skipped on repeat calls. Call this at the start of every session. |
| `end_session` | `agent_id` (required), `task_id`, `summary`, `model`, `provider`, `input_tokens`, `output_tokens`, `cost_usd` | Persist session knowledge as structured memories and optionally report token usage. Call at the end of every session — this is how institutional knowledge accumulates. |
| `explain_codebase` | — | Returns a ~1000 token natural-language orientation: entry points, key types by fanin, architectural patterns, package structure, tech stack. Cached until a structural change. |
| `get_repo_map` | `detail` | Navigable package+entity overview grouped by architectural layer. `detail="compact"` (~500 tokens, default) or `"full"` (~2000 tokens). |
| `discover_tools` | `query` (required), `debug` | Find the right Synapses tool for a task. Describe what you need in natural language. ~300 tokens vs ~4200 for reading all definitions. |

### Code Graph
| Tool | Params | Description |
|------|--------|-------------|
| `prepare_context` | `intent` (required), `target` (required), `file`, `task_id`, `token_budget`, `projects` | **Start here.** Intent-based context assembly. Declare `modify`/`understand`/`review`/`debug`/`add`/`plan` and a target; Synapses composes the right context in one round-trip. |
| `get_context` | `entity` (required), `depth`, `token_budget`, `task_id`, `mode`, `file`, `format`, `detail_level`, `helpful`, `include_inferred`, `known_hash`, `agent_id`, `projects` | Power-user: raw BFS ego-subgraph with decay. Use `prepare_context` for most cases. `format="compact"` returns 400-600 token prose; `format="json"` returns full JSON. Pass `known_hash` for conditional fetching. |
| `find_entity` | `query` (required), `format` | Locate nodes by name/substring. Returns ID, type, file, line. Use before `get_context` to resolve the exact name. |
| `get_file_context` | `file` (required), `token_budget` | All entities in a file ordered by line number. |
| `search` | `query` (required), `mode`, `limit` | Keyword search or FTS5 BM25 full-text search. `mode="keyword"` (default) or `mode="fulltext"`. CamelCase auto-split. |
| `get_call_chain` | `from` (required), `to` (required) | Shortest CALLS path between two entities (BFS). |
| `get_impact` | `symbol` (required), `depth`, `token_budget`, `projects` | Blast-radius reverse-BFS: direct (1.0), indirect (0.6), peripheral (0.3) confidence tiers. |
| `get_entity_history` | `entity` (required), `file`, `limit` | Chronological timeline compositing memories, episodes, annotations, task references, and git changes — one call instead of five. |

### Architecture & Rules
| Tool | Params | Description |
|------|--------|-------------|
| `validate_plan` | `changes` (required), `check_safety`, `plan_description`, `skip_logic_checks` | Check proposed call-graph changes against architectural rules before writing code. Pass `check_safety=true` to also run failure-episode safety check inline. |
| `verify_implementation` | `files_written` (required), `task_id` | Post-write verification: check actual graph state against rules. Closes the plan→implement→verify loop. |
| `get_violations` | `rule_id`, `include_log`, `log_limit` | List current rule violations; optionally include historical audit log. |
| `upsert_rule` | `rule_id` (required), `description` (required), `severity` (required), `edge_type`, `from_file_pattern`, `to_file_pattern`, `to_name_pattern`, `context_source` | Create/update dynamic architectural rule. Persisted immediately, no restart needed. |
| `upsert_gap` | `node_id` (required), `gap_id` (required), `description` (required), `severity`, `status`, `fix_notes`, `agent_id` | Record a quality gap on a code entity (edge case, missing coverage, known limitation). Persists across sessions. |
| `get_gaps` | `node_id`, `file`, `severity`, `status` | Query quality gaps. Open gaps also appear in `get_violations` and `get_context`. |

### Task Memory
| Tool | Params | Description |
|------|--------|-------------|
| `create_plan` | `title` (required), `tasks` (required), `description`, `agent_id` | Save a plan with prioritized tasks (p0-p3). |
| `get_pending_tasks` | `plan_id`, `agent_id`, `suggest_next` | All pending/in-progress tasks ordered by priority. In-progress tasks include session state for resumption. |
| `update_task` | `id` (required), `status` (required), `notes`, `agent_id`, `intent` | Update task status, append notes. |
| `save_session_state` | `task_id` (required), `agent_id`, `approach`, `files_modified`, `completed_steps`, `remaining_steps`, `blockers`, `decisions`, `context_snapshot` | Save exact work state for cross-session resumption. |
| `get_session_state` | `task_id` (required) | Retrieve saved state for a task. (For most cases, `get_pending_tasks` already includes this inline for in-progress tasks.) |
| `get_plans` | — | List all plans with task completion counts. |
| `link_task_nodes` | `task_id` (required), `node_ids` (required) | Link task to graph nodes for relevance boosting in `get_context`. |
| `annotate_node` | `node_id` (required), `note` (required), `agent_id` | Attach a persistent note to a code entity, visible to all agents via `get_context`. |

### Agent Coordination
| Tool | Params | Description |
|------|--------|-------------|
| `get_agents` | `projects` | List all active agents sorted by last-seen. Cross-project with `projects="*"`. |
| `get_events` | `since_seq`, `types`, `limit`, `agent_id`, `projects` | Event log with cursor: file_change, task_update, annotation_added. Cross-project with `projects="*"`. |

### Agent Message Bus
| Tool | Params | Description |
|------|--------|-------------|
| `send_message` | `from_agent` (required), `topic` (required), `payload`, `to_agent`, `project_id` | Direct or broadcast message via SQLite. Omit `to_agent` to broadcast to all. |
| `get_messages` | `agent_id` (required), `since_seq`, `topic_filter`, `unread_only`, `limit`, `mark_read_ids`, `projects` | Retrieve messages. Pass `mark_read_ids=["id1","id2"]` to batch-acknowledge in the same call. |

### Episodic Memory
| Tool | Params | Description |
|------|--------|-------------|
| `remember` | `agent_id` (required), `decision` (required), `episode_type`, `outcome`, `rationale`, `trigger`, `affected_files`, `affected_nodes`, `tags`, `project_id`, `anchor_nodes`, `memory_importance` | Record decision or failure as a persistent episode. Pass `anchor_nodes` to auto-invalidate this memory when anchored graph entities change. |
| `recall` | `query`, `project_id`, `agent_id`, `episode_type`, `outcome_filter`, `tags`, `limit`, `include_stale`, `projects`, `as_of`, `since`, `until`, `depth` | Search or browse episodic memory. With `query`: FTS5 BM25 ranked search. Without `query`: chronological browse. Also surfaces `dynamic_rules` from similar past failures. |
| `get_rule_candidates` | — | Failure episodes that have appeared N or more times and have not yet been promoted to a dynamic rule. Use this to close the feedback loop. |

### Architecture Decision Records
| Tool | Params | Description |
|------|--------|-------------|
| `upsert_adr` | `id` (required), `title` (required), `decision` (required), `status`, `context`, `consequences`, `linked_files` | Create/update an Architectural Decision Record. Appears in `get_context` compact output when `linked_files` matches the entity's file. Requires `brain.enabled: true` in synapses.json. |
| `get_adrs` | `file` | List ADRs; filter by file using `linked_files` patterns. Requires `brain.enabled: true` in synapses.json. |

### Web & Docs
| Tool | Params | Description |
|------|--------|-------------|
| `lookup_docs` | `package`, `url`, `entity` | Returns cached Go package documentation or arbitrary URL content. Package docs are version-pinned from go.mod. Use to verify API signatures before writing code. |
| `web_annotate` | `node_id` (required), `note`, `hits`, `agent_id` | Persist web findings to a graph node annotation. Makes web research a first-class data object visible to all future sessions. |

### Skills (Composable Tool Sequences)
| Tool | Params | Description |
|------|--------|-------------|
| `list_skills` | — | List all available skill recipes with their IDs, descriptions, parameters, and step counts. |
| `execute_skill` | `skill_id` (required), `params` | Execute a named skill recipe that composes multiple tools into a single call. |

---

## CLI Reference

All commands use the syntax `synapses <command> [flags]`.

### Daemon Commands
| Command | Flags | Description |
|---------|-------|-------------|
| `start` | `-path` | Ensure daemon is running and register project (proxy mode). Indexes the codebase and serves MCP. |
| `stop` | — | Stop the singleton daemon. |
| `daemon install` | — | Register daemon as system service (launchd/systemd) with socket activation. Auto-restarts on crash, port stays open during restart. **Run automatically by `init`.** |
| `daemon uninstall` | — | Remove system service registration. |
| `projects` | — | List projects registered with the running daemon. |
| `logs` | `-n` | Tail the daemon log (`~/.synapses/daemon.log`). |
| `status` | `-path` | Show index statistics and daemon health. |
| `doctor` | `-path` | Full health check (index, brain, scout). |

### Index Commands
| Command | Flags | Description |
|---------|-------|-------------|
| `index` | `-path`, `-reindex` | Parse and cache graph, then exit (no server). |
| `list` | — | Scan cache dir, print summary of all indexed projects. |
| `reset` | `-path`, `-all` | Remove SQLite cache for one project or all projects. |

### Setup Commands
| Command | Flags | Description |
|---------|-------|-------------|
| `init` | `-path`, `--yes`/`-y`, `--agents`, `--no-agents` | Interactive 4-step wizard: project setup, indexing, daemon start, agent connection. The single golden-path command for new users. |
| `connect` | `--agent` (claude/cursor/windsurf/zed/vscode/antigravity), `-path` | Write per-agent IDE configs (MCP config + agent rules file). Use to connect additional agents after `init`. |
| `uninstall` | `-path`, `--yes`/`-y`, `--global`, `--keep-data`, `--keep-binary` | Complete removal wizard — the inverse of `init`. Stops daemon, removes indexes, cleans agent configs. Use `--global` for full system cleanup including `~/.synapses` and the binary. |

### Security Commands
| Command | Flags | Description |
|---------|-------|-------------|
| `approve` | `--all` / `-a` | Review and approve pending cross-project write requests. Agents that attempt a broadcast `send_message` or cross-project `remember` are gated until a human runs this command. `--all` approves all non-interactively. |

### Update Commands
| Command | Flags | Description |
|---------|-------|-------------|
| `update` | `--check` | Check for a new release and print a diff. Without `--check`, downloads and installs the latest binary. |

### Other Commands
| Command | Flags | Description |
|---------|-------|-------------|
| `query` | `-path`, `-entity` | JSON lookup of entity (read-only). |
| `brief` | `-path` | Concise session brief. |
| `export` | `-path`, `-entity`, `-format` (dot/mermaid/graphml), `-depth` | Export graph to stdout. |
| `memory` | — | Session memory subcommands (`list`, `get`, `delete`). |
| `allow-plugin` | — | Add a plugin to the allowlist. |
| `benchmark` | — | Run performance benchmarks (graph BFS, FTS, embeddings). |
| `version` | — | Print version. Shows update hint if a newer release is cached. |
| `help` | — | Print usage. |

---

## Configuration: synapses.json

Synapses reads runtime config from `synapses.json` in your project root. Run `synapses setup` to generate one with sensible defaults.

```json
{
  "version": "1",
  "mode": "full",
  "rules": [
    {
      "id": "no-db-in-handler",
      "description": "Handlers must not call database functions directly",
      "severity": "warning",
      "rule_type": "structural",
      "forbidden_edge": {
        "from_file_pattern": "*/handlers/*",
        "to_file_pattern": "*/db/*",
        "edge_type": "CALLS"
      }
    }
  ],
  "edge_weights": {
    "CALLS": 1.0,
    "IMPLEMENTS": 0.8,
    "IMPORTS": 0.3
  },
  "context_carve": {
    "default_depth": 2,
    "decay_factor": 0.6,
    "token_budget": 4000,
    "min_relevance": 0.05,
    "exclude_test_files": true
  },
  "brain": {
    "enabled": false,
    "ollama_url": "http://localhost:11434",
    "model": "qwen3.5:2b",
    "ingest": false,
    "enrich": false,
    "context_builder": false
  },
  "embeddings": "builtin",
  "use_go_types": false,
  "use_ts_types": false,
  "metrics_days": 90,
  "federation": [
    {
      "path": "../sibling-project",
      "alias": "sibling"
    }
  ],
  "session": {
    "auto_end_threshold_calls": 80,
    "reconnect_window_secs": 300,
    "stale_threshold_mins": 30
  }
}
```

**Key fields:**

| Field | Description |
|-------|-------------|
| `mode` | `"full"` (default) or `"knowledge"` (no code graph — memory/tasks/messages only) |
| `rules` | Architectural rules enforced by `validate_plan` and `get_violations` |
| `edge_weights` | BFS weights for relevance decay during context carving |
| `context_carve` | Graph carving thresholds (depth, tokens, decay) |
| `brain.enabled` | Enable in-process LLM enrichment via Ollama. Required for `upsert_adr`/`get_adrs`. |
| `brain.ollama_url` | Ollama server base URL (default: `http://localhost:11434`) |
| `embeddings` | `"builtin"` (default, zero-dep ONNX model), `"ollama"`, or `"off"` |
| `embedding_endpoint` | Optional OpenAI-compatible embeddings endpoint |
| `use_go_types` | Enable type-checked CALLS resolution for Go (requires valid go module) |
| `use_ts_types` | Enable type-checked CALLS resolution for TypeScript (requires Node.js + typescript) |
| `metrics_days` | Git history window for churn computation (default: 90) |
| `coverage_profile` | Path to `go test -coverprofile` output for coverage annotations |
| `federation` | Local sibling projects for cross-project queries (filesystem, no daemon required) |
| `federation_acl` | Controls which daemon-registered projects this project can query |
| `constitution` | Project-wide principles injected into every agent session |
| `session` | Session memory behavior (auto-end, reconnect window, stale threshold) |
| `rate_limits` | Per-session rate limits for write ops, expensive reads, cross-project queries |
| `content_safety` | Prompt injection scanner for stored content (`"warn"` / `"truncate"` / `"reject"`) |

See `synapses.example.json` for all available fields with documentation.

---

## Brain Integration

The brain is an optional in-process LLM layer that enriches context packets:

- Generate prose summaries of code entities
- Explain architectural rule violations in plain English
- Build compact context packets (~800 tokens vs 4000 raw)
- Enable Architectural Decision Records (`upsert_adr`/`get_adrs`)

No external sidecar binary is needed — the brain runs in the same process as the MCP server.

### Choosing a Backend

Synapses supports three LLM backends. Choose one based on your setup:

| Backend | When to use | Setup |
|---------|------------|-------|
| **llama-server** | No Ollama, want GPU/CPU auto-detect | `brain setup --llama-server` |
| **ollama** | Ollama already installed | `brain setup` |
| **local** | In-process GGUF, CGo build | `brain setup --local` |

**llama-server is the recommended default** — it requires no Ollama installation, auto-detects Metal/CUDA/CPU, and manages the subprocess automatically.

### Quick Enable

```json
"brain": {
  "enabled": true,
  "ollama_url": "http://localhost:11434",
  "model": "qwen3.5:2b",
  "ingest": true,
  "enrich": true
}
```

Run `brain setup` (or `brain setup --llama-server`) to auto-detect your hardware and write a tuned `~/.synapses/brain.json`. The setup command probes installed models for latency and picks the right tier for your machine.

### Default Models

The brain is **fully configurable** — you can point it at any Ollama model, OpenAI-compatible endpoint, or local GGUF file. The default configuration uses [Qwen2.5-Coder](https://huggingface.co/Qwen) models (by the Qwen team at Alibaba Cloud, Apache 2.0) on CPU, and [Qwen3.5](https://huggingface.co/Qwen) on GPU. These are sensible defaults chosen for their balance of speed and quality at small sizes; you are not required to use them.

---

## Architecture

### Core Design Principles

**Pre-built Binaries** — No toolchain needed. Download from GitHub Releases or install via Homebrew.

**Never Go Down** — Socket activation (launchd/systemd) holds port 11435 during daemon restarts. Process auto-restarts on crash. Panic recovery in MCP handlers. Per-project circuit breakers isolate failures.

**Fail-Silent** — Brain LLM crashes? Graph queries still work. Web cache down? `lookup_docs` returns a clear error. Tool panics? Daemon recovers and keeps serving.

**Single Binary** — One MCP server per machine, serving all projects. Works with any IDE via HTTP or stdio.

**Local Cache** — All state at `~/.synapses/`. No cloud. Graph snapshots in SQLite.

**Incremental** — File watcher re-parses only changed files. Session state accumulates across connections.

### Daemon Model

Synapses runs ONE singleton daemon per machine (`127.0.0.1:11435`), serving multiple projects concurrently. Each project gets its own graph, store, file watcher, and MCP server instance.

```
AI Agent → HTTP POST /mcp?project=<path> → Daemon → ProjectInstance → Tool Handler
```

**Reliability layers:**
- **Socket activation** — OS holds port 11435; connections queue during restart (up to 128)
- **Process supervision** — launchd (macOS) / systemd (Linux) auto-restarts on crash
- **Panic recovery** — 4 layers: `WithRecovery()` for tools, `defer recover()` in HTTP handler, Go stdlib per-connection recovery, process restart
- **Project warming** — Known projects pre-initialized on daemon startup from `~/.synapses/projects.json`
- **Agent-scoped rate limits** — Keyed by agent identity, persist across reconnections
- **Cycle detection** — Loop guard catches repeated calls AND alternating patterns (A-B-A-B)

### Data Model

**Nodes**: Typed entities (function, method, struct, interface, variable, file, package)
**Edges**: Relation types (CALLS, IMPLEMENTS, IMPORTS, DEFINES, EMBEDS, DEPENDS_ON, EXPORTS, DATA_FLOWS)
**Graph**: In-memory adjacency lists + columnar GraphIndex for fast BFS

Serialized to SQLite: full graph snapshot for recovery, FTS5 for semantic search, episodic memory tables, task/plan tables, message bus.

### Stack

- **Language**: Go 1.22+
- **Graph DB**: SQLite (modernc.org/sqlite, pure Go)
- **Parser**: Tree-sitter (49+ languages)
- **MCP**: mark3labs/mcp-go (Streamable HTTP + stdio transports)
- **Embeddings**: Built-in all-MiniLM-L6-v2 ONNX model (pure Go, ~23MB)
- **Socket Activation**: tprasadtp/go-launchd (macOS, pure Go), coreos/go-systemd (Linux)

---

## Contributing

We welcome contributions! See [CONTRIBUTING.md](CONTRIBUTING.md) for:
- Setup instructions
- Code style guide
- How to add a new MCP tool
- How to add a language parser
- Testing and CI requirements

---

## License

MIT License — See [LICENSE](LICENSE) for details.

---

## Acknowledgments

Synapses builds on several excellent open-source projects:

- [mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) — MCP protocol implementation
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) — Pure-Go SQLite driver
- [go-tree-sitter](https://github.com/smacker/go-tree-sitter) — Language parsing
- [Qwen team (Alibaba Cloud)](https://huggingface.co/Qwen) — Default brain models (Qwen2.5-Coder, Qwen3.5), Apache 2.0

---

## Links

- **GitHub**: https://github.com/SynapsesOS/synapses
- **Organization**: https://github.com/SynapsesOS
- **Desktop App**: https://github.com/SynapsesOS/synapses-app

## Support

- **Issues**: https://github.com/SynapsesOS/synapses/issues
- **Discussions**: https://github.com/SynapsesOS/synapses/discussions
- **Security**: security@synapsesos.dev (see [SECURITY.md](SECURITY.md))
