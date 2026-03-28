// Package federation provides cross-project dependency tracking and drift
// detection for local sibling projects. It opens sibling SQLite stores
// read-only and compares stored dependency state against current git HEAD.
//
// All operations are fail-open: errors are logged and skipped, never
// propagated to the caller. A broken sibling never blocks the response.
//
// The Resolver is a thin coordinator that delegates to three components:
//   - DriftDetector:        cross-project drift detection (drift_detector.go)
//   - BrainEnricher:        brain-enhanced summaries (brain_enricher.go)
//   - CrossProjectSearch:   entity search, BFS context, memory queries (cross_project_search.go)
package federation

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

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
	Project      string   `json:"project"`       // federation alias
	Entity       string   `json:"entity"`        // entity name in sibling
	File         string   `json:"file"`          // file in sibling
	Change       string   `json:"change"`        // "signature_changed" | "removed" | "file_deleted"
	Severity     string   `json:"severity"`      // "breaking" | "info"
	DiffSummary  string   `json:"diff_summary"`  // human-readable summary of what changed
	YourCallers  []string `json:"your_callers"`  // local entities that call this sibling entity
	OldCommit    string   `json:"old_commit"`    // commit hash at last verification
	NewCommit    string   `json:"new_commit"`    // current HEAD of sibling
	LastVerified string   `json:"last_verified"` // timestamp of last verification
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
	Alias     string             `json:"alias"`
	Entity    string             `json:"entity"`
	NodeCount int                `json:"node_count"`
	Nodes     []graph.CarvedNode `json:"nodes"`
	Edges     []*graph.Edge      `json:"edges,omitempty"`
}

// BrainSummaryProvider retrieves brain summaries for cross-project entities.
// Decoupled from the brain package so federation doesn't import it directly.
type BrainSummaryProvider interface {
	Summary(projectID string, nodeID string) string
	Available() bool
}

// staleThreshold is how long since last index before a sibling is "stale".
const staleThreshold = 24 * time.Hour

// FederationParallelism is the max concurrent I/O operations against sibling
// stores. Each operation opens a SQLite file, so this bounds file descriptor
// and I/O pressure. Tuned for typical developer machines (8+ cores).
const FederationParallelism = 8

// SiblingQueryTimeout is the maximum time a single sibling query may take
// before being cancelled. Prevents one hanging sibling from exhausting a
// parallelism slot indefinitely.
const SiblingQueryTimeout = 10 * time.Second

// Clock is a function that returns the current time.
// Injected into Resolver for deterministic staleness testing.
type Clock func() time.Time

// Resolver provides cross-project query capabilities over local sibling
// SQLite stores. Stores are opened lazily on first access and cached for
// the session lifetime.
//
// Resolver is a thin coordinator that delegates to three components:
//   - drift:  *DriftDetector       — drift detection across siblings
//   - brain:  *BrainEnricher       — brain-enhanced summaries and explanations
//   - search: *CrossProjectSearch  — entity search, BFS context, memory queries
type Resolver struct {
	entries   []config.FederationEntry
	configDir string // directory containing synapses.json
	clock     Clock  // time source (defaults to time.Now)

	mu         sync.RWMutex
	stores     map[string]*store.Store // alias → read-only store (lazy-opened)
	storeErr   map[string]error        // alias → last open error (prevents retry loops)
	compatible map[string]bool         // alias → schema compat result (cached)
	gitHeads   map[string]string       // alias → cached HEAD commit hash

	// Components — initialized at construction, accessed via delegate methods.
	drift  *DriftDetector
	brain  *BrainEnricher
	search *CrossProjectSearch

	// statusGroup deduplicates concurrent statusForEntry calls for the same
	// alias. On NFS-mounted paths os.Stat can block for minutes; without dedup
	// repeated health checks accumulate goroutines until the NFS timeout fires.
	statusGroup singleflight.Group
}

