// Package federation — tracker.go provides Tier 1 deterministic cross-project
// dependency detection. It reads language manifests (go.mod, package.json,
// Cargo.toml) from sibling projects to build a module index, then matches
// import statements in local files against the index.
//
// Detected entities are resolved against the sibling's parsed graph (not regex)
// to populate ToFile and VerifiedSignature — enabling graph-first drift
// detection from day one.
//
// Tier 1 covers Go, TypeScript/JavaScript, and Rust — languages with
// deterministic import paths. Tier 2 (Brain LLM) extends coverage to
// ambiguous languages in Phase 5.
package federation

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/store"
)

// RawCrossDep is an unverified cross-project dependency detected from import
// analysis. The tracker resolves these against the sibling store before
// converting to store.CrossProjectDep for storage.
type RawCrossDep struct {
	FromFile      string // local file path containing the import
	FromImport    string // the import path that matched
	ToProject     string // federation alias
	ToEntity      string // entity name referenced in the import
	DetectionTier string // "deterministic" or "brain"
}

// moduleEntry maps an import prefix to a federation sibling.
type moduleEntry struct {
	Alias    string // federation alias
	Prefix   string // import prefix (e.g., "github.com/user/synapses")
	Lang     string // "go", "typescript", "rust"
	RepoPath string // absolute path to sibling project root
}

// DeterministicDetector detects cross-project dependencies by matching
// import statements against a module index built from sibling manifests.
//
// Thread-safe: mu protects modules so Rebuild() and DetectDeps() can run
// concurrently. Entity resolution goes through the Resolver which has its
// own synchronization.
type DeterministicDetector struct {
	mu       sync.RWMutex
	modules  []moduleEntry // all known module prefixes
	resolver *Resolver     // for querying sibling stores (entity resolution)
}

// NewDeterministicDetector creates a detector and builds the module index
// from federation entries' manifest files. Errors reading individual manifests
// are logged and skipped (fail-open).
func NewDeterministicDetector(entries []config.FederationEntry, resolver *Resolver) *DeterministicDetector {
	d := &DeterministicDetector{
		resolver: resolver,
	}
	d.buildModuleIndex(entries)
	return d
}

// buildModuleIndex reads go.mod, package.json, and Cargo.toml from each
// sibling project and populates the module index.
func (d *DeterministicDetector) buildModuleIndex(entries []config.FederationEntry) {
	for _, e := range entries {
		// Go: read go.mod for module path.
		if mod := readGoMod(e.Path); mod != "" {
			d.modules = append(d.modules, moduleEntry{
				Alias: e.Alias, Prefix: mod, Lang: "go", RepoPath: e.Path,
			})
		}

		// TypeScript/JavaScript: read package.json for package name.
		if pkg := readPackageJSON(e.Path); pkg != "" {
			d.modules = append(d.modules, moduleEntry{
				Alias: e.Alias, Prefix: pkg, Lang: "typescript", RepoPath: e.Path,
			})
		}

		// Rust: read Cargo.toml for crate name.
		if crate := readCargoToml(e.Path); crate != "" {
			d.modules = append(d.modules, moduleEntry{
				Alias: e.Alias, Prefix: crate, Lang: "rust", RepoPath: e.Path,
			})
		}
	}
}

// DetectDeps scans a local file for cross-project import references and
// resolves each one against the sibling's parsed graph. Returns fully
// populated CrossProjectDep structs ready for upserting.
//
// The file's language is determined by extension. Unsupported languages
// return nil (Tier 2 Brain handles those in Phase 5).
//
// Entity resolution uses the sibling store's graph — not regex, not git.
// This means every dep gets a VerifiedSignature from the parsed graph,
// enabling graph-first drift detection immediately.
// Rebuild replaces the module index with a fresh one built from entries.
// Safe to call concurrently with DetectDeps — protected by mu.
func (d *DeterministicDetector) Rebuild(entries []config.FederationEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.modules = nil
	d.buildModuleIndex(entries)
}

