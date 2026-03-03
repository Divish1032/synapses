<!-- synapses:start -->
## Synapses — Code Navigation

This project is indexed by Synapses. **Use these MCP tools instead of raw file reads for code exploration:**

| Goal | Use this tool |
|---|---|
| Understand a function / struct / interface | `get_context(entity="Name")` |
| Find a symbol by name or substring | `find_entity(query="name")` |
| Search by concept ("auth", "rate limiting") | `search(query="...", mode="semantic")` |
| Trace a call path between two functions | `get_call_chain(from="A", to="B")` |
| Find what breaks if a symbol changes | `get_impact(symbol="Name")` |
| List all entities in a file | `get_file_context(file="path/to/file")` |
| Understand the project structure | `get_project_identity()` |

**Start every session with** `get_project_identity` to orient yourself before diving into code.

> Raw file tools (Read, Glob, Grep) are for **writing** code. Synapses tools are for **understanding** it —
> they return pre-ranked, token-efficient context instead of raw file bytes.
<!-- synapses:end -->
