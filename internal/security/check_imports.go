// Package security — check_imports.go: unknown-package detection via registry lookup.
//
// Sprint 26.11: Slopsquatting / unknown package detection.
//
// Engine.CheckImports is the per-file entry point that flags imports not present in the
// PackageRegistry. This is separate from CheckFile because it uses a different data source
// (a package name registry rather than SecurityPattern specifications).
//
// Integration with Engine:
//   - Engine.WithRegistry returns a new Engine with the registry attached.
//   - DefaultEngineWithRegistry loads both built-in patterns and the built-in registry.
//   - Callers run both CheckFile and CheckImports to get full security coverage.
//
// Violation output:
//   - PatternID: "unknown-package-<lang>" (e.g., "unknown-package-pypi")
//   - Severity: HIGH (not CRITICAL — the agent should verify, but can proceed)
//   - Message: includes "did you mean" suggestion when one exists
//   - Evidence: registry source and normalized name for transparency
package security

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// WithRegistry returns a new Engine that includes package registry checks.
// The original Engine is unchanged (immutable builder pattern).
// If r is nil, the returned Engine skips CheckImports entirely.
func (e *Engine) WithRegistry(r *PackageRegistry) *Engine {
	return &Engine{
		patterns: e.patterns,
		registry: r,
	}
}

// DefaultEngineWithRegistry loads both the built-in security patterns and the
// built-in package registry, returning an Engine ready for both CheckFile and
// CheckImports. Degrades gracefully: if the registry fails to load, returns an
// Engine with patterns only (no nil return, no panic).
func DefaultEngineWithRegistry() *Engine {
	e := DefaultEngine()
	r, err := LoadBuiltinRegistry()
	if err != nil || r == nil {
		// Registry load failure is not fatal: pattern checks still work.
		return e
	}
	return e.WithRegistry(r)
}

// DefaultEngineWithDirAndRegistry loads built-in patterns merged with user patterns
// from extraDir, plus the built-in package registry.
// Falls back to built-ins only if extraDir loading fails. Never returns nil.
func DefaultEngineWithDirAndRegistry(extraDir string) *Engine {
	e := DefaultEngineWithDir(extraDir)
	r, err := LoadBuiltinRegistry()
	if err != nil || r == nil {
		return e
	}
	return e.WithRegistry(r)
}

// CheckImports checks all import statements in filePath against the known package
// registry. For each import not found in the registry, it returns a HIGH severity
// violation with a natural-language message including a "did you mean" suggestion
// when a close match exists.
//
// Returns nil if:
//   - The registry is nil (no registry loaded)
//   - The file is in a vendor or node_modules directory
//   - No unknown imports are found
//
// The returned slice is never empty (nil or ≥1 violations).
func (e *Engine) CheckImports(g *graph.Graph, filePath string) []Violation {
	if e == nil || e.registry == nil || g == nil || filePath == "" {
		return nil
	}
	// Skip vendor/generated directories: we don't control those imports.
	if isVendoredPath(filePath) {
		return nil
	}

	lang := languageFromPath(filePath)
	if lang == "" {
		return nil
	}

	// Build the file context (reuses the same logic as CheckFile — one graph read).
	fc := buildFileContext(g, filePath)
	if len(fc.imports) == 0 {
		return nil
	}

	var violations []Violation
	for _, imp := range fc.imports {
		importPath := imp.Name
		if e.registry.IsKnown(lang, importPath) {
			continue
		}
		// Unknown import found: build violation.
		suggestion := e.registry.Suggest(lang, importPath)
		v := buildUnknownImportViolation(filePath, importPath, lang, suggestion, e.registry)
		violations = append(violations, v)
	}

	if len(violations) == 0 {
		return nil
	}
	return withActions(violations)
}

// buildUnknownImportViolation constructs a Violation for an import not found
// in the package registry.
func buildUnknownImportViolation(filePath, importPath, lang string, suggestion string, r *PackageRegistry) Violation {
	langLabel := registryLangLabel(lang)
	patternID := "unknown-package-" + strings.ReplaceAll(strings.ToLower(langLabel), " ", "-")

	var msg string
	if suggestion != "" {
		msg = fmt.Sprintf(
			"Package %q is not found in %s. Did you mean %q? AI agents hallucinate package names ~20%% of the time — verify this dependency exists before running.",
			importPath, langLabel, suggestion,
		)
	} else {
		msg = fmt.Sprintf(
			"Package %q is not found in %s. AI agents hallucinate package names ~20%% of the time — verify this dependency exists before running.",
			importPath, langLabel,
		)
	}

	evidence := fmt.Sprintf(
		"Registry: %s built-in (%d known packages, loaded %s). Normalized lookup key: %q.",
		langLabel,
		r.Size(),
		r.LoadedAt().Format("2006-01-02"),
		normalizedKeyForDisplay(lang, importPath),
	)
	if suggestion != "" {
		evidence += fmt.Sprintf(" Nearest known package: %q (edit distance: %d).",
			suggestion, editDistance(normalizedKeyForDisplay(lang, importPath), suggestion))
	}

	return Violation{
		PatternID:   patternID,
		PatternName: fmt.Sprintf("Unknown %s package", langLabel),
		Severity:    SeverityHigh,
		File:        filePath,
		Target:      importPath,
		Message:     msg,
		Evidence:    evidence,
		Tags:        []string{"supply-chain", "slopsquatting", "hallucination"},
	}
}

// registryLangLabel returns a human-readable label for the language's package registry.
func registryLangLabel(lang string) string {
	switch strings.ToLower(lang) {
	case "go":
		return "Go modules"
	case "typescript", "javascript":
		return "npm"
	case "python":
		return "PyPI"
	case "rust":
		return "crates.io"
	default:
		return lang
	}
}

// normalizedKeyForDisplay returns the normalized package name used for registry lookup,
// for inclusion in violation evidence.
func normalizedKeyForDisplay(lang, importPath string) string {
	_, normalized := registryKey(lang, importPath)
	return normalized
}

// isVendoredPath reports whether filePath is inside a vendor or generated directory
// that we should skip during import checks.
func isVendoredPath(filePath string) bool {
	// Normalise to forward slashes for consistent matching.
	p := filepath.ToSlash(filePath)
	for _, segment := range []string{"/vendor/", "/node_modules/", "/.gen/", "/generated/"} {
		if strings.Contains(p, segment) {
			return true
		}
	}
	// Also check if the path starts with these (relative paths in tests).
	for _, prefix := range []string{"vendor/", "node_modules/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
