package federation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/store"
)

// DriftDetector handles cross-project dependency drift detection.
// It compares stored dependency signatures against current sibling state
// using a graph-first strategy with git-diff fallback.
type DriftDetector struct {
	resolver *Resolver
	clock    Clock // injected time source for deterministic testing

	mu        sync.RWMutex
	cache     map[string][]DriftAlert // alias → cached drift results (session-level)
	cacheTime map[string]time.Time    // alias → when the cache entry was created
}

func newDriftDetector(r *Resolver) *DriftDetector {
	return &DriftDetector{
		resolver:  r,
		clock:     r.clock,
		cache:     make(map[string][]DriftAlert),
		cacheTime: make(map[string]time.Time),
	}
}

// CheckDrift runs drift detection across all federation entries.
// Results are session-cached per alias.
func (d *DriftDetector) CheckDrift(ctx context.Context, localStore *store.Store) []DriftAlert {
	if localStore == nil || ctx.Err() != nil {
		return nil
	}

	var allAlerts []DriftAlert
	for _, e := range d.resolver.entries {
		if ctx.Err() != nil {
			break
		}

		// Check session-level cache first (with TTL).
		d.mu.RLock()
		cached, hasCached := d.cache[e.Alias]
		fresh := hasCached && d.clock().Sub(d.cacheTime[e.Alias]) < 5*time.Minute
		d.mu.RUnlock()
		if hasCached && !fresh {
			d.mu.Lock()
			delete(d.cache, e.Alias)
			delete(d.cacheTime, e.Alias)
			d.mu.Unlock()
			hasCached = false
		}
		if hasCached {
			allAlerts = append(allAlerts, cached...)
			continue
		}

		alerts := d.checkDriftForEntry(ctx, e, localStore)

		d.mu.Lock()
		d.cache[e.Alias] = alerts
		d.cacheTime[e.Alias] = d.clock()
		if len(d.cache) > 20 {
			var oldest string
			var oldestTime time.Time
			for k, t := range d.cacheTime {
				if oldest == "" || t.Before(oldestTime) {
					oldest = k
					oldestTime = t
				}
			}
			delete(d.cache, oldest)
			delete(d.cacheTime, oldest)
		}
		d.mu.Unlock()

		allAlerts = append(allAlerts, alerts...)
	}
	return allAlerts
}

// InvalidateCache clears the drift result cache.
func (d *DriftDetector) InvalidateCache() {
	d.mu.Lock()
	d.cache = make(map[string][]DriftAlert)
	d.cacheTime = make(map[string]time.Time)
	d.mu.Unlock()
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
func (d *DriftDetector) checkDriftForEntry(ctx context.Context, e config.FederationEntry, localStore *store.Store) []DriftAlert {
	// Get all deps targeting this sibling project.
	deps, err := localStore.GetCrossProjectDepsByProject(e.Alias)
	if err != nil || len(deps) == 0 {
		return nil
	}

	// Step 1: Read sibling's current HEAD.
	currentHead, err := gitRevParseHead(ctx, e.Path)
	if err != nil || currentHead == "" {
		// Not a git repo or git unavailable — fall back to signature comparison.
		return d.checkDriftFallback(ctx, e.Alias, deps)
	}

	// Cache the HEAD for other uses.
	d.resolver.mu.Lock()
	d.resolver.gitHeads[e.Alias] = currentHead
	d.resolver.mu.Unlock()

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
	siblingStore := d.resolver.getStore(e.Alias)
	if siblingStore != nil && d.resolver.isSiblingStoreFresh(ctx, siblingStore, currentHead, e.Path) {
		alerts := d.checkDriftGraphFirst(ctx, e.Alias, currentHead, staleDeps, siblingStore, localStore)
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
		groupAlerts := d.checkDriftForCommitGroup(ctx, e, oldCommit, currentHead, groupDeps, localStore)
		alerts = append(alerts, groupAlerts...)
	}
	return mergeAlerts(alerts)
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
func (d *DriftDetector) checkDriftGraphFirst(
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
func (d *DriftDetector) checkDriftForCommitGroup(
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
		return d.checkDriftFallback(ctx, e.Alias, deps)
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
func (d *DriftDetector) checkDriftFallback(ctx context.Context, alias string, deps []store.CrossProjectDep) []DriftAlert {
	st := d.resolver.getStore(alias)
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

// truncate shortens a string to max characters with an ellipsis suffix.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
