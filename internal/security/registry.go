// Package security — registry.go: known-package registry for slopsquatting detection.
//
// Sprint 26.11: AI agents hallucinate package names (~20% of the time per the DryRun
// study). The PackageRegistry holds a per-language set of known package names sourced
// from the top packages of each ecosystem. When the security engine encounters an import
// not in the registry, it fires a HIGH violation with a fuzzy "did you mean" suggestion.
//
// Architecture:
//   - PackageRegistry is loaded once at startup via LoadBuiltinRegistry or constructed
//     via NewPackageRegistry + AddPackages for tests.
//   - Engine.WithRegistry attaches a registry to an existing Engine (immutable pattern).
//   - Engine.CheckImports is the per-file entry point — separate from CheckFile because
//     it uses the registry as its data source rather than the SecurityPattern specs.
//   - IsKnown is the hot path: O(1) hash-set lookup per import.
//   - Suggest is the cold path (only called on unknown imports): O(N) edit-distance scan
//     filtered by length and first-character to keep it fast in practice.
//
// Thread-safety:
//   PackageRegistry is immutable after construction and safe for concurrent use.
//
// Registry refresh:
//
//go:generate scripts/refresh-package-registry.sh
//
// The embedded files in builtin/*.txt contain a curated seed of well-known packages.
// Run the generate script to download the full top-50K lists from each registry API.
package security

import (
	"bufio"
	"bytes"
	"embed"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// builtinRegistryFiles contains the built-in package registry text files embedded
// at compile time. Files are located in the builtin/ subdirectory.
//
//go:embed builtin/*.txt
var builtinRegistryFiles embed.FS

// registryLang is the internal key for a language ecosystem in the registry.
// Separate from the security pattern Language field to avoid coupling.
type registryLang string

const (
	regLangNPM    registryLang = "npm"
	regLangPyPI   registryLang = "pypi"
	regLangCrates registryLang = "crates"
	regLangGo     registryLang = "go"
)

// PackageRegistry is an in-memory store of known package names per language ecosystem.
// Loaded from embedded text files at startup. Used by Engine.CheckImports to detect
// unknown or hallucinated package imports.
//
// Zero value is safe: all methods return "known" / no suggestion (safe fallback).
type PackageRegistry struct {
	known    map[registryLang]map[string]struct{} // normalized name → present
	names    map[registryLang][]string            // all names, for fuzzy matching
	loadedAt time.Time
	total    int // total package count across all ecosystems
}

// NewPackageRegistry returns an empty registry. Use AddPackages to populate it
// (useful in tests). For production use, prefer LoadBuiltinRegistry.
func NewPackageRegistry() *PackageRegistry {
	return &PackageRegistry{
		known:    make(map[registryLang]map[string]struct{}),
		names:    make(map[registryLang][]string),
		loadedAt: time.Now(),
	}
}

// LoadBuiltinRegistry loads all embedded *.txt registry files and returns a
// populated PackageRegistry. Never returns nil. Returns an error only if an
// embedded file cannot be read — this should never happen in a correct binary.
func LoadBuiltinRegistry() (*PackageRegistry, error) {
	r := NewPackageRegistry()
	r.loadedAt = time.Now()

	err := fs.WalkDir(builtinRegistryFiles, "builtin", func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".txt") {
			return nil
		}
		lang := registryLangFromFilename(path)
		if lang == "" {
			return nil // unrecognised filename, skip silently
		}
		data, err := builtinRegistryFiles.ReadFile(path)
		if err != nil {
			return err
		}
		return r.addPackages(lang, data)
	})
	if err != nil {
		return r, err
	}
	return r, nil
}

// registryLangFromFilename maps a builtin txt filename to its registryLang.
func registryLangFromFilename(filename string) registryLang {
	base := strings.TrimSuffix(filepath.Base(filename), ".txt")
	switch base {
	case "npm-packages":
		return regLangNPM
	case "pypi-packages":
		return regLangPyPI
	case "crates-packages":
		return regLangCrates
	case "go-modules":
		return regLangGo
	}
	return ""
}

// AddPackages adds the newline-separated package names in data to the registry
// under lang. Lines starting with '#' and blank lines are ignored.
// Useful for tests and the registry-refresh command.
func (r *PackageRegistry) AddPackages(lang registryLang, data []byte) error {
	return r.addPackages(lang, data)
}

func (r *PackageRegistry) addPackages(lang registryLang, data []byte) error {
	if r.known[lang] == nil {
		r.known[lang] = make(map[string]struct{})
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		normalized := normalizePackageName(lang, line)
		if normalized == "" {
			continue
		}
		if _, exists := r.known[lang][normalized]; !exists {
			r.known[lang][normalized] = struct{}{}
			r.names[lang] = append(r.names[lang], normalized)
			r.total++
		}
	}
	return sc.Err()
}

