// Package security — engine.go: the pattern matching engine (Sprint 26.7).
//
// The engine applies SecurityPatterns from a PatternSet against the parsed AST
// graph, producing Violations. It is the runtime counterpart to the declarative
// pattern format defined in pattern.go.
//
// Architecture:
//   - Engine is constructed via NewEngine(PatternSet) or DefaultEngine().
//   - CheckFile(g, filePath, content) is the primary entry point; it evaluates
//     all patterns applicable to a single file.
//   - CheckProject(g) evaluates project-scope patterns (CheckTypeCrossTransportAuth).
//   - Per-file context is built once per CheckFile call via buildFileContext and
//     shared across all check algorithms — avoids repeated graph queries.
//   - Every check algorithm returns nil (not empty slice) when no violation fires.
//
// Thread-safety:
//   Engine is immutable after construction and safe for concurrent use.
//   buildFileContext takes only read-locks via the Graph API.
package security

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ──────────────────────────────────────────────────────────────────────────────
// Violation — the output of a pattern match
// ──────────────────────────────────────────────────────────────────────────────

// Violation is a single security finding produced by the pattern matching engine.
// Every field except Evidence and Tags is always populated on a returned violation.
type Violation struct {
	// PatternID is the unique slug of the pattern that fired (e.g. "go-chi-missing-auth").
	PatternID string `json:"pattern_id"`

	// PatternName is the human-readable name of the pattern.
	PatternName string `json:"pattern_name"`

	// Severity determines the required agent response (CRITICAL/HIGH/MEDIUM).
	Severity Severity `json:"severity"`

	// Action is the derived behavioral directive for the agent: "block", "warn", or "inform".
	//   "block"  — CRITICAL: agent must fix before proceeding.
	//   "warn"   — HIGH: strong warning; agent can override with justification.
	//   "inform" — MEDIUM: informational; agent decides.
	// Always set on violations returned by CheckFile, CheckProject, and CheckImports.
	Action string `json:"action"`

	// File is the absolute or repo-relative path of the file containing the violation.
	File string `json:"file"`

	// Target is the specific entity where the violation was found:
	// a route path, function name, variable name, or import path.
	Target string `json:"target"`

	// Message is the natural-language finding with template placeholders filled.
	// Written for an AI agent to act on — never contains raw source code.
	Message string `json:"message"`

	// Evidence is the structural proof: "8/8 handlers have auth, this one doesn't".
	// Always a natural-language string.
	Evidence string `json:"evidence"`

	// Tags from the pattern (e.g. "owasp-a01", "auth", "production-critical").
	Tags []string `json:"tags,omitempty"`
}

// actionForSeverity returns the behavioral directive string for the given severity.
// This is the canonical mapping used by withActions and any handler that needs
// to derive an action from an arch-rule severity string.
func actionForSeverity(s Severity) string {
	switch s {
	case SeverityCritical:
		return "block"
	case SeverityHigh:
		return "warn"
	default: // SeverityMedium and any unknown value
		return "inform"
	}
}

// withActions returns violations with the Action field set from each violation's
// Severity. This is called at the exit boundary of CheckFile, CheckProject, and
// CheckImports so Action is always consistent with Severity — including after any
// in-flight severity downgrades (e.g. CRITICAL→MEDIUM via global suppression).
//
// If violations is nil, returns nil. Never returns an empty slice.
func withActions(violations []Violation) []Violation {
	if len(violations) == 0 {
		return nil
	}
	for i := range violations {
		violations[i].Action = actionForSeverity(violations[i].Severity)
	}
	return violations
}

// ──────────────────────────────────────────────────────────────────────────────
// Engine
// ──────────────────────────────────────────────────────────────────────────────

// Engine applies SecurityPatterns against the parsed graph.
// Construct via NewEngine, DefaultEngine, or DefaultEngineWithDir.
// The zero value is valid but produces no violations.
type Engine struct {
	patterns *PatternSet
	registry *PackageRegistry // optional; nil = skip unknown-package checks
}

// NewEngine creates an engine backed by the given PatternSet.
// A nil PatternSet is valid; CheckFile and CheckProject return nil.
func NewEngine(patterns *PatternSet) *Engine {
	return &Engine{patterns: patterns}
}

// DefaultEngine loads the built-in patterns and returns an engine.
// If loading fails (should never happen in a correctly built binary),
// returns an engine with no patterns. Never returns nil.
func DefaultEngine() *Engine {
	ps, err := LoadBuiltin()
	if err != nil {
		return &Engine{patterns: newPatternSet(nil)}
	}
	return &Engine{patterns: ps}
}

// DefaultEngineWithDir loads built-in patterns merged with user patterns
// from extraDir. Falls back to built-ins only if extraDir loading fails.
// Never returns nil.
func DefaultEngineWithDir(extraDir string) *Engine {
	ps, err := LoadAll(extraDir)
	if err != nil {
		ps, _ = LoadBuiltin() // nolint:errcheck — already handled above
	}
	if ps == nil {
		ps = newPatternSet(nil)
	}
	return &Engine{patterns: ps}
}

// CheckFile runs all applicable patterns against filePath using the graph.
//
// content is the raw source file bytes. Pass nil to skip content-based checks:
// CheckTypeHardcodedSecret requires content and is silently skipped when content
// is nil. All graph-based checks run regardless of content.
//
// Returns nil if no violations are found. The returned slice is never empty.
func (e *Engine) CheckFile(g *graph.Graph, filePath string, content []byte) []Violation {
	if e == nil || e.patterns == nil || g == nil || filePath == "" {
		return nil
	}

	lang := languageFromPath(filePath)
	applicable := e.patterns.ForLanguage(lang)
	if len(applicable) == 0 {
		return nil
	}

	// Build per-file context once; shared across all check algorithms.
	fc := buildFileContext(g, filePath)

	var violations []Violation
	for _, p := range applicable {
		if !p.IsEnabled() {
			continue
		}

		// Framework gate: if FrameworkIdentifiers are specified, this file MUST
		// import at least one matching package to be eligible.
		// This is the zero-false-positive guarantee: chi patterns never fire on
		// files that don't use chi.
		if len(p.Detection.FrameworkIdentifiers) > 0 {
			if !fc.importsAny(p.Detection.FrameworkIdentifiers) {
				continue
			}
		}

		var found []Violation
		switch p.Detection.CheckType {
		case CheckTypeDirectImport:
			found = checkDirectImport(fc, p)
		case CheckTypeMissingMiddleware:
			found = checkMissingMiddleware(fc, p, g)
		case CheckTypeMissingAnnotation:
			found = checkMissingAnnotation(fc, p)
			// Global suppression: if any project file imports a known global-auth
			// config identifier (e.g. Spring SecurityFilterChain), downgrade CRITICAL
			// → MEDIUM. The project likely has auth enforced globally; we surface a
			// MEDIUM to prompt coverage verification rather than blocking.
			if len(found) > 0 && len(p.Detection.GlobalSuppressionIdentifiers) > 0 {
				if projectImportsAny(g, p.Detection.GlobalSuppressionIdentifiers) {
					for i := range found {
						if found[i].Severity == SeverityCritical {
							found[i].Severity = SeverityMedium
							found[i].Message += " (Downgraded: global auth config detected in project — verify SecurityFilterChain or equivalent covers all controller paths.)"
						}
					}
				}
			}
		case CheckTypeHardcodedSecret:
			if content != nil {
				found = checkHardcodedSecret(fc, p, content)
			}
		case CheckTypeAdminElevation:
			found = checkAdminElevation(fc, p)
		case CheckTypeCrossTransportAuth:
			// Project-scope check: skipped per-file.
			// Use CheckProject for cross-transport analysis.
		}
		violations = append(violations, found...)
	}

	if len(violations) == 0 {
		return nil
	}
	return withActions(violations)
}

// CheckProject runs project-scope patterns against the entire graph.
// Currently dispatches: CheckTypeCrossTransportAuth, CheckTypeLayerMapping.
// Per-file patterns are NOT run here — use CheckFile for those.
// Returns nil if no violations are found.
func (e *Engine) CheckProject(g *graph.Graph) []Violation {
	if e == nil || e.patterns == nil || g == nil {
		return nil
	}
	var violations []Violation
	for _, p := range e.patterns.ForCheckType(CheckTypeCrossTransportAuth) {
		if p.IsEnabled() {
			found := checkCrossTransportAuth(g, p)
			violations = append(violations, found...)
		}
	}
	for _, p := range e.patterns.ForCheckType(CheckTypeLayerMapping) {
		if p.IsEnabled() {
			found := checkLayerMapping(g, p)
			violations = append(violations, found...)
		}
	}
	if len(violations) == 0 {
		return nil
	}
	return withActions(violations)
}

