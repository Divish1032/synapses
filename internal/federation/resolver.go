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
	"hash/fnv"
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
	Project      string   `json:"project"`       // federation alias
	Entity       string   `json:"entity"`        // entity name in sibling
	File         string   `json:"file"`          // file in sibling
	Change       string   `json:"change"`        // "signature_changed" | "removed" | "file_deleted"
	Severity     string   `json:"severity"`      // "breaking" | "info"
	DiffSummary  string   `json:"diff_summary"`  // human-readable change description
	YourCallers  []string `json:"your_callers"`  // which local entities depend on this
	OldCommit    string   `json:"old_commit"`    // commit when dep was verified
	NewCommit    string   `json:"new_commit"`    // sibling's current HEAD
	LastVerified string   `json:"last_verified"` // RFC3339 timestamp of last verification
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

// BrainSummaryProvider retrieves brain summaries for cross-project entities.
// Decoupled from the brain package so federation doesn't import it directly.
type BrainSummaryProvider interface {
	// Summary returns the brain-generated summary for a node, or "".
	Summary(projectID, nodeID string) string
	// Available reports whether the brain LLM is accessible.
	Available() bool
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
	brain     BrainSummaryProvider // optional, for cross-project summaries

	// brainGenerate is an optional LLM generate function for brain-enhanced
	// drift summaries. When set, BrainDriftSummary feeds the structural diff
	// to the LLM for a natural-language explanation of impact on callers.
	// Injected via SetBrainGenerate. Nil = structural diff only.
	brainGenerate func(ctx context.Context, prompt string) (string, error)

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

	targets := r.filterEntries(aliases)

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
		status := CrossProjectDepStatus{
			Project: dep.ToProject,
			Entity:  dep.ToEntity,
			File:    dep.ToFile,
		}

		// Enrich with graph-based drift check if sibling store is available,
		// fresh (re-indexed after latest commit), and we have a stored
		// signature to compare against. If the store is stale, skip
		// enrichment entirely — the agent gets no false confidence, and
		// the authoritative CheckDrift from session_init handles it.
		if dep.VerifiedSignature != "" {
			sibStore := r.getStore(dep.ToProject)
			repoPath := r.entryPath(dep.ToProject)
			if sibStore != nil && repoPath != "" && r.isSiblingStoreFresh(sibStore, r.cachedHead(dep.ToProject), repoPath) {
				nodes, findErr := sibStore.FindNodesByNameCtx(ctx, dep.ToEntity, 1)
				if findErr == nil && len(nodes) > 0 {
					if nodes[0].Signature != dep.VerifiedSignature {
						status.Drifted = true
						status.DiffSummary = structuralSignatureDiff(dep.VerifiedSignature, nodes[0].Signature)
					}
				} else if findErr == nil && len(nodes) == 0 {
					status.Drifted = true
					status.DiffSummary = "Entity no longer exists in sibling project"
				}
				// findErr != nil → fail-open, leave as not drifted
			}
		}

		results = append(results, status)
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
//
// Detection strategy (graph-first):
//  1. Read sibling HEAD — if unchanged, skip (fast path).
//  2. Try graph-based comparison: query sibling store for current entity
//     signatures, compare against stored verified_signature. This uses
//     Synapses' own parsed graph — all 49 languages, no regex, no subprocess.
//  3. If sibling store unavailable, fall back to git diff + regex matching.
//  4. If git unavailable, fall back to stored signature comparison.
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

	// Step 2: If ALL deps match current HEAD, no drift.
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

	// Step 3: Collect stale deps (verified_commit != HEAD).
	var staleDeps []store.CrossProjectDep
	for _, dep := range deps {
		if dep.VerifiedCommit != currentHead {
			staleDeps = append(staleDeps, dep)
		}
	}

	// Step 4: Graph-first — use the sibling's parsed graph for comparison.
	// This is the primary path: no git subprocess, no regex, works for all
	// 49 languages Synapses can parse.
	//
	// IMPORTANT: Only trust graph comparison when the sibling store is fresh.
	// If the store hasn't been re-indexed since HEAD moved, the stored
	// signatures are stale and comparing them against verified_signature
	// would produce false negatives (both are old → "no drift" → advance
	// verified_commit → permanently missed drift).
	siblingStore := r.getStore(e.Alias)
	if siblingStore != nil && r.isSiblingStoreFresh(siblingStore, currentHead, e.Path) {
		alerts := r.checkDriftGraphFirst(ctx, e.Alias, currentHead, staleDeps, siblingStore, localStore)
		return mergeAlerts(alerts)
	}

	// Step 5: Sibling store unavailable — fall back to git diff + regex.
	commitGroups := make(map[string][]store.CrossProjectDep)
	for _, dep := range staleDeps {
		commitGroups[dep.VerifiedCommit] = append(commitGroups[dep.VerifiedCommit], dep)
	}

	var alerts []DriftAlert
	for oldCommit, groupDeps := range commitGroups {
		if ctx.Err() != nil {
			break
		}
		groupAlerts := r.checkDriftForCommitGroup(ctx, e, oldCommit, currentHead, groupDeps, localStore)
		alerts = append(alerts, groupAlerts...)
	}
	return mergeAlerts(alerts)
}

