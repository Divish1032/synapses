// Package skills provides activation-context prompt templates for Synapses.
// Prompts are Markdown files with YAML frontmatter that Synapses auto-injects
// into agent context when declared patterns (file, entity, module) match.
// They are pure text injection — no code execution — and are always safe to
// load regardless of origin (builtin, user-scoped, or project-scoped).
package skills

import (
	"bufio"
	"embed"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

//go:embed builtin_prompts/*.md
var builtinPromptsFS embed.FS

// PromptTemplate is a named activation-context snippet that Synapses injects
// into agent context when its declared patterns match the queried entity.
type PromptTemplate struct {
	ID            string // unique identifier (from frontmatter "id" field)
	Description   string // one-line summary for prompts/list
	FilePattern   string // glob: match when entity file path matches (e.g. "**/*.go")
	EntityPattern string // regex: match when entity name matches (e.g. ".*Service")
	ModulePattern string // glob: match when entity package path matches (e.g. "internal/*")
	AutoLoad      bool   // if true, include in session_init for project-wide conventions
	Body          string // full Markdown body — the activation context text
	Source        string // "builtin" | "user" | "project"
}

// BuiltinPrompts returns the prompt templates compiled into the binary.
// These cover common Go conventions and Synapses internals.
func BuiltinPrompts() []PromptTemplate {
	var out []PromptTemplate
	entries, err := builtinPromptsFS.ReadDir("builtin_prompts")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := builtinPromptsFS.ReadFile("builtin_prompts/" + entry.Name())
		if err != nil {
			continue
		}
		pt := parsePromptFile(data, "builtin")
		if pt.ID == "" {
			// Fall back to filename stem as ID.
			pt.ID = strings.TrimSuffix(entry.Name(), ".md")
		}
		out = append(out, pt)
	}
	return out
}

// LoadPromptDir reads all .md files from dir and parses them as PromptTemplates.
// source should be "user" or "project". Non-existent directories are silently
// skipped (returns nil, nil) so startup never fails on missing directories.
func LoadPromptDir(dir, source string) ([]PromptTemplate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []PromptTemplate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		fullPath := filepath.Join(dir, entry.Name())
		if fi, err := os.Lstat(fullPath); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			continue // skip unreadable files silently
		}
		pt := parsePromptFile(data, source)
		if pt.ID == "" {
			pt.ID = strings.TrimSuffix(entry.Name(), ".md")
		}
		out = append(out, pt)
	}
	return out, nil
}

// MatchPrompts returns templates whose patterns match the given entity context.
// A template matches if ALL of its non-empty patterns match the inputs (AND semantics).
// A template with file_pattern AND entity_pattern only fires when both the file
// glob and the entity regex both match — not when just one does.
// Templates with no patterns set (only auto_load) are not returned by this
// function — use AutoLoadPrompts to retrieve those.
func MatchPrompts(templates []PromptTemplate, file, entity, pkg string) []PromptTemplate {
	var out []PromptTemplate
	for _, pt := range templates {
		if matchesAll(pt, file, entity, pkg) {
			out = append(out, pt)
		}
	}
	return out
}

// AutoLoadPrompts returns templates marked auto_load: true.
// These are project-wide conventions surfaced in session_init.
func AutoLoadPrompts(templates []PromptTemplate) []PromptTemplate {
	var out []PromptTemplate
	for _, pt := range templates {
		if pt.AutoLoad {
			out = append(out, pt)
		}
	}
	return out
}

// DeduplicatePrompts removes duplicate IDs, keeping the last occurrence per ID.
// However, project-scoped prompts (Source == "project") cannot shadow user or
// builtin prompts — an untrusted project repo must not override user-defined IDs.
func DeduplicatePrompts(templates []PromptTemplate) []PromptTemplate {
	// First pass: collect IDs defined by user or builtin sources.
	protectedIDs := make(map[string]bool)
	for _, pt := range templates {
		if pt.ID != "" && (pt.Source == "user" || pt.Source == "builtin") {
			protectedIDs[pt.ID] = true
		}
	}

	// Second pass: keep last occurrence per ID, but skip project prompts
	// that collide with a protected (user/builtin) ID.
	last := make(map[string]int, len(templates))
	for i, pt := range templates {
		if pt.ID == "" {
			continue
		}
		if protectedIDs[pt.ID] && pt.Source == "project" {
			continue // project cannot shadow user/builtin
		}
		last[pt.ID] = i
	}
	out := make([]PromptTemplate, 0, len(templates))
	for i, pt := range templates {
		if pt.ID == "" || last[pt.ID] == i {
			out = append(out, pt)
		}
	}
	return out
}