// CheckNorms observes call patterns shared across route-registering sibling files
// and reports when the target file deviates from an established norm.
//
// A "norm" is a function call made by a statistically significant proportion of
// route-registering files (files with NodeRoute nodes) in the same package. When a
// new route-registering file omits a normed call, a violation fires — even without
// any explicit security rule covering that function.
//
// This is the Sprint 27.5 complement to CheckFile: CheckFile enforces explicit
// patterns; CheckNorms enforces observed convention.
//
// Confidence tiers:
//
//	HIGH   — call in ≥75% of sibling route files AND at least 3 sibling route files
//	MEDIUM — call in ≥50% of sibling route files AND at least 2 sibling route files
//
// Returns nil when:
//   - the target file has no NodeRoute nodes (not a route-registering file)
//   - fewer than 2 sibling route files exist in the same directory
//   - no call pattern reaches the minimum confidence threshold
//   - the file is a test file
func (e *Engine) CheckNorms(g *graph.Graph, filePath string) []Violation {
	if e == nil || g == nil || filePath == "" {
		return nil
	}
	if isTestFile(filePath) {
		return nil
	}

	// Build context for the target file. Only route-registering files are checked.
	fc := buildFileContext(g, filePath)
	if len(fc.routes) == 0 {
		return nil
	}

	lang := languageFromPath(filePath)
	dir := filepath.Dir(filePath)
	if dir == "." {
		// filePath is a bare filename with no directory (e.g. "handler.go").
		// No meaningful package scope can be inferred — skip norm detection.
		return nil
	}

	// normFreq tracks how many sibling route files exhibit each norm.
	// Keys are either plain function names (call norms, e.g. "AuthMiddleware") or
	// @Annotation strings (annotation norms, e.g. "@PreAuthorize"). The @ prefix
	// distinguishes annotation norms from call norms and prevents key collisions.
	normFreq := make(map[string]int)
	siblingRouteCount := 0
	siblingFilesExamined := 0
	seen := make(map[string]bool)
	seen[filePath] = true // exclude target from sibling scan

	const maxSiblingFiles = 30
	g.IterateNodes(func(n *graph.Node) {
		if siblingFilesExamined >= maxSiblingFiles {
			return
		}
		if n.Type != graph.NodeFile {
			return
		}
		if seen[n.File] {
			return
		}
		if filepath.Dir(n.File) != dir {
			return
		}
		if languageFromPath(n.File) != lang {
			return
		}
		seen[n.File] = true
		siblingFilesExamined++

		sib := buildFileContext(g, n.File)
		if len(sib.routes) == 0 {
			return // not a route-registering file; don't contribute to norms
		}
		siblingRouteCount++

		// Call-based norms: functions called by any function in this route file.
		for callee := range sib.callees {
			normFreq[callee]++
		}

		// Annotation-based norms: @Annotation patterns in function signatures.
		// These catch Java @PreAuthorize/@Secured/@RolesAllowed, Python @login_required,
		// and TypeScript @UseGuards — annotations that appear in Metadata["signature"]
		// rather than as CALLS edges.
		for _, fn := range sib.nodes {
			if fn.Type != graph.NodeFunction && fn.Type != graph.NodeMethod {
				continue
			}
			if sig, ok := fn.Metadata["signature"]; ok {
				for _, annot := range extractAnnotationsFromSig(sig) {
					normFreq[annot]++
				}
			}
		}
	})

	if siblingRouteCount < 2 {
		return nil // not enough route files to establish a norm
	}

	// Build the target's annotation set from function signature metadata.
	// Used to check annotation-based norms (complementing fc.callees for call norms).
	targetAnnotations := make(map[string]bool)
	for _, n := range fc.nodes {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if sig, ok := n.Metadata["signature"]; ok {
			for _, annot := range extractAnnotationsFromSig(sig) {
				targetAnnotations[annot] = true
			}
		}
	}

	// Find norms that are "universal" across sibling route files but absent in target.
	// Exactly 50% (e.g. 2/4) is treated as below the MEDIUM threshold — a 50/50
	// split is too ambiguous to constitute an established norm.
	var violations []Violation
	for normKey, count := range normFreq {
		ratio := float64(count) / float64(siblingRouteCount)

		var severity Severity
		switch {
		case ratio >= 0.75 && siblingRouteCount >= 3:
			severity = SeverityHigh
		case ratio > 0.50 && siblingRouteCount >= 2:
			severity = SeverityMedium
		default:
			continue // below minimum confidence threshold
		}

		// Check whether the target is missing this norm.
		// Annotation norms (key starts with '@') use the targetAnnotations set;
		// call norms use the fc.callees map.
		isAnnotation := strings.HasPrefix(normKey, "@")
		if isAnnotation {
			if targetAnnotations[normKey] {
				continue
			}
		} else {
			if fc.callees[normKey] {
				continue
			}
		}

		var evidence, msg string
		if isAnnotation {
			evidence = fmt.Sprintf(
				"%d/%d route-registering file(s) in this package annotate handlers with %q — this file registers routes but does not",
				count, siblingRouteCount, normKey,
			)
			msg = fmt.Sprintf(
				"%s registers routes but no handler is annotated with %q, which appears in %d/%d other route-registering file(s). "+
					"If %q is a security annotation (auth, role enforcement), verify this omission is intentional.",
				filepath.Base(filePath), normKey, count, siblingRouteCount, normKey,
			)
		} else {
			evidence = fmt.Sprintf(
				"%d/%d route-registering file(s) in this package call %q — this file registers routes but does not",
				count, siblingRouteCount, normKey,
			)
			msg = fmt.Sprintf(
				"%s registers routes but does not call %q, which %d/%d other route-registering file(s) in this package call. "+
					"If %q is a security middleware (auth, rate limiting, CSRF), verify this omission is intentional.",
				filepath.Base(filePath), normKey, count, siblingRouteCount, normKey,
			)
		}

		violations = append(violations, Violation{
			PatternID:   "norm:" + normKey,
			PatternName: fmt.Sprintf("Observed norm — %q in %d/%d route file(s)", normKey, count, siblingRouteCount),
			Severity:    severity,
			File:        filePath,
			Target:      filepath.Base(filePath),
			Message:     msg,
			Evidence:    evidence,
		})
	}

	if len(violations) == 0 {
		return nil
	}

	// Sort for deterministic output: HIGH first, then MEDIUM; within tier, alphabetical.
	// CheckNorms only assigns SeverityHigh or SeverityMedium — no CRITICAL branch needed.
	sort.Slice(violations, func(i, j int) bool {
		si, sj := violations[i].Severity, violations[j].Severity
		if si != sj {
			return si == SeverityHigh // HIGH sorts before MEDIUM
		}
		return violations[i].PatternID < violations[j].PatternID
	})

	return withActions(violations)
}

// ──────────────────────────────────────────────────────────────────────────────
// Layer mapping (Sprint 27.6)
// ──────────────────────────────────────────────────────────────────────────────

// defaultLayerConfig returns the built-in 3-tier architectural layer hierarchy.
//
// Keywords are conservative by design: they avoid segments that commonly appear
// in external library import paths (e.g. "database" appears in "database/sql"
// but is excluded here because external DB libraries are already caught by
// checkDirectImport with explicit forbidden_import_patterns).
//
// Layer ordering (index 0 = outermost, index 2 = innermost):
//
//	presentation (0) → service (1) → data (2)
//
// A violation fires when a file in layer N imports a package in layer N+2 or
// deeper (skipping at least one intermediate layer).
func defaultLayerConfig() []LayerDef {
	return []LayerDef{
		{
			Name: "presentation",
			Keywords: []string{
				"handler", "handlers",
				"api",
				"controller", "controllers",
				"route", "routes",
				"transport",
			},
		},
		{
			Name: "service",
			Keywords: []string{
				"service", "services",
				"usecase", "usecases",
				"business",
				"domain",
			},
		},
		{
			Name: "data",
			Keywords: []string{
				"repo", "repository", "repositories",
				"dal",
			},
		},
	}
}

// inferLayerFromPath determines which architectural layer a path belongs to by
// checking each slash-delimited segment against the layer keyword lists.
// Returns the layer name and its zero-based index in layers, or ("", -1) when
// no keyword matches (unknown layer — callers should skip the path).
//
// Only exact segment matches are accepted: "repository" matches a path containing
// ".../repository/..." but NOT ".../user-repository/..." or ".../repo-factory/...".
// Matching is case-insensitive.
//
// When multiple segments match different layers, the deepest matching layer
// (highest index) takes precedence — the last directory segment typically best
// identifies the package's role.
func inferLayerFromPath(p string, layers []LayerDef) (string, int) {
	// Normalise to forward slashes for consistent segment splitting.
	p = filepath.ToSlash(p)
	// Strip file extension from the last segment so "handler.go" → "handler".
	if idx := strings.LastIndexByte(p, '.'); idx > strings.LastIndexByte(p, '/') {
		p = p[:idx]
	}
	segments := strings.Split(p, "/")

	bestName := ""
	bestIdx := -1
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		seg = strings.ToLower(seg)
		for li, layer := range layers {
			for _, kw := range layer.Keywords {
				if seg == strings.ToLower(kw) {
					// Take the deepest matching layer in the hierarchy.
					if li > bestIdx {
						bestIdx = li
						bestName = layer.Name
					}
				}
			}
		}
	}
	return bestName, bestIdx
}

