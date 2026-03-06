# Hybrid Context Delivery: Push + Intent + Precision

## Context

Agents interact with Synapses through 41 pull-only MCP tools. Three failure modes:
1. **Agent forgets to call** — falls back to Read/Grep (happened this session)
2. **Too many round-trips** — understanding a modification needs chaining: `get_context` → `get_impact` → `get_violations` → `get_file_context` (4 calls, ~8000 tokens of JSON)
3. **Wasted tokens** — agent gets full JSON blob when it only needed "is this safe to modify?"

The fix: a three-layer hybrid that takes the best from static tools (simplicity, no execution risk) and dynamic scripting (composability, filtering) without the downsides of either.

---

## Architecture: Three Layers

```
Layer 0: Push (MCP Resources)     — agent gets context WITHOUT asking
Layer 1: Intent (prepare_context) — agent says WHAT, synapses figures out HOW
Layer 2: Precision (existing 41)  — agent knows exactly what it wants
```

---

## Layer 0: MCP Resources (Push)

mark3labs/mcp-go supports `AddResource`, `AddResourceTemplate`, and `WithResourceCapabilities(subscribe, listChanged)`. Synapses currently only uses `WithToolCapabilities`. Adding Resources means agents see context without making tool calls.

### Resource 1: `synapses://active-context`

Static resource — compact project briefing (~200-400 tokens).

Contents: project scale + node count, top 3 recently changed files, active violation count, pending task count + top-priority task, constitution principles.

Format: compact text (reuses `writeNodeHeader`-style formatting from `digest.go`), not JSON.

### Resource 2: `synapses://file/{path}`

Resource template — entity map for any file the agent opens (~100-300 tokens).

Contents: all entities in the file with types, line numbers, exported status. If brain available, includes one-line summaries.

### Resource 3: `synapses://violations`

Static resource — current architectural violations. 0 tokens when clean.

### Update Mechanism: Delta Notifications (not full push)

**Critical**: Resources use `notifications/resources/updated` — the server sends only the URI that changed, NOT the full content. The client decides when to re-read based on its current task urgency. This prevents stdio pipe overload.

File watcher already calls `InvalidatePacketCache()`. Extend to also emit the notification:

```go
func (s *Server) InvalidatePacketCache() {
    s.packetCacheMu.Lock()
    s.packetCache = make(map[string]*packetCacheEntry, packetCacheMax)
    s.packetCacheMu.Unlock()
    // Notify subscribed clients that resources have changed.
    // Client decides when/whether to re-read — no content pushed.
    s.notifyResourceChanged("synapses://active-context")
    s.notifyResourceChanged("synapses://violations")
}
```

### New File: `synapses/internal/mcp/resources.go` (~200-300 lines)

---

## Layer 1: `prepare_context` Tool (Intent-Based)

Highest-impact addition. Agent declares intent, gets one tailored response. Replaces 4-5 tool call chains.

### Tool Schema

```go
mcp.NewTool("prepare_context",
    mcp.WithDescription(
        "Intent-based context assembly. Declare WHAT you need "+
        "(modify, understand, review, debug, add, plan) and a target. "+
        "Synapses composes the right context in one round-trip. "+
        "Replaces multi-tool chains like get_context→get_impact→get_violations.",
    ),
    mcp.WithString("intent", mcp.Required(),
        mcp.Description("'modify' | 'understand' | 'review' | 'debug' | 'add' | 'plan'")),
    mcp.WithString("target", mcp.Required(),
        mcp.Description("Entity name, file path, or search query")),
    mcp.WithString("file",
        mcp.Description("Optional file suffix to disambiguate")),
    mcp.WithString("task_id",
        mcp.Description("Optional task ID for relevance boost")),
    mcp.WithNumber("token_budget",
        mcp.Description("Max tokens. Defaults vary by intent.")),
)
```

### Ambiguity Handling: Choice Map (solves Ghost Search)

When `resolveTarget` finds multiple matches, **don't error out**. Return a choice map with 1-line summaries so the agent can refine without wasting the round-trip:

```
## Ambiguous Target: "Auth" (3 matches)
1. [AuthService] struct · auth/service.go:42
   Summary: Handles JWT-based user authentication
2. [AuthMiddleware] function · middleware/auth.go:15
   Summary: HTTP middleware for token validation
3. [AuthConfig] struct · config/auth.go:8
   Summary: Authentication configuration and defaults

→ Re-call with target="AuthService" or file="auth/service.go" to pin.

## Best-Guess Context (AuthService — highest connectivity):
[...full context for AuthService follows...]
```

The agent gets both the disambiguation AND the best-guess context in one call. Zero wasted round-trips.

Implementation in `resolveTarget`:

```go
type resolvedTarget struct {
    bestNode   *graph.Node
    candidates []*graph.Node   // all matches (len > 1 = ambiguous)
    file       string
    isFile     bool
    isConcept  bool
}
```

Each `assemble*Context` method checks `len(resolved.candidates) > 1` and prepends the choice map before the main content.

### Brain Latency: Async Hydration (solves 5-10s wait)