// IsKnown reports whether importPath is a known package for the given language.
//
// The following are always treated as known, regardless of registry contents:
//   - Standard library imports (language-specific detection)
//   - Local/relative imports (starting with "." or "/")
//   - Languages not supported by the registry (Java, Ruby, PHP, etc.)
//
// If the registry is nil, IsKnown always returns true (safe fallback: no false positives).
func (r *PackageRegistry) IsKnown(lang, importPath string) bool {
	if r == nil {
		return true
	}
	// Always skip local/relative imports.
	if isLocalImport(importPath) {
		return true
	}
	// Always skip standard library imports.
	if isStdlibImport(lang, importPath) {
		return true
	}

	lk, normalized := registryKey(lang, importPath)
	if lk == "" {
		// Unsupported language: assume known to avoid false positives.
		return true
	}
	pkgs, ok := r.known[lk]
	if !ok || len(pkgs) == 0 {
		// No registry data for this language: assume known.
		return true
	}

	// Direct normalized match.
	if _, found := pkgs[normalized]; found {
		return true
	}

	// Go-specific: prefix match — import github.com/foo/bar/v5/subpkg is known
	// if registry has github.com/foo/bar/v5 (or github.com/foo/bar).
	if lk == regLangGo {
		return goImportKnownByPrefix(pkgs, normalized)
	}

	return false
}

// Suggest returns the closest known package name to importPath, or "" if no
// reasonable suggestion exists (edit distance > threshold). Only called on
// imports already determined to be unknown by IsKnown.
//
// Threshold: min(3, max(2, len(normalized)/4+1)).
// Performance: O(N) over the registry for the given language, but filtered by
// length proximity (±threshold) and first-character match to reduce comparisons.
func (r *PackageRegistry) Suggest(lang, importPath string) string {
	if r == nil {
		return ""
	}
	lk, normalized := registryKey(lang, importPath)
	if lk == "" || normalized == "" {
		return ""
	}
	candidates, ok := r.names[lk]
	if !ok || len(candidates) == 0 {
		return ""
	}

	maxDist := suggestMaxDist(normalized)

	best := ""
	bestDist := maxDist + 1 // start just above threshold so the first match replaces it

	// For Go imports, compare only the module-name component to avoid cross-host
	// suggestions (e.g., don't suggest github.com/foo/bar for github.com/foo/baz).
	compareKey := normalized
	if lk == regLangGo {
		// For Go modules, extract the base name from both the target AND each candidate.
		// Comparing "chi" against "github.com/go-chi/chi/v5" would be misleading due to
		// the path length difference; base-to-base comparison gives meaningful suggestions.
		compareKey = goModuleBase(normalized)
		for _, cand := range candidates {
			candBase := goModuleBase(cand)
			if absDiff(len(candBase), len(compareKey)) > maxDist {
				continue
			}
			if len(candBase) > 0 && len(compareKey) > 0 && candBase[0] != compareKey[0] {
				continue
			}
			d := editDistance(compareKey, candBase)
			if d < bestDist {
				best = cand // return the full module path, not just the base
				bestDist = d
			}
		}
		return best
	}

	for _, cand := range candidates {
		// Fast filter: length must be within maxDist of the target.
		if absDiff(len(cand), len(compareKey)) > maxDist {
			continue
		}
		// Fast filter: first byte must match (package names rarely differ on first char).
		if len(cand) > 0 && len(compareKey) > 0 && cand[0] != compareKey[0] {
			continue
		}
		d := editDistance(compareKey, cand)
		if d < bestDist {
			best = cand
			bestDist = d
		}
	}
	return best
}

// Size returns the total number of known package entries across all ecosystems.
func (r *PackageRegistry) Size() int {
	if r == nil {
		return 0
	}
	return r.total
}

// LoadedAt returns when this registry was constructed or last refreshed.
func (r *PackageRegistry) LoadedAt() time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.loadedAt
}

// ──────────────────────────────────────────────────────────────────────────────
// Language-specific normalisation and detection
// ──────────────────────────────────────────────────────────────────────────────

// registryKey maps an import path for the given language to its (registryLang, normalizedName).
// Returns ("", "") for unsupported languages or empty paths.
func registryKey(lang, importPath string) (registryLang, string) {
	if importPath == "" {
		return "", ""
	}
	switch strings.ToLower(lang) {
	case "go":
		return regLangGo, normalizePackageName(regLangGo, importPath)
	case "typescript", "javascript":
		return regLangNPM, normalizePackageName(regLangNPM, importPath)
	case "python":
		return regLangPyPI, normalizePackageName(regLangPyPI, importPath)
	case "rust":
		return regLangCrates, normalizePackageName(regLangCrates, importPath)
	default:
		// Java, Ruby, PHP, C#, etc. — not yet supported.
		return "", ""
	}
}

// normalizePackageName normalises a package name for registry comparison.
// Normalization is ecosystem-specific:
//   - PyPI (PEP 503): lowercase, replace _ and . with -
//   - npm: lowercase only
//   - crates.io: lowercase, replace - with _ (Rust uses underscore in crate names)
//   - Go: preserve original module path (go module paths are case-sensitive)
func normalizePackageName(lang registryLang, name string) string {
	if name == "" {
		return ""
	}
	switch lang {
	case regLangPyPI:
		// PEP 503 normalization: lowercase, replace runs of [-_. ] with a single -.
		// https://packaging.python.org/en/latest/specifications/name-normalization/
		name = strings.ToLower(name)
		return pypiNormalize(name)
	case regLangNPM:
		return strings.ToLower(name)
	case regLangCrates:
		// Rust crate names: lowercase, normalise hyphens and underscores as equivalent.
		name = strings.ToLower(name)
		name = strings.ReplaceAll(name, "-", "_")
		return name
	case regLangGo:
		// Go module paths are case-sensitive and canonical. Preserve as-is.
		return name
	default:
		return strings.ToLower(name)
	}
}

// isLocalImport reports whether importPath is a local/relative import that never
// resolves to an external package.
func isLocalImport(importPath string) bool {
	return strings.HasPrefix(importPath, ".") ||
		strings.HasPrefix(importPath, "/")
}

// isStdlibImport reports whether importPath is a standard library import for lang.
func isStdlibImport(lang, importPath string) bool {
	switch strings.ToLower(lang) {
	case "go":
		return isGoStdlib(importPath)
	case "python":
		return isPythonStdlib(importPath)
	case "rust":
		return isRustStdlib(importPath)
	case "typescript", "javascript":
		return isNodeBuiltin(importPath)
	default:
		return false
	}
}

// isGoStdlib reports whether an import path refers to the Go standard library.
// Rule: if the first path component contains no dot, it is stdlib.
// Examples: "fmt" → true, "net/http" → true, "github.com/foo/bar" → false.
func isGoStdlib(importPath string) bool {
	if importPath == "" {
		return false
	}
	first := importPath
	if i := strings.IndexByte(importPath, '/'); i >= 0 {
		first = importPath[:i]
	}
	return !strings.ContainsRune(first, '.')
}

// isRustStdlib reports whether a Rust import path refers to std, core, or alloc.
func isRustStdlib(importPath string) bool {
	for _, prefix := range []string{"std", "core", "alloc", "proc_macro"} {
		if importPath == prefix ||
			strings.HasPrefix(importPath, prefix+"::") ||
			strings.HasPrefix(importPath, prefix+"/") {
			return true
		}
	}
	return false
}

// isNodeBuiltin reports whether a package name is a Node.js built-in module.
func isNodeBuiltin(importPath string) bool {
	// node: prefix (modern Node.js)
	if strings.HasPrefix(importPath, "node:") {
		return true
	}
	_, ok := nodeBuiltins[importPath]
	return ok
}

// isPythonStdlib reports whether an import name is from the Python standard library.
func isPythonStdlib(importPath string) bool {
	// Handle dotted paths like "os.path" — check the root module "os".
	root := importPath
	if i := strings.IndexByte(importPath, '.'); i >= 0 {
		root = importPath[:i]
	}
	_, ok := pythonStdlib[root]
	return ok
}

// goImportKnownByPrefix reports whether any key in pkgs is a prefix of goImport.
// This handles sub-package imports: github.com/go-chi/chi/v5/middleware is known
// if the registry contains github.com/go-chi/chi/v5 (or github.com/go-chi/chi).
//
// Important: version suffixes (v2, v3, …) are NOT treated as sub-packages.
// github.com/foo/bar/v2 is a distinct module, not a sub-package of github.com/foo/bar.
// A prefix match is only accepted when the remaining component after the match is
// not a bare Go major-version suffix.
func goImportKnownByPrefix(pkgs map[string]struct{}, goImport string) bool {
	parts := strings.Split(goImport, "/")
	// We need at least host + org + repo (3 components) to avoid matching "github.com" alone.
	for n := len(parts); n >= 3; n-- {
		prefix := strings.Join(parts[:n], "/")
		if _, found := pkgs[prefix]; found {
			if n == len(parts) {
				return true // exact match
			}
			// The next component after the matched prefix must not be a major-version
			// suffix (v2, v3, …). If it is, this is a separate module, not a sub-package.
			next := parts[n]
			if isGoVersionSuffix(next) {
				continue
			}
			return true
		}
	}
	return false
}