// checkLayerMapping fires when a file in one architectural layer imports a
// package from a non-adjacent (skipped) layer.
//
// The check is project-scope: it iterates all NodeFile nodes in the graph,
// infers each file's layer from its path, then checks each imported NodePackage
// for a layer classification. A violation fires when the import's layer index
// exceeds the file's layer index by more than 1 (a skip).
//
// At most maxLayerViolations violations are returned to avoid overwhelming
// response payloads in monorepos with widespread layering issues.
//
// Thread-safety: reads the graph under read-locks via the public Graph API.
func checkLayerMapping(g *graph.Graph, p SecurityPattern) []Violation {
	layers := p.Detection.LayerConfig
	if len(layers) == 0 {
		layers = defaultLayerConfig()
	}

	const maxLayerViolations = 50

	// seen deduplicates (filePath, importPath) pairs — a single IMPORTS edge is
	// enough to fire, but graph traversal may encounter duplicates.
	type dedupKey struct{ file, imp string }
	seen := make(map[dedupKey]bool)

	var violations []Violation

	g.IterateNodes(func(n *graph.Node) {
		if len(violations) >= maxLayerViolations {
			return
		}
		if n.Type != graph.NodeFile {
			return
		}

		filePath := n.File
		if filePath == "" {
			return
		}
		// Skip test files — layer violations in tests are expected (tests import
		// across layers by necessity).
		if isTestFile(filePath) {
			return
		}
		// Skip vendor/generated directories.
		if isVendoredPath(filePath) {
			return
		}

		// Determine the file's layer from its path.
		_, srcIdx := inferLayerFromPath(filePath, layers)
		if srcIdx < 0 {
			return // unknown layer — cannot make a valid judgement
		}
		srcLayerName := layers[srcIdx].Name

		// Walk IMPORTS edges from this NodeFile to NodePackage nodes.
		for _, e := range g.OutEdges(n.ID) {
			if len(violations) >= maxLayerViolations {
				return
			}
			if e.Type != graph.EdgeImports {
				continue
			}
			imp := g.GetNode(e.To)
			if imp == nil || imp.Type != graph.NodePackage {
				continue
			}
			importPath := imp.Name
			if importPath == "" {
				continue
			}

			// Dedup (file, import) to avoid repeated violations for the same pair.
			key := dedupKey{filePath, importPath}
			if seen[key] {
				continue
			}
			seen[key] = true

			// Determine the import's layer from its path.
			_, dstIdx := inferLayerFromPath(importPath, layers)
			if dstIdx < 0 {
				continue // import is from an unknown layer — skip
			}

			// A skip violation: destination layer is more than one step deeper.
			// e.g. presentation (0) → data (2): skip = 2 - 0 = 2 > 1 → violation.
			// Adjacent: presentation (0) → service (1): 1 - 0 = 1 → fine.
			// Reverse: service (1) → presentation (0): fine (different concern,
			//   lower severity; reverse dependency is a design smell but not this check's scope).
			if dstIdx <= srcIdx+1 {
				continue
			}

			dstLayerName := layers[dstIdx].Name

			// Build natural-language violation.
			// srcIdx+1 < len(layers) is guaranteed: the violation condition
			// dstIdx > srcIdx+1 requires dstIdx ≥ srcIdx+2, and dstIdx is a
			// valid layer index (< len(layers)), so srcIdx+1 < len(layers).
			skippedLayer := layers[srcIdx+1].Name

			base := filepath.Base(filePath)
			evidence := fmt.Sprintf(
				"%s is in the %s layer but directly imports %q which is in the %s layer, "+
					"skipping the %s layer. Route this access through the %s layer instead.",
				base, srcLayerName, importPath, dstLayerName,
				skippedLayer, skippedLayer,
			)

			msg := fillTemplate(p.Message, map[string]string{
				"file":        filePath,
				"target":      importPath,
				"src_layer":   srcLayerName,
				"dst_layer":   dstLayerName,
				"skip_layer":  skippedLayer,
			})

			violations = append(violations, Violation{
				PatternID:   p.ID,
				PatternName: p.Name,
				Severity:    p.Severity,
				File:        filePath,
				Target:      importPath,
				Message:     msg,
				Evidence:    evidence,
				Tags:        p.Tags,
			})
		}
	})

	return nilIfEmpty(violations)
}

// PatternCount returns the total number of patterns in the engine.
func (e *Engine) PatternCount() int {
	if e == nil || e.patterns == nil {
		return 0
	}
	return e.patterns.Len()
}

// ──────────────────────────────────────────────────────────────────────────────
// fileContext — per-file graph data, built once per CheckFile call
// ──────────────────────────────────────────────────────────────────────────────

// fileContext caches graph data for a single file, shared by all check algorithms.
type fileContext struct {
	g        *graph.Graph
	filePath string
	nodes    []*graph.Node // all nodes whose File == filePath
	imports  []*graph.Node // NodePackage nodes this file imports (via IMPORTS edges)
	// callees maps callee node Name → true for all CALLS edges from any node in this file.
	// Function-level (not per-node) — sufficient for file-scope auth checks.
	callees map[string]bool
	routes  []*graph.Node // NodeRoute nodes in this file
}

// buildFileContext constructs the fileContext by issuing a single FindByFile
// call and walking the resulting nodes' outgoing edges under read-locks.
func buildFileContext(g *graph.Graph, filePath string) *fileContext {
	fc := &fileContext{
		g:        g,
		filePath: filePath,
		callees:  make(map[string]bool),
	}

	nodes := g.FindByFile(filePath)
	fc.nodes = nodes

	for _, n := range nodes {
		switch n.Type {
		case graph.NodeFile:
			// Collect NodePackage nodes via IMPORTS edges from the file node.
			for _, e := range g.OutEdges(n.ID) {
				if e.Type == graph.EdgeImports {
					if imp := g.GetNode(e.To); imp != nil && imp.Type == graph.NodePackage {
						fc.imports = append(fc.imports, imp)
					}
				}
			}

		case graph.NodeRoute:
			fc.routes = append(fc.routes, n)

		case graph.NodeFunction, graph.NodeMethod:
			// Collect callee names from CALLS edges.
			for _, e := range g.OutEdges(n.ID) {
				if e.Type == graph.EdgeCalls {
					if callee := g.GetNode(e.To); callee != nil {
						fc.callees[callee.Name] = true
					}
				}
			}
		}
	}

	return fc
}

// importsAny reports whether any imported package name matches any of the
// identifier patterns. Identifiers can be exact import paths or glob patterns
// (e.g. "github.com/go-chi/chi/*" matches "github.com/go-chi/chi/v5").
func (fc *fileContext) importsAny(identifiers []string) bool {
	for _, imp := range fc.imports {
		for _, id := range identifiers {
			if matchGlob(id, imp.Name) {
				return true
			}
		}
	}
	return false
}

// projectImportsAny reports whether ANY file in the graph imports a package
// matching any of the identifier patterns. Used to detect project-level auth
// configuration (e.g. a Spring SecurityFilterChain bean) that may cover all
// controllers without per-controller annotations.
func projectImportsAny(g *graph.Graph, identifiers []string) bool {
	if len(identifiers) == 0 {
		return false
	}
	for _, pkg := range g.FindByType(graph.NodePackage) {
		for _, id := range identifiers {
			if matchGlob(id, pkg.Name) {
				return true
			}
		}
	}
	return false
}

// callsAny reports whether any function in this file calls a function whose
// name matches any of the glob patterns.
func (fc *fileContext) callsAny(patterns []string) bool {
	for callee := range fc.callees {
		for _, p := range patterns {
			if matchGlob(p, callee) {
				return true
			}
		}
	}
	return false
}

// signaturesMatchAny reports whether any function or method node in this file
// has a Metadata["signature"] that matches any of the glob patterns.
// Used to detect auth types that appear as handler parameter types (e.g. Rust
// extractors/guards) rather than as function calls.
func (fc *fileContext) signaturesMatchAny(patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, n := range fc.nodes {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		sig, ok := n.Metadata["signature"]
		if !ok || sig == "" {
			continue
		}
		for _, pat := range patterns {
			if matchGlob(pat, sig) {
				return true
			}
		}
	}
	return false
}