func (d *DeterministicDetector) DetectDeps(ctx context.Context, filePath string, localStore *store.Store) []store.CrossProjectDep {
	// Hold RLock for the entire detection so Rebuild() cannot replace d.modules
	// mid-scan. Rebuild() only happens on config reload (infrequent); the
	// short lock hold here does not cause meaningful contention.
	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.modules) == 0 || ctx.Err() != nil {
		return nil
	}

	lang := langFromExt(filePath)
	if lang == "" {
		return nil // unsupported language
	}

	// Reject symlinks before reading to prevent exfiltration of files
	// outside the project root via crafted symlinks.
	lfi, lstatErr := os.Lstat(filePath)
	if lstatErr != nil {
		return nil // fail-open
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		return nil // never read through symlinks
	}

	// Skip files larger than 1MB — they are typically generated or vendored
	// and cause excessive allocation with no meaningful import signal.
	if lfi.Size() > 1*1024*1024 {
		return nil
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil // fail-open
	}

	// Extract import statements and the entities they reference.
	refs := d.extractCrossProjectRefs(string(content), lang)
	if len(refs) == 0 {
		return nil
	}

	// Use the absolute file path for FromEntity. A relative path would be
	// cleaner but requires knowing the local project root, which isn't
	// available here (the resolver only knows sibling paths).
	relPath := filePath

	// Get sibling HEAD commits for VerifiedCommit.
	now := time.Now().UTC().Format(time.RFC3339)

	var deps []store.CrossProjectDep
	for _, ref := range refs {
		if ctx.Err() != nil {
			break
		}

		// Resolve entity against sibling's graph.
		sibStore := d.resolver.getStore(ref.alias)
		if sibStore == nil {
			continue // sibling store unavailable — skip silently
		}

		results, err := sibStore.FindNodesByNameCtx(ctx, ref.entity, 1)
		if err != nil || len(results) == 0 {
			continue // entity not found — skip (anti-hallucination)
		}

		// Extract file from node ID: "repoID::file::name" → file
		toFile := fileFromNodeID(results[0].ID)

		// Get sibling HEAD for VerifiedCommit.
		head := d.getSiblingHead(ctx, ref.alias)

		deps = append(deps, store.CrossProjectDep{
			FromEntity:        "file:" + relPath,
			ToProject:         ref.alias,
			ToEntity:          ref.entity,
			ToFile:            toFile,
			VerifiedCommit:    head,
			VerifiedAt:        now,
			DetectionTier:     "tier1",
			VerifiedSignature: results[0].Signature,
		})
	}
	return deps
}

// ResolveBrainDeps takes deps detected by the brain detector and converts
// them to fully populated CrossProjectDep structs with entity resolution
// and verified signatures. Deduplicates against existing Tier 1 deps.
func (d *DeterministicDetector) ResolveBrainDeps(
	ctx context.Context,
	brainDeps []RawCrossDep,
	tier1Deps []store.CrossProjectDep,
	filePath string,
) []store.CrossProjectDep {
	if len(brainDeps) == 0 {
		return nil
	}

	// Build a set of already-detected (project, entity) pairs from Tier 1.
	existing := make(map[string]bool, len(tier1Deps))
	for _, d := range tier1Deps {
		existing[d.ToProject+"::"+d.ToEntity] = true
	}

	relPath := filePath
	now := time.Now().UTC().Format(time.RFC3339)
	var deps []store.CrossProjectDep

	for _, raw := range brainDeps {
		if ctx.Err() != nil {
			break
		}

		// Skip if Tier 1 already detected this dep.
		key := raw.ToProject + "::" + raw.ToEntity
		if existing[key] {
			continue
		}

		// Entity was already validated by BrainDetector.validateBrainDeps,
		// but we still need the signature for drift detection.
		sibStore := d.resolver.getStore(raw.ToProject)
		if sibStore == nil {
			continue
		}

		results, err := sibStore.FindNodesByNameCtx(ctx, raw.ToEntity, 1)
		if err != nil || len(results) == 0 {
			continue
		}

		toFile := fileFromNodeID(results[0].ID)
		head := d.getSiblingHead(ctx, raw.ToProject)

		deps = append(deps, store.CrossProjectDep{
			FromEntity:        "file:" + relPath,
			ToProject:         raw.ToProject,
			ToEntity:          raw.ToEntity,
			ToFile:            toFile,
			VerifiedCommit:    head,
			VerifiedAt:        now,
			DetectionTier:     "tier2",
			VerifiedSignature: results[0].Signature,
		})
		existing[key] = true // prevent duplicates within brain results
	}
	return deps
}

// crossProjectRef is an entity reference detected in a local file that
// targets a sibling project.
type crossProjectRef struct {
	alias  string // federation alias
	entity string // entity name in sibling
}

