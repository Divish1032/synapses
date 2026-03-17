package parser

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ClojureParser parses Clojure (.clj, .cljs, .cljc, .edn) source files.
// Used in data engineering, fintech backend systems, and JVM functional programming.
// Uses regex-based extraction (Clojure is not in go-tree-sitter).
//
// Extracts:
//   - (defn fn-name [args] body)          → NodeFunction (Exported: true)
//   - (defn- fn-name [args] body)         → NodeFunction (Exported: false)
//   - (defn ^:private fn-name [args])     → NodeFunction (Exported: false)
//   - (defmacro name [args] body)         → NodeFunction kind=macro
//   - (defmulti name dispatch-fn)         → NodeFunction kind=multimethod
//   - (defmethod name dispatch-val [...]) → NodeFunction kind=multimethod
//   - (defrecord Name [fields])           → NodeStruct   kind=record
//   - (defprotocol Name ...)              → NodeStruct   kind=protocol
//   - (deftype Name [fields])             → NodeStruct   kind=type
//   - (ns my.namespace (:require [...]))  → NodePackage  + EdgeImports
//   - (require '[clojure.string :as str]) → NodePackage  + EdgeImports
//   - (def var-name ...)                  → NodeVariable
//   - (def ^:private var-name ...)        → NodeVariable (Exported: false)
type ClojureParser struct{}

// NewClojureParser creates a ready-to-use ClojureParser.
func NewClojureParser() *ClojureParser { return &ClojureParser{} }

// Extensions returns the file extensions handled by this parser.
func (p *ClojureParser) Extensions() []string {
	return []string{".clj", ".cljs", ".cljc", ".edn"}
}

