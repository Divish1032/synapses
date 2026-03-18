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
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
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

// staleThreshold is how long since last index before a sibling is "stale".
const staleThreshold = 24 * time.Hour

// nowFunc is the time source — overridable in tests for deterministic staleness.
var nowFunc = time.Now

// Resolver provides cross-project query capabilities over local sibling
// SQLite stores. Stores are opened lazily on first access and cached for
// the session lifetime.
type Resolver struct {
	entries   []config.FederationEntry
	configDir string // directory containing synapses.json

	mu         sync.RWMutex
	stores     map[string]*store.Store   // alias → read-only store (lazy-opened)
	storeErr   map[string]error          // alias → last open error (prevents retry loops)
	driftCache map[string][]DriftAlert   // alias → cached drift results (session-level)
	gitHeads   map[string]string         // alias → cached HEAD commit hash
}

// NewResolver creates a Resolver for the given federation entries.
// configDir is the directory containing synapses.json (paths are already
// resolved to absolute by config.Load).
func NewResolver(entries []config.FederationEntry, configDir string) *Resolver {
	return &Resolver{
		entries:    entries,
		configDir:  configDir,
		stores:     make(map[string]*store.Store, len(entries)),
		storeErr:   make(map[string]error, len(entries)),
		driftCache: make(map[string][]DriftAlert),
		gitHeads:   make(map[string]string),
	}
}

// Status returns health info for each federation entry.
// Errors on individual entries are contained — a broken sibling returns
// a status entry with Error set, never a top-level error.
// ctx is used for timeout; if the deadline expires, remaining entries
// are reported as-is.
func (r *Resolver) Status(ctx context.Context) []EntryStatus {
	results := make([]EntryStatus, 0, len(r.entries))
	for _, e := range r.entries {
		if ctx.Err() != nil {
			// Context expired — report remaining as unknown.
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

	// Schema compatibility check: verify the sibling's DB has the
	// critical tables we need. If it's from a wildly different version
	// of Synapses (or a different tool entirely), we skip it.
	if err := checkSchemaCompatibility(dbPath); err != nil {
		es.Status = "incompatible"
		es.Error = err.Error()
		return es
	}

	// Open the sibling store read-only.
	// Note: we do NOT cache the store during Status() — status is
	// a diagnostic check. Caching happens on first query via getStore().
	st, err := store.OpenReadOnly(dbPath)
	if err != nil {
		es.Status = "not_indexed"
		es.Error = err.Error()
		return es
	}
	defer st.Close()

	// Read project stats.
	stat, err := st.Stat(dbPath)
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

	// Staleness: consider stale if indexed more than 24 hours ago.
	if nowFunc().Sub(stat.SavedAt) > staleThreshold {
		es.Status = "stale"
	} else {
		es.Status = "indexed"
	}

	return es
}

// checkSchemaCompatibility opens the DB briefly to verify that the tables
// we need for federation queries exist. Returns an error if the DB is
// incompatible (missing critical tables).
func checkSchemaCompatibility(dbPath string) error {
	db, err := openRawReadOnly(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// Check that the 'nodes' and 'meta' tables exist — these are the
	// minimum for any federation query.
	for _, table := range []string{"nodes", "meta"} {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			return fmt.Errorf("missing table %q — incompatible store version", table)
		}
	}
	return nil
}

// openRawReadOnly opens a SQLite DB in read-only mode without going through
// store.Open (which runs migrations). Used for schema compatibility checks.
func openRawReadOnly(path string) (rawDB, error) {
	// We use the sql package directly here. Import is already available
	// transitively via store, but we need a direct reference.
	return newRawDB(path)
}

// EntityExists checks if an entity name exists in a sibling's store.
// Returns false on any error (fail-open).
func (r *Resolver) EntityExists(ctx context.Context, alias string, entityName string) bool {
	st := r.getStore(alias)
	if st == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	exists, err := st.NodeExistsByName(entityName)
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

		nodes, err := st.FindNodesByName(query, limit)
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

// InvalidateCache clears session-level caches. Called on session_init
// to ensure fresh data each session.
func (r *Resolver) InvalidateCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.driftCache = make(map[string][]DriftAlert)
	r.gitHeads = make(map[string]string)
	// Clear store errors so stale failures are retried.
	r.storeErr = make(map[string]error, len(r.entries))
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
	// Check if we already failed to open this store.
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

	// Schema check before opening.
	if err := checkSchemaCompatibility(dbPath); err != nil {
		r.mu.Lock()
		r.storeErr[alias] = err
		r.mu.Unlock()
		log.Printf("federation: incompatible store %q: %v", alias, err)
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

// SiblingDBPath derives the store DB path for a sibling project using
// the same logic as store.DefaultPath.
func SiblingDBPath(projectPath string) (string, error) {
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return "", fmt.Errorf("abs path: %w", err)
	}
	return store.DefaultPath(abs)
}
