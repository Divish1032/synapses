// Package watcher implements incremental graph updates via filesystem events.
// When a source file changes, only that file's nodes and edges are removed and
// re-parsed — the rest of the graph is untouched. This keeps context fresh
// without the cost of a full re-index on every save.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/metrics"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/store"
)

const debounceDelay = 150 * time.Millisecond
const changeLogCap = 50
const reparseWorkChanSize = 100
const reparseBacklogThreshold = 500 // switch to full reindex above this many pending files
const brainWorkChanSize = 32        // bounded channel for brain ingestion work
const parseChanSize = 100           // buffered channel between parse workers and merge goroutine
const batchCoalesceThreshold = 3    // min pending results to trigger 50ms coalesce wait
const batchCoalesceDelay = 50 * time.Millisecond

// reparseWork is a unit of work for the bounded reparse worker pool.
type reparseWork struct {
	path string
	root string
}

// parseFileResult holds the output of the parallel (CPU-bound) parse phase.
// It is produced by prepareParseResult and consumed by applyBatch.
type parseFileResult struct {
	path      string
	src       []byte       // file bytes read once; reused for hash + parse
	hasErrors bool         // tree-sitter detected syntax errors in src
	hash      string       // SHA-256 content hash of src
	hashKnown bool         // false when hash computation failed (conservative)
	lstatIno  uint64       // inode at Lstat time; 0 if unavailable
	tempGraph *graph.Graph // nodes/edges/callSites/terraformRefs from parse
	err       error        // non-nil → skip this file in applyBatch
	reparseStart time.Time // timing for pulse ReparseEvent
}

// ChangeEvent records a single file modification processed by the watcher.
type ChangeEvent struct {
	Timestamp    time.Time `json:"at"`
	File         string    `json:"file"` // repo-relative when graph root is set
	NodesAdded   int       `json:"nodes_added"`
	NodesRemoved int       `json:"nodes_removed"`
	EdgesAdded   int       `json:"edges_added"`
}

// PacketCacheInvalidator is implemented by types that cache brain context packets
// and need to be notified when file changes make cached packets stale.
type PacketCacheInvalidator interface {
	InvalidatePacketCache()
	// InvalidatePacketCacheForFile is like InvalidatePacketCache but also triggers
	// MCP resource notifications and proactive brain cache warming for the given file.
	InvalidatePacketCacheForFile(changedFile string)
}

// ConfigChangeHandler is called when synapses.json changes on disk.
// The argument is the new parsed config. Implementations should reconnect
// any clients (scout, brain) whose settings may have changed.
type ConfigChangeHandler func(newCfg *config.Config)

// Watcher watches a directory tree and keeps a Graph current as files change.
// CrossProjectTracker detects cross-project dependencies in parsed files.
// Implemented by federation.DeterministicDetector.
type CrossProjectTracker interface {
	// DetectAndStore scans a file for cross-project imports, resolves entities
	// against sibling stores, and persists the results. Errors are logged and
	// skipped (fail-open). ctx controls cancellation and timeout.
	DetectAndStore(ctx context.Context, filePath string, localStore *store.Store)
}

// BrainCrossProjectTracker provides Tier 2 brain-enhanced cross-project
// dependency detection for languages that Tier 1 can't handle well.
// Runs asynchronously after Tier 1 detection. Implemented by a wrapper
// that calls federation.BrainDetector + DeterministicDetector.ResolveBrainDeps.
type BrainCrossProjectTracker interface {
	// DetectAndStoreBrain reads the file content, calls the brain LLM for
	// cross-project dependency detection, validates results against sibling
	// stores, and persists any new deps not already found by Tier 1.
	// Runs fire-and-forget — errors are logged, never propagated.
	DetectAndStoreBrain(ctx context.Context, filePath string, localStore *store.Store)
}

// NameMatcherRunner runs the cross-domain name-matching pass after each reindex.
// Implemented by namematcher.Matcher. Runs fire-and-forget in a tracked goroutine.
type NameMatcherRunner interface {
	// RunAsync scans g for cross-domain name matches and persists MENTIONS edges
	// to st. changedFiles is the list of files in the triggering batch; pass nil
	// to indicate a full re-walk (all domains affected — always run).
	// Must respect ctx cancellation. Never blocks — errors are logged.
	RunAsync(ctx context.Context, g *graph.Graph, st *store.Store, changedFiles []string)
}

type Watcher struct {
	fw          *fsnotify.Watcher
	graph       *graph.Graph
	walker      *parser.Walker
	store       *store.Store           // may be nil — cache update is best-effort
	cfg         *config.Config         // may be nil — violation checking is best-effort
	brainClient *brain.Client          // set via SetBrainClient; nil if brain not configured
	pktInval    PacketCacheInvalidator // set via SetPacketInvalidator; may be nil
	cfgHandler  ConfigChangeHandler    // called when synapses.json changes; may be nil
	configPath  string                 // absolute path to synapses.json (set by Start)
	projectID   string                 // stable project identifier (FNV hash of project root path)
	rootPath    string                 // absolute resolved project root (set by Start)
	cpTracker      CrossProjectTracker      // set via SetCrossProjectTracker; may be nil
	cpBrainTracker BrainCrossProjectTracker // set via SetBrainCrossProjectTracker; may be nil
	nmRunner       NameMatcherRunner        // set via SetNameMatcher; may be nil

	mu        sync.Mutex
	timers    map[string]*time.Timer // debounce timers keyed by absolute file path
	stopCh    chan struct{}
	stopCtx   context.Context    // cancelled when Stop() is called
	stopCtxFn context.CancelFunc // cancels stopCtx
	stopped   bool
	reparseMu sync.Mutex // serialises concurrent reparseFile goroutines (debounce timers)

	// workCh is a bounded channel that prevents thundering herd on large checkouts.
	// When the timer map exceeds reparseBacklogThreshold, all pending timers are
	// drained and a single full re-index is triggered instead.
	workCh chan reparseWork

	// parseCh connects the parse worker pool to the single merge goroutine.
	// Workers produce parseFileResult values (CPU-bound tree-sitter parse);
	// the merge goroutine consumes them and applies graph mutations + resolvers.
	parseCh chan parseFileResult

	// brainWorkCh is a bounded channel for brain ingestion work. Prevents
	// unbounded goroutine spawning during burst file changes (e.g. git checkout).
	// If the channel is full, new work is dropped — the file will be re-parsed
	// and re-ingested on the next change.
	brainWorkCh chan string

	wg sync.WaitGroup // tracks fire-and-forget goroutines so Stop() can drain them

	changeMu  sync.RWMutex
	changeLog []ChangeEvent // bounded log of recent file events (max changeLogCap)

	// fileHashMu guards fileHashes for concurrent reparseFile goroutines.
	// fileHashes stores the SHA-256 content hash of each file after its most
	// recent successful re-parse. Used by Sprint 10.7 embedding invalidation to
	// skip staling when file content did not actually change (no-op saves,
	// IDE auto-saves without edits). Keyed by absolute file path.
	fileHashMu sync.Mutex
	fileHashes map[string]string

	// fileHadParseErrors tracks files whose most recent watcher reparse was
	// skipped due to tree-sitter parse errors. On the FIRST error occurrence,
	// the reparse is skipped (likely a transient mid-save artifact). On the
	// SECOND consecutive error, the reparse proceeds (the errors are persistent
	// — real syntax errors or grammar gaps — and stale data is worse than
	// imperfect data). Cleared when a file parses cleanly or is deleted.
	// Protected by reparseMu (only accessed inside reparseFile).
	fileHadParseErrors map[string]bool

	// Sprint 14.3: Dependency-aware incremental reanalysis.
	//
	// filePkg maps each known source file to its own package name.
	// Used by computeInvalidationSet to include same-package files (which can
	// call each other without a package qualifier and may have unresolved call
	// sites that reference functions not yet present via CALLS edges).
	//
	// Protected by reparseMu (only accessed from reparseFile, which is
	// serialised). Populated at startup from graph nodes and updated after
	// each reparseFile.
	filePkg map[string]string

	// loopAlive tracks whether the event processing loop is running.
	// Set to 0 (dead) when the loop exhausts all restart attempts.
	loopAlive atomic.Int32

	// pulseClient, when non-nil, receives pipeline instrumentation events.
	// Set via SetPulseClient; may be nil (pulse disabled). (P2-3/P2-4)
	pulseClient *pulse.Client
	// loopPanics counts how many times the watcher event loop panicked. (P2-4)
	loopPanics atomic.Int64
}

// New creates a Watcher. store may be nil; if provided the cache is updated
// after each incremental re-parse.
func New(g *graph.Graph, w *parser.Walker, st *store.Store) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}
	stopCtx, stopCtxFn := context.WithCancel(context.Background())
	watcher := &Watcher{
		fw:                 fw,
		graph:              g,
		walker:             w,
		store:              st,
		timers:             make(map[string]*time.Timer),
		stopCh:             make(chan struct{}),
		stopCtx:            stopCtx,
		stopCtxFn:          stopCtxFn,
		fileHashes:         make(map[string]string),
		fileHadParseErrors: make(map[string]bool),
		workCh:             make(chan reparseWork, reparseWorkChanSize),
		parseCh:            make(chan parseFileResult, parseChanSize),
		brainWorkCh:        make(chan string, brainWorkChanSize),
		filePkg:            make(map[string]string),
	}

	// Sprint 14.3: seed import graph from persisted import_edges table and
	// from the in-memory graph. The graph is always the authoritative source;
	// the store fills in any edges not yet materialised in memory.
	watcher.initImportGraph()

	// Start bounded parse workers (CPU-bound tree-sitter phase).
	// Each worker reads a file, checks for parse errors, computes a content
	// hash, and parses into a temporary graph — all without holding reparseMu.
	// Results are sent to parseCh for the single merge goroutine to apply.
	for i := 0; i < 4; i++ {
		watcher.wg.Add(1)
		go func() {
			defer watcher.wg.Done()
			for {
				select {
				case work, ok := <-watcher.workCh:
					if !ok {
						return
					}
					// Wrap in a closure with panic recovery so a crashing
					// parser (e.g. tree-sitter nil-deref on a malformed file)
					// cannot kill the worker goroutine permanently. The result
					// carries the error, which applyBatch skips gracefully.
					result := func() (r parseFileResult) {
						defer func() {
							if rec := recover(); rec != nil {
								logutil.Error("synapses/watcher: parse worker panic for %s: %v\n", work.path, rec)
								r = parseFileResult{path: work.path, err: fmt.Errorf("parser panic: %v", rec)}
							}
						}()
						return watcher.prepareParseResult(work.path)
					}()
					select {
					case watcher.parseCh <- result:
					case <-watcher.stopCh:
						return
					}
				case <-watcher.stopCh:
					// Drain remaining work items: prepare results and
					// forward them so the merge goroutine can flush.
					for {
						select {
						case work, ok := <-watcher.workCh:
							if !ok {
								return
							}
							result := func() (r parseFileResult) {
								defer func() {
									if rec := recover(); rec != nil {
										logutil.Error("synapses/watcher: parse worker panic for %s: %v\n", work.path, rec)
										r = parseFileResult{path: work.path, err: fmt.Errorf("parser panic: %v", rec)}
									}
								}()
								return watcher.prepareParseResult(work.path)
							}()
							select {
							case watcher.parseCh <- result:
							default:
								// parseCh full during shutdown — drop.
							}
						default:
							return
						}
					}
				}
			}
		}()
	}

	// Start single merge goroutine: consumes parsed results from parseCh,
	// coalesces bursts (branch switches), then applies graph mutations and
	// runs resolvers once per batch.
	watcher.wg.Add(1)
	go watcher.mergeLoop()

	// Start bounded brain ingestion workers (2 goroutines).
	// Prevents unbounded goroutine spawning during burst file changes.
	for i := 0; i < 2; i++ {
		watcher.wg.Add(1)
		go func() {
			defer watcher.wg.Done()
			for {
				select {
				case path, ok := <-watcher.brainWorkCh:
					if !ok {
						return
					}
					watcher.ingestToBrain(path)
				case <-watcher.stopCh:
					return
				}
			}
		}()
	}

	return watcher, nil
}

