# Document Graph Enrichment Plan

**Status:** Approved, not started
**Created:** 2026-03-27
**Goal:** Improve documentation parsing quality — extract richer signals from .md/.rst/.txt files, create more accurate doc↔code edges, and make embedding-derived edges safer.

**Core Principle:** No wrong data. Prefer silence (no edge) over noise (wrong edge). Every phase is independently deployable and rollbackable.

---

## Current State

### What exists today

1. **Markdown parser** (`internal/parser/markdown.go`):
   - Extracts ATX/setext headings as `NodeSection` nodes
   - Stores `title`, `depth`, `body` (max 2000 chars), `body_preview` (200 chars) in metadata
   - Creates `EdgeContains` (hierarchy) and `EdgeLinksTo` (markdown links)
   - **Fenced code blocks are actively skipped** (lines 215-231, `inFence` logic)

2. **Plaintext/RST parser** (`internal/parser/plaintext.go`):
   - RST heading detection via docutils underline spec
   - TXT heading heuristics (ALL-CAPS, colon-suffix, paragraph splitting)
   - No RST directive parsing (e.g., `.. code-block::`, `.. autofunction::`)

3. **Name-match doc↔code linking** (`internal/resolver/docedges.go`):
   - `ResolveDocEdges(g)` → scans section body text for backtick spans, HTML `<code>` tags, CamelCase tokens
   - `buildCodeNames(g)` → maps identifier names (≥4 chars) to code nodes
   - `linkSectionsToFiles(g)` → file path references in backtick/prose
   - No ambiguity filtering — a name matching 15 code nodes creates 15 edges
   - No test file filtering — test helpers get doc edges same as production code

4. **Embedding-based doc↔code linking** (`internal/resolver/nl_embed_doccode.go`):
   - `DiscoverDocCodeRelations(g, er, threshold)` — cascade: entity (0.60) → file (0.55) → module (0.50)
   - `buildSectionEmbedText(title, body)` — uses title + body[:300] chars
   - Max 3 edges per section per specificity level
   - No corroboration requirement — embedding-only edges created at threshold

5. **Edge struct** (`internal/graph/types.go`):
   ```go
   type Edge struct {
       From NodeID   `json:"from"`
       To   NodeID   `json:"to"`
       Type EdgeType `json:"type"`
   }
   ```
   - No confidence field. 3-field struct is load-bearing across 40+ files, SQLite schema, FlatGraph CSR index.
   - **Do NOT modify this struct.** Use node metadata for confidence (HANDLES pattern).

6. **HANDLES precedent** (`internal/graph/traverse.go` lines 273-278):
   - Route confidence stored in route **node's** metadata, not edge
   - BFS reads `routeNode.Metadata["confidence"]` and multiplies edge weight
   - This is the proven pattern for per-edge confidence without changing Edge struct

7. **Embedding pipeline**:
   - Model: nomic-embed-text-v1.5 (local ONNX, 384 dims, ~12s/embed on CPU)
   - `nodeText(name, sig, doc)` — flat concatenation, no structural context
   - Content hash: SHA256 of `name + sig + doc` for staleness detection
   - Background sweep with 50ms delay between embeds

---

## The Plan: 5 Phases

### Phase 1: Code Block Extraction from Docs

**Confidence: 97%**
**Files:** `internal/parser/markdown.go`, `internal/parser/plaintext.go`
**Tests:** `internal/parser/markdown_test.go`, `internal/parser/plaintext_test.go`

**What:** Extract fenced code blocks from markdown/RST and store as section node metadata instead of skipping them.

**Implementation:**

1. Define a struct for extracted code blocks:
   ```go
   type codeBlock struct {
       Language string `json:"language"`
       Content  string `json:"content"`
       Line     int    `json:"line"`
   }
   ```

2. In `markdown.go` `extractSections()`: when entering a fenced block, capture the language tag and content. When the fence closes, attach the codeBlock to the most recent section's slice.

3. After section extraction, serialize each section's code blocks as JSON and store in `metadata["code_blocks"]`.

4. In `plaintext.go`: parse RST `.. code-block:: python` directives similarly. Content is the indented block following the directive.

**Safety constraints:**
- Max 2000 chars per code block (truncate)
- Max 5 code blocks per section (ignore extras)
- Empty code blocks (whitespace only) are skipped
- Malformed fences (no closing) — block not captured, body includes it as prose (current behavior preserved)

**Validation:** Unit tests parsing markdown with code blocks verifying metadata. All existing tests pass unchanged.

**Rollback:** Remove code block capture logic. Section nodes revert to current metadata.

---

### Phase 2: Safety Filters for Doc↔Code Linking

**Confidence: 98%**
**Files:** `internal/resolver/docedges.go`, `internal/resolver/nl_embed_doccode.go`
**Tests:** `internal/resolver/docedges_test.go`, `internal/resolver/nl_embed_doccode_test.go`

