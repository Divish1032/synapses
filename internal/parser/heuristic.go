// Package parser — heuristic.go: post-AST pass that injects synthetic HANDLES
// edges for framework routing registrations (R1).
//
// Detects patterns such as:
//   - Go:  http.HandleFunc("/path", handler), r.GET("/path", handler)
//   - TS:  app.get('/path', handler), router.post('/path', handler)
//   - Py:  @app.get('/path'), path('/path', view)
//
// For each match, a virtual NodeRoute node is created representing the route,
// and two synthetic edges are injected:
//
//	enclosingFn --CALLS--> routeNode
//	routeNode   --HANDLES--> handlerFn
//
// Both edges are synthetic (not derived from AST call resolution) and are
// skipped if either endpoint cannot be resolved in the current graph.
// Confidence is stored in routeNode.Metadata["confidence"] (0.0–1.0 as string).
package parser

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// RouteRegistration holds a single detected route registration.
type RouteRegistration struct {
	File         string
	Line         int    // 1-based line number of the registration call
	Method       string // HTTP method ("GET", "POST", …) or "*" for wildcard
	Path         string // URL path e.g. "/api/users"
	Handler      string // handler function name
	EnclosingFn  string // function/method that contains the registration call
	Confidence   float64
}

// -- compiled regexes ---------------------------------------------------------

var (
	// Go: http.HandleFunc("/path", handler) or router.HandleFunc("/path", handler)
	// Captures the LAST identifier before ) — skips any middleware args.
	reGoHandleFunc = regexp.MustCompile(
		`(?:http|r|router|mux|s|srv|server)\.HandleFunc\s*\(\s*` +
			`"([^"]+)"\s*,\s*(?:\w+\s*,\s*)*(\w+)\s*\)`,
	)
	// Go: http.Handle("/path", handler)
	reGoHandle = regexp.MustCompile(
		`(?:http|r|router|mux|s|srv|server)\.Handle\s*\(\s*` +
			`["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]\s*,\s*(?:\w+\s*,\s*)*(\w+)\s*\)`,
	)
	// Go: r.GET("/path", handler) or r.GET(`/path`, handler) etc. (gin, chi, echo, gorilla)
	// Captures the LAST identifier before ) — skips middleware chain args.
	// Variable names include common sub-router aliases: group, h, public, private, admin.
	// Path may use double-quoted or raw (backtick) string literals.
	reGoMethodRoute = regexp.MustCompile(
		`(?:r|router|mux|g|e|app|srv|server|v\d+|api|v1|v2|group|h|public|private|admin)\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS|Any|Handle|Route)\s*\(\s*` +
			`["` + "`" + `]([^"` + "`" + `]+)["` + "`" + `]\s*,\s*(?:\w+\s*,\s*)*(\w+)\s*\)`,
	)
	// TypeScript/JS: app.get('/path', handler), router.post('/path', handler)
	// Captures the LAST identifier before ) — skips middleware chain args.
	// Variable names include sub-router aliases: api, v1, v2, routes.
	reTSMethodRoute = regexp.MustCompile(
		`(?:app|router|server|r|express\(\)|fastify|hono|api|v1|v2|routes)\.(get|post|put|delete|patch|head|options|all|use)\s*\(\s*` +
			`['"]([^'"]+)['"]\s*,\s*(?:\w+\s*,\s*)*(\w+)\s*\)`,
	)

	// Python FastAPI/Flask decorator: @app.get('/path') or @router.post('/path')
	rePyDecorator = regexp.MustCompile(
		`@\s*(?:\w+)\.(get|post|put|delete|patch|head|options|route|add_api_route)\s*\(\s*` +
			`['"]([^'"]+)['"]`,
	)
	// Python Django: path('/path', view_func) or re_path(r'/path', view_func)
	rePyDjango = regexp.MustCompile(
		`(?:path|re_path|url)\s*\(\s*(?:r?['"])([^'"]+)['"]\s*,\s*(\w+)`,
	)

	// Enclosing function detection per language.
	reEnclosingGo  = regexp.MustCompile(`^\s*func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(`)
	reEnclosingTS  = regexp.MustCompile(`^\s*(?:export\s+)?(?:async\s+)?function\s+(\w+)\s*[(<]`)
	reEnclosingPy  = regexp.MustCompile(`^\s*(?:async\s+)?def\s+(\w+)\s*\(`)
	reEnclosingAny = regexp.MustCompile(`^\s*(?:func|function|def|sub|fn)\s+(\w+)\s*[\((\[]`)
)

