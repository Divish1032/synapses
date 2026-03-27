package resolver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// tsConfigPaths holds the parsed path alias configuration from tsconfig.json.
type tsConfigPaths struct {
	BaseURL  string              // baseUrl from compilerOptions (relative to tsconfig dir)
	Paths    map[string][]string // paths from compilerOptions (e.g., {"@/*": ["src/*"]})
	RootDir  string              // absolute directory containing tsconfig.json
	matchers []pathMatcher       // pre-compiled alias matchers
}

// pathMatcher is a pre-compiled path alias rule.
type pathMatcher struct {
	prefix string // e.g., "@/" from "@/*"
	suffix string // e.g., "" (after removing "*")
	target string // e.g., "src/" from "src/*"
}

// loadTSConfigPaths reads tsconfig.json (or jsconfig.json) from the project
// root and returns the path alias configuration. Returns nil if no config
// exists or if there are no path aliases.
func loadTSConfigPaths(projectRoot string) *tsConfigPaths {
	for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
		configPath := filepath.Join(projectRoot, name)
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}
		cfg := parseTSConfigJSON(data, filepath.Dir(configPath))
		if cfg != nil && len(cfg.matchers) > 0 {
			return cfg
		}
	}
	return nil
}

// parseTSConfigJSON parses the raw tsconfig.json content and extracts paths.
func parseTSConfigJSON(data []byte, configDir string) *tsConfigPaths {
	var raw struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
		Extends string `json:"extends"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	// Follow extends chain (max depth 3 to prevent cycles).
	// If child has no paths of its own, inherit from parent entirely.
	// If child has its own paths, merge parent paths as fallback (child wins on conflict).
	if raw.Extends != "" {
		parentCfg := followTSConfigExtends(raw.Extends, configDir, 3)
		if parentCfg != nil && len(raw.CompilerOptions.Paths) == 0 {
			return parentCfg
		}
		// Merge parent paths into child (child paths take precedence).
		if parentCfg != nil && len(parentCfg.Paths) > 0 {
			if raw.CompilerOptions.Paths == nil {
				raw.CompilerOptions.Paths = make(map[string][]string)
			}
			for k, v := range parentCfg.Paths {
				if _, exists := raw.CompilerOptions.Paths[k]; !exists {
					raw.CompilerOptions.Paths[k] = v
				}
			}
		}
	}

	if len(raw.CompilerOptions.Paths) == 0 {
		return nil
	}

	baseURL := raw.CompilerOptions.BaseURL
	if baseURL == "" {
		baseURL = "."
	}

	cfg := &tsConfigPaths{
		BaseURL: baseURL,
		Paths:   raw.CompilerOptions.Paths,
		RootDir: configDir,
	}

	// Pre-compile matchers.
	for pattern, targets := range raw.CompilerOptions.Paths {
		if len(targets) == 0 {
			continue
		}
		target := targets[0] // Use first mapping (most common case)

		// Patterns are like "@/*" or "~/*" or "utils/*"
		// Targets are like "src/*" or "./src/*"
		starIdx := strings.IndexByte(pattern, '*')
		targetStarIdx := strings.IndexByte(target, '*')

		if starIdx < 0 || targetStarIdx < 0 {
			// Exact match (no wildcard) — less common but valid.
			cfg.matchers = append(cfg.matchers, pathMatcher{
				prefix: pattern,
				suffix: "",
				target: target,
			})
			continue
		}

		cfg.matchers = append(cfg.matchers, pathMatcher{
			prefix: pattern[:starIdx],
			suffix: pattern[starIdx+1:],
			target: target[:targetStarIdx],
		})
	}

	return cfg
}

// followTSConfigExtends resolves tsconfig extends chains.
func followTSConfigExtends(extends, configDir string, maxDepth int) *tsConfigPaths {
	if maxDepth <= 0 {
		return nil
	}

	var extPath string
	if strings.HasPrefix(extends, ".") {
		extPath = filepath.Join(configDir, extends)
	} else {
		// Could be a package reference like "@tsconfig/node18/tsconfig.json"
		// — skip for now, would need node_modules resolution.
		return nil
	}

	// Add .json extension if missing.
	if filepath.Ext(extPath) == "" {
		extPath += ".json"
	}

	data, err := os.ReadFile(extPath)
	if err != nil {
		return nil
	}

	return parseTSConfigJSON(data, filepath.Dir(extPath))
}

// resolvePathAlias applies tsconfig path alias normalization to an import path.
// Returns the normalized path and true if a match was found, or the original
// path and false if no alias matched.
func (cfg *tsConfigPaths) resolvePathAlias(importPath string) (string, bool) {
	if cfg == nil {
		return importPath, false
	}

	for _, m := range cfg.matchers {
		if !strings.HasPrefix(importPath, m.prefix) {
			continue
		}
		rest := importPath[len(m.prefix):]
		if m.suffix != "" && !strings.HasSuffix(rest, m.suffix) {
			continue
		}
		if m.suffix != "" {
			rest = rest[:len(rest)-len(m.suffix)]
		}
		resolved := m.target + rest

		// Normalize: strip leading "./" and apply baseUrl.
		resolved = strings.TrimPrefix(resolved, "./")
		if cfg.BaseURL != "" && cfg.BaseURL != "." {
			resolved = filepath.Join(cfg.BaseURL, resolved)
		}

		return resolved, true
	}
	return importPath, false
}

// ResolvePathAliases rewrites import package nodes in the graph that match
// tsconfig/jsconfig path aliases. This enables the resolver to match aliased
// imports (e.g., @/components/Foo) to their actual module locations.
//
// Must be called after all files are parsed and before ResolveCallEdges.
// Returns the number of import nodes rewritten.
func ResolvePathAliases(g *graph.Graph) int {
	projectRoot := g.RepoID()
	if projectRoot == "" {
		return 0
	}

	cfg := loadTSConfigPaths(projectRoot)
	if cfg == nil {
		return 0
	}

	rewritten := 0
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodePackage {
			continue
		}
		resolved, matched := cfg.resolvePathAlias(n.Name)
		if !matched {
			continue
		}
		// Rewrite the package node's Name and Package to the resolved path.
		// This allows the resolver's import map to match against actual module paths.
		n.Name = resolved
		n.Package = resolved
		rewritten++
	}
	return rewritten
}
