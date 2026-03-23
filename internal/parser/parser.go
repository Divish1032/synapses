// Package parser converts source files into graph nodes and edges using
// Tree-sitter for accurate, incremental, multi-language AST parsing.
package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/pulse"
)

// LanguageParser is implemented by each language-specific parser.
// Parse receives the raw source bytes and the file path; it appends
// discovered nodes and edges into the provided graph.
type LanguageParser interface {
	// Extensions returns the file extensions this parser handles (e.g. ".go").
	Extensions() []string
	// Parse extracts code entities from src and merges them into g.
	Parse(g *graph.Graph, filePath string, src []byte) error
}

// FilenameParser is an optional interface that parsers can implement
// if they handle files matched by base filename rather than extension.
// The Walker checks this interface when no extension-based match is found.
type FilenameParser interface {
	Filenames() []string
}

// FilenamePatternParser is an optional interface for parsers that handle
// files whose base name matches a prefix (e.g. "Dockerfile" handles
// "Dockerfile.staging", "Dockerfile.ci", etc.).
type FilenamePatternParser interface {
	FilenamePrefixes() []string
}

// TreeSitterLanguageProvider is an optional interface for parsers that use
// tree-sitter internally. Returning the language enables the watcher to
// pre-check files for parse errors before updating the graph, preventing
// corrupted ASTs from half-saved files during active editing.
type TreeSitterLanguageProvider interface {
	TSLanguageForFile(filePath string) *sitter.Language
}

// filenamePrefixEntry associates a filename prefix with the parser that handles it.
type filenamePrefixEntry struct {
	prefix string
	parser LanguageParser
}

// Walker orchestrates multi-language parsing over a directory tree.
type Walker struct {
	parsers               map[string]LanguageParser // extension → parser
	filenameParsers       map[string]LanguageParser // base filename → parser
	filenamePrefixParsers []filenamePrefixEntry      // base filename prefix → parser

	// BeginFunc is called once, on the main goroutine, after the filesystem
	// scan completes and the total file count is known, but before any parsing
	// begins. Use this to initialise progress state with the real total.
	// Set to nil to disable.
	BeginFunc func(total int)

	// ProgressFunc is called from worker goroutines after each file is parsed.
	// done is the number of files completed so far; total is the same value
	// passed to BeginFunc. byExt maps file extension (e.g. ".go") to the
	// number of files of that type parsed so far (snapshot copy — safe to read
	// without locking). Calls are throttled to at most 1 per 200ms via an
	// atomic CAS; the final call (done==total) always fires.
	// The callback is called from multiple goroutines; implementations must be
	// goroutine-safe. Set to nil to disable progress reporting.
	ProgressFunc func(done, total int, byExt map[string]int)

	// PulseClient, when non-nil, receives a ParseEvent after each file is
	// parsed during WalkDir. Fire-and-forget — never blocks parsing. (P2-2)
	PulseClient *pulse.Client
	// ProjectID is included in ParseEvent so Pulse can scope data per project.
	ProjectID     string
	mMATLABParser LanguageParser
	mObjCParser   LanguageParser
}