// hasRouteRegistrations reports whether this file appears to register routes.
// Checked via: (1) NodeRoute nodes detected by the heuristic pass, or
// (2) CALLS edges to functions matching the routeNodeNames list.
func (fc *fileContext) hasRouteRegistrations(routeNodeNames []string) bool {
	if len(fc.routes) > 0 {
		return true
	}
	return fc.callsAny(routeNodeNames)
}

// ──────────────────────────────────────────────────────────────────────────────
// Check algorithms
// ──────────────────────────────────────────────────────────────────────────────

// checkDirectImport fires when a file that matches HandlerFilePatterns directly
// imports a package matching ForbiddenImportPatterns.
//
// This detects layer violations where a handler/controller accesses the data
// layer directly instead of going through a repository or service.
func checkDirectImport(fc *fileContext, p SecurityPattern) []Violation {
	// Step 1: Does this file path match the handler file patterns?
	// Empty HandlerFilePatterns means "match all files".
	if len(p.Detection.HandlerFilePatterns) > 0 {
		if !fileMatchesAny(fc.filePath, p.Detection.HandlerFilePatterns) {
			return nil
		}
	}

	// Step 2: Does this file import any forbidden package?
	var violations []Violation
	for _, imp := range fc.imports {
		for _, forbidden := range p.Detection.ForbiddenImportPatterns {
			if matchGlob(forbidden, imp.Name) {
				msg := fillTemplate(p.Message, map[string]string{
					"file":   fc.filePath,
					"target": imp.Name,
				})
				violations = append(violations, Violation{
					PatternID:   p.ID,
					PatternName: p.Name,
					Severity:    p.Severity,
					File:        fc.filePath,
					Target:      imp.Name,
					Message:     msg,
					Evidence:    fmt.Sprintf("%s matches a handler path pattern and directly imports %q — bypass the data layer via a repository or service instead", filepath.Base(fc.filePath), imp.Name),
					Tags:        p.Tags,
				})
				break // one violation per import, not per pattern
			}
		}
	}
	return nilIfEmpty(violations)
}

// checkMissingMiddleware fires when a file registers routes but no function in
// the file calls any required call pattern (e.g. auth middleware).
//
// Scope: file-level. A file that registers routes but has zero auth calls
// is the primary target. Evidence is enriched by counting sibling files in
// the same package that do call auth, producing "N/M files have auth, this one doesn't".
func checkMissingMiddleware(fc *fileContext, p SecurityPattern, g *graph.Graph) []Violation {
	// Must register routes to be relevant.
	if !fc.hasRouteRegistrations(p.Detection.RouteNodeNames) {
		return nil
	}

	// If any function in this file calls a required auth pattern, no violation.
	if fc.callsAny(p.Detection.RequiredCallPatterns) {
		return nil
	}

	// If any function signature contains a required auth type (extractor/guard),
	// no violation. This catches Rust-style auth where the auth type appears as a
	// handler parameter (e.g. "fn handle(user: AuthUser)") rather than a call site.
	if fc.signaturesMatchAny(p.Detection.RequiredSignaturePatterns) {
		return nil
	}

	// Also check for middleware application via Use/Group/With calls.
	// If MiddlewareNodeNames is configured and the file calls them, it might be
	// applying middleware — but without knowing which middleware was applied, we
	// still fire the violation as a reminder to confirm auth is included.
	// (Per-route precision is outside scope for Sprint 26.7.)

	// Build evidence: count sibling files in same directory that DO call auth.
	authCount, totalSiblings := countSiblingsWithCall(g, fc.filePath, p.Detection.RequiredCallPatterns)
	var evidence string
	switch {
	case totalSiblings == 0:
		evidence = "This file registers routes but does not call any required auth pattern"
	case authCount == 0:
		evidence = fmt.Sprintf("No file in this package calls an auth pattern; this file registers routes without auth (%d sibling(s) checked)", totalSiblings)
	default:
		evidence = fmt.Sprintf("%d/%d other file(s) in this package call an auth function — this file registers routes without one", authCount, totalSiblings)
	}

	target := filepath.Base(fc.filePath)
	msg := fillTemplate(p.Message, map[string]string{
		"file":   fc.filePath,
		"target": target,
		"count":  fmt.Sprint(authCount),
		"total":  fmt.Sprint(totalSiblings),
	})

	return []Violation{{
		PatternID:   p.ID,
		PatternName: p.Name,
		Severity:    p.Severity,
		File:        fc.filePath,
		Target:      target,
		Message:     msg,
		Evidence:    evidence,
		Tags:        p.Tags,
	}}
}

// checkMissingAnnotation fires when handler functions in the file do not have
// required annotations. Annotations are detected via CALLS edges (decorators
// resolved as call sites) and node metadata (signature field).
//
// This primarily targets Java Spring (@PreAuthorize) and Python FastAPI (Depends).
func checkMissingAnnotation(fc *fileContext, p SecurityPattern) []Violation {
	// Scope to handler files when HandlerFilePatterns is configured.
	if len(p.Detection.HandlerFilePatterns) > 0 {
		if !fileMatchesAny(fc.filePath, p.Detection.HandlerFilePatterns) {
			return nil
		}
	}
	if len(p.Detection.AnnotationPatterns) == 0 {
		return nil
	}

	// Check at file-level: if ANY annotation pattern is called from this file,
	// we assume the annotations are present. Per-function precision would require
	// per-function call-graph analysis and is deferred to Sprint 28 (LSP).
	if fc.callsAny(p.Detection.AnnotationPatterns) {
		return nil
	}

	// Also check function metadata.signatures for annotation markers.
	for _, n := range fc.nodes {
		if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
			continue
		}
		if sig, ok := n.Metadata["signature"]; ok {
			for _, annotPat := range p.Detection.AnnotationPatterns {
				if matchGlob(annotPat, sig) {
					return nil // at least one function has the annotation in its signature
				}
			}
		}
	}

	// Check struct/class nodes for heritage_extends metadata (class-based auth mixins).
	// Python CBV patterns like `class MyView(LoginRequiredMixin, View)` store base class
	// names in metadata["heritage_extends"] as a comma-separated list. This is the only
	// way to detect mixin-based auth without LSP — CALLS edges are not emitted for base
	// class references.
	for _, n := range fc.nodes {
		if n.Type != graph.NodeStruct {
			continue
		}
		he, ok := n.Metadata["heritage_extends"]
		if !ok || he == "" {
			continue
		}
		for _, base := range strings.Split(he, ",") {
			base = strings.TrimSpace(base)
			for _, annotPat := range p.Detection.AnnotationPatterns {
				if matchGlob(annotPat, base) {
					return nil // class inherits from an auth mixin
				}
			}
		}
	}

	target := filepath.Base(fc.filePath)
	msg := fillTemplate(p.Message, map[string]string{
		"file":   fc.filePath,
		"target": target,
	})
	return []Violation{{
		PatternID:   p.ID,
		PatternName: p.Name,
		Severity:    p.Severity,
		File:        fc.filePath,
		Target:      target,
		Message:     msg,
		Evidence:    fmt.Sprintf("No function in %s calls any required annotation pattern (%s)", filepath.Base(fc.filePath), strings.Join(p.Detection.AnnotationPatterns, ", ")),
		Tags:        p.Tags,
	}}
}

// credentialVarRE matches variable names that commonly hold credentials.
// Uses substring matching (no word boundaries) so it catches both snake_case and camelCase:
// "jwtSecret", "apiKey", "access_key", "accessKey", "connectionString", etc.
// Multi-word terms use [_]? to match with or without underscore separator.
var credentialVarRE = regexp.MustCompile(
	`(?i)(secret|password|passwd|passphrase|bearer|credential|token|jwt|dsn|` +
		`api[_]?key|api[_]?secret|auth[_]?key|` +
		`access[_]?key|client[_]?secret|private[_]?key|` +
		`signing[_]?key|encryption[_]?key|webhook[_]?secret|` +
		`database[_]?url|db[_]?url|db[_]?pass(?:word|wd)?|db[_]?pwd|db[_]?password|` +
		`conn(?:ection)?[_]?str(?:ing)?)`,
)

// stringLiteralRE captures the value of a string literal assignment.
// Matches: varname = "value", varname := "value", varname = 'value' (single-quote for
// Python/TypeScript/JS), or varname = `value` (backtick for Go).
// The variable name is in group 1, the string value in group 2.
var stringLiteralRE = regexp.MustCompile(
	`\b(\w+)\s*:?=\s*["'` + "`" + `]([^"'` + "`" + `\r\n]{6,})["'` + "`" + `]`,
)

// ── Fallback-secret detection regexes ────────────────────────────────────────