// ExtractRouteRegistrations scans src for handler registration patterns and
// returns a slice of RouteRegistration describing each match. filePath is used
// to select the right language-specific patterns.
func ExtractRouteRegistrations(filePath string, src []byte) []RouteRegistration {
	ext := strings.ToLower(filePath)
	switch {
	case strings.HasSuffix(ext, ".go"):
		return extractGoRoutes(filePath, src)
	case strings.HasSuffix(ext, ".ts") || strings.HasSuffix(ext, ".tsx") ||
		strings.HasSuffix(ext, ".js") || strings.HasSuffix(ext, ".jsx") ||
		strings.HasSuffix(ext, ".mjs") || strings.HasSuffix(ext, ".cjs"):
		return extractTSRoutes(filePath, src)
	case strings.HasSuffix(ext, ".py"):
		return extractPyRoutes(filePath, src)
	}
	return nil
}

// InjectHandlesEdges resolves registrations to graph nodes and creates
// synthetic route nodes + HANDLES edges. Returns the number of edges injected.
func InjectHandlesEdges(g *graph.Graph, registrations []RouteRegistration) int {
	injected := 0
	for _, reg := range registrations {
		// Skip if no handler or path.
		if reg.Handler == "" || reg.Path == "" {
			continue
		}

		// Resolve the handler function node; prefer the same file/directory
		// to avoid false connections when multiple handlers share a name.
		handlerNode := resolveByNamePreferFile(g, reg.Handler, reg.File)
		if handlerNode == nil {
			continue // handler not in graph, skip
		}

		// Normalise method to uppercase, default to *.
		method := strings.ToUpper(reg.Method)
		if method == "" || method == "ANY" || method == "ALL" || method == "HANDLE" || method == "ROUTE" || method == "ADD_API_ROUTE" {
			method = "*"
		}

		// Build route name and node ID.
		routeName := method + " " + reg.Path
		routeID := g.MakeNodeID(reg.File, "route:"+routeName)

		// Upsert route node — atomic check-and-insert prevents the TOCTOU race
		// where two concurrent incremental-reindex goroutines both see nil and
		// call AddNode, giving the same route node two different StableIDs.
		g.UpsertRouteNode(&graph.Node{
			ID:   routeID,
			Type: graph.NodeRoute,
			Name: routeName,
			File: reg.File,
			Line: reg.Line,
			Metadata: map[string]string{
				"method":     method,
				"path":       reg.Path,
				"handler":    reg.Handler,
				"confidence": fmt.Sprintf("%.2f", reg.Confidence),
				"inferred":   "true",
			},
		})

		// routeNode --HANDLES--> handlerFn
		g.AddEdge(&graph.Edge{
			From: routeID,
			To:   handlerNode.ID,
			Type: graph.EdgeHandles,
		})
		injected++

		// If we know the enclosing function, wire it to the route node with a
		// synthetic CALLS edge so get_call_chain can traverse the full path:
		//   setupFn --CALLS--> routeNode --HANDLES--> handlerFn
		if reg.EnclosingFn != "" {
			enclosingNode := resolveByNameInFile(g, reg.EnclosingFn, reg.File)
			if enclosingNode == nil {
				enclosingNode = resolveByName(g, reg.EnclosingFn)
			}
			if enclosingNode != nil {
				g.AddEdge(&graph.Edge{
					From: enclosingNode.ID,
					To:   routeID,
					Type: graph.EdgeCalls, // synthetic call: setup → route
				})
			}
		}
	}
	return injected
}

// ApplyHeuristics is the convenience entry-point called from WalkDir / ParseFile.
// It extracts route registrations from src and injects HANDLES edges into g.
func ApplyHeuristics(g *graph.Graph, filePath string, src []byte) {
	regs := ExtractRouteRegistrations(filePath, src)
	if len(regs) == 0 {
		return
	}
	InjectHandlesEdges(g, regs)
}

// -- language-specific extractors ---------------------------------------------

