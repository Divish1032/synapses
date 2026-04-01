# GraphBench Results — Full Analysis Report
**Date:** 2026-03-31
**Repos tested:** 37
**Languages:** 20
**Total tests:** 323
**Skipped repos:** scala/scala3, ghc/ghc, ziglang/zig, JuliaLang/julia, PowerShell/PowerShell, erlang/otp, grafana/grafana, dart-lang/sdk
**Skip reason:** daemon indexing timeout (>5 min for mega-repos)

## Overall Summary
| Metric | Score |
|---|---|
| **F1** | **34.0%** |
| Precision | 31.1% |
| Recall | 48.6% |
| Tests with zero recall | 131/323 (41%) |

## Results by Query Type
| Query Type | Tests | P | R | F1 | Zero Recall |
|---|---|---|---|---|---|
| exact_entity_lookup | 10 | 78.7% | 90.0% | 82.9% | 1 |
| find_imports | 90 | 37.2% | 66.8% | 44.4% | 16 |
| file_entities | 8 | 40.7% | 48.8% | 34.5% | 1 |
| find_callers | 55 | 31.8% | 38.7% | 32.1% | 26 |
| find_callees | 89 | 28.9% | 40.8% | 30.8% | 38 |
| impact_analysis | 54 | 21.2% | 45.8% | 23.2% | 18 |
| find_implementations | 7 | 19.0% | 21.4% | 16.7% | 5 |
| coverage_probe | 1 | 0.0% | 0.0% | 0.0% | 1 |
| disambiguation | 1 | 0.0% | 0.0% | 0.0% | 1 |
| find_call_chain | 3 | 0.0% | 0.0% | 0.0% | 3 |
| find_cross_domain | 5 | 0.0% | 0.0% | 0.0% | 5 |

## Results by Language
| Language | Tests | P | R | F1 |
|---|---|---|---|---|
| go | 56 | 46.8% | 64.2% | 50.3% |
| python | 53 | 42.5% | 59.8% | 45.2% |
| java | 22 | 32.9% | 74.2% | 40.5% |
| rust | 33 | 33.1% | 56.4% | 36.9% |
| ruby | 30 | 33.5% | 53.3% | 36.1% |
| kotlin | 6 | 26.7% | 50.0% | 33.5% |
| elixir | 6 | 33.3% | 33.3% | 33.3% |
| groovy | 6 | 22.5% | 66.7% | 33.2% |
| typescript | 32 | 28.3% | 39.9% | 30.1% |
| swift | 12 | 30.5% | 33.3% | 29.8% |
| php | 7 | 16.4% | 47.6% | 18.3% |
| csharp | 6 | 19.0% | 25.0% | 15.3% |
| r | 6 | 10.2% | 27.8% | 14.3% |
| c | 12 | 14.2% | 18.8% | 13.9% |
| cpp | 6 | 8.3% | 16.7% | 11.2% |
| clojure | 6 | 7.3% | 27.8% | 10.8% |
| ocaml | 6 | 6.3% | 12.5% | 8.3% |
| lua | 6 | 0.3% | 5.5% | 0.5% |
| nim | 6 | 0.0% | 0.0% | 0.0% |
| perl | 6 | 0.0% | 0.0% | 0.0% |

## Gap Analysis — What Needs Fixing

### GAP 1: Call Chain Pathfinding (0% F1)
`get_context(mode=path, from=X, to=Y)` returns `found: false` for all tested pairs.
The daemon responds with `closest_reachable` but cannot traverse CALLS edges between entities.
**Root cause hypothesis:** The BFS pathfinder may only follow certain edge types, or cross-module calls are not represented as CALLS edges in the graph.
**Impact:** Users cannot trace call chains between functions.

### GAP 2: Cross-Domain Links (0% F1)
`get_context` returns empty cross_domain sections for all tested entities.
The `deploys`, `configured_by`, `documented_in`, `consumes` categories are always empty.
**Root cause hypothesis:** Cross-domain edge extraction may not be running, or repos lack file patterns the detector expects.
**Impact:** No config/deploy/doc awareness.

### GAP 3: Find Implementations (16.7% F1)
Interface/trait implementation discovery is weak. The `related` field returns some implementations but misses most.
**Affected languages:** Java (interfaces), Go (interfaces), Rust (traits).
**Root cause hypothesis:** IMPLEMENTS edges may not be reliably created by parsers.

### GAP 4: Impact Analysis Noise (23.2% F1, 21.2% precision)
`get_impact` returns too many irrelevant nodes. BFS at depth=1 follows all edge types.
**Fix direction:** Weight edges by type (CALLS > DEFINES for impact), or filter by edge type.

### GAP 5: Find Callers — Low Precision (31.8%)
Callers list includes noise — entities that are related but don't actually call the target.

### GAP 6: Find Callees — Misses Cross-Module Calls (28.9% P, 40.8% R)
Callees list misses calls to external libraries. Also returns noise from BFS traversal.
Example: `Flask.run` callees include `_AppCtxGlobals.get` (unrelated).

### GAP 7: Find Imports — Good Recall but Low Precision (37.2% P, 66.8% R)
Returns internal relative imports alongside external packages.
Example: `src/flask/app.py` expected 7 imports, got 30.

