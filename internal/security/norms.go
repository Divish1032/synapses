// Package security — norms.go: project-wide architectural norm discovery (Sprint 29.5).
//
// DiscoverNorms scans the project graph for implicit structural conventions and
// surfaces them as recommended rules. Unlike CheckNorms (which checks a single
// file against directory siblings and reports violations), DiscoverNorms operates
// project-wide and returns positive adherence observations — "18/18 handler files
// avoid direct data-layer imports" — along with a suggestion to promote the norm
// to an enforced rule when adherence is near-perfect.
//
// Two norm categories are computed from graph structure alone (no pattern library
// dependency, no file I/O):
//
//  1. Route call norms — functions that ≥75% of route-registering files call.
//     These are project-wide call conventions, typically auth middleware or rate
//     limiting. Example: "AuthMiddleware called by 12/12 route files."
//
//  2. Layer isolation norms — whether presentation-layer files consistently avoid
//     direct data-layer imports. Example: "No handler files import repo packages
//     directly (0/9 violations)."
//
// Minimum sample threshold: 3 files in any category. Below this, the project is
// too small to generalise — no norms are returned to avoid false confidence.
package security

import (
	"fmt"
	"sort"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// DiscoveredNorm is a structural convention found consistently in the project
// graph, surfaced as a candidate for promotion to an enforced rule.
//
// Unlike Violation (which flags deviations), a DiscoveredNorm is a positive
// observation: it describes what the project already does consistently. The
// agent receives it as contextual guidance — and, when SuggestRule is true,
// as a prompt to consider formalising the convention.
type DiscoveredNorm struct {
	// Category classifies the norm type. Currently: "route_call_norm" or "layer_isolation".
	Category string `json:"category"`

	// Description is a brief human-readable label for the norm.
	// Example: "Consistent route middleware: AuthMiddleware"
	Description string `json:"description"`

	// Evidence is the structural proof in natural language.
	// Example: "12/12 route-registering files call AuthMiddleware"
	Evidence string `json:"evidence"`

	// Adherence is the fraction of examined entities that follow the norm (0.0–1.0).
	// 1.0 means every examined entity complies.
	Adherence float64 `json:"adherence"`

	// Compliant is the count of entities that follow the norm.
	Compliant int `json:"compliant"`

	// Total is the count of entities examined for this norm.
	Total int `json:"total"`

	// Confidence reflects how reliable the detection is.
	// "HIGH" when Total ≥ 5 and Adherence ≥ 0.95; "MEDIUM" otherwise.
	Confidence string `json:"confidence"`

	// SuggestRule is true when adherence ≥ 0.95 and Total ≥ 3 — indicating
	// the pattern is consistent enough to consider promoting to an enforced rule.
	SuggestRule bool `json:"suggest_rule"`
}

// DiscoverNorms computes project-wide architectural norms from the graph.
//
// Returns nil when the graph is nil, or when fewer than 3 files qualify for
// any norm category (too few samples to generalise). The returned slice is
// sorted by descending adherence, then alphabetically by description.
//
// Engine is immutable after construction; this method acquires only read-locks
// via the Graph API and is safe for concurrent use.
func (e *Engine) DiscoverNorms(g *graph.Graph) []DiscoveredNorm {
	if e == nil || g == nil {
		return nil
	}

	var norms []DiscoveredNorm
	norms = append(norms, discoverRouteCallNorms(g)...)
	norms = append(norms, discoverLayerNorms(g, defaultLayerConfig())...)

	if len(norms) == 0 {
		return nil
	}

	// Sort: highest adherence first, then alphabetically for deterministic output.
	sort.Slice(norms, func(i, j int) bool {
		if norms[i].Adherence != norms[j].Adherence {
			return norms[i].Adherence > norms[j].Adherence
		}
		return norms[i].Description < norms[j].Description
	})

	// Cap at 5 norms — the session_init briefing must remain concise. Highest-
	// adherence norms are most valuable and already sorted to the front.
	const maxNorms = 5
	if len(norms) > maxNorms {
		norms = norms[:maxNorms]
	}

	return norms
}

// ─────────────────────────────────────────────────────────────────────────────
// Route call norm discovery (category: "route_call_norm")
// ─────────────────────────────────────────────────────────────────────────────

// discoverRouteCallNorms finds functions that are consistently called across
// route-registering files in the project. A callee becomes a discovered norm
// when it appears in ≥75% of route files AND at least 3 route files exist.
//
// Results are grouped by language to avoid cross-language noise (a Python auth
// decorator is not a norm for Go route files in a polyglot monorepo).
//
// Callee name filter: names shorter than 4 characters are excluded. These are
// overwhelmingly stdlib or trivial helpers ("Get", "Use", "New", "Set") that
// appear in every file and carry no architectural signal. 4-char names like
// "Auth" and "CSRF" are intentionally allowed through.
//
// Performance: uses FindByType(NodeRoute) to locate route files first — O(N_routes)
// instead of O(N_all_nodes × buildFileContext), which matters in large projects.
func discoverRouteCallNorms(g *graph.Graph) []DiscoveredNorm {
	// Per-language data: routeFiles maps language → list of per-file callee sets.
	type fileCallees struct {
		file    string
		callees map[string]bool
	}
	byLang := make(map[string][]fileCallees)

	// Collect route-registering files by scanning NodeRoute nodes first.
	// This is O(N_routes) rather than O(N_all_nodes) — avoids calling
	// buildFileContext on every file in the project.
	const maxRouteFiles = 300 // avoid unbounded work in enormous monorepos
	seenFiles := make(map[string]bool)

	routeNodes := g.FindByType(graph.NodeRoute)
	for _, rn := range routeNodes {
		if len(seenFiles) >= maxRouteFiles {
			break
		}
		filePath := rn.File
		if filePath == "" || seenFiles[filePath] {
			continue
		}
		seenFiles[filePath] = true

		if isTestFile(filePath) || isVendoredPath(filePath) {
			continue
		}

		fc := buildFileContext(g, filePath)
		lang := languageFromPath(filePath)
		callees := make(map[string]bool, len(fc.callees))
		for name := range fc.callees {
			if len(name) < 4 {
				continue // too short — generic stdlib/helper noise ("Get", "Use", "New")
			}
			callees[name] = true
		}
		byLang[lang] = append(byLang[lang], fileCallees{file: filePath, callees: callees})
	}

	var norms []DiscoveredNorm

	for lang, files := range byLang {
		total := len(files)
		if total < 3 {
			continue // minimum sample threshold
		}

		// Build frequency map: callee → how many route files call it.
		freq := make(map[string]int)
		for _, fc := range files {
			for name := range fc.callees {
				freq[name]++
			}
		}

		// Find callees at or above the 75% threshold.
		// Sort callee names for deterministic iteration order.
		var candidates []string
		for name, count := range freq {
			if float64(count)/float64(total) >= 0.75 {
				candidates = append(candidates, name)
			}
		}
		sort.Strings(candidates)

		for _, callee := range candidates {
			count := freq[callee]
			adherence := float64(count) / float64(total)

			confidence := "MEDIUM"
			if total >= 5 && adherence >= 0.95 {
				confidence = "HIGH"
			}

			desc := fmt.Sprintf("Consistent route middleware: %s", callee)
			if lang != "" {
				desc = fmt.Sprintf("Consistent route middleware (%s): %s", lang, callee)
			}

			norms = append(norms, DiscoveredNorm{
				Category:    "route_call_norm",
				Description: desc,
				Evidence:    fmt.Sprintf("%d/%d route-registering files call %q", count, total, callee),
				Adherence:   adherence,
				Compliant:   count,
				Total:       total,
				Confidence:  confidence,
				SuggestRule: adherence >= 0.95 && total >= 3,
			})
		}
	}

	return norms
}

// ─────────────────────────────────────────────────────────────────────────────
// Layer isolation norm discovery (category: "layer_isolation")
// ─────────────────────────────────────────────────────────────────────────────

// discoverLayerNorms examines presentation-layer files (handler/controller/route
// directories) for direct data-layer imports. It reports the adherence rate —
// what fraction of presentation files avoid skipping the service layer.
//
// layers defines the ordered tier hierarchy (index 0 = outermost/presentation,
// last index = innermost/data). Callers pass defaultLayerConfig() for the
// built-in 3-tier model, or a custom config for projects with different layering
// (2-tier handler/store, 4-tier presentation/gateway/service/data, etc.).
//
// When all presentation-layer files are clean (0 violations), the norm is
// surfaced with SuggestRule=true. When most are clean but some violate, the norm
// is surfaced with the actual counts to give the agent a project picture.
//
// Requires at least 3 identifiable presentation-layer files to return a norm.
func discoverLayerNorms(g *graph.Graph, layers []LayerDef) []DiscoveredNorm {
	// presentationIdx and dataIdx identify the outermost and innermost layers.
	// The skip violation fires when a file at the outermost layer directly imports
	// the innermost layer (skipping the intermediate service layer).
	//
	// A skip = dataIdx - presentationIdx > 1.
	var presentationIdx, dataIdx int
	presentationIdx = 0
	dataIdx = len(layers) - 1
	if dataIdx-presentationIdx <= 1 {
		// Only 1 or 2 layers — no meaningful "skip" violation possible.
		return nil
	}

	type layerFile struct {
		path     string
		violated bool // true = direct data-layer import found
	}
	var presentationFiles []layerFile

	const maxPresentationFiles = 300 // avoid unbounded work in enormous monorepos
	seen := make(map[string]bool)
	g.IterateNodes(func(n *graph.Node) {
		if len(presentationFiles) >= maxPresentationFiles {
			return
		}
		if n.Type != graph.NodeFile {
			return
		}
		if seen[n.File] {
			return
		}
		seen[n.File] = true

		if n.File == "" || isTestFile(n.File) || isVendoredPath(n.File) {
			return
		}

		_, srcIdx := inferLayerFromPath(n.File, layers)
		if srcIdx != presentationIdx {
			return // only examine presentation-layer files
		}

		// Check for direct data-layer imports.
		violated := false
		for _, e := range g.OutEdges(n.ID) {
			if e.Type != graph.EdgeImports {
				continue
			}
			imp := g.GetNode(e.To)
			if imp == nil || imp.Type != graph.NodePackage {
				continue
			}
			_, dstIdx := inferLayerFromPath(imp.Name, layers)
			// Skip violation: presentation (0) → data (2), skipping service (1).
			// dstIdx > srcIdx+1 captures any multi-hop skip (works for N-layer configs).
			if dstIdx > srcIdx+1 {
				violated = true
				break
			}
		}

		presentationFiles = append(presentationFiles, layerFile{path: n.File, violated: violated})
	})

	total := len(presentationFiles)
	if total < 3 {
		return nil // not enough samples
	}

	violations := 0
	for _, f := range presentationFiles {
		if f.violated {
			violations++
		}
	}
	compliant := total - violations
	adherence := float64(compliant) / float64(total)

	// Only surface as a norm if at least 75% are clean — below that the project
	// has a widespread issue better addressed via CheckProject violations.
	if adherence < 0.75 {
		return nil
	}

	presentationName := layers[presentationIdx].Name
	dataName := layers[dataIdx].Name

	// Build evidence and description strings.
	var evidence string
	if violations == 0 {
		evidence = fmt.Sprintf(
			"0/%d %s-layer files import %s-layer packages directly",
			total, presentationName, dataName,
		)
	} else {
		evidence = fmt.Sprintf(
			"%d/%d %s-layer files avoid direct %s-layer imports (%d violation(s))",
			compliant, total, presentationName, dataName, violations,
		)
	}

	confidence := "MEDIUM"
	if total >= 5 && adherence >= 0.95 {
		confidence = "HIGH"
	}

	// Description uses layer names rather than directory names so it reads the
	// same across Go (handler/), Python (views/), Java (controller/) projects.
	skippedLayer := layers[presentationIdx+1].Name
	desc := fmt.Sprintf(
		"Layer isolation: %s layer avoids direct %s access (routes through %s)",
		presentationName, dataName, skippedLayer,
	)

	// When there are no violations at all, use the canonical three-layer framing.
	if violations == 0 {
		desc = fmt.Sprintf(
			"Layer isolation: no %s-layer file imports %s-layer packages directly",
			presentationName, dataName,
		)
	}

	return []DiscoveredNorm{
		{
			Category:    "layer_isolation",
			Description: desc,
			Evidence:    evidence,
			Adherence:   adherence,
			Compliant:   compliant,
			Total:       total,
			Confidence:  confidence,
			SuggestRule: adherence >= 0.95 && total >= 3,
		},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers for session_init delivery (internal/mcp/handlers_session.go)
// ─────────────────────────────────────────────────────────────────────────────

// FormatDiscoveredNorm renders a DiscoveredNorm as a single natural-language
// string suitable for inclusion in the session_init briefing conventions field.
//
// Format: "[Description] ([Evidence])" or, when SuggestRule is true:
// "[Description] ([Evidence]) — promote to enforced rule?"
//
// The output never contains raw source code — it is always natural language
// intelligence compliant with the Communication Protocol.
func FormatDiscoveredNorm(n DiscoveredNorm) string {
	base := fmt.Sprintf("%s (%s)", n.Description, n.Evidence)
	if n.SuggestRule {
		base += " — promote to enforced rule?"
	}
	// Trim long descriptions at 160 runes (generous limit; norms are concise by design).
	const maxRunes = 160
	runes := []rune(base)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes-1]) + "…"
	}
	return base
}