func extractGoRoutes(filePath string, src []byte) []RouteRegistration {
	// Merge two-line route calls: r.GET("/path",\n\thandler) is common in
	// formatted Go code. Joining them lets the single-line regexes match.
	lines := mergeGoMultilineRoutes(bytes.Split(src, []byte("\n")))
	var out []RouteRegistration

	for i, rawLine := range lines {
		line := string(rawLine)
		lineNum := i + 1

		// http.HandleFunc / router.HandleFunc
		if m := reGoHandleFunc.FindStringSubmatch(line); m != nil {
			out = append(out, RouteRegistration{
				File:        filePath,
				Line:        lineNum,
				Method:      "*",
				Path:        m[1],
				Handler:     m[2],
				EnclosingFn: findEnclosingFunc(lines, i, reEnclosingGo),
				Confidence:  0.95,
			})
			continue
		}
		// http.Handle / router.Handle
		if m := reGoHandle.FindStringSubmatch(line); m != nil {
			out = append(out, RouteRegistration{
				File:        filePath,
				Line:        lineNum,
				Method:      "*",
				Path:        m[1],
				Handler:     m[2],
				EnclosingFn: findEnclosingFunc(lines, i, reEnclosingGo),
				Confidence:  0.90,
			})
			continue
		}
		// r.GET / r.POST / etc.
		if m := reGoMethodRoute.FindStringSubmatch(line); m != nil {
			out = append(out, RouteRegistration{
				File:        filePath,
				Line:        lineNum,
				Method:      m[1],
				Path:        m[2],
				Handler:     m[3],
				EnclosingFn: findEnclosingFunc(lines, i, reEnclosingGo),
				Confidence:  0.90,
			})
			continue
		}

	}
	return out
}

func extractTSRoutes(filePath string, src []byte) []RouteRegistration {
	lines := mergeMultilineRoutesTS(bytes.Split(src, []byte("\n")))
	var out []RouteRegistration
	for i, rawLine := range lines {
		line := string(rawLine)
		lineNum := i + 1
		if m := reTSMethodRoute.FindStringSubmatch(line); m != nil {
			method := strings.ToUpper(m[1])
			if method == "USE" || method == "ALL" {
				method = "*"
			}
			out = append(out, RouteRegistration{
				File:        filePath,
				Line:        lineNum,
				Method:      method,
				Path:        m[2],
				Handler:     m[3],
				EnclosingFn: findEnclosingFunc(lines, i, reEnclosingTS),
				Confidence:  0.90,
			})
		}
	}
	return out
}

// rePyIncompleteDecorator matches a Python route decorator whose opening paren
// is on the line but the path argument is on the next line:
//
//	@app.get(
//	    "/users",         ← path is here, not on the decorator line
var rePyIncompleteDecorator = regexp.MustCompile(
	`@\s*\w+\.(get|post|put|delete|patch|head|options|route|add_api_route)\s*\(\s*$`,
)

// mergePyMultilineRoutes joins Python route decorator lines where the quoted
// path argument appears on the continuation line rather than on the decorator
// line itself. This is common with multi-argument FastAPI decorators:
//
//	@app.get(
//	    "/users",
//	    response_model=List[User],
//	)
//
// Only the decorator + path line are merged; the rest of the arguments are
// left on their original lines. The continuation line is blanked so line
// indices remain correct for RouteRegistration.Line.
func mergePyMultilineRoutes(lines [][]byte) [][]byte {
	result := make([][]byte, len(lines))
	copy(result, lines)
	for i := 0; i < len(result)-1; i++ {
		trimmed := bytes.TrimRight(result[i], " \t\r\n")
		if !rePyIncompleteDecorator.Match(trimmed) {
			continue
		}
		// Find the next non-empty line and check whether it holds the path arg.
		for j := i + 1; j < len(result); j++ {
			next := bytes.TrimSpace(result[j])
			if len(next) == 0 {
				continue
			}
			// Only merge if the continuation line starts with a quoted string
			// (the path argument). Lines starting with a keyword argument or
			// closing paren indicate the path was on the decorator line already.
			if len(next) > 0 && (next[0] == '"' || next[0] == '\'') {
				result[i] = append(trimmed, ' ')
				result[i] = append(result[i], next...)
				result[j] = nil
				// Blank remaining decorator args up to and including the
				// closing paren so findNextFunc reaches the `def` line below.
				for k := j + 1; k < len(result); k++ {
					kLine := bytes.TrimSpace(result[k])
					if len(kLine) == 0 {
						continue
					}
					result[k] = nil
					if bytes.ContainsRune(kLine, ')') {
						break
					}
				}
			}
			break
		}
	}
	for i, l := range result {
		if l == nil {
			result[i] = []byte{}
		}
	}
	return result
}

