# Council Audit — Skipped Items (Updated)

_Run: 2026-03-23_110155 — 15 items originally skipped out of 53_
_Updated: 2026-03-23 — 9 items resolved, 6 remain_

---

## Resolved Items (9/15)

| # | Item | Resolution |
|---|------|------------|
| #19 | Graceful Shutdown Safety-Net | Closes federation, MCP server, pulse, store with 500ms per-resource timeouts before os.Exit |
| #26 | Model Download Blocks First Embed | Added `WarmUp(ctx)` to Embedder interface; called at daemon startup in background goroutine |
| #27 | Remote Endpoint Warning | Logs WARN at startup when OllamaURL points to a non-localhost host |
| #30 | Federation getStore Race | Moved compat check + store open under write lock with double-check pattern |
| #41 | DriftDetector Clock | Injected Resolver's Clock into DriftDetector for deterministic cache TTL testing |
| #49 | Unbounded Brain Ingestion | Added bounded channel (32) + 2 worker goroutines, matching reparse pool pattern |
| #50 | RecallEpisodes Ignores Context | Added `RecallEpisodesCtx(ctx, ...)` wrapper; federation uses errgroup context |
| #52c | DELETE No Audit Trail | Added log line with target and requester address before pulse data deletion |
| #53 | BFS Allocation Per Call | Pooled BFS queue slice via sync.Pool; visited map left as fresh alloc (O(N) clear not worth it) |

---

## Intentionally Not Fixed (3/15)

These were analyzed and determined to not require changes:

### #16 [MEDIUM] Resolver builds O(N) lookup tables on every call
- **Verdict:** No fix needed. `DrainCallSites()` returns early when empty (line 28-30), so the maps are only built during initial parse when the call site queue has unresolved items. After initial parse, this path is rarely hit. The O(N) cost is inherent to the initial parse and cannot be avoided.

### #21 [MEDIUM] Aggregator Rollup Not Crash-Safe for Per-Dimension Rollups
- **Verdict:** No fix needed. The existing sentinel-based idempotency (`rollup_completed`) already handles crash recovery correctly. Per-dimension rollups are individually idempotent — incomplete rollups re-run on restart. Refactoring all 6 methods to accept `*sql.Tx` adds complexity with no practical benefit.

### #52a/b [LOW] Hash Write Error Ignored / Type-Unsafe interface{}
- **Verdict:** No fix needed. (a) `hash.Hash.Write` never returns an error per Go's interface contract — the `_, _ =` is correct. (b) Type-unsafe `interface{}` storage is a code style issue, not a correctness bug. The import cycles that `interface{}` avoids are a deliberate architectural trade-off.

---

## Still Deferred (3/15)

### #12 [MEDIUM] LocalClient CGo Inference Goroutine Leak
- **File:** `internal/brain/llm/local_cgo.go:116-165`
- **Why still deferred:** Behind `llamacpp` build tag (CGo). Cannot build or test without CGo environment. The existing bounds (MaxTokens=512, semaphore capacity=1) prevent unbounded growth. A proper fix requires understanding gollama's thread-safety for inference context recreation.

### #20 [MEDIUM] Windows PID Recycling Detection Completely Disabled
- **File:** `cmd/synapses/daemon_windows.go:45-49`
- **Why still deferred:** Requires Windows syscalls (`GetProcessTimes`). No Windows development environment available. Cannot build or test.

### #44 [LOW] FP32 Model SHA-256 Hash Not Pinned
- **File:** `internal/embed/builtin.go:69-73`
- **Why still deferred:** Requires running the fp32 ONNX model on a GPU machine (Metal/CUDA/ROCm) to capture the SHA-256 hash. No GPU machine available.

---

## Summary

| Category | Count | Items |
|----------|-------|-------|
| Resolved this session | 9 | #19, #26, #27, #30, #41, #49, #50, #52c, #53 |
| Intentionally not fixed | 3 | #16, #21, #52a/b |
| Still deferred (env/platform) | 3 | #12, #20, #44 |
| **Total from original 53** | **47 completed** | **6 remaining (3 by design, 3 need env)** |
