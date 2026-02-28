# Synapses CLI Commands

## Build

```bash
make build          # compile → bin/synapses
make install        # install to $GOPATH/bin (makes `synapses` available globally)
make test           # run all tests with race detector
make lint           # run golangci-lint (requires golangci-lint v2 installed)
make clean          # remove bin/ and coverage files
```

---

## Commands

### `setup` — First-time configuration wizard (recommended)

```bash
synapses setup
synapses setup -path ./my-project
synapses setup -core               # skip brain sidecar, configure Tier 1 only
```

Checks whether the `brain` sidecar is installed, runs `brain setup` to configure Ollama and pull the model, writes `synapses.json` with the brain URL, then prints the `claude mcp add` command to wire everything into Claude Code.

---

### `init` — Index + write `.mcp.json` (zero-friction onboarding)

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

Prints node/edge counts, entry points, top connected entities, active rules, and the top tools used in the last 7 days (from the `tool_calls` usage log).

---

### `query` — Look up a single entity by name (JSON output)

```bash
synapses query -entity Store
synapses query -path ./my-project -entity CarveEgoGraph
```

Loads the cached graph in read-only mode and outputs a JSON object with the entity's name, type, file, line, doc, signature, callers, callees, and metadata. Safe to run concurrently with a running MCP server (WAL mode). Used internally by the VS Code extension hover provider.

---

### `export` — Export the graph as DOT, Mermaid, or GraphML

```bash
synapses export -format dot
synapses export -format mermaid -entity CarveEgoGraph -depth 3
synapses export -format graphml -path ./my-project -meta
```

Writes the graph (or an ego-subgraph around `--entity`) to stdout. Without `--entity`, exports the full graph minus file/package hub nodes.

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
| `-path <dir>` | all | `.` | Repository root |
| `-reindex` | `init`, `start`, `index` | false | Force full re-parse, ignore cache |
| `-no-watch` | `start` | false | Disable file watcher |
| `-all` | `reset` | false | Remove all project indexes |
| `-core` | `setup` | false | Skip brain sidecar setup (Tier 1 only) |
| `-entity <name>` | `query`, `export` | — | Entity name to look up or use as ego root |
| `-format <fmt>` | `export` | `dot` | Output format: `dot`, `mermaid`, or `graphml` |
| `-depth <n>` | `export` | `2` | BFS depth for ego-subgraph export |
| `-meta` | `export` | false | Include signature metadata in node labels |

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
| `get_project_identity` | — | Compact architectural summary: node/edge counts, entry points, key entities, active rules, workflow hints, and federation status |
| `get_usage_guide` | — | Quick-start sequence, per-tool `when_to_use` catalogue, live entry points, implementation workflow, antipatterns |
| `get_working_state` | `window_minutes` (opt) | Recent file-change events from the watcher + best-effort `git diff --stat HEAD`; includes suggested next tools |
| `sdlc` | `action` ("get"/"set"), `phase` | Get or set the current SDLC phase (planning/development/testing/review/deployment) and quality mode; returns safe defaults when brain is not configured |

### Context & Discovery

| Tool | Parameters | Description |
|---|---|---|
| `get_context` | `entity` (req), `depth`, `token_budget`, `task_id`, `mode` | Relevance-ranked ego-subgraph using BFS with edge-type-weighted decay. `mode="impact"` does reverse-BFS (same as get_impact) |
| `find_entity` | `query` (req) | Locate nodes by exact name or substring; returns file, line, signature, doc, and annotations |
| `get_file_context` | `file` (req) | All entities defined in a file, ordered by line number. Accepts partial path suffix |
| `get_api_contract` | `package` (opt), `file` (opt) | Detect HTTP/gRPC endpoints by framework convention (net/http, Gin, Echo, Fiber, gRPC, proto RPC) + custom `api_entries` patterns |
| `search` | `query` (req), `mode` ("keyword"/"semantic"), `limit` | Keyword search across entity names and doc comments (mode=keyword, default), or FTS BM25 concept search (mode=semantic). CamelCase auto-split |
| `get_call_chain` | `from` (req), `to` (req) | Shortest CALLS path between two entities; follows IMPLEMENTS edges across interface boundaries |
| `semantic_search` | — | Removed — use `search` with `mode=semantic` |

### Architecture & Rules

