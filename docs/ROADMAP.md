# SynapsesOS — Roadmap V3

*Intelligence Infrastructure for AI Agents. Last updated: 2026-04-01.*

---

## The New Thesis

AI agents can write code. They can't remember, they can't stay safe, and they can't learn. Synapses fixes that.

**Old positioning:** "The local-first knowledge graph that gives AI agents better code context."
**New positioning:** "Intelligence infrastructure for AI agents — persistent memory, structural security guardrails, and cross-session learning."

**Why the pivot:** State-of-the-art models (Claude, GPT, Gemini) don't need help finding or writing code. SWE-bench pilot: 20/20 both with and without Synapses tools. The graph-as-context-provider thesis hit a dead end for top models. But three massive gaps remain where no competitor operates: memory (11% FeatureBench vs 74% SWE-bench), safety (87% of AI PRs have security vulnerabilities), and learning (every session starts from zero).

**Communication principle:** All Synapses tool responses provide natural language intelligence — relationships, rules, history, risk. Never code snippets. Never file contents. The agent reads code itself. Synapses tells it what it doesn't know and what it's about to break.

**Language focus:** Go, TypeScript, Python, Java, Rust. These 5 cover ~80% of enterprise codebases. All 49+ existing parsers remain but are not the active development focus.

---

## Three Values — Never Compromise

1. **Speed** — No latency added without proportional value
2. **Privacy** — Code never leaves the machine without explicit config
3. **Accuracy** — Wrong intelligence is worse than no intelligence

---

## What's Already Shipped (Sprints 1-22)

260K lines of Go. 49+ language parsers. 32 SQLite tables. Full MCP server. Shipped between 2026-03-14 and 2026-03-31.

See `ROADMAP_V1_ARCHIVED.md` and `ROADMAP_V2_ARCHIVED.md` for full history.

**Foundation assets being reused:**
- AST graph engine (tree-sitter, typed edges, BFS traversal)
- Episodic memory system (remember/recall across sessions)
- Session continuity (save/restore state)
- Architectural rules engine (validate with violation detection)
- Cross-project federation (drift detection, impact analysis)
- Quad-channel retrieval (graph + BM25 + semantic + temporal)
- Embedded vector search (nomic-embed-text-v1.5 ONNX)
- Brain integration (Ollama, 6-tier task scheduler)
- Daemon architecture (per-machine, multi-project, MCP over stdio/HTTP)

**GraphBench current state:** 40.3% F1 across 80 tests, 6 languages. Go 50.4%, Python 54.9%, TypeScript 22.9%, Java 14.7%, Rust 38.3%.

---

## The Three Pillars

```
PILLAR 1: MEMORY           PILLAR 2: SAFETY           PILLAR 3: LEARNING
─────────────────          ─────────────────          ──────────────────
Compaction recovery        Pre-write constraints      Convention extraction
Hypothesis tracking        Mid-write validation       Failure avoidance
Decision journaling        Post-write audit           Pattern promotion
Convention memory          Drift monitoring           User preference learning
Proactive save nudges      Confidence levels          Architectural norm discovery
Cross-session recall       Framework auth patterns    Knowledge lifecycle
Spec coverage tracking     LSP precision enrichment   Episodic→semantic consolidation
```

---

## Design Principles — How Synapses Communicates

These principles govern EVERY tool response and feature implementation. They are not optional.

### The Three Roles

Synapses plays three roles, none of which write code:

**1. The Architect (Security & Rules)** — A senior architect reviewing your design.
- "That endpoint needs auth"
- "Don't call the database from the handler layer"
- "Your WebSocket handler doesn't follow the same auth pattern as your REST handlers"
- Names concepts and patterns, not code lines. Points to norms ("8 of 8 endpoints do X, yours doesn't").

**2. The Mentor (Memory & Context)** — A senior dev who's been on the project for years.
- "We tried that approach last quarter, it didn't work because X"
- "This project uses table-driven tests, not individual test functions"
- "You're 2 of 4 files into this feature, here's what's left"
- Shares knowledge, conventions, history. Lets the agent make its own coding decisions.

**3. The Radar (Impact & Risk)** — A staff engineer doing impact analysis.
- "If you change this function's signature, 12 callers across 4 packages break"
- "The last 3 AI-generated commits increased coupling between these packages by 23%"
- "This file has been modified by 3 different agent sessions this week — high conflict risk"
- Quantified risk, not code fixes.

### The Communication Protocol

What each tool provides vs what it never provides:

| Tool | Gives | Never Gives |
|------|-------|-------------|
| `get_context` | Entity names + file:line, relationship descriptions ("A calls B which calls C"), entity signatures, relevant memories, security constraints for the area | Raw source code, file contents, implementation suggestions |
| `validate` | Violations with reasoning, pattern comparisons ("8/8 have auth, yours doesn't"), severity, evidence paths | Fixed code snippets, "change line X to Y" |
| `get_impact` | Affected entity names, risk quantification, blast radius summary, critical path warnings | Source code of callers/callees |
| `memory` | Decisions, hypotheses, findings, rejected approaches, conventions | Code from previous sessions |
| `search` | Entity names, file locations, relationship types, relevance reasoning | File contents, matching source lines |
| `session_init` | Pending tasks, project conventions, unfinished work, drift alerts, recent decisions | Code to resume from |

**One exception:** Entity signatures (name + params + return type) are the ONE piece of "code" Synapses provides. Not implementations — just interfaces. This prevents FeatureBench's #1 failure mode (hallucinated interfaces).

### Why Natural Language, Not Code

1. **Compact** — 200 tokens of guidance vs 2000 tokens of code
2. **Additive** — information the agent can't get from reading files (relationships, history, rules)
3. **Non-redundant** — doesn't duplicate what grep/read already provide
4. **Never stale** — relationships and rules don't go stale the way cached code snippets do
5. **Non-competing** — two versions of the same code in context creates confusion; intelligence doesn't conflict

### The Three Memory Layers

Memory operates at three layers simultaneously:

**Layer 1: Pre-Compaction Shield (Silent)** — Synapses auto-captures the agent's reasoning state from tool calls. The agent doesn't know compaction is coming. After compaction, Synapses injects a recovery packet. The agent doesn't need to know it lost context — it receives a structured briefing.

**Layer 2: In-Session Augmentation (Active)** — The agent explicitly stores and retrieves from Synapses memory. Synapses nudges the agent to save when it detects accumulated un-persisted findings. Auto-extraction from tool calls supplements explicit saves.

**Layer 3: Cross-Session Intelligence (Strategic)** — Synapses observes behavior across sessions, extracts conventions, tracks failures, promotes knowledge through the lifecycle (observation → pattern → convention → rule). New sessions start with project intelligence, not zero.

### The Key Memory Principle

**Synapses doesn't dump memory at the agent. It answers questions the agent is about to ask.**

- Agent is about to edit `auth.go` → "Last session discovered session middleware at line 47 and planned PKCE changes"
- Agent just searched for `handleLogin` → "You explored this function 2 sessions ago and found it doesn't validate redirect URIs"
- Agent is writing tests → "This project uses table-driven tests with testify assertions (learned from 3 prior sessions)"

This is **intent-aware memory retrieval** — matching the `get_context` intent model (understand/modify/debug/add) but applied to memory.

### The Four Security Timing Points

