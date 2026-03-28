package federation

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// GoModule represents a parsed go.mod file.
type GoModule struct {
	ModulePath string            // module path (e.g., "github.com/org/repo")
	GoVersion  string            // go version (e.g., "1.21")
	Require    []GoModRequire    // required dependencies
	Replace    map[string]string // replace directives: old module → new path/module
}

// GoModRequire is a single require directive from go.mod.
type GoModRequire struct {
	Path    string // module path (e.g., "github.com/foo/bar")
	Version string // version (e.g., "v1.2.3")
}

// ParseGoMod reads and parses a go.mod file, extracting module path,
// require directives, and replace directives.
func ParseGoMod(goModPath string) (*GoModule, error) {
	f, err := os.Open(goModPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	mod := &GoModule{
		Replace: make(map[string]string),
	}

	scanner := bufio.NewScanner(f)
	inRequire := false
	inReplace := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines.
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		// Handle block open/close.
		if line == ")" {
			inRequire = false
			inReplace = false
			continue
		}

		if strings.HasPrefix(line, "module ") {
			mod.ModulePath = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			continue
		}
		if strings.HasPrefix(line, "go ") {
			mod.GoVersion = strings.TrimSpace(strings.TrimPrefix(line, "go "))
			continue
		}

		if line == "require (" {
			inRequire = true
			continue
		}
		if line == "replace (" {
			inReplace = true
			continue
		}

		// Single-line require: require github.com/foo/bar v1.0.0
		if strings.HasPrefix(line, "require ") {
			parts := strings.Fields(strings.TrimPrefix(line, "require "))
			if len(parts) >= 2 {
				mod.Require = append(mod.Require, GoModRequire{
					Path:    parts[0],
					Version: parts[1],
				})
			}
			continue
		}

		// Single-line replace: replace github.com/foo/bar => ../local-bar
		if strings.HasPrefix(line, "replace ") {
			parseReplaceLine(line[len("replace "):], mod)
			continue
		}

		if inRequire {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				mod.Require = append(mod.Require, GoModRequire{
					Path:    parts[0],
					Version: parts[1],
				})
			}
		}

		if inReplace {
			parseReplaceLine(line, mod)
		}
	}

	return mod, scanner.Err()
}

func parseReplaceLine(line string, mod *GoModule) {
	// Format: old => new [version]
	// or: old version => new [version]
	parts := strings.SplitN(line, "=>", 2)
	if len(parts) != 2 {
		return
	}
	oldParts := strings.Fields(strings.TrimSpace(parts[0]))
	newParts := strings.Fields(strings.TrimSpace(parts[1]))
	if len(oldParts) == 0 || len(newParts) == 0 {
		return
	}
	mod.Replace[oldParts[0]] = newParts[0]
}

// FindGoMod searches for go.mod in the given directory and parent directories.
func FindGoMod(dir string) string {
	for {
		candidate := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// MatchesSibling checks if a Go import path matches a sibling project's module path.
// Returns the sibling alias and true if matched, or empty and false.
func (m *GoModule) MatchesSibling(importPath string, siblingModules map[string]string) (alias string, matched bool) {
	// Apply replace directives first.
	for old, replacement := range m.Replace {
		if strings.HasPrefix(importPath, old) {
			importPath = replacement + importPath[len(old):]
			break
		}
	}

	// Check if the import path is a prefix of any sibling module.
	for modPath, sibAlias := range siblingModules {
		if importPath == modPath || strings.HasPrefix(importPath, modPath+"/") {
			return sibAlias, true
		}
	}
	return "", false
}
