# Council Audit — Skipped Items

_Run: 2026-03-23_110155 — 15 items skipped out of 53_

These items were validated as real issues but require design work, platform-specific
tooling, or architectural changes beyond a safe minimal fix. Each entry links back
to the full description in `improvement_council.md`.

---

## Medium Effort — Requires Design

### #12 [MEDIUM] LocalClient CGo Inference Goroutine Leak on Context Cancellation
- **File:** `internal/brain/llm/local_cgo.go:116-165`
- **Values:** SPEED, ACCURACY
- **Why skipped:** Behind `llamacpp` build tag (CGo). The goroutine is bounded by MaxTokens=512 and the semaphore serializes callers. A proper fix requires either (a) a cancellation-aware CGo inference API, or (b) a goroutine pool with context-aware lifecycle that recreates the inference context when abandoned goroutines exceed a threshold.
- **Suggested approach:** Track abandoned goroutine count; when threshold (e.g., 3) is exceeded, drain the semaphore, destroy the inference context, and recreate it. This requires understanding gollama's thread-safety guarantees.

### #16 [MEDIUM] Resolver builds O(N) lookup tables on every call with no caching
- **File:** `internal/resolver/resolver.go:30-32`
- **Values:** SPEED
- **Why skipped:** `ResolveCallEdges` rebuilds three full-graph maps on every call. A proper fix requires a graph-invalidation-aware cache: the resolver needs to know when the graph has changed (new edges added, nodes removed) to invalidate its cached maps. This involves adding a generation counter to the Graph and checking it at the resolver layer.
- **Suggested approach:** Add `g.Generation() uint64` that increments on any mutation. Resolver caches maps with the generation number and rebuilds only when stale.

### #19 [MEDIUM] Graceful Shutdown Safety-Net Bypasses Deferred Cleanup
- **File:** `cmd/synapses/main.go:505-512`
- **Values:** ACCURACY, SPEED
- **Why skipped:** The 5-second `os.Exit(1)` safety-net skips deferred cleanup (federation resolver, watcher, MCP server, memory embedder). A proper fix requires staged shutdown: (1) signal all subsystems to stop, (2) wait up to N seconds per stage, (3) force-close remaining resources, (4) then os.Exit. This touches the entire daemon lifecycle.
- **Suggested approach:** Implement ordered shutdown stages with per-stage timeouts. Close resources in reverse-initialization order. Log which resources timed out.

### #21 [MEDIUM] Aggregator Rollup Not Crash-Safe for Per-Dimension Rollups
- **File:** `internal/pulse/aggregator/aggregator.go:344-412`
- **Values:** ACCURACY
- **Why skipped:** Per-dimension rollup methods internally acquire the store mutex, which would deadlock if wrapped in BeginBatch's held mutex. The existing idempotency guard (`rollup_completed` sentinel) already handles crash recovery — incomplete per-dimension rollups are re-run on restart.
- **Suggested approach:** Refactor per-dimension rollup methods to accept a `*sql.Tx` parameter instead of acquiring their own mutex. Then wrap the entire rollup (sentinel + per-dimension) in a single transaction.

### #24 [MEDIUM] SaveGraph Pre-Scan Unbounded Full-Table Scans
- **File:** `internal/store/store.go:1881-1906`
- **Values:** SPEED
- **Why skipped:** `SaveGraph()` snapshots all CALLS fan-in counts and signatures before the write transaction. Requires an incremental delta computation architecture — only compute for nodes/edges that actually changed.
- **Suggested approach:** Track a dirty set of modified node IDs during graph mutations. SaveGraph reads only dirty nodes' fan-in counts and signatures instead of the full table.

### #26 [MEDIUM] Model Download Blocks First Embed Request
- **File:** `internal/embed/builtin.go:386-461`
- **Values:** SPEED
- **Why skipped:** First `recall()` triggers a 137 MB model download synchronously. A `WarmUp()` method is needed, called at server startup in a background goroutine. This requires plumbing WarmUp into the daemon's initialization sequence and handling the case where recall is called before warmup completes.
- **Suggested approach:** Add `WarmUp(ctx)` to the Embedder interface. Call it from daemon start in a goroutine. Block `Embed()` on a sync.Once that either completes warmup or returns an error.

### #30 [MEDIUM] Federation Resolver getStore Race on Compatibility Check
- **File:** `internal/federation/resolver.go:408-483`
- **Values:** ACCURACY
- **Why skipped:** Compatibility check opens a separate DB connection outside any lock. Concurrent goroutines waste file descriptors. Requires moving the compat check under the write lock, or using per-alias `sync.Once` to ensure the check runs exactly once.
- **Suggested approach:** Add a `compatChecked sync.Once` per alias in the resolver's store cache entry. Run the compat check inside the Once, then store the result.