// SetConfig wires the project config into the watcher so that rule violations
// are checked after each incremental re-parse and emitted to the event log.
// Must be called before Start. cfg may be nil to disable violation checking.
func (w *Watcher) SetConfig(cfg *config.Config) {
	w.cfg = cfg
}

// SetBrainClient wires a *brain.Client into the watcher so that changed files
// are incrementally ingested to the intelligence sidecar.
func (w *Watcher) SetBrainClient(bc *brain.Client) {
	w.brainClient = bc
}

// SetProjectID sets the stable project identifier used when ingesting nodes to brain.
func (w *Watcher) SetProjectID(id string) {
	w.projectID = id
}

// SetPacketInvalidator wires a PacketCacheInvalidator (typically the MCP Server)
// into the watcher. On every file change the packet cache is cleared so that
// stale brain context packets are not returned to agents.
func (w *Watcher) SetPacketInvalidator(pi PacketCacheInvalidator) {
	w.pktInval = pi
}

// SetCrossProjectTracker wires a federation dependency tracker into the watcher.
// When set, every file re-parse triggers cross-project import detection and
// stores discovered dependencies for drift checking by session_init.
// Must be called before Start. tracker may be nil to disable.
func (w *Watcher) SetCrossProjectTracker(tracker CrossProjectTracker) {
	w.cpTracker = tracker
}

// SetBrainCrossProjectTracker wires a Tier 2 brain-enhanced dependency tracker.
// When set, after Tier 1 detection, the watcher runs brain detection in a
// fire-and-forget goroutine for files whose language isn't well-covered by Tier 1.
// Must be called before Start. tracker may be nil to disable.
func (w *Watcher) SetBrainCrossProjectTracker(tracker BrainCrossProjectTracker) {
	w.cpBrainTracker = tracker
}

// SetNameMatcher wires the cross-domain name-matching pass into the watcher.
// When nm is non-nil, an initial unconditional pass is scheduled immediately
// so that MENTIONS edges are created from the already-loaded graph on first run
// and after daemon restarts (hasCrossDomain would otherwise stay false until a
// cross-domain file changes, silently skipping all code-only file saves).
func (w *Watcher) SetNameMatcher(nm NameMatcherRunner) {
	w.mu.Lock()
	w.nmRunner = nm
	w.mu.Unlock()
	if nm != nil {
		// nil changedFiles = always run, regardless of hasCrossDomain state.
		w.triggerNameMatcher(nil)
	}
}

// triggerNameMatcher fires the name-matching pass in a tracked goroutine.
// changedFiles is forwarded to RunAsync for domain-relevance filtering.
// Pass nil to indicate a full re-walk (all domains affected — always run).
// Fire-and-forget: errors are handled inside RunAsync.
func (w *Watcher) triggerNameMatcher(changedFiles []string) {
	w.mu.Lock()
	nm := w.nmRunner
	w.mu.Unlock()
	if nm == nil {
		return
	}
	w.trackGo(func() {
		nm.RunAsync(w.stopCtx, w.graph, w.store, changedFiles)
	})
}

// initFilePkg seeds filePkg from the graph's current nodes.
// Called once during New() — no locking needed (no goroutines active yet).
func (w *Watcher) initImportGraph() {
	// O(N) one-time scan. Skip NodePackage nodes: they represent imported
	// packages and have File=importer_file, so their Package field would
	// corrupt filePkg with the imported package name rather than the file's
	// own package.
	for _, n := range w.graph.AllNodes() {
		if n.Type == graph.NodePackage {
			continue
		}
		if n.Package != "" && n.File != "" {
			w.filePkg[n.File] = n.Package
		}
	}
}

// updateFilePkg updates filePkg for filePath after it has been re-parsed.
// Must be called under reparseMu.
func (w *Watcher) updateImportGraphForFile(filePath string) {
	// Re-read the package name from the fresh nodes. Skip NodePackage nodes.
	for _, n := range w.graph.NodesForFile(filePath) {
		if n.Type == graph.NodePackage {
			continue
		}
		if n.Package != "" {
			w.filePkg[filePath] = n.Package
			return
		}
	}
}

// computeInvalidationSet returns the set of files whose stored call sites need
// to be reloaded when filePath changes.
//
// Three complementary layers ensure complete coverage:
//
//  1. Resolved CALLS/IMPLEMENTS edges — inspect live graph edges pointing INTO
//     filePath (must be called BEFORE RemoveFile). Any file with such an edge
//     is a confirmed caller; its stored call sites reference nodes about to be
//     removed and re-created. Language-agnostic; exact.
//
//  2. Same-package files — always included because they share the package
//     namespace and can call each other without a qualifier; unresolved
//     same-package call sites may not yet have CALLS edges.
//
//  3. Unresolved cross-package callers (DB query) — when filePath exports a
//     new function, other files that import its package have call sites stored
//     in call_sites with pkg_alias matching this package but no CALLS edge yet.
//     LoadCallerFilesForPkgAliases finds them by pkg_alias so they get
//     re-resolved on the next pass. This closes the "new public function" gap.
//
// Must be called BEFORE RemoveFile so the incoming edges are still present.
// Must be called under reparseMu (reparseFile is already serialised).
func (w *Watcher) computeInvalidationSet(filePath string) []string {
	invalid := map[string]bool{filePath: true}

	// All files that have a resolved CALLS or IMPLEMENTS edge into this file.
	for _, e := range w.graph.InEdgesForFile(filePath) {
		if e.Type != graph.EdgeCalls && e.Type != graph.EdgeImplements {
			continue
		}
		if from := w.graph.GetNode(e.From); from != nil && from.File != "" {
			invalid[from.File] = true
		}
	}

	// 1-hop transitive callers: for each direct caller, also include ITS callers.
	// This catches chains like A→B→C where C changes — we invalidate both B and A.
	// Full BFS is too expensive; 1-hop covers 95%+ of real dependency chains.
	directCallers := make([]string, 0, len(invalid))
	for f := range invalid {
		if f != filePath {
			directCallers = append(directCallers, f)
		}
	}
	for _, caller := range directCallers {
		for _, e := range w.graph.InEdgesForFile(caller) {
			if e.Type != graph.EdgeCalls && e.Type != graph.EdgeImplements {
				continue
			}
			if from := w.graph.GetNode(e.From); from != nil && from.File != "" {
				invalid[from.File] = true
			}
		}
	}

	// Same-package files: may have unresolved call sites that will succeed
	// once the re-parsed functions are in the graph.
	pkg := w.filePkg[filePath]
	if pkg != "" {
		for f, fpkg := range w.filePkg {
			if fpkg == pkg {
				invalid[f] = true
			}
		}
	}

	// Unresolved cross-package callers (the "new public function" gap).
	//
	// When filePath adds a new exported function, other packages may already
	// have call sites in the DB with pkg_alias matching this file's package name
	// or filename stem — but no CALLS edge exists yet because the function was
	// not there during their last parse. Query call_sites by pkg_alias to find
	// these latent callers so they get re-resolved on the next analysis pass.
	//
	// Two aliases are tried: the package declaration name (e.g. "models") and
	// the filename stem (e.g. "user" from "user.go"), because some languages
	// use the filename as the import alias.
	if w.store != nil && pkg != "" {
		base := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))
		aliases := []string{pkg}
		if base != pkg {
			aliases = append(aliases, base)
		}
		if callers, err := w.store.LoadCallerFilesForPkgAliases(aliases); err == nil {
			for _, f := range callers {
				invalid[f] = true
			}
		}
		// Errors are silently ignored: the invalidation set is best-effort.
		// A missed latent caller is corrected on its next manual save; it
		// never causes data corruption, only a delayed re-resolution.
	}

	result := make([]string, 0, len(invalid))
	for f := range invalid {
		result = append(result, f)
	}
	return result
}

// SetPulseClient wires a pulse.Client into the watcher for pipeline instrumentation.
// When set, reparseFile emits ReparseEvents and the event loop emits health events.
// Must be called before Start. pc may be nil to disable. (P2-3/P2-4)
func (w *Watcher) SetPulseClient(pc *pulse.Client) {
	w.pulseClient = pc
}

// SetConfigChangeHandler registers a callback that is invoked whenever
// synapses.json changes on disk. The callback receives the newly parsed config.
// This enables hot-reload of brain/scout client settings without restarting.
func (w *Watcher) SetConfigChangeHandler(fn ConfigChangeHandler) {
	w.mu.Lock()
	w.cfgHandler = fn
	w.mu.Unlock()
}

// Start begins watching root recursively. It returns immediately; the event
// loop runs in a background goroutine. Call Stop to shut it down.
func (w *Watcher) Start(root string) error {
	// Record the config file path so handleEvent can detect changes to it.
	// Store the resolved root so reparseFile can check symlinks against it.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		w.rootPath = resolved
	} else {
		w.rootPath = root
	}
	w.configPath = filepath.Join(root, "synapses.json")

	// Add every subdirectory under root to the fsnotify watch list.
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable paths
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return w.fw.Add(path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("walk %s: %w", root, err)
	}

	// Watch .git directory so we detect git commits via COMMIT_EDITMSG writes
	// and can refresh stale blame data for recently-changed files.
	gitDir := filepath.Join(root, ".git")
	if info, err := os.Stat(gitDir); err == nil && info.IsDir() {
		_ = w.fw.Add(gitDir)
	}

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.loop(root)
	}()
	return nil
}

// IsAlive reports whether the file-watching event loop is still running.
// Returns false if the loop has exhausted all restart attempts after panics
// or if Start() was never called.
func (w *Watcher) IsAlive() bool {
	return w.loopAlive.Load() == 1
}

// Stop shuts down the watcher and releases resources. It blocks until all
// fire-and-forget goroutines (persistAsync, ingestToBrain, brain summary
// write-back, cross-project brain detection, index rebuild) have returned,
// preventing writes to a closed SQLite store after Stop returns.
func (w *Watcher) Stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	w.stopCtxFn()
	close(w.stopCh)
	w.fw.Close()

	// Cancel any pending debounce timers. Each timer was registered with
	// wg.Add(1) in debounce/debounceConfigReload. If Stop succeeds (timer
	// hasn't fired yet), we must call wg.Done to balance the Add — otherwise
	// wg.Wait blocks forever.
	for _, t := range w.timers {
		if t.Stop() {
			w.wg.Done()
		}
	}
	w.mu.Unlock()

	// Wait for all in-flight goroutines to finish. This must happen AFTER
	// closing stopCh (so goroutines checking stopCh can exit) and OUTSIDE
	// the mutex (goroutines may call methods that take mu).
	w.wg.Wait()
}

// trackGo launches fn in a goroutine tracked by the WaitGroup. If the watcher
// is already stopped, fn is NOT launched and trackGo returns false. The stopped
// check and wg.Add are performed atomically under mu to prevent a race where
// Stop().wg.Wait() returns before a concurrent reparseFile calls wg.Add.
func (w *Watcher) trackGo(fn func()) bool {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return false
	}
	w.wg.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		fn()
	}()
	return true
}