// goEmptyCheckRE detects: if varName == "" {
// Used in the multi-line Go fallback scan.
var goEmptyCheckRE = regexp.MustCompile(`if\s+(\w+)\s*==\s*""`)

// fallbackAssignRE detects a bare assignment of a string literal: varName = "value".
// Used to find the hardcoded assignment inside an empty-check block.
var fallbackAssignRE = regexp.MustCompile(
	`\b(\w+)\s*=\s*["'` + "`" + `]([^"'` + "`" + `\r\n]{1,})["'` + "`" + `]`,
)

// jsFallbackRE detects: process.env.VAR || "fallback"  and  process.env.VAR ?? "fallback"
var jsFallbackRE = regexp.MustCompile(
	`\bprocess\.env\.(\w+)\s*(?:\|\||\?\?)\s*["']([^"'\r\n]{1,})["']`,
)

// pyGetenvFallbackRE detects: os.environ.get("VAR", "fallback") or os.getenv("VAR", "fallback")
// Group 1: env var name (from the quoted first arg).  Group 2: fallback value.
var pyGetenvFallbackRE = regexp.MustCompile(
	`os\.(?:environ\.get|getenv)\s*\(\s*["']([^"'\r\n]+)["']\s*,\s*["']([^"'\r\n]{1,})["']\s*\)`,
)

// pyOrFallbackRE detects: os.environ.get("VAR") or "fallback"  /  os.getenv("VAR") or "fallback"
// Group 1: env var name (from the quoted first arg).  Group 2: fallback value.
var pyOrFallbackRE = regexp.MustCompile(
	`os\.(?:environ\.get|getenv)\s*\(\s*["']([^"'\r\n]+)["'][^)]*\)\s*or\s*["']([^"'\r\n]{1,})["']`,
)

// javaEnvFallbackRE detects: System.getenv("VAR") != null ? ... : "fallback"
// and  Optional.ofNullable(System.getenv("VAR")).orElse("fallback")
// Note: Optional.ofNullable(System.getenv("X")) has two closing parens before .orElse —
// the inner ) closes getenv("X") and the outer ) closes ofNullable(...).
var javaEnvFallbackRE = regexp.MustCompile(
	`(?:System\.getenv\s*\([^)]+\)\s*!=\s*null[^:]*:[^"']*|` +
		`Optional\.ofNullable\s*\(\s*System\.getenv[^)]*\)\s*\)\.orElse\s*\(\s*)` +
		`["']([^"'\r\n]{1,})["']`,
)

// rustEnvFallbackRE detects two forms:
//
//	unwrap_or("fallback")               — direct string argument
//	unwrap_or_else(|param| "fallback")  — closure returning a string literal directly
//
// Also matches std::env::var("VAR").* since "env::var" is a suffix match.
// Does NOT match unwrap_or_else(|_| String::from("fallback")) — the string is nested
// inside a function call, not a direct closure return value.
var rustEnvFallbackRE = regexp.MustCompile(
	`env::var\s*\([^)]+\)\.unwrap_or(?:_else\s*\(\s*\|[^|]*\|\s*|\s*\(\s*)["']([^"'\r\n]{1,})["']`,
)

// placeholderRE matches string values that are obviously placeholder / demo credentials,
// not real secrets. Used by isPlaceholderValue to reduce false positives.
var placeholderRE = regexp.MustCompile(
	`(?i)^(test|example|placeholder|dummy|fake|changeme|change.me|` +
		`your[._-]|enter[._-]your|replace[._-]with|insert[._-]your|add[._-]your|` +
		`sample|default|none|null|todo|fixme|redacted|` +
		`dev[._-]secret|development[._-]|local[._-]|` +
		`password123|admin123|qwerty|letmein|` +
		`x{4,}|0{8,}|1{8,}|a{4,})`,
)

// checkHardcodedSecret scans file content for hardcoded credentials.
// A violation fires when a variable with a credential-suggesting name is assigned
// a string literal that matches a secret value pattern AND the value is not an
// obvious placeholder (see isPlaceholderValue).
//
// Test files (_test.go, testdata/, fixtures/, etc.) have their severity downgraded
// to MEDIUM since test fixtures commonly use dummy credentials.
//
// When Detection.DetectFallback is true, also scans for the "load from env with
// hardcoded fallback" anti-pattern using language-appropriate heuristics.
func checkHardcodedSecret(fc *fileContext, p SecurityPattern, content []byte) []Violation {
	if len(p.Detection.SecretPatterns) == 0 && !p.Detection.DetectFallback {
		return nil
	}

	// Compile secret value patterns once.
	var valueREs []*regexp.Regexp
	for _, pat := range p.Detection.SecretPatterns {
		if re, err := regexp.Compile(pat); err == nil {
			valueREs = append(valueREs, re)
		}
	}

	// Downgrade severity for test files — test fixtures use dummy credentials.
	severity := p.Severity
	if isTestFile(fc.filePath) {
		severity = SeverityMedium
	}

	var violations []Violation
	lines := strings.Split(string(content), "\n")

	// Pass 1: string literal assignment check (requires SecretPatterns).
	if len(valueREs) > 0 {
		for lineNum, line := range lines {
			// Quick pre-filter: skip lines that don't have a string delimiter.
			if !strings.ContainsAny(line, `"'`+"`") {
				continue
			}
			// Skip blank/comment lines and import blocks.
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "//") ||
				strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "/*") ||
				strings.HasPrefix(trimmed, "*") {
				continue
			}
			if strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, `"`) {
				continue
			}

			// Find string literal assignments on this line.
			matches := stringLiteralRE.FindAllStringSubmatch(line, -1)
			for _, m := range matches {
				if len(m) < 3 {
					continue
				}
				varName := m[1]
				value := m[2]

				// Both conditions must hold: credential variable name AND value pattern.
				if !credentialVarRE.MatchString(varName) {
					continue
				}
				if isPlaceholderValue(value) {
					continue
				}
				for _, valueRE := range valueREs {
					if valueRE.MatchString(value) {
						msg := fillTemplate(p.Message, map[string]string{
							"file":   fc.filePath,
							"target": varName,
						})
						violations = append(violations, Violation{
							PatternID:   p.ID,
							PatternName: p.Name,
							Severity:    severity,
							File:        fc.filePath,
							Target:      varName,
							Message:     msg,
							Evidence:    fmt.Sprintf("Line %d: variable %q is assigned a string literal matching a credential pattern", lineNum+1, varName),
							Tags:        p.Tags,
						})
						break // one violation per variable, not per pattern
					}
				}
			}
		}
	}

	// Pass 2: fallback-secret pattern check.
	if p.Detection.DetectFallback {
		fallbackViolations := checkFallbackEnvPattern(fc, p, lines, severity)
		violations = append(violations, fallbackViolations...)
	}

	return nilIfEmpty(violations)
}

