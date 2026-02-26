# Synapses

**The Agentic Control Plane.** A local-first, graph-based context manager for AI coding agents.

Instead of letting an agent grep through your codebase, Synapses parses your code into a relational graph and hands the agent a mathematically-carved slice — only the nodes and edges it actually needs.

```
Agent → "What is AuthService?"
Synapses → 2-hop BFS subgraph, edge-weighted, token-budgeted
         → AuthService + its direct callers + its direct dependencies
         → Delivered in <10ms, never exceeds your token budget
```

The agent gets structural truth. Not grep results.

---

## Features

- **Context Carving** — BFS ego-graph with relevance decay. The agent gets a ranked subgraph, not a file dump.
- **Project Identity** — Compact architectural handshake at session start: key entities, entry points, node/edge counts.
- **Architectural Rules** — Enforce constraints via `synapses.json` or `upsert_rule` at runtime. AI agents can define new rules during a session; they persist to SQLite and take effect immediately.
- **MCP Protocol** — Works natively with Claude Code, Cursor, and any MCP-compatible agent over stdio.
- **18 Language Parsers** — Deep AST parsing for all major languages. Fallback file-tracking for 80+ more.
- **Persistent Index** — Parse once, load in <1s on subsequent runs. Cache lives outside your project tree.
- **Live File Watcher** — Incremental re-parse on save. Graph stays current as you code.
- **Agent Task Memory** — `create_plan` / `get_pending_tasks` / `update_task` persist agent work across LLM sessions.
- **IMPLEMENTS Edges** — Structural interface satisfaction detection for Go; `get_call_chain` crosses interface boundaries automatically.
- **Federation / Monorepo** — Link multiple project graphs; cross-project CALLS edges resolved automatically.
- **Global Project List** — `synapses list` shows all indexed projects from one command.
- **Zero Operational Overhead** — Single binary. No Docker, no external services, no background daemons.

---

## Language Support

### Deep AST (functions, classes, imports, methods)

| Language | Extensions |
|---|---|
| Go | `.go` |
| TypeScript | `.ts`, `.tsx` |
| JavaScript | `.js`, `.jsx`, `.mjs`, `.cjs` |
| Python | `.py`, `.pyi` |
| Java | `.java` |
| Kotlin | `.kt`, `.kts` |
| Scala | `.scala` |
| Groovy | `.groovy`, `.gradle` |
| Rust | `.rs` |
| C | `.c`, `.h`, `.ino` |
| C++ | `.cpp`, `.cc`, `.cxx`, `.hpp`, `.hh`, `.hxx`, `.mm` |
| C# | `.cs` |
| Swift | `.swift` |
| Ruby | `.rb` |
| PHP | `.php` |
| Lua | `.lua` |
| Elixir | `.ex`, `.exs` |
| Protocol Buffers | `.proto` |

### File-level tracking (appear in graph, no AST)

HTML, CSS/SCSS/SASS/LESS, YAML, JSON, TOML, XML, SQL, Markdown, Dockerfile, Terraform, Shell scripts, Vue, Svelte, Jinja, Handlebars, Dart, R, Perl, PowerShell, Haskell, OCaml, Erlang, Clojure, F#, and 50+ more.

---

## Installation

### Homebrew (macOS / Linux — recommended)

```bash
brew install synapses/tap/synapses
```

### curl installer (macOS / Linux)

```bash
curl -fsSL https://raw.githubusercontent.com/synapses/synapses/main/install.sh | sh
```

This downloads the latest pre-built binary from GitHub Releases and places it in `/usr/local/bin` (or `~/.local/bin` if you prefer no `sudo`).

### Pre-built binaries (all platforms)