// RecentChanges returns all ChangeEvents recorded within the last windowMinutes.
// If windowMinutes is <= 0 all recorded events are returned.
func (w *Watcher) RecentChanges(windowMinutes int) []ChangeEvent {
	w.changeMu.RLock()
	defer w.changeMu.RUnlock()
	if len(w.changeLog) == 0 {
		return nil
	}
	if windowMinutes <= 0 {
		out := make([]ChangeEvent, len(w.changeLog))
		copy(out, w.changeLog)
		return out
	}
	cutoff := time.Now().Add(-time.Duration(windowMinutes) * time.Minute)
	var result []ChangeEvent
	for _, e := range w.changeLog {
		if e.Timestamp.After(cutoff) {
			result = append(result, e)
		}
	}
	return result
}

// loop is the background event processing goroutine. If a panic occurs inside
// the event loop it is recovered and the loop is restarted with exponential
// backoff (up to maxLoopRestarts times). If all restarts are exhausted the
// watcher silently stops — file watching is disabled but the daemon stays up.
func (w *Watcher) loop(root string) {
	const maxRestarts = 3
	backoff := 100 * time.Millisecond
	w.loopAlive.Store(1)

	for attempt := 0; attempt <= maxRestarts; attempt++ {
		panicked := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					logutil.Error("synapses/watcher: panic in loop (attempt %d/%d): %v\n", attempt+1, maxRestarts+1, r)
					panicked = true
					w.loopPanics.Add(1) // P2-4: track panic count
					// P7-4: emit watcher panic count to Pulse.
					if w.pulseClient != nil {
						w.pulseClient.RecordLifecycleEvent("watcher_panic", float64(w.loopPanics.Load()), w.projectID)
					}
				}
			}()
			for {
				select {
				case <-w.stopCh:
					return
				case event, ok := <-w.fw.Events:
					if !ok {
						return
					}
					w.handleEvent(event, root)
				case err, ok := <-w.fw.Errors:
					if !ok {
						return
					}
					logutil.Error("synapses/watcher: %v\n", err)
				}
			}
		}()

		// Clean exit (stopCh closed or fw closed) — no restart needed.
		if !panicked {
			return
		}

		// Panic recovery: check if stop was also requested before restarting.
		select {
		case <-w.stopCh:
			return
		default:
		}

		if attempt < maxRestarts {
			logutil.Info("synapses/watcher: restarting event loop in %v\n", backoff)
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	w.loopAlive.Store(0)
	// P7-5: emit watcher death event when all restarts exhausted.
	if w.pulseClient != nil {
		w.pulseClient.RecordLifecycleEvent("watcher_dead", float64(w.loopPanics.Load()), w.projectID)
	}
	logutil.Error("synapses/watcher: loop exhausted all %d restart attempts, file watching disabled\n", maxRestarts)
}

// handleEvent processes a single fsnotify event.
func (w *Watcher) handleEvent(event fsnotify.Event, root string) {
	path := event.Name

	// New directory created: add it and its children to the watch list.
	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if !shouldSkipDir(filepath.Base(path)) {
				_ = w.fw.Add(path)
			}
			return
		}
	}

	// File removed or renamed: prune its nodes from the graph immediately.
	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		// reparseMu must cover the entire graph-mutation block here — not just
		// the map deletions below.  applyBatch Phase 0 reads graph edges (via
		// graph.mu) without holding reparseMu, relying on the guarantee that all
		// graph writers hold reparseMu.  If we mutated the graph outside this
		// lock, Phase 0 would observe a partially-removed file and compute a
		// stale invalidation set that misses callers of the deleted node.
		// Holding reparseMu for the full remove also serialises with applyBatch
		// Phase 1, preventing a concurrent "remove + reparse" interleaving.
		w.reparseMu.Lock()
		// AM-2: snapshot node IDs before removal so we can cascade memory invalidation.
		var removedIDs []string
		if w.store != nil {
			for _, n := range w.graph.NodesForFile(path) {
				removedIDs = append(removedIDs, string(n.ID))
			}
		}
		w.graph.RemoveFile(path)
		w.graph.RemoveCallSitesForFile(path)
		w.graph.RemoveTerraformRefsForFile(path)
		w.graph.InvalidateCache()
		// Clean up parse-error tracking and filePkg for deleted files.
		delete(w.fileHadParseErrors, path)
		delete(w.filePkg, path)
		w.reparseMu.Unlock()
		// fileHashMu is independent of reparseMu — update outside the lock.
		w.fileHashMu.Lock()
		delete(w.fileHashes, path)
		w.fileHashMu.Unlock()
		// Remove this file's call sites from the persisted table so they are
		// not reloaded by future reparseFile calls for other files.
		if w.store != nil {
			if err := w.store.UpdateCallSitesForFile(path, nil); err != nil {
				logutil.Error("synapses/watcher: remove call sites for %s: %v\n", path, err)
			}
		}
		// AM-2: cascade stale flag to memories anchored to the removed nodes.
		// Gap-4: also stale entity-tier memories written with entity_id but no anchors.
		if w.store != nil && len(removedIDs) > 0 {
			if err := w.store.MarkAnchoredMemoriesStale(removedIDs, "anchor node removed"); err != nil {
				logutil.Warn("synapses/watcher: cascade memory stale: %v\n", err)
			}
			if err := w.store.MarkEntityMemoriesStaleForNodes(removedIDs, "entity node removed"); err != nil {
				logutil.Warn("synapses/watcher: cascade entity memory stale: %v\n", err)
			}
		}
		w.persistAsync("")
		return
	}

	// File written or created: debounce then re-parse.
	if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
		// synapses.json hot-reload: reload config and reconnect clients.
		if w.cfgHandler != nil && path == w.configPath {
			w.debounceConfigReload(path)
			return
		}

		// Git commit detected: COMMIT_EDITMSG is written on every `git commit`.
		// Re-enrich blame for recently-changed files so blame reflects the new
		// commit hash rather than the pre-commit state captured at save time.
		if filepath.Base(path) == "COMMIT_EDITMSG" {
			w.refreshBlameAfterCommit(root)
			return
		}

		w.debounce(path, root)
	}
}

// debounce coalesces rapid write events for the same file into a single
// re-parse after debounceDelay of silence. Uses a bounded work channel to
// prevent thundering herd on large checkouts (>500 pending files trigger
// a batched full re-index instead of individual reparses).
func (w *Watcher) debounce(path, root string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.timers[path]; ok {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(debounceDelay)
		return
	}

	// Thundering herd protection: if too many files are pending, switch to
	// a full reindex instead of scheduling individual reparses.
	if len(w.timers) >= reparseBacklogThreshold {
		// Cancel all pending timers and schedule one full re-walk.
		for p, t := range w.timers {
			if t.Stop() {
				w.wg.Done() // balance the wg.Add(1) from when the timer was created
			}
			delete(w.timers, p)
		}
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			// Abort if watcher is already stopping to avoid writing to a closed store.
			select {
			case <-w.stopCh:
				return
			default:
			}
			logutil.Info("synapses/watcher: backlog > %d files, triggering full re-walk of %s\n", reparseBacklogThreshold, root)
			if _, err := w.walker.WalkDir(w.graph, root); err != nil {
				logutil.Warn("synapses/watcher: full re-walk failed: %v\n", err)
			} else {
				// Full re-walk bypasses applyBatch, so trigger the name matcher
				// explicitly. Pass nil = all domains affected, always run.
				w.triggerNameMatcher(nil)
			}
		}()
		return
	}

	w.wg.Add(1) // track the timer callback; when work is sent to workCh, the
	// callback's wg.Done fires before the worker processes the item, but this
	// is safe: worker goroutines are separately tracked by wg (see New()),
	// and Stop() closes workCh causing workers to drain before wg.Wait returns.
	w.timers[path] = time.AfterFunc(debounceDelay, func() {
		defer w.wg.Done()
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()

		// Use bounded work channel to limit concurrent reparses.
		// Guard with stopCh so timer callbacks that fire after Stop()
		// closes stopCh do not block or write to a closed store.
		select {
		case w.workCh <- reparseWork{path, root}:
			return
		case <-w.stopCh:
			return
		default:
			// Channel full — drop rather than bypass the worker pool bound.
			// The file will be re-parsed on the next change event.
			logutil.Debug("synapses: watcher: work channel full, dropping reparse for %s\n", path)
			return
		}
	})
}

// debounceConfigReload coalesces rapid writes to synapses.json into a single
// config reload after debounceDelay of silence. Uses the same timer map as
// code file debouncing since the key space is separate.
func (w *Watcher) debounceConfigReload(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.timers[path]; ok {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(debounceDelay)
		return
	}

	w.wg.Add(1) // track the timer callback so Stop() waits for it
	w.timers[path] = time.AfterFunc(debounceDelay, func() {
		defer w.wg.Done()
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()

		w.reloadConfig(path)
	})
}

// reloadConfig parses the updated synapses.json and calls the registered
// ConfigChangeHandler. Errors are logged to stderr but are otherwise non-fatal.
func (w *Watcher) reloadConfig(configPath string) {
	dir := filepath.Dir(configPath)
	newCfg, err := config.Load(dir)
	if err != nil {
		logutil.Error("synapses/watcher: reload %s: %v\n", configPath, err)
		if w.pulseClient != nil {
			w.pulseClient.RecordConfigReload(pulse.ConfigReloadEvent{
				Success:   false,
				ProjectID: w.projectID,
			})
		}
		return
	}
	// Update the watcher's own violation-checking config so future file changes
	// use the freshly loaded rules. Snapshot handler under the same lock so
	// reads of w.cfg and w.cfgHandler from concurrent reparseFile goroutines
	// don't race with this timer-goroutine write.
	w.mu.Lock()
	w.cfg = newCfg
	handler := w.cfgHandler
	w.mu.Unlock()

	logutil.Info("synapses/watcher: config reloaded from %s\n", configPath)
	if handler != nil {
		handler(newCfg)
	}
	if w.pulseClient != nil {
		w.pulseClient.RecordConfigReload(pulse.ConfigReloadEvent{
			Success:   true,
			ProjectID: w.projectID,
		})
	}
}

// fileContentHash returns the hex-encoded SHA-256 hash of a file's content.
// Returns ("", false) if the file cannot be read (e.g., deleted between
// fsnotify event and hash computation). Caller treats ("", false) as
// "hash unknown" and proceeds conservatively (as if content changed).
func fileContentHash(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return fmt.Sprintf("%x", h.Sum(nil)), true
}

// prepareParseResult performs the CPU-bound (parallelizable) part of file
// re-parsing: symlink validation, file reading, parse-error check, content
// hashing, and tree-sitter AST parsing into a temporary graph. It does NOT
// acquire reparseMu and does NOT touch the main graph.
//
// The temporary graph is created with the same repoID and root as the main
// graph so that all node IDs are identical to what the main graph would
// produce — making MergeFrom / AddNode idempotent.
func (w *Watcher) prepareParseResult(path string) (result parseFileResult) {
	result.path = path
	result.reparseStart = time.Now()

	// BUG-009: Symlink traversal defense with TOCTOU protection.
	var lstatIno uint64
	if fi, lstatErr := os.Lstat(path); lstatErr == nil {
		if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
			lstatIno = stat.Ino
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			resolved, evalErr := filepath.EvalSymlinks(path)
			if evalErr != nil {
				logutil.Warn("synapses/watcher: skipping broken symlink %s: %v\n", path, evalErr)
				result.err = evalErr
				return
			}
			root := w.rootPath
			if root == "" {
				logutil.Warn("synapses/watcher: skipping symlink %s → %s (no root path to verify against)\n", path, resolved)
				result.err = fmt.Errorf("no root path")
				return
			}
			if !strings.HasPrefix(resolved, root+string(filepath.Separator)) && resolved != root {
				logutil.Warn("synapses/watcher: skipping symlink %s → %s (outside project root %s)\n", path, resolved, root)
				result.err = fmt.Errorf("symlink outside project root")
				return
			}
		}
	}
	result.lstatIno = lstatIno

	// Read the file once — reused for HasParseErrors, hash, and parse.
	src, err := os.ReadFile(path)
	if err != nil {
		result.err = err
		return
	}

	// BUG-009 TOCTOU check: verify inode hasn't changed between Lstat and ReadFile.
	if lstatIno != 0 {
		if postFi, postErr := os.Lstat(path); postErr == nil {
			if postStat, ok := postFi.Sys().(*syscall.Stat_t); ok && postStat.Ino != lstatIno {
				logutil.Warn("synapses/watcher: inode changed for %s between check and read (possible symlink swap) — discarding\n", path)
				result.err = fmt.Errorf("inode changed (TOCTOU)")
				return
			}
		}
	}
	result.src = src

	// Sprint 11.9: Check for tree-sitter parse errors using pre-read bytes.
	result.hasErrors = w.walker.HasParseErrors(path, src)

	// Content hash from the already-read bytes (avoids a second ReadFile).
	h := sha256.New()
	h.Write(src)
	result.hash = fmt.Sprintf("%x", h.Sum(nil))
	result.hashKnown = true

	// Parse into a temporary graph with the same repoID and root so node IDs
	// match the main graph exactly. The temp graph is discarded after merge.
	tempGraph := graph.New(w.graph.RepoID())
	if root := w.graph.Root(); root != "" {
		tempGraph.SetRoot(root)
	}
	if err := w.walker.ParseFileSrc(tempGraph, path, src); err != nil {
		logutil.Error("synapses/watcher: pre-parse %s: %v\n", path, err)
		result.err = err
		return
	}
	result.tempGraph = tempGraph
	return
}