| Timing | Trigger | Receiver | Purpose |
|--------|---------|----------|---------|
| **Pre-Write** | Agent calls `get_context` with intent `add`/`modify` | Agent (inline with context) | Inject security constraints BEFORE code is written |
| **Mid-Write** | Agent calls `validate scope=pre_write` | Agent (fix before writing) | Check proposed changes against rules, patterns, norms |
| **Post-Write** | Agent or CI calls `validate scope=post_write` | Agent + Developer (PR comment) | Full structural audit of completed changes |
| **Drift Monitor** | Cron / scheduled | Developer (dashboard/report) | Track architectural property trends over time |

### Severity Tiers

| Severity | Action | Example |
|----------|--------|---------|
| **CRITICAL** | Block — agent must fix before proceeding | Unauthenticated destructive endpoint, DB call bypassing auth layer |
| **HIGH** | Warn strongly — agent can override with justification | Missing rate limiting, no validation on non-destructive endpoint |
| **MEDIUM** | Inform — agent decides | Coupling increase, inconsistent error handling |

### Tool Adoption Strategy — Three Reliability Tiers

The biggest risk to Synapses' value is agents not calling tools at the right time. Research shows 97% of tool descriptions have quality issues, and agents with too many tools degrade in selection accuracy. The strategy:

**Tier 1: Server-Side Auto-Capture (~100% reliable)**
Synapses extracts intelligence from its own tool call data. No agent decision needed.
- Exploration log built from every `get_context`, `search`, `get_impact` call
- Convention observation from cross-session tool call patterns
- Session summaries auto-generated from tool call history
- *Limitation:* Only captures what flows through Synapses tools. If agent uses grep/read exclusively, Synapses is blind.

**Tier 2: Deterministic Triggers (~95% reliable)**
Hooks and file watchers fire without agent decision. Two mechanisms:

*Claude Code Hooks (Claude Code only):*
```
PostToolUse hook on Write/Edit:
  → synapses validate --scope=post_write --file=$FILE
  → Security findings injected into agent's next response

Stop hook:
  → synapses end-session --auto-summary
  → Session state persisted automatically
```

*File Watcher (universal, all editors):*
```
File changed on disk
  → Synapses daemon detects change (existing watcher)
  → Re-parses changed file + runs security patterns
  → Queues findings
  → Next time agent calls ANY Synapses tool, findings piggyback on response
```

**Tier 3: Agent-Initiated (~40-70% reliable)**
Agent decides to call tool. Reliability depends on tool description quality.
- Explicit hypothesis storage, decision journaling, manual memory saves
- These are additive, not critical. If agent doesn't call them, Tiers 1+2 still capture essential state.

**Design principle: Never rely on agent initiative for critical operations.** Conventions, security validation, compaction recovery, and session persistence must work through Tier 1 or Tier 2. Tier 3 is for features that enhance but aren't essential.

**Reliability by feature:**

| Feature | Tier | Mechanism | Confidence |
|---------|------|-----------|------------|
| Convention delivery | 1 | session_init (guaranteed touch point) | 95% |
| Failure avoidance warnings | 1 | session_init | 95% |
| Post-write security (Claude Code) | 2 | PostToolUse hook | 95% |
| Post-write security (other editors) | 2 | File watcher + piggyback | 70% |
| Session persistence | 2 | Stop hook (Claude Code) / end_session | 85% |
| Compaction recovery | 1+2 | session_init on re-connect | 90% |
| Pre-write security constraints | 3 | Piggyback on get_context | 60% |
| Exploration log auto-capture | 1 | Server-side from tool calls | 80%* |
| Decision journaling | 3 | Agent calls memory(store) | 40-50% |
| Hypothesis tracking | 3 | Agent calls memory(store) | 40-50% |

*\*80% assumes agent uses Synapses tools regularly. 0% if agent never calls them.*

### MCP Tool Surface — 8 Tools (Consolidated)

Research: agents degrade with >10 tools (decision friction, token waste). The "six-tool pattern" is ideal. Synapses consolidates from 13 tools to 8:

| # | Tool | New Vision Purpose | Absorbs |
|---|------|--------------------|---------|
| 1 | `session_init` | Guaranteed injection: conventions, warnings, unfinished work, security rules, recovery packet | — |
| 2 | `end_session` | Session persistence: auto-summary, exploration log, learning extraction | — |
| 3 | `get_context` | Relationships, entity signatures, security constraints, attached memories | Absorbs `get_file_context` |
| 4 | `search` | Entity discovery with relationship context and relevance reasoning | — |
| 5 | `get_impact` | Blast radius, risk quantification, critical path warnings | — |
| 6 | `validate` | All security: pre-write, mid-write, post-write, rules, norms, drift | Absorbs `rules` |
| 7 | `tasks` | Spec tracking, plan management, progress, completion verification | — |
| 8 | `memory` | Decisions, hypotheses, recall, annotations, rejected approaches | Absorbs `annotate` |

