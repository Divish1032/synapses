# Synapses CLI Commands

## Build

```bash
make build          # compile → bin/synapses
make install        # install to $GOPATH/bin (makes `synapses` available globally)
make test           # run all tests with race detector
make lint           # run golangci-lint (requires golangci-lint installed)
make clean          # remove bin/ and coverage files 
```

---

## Commands

### `init` — Index + write `.mcp.json` (recommended first step)

```bash
cd /your/project
synapses init
synapses init -path ./my-project
synapses init -path ./my-project -reindex   # force full re-parse
```

Indexes the project (or loads from cache if already indexed) and writes `.mcp.json` in the project root with the correct absolute path. Safe to re-run — only updates the `synapses` entry and preserves any other MCP servers already configured.

After running `init`, reload MCP servers in Claude Code by typing `/mcp` in the chat panel, or close and reopen the chat.

---

### `index` — Parse and cache a repository

```bash
synapses index
synapses index -path ./my-project
synapses index -path ./my-project -reindex   # force full re-parse
```

Parses all supported source files, builds the in-memory graph, and saves it to the local cache. Exits when done.

**Supported languages (deep AST):** Go, TypeScript, TSX, JavaScript, JSX, Python, Java, Kotlin, Scala, Groovy, Rust, C, C++, C#, Swift, Ruby, PHP, Lua, Elixir, Protocol Buffers — plus file-level tracking for 80+ additional formats (YAML, JSON, Markdown, HTML, CSS, SQL, shell scripts, etc.).

---

### `start` — Start the MCP server

```bash
synapses start
synapses start -path ./my-project
synapses start -path ./my-project -reindex    # force re-index on start
synapses start -path ./my-project -no-watch   # disable file watcher
```

Loads (or builds) the graph, starts the file watcher for live updates, then serves MCP tools over stdio. Blocks until killed (`Ctrl+C` or `SIGTERM`).

---

### `status` — Show index statistics for one project

```bash
synapses status
synapses status -path ./my-project
```

Prints node/edge counts, entry points, and top connected entities from the cached index — without re-parsing.

---

### `list` — Global overview of all indexed projects

```bash
synapses list
```

Scans the Synapses cache directory and prints a summary row for every project that has been indexed. Does not load any graph — reads only the lightweight metadata stored per project.

Example output:
```
PROJECT                          FILES   NODES   EDGES  INDEXED AT
──────────────────────────────  ──────  ──────  ──────  ───────────────────────
my-api                              40     351     424  2026-02-23 10:15:28
  /Users/you/projects/my-api
frontend                           120     890    1203  2026-02-22 18:40:11
  /Users/you/projects/frontend

2 project(s) indexed
```

---

### `reset` — Remove cached index

```bash
synapses reset                  # reset current directory's index
synapses reset -path ./my-project
synapses reset -all             # remove ALL project indexes
```

Deletes the SQLite cache file(s). Source files are never touched. Run `index` again to rebuild.

`reset -all` lists every project it removes before wiping.

---

### `version`

```bash
synapses version
```

---

### `help`

```bash
synapses help
```

---

## Flags Reference

| Flag | Commands | Default | Description |
|---|---|---|---|
| `-path <dir>` | `init`, `start`, `index`, `status`, `reset` | `.` | Repository root |
| `-reindex` | `init`, `start`, `index` | false | Force full re-parse, ignore cache |
| `-no-watch` | `start` | false | Disable file watcher |
| `-all` | `reset` | false | Remove all project indexes |

---

## Cache Location

Indexes are stored outside the project tree in the OS cache directory:

```
~/Library/Caches/synapses/cache/   (macOS)
~/.cache/synapses/cache/           (Linux)
%LocalAppData%\synapses\cache\     (Windows)
```

Each project gets its own `.db` file named by repo folder + FNV path hash, e.g. `my-project_3a4f1b2c.db`. This means renaming/moving the project directory creates a new DB (the old one is orphaned and shows up in `synapses list` until removed with `reset`).