// Clojure regex patterns. All patterns match top-level forms (column 0 or
// preceded only by whitespace).
var (
	// (defn name ...) or (defn- name ...) or (defn ^:private name ...)
	// Group 1: "defn-" or "defn" (the exact keyword)
	// Group 2: optional ^:private or other metadata before name
	// Group 3: function name
	// Group 4: optional docstring (quoted string right after name + whitespace + args)
	// NOTE: We use a two-step approach: first match the declaration line, then
	// look for an inline docstring in the following content.
	reClojureDefn = regexp.MustCompile(
		`(?m)^\s*\(defn(-?)\s+(?:\^:private\s+)?(?:\^[^\s]+\s+)*([A-Za-z_\-\?!><=\+\*/\.][A-Za-z0-9_\-\?!><=\+\*/\.]*)`,
	)

	// (defmacro name [args] ...) — same shape as defn but keyword=defmacro
	reClojureDefmacro = regexp.MustCompile(
		`(?m)^\s*\(defmacro\s+(?:\^:private\s+)?(?:\^[^\s]+\s+)*([A-Za-z_\-\?!><=\+\*/\.][A-Za-z0-9_\-\?!><=\+\*/\.]*)`,
	)

	// defmulti: (defmulti name dispatch-fn) — multimethod declaration
	// Group 1: multimethod name
	reClojureDefmulti = regexp.MustCompile(`(?m)^\s*\((?:\w+/)?defmulti\s+([\w?!*+/.<>=-]+)`)

	// defmethod: (defmethod multifn dispatch-val [args] body)
	// dispatch-val can be a keyword (:circle), vector, nil, :default, or symbol
	// Group 1: multimethod name, Group 2: dispatch value (for unique naming)
	reClojureDefmethod = regexp.MustCompile(`(?m)^\s*\((?:\w+/)?defmethod\s+([\w?!*+/.<>=-]+)\s+(:{0,2}[\w/.\-?!]+|nil|:default|\[[^\]]*\]|[^\s\)]+)`)

	// defalias: (defalias new-name existing-name)
	// Group 1: alias name
	reClojureDefalias = regexp.MustCompile(`(?m)^\s*\((?:\w+/)?defalias\s+([\w?!*+/.<>=-]+)`)

	// (defrecord Name [field1 field2])
	// Optional Clojure metadata before the name: ^:keyword, ^{:key val}, ^TypeHint
	reClojureDefrecord = regexp.MustCompile(
		`(?m)^\s*\(defrecord\s+(?:\^(?:[^\s\{]+|\{[^\}]*\})\s+)*([A-Za-z][A-Za-z0-9_\-]*)`,
	)

	// (defprotocol Name ...)
	// Optional Clojure metadata before the name: ^:keyword, ^{:key val}, ^TypeHint
	// e.g. (defprotocol ^{:added "1.6"} StreamableResponseBody ...)
	reClojureDefprotocol = regexp.MustCompile(
		`(?m)^\s*\(defprotocol\s+(?:\^(?:[^\s\{]+|\{[^\}]*\})\s+)*([A-Za-z][A-Za-z0-9_\-]*)`,
	)

	// (deftype Name [fields] ...)
	// Optional Clojure metadata before the name: ^:keyword, ^{:key val}, ^TypeHint
	reClojureDeftype = regexp.MustCompile(
		`(?m)^\s*\(deftype\s+(?:\^(?:[^\s\{]+|\{[^\}]*\})\s+)*([A-Za-z][A-Za-z0-9_\-]*)`,
	)

	// (ns my.namespace ...) — namespace declaration
	// Group 1: namespace name (can contain dots and hyphens)
	reClojureNS = regexp.MustCompile(
		`(?m)^\s*\(ns\s+([A-Za-z][A-Za-z0-9_\-\.]*(?:\.[A-Za-z][A-Za-z0-9_\-\.]*)*)`,
	)

	// :require in ns form: (:require [ns-name ...] [ns-name2 ...])
	// Matches individual require vectors: [clojure.string :as str] or [clojure.string]
	reClojureRequireVec = regexp.MustCompile(
		`\[([A-Za-z][A-Za-z0-9_\-\.]*(?:\.[A-Za-z][A-Za-z0-9_\-\.]*)*)(?:\s+:[^\]]+)?\]`,
	)

	// Standalone (require '[clojure.string :as str]) form
	reClojureRequire = regexp.MustCompile(
		`(?m)^\s*\(require\s+'?\[([A-Za-z][A-Za-z0-9_\-\.]*(?:\.[A-Za-z][A-Za-z0-9_\-\.]*)*)`,
	)

	// (def name value) or (def ^:private name value) or (def ^{:added "1.4"} name value)
	// Group 1: optional metadata (^:private or ^{...})
	// Group 2: var name
	reClojureDef = regexp.MustCompile(
		`(?m)^\s*\(def\s+(?:(\^:private|\^[^\s\)]+|\^\{[^\}]*\})\s+)*([A-Za-z_\-\?!><=\+\*/\.][A-Za-z0-9_\-\?!><=\+\*/\.]*)`,
	)

	// Docstring: the first string literal after the name + args vector in defn/defmacro.
	// Captures an inline docstring: (defn name "docstring" [args] body)
	// This pattern matches starting at the beginning of the remainder of the line.
	reClojureDocstring = regexp.MustCompile(`^\s*"((?:[^"\\]|\\.)*)"`)

	// Re-frame event registration: (reg-event-db ::event-name handler)
	// Also handles: (rf/reg-event-db ...), (reg-event-fx ...), (reg-cofx ...)
	// Group 1: the fully-qualified event key (keyword like ::load-users or :app/load-users)
	reClojureRegEvent = regexp.MustCompile(
		`(?m)^\s*\((?:\w+/)?reg-event-(?:db|fx)\s+(:{1,2}[\w/.\-?!]+)`,
	)

	// Re-frame subscription registration: (reg-sub ::query-name query-fn)
	// Also handles: (rf/reg-sub ...), (reg-sub-raw ...)
	// Group 1: the subscription key
	reClojureRegSub = regexp.MustCompile(
		`(?m)^\s*\((?:\w+/)?reg-sub(?:-raw)?\s+(:{1,2}[\w/.\-?!]+)`,
	)

	// Re-frame effect/coeffect registration: (reg-fx :effect-name handler)
	// and (reg-cofx :cofx-name handler)
	// Group 1: the effect key
	reClojureRegFxCofx = regexp.MustCompile(
		`(?m)^\s*\((?:\w+/)?reg-(?:fx|cofx)\s+(:{1,2}[\w/.\-?!]+)`,
	)

	// clojure.spec definition: (s/def ::key spec)
	// Also handles: (spec/def ::key spec), (s.alpha/def ::key spec)
	// Group 1: the spec key
	reClojureSpecDef = regexp.MustCompile(
		`(?m)^\s*\((?:\w+/)?(?:s/def|spec/def)\s+(:{1,2}[\w/.\-?!]+)`,
	)
)