// checkFallbackEnvPattern detects the "load from environment with hardcoded fallback"
// anti-pattern. A real credential is never in source — but a hardcoded fallback value
// IS in source, and is used whenever the environment variable is missing.
//
// Detection is language-aware:
//   - Go:     multi-line: `if varName == "" { varName = "fallback" }` near a credential var
//   - TS/JS:  single-line: `process.env.VAR || "fallback"`  or  `process.env.VAR ?? "fallback"`
//   - Python: single-line: `os.environ.get("VAR", "fallback")`  or  `os.getenv("VAR") or "fallback"`
//   - Java:   single-line: `System.getenv("VAR") != null ? ... : "fallback"`
//   - Rust:   single-line: `env::var("VAR").unwrap_or("fallback")`
func checkFallbackEnvPattern(fc *fileContext, p SecurityPattern, lines []string, severity Severity) []Violation {
	lang := languageFromPath(fc.filePath)
	var violations []Violation

	switch lang {
	case "go":
		// Multi-line scan: look for `if credVar == "" {` followed within 4 lines by `credVar = "literal"`.
		for i, line := range lines {
			emptyM := goEmptyCheckRE.FindStringSubmatch(line)
			if emptyM == nil {
				continue
			}
			varName := emptyM[1]
			if !credentialVarRE.MatchString(varName) {
				continue
			}
			// Look in the next 4 lines for: varName = "literal"
			end := i + 5
			if end > len(lines) {
				end = len(lines)
			}
			for _, inner := range lines[i+1 : end] {
				assignM := fallbackAssignRE.FindStringSubmatch(inner)
				if assignM == nil || len(assignM) < 3 {
					continue
				}
				if assignM[1] != varName {
					continue
				}
				fallbackVal := assignM[2]
				if isPlaceholderValue(fallbackVal) {
					continue
				}
				msg := fillTemplate(p.Message, map[string]string{
					"file":   fc.filePath,
					"target": varName,
				})
				violations = append(violations, Violation{
					PatternID:   p.ID,
					PatternName: p.Name,
					Severity:    severity,
					File:        fc.filePath,
					Target:      varName,
					Message:     msg,
					Evidence: fmt.Sprintf(
						"Variable %q falls back to a hardcoded value when the environment variable is not set — "+
							"the fallback credential is embedded in source code",
						varName,
					),
					Tags: p.Tags,
				})
				break // one violation per varName
			}
		}

	case "typescript", "javascript":
		// Single-line: process.env.VAR || "fallback" or process.env.VAR ?? "fallback"
		// Guard: only fire when the env var name suggests a credential (credentialVarRE).
		// This prevents false positives on non-credential vars like process.env.NODE_ENV.
		for lineNum, line := range lines {
			m := jsFallbackRE.FindStringSubmatch(line)
			if m == nil || len(m) < 3 {
				continue
			}
			envVarName := m[1] // e.g. "JWT_SECRET"
			if !credentialVarRE.MatchString(envVarName) {
				continue
			}
			fallbackVal := m[2]
			if isPlaceholderValue(fallbackVal) {
				continue
			}
			msg := fillTemplate(p.Message, map[string]string{
				"file":   fc.filePath,
				"target": "process.env." + envVarName,
			})
			violations = append(violations, Violation{
				PatternID:   p.ID,
				PatternName: p.Name,
				Severity:    severity,
				File:        fc.filePath,
				Target:      "process.env." + m[1],
				Message:     msg,
				Evidence: fmt.Sprintf(
					"Line %d: environment variable %q has a hardcoded fallback value in source code",
					lineNum+1, "process.env."+m[1],
				),
				Tags: p.Tags,
			})
		}

	case "python":
		// os.environ.get("VAR", "fallback") or os.getenv("VAR") or "fallback"
		// Guard: only fire when the env var name suggests a credential (credentialVarRE).
		for lineNum, line := range lines {
			var envVarName, fallbackVal string
			if m := pyGetenvFallbackRE.FindStringSubmatch(line); m != nil && len(m) >= 3 {
				envVarName, fallbackVal = m[1], m[2]
			} else if m := pyOrFallbackRE.FindStringSubmatch(line); m != nil && len(m) >= 3 {
				envVarName, fallbackVal = m[1], m[2]
			}
			if envVarName == "" || !credentialVarRE.MatchString(envVarName) {
				continue
			}
			if isPlaceholderValue(fallbackVal) {
				continue
			}
			target := "os.environ[" + envVarName + "]"
			msg := fillTemplate(p.Message, map[string]string{
				"file":   fc.filePath,
				"target": target,
			})
			violations = append(violations, Violation{
				PatternID:   p.ID,
				PatternName: p.Name,
				Severity:    severity,
				File:        fc.filePath,
				Target:      target,
				Message:     msg,
				Evidence: fmt.Sprintf(
					"Line %d: os.getenv/os.environ.get call for credential %q includes a hardcoded fallback value in source code",
					lineNum+1, envVarName,
				),
				Tags: p.Tags,
			})
		}

	case "java":
		// System.getenv("VAR") != null ? ... : "fallback"  or  Optional.ofNullable(...).orElse("fallback")
		for lineNum, line := range lines {
			m := javaEnvFallbackRE.FindStringSubmatch(line)
			if m == nil || len(m) < 2 {
				continue
			}
			fallbackVal := m[1]
			if isPlaceholderValue(fallbackVal) {
				continue
			}
			msg := fillTemplate(p.Message, map[string]string{
				"file":   fc.filePath,
				"target": "System.getenv",
			})
			violations = append(violations, Violation{
				PatternID:   p.ID,
				PatternName: p.Name,
				Severity:    severity,
				File:        fc.filePath,
				Target:      "System.getenv",
				Message:     msg,
				Evidence: fmt.Sprintf(
					"Line %d: System.getenv call includes a hardcoded fallback value in source code",
					lineNum+1,
				),
				Tags: p.Tags,
			})
		}

	case "rust":
		// env::var("VAR").unwrap_or("fallback")
		for lineNum, line := range lines {
			m := rustEnvFallbackRE.FindStringSubmatch(line)
			if m == nil || len(m) < 2 {
				continue
			}
			fallbackVal := m[1]
			if isPlaceholderValue(fallbackVal) {
				continue
			}
			msg := fillTemplate(p.Message, map[string]string{
				"file":   fc.filePath,
				"target": "env::var",
			})
			violations = append(violations, Violation{
				PatternID:   p.ID,
				PatternName: p.Name,
				Severity:    severity,
				File:        fc.filePath,
				Target:      "env::var",
				Message:     msg,
				Evidence: fmt.Sprintf(
					"Line %d: env::var call uses unwrap_or with a hardcoded fallback value in source code",
					lineNum+1,
				),
				Tags: p.Tags,
			})
		}
	}

	return violations
}

// isPlaceholderValue reports whether val is an obvious placeholder / demo credential
// rather than a real secret. Used to suppress false positives in checkHardcodedSecret.
//
// A placeholder is: a very short value, a common dummy word, a repeated-character
// string, or a value with a well-known "example" prefix.
func isPlaceholderValue(val string) bool {
	if len(val) < 6 {
		return true // too short to be a real credential
	}
	lower := strings.ToLower(val)
	if placeholderRE.MatchString(lower) {
		return true
	}
	// Repeated-character strings: "aaaaaaa", "xxxxxxx", "1111111"
	if len(val) >= 6 {
		first := val[0]
		allSame := true
		for i := 1; i < len(val); i++ {
			if val[i] != first {
				allSame = false
				break
			}
		}
		if allSame {
			return true
		}
	}
	return false
}

// checkAdminElevation fires when a route, function, or file identified as
// admin-level does not call elevated authorization functions.
//
// Three detection strategies are applied in order; the first two are independent,
// the third fires only when the first two produce no violations:
//
//  1. Route path patterns (AdminPathPatterns): route nodes whose path matches
//     an admin path pattern or contains "/admin" as a URL path component.
//  2. Handler name patterns (AdminHandlerNamePatterns): functions or methods
//     whose names match admin-indicating glob patterns (case-insensitive).
//  3. Admin package paths (AdminPackagePaths): files in admin directories are
//     treated as admin handler files and produce a single file-level violation.
//     Only fires when strategies 1 and 2 find nothing (avoids double-reporting).
//
// In all cases: if ElevatedAuthPatterns is non-empty and the file calls any
// matching function, the file is considered compliant and no violation is fired.
func checkAdminElevation(fc *fileContext, p SecurityPattern) []Violation {
	// Fast path: if elevated auth is called anywhere in this file, it is compliant.
	if len(p.Detection.ElevatedAuthPatterns) > 0 && fc.callsAny(p.Detection.ElevatedAuthPatterns) {
		return nil
	}

	var violations []Violation

	// ── Strategy 1: route nodes with admin path patterns ─────────────────────
	if len(p.Detection.AdminPathPatterns) > 0 && len(fc.routes) > 0 {
		for _, route := range fc.routes {
			routePath := route.Metadata["path"]
			if routePath == "" {
				routePath = route.Name // fallback: "GET /admin/users"
			}
			isAdmin := false
			for _, adminPat := range p.Detection.AdminPathPatterns {
				if matchGlob(adminPat, routePath) || matchAdminComponent(routePath) {
					isAdmin = true
					break
				}
			}
			if !isAdmin {
				continue
			}
			msg := fillTemplate(p.Message, map[string]string{
				"file":   fc.filePath,
				"target": routePath,
			})
			violations = append(violations, Violation{
				PatternID:   p.ID,
				PatternName: p.Name,
				Severity:    p.Severity,
				File:        fc.filePath,
				Target:      routePath,
				Message:     msg,
				Evidence:    fmt.Sprintf("Admin route %q in %s does not call elevated authorization (role check, admin permission)", routePath, filepath.Base(fc.filePath)),
				Tags:        p.Tags,
			})
		}
	}

	// ── Strategy 2: functions/methods whose names indicate admin handling ─────
	// Patterns are matched case-insensitively so both "AdminUsers" and
	// "adminUsers" are caught without requiring duplicate pattern entries.
	//
	// Test files (_test.go, testdata/, etc.) are skipped entirely: test helpers
	// like TestAdminEndpoint or setupAdminFixture match "*admin*" but do not
	// represent production handlers that need elevated auth. Flagging them is
	// always a false positive.
	if len(p.Detection.AdminHandlerNamePatterns) > 0 && !isTestFile(fc.filePath) {
		seen := make(map[string]bool)
		for _, n := range fc.nodes {
			if n.Type != graph.NodeFunction && n.Type != graph.NodeMethod {
				continue
			}
			lowerName := strings.ToLower(n.Name)
			for _, pat := range p.Detection.AdminHandlerNamePatterns {
				if matchGlob(strings.ToLower(pat), lowerName) {
					if seen[n.Name] {
						break
					}
					seen[n.Name] = true
					msg := fillTemplate(p.Message, map[string]string{
						"file":   fc.filePath,
						"target": n.Name,
					})
					violations = append(violations, Violation{
						PatternID:   p.ID,
						PatternName: p.Name,
						Severity:    p.Severity,
						File:        fc.filePath,
						Target:      n.Name,
						Message:     msg,
						Evidence:    fmt.Sprintf("Function %q appears to be an admin handler but does not call elevated authorization (role check, admin permission)", n.Name),
						Tags:        p.Tags,
					})
					break
				}
			}
		}
	}

	// ── Strategy 3: file located in an admin package/directory ───────────────
	// Only fires when strategies 1 and 2 found nothing — prevents double-reporting
	// the same file via multiple strategies (e.g. a file in admin/ that also has
	// admin-named routes would already be caught by strategy 1 or 2).
	// Requires the file to contain at least one function or method to be worth flagging
	// (avoids noisy findings on config/init files with no handler logic).
	// Test files are skipped: admin/handlers_test.go is a test file, not a production
	// admin handler, and flagging it for missing elevated auth is a false positive.
	if len(violations) == 0 && len(p.Detection.AdminPackagePaths) > 0 &&
		!isTestFile(fc.filePath) &&
		fileMatchesAny(fc.filePath, p.Detection.AdminPackagePaths) {
		hasFunctions := false
		for _, n := range fc.nodes {
			if n.Type == graph.NodeFunction || n.Type == graph.NodeMethod {
				hasFunctions = true
				break
			}
		}
		if hasFunctions {
			target := filepath.Base(fc.filePath)
			msg := fillTemplate(p.Message, map[string]string{
				"file":   fc.filePath,
				"target": target,
			})
			violations = append(violations, Violation{
				PatternID:   p.ID,
				PatternName: p.Name,
				Severity:    p.Severity,
				File:        fc.filePath,
				Target:      target,
				Message:     msg,
				Evidence:    fmt.Sprintf("File %s is in an admin package but does not call elevated authorization (role check, admin permission)", filepath.Base(fc.filePath)),
				Tags:        p.Tags,
			})
		}
	}

	return nilIfEmpty(violations)
}

