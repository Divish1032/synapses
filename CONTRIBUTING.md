# Contributing to Synapses

Thank you for your interest in contributing. This document covers how to get started, the conventions we follow, and what we're looking for.

## Development Setup

**Prerequisites:**
- Go 1.24+
- A C compiler (required for tree-sitter CGo bindings)
- `make`

```bash
git clone https://github.com/synapses/synapses
cd synapses
go mod tidy
make build
make test
```

## Project Structure

```
cmd/synapses/        Entry point — CLI flags, wiring
internal/graph/      Core graph engine. No external dependencies.
internal/parser/     Tree-sitter integration. One file per language.
internal/resolver/   Post-parse CALLS + IMPLEMENTS edge resolution.
internal/store/      SQLite persistence. One connection, one schema.
                       store.go    — graph nodes, edges, call sites
                       tasks.go    — agent task memory (plans + tasks)
internal/config/     Config loading. Parses synapses.json.
internal/mcp/        MCP server. Thin handlers that delegate to graph/.
                       tools.go       — structural tools (get_context, search, …)
                       task_tools.go  — agent memory tools (create_plan, …)
internal/watcher/    fsnotify-based file watcher with 150ms debounce.
```

**The dependency rule:** packages closer to `internal/graph/` must not import packages closer to `internal/mcp/`. The graph is the core. MCP is the interface.

## Architecture Overview

### Data Flow

```
Source files
     │
     ▼
 Walker.Walk()          (internal/parser/parser.go)
     │   Dispatches each file to the matching LanguageParser.
     │   Falls back to GenericParser for unrecognised extensions.
     ▼
 LanguageParser.Parse() (internal/parser/{language}.go)
     │   Runs tree-sitter queries against the file's AST.
     │   Calls g.AddNode / g.AddEdge for each entity found.
     │   Registers raw call sites via g.AddCallSite (Go parser).
     ▼
 resolver.ResolveCallEdges / ResolveImplementsEdges
     │   (internal/resolver/)
     │   Drains raw call sites, resolves CALLS edges cross-file.
     │   Adds IMPLEMENTS edges for structs satisfying interfaces.
     ▼
 graph.Graph            (internal/graph/graph.go)
     │   Thread-safe in-memory store.
     │   Keyed by NodeID = "repoID::file::name".
     ▼
 store.SaveGraph()      (internal/store/store.go)
     │   Persists nodes + edges to SQLite (pure-Go driver, no CGo).
     │   Subsequent startups call LoadGraph() — parse-once semantics.
     ▼
 mcp.Server             (internal/mcp/server.go)
     │   Exposes 16 MCP tools over stdio using the MCP protocol.
     │   Each tool delegates to graph.Graph methods.
     ▼
 AI Agent
```

### Key Invariants

- **NodeID uniqueness**: `g.MakeNodeID(file, name)` produces `"<repoID>::<file>::<name>"`. Two different files that define an entity with the same name get distinct IDs.
- **Idempotent AddNode**: calling `AddNode` with an existing ID is a no-op. Parsers may safely re-add nodes (e.g. after incremental re-parse).
- **Edge deduplication**: SQLite schema uses `PRIMARY KEY (from_id, to_id, type)`. In-memory graph uses a `map` keyed by the same triple.
- **Query degradation**: if a tree-sitter query fails to compile (e.g. wrong node type for a grammar), `runQuery` logs to stderr and returns nil — the file node is always emitted even if entity queries produce nothing.

### Context Carving

`graph.CarveEgoGraph(root, config)` implements a BFS that:
1. Starts at the root node.
2. Follows edges in both directions up to `config.MaxDepth` hops.
3. Scores each visited node: `relevance = edgeWeight × (decayFactor ^ hop)`.
4. Sorts by relevance descending.
5. Prunes to `config.TokenBudget` (1 token ≈ 4 bytes of JSON).

Default weights: `CALLS=1.0`, `IMPLEMENTS=0.9`, `EMBEDS=0.85`, `DEPENDS_ON=0.8`, `IMPORTS=0.7`, `EXPORTS=0.5`, `DEFINES=0.15`. Override via `edge_weights` in `synapses.json`.

`DEFINES` is intentionally low (0.15) — file→entity edges would otherwise turn every file node into a hub with equal, misleading relevance scores for all sibling functions.

## Architecture Decision Records

### ADR-001: Pure-Go SQLite (modernc.org/sqlite)

**Decision:** Use `modernc.org/sqlite` instead of `mattn/go-sqlite3`.

**Rationale:** `mattn/go-sqlite3` requires CGo, which complicates cross-compilation and CI. `modernc.org/sqlite` is a pure-Go port of the same SQLite C code (generated via cgo2go), requires no C toolchain at runtime, and supports `GOOS=windows` out of the box. The performance difference is negligible for our access patterns (one write at index time, fast reads thereafter).