// Parse extracts code entities from a single Clojure file and merges them into the graph.
func (p *ClojureParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	content := string(src)
	if content == "" {
		return nil
	}

	// Strip (comment ...) blocks before regex matching to avoid extracting
	// ghost functions from commented-out code. Replace with spaces (preserving
	// byte offsets and line numbers for all subsequent patterns).
	content = stripClojureCommentBlocks(content)

	lines := strings.Split(content, "\n")
	emitted := make(map[string]bool)
	importedPkgs := make(map[string]bool)

	// ── Helper: emit a package import node + EdgeImports ──────────────────────
	// lineNum is the 1-based source line where the import was found (0 = unknown).
	emitImport := func(pkgName string, lineNum int) {
		if pkgName == "" || importedPkgs[pkgName] {
			return
		}
		importedPkgs[pkgName] = true
		pkgNodeID := g.MakeNodeID(pkgName, pkgName)
		g.AddNode(&graph.Node{
			ID:      pkgNodeID,
			Type:    graph.NodePackage,
			Name:    pkgName,
			Package: pkgName,
			File:    filePath,
			Line:    lineNum,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: pkgNodeID, Type: graph.EdgeImports})
	}

	// ── Helper: compute 1-based line number from byte offset ──────────────────
	lineOfOffset := func(offset int) int {
		return strings.Count(content[:offset], "\n") + 1
	}

	// ── Helper: extract docstring from the text that follows the name/args in defn ──
	// After matching a defn, find the remainder of that form and check for a
	// leading string literal. The remainder starts right after the matched span.
	extractDocstringAfter := func(matchEnd int) string {
		if matchEnd >= len(content) {
			return ""
		}
		// Skip past the name (already consumed by regex) and look for
		// optional whitespace + optional metadata + optional arg vector + optional doc.
		// We just look ahead for the pattern: optional-spaces, then '"...'
		// Pattern: (defn name "docstring" [...])
		// After the match end: we need to skip optional whitespace and look for a string.
		rest := content[matchEnd:]
		// Skip over arg vector(s) to find docstring.
		// Look for a quoted string immediately after the match (name already matched).
		// The docstring appears BEFORE the args vector in: (defn name "doc" [args] body)
		// So we check if the next non-space token is a quoted string.
		trimmed := strings.TrimLeft(rest, " \t")
		if m := reClojureDocstring.FindStringSubmatch(trimmed); m != nil {
			return m[1]
		}
		return ""
	}

	// ── 1. Namespace declaration ───────────────────────────────────────────────
	var namespaceName string
	if m := reClojureNS.FindStringSubmatchIndex(content); m != nil {
		namespaceName = content[m[2]:m[3]]
		nsNodeID := g.MakeNodeID(namespaceName, namespaceName)
		g.AddNode(&graph.Node{
			ID:      nsNodeID,
			Type:    graph.NodePackage,
			Name:    namespaceName,
			Package: namespaceName,
			File:    filePath,
			Line:    lineOfOffset(m[0]),
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nsNodeID, Type: graph.EdgeImports})
		importedPkgs[namespaceName] = true

		// Extract :require inside the ns form.
		// Find the ns form's body and look for (:require [...]) sections.
		nsLine := lineOfOffset(m[0])
		nsFormEnd := findFormEnd(content, m[0])
		if nsFormEnd > m[0] {
			nsBody := content[m[0]:nsFormEnd]
			// Find the :require section.
			reqIdx := strings.Index(nsBody, ":require")
			if reqIdx >= 0 {
				// Extract all [ns-name ...] vectors within the require section.
				// Scope: from :require to the next top-level keyword or end of ns form.
				requireSection := nsBody[reqIdx:]
				// Limit scope to current :require block (until next opening keyword at same level).
				endOfRequire := findRequireBlockEnd(requireSection)
				requireBlock := requireSection[:endOfRequire]
				for _, vm := range reClojureRequireVec.FindAllStringSubmatchIndex(requireBlock, -1) {
					ns := requireBlock[vm[2]:vm[3]]
					if ns != "" && ns != namespaceName {
						// Use the ns form's line as the line number for its requirements —
						// the exact per-require line would require tracking substring offsets.
						emitImport(ns, nsLine)
					}
				}
			}
		}
	}
	_ = namespaceName

	// ── 2. Standalone (require '[...]) forms ──────────────────────────────────
	for _, m := range reClojureRequire.FindAllStringSubmatchIndex(content, -1) {
		ns := content[m[2]:m[3]]
		emitImport(ns, lineOfOffset(m[0]))
	}

	// ── 3. defn / defn- ───────────────────────────────────────────────────────
	for _, m := range reClojureDefn.FindAllStringSubmatchIndex(content, -1) {
		// Group 1: "-" suffix (non-empty means defn-)
		// Group 2: function name
		dash := content[m[2]:m[3]]  // "" or "-"
		name := content[m[4]:m[5]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := lineOfOffset(m[0])

		// Determine export status:
		// - defn- → Exported: false
		// - ^:private metadata in the matched text → Exported: false
		matchedText := content[m[0]:m[1]]
		isPrivate := dash == "-" || strings.Contains(matchedText, "^:private")
		exported := !isPrivate

		// Extract docstring: appears after the name and before the args vector.
		doc := extractDocstringAfter(m[5])

		var meta map[string]string
		if doc != "" {
			meta = map[string]string{"doc": doc}
		}

		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: exported,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 4. defmacro ───────────────────────────────────────────────────────────
	for _, m := range reClojureDefmacro.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := lineOfOffset(m[0])
		doc := extractDocstringAfter(m[3])

		meta := map[string]string{"kind": "macro"}
		if doc != "" {
			meta["doc"] = doc
		}

		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 5. defmulti — multimethod declarations ────────────────────────────────
	for _, m := range reClojureDefmulti.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: !strings.HasPrefix(name, "-"),
			Metadata: map[string]string{"kind": "defmulti"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 6. defmethod — multimethod implementations ────────────────────────────
	for _, m := range reClojureDefmethod.FindAllStringSubmatchIndex(content, -1) {
		multifnName := content[m[2]:m[3]]
		dispatchVal := content[m[4]:m[5]]
		// Node name: "multifn.dispatch-val" (unique per dispatch value)
		nodeName := multifnName + "." + strings.Trim(dispatchVal, ":")
		if emitted[nodeName] {
			continue
		}
		emitted[nodeName] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		nodeID := g.MakeNodeID(filePath, nodeName)
		meta := map[string]string{
			"kind":           "defmethod",
			"multimethod":    multifnName,
			"dispatch_value": dispatchVal,
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     nodeName,
			File:     filePath,
			Line:     line,
			Exported: !strings.HasPrefix(multifnName, "-"),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 7. defrecord ──────────────────────────────────────────────────────────
	for _, m := range reClojureDefrecord.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := lineOfOffset(m[0])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: map[string]string{"kind": "record"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 8. defprotocol ────────────────────────────────────────────────────────
	for _, m := range reClojureDefprotocol.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := lineOfOffset(m[0])

		// Extract optional docstring from protocol form.
		doc := extractDocstringAfter(m[3])

		meta := map[string]string{"kind": "protocol"}
		if doc != "" {
			meta["doc"] = doc
		}

		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 9. deftype ────────────────────────────────────────────────────────────
	for _, m := range reClojureDeftype.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := lineOfOffset(m[0])
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeStruct,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: map[string]string{"kind": "type"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 10. def (top-level vars) ──────────────────────────────────────────────
	for _, m := range reClojureDef.FindAllStringSubmatchIndex(content, -1) {
		// Group 1: metadata like "^:private" or "^{:added "1.4"}" (may be -1)
		// Group 2: var name
		var metaFlag string
		if m[2] >= 0 && m[3] >= 0 {
			metaFlag = content[m[2]:m[3]]
		}
		name := content[m[4]:m[5]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineNum := lineOfOffset(m[0])
		isPrivate := metaFlag == "^:private"
		exported := !isPrivate

		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     name,
			File:     filePath,
			Line:     lineNum,
			Exported: exported,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 11. Re-frame event registrations ──────────────────────────────────────
	for _, m := range reClojureRegEvent.FindAllStringSubmatchIndex(content, -1) {
		eventKey := content[m[2]:m[3]]
		if eventKey == "" || emitted[eventKey] {
			continue
		}
		emitted[eventKey] = true
		lineNum := lineOfOffset(m[0])
		nodeID := g.MakeNodeID(filePath, eventKey)
		g.AddNode(&graph.Node{
			ID:   nodeID,
			Type: graph.NodeFunction,
			Name: eventKey,
			File: filePath,
			Line: lineNum,
			// Events are always "exported" — they're dispatched by key from anywhere.
			Exported: true,
			Metadata: map[string]string{
				"kind": "re-frame-event",
			},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 12. Re-frame subscription registrations ────────────────────────────────
	for _, m := range reClojureRegSub.FindAllStringSubmatchIndex(content, -1) {
		subKey := content[m[2]:m[3]]
		if subKey == "" || emitted[subKey] {
			continue
		}
		emitted[subKey] = true
		lineNum := lineOfOffset(m[0])
		nodeID := g.MakeNodeID(filePath, subKey)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     subKey,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: map[string]string{
				"kind": "re-frame-sub",
			},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 13. Re-frame effect/coeffect registrations ─────────────────────────────
	for _, m := range reClojureRegFxCofx.FindAllStringSubmatchIndex(content, -1) {
		fxKey := content[m[2]:m[3]]
		if fxKey == "" || emitted[fxKey] {
			continue
		}
		emitted[fxKey] = true
		lineNum := lineOfOffset(m[0])
		nodeID := g.MakeNodeID(filePath, fxKey)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     fxKey,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: map[string]string{
				"kind": "re-frame-fx",
			},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 14. clojure.spec definitions (s/def) ──────────────────────────────────
	for _, m := range reClojureSpecDef.FindAllStringSubmatchIndex(content, -1) {
		specKey := content[m[2]:m[3]]
		if specKey == "" || emitted[specKey] {
			continue
		}
		emitted[specKey] = true
		lineNum := lineOfOffset(m[0])
		nodeID := g.MakeNodeID(filePath, specKey)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     specKey,
			File:     filePath,
			Line:     lineNum,
			Exported: true,
			Metadata: map[string]string{
				"kind": "spec",
			},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// ── 15. defalias — alias declarations ────────────────────────────────────
	for _, m := range reClojureDefalias.FindAllStringSubmatchIndex(content, -1) {
		name := content[m[2]:m[3]]
		if emitted[name] {
			continue
		}
		emitted[name] = true
		lineIdx := strings.Count(content[:m[0]], "\n")
		line := lineIdx + 1
		nodeID := g.MakeNodeID(filePath, name)
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     line,
			Exported: !strings.HasPrefix(name, "-"),
			Metadata: map[string]string{"kind": "alias"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	_ = lines
	return nil
}

// stripClojureCommentBlocks replaces the contents of all top-level (comment ...)
// forms with spaces, preserving byte offsets and newlines so that line numbers
// computed from byte offsets remain accurate for all other patterns.
//
// A (comment ...) form in Clojure is a legitimate "rich comment block" widely
// used for REPL experiments and example code — it evaluates to nil at runtime.
// Without stripping, regex patterns like reClojureDefn would match any (defn
// inside a (comment (defn foo ...)), producing ghost function nodes.
func stripClojureCommentBlocks(content string) string {
	// reCommentForm matches the opening of a (comment ...) form at the start
	// of a line (allowing leading whitespace) — this is the typical top-level usage.
	reCommentForm := regexp.MustCompile(`(?m)^\s*\(comment\b`)
	buf := []byte(content)
	for _, loc := range reCommentForm.FindAllIndex(buf, -1) {
		// loc[0] is the start of whitespace/opening paren.
		// Find the opening '(' of the comment form.
		parenIdx := loc[0]
		for parenIdx < len(buf) && buf[parenIdx] != '(' {
			parenIdx++
		}
		if parenIdx >= len(buf) {
			continue
		}
		// Find the end of this form using the paren-balancing helper logic inline.
		end := findFormEndBytes(buf, parenIdx)
		if end <= parenIdx {
			continue
		}
		// Replace everything between the outer parens (exclusive) with spaces,
		// keeping newlines intact so line-number calculations stay correct.
		for i := parenIdx + 1; i < end-1 && i < len(buf); i++ {
			if buf[i] != '\n' {
				buf[i] = ' '
			}
		}
	}
	return string(buf)
}

// findFormEndBytes is the []byte equivalent of findFormEnd, used by
// stripClojureCommentBlocks to avoid repeated string↔[]byte conversions.
func findFormEndBytes(buf []byte, startIdx int) int {
	depth := 0
	for i := startIdx; i < len(buf); i++ {
		switch buf[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		case '"':
			i++
			for i < len(buf) && buf[i] != '"' {
				if buf[i] == '\\' {
					i++
				}
				i++
			}
		case ';':
			for i < len(buf) && buf[i] != '\n' {
				i++
			}
		}
	}
	return startIdx
}

// findFormEnd finds the index of the closing paren of the form starting at
// startIdx in content. Returns startIdx if no closing paren is found.
// This is a simple paren-counter — not a full Clojure reader.
func findFormEnd(content string, startIdx int) int {
	depth := 0
	for i := startIdx; i < len(content); i++ {
		switch content[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		case '"':
			// Skip string literals to avoid counting parens inside strings.
			i++
			for i < len(content) && content[i] != '"' {
				if content[i] == '\\' {
					i++ // skip escaped char
				}
				i++
			}
		case ';':
			// Skip line comments.
			for i < len(content) && content[i] != '\n' {
				i++
			}
		}
	}
	return startIdx
}

// findRequireBlockEnd returns the length of the :require block starting at the
// beginning of s (which should start with ":require"). The block ends when we
// hit another top-level keyword (:import, :use, :refer-clojure, etc.) at
// the same bracket depth, or the enclosing ns form ends.
func findRequireBlockEnd(s string) int {
	// Simple heuristic: find the next occurrence of a keyword like :import, :use,
	// :refer-clojure, :gen-class at depth 1 (inside ns but outside :require vectors).
	// We track bracket depth to know when we leave the :require section.
	if len(s) == 0 {
		return 0
	}
	// Start after ":require" itself.
	inString := false
	depth := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			if ch == '\\' {
				i++
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth < 0 {
				return i // end of enclosing form
			}
		}
		// Look for another top-level ns clause keyword at depth 0/1
		// (after the initial descent into :require).
		if depth == 0 && i > 8 { // past ":require"
			// Check if we're at another keyword.
			rest := s[i:]
			for _, kw := range []string{":import", ":use ", ":refer-clojure", ":gen-class", ":load"} {
				if strings.HasPrefix(rest, kw) {
					return i
				}
			}
		}
	}
	return len(s)
}