### #49 [LOW] Unbounded Goroutine Spawn in Watcher Brain Ingestion
- **File:** `internal/watcher/watcher.go:1200-1287`
- **Values:** SPEED
- **Why skipped:** Per-file goroutines with 15-second timers accumulate under burst file changes. Requires coalescing brain write-back by debouncing at the file level — a fundamental change to the watcher's brain ingestion pipeline.
- **Suggested approach:** Use a bounded worker pool (similar to the reparse pool) with a debounce map that coalesces rapid changes to the same file. Only dispatch brain ingestion after the debounce window closes.

### #50 [LOW] RecallEpisodes in CrossProjectSearch Ignores Context
- **File:** `internal/federation/cross_project_search.go:224,238`
- **Values:** SPEED
- **Why skipped:** `RecallEpisodes` takes no context parameter. A hung sibling blocks for the full busy timeout. Requires adding a context-aware variant to the Store API, which is a signature change affecting multiple callers.
- **Suggested approach:** Add `RecallEpisodesCtx(ctx, ...)` to Store. Migrate callers. Keep the old method as a wrapper calling `RecallEpisodesCtx(context.Background(), ...)`.

---

## Platform-Specific / External Dependency

### #20 [MEDIUM] Windows PID Recycling Detection Completely Disabled
- **File:** `cmd/synapses/daemon_windows.go:45-49`
- **Values:** ACCURACY
- **Why skipped:** `processStartTime()` returns 0 on Windows, disabling PID recycling detection. Implementation requires Windows syscall (`GetProcessTimes` via `windows.OpenProcess` + `windows.GetProcessTimes`). No Windows development environment available.
- **Suggested approach:** Use `golang.org/x/sys/windows` to call `GetProcessTimes`, extract `CreationTime`, convert FILETIME to Unix nanos.

### #44 [LOW] FP32 Model SHA-256 Hash Not Pinned
- **File:** `internal/embed/builtin.go:69-73`
- **Values:** ACCURACY
- **Why skipped:** GPU users permanently forced to quantized model because the fp32 hash field is empty. Requires running the model on a GPU machine, capturing the SHA-256 hash, and pinning it in the code.
- **Suggested approach:** Run `sha256sum` on the fp32 ONNX model file from a Metal/CUDA machine and populate the hash constant.

---

## Low Priority / Negligible Value

### #27 [MEDIUM] Ingestor Sends Source Code to LLM Without Remote-Endpoint Awareness
- **File:** `internal/brain/ingestor/ingestor.go:161-172`
- **Values:** PRIVACY
- **Why skipped:** Config-only change. The data flow is by design — the user configures `OllamaURL`. A warning for remote endpoints is a UX improvement, not a code bug.
- **Suggested approach:** Check if OllamaURL host is not localhost/127.0.0.1/::1. If remote, log a one-time WARN and add a `remote_endpoint_acknowledged` config flag.

### #41 [LOW] DriftDetector Bypasses Injected Clock
- **File:** `internal/federation/drift_detector.go:49,67`
- **Values:** ACCURACY
- **Why skipped:** Uses `time.Now()` instead of the Resolver's injectable Clock. Only affects test determinism, not production correctness.
- **Suggested approach:** Pass `Clock` interface from Resolver to DriftDetector constructor. Replace `time.Now()` calls.

### #52 [LOW] Hash Write Error Ignored / Type-Unsafe interface{} / DELETE No Audit Trail
- **File:** `internal/mcp/handlers_context.go:126`, `internal/mcp/server.go:116`, `cmd/synapses/daemon_serve.go:1276-1301`
- **Values:** ACCURACY
- **Why skipped:** (a) hash.Hash.Write never returns an error per Go's interface contract — the `_, _ =` is correct. (b) Type-unsafe interface{} storage is a code style issue. (c) DELETE audit trail is a feature request.
- **Suggested approach:** For (b), introduce narrow interfaces like `type PulseClient interface { ... }` instead of `interface{}`. For (c), emit lifecycle events before destructive operations.

### #53 [LOW] CarveEgoGraph BFS Allocation Per Call
- **File:** `internal/graph/traverse.go:121-124`
- **Values:** SPEED
- **Why skipped:** Allocates visited map and queue per BFS call. The MCP-layer cache already mitigates this — repeated calls for the same entity return cached results.
- **Suggested approach:** Use `sync.Pool` for the visited map and queue slice. Minimal expected benefit given cache hit rates.

---

## Summary

| Category | Count | Items |
|----------|-------|-------|
| Medium effort / design needed | 9 | #12, #16, #19, #21, #24, #26, #30, #49, #50 |
| Platform-specific | 2 | #20, #44 |
| Low priority / negligible | 4 | #27, #41, #52, #53 |
| **Total skipped** | **15** | |
| **Total completed** | **38** | |