// NewWalker creates a Walker pre-loaded with all built-in language parsers.
// Registration order matters: generic (file-only) is registered first so that
// dedicated AST parsers registered afterward take precedence for their extensions.
func NewWalker() *Walker {
	w := &Walker{
		parsers:         make(map[string]LanguageParser),
		filenameParsers: make(map[string]LanguageParser),
	}
	w.Register(newGenericParser())    // file-level fallback for all other languages
	w.Register(NewGoParser())         // deep: .go
	w.Register(NewTypeScriptParser()) // deep: .ts .tsx
	w.Register(NewJavaScriptParser()) // deep: .js .jsx .mjs .cjs
	w.Register(NewPythonParser())     // deep: .py .pyi
	// JVM
	w.Register(NewJavaParser())   // deep: .java
	w.Register(NewKotlinParser()) // deep: .kt .kts
	w.Register(NewScalaParser())  // deep: .scala
	w.Register(NewGroovyParser()) // deep: .groovy .gradle
	// Systems
	w.Register(NewRustParser())   // deep: .rs
	w.Register(NewCParser())      // deep: .c .h .ino
	w.Register(NewCppParser())    // deep: .cpp .cc .cxx .hpp .hh .hxx .mm
	w.Register(NewCSharpParser()) // deep: .cs
	w.Register(NewSwiftParser())  // deep: .swift
	// Scripting
	w.Register(NewRubyParser())      // deep: .rb .rbi (Sorbet type stubs)
	w.Register(NewRBSParser())       // deep: .rbs (Ruby type signatures)
	w.Register(NewPHPParser())       // deep: .php
	w.Register(NewLuaParser())       // deep: .lua
	w.Register(NewBashParser())      // deep: .sh .bash
	w.Register(NewPowerShellParser()) // deep: .ps1 .psm1 .psd1
	// Functional
	w.Register(NewElixirParser())  // deep: .ex .exs
	w.Register(NewOCamlParser())   // deep: .ml .mli
	w.Register(NewElmParser())     // deep: .elm
	w.Register(NewHaskellParser()) // deep: .hs .lhs
	w.Register(NewErlangParser())  // deep: .erl .hrl
	w.Register(NewFSharpParser())  // deep: .fs .fsi .fsx
	w.Register(NewClojureParser()) // deep: .clj .cljs .cljc .edn
	// Database
	w.Register(NewSQLParser()) // deep: .sql
	// Frontend
	w.Register(NewCSSParser())    // deep: .css
	w.Register(NewSCSSParser())   // deep: .scss .sass
	w.Register(NewSvelteParser()) // deep: .svelte
	w.Register(NewRParser())      // deep: .r .R
	w.Register(NewDartParser())   // deep: .dart
	// Infrastructure
	w.Register(NewHCLParser())        // deep: .tf .tfvars .hcl
	w.Register(NewDockerfileParser()) // deep: .dockerfile
	w.Register(NewMakefileParser())  // deep: .mk, Makefile
	w.Register(NewCMakeParser())     // deep: .cmake, CMakeLists.txt
	w.Register(NewNixParser())       // deep: .nix
	w.Register(NewStarlarkParser())  // deep: .bzl .star, BUILD
	w.Register(NewCUEParser())        // deep: .cue
	w.Register(NewYAMLParser())       // deep: .yaml .yml (overrides generic)
	w.Register(NewTOMLParser())       // deep: .toml
	w.Register(NewXMLParser())        // deep: .xml (pom.xml, AndroidManifest.xml, Spring context)
	w.Register(NewBicepParser())      // deep: .bicep (Azure IaC)
	// Schema / API
	w.Register(NewProtobufParser()) // deep: .proto
	w.Register(NewGraphQLParser())  // deep: .graphql .gql
	w.Register(NewThriftParser())   // deep: .thrift (Apache Thrift IDL)
	// Smart Contracts
	w.Register(NewSolidityParser()) // deep: .sol
	// Configuration scripting
	w.Register(NewJsonnetParser()) // deep: .jsonnet .libsonnet
	// Scripting
	w.Register(NewPerlParser())   // deep: .pl .pm .t
	// Scientific computing
	w.Register(NewMATLABParser()) // deep: .m (MATLAB/Octave)
	// Documentation
	w.Register(NewMarkdownParser()) // deep: .md .markdown .mdx (overrides generic)
	// Frontend
	w.Register(NewVueParser()) // deep: .vue (SFC, delegates script to JS/TS)
	// Data formats
	w.Register(NewJSONParser()) // deep: .json (scoped to package.json, tsconfig.json, *.schema.json)
	// Systems
	w.Register(NewZigParser())  // deep: .zig
	w.Register(NewObjCParser()) // deep: .m (Objective-C)
	// Scientific
	w.Register(NewJuliaParser()) // deep: .jl

	// Populate disambiguation fields for .m extension (MATLAB vs Objective-C).
	// MATLAB was registered first; ObjC registered last wins in w.parsers[".m"].
	// resolveParser uses these fields to disambiguate at parse time.
	w.mObjCParser = w.parsers[".m"]   // ObjC (last registered, current winner)
	w.mMATLABParser = NewMATLABParser() // MATLAB (re-instantiate since overwritten)
	return w
}