// mergeLoop is the single serialised goroutine that applies pre-parsed results
// to the main graph. It reads from parseCh and coalesces bursts (e.g. branch
// switches) into batches so that the resolver runs once per batch rather than
// once per file.
func (w *Watcher) mergeLoop() {
	defer w.wg.Done()
	for {
		var first parseFileResult
		select {
		case first = <-w.parseCh:
		case <-w.stopCh:
			// Drain any already-prepared results before exiting.
			for {
				select {
				case r := <-w.parseCh:
					w.applyBatch([]parseFileResult{r})
				default:
					return
				}
			}
		}

		batch := []parseFileResult{first}

		// Batch coalescing: if ≥ batchCoalesceThreshold results are already
		// in the channel (branch switch scenario), wait batchCoalesceDelay to
		// let the parse workers fill up the channel further before applying.
		if len(w.parseCh) >= batchCoalesceThreshold {
			timer := time.NewTimer(batchCoalesceDelay)
		coalesce:
			for {
				select {
				case r := <-w.parseCh:
					batch = append(batch, r)
				case <-timer.C:
					break coalesce
				case <-w.stopCh:
					timer.Stop()
					w.applyBatch(batch)
					return
				}
			}
			timer.Stop()
		}

		// Drain any immediately-available results without additional waiting.
	drain:
		for {
			select {
			case r := <-w.parseCh:
				batch = append(batch, r)
			default:
				break drain
			}
		}

		w.applyBatch(batch)
	}
}