func extractPyRoutes(filePath string, src []byte) []RouteRegistration {
	lines := mergePyMultilineRoutes(bytes.Split(src, []byte("\n")))
	var out []RouteRegistration
	for i, rawLine := range lines {
		line := string(rawLine)
		lineNum := i + 1

		// FastAPI/Flask decorator: @app.get('/path')
		if m := rePyDecorator.FindStringSubmatch(line); m != nil {
			method := strings.ToUpper(m[1])
			if method == "ROUTE" || method == "ADD_API_ROUTE" {
				method = "*"
			}
			// Handler is the function declared AFTER the decorator (next def line).
			handler := findNextFunc(lines, i, reEnclosingPy)
			if handler == "" {
				handler = findEnclosingFunc(lines, i, reEnclosingPy)
			}
			out = append(out, RouteRegistration{
				File:        filePath,
				Line:        lineNum,
				Method:      method,
				Path:        m[2],
				Handler:     handler,
				EnclosingFn: enclosingFnPy(lines, i),
				Confidence:  0.95,
			})
			continue
		}
		// Django: path('/path', view_func)
		if m := rePyDjango.FindStringSubmatch(line); m != nil {
			out = append(out, RouteRegistration{
				File:        filePath,
				Line:        lineNum,
				Method:      "*",
				Path:        m[1],
				Handler:     m[2],
				EnclosingFn: findEnclosingFunc(lines, i, reEnclosingPy),
				Confidence:  0.85,
			})
		}
	}
	return out
}

// -- helpers ------------------------------------------------------------------