// Register adds a language parser. If two parsers claim the same extension
// the last one registered wins.
func (w *Walker) Register(p LanguageParser) {
	for _, ext := range p.Extensions() {
		w.parsers[ext] = p
	}
	if fp, ok := p.(FilenameParser); ok {
		for _, name := range fp.Filenames() {
			w.filenameParsers[name] = p
		}
	}
	if fpp, ok := p.(FilenamePatternParser); ok {
		for _, prefix := range fpp.FilenamePrefixes() {
			w.filenamePrefixParsers = append(w.filenamePrefixParsers, filenamePrefixEntry{prefix: prefix, parser: p})
		}
	}
}

// RegisterPlugin adds an external parser plugin for the given file extensions.
// command is split on whitespace into binary + args (e.g. "node parsers/graphql.js").
// If two parsers claim the same extension the last one registered wins, so
// plugins registered here override built-in parsers for their extensions.
//
// If checker is non-nil, the command is validated against the per-machine
// allowlist before registration. Unapproved plugins are skipped with a
// stderr warning.
func (w *Walker) RegisterPlugin(extensions []string, command string, checker *PluginChecker) {
	if checker != nil {
		if err := checker.IsAllowed(command); err != nil {
			logutil.Warn("skipping plugin for %v: %v\n", extensions, err)
			return
		}
	}
	w.Register(newPluginParser(extensions, command))
}

// WalkDir recursively parses all supported files under root and populates g.
// It skips hidden directories (prefixed with ".") and common non-source dirs.
// Returns a map of absolute file path → modification time (UnixNano) for every
// file that was successfully (or non-fatally) parsed. This map can be persisted
// to the store and used by IncrementalReindex on subsequent runs.
//
// Parsing is parallelised across min(runtime.NumCPU(), 8) workers. All language
// parsers create a fresh sitter.Parser per call so there is no shared mutable
// state between goroutines. Graph mutations are protected by Graph's own mutex.
func (w *Walker) WalkDir(g *graph.Graph, root string) (map[string]int64, error) {
	type fileJob struct {
		path   string
		parser LanguageParser
		mtime  int64
	}

	// Phase 1 — sequential directory scan: collect parseable files + mtimes.
	// filepath.WalkDir itself is not parallelisable (tree traversal must be
	// ordered), but it's fast — just stat calls and map lookups.
	var jobs []fileJob
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}

		// Security: Prevent Directory Traversal via Symlinks.
		// Resolve the symlink and verify containment using inode comparison
		// (os.SameFile), which is immune to case/Unicode/TOCTOU races.
		if d.Type()&os.ModeSymlink != 0 {
			resolved, symErr := filepath.EvalSymlinks(path)
			if symErr != nil {
				return nil // Skip dangling or invalid symlinks
			}
			if !isPathContainedIn(resolved, root) {
				logutil.Warn("synapses/security: skipped symlink resolving outside repo root: %s -> %s\n", path, resolved)
				return nil
			}
		}

		ext := strings.ToLower(filepath.Ext(path))
		p, ok := w.resolveParser(path, ext)
		if !ok {
			return nil
		}

		mtime := info.ModTime().UnixNano()
		jobs = append(jobs, fileJob{path: path, parser: p, mtime: mtime})
		return nil
	}); err != nil {
		return nil, err
	}

	if len(jobs) == 0 {
		return make(map[string]int64), nil
	}

	// Phase 2 — parallel read + parse.
	// Worker count is bounded to 8 to avoid opening too many files at once on
	// systems with low file-descriptor limits.
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	mtimes := make(map[string]int64, len(jobs))
	var mtimesMu sync.Mutex

	// R1: collect route registrations during parallel parse; inject HANDLES
	// edges serially after wg.Wait() so all handler nodes are present.
	type parsedFile struct {
		path string
		src  []byte
	}
	var heuristicFiles []parsedFile
	var heuristicMu sync.Mutex

	// Phase 1 complete: total is now known. Notify before goroutines start.
	total := len(jobs)
	if w.BeginFunc != nil {
		w.BeginFunc(total)
	}
	var doneCount atomic.Int64   // files completed (read+parsed)
	var byExtMu sync.Mutex
	byExt := make(map[string]int, 16)
	var lastEmitNs atomic.Int64  // UnixNano of last ProgressFunc call (throttle)

	emitProgress := func(final bool) {
		if w.ProgressFunc == nil {
			return
		}
		now := time.Now().UnixNano()
		if !final {
			last := lastEmitNs.Load()
			if now-last < int64(200*time.Millisecond) {
				return
			}
			if !lastEmitNs.CompareAndSwap(last, now) {
				return // another goroutine won the CAS — skip this tick
			}
		}
		done := int(doneCount.Load())
		byExtMu.Lock()
		snapshot := make(map[string]int, len(byExt))
		for k, v := range byExt {
			snapshot[k] = v
		}
		byExtMu.Unlock()
		w.ProgressFunc(done, total, snapshot)
	}

	pc := w.PulseClient   // capture to avoid data race on walker field
	projID := w.ProjectID

	for _, job := range jobs {
		job := job // capture loop variable
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			parseStart := time.Now()
			src, err := os.ReadFile(job.path)
			errType := ""
			var nodesProduced int
			if err != nil {
				logutil.Error("synapses: read %s: %v\n", job.path, err)
				errType = "read_error"
			} else if parseErr := job.parser.Parse(g, job.path, src); parseErr != nil {
				logutil.Error("synapses: parse %s: %v\n", job.path, parseErr)
				errType = "parse_error"
			} else {
				// R28: stamp provenance on all nodes produced by this file.
				ApplyProvenance(g, job.path, src)
				// R1: collect for heuristic pass.
				heuristicMu.Lock()
				heuristicFiles = append(heuristicFiles, parsedFile{job.path, src})
				heuristicMu.Unlock()
				nodesProduced = len(g.NodesForFile(job.path))
			}
			parseElapsed := time.Since(parseStart).Milliseconds()

			// P2-2: emit ParseEvent. Enqueue is mutex+append (O(1)) — direct call,
			// no goroutine needed. The outer goroutine is already off the parse path.
			if pc != nil {
				lang := strings.TrimPrefix(strings.ToLower(filepath.Ext(job.path)), ".")
				pc.RecordParseEvent(pulse.ParseEvent{
					File:          job.path,
					Language:      lang,
					DurationMs:    parseElapsed,
					NodesProduced: nodesProduced,
					ErrorType:     errType,
					ProjectID:     projID,
				})
			}

			if job.mtime != 0 {
				mtimesMu.Lock()
				mtimes[job.path] = job.mtime
				mtimesMu.Unlock()
			}

			// Progress: increment counter, update byExt, maybe emit tick.
			if w.ProgressFunc != nil {
				ext := strings.ToLower(filepath.Ext(job.path))
				byExtMu.Lock()
				byExt[ext]++
				byExtMu.Unlock()
				doneCount.Add(1)
				emitProgress(false)
			}
		}()
	}

	wg.Wait()

	// Emit final progress tick (done == total).
	emitProgress(true)

	// R1: serial heuristic pass — all AST nodes are now present in the graph.
	for _, pf := range heuristicFiles {
		ApplyHeuristics(g, pf.path, pf.src)
	}

	return mtimes, nil
}