// FederatedEpisode wraps a store.Episode with its source project alias.
type FederatedEpisode struct {
	Alias   string        `json:"alias"`
	Episode store.Episode `json:"episode"`
}

// FederatedMemoryHint is a 1-line summary of a sibling memory relevant to an entity.
// Used in prepare_context's Relevant tier to hint at cross-project knowledge.
type FederatedMemoryHint struct {
	Alias   string `json:"alias"`
	Summary string `json:"summary"` // 1-line: "AuthService rewrite driven by compliance"
	Query   string `json:"query"`   // recall query to get full context
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
	r := &Resolver{
		entries:    entries,
		configDir:  configDir,
		clock:      clock,
		stores:     make(map[string]*store.Store, len(entries)),
		storeErr:   make(map[string]error, len(entries)),
		compatible: make(map[string]bool, len(entries)),
		gitHeads:   make(map[string]string),
	}
	r.drift = newDriftDetector(r)
	r.brain = newBrainEnricher(r)
	r.search = newCrossProjectSearch(r)
	return r
}

// ---------------------------------------------------------------------------
// Lifecycle & shared infrastructure
// ---------------------------------------------------------------------------

// Status returns health info for each federation entry.
// Errors on individual entries are contained — a broken sibling returns
// a status entry with Error set, never a top-level error.
// Entries are checked in parallel (bounded to 8) to avoid 20×I/O latency
// at large federation sizes. Result ordering matches r.entries ordering.
func (r *Resolver) Status(ctx context.Context) []EntryStatus {
	if len(r.entries) == 0 {
		return nil
	}

	results := make([]EntryStatus, len(r.entries))

	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(FederationParallelism) // bound I/O parallelism — each entry opens a SQLite file

	for i, e := range r.entries {
		i, e := i, e // capture loop variables
		eg.Go(func() error {
			// Per-query timeout: statusForEntry does os.Stat + SQLite reads,
			// all bounded by SQLite busy_timeout(5000ms). The SiblingQueryTimeout
			// here guards against unexpected hangs (e.g., NFS-mounted paths where
			// os.Stat blocks). The SQLite busy_timeout is the inner bound; this
			// context timeout is the outer safety net.
			qctx, cancel := context.WithTimeout(egCtx, SiblingQueryTimeout)
			defer cancel()
			if qctx.Err() != nil {
				results[i] = EntryStatus{
					Alias:  e.Alias,
					Path:   e.Path,
					Status: "not_indexed",
					Error:  "timeout",
				}
				return nil
			}
			results[i] = r.statusForEntry(qctx, e)
			return nil
		})
	}
	_ = eg.Wait() // goroutines always return nil (fail-open)
	return results
}

func (r *Resolver) statusForEntry(ctx context.Context, e config.FederationEntry) EntryStatus {
	// Dedup concurrent calls for the same alias. On NFS-mounted paths os.Stat
	// can block for minutes; without dedup, repeated health checks accumulate
	// goroutines for the same path until the NFS timeout fires.
	result, _, _ := r.statusGroup.Do(e.Alias, func() (interface{}, error) {
		return r.doStatusForEntry(ctx, e), nil
	})
	return result.(EntryStatus)
}