// findEnclosingFunc returns the name of the function enclosing the given line
// by scanning backward with the provided regex. Returns "" if not found.
func findEnclosingFunc(lines [][]byte, targetIdx int, re *regexp.Regexp) string {
	for i := targetIdx; i >= 0; i-- {
		if m := re.FindSubmatch(lines[i]); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// enclosingFnPy finds the Python function that ACTUALLY CONTAINS targetIdx by
// requiring that the enclosing `def` has strictly lower indentation than the
// target line. A plain backward scan can falsely match the previous sibling
// function at the same indentation level — this version avoids that error.
// Returns "" when the target is at module level (no true enclosing function).
func enclosingFnPy(lines [][]byte, targetIdx int) string {
	if targetIdx < 0 || targetIdx >= len(lines) {
		return ""
	}
	targetIndent := lineIndent(lines[targetIdx])
	for i := targetIdx - 1; i >= 0; i-- {
		if strings.TrimSpace(string(lines[i])) == "" {
			continue
		}
		indent := lineIndent(lines[i])
		if indent >= targetIndent {
			// Same or deeper indentation — sibling or child, keep scanning up.
			continue
		}
		// Strictly lower indentation: check if it is a def.
		if m := reEnclosingPy.FindSubmatch(lines[i]); m != nil {
			return string(m[1])
		}
		// Lower-indented but not a def (class, if, with, etc.) — stop; the
		// decorator is inside a non-function block, treat as no enclosing fn.
		break
	}
	return ""
}

// lineIndent returns the number of leading whitespace characters (spaces + tabs).
func lineIndent(line []byte) int {
	return len(line) - len(bytes.TrimLeft(line, " \t"))
}

// findNextFunc returns the name of the next function declaration AFTER targetIdx.
// Stops at the first non-decorator, non-blank line that doesn't match.
func findNextFunc(lines [][]byte, targetIdx int, re *regexp.Regexp) string {
	for i := targetIdx + 1; i < len(lines); i++ {
		line := strings.TrimSpace(string(lines[i]))
		if line == "" {
			continue
		}
		if m := re.FindSubmatch(lines[i]); m != nil {
			return string(m[1])
		}
		// If the next non-empty line starts another decorator, keep scanning.
		if strings.HasPrefix(line, "@") {
			continue
		}
		break
	}
	return ""
}

// resolveByName finds the best-match node for a function/method name using
// exact name lookup only. The former FindByPattern fuzzy fallback was removed
// because substring matches create false HANDLES edges when handler names are
// substrings of unrelated functions (e.g. "Get" matching "GetUser", "GetOrder").
func resolveByName(g *graph.Graph, name string) *graph.Node {
	nodes := g.FindByName(name)
	for _, n := range nodes {
		if n.Type == graph.NodeFunction || n.Type == graph.NodeMethod {
			return n
		}
	}
	if len(nodes) > 0 {
		return nodes[0]
	}
	return nil
}

// resolveByNameInFile finds a node by name preferring nodes from the given file.
func resolveByNameInFile(g *graph.Graph, name, filePath string) *graph.Node {
	nodes := g.FindByName(name)
	for _, n := range nodes {
		if n.File == filePath {
			return n
		}
	}
	return nil
}

// resolveByNamePreferFile resolves a handler name with a preference cascade:
//  1. Same file as the route registration (most accurate)
//  2. Same directory as the route registration (common pattern: handlers in
//     the same package as the router setup file)
//  3. Any function/method match (original resolveByName behaviour)
//
// This avoids false connections when multiple packages define handlers with
// the same name (e.g. every service defining its own "Health" handler).
func resolveByNamePreferFile(g *graph.Graph, name, filePath string) *graph.Node {
	// 1. Same file.
	if n := resolveByNameInFile(g, name, filePath); n != nil {
		return n
	}
	// 2. Same directory.
	dir := filePath
	if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
		dir = filePath[:idx]
	}
	nodes := g.FindByName(name)
	for _, n := range nodes {
		if (n.Type == graph.NodeFunction || n.Type == graph.NodeMethod) &&
			strings.HasPrefix(n.File, dir+"/") {
			return n
		}
	}
	// 3. Fall back to global first-function-match.
	return resolveByName(g, name)
}

// reTSIncompleteRoute matches a TS/JS route call that has a quoted path argument
// but is missing the handler (the closing paren is on the next line).
// Used by mergeMultilineRoutesTS to normalise split-line route registrations.
var reTSIncompleteRoute = regexp.MustCompile(
	`(?:app|router|server|r|express\(\)|fastify|hono|api|v1|v2|routes)\.` +
		`(?:get|post|put|delete|patch|head|options|all|use)\s*` +
		`\(\s*['"][^'"]+['"]\s*,\s*$`,
)

// mergeMultilineRoutesTS joins consecutive lines where a TS/JS route call begins
// on one line (path arg present) but the handler identifier appears on the next.
// This is common in auto-formatted TypeScript:
//
//	app.get('/users',
//	  listUsers)
//
// Behaviour mirrors mergeGoMultilineRoutes — continuation lines become empty
// so RouteRegistration.Line numbers remain correct.
func mergeMultilineRoutesTS(lines [][]byte) [][]byte {
	result := make([][]byte, len(lines))
	copy(result, lines)
	for i := 0; i < len(result)-1; i++ {
		trimmed := bytes.TrimRight(result[i], " \t\r\n")
		if !reTSIncompleteRoute.Match(trimmed) {
			continue
		}
		for j := i + 1; j < len(result); j++ {
			next := bytes.TrimSpace(result[j])
			if len(next) == 0 {
				continue
			}
			result[i] = append(trimmed, ' ')
			result[i] = append(result[i], next...)
			result[j] = nil
			break
		}
	}
	for i, l := range result {
		if l == nil {
			result[i] = []byte{}
		}
	}
	return result
}

// reGoIncompleteRoute matches a Go route call that has a quoted path argument
// (double-quoted or backtick raw string) but is missing the handler (the
// closing paren is on the next line).
// Used by mergeGoMultilineRoutes to normalise split-line route registrations.
var reGoIncompleteRoute = regexp.MustCompile(
	`(?:http|r|router|mux|g|e|app|srv|server|v\d+|api|v1|v2|group|h|public|private|admin)\.` +
		`(?:GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS|HandleFunc|Handle|Any|Route)\s*` +
		`\(\s*["` + "`" + `][^"` + "`" + `]+["` + "`" + `]\s*,\s*$`,
)

// mergeGoMultilineRoutes joins consecutive lines where a route call begins on
// one line (path arg present) but the handler identifier appears on the next.
// This is a common Go formatting pattern:
//
//	r.GET("/users",
//	    GetUsers)
//
// The merged result is returned as a new slice; original line indices are
// preserved for all lines that were not merged (the continuation line becomes
// empty so line numbers remain correct for RouteRegistration.Line).
func mergeGoMultilineRoutes(lines [][]byte) [][]byte {
	result := make([][]byte, len(lines))
	copy(result, lines)
	for i := 0; i < len(result)-1; i++ {
		trimmed := bytes.TrimRight(result[i], " \t\r\n")
		if !reGoIncompleteRoute.Match(trimmed) {
			continue
		}
		// Accumulate continuation lines until the closing paren is found or
		// we run out of lines. This handles routes split across 3+ lines:
		//
		//   r.GET("/users",         ← line i   — matches reGoIncompleteRoute
		//       authMiddleware,     ← line i+1 — no ")", keep going
		//       GetUsers)           ← line i+2 — has ")", stop
		//
		// Each consumed continuation line is blanked so line numbers stay valid
		// for RouteRegistration.Line (they were all i+N originally).
		for j := i + 1; j < len(result); j++ {
			next := bytes.TrimSpace(result[j])
			if len(next) == 0 {
				continue
			}
			result[i] = append(bytes.TrimRight(result[i], " \t\r\n"), ' ')
			result[i] = append(result[i], next...)
			result[j] = nil
			// Stop once the closing paren is on this continuation line.
			if bytes.ContainsRune(next, ')') {
				break
			}
		}
	}
	// Compact: replace nil entries with empty slices so line indices stay valid.
	for i, l := range result {
		if l == nil {
			result[i] = []byte{}
		}
	}
	return result
}