// IncrementalReindex re-parses only files whose modification time has changed
// since the last full index. g must be a fully-populated graph (e.g. loaded
// from SQLite). known maps absolute path → UnixNano mtime from the last index.
//
// Returns: fresh mtime map (all current parseable files), # changed/new files,
// # removed files (deleted from disk since last index).
//
// Note: CALLS edges from unchanged files that point INTO re-parsed files may be
// lost if the re-parsed file's node IDs changed. Run a full reindex if perfect
// edge fidelity is required.
func (w *Walker) IncrementalReindex(g *graph.Graph, root string, known map[string]int64) (fresh map[string]int64, changed, removed int, err error) {
	fresh = make(map[string]int64, len(known))

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		p, ok := w.resolveParser(path, ext)
		if !ok {
			return nil // unsupported extension or filename
		}

		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}

		// Security: Prevent Directory Traversal via Symlinks.
		// Resolve the symlink and verify containment using inode comparison
		// (os.SameFile), which is immune to case/Unicode/TOCTOU races.
		if d.Type()&os.ModeSymlink != 0 {
			resolved, symErr := filepath.EvalSymlinks(path)
			if symErr != nil {
				return nil // Skip dangling or invalid symlinks
			}
			if !isPathContainedIn(resolved, root) {
				logutil.Warn("synapses/security: skipped symlink resolving outside repo root: %s -> %s\n", path, resolved)
				return nil
			}
		}

		mtime := info.ModTime().UnixNano()

		if stored, hit := known[path]; hit && stored == mtime {
			fresh[path] = mtime // unchanged: carry forward
			return nil
		}

		// Changed or new file: remove stale data and re-parse.
		g.RemoveFile(path)
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			logutil.Error("synapses: read %s: %v\n", path, readErr)
			return nil
		}
		if parseErr := p.Parse(g, path, src); parseErr != nil {
			logutil.Error("synapses: parse %s: %v\n", path, parseErr)
		} else {
			// R28: stamp provenance on all nodes produced by this file.
			ApplyProvenance(g, path, src)
			// R1: re-inject HANDLES edges for the changed file.
			ApplyHeuristics(g, path, src)
		}
		fresh[path] = mtime
		changed++
		return nil
	})
	if err != nil {
		return nil, 0, 0, err
	}

	// Detect files that have been deleted since the last full index.
	for path := range known {
		if _, ok := fresh[path]; !ok {
			g.RemoveFile(path)
			removed++
		}
	}
	return fresh, changed, removed, nil
}

