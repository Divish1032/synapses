# Contributing to Synapses

Thank you for your interest in contributing to Synapses! We welcome pull requests, issues, and discussions.

## Prerequisites

- **Go 1.22+** — Download from [golang.org](https://golang.org/dl)
- **make** — Standard build tool
- **golangci-lint** — Linter (v2.10+): `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest`
- **Node.js 18+** — Required only if modifying the web console UI (`web/console/`)
- **npm** — Comes with Node.js

## Development Setup

```bash
# Clone the repository
git clone https://github.com/SynapsesOS/synapses.git
cd synapses

# Build the binary
make build
./bin/synapses version

# Run tests
make test           # Full test suite with race detector
make test/cover     # Generate coverage report
```

## Code Style

Synapses follows standard Go conventions:

```bash
make fmt    # Format with gofmt
make vet    # Run go vet
make lint   # Run golangci-lint
```

All PRs must pass linting. Run these before pushing:

```bash
make fmt && make vet && make lint && make test
```

## Project Structure

```
synapses/
├── cmd/synapses/                       # Binary entry point — CLI commands, daemon, proxy
│   ├── main.go                         # Command dispatch
│   ├── daemon_serve.go                 # Singleton daemon + HTTP server (port 11435)
│   ├── daemon.go                       # Service install/uninstall (launchd/systemd)
│   ├── registry.go                     # Project registry (singleflight, in-memory)
│   ├── projects.go                     # Project persistence (~/.synapses/projects.json)
│   ├── proxy.go                        # Stdio proxy (bridges agent ↔ daemon via Unix socket)
│   ├── init.go                         # `synapses init` — interactive setup wizard
│   ├── socket_activation_darwin.go     # launchd socket activation (pure Go, no cgo)
│   ├── socket_activation_linux.go      # systemd socket activation
│   ├── socket_activation_other.go      # Fallback (no-op) for unsupported platforms
│   └── selfupdate.go                   # Self-update logic (GitHub Releases)
├── internal/
│   ├── brain/          # In-process LLM layer (Ollama / llama-server / local GGUF)
│   ├── graph/          # In-memory code graph (nodes, edges, BFS)
│   ├── mcp/            # MCP server — tool registration and handlers
│   │   ├── server.go       # Server constructor, hooks, tool registration
│   │   ├── rate_limiter.go # Agent-scoped token bucket rate limiting
│   │   └── loop_guard.go   # Suffix cycle detection (catches A-B-A-B patterns)
│   ├── parser/         # 49+ language parsers (tree-sitter)
│   ├── store/          # SQLite persistence (graph, episodic memory, tasks)
│   ├── embed/          # ONNX embedding model (all-MiniLM-L6-v2)
│   ├── scout/          # HTTP client for synapses-scout sidecar
│   └── watcher/        # fsnotify-based incremental re-indexer
└── web/
    └── console/        # React/TypeScript web console (built to web/console/dist/)
```

## Architecture Guidelines

These principles guide all contributions:

**No CGo** — Pure Go only. No C dependencies at runtime. Use `modernc.org/sqlite` (pure Go) instead of `github.com/mattn/go-sqlite3` (CGo).

**Fail-Silent** — MCP tools and sidecar clients (`brain`, `scout`, `embed`, `peer`) should never panic. Return an error message instead:
```go
// ✅ Good
if err := client.Call(); err != nil {
    return nil, fmt.Errorf("brain unavailable: %w", err)  // Tool returns error, doesn't panic
}

// ❌ Bad
if err := client.Call(); err != nil {
    panic(err)  // Crashes the MCP server
}
```

**No Circular Imports** — Use `interface{}` fields to decouple. Example: `mcp.Server` uses `interface{}` for brain/scout to avoid import cycles.

**Columnar Index** — The `GraphIndex` is built asynchronously. BFS falls back to pointer-based navigation until ready. See `internal/graph/index.go`.

**Daemon Reliability** — The singleton daemon serves ALL projects. A crash affects every connected agent. Follow these rules:

- **Never panic in tool handlers.** Return `mcp.NewToolResultError(msg)` instead. mcp-go's `WithRecovery()` is enabled as a safety net, but it's a fallback, not a strategy.
- **Use `defer recover()` in HTTP handlers.** The `/mcp` route already has this. If you add new routes that can call into project-specific code, wrap them the same way.
- **Socket activation is the production path.** The daemon detects launchd/systemd activated sockets at startup. If you change the listener setup in `daemon_serve.go`, preserve the `trySocketActivation()` → `net.Listen()` fallback chain.
- **Persist project registrations.** Call `go saveKnownProject(absPath)` after any successful `GetOrSet` that initializes a project. This enables eager warming on next daemon restart.
- **Rate limits are agent-scoped.** Buckets are keyed by `agent_id:project_id`, not MCP session ID. Don't assume a new session means a new rate limit budget.
- **Loop guard detects cycles.** The suffix cycle detection algorithm catches patterns of any length (1-10). If you modify `loop_guard.go`, run `TestLoopGuardSession_cycleDetection` to verify alternating patterns are still caught.

## Adding a New MCP Tool

To add a new MCP tool (e.g., `my_new_tool`):

1. **Define the handler** in `internal/mcp/tools.go`:
```go
func (s *Server) handleMyNewTool(params map[string]interface{}) (interface{}, error) {
    name, ok := params["name"].(string)
    if !ok {
        return nil, fmt.Errorf("name parameter required")
    }
    // Implementation
    return result, nil
}
```

2. **Register in `registerTools()`**:
```go
tool := &mcp.Tool{
    Name: "my_new_tool",
    Description: "Does something useful with a code entity",
    InputSchema: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "name": map[string]interface{}{"type": "string", "description": "Entity name"},
        },
        "required": []string{"name"},
    },
    Handler: s.handleMyNewTool,
}
s.tools = append(s.tools, tool)
```

3. **Add a test** in `internal/mcp/` or `internal/graph/` depending on the tool's focus.

## Adding a Language Parser

To add support for a new language (e.g., Rust):

1. **Implement the `LanguageParser` interface** in a new file `internal/parser/rust.go`:
```go
type RustParser struct{}

func (p *RustParser) Extensions() []string {
    return []string{".rs"}
}

func (p *RustParser) ParseFile(g *graph.Graph, path string, content string) error {
    // Use tree-sitter or regex to extract functions, structs, impls
    // Call g.AddNode() and g.AddEdge() for each entity
    return nil
}

func (p *RustParser) Language() string {
    return "Rust"
}
```

2. **Register in `internal/parser/parser.go`**:
```go
func (w *Walker) ParseFile(g *graph.Graph, path string) error {
    // ...
    case strings.HasSuffix(path, ".rs"):
        return new(RustParser).ParseFile(g, path, content)
}
```

3. **Add tests** in `internal/parser/rust_test.go`.

## Working on the Web Console

The web console lives in `web/console/` (React + TypeScript + Vite). To run it in dev mode:

```bash
cd web/console
npm install
npm run dev        # Vite dev server at http://localhost:5173
```

To build the production bundle (embedded in the binary via `//go:embed`):

```bash
cd web/console
npm run build      # outputs to web/console/dist/
```

Then rebuild the Go binary:
```bash
make build
```

> **Tip**: During local development you can serve the dev bundle directly by running `npm run dev` and visiting `http://localhost:5173`. The Go binary does not need to be running for frontend work.

## Running Tests

```bash
# Run all tests
make test

# Run tests for a specific package
go test ./internal/mcp/ -v

# Run a specific test
go test -run TestGetContext ./internal/mcp/ -v

# With coverage
make test/cover
open coverage.html
```

All tests use Go's standard `testing` package. No external test frameworks.

## Submitting a Pull Request

1. **Fork and branch** — Create a feature branch from `main`:
   ```bash
   git checkout -b feature/my-new-tool
   ```

2. **Make your changes** — Follow code style, add tests, update docs.

3. **Run tests locally**:
   ```bash
   make fmt && make vet && make lint && make test
   ```

4. **Commit with clear messages**:
   ```bash
   git commit -m "Add my_new_tool MCP tool for querying entities"
   ```

5. **Push and create PR** on GitHub.

6. **CI will run** — GitHub Actions runs tests on Linux, macOS, Windows. All must pass.

7. **Code review** — A maintainer will review and suggest changes.

## Reporting Issues

- **Bug reports**: Include minimal reproduction steps, Go version, OS, output.
- **Feature requests**: Describe the use case and proposed solution.
- **Security**: Report privately to security@synapsesos.dev, not in public issues.

## Code Review Expectations

- Tests are required for new functionality
- Documentation should be updated (docstrings, README, etc.)
- Follow the no-CGo and fail-silent principles
- Avoid adding new external dependencies without discussion

## Questions?

Open a GitHub Discussion or reach out at security@synapsesos.dev.

Happy coding! 🚀