// isSiblingStoreFresh checks whether the sibling's store was re-indexed
// recently enough to trust its signatures for graph-based drift detection.
//
// Strategy: compare the sibling store's SavedAt timestamp against the HEAD
// commit's author date. If the store was saved AFTER the commit, the parser
// has seen the latest code and signatures are trustworthy. If the store is
// older than the commit, the signatures may be stale — fall through to git
// diff which compares actual file content between commits.
//
// This prevents the "permanently missed drift" bug: when a sibling's HEAD
// moves but its daemon hasn't re-indexed yet, graph comparison would see
// old signatures matching verified_signature and silently advance the
// verified_commit past the real change.
func (r *Resolver) isSiblingStoreFresh(siblingStore *store.Store, currentHead, repoPath string) bool {
	savedAt, err := siblingStore.SavedAt()
	if err != nil || savedAt.IsZero() {
		return false // can't determine freshness — don't trust
	}

	// Get the HEAD commit's timestamp.
	commitTime, err := gitCommitTime(context.Background(), repoPath, currentHead)
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

// checkDriftGraphFirst uses the sibling's parsed graph to detect signature
// changes. This is the primary detection path — it leverages Synapses' own
// intelligence (49 AST parsers) instead of regex on raw diff text.
//
// For each stale dep:
//  1. Query sibling store for the entity's current signature.
//  2. Compare against dep.VerifiedSignature.
//  3. Same → silently update verified_commit. Different → drift alert.
//  4. Entity not found → "removed" alert.
//
// Produces structural signature diffs (parameter changes, return type changes)
// instead of raw diff text.
func (r *Resolver) checkDriftGraphFirst(
	ctx context.Context,
	alias, currentHead string,
	deps []store.CrossProjectDep,
	siblingStore *store.Store,
	localStore *store.Store,
) []DriftAlert {
	var alerts []DriftAlert
	for _, dep := range deps {
		if ctx.Err() != nil {
			break
		}

		// Query the sibling's graph for the entity's current state.
		results, err := siblingStore.FindNodesByNameCtx(ctx, dep.ToEntity, 1)
		if err != nil {
			// Store query failed — fail-open, silently update.
			_ = localStore.UpdateVerifiedCommit(dep.ToProject, dep.ToEntity, currentHead)
			continue
		}

		if len(results) == 0 {
			// Entity no longer exists in sibling store.
			alerts = append(alerts, buildAlert(dep, alias, "removed", "breaking",
				"Entity no longer exists in sibling project", dep.VerifiedCommit, currentHead))
			continue
		}

		currentSig := results[0].Signature
		if dep.VerifiedSignature == "" {
			// Legacy dep without stored signature — can't compare, silently update.
			_ = localStore.UpdateVerifiedCommit(dep.ToProject, dep.ToEntity, currentHead)
			continue
		}

		if currentSig == dep.VerifiedSignature {
			// Signature unchanged — silently update commit.
			_ = localStore.UpdateVerifiedCommit(dep.ToProject, dep.ToEntity, currentHead)
			continue
		}

		// Signature changed — produce a structural diff.
		summary := structuralSignatureDiff(dep.VerifiedSignature, currentSig)
		alerts = append(alerts, buildAlert(dep, alias, "signature_changed", "breaking",
			summary, dep.VerifiedCommit, currentHead))
	}
	return alerts
}

// checkDriftForCommitGroup is the git-diff fallback for when the sibling's
// parsed graph is unavailable (store not indexed or incompatible). It uses
// git diff + regex signature matching — less precise than graph comparison
// but functional without a sibling store.
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
				"Entity removed from "+dep.ToFile, oldCommit, newCommit))
			continue
		}

		// Signature changed but entity still exists.
		summary := extractDiffSummary(diff, dep.ToEntity)
		alerts = append(alerts, buildAlert(dep, e.Alias, "signature_changed", "breaking",
			summary, oldCommit, newCommit))
	}
	return alerts
}