func (r *Resolver) doStatusForEntry(ctx context.Context, e config.FederationEntry) EntryStatus {
	es := EntryStatus{
		Alias: e.Alias,
		Path:  e.Path,
	}

	// Run os.Stat in a goroutine so ctx cancellation can interrupt it.
	// This is important for NFS-mounted paths where Stat can block for minutes.
	type statResult struct {
		info os.FileInfo
		err  error
	}
	statCh := make(chan statResult, 1)
	go func() {
		info, err := os.Stat(e.Path)
		statCh <- statResult{info: info, err: err}
	}()
	var info os.FileInfo
	var err error
	select {
	case <-ctx.Done():
		es.Status = "not_indexed"
		es.Error = "timeout"
		return es
	case res := <-statCh:
		info, err = res.info, res.err
	}
	if err != nil || !info.IsDir() {
		es.Status = "not_found"
		if err != nil {
			es.Error = err.Error()
		}
		return es
	}

	// All remaining work (os.Stat(dbPath), newRawDB, SQLite reads) runs in a
	// goroutine so ctx cancellation interrupts the caller even if the OS/NFS
	// call blocks. The goroutine itself leaks until the OS releases it (NFS
	// timeout), but the caller returns promptly — the channel is buffered(1)
	// so the goroutine can send and exit without the caller being present.
	resCh := make(chan EntryStatus, 1)
	go func() {
		var out EntryStatus
		out.Alias = e.Alias
		out.Path = e.Path

		dbPath, err := SiblingDBPath(e.Path)
		if err != nil {
			out.Status = "not_indexed"
			out.Error = err.Error()
			resCh <- out
			return
		}

		if _, statErr := os.Stat(dbPath); os.IsNotExist(statErr) {
			out.Status = "not_indexed"
			resCh <- out
			return
		}

		// Single open: use rawDB for both schema check and stats read.
		// This avoids the double-open penalty of checking compat then opening again.
		db, err := newRawDB(dbPath)
		if err != nil {
			out.Status = "not_indexed"
			out.Error = err.Error()
			resCh <- out
			return
		}
		defer db.Close()

		// Schema compatibility: verify critical tables exist.
		if err := db.checkTables("nodes", "meta"); err != nil {
			out.Status = "incompatible"
			out.Error = err.Error()
			resCh <- out
			return
		}

		// Read stats from meta table (same queries as store.Stat).
		stat, err := db.readProjectStat(dbPath)
		if err != nil || stat == nil {
			out.Status = "not_indexed"
			if err != nil {
				out.Error = err.Error()
			}
			resCh <- out
			return
		}

		out.NodeCount = stat.NodeCount
		out.FileCount = stat.FileCount
		out.IndexedAt = stat.SavedAt

		if r.clock().Sub(stat.SavedAt) > staleThreshold {
			out.Status = "stale"
		} else {
			out.Status = "indexed"
		}
		resCh <- out
	}()

	select {
	case <-ctx.Done():
		es.Status = "not_indexed"
		es.Error = "timeout"
		return es
	case result := <-resCh:
		return result
	}
}

// InvalidateCache clears all cached state — stores, drift results, git heads,
// and compatibility results. Existing store handles are closed.
func (r *Resolver) InvalidateCache() {
	// Clear drift cache first — CheckDrift checks cache before touching stores,
	// so clearing cache first ensures any concurrent CheckDrift sees empty cache
	// and starts fresh rather than returning stale results.
	r.drift.InvalidateCache()

	r.mu.Lock()
	defer r.mu.Unlock()
	// Close and clear existing sibling store handles before resetting caches.
	// This ensures any schema upgrades in sibling daemons take effect on next open.
	for alias, st := range r.stores {
		if err := st.Close(); err != nil {
			log.Printf("federation: InvalidateCache close store %q: %v", alias, err)
		}
	}
	r.stores = make(map[string]*store.Store, len(r.entries))
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

// Aliases returns all configured federation aliases.
func (r *Resolver) Aliases() []string {
	aliases := make([]string, len(r.entries))
	for i, e := range r.entries {
		aliases[i] = e.Alias
	}
	return aliases
}

// SiblingProjectID derives the brain-compatible projectID for a sibling.
// Uses the same FNV32a hash of the absolute path as the main project.
func (r *Resolver) SiblingProjectID(alias string) string {
	path := r.entryPath(alias)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(abs))
	return fmt.Sprintf("%x", h.Sum32())
}

// ---------------------------------------------------------------------------
// Shared internal helpers (used by components)
// ---------------------------------------------------------------------------

// entryPath returns the filesystem path for a federation alias.
// Returns "" if the alias is not configured.
func (r *Resolver) entryPath(alias string) string {
	for _, e := range r.entries {
		if e.Alias == alias {
			return e.Path
		}
	}
	return ""
}

