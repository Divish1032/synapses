<!-- synapses:start -->
## Synapses — Code Intelligence (MCP)

This project is indexed by **Synapses**, a graph-based code intelligence server.

### Session Start
Call **one tool** at the start of every session:
```
session_init()   ← replaces get_pending_tasks + get_project_identity + get_working_state
```
Returns: pending tasks, project identity, working state, recent agent events, and **scale_guidance** — a repo-size-aware recommendation on which tools to prefer.

### Tool Selection — follow scale_guidance from session_init

| Repo scale | When to use Synapses | When to use Read/Grep |
|---|---|---|
| micro (<100 nodes) | Structural analysis, multi-file understanding | Simple targeted edits to a known file |
| small (100–499) | Code exploration, cross-file analysis | Targeted single-file edits |
| medium (500–1999) | All code exploration — Glob/Grep surfaces too much noise | Writing to a specific file you already identified |
| large (2000+) | Always — direct scanning is too noisy at this scale | Writing to a specific file you already identified |

### Code Exploration

| When you want to... | Use this |
|---|---|
| Understand a function, struct, or interface | `get_context(entity="Name")` |
| Pin to a specific file (avoids wrong-entity picks) | `get_context(entity="Name", file="cmd/server/main.go")` |
| Boost nodes linked to current task | `get_context(entity="Name", task_id="...")` |
| Find a symbol by name or substring | `find_entity(query="name")` |
| Search by concept ("auth", "rate limiting") | `search(query="...", mode="semantic")` |
| List all entities in a file | `get_file_context(file="path/to/file")` |
| Trace how function A calls function B | `get_call_chain(from="A", to="B")` |
| Find what breaks if a symbol changes | `get_impact(symbol="Name")` |

### Before Writing Code

| When you want to... | Use this |
|---|---|
| Check proposed changes against architecture rules | `validate_plan(changes=[...])` |
| Reserve a file/package before editing | `claim_work(agent_id="...", scope="pkg/auth", scope_type="package")` |
| Check if another agent is editing the same code | `get_conflicts(agent_id="...")` |
| Release locks when done | `release_claims(agent_id="...")` |

### Task & Session Management

| When you want to... | Use this |
|---|---|
| Save a plan with tasks for future sessions | `create_plan(title="...", tasks=[...])` |
| Mark a task as done or add notes | `update_task(id="...", status="done", notes="...")` |
| Save progress so next session can resume | `save_session_state(task_id="...")` |
| Leave a note on a code entity for other agents | `annotate_node(node_id="...", note="...")` |
| See what other agents have been doing | `get_events(since_seq=N)` (use latest_event_seq from session_init) |

### Rules
- **Read/Grep** are for *writing* code (editing a specific file you have already found). For *understanding* code structure, always prefer Synapses tools.
- **Call `session_init()`** at the start of every session. It replaces the 3-call startup ritual.
- **Call `validate_plan()`** before implementing multi-file changes.
- **Call `claim_work()`** before editing to avoid conflicts with other agents.
- When `get_context` returns `other_candidates`, re-call with `file=` to pin to the right entity.
<!-- synapses:end -->