// isGoVersionSuffix reports whether s is a Go major-version path component like
// "v2", "v3", "v10", etc. These denote distinct modules, not sub-packages.
func isGoVersionSuffix(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// goModuleBase returns the last non-version path component of a Go module path.
// Used for fuzzy suggestion matching to avoid cross-host suggestions.
// E.g., "github.com/go-chi/chi/v5" → "chi".
func goModuleBase(modulePath string) string {
	parts := strings.Split(modulePath, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		if p == "" {
			continue
		}
		// Skip Go version suffixes (v2, v3, ...).
		if len(p) > 1 && p[0] == 'v' {
			isVer := true
			for _, c := range p[1:] {
				if c < '0' || c > '9' {
					isVer = false
					break
				}
			}
			if isVer {
				continue
			}
		}
		return p
	}
	return modulePath
}

// pypiNormalize applies PEP 503 name normalization: replaces consecutive runs of
// [-_.] with a single '-'. Called after lowercasing.
// See: https://packaging.python.org/en/latest/specifications/name-normalization/
func pypiNormalize(name string) string {
	b := make([]byte, 0, len(name))
	prevWasSep := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '-' || c == '_' || c == '.' {
			if !prevWasSep {
				b = append(b, '-')
				prevWasSep = true
			}
			// else: collapse consecutive separators
		} else {
			b = append(b, c)
			prevWasSep = false
		}
	}
	// Trim trailing separator (shouldn't happen for valid names, but defensive).
	result := string(b)
	return strings.TrimRight(result, "-")
}

// suggestMaxDist computes the maximum edit distance to consider for suggestions.
// Threshold: min(3, max(2, len/4+1)).
func suggestMaxDist(s string) int {
	d := len(s)/4 + 1
	if d < 2 {
		d = 2
	}
	if d > 3 {
		d = 3
	}
	return d
}

// ──────────────────────────────────────────────────────────────────────────────
// Edit distance (Levenshtein)
// ──────────────────────────────────────────────────────────────────────────────

// editDistance computes the Levenshtein edit distance between two strings.
// Uses two-row DP for O(min(m,n)) space. Package names are short ASCII strings,
// so this is fast in practice.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	m, n := len(a), len(b)
	if m == 0 {
		return n
	}
	if n == 0 {
		return m
	}
	// Ensure m <= n for space optimisation (swap so shorter string is rows).
	if m > n {
		a, b = b, a
		m, n = n, m
	}
	prev := make([]int, m+1)
	curr := make([]int, m+1)
	for i := range prev {
		prev[i] = i
	}
	for j := 1; j <= n; j++ {
		curr[0] = j
		for i := 1; i <= m; i++ {
			if a[i-1] == b[j-1] {
				curr[i] = prev[i-1]
			} else {
				curr[i] = 1 + minInt3(prev[i], curr[i-1], prev[i-1])
			}
		}
		prev, curr = curr, prev
	}
	return prev[m]
}

func minInt3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// ──────────────────────────────────────────────────────────────────────────────
// Node.js built-in module list
// ──────────────────────────────────────────────────────────────────────────────

// nodeBuiltins is the set of Node.js core module names (without "node:" prefix).
// Source: Node.js 20 LTS (https://nodejs.org/api/).
var nodeBuiltins = map[string]struct{}{
	"assert": {}, "async_hooks": {}, "buffer": {}, "child_process": {},
	"cluster": {}, "console": {}, "constants": {}, "crypto": {},
	"dgram": {}, "diagnostics_channel": {}, "dns": {}, "domain": {},
	"events": {}, "fs": {}, "http": {}, "http2": {}, "https": {},
	"inspector": {}, "module": {}, "net": {}, "os": {}, "path": {},
	"perf_hooks": {}, "process": {}, "punycode": {}, "querystring": {},
	"readline": {}, "repl": {}, "stream": {}, "string_decoder": {},
	"sys": {}, "timers": {}, "tls": {}, "trace_events": {}, "tty": {},
	"url": {}, "util": {}, "v8": {}, "vm": {}, "wasi": {},
	"worker_threads": {}, "zlib": {},
}

// ──────────────────────────────────────────────────────────────────────────────
// Python standard library module list
// ──────────────────────────────────────────────────────────────────────────────