// checkDriftFallback uses direct signature comparison when git is unavailable.
// Compares the stored verified_signature against the current sibling store
// signature. Less precise than git diff (catches formatting changes) but
// functional. If verified_signature is empty (legacy dep without stored
// signature), only removal is detectable.
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

		results, err := st.FindNodesByNameCtx(ctx, dep.ToEntity, 1)
		if err != nil {
			continue // fail-open
		}

		if len(results) == 0 {
			alerts = append(alerts, DriftAlert{
				Project:      alias,
				Entity:       dep.ToEntity,
				File:         dep.ToFile,
				Change:       "removed",
				Severity:     "breaking",
				DiffSummary:  "Entity not found in sibling store (fallback check)",
				YourCallers:  []string{dep.FromEntity},
				OldCommit:    dep.VerifiedCommit,
				LastVerified: dep.VerifiedAt,
			})
			continue
		}

		// Compare stored signature against current signature.
		if dep.VerifiedSignature != "" && results[0].Signature != dep.VerifiedSignature {
			alerts = append(alerts, DriftAlert{
				Project:      alias,
				Entity:       dep.ToEntity,
				File:         dep.ToFile,
				Change:       "signature_changed",
				Severity:     "breaking",
				DiffSummary:  fmt.Sprintf("Signature changed (fallback): %s → %s", truncate(dep.VerifiedSignature, 60), truncate(results[0].Signature, 60)),
				YourCallers:  []string{dep.FromEntity},
				OldCommit:    dep.VerifiedCommit,
				LastVerified: dep.VerifiedAt,
			})
			continue
		}

		// Signature matches or no stored signature → no drift detectable.
	}
	return mergeAlerts(alerts)
}

// buildAlert constructs a DriftAlert with local callers and verification timestamp.
func buildAlert(dep store.CrossProjectDep, alias, change, severity, summary, oldCommit, newCommit string) DriftAlert {
	return DriftAlert{
		Project:      alias,
		Entity:       dep.ToEntity,
		File:         dep.ToFile,
		Change:       change,
		Severity:     severity,
		DiffSummary:  summary,
		YourCallers:  []string{dep.FromEntity},
		OldCommit:    oldCommit,
		NewCommit:    newCommit,
		LastVerified: dep.VerifiedAt,
	}
}

// mergeAlerts deduplicates alerts for the same (entity, file) by combining
// their YourCallers lists. This handles the case where multiple local
// entities depend on the same sibling entity — one alert with all callers
// instead of N alerts with one caller each.
func mergeAlerts(alerts []DriftAlert) []DriftAlert {
	if len(alerts) <= 1 {
		return alerts
	}

	type key struct{ entity, file string }
	merged := make(map[key]*DriftAlert, len(alerts))
	order := make([]key, 0, len(alerts))

	for i := range alerts {
		k := key{alerts[i].Entity, alerts[i].File}
		if existing, ok := merged[k]; ok {
			existing.YourCallers = append(existing.YourCallers, alerts[i].YourCallers...)
		} else {
			a := alerts[i] // copy
			merged[k] = &a
			order = append(order, k)
		}
	}

	result := make([]DriftAlert, 0, len(merged))
	for _, k := range order {
		a := merged[k]
		a.YourCallers = dedupStrings(a.YourCallers)
		result = append(result, *a)
	}
	return result
}