**What:** Reduce false positive doc↔code edges via three sub-changes.

**2a. Ambiguity cap in `buildCodeNames`:**

After building the `codeNames` map, remove entries where the name maps to >3 code nodes:
```go
for name, targets := range codeNames {
    if len(targets) > 3 {
        delete(codeNames, name)
    }
}
```
Rationale: If "Handler" matches 15 different entities, linking a doc section to any of them is a coin flip. Better no edge than wrong edge.

**2b. Test file deprioritization in `buildCodeNames`:**

Skip entities defined in test files:
```go
if isTestFile(n.File) {
    continue
}

func isTestFile(path string) bool {
    return strings.HasSuffix(path, "_test.go") ||
        strings.HasSuffix(path, ".test.ts") || strings.HasSuffix(path, ".test.js") ||
        strings.HasSuffix(path, ".test.tsx") || strings.HasSuffix(path, ".test.jsx") ||
        strings.HasSuffix(path, "_test.py") || strings.HasSuffix(path, "_spec.rb") ||
        strings.Contains(path, "/test/") || strings.Contains(path, "/tests/") ||
        strings.Contains(path, "/__tests__/") || strings.Contains(path, "/testdata/")
}
```
Rationale: Documentation never explains test code. Doc sections should link to production entities.

**2c. Store linking provenance on section node metadata:**

In `linkSections` after creating an edge:
```go
sec.Metadata["doc_link_source"] = "name_match"
```

In `nl_embed_doccode.go` `linkMatches` after creating an edge:
```go
sec.Metadata["doc_link_source"] = "embedding"
sec.Metadata["doc_link_confidence"] = fmt.Sprintf("%.3f", m.Score)
```

Rationale: Auditability. Enables Phase 5 (confidence-weighted BFS). Follows HANDLES pattern of storing confidence on the source node.

**Validation:** Unit tests for ambiguity cap and test-file filtering. NLBench F1 should stay same or improve.

**Rollback:** Remove filters. System reverts to current behavior.

---

### Phase 3: Code Block Identifiers → EXPLAINS Edges

**Confidence: 93%**
**Files:** `internal/resolver/docedges.go`
**Tests:** `internal/resolver/docedges_test.go`

**What:** Parse code blocks (from Phase 1 metadata) to extract identifiers, create EXPLAINS edges to matching code nodes.

**Implementation:**

New function `linkCodeBlocks`:
```go
func linkCodeBlocks(g *graph.Graph, sections []*graph.Node, codeNames map[string][]*graph.Node) int
```

Called from `ResolveDocEdges` after `linkSections`.

**Identifier extraction (`extractCodeBlockIdentifiers`):**
1. Import statements: `import X` / `from X import Y` / `require('X')` / `use X` → extract imported names
2. Qualified calls: `X.method()` where X is CamelCase and ≥4 chars → extract X
3. Type annotations: `: TypeName` / `-> TypeName` → extract TypeName
4. Decorator/attribute usage: `@decorator` / `#[attribute]` → extract name

Language-aware regex sets keyed by the `language` field from Phase 1 metadata. Falls back to a generic identifier extractor if language unknown.

**Safety — corroboration requirement:**
Code block identifiers ONLY create edges if they **exactly match** a code node name in `codeNames` (which already has Phase 2 ambiguity cap and test-file filter applied). No fuzzy matching, no embedding similarity.

**Additional safety:**
- Mark provenance: `sec.Metadata["doc_link_source"] = "code_block"`
- Same dedup as `linkSections` — skip if EXPLAINS edge already exists for this (section, target) pair
- Max 5 new edges per section from code blocks (prevent noisy code examples from creating dozens of edges)

**Validation:** Unit tests with real-world code block examples. NLBench `doc_explains_code` and `find_doc_entities` should improve.

**Rollback:** Remove `linkCodeBlocks` call from `ResolveDocEdges`. Phase 1 metadata stays (harmless).

---

### Phase 4: Richer Embedding Text

**Confidence: 92%**
**Files:** `internal/resolver/nl_embed_doccode.go`, `internal/store/embeddings.go`, `internal/watcher/node_embed.go`
**Tests:** `internal/resolver/nl_embed_doccode_test.go`, `internal/store/embeddings_test.go`

**What:** Improve embedding input text for both doc sections and code nodes.

**4a. Section embedding enrichment:**

Update `buildSectionEmbedText` to accept code blocks metadata:
```go
func buildSectionEmbedText(title, body, codeBlocksJSON string) string
```

Append extracted import/call names from code blocks as structured suffix:
```
"Quick Start: Flask is a micro web framework... [code: Flask, render_template, url_for]"
```

Cap total at 500 chars.

**4b. Node embedding enrichment (parser-derived edges ONLY):**

Update `nodeText` in `embeddings.go` to include caller/callee names:
```go
func nodeTextEnriched(name, sig, doc string, callers, callees []string) string
```