**Removed:**
- `get_compaction_guide` — replaced by automatic compaction recovery (Sprint 24.2)
- `get_file_context` — agent reads files directly; absorbed into `get_context`
- `lookup_docs` — agent browses docs itself; not natural language intelligence
- `rules` — merged into `validate` (rules are validate's configuration)
- `annotate` — merged into `memory` (annotations are a memory type)

8 tools. Every tool has a clear "when to call" story. No overlap. Under the 10-tool threshold where agent performance degrades.

### The Combined Vision

Memory and Safety reinforce each other:

```
MEMORY tells the agent:          SAFETY tells the agent:
"Here's what you know"           "Here's what you must not do"
"Here's what you tried"          "Here's what you're about to break"
"Here's what you forgot"         "Here's what you missed"
     ↓                                ↓
     └──────────── SYNAPSES ──────────┘
                     │
        "The agent's external brain
         with a built-in conscience"
```

Memory makes agents **smarter over time**. Safety makes agents **safer in real-time**. Together: an agent that doesn't repeat mistakes AND doesn't introduce vulnerabilities.

---

## Sprint Sequence

```
Phase 1: REFRAME ──── Sprints 23-25 (Tool Response Redesign + Memory Foundation)
Phase 2: SAFETY ───── Sprints 26-28 (Security Guardrails + Graph Precision)
Phase 3: LEARNING ─── Sprints 29-30 (Cross-Session Intelligence)
Phase 4: PROVE ────── Sprint 31 (Benchmark the Value)
Phase 5: EXPAND ───── Sprints 32-33 (Enterprise + Distribution)
```

---

## Phase 1: REFRAME — Tool Response Redesign + Memory Foundation

### Sprint 23: Natural Language Intelligence Responses

**Goal:** Strip code snippets from all tool responses. Replace with natural language intelligence — relationships, constraints, history, risk. This is the foundational communication change.

| # | Task | What | Effort |
|---|------|------|--------|
| ~~23.1~~ | ~~**Redesign `get_context` response format**~~ | ~~Remove raw source code from responses. Replace with: entity name + file:line location, relationship descriptions ("A calls B which calls C"), incoming/outgoing edge summaries, entity signatures (function/method signatures only — not implementations), relevant memories attached to entities, security constraints for the area. Output should read like a senior architect briefing, not a code dump.~~ | ~~High~~ |
| ~~23.2~~ | ~~**Redesign `search` response format**~~ | ~~Return entity names, file locations, relationship types, and relevance ranking. Not matching source lines. Add a `why` field explaining why each result is relevant (e.g., "high fan-in node", "recently modified", "has architectural rule").~~ | ~~Medium~~ |
| ~~23.3~~ | ~~**Redesign `get_impact` response format**~~ | ~~Return affected entity names with risk quantification: number of callers, packages affected, whether entity is in a critical path (payment, auth, data). Include blast radius summary in natural language: "Changing X affects 12 callers across 4 packages. 3 are in the payment path." No caller/callee source code.~~ | ~~Medium~~ |
| ~~23.4~~ | ~~**Redesign `session_init` response format**~~ | ~~Lead with: (1) unfinished work from prior sessions, (2) project conventions learned from history, (3) active drift alerts, (4) security rules summary, (5) recent decisions and rejected approaches. Not a list of graph stats. This is the agent's "morning briefing."~~ | ~~Medium~~ |
| ~~23.5~~ | ~~**Add `conventions` field to session_init**~~ | ~~Auto-populate from cross-session observations (Sprint 29 builds the learner; Sprint 23 builds the delivery slot). Initially populated manually from rules. Format: "This project uses table-driven tests", "All handlers use AuthMiddleware", "DB access goes through repository layer."~~ | ~~Low~~ |
| ~~23.6~~ | ~~**Add `security_constraints` section to get_context**~~ | ~~When intent is `add` or `modify`, include relevant architectural/security rules for the area being modified. Format: "All handlers in /api/* use AuthMiddleware (8/8 current)", "DB access must go through repo layer." Drawn from rules engine + observed graph patterns.~~ | ~~Medium~~ |
| ~~23.7~~ | ~~**Entity signature extraction**~~ | ~~For functions/methods/classes, extract and store the signature (name + params + return type) separately from the body. Signatures are the ONE piece of "code" Synapses provides — not implementations, just interfaces. Addresses FeatureBench D2 failure (hallucinated interfaces).~~ | ~~Medium~~ |
| ~~23.8~~ | ~~**Formatter-awareness in conventions**~~ | ~~Detect formatter configs in project root: `.prettierrc`, `.editorconfig`, `rustfmt.toml`, `pyproject.toml` (black/ruff), `.golangci.yml`, `biome.json`. Auto-populate convention: "This project auto-formats with [Prettier/gofmt/black] on save. File contents change after edits — re-read files after writing to avoid stale-content errors." Prevents edit-format drift that wastes ~5 agent turns per occurrence. One-time detection at index time.~~ | ~~Low~~ |
| ~~23.9~~ | ~~**Tool consolidation: 13 → 8 tools**~~ | Remove: `get_compaction_guide` (replaced by auto-recovery 24.2), `get_file_context` (agent reads files directly), `lookup_docs` (agent browses docs itself). Merge: `rules` into `validate` (rules become validate configuration), `annotate` into `memory` (annotations become a memory type). Update all tool descriptions to explain WHEN to call (not just WHAT it does). Research shows tool descriptions are the #1 factor in agent selection — descriptions must create urgency and explain consequences of NOT calling. | High |
| ~~23.10~~ | ~~**Tool description engineering**~~ | ~~Rewrite all 8 tool descriptions following proven patterns. Bad: "memory — Store and retrieve memories." Good: "memory — CALL THIS when you've explored 3+ files or made a decision. Saves findings that survive context compaction. Without this, you'll re-explore everything after compaction." Each description must answer: (1) when to call, (2) what happens if you don't, (3) what you get back. Max 150 words per description.~~ | ~~Medium~~ |

**Dependencies:** 23.9 (tool consolidation) should ship FIRST — it changes the tool surface that everything else redesigns. 23.7 (entity signatures) ships early — other response redesigns reference signatures. 23.1-23.4 are parallel after 23.9. 23.5-23.6 depend on 23.1 and 23.4. 23.8 is independent. 23.10 (description engineering) ships LAST — after all response formats are finalized, descriptions are tuned to match.

**Success criteria:** Zero raw source code in any tool response. Every response reads as natural language intelligence with entity pointers (name + file:line). Agent can still read files directly when it needs code.

---

### Sprint 24: Memory Foundation — Compaction Recovery + Decision Journaling

**Goal:** Build the external working memory that survives context compaction. This directly targets the 11% FeatureBench gap.

| # | Task | What | Effort |
|---|------|------|--------|
| ~~24.1~~ | ~~**Session exploration log (auto-capture)**~~ | Silently build an exploration log from every tool call during a session. Each `get_context`, `search`, `get_impact`, `validate` call generates a log entry: what was queried, what was found, key findings. Stored in session state. No agent action required. | High |
| ~~24.2~~ | ~~**Compaction recovery packet**~~ | ~~When `session_init` detects a resumed session (same project, recent prior session), inject a recovery packet: task progress, decisions made, files explored with key findings, hypotheses (active/rejected), current plan state. Built from exploration log + task state + memory entries. Format: structured natural language briefing.~~ | ~~High~~ |
| ~~24.3~~ | ~~**Compaction detection heuristic**~~ | ~~Multiple detection signals, any one triggers recovery: (1) **Re-init signal:** agent calls `session_init` again within the same calendar session — strongest signal, near-certain compaction. (2) **Re-exploration signal:** agent calls `get_context` or `search` for entities already explored in this session (tracked via exploration log from 24.1) — likely post-compaction re-exploration, inject recovery packet proactively. (3) **Token gap signal:** if Synapses tracks cumulative tool output tokens (24.8) and agent suddenly re-requests basic project info, infer compaction. Design as OR-logic: any signal triggers recovery. False positives are cheap (agent gets a briefing it didn't need); false negatives are expensive (agent re-explores everything).~~ | ~~Medium~~ |
| **[IN PROGRESS]** 24.4 | **Hypothesis tracking in memory** | New memory category: `hypothesis`. Agent stores: "I think the bug is in X because Y." Hypotheses have states: ACTIVE, CONFIRMED, REJECTED. When agent stores evidence against a hypothesis, prompt: "Your hypothesis about X was invalidated. Update or remove?" Hypotheses survive compaction via recovery packet. | Medium |
| 24.5 | **Decision journaling in memory** | New memory category: `decision`. Structured format: DECISION (what was chosen), ALTERNATIVES (what was considered), REASONING (why this choice), CONTEXT (when/where). Surfaces in future sessions: "This decision was already evaluated. X was chosen because Y. Z was rejected because W." | Medium |
| 24.6 | **Rejected approach memory** | When agent explicitly abandons an approach (task marked failed, or agent stores failure note), persist: what was tried, why it failed, what error/blocker was hit. Future sessions: if agent starts down same path, inject warning: "A previous session tried this approach and abandoned it because [reason]." | Medium |
| 24.7 | **Proactive memory save nudge** | Monitor tool call volume in session. When agent has made 10+ tool calls without saving to memory, include a nudge in next tool response: "You've explored 7 files and made 3 decisions this session without saving to memory. Consider persisting findings to protect against context loss." Suppressible. | Low |
| 24.8 | **Token budget awareness** | Track approximate token usage from Synapses tool responses across the session. When cumulative tool output approaches 60% of estimated context budget (configurable per model), trigger proactive save and switch nudge strategy from tool-call-count-based to token-budget-based. Warn: "Context budget ~60% consumed. Recommend saving working state now." | Medium |

**Dependencies:** 24.1 (exploration log) must ship first — 24.2 and 24.3 consume it. 24.4-24.6 are parallel (independent memory categories). 24.7 and 24.8 are independent.

**Success criteria:** After context compaction, agent can resume work without re-exploring files or re-making decisions. Recovery packet covers: task progress, decisions, hypotheses, explored files, and relevant findings.

---

### Sprint 25: Spec Coverage Tracking + Cross-Session Continuity

**Goal:** Solve the "completion illusion" (agents declare done at 30-40%) and make memory retrieval intent-aware.

| # | Task | What | Effort |
|---|------|------|--------|
| 25.1 | **Spec coverage tracking in tasks** | Extend task system to track specification items. When agent creates a plan with N items, each item can be individually marked. `validate` post-write checks: "You marked 'add OAuth' complete but only modified 2 of 4 files that reference auth." Prevents premature completion declarations. | Medium |
| 25.2 | **Multi-file change tracking** | When a task involves multiple files, track which files have been modified vs which were identified as needing changes. On task completion, warn if identified files remain unmodified: "Files handler.go and config.go were identified as needing changes but haven't been modified." | Medium |
| 25.3 | **Intent-aware memory retrieval** | Memory recall currently uses semantic similarity. Add intent awareness: when agent is about to modify `auth.go`, retrieve memories attached to auth.go's entities, decisions about auth patterns, and hypotheses about auth-related bugs. Match the `get_context` intent model (understand/modify/debug/add). | Medium |
| 25.4 | **Cross-session exploration dedup** | Track which files/entities were explored across sessions. When agent begins exploring something already extensively explored in a prior session, inject: "This was explored in detail in session N. Key findings: [summary]." Prevents redundant re-exploration (saves ~5 turns per occurrence). | Medium |
| 25.5 | **Session handoff protocol** | When `end_session` is called, auto-generate a session summary: what was accomplished, what remains, key decisions, open hypotheses. Store as a first-class memory object. Next session's `session_init` retrieves it as the primary context. | Medium |
| 25.6 | **Goal and convention reinforcement** | Every Nth tool response (configurable, default every 10), append a compact reminder: current task goal (1 line) + top 3 active conventions. Prevents mid-session drift where the original task and project rules get pushed to the "middle" of context and decay. Lightweight — adds ~50 tokens per injection. Addresses lost-in-the-middle degradation for instructions. | Low |
| 25.7 | **Cross-agent exploration sharing** | When Agent B calls `session_init` on the same project where Agent A recently worked (within configurable window, default 24h), include Agent A's exploration log, decisions, and hypotheses in the response. Uses existing message bus and session storage. Prevents redundant work across agents working on the same project sequentially or in parallel. | Medium |

**Dependencies:** 25.1 and 25.2 are parallel. 25.3 depends on existing memory system. 25.4 depends on 24.1 (exploration log). 25.5 is independent. 25.6 is independent. 25.7 depends on 24.1 (exploration log) and existing message bus.

**Success criteria:** Agent cannot declare a multi-file task complete without Synapses confirming all identified files were addressed. Session handoffs carry full reasoning context.

---

## Phase 2: SAFETY — Security Guardrails + Graph Precision

### Sprint 26: Framework-Aware Security Pattern Library

**Goal:** Build a library of framework-specific security patterns that catch the #1 vulnerability (missing authentication) with near-perfect accuracy using tree-sitter only. No LSP needed. Highest ROI security work.

| # | Task | What | Effort |
|---|------|------|--------|
| 26.1 | **Security pattern specification format** | Define a declarative format for security patterns. Each pattern specifies: framework (chi, gin, echo, express, fastapi, spring, actix-web), pattern type (auth_middleware, rate_limiting, input_validation, csrf_protection), detection method (AST pattern match), severity (CRITICAL/HIGH/MEDIUM). Stored in a patterns directory, loadable at startup. | Medium |
| 26.2 | **Go HTTP framework patterns** | Patterns for: chi (`r.Use(middleware)`), gin (`r.Use(middleware)`), echo (`e.Use(middleware)`), net/http (`http.Handle` with middleware wrapping). Detect: route registration without auth middleware, handler without rate limiting, direct DB import from handler package. | High |
| 26.3 | **TypeScript/JavaScript framework patterns** | Patterns for: Express (`app.use(auth)`, `router.use(auth)`), Fastify (`fastify.register(authPlugin)`), Next.js (middleware.ts patterns), Koa (`app.use(auth)`). Detect: route without auth, missing CSRF on POST, direct DB import from route handler. | High |
| 26.4 | **Python framework patterns** | Patterns for: FastAPI (`Depends(get_current_user)`), Django (`@login_required`, `@permission_required`), Flask (`@login_required`, `before_request`). Detect: endpoint without auth dependency, missing input validation (no Pydantic model), direct ORM from view. | High |
| 26.5 | **Java framework patterns** | Patterns for: Spring Boot (`@PreAuthorize`, `@Secured`, `SecurityFilterChain`), Jakarta EE (`@RolesAllowed`). Detect: controller without security annotation, direct repository call from controller (bypassing service layer). | High |
| 26.6 | **Rust framework patterns** | Patterns for: Actix-web (`.wrap(middleware)`), Axum (`.layer(middleware)`), Rocket (`#[guard]`). Detect: route without auth middleware, handler without extractors for auth state. | Medium |
| 26.7 | **Pattern matching engine** | Runtime engine that applies patterns against the parsed AST graph. For each file modified by the agent, check applicable framework patterns. Return violations with: pattern name, severity, evidence ("8/8 existing handlers have auth, yours doesn't"), and natural language explanation. | High |
| 26.8 | **Cross-transport auth consistency** | For each framework, detect all transport types used in the project (HTTP routes, WebSocket upgrade handlers, gRPC service registrations). Verify auth middleware is applied consistently across ALL transports — not just REST. Flag: "REST endpoints in /api/* use AuthMiddleware but WebSocket handler at ws.go:34 does not." This is the #1 structural gap agents introduce (DryRun study: universal across all agents). | Medium |
| 26.9 | **Hardcoded secrets pattern** | Detect: JWT secrets as string literals (not loaded from environment), API keys in source (matching common key formats), database connection strings with embedded credentials, fallback secret patterns (`secret := os.Getenv("X"); if secret == "" { secret = "hardcoded" }`). AST-aware: distinguish config assignment from test fixtures. | Medium |
| 26.10 | **Admin route elevation pattern** | Routes matching `/admin/*`, handler functions with `admin` in name, or controllers in `admin/` package should require elevated authorization beyond basic auth. Detect: admin routes using same auth level as regular routes, admin endpoints without role-based access control annotations/middleware. | Low |
| 26.11 | **Slopsquatting / unknown package detection** | Maintain a local cache of known packages per language: top 50K from npm, PyPI, crates.io, Maven Central, Go modules (~5MB total, refreshed weekly or on-demand). When Synapses parses imports, flag any package not in the cache: "WARNING: Package 'flask-corse' is not found in PyPI. Did you mean 'flask-cors'? AI agents hallucinate package names 20% of the time — verify this dependency exists." Leverage existing import graph — zero new parsing needed. | Medium |

**Dependencies:** 26.1 (spec format) and 26.7 (pattern matching engine) must ship first — they are the foundation. 26.2-26.6 (per-language patterns) are fully parallel after that. 26.8-26.11 are independent of 26.2-26.6 but require 26.7 (engine).

**Success criteria:** For supported frameworks, detect missing auth middleware with >90% precision and >85% recall. Zero false positives on projects that don't use the detected framework.

---

### Sprint 27: Validate Tool Security Integration

**Goal:** Wire the security pattern library into the `validate` tool at all four timing points: pre-write guidance, mid-write validation, post-write audit, and drift detection.

| # | Task | What | Effort |
|---|------|------|--------|
| 27.1 | **Pre-write security constraints in `get_context`** | When `get_context` intent is `add` or `modify`, run pattern detection on the target area. Include discovered security constraints in response: "This package uses chi router. All 8 handlers use AuthMiddleware. Rate limiting is applied via RateLimit middleware. Input validation uses ValidateInput()." | Medium |
| 27.2 | **Mid-write validation (`validate scope=pre_write`)** | New validate scope. Agent describes proposed changes in natural language. Synapses checks against: (1) security patterns for the framework, (2) architectural rules, (3) observed norms (e.g., "all endpoints in this package have auth"). Returns violations with severity and evidence BEFORE code is written. | High |
| 27.3 | **Post-write validation enhancement** | Enhance existing `validate scope=post_write` to include security pattern checks. After agent writes code, Synapses re-parses changed files (incremental), runs security patterns, and reports: (1) new violations introduced, (2) existing violations in modified area, (3) violations fixed by the change. | High |
| 27.4 | **Severity tiers in validate response** | Three tiers: CRITICAL (block — agent must fix before proceeding), HIGH (strong warning — agent can override with justification), MEDIUM (inform — agent decides). Severity is per-pattern configurable. Default: missing auth on destructive endpoint = CRITICAL, missing rate limiting = HIGH, coupling increase = MEDIUM. | Medium |
| 27.5 | **Norm-based violation detection** | Beyond explicit rules, detect violations of observed norms. If 8/8 handlers in a package use AuthMiddleware and the new handler doesn't, that's a norm violation even without an explicit rule. Compute norm confidence from ratio (8/8 = HIGH, 3/5 = MEDIUM, 2/4 = LOW). | Medium |
| 27.6 | **Layer violation detection** | Detect when code in one architectural layer directly accesses code in a non-adjacent layer. Requires package-to-layer mapping (configurable or auto-inferred from directory structure: `handler/` → presentation, `service/` → business, `repo/` → data). Flag: handler importing database package, controller calling repository directly. | High |
| 27.7 | **Data flow path checking (heuristic)** | For routes that handle user input, trace the call path from handler to database operations. Check for presence of validation/sanitization function in the path (name-based: functions matching `Validate*`, `Sanitize*`, `Clean*`, `Escape*`). Medium confidence — flag as "may lack validation" not "definitely insecure." | High |
| 27.8 | **File watcher security integration (universal)** | Extend existing file watcher: when a file changes on disk, re-parse it (already happens), run security patterns against changed file (NEW), queue findings. Next time agent calls ANY Synapses tool (get_context, search, memory — anything), piggyback queued security findings onto the response. This is the Tier 2 universal mechanism — works with any editor/agent, no hooks needed. If agent never calls Synapses again that session, findings persist and surface at next session_init. | High |
| 27.9 | **Claude Code hook templates** | Ship pre-built hook configurations for Claude Code users. PostToolUse hook on Write/Edit → triggers `synapses validate --scope=post_write`. Stop hook → triggers `synapses end-session --auto-summary`. Installable via `synapses hooks install`. This is the Tier 2 high-reliability mechanism for Claude Code — deterministic security validation on every file write, automatic session persistence on every session end. | Medium |
| 27.10 | **Finding queue and piggyback delivery** | Infrastructure for queued findings. Any component (file watcher, background analysis, drift detection) can queue findings. Any tool response can piggyback queued findings as a `pending_findings` section. Findings are delivered exactly once, then cleared from queue. Priority ordering: CRITICAL first. Max 3 findings per piggyback to avoid overwhelming the agent. | Medium |

**Dependencies:** 27.1 depends on Sprint 26 (pattern library). 27.2 (mid-write) and 27.3 (post-write) are parallel. 27.4 (severity tiers) is independent. 27.5 (norm detection) is independent. 27.6 (layer violations) and 27.7 (data flow) are parallel and independent of 27.1-27.3. 27.8 (file watcher) depends on 26.7 (pattern engine) + existing watcher. 27.9 (hooks) depends on 27.3 (post-write validation). 27.10 (finding queue) should ship before 27.8 — it's the infrastructure 27.8 delivers through.

**Success criteria:** `validate` catches missing auth middleware, layer violations, and missing input validation for all 5 target languages across supported frameworks. Pre-write constraints prevent agents from writing insecure code in the first place.

---

### Sprint 28: LSP Precision Enrichment + Confidence Levels

**Goal:** Add LSP as an on-demand precision amplifier for Go and TypeScript (best LSP ecosystems). Add confidence levels to all findings so agents and users know what to trust.

| # | Task | What | Effort |
|---|------|------|--------|
| 28.1 | **LSP integration architecture** | Design the LSP-as-enrichment pattern: tree-sitter builds the graph, LSP answers targeted questions about ambiguous edges. LSP starts lazily on first ambiguous query, answers 10-50 questions to resolve edges, then idles. NOT a graph replacement — a verification oracle. Define the interface: `ResolveEdge(from, to, callSite) → VerifiedEdge`. | Medium |
| 28.2 | **gopls integration** | Integrate with gopls for Go projects. On ambiguous call edges (e.g., `store.Close()` — which Close?), query gopls go-to-definition. Update graph edge with verified target. Cache results. Start gopls lazily, kill after 5 min idle. | High |
| 28.3 | **tsserver integration** | Integrate with tsserver for TypeScript projects. Same pattern as gopls: resolve ambiguous edges, verify type information, cache results. Handle tsconfig.json detection and project setup. | High |
| 28.4 | **Confidence levels on all findings** | Every security finding, impact analysis result, and graph query result gets a confidence level: HIGH (import-level or LSP-verified), MEDIUM (tree-sitter name-based, consistent pattern), LOW (heuristic, name-based call matching). Communicate confidence to agent: "CRITICAL [Confidence: HIGH — import-level check]" vs "MEDIUM [Confidence: MEDIUM — heuristic, not type-verified]." | Medium |
| 28.5 | **LSP-triggered re-verification** | When a security finding has MEDIUM confidence from tree-sitter, and LSP is available, automatically query LSP to upgrade or downgrade confidence. "Missing auth middleware" with HIGH confidence after LSP confirms the type vs MEDIUM confidence from name matching alone. | Medium |
| 28.6 | **Pyright integration (stretch)** | Same pattern for Python. Pyright provides excellent type resolution. Lower priority than Go/TS because Python's dynamic typing means LSP coverage is inherently lower. | High |
| 28.7 | **GraphBench with LSP enrichment** | Re-run GraphBench for Go and TypeScript with LSP enrichment enabled. Measure F1 improvement. Target: Go 50% → 70%+, TypeScript 23% → 50%+. This validates whether LSP enrichment is worth the operational complexity. | Medium |

**Dependencies:** 28.1 (architecture) must ship first. 28.2 (gopls) and 28.3 (tsserver) are parallel. 28.4 (confidence levels) is independent — can ship before LSP. 28.5 depends on 28.2/28.3 + 28.4. 28.6 (Pyright) depends on 28.1. 28.7 depends on 28.2/28.3.

**Success criteria:** GraphBench F1 for Go ≥ 65% and TypeScript ≥ 45% with LSP enrichment. All security findings carry confidence levels. LSP starts in <3s and adds <500ms per verification query.

---

## Phase 3: LEARNING — Cross-Session Intelligence

### Sprint 29: Convention Auto-Extraction + Failure Avoidance

**Goal:** Synapses observes agent behavior across sessions and extracts reusable knowledge. This is the episodic-to-semantic consolidation that research identifies as the critical unsolved problem.

| # | Task | What | Effort |
|---|------|------|--------|
| 29.1 | **Session observation pipeline** | At `end_session`, analyze the session's tool calls, memory writes, task outcomes, and file modifications. Extract: (1) patterns repeated across this and prior sessions, (2) approaches that failed, (3) conventions followed, (4) user corrections. Store as structured observations with confidence scores. | High |
| 29.2 | **Convention extraction engine** | From cross-session observations, identify conventions: patterns that appear consistently across 3+ sessions. Categories: testing patterns ("table-driven tests"), error handling ("wrapped errors with %w"), code organization ("handlers in handler/, services in service/"), naming conventions. Confidence increases with repetition, decreases if violated without user complaint. | High |
| 29.3 | **Convention delivery in session_init** | Extracted conventions populate the `conventions` field added in Sprint 23.5. Format: "This project uses table-driven tests with testify (observed in 14/14 test files, confirmed across 8 sessions)." New sessions start with project knowledge instead of zero. | Medium |
| 29.4 | **Failure avoidance system** | Track approaches that failed across sessions. When agent begins a similar approach, inject warning: "A previous session tried jwt-go v3 for this project and found it incompatible with existing middleware (session 14, 23, 31 — same error each time)." Match by: library name, function name, error pattern. | High |
| 29.5 | **Architectural norm discovery** | Analyze the graph for implicit architectural norms: "all /api/* handlers use AuthMiddleware (24/24)", "all DB access goes through repo/ package (18/18)", "no handler imports database/sql (0 violations)". Surface as recommended rules: "Discovered norm with 100% adherence. Promote to enforced rule?" | Medium |
| 29.6 | **User preference tracking** | Track user corrections and confirmations across sessions. "User prefers single bundled PRs for refactors" (confirmed 3 times), "User wants verbose commit messages" (corrected 2 times). Surface in session_init as preferences. | Medium |
| 29.7 | **Task-completion learning extraction** | At individual task completion (not just end_session), auto-extract learnings: what approach worked, what patterns were used, what was discovered about the codebase. Persist as episodic memories. Addresses the gap where tasks "complete" but learnings are lost (13.1% recall in MemoryAgentBench). | Medium |

**Dependencies:** 29.1 (observation pipeline) must ship first — 29.2-29.4 consume its output. 29.2 and 29.4 are parallel. 29.3 depends on 29.2 + Sprint 23.5 (conventions field). 29.5-29.7 are independent.

**Success criteria:** After 5+ sessions on a project, Synapses can populate session_init with project conventions, known failure patterns, and architectural norms — all learned automatically without human configuration.

---

### Sprint 30: Knowledge Lifecycle + Contradiction Resolution

**Goal:** Build the full knowledge lifecycle: observation → pattern → convention → rule. Handle contradictions and staleness.

| # | Task | What | Effort |
|---|------|------|--------|
| 30.1 | **Knowledge promotion pipeline** | Formalize the lifecycle: OBSERVATION (single session) → PATTERN (3+ sessions, confidence > 0.6) → CONVENTION (5+ sessions, confidence > 0.8) → RULE (promoted by user or auto when confidence > 0.95). Each stage has different enforcement: observations are silent, patterns are mentioned, conventions are recommended, rules are enforced. | High |
| 30.2 | **Contradiction detection in memory** | When agent stores a fact that contradicts existing memory, flag it: "Previous session recorded 'using PostgreSQL'. You're now recording 'using MySQL'. Which is current?" Resolution options: update (replace old), fork (both true in different contexts), reject (new info is wrong). | Medium |
| 30.3 | **Memory staleness detection** | Memories reference entities (file:line, function names). When referenced entities are renamed, deleted, or significantly modified, mark memory as potentially stale. On retrieval, flag: "This memory references OrderProcessor which was renamed to PaymentProcessor 3 sessions ago. Memory may be outdated." | Medium |
| 30.4 | **Episodic-to-semantic consolidation** | When memory corpus exceeds threshold per project, consolidate similar episodic memories into semantic summaries. "Fixed auth bug in OrderProcessor" + "Fixed auth bug in PaymentService" + "Fixed auth bug in NotificationHandler" → semantic: "Auth middleware has recurring bugs in service layer — the authentication check runs after request parsing, missing early-return errors." Run as background brain task. | High |
| 30.5 | **Cross-session conflict resolution** | When two sessions record contradictory decisions, resolve by recency + user confirmation. Present: "Session 14 decided to use gorilla/sessions. Session 18 decided to use scs/v2. Which is current?" Auto-resolve if the later session was confirmed by user. | Medium |
| 30.6 | **Information loss signaling** | When context compaction occurs or memories are consolidated, generate a "loss manifest": what was compressed, what details may be unavailable, what can be recovered by re-reading specific files. Agent knows what it doesn't know. | Medium |

**Dependencies:** 30.1 (promotion pipeline) depends on Sprint 29 (observations + conventions). 30.2-30.3 are parallel and independent. 30.4 depends on 30.1 + brain integration. 30.5 depends on 30.2. 30.6 depends on 24.2 (compaction recovery).

**Success criteria:** Knowledge promotion pipeline works end-to-end. Contradictions are detected and resolved. Stale memories are flagged. Agent has explicit awareness of what it knows vs doesn't know.

---

## Phase 4: PROVE — Benchmark the Value

### Sprint 31: FeatureBench + Security Benchmark

**Goal:** Prove measurable value on the two axes: memory improves long-horizon task completion, safety catches vulnerabilities that other tools miss.

| # | Task | What | Effort |
|---|------|------|--------|
| 31.1 | **FeatureBench evaluation harness** | Build agent harness that runs FeatureBench tasks (200 tasks, 24 repos) in two conditions: Claude alone vs Claude + Synapses (memory + conventions + spec tracking). Measure: Pass@1 delta, number of re-explorations avoided, hypothesis tracking benefit. | High |
| 31.2 | **FeatureBench pilot (20 tasks)** | Run 20 diverse FeatureBench tasks. Baseline: ~11% (Claude alone). Target: ≥ 20% with Synapses memory. If delta < 5pp, analyze why and iterate on memory protocol. | Medium |
| 31.3 | **Security benchmark: DryRun-style evaluation** | Build a security evaluation harness: give agent a feature task, let it generate code, run Synapses validate, compare against manual security audit. Metrics: (1) what % of real vulnerabilities does Synapses catch?, (2) what's the false positive rate?, (3) what does Synapses catch that SAST misses? | High |
| 31.4 | **Security benchmark: structural vs textual** | Compare Synapses validate against Semgrep/Snyk on the same AI-generated code. Categories: (A) both catch it (textual pattern), (B) only SAST catches it (Synapses gap), (C) only Synapses catches it (structural advantage), (D) neither catches it. Target: category C should be non-empty and meaningful. | Medium |
| 31.5 | **Token efficiency measurement** | Measure tokens consumed per task with and without Synapses. Hypothesis: Synapses reduces redundant exploration (convention memory, compaction recovery, dedup), saving tokens even after Synapses tool call overhead. | Medium |
| 31.6 | **Regression test suite** | Combine GraphBench + FeatureBench pilot + Security benchmark into a CI-runnable regression suite. Run on every PR to `release/*`. Fail on >2pp regression in any metric. | Medium |

**Dependencies:** 31.1 (harness) must ship first. 31.2 depends on 31.1. 31.3 and 31.4 are parallel with 31.1-31.2. 31.5 is independent. 31.6 depends on all others.

**Success criteria:** FeatureBench delta ≥ +5pp (stretch). Security benchmark shows Synapses catches structural vulnerabilities (category C) that SAST tools miss. Token efficiency is net-positive.

**Minimum viable delta analysis:** If FeatureBench delta < +3pp, do NOT conclude failure. Analyze per-failure-mode:
- **(A) Memory would have helped but didn't trigger** — compaction recovery or convention didn't inject at the right moment. Fixable: iterate on detection heuristics and injection timing.
- **(B) Memory triggered but agent ignored it** — Synapses provided the right intelligence but the agent didn't act on it. Fixable: prompt engineering, tool description tuning, or response format changes.
- **(C) Failure is not memory-related** — the task requires multi-file reasoning capability the model lacks regardless of memory. Out of scope: this is a model limitation, not a Synapses gap.
Category A is the most actionable. Category B requires experimentation with how intelligence is surfaced. Category C validates scope boundaries. Any positive delta in category A+B tasks proves the thesis even if the aggregate number is small.

---

## Phase 5: EXPAND — Enterprise + Distribution

### Sprint 32: Enterprise Readiness

**Goal:** Package Synapses for team and enterprise adoption.

| # | Task | What | Effort |
|---|------|------|--------|
| 32.1 | **AI agent audit trail** | Every AI-generated change gets a provenance record: which agent, what session, what tools were called, what context was provided, what rules were checked, what decisions were made. Queryable: "Show me all AI changes to auth/ in the last week with their reasoning." | High |
| 32.2 | **Continuous drift monitoring** | Periodic (configurable: daily/weekly) structural analysis of the codebase graph. Report: new coupling trends, layer violation trends, auth coverage trends, pattern compliance trends. Format: "Last 7 days: 47 AI commits, 3 new endpoints missing rate limiting (was 0/24), coupling between api/ and db/ up 23%." | High |
| 32.3 | **Team convention sharing** | Export/import learned conventions between team members. Developer A's machine learns conventions over 50 sessions → exports → Developer B imports → starts with full project knowledge on day 1. Conflict resolution: if two developers' conventions contradict, flag for team resolution. | Medium |
| 32.4 | **CI/CD integration for post-commit validation** | GitHub Action / GitLab CI component that runs Synapses validate on every AI-generated commit. Reports security findings as PR comments. Blocks merge for CRITICAL findings. | Medium |
| 32.5 | **Knowledge import/export** | `import_knowledge(format="json")` counterpart to existing export. Merge imported memories with existing (dedup by content hash, re-anchor to local entities, validate schema). Enables team knowledge sync. | Medium |
| 32.6 | **MCP Streamable HTTP transport** | Replace stdio with HTTP streaming for remote daemon access. Enables: multiple concurrent agents, load balancing, centralized team deployment. Build on existing REST API infrastructure. | Medium |

**Success criteria:** A team of 5 developers can share conventions, audit AI-generated changes, and enforce security standards via CI.

---

### Sprint 33: Distribution + Protocol Expansion

**Goal:** Make Synapses accessible beyond MCP-only agents.

| # | Task | What | Effort |
|---|------|------|--------|
| 33.1 | **VS Code extension** | Direct integration via REST API. Provides: memory sidebar (decisions, hypotheses, conventions), security findings inline, impact analysis on hover, architectural violation warnings. The 30M VS Code users can't use Synapses without this. | High |
| 33.2 | **A2A agent card** | Publish Synapses as a Google A2A agent. Enables discovery by A2A-compatible orchestrators. | Medium |
| 33.3 | **OpenAI-compatible context API** | `/v1/chat/completions`-compatible endpoint wrapping memory + validate as function calls. Enables any OpenAI-SDK agent to use Synapses without MCP. | Medium |
| 33.4 | **JetBrains plugin (stretch)** | IntelliJ/GoLand integration for Java/Go developers. Same capabilities as VS Code extension. | High |
| 33.5 | **Security dashboard in Tauri app** | Visual dashboard showing: structural drift over time, security findings by severity, convention compliance, agent activity audit trail. The control plane for enterprise teams. | High |

---

## Backlog — Build When Triggered

Items with clear value but no immediate schedule. Pull into a sprint when the trigger fires.

| Item | Trigger | Origin |
|------|---------|--------|
| Pyright LSP integration (Python) | After Go/TS LSP proves value | Sprint 28.6 |
| rust-analyzer LSP integration | After Go/TS LSP proves value | Sprint 28 |
| JDT LS integration (Java) | After Go/TS LSP proves value + enterprise demand | Sprint 28 |
| Full taint analysis engine | When heuristic data flow proves insufficient | Sprint 27.7 |
| Cross-language LSP integration (beyond top 5) | When enterprise demands specific languages | Old roadmap backlog |
| Datalog rule engine | When path-pattern rules prove insufficient | Old roadmap backlog |
| Cross-project memory transfer | When federation + convention sharing ship | Old Sprint 27.6 |
| SDLC auto-detection | When memory + safety are proven | Old Sprint 27.1 |
| Phase-aware CarveConfig | When SDLC detection ships | Old Sprint 27.2 |
| Doc-to-graph pipeline (PDF/DOCX) | When non-code domain demand materializes | Old Sprint 27.4 |
| SQL parser | When database-aware security checks are needed | Old Sprint 22.1 |
| Protobuf/gRPC parser | When API schema security checks are needed | Old Sprint 22.2 |
| CI/CD pipeline parser | When deployment security checks are needed | Old Sprint 22.4 |
| Dockerfile parser | When container security checks are needed | Old Sprint 22.3 |
| Integer quantization for embeddings | When HNSW memory exceeds 500MB | Old roadmap backlog |
| BitNet for brain inference | When 7B+ BitNet model with JSON extraction exists | Old V2 "things to think on" |
| Causal memory graph | When decision journaling proves high usage | Old Sprint 21.4 |
| Sequential pattern mining | When sufficient session data volume exists | Old Sprint 21.5 |
| Server struct decomposition | When adding new services becomes painful | Old Sprint 19.4 |
| Capability-based RBAC | When multi-tenant scenario materializes | Old roadmap backlog |
| Merkle audit trail | When enterprise compliance demands cryptographic proof | Old roadmap backlog |

---

## Problem Coverage Matrix

Every problem identified in the research phase mapped to its roadmap solution. Problems without coverage are acknowledged as out-of-scope or deferred.

### Memory Problems (A1-F3)

| Problem | Status | Task(s) |
|---------|--------|---------|
| A1: Compaction destroys relationships | COVERED | 24.1, 24.2, 24.3 |
| A2: Lost-in-the-middle degradation | COVERED | 25.6 (goal reinforcement) |
| A3: Effective context < claimed | COVERED | 24.8 (token budget awareness) |
| A4: Precision details vanish | COVERED | 24.1, 24.2 |
| A5: Cascading hallucination post-compaction | COVERED | 24.2, 24.3 |
| A6: Buffer reservation (platform constraint) | N/A | Platform constraint, not solvable |
| B1: Instruction decay ~45 min | COVERED | 25.6 (periodic reinforcement) |
| B2: Goal pushed to middle | COVERED | 25.6, 25.1 |
| B3: Completion illusion | COVERED | 25.1, 25.2 |
| B4: Soft collapse | COVERED | 25.6 |
| B5: Compound reliability | INDIRECT | System-level improvement from all memory tasks |
| C1: Re-exploration | COVERED | 25.4, 24.1 |
| C2: Convention re-learning | COVERED | 23.5, 29.2, 29.3 |
| C3: Edit-format drift | COVERED | 23.8 (formatter-awareness in conventions) |
| C4: Over-exploration | PARTIAL | 25.4, 23.1 (agent still decides) |
| D1: NameError (cross-file) | COVERED | 23.7, 28.2, 28.3 |
| D2: Hallucinated interfaces | COVERED | 23.7 (entity signatures) |
| D3: Multi-file failure rate | COVERED | 25.1, 25.2, 23.1 |
| D4: No spec tracking | COVERED | 25.1, 25.2 |
| E1: RAG similarity ≠ relevance | COVERED | 25.3 (intent-aware retrieval) |
| E2: No contradiction resolution | COVERED | 30.2, 30.5 |
| E3: No information loss signal | COVERED | 30.6 |
| E4: No episodic→semantic consolidation | COVERED | 30.4, 30.1 |
| E5: Learnings lost at task completion | COVERED | 29.7 (task-completion learning) |
| E6: Checkpoint = execution, not reasoning | COVERED | 24.1, 24.2, 24.5, 25.5 |
| F1: No hypothesis tracking | COVERED | 24.4, 24.6, 29.4 |
| F2: Multi-agent handoff failure | COVERED | 25.7 (cross-agent sharing) |
| F3: No rejected-alternative memory | COVERED | 24.5, 24.6 |

### Security Problems (G1-L4)

| Problem | Status | Task(s) |
|---------|--------|---------|
| G1: Unauthenticated endpoints | COVERED | 26.2-26.6, 27.1, 27.5 |
| G2: WebSocket auth gap | COVERED | 26.8 (cross-transport consistency) |
| G3: Rate limiting unmounted | COVERED | 26.2-26.6, 27.5 |
| G4: 2FA-disable bypass | PARTIAL | 27.2 (mid-write catches some); needs domain rules |
| G5: JWT hardcoded secrets | COVERED | 26.9 (hardcoded secrets pattern) |
| G6: User enumeration | OUT OF SCOPE | Business logic / DAST territory |
| H1: XSS | OUT OF SCOPE | Textual pattern — use Semgrep/Snyk |
| H2: Log injection | OUT OF SCOPE | Textual pattern — use Semgrep/Snyk |
| H3: SQL injection | OUT OF SCOPE | Textual pattern — use Semgrep/Snyk |
| H4: Client-side trust | COVERED | 27.6, 27.7 |
| I1: Layer violations | COVERED | 27.6 |
| I2: Missing sanitization in data flow | COVERED | 27.7 |
| I3: Protocol-specific auth gaps | COVERED | 26.8 |
| I4: Admin panels without auth | COVERED | 26.10 |
| J1: Slopsquatting | COVERED | 26.11 (unknown package detection) |
| J2: Documentation poisoning | OUT OF SCOPE | Supply chain attack vector |
| J3: Malicious packages | OUT OF SCOPE | Registry-level security |
| K1: Hardcoded credentials | COVERED | 26.9 |
| K2: Secrets in GitHub | OUT OF SCOPE | GitGuardian/gitleaks territory |
| K3: MCP default 0.0.0.0 | OUT OF SCOPE | Infrastructure config |
| K4: Dev config in production | PARTIAL | Could detect debug flags; borderline linting |
| L1: SAST misses relationships | COVERED | Sprint 26-27 (entire purpose) |
| L2: Self-review bias | INDIRECT | Synapses is a separate rule-based system |
| L3: Business logic flaws | OUT OF SCOPE | Requires domain-specific rules |
| L4: Semantic privilege escalation | PARTIAL | 32.1 (audit trail enables post-hoc detection) |

**Coverage summary:** 36 of 46 problems COVERED. 4 PARTIAL. 0 DEFERRED. 6 OUT OF SCOPE (textual patterns best served by SAST). Explicitly acknowledged scope boundaries prevent scope creep into SAST/DAST territory.

---

## What V3 Does NOT Include

- **New language parsers.** 49+ exist. Focus on depth for 5 languages, not breadth for 50+.
- **Code snippet delivery.** Synapses provides intelligence, not code. Models read code themselves.
- **Agent orchestration.** Claude Code Agent Teams, LangGraph, A2A handle coordination.
- **RAG-style text chunk retrieval.** Synapses provides structured intelligence, not text chunks.
- **Competing with SAST/DAST.** Synapses catches structural violations. Semgrep/Snyk catch textual patterns. Complementary, not competitive.
- **Cloud-hosted service.** Local-first is architectural, not optional.
- **Killing existing parsers.** All 49+ parsers remain. They're just not the active development focus.

---

## The Narrative Arc

```
Today:       "Synapses has a 260K LOC foundation with graph, memory, rules,
              and federation. But top models don't need help finding code.
              GraphBench: 40% F1. SWE-bench: 20/20 without Synapses."

After S25:   "Tool responses are pure intelligence — no code dumps. Memory
              survives compaction. Agents resume work without re-exploring.
              Decisions and hypotheses persist across sessions."

After S28:   "Security guardrails catch missing auth, layer violations, and
              data flow gaps across Go/TS/Python/Java/Rust. Pre-write
              constraints prevent insecure code. LSP enrichment pushes
              Go graph accuracy to 65%+. All findings have confidence levels."

After S30:   "Synapses auto-learns project conventions from agent behavior.
              Failed approaches are remembered. Knowledge promotes through
              observation → pattern → convention → rule lifecycle. Contradictions
              detected and resolved."

After S31:   "FeatureBench: +5pp over Claude alone. Security: catches structural
              vulnerabilities SAST misses. Token efficiency: net-positive.
              The value is measured and provable."

After S33:   "Teams share conventions and audit AI changes. CI blocks insecure
              commits. VS Code extension brings Synapses to 30M developers.
              Enterprise-ready."
```

---

## The Measuring Stick

Every sprint must move at least one of these numbers:

| Metric | Current | Target | Pillar |
|--------|---------|--------|--------|
| FeatureBench (Claude + Synapses) | ~11% (Claude alone baseline) | ≥ 20% | Memory |
| Security violations caught (structural) | 0 (not built yet) | ≥ 85% of structural violations | Safety |
| False positive rate (security) | N/A | < 10% | Safety |
| GraphBench F1 (Go) | 50.4% | ≥ 70% (with LSP) | Safety |
| GraphBench F1 (TypeScript) | 22.9% | ≥ 50% (with LSP) | Safety |
| GraphBench F1 (Java) | 14.7% | ≥ 40% | Safety |
| Redundant exploration (tokens wasted) | ~5 turns per recovery | ≤ 1 turn | Memory |
| Convention auto-discovery | 0 (not built) | ≥ 10 conventions per mature project | Learning |
| Cross-session decision recall | 0% (no journaling) | ≥ 90% of decisions recoverable | Memory |
| Compaction recovery completeness | 0% (no recovery) | ≥ 85% of working state recovered | Memory |

---

## What Doesn't Change

- **Local-first** — Graph, memory, rules engine on user's machine. No Synapses cloud.
- **Background silent worker** — Agents interact with Synapses. Users see the Tauri dashboard.
- **MCP as the primary interface** — Any agent that speaks MCP gets full intelligence.
- **Code graph as foundation** — The graph powers safety and impact analysis. It's the engine, not the product.
- **Speed** — No latency without proportional value.
- **Accuracy** — Wrong intelligence is worse than no intelligence.
- **Privacy** — Code never leaves the machine.
