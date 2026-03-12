// Package watcher implements incremental graph updates via filesystem events.
// When a source file changes, only that file's nodes and edges are removed and
// re-parsed — the rest of the graph is untouched. This keeps context fresh
// without the cost of a full re-index on every save.
package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/store"
)

const debounceDelay = 150 * time.Millisecond
const changeLogCap = 50

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
type Watcher struct {
	fw          *fsnotify.Watcher
	graph       *graph.Graph
	walker      *parser.Walker
	store       *store.Store           // may be nil — cache update is best-effort
	cfg         *config.Config         // may be nil — violation checking is best-effort
	brainClient interface{}            // *brain.Client — set via SetBrainClient; nil if brain not configured
	pktInval    PacketCacheInvalidator // set via SetPacketInvalidator; may be nil
	cfgHandler  ConfigChangeHandler    // called when synapses.json changes; may be nil
	configPath  string                 // absolute path to synapses.json (set by Start)

	mu      sync.Mutex
	timers  map[string]*time.Timer // debounce timers keyed by absolute file path
	stopCh  chan struct{}
	stopped bool

	changeMu  sync.RWMutex
	changeLog []ChangeEvent // bounded log of recent file events (max changeLogCap)
}

// New creates a Watcher. store may be nil; if provided the cache is updated
// after each incremental re-parse.
func New(g *graph.Graph, w *parser.Walker, st *store.Store) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}
	return &Watcher{
		fw:     fw,
		graph:  g,
		walker: w,
		store:  st,
		timers: make(map[string]*time.Timer),
		stopCh: make(chan struct{}),
	}, nil
}

// SetConfig wires the project config into the watcher so that rule violations
// are checked after each incremental re-parse and emitted to the event log.
// Must be called before Start. cfg may be nil to disable violation checking.
func (w *Watcher) SetConfig(cfg *config.Config) {
	w.cfg = cfg
}

// SetBrainClient wires a *brain.Client into the watcher so that changed files
// are incrementally ingested to the intelligence sidecar. Using interface{}
// avoids an import cycle (brain imports only stdlib, not watcher).
func (w *Watcher) SetBrainClient(bc interface{}) {
	w.brainClient = bc
}

// SetPacketInvalidator wires a PacketCacheInvalidator (typically the MCP Server)
// into the watcher. On every file change the packet cache is cleared so that
// stale brain context packets are not returned to agents.
func (w *Watcher) SetPacketInvalidator(pi PacketCacheInvalidator) {
	w.pktInval = pi
}

// SetConfigChangeHandler registers a callback that is invoked whenever
// synapses.json changes on disk. The callback receives the newly parsed config.
// This enables hot-reload of brain/scout client settings without restarting.
func (w *Watcher) SetConfigChangeHandler(fn ConfigChangeHandler) {
	w.cfgHandler = fn
}

// Start begins watching root recursively. It returns immediately; the event
// loop runs in a background goroutine. Call Stop to shut it down.
func (w *Watcher) Start(root string) error {
	// Record the config file path so handleEvent can detect changes to it.
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

	go w.loop(root)
	return nil
}

// Stop shuts down the watcher and releases resources.
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.stopped = true
	close(w.stopCh)
	w.fw.Close()

	// Cancel any pending debounce timers.
	for _, t := range w.timers {
		t.Stop()
	}
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

// loop is the background event processing goroutine.
func (w *Watcher) loop(root string) {
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
			fmt.Fprintf(os.Stderr, "synapses/watcher: %v\n", err)
		}
	}
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
		w.graph.RemoveFile(path)
		w.graph.RemoveCallSitesForFile(path)
		w.graph.InvalidateCache()
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
		w.debounce(path, root)
	}
}

// debounce coalesces rapid write events for the same file into a single
// re-parse after debounceDelay of silence.
func (w *Watcher) debounce(path, root string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.timers[path]; ok {
		t.Reset(debounceDelay)
		return
	}

	w.timers[path] = time.AfterFunc(debounceDelay, func() {
		w.mu.Lock()
		delete(w.timers, path)
		w.mu.Unlock()

		w.reparseFile(path, root)
	})
}

// debounceConfigReload coalesces rapid writes to synapses.json into a single
// config reload after debounceDelay of silence. Uses the same timer map as
// code file debouncing since the key space is separate.
func (w *Watcher) debounceConfigReload(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if t, ok := w.timers[path]; ok {
		t.Reset(debounceDelay)
		return
	}

	w.timers[path] = time.AfterFunc(debounceDelay, func() {
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
		fmt.Fprintf(os.Stderr, "synapses/watcher: reload %s: %v\n", configPath, err)
		return
	}
	// Update the watcher's own violation-checking config so future file changes
	// use the freshly loaded rules.
	w.cfg = newCfg
	fmt.Fprintf(os.Stderr, "synapses/watcher: config reloaded from %s\n", configPath)
	if w.cfgHandler != nil {
		w.cfgHandler(newCfg)
	}
}

// reparseFile removes stale nodes for path and re-parses it into the graph.
func (w *Watcher) reparseFile(path, _ string) {
	// Snapshot counts before mutation for ChangeEvent delta.
	nodesBefore := w.countNodesForFile(path)
	edgesBefore := w.graph.EdgeCount()

	// Snapshot stable UUIDs before removing nodes so they can be migrated to
	// the re-parsed nodes (preserving cross-project references across renames).
	w.graph.SnapshotFileStableIDs(path)

	// Remove stale graph data and call sites for this file before re-parsing.
	w.graph.RemoveFile(path)
	w.graph.RemoveCallSitesForFile(path)

	if err := w.walker.ParseFile(w.graph, path); err != nil {
		fmt.Fprintf(os.Stderr, "synapses/watcher: re-parse %s: %v\n", path, err)
		w.graph.ClearFileSnapshot(path)
		return
	}

	// Migrate stable UUIDs: reuse old UUIDs for nodes that survived the re-parse
	// (same identity) so cross-project references remain valid.
	for _, n := range w.graph.NodesForFile(path) {
		w.graph.MigrateStableID(n)
	}
	w.graph.ClearFileSnapshot(path)

	resolver.ResolveCallEdges(w.graph)
	resolver.ResolveImplementsEdges(w.graph)
	w.graph.InvalidateCache()

	// Rebuild the columnar GraphIndex asynchronously so BFS reads pick up the
	// latest graph state without blocking the watcher loop.
	// Snapshot is saved by main.go via the store; watcher discards the bytes.
	go func() { w.graph.RebuildIndex() }() //nolint:errcheck
	if w.pktInval != nil {
		w.pktInval.InvalidatePacketCacheForFile(path)
	}

	// Record change event with delta counts.
	nodesAfter := w.countNodesForFile(path)
	edgesAfter := w.graph.EdgeCount()
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

	fmt.Fprintf(os.Stderr, "synapses/watcher: updated %s\n", path)
	w.persistAsync(path)

	// Fire-and-forget: ingest changed nodes to brain for semantic summarization.
	if w.brainClient != nil {
		go w.ingestToBrain(path)
	}
}

// checkViolations runs the rule engine against edges touching path and emits
// events for violations that were not already present in the log.
func (w *Watcher) checkViolations(path string) {
	if w.cfg == nil || w.store == nil || len(w.cfg.Rules) == 0 {
		return
	}

	// Snapshot existing violation IDs for this file BEFORE logging new ones,
	// so we can detect which violations are genuinely new.
	existingIDs, err := w.store.ViolationIDsForFile(path)
	if err != nil {
		existingIDs = make(map[string]struct{}) // safe fallback: treat all as new
	}

	violations := w.cfg.CheckViolationsForFile(w.graph, path)
	if len(violations) == 0 {
		return
	}

	// Persist to violation_log (upsert — safe to call repeatedly).
	if err := w.store.LogViolations(violations); err != nil {
		fmt.Fprintf(os.Stderr, "synapses/watcher: log violations: %v\n", err)
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
func (w *Watcher) countNodesForFile(path string) int {
	count := 0
	for _, n := range w.graph.AllNodes() {
		if n.File == path {
			count++
		}
	}
	return count
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

// persistAsync saves the current graph, call sites, and file mtime to the
// SQLite cache in a goroutine. The changedFile path is used to update the
// file_hashes table so the next smart-reindex skips this file correctly.
// Failures are logged but do not interrupt the watcher.
func (w *Watcher) persistAsync(changedFile string) {
	if w.store == nil {
		return
	}
	sites := w.graph.PeekCallSites()
	mtime := time.Now().UnixNano()
	go func() {
		if err := w.store.SaveGraph(w.graph); err != nil {
			fmt.Fprintf(os.Stderr, "synapses/watcher: cache save: %v\n", err)
		}
		if err := w.store.SaveCallSites(sites); err != nil {
			fmt.Fprintf(os.Stderr, "synapses/watcher: save call sites: %v\n", err)
		}
		if changedFile != "" {
			if err := w.store.UpsertFileMtime(changedFile, mtime); err != nil {
				fmt.Fprintf(os.Stderr, "synapses/watcher: update mtime %s: %v\n", changedFile, err)
			}
		}
	}()
}

// ingestToBrain sends nodes from the re-parsed file to the intelligence sidecar
// so its semantic summaries stay current. After ingest, schedules a delayed
// write-back to fetch the generated summaries and store them as annotations.
// Runs in a goroutine; all errors are silently discarded (fail-silent contract).
func (w *Watcher) ingestToBrain(path string) {
	bc, ok := w.brainClient.(*brain.Client)
	if !ok || bc == nil {
		return
	}
	nodes := w.graph.NodesForFile(path)
	for _, n := range nodes {
		if string(n.Type) == "package" || string(n.Type) == "file" {
			continue
		}
		code := ""
		if sig, ok := n.Metadata["signature"]; ok && sig != "" {
			code = sig
		}
		if doc, ok := n.Metadata["doc"]; ok && doc != "" && code != "" {
			code = "// " + doc + "\n" + code
		}
		bc.Ingest(context.Background(), brain.IngestRequest{
			NodeID:   string(n.ID),
			NodeName: n.Name,
			NodeType: string(n.Type),
			Package:  n.Package,
			Code:     code,
		})
	}

	// Delayed write-back: fetch summaries after the brain has processed the ingest queue.
	if w.store != nil {
		go func(nodeList []*graph.Node) {
			time.Sleep(15 * time.Second)
			for _, n := range nodeList {
				if string(n.Type) == "package" || string(n.Type) == "file" {
					continue
				}
				summary := bc.GetSummary(context.Background(), string(n.ID))
				if summary != "" {
					_, _, _ = w.store.AddAnnotationIfNew(string(n.ID), "brain", summary, 60*time.Second)
				}
			}
		}(nodes)
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
			fmt.Fprintf(os.Stderr, "synapses/watcher: marshal cross_project_impact: %v\n", err)
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
// affected by the re-parsed file within the same project. Two probes run:
//
//  1. Claimed-scope probe: if the changed file is inside an agent's claimed
//     scope, that agent receives a "scope_change_alert" message so it knows
//     its active work area was touched.
//
//  2. Task-node probe: if any in-progress task has linked nodes that live in
//     the changed file, the task's assigned agent receives a "task_node_changed"
//     message.
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

	// Collect changed symbol names and node IDs for payloads.
	changedNodes := w.graph.NodesForFile(changedFile)
	changedNames := make([]string, 0, len(changedNodes))
	changedNodeIDs := make(map[string]struct{}, len(changedNodes))
	for _, n := range changedNodes {
		changedNames = append(changedNames, n.Name)
		changedNodeIDs[string(n.ID)] = struct{}{}
	}
	symbols := dedup(changedNames)

	// ── 1. Claimed-scope probe ─────────────────────────────────────────────
	claims, err := w.store.GetAllClaims()
	if err == nil && len(claims) > 0 {
		notifiedAgent := make(map[string]struct{})
		for _, claim := range claims {
			if !fileInScope(relFile, claim.Scope) {
				continue
			}
			if _, seen := notifiedAgent[claim.AgentID]; seen {
				continue
			}
			notifiedAgent[claim.AgentID] = struct{}{}

			payload, merr := json.Marshal(map[string]interface{}{
				"changed_file":    relFile,
				"project":         primaryRepoID,
				"claimed_scope":   claim.Scope,
				"changed_symbols": symbols,
				"hint": fmt.Sprintf(
					"File %q (inside your claimed scope %q) was just modified. %d symbol(s) changed.",
					relFile, claim.Scope, len(symbols),
				),
			})
			if merr != nil {
				continue
			}
			_, _ = w.store.SendMessage(
				"synapses-watcher",
				claim.AgentID,
				"scope_change_alert",
				string(payload),
				primaryRepoID,
			)
		}
	}

	// ── 2. Task-node probe ─────────────────────────────────────────────────
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

// fileInScope reports whether relFile is covered by the given claim scope.
// scope may be an exact relative file path or a directory prefix.
func fileInScope(relFile, scope string) bool {
	if relFile == scope {
		return true
	}
	dir := scope
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	return strings.HasPrefix(relFile, dir)
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
	case "vendor", "node_modules", "dist", "build", ".git",
		".idea", ".vscode", "__pycache__", "testdata":
		return true
	}
	if len(name) > 0 && name[0] == '.' {
		return true
	}
	return false
}