Append: `"calls: X, Y"` and `"called by: A, B"` (max 5 callees, 3 callers).

**CRITICAL SAFETY RULE:** `callers` and `callees` MUST be populated ONLY from parser-derived edges (CALLS, IMPLEMENTS). **NEVER** from EXPLAINS, RELATES_TO, DOCUMENTED_BY, or any embedding-derived edge type. This breaks the circular amplification loop where bad embedding edges feed back into embedding input text.

**4c. Content hash update:**

Include callers/callees in the content hash so re-embedding triggers when the call graph changes:
```go
func nodeContentHashEnriched(name, sig, doc string, callers, callees []string) string
```

**Pre-deployment validation (REQUIRED):**
Before merging, run a micro-benchmark:
1. Select 50 doc sections with known correct code targets (from NLBench gold data)
2. Embed each section with current text and enriched text
3. Compare cosine similarity to correct code targets
4. If enriched cosine is higher for ≥60% of sections → ship
5. If not → do NOT ship 4a/4b, investigate why

**Rollback:** Revert `buildSectionEmbedText` and `nodeText`. Bump model version string to trigger full re-embedding.

---

### Phase 5: Confidence-Weighted BFS for Doc Edges

**Confidence: 95%**
**Files:** `internal/graph/traverse.go`
**Tests:** `internal/graph/traverse_test.go`

**What:** During BFS/PPR traversal, scale EXPLAINS/DOCUMENTED_BY edge weight by the confidence stored in section node metadata (set by Phase 2c).

**Implementation (~8 lines, follows HANDLES pattern exactly):**

In the BFS edge weight calculation, after the existing HANDLES block:
```go
if e.Type == EdgeExplains || e.Type == EdgeDocumentedBy {
    sourceID := e.From
    if e.Type == EdgeDocumentedBy {
        sourceID = e.To // confidence lives on the doc section node
    }
    if sourceNode := g.nodes[sourceID]; sourceNode != nil {
        if confStr := sourceNode.Metadata["doc_link_confidence"]; confStr != "" {
            if conf, err := strconv.ParseFloat(confStr, 64); err == nil && conf > 0 {
                w *= conf
            }
        }
        // Name-match edges: no doc_link_confidence → w unchanged (full weight 0.7/0.6)
    }
}
```

**Behavior:**
- Name-match EXPLAINS edges: no confidence metadata → weight = 0.7 (unchanged from today)
- Embedding EXPLAINS edges at cosine 0.65: weight = 0.7 × 0.65 = 0.455
- Embedding EXPLAINS edges at cosine 0.60: weight = 0.7 × 0.60 = 0.42 (still above RELATES_TO at 0.3)

**Validation:** Unit test: create edges with and without confidence metadata, verify BFS relevance scores differ correctly. Integration: NLBench precision should improve (wrong edges weighted down).

**Rollback:** Remove the 8 lines. BFS reverts to flat weight for all doc edges.

---

## Deferred (NOT in this plan)

| Item | Why deferred |
|------|-------------|
| **Add Confidence field to Edge struct** | Touches 40+ files, SQLite schema, FlatGraph CSR. Node metadata (HANDLES pattern) gives 90% of value at 5% of risk. |
| **Hierarchical embedding aggregation** | Cascade invalidation (when to recompute centroids) has no clean solution that guarantees fresh data. |
| **Hybrid query-time scoring** | 12s per embed on CPU makes it unusable as default. Needs faster local model or GPU first. |
| **Embedding model upgrade** | Architecture improvements (Phases 1-5) should come first. Model upgrade later compounds with them. |
| **Cross-doc structure (toctree, mkdocs.yml)** | Nice to have but lower value than within-file enrichment. |

---

## Key Files Reference

| File | Role |
|------|------|
| `internal/parser/markdown.go` | Markdown section extraction (Phase 1) |
| `internal/parser/plaintext.go` | RST/TXT section extraction (Phase 1) |
| `internal/parser/markdown_test.go` | Parser tests |
| `internal/resolver/docedges.go` | Name-match doc↔code linking (Phases 2, 3) |
| `internal/resolver/docedges_test.go` | Resolver tests |
| `internal/resolver/nl_embed_doccode.go` | Embedding-based doc↔code linking (Phase 2c, 4a) |
| `internal/store/embeddings.go` | Embedding storage, `nodeText` (Phase 4b) |
| `internal/watcher/node_embed.go` | Background embed sweep (Phase 4b caller) |
| `internal/graph/traverse.go` | BFS/PPR traversal (Phase 5) |
| `internal/graph/types.go` | Edge/Node type constants, DefaultEdgeWeights |

## Execution Notes

- Run all Go tests with `-p 1 -parallel 1` to avoid RAM exhaustion
- Each phase should be a separate commit with passing tests
- NLBench should be run after each phase to track F1 progression
- Phase 4 requires micro-benchmark validation BEFORE merge (see 4a/4b pre-deployment section)
