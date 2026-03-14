package scout

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TechStackEntry represents one detected dependency with optional doc enrichment.
type TechStackEntry struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Ecosystem string `json:"ecosystem"` // "go", "node", "python", "rust", "java", "unknown"
	DocURL    string `json:"doc_url,omitempty"`
	Snippet   string `json:"snippet,omitempty"`
}

// maxDeps is the maximum number of dependencies surfaced per project.
const maxDeps = 10

// DetectTechStack reads well-known manifest files in projectRoot and returns
// up to maxDeps dependency entries. It is fast (pure file I/O, no network)
// and always runs regardless of whether scout is configured.
func DetectTechStack(projectRoot string) []TechStackEntry {
	var entries []TechStackEntry

	// Try each manifest parser in priority order.
	// Each appends to entries until maxDeps is reached.
	parsers := []func(string) []TechStackEntry{
		parseGoMod,
		parsePackageJSON,
		parseRequirementsTxt,
		parseCargoToml,
		parsePyprojectToml,
	}
	for _, p := range parsers {
		if len(entries) >= maxDeps {
			break
		}
		got := p(projectRoot)
		for _, e := range got {
			if len(entries) >= maxDeps {
				break
			}
			entries = append(entries, e)
		}
	}
	return entries
}

// ── manifest parsers ──────────────────────────────────────────────────────────

func parseGoMod(root string) []TechStackEntry {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil
	}

	var entries []TechStackEntry
	inRequire := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}
		// Single-line require: require github.com/foo/bar v1.2.3
		if strings.HasPrefix(line, "require ") {
			line = strings.TrimPrefix(line, "require ")
			inRequire = false
		}
		if inRequire || strings.HasPrefix(line, "github.com/") || strings.HasPrefix(line, "golang.org/") {
			parts := strings.Fields(line)
			if len(parts) >= 2 && !strings.HasSuffix(parts[1], "// indirect") {
				name := moduleShortName(parts[0])
				version := parts[1]
				if len(parts) >= 3 && parts[2] == "//" {
					continue // skip indirect deps
				}
				entries = append(entries, TechStackEntry{
					Name:      name,
					Version:   version,
					Ecosystem: "go",
				})
			}
		}
	}
	// Sort by name for stability, cap at maxDeps.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if len(entries) > maxDeps {
		entries = entries[:maxDeps]
	}
	return entries
}

// moduleShortName extracts a readable short name from a Go module path.
// github.com/mark3labs/mcp-go → mcp-go
func moduleShortName(modPath string) string {
	parts := strings.Split(modPath, "/")
	if len(parts) == 0 {
		return modPath
	}
	return parts[len(parts)-1]
}

func parsePackageJSON(root string) []TechStackEntry {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}
	var entries []TechStackEntry
	// Production deps first, then dev deps.
	for name, ver := range pkg.Dependencies {
		entries = append(entries, TechStackEntry{
			Name:      name,
			Version:   strings.TrimLeft(ver, "^~"),
			Ecosystem: "node",
		})
	}
	for name, ver := range pkg.DevDependencies {
		entries = append(entries, TechStackEntry{
			Name:      name,
			Version:   strings.TrimLeft(ver, "^~"),
			Ecosystem: "node",
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if len(entries) > maxDeps {
		entries = entries[:maxDeps]
	}
	return entries
}

func parseRequirementsTxt(root string) []TechStackEntry {
	data, err := os.ReadFile(filepath.Join(root, "requirements.txt"))
	if err != nil {
		return nil
	}
	var entries []TechStackEntry
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// name==version or name>=version or just name
		name := line
		version := ""
		for _, sep := range []string{"==", ">=", "<=", "~=", "!="} {
			if idx := strings.Index(line, sep); idx >= 0 {
				name = line[:idx]
				version = line[idx+len(sep):]
				break
			}
		}
		entries = append(entries, TechStackEntry{
			Name:      strings.TrimSpace(name),
			Version:   strings.TrimSpace(version),
			Ecosystem: "python",
		})
	}
	if len(entries) > maxDeps {
		entries = entries[:maxDeps]
	}
	return entries
}

func parseCargoToml(root string) []TechStackEntry {
	data, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil {
		return nil
	}
	var entries []TechStackEntry
	inDeps := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "[dependencies]" || line == "[dev-dependencies]" {
			inDeps = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			inDeps = false
			continue
		}
		if inDeps && strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			name := strings.TrimSpace(parts[0])
			version := strings.Trim(strings.TrimSpace(parts[1]), `"{}`)
			entries = append(entries, TechStackEntry{
				Name:      name,
				Version:   version,
				Ecosystem: "rust",
			})
		}
	}
	if len(entries) > maxDeps {
		entries = entries[:maxDeps]
	}
	return entries
}

func parsePyprojectToml(root string) []TechStackEntry {
	data, err := os.ReadFile(filepath.Join(root, "pyproject.toml"))
	if err != nil {
		return nil
	}
	var entries []TechStackEntry
	inDeps := false
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Match [tool.poetry.dependencies] or [project] with dependencies list.
		if line == "[tool.poetry.dependencies]" || line == "[project]" {
			inDeps = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			inDeps = false
			continue
		}
		if inDeps && strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			name := strings.TrimSpace(parts[0])
			if name == "python" || name == "name" || name == "version" || name == "description" {
				continue
			}
			version := strings.Trim(strings.TrimSpace(parts[1]), `"^~`)
			entries = append(entries, TechStackEntry{
				Name:      name,
				Version:   version,
				Ecosystem: "python",
			})
		}
	}
	if len(entries) > maxDeps {
		entries = entries[:maxDeps]
	}
	return entries
}