### GAP 8: Language Parser Coverage
| Language | F1 | Issue |
|---|---|---|
| Perl | 0.0% | No entities parsed |
| Nim | 0.0% | No entities parsed |
| Lua | 0.5% | Minimal entity extraction |
| OCaml | 8.3% | Very few entities found |
| Clojure | 10.8% | Poor function/macro detection |
| C++ | 11.2% | Template/class parsing weak |
| C | 13.9% | Struct/function detection incomplete |
| R | 14.3% | Function detection weak |

### GAP 9: Daemon Cannot Index Mega-Repos
8 repos skipped: scala/scala3, ghc/ghc, ziglang/zig, JuliaLang/julia, PowerShell/PowerShell, erlang/otp, grafana/grafana, dart-lang/sdk
Indexing exceeds 5+ minutes. Pipeline doesn't scale to 10k+ files.

## Strengths

1. **Entity Resolution (82.9% F1)** — `search(mode=exact)` correctly finds entities by name
2. **Go Language Support (50.3% F1)** — best overall, callers/callees/imports all work
3. **Python Support (45.2% F1)** — second best, import detection strong
4. **File Entity Extraction (34.5% F1)** — parser finds real entities per file

## Response Structure Reference

### get_context (format=json)
```
root: {id, type, name, package, file, line, exported, metadata}
callees: [{node: {name, type, file}, relevance}]
callers: [{node: {name, type, file}}]
related: [{node: {name, type, file}}]
imports: [{node: {name, type, file}}]
cross_domain: {deploys, consumes, configured_by, documented_in, mentions, manual, related}
other_candidates: [{name, type, file}]
disambig_hint: string
```

### get_impact
```
root: {name, type, file, line}
tiers: [{depth, label, confidence, nodes: [{name, type, file, line}]}]
affected_files: [string]
total_affected: int
```

### get_context(mode=path) — when path NOT found
```
found: false
from: {name, file, type}
closest_reachable: {name, file, hops, type}
reason: string
hint: string
```

## Priority Fix Order (by impact)

1. **Callers/Callees accuracy** (155 tests, ~31% F1) — highest test count, core feature
2. **Impact analysis precision** (54 tests, 23% F1) — too noisy, needs edge-type filtering
3. **Import precision** (90 tests, 37% P) — filter internal vs external imports
4. **Call chain pathfinding** (broken) — fix BFS to traverse CALLS edges
5. **Cross-domain links** (broken) — enable/fix cross-domain edge extraction
6. **Interface implementations** (7 tests, 17% F1) — fix IMPLEMENTS edge creation
7. **Language parser gaps** (Perl, Nim, Lua, OCaml, Clojure) — add/fix tree-sitter grammars
8. **Mega-repo indexing** — optimize for repos with 10k+ files

## Repos Tested
| Repo | Language | Index Time | Nodes | Edges |
|---|---|---|---|---|
| jqlang/jq | c | 4131ms | 9928 | 28026 |
| DaveGamble/cJSON | c | 23226ms | 72284 | 183716 |
| clojure/clojure | clojure | 11125ms | 160739 | 237064 |
| nlohmann/json | cpp | 12369ms | 13933 | 39398 |
| App-vNext/Polly | csharp | 20804ms | 20830 | 58473 |
| elixir-lang/elixir | elixir | 52879ms | 426139 | 1562175 |
| gin-gonic/gin | go | 25455ms | 27068 | 62915 |
| junegunn/fzf | go | 8843ms | 27814 | 63432 |
| spf13/cobra | go | 7591ms | 99195 | 247265 |
| go-chi/chi | go | 5905ms | 137142 | 509251 |
| traefik/traefik | go | 451263ms | 90747 | 219365 |
| kubernetes/minikube | go | 857531ms | ? | ? |
| apache/groovy | groovy | 266174ms | 73623 | 186269 |
| square/retrofit | java | 13609ms | 31834 | 76344 |
| FasterXML/jackson-databind | java | 59931ms | 120994 | 316378 |
| Kotlin/kotlinx.coroutines | kotlin | 38108ms | 45293 | 110629 |
| lua/lua | lua | 5077ms | 411678 | 1538742 |
| nim-lang/Nim | nim | 9877ms | 169096 | 241934 |
| ocaml/ocaml | ocaml | 88627ms | 153951 | 210098 |
| Perl/perl5 | perl | 312150ms | 409661 | 1533534 |
| laravel/framework | php | 201908ms | 38564 | 95773 |
| pallets/flask | python | 5083ms | 29109 | 64985 |
| psf/requests | python | 3806ms | 30329 | 66506 |
| django/django | python | 133666ms | 99677 | 249943 |
| tiangolo/fastapi | python | 71026ms | 137737 | 511223 |
| wch/r-source | r | 85926ms | 21089 | 50589 |
| rack/rack | ruby | 4982ms | 32627 | 79232 |
| sinatra/sinatra | ruby | 6462ms | 124216 | 324061 |
| hanami/hanami | ruby | 6666ms | 10030 | 28198 |
| seanmonstar/reqwest | rust | 4015ms | 32409 | 79816 |
| clap-rs/clap | rust | 18248ms | 123550 | 322061 |
| tokio-rs/axum | rust | 9499ms | 10653 | 31423 |
| apple/swift-nio | swift | 17664ms | 30626 | 75106 |
| nicklockwood/SwiftFormat | swift | 15584ms | 30685 | 68962 |
| expressjs/express | typescript | 6094ms | 26637 | 60716 |
| fastify/fastify | typescript | 8492ms | 101381 | 250303 |
| honojs/hono | typescript | 16120ms | 139727 | 513274 |