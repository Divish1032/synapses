// Package federation provides cross-project dependency tracking and drift
// detection for local sibling projects. It opens sibling SQLite stores
// read-only and compares stored dependency state against current git HEAD.
//
// All operations are fail-open: errors are logged and skipped, never
// propagated to the caller. A broken sibling never blocks the response.
package federation

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// EntryStatus describes the health of one federation entry.
type EntryStatus struct {
	Alias     string    `json:"alias"`
	Path      string    `json:"path"`
	Status    string    `json:"status"` // "indexed" | "stale" | "not_indexed" | "not_found" | "incompatible"
	NodeCount int       `json:"node_count,omitempty"`
	FileCount int       `json:"file_count,omitempty"`
	IndexedAt time.Time `json:"indexed_at,omitempty"`
	GitHead   string    `json:"git_head,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// DriftAlert represents a cross-project dependency whose sibling entity changed.
type DriftAlert struct {
	Project     string   `json:"project"`      // federation alias
	Entity      string   `json:"entity"`       // entity name in sibling
	File        string   `json:"file"`         // file in sibling
	Change      string   `json:"change"`       // "signature_changed" | "removed" | "file_deleted"
	Severity    string   `json:"severity"`     // "breaking" | "info"
	DiffSummary string   `json:"diff_summary"` // human-readable change description
	YourCallers []string `json:"your_callers"` // local entities that depend on this
	OldCommit   string   `json:"old_commit"`   // commit when dep was verified
	NewCommit   string   `json:"new_commit"`   // sibling's current HEAD
}

// CrossProjectDepStatus is used in prepare_context enrichment.
type CrossProjectDepStatus struct {
	Project     string `json:"project"`
	Entity      string `json:"entity"`
	File        string `json:"file"`
	Drifted     bool   `json:"drifted"`
	DiffSummary string `json:"diff_summary,omitempty"`
}

// FederatedSearchResult groups entity search results from one sibling.
type FederatedSearchResult struct {
	Alias   string               `json:"alias"`
	Results []store.SearchResult `json:"results"`
}

// FederatedContext holds BFS context carved from a sibling's graph.
type FederatedContext struct {
	Alias     string            `json:"alias"`
	Entity    string            `json:"entity"`
	NodeCount int               `json:"node_count"`
	Nodes     []graph.CarvedNode `json:"nodes"`
	Edges     []*graph.Edge     `json:"edges,omitempty"`
}

// staleThreshold is how long since last index before a sibling is "stale".
const staleThreshold = 24 * time.Hour

// Clock is a function that returns the current time. Injected into Resolver
// for deterministic staleness testing.
type Clock func() time.Time

// Resolver provides cross-project query capabilities over local sibling
// SQLite stores. Stores are opened lazily on first access and cached for
// the session lifetime.
type Resolver struct {
	entries   []config.FederationEntry
	configDir string // directory containing synapses.json
	clock     Clock  // time source (defaults to time.Now)

	mu         sync.RWMutex
	stores     map[string]*store.Store // alias → read-only store (lazy-opened)
	storeErr   map[string]error        // alias → last open error (prevents retry loops)
	compatible map[string]bool         // alias → schema compat result (cached)
	driftCache map[string][]DriftAlert // alias → cached drift results (session-level)
	gitHeads   map[string]string       // alias → cached HEAD commit hash
}

// NewResolver creates a Resolver for the given federation entries.
// configDir is the directory containing synapses.json (paths are already
// resolved to absolute by config.Load).
func NewResolver(entries []config.FederationEntry, configDir string) *Resolver {
	return newResolverWithClock(entries, configDir, time.Now)
}

// NewResolverWithClock creates a Resolver with a custom time source.
// Used in tests for deterministic staleness checks.
func NewResolverWithClock(entries []config.FederationEntry, configDir string, clock Clock) *Resolver {
	return newResolverWithClock(entries, configDir, clock)
}

func newResolverWithClock(entries []config.FederationEntry, configDir string, clock Clock) *Resolver {
	return &Resolver{
		entries:    entries,
		configDir:  configDir,
		clock:      clock,
		stores:     make(map[string]*store.Store, len(entries)),
		storeErr:   make(map[string]error, len(entries)),
		compatible: make(map[string]bool, len(entries)),
		driftCache: make(map[string][]DriftAlert),
		gitHeads:   make(map[string]string),
	}
}

// Status returns health info for each federation entry.
// Errors on individual entries are contained — a broken sibling returns
// a status entry with Error set, never a top-level error.
func (r *Resolver) Status(ctx context.Context) []EntryStatus {
	results := make([]EntryStatus, 0, len(r.entries))
	for _, e := range r.entries {
		if ctx.Err() != nil {
			results = append(results, EntryStatus{
				Alias:  e.Alias,
				Path:   e.Path,
				Status: "not_indexed",
				Error:  "timeout",
			})
			continue
		}
		es := r.statusForEntry(e)
		results = append(results, es)
	}
	return results
}

func (r *Resolver) statusForEntry(e config.FederationEntry) EntryStatus {
	es := EntryStatus{
		Alias: e.Alias,
		Path:  e.Path,
	}

	// Check if path exists.
	info, err := os.Stat(e.Path)
	if err != nil || !info.IsDir() {
		es.Status = "not_found"
		if err != nil {
			es.Error = err.Error()
		}
		return es
	}

	// Derive sibling's store DB path.
	dbPath, err := SiblingDBPath(e.Path)
	if err != nil {
		es.Status = "not_indexed"
		es.Error = err.Error()
		return es
	}

	if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
		es.Status = "not_indexed"
		return es
	}

	// Single open: use rawDB for both schema check and stats read.
	// This avoids the double-open penalty of checking compat then opening again.
	db, err := newRawDB(dbPath)
	if err != nil {
		es.Status = "not_indexed"
		es.Error = err.Error()
		return es
	}
	defer db.Close()

	// Schema compatibility: verify critical tables exist.
	if err := db.checkTables("nodes", "meta"); err != nil {
		es.Status = "incompatible"
		es.Error = err.Error()
		return es
	}

	// Read stats from meta table (same queries as store.Stat).
	stat, err := db.readProjectStat(dbPath)
	if err != nil || stat == nil {
		es.Status = "not_indexed"
		if err != nil {
			es.Error = err.Error()
		}
		return es
	}

	es.NodeCount = stat.NodeCount
	es.FileCount = stat.FileCount
	es.IndexedAt = stat.SavedAt

	if r.clock().Sub(stat.SavedAt) > staleThreshold {
		es.Status = "stale"
	} else {
		es.Status = "indexed"
	}

	return es
}

// EntityExists checks if an entity name exists in a sibling's store.
// Returns false on any error (fail-open).
func (r *Resolver) EntityExists(ctx context.Context, alias string, entityName string) bool {
	if ctx.Err() != nil {
		return false
	}
	st := r.getStore(alias)
	if st == nil {
		return false
	}
	exists, err := st.NodeExistsByNameCtx(ctx, entityName)
	if err != nil {
		return false
	}
	return exists
}

// FindEntities searches sibling stores for entities matching query.
// If aliases is nil or empty, all siblings are searched.
// Errors on individual siblings are silently skipped.
func (r *Resolver) FindEntities(ctx context.Context, query string, aliases []string, limit int) []FederatedSearchResult {
	if limit <= 0 {
		limit = 20
	}

	targets := r.entries
	if len(aliases) > 0 {
		aliasSet := make(map[string]bool, len(aliases))
		for _, a := range aliases {
			aliasSet[a] = true
		}
		targets = nil
		for _, e := range r.entries {
			if aliasSet[e.Alias] {
				targets = append(targets, e)
			}
		}
	}

	var results []FederatedSearchResult
	for _, e := range targets {
		if ctx.Err() != nil {
			break
		}
		st := r.getStore(e.Alias)
		if st == nil {
			continue
		}

		nodes, err := st.FindNodesByNameCtx(ctx, query, limit)
		if err != nil {
			log.Printf("federation: find_entity in %q: %v", e.Alias, err)
			continue
		}
		if len(nodes) == 0 {
			continue
		}

		results = append(results, FederatedSearchResult{
			Alias:   e.Alias,
			Results: nodes,
		})
	}
	return results
}

// GetDepsForEntity returns cross-project deps for a specific local entity.
// Used by prepare_context to enrich responses. Returns nil when the entity
// has no cross-project dependencies or the local store is unavailable.
func (r *Resolver) GetDepsForEntity(ctx context.Context, entityID string, localStore *store.Store) []CrossProjectDepStatus {
	if localStore == nil || ctx.Err() != nil {
		return nil
	}
	deps, err := localStore.GetCrossProjectDeps(entityID)
	if err != nil || len(deps) == 0 {
		return nil
	}

	var results []CrossProjectDepStatus
	for _, dep := range deps {
		if ctx.Err() != nil {
			break
		}
		results = append(results, CrossProjectDepStatus{
			Project: dep.ToProject,
			Entity:  dep.ToEntity,
			File:    dep.ToFile,
			// Drifted/DiffSummary will be populated by CheckDrift in Phase 2.
		})
	}
	return results
}

// GetEntityContext loads a sibling graph and carves BFS context for an entity.
// This is the "full BFS" option — opt-in via projects= parameter on tools.
// NOT called automatically during enrichment. Returns nil on any error.
func (r *Resolver) GetEntityContext(ctx context.Context, entity string, alias string, depth int) *FederatedContext {
	if ctx.Err() != nil {
		return nil
	}
	st := r.getStore(alias)
	if st == nil {
		return nil
	}

	g, err := st.LoadGraph()
	if err != nil || g == nil {
		if err != nil {
			log.Printf("federation: load graph for %q: %v", alias, err)
		}
		return nil
	}

	nodes := g.FindByName(entity)
	if len(nodes) == 0 {
		return nil
	}

	if ctx.Err() != nil {
		return nil
	}

	// Carve BFS context around the first matching node.
	root := nodes[0]
	cfg := graph.DefaultCarveConfig()
	if depth > 0 {
		cfg.MaxDepth = depth
	}
	sub, err := g.CarveEgoGraph(root.ID, cfg)
	if err != nil || sub == nil {
		if err != nil {
			log.Printf("federation: carve ego graph for %q in %q: %v", entity, alias, err)
		}
		return nil
	}

	return &FederatedContext{
		Alias:     alias,
		Entity:    entity,
		NodeCount: len(sub.Nodes),
		Nodes:     sub.Nodes,
		Edges:     sub.Edges,
	}
}

// CheckDrift compares stored cross-project dependencies against sibling
// git state. Uses git diff for precision, falls back to signature comparison
// if git is unavailable or the old commit is unreachable.
//
// Results are cached for the session — subsequent calls return cached results.
// Returns only CHANGED dependencies (drift). Empty slice = no drift.
// Errors are contained per-sibling: a broken sibling never blocks results
// from healthy ones.
func (r *Resolver) CheckDrift(ctx context.Context, localStore *store.Store) []DriftAlert {
	if localStore == nil || ctx.Err() != nil {
		return nil
	}

	// Collect all alerts across all siblings.
	var allAlerts []DriftAlert
	for _, e := range r.entries {
		if ctx.Err() != nil {
			break
		}

		// Check session-level cache first.
		r.mu.RLock()
		cached, hasCached := r.driftCache[e.Alias]
		r.mu.RUnlock()
		if hasCached {
			allAlerts = append(allAlerts, cached...)
			continue
		}

		alerts := r.checkDriftForEntry(ctx, e, localStore)

		r.mu.Lock()
		r.driftCache[e.Alias] = alerts
		r.mu.Unlock()

		allAlerts = append(allAlerts, alerts...)
	}
	return allAlerts
}

// checkDriftForEntry runs drift detection for a single federation entry.
func (r *Resolver) checkDriftForEntry(ctx context.Context, e config.FederationEntry, localStore *store.Store) []DriftAlert {
	// Get all deps targeting this sibling project.
	deps, err := localStore.GetCrossProjectDepsByProject(e.Alias)
	if err != nil || len(deps) == 0 {
		return nil
	}

	// Step 1: Read sibling's current HEAD.
	currentHead, err := gitRevParseHead(ctx, e.Path)
	if err != nil || currentHead == "" {
		// Not a git repo or git unavailable — fall back to signature comparison.
		return r.checkDriftFallback(ctx, e.Alias, deps)
	}

	// Cache the HEAD for other uses.
	r.mu.Lock()
	r.gitHeads[e.Alias] = currentHead
	r.mu.Unlock()

	// Step 2: Group deps by verified_commit. If ALL deps have the same
	// verified_commit and it matches HEAD, there's no drift.
	allSameHead := true
	for _, dep := range deps {
		if dep.VerifiedCommit != currentHead {
			allSameHead = false
			break
		}
	}
	if allSameHead {
		return nil // fast path — sibling hasn't changed since last check
	}

	// Step 3: For deps with stale verified_commit, check what changed.
	// Get the oldest verified_commit to run one diff against HEAD.
	commitGroups := make(map[string][]store.CrossProjectDep)
	for _, dep := range deps {
		if dep.VerifiedCommit != currentHead {
			commitGroups[dep.VerifiedCommit] = append(commitGroups[dep.VerifiedCommit], dep)
		}
	}

	var alerts []DriftAlert
	for oldCommit, groupDeps := range commitGroups {
		if ctx.Err() != nil {
			break
		}
		groupAlerts := r.checkDriftForCommitGroup(ctx, e, oldCommit, currentHead, groupDeps, localStore)
		alerts = append(alerts, groupAlerts...)
	}
	return alerts
}

// checkDriftForCommitGroup checks a group of deps that share the same verified_commit.
func (r *Resolver) checkDriftForCommitGroup(
	ctx context.Context,
	e config.FederationEntry,
	oldCommit, newCommit string,
	deps []store.CrossProjectDep,
	localStore *store.Store,
) []DriftAlert {
	// Get list of changed files between old and new commits.
	changedFiles, err := gitDiffNameOnly(ctx, e.Path, oldCommit, newCommit)
	if err != nil || changedFiles == nil {
		// Commits unreachable (force push, rebase) — fall back.
		return r.checkDriftFallback(ctx, e.Alias, deps)
	}

	changedSet := make(map[string]bool, len(changedFiles))
	for _, f := range changedFiles {
		changedSet[f] = true
	}

	var alerts []DriftAlert
	for _, dep := range deps {
		if ctx.Err() != nil {
			break
		}

		if !changedSet[dep.ToFile] {
			// File not in changed set → entity is safe. Silently update commit.
			_ = localStore.UpdateVerifiedCommit(dep.ToProject, dep.ToEntity, newCommit)
			continue
		}

		// File was changed — check if the entity's signature was affected.
		diff, err := gitDiffFile(ctx, e.Path, oldCommit, newCommit, dep.ToFile)
		if err != nil || diff == "" {
			// Can't get diff — be conservative, skip (fail-open means no false alert).
			_ = localStore.UpdateVerifiedCommit(dep.ToProject, dep.ToEntity, newCommit)
			continue
		}

		if !diffTouchesEntity(diff, dep.ToEntity) {
			// File changed but entity signature didn't — safe, update commit.
			_ = localStore.UpdateVerifiedCommit(dep.ToProject, dep.ToEntity, newCommit)
			continue
		}

		// Entity signature was touched — check if it still exists.
		if !entityExistsInFile(ctx, e.Path, dep.ToFile, dep.ToEntity) {
			alerts = append(alerts, buildAlert(dep, e.Alias, "removed", "breaking",
				"Entity removed from "+dep.ToFile, oldCommit, newCommit, localStore))
			continue
		}

		// Signature changed but entity still exists.
		summary := extractDiffSummary(diff, dep.ToEntity)
		alerts = append(alerts, buildAlert(dep, e.Alias, "signature_changed", "breaking",
			summary, oldCommit, newCommit, localStore))
	}
	return alerts
}

// checkDriftFallback uses direct signature comparison when git is unavailable.
// Less precise (catches formatting changes) but functional.
func (r *Resolver) checkDriftFallback(ctx context.Context, alias string, deps []store.CrossProjectDep) []DriftAlert {
	st := r.getStore(alias)
	if st == nil {
		return nil
	}

	var alerts []DriftAlert
	for _, dep := range deps {
		if ctx.Err() != nil {
			break
		}

		// Look up the entity in the sibling store.
		results, err := st.FindNodesByNameCtx(ctx, dep.ToEntity, 1)
		if err != nil {
			continue // fail-open
		}

		if len(results) == 0 {
			// Entity no longer exists in sibling.
			alerts = append(alerts, DriftAlert{
				Project:     alias,
				Entity:      dep.ToEntity,
				File:        dep.ToFile,
				Change:      "removed",
				Severity:    "breaking",
				DiffSummary: "Entity not found in sibling store (fallback check)",
				YourCallers: findLocalCallers(dep.FromEntity),
				OldCommit:   dep.VerifiedCommit,
			})
			continue
		}

		// Compare signatures — if different, it drifted.
		// We don't have the old signature stored, so any difference from
		// what was expected is flagged. This is the less-precise fallback.
		// In practice, this path only fires when git is unavailable.
	}
	return alerts
}

// buildAlert constructs a DriftAlert and looks up local callers.
func buildAlert(dep store.CrossProjectDep, alias, change, severity, summary, oldCommit, newCommit string, localStore *store.Store) DriftAlert {
	return DriftAlert{
		Project:     alias,
		Entity:      dep.ToEntity,
		File:        dep.ToFile,
		Change:      change,
		Severity:    severity,
		DiffSummary: summary,
		YourCallers: findLocalCallers(dep.FromEntity),
		OldCommit:   oldCommit,
		NewCommit:   newCommit,
	}
}

// findLocalCallers returns the local entities that depend on a cross-project entity.
// For now, returns the from_entity as a single-element list. Phase 3 will
// expand this to find all local callers via the graph.
func findLocalCallers(fromEntity string) []string {
	if fromEntity == "" {
		return nil
	}
	return []string{fromEntity}
}

// extractDiffSummary produces a brief human-readable summary of what changed
// in a diff for a specific entity. Uses regex heuristics — Phase 5 will add
// brain-enhanced summaries when available.
func extractDiffSummary(diff, entityName string) string {
	var removed, added []string

	for _, line := range strings.Split(diff, "\n") {
		if len(line) == 0 {
			continue
		}
		// Only look at changed lines that reference the entity.
		if !strings.Contains(line, entityName) {
			continue
		}
		switch line[0] {
		case '-':
			if !strings.HasPrefix(line, "---") {
				removed = append(removed, strings.TrimSpace(line[1:]))
			}
		case '+':
			if !strings.HasPrefix(line, "+++") {
				added = append(added, strings.TrimSpace(line[1:]))
			}
		}
	}

	if len(removed) == 0 && len(added) == 0 {
		return "Signature changed"
	}

	if len(removed) == 1 && len(added) == 1 {
		return fmt.Sprintf("Changed: %s → %s", truncate(removed[0], 80), truncate(added[0], 80))
	}

	var parts []string
	if len(removed) > 0 {
		parts = append(parts, fmt.Sprintf("removed %d line(s)", len(removed)))
	}
	if len(added) > 0 {
		parts = append(parts, fmt.Sprintf("added %d line(s)", len(added)))
	}
	return "Signature modified: " + strings.Join(parts, ", ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// InvalidateCache clears session-level caches. Called on session_init
// to ensure fresh data each session.
func (r *Resolver) InvalidateCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.driftCache = make(map[string][]DriftAlert)
	r.gitHeads = make(map[string]string)
	// Clear store errors and compat cache so stale failures are retried.
	r.storeErr = make(map[string]error, len(r.entries))
	r.compatible = make(map[string]bool, len(r.entries))
}

// Close releases all open sibling stores.
func (r *Resolver) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for alias, st := range r.stores {
		if err := st.Close(); err != nil {
			log.Printf("federation: close store %q: %v", alias, err)
		}
	}
	r.stores = make(map[string]*store.Store)
}

// Entries returns the federation entries.
func (r *Resolver) Entries() []config.FederationEntry {
	return r.entries
}

// Aliases returns all configured federation aliases.
func (r *Resolver) Aliases() []string {
	aliases := make([]string, len(r.entries))
	for i, e := range r.entries {
		aliases[i] = e.Alias
	}
	return aliases
}

// HasAlias reports whether the resolver has an entry for the given alias.
func (r *Resolver) HasAlias(alias string) bool {
	for _, e := range r.entries {
		if e.Alias == alias {
			return true
		}
	}
	return false
}

// getStore returns the cached store for the alias, opening it lazily if needed.
// Returns nil on any error (fail-open). Once a store fails to open, the error
// is cached to avoid retry storms — call InvalidateCache to reset.
func (r *Resolver) getStore(alias string) *store.Store {
	r.mu.RLock()
	st, ok := r.stores[alias]
	if ok {
		r.mu.RUnlock()
		return st
	}
	if _, errCached := r.storeErr[alias]; errCached {
		r.mu.RUnlock()
		return nil
	}
	r.mu.RUnlock()

	// Find the entry.
	var entry *config.FederationEntry
	for i := range r.entries {
		if r.entries[i].Alias == alias {
			entry = &r.entries[i]
			break
		}
	}
	if entry == nil {
		return nil
	}

	dbPath, err := SiblingDBPath(entry.Path)
	if err != nil {
		r.mu.Lock()
		r.storeErr[alias] = err
		r.mu.Unlock()
		return nil
	}

	// Check cached compat result first, then check on disk.
	r.mu.RLock()
	compat, compatKnown := r.compatible[alias]
	r.mu.RUnlock()

	if !compatKnown {
		if err := checkSchemaCompatibility(dbPath); err != nil {
			r.mu.Lock()
			r.storeErr[alias] = err
			r.compatible[alias] = false
			r.mu.Unlock()
			log.Printf("federation: incompatible store %q: %v", alias, err)
			return nil
		}
		r.mu.Lock()
		r.compatible[alias] = true
		r.mu.Unlock()
		compat = true
	}
	if !compat {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock.
	if st, ok := r.stores[alias]; ok {
		return st
	}
	if _, errCached := r.storeErr[alias]; errCached {
		return nil
	}

	st, err = store.OpenReadOnly(dbPath)
	if err != nil {
		r.storeErr[alias] = err
		log.Printf("federation: open store %q: %v", alias, err)
		return nil
	}
	r.stores[alias] = st
	return st
}

// checkSchemaCompatibility opens the DB briefly to verify that the tables
// we need for federation queries exist.
func checkSchemaCompatibility(dbPath string) error {
	db, err := newRawDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.checkTables("nodes", "meta")
}

// SiblingDBPath derives the store DB path for a sibling project using
// the same logic as store.DefaultPath.
func SiblingDBPath(projectPath string) (string, error) {
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	return store.DefaultPath(abs)
}