// ParseFile parses a single file and updates g. If the file extension is not
// supported it returns nil without error.
func (w *Walker) ParseFile(g *graph.Graph, path string) error {
	ext := strings.ToLower(filepath.Ext(path))
	p, ok := w.resolveParser(path, ext)
	if !ok {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := p.Parse(g, path, src); err != nil {
		return err
	}
	// R28: stamp provenance on all nodes produced by this file.
	ApplyProvenance(g, path, src)
	// R1: inject HANDLES edges for this file.
	ApplyHeuristics(g, path, src)
	return nil
}

// parserForPath returns the language parser for the given file path, or nil
// if no parser is registered for the file's extension or name.
func (w *Walker) parserForPath(path string) LanguageParser {
	ext := strings.ToLower(filepath.Ext(path))
	if p, ok := w.parsers[ext]; ok {
		return p
	}
	base := filepath.Base(path)
	if p, ok := w.filenameParsers[base]; ok {
		return p
	}
	for _, entry := range w.filenamePrefixParsers {
		if strings.HasPrefix(base, entry.prefix) {
			return entry.parser
		}
	}
	return nil
}

// HasParseErrors performs a lightweight tree-sitter parse of src and returns
// true if the resulting AST contains syntax errors. Used by the watcher to
// skip reparsing files that are mid-save (half-written), preventing corrupted
// AST data from replacing valid graph nodes.
//
// Returns false for parsers that don't use tree-sitter or for unknown extensions.
func (w *Walker) HasParseErrors(path string, src []byte) bool {
	p := w.parserForPath(path)
	if p == nil {
		return false
	}
	tsp, ok := p.(TreeSitterLanguageProvider)
	if !ok {
		return false
	}
	lang := tsp.TSLanguageForFile(path)
	if lang == nil {
		return false
	}
	parser := sitter.NewParser()
	parser.SetLanguage(lang)
	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, err := parser.ParseString(parseCtx, nil, src)
	if err != nil || tree == nil {
		return true
	}
	defer tree.Close()
	return tree.RootNode().HasError()
}

// shouldSkipDir returns true for directories that never contain useful source:
// dependency folders, build artefacts, generated output, and hidden dirs.
func shouldSkipDir(name string) bool {
	switch name {
	// --- Dependency managers ---
	case "node_modules", // Node.js
		"vendor",           // Go / PHP / Ruby
		"bower_components", // Bower (legacy JS)
		"jspm_packages",    // JSPM
		"Pods":             // iOS CocoaPods
		return true

	// --- Build / compiled output ---
	case "dist",
		"build",
		"out",
		"target", // Rust (Cargo), Java (Maven)
		"obj",    // C/C++
		"storybook-static",
		"coverage",
		".nyc_output":
		return true

	// --- Temporary / cache ---
	case "tmp",
		"temp",
		"cache":
		return true

	// --- Generated code ---
	case "generated",
		"gen",
		"__generated__",
		"__mocks__": // Jest mock directories
		return true

	// --- Third-party bundled sources ---
	case "third_party",
		"vendor_ruby",
		"testdata": // Go test fixtures rarely need indexing
		return true

	// --- IDE / tooling ---
	case ".git", ".svn", ".hg",
		".idea", ".vscode",
		"__pycache__":
		return true
	}

	// Skip all hidden directories (e.g. .next, .nuxt, .turbo, .cache, .gradle).
	return strings.HasPrefix(name, ".")
}