The brain's insight cache already lives in SQLite (6h TTL). Currently it's reactive — only populated after the first `get_context` call. The fix: **proactive cache warming**.

File watcher detects change → background goroutine pre-computes brain packets for entities in the changed file → stores in SQLite. When `prepare_context` fires, it hits warm cache (SQLite read, ~5ms) instead of cold LLM call (~5-10s).

```go
// In InvalidatePacketCache or file watcher callback:
func (s *Server) warmBrainCache(changedFile string) {
    bc := s.getBrainClient()
    if bc == nil { return }

    // Find entities in the changed file
    entities := s.graph.FindByFile(changedFile)

    go func() {
        for _, entity := range entities {
            // Build context packet in background — result stored in SQLite cache
            _ = bc.BuildContextPacket(context.Background(), brain.ContextPacketRequest{
                Snapshot: brain.SnapshotInput{
                    RootNodeID: string(entity.ID),
                    RootName:   entity.Name,
                    RootType:   string(entity.Type),
                    RootFile:   entity.File,
                },
                EnableLLM: true,
            })
        }
    }()
}
```

Rate-limit: max 5 entities per file change, debounce 2s between warm cycles.

### 6 Intents

All output is **compact text** (not JSON) for token efficiency.

#### `modify` — "I'm about to change this. What do I need to know?"

Internal: `CarveEgoGraph(depth=2)` + `ImpactAnalysis(depth=2)` + violations + annotations + brain packet

```
## Target: [AuthService] struct · service.go:42
Summary: Handles user authentication via JWT tokens.

## Blast Radius (7 affected)
DIRECT: LoginHandler, RegisterHandler, TokenMiddleware
INDIRECT: UserRepository.FindByEmail, SessionStore.Create

## Architecture Rules
OK: no violations for auth/service.go

## Dependencies (callees)
UserRepository.FindByEmail · SessionStore.Create · JWTService.Sign

## Agent Notes
[agent-1, 2h ago] "JWT expiry has a known edge case with timezones"

## Pre-Edit Checklist
- 3 direct callers must remain compatible
- Test file exists: auth/service_test.go
```

Default budget: 3000 tokens.

#### `understand` — "How does this work?"

Internal: `CarveEgoGraph(depth=2)` + brain summaries + ADRs

Output: target header + summary, methods (if struct), calls, called-by, ADRs, related types.

Default budget: 2000 tokens.

#### `review` — "Is this code safe/healthy?"

Internal: `CarveEgoGraph(depth=1)` + `ImpactAnalysis(depth=3)` + violations + brain concerns

Output: target + metadata (complexity, coverage), concerns, violations, blast radius, agent notes.

Default budget: 3000 tokens.

#### `debug` — "Something is broken, help me trace it"

Internal: `CarveEgoGraph(depth=3)` + `get_call_chain` from entry points

Output: target, call path (entry → target), downstream (target → effects), related state.

Default budget: 3500 tokens.

#### `add` — "I want to add new code near this"

Internal: `get_file_context` + sibling entities + applicable rules

Output: file entity map, package conventions, architecture rules for this directory.

Default budget: 1500 tokens.

#### `plan` — "What's the scope of changing this?" (Dry Run)

**The "think before you leap" intent.** Returns the *structure* of the change, not the code context. Prevents agents from starting refactors they can't finish within their token limit.

Internal: `ImpactAnalysis(depth=3)` + `matchRulesForFile` + test detection + `get_file_context` for impacted files

```
## Change Plan: AuthService (auth/service.go)

## Files You'll Touch
1. auth/service.go (target — 6 entities)
2. auth/service_test.go (tests — update required)
3. handlers/login.go (caller — uses AuthService.Login)
4. handlers/register.go (caller — uses AuthService.Register)
5. middleware/auth.go (caller — uses AuthService.ValidateToken)

## Interfaces to Preserve
- AuthService implements Authenticator (auth/interfaces.go:12)
- Any signature change requires updating the interface

## Architecture Rules
- no-db-in-handler: if you add Store calls, keep them in service layer
- auth-must-log: all auth methods must call Logger

## Scope Assessment
Files: 5 · Direct callers: 3 · Interfaces: 1 · Test files: 1
Risk: MEDIUM (3+ callers, interface contract)

## Recommendation
Consider using claim_work(scope="auth/") before starting.
```

Default budget: 2000 tokens.

### Target Resolution

```go
func (s *Server) resolveTarget(target, fileHint string) (*resolvedTarget, error) {
    // 1. Try as entity name (FindByName) — exact match
    // 2. If file hint provided, filter by suffix
    // 3. Try as file path (FindByFile) — all entities in file
    // 4. Try pattern match (FindByPattern) — substring/fuzzy
    // 5. Fall through to semantic search (FTS5 BM25)
    // Never returns error on no match — returns isConcept=true for fallback
}
```

### New File: `synapses/internal/mcp/intents.go` (~500-600 lines)

Contains: `handlePrepareContext`, `resolveTarget`, 6 `assemble*Context` methods, `intentDefaultBudget`, choice map formatter.