// filterEntries returns federation entries matching the given aliases.
// If aliases is nil or empty, returns all entries.
func (r *Resolver) filterEntries(aliases []string) []config.FederationEntry {
	if len(aliases) == 0 {
		return r.entries
	}
	set := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		set[a] = true
	}
	var filtered []config.FederationEntry
	for _, e := range r.entries {
		if set[e.Alias] {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// cachedHead returns the cached git HEAD for an alias, or fetches it fresh.
// Returns "" if git is unavailable. Used by GetDepsForEntity to avoid
// redundant git calls when CheckDrift already cached the HEAD.
func (r *Resolver) cachedHead(ctx context.Context, alias string) string {
	r.mu.RLock()
	head, ok := r.gitHeads[alias]
	r.mu.RUnlock()
	if ok && head != "" {
		return head
	}

	// Fetch fresh HEAD.
	path := r.entryPath(alias)
	if path == "" {
		return ""
	}
	h, err := gitRevParseHead(ctx, path)
	if err != nil || h == "" {
		return ""
	}
	// Double-check under write lock to prevent duplicate git subprocess from
	// racing: if another goroutine already stored a value, keep it.
	r.mu.Lock()
	if existing := r.gitHeads[alias]; existing == "" {
		r.gitHeads[alias] = h
	} else {
		h = existing
	}
	r.mu.Unlock()
	return h
}

// SetGitHead stores a cached HEAD commit hash for a sibling alias.
// Exposed so that DriftDetector can update the cache without reaching
// into Resolver internals (avoids lock ordering risks).
func (r *Resolver) SetGitHead(alias, head string) {
	r.mu.Lock()
	r.gitHeads[alias] = head
	r.mu.Unlock()
}

// CachedHead is the exported counterpart of cachedHead. It returns the cached
// git HEAD commit hash for a sibling project, fetching it fresh if not cached.
// Returns "" if the alias is unknown, not a git repo, or git is unavailable.
// Safe for concurrent use.
func (r *Resolver) CachedHead(ctx context.Context, alias string) string {
	return r.cachedHead(ctx, alias)
}

// isSiblingStoreFresh checks if a sibling store was indexed after the given
// commit. Returns false if freshness cannot be determined (fail-safe).
func (r *Resolver) isSiblingStoreFresh(ctx context.Context, siblingStore *store.Store, currentHead, repoPath string) bool {
	savedAt, err := siblingStore.SavedAt()
	if err != nil || savedAt.IsZero() {
		return false // can't determine freshness — don't trust
	}

	// Get the HEAD commit's timestamp.
	commitTime, err := gitCommitTime(ctx, repoPath, currentHead)
	if err != nil || commitTime.IsZero() {
		// Can't get commit time — if store was saved very recently (within
		// 5 minutes), trust it. Otherwise fall back to git diff.
		return r.clock().Sub(savedAt) < 5*time.Minute
	}

	// Store was saved at or after the commit → parser has seen the latest code.
	// Using !Before instead of After so that same-second timestamps (common
	// when commit and re-index happen rapidly) are treated as fresh.
	return !savedAt.Before(commitTime)
}

// getStore returns the cached store for the alias, opening it lazily if needed.
// Returns nil on any error (fail-open). Once a store fails to open, the error
// is cached to avoid retry storms — call InvalidateCache to reset.
func (r *Resolver) getStore(alias string) *store.Store {
	// Fast path: check if already cached under read lock.
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

	// Acquire write lock for compat check + store open. This eliminates the
	// TOCTOU race where concurrent goroutines would run duplicate compat checks
	// and open duplicate store connections outside any lock.
	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock — another goroutine may have
	// completed the open while we waited for the lock.
	if st, ok := r.stores[alias]; ok {
		return st
	}
	if _, errCached := r.storeErr[alias]; errCached {
		return nil
	}

	// Check cached compat result, or run the check under the write lock.
	compat, compatKnown := r.compatible[alias]
	if !compatKnown {
		if err := checkSchemaCompatibility(dbPath); err != nil {
			r.storeErr[alias] = err
			r.compatible[alias] = false
			log.Printf("federation: incompatible store %q: %v", alias, err)
			return nil
		}
		r.compatible[alias] = true
		compat = true
	}
	if !compat {
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

// GetStore is the exported version of getStore. Returns the cached sibling
// store for the alias, opening it lazily. Returns nil on any error (fail-open).
func (r *Resolver) GetStore(alias string) *store.Store {
	return r.getStore(alias)
}

// ---------------------------------------------------------------------------
// Schema utilities
// ---------------------------------------------------------------------------

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

	// BUG-031: validate that the resolved project path exists as a real
	// directory and is not a symlink pointing outside the expected tree.
	// Prevents a compromised .synapses/synapses.json from pointing federation
	// at crafted SQLite files in arbitrary locations.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("eval symlinks for federation path %q: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("federation path %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("federation path %q is not a directory", resolved)
	}

	return store.DefaultPath(resolved)
}

// ---------------------------------------------------------------------------
// Delegate methods — preserve the public API by forwarding to components.
// Callers continue to use *Resolver without modification.
// ---------------------------------------------------------------------------

// CheckDrift delegates to DriftDetector.
func (r *Resolver) CheckDrift(ctx context.Context, localStore *store.Store) []DriftAlert {
	return r.drift.CheckDrift(ctx, localStore)
}

// EntityExists delegates to CrossProjectSearch.
func (r *Resolver) EntityExists(ctx context.Context, alias string, entityName string) bool {
	return r.search.EntityExists(ctx, alias, entityName)
}

// FindEntities delegates to CrossProjectSearch.
func (r *Resolver) FindEntities(ctx context.Context, query string, aliases []string, limit int) []FederatedSearchResult {
	return r.search.FindEntities(ctx, query, aliases, limit)
}

// GetDepsForEntity delegates to CrossProjectSearch.
func (r *Resolver) GetDepsForEntity(ctx context.Context, entityID string, localStore *store.Store) []CrossProjectDepStatus {
	return r.search.GetDepsForEntity(ctx, entityID, localStore)
}

// GetEntityContext delegates to CrossProjectSearch.
func (r *Resolver) GetEntityContext(ctx context.Context, entity string, alias string, depth int) *FederatedContext {
	return r.search.GetEntityContext(ctx, entity, alias, depth)
}

// SearchEpisodes delegates to CrossProjectSearch.
func (r *Resolver) SearchEpisodes(ctx context.Context, query string, aliases []string, limit int) []FederatedEpisode {
	return r.search.SearchEpisodes(ctx, query, aliases, limit)
}

// SearchMemoriesForEntity delegates to CrossProjectSearch.
func (r *Resolver) SearchMemoriesForEntity(ctx context.Context, entityName string, aliases []string) []FederatedMemoryHint {
	return r.search.SearchMemoriesForEntity(ctx, entityName, aliases)
}

// SetBrain delegates to BrainEnricher.
func (r *Resolver) SetBrain(brain BrainSummaryProvider) {
	r.brain.SetBrain(brain)
}

// SetBrainGenerate delegates to BrainEnricher.
func (r *Resolver) SetBrainGenerate(fn func(ctx context.Context, prompt string) (string, error)) {
	r.brain.SetBrainGenerate(fn)
}

// GetEntitySummary delegates to BrainEnricher.
func (r *Resolver) GetEntitySummary(ctx context.Context, alias, entityName string) string {
	return r.brain.GetEntitySummary(ctx, alias, entityName)
}

// BrainDriftSummary delegates to BrainEnricher.
func (r *Resolver) BrainDriftSummary(ctx context.Context, oldSig, newSig, entityName string) string {
	return r.brain.BrainDriftSummary(ctx, oldSig, newSig, entityName)
}