// dedupStrings removes duplicate strings from a slice, preserving order.
func dedupStrings(ss []string) []string {
	if len(ss) <= 1 {
		return ss
	}
	seen := make(map[string]bool, len(ss))
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// structuralSignatureDiff produces a human-readable summary of what changed
// between two parsed entity signatures. This operates on Synapses' own graph
// data (the output of 49 AST parsers), not raw source code.
//
// Examples:
//
//	"func Validate(token string) error" → "func Validate(token string, opts ...Option) (bool, error)"
//	Output: "Params: added 'opts ...Option'. Returns: 'error' → '(bool, error)'"
//
//	"func Login(user string)" → ""  (entity removed)
//	Output: handled by caller as "removed", not by this function
func structuralSignatureDiff(oldSig, newSig string) string {
	if oldSig == "" || newSig == "" {
		return fmt.Sprintf("Changed: %s → %s", truncate(oldSig, 80), truncate(newSig, 80))
	}

	// Extract parameter lists and return types for structural comparison.
	oldParams, oldReturns := parseSignatureParts(oldSig)
	newParams, newReturns := parseSignatureParts(newSig)

	var parts []string

	// Compare parameters.
	if oldParams != newParams {
		addedParams, removedParams := diffCommaSeparated(oldParams, newParams)
		if len(removedParams) > 0 && len(addedParams) > 0 {
			parts = append(parts, fmt.Sprintf("Params: removed '%s', added '%s'",
				strings.Join(removedParams, ", "), strings.Join(addedParams, ", ")))
		} else if len(addedParams) > 0 {
			parts = append(parts, fmt.Sprintf("Params: added '%s'", strings.Join(addedParams, ", ")))
		} else if len(removedParams) > 0 {
			parts = append(parts, fmt.Sprintf("Params: removed '%s'", strings.Join(removedParams, ", ")))
		} else {
			// Parameter order or types changed but same count — show raw diff.
			parts = append(parts, fmt.Sprintf("Params: '%s' → '%s'", truncate(oldParams, 60), truncate(newParams, 60)))
		}
	}

	// Compare return types.
	if oldReturns != newReturns {
		if oldReturns == "" {
			parts = append(parts, fmt.Sprintf("Returns: added '%s'", truncate(newReturns, 60)))
		} else if newReturns == "" {
			parts = append(parts, fmt.Sprintf("Returns: removed '%s'", truncate(oldReturns, 60)))
		} else {
			parts = append(parts, fmt.Sprintf("Returns: '%s' → '%s'", truncate(oldReturns, 60), truncate(newReturns, 60)))
		}
	}

	if len(parts) > 0 {
		return strings.Join(parts, ". ")
	}

	// Signatures differ but we couldn't parse the structural difference.
	// Fall back to showing the raw change.
	return fmt.Sprintf("Changed: %s → %s", truncate(oldSig, 80), truncate(newSig, 80))
}

// parseSignatureParts extracts parameter list and return type from a parsed
// entity signature string. Works across languages because the signatures are
// already normalized by Synapses' AST parsers.
//
// Go:     "func Validate(ctx context.Context, token string) (bool, error)"
//
//	→ params="ctx context.Context, token string", returns="(bool, error)"
//
// Python: "def validate(self, token: str) -> bool"
//
//	→ params="self, token: str", returns="bool"
//
// Rust:   "fn validate(&self, token: &str) -> Result<bool, Error>"
//
//	→ params="&self, token: &str", returns="Result<bool, Error>"
//
// TS:     "function validate(token: string): Promise<boolean>"
//
//	→ params="token: string", returns="Promise<boolean>"
func parseSignatureParts(sig string) (params string, returns string) {
	// Find the parameter list: first '(' to its matching ')'.
	openParen := strings.Index(sig, "(")
	if openParen < 0 {
		return "", "" // no params — e.g., a type/struct declaration
	}

	// Find the matching close paren (handle nested parens for return types).
	depth := 0
	closeParen := -1
	for i := openParen; i < len(sig); i++ {
		switch sig[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				closeParen = i
				goto found
			}
		}
	}
found:
	if closeParen < 0 {
		return "", ""
	}

	params = strings.TrimSpace(sig[openParen+1 : closeParen])

	// Extract return type: everything after the closing paren.
	rest := strings.TrimSpace(sig[closeParen+1:])
	// Go: "(bool, error)" or "error"
	// Python: "-> bool"
	// Rust: "-> Result<bool, Error>"
	// TS: ": Promise<boolean>"
	rest = strings.TrimPrefix(rest, "->")
	rest = strings.TrimPrefix(rest, ":")
	returns = strings.TrimSpace(rest)

	return params, returns
}

// diffCommaSeparated compares two comma-separated parameter lists and returns
// which items were added and removed. Trims whitespace around each item.
func diffCommaSeparated(oldList, newList string) (added, removed []string) {
	oldItems := splitAndTrim(oldList)
	newItems := splitAndTrim(newList)

	oldSet := make(map[string]bool, len(oldItems))
	for _, item := range oldItems {
		oldSet[item] = true
	}
	newSet := make(map[string]bool, len(newItems))
	for _, item := range newItems {
		newSet[item] = true
	}

	for _, item := range newItems {
		if !oldSet[item] {
			added = append(added, item)
		}
	}
	for _, item := range oldItems {
		if !newSet[item] {
			removed = append(removed, item)
		}
	}
	return added, removed
}