**Trade-offs:** Slightly larger binary. No WAL mode by default (mitigated by `SetMaxOpenConns(1)`).

---

### ADR-002: Tree-sitter via smacker/go-tree-sitter

**Decision:** Use `github.com/smacker/go-tree-sitter` with bundled grammar packages.

**Rationale:** Tree-sitter provides incremental, error-tolerant parsing for 20+ languages through a uniform C API. The smacker wrapper exposes this as Go with pre-compiled grammars, avoiding manual grammar management. Each language parser is isolated to one file and one Go struct — adding a new language is mechanical (see below).

**Trade-offs:** Requires CGo. Bundled grammars may lag upstream grammar changes; node type names must be verified against the actual bundled version, not the latest grammar documentation.

---

### ADR-003: In-process MCP Server (stdio)

**Decision:** The MCP server runs in the same process as the indexer, communicating over stdio.

**Rationale:** MCP agents (Claude Code, Cursor, etc.) launch servers as child processes via `stdio` transport. Running in-process eliminates a network hop, keeps deployment to a single binary, and avoids port conflicts. The file watcher runs as a goroutine in the same process, updating the in-memory graph on save.

**Trade-offs:** The indexer and MCP server share the same process lifetime. A crash kills both. Acceptable for a CLI tool — the agent restarts the process on the next tool call.

---

### ADR-004: NodeID Format

**Decision:** `NodeID = "<repoID>::<file>::<name>"` (double-colon separator).

**Rationale:** File paths can contain single colons on Windows (`C:\...`). Double colon is illegal in file paths on all supported platforms and unlikely in function names, making splitting unambiguous.

**Trade-offs:** NodeIDs are opaque strings to callers — always use `g.MakeNodeID()` rather than constructing them manually.

---

### ADR-005: Parse-Once / Load-Fast Cache Strategy

**Decision:** Index is stored in a user-scoped OS cache directory (`~/.cache/synapses/cache/` on Linux, `~/Library/Caches/synapses/cache/` on macOS). The filename is `<reponame>_<fnv32(absPath)>.db`.

**Rationale:** Keeping the cache outside the project tree avoids polluting `.gitignore`, prevents accidental commits of binary cache files, and works across multiple checkout locations of the same repo. FNV-32 of the absolute path provides a collision-resistant suffix without external dependencies.

**Trade-offs:** If the cache directory is wiped (e.g. `synapses reset -all`), a full re-parse is required.

## Submitting Changes

1. Fork the repository and create a branch: `git checkout -b feat/your-feature`
2. Write tests for new behaviour
3. Ensure `make test` and `make vet` pass
4. Keep commits focused — one logical change per commit
5. Open a pull request with a clear description of _why_, not just _what_

## Code Style

- `gofmt -s` is non-negotiable. Run `make fmt` before committing.
- Follow standard Go conventions: [Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- Exported symbols must have doc comments
- No `init()` functions
- Error strings are lowercase and do not end with punctuation

## Adding a Language Parser

1. Create `internal/parser/{language}.go`
2. Implement the `LanguageParser` interface (see `internal/parser/parser.go`)
3. Register it in `internal/parser/parser.go` `NewWalker()`
4. Add the tree-sitter grammar to `go.mod` and `go.sum` (`go get github.com/smacker/go-tree-sitter/{language}`)
5. Write tests in `internal/parser/languages_test.go` covering: file node creation, entity extraction, DEFINES edges, and empty-file safety

**Discovering node type names:** The bundled grammars may differ from upstream docs. To find the real node types for a given grammar, write a small diagnostic:

```go
parser := sitter.NewParser()
parser.SetLanguage(yourlang.GetLanguage())
tree := parser.Parse(nil, []byte(sampleSrc))
var walk func(n *sitter.Node, depth int)
walk = func(n *sitter.Node, depth int) {
    fmt.Printf("%s%s\n", strings.Repeat("  ", depth), n.Type())
    for i := 0; i < int(n.ChildCount()); i++ {
        walk(n.Child(i), depth+1)
    }
}
walk(tree.RootNode(), 0)
```

## Adding an MCP Tool

1. Write the handler in `internal/mcp/tools.go` (structural) or `internal/mcp/task_tools.go` (agent memory)
2. Register it in `internal/mcp/server.go` `New()`
3. Update the MCP Tools table in `README.md` and `COMMANDS.md`

## Reporting Issues

Please use GitHub Issues. Include:
- OS and Go version
- The command you ran
- The output you got vs. what you expected
- A minimal reproducing project if possible

## What We Are Not Looking For (Right Now)

- GUI / Tauri frontend — deferred to a later phase
- Vector/embedding search — deferred
- Multi-repo sync — deferred
- Cloud sync or telemetry of any kind

Focus is on graph accuracy and MCP reliability for a single repo.
