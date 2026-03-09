# Synapses — Code Intelligence for AI Agents

[![Release](https://img.shields.io/github/v/release/SynapsesOS/synapses?style=for-the-badge&color=00ADD8)](https://github.com/SynapsesOS/synapses/releases/latest)
[![License](https://img.shields.io/badge/License-MIT-green?style=for-the-badge)](LICENSE)
[![CI](https://github.com/SynapsesOS/synapses/actions/workflows/ci.yml/badge.svg)](https://github.com/SynapsesOS/synapses/actions)
[![VS Code](https://img.shields.io/visual-studio-marketplace/v/SynapsesOS.synapses?style=for-the-badge&label=VS%20Code&color=007ACC)](https://marketplace.visualstudio.com/items?itemName=SynapsesOS.synapses)

**Synapses** is a graph-based code intelligence server that gives AI coding agents structured understanding of large codebases. Replace ad-hoc grep with typed graph queries. Supports 18 languages. Works with Claude Code, Cursor, Zed, Windsurf, Gemini, and any editor via [MCP](https://modelcontextprotocol.io).

```
IDE → MCP Tools → Synapses (Graph+SQLite)
                    ↓
           Brain Sidecar (LLM)
           Scout Sidecar (Web)
```

---

## What is Synapses?

Synapses solves a core problem in AI-assisted development: **large codebases are too big to fit in context, and grep is too dumb to understand code structure.**

Instead of line-by-line searching, Synapses maintains an in-memory graph of your codebase:
- **Nodes**: functions, methods, structs, classes, interfaces, variables, files, packages
- **Edges**: calls, implements, defines, embeds, imports, depends on, data flows

AI agents query the graph via **38 MCP tools** to answer questions like:
- "Find all callers of auth.Login()"
- "What breaks if I change this function signature?"
- "Architect a context packet for debugging checkout flow"
- "Explain this architectural rule violation in English"

Synapses maintains **episodic memory** (past decisions, failures), an **agent message bus**, **vector embeddings** (semantic search), and **cross-project propagation** so agents don't repeat work across sessions.

---

## Features

✅ **38 MCP Tools** — session management, code graph queries, task memory, agent coordination, episodic memory, web intelligence, architecture enforcement
✅ **18-Language Parser** — Go, TypeScript, Python, Java, Rust, C, C++, C#, Swift, Ruby, PHP, Kotlin, Scala, Lua, Elixir, Protobuf, Groovy, and a generic fallback
✅ **Episodic Memory** — persist past decisions and failures; future sessions query them to avoid repeating mistakes
✅ **Agent Message Bus** — broadcast work status across agents; unread messages surface on session start
✅ **Vector Embeddings** — semantic search via content-hash invalidation (detect stale embeddings automatically)
✅ **Cross-Project Propagation** — when one repo changes, affected repos in a monorepo are notified
✅ **Intent-Based Context** — context packets adapt to agent intent (understand/review/debug/add/modify/plan)
✅ **Architectural Rules** — enforce no-regex constraints (e.g., "graph layer cannot call store"); get violations + suggestions
✅ **Single Binary** — one MCP server, works with any IDE. Pre-built for macOS, Linux, Windows.
✅ **Fail-Silent** — brain sidecar crashes? graph queries still work. Scout down? web tools return "unavailable"

---

## Supported Languages

| Language | Tier | Status |
|----------|------|--------|
| Go | 1st | ✅ Full support (go/types, build graph, test detection) |
| TypeScript/JavaScript | 1st | ✅ Full support (tree-sitter + optional tsserver for types) |
| Python | 1st | ✅ Full support (dynamic import detection) |
| Java | 1st | ✅ Full support (classpath resolution) |
| Rust | 1st | ✅ Full support |
| C/C++ | 1st | ✅ Full support (via tree-sitter) |
| C# | 2nd | ✅ Supported |
| Swift | 2nd | ✅ Supported |
| Ruby | 2nd | ✅ Supported |
| PHP | 2nd | ✅ Supported |
| Kotlin | 2nd | ✅ Supported |
| Scala | 2nd | ✅ Supported |
| Lua | 2nd | ✅ Supported |
| Elixir | 2nd | ✅ Supported |
| Protobuf | 2nd | ✅ Supported |
| Groovy | 2nd | ✅ Supported |
| Generic (regex-based) | Fallback | ✅ Catches basic function defs in any language |

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

This:
- Parses your codebase into a graph (18-language support)
- Creates SQLite cache at `~/.cache/synapses/`
- Writes `.claude/CLAUDE.md` with navigation rules
- Writes `.mcp.json` for IDE integration

### 3. Add to Your IDE

```bash
synapses mcp-setup --agent claude  # Claude Code
synapses mcp-setup --agent cursor  # Cursor
synapses mcp-setup --agent zed     # Zed
synapses mcp-setup --agent windsurf # Windsurf
```

This updates your IDE's MCP config to point to the Synapses server.

### 4. Start the Server

```bash
synapses start -path /path/to/repo
```

Keep this running in a terminal. The IDE connects via stdio.

---

## IDE Integrations

**Synapses works with any editor that supports MCP (Model Context Protocol):**

- **Claude Code** — `synapses mcp-setup --agent claude`
- **Cursor** — `synapses mcp-setup --agent cursor`
- **Zed** — `synapses mcp-setup --agent zed`
- **Windsurf** — `synapses mcp-setup --agent windsurf`
- **Gemini Code Assist** — `synapses mcp-setup --agent gemini`
- **Manual config** — edit `~/.cursor/mcp_config.json` or equivalent

Each editor's config points to: `{"command": "synapses", "args": ["start", "-path", "/path/to/repo"]}`

---

## MCP Tools Reference

Synapses registers **38 MCP tools** across 9 categories. All are available in your IDE's tool palette.

### Session Bootstrap
| Tool | Params | Description |
|------|--------|-------------|
| `session_init` | `agent_id` (optional) | One round-trip: pending tasks + project identity + working state + recent events. Replaces the 3-call startup ritual. |

### Code Graph
| Tool | Params | Description |
|------|--------|-------------|
| `get_project_identity` | — | Compact architectural summary: node/edge counts, entry points, key entities, active rules. |
| `get_context` | `entity`, `depth`, `token_budget`, `task_id`, `file`, `format`, `detail_level` | BFS ego-subgraph with decay. `format=compact` returns 400-600 token prose; `format=json` returns full JSON. |
| `find_entity` | `query` | Locate nodes by name/substring. Returns ID, type, file, line, doc, signature. |
| `get_file_context` | `file` | All entities in a file ordered by line. |
| `search` | `query`, `mode`, `limit` | Keyword search or FTS5 BM25 semantic search. CamelCase auto-split. |
| `get_call_chain` | `from`, `to` | Shortest CALLS path (BFS), follows IMPLEMENTS edges. |
| `get_impact` | `symbol`, `depth` | Blast-radius reverse-BFS: direct (1.0), indirect (0.6), peripheral (0.3) tiers. |

### Architecture & Rules
| Tool | Params | Description |
|------|--------|-------------|
| `validate_plan` | `changes` (JSON) | Check proposed call-graph changes against architectural rules before coding. |
| `get_violations` | `rule_id`, `include_log` | List current rule violations; optionally show historical audit log. |
| `upsert_rule` | `rule_id`, `description`, `severity`, `edge_type`, `from_file_pattern`, `to_file_pattern`, `to_name_pattern` | Create/update dynamic architectural rule. Persisted immediately, no restart needed. |

### Task Memory
| Tool | Params | Description |
|------|--------|-------------|
| `create_plan` | `title`, `tasks`, `description`, `agent_id` | Save a plan with prioritized tasks (p0-p3). |
| `get_pending_tasks` | `plan_id`, `agent_id` | All pending/in-progress tasks; in_progress tasks include session state for resumption. |
| `update_task` | `id`, `status`, `notes`, `agent_id` | Update task status, append notes. |
| `save_session_state` | `task_id`, `agent_id`, `approach`, `files_modified`, `completed_steps`, `remaining_steps`, `blockers`, `decisions`, `context_snapshot` | Save exact work state for cross-session resumption. |
| `get_session_state` | `task_id` | Retrieve saved state for a task. |
| `get_plans` | — | List all plans with completion counts. |
| `get_my_tasks` | `agent_id`, `plan_id` | Unblocked tasks for a specific agent. |
| `link_task_nodes` | `task_id`, `node_ids` (JSON) | Link task to graph nodes for relevance boosting. |
| `annotate_node` | `node_id`, `note`, `agent_id` | Attach persistent note to a code entity. |
| `get_working_state` | `window_minutes` | Recent file changes + git diff stats. |

### Agent Coordination
| Tool | Params | Description |
|------|--------|-------------|
| `get_agents` | — | List all active agents sorted by last-seen. |
| `claim_work` | `agent_id`, `scope`, `scope_type`, `ttl_minutes` | Reserve a scope; conflicts returned immediately. |
| `release_claims` | `agent_id` | Release all work claims. |
| `get_conflicts` | `agent_id` | All overlapping claims by other agents. |
| `get_events` | `since_seq`, `types`, `limit` | Event log with cursor: file_change, task_update, annotation_added, agent_activity. |

### Agent Message Bus
| Tool | Params | Description |
|------|--------|-------------|
| `send_message` | `from_agent`, `topic`, `payload`, `to_agent` (optional), `project_id` (optional) | Direct or broadcast message via SQLite. Broadcast to all: set `to_agent=""`. |
| `get_messages` | `agent_id`, `since_seq`, `topic_filter`, `unread_only`, `limit` | Retrieve messages. Unread surface automatically in `session_init`. |
| `mark_read` | `message_id`, `agent_id` | Mark message as read. |

### Episodic Memory
| Tool | Params | Description |
|------|--------|-------------|
| `remember` | `agent_id`, `decision`, `episode_type`, `outcome`, `rationale`, `trigger`, `affected_files`, `affected_nodes`, `tags`, `project_id` | Record decision/failure as persistent episode. |
| `recall` | `query`, `project_id`, `agent_id`, `episode_type`, `outcome_filter` | FTS5 BM25 search over past episodes. |
| `get_episodes` | `project_id`, `agent_id`, `episode_type`, `tags` | List episodes without search. |
| `check_plan_safety` | `plan_description`, `agent_id`, `project_id` | Search for past failures matching proposed plan (Reactive Interjection). |
| `get_rule_candidates` | — | Failure episodes ≥N times, not yet promoted to rules. |

### Web Intelligence (requires scout sidecar)
| Tool | Params | Description |
|------|--------|-------------|
| `web_search` | `query`, `max_results`, `region`, `timelimit` | Search via synapses-scout. |
| `web_fetch` | `input`, `force_refresh` | Fetch URL or search query. Returns Markdown. Triggers brain ingest. |
| `web_annotate` | `node_id`, `note`, `hits`, `agent_id` | Persist web findings to graph node. |
| `web_deep_search` | `query`, `max_results`, `region`, `timelimit` | Multi-query orchestrated search. |

### Brain / ADRs (requires brain sidecar)
| Tool | Params | Description |
|------|--------|-------------|
| `upsert_adr` | `id`, `title`, `decision`, `status`, `context`, `consequences` | Create/update Architectural Decision Record. |
| `get_adrs` | `file` (optional) | List ADRs; filter by file. |

---

## CLI Reference

All commands use the syntax `synapses <command> [flags]`.

| Command | Flags | Description |
|---------|-------|-------------|
| `init` | `-path`, `-reindex` | Parse project, write `.mcp.json`, write navigation files. Zero-friction onboarding. |
| `start` | `-path`, `-reindex`, `-no-watch` | Load/build graph, start file watcher, serve MCP over stdio. |
| `index` | `-path`, `-reindex` | Parse + cache graph, exit. |
| `status` | `-path` | Node/edge counts, entry points, violations, top tools. |
| `query` | `-path`, `-entity` | JSON lookup of entity (read-only). |
| `export` | `-path`, `-entity`, `-format` (dot/mermaid/graphml), `-depth` | Export graph to stdout. |
| `list` | — | Scan cache dir, print summary of all indexed projects. |
| `reset` | `-path`, `-all` | Remove SQLite cache. |
| `setup` | `-path`, `-core` | First-time config: check brain binary, run `brain setup`, write synapses.json. |
| `mcp-setup` | `-agent` (cursor/gemini/zed/windsurf/claude/all), `-path` | Write agent-specific MCP config. |
| `version` | — | Print version. |

---

## Configuration: synapses.json

Synapses reads runtime config from `synapses.json` in your project root:

```json
{
  "version": "1",
  "brain": {
    "url": "http://localhost:11435",
    "timeout_sec": 60,
    "enable_llm": true
  },
  "scout": {
    "url": "http://localhost:11436",
    "timeout_sec": 30
  },
  "rules": [
    {
      "id": "no-store-in-graph",
      "description": "Graph layer cannot call Store methods",
      "forbidden_edge": {
        "from_file_pattern": "*/graph/*.go",
        "edge_type": "CALLS",
        "to_name_pattern": "Store."
      },
      "severity": "error"
    }
  ],
  "edge_weights": {
    "CALLS": 1.0,
    "IMPLEMENTS": 1.2,
    "IMPORTS": 0.5
  },
  "context_carve": {
    "max_depth": 3,
    "token_budget": 4000,
    "decay_rate": 0.7
  },
  "use_go_types": true,
  "use_ts_types": false,
  "metrics_days": 30,
  "linked": [
    {
      "path": "../other-repo",
      "peer_url": "http://localhost:3000"
    }
  ]
}
```

**Key fields:**
- `brain.url` — Brain sidecar HTTP endpoint (default: http://localhost:11435)
- `brain.timeout_sec` — LLM timeout in seconds (default: 60, increase for slow CPUs)
- `scout.url` — Scout sidecar HTTP endpoint (default: http://localhost:11436)
- `rules` — Dynamic architectural rules (hot-reloaded)
- `edge_weights` — BFS weights for relevance decay
- `context_carve` — Graph carving thresholds (depth, tokens, decay)
- `linked` — Monorepo peer projects

See `synapses.example.json` for all 20+ config options.

---

## Working with Sidecars

Synapses is most powerful when paired with two optional sidecars:

### Brain Sidecar (synapses-intelligence)

Adds semantic enrichment via local LLMs:
- Generate prose summaries of code entities
- Explain architectural rule violations in English
- Build context packets (~800 tokens vs 4000 raw)
- Episodic memory learning loop

```bash
brain setup --llama-server  # Recommended: no Ollama, CPU/GPU auto-detect
brain serve                  # Start on :11435
```

Then configure `synapses.json` with `brain.url: "http://localhost:11435"`.

### Scout Sidecar (synapses-scout)

Adds web intelligence:
- Search the web via DuckDuckGo or Tavily
- Fetch and extract URLs
- Distill web content (boilerplate removal, AI summary)
- Cache search results (6h TTL)

```bash
pip install synapses-scout
scout serve  # Start on :11436
```

Then configure `synapses.json` with `scout.url: "http://localhost:11436"`.

Both sidecars are optional — Synapses works without them (fail-silent).

---

## Architecture

### Core Design Principles

📦 **Pre-built Binaries** — No toolchain needed. Download from GitHub Releases or install via Homebrew.

🔄 **Fail-Silent** — Brain crashes? Graph queries still work. Scout down? Web tools return "unavailable".

📦 **Single Binary** — One MCP server. Works with any IDE via stdio.

💾 **Local Cache** — All state at `~/.cache/synapses/cache/<hash>.db`. No cloud.

🔁 **Incremental** — File watcher re-parses only changed files.

### Data Model

**Nodes**: Typed entities (function, method, struct, interface, variable, file, package)
**Edges**: Relation types (CALLS, IMPLEMENTS, IMPORTS, DEFINES, EMBEDS, DEPENDS_ON, EXPORTS, DATA_FLOWS)
**Graph**: In-memory adjacency lists + columnar GraphIndex for fast BFS

Serialized to SQLite: full graph snapshot for recovery, FTS5 for semantic search, episodic memory tables.

### Stack

- **Language**: Go 1.26
- **Graph DB**: SQLite (modernc.org/sqlite, pure Go)
- **Parser**: Tree-sitter (18 languages)
- **MCP**: mark3labs/mcp-go (stdio transport)
- **Compression**: klauspost/compress (snapshot blobs)

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

## Links

- **GitHub**: https://github.com/SynapsesOS/synapses
- **Brain Sidecar**: https://github.com/SynapsesOS/synapses-intelligence
- **Web Intelligence**: https://github.com/SynapsesOS/synapses-scout
- **Organization**: https://github.com/SynapsesOS

## Support

- **Issues**: https://github.com/SynapsesOS/synapses/issues
- **Discussions**: https://github.com/SynapsesOS/synapses/discussions
- **Security**: security@synapsesos.dev (see [SECURITY.md](SECURITY.md))
