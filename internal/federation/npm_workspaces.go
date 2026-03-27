package federation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// NPMWorkspace represents a parsed monorepo workspace configuration.
type NPMWorkspace struct {
	RootDir  string            // absolute path to monorepo root
	Packages map[string]string // package name → relative directory (e.g., "@org/auth" → "packages/auth")
}

// ParseNPMWorkspace detects and parses npm/pnpm/yarn workspace configuration
// from a project root. Returns nil if no workspace is detected.
func ParseNPMWorkspace(projectRoot string) *NPMWorkspace {
	ws := &NPMWorkspace{
		RootDir:  projectRoot,
		Packages: make(map[string]string),
	}

	// Try pnpm-workspace.yaml first.
	if patterns := parsePnpmWorkspaceYAML(filepath.Join(projectRoot, "pnpm-workspace.yaml")); len(patterns) > 0 {
		ws.resolveWorkspacePatterns(patterns)
		if len(ws.Packages) > 0 {
			return ws
		}
	}

	// Try package.json workspaces field.
	pkgJSON := filepath.Join(projectRoot, "package.json")
	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return nil
	}

	var pkg struct {
		Workspaces interface{} `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}

	var patterns []string
	switch v := pkg.Workspaces.(type) {
	case []interface{}:
		for _, p := range v {
			if s, ok := p.(string); ok {
				patterns = append(patterns, s)
			}
		}
	case map[string]interface{}:
		// yarn workspaces: { packages: ["packages/*"] }
		if pkgs, ok := v["packages"].([]interface{}); ok {
			for _, p := range pkgs {
				if s, ok := p.(string); ok {
					patterns = append(patterns, s)
				}
			}
		}
	}

	if len(patterns) == 0 {
		return nil
	}

	ws.resolveWorkspacePatterns(patterns)
	if len(ws.Packages) == 0 {
		return nil
	}
	return ws
}

// resolveWorkspacePatterns expands workspace glob patterns to actual packages.
func (ws *NPMWorkspace) resolveWorkspacePatterns(patterns []string) {
	for _, pattern := range patterns {
		// Expand glob patterns like "packages/*" or "apps/*"
		matches, err := filepath.Glob(filepath.Join(ws.RootDir, pattern))
		if err != nil {
			continue
		}
		for _, dir := range matches {
			pkgJSONPath := filepath.Join(dir, "package.json")
			data, err := os.ReadFile(pkgJSONPath)
			if err != nil {
				continue
			}
			var pkg struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(data, &pkg); err != nil || pkg.Name == "" {
				continue
			}
			relDir, _ := filepath.Rel(ws.RootDir, dir)
			ws.Packages[pkg.Name] = relDir
		}
	}
}

// MatchesWorkspacePackage checks if an import path matches a workspace package.
// Returns the package directory and true if matched.
// Prefers longest package name match to avoid ambiguity with overlapping names
// (e.g., "@org/auth" vs "@org/auth-utils").
func (ws *NPMWorkspace) MatchesWorkspacePackage(importPath string) (pkgDir string, matched bool) {
	if ws == nil {
		return "", false
	}
	// Direct match: import "@org/auth" → packages/auth
	if dir, ok := ws.Packages[importPath]; ok {
		return dir, true
	}
	// Prefix match: import "@org/auth/middleware" → packages/auth
	// Use longest match to handle overlapping names deterministically.
	bestName := ""
	bestDir := ""
	for name, dir := range ws.Packages {
		if strings.HasPrefix(importPath, name+"/") && len(name) > len(bestName) {
			bestName = name
			bestDir = dir
		}
	}
	if bestName != "" {
		return bestDir, true
	}
	return "", false
}

// parsePnpmWorkspaceYAML parses a pnpm-workspace.yaml file and returns
// the workspace package patterns. Uses a simple line parser (no YAML dependency).
func parsePnpmWorkspaceYAML(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var patterns []string
	inPackages := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "packages:" {
			inPackages = true
			continue
		}
		if inPackages {
			if strings.HasPrefix(trimmed, "- ") {
				pattern := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
				pattern = strings.Trim(pattern, "'\"")
				patterns = append(patterns, pattern)
			} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				// End of packages section.
				break
			}
		}
	}
	return patterns
}