// applyBatch applies a batch of pre-parsed file results to the main graph.
//
// Three-phase design (lock-order safe):
//
//   Phase 0 (NO lock): pre-snapshot.
//     Compute invalidation sets and pre-load stored call sites from SQLite.
//     Safe without reparseMu because all graph writers hold reparseMu (both
//     applyBatch Phase 1 and handleEvent's remove path).  Reads are serialised
//     at graph.mu.  Any staleness between Phase 0 and Phase 1 is benign.
//     All store I/O happens here, eliminating the implicit reparseMu→store.mu
//     lock-ordering dependency that would cause a deadlock if store ever needed
//     to re-enter the watcher.
//
//   Phase 1 (reparseMu held): pure in-memory graph mutations.
//     RemoveFile / MergeFrom / BulkAddCallSites / resolvers.
//     Zero store I/O. reparseMu is held for the minimum required duration.
//
//   Phase 2 (NO lock): store writes + side effects.
//     Memory staling, call-site persistence, federation, pulse events.
//     Uses pre-computed IDs captured under the lock (removedIDs, changedIDs).
//
// Running resolvers once per batch (rather than once per file) is the primary
// performance win for branch switches: 20 files → 1 resolver pass instead of 20.
func (w *Watcher) applyBatch(results []parseFileResult) {
	if len(results) == 0 {
		return
	}

	// ── Phase 0: pre-snapshot (NO lock held) ─────────────────────────────────
	// Pre-compute invalidation sets and load stored call sites from SQLite.
	// All graph writers (applyBatch Phase 1 and handleEvent's remove path) hold
	// reparseMu before mutating the graph.  Phase 0 reads are serialised at the
	// graph.mu level, so they see a consistent snapshot of the last completed
	// writer.  Any edge observed here may be removed by a concurrent handleEvent
	// between Phase 0 and Phase 1, but that is benign: the invalidation set
	// erring toward inclusion is safe (extra entries are no-ops).
	type preFetch struct {
		invalidSet     []string
		preloadedSites []graph.CallSite
	}
	preFetches := make([]preFetch, len(results))
	if w.store != nil {
		for i, result := range results {
			if result.err != nil {
				continue
			}
			preFetches[i].invalidSet = w.computeInvalidationSet(result.path)
			sites, loadErr := w.store.LoadCallSitesForFiles(preFetches[i].invalidSet)
			if loadErr != nil {
				logutil.Warn("synapses/watcher: scoped call-site pre-load failed, falling back to full load: %v\n", loadErr)
				sites, _ = w.store.LoadCallSites()
			}
			preFetches[i].preloadedSites = sites
		}
	}

	// ── Phase 1: acquire lock, pure graph mutations ───────────────────────────
	w.reparseMu.Lock()
	reparseMuHeld := true
	defer func() {
		if reparseMuHeld {
			w.reparseMu.Unlock()
		}
	}()

	// Phase 1: per-file validation and graph mutation.
	// Filter out errored results, handle parse-error skip/proceed logic, then
	// remove stale data and merge new nodes/edges into the main graph.
	type fileState struct {
		result         parseFileResult
		errorAction    string // "clean", "proceed", "skip"
		nodesBefore    int
		edgesBefore    int
		nodesAfter     int    // captured under reparseMu for accurate pulse reporting
		edgesAfter     int    // captured under reparseMu for accurate pulse reporting
		beforeNodeIDs  []string
		prevFileHash   string
		newCallSites   []graph.CallSite
		memoriesStaled int
		// Pre-computed under reparseMu, consumed by Phase 2 store writes.
		// Separating capture (in-memory, needs lock) from persistence (I/O, no
		// lock needed) eliminates store I/O from the critical section.
		removedIDs     []string // node IDs absent after reparse (stale anchors)
		changedIDs     []string // node IDs present before and after (re-anchored)
		contentChanged bool     // hash changed — embeddings need invalidation
	}

	valid := make([]fileState, 0, len(results))
	for i, result := range results {
		if result.err != nil {
			// prepareParseResult failed (disk error, symlink rejection, etc.)
			continue
		}

		state := fileState{result: result}

		// Sprint 11.9: parse error skip/proceed logic.
		// fileHadParseErrors is accessed only from this goroutine (reparseMu).
		if result.hasErrors {
			if !w.fileHadParseErrors[result.path] {
				// First error: skip, retain previous clean data.
				w.fileHadParseErrors[result.path] = true
				logutil.Warn("synapses/watcher: skipping reparse of %s: AST has errors (file may be mid-save)\n", result.path)
				if w.pulseClient != nil {
					lang := strings.TrimPrefix(strings.ToLower(filepath.Ext(result.path)), ".")
					w.pulseClient.RecordReparseEvent(pulse.ReparseEvent{
						File:        result.path,
						Language:    lang,
						ProjectID:   w.projectID,
						ErrorAction: "skip",
					})
				}
				continue
			}
			// Persistent error: proceed with the error-recovered AST.
			logutil.Warn("synapses/watcher: reparsing %s despite errors (persistent, not mid-save)\n", result.path)
			state.errorAction = "proceed"
		} else {
			delete(w.fileHadParseErrors, result.path)
			state.errorAction = "clean"
		}

		// Snapshot counts before mutation.
		state.nodesBefore = w.countNodesForFile(result.path)
		state.edgesBefore = len(w.graph.OutEdgesForFile(result.path))

		// Snapshot stable UUIDs before removing nodes.
		w.graph.SnapshotFileStableIDs(result.path)

		// AM-2: capture node IDs before removal for memory staling cascade.
		// Sprint 10.7: capture previous file hash for embedding invalidation.
		if w.store != nil {
			for _, n := range w.graph.NodesForFile(result.path) {
				state.beforeNodeIDs = append(state.beforeNodeIDs, string(n.ID))
			}
			w.fileHashMu.Lock()
			state.prevFileHash = w.fileHashes[result.path]
			w.fileHashMu.Unlock()
		}

		// Remove stale data from main graph.
		w.graph.RemoveFile(result.path)
		w.graph.RemoveCallSitesForFile(result.path)
		w.graph.RemoveTerraformRefsForFile(result.path)

		// Merge pre-parsed nodes and edges from tempGraph into main graph.
		// tempGraph was built with the same repoID+root, so node IDs match.
		w.graph.MergeFrom(result.tempGraph)

		// Migrate stable UUIDs for re-parsed nodes (preserves cross-project refs).
		for _, n := range w.graph.NodesForFile(result.path) {
			w.graph.MigrateStableID(n)
		}
		w.graph.ClearFileSnapshot(result.path)

		// Sprint 10.7: persist new content hash (fileHashMu is independent of
		// reparseMu — updating it here is safe and keeps hash lookups consistent).
		if w.store != nil && result.hashKnown {
			w.fileHashMu.Lock()
			w.fileHashes[result.path] = result.hash
			w.fileHashMu.Unlock()
		}

		// AM-2: compute which nodes were removed/changed for Phase 2 store writes.
		// We capture the IDs here (under reparseMu, after the graph mutation) so
		// Phase 2 can call MarkAnchoredMemoriesStale without holding any lock.
		if w.store != nil && len(state.beforeNodeIDs) > 0 {
			afterIDs := make(map[string]struct{}, len(state.beforeNodeIDs))
			for _, n := range w.graph.NodesForFile(result.path) {
				afterIDs[string(n.ID)] = struct{}{}
			}
			for _, id := range state.beforeNodeIDs {
				if _, ok := afterIDs[id]; !ok {
					state.removedIDs = append(state.removedIDs, id)
				} else {
					state.changedIDs = append(state.changedIDs, id)
				}
			}
			state.contentChanged = !result.hashKnown || result.hash != state.prevFileHash
		}

		// Sprint 14.3: update filePkg for the re-parsed file.
		w.updateImportGraphForFile(result.path)

		// Capture new call sites from the temp graph.
		state.newCallSites = result.tempGraph.DrainCallSites()

		// Apply pre-loaded stored call sites (computed in Phase 0, no store I/O here).
		// Filter out any sites whose CallerFile is this file — they are stale and
		// will be replaced by state.newCallSites below.
		if w.store != nil {
			pf := preFetches[i]
			var filtered []graph.CallSite
			for _, cs := range pf.preloadedSites {
				if cs.CallerFile != result.path {
					filtered = append(filtered, cs)
				}
			}
			w.graph.BulkAddCallSites(filtered)
		}

		// Add fresh call sites and terraform refs from this file's parse.
		w.graph.BulkAddCallSites(state.newCallSites)
		for _, ref := range result.tempGraph.DrainTerraformRefs() {
			w.graph.AddTerraformRef(ref)
		}

		valid = append(valid, state)
	}

	if len(valid) == 0 {
		return
	}

	// Phase 2: run resolvers ONCE for the entire batch.
	// This is the primary win for branch switches: N files → 1 resolver pass.
	resolver.ResolveCallEdges(w.graph)
	resolver.ResolveHeritageEdges(w.graph)
	resolver.ResolveImplementsEdges(w.graph)

	// Doc edges: if any file in the batch is a code file, run the full scan
	// (markdown sections may reference the new entities). Otherwise use the
	// file-scoped variant (only markdown files changed — code entities unchanged).
	hasNonMarkdown := false
	for _, s := range valid {
		ext := strings.ToLower(filepath.Ext(s.result.path))
		if ext != ".md" && ext != ".markdown" && ext != ".mdx" {
			hasNonMarkdown = true
			break
		}
	}
	if hasNonMarkdown {
		resolver.ResolveDocEdges(w.graph)
	} else {
		for _, s := range valid {
			resolver.ResolveDocEdgesForFile(w.graph, s.result.path)
		}
	}

	// NL-to-graph Tier 0+1: extract entity candidates from markdown section
	// bodies and create knowledge nodes (concept/entity/artifact/decision).
	// Only meaningful for markdown files — code file changes don't add sections.
	// Batch variant: ResolveNLEntitiesForFiles calls buildCodeNames once for all
	// files in the batch (O(|graph|) instead of O(N×|graph|) for N markdown files).
	// Tier 2 LLM classification is submitted as a P1 brain task per file.
	var mdPaths []string
	for _, s := range valid {
		if ext := strings.ToLower(filepath.Ext(s.result.path)); ext == ".md" || ext == ".markdown" || ext == ".mdx" {
			mdPaths = append(mdPaths, s.result.path)
		}
	}
	if len(mdPaths) > 0 {
		for fp, unresolved := range resolver.ResolveNLEntitiesForFiles(w.graph, mdPaths) {
			w.scheduleNLClassification(fp, unresolved)
		}
	}

	// Phase 3: per-file post-resolve work (still under reparseMu).
	// Only in-memory operations here — zero store I/O.
	// Store writes (call-site persistence, memory staling) moved to Phase 2
	// (after reparseMu is released) to keep the critical section lock-order safe.
	for i := range valid {
		s := &valid[i] // pointer so mutations are visible outside the loop

		// R3/R34: re-enrich blame and commit context for changed file.
		if root := w.graph.Root(); root != "" {
			metrics.EnrichBlameForFile(w.graph, root, s.result.path)
			metrics.EnrichCommitContextForFile(w.graph, root, s.result.path)
		}

		w.graph.InvalidateCacheForFile(s.result.path)

		// Rebuild columnar GraphIndex asynchronously.
		w.trackGo(func() {
			rebuildStart := time.Now()
			w.graph.RebuildIndex()
			if w.pulseClient != nil {
				w.pulseClient.RecordGraphSnapshot(pulse.GraphSnapshotEvent{
					RebuildDurationMs: float64(time.Since(rebuildStart).Milliseconds()),
					RebuildTrigger:    "file_change",
				})
			}
		})

		if w.pktInval != nil {
			w.pktInval.InvalidatePacketCacheForFile(s.result.path)
			if w.pulseClient != nil {
				w.pulseClient.RecordMemoryOp(pulse.MemoryOperationEvent{
					Operation: "invalidation_cascade",
					Count:     1,
				})
			}
		}

		// Capture node/edge counts under reparseMu so pulse events are accurate.
		// Using index-based loop + pointer ensures the stored values are visible
		// in the post-lock section that reads valid[i].nodesAfter.
		s.nodesAfter = w.countNodesForFile(s.result.path)
		s.edgesAfter = len(w.graph.OutEdgesForFile(s.result.path))
		w.recordChange(s.result.path, s.nodesBefore, s.nodesAfter, s.edgesAfter-s.edgesBefore)

		w.checkViolations(s.result.path)
		w.notifyCrossProjectImpact(s.result.path)
		w.notifyIntraProjectImpact(s.result.path)
	}

	// Release reparseMu before I/O-heavy post-steps (federation, pulse, persist).
	w.reparseMu.Unlock()
	reparseMuHeld = false

	// ── Phase 2: store writes (NO lock held) ──────────────────────────────────
	// Memory staling and call-site persistence run here, outside reparseMu.
	// They use IDs captured under the lock (removedIDs, changedIDs, newCallSites)
	// so no lock is required.  Running these sequentially (not via trackGo)
	// prevents goroutine storms against the SQLite store during burst changes.
	for i := range valid {
		s := &valid[i]

		// AM-2: cascade stale flag to memories anchored to disappeared nodes.
		if w.store != nil && len(s.removedIDs) > 0 {
			s.memoriesStaled = len(s.removedIDs)
			if err := w.store.MarkAnchoredMemoriesStale(s.removedIDs, "anchor node removed"); err != nil {
				logutil.Warn("synapses/watcher: cascade memory stale: %v\n", err)
			}
			if err := w.store.MarkEntityMemoriesStaleForNodes(s.removedIDs, "entity node removed"); err != nil {
				logutil.Warn("synapses/watcher: cascade entity memory stale: %v\n", err)
			}
		}
		if w.store != nil && len(s.changedIDs) > 0 && s.contentChanged {
			memIDs, err := w.store.GetMemoryIDsByAnchorNodes(s.changedIDs, 500)
			if err != nil {
				logutil.Warn("synapses/watcher: get anchor memory ids for embedding invalidation: %v\n", err)
			} else if len(memIDs) > 0 {
				if err := w.store.MarkMemoryEmbeddingsStale(memIDs); err != nil {
					logutil.Warn("synapses/watcher: invalidate anchor embeddings: %v\n", err)
				}
			}
		}

		// Persist updated call sites for this file.
		if w.store != nil {
			if err := w.store.UpdateCallSitesForFile(s.result.path, s.newCallSites); err != nil {
				logutil.Error("synapses/watcher: update call sites for %s: %v\n", s.result.path, err)
			}
		}
	}

	for _, s := range valid {
		// RX2 Phase 3: cross-project dependency detection.
		if w.cpTracker != nil && w.store != nil {
			fedStart := time.Now()
			cpCtx, cpCancel := context.WithTimeout(w.stopCtx, 2*time.Second)
			w.cpTracker.DetectAndStore(cpCtx, s.result.path, w.store)
			cpCancel()
			if w.pulseClient != nil {
				w.pulseClient.RecordFederationEvent(pulse.FederationDetectEvent{
					ProjectID:  w.graph.RepoID(),
					Tier:       1,
					DurationMs: float64(time.Since(fedStart).Milliseconds()),
					EventType:  "detect_and_store",
				})
			}
			if w.cpBrainTracker != nil {
				filePath := s.result.path
				w.trackGo(func() {
					brainStart := time.Now()
					brainCtx, brainCancel := context.WithTimeout(w.stopCtx, 10*time.Second)
					defer brainCancel()
					w.cpBrainTracker.DetectAndStoreBrain(brainCtx, filePath, w.store)
					brainDurationMs := float64(time.Since(brainStart).Milliseconds())
					if w.pulseClient != nil {
						w.pulseClient.RecordFederationEvent(pulse.FederationDetectEvent{
							ProjectID:  w.graph.RepoID(),
							Tier:       2,
							DurationMs: brainDurationMs,
							EventType:  "detect_brain",
						})
						w.pulseClient.RecordBrainUsage(pulse.BrainUsageEvent{
							Model:      "brain:federation",
							Tier:       "federation",
							Endpoint:   "cross_project_detect",
							DurationMs: int64(brainDurationMs),
							ProjectID:  w.graph.RepoID(),
							Success:    true,
						})
					}
				})
			}
		}

		// P2-3: emit ReparseEvent. Use counts captured under reparseMu (Phase 3)
		// so values are not affected by concurrent parses that ran after unlock.
		if w.pulseClient != nil {
			lang := strings.TrimPrefix(strings.ToLower(filepath.Ext(s.result.path)), ".")
			deltaRows := s.nodesAfter - s.nodesBefore
			if deltaRows < 0 {
				deltaRows = -deltaRows
			}
			w.pulseClient.RecordReparseEvent(pulse.ReparseEvent{
				File:           s.result.path,
				Language:       lang,
				DurationMs:     time.Since(s.result.reparseStart).Milliseconds(),
				NodesBefore:    s.nodesBefore,
				NodesAfter:     s.nodesAfter,
				EdgesDelta:     s.edgesAfter - s.edgesBefore,
				MemoriesStaled: s.memoriesStaled,
				ProjectID:      w.projectID,
				DeltaRows:      deltaRows,
				ErrorAction:    s.errorAction,
			})
		}

		logutil.Info("synapses/watcher: updated %s\n", s.result.path)
		w.persistAsync(s.result.path)

		if w.brainClient != nil {
			select {
			case w.brainWorkCh <- s.result.path:
			default:
			}
		}
	}
	// Collect changed file paths for domain-relevance filtering in the name matcher.
	// Only paths from successful parses are included (valid slice, not results slice).
	changedPaths := make([]string, 0, len(valid))
	for _, s := range valid {
		changedPaths = append(changedPaths, s.result.path)
	}
	w.triggerNameMatcher(changedPaths)
}