// splitAndTrim splits a comma-separated string and trims whitespace from each part.
// Returns nil for empty input.
func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// extractDiffSummary produces a brief human-readable summary of what changed
// in a diff for a specific entity. Used ONLY in the git-diff fallback path
// when the sibling's parsed graph is unavailable.
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

// SearchEpisodes queries sibling stores' episodes tables using FTS5 search.
// Results are labeled with their source alias. If aliases is nil or empty,
// all siblings are searched. Errors on individual siblings are silently skipped.
func (r *Resolver) SearchEpisodes(ctx context.Context, query string, aliases []string, limit int) []FederatedEpisode {
	if limit <= 0 {
		limit = 5
	}

	targets := r.filterEntries(aliases)
	var results []FederatedEpisode

	for _, e := range targets {
		if ctx.Err() != nil {
			break
		}
		st := r.getStore(e.Alias)
		if st == nil {
			continue
		}

		// Check if the sibling store has the episodes_fts table.
		// Older stores might not have episodic memory tables.
		if !r.hasEpisodesTable(st) {
			continue
		}

		episodes, err := st.RecallEpisodes(query, "", "", "", "", limit, 0)
		if err != nil {
			log.Printf("federation: search episodes in %q: %v", e.Alias, err)
			continue
		}

		for _, ep := range episodes {
			results = append(results, FederatedEpisode{
				Alias:   e.Alias,
				Episode: ep,
			})
		}
	}
	return results
}

