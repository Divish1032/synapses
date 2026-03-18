// Package federation provides cross-project dependency tracking and drift
// detection for local sibling projects. It opens sibling SQLite stores
// read-only and compares stored dependency state against current git HEAD.
//
// All operations are fail-open: errors are logged and skipped, never
// propagated to the caller. A broken sibling never blocks the response.
package federation

import (
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
	Status    string    `json:"status"` // "indexed" | "stale" | "not_indexed" | "not_found" | "incompatible_version"
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

// Resolver provides cross-project query capabilities over local sibling
// SQLite stores. Stores are opened lazily on first access and cached for
// the session lifetime.
type Resolver struct {
	entries   []config.FederationEntry
	configDir string // directory containing synapses.json

	mu         sync.RWMutex
	stores     map[string]*store.Store // alias → read-only store (lazy-opened)
	driftCache map[string][]DriftAlert // alias → cached drift results (session-level)
	gitHeads   map[string]string       // alias → cached HEAD commit hash
}

// NewResolver creates a Resolver for the given federation entries.
// configDir is the directory containing synapses.json (paths are already
// resolved to absolute by config.Load).
func NewResolver(entries []config.FederationEntry, configDir string) *Resolver {
	return &Resolver{
		entries:    entries,
		configDir:  configDir,
		stores:     make(map[string]*store.Store, len(entries)),
		driftCache: make(map[string][]DriftAlert),
		gitHeads:   make(map[string]string),
	}
}

// Status returns health info for each federation entry.
// Errors on individual entries are contained — a broken sibling returns
// a status entry with Error set, never a top-level error.
func (r *Resolver) Status() []EntryStatus {
	results := make([]EntryStatus, 0, len(r.entries))
	for _, e := range r.entries {
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

	// Open the sibling store read-only.
	st, err := r.openStore(e.Alias, dbPath)
	if err != nil {
		es.Status = "not_indexed"
		es.Error = err.Error()
		return es
	}

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
	if time.Since(stat.SavedAt) > 24*time.Hour {
		es.Status = "stale"
	} else {
		es.Status = "indexed"
	}

	return es
}

// EntityExists checks if an entity name exists in a sibling's store.
// Returns false on any error (fail-open).
func (r *Resolver) EntityExists(alias string, entityName string) bool {
	st := r.getStore(alias)
	if st == nil {
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
func (r *Resolver) FindEntities(query string, aliases []string, limit int) []FederatedSearchResult {
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

// InvalidateCache clears session-level caches. Called on session_init
// to ensure fresh data each session.
func (r *Resolver) InvalidateCache() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.driftCache = make(map[string][]DriftAlert)
	r.gitHeads = make(map[string]string)
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

// openStore opens a sibling store read-only, caching the result.
func (r *Resolver) openStore(alias, dbPath string) (*store.Store, error) {
	r.mu.RLock()
	if st, ok := r.stores[alias]; ok {
		r.mu.RUnlock()
		return st, nil
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock.
	if st, ok := r.stores[alias]; ok {
		return st, nil
	}

	st, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sibling store %q: %w", alias, err)
	}
	r.stores[alias] = st
	return st, nil
}

// getStore returns the cached store for the alias, opening it lazily if needed.
// Returns nil on any error (fail-open).
func (r *Resolver) getStore(alias string) *store.Store {
	r.mu.RLock()
	st, ok := r.stores[alias]
	r.mu.RUnlock()
	if ok {
		return st
	}

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
		return nil
	}

	st, err = r.openStore(alias, dbPath)
	if err != nil {
		log.Printf("federation: lazy open %q: %v", alias, err)
		return nil
	}
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