// matchAdminComponent reports whether routePath contains "/admin" as a path
// component (i.e. not just a prefix like "/administrator").
func matchAdminComponent(routePath string) bool {
	parts := strings.Split(routePath, "/")
	for _, part := range parts {
		if strings.EqualFold(part, "admin") || strings.EqualFold(part, "management") {
			return true
		}
	}
	return false
}

// checkCrossTransportAuth fires when auth middleware is applied to some transport
// types (e.g. HTTP routes) but not others (e.g. WebSocket handlers, gRPC services)
// in the same project. This is a project-scope check.
//
// Framework scoping: if the pattern specifies FrameworkIdentifiers, the pattern
// only fires in projects where at least one file imports one of those identifiers.
// This is the same zero-false-positive guarantee as the per-file framework gate.
// Without this, a Go/chi pattern would fire on a pure Node.js project.
//
// Transport detection: uses detectTransportType with path/name heuristics,
// supplemented by the pattern's WebSocketNodeNames and GRPCNodeNames fields.
func checkCrossTransportAuth(g *graph.Graph, p SecurityPattern) []Violation {
	// Framework gate (project-scope variant): if FrameworkIdentifiers are specified,
	// verify at least one file in the project imports one of them.
	// This prevents, e.g., a chi-specific pattern from firing on a Gin project.
	if len(p.Detection.FrameworkIdentifiers) > 0 {
		found := false
		g.IterateNodes(func(n *graph.Node) {
			if found || n.Type != graph.NodeFile {
				return
			}
			fc := buildFileContext(g, n.File)
			if fc.importsAny(p.Detection.FrameworkIdentifiers) {
				found = true
			}
		})
		if !found {
			return nil
		}
	}

	// Collect all route nodes across the project.
	type routeEntry struct {
		file      string
		path      string
		transport string // "http", "websocket", "grpc"
		hasAuth   bool
	}

	var routes []routeEntry
	g.IterateNodes(func(n *graph.Node) {
		if n.Type != graph.NodeRoute {
			return
		}
		routePath := n.Metadata["path"]
		if routePath == "" {
			return
		}
		transport := detectTransportType(routePath, n.Name,
			p.Detection.WebSocketNodeNames, p.Detection.GRPCNodeNames)
		entry := routeEntry{
			file:      n.File,
			path:      routePath,
			transport: transport,
		}
		routes = append(routes, entry)
	})

	if len(routes) == 0 {
		return nil
	}

	// For each route, check whether auth calls exist in its file.
	// Build a file→hasAuth index to avoid repeated graph queries.
	fileAuthIndex := make(map[string]bool)
	for _, route := range routes {
		if _, ok := fileAuthIndex[route.file]; ok {
			continue
		}
		fc := buildFileContext(g, route.file)
		fileAuthIndex[route.file] = fc.callsAny(p.Detection.RequiredCallPatterns)
	}

	// Tag each route with its auth status.
	var authByTransport = make(map[string]int)   // transport → auth-protected count
	var totalByTransport = make(map[string]int)  // transport → total count
	var fileByTransport = make(map[string]string) // transport → example file without auth

	for _, route := range routes {
		totalByTransport[route.transport]++
		if fileAuthIndex[route.file] {
			authByTransport[route.transport]++
		} else if fileByTransport[route.transport] == "" {
			fileByTransport[route.transport] = route.file
		}
	}

	// If only one transport type exists, no cross-transport inconsistency possible.
	if len(totalByTransport) <= 1 {
		return nil
	}

	// Find transports that are less protected than HTTP (the baseline).
	httpAuth := authByTransport["http"]
	httpTotal := totalByTransport["http"]
	if httpTotal == 0 || httpAuth == 0 {
		return nil // HTTP has no auth either — not a cross-transport issue
	}
	httpRatio := float64(httpAuth) / float64(httpTotal)

	var violations []Violation
	for transport, total := range totalByTransport {
		if transport == "http" {
			continue
		}
		authCount := authByTransport[transport]
		if total == 0 {
			continue
		}
		ratio := float64(authCount) / float64(total)
		// Fire if HTTP auth coverage is significantly better than this transport.
		if httpRatio-ratio > 0.3 {
			exampleFile := fileByTransport[transport]
			msg := fillTemplate(p.Message, map[string]string{
				"file":   exampleFile,
				"target": transport,
			})
			violations = append(violations, Violation{
				PatternID:   p.ID,
				PatternName: p.Name,
				Severity:    p.Severity,
				File:        exampleFile,
				Target:      transport,
				Message:     msg,
				Evidence:    fmt.Sprintf("HTTP routes: %d/%d have auth. %s handlers: %d/%d have auth — inconsistent protection across transports", httpAuth, httpTotal, transport, authCount, total),
				Tags:        p.Tags,
			})
		}
	}
	return nilIfEmpty(violations)
}

// detectTransportType classifies a route as "http", "websocket", or "grpc"
// based on path/name heuristics supplemented by pattern-provided node name lists.
//
// Classification order (first match wins):
//  1. Pattern-provided gRPC node names (most specific — user-declared)
//  2. Pattern-provided WebSocket node names
//  3. Path/name heuristics for gRPC (rpc prefix, grpc keyword)
//  4. Path/name heuristics for WebSocket (ws://, /ws, /socket, /stream, etc.)
//  5. Default: http
//
// wsNodeNames and grpcNodeNames support glob patterns (e.g. "Register*Server").
func detectTransportType(routePath, nodeName string, wsNodeNames, grpcNodeNames []string) string {
	lowerNode := strings.ToLower(nodeName)
	lower := strings.ToLower(routePath + " " + nodeName)

	// 1. Pattern-provided gRPC node names — highest specificity.
	for _, grpcFn := range grpcNodeNames {
		if matchGlob(strings.ToLower(grpcFn), lowerNode) {
			return "grpc"
		}
	}

	// 2. Pattern-provided WebSocket node names.
	for _, wsFn := range wsNodeNames {
		if matchGlob(strings.ToLower(wsFn), lowerNode) {
			return "websocket"
		}
	}

	// 3. gRPC heuristics: explicit gRPC keywords or gRPC service naming conventions.
	if strings.Contains(lower, "grpc") ||
		strings.HasPrefix(lower, "rpc ") ||
		strings.HasPrefix(lowerNode, "register") && strings.HasSuffix(lowerNode, "server") {
		return "grpc"
	}

	// 4. WebSocket heuristics: protocol prefix, path segments, common upgrade patterns.
	if strings.Contains(lower, "ws://") ||
		strings.Contains(lower, "wss://") ||
		strings.Contains(lower, "websocket") ||
		strings.HasPrefix(lower, "ws ") ||
		containsPathSegment(routePath, "ws") ||
		containsPathSegment(routePath, "socket") ||
		containsPathSegment(routePath, "stream") ||
		containsPathSegment(routePath, "live") ||
		containsPathSegment(routePath, "events") ||
		containsPathSegment(routePath, "notify") ||
		containsPathSegment(routePath, "push") {
		return "websocket"
	}

	return "http"
}