// SearchMemoriesForEntity searches sibling stores for episodic memories
// related to a specific entity. Uses graph-anchored search (node ID in
// affected_nodes) as the primary path — more precise than text matching.
// Falls back to FTS text search if no node ID is found.
// Returns 1-line hints for prepare_context. At most 3 hints per sibling.
func (r *Resolver) SearchMemoriesForEntity(ctx context.Context, entityName string, aliases []string) []FederatedMemoryHint {
	targets := r.filterEntries(aliases)
	var hints []FederatedMemoryHint

	for _, e := range targets {
		if ctx.Err() != nil {
			break
		}
		st := r.getStore(e.Alias)
		if st == nil {
			continue
		}
		if !r.hasEpisodesTable(st) {
			continue
		}

		// Primary: graph-anchored search via node ID in affected_nodes.
		// This is precise — finds only memories explicitly linked to the entity,
		// not just any memory that mentions the name in text.
		var episodes []store.Episode
		nodes, err := st.FindNodesByNameCtx(ctx, entityName, 1)
		if err == nil && len(nodes) > 0 {
			episodes, _ = st.FindEpisodesByNodeID(nodes[0].ID, 3)
		}

		// Fallback: FTS text search on entity name.
		// Used when the entity has no node in the sibling store (e.g., removed
		// entity with memories still referencing it by name).
		if len(episodes) == 0 {
			episodes, _ = st.RecallEpisodes(entityName, "", "", "", "", 3, 0)
		}

		for _, ep := range episodes {
			summary := ep.Decision
			if len(summary) > 120 {
				summary = summary[:117] + "..."
			}
			hints = append(hints, FederatedMemoryHint{
				Alias:   e.Alias,
				Summary: summary,
				Query:   entityName,
			})
		}
	}
	return hints
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

// hasEpisodesTable checks if a sibling store has the episodes and
// episodes_fts tables required for cross-project memory search.
// Uses sqlite_master introspection — no probe queries, no side effects.
func (r *Resolver) hasEpisodesTable(st *store.Store) bool {
	return st.HasTable("episodes") && st.HasTable("episodes_fts")
}

// filterEntries returns federation entries matching the given aliases.
// If aliases is nil or empty, returns all entries.
func (r *Resolver) filterEntries(aliases []string) []config.FederationEntry {
	if len(aliases) == 0 {
		return r.entries
	}
	aliasSet := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		aliasSet[a] = true
	}
	var targets []config.FederationEntry
	for _, e := range r.entries {
		if aliasSet[e.Alias] {
			targets = append(targets, e)
		}
	}
	return targets
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

// cachedHead returns the cached git HEAD for an alias, or fetches it fresh.
// Returns "" if git is unavailable. Used by GetDepsForEntity to avoid
// redundant git calls when CheckDrift already cached the HEAD.
func (r *Resolver) cachedHead(alias string) string {
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
	h, err := gitRevParseHead(context.Background(), path)
	if err != nil || h == "" {
		return ""
	}
	r.mu.Lock()
	r.gitHeads[alias] = h
	r.mu.Unlock()
	return h
}

// SetBrain attaches a brain summary provider for cross-project summarization.
// Optional — when nil, cross-project summaries use raw entity signatures.
func (r *Resolver) SetBrain(brain BrainSummaryProvider) {
	r.brain = brain
}

// SetBrainGenerate attaches an LLM generate function for brain-enhanced
// drift summaries. When set and brain is available, BrainDriftSummary
// feeds the structural diff to the LLM for a natural-language explanation.
// Typically wired to the brain's ingestor LLM client.
func (r *Resolver) SetBrainGenerate(fn func(ctx context.Context, prompt string) (string, error)) {
	r.brainGenerate = fn
}

// GetEntitySummary returns a brain-generated summary for a sibling entity.
// Falls back to the entity's raw signature if brain is unavailable or has
// no summary. This is the cross-project context summarization path.
func (r *Resolver) GetEntitySummary(ctx context.Context, alias, entityName string) string {
	if ctx.Err() != nil {
		return ""
	}

	// Try brain summary first (zero LLM calls — reads from brain.sqlite).
	if r.brain != nil {
		projectID := r.SiblingProjectID(alias)
		if projectID != "" {
			// Brain summaries are indexed by nodeID. We need to find the
			// entity's nodeID in the sibling store first.
			st := r.getStore(alias)
			if st != nil {
				results, err := st.FindNodesByNameCtx(ctx, entityName, 1)
				if err == nil && len(results) > 0 {
					summary := r.brain.Summary(projectID, results[0].ID)
					if summary != "" {
						return summary
					}
				}
			}
		}
	}

	// Fallback: raw entity signature from sibling store.
	st := r.getStore(alias)
	if st == nil {
		return ""
	}
	results, err := st.FindNodesByNameCtx(ctx, entityName, 1)
	if err != nil || len(results) == 0 {
		return ""
	}
	if results[0].Signature != "" {
		return results[0].Signature
	}
	return results[0].Name
}

// driftSummaryPrompt is the prompt template for brain-enhanced drift summaries.
const driftSummaryPrompt = `Given this function signature change:
Old: %s
New: %s
Structural diff: %s

Summarize in ONE sentence what changed and how it affects callers. Focus on whether existing callers need updating. Output only the summary sentence, no other text.`

// BrainDriftSummary generates a brain-enhanced drift summary for a changed
// entity. When brain is available and brainGenerate is set, produces a
// human-readable natural-language explanation. When unavailable, uses
// structuralSignatureDiff (the existing graph-based heuristic).
func (r *Resolver) BrainDriftSummary(ctx context.Context, oldSig, newSig, entityName string) string {
	structural := structuralSignatureDiff(oldSig, newSig)

	// If brain generate is unavailable, return structural diff.
	if r.brainGenerate == nil || r.brain == nil || !r.brain.Available() {
		return structural
	}

	// Feed the structural diff to the brain for natural-language explanation.
	// Use a short timeout — brain enhancement is best-effort.
	brainCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	prompt := fmt.Sprintf(driftSummaryPrompt, oldSig, newSig, structural)
	response, err := r.brainGenerate(brainCtx, prompt)
	if err != nil || response == "" {
		return structural // fail-open: brain error → structural diff
	}

	// Clean up the response — remove quotes, trim whitespace.
	response = strings.TrimSpace(response)
	response = strings.Trim(response, `"'`)
	response = strings.TrimSpace(response)

	// Validate: response should be a reasonable prose sentence, not code or garbage.
	if !isValidDriftSummary(response) {
		return structural
	}

	return response
}

// isValidDriftSummary checks that a brain-generated drift summary looks like
// a natural-language sentence, not code, JSON, or garbage. Returns false
// if the response should be discarded in favor of the structural diff.
func isValidDriftSummary(s string) bool {
	if len(s) < 10 || len(s) > 500 {
		return false
	}
	// Must contain at least one space (sentences have spaces).
	if !strings.Contains(s, " ") {
		return false
	}
	// Reject responses that look like code.
	codePrefixes := []string{"{", "func ", "def ", "fn ", "class ", "import ", "package ", "```"}
	for _, prefix := range codePrefixes {
		if strings.HasPrefix(s, prefix) {
			return false
		}
	}
	// Reject responses that are just the signatures echoed back.
	if strings.HasPrefix(s, "Old:") || strings.HasPrefix(s, "New:") {
		return false
	}
	return true
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
