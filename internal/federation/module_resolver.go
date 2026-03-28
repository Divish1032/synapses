package federation

import (
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/config"
)

// ModuleDependency represents a cross-repo dependency discovered from
// package manifests (go.mod, package.json).
type ModuleDependency struct {
	ImportPath  string // the import path as it appears in source code
	ModulePath  string // the module/package identifier
	Version     string // version string (e.g., "v1.2.3", "workspace:*")
	SiblingAlias string // federation alias if matches a sibling, or ""
	Ecosystem   string // "gomod", "npm", "pnpm"
}

// DiscoverModuleSiblings analyzes the project's dependency manifests (go.mod,
// package.json) and returns dependencies that match known federation siblings.
// This enables automatic cross-repo edge discovery without manual configuration.
//
// projectRoot: absolute path to the project being indexed
// entries: federation entries from synapses.json
func DiscoverModuleSiblings(projectRoot string, entries []config.FederationEntry) []ModuleDependency {
	var deps []ModuleDependency

	// Try Go modules.
	goModPath := FindGoMod(projectRoot)
	if goModPath != "" {
		goMod, err := ParseGoMod(goModPath)
		if err == nil && goMod != nil {
			// Build sibling module map from federation entries.
			siblingModules := buildGoSiblingModules(entries)
			for _, req := range goMod.Require {
				alias, matched := goMod.MatchesSibling(req.Path, siblingModules)
				dep := ModuleDependency{
					ImportPath: req.Path,
					ModulePath: req.Path,
					Version:    req.Version,
					Ecosystem:  "gomod",
				}
				if matched {
					dep.SiblingAlias = alias
				}
				deps = append(deps, dep)
			}
		}
	}

	// Try npm/pnpm workspaces.
	ws := ParseNPMWorkspace(projectRoot)
	if ws != nil {
		// Each workspace package is effectively a sibling.
		for pkgName, pkgDir := range ws.Packages {
			// Check if the workspace package maps to a federation entry.
			alias := matchNPMSibling(pkgDir, projectRoot, entries)
			deps = append(deps, ModuleDependency{
				ImportPath:   pkgName,
				ModulePath:   pkgName,
				Version:      "workspace:*",
				SiblingAlias: alias,
				Ecosystem:    "npm",
			})
		}
	}

	return deps
}

// buildGoSiblingModules builds a map of Go module paths → federation aliases
// by reading go.mod from each sibling's project directory.
func buildGoSiblingModules(entries []config.FederationEntry) map[string]string {
	result := make(map[string]string)
	for _, e := range entries {
		goModPath := FindGoMod(e.Path)
		if goModPath == "" {
			continue
		}
		mod, err := ParseGoMod(goModPath)
		if err != nil || mod == nil || mod.ModulePath == "" {
			continue
		}
		result[mod.ModulePath] = e.Alias
	}
	return result
}

// matchNPMSibling checks if a workspace package directory matches a federation entry.
func matchNPMSibling(pkgDir, projectRoot string, entries []config.FederationEntry) string {
	absDir := filepath.Join(projectRoot, pkgDir)
	for _, e := range entries {
		// Normalize both paths for comparison.
		if absDir == e.Path || strings.HasPrefix(absDir, e.Path+"/") || strings.HasPrefix(e.Path, absDir+"/") {
			return e.Alias
		}
	}
	return ""
}

// FilterSiblingDeps returns only dependencies that match a known federation sibling.
func FilterSiblingDeps(deps []ModuleDependency) []ModuleDependency {
	var siblings []ModuleDependency
	for _, d := range deps {
		if d.SiblingAlias != "" {
			siblings = append(siblings, d)
		}
	}
	return siblings
}