Each `assemble*` method reuses existing internal functions — **no new graph logic**:
- `CarveEgoGraph` → `traverse.go`
- `ImpactAnalysis` → `traverse.go`
- `serializeCompact`, `writeNodeHeader` → `digest.go`
- `toDirectionalContext`, `normalizeSubgraph`, `pickBestNode` → `tools.go`
- `matchRulesForFile`, `fileHasTests` → `tools.go`
- Brain packet building (tools.go:223-278) → extract into shared helper `buildBrainPacket()`

---

## Layer 2: Existing Tools (Unchanged)

All 41 tools remain as precision escape hatches. Two small changes:

1. `suggestNextAfterContext` updated to suggest `prepare_context` as first-choice when the agent likely needs a broader view
2. Brain packet building extracted into a shared helper (used by both `handleGetContext` and `handlePrepareContext`)

---

## Server Construction Changes

```go
// server.go — New()
s.mcp = server.NewMCPServer(serverName, serverVersion,
    server.WithToolCapabilities(true),
    server.WithResourceCapabilities(true, true),   // subscribe + listChanged
    server.WithHooks(hooks),
)
s.registerTools()
s.registerResources()   // NEW
```

---

## Token Impact

| Mechanism | Added | Saved | Net |
|-----------|-------|-------|-----|
| `synapses://active-context` resource | +300 always | -1500 (replaces session_init for orientation) | **-1200** |
| `prepare_context(modify)` single call | +3000 once | -8000 (replaces 4-5 tool calls + JSON) | **-5000** |
| `prepare_context(plan)` dry run | +2000 once | -4000 (prevents wasted refactor attempts) | **-2000** |
| `synapses://file/{path}` resource | +200 per file | -500 (replaces get_file_context) | **-300** |
| Async brain hydration | +0 (background) | -5000ms latency (cache hit vs cold LLM) | **-5s** |
| Choice map (ambiguity) | +200 (map section) | -2000 (saves disambiguation round-trip) | **-1800** |

---

## Files to Create/Modify

| File | Action | Contents |
|------|--------|----------|
| `synapses/internal/mcp/intents.go` | **NEW** | `handlePrepareContext` + 6 `assemble*Context` + `resolveTarget` + choice map (~500-600 lines) |
| `synapses/internal/mcp/resources.go` | **NEW** | `registerResources` + 3 resource handlers + `notifyResourceChanged` + `warmBrainCache` (~250-350 lines) |
| `synapses/internal/mcp/server.go` | **MODIFY** | Add `WithResourceCapabilities`, call `registerResources()` in `New()`, extend `InvalidatePacketCache` (~10 lines) |
| `synapses/internal/mcp/tools.go` | **MODIFY** | Add `prepare_context` registration in `registerTools()`, extract `buildBrainPacket` helper, update `suggestNextAfterContext` (~30 lines) |

### Key functions to reuse (not rewrite):
- `graph.CarveEgoGraph` → `internal/graph/traverse.go:62`
- `graph.ImpactAnalysis` → `internal/graph/traverse.go:312`
- `serializeCompact`, `writeNodeHeader`, `getRootSummary` → `internal/mcp/digest.go`
- `toDirectionalContext`, `normalizeSubgraph`, `pickBestNode` → `internal/mcp/tools.go`
- `matchRulesForFile`, `fileHasTests`, `suggestNextAfterContext` → `internal/mcp/tools.go`
- Brain packet building → `internal/mcp/tools.go:223-278` (extract to shared helper)

---

## Implementation Order

| Phase | What | Effort |
|-------|------|--------|
| **P0** | `intents.go` — `prepare_context` tool with 6 intents + choice map + target resolution | 2 days |
| **P1** | `resources.go` — 3 MCP Resources + delta notifications + async brain hydration | 1.5 days |
| **P2** | Wire into `server.go` + extract shared helpers + update suggestions | 0.5 day |

---

## Verification

### P0: prepare_context
- `go vet ./internal/mcp/...` passes
- `go build ./cmd/synapses/` links cleanly
- Manual test: `prepare_context(intent="modify", target="CarveEgoGraph")` returns compact briefing with blast radius, rules, annotations
- Manual test: `prepare_context(intent="plan", target="Graph")` returns change plan with file list and scope assessment
- Ambiguity test: `prepare_context(intent="understand", target="Store")` returns choice map + best-guess context
- Token comparison: `prepare_context(modify)` vs `get_context` + `get_impact` + `get_violations` chain

### P1: MCP Resources
- `synapses://active-context` appears in `resources/list` response
- `synapses://file/traverse.go` returns entity map
- File change triggers `notifications/resources/updated` (not full content push)
- Brain cache is warm for recently-changed file entities

### Integration
- Start synapses with `synapses start --path /path/to/repo`
- Resources show up in Claude Code's MCP resource list
- `prepare_context` appears in tool list
- End-to-end: agent uses `prepare_context(intent="plan", target="AuthService")` → sees scope → then `prepare_context(intent="modify", target="AuthService")` → edits with full context

---

## Post-Approval: Persist Plan

Save this plan as `synapses/HYBRID-CONTEXT.md` in the project root so it's version-controlled alongside the codebase.