// extractCrossProjectRefs scans file content for import statements and
// identifies which imported entities belong to sibling projects.
func (d *DeterministicDetector) extractCrossProjectRefs(content, lang string) []crossProjectRef {
	switch lang {
	case "go":
		return d.extractGoRefs(content)
	case "typescript":
		return d.extractTSRefs(content)
	case "rust":
		return d.extractRustRefs(content)
	default:
		return nil
	}
}

// ── Go import extraction ────────────────────────────────────────────────────

// goImportRe matches individual import lines: `"path"` or `alias "path"`
var goImportRe = regexp.MustCompile(`^\s*(?:(\w+)\s+)?"([^"]+)"`)

// aliasPatternCache caches compiled regex patterns for Go import alias
// entity reference scanning. Key: alias name, Value: compiled regex.
// Bounded at 10 K entries (alias cardinality is low but unbounded in
// theory) — evicted entries are recomputed on next access.
var aliasPatternCache = newBoundedCache[string, *regexp.Regexp](10_000)

// getAliasPattern returns a cached regex for scanning `alias.ExportedName` references.
func getAliasPattern(alias string) *regexp.Regexp {
	if cached, ok := aliasPatternCache.load(alias); ok {
		return cached
	}
	pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(alias) + `\.([A-Z]\w*)`)
	aliasPatternCache.store(alias, pattern)
	return pattern
}

// extractGoRefs parses Go import statements and scans for entity references.
//
// Strategy:
//  1. Extract all import paths and their aliases from import blocks.
//  2. Match import paths against module index.
//  3. For matched imports, scan code for `alias.EntityName` patterns.
func (d *DeterministicDetector) extractGoRefs(content string) []crossProjectRef {
	// Step 1: Extract imports with their aliases.
	type goImport struct {
		alias      string // explicit alias, or last segment of path
		path       string // full import path
		fedAlias   string // federation alias if matches a sibling
		fedPrefix  string // matched module prefix
	}

	var imports []goImport
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "import (") {
			inBlock = true
			continue
		}
		if inBlock && line == ")" {
			inBlock = false
			continue
		}

		// Single-line import: import "path" or import alias "path"
		if strings.HasPrefix(line, "import ") && !inBlock {
			line = strings.TrimPrefix(line, "import ")
			line = strings.TrimSpace(line)
		}

		if inBlock || strings.HasPrefix(line, "\"") || goImportRe.MatchString(line) {
			m := goImportRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			aliasName := m[1]
			importPath := m[2]

			if aliasName == "_" {
				continue // blank imports are for side effects (init) — no entity refs
			}
			if aliasName == "" {
				// Use last segment of import path as alias.
				parts := strings.Split(importPath, "/")
				aliasName = parts[len(parts)-1]
			}
			if aliasName == "." {
				continue // dot imports are ambiguous — skip for Tier 1
			}

			imports = append(imports, goImport{alias: aliasName, path: importPath})
		}
	}

	// Step 2: Match against module index.
	var matchedImports []goImport
	for i := range imports {
		for _, mod := range d.modules {
			if mod.Lang != "go" {
				continue
			}
			if imports[i].path == mod.Prefix || strings.HasPrefix(imports[i].path, mod.Prefix+"/") {
				imports[i].fedAlias = mod.Alias
				imports[i].fedPrefix = mod.Prefix
				matchedImports = append(matchedImports, imports[i])
				break
			}
		}
	}

	if len(matchedImports) == 0 {
		return nil
	}

	// Step 3: Scan for entity references: `alias.EntityName`
	// Strip comments and string literals to avoid false positives from
	// `// auth.Validate handles tokens` or `"auth.Validate failed"`.
	codeOnly := stripGoCommentsAndStrings(content)

	seen := make(map[string]bool) // dedup: "alias:Entity"
	var refs []crossProjectRef

	for _, imp := range matchedImports {
		// Build regex for this import's alias: `alias.ExportedName`
		// Go exported names start with uppercase.
		// Cache compiled patterns per alias to avoid recompilation across files.
		pattern := getAliasPattern(imp.alias)
		matches := pattern.FindAllStringSubmatch(codeOnly, -1)
		for _, m := range matches {
			entityName := m[1]
			key := imp.fedAlias + ":" + entityName
			if seen[key] {
				continue
			}
			seen[key] = true
			refs = append(refs, crossProjectRef{
				alias:  imp.fedAlias,
				entity: entityName,
			})
		}
	}
	return refs
}