// matchesAll returns true if ALL non-empty patterns on the template match their
// respective context fields. A template with no patterns set returns false
// (those are handled by AutoLoadPrompts). AutoLoad templates are also excluded
// here because they are already injected globally via session_init — re-injecting
// them per entity would duplicate content in the agent's context window.
func matchesAll(pt PromptTemplate, file, entity, pkg string) bool {
	if pt.AutoLoad {
		return false // already delivered via session_init; skip per-entity injection
	}
	hasPattern := pt.FilePattern != "" || pt.EntityPattern != "" || pt.ModulePattern != ""
	if !hasPattern {
		return false
	}
	if pt.FilePattern != "" {
		if file == "" || !matchGlob(pt.FilePattern, file) {
			return false
		}
	}
	if pt.EntityPattern != "" {
		if entity == "" || !matchRegex(pt.EntityPattern, entity) {
			return false
		}
	}
	if pt.ModulePattern != "" {
		if pkg == "" || !matchGlob(pt.ModulePattern, pkg) {
			return false
		}
	}
	return true
}

// matchGlob matches a glob pattern against a path.
// Handles the common "**/*.ext" prefix by checking only the base name,
// and "**" anywhere to mean any path depth.
func matchGlob(pattern, path string) bool {
	// "**/*.go" → check if the file's base name matches "*.go"
	if strings.HasPrefix(pattern, "**/") {
		ok, _ := filepath.Match(pattern[3:], filepath.Base(path))
		return ok
	}
	// "internal/**" → check if path starts with the prefix
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(path, pattern[:len(pattern)-3])
	}
	ok, _ := filepath.Match(pattern, path)
	return ok
}

// regexCache is a bounded cache of compiled regexes to avoid recompiling on
// every MatchPrompts call. When the cache exceeds maxRegexCacheSize entries,
// it is cleared entirely (simple LRU-eviction alternative).
var (
	regexCacheMu      sync.RWMutex
	regexCacheMap     = make(map[string]*regexp.Regexp)
	maxRegexCacheSize = 1000
)

// matchRegex matches a regex pattern against a name.
// Compiled regexes are cached globally with a bounded map. Invalid patterns
// are silently treated as non-matching.
func matchRegex(pattern, name string) bool {
	// Fast path: read lock for cache hit.
	regexCacheMu.RLock()
	re, ok := regexCacheMap[pattern]
	regexCacheMu.RUnlock()
	if ok {
		return re.MatchString(name)
	}

	// Reject overly complex patterns to limit CPU usage from project-controlled content.
	if len(pattern) > 1024 {
		return false
	}
	// Slow path: compile and store under write lock.
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	regexCacheMu.Lock()
	// Double-check after acquiring write lock.
	if existing, exists := regexCacheMap[pattern]; exists {
		regexCacheMu.Unlock()
		return existing.MatchString(name)
	}
	// Evict all entries when cache is full.
	if len(regexCacheMap) >= maxRegexCacheSize {
		regexCacheMap = make(map[string]*regexp.Regexp)
	}
	regexCacheMap[pattern] = compiled
	regexCacheMu.Unlock()
	return compiled.MatchString(name)
}

// parsePromptFile parses a Markdown file with optional YAML frontmatter.
// Frontmatter is delimited by "---" lines at the start of the file.
// Supported flat keys: id, description, file_pattern, entity_pattern,
// module_pattern, auto_load. Missing or malformed frontmatter is silently
// skipped — the whole file content is used as the body.
func parsePromptFile(data []byte, source string) PromptTemplate {
	var pt PromptTemplate
	pt.Source = source

	content := strings.TrimSpace(string(data))
	if !strings.HasPrefix(content, "---") {
		pt.Body = content
		return pt
	}

	// Strip the opening "---\n"
	rest := content[3:]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	}

	// Find the closing "---"
	closeIdx := strings.Index(rest, "\n---")
	if closeIdx < 0 {
		// No closing delimiter — treat whole file as body
		pt.Body = content
		return pt
	}

	frontmatter := strings.TrimSpace(rest[:closeIdx])
	body := strings.TrimSpace(rest[closeIdx+4:]) // skip "\n---"
	// Skip the rest of the closing delimiter line (e.g. "---\n" or "---")
	if nl := strings.Index(body, "\n"); nl >= 0 && strings.TrimSpace(body[:nl]) == "" {
		body = strings.TrimSpace(body[nl:])
	}

	// Parse flat "key: value" frontmatter (no nested YAML needed).
	scanner := bufio.NewScanner(strings.NewReader(frontmatter))
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// Strip surrounding quotes if present.
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		switch key {
		case "id":
			pt.ID = val
		case "description":
			pt.Description = val
		case "file_pattern":
			pt.FilePattern = val
		case "entity_pattern":
			pt.EntityPattern = val
		case "module_pattern":
			pt.ModulePattern = val
		case "auto_load":
			pt.AutoLoad, _ = strconv.ParseBool(val)
		}
	}
	pt.Body = body
	return pt
}