// pythonStdlib is the set of Python 3 standard library top-level module names.
// Source: Python 3.12 module index (https://docs.python.org/3/py-modindex.html).
var pythonStdlib = map[string]struct{}{
	"__future__": {}, "__main__": {}, "_thread": {}, "abc": {}, "aifc": {},
	"argparse": {}, "array": {}, "ast": {}, "asynchat": {}, "asyncio": {},
	"asyncore": {}, "atexit": {}, "audioop": {}, "base64": {}, "bdb": {},
	"binascii": {}, "binhex": {}, "bisect": {}, "builtins": {}, "bz2": {},
	"calendar": {}, "cgi": {}, "cgitb": {}, "chunk": {}, "cmath": {},
	"cmd": {}, "code": {}, "codecs": {}, "codeop": {}, "colorsys": {},
	"collections": {}, "compileall": {}, "concurrent": {}, "configparser": {}, "contextlib": {},
	"contextvars": {}, "copy": {}, "copyreg": {}, "cProfile": {}, "csv": {},
	"ctypes": {}, "curses": {}, "dataclasses": {}, "datetime": {}, "dbm": {},
	"decimal": {}, "difflib": {}, "dis": {}, "doctest": {}, "email": {},
	"encodings": {}, "enum": {}, "errno": {}, "faulthandler": {}, "fcntl": {},
	"filecmp": {}, "fileinput": {}, "fnmatch": {}, "fractions": {}, "ftplib": {},
	"functools": {}, "gc": {}, "getopt": {}, "getpass": {}, "gettext": {},
	"glob": {}, "grp": {}, "gzip": {}, "hashlib": {}, "heapq": {}, "hmac": {},
	"html": {}, "http": {}, "idlelib": {}, "imaplib": {}, "imghdr": {},
	"imp": {}, "importlib": {}, "inspect": {}, "io": {}, "ipaddress": {},
	"itertools": {}, "json": {}, "keyword": {}, "lib2to3": {}, "linecache": {},
	"locale": {}, "logging": {}, "lzma": {}, "mailbox": {}, "mailcap": {},
	"marshal": {}, "math": {}, "mimetypes": {}, "mmap": {}, "modulefinder": {},
	"multiprocessing": {}, "netrc": {}, "nis": {}, "nntplib": {}, "numbers": {},
	"operator": {}, "optparse": {}, "os": {}, "ossaudiodev": {}, "pathlib": {},
	"pdb": {}, "pickle": {}, "pickletools": {}, "pipes": {}, "pkgutil": {},
	"platform": {}, "plistlib": {}, "poplib": {}, "posix": {}, "posixpath": {},
	"pprint": {}, "profile": {}, "pstats": {}, "pty": {}, "pwd": {},
	"py_compile": {}, "pyclbr": {}, "pydoc": {}, "queue": {}, "quopri": {},
	"random": {}, "re": {}, "readline": {}, "reprlib": {}, "resource": {},
	"rlcompleter": {}, "runpy": {}, "sched": {}, "secrets": {}, "select": {},
	"selectors": {}, "shelve": {}, "shlex": {}, "shutil": {}, "signal": {},
	"site": {}, "smtpd": {}, "smtplib": {}, "sndhdr": {}, "socket": {},
	"socketserver": {}, "spwd": {}, "sqlite3": {}, "sre_compile": {},
	"sre_constants": {}, "sre_parse": {}, "ssl": {}, "stat": {}, "statistics": {},
	"string": {}, "stringprep": {}, "struct": {}, "subprocess": {}, "sunau": {},
	"symtable": {}, "sys": {}, "sysconfig": {}, "syslog": {}, "tabnanny": {},
	"tarfile": {}, "telnetlib": {}, "tempfile": {}, "termios": {}, "test": {},
	"textwrap": {}, "threading": {}, "time": {}, "timeit": {}, "tkinter": {},
	"token": {}, "tokenize": {}, "tomllib": {}, "trace": {}, "traceback": {},
	"tracemalloc": {}, "tty": {}, "turtle": {}, "turtledemo": {}, "types": {},
	"typing": {}, "unicodedata": {}, "unittest": {}, "urllib": {}, "uu": {},
	"uuid": {}, "venv": {}, "warnings": {}, "wave": {}, "weakref": {},
	"webbrowser": {}, "winreg": {}, "winsound": {}, "wsgiref": {}, "xdrlib": {},
	"xml": {}, "xmlrpc": {}, "zipapp": {}, "zipfile": {}, "zipimport": {},
	"zlib": {}, "zoneinfo": {},
}