Download the binary for your OS and architecture from the [latest GitHub Release](https://github.com/synapses/synapses/releases/latest):

| Platform | File |
|---|---|
| macOS (Apple Silicon) | `synapses_darwin_arm64.tar.gz` |
| macOS (Intel) | `synapses_darwin_amd64.tar.gz` |
| Linux (x86-64) | `synapses_linux_amd64.tar.gz` |
| Linux (ARM64) | `synapses_linux_arm64.tar.gz` |
| Windows (x86-64) | `synapses_windows_amd64.zip` |

Extract and place the `synapses` binary somewhere on your `PATH`.

### go install (Go developers)

```bash
go install github.com/synapses/synapses/cmd/synapses@latest
```

Requires Go 1.24+. Binary lands in `$GOPATH/bin` (usually `~/go/bin`).

### From source

```bash
git clone https://github.com/synapses/synapses
cd synapses
make install        # installs to $GOPATH/bin
```

Make sure `$GOPATH/bin` is in your `PATH`:

```bash
echo 'export PATH="$PATH:$HOME/go/bin"' >> ~/.zshrc && source ~/.zshrc
```

---

## Quick Start

```bash
cd /your/project
synapses init
```

That's it. `init` does three things automatically:
1. Indexes your project (or loads from cache if already indexed)
2. Writes `.mcp.json` in your project root with the correct absolute path
3. Tells you exactly how to reload Claude Code

Then in Claude Code, type `/mcp` to reload — or just close and reopen the chat panel.

Your agent immediately has access to all 16 MCP tools — from `get_project_identity` and `get_context` to `get_usage_guide` (which tells the agent exactly when to use each tool) and `get_pending_tasks` (session continuity across LLM conversations).

**Re-running `init` is safe** — it updates only the `synapses` entry in `.mcp.json` and preserves any other MCP servers you have configured.

---

## Commands

| Command | Description |
|---|---|
| `synapses init` | **Index + write `.mcp.json` — the one command you need** |
| `synapses index -path <dir>` | Index only (no .mcp.json written) |
| `synapses start -path <dir>` | Start MCP server manually (blocks) |
| `synapses status -path <dir>` | Show stats for one indexed project |
| `synapses list` | Global overview of all indexed projects |
| `synapses reset -path <dir>` | Remove one project's index |
| `synapses reset -all` | Remove all indexes |
| `synapses version` | Print version |

Full flag reference: see [COMMANDS.md](COMMANDS.md).

---

## MCP Tools

### Orientation

| Tool | Parameters | What it returns |
|---|---|---|
| `get_project_identity` | — | Node/edge counts, entry points, key entities by connectivity, active rules |
| `get_usage_guide` | — | Quick-start sequence, per-tool catalogue, live entry points, top-5 key entities |
| `get_working_state` | `window_minutes?` | Recent file changes from the watcher + `git diff --stat HEAD` |

### Context & Discovery

| Tool | Parameters | What it returns |
|---|---|---|
| `get_context` | `entity`, `depth?`, `token_budget?`, `task_id?` | BFS subgraph around entity — ranked callers, callees, and related nodes |
| `get_file_context` | `file` | All entities defined in a file, ordered by line number |
| `get_api_contract` | `package?`, `file?` | HTTP/gRPC endpoint detection by convention; returns framework, callers, and callees per endpoint |
| `find_entity` | `query` | Matching nodes with file, line, signature, and doc |
| `search` | `query` | Keyword search across entity names and doc comments — returns signature |
| `get_call_chain` | `from`, `to` | Shortest CALLS path between two entities; crosses interface boundaries via IMPLEMENTS |
| `find_orphans` | `include_tests?` | Unexported functions/methods with no callers (dead-code candidates) |

### Architecture & Rules

| Tool | Parameters | What it returns |
|---|---|---|
| `validate_plan` | `changes` (JSON) | Rule violations for proposed call-graph changes, without touching the live graph |
| `get_violations` | — | All current rule violations across the graph |
| `upsert_rule` | `rule_id`, `description`, `severity`, `edge_type?`, `from_file_pattern?`, `to_file_pattern?`, `to_name_pattern?` | Creates or updates a dynamic architectural rule; persisted to SQLite, active immediately |
| `get_federation_status` | — | Linked project node counts and cross-project CALLS edge count |

### Agent Task Memory

| Tool | Parameters | What it returns |
|---|---|---|
| `create_plan` | `title`, `tasks`, `description?` | Persisted plan with prioritised task list (p0–p3) |
| `get_pending_tasks` | `plan_id?` | All pending/in-progress tasks ordered by priority |
| `update_task` | `id`, `status`, `notes?` | Updated task — use to track progress across sessions |
| `get_plans` | — | All plans with task completion counts |

---

## Configuration (`synapses.json`)

Place a `synapses.json` in your project root to define architectural rules and tuning parameters. The file is optional — Synapses works with zero configuration.

```json
{
  "version": 1,
  "rules": [
    {
      "id": "no-sql-in-view",
      "description": "Database queries must not appear in view/component files",
      "forbidden_edge": {
        "from_file_pattern": "*.tsx",
        "edge_type": "CALLS",
        "to_name_pattern": "SELECT|INSERT|UPDATE|DELETE"
      },
      "severity": "error"
    },
    {
      "id": "no-direct-db-from-handler",
      "description": "HTTP handlers must go through the service layer",
      "forbidden_edge": {
        "from_file_pattern": "*/handlers/*",
        "to_file_pattern": "*/db/*",
        "edge_type": "IMPORTS"
      },
      "severity": "warning"
    }
  ],
  "context_carve": {
    "default_depth": 2,
    "decay_factor": 0.5,
    "token_budget": 4000
  }
}
```

See `synapses.example.json` for a full reference with all options.

---

## How Context Carving Works

When an agent calls `get_context("AuthService", depth=2)`:

1. Synapses finds the `AuthService` node in the graph.
2. BFS expands outward up to `depth` hops, following edges in both directions.
3. Each node gets a **relevance score** = `edge_weight × decay^hop_distance`.
   - Edge weights: `CALLS=1.0`, `IMPLEMENTS=0.9`, `EMBEDS=0.85`, `DEPENDS_ON=0.8`, `IMPORTS=0.7`, `EXPORTS=0.5`, `DEFINES=0.15`
   - Default decay: `0.5` (each hop halves relevance)
4. Nodes are sorted by relevance score, then pruned to fit within `token_budget`.
5. The result is a compact, ranked subgraph — not a file dump.

This gives the agent the exact structural context it needs without hallucination-inducing noise.

---

## Architecture

```
synapses/
├── cmd/synapses/        # CLI entry point (start, index, status, list, reset)
├── internal/
│   ├── graph/           # In-memory graph engine
│   │   ├── types.go     # Node/Edge types, CarveConfig
│   │   ├── graph.go     # Thread-safe graph (Add, Find, Remove, ProjectIdentity)
│   │   ├── traverse.go  # BFS ego-graph carver with relevance decay
│   │   └── cache.go     # 20-entry FIFO subgraph cache (30s TTL)
│   ├── parser/          # Tree-sitter → graph mapper (18 languages + generic fallback)
│   ├── resolver/        # Post-parse CALLS + IMPLEMENTS edge resolution
│   ├── store/           # SQLite persistence (SaveGraph, LoadGraph, task memory)
│   ├── config/          # synapses.json loader and rule checker
│   ├── watcher/         # fsnotify file watcher with 150ms debounce
│   └── mcp/             # MCP server and 16 tool handlers
└── synapses.example.json
```

---

## Performance Targets

| Operation | Target |
|---|---|
| Cold parse (10k files) | < 30s |
| Incremental re-parse on save | < 100ms |
| `get_context` query | < 10ms |
| `validate_plan` | < 5ms |
| `get_project_identity` | < 50ms |
| Cache load (any size) | < 1s |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

---

## License

MIT — see [LICENSE](LICENSE).