// stripGoCommentsAndStrings removes Go comments and string/rune literals from
// source code, replacing them with whitespace. This prevents false positive
// entity reference detection from comments like `// auth.Validate handles...`
// or strings like `"auth.Validate failed"`.
//
// Handles: // line comments, /* block comments */, "strings", `raw strings`, 'runes'
func stripGoCommentsAndStrings(src string) string {
	var b strings.Builder
	b.Grow(len(src))

	i := 0
	for i < len(src) {
		// Line comment: //
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
			b.WriteByte(' ')
			i += 2
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		// Block comment: /* ... */
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			b.WriteByte(' ')
			i += 2
			for i+1 < len(src) {
				if src[i] == '*' && src[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			continue
		}
		// Double-quoted string: "..."
		if src[i] == '"' {
			b.WriteByte(' ')
			i++
			for i < len(src) && src[i] != '"' {
				if src[i] == '\\' && i+1 < len(src) {
					i += 2 // skip escaped char
				} else {
					i++
				}
			}
			if i < len(src) {
				i++ // skip closing "
			}
			continue
		}
		// Raw string: `...`
		if src[i] == '`' {
			b.WriteByte(' ')
			i++
			for i < len(src) && src[i] != '`' {
				i++
			}
			if i < len(src) {
				i++ // skip closing `
			}
			continue
		}
		// Rune literal: '...'
		if src[i] == '\'' {
			b.WriteByte(' ')
			i++
			for i < len(src) && src[i] != '\'' {
				if src[i] == '\\' && i+1 < len(src) {
					i += 2
				} else {
					i++
				}
			}
			if i < len(src) {
				i++
			}
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}

// ── TypeScript/JavaScript import extraction ─────────────────────────────────

// tsNamedImportRe: import { Foo, Bar } from "package"
var tsNamedImportRe = regexp.MustCompile(`import\s*\{([^}]+)\}\s*from\s*["']([^"']+)["']`)

// tsDefaultImportRe: import Foo from "package"
var tsDefaultImportRe = regexp.MustCompile(`import\s+(\w+)\s+from\s*["']([^"']+)["']`)

// tsRequireRe: const { Foo } = require("package") or const Foo = require("package")
var tsRequireRe = regexp.MustCompile(`require\s*\(\s*["']([^"']+)["']\s*\)`)

// tsRequireDestructureRe: const { Foo, Bar } = require("package")
var tsRequireDestructureRe = regexp.MustCompile(`(?:const|let|var)\s*\{([^}]+)\}\s*=\s*require\s*\(\s*["']([^"']+)["']\s*\)`)

// extractTSRefs parses TypeScript/JavaScript import and require statements.
//
// Strategy: TS imports explicitly name the imported entities, so we get
// entity names directly from the import statement — no code scanning needed.
func (d *DeterministicDetector) extractTSRefs(content string) []crossProjectRef {
	seen := make(map[string]bool)
	var refs []crossProjectRef

	// Named imports: import { Foo, Bar } from "package"
	for _, m := range tsNamedImportRe.FindAllStringSubmatch(content, -1) {
		names := m[1]
		path := m[2]
		alias := d.matchTSModule(path)
		if alias == "" {
			continue
		}
		for _, name := range splitTSNames(names) {
			key := alias + ":" + name
			if !seen[key] {
				seen[key] = true
				refs = append(refs, crossProjectRef{alias: alias, entity: name})
			}
		}
	}

	// Default imports: import Foo from "package"
	for _, m := range tsDefaultImportRe.FindAllStringSubmatch(content, -1) {
		name := m[1]
		path := m[2]
		alias := d.matchTSModule(path)
		if alias == "" {
			continue
		}
		key := alias + ":" + name
		if !seen[key] {
			seen[key] = true
			refs = append(refs, crossProjectRef{alias: alias, entity: name})
		}
	}

	// Destructured require: const { Foo, Bar } = require("package")
	for _, m := range tsRequireDestructureRe.FindAllStringSubmatch(content, -1) {
		names := m[1]
		path := m[2]
		alias := d.matchTSModule(path)
		if alias == "" {
			continue
		}
		for _, name := range splitTSNames(names) {
			key := alias + ":" + name
			if !seen[key] {
				seen[key] = true
				refs = append(refs, crossProjectRef{alias: alias, entity: name})
			}
		}
	}

	return refs
}

// matchTSModule checks if a TS/JS import path matches a sibling module.
func (d *DeterministicDetector) matchTSModule(importPath string) string {
	for _, mod := range d.modules {
		if mod.Lang != "typescript" {
			continue
		}
		if importPath == mod.Prefix || strings.HasPrefix(importPath, mod.Prefix+"/") {
			return mod.Alias
		}
	}
	return ""
}

// splitTSNames splits "Foo, Bar as Baz, Qux" into ["Foo", "Bar", "Qux"].
// Handles `as` aliases by extracting the original name.
func splitTSNames(nameList string) []string {
	var names []string
	for _, part := range strings.Split(nameList, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		// Handle "Foo as Bar" → take "Foo" (the original name in the sibling).
		if idx := strings.Index(name, " as "); idx >= 0 {
			name = strings.TrimSpace(name[:idx])
		}
		// Handle "type Foo" → take "Foo" (TypeScript type imports).
		name = strings.TrimPrefix(name, "type ")
		name = strings.TrimSpace(name)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// ── Rust import extraction ──────────────────────────────────────────────────

// rustUseRe: use crate_name::module::Entity;
var rustUseRe = regexp.MustCompile(`use\s+([\w]+(?:::\w+)*);`)

// rustUseGroupRe: use crate_name::module::{Entity1, Entity2};
var rustUseGroupRe = regexp.MustCompile(`use\s+([\w]+(?:::\w+)*)::\{([^}]+)\};`)

// rustUseWildcardRe: use crate_name::module::*;
var rustUseWildcardRe = regexp.MustCompile(`use\s+([\w]+(?:::\w+)*)::\*;`)

// extractRustRefs parses Rust use statements.
//
// Strategy: Rust use statements explicitly name the imported entities,
// similar to TypeScript.
func (d *DeterministicDetector) extractRustRefs(content string) []crossProjectRef {
	seen := make(map[string]bool)
	var refs []crossProjectRef

	// Grouped use: use crate::module::{Entity1, Entity2};
	for _, m := range rustUseGroupRe.FindAllStringSubmatch(content, -1) {
		prefix := m[1]
		names := m[2]
		alias := d.matchRustCrate(prefix)
		if alias == "" {
			continue
		}
		for _, name := range splitRustNames(names) {
			key := alias + ":" + name
			if !seen[key] {
				seen[key] = true
				refs = append(refs, crossProjectRef{alias: alias, entity: name})
			}
		}
	}

	// Single use: use crate::module::Entity;
	for _, m := range rustUseRe.FindAllStringSubmatch(content, -1) {
		path := m[1]
		alias := d.matchRustCrate(path)
		if alias == "" {
			continue
		}
		// Entity name is the last segment.
		parts := strings.Split(path, "::")
		entity := parts[len(parts)-1]
		// Skip if it looks like a module name (lowercase) — we want types/functions.
		// Rust convention: types are CamelCase, functions are snake_case.
		// Both are valid cross-project deps, so include all.
		key := alias + ":" + entity
		if !seen[key] {
			seen[key] = true
			refs = append(refs, crossProjectRef{alias: alias, entity: entity})
		}
	}

	// Wildcard use: use crate::module::*;
	// Can't determine specific entities — skip for Tier 1.
	// Tier 2 (Brain) can resolve these.

	return refs
}

// matchRustCrate checks if a Rust use path's root crate matches a sibling.
func (d *DeterministicDetector) matchRustCrate(usePath string) string {
	root := usePath
	if idx := strings.Index(usePath, "::"); idx >= 0 {
		root = usePath[:idx]
	}
	for _, mod := range d.modules {
		if mod.Lang != "rust" {
			continue
		}
		// Rust crate names use underscores; the Cargo.toml name may use hyphens.
		// Normalize both for comparison.
		if normalizeRustCrate(root) == normalizeRustCrate(mod.Prefix) {
			return mod.Alias
		}
	}
	return ""
}

// normalizeRustCrate converts hyphens to underscores for crate name comparison.
func normalizeRustCrate(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

// splitRustNames splits "Entity1, Entity2, self" into ["Entity1", "Entity2"].
// Filters out `self` which refers to the module itself, not an entity.
func splitRustNames(nameList string) []string {
	var names []string
	for _, part := range strings.Split(nameList, ",") {
		name := strings.TrimSpace(part)
		if name == "" || name == "self" {
			continue
		}
		names = append(names, name)
	}
	return names
}

// ── Manifest readers ────────────────────────────────────────────────────────

// safeReadManifest resolves symlinks and validates that the target file still
// resides under projectPath before reading. This prevents symlink-based path
// traversal when reading untrusted project manifests.
func safeReadManifest(projectPath, filename string) ([]byte, error) {
	target := filepath.Join(projectPath, filename)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil, err
	}
	absProject, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		absProject, err = filepath.Abs(projectPath)
		if err != nil {
			return nil, err
		}
	}
	absProject = filepath.Clean(absProject) + string(filepath.Separator)
	resolved = filepath.Clean(resolved)
	if !strings.HasPrefix(resolved, absProject) && resolved != filepath.Clean(absProject[:len(absProject)-1]) {
		return nil, fmt.Errorf("manifest %s resolves outside project: %s", filename, resolved)
	}
	return os.ReadFile(resolved)
}

// goModRe matches the `module` directive in go.mod.
var goModRe = regexp.MustCompile(`^module\s+(\S+)`)

// readGoMod extracts the module path from go.mod.
// Returns "" if go.mod doesn't exist or can't be parsed.
func readGoMod(projectPath string) string {
	data, err := safeReadManifest(projectPath, "go.mod")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if m := goModRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// pkgNameRe matches "name": "value" in package.json (handles inline JSON too).
var pkgNameRe = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)

// readPackageJSON extracts the package name from package.json.
// Returns "" if package.json doesn't exist or has no name field.
func readPackageJSON(projectPath string) string {
	data, err := safeReadManifest(projectPath, "package.json")
	if err != nil {
		return ""
	}
	m := pkgNameRe.FindSubmatch(data)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// cargoNameRe matches `name = "crate_name"` in Cargo.toml's [package] section.
var cargoNameRe = regexp.MustCompile(`^name\s*=\s*"([^"]+)"`)

// readCargoToml extracts the crate name from Cargo.toml.
// Returns "" if Cargo.toml doesn't exist or has no name field.
func readCargoToml(projectPath string) string {
	data, err := safeReadManifest(projectPath, "Cargo.toml")
	if err != nil {
		return ""
	}
	inPackage := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "[package]" {
			inPackage = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			inPackage = false
			continue
		}
		if inPackage {
			if m := cargoNameRe.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// ── Utilities ───────────────────────────────────────────────────────────────

// langFromExt returns the language identifier for a file extension.
// Returns "" for unsupported languages.
func langFromExt(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return "typescript"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".cxx", ".hpp":
		return "cpp"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".php":
		return "php"
	default:
		return ""
	}
}

// fileFromNodeID extracts the file path from a node ID.
// Node ID format: "repoID::file::name"
// Returns "" if the ID doesn't follow the expected format.
func fileFromNodeID(id string) string {
	// Split on "::" — expect at least 3 parts: repoID, file, name.
	// The name might contain "." (Go methods) but not "::".
	parts := strings.SplitN(id, "::", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// getSiblingHead returns the cached or fresh HEAD commit for a sibling.
// Delegates to Resolver.CachedHead to avoid direct access to resolver internals.
func (d *DeterministicDetector) getSiblingHead(ctx context.Context, alias string) string {
	return d.resolver.CachedHead(ctx, alias)
}

// DetectAndStore scans a file for cross-project imports, resolves entities
// against sibling stores, and persists the results. This method satisfies the
// watcher.CrossProjectTracker interface. Errors are logged and skipped.
func (d *DeterministicDetector) DetectAndStore(ctx context.Context, filePath string, localStore *store.Store) {
	deps := d.DetectDeps(ctx, filePath, localStore)
	if len(deps) > 0 {
		d.StoreDeps(deps, localStore)
	}
}

// StoreDeps persists detected dependencies via the local store.
// Errors are logged and skipped (fail-open). Existing deps for the same
// file are cleaned up before inserting new ones.
func (d *DeterministicDetector) StoreDeps(deps []store.CrossProjectDep, localStore *store.Store) {
	if localStore == nil || len(deps) == 0 {
		return
	}

	// Group by FromEntity to efficiently clean up stale deps.
	byFrom := make(map[string][]store.CrossProjectDep)
	for _, dep := range deps {
		byFrom[dep.FromEntity] = append(byFrom[dep.FromEntity], dep)
	}

	for from, fileDeps := range byFrom {
		// Delete old deps for this file — they'll be replaced by current ones.
		if err := localStore.DeleteCrossProjectDeps(from); err != nil {
			log.Printf("federation/tracker: delete old deps for %s: %v", from, err)
		}
		for _, dep := range fileDeps {
			if err := localStore.UpsertCrossProjectDep(dep); err != nil {
				log.Printf("federation/tracker: upsert dep %s → %s.%s: %v",
					dep.FromEntity, dep.ToProject, dep.ToEntity, err)
			}
		}
	}
}