// reparseFile removes stale nodes for path and re-parses it into the graph.
//
// reparseMu serialises concurrent calls: debounce timers for different files
// can fire simultaneously, and ResolveCallEdges drains ALL pending call sites
// from the graph (not just the ones for this file).  If two reparseFile calls
// race, the second DrainCallSites returns empty and those edges are lost.
func (w *Watcher) reparseFile(path, _ string) {
	w.reparseMu.Lock()
	// reparseMu is explicitly unlocked after notify*Impact and before federation
	// detection to avoid holding it across network timeouts. Early returns are
	// covered by the deferred conditional unlock below.
	reparseMuHeld := true
	defer func() {
		if reparseMuHeld {
			w.reparseMu.Unlock()
		}
	}()

	// BUG-009: Symlink traversal defense with TOCTOU protection.
	//
	// Step 1: Lstat to detect symlinks and capture inode.
	// Step 2: If symlink, verify target is within project root.
	// Step 3: After reading, verify inode hasn't changed (prevents swap attacks).
	//
	// On macOS, /var → /private/var is a filesystem mount, not a symlink at the
	// file level — Lstat correctly sees it as a regular file, not ModeSymlink.
	var lstatIno uint64
	if fi, lstatErr := os.Lstat(path); lstatErr == nil {
		if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
			lstatIno = stat.Ino
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			resolved, evalErr := filepath.EvalSymlinks(path)
			if evalErr != nil {
				logutil.Warn("synapses/watcher: skipping broken symlink %s: %v\n", path, evalErr)
				return
			}
			root := w.rootPath
			if root == "" {
				logutil.Warn("synapses/watcher: skipping symlink %s → %s (no root path to verify against)\n", path, resolved)
				return
			}
			if !strings.HasPrefix(resolved, root+string(filepath.Separator)) && resolved != root {
				logutil.Warn("synapses/watcher: skipping symlink %s → %s (outside project root %s)\n", path, resolved, root)
				return
			}
		}
	}

	reparseStart := time.Now() // P2-3: timing

	// Sprint 11.9: Check for tree-sitter parse errors before updating graph.
	// Half-saved files during active editing produce corrupted ASTs with phantom
	// nodes. Strategy: skip on FIRST error (likely transient mid-save), but
	// proceed on SECOND consecutive error (persistent syntax error or grammar
	// gap — stale data is worse than imperfect data from error-recovering parse).
	errorAction := "clean" // P9-7: track parse error action
	if src, err := os.ReadFile(path); err == nil {
		// BUG-009 TOCTOU check: verify inode hasn't changed between Lstat and ReadFile.
		// If an attacker replaced the regular file with a symlink between our check
		// and the read, the inode will differ. Reject to prevent exfiltration.
		if lstatIno != 0 {
			if postFi, postErr := os.Lstat(path); postErr == nil {
				if postStat, ok := postFi.Sys().(*syscall.Stat_t); ok && postStat.Ino != lstatIno {
					logutil.Warn("synapses/watcher: inode changed for %s between check and read (possible symlink swap) — discarding\n", path)
					return
				}
			}
		}
		if w.walker.HasParseErrors(path, src) {
			if !w.fileHadParseErrors[path] {
				// First error: skip reparse, retain previous clean data.
				w.fileHadParseErrors[path] = true
				logutil.Warn("synapses/watcher: skipping reparse of %s: AST has errors (file may be mid-save)\n", path)
				// P9-7: emit skip event so parse error frequency is visible.
				if w.pulseClient != nil {
					lang := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
					w.pulseClient.RecordReparseEvent(pulse.ReparseEvent{
						File:        path,
						Language:    lang,
						ProjectID:   w.projectID,
						ErrorAction: "skip",
					})
				}
				return
			}
			// Persistent error: proceed with parse — tree-sitter's error
			// recovery produces a best-effort AST that's better than stale data.
			logutil.Warn("synapses/watcher: reparsing %s despite errors (persistent, not mid-save)\n", path)
			errorAction = "proceed" // P9-7
		} else {
			// Clean parse: clear the error flag.
			delete(w.fileHadParseErrors, path)
		}
	}

	// Snapshot counts before mutation for ChangeEvent delta.
	// Use OutEdgesForFile (not global EdgeCount) so edges_added reflects
	// edges from this file only, not the entire resolver pass delta.
	nodesBefore := w.countNodesForFile(path)
	edgesBefore := len(w.graph.OutEdgesForFile(path))

	// Snapshot stable UUIDs before removing nodes so they can be migrated to
	// the re-parsed nodes (preserving cross-project references across renames).
	w.graph.SnapshotFileStableIDs(path)

	// AM-2: capture node IDs before removal to detect disappeared nodes after re-parse.
	// Sprint 10.7: also capture the file's current content hash BEFORE re-parsing,
	// so we can compare it against the hash stored from the previous parse. This
	// lets us skip embedding invalidation for no-op saves (same content, different mtime).
	var beforeNodeIDs []string
	var prevFileHash, newFileHash string
	var fileHashKnown bool
	if w.store != nil {
		for _, n := range w.graph.NodesForFile(path) {
			beforeNodeIDs = append(beforeNodeIDs, string(n.ID))
		}
		// Snapshot the OLD hash before we parse. We'll store the NEW hash after.
		w.fileHashMu.Lock()
		prevFileHash = w.fileHashes[path]
		w.fileHashMu.Unlock()
		newFileHash, fileHashKnown = fileContentHash(path)
	}

	// Sprint 14.3: capture callers BEFORE RemoveFile deletes the CALLS/IMPLEMENTS
	// edges that point into this file's nodes. computeInvalidationSet reads
	// InEdgesForFile, which is empty after RemoveFile.
	var invalidSet []string
	if w.store != nil {
		invalidSet = w.computeInvalidationSet(path)
	}

	// Remove stale graph data and call sites for this file before re-parsing.
	w.graph.RemoveFile(path)
	w.graph.RemoveCallSitesForFile(path)
	w.graph.RemoveTerraformRefsForFile(path)

	if err := w.walker.ParseFile(w.graph, path); err != nil {
		logutil.Error("synapses/watcher: re-parse %s: %v\n", path, err)
		w.graph.ClearFileSnapshot(path)
		return
	}

	// Migrate stable UUIDs: reuse old UUIDs for nodes that survived the re-parse
	// (same identity) so cross-project references remain valid.
	for _, n := range w.graph.NodesForFile(path) {
		w.graph.MigrateStableID(n)
	}
	w.graph.ClearFileSnapshot(path)

	// Sprint 10.7: persist the new hash after a successful parse so subsequent
	// parses can compare against it. Done here (outside the beforeNodeIDs guard)
	// so the hash is seeded even on first parse (when beforeNodeIDs is empty).
	if w.store != nil && fileHashKnown {
		w.fileHashMu.Lock()
		w.fileHashes[path] = newFileHash
		w.fileHashMu.Unlock()
	}

	// AM-2: cascade stale flag to memories anchored to nodes that disappeared
	// during re-parse (functions renamed or deleted within the file).
	var memoriesStaled int // P2-14: count for ReparseEvent
	if w.store != nil && len(beforeNodeIDs) > 0 {
		afterIDs := make(map[string]struct{}, len(beforeNodeIDs))
		for _, n := range w.graph.NodesForFile(path) {
			afterIDs[string(n.ID)] = struct{}{}
		}
		var removedIDs []string
		var changedIDs []string // nodes that survived re-parse — implementation may have changed
		for _, id := range beforeNodeIDs {
			if _, ok := afterIDs[id]; !ok {
				removedIDs = append(removedIDs, id)
			} else {
				changedIDs = append(changedIDs, id)
			}
		}
		if len(removedIDs) > 0 {
			memoriesStaled = len(removedIDs) // P2-14: capture for ReparseEvent
			if err := w.store.MarkAnchoredMemoriesStale(removedIDs, "anchor node removed"); err != nil {
				logutil.Warn("synapses/watcher: cascade memory stale: %v\n", err)
			}
			// Gap-4: also stale entity-tier memories written with entity_id but no anchors.
			if err := w.store.MarkEntityMemoriesStaleForNodes(removedIDs, "entity node removed"); err != nil {
				logutil.Warn("synapses/watcher: cascade entity memory stale: %v\n", err)
			}
		}
		// Sprint 10.7: mark EMBEDDINGS stale for surviving (changed) nodes,
		// but only when the file content actually changed. IDE auto-saves and
		// no-op writes with identical content produce the same hash, so we skip
		// embedding invalidation for those (avoiding pointless re-embedding of
		// identical content on next recall).
		// contentChanged is true when: hash unreadable (conservative) OR hash differs.
		contentChanged := !fileHashKnown || newFileHash != prevFileHash
		if len(changedIDs) > 0 && contentChanged {
			memIDs, err := w.store.GetMemoryIDsByAnchorNodes(changedIDs, 500)
			if err != nil {
				logutil.Warn("synapses/watcher: get anchor memory ids for embedding invalidation: %v\n", err)
			} else if len(memIDs) > 0 {
				if err := w.store.MarkMemoryEmbeddingsStale(memIDs); err != nil {
					logutil.Warn("synapses/watcher: invalidate anchor embeddings: %v\n", err)
				}
			}
		}
	}

	// Sprint 14.3: update filePkg for the re-parsed file so the same-package
	// scan in computeInvalidationSet stays accurate for future saves.
	w.updateImportGraphForFile(path)

	// Peek the newly-registered call sites from the re-parsed file before the
	// resolver drains them. We need these to update the stored call-site table.
	newSites := w.graph.PeekCallSites()

	// Reload stored call sites from the INVALIDATION SET only.
	// invalidSet was captured before RemoveFile using the live CALLS edges, so
	// it contains exactly the files whose stored call sites need re-resolution.
	// Files outside the set have no edges into the changed file; their stored
	// edges remain valid. This gives a 10-50x reduction in resolver work for
	// leaf-module changes, and is correct for all languages (no name matching).
	if w.store != nil {
		var stored []graph.CallSite
		var loadErr error
		if stored, loadErr = w.store.LoadCallSitesForFiles(invalidSet); loadErr != nil {
			// Fall back to full load on error so correctness is preserved.
			logutil.Warn("synapses/watcher: scoped call-site load failed, falling back to full load: %v\n", loadErr)
			stored, loadErr = w.store.LoadCallSites()
		}
		if loadErr == nil {
			var filtered []graph.CallSite
			for _, cs := range stored {
				if cs.CallerFile != path {
					filtered = append(filtered, cs)
				}
			}
			w.graph.BulkAddCallSites(filtered)
		}
	}

	resolver.ResolveCallEdges(w.graph)
	resolver.ResolveHeritageEdges(w.graph)
	resolver.ResolveImplementsEdges(w.graph)
	// R31: re-resolve doc edges. For markdown files only the newly parsed
	// sections need linking (code entities are unchanged), so use the
	// file-scoped variant to avoid O(all_sections) work on every file save.
	// For code file changes, all sections may reference the new entities,
	// so a full scan is required.
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".markdown", ".mdx":
		resolver.ResolveDocEdgesForFile(w.graph, path)
	default:
		resolver.ResolveDocEdges(w.graph)
	}

	// NL-to-graph Tier 0+1 (markdown files only).
	// Tier 2 LLM classification submitted as P1 when brain is available.
	if ext == ".md" || ext == ".markdown" || ext == ".mdx" {
		unresolved := resolver.ResolveNLEntitiesForFile(w.graph, path)
		w.scheduleNLClassification(path, unresolved)
	}

	// Keep the stored call-site table consistent with the re-parsed file.
	if w.store != nil {
		if err := w.store.UpdateCallSitesForFile(path, newSites); err != nil {
			logutil.Error("synapses/watcher: update call sites for %s: %v\n", path, err)
		}
	}

	// Re-inject persisted manual edges so user-defined links survive file changes.
	// RemoveFile drops all edges for re-parsed nodes; this restores them.
	// ReinjectManualEdges is idempotent — AddEdge silently drops duplicates.
	if w.store != nil {
		if err := w.store.ReinjectManualEdges(w.graph); err != nil {
			logutil.Warn("synapses/watcher: reinject manual edges after reparse: %v\n", err)
		}
	}

	// R3: re-enrich blame for nodes in the changed file only — keeps blame
	// current without re-running git against the entire graph.
	if root := w.graph.Root(); root != "" {
		metrics.EnrichBlameForFile(w.graph, root, path)
		// R34: re-enrich commit context for the changed file.
		metrics.EnrichCommitContextForFile(w.graph, root, path)
	}

	w.graph.InvalidateCacheForFile(path)

	// Rebuild the columnar GraphIndex asynchronously so BFS reads pick up the
	// latest graph state without blocking the watcher loop.
	// Snapshot is saved by main.go via the store; watcher discards the bytes.
	w.trackGo(func() {
		rebuildStart := time.Now()
		w.graph.RebuildIndex()
		// P5 — COV-7: emit graph rebuild duration to pulse.
		if w.pulseClient != nil {
			w.pulseClient.RecordGraphSnapshot(pulse.GraphSnapshotEvent{
				RebuildDurationMs: float64(time.Since(rebuildStart).Milliseconds()),
				RebuildTrigger:    "file_change",
			})
		}
	})
	if w.pktInval != nil {
		w.pktInval.InvalidatePacketCacheForFile(path)
		// P5 — COV-10: emit memory invalidation cascade event.
		if w.pulseClient != nil {
			w.pulseClient.RecordMemoryOp(pulse.MemoryOperationEvent{
				Operation: "invalidation_cascade",
				Count:     1,
			})
		}
	}

	// Record change event with delta counts.
	nodesAfter := w.countNodesForFile(path)
	edgesAfter := len(w.graph.OutEdgesForFile(path))
	w.recordChange(path, nodesBefore, nodesAfter, edgesAfter-edgesBefore)

	// Proactive violation detection: check rules scoped to the changed file and
	// emit 'rule_violation' events for any violations that are NEW (not already
	// in the violation log). This lets agents discover rule breaks in real time
	// by polling get_events, without manually calling get_violations.
	w.checkViolations(path)

	// Cross-project reactive propagation: if any linked-project node depends on
	// a node we just changed, broadcast a cross_project_impact message so agents
	// working in this session are warned before they continue.
	w.notifyCrossProjectImpact(path)

	// Intra-project change alerts: notify agents whose claimed scope covers the
	// changed file, and agents whose in-progress task has nodes in the changed file.
	w.notifyIntraProjectImpact(path)

	// Release reparseMu before federation detection — cpTracker.DetectAndStore
	// reads sibling stores and writes to local store (both have their own locks).
	// This lets the 4-worker pool achieve real concurrency during federation waits.
	w.reparseMu.Unlock()
	reparseMuHeld = false

	// RX2 Phase 3: detect cross-project dependencies in the changed file.
	// Runs after parsing so sibling entity resolution has fresh graph data.
	// Fail-open: errors logged inside tracker, never blocks the watcher.
	if w.cpTracker != nil && w.store != nil {
		fedStart := time.Now()
		cpCtx, cpCancel := context.WithTimeout(w.stopCtx, 2*time.Second)
		w.cpTracker.DetectAndStore(cpCtx, path, w.store)
		cpCancel()
		// P5 — COV-8: emit federation detection event to pulse.
		if w.pulseClient != nil {
			w.pulseClient.RecordFederationEvent(pulse.FederationDetectEvent{
				ProjectID:  w.graph.RepoID(),
				Tier:       1,
				DurationMs: float64(time.Since(fedStart).Milliseconds()),
				EventType:  "detect_and_store",
			})
		}

		// Tier 2: brain-enhanced detection runs async for languages Tier 1
		// handles poorly (Python, dynamic imports, transitive deps).
		if w.cpBrainTracker != nil {
			filePath := path
			w.trackGo(func() {
				brainStart := time.Now()
				brainCtx, brainCancel := context.WithTimeout(w.stopCtx, 10*time.Second)
				defer brainCancel()
				w.cpBrainTracker.DetectAndStoreBrain(brainCtx, filePath, w.store)
				brainDurationMs := float64(time.Since(brainStart).Milliseconds())
				// P5 — COV-8: emit Tier 2 federation detection event.
				if w.pulseClient != nil {
					w.pulseClient.RecordFederationEvent(pulse.FederationDetectEvent{
						ProjectID:  w.graph.RepoID(),
						Tier:       2,
						DurationMs: brainDurationMs,
						EventType:  "detect_brain",
					})
					// P10-3: record brain LLM cost for federation analysis.
					// Model is synthetic — actual model is encapsulated inside
					// BrainDetector.Generate. Tier and endpoint identify the call path.
					w.pulseClient.RecordBrainUsage(pulse.BrainUsageEvent{
						Model:      "brain:federation",
						Tier:       "federation",
						Endpoint:   "cross_project_detect",
						DurationMs: int64(brainDurationMs),
						ProjectID:  w.graph.RepoID(),
						Success:    true,
					})
				}
			})
		}
	}

	// P2-3: emit ReparseEvent. Enqueue is mutex+append (O(1)) — direct call.
	if w.pulseClient != nil {
		lang := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		// P5 — Item 37: compute delta rows (node count change).
		deltaRows := nodesAfter - nodesBefore
		if deltaRows < 0 {
			deltaRows = -deltaRows
		}
		w.pulseClient.RecordReparseEvent(pulse.ReparseEvent{
			File:           path,
			Language:       lang,
			DurationMs:     time.Since(reparseStart).Milliseconds(),
			NodesBefore:    nodesBefore,
			NodesAfter:     nodesAfter,
			EdgesDelta:     edgesAfter - edgesBefore,
			MemoriesStaled: memoriesStaled, // P2-14: memories_staled proxy
			ProjectID:      w.projectID,
			DeltaRows:      deltaRows,
			ErrorAction:    errorAction, // P9-7
		})
	}

	logutil.Info("synapses/watcher: updated %s\n", path)
	w.persistAsync(path)

	// Ingest changed nodes to brain for semantic summarization.
	// Non-blocking send: if the channel is full during burst file changes,
	// the ingestion is dropped — the file will be re-ingested on the next change.
	if w.brainClient != nil {
		select {
		case w.brainWorkCh <- path:
		default:
			// Channel full — drop this ingestion to avoid backpressure.
		}
	}
}