// containsPathSegment reports whether routePath contains segment as a discrete
// URL path component. E.g. "/api/ws/events" contains "ws" but "/towson" does not.
func containsPathSegment(routePath, segment string) bool {
	lower := strings.ToLower(routePath)
	seg := strings.ToLower(segment)
	// Check for /segment or /segment/ patterns.
	return strings.Contains(lower, "/"+seg+"/") ||
		strings.Contains(lower, "/"+seg+"?") ||
		strings.HasSuffix(lower, "/"+seg)
}

// ──────────────────────────────────────────────────────────────────────────────
// Evidence helpers
// ──────────────────────────────────────────────────────────────────────────────

// countSiblingsWithCall returns the number of sibling files (same directory,
// excluding currentFile) that call at least one function matching callPatterns,
// plus the total number of sibling files scanned.
//
// Scans are bounded: at most 30 sibling files are evaluated.
func countSiblingsWithCall(g *graph.Graph, currentFile string, callPatterns []string) (authCount, totalSiblings int) {
	if len(callPatterns) == 0 {
		return 0, 0
	}
	dir := filepath.Dir(currentFile)
	if dir == "" || dir == "." {
		return 0, 0
	}

	seen := make(map[string]bool)
	seen[currentFile] = true

	const maxSiblings = 30
	g.IterateNodes(func(n *graph.Node) {
		if totalSiblings >= maxSiblings {
			return
		}
		if n.Type != graph.NodeFile {
			return
		}
		if seen[n.File] {
			return
		}
		if filepath.Dir(n.File) != dir {
			return
		}
		seen[n.File] = true
		totalSiblings++
		sib := buildFileContext(g, n.File)
		if sib.callsAny(callPatterns) {
			authCount++
		}
	})
	return authCount, totalSiblings
}

// ──────────────────────────────────────────────────────────────────────────────
// Pattern/path matching utilities
// ──────────────────────────────────────────────────────────────────────────────

// languageFromPath maps a file path to a language string matching the
// Language field in SecurityPattern ("go", "typescript", "javascript", etc.).
// Returns the lowercased extension (without dot) for unknown types.
func languageFromPath(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".java":
		return "java"
	case ".rs":
		return "rust"
	case ".rb":
		return "ruby"
	case ".cs":
		return "csharp"
	case ".php":
		return "php"
	default:
		if ext == "" {
			return ""
		}
		return ext[1:] // strip leading dot
	}
}

// fileMatchesAny reports whether filePath matches any of the given glob patterns.
//
// Matching strategy (tried in order, first match wins):
//  1. Match the basename alone against the pattern.
//  2. Match progressively longer path suffixes against the pattern.
//     E.g. "*/handler/*.go" matches "internal/handler/users.go" via the suffix
//     "internal/handler/users.go".
//  3. Cross-slash match of the pattern against the full normalized path, where
//     * can span path separators. E.g. "*handler*.go" matches any path that
//     contains "handler" anywhere (including as a directory component).
//
// Uses path.Match (forward-slash, cross-platform) for strategies 1 and 2.
func fileMatchesAny(filePath string, patterns []string) bool {
	// Normalize to forward slashes for consistent cross-platform matching.
	clean := filepath.ToSlash(filePath)
	base := filepath.Base(filePath)

	for _, pat := range patterns {
		pat = filepath.ToSlash(pat)

		// 1. Base name match (e.g. "*_handler.go", "users_controller.go").
		if m, _ := path.Match(pat, base); m {
			return true
		}

		// 2. Suffix match: try each successive path suffix.
		// "a/b/c/d.go" → "a/b/c/d.go", "b/c/d.go", "c/d.go"
		// This allows "*/handler/*.go" to match "internal/handler/users.go".
		parts := strings.Split(clean, "/")
		for i := 0; i < len(parts)-1; i++ {
			suffix := strings.Join(parts[i:], "/")
			if m, _ := path.Match(pat, suffix); m {
				return true
			}
		}

		// 3. Cross-slash match: * can span path separators.
		// Handles patterns like "*handler*.go" that should match a file in a
		// directory named "handler" even when the basename doesn't contain "handler".
		if matchGlobCrossSlash(pat, clean) {
			return true
		}
	}
	return false
}

// matchGlobCrossSlash reports whether pattern matches s with * matching any
// sequence of characters including path separators (/).
// This is used by fileMatchesAny as a final fallback.
func matchGlobCrossSlash(pattern, s string) bool {
	for {
		if len(pattern) == 0 {
			return len(s) == 0
		}
		if pattern[0] == '*' {
			// Consume consecutive stars (they're equivalent to a single *).
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true // trailing * matches everything
			}
			// Try matching the remainder of the pattern at each position in s.
			for i := 0; i <= len(s); i++ {
				if matchGlobCrossSlash(pattern, s[i:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 {
			return false
		}
		if pattern[0] == '?' {
			// ? matches any single character (including /).
			pattern = pattern[1:]
			s = s[1:]
			continue
		}
		if pattern[0] != s[0] {
			return false
		}
		pattern = pattern[1:]
		s = s[1:]
	}
}

// matchGlob reports whether name matches the glob pattern.
// Used for import paths, function names, and package names.
// Uses path.Match (not filepath.Match) so behaviour is consistent cross-platform.
// An empty pattern never matches. A "*" pattern matches everything.
//
// Special case: patterns ending with "/*" (e.g. "gorm.io/*") match any
// sub-path at any depth, so "gorm.io/*" matches both "gorm.io/gorm" and
// "gorm.io/driver/postgres". This is needed for import-path prefixes where
// the full sub-module path is not known at pattern-authoring time.
func matchGlob(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	// Fast path: exact match (most common for import identifiers).
	if pattern == name {
		return true
	}
	m, _ := path.Match(pattern, name)
	if m {
		return true
	}
	// Fallback: patterns ending with "/*" should match any sub-path at any depth.
	// e.g. "gorm.io/*" should match "gorm.io/driver/postgres" (not just "gorm.io/gorm").
	// path.Match only matches * within a single path component (no /), so we need
	// a prefix-based fallback here.
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "*") // e.g. "gorm.io/"
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────────
// Template and utility helpers
// ──────────────────────────────────────────────────────────────────────────────

// fillTemplate replaces placeholders in tmpl with values from vars.
// Supported placeholders: {target}, {file}, {count}, {total}.
// Unknown placeholders are left as-is.
func fillTemplate(tmpl string, vars map[string]string) string {
	result := tmpl
	for k, v := range vars {
		result = strings.ReplaceAll(result, "{"+k+"}", v)
	}
	return result
}

// isTestFile reports whether filePath looks like a test or test-fixture file.
// Used to downgrade severity for CheckTypeHardcodedSecret.
func isTestFile(filePath string) bool {
	base := strings.ToLower(filepath.Base(filePath))
	return strings.HasSuffix(base, "_test.go") ||
		strings.Contains(filePath, "/testdata/") ||
		strings.Contains(filePath, "/test/") ||
		strings.Contains(filePath, "/tests/") ||
		strings.Contains(filePath, "/fixtures/") ||
		strings.Contains(filePath, "/mocks/")
}

// extractAnnotationsFromSig parses a function signature string and returns any
// @Annotation tokens it contains. This covers:
//   - Java: @PreAuthorize("..."), @Secured, @RolesAllowed(...)
//   - Python: @login_required, @permission_required(...)
//   - TypeScript/NestJS: @UseGuards(...), @Roles(...)
//
// Trailing punctuation (parentheses, commas, semicolons) is stripped so that
// "@PreAuthorize(" → "@PreAuthorize". Tokens shorter than 2 characters after
// the '@' are ignored. Returns nil if no annotations are present.
func extractAnnotationsFromSig(sig string) []string {
	var found []string
	for _, word := range strings.Fields(sig) {
		if !strings.HasPrefix(word, "@") || len(word) < 2 {
			continue
		}
		name := word
		if i := strings.IndexAny(name, ".(,;)"); i > 1 {
			name = name[:i]
		}
		found = append(found, name)
	}
	return found
}

// nilIfEmpty returns nil when violations is empty, otherwise returns violations.
// Ensures CheckFile never returns an empty (non-nil) slice.
func nilIfEmpty(violations []Violation) []Violation {
	if len(violations) == 0 {
		return nil
	}
	return violations
}