---

## MCP Tools (exposed to AI agents via `start`)

### Orientation

| Tool | Parameters | Description |
|---|---|---|
| `get_project_identity` | — | Compact architectural summary: node/edge counts, entry points, key entities, active rules |
| `get_usage_guide` | — | Quick-start sequence, per-tool `when_to_use` catalogue, live entry points, top-5 key entities |
| `get_working_state` | `window_minutes` (optional) | Recent file-change events from the watcher + best-effort `git diff --stat HEAD` |

### Context & Discovery

| Tool | Parameters | Description |
|---|---|---|
| `get_context` | `entity` (required), `depth`, `token_budget`, `task_id` | N-hop ego-subgraph split into callers, callees, and related sections with relevance decay |
| `get_file_context` | `file` (required) | All entities defined in a file, ordered by line number |
| `get_api_contract` | `package` (optional), `file` (optional) | Detect HTTP/gRPC endpoints by framework convention (net/http, Gin, Echo, Fiber, gRPC, proto RPC) + custom `api_entries` patterns; returns framework, callers, callees per endpoint |
| `find_entity` | `query` (required) | Locate nodes by exact name or substring; returns file, line, signature, and doc |
| `search` | `query` (required) | Keyword search across entity names and doc comments; returns signature and file-path matches |
| `get_call_chain` | `from` (required), `to` (required) | Shortest CALLS path between two entities; follows IMPLEMENTS edges across interface boundaries |
| `find_orphans` | `include_tests` (optional) | Unexported functions/methods with zero callers — dead-code candidates |

### Architecture & Rules

| Tool | Parameters | Description |
|---|---|---|
| `validate_plan` | `changes` (required JSON) | Check proposed call-graph changes against architectural rules without modifying the live graph |
| `get_violations` | — | List all current rule violations across the entire graph |
| `upsert_rule` | `rule_id` (required), `description` (required), `severity` (required), `edge_type`, `from_file_pattern`, `to_file_pattern`, `to_name_pattern` | Create or update a dynamic rule; persisted to SQLite and active immediately — no restart needed |
| `get_federation_status` | — | Node counts per linked project and number of cross-project CALLS edges |

### Agent Task Memory

| Tool | Parameters | Description |
|---|---|---|
| `create_plan` | `title` (required), `tasks` (required JSON), `description` | Create a persisted plan with prioritised tasks (p0–p3) for session continuity |
| `get_pending_tasks` | `plan_id` (optional) | All pending/in-progress tasks ordered by priority (p0 first) |
| `update_task` | `id` (required), `status` (required), `notes` | Update task status and append timestamped notes for the next session |
| `get_plans` | — | List all plans with task completion counts |

---

## Claude Code Integration

**Recommended:** run `synapses init` in your project root. It writes `.mcp.json` automatically with the correct absolute path, then type `/mcp` in Claude Code to reload.

**Manual setup** — add to `.mcp.json` in your project root:

```json
{
  "mcpServers": {
    "synapses": {
      "type": "stdio",
      "command": "synapses",
      "args": ["start", "-path", "/absolute/path/to/your/project"]
    }
  }
}
```

**User-scoped** (works across all projects, no per-project file needed):

```bash
claude mcp add --scope user synapses -- synapses start -path /absolute/path/to/your/project
```

---

## Claude Desktop Integration

Add to `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "synapses": {
      "command": "/absolute/path/to/bin/synapses",
      "args": ["start", "-path", "/absolute/path/to/your/project"]
    }
  }
}
```

Restart Claude Desktop after editing.

---

## Makefile Shortcuts

If you installed via source, these shortcuts build + run against the current directory:

```bash
make run/index    # index current repo
make run/start    # start MCP server for current repo
make run/status   # show stats for current repo
make run/reset    # remove index for current repo
make run/reset-all # remove ALL indexes

# Target a different repo
make run/index REPO_PATH=../other-project
```