// checkViolations runs the rule engine against edges touching path and emits
// events for violations that were not already present in the log.
// refreshBlameAfterCommit is called when .git/COMMIT_EDITMSG is written,
// indicating a git commit just completed. It re-enriches blame for all files
// that changed recently (within the last 5 minutes) so that blame_commit
// reflects the new HEAD rather than the pre-commit state.
func (w *Watcher) refreshBlameAfterCommit(root string) {
	graphRoot := string(w.graph.Root())
	if graphRoot == "" {
		return
	}

	recent := w.RecentChanges(5) // last 5 minutes
	if len(recent) == 0 {
		return
	}

	seen := make(map[string]bool, len(recent))
	for _, ev := range recent {
		if seen[ev.File] {
			continue
		}
		seen[ev.File] = true

		absFile := ev.File
		if !filepath.IsAbs(absFile) {
			absFile = filepath.Join(graphRoot, absFile)
		}

		metrics.EnrichBlameForFile(w.graph, graphRoot, absFile)
		metrics.EnrichCommitContextForFile(w.graph, graphRoot, absFile)
	}

	w.graph.InvalidateCache()
}

func (w *Watcher) checkViolations(path string) {
	// Snapshot cfg under lock — reloadConfig may write w.cfg concurrently from
	// a debounce timer goroutine while reparseFile (which calls checkViolations)
	// runs under reparseMu on a different goroutine.
	w.mu.Lock()
	cfg := w.cfg
	w.mu.Unlock()

	if cfg == nil || w.store == nil || len(cfg.Rules) == 0 {
		return
	}

	// Snapshot existing violation IDs for this file BEFORE logging new ones,
	// so we can detect which violations are genuinely new.
	existingIDs, err := w.store.ViolationIDsForFile(path)
	if err != nil {
		existingIDs = make(map[string]struct{}) // safe fallback: treat all as new
	}

	violations := cfg.CheckViolationsForFile(w.graph, path)
	if len(violations) == 0 {
		return
	}

	// Persist to violation_log (upsert — safe to call repeatedly).
	if err := w.store.LogViolations(violations); err != nil {
		logutil.Error("synapses/watcher: log violations: %v\n", err)
	}

	// P5 — COV-13: emit watcher violation count to pulse.
	if w.pulseClient != nil {
		status := "ok"
		if len(violations) > 0 {
			status = "violations_found"
		}
		w.pulseClient.RecordValidationEvent(pulse.ValidationEvent{
			ToolName:       "watcher_violation_check",
			Status:         status,
			ViolationCount: len(violations),
		})
	}

	// Emit an event only for violations that weren't already in the log.
	for _, v := range violations {
		id := store.ViolationID(v.RuleID, string(v.FromNode), string(v.ToNode), string(v.EdgeType))
		if _, known := existingIDs[id]; known {
			continue
		}
		payload, _ := json.Marshal(map[string]string{
			"rule_id":   v.RuleID,
			"severity":  v.Severity,
			"from_node": string(v.FromNode),
			"to_node":   string(v.ToNode),
			"edge_type": string(v.EdgeType),
			"file":      path,
		})
		_ = w.store.AppendEvent("rule_violation", "", string(payload))
	}
}

// countNodesForFile counts nodes in the graph whose File matches path.
// Uses the indexed NodesForFile lookup instead of scanning all nodes.
func (w *Watcher) countNodesForFile(path string) int {
	return len(w.graph.NodesForFile(path))
}

// recordChange appends a ChangeEvent to the circular log, evicting the oldest
// entry when the log is at capacity.
func (w *Watcher) recordChange(path string, nodesBefore, nodesAfter, edgesAdded int) {
	added := 0
	removed := 0
	if nodesAfter > nodesBefore {
		added = nodesAfter - nodesBefore
	} else {
		removed = nodesBefore - nodesAfter
	}
	if edgesAdded < 0 {
		edgesAdded = 0
	}

	// Make the file path repo-relative for cleaner output.
	relFile := w.graph.Root()
	if relFile != "" {
		prefix := relFile
		if len(prefix) > 0 && prefix[len(prefix)-1] != '/' {
			prefix += "/"
		}
		if len(path) > len(prefix) && path[:len(prefix)] == prefix {
			relFile = path[len(prefix):]
		} else {
			relFile = path
		}
	} else {
		relFile = path
	}

	ev := ChangeEvent{
		Timestamp:    time.Now(),
		File:         relFile,
		NodesAdded:   added,
		NodesRemoved: removed,
		EdgesAdded:   edgesAdded,
	}

	w.changeMu.Lock()
	if len(w.changeLog) >= changeLogCap {
		// Evict oldest entry (shift left).
		copy(w.changeLog, w.changeLog[1:])
		w.changeLog[len(w.changeLog)-1] = ev
	} else {
		w.changeLog = append(w.changeLog, ev)
	}
	w.changeMu.Unlock()

	// Emit event to the pull-based log so agents polling get_events see file changes.
	if w.store != nil {
		payload := fmt.Sprintf(
			`{"file":%q,"nodes_added":%d,"nodes_removed":%d,"edges_added":%d}`,
			ev.File, ev.NodesAdded, ev.NodesRemoved, ev.EdgesAdded,
		)
		_ = w.store.AppendEvent("file_change", "", payload)
	}
}

// persistAsync saves the current graph and file mtime to the SQLite cache in
// a goroutine. The changedFile path is used to update the file_hashes table so
// the next smart-reindex skips this file correctly.
//
// Call-site persistence is NOT done here: ResolveCallEdges drains the pending
// call-site list before persistAsync is called, so PeekCallSites() would return
// empty and SaveCallSites would wipe the table. Instead, reparseFile calls
// UpdateCallSitesForFile to keep the stored table current on a per-file basis.
// Failures are logged but do not interrupt the watcher.
func (w *Watcher) persistAsync(changedFile string) {
	if w.store == nil {
		return
	}
	mtime := time.Now().UnixNano()
	w.trackGo(func() {
		persistStart := time.Now()
		if err := w.store.SaveGraphDelta(changedFile, w.graph); err != nil {
			logutil.Error("synapses/watcher: cache save: %v\n", err)
		}
		if changedFile != "" {
			if err := w.store.UpsertFileMtime(changedFile, mtime); err != nil {
				logutil.Error("synapses/watcher: update mtime %s: %v\n", changedFile, err)
			}
		}
		if w.pulseClient != nil {
			w.pulseClient.RecordPersistenceEvent(pulse.PersistenceEvent{
				DurationMs: float64(time.Since(persistStart).Milliseconds()),
				ProjectID:  w.projectID,
			})
		}
	})
}