| Tool | Parameters | Description |
|---|---|---|
| `validate_plan` | `changes` (req JSON) | Check proposed call-graph changes against architectural rules before writing code |
| `get_violations` | `rule_id` (opt), `include_log` (opt), `log_limit` (opt) | List current rule violations; pass `include_log=true` to also return the historical audit trail |
| `upsert_rule` | `rule_id` (req), `description` (req), `severity` (req), `edge_type`, `from_file_pattern`, `to_file_pattern`, `to_name_pattern` | Create or update a dynamic architectural rule; persisted and active immediately — no restart needed |
| `get_impact` | `symbol` (req), `depth` | Blast-radius analysis: reverse-BFS to find everything that breaks if the entity changes; tiered by confidence |
| `get_communities` | `include_nodes`, `max_iterations`, `min_community_size` | Emergent community clusters via Label Propagation; mixed-package communities reveal hidden coupling |
| `find_data_paths` | `source` (opt), `sink` (opt) | Find DATA_FLOWS paths from HTTP/parser sources to SQL/exec/file sinks; answers "can user input reach this DB call?" |
| `find_orphans` | `include_tests`, `min_confidence` | Unexported functions/methods with zero callers; excludes interface implementors and framework entry points |

### Code Intelligence & Metrics

| Tool | Parameters | Description |
|---|---|---|
| `get_context` | (see above) | Context packets enriched with brain summaries, SDLC-phase-specific insights, and active claim status when brain is configured |
| `annotate_node` | `node_id` (req), `note` (req), `agent_id` (opt) | Attach a persistent note to a graph node, visible to all agents via get_context — shared whiteboard |
| `log_decision` | `agent_id` (req), `entity_name` (req), `action` (req), `outcome`, `phase`, `related_entities`, `notes` | Record an architectural decision for audit and future context; requires brain configured |

### Change Analysis

| Tool | Parameters | Description |
|---|---|---|
| `detect_changes` | `ref` (opt) | Map a git diff to affected graph symbols; answers "which symbols were touched?" |
| `get_change_coupling` | `commit_limit`, `min_confidence` | Files that frequently change together (hidden coupling); based on git log co-occurrence |
| `get_events` | `since_seq`, `types`, `limit` | Pull-based event log (file_change, task_update, annotation_added, agent_activity); poll with cursor for multi-agent coordination |

### Agent Task Memory

| Tool | Parameters | Description |
|---|---|---|
| `create_plan` | `title` (req), `tasks` (req JSON), `description`, `agent_id` | Create a persisted plan with prioritised tasks (p0–p3) for session continuity; auto-links nodes from task text |
| `get_pending_tasks` | `plan_id` (opt), `agent_id` (opt) | All pending/in-progress tasks ordered by priority (p0 first); auto-claims unassigned tasks for the requesting agent |
| `update_task` | `id` (req), `status` (req), `notes`, `agent_id` | Update task status; append timestamped notes for the next session |
| `get_plans` | — | List all plans with task completion counts |
| `link_task_nodes` | `task_id` (req), `query` (opt) | Explicitly link a task to graph nodes; auto-scans task text if query omitted; merges with existing links |
| `get_my_tasks` | `agent_id` (req), `plan_id` (opt) | Unblocked pending tasks for a specific agent with a suggested next task |

### Agent Coordination

| Tool | Parameters | Description |
|---|---|---|
| `claim_work` | `agent_id` (req), `scope` (req), `scope_type`, `ttl_minutes` | Register active work on a file, package, directory, or entity; returns conflicting claims immediately |
| `get_conflicts` | `agent_id` (req) | All work claims by other agents that overlap with the calling agent's current claims; includes cross-project peer conflicts |
| `release_claims` | `agent_id` (req) | Release all active work claims; call when done editing |
| `proposals` | `action` ("create"/"list"), `title`, `description`, `affected_nodes`, `agent_id`, `vote_threshold` | Create an architectural change proposal or list open proposals waiting for votes |
| `vote_proposal` | `proposal_id` (req), `vote` (req), `agent_id` (req), `rationale` | Vote on (approve/reject/abstain) or withdraw an open architectural proposal |
| `get_agents` | — | All agents that have interacted with Synapses, ordered by last-seen timestamp |

### Inter-Project Federation (Peer API)

| Tool | Parameters | Description |
|---|---|---|
| `list_peers` | — | All configured peer instances with connection status, node count, and shared entity count |
| `get_peer_context` | `project` (req), `entity` (req), `depth` | Get context subgraph for an entity from a peer project's graph |
| `get_dependency_graph` | — | Inter-project dependency overview with Mermaid diagram of cross-project links |

---

## Claude Code Integration

**Recommended:** run `synapses setup` or `synapses init` in your project root. It writes `.mcp.json` automatically with the correct absolute path, then type `/mcp` in Claude Code to reload.

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
