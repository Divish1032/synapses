---
id: graph-package-guide
description: Synapses internal/graph package architecture guide
entity_pattern: "(?i)(Graph|CarveEgoGraph|FlatGraph|BFS|Traverse|NodeID|EdgeType)"
module_pattern: "internal/graph*"
auto_load: false
---
The `internal/graph` package uses adjacency lists, not matrices. Key facts:
- `Node.ID` format: `{repo_id}::{relative_file}::{entity_name}` (e.g. `myrepo::pkg/auth/auth.go::Validate`)
- Edge types: CALLS, IMPORTS, DEFINES, IMPLEMENTS
- `CarveEgoGraph(rootID, cfg)` does BFS with exponential relevance decay + token-budget pruning
- `Graph.FindByName(name)` returns multiple candidates when ambiguous — always check length
- Never suggest matrix graph representations — this graph is sparse (thousands of nodes, millions possible)