// ingestToBrain sends nodes from the re-parsed file to the intelligence sidecar
// so its semantic summaries stay current. After ingest, schedules a delayed
// write-back to fetch the generated summaries and store them as annotations.
// Runs in a goroutine; all errors are silently discarded (fail-silent contract).
// scheduleNLClassification submits a Tier 2 LLM classification task for the
// given unresolved entity candidates from filePath. The task runs as P1
// (background, soon) via the brain scheduler. When the LLM returns, each
// knowledge node's NodeType is upgraded from the Tier 0 default ("concept")
// to the classified type (concept | entity | artifact | decision).
//
// No-op when brainClient is nil or the unresolved list is empty.
// Safe to call while reparseMu is held: applyFn runs in the scheduler goroutine
// (not under reparseMu) but graph.UpdateNodeMetadata is internally mutex-safe.
func (w *Watcher) scheduleNLClassification(filePath string, unresolved []parser.EntityCandidate) {
	if w.brainClient == nil || len(unresolved) == 0 {
		return
	}

	// Build NLCandidate list by reconstructing NodeIDs from the candidate names.
	// The NodeID formula must match resolver.makeKnowledgeNodeID:
	//   g.MakeNodeID(filePath, "knowledge:"+resolver.NormalizeKnowledgeName(name))
	candidates := make([]brain.NLCandidate, 0, len(unresolved))
	for _, c := range unresolved {
		norm := resolver.NormalizeKnowledgeName(c.Name)
		if norm == "" {
			continue
		}
		nodeID := w.graph.MakeNodeID(filePath, "knowledge:"+norm)
		candidates = append(candidates, brain.NLCandidate{
			Name:    c.Name,
			Context: c.Context,
			NodeID:  string(nodeID),
		})
	}
	if len(candidates) == 0 {
		return
	}

	req := brain.NLClassifyRequest{FilePath: filePath, Candidates: candidates}
	w.brainClient.ScheduleNLClassification(req, func(results []brain.NLClassifyResult) {
		for _, r := range results {
			if r.NodeType == "" {
				continue
			}
			w.graph.UpdateNodeMetadata(graph.NodeID(r.NodeID), func(n *graph.Node) {
				n.Type = graph.NodeType(r.NodeType)
				if n.Metadata == nil {
					n.Metadata = make(map[string]string)
				}
				n.Metadata["tier"] = "2"
				if r.Description != "" {
					n.Metadata["description"] = r.Description
				}
			})
		}
	})
}

func (w *Watcher) ingestToBrain(path string) {
	bc := w.brainClient
	if bc == nil {
		return
	}
	// Derive context from stopCtx so in-flight ingests are cancelled on shutdown.
	// stopCtx is cancelled by Stop(), so no additional goroutine is needed.
	ctx, cancel := context.WithCancel(w.stopCtx)
	defer cancel()
	nodes := w.graph.NodesForFile(path)
	ingestStart := time.Now()
	var ingestCount int
	for _, n := range nodes {
		if string(n.Type) == "package" || string(n.Type) == "file" {
			continue
		}
		// Check for shutdown between ingest calls.
		select {
		case <-w.stopCh:
			return
		default:
		}
		code := ""
		if sig, ok := n.Metadata["signature"]; ok && sig != "" {
			code = sig
		}
		if doc, ok := n.Metadata["doc"]; ok && doc != "" && code != "" {
			code = "// " + doc + "\n" + code
		}
		bc.Ingest(ctx, brain.IngestRequest{
			ProjectID: w.projectID,
			NodeID:    string(n.ID),
			NodeName:  n.Name,
			NodeType:  string(n.Type),
			Package:   n.Package,
			Code:      code,
		})
		ingestCount++
	}
	// P7-3: emit brain usage for ingest pipeline.
	if w.pulseClient != nil && ingestCount > 0 {
		w.pulseClient.RecordBrainUsage(pulse.BrainUsageEvent{
			Tier: "ingest", Endpoint: "Ingest",
			DurationMs: time.Since(ingestStart).Milliseconds(),
			ProjectID:  w.projectID, Success: true,
		})
	}

	// Delayed write-back: fetch summaries after the brain has processed the ingest queue.
	// Respects stopCh so shutdown isn't delayed by the 15-second wait.
	if w.store != nil {
		nodeList := nodes
		w.trackGo(func() {
			select {
			case <-time.After(15 * time.Second):
			case <-w.stopCh:
				return
			}
			wbStart := time.Now()
			wbSuccess := true
			for _, n := range nodeList {
				if string(n.Type) == "package" || string(n.Type) == "file" {
					continue
				}
				summary := bc.GetSummary(context.Background(), string(n.ID))
				if summary != "" {
					if _, _, err := w.store.AddAnnotationIfNew(string(n.ID), "brain", summary, 60*time.Second); err != nil {
						wbSuccess = false
					}
				}
			}
			// P7-3: emit enrichment event for brain write-back.
			if w.pulseClient != nil {
				w.pulseClient.RecordEnrichmentEvent(pulse.EnrichmentEvent{
					EnrichmentType: "brain_writeback",
					DurationMs:     time.Since(wbStart).Milliseconds(),
					Success:        wbSuccess, ProjectID: w.projectID,
				})
			}
		})
	}
}

// notifyCrossProjectImpact checks whether any node in the re-parsed file is
// depended upon by nodes from a linked (federated) project. When it finds such
// cross-project edges it broadcasts a "cross_project_impact" message on the
// agent bus so all agents connected to this project know their change may
// affect dependent projects.
//
// This is the core of Phase 5 (Cross-Project Reactive Propagation):
//   - Project A has `linked: ["../project-b"]` in synapses.json.
//   - Project B's nodes are merged into A's graph at startup.
//   - When A's watcher re-parses a file it calls this method.
//   - If any of B's nodes CALLS one of A's changed nodes, a broadcast fires.
//   - B's agents (also connected to A's instance) see it in session_init.
//
// Fail-silent: any error is ignored so the watcher loop is never interrupted.
func (w *Watcher) notifyCrossProjectImpact(changedFile string) {
	if w.store == nil {
		return
	}

	primaryRepoID := w.graph.RepoID()
	changedNodes := w.graph.NodesForFile(changedFile)
	if len(changedNodes) == 0 {
		return
	}

	// Compute repo-relative path for the payload (cleaner for agents to read).
	relFile := changedFile
	if root := w.graph.Root(); root != "" {
		prefix := root
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if strings.HasPrefix(changedFile, prefix) {
			relFile = changedFile[len(prefix):]
		}
	}

	// For each changed node, find in-edges from nodes in a different project
	// (linked project nodes that CALL or IMPORT something we just changed).
	type projectImpact struct {
		callerNames []string
		calledNames []string
		edgeTypes   map[string]bool
	}
	byProject := make(map[string]*projectImpact)

	for _, n := range changedNodes {
		// In-edges: linked project depends on this changed node.
		for _, e := range w.graph.InEdges(n.ID) {
			fromRepoID := repoIDOfNodeID(string(e.From))
			if fromRepoID == "" || fromRepoID == primaryRepoID {
				continue
			}
			fromNode := w.graph.GetNode(e.From)
			if fromNode == nil {
				continue
			}
			imp := byProject[fromRepoID]
			if imp == nil {
				imp = &projectImpact{edgeTypes: make(map[string]bool)}
				byProject[fromRepoID] = imp
			}
			imp.callerNames = append(imp.callerNames, fromNode.Name)
			imp.calledNames = append(imp.calledNames, n.Name)
			imp.edgeTypes[string(e.Type)] = true
		}
	}

	if len(byProject) == 0 {
		return
	}

	// P5 — COV-14: emit cross-project impact alert to pulse (one per affected project).
	if w.pulseClient != nil {
		for range byProject {
			w.pulseClient.RecordGuardEvent(pulse.GuardEvent{
				GuardType: "cross_project_impact",
				ProjectID: primaryRepoID,
			})
		}
	}

	for linkedRepoID, imp := range byProject {
		callers := dedup(imp.callerNames)
		called := dedup(imp.calledNames)
		edgeTypes := make([]string, 0, len(imp.edgeTypes))
		for et := range imp.edgeTypes {
			edgeTypes = append(edgeTypes, et)
		}

		payload, err := json.Marshal(map[string]interface{}{
			"changed_file":     relFile,
			"changed_project":  primaryRepoID,
			"affected_project": linkedRepoID,
			"callers_affected": callers, // linked-project nodes that call the changed code
			"changed_symbols":  called,  // which symbols in the changed file are depended on
			"edge_types":       edgeTypes,
			"hint": fmt.Sprintf(
				"Project %q depends on %d symbol(s) you just changed in %s. Verify compatibility.",
				linkedRepoID, len(called), relFile,
			),
		})
		if err != nil {
			logutil.Error("synapses/watcher: marshal cross_project_impact: %v\n", err)
			continue
		}
		_, _ = w.store.SendMessage(
			"synapses-watcher", // from: the watcher itself
			"",                 // to: broadcast to all connected agents
			"cross_project_impact",
			string(payload),
			primaryRepoID,
		)
	}
}

// notifyIntraProjectImpact fires targeted messages to agents that are directly
// affected by the re-parsed file within the same project. If any in-progress
// task has linked nodes that live in the changed file, the task's assigned
// agent receives a "task_node_changed" message.
//
// Fail-silent: any error is ignored so the watcher loop is never interrupted.
func (w *Watcher) notifyIntraProjectImpact(changedFile string) {
	if w.store == nil {
		return
	}

	primaryRepoID := w.graph.RepoID()

	// Compute repo-relative path for cleaner payloads.
	relFile := changedFile
	if root := w.graph.Root(); root != "" {
		prefix := root
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if strings.HasPrefix(changedFile, prefix) {
			relFile = changedFile[len(prefix):]
		}
	}

	// Collect changed node IDs for task-node probes.
	changedNodes := w.graph.NodesForFile(changedFile)
	changedNodeIDs := make(map[string]struct{}, len(changedNodes))
	for _, n := range changedNodes {
		changedNodeIDs[string(n.ID)] = struct{}{}
	}

	// ── Task-node probe ──────────────────────────────────────────────────
	if len(changedNodeIDs) == 0 {
		return
	}
	tasks, terr := w.store.GetPendingTasks("", "")
	if terr != nil {
		return
	}
	for _, task := range tasks {
		if task.AssignedTo == "" {
			continue
		}
		var hitNodes []string
		for _, nid := range task.LinkedNodes {
			if _, ok := changedNodeIDs[nid]; ok {
				hitNodes = append(hitNodes, nid)
			}
		}
		if len(hitNodes) == 0 {
			continue
		}
		payload, merr := json.Marshal(map[string]interface{}{
			"changed_file":   relFile,
			"project":        primaryRepoID,
			"task_id":        task.ID,
			"task_title":     task.Title,
			"affected_nodes": hitNodes,
			"hint": fmt.Sprintf(
				"Task %q: %d linked node(s) in %q were just modified.",
				task.Title, len(hitNodes), relFile,
			),
		})
		if merr != nil {
			continue
		}
		_, _ = w.store.SendMessage(
			"synapses-watcher",
			task.AssignedTo,
			"task_node_changed",
			string(payload),
			primaryRepoID,
		)
	}
}

// repoIDOfNodeID extracts the repoID prefix from a NodeID of the form
// "repoID::file::name". Returns "" for malformed IDs.
func repoIDOfNodeID(nodeID string) string {
	if idx := strings.Index(nodeID, "::"); idx >= 0 {
		return nodeID[:idx]
	}
	return ""
}

// dedup returns a copy of ss with duplicate strings removed, preserving order.
func dedup(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := ss[:0:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// shouldSkipDir matches the same exclusion list used by the parser walker.
func shouldSkipDir(name string) bool {
	switch name {
	// --- Dependency managers ---
	case "node_modules", // Node.js
		"vendor",           // Go / PHP / Ruby
		"bower_components", // Bower (legacy JS)
		"jspm_packages",    // JSPM
		"Pods":             // iOS CocoaPods
		return true

	// --- Build / compiled output ---
	case "dist",
		"build",
		"out",
		"target", // Rust (Cargo), Java (Maven)
		"obj",    // C/C++
		"storybook-static",
		"coverage",
		".nyc_output":
		return true

	// --- Temporary / cache ---
	case "tmp",
		"temp",
		"cache",
		"__pycache__": // Python bytecode cache
		return true

	// --- Generated code ---
	case "generated",
		"gen",
		"__generated__",
		"__mocks__": // Jest mock directories
		return true

	// --- Third-party bundled sources ---
	case "third_party",
		"vendor_ruby",
		"testdata": // Go test fixtures rarely need indexing
		return true

	// --- Synapses-specific ---
	case "tmp_repos": // synapses-fine-distilling: cloned training repos — never index
		return true
	}

	// Skip all hidden directories (e.g. .next, .nuxt, .turbo, .cache, .gradle).
	return strings.HasPrefix(name, ".")
}
