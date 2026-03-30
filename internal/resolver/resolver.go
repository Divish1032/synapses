// Package resolver performs a post-parse cross-file CALLS edge resolution pass.
//
// The Go parser (and other language parsers) collect raw call sites during
// AST traversal but cannot resolve cross-file targets at that time because not
// all nodes exist yet. This package drains those call sites after all files are
// parsed and links them to their target nodes via CALLS edges.
package resolver

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ambiguousUnqualified lists method names that are too common to resolve via
// the cross-package fallback (step 2 of direct calls). Without a qualified
// receiver, these names match dozens of unrelated targets (e.g. "write" →
// HTML.write, file.write, csv.writer.write, …) producing false CALLS edges.
var ambiguousUnqualified = map[string]bool{
	"write": true, "read": true, "get": true, "set": true,
	"close": true, "open": true, "init": true, "run": true,
	"start": true, "stop": true, "send": true, "recv": true,
	"push": true, "pop": true, "add": true, "remove": true,
	"delete": true, "update": true, "create": true, "reset": true,
	"clear": true, "flush": true, "load": true, "save": true,
	"parse": true, "render": true, "handle": true, "process": true,
	"execute": true, "call": true, "format": true, "validate": true,
	"copy": true, "clone": true, "merge": true, "split": true,
	"encode": true, "decode": true, "dump": true, "append": true,
	"extend": true, "insert": true, "keys": true, "values": true,
	"items": true, "next": true, "iter": true, "len": true,
	"str": true, "repr": true, "hash": true, "eq": true,
}

// ResolveCallEdges drains all pending call sites from the graph and creates
// CALLS edges for any targets that can be resolved. Returns the number of
// edges created.
//
// Must be called after all files have been parsed (i.e., after WalkDir or
// ParseFile returns) so that all target nodes already exist in the graph.
//
// RTA multi-target: when instantiation data is available (Java/TypeScript),
// an untyped method call may resolve to MULTIPLE targets — all instantiated
// classes that define the method. An edge is emitted to each, matching true
// RTA semantics (Bacon & Sweeney, OOPSLA 1996).
func ResolveCallEdges(g *graph.Graph) int {
	sites := g.DrainCallSites()
	if len(sites) == 0 {
		return 0
	}

	// Build all lookup tables in a single AllNodes() pass to avoid redundant
	// full-graph scans. Previously 3 separate passes; now 1 pass + edge lookups.
	importMap, pkgIndex, dirBaseToPkg := buildLookupTables(g)
	methodIndex := buildMethodIndex(pkgIndex) // derived from pkgIndex, no graph scan
	nameIdx := buildPkgNameIndex(pkgIndex)    // O(1) name lookup per package

	// RTA: collect which types are explicitly instantiated across the project.
	// nil means no instantiation data (e.g. pure Go project) — CHA behavior used.
	instantiated := g.GetInstantiatedTypes()

	// Build inheritance map from heritage_extends metadata (Java/TypeScript).
	// Maps child class name → list of parent class names for method resolution
	// fallback: if child.method() is not found, try parent.method().
	inheritanceMap := buildInheritanceMap(g)

	// Track edges added in this batch. Existing edges checked via g.HasEdge.
	type edgeKey struct{ from, to graph.NodeID }
	seen := make(map[edgeKey]bool)
	resolved := 0

	for _, site := range sites {
		// targets holds all resolved NodeIDs for this call site. Multiple targets
		// occur when RTA finds several instantiated classes with the same method.
		var targets []graph.NodeID

		// Normalize chained self/this access: "self.router" → "router",
		// "this.service" → "service". This handles Python/Java patterns like
		// self.router.add_api_route() where the alias is "self.router".
		// Preserve the original for var-type lookup (keyed as "self.router").
		originalAlias := site.PkgAlias
		if strings.HasPrefix(site.PkgAlias, "self.") || strings.HasPrefix(site.PkgAlias, "this.") {
			site.PkgAlias = site.PkgAlias[strings.IndexByte(site.PkgAlias, '.')+1:]
		}

		// Self/this intra-class resolution: self.method() or this.method()
		// When PkgAlias is "self" or "this", extract the class name from the
		// caller's node name (e.g., "View.dispatch" → class "View") and look
		// for "View.method" in the method index. This is more precise than the
		// package-wide suffix fallback because it targets the exact class.
		if (site.PkgAlias == "self" || site.PkgAlias == "this" || site.PkgAlias == "super" ||
			originalAlias == "self" || originalAlias == "this" || originalAlias == "super") && len(targets) == 0 {
			callerNode := g.GetNode(site.CallerID)
			if callerNode != nil {
				if dot := strings.LastIndexByte(callerNode.Name, '.'); dot > 0 {
					className := callerNode.Name[:dot]
					isSuperCall := site.PkgAlias == "super" || originalAlias == "super"
					if isSuperCall {
						// super.method() — skip the current class, start from parent.
						if id := findByInheritedMethod(methodIndex, inheritanceMap, className, site.FuncName, true); id != "" {
							targets = []graph.NodeID{id}
						}
					} else {
						// this.method() — try current class first, then walk parents.
						if id := findByTypedMethod(methodIndex, className, site.FuncName); id != "" {
							targets = []graph.NodeID{id}
						} else if id := findByInheritedMethod(methodIndex, inheritanceMap, className, site.FuncName, false); id != "" {
							targets = []graph.NodeID{id}
						}
					}
				}
			}
		}

		if site.PkgAlias != "" {
			// Qualified call: pkg.Func() or var.Method()
			// The parser cannot distinguish these at AST time, so we try
			// the import-based resolution first and fall through to
			// method resolution if the alias is not an import.
			aliases, ok := importMap[site.CallerFile]
			if ok {
				if importPath, found := aliases[site.PkgAlias]; found {
					shortPkg := path.Base(importPath)
					targets = findInPackage(nameIdx, shortPkg, site.FuncName, instantiated)
					// Fallback: directory name may differ from Go package clause
					// (e.g. directory "v2" but `package foo`). Use dirBaseToPkg
					// to find the actual package name used in pkgIndex.
					if len(targets) == 0 {
						if actualPkg, ok := dirBaseToPkg[shortPkg]; ok {
							targets = findInPackage(nameIdx, actualPkg, site.FuncName, instantiated)
						}
					}
				}
			}

			// Second try: var type map — Python/Java obj.method() where obj has a
			// known declared type (e.g. repo: Repository = ... or Repository repo = ...).
			// Resolve the type name then look for TypeName.method across all packages.
			// This path is already precise (exact type known) — single target only.
			// Falls back to inheritance chain if the exact type lacks the method.
			if len(targets) == 0 {
				varTypes := g.GetVarTypes(site.CallerFile)
				// Try both normalized alias ("router") and original ("self.router").
				for _, tryAlias := range []string{site.PkgAlias, originalAlias} {
					if typeName, hasType := varTypes[tryAlias]; hasType {
						if id := findByTypedMethod(methodIndex, typeName, site.FuncName); id != "" {
							targets = []graph.NodeID{id}
							break
						}
						// Inheritance fallback: type may inherit the method from a parent.
						if id := findByInheritedMethod(methodIndex, inheritanceMap, typeName, site.FuncName, false); id != "" {
							targets = []graph.NodeID{id}
							break
						}
					}
				}
			}

			// Class-name resolution: if the alias looks like a type name (starts
			// with an uppercase letter or contains "::" for Ruby/Rust), try it
			// directly as a class name in the method index. This handles
			// Retrofit.create(), Builder.use(), Rack::Handler.get() etc. without
			// the imprecision of a package-wide suffix search.
			if len(targets) == 0 && len(site.PkgAlias) > 0 {
				first := site.PkgAlias[0]
				isTypeName := (first >= 'A' && first <= 'Z') || strings.Contains(site.PkgAlias, "::")
				if isTypeName {
					// Build candidate type names: the full alias, and for Ruby "::"
					// namespaces (Rack::Handler), the last segment (Handler).
					tryNames := []string{site.PkgAlias}
					if idx := strings.LastIndex(site.PkgAlias, "::"); idx >= 0 {
						tryNames = append(tryNames, site.PkgAlias[idx+2:])
					}
					for _, typeName := range tryNames {
						if id := findByTypedMethod(methodIndex, typeName, site.FuncName); id != "" {
							targets = []graph.NodeID{id}
							break
						}
						if id := findByInheritedMethod(methodIndex, inheritanceMap, typeName, site.FuncName, false); id != "" {
							targets = []graph.NodeID{id}
							break
						}
					}
				}
			}

			// Capitalized-alias heuristic: for Rust/Go, variable names are often
			// lowercase versions of their type (router → Router, app → App).
			// Try capitalizing the first letter and look up TypeName.method.
			if len(targets) == 0 && len(site.PkgAlias) > 0 {
				first := site.PkgAlias[0]
				if first >= 'a' && first <= 'z' {
					capitalized := strings.ToUpper(site.PkgAlias[:1]) + site.PkgAlias[1:]
					if id := findByTypedMethod(methodIndex, capitalized, site.FuncName); id != "" {
						targets = []graph.NodeID{id}
					}
				}
			}

			// Fallback: alias was not an import, typed var, or class name — treat as var.Method().
			// Search the caller's own package and all imported packages
			// for a method matching ".FuncName" (e.g. Graph.CarveEgoGraph).
			if len(targets) == 0 {
				callerNode := g.GetNode(site.CallerID)
				if callerNode != nil {
					targets = findInPackage(nameIdx, callerNode.Package, site.FuncName, instantiated)
				}
				if len(targets) == 0 {
					if aliases, ok := importMap[site.CallerFile]; ok {
						sortedPaths := make([]string, 0, len(aliases))
						for _, p := range aliases {
							sortedPaths = append(sortedPaths, p)
						}
						sort.Strings(sortedPaths)
						for _, importPath := range sortedPaths {
							shortPkg := path.Base(importPath)
							ids := findInPackage(nameIdx, shortPkg, site.FuncName, instantiated)
							if len(ids) == 0 {
								if actualPkg, ok2 := dirBaseToPkg[shortPkg]; ok2 {
									ids = findInPackage(nameIdx, actualPkg, site.FuncName, instantiated)
								}
							}
							if len(ids) > 0 {
								targets = ids
								break
							}
						}
					}
				}
			}
		} else {
			// Direct call: Func()
			// 1. Look in the caller's own package (Go-style, same-package calls).
			callerNode := g.GetNode(site.CallerID)
			if callerNode == nil {
				continue
			}
			targets = findInPackage(nameIdx, callerNode.Package, site.FuncName, instantiated)

			// 2. Fallback: search all packages imported by the caller's file.
			//    This handles Python/TypeScript `from X import Y` style calls where
			//    the symbol is imported directly (no qualifier) from another module.
			//
			//    Guard: skip cross-package fallback for extremely common method
			//    names (write, read, get, set, etc.) — false positives from these
			//    are far more harmful than false negatives.
			if len(targets) == 0 && !ambiguousUnqualified[strings.ToLower(site.FuncName)] {
				if aliases, ok := importMap[site.CallerFile]; ok {
					sortedPaths := make([]string, 0, len(aliases))
					for _, p := range aliases {
						sortedPaths = append(sortedPaths, p)
					}
					sort.Strings(sortedPaths)
					for _, importPath := range sortedPaths {
						shortPkg := path.Base(importPath)
						ids := findInPackage(nameIdx, shortPkg, site.FuncName, instantiated)
						if len(ids) == 0 {
							if actualPkg, ok2 := dirBaseToPkg[shortPkg]; ok2 {
								ids = findInPackage(nameIdx, actualPkg, site.FuncName, instantiated)
							}
						}
						if len(ids) > 0 {
							targets = ids
							break
						}
					}
				}
			}
		}

		for _, targetID := range targets {
			key := edgeKey{site.CallerID, targetID}
			if seen[key] || g.HasEdge(site.CallerID, targetID, graph.EdgeCalls) {
				continue // deduplicate: same function may call the same target multiple times
			}
			seen[key] = true
			g.AddEdge(&graph.Edge{
				From: site.CallerID,
				To:   targetID,
				Type: graph.EdgeCalls,
			})
			resolved++
		}
	}

	return resolved
}

// buildLookupTables builds the import map and package index in a single
// AllNodes() pass, avoiding redundant full-graph scans. Previously these were
// two separate functions (buildImportMap + buildPackageIndex) with two passes.
//
// importMap: absFilePath → {packageAlias → importPath}
// pkgIndex:  shortPkgName → []*Node (functions and methods), sorted by Name
//
// The pkgIndex is sorted by node name to guarantee deterministic resolution
// order in findInPackage across runs (Go map iteration is non-deterministic).
func buildLookupTables(g *graph.Graph) (map[string]map[string]string, map[string][]*graph.Node, map[string]string) {
	importMap := make(map[string]map[string]string)
	pkgIndex := make(map[string][]*graph.Node)

	// Single-pass snapshot: acquire the read lock once for all imports adjacency
	// and the full node map, avoiding O(N×imports) per-node lock acquisitions.
	importAdj, nodeMap := g.SnapshotImportAdjacency()

	// Explicit import aliases recorded by the Go parser (e.g., import alias "pkg").
	// These override the path.Base() derived alias when the two differ.
	explicitAliases := g.SnapshotImportAliases()

	for id, n := range nodeMap {
		switch n.Type {
		case graph.NodeFile:
			aliases := make(map[string]string)
			for _, toID := range importAdj[id] {
				pkgNode := nodeMap[toID]
				if pkgNode == nil || pkgNode.Type != graph.NodePackage {
					continue
				}
				alias := path.Base(pkgNode.Name)
				aliases[alias] = pkgNode.Name
			}
			// Overlay explicit import aliases — these take precedence over
			// path.Base() derived aliases because Go allows renaming imports
			// (e.g., `import fuzzy "github.com/foo/algo"` uses "fuzzy" not "algo").
			if fileAliases := explicitAliases[n.File]; fileAliases != nil {
				for alias, importPath := range fileAliases {
					aliases[alias] = importPath
				}
			}
			importMap[n.File] = aliases
		case graph.NodeFunction, graph.NodeMethod:
			pkgIndex[n.Package] = append(pkgIndex[n.Package], n)
		}
	}

	// Build a directory-base → Go package name mapping from file nodes.
	// In Go, the default import identifier is the package clause name (e.g.
	// `package foo`), NOT the directory name. When these differ (e.g. directory
	// "v2" but `package foo`), path.Base(importPath) returns "v2" but pkgIndex
	// is keyed by "foo". This mapping lets us bridge the gap.
	dirBaseToPkg := make(map[string]string)
	for _, n := range nodeMap {
		if n.Type == graph.NodeFile && n.File != "" && n.Package != "" {
			dirBase := filepath.Base(filepath.Dir(n.File))
			if dirBase != "" && dirBase != "." && dirBase != n.Package {
				dirBaseToPkg[dirBase] = n.Package
			}
		}
	}

	// Sort each package's node list by name for deterministic resolution order.
	// Without this, Go map iteration produces different orderings across runs,
	// causing non-deterministic CALLS edge selection when multiple candidates match.
	for pkg := range pkgIndex {
		sort.Slice(pkgIndex[pkg], func(i, j int) bool {
			return pkgIndex[pkg][i].Name < pkgIndex[pkg][j].Name
		})
	}

	return importMap, pkgIndex, dirBaseToPkg
}

// findInPackage returns NodeIDs for functions/methods named `name` in package `pkg`.
// Methods are stored as "ReceiverType.MethodName", so we match on the suffix.
//
// RTA mode (len(instantiated) > 0):
//   - For method calls: returns ALL candidates whose receiver type is in the
//     instantiated set. If none are instantiated, falls back to [firstMatch]
//     (CHA fallback — avoids losing edges entirely).
//   - For plain function calls (no receiver dot): returns [firstMatch].
//
// CHA fast-path mode (instantiated nil/empty):
//   - Returns [firstMatch] — original behavior, no change.
//
// Returns nil if no candidate exists in the package at all.
//
// The pkgIndex slice is pre-sorted by name (see buildLookupTables), guaranteeing
// deterministic results across runs regardless of Go map iteration order.
// pkgNameIndex maps pkg → funcSuffix → []*Node for O(1) name resolution.
// For a node named "ReceiverType.MethodName", it's indexed under "MethodName".
// For a plain function "Func", it's indexed under "Func".
// Built once by buildPkgNameIndex, used by findInPackage.
type pkgNameIndex map[string]map[string][]*graph.Node

func buildPkgNameIndex(pkgIdx map[string][]*graph.Node) pkgNameIndex {
	result := make(pkgNameIndex, len(pkgIdx))
	for pkg, nodes := range pkgIdx {
		nameMap := make(map[string][]*graph.Node)
		for _, n := range nodes {
			// Index by the function/method suffix: "Recv.Method" → "Method", "Func" → "Func"
			key := n.Name
			if dot := strings.LastIndexByte(n.Name, '.'); dot >= 0 {
				key = n.Name[dot+1:]
			}
			nameMap[key] = append(nameMap[key], n)
		}
		result[pkg] = nameMap
	}
	return result
}

func findInPackage(nameIdx pkgNameIndex, pkg, name string, instantiated map[string]bool) []graph.NodeID {
	pkgNames := nameIdx[pkg]
	if len(pkgNames) == 0 {
		return nil
	}
	candidates := pkgNames[name]
	if len(candidates) == 0 {
		return nil
	}

	var instantiatedMatches []graph.NodeID
	var first graph.NodeID

	for _, n := range candidates {
		if first == "" {
			first = n.ID
		}
		if len(instantiated) > 0 {
			dot := strings.IndexByte(n.Name, '.')
			if dot > 0 {
				// Method node "ReceiverType.MethodName": filter by instantiated receiver.
				if instantiated[n.Name[:dot]] {
					instantiatedMatches = append(instantiatedMatches, n.ID)
				}
			} else {
				// Plain function (no receiver): not subject to RTA filtering.
				if len(instantiatedMatches) == 0 {
					instantiatedMatches = append(instantiatedMatches, n.ID)
				}
			}
		}
	}

	if len(instantiated) > 0 {
		if len(instantiatedMatches) > 0 {
			return instantiatedMatches
		}
		if first != "" {
			return []graph.NodeID{first}
		}
		return nil
	}

	if first != "" {
		return []graph.NodeID{first}
	}
	return nil
}

// buildMethodIndex builds a flat "TypeName.MethodName" → NodeID map from the
// package index, enabling O(1) typed method resolution instead of O(N) full scan.
func buildMethodIndex(pkgIndex map[string][]*graph.Node) map[string]graph.NodeID {
	result := make(map[string]graph.NodeID)
	for _, nodes := range pkgIndex {
		for _, n := range nodes {
			// Only index names that look like qualified methods (contain a dot).
			if strings.Contains(n.Name, ".") {
				result[n.Name] = n.ID
			}
		}
	}
	return result
}

// findByTypedMethod looks up a "TypeName.MethodName" in the pre-built method index.
// O(1) per call instead of O(N) full scan.
func findByTypedMethod(methodIndex map[string]graph.NodeID, typeName, methodName string) graph.NodeID {
	return methodIndex[typeName+"."+methodName]
}

// buildInheritanceMap builds a class name → parent class names map from
// heritage_extends metadata stored on struct/interface nodes during parsing.
// Supports Java (extends/implements) and TypeScript (extends/implements).
//
// Note: keyed by simple class name (not package-qualified), consistent with
// methodIndex. If two packages have same-named classes with different parents,
// parents are merged (both hierarchies are walked). This trades a small risk
// of false positives for much better recall.
func buildInheritanceMap(g *graph.Graph) map[string][]string {
	result := make(map[string][]string)
	seen := make(map[string]map[string]bool) // dedup parents per class name
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeStruct && n.Type != graph.NodeInterface {
			continue
		}
		if n.Metadata == nil {
			continue
		}
		var parents []string
		if ext := n.Metadata["heritage_extends"]; ext != "" {
			parents = append(parents, strings.Split(ext, ",")...)
		}
		if impl := n.Metadata["heritage_implements"]; impl != "" {
			parents = append(parents, strings.Split(impl, ",")...)
		}
		if len(parents) == 0 {
			continue
		}
		if seen[n.Name] == nil {
			seen[n.Name] = make(map[string]bool)
		}
		for _, p := range parents {
			p = strings.TrimSpace(p)
			if p == "" || p == n.Name {
				continue // skip self-references and empty
			}
			if !seen[n.Name][p] {
				seen[n.Name][p] = true
				result[n.Name] = append(result[n.Name], p)
			}
		}
	}
	return result
}

// findByInheritedMethod walks the inheritance chain to find a method defined
// on a parent class. If skipSelf is true (super.method() calls), starts from
// the parents directly; otherwise tries the class itself first (already done
// by caller) then walks parents.
// Guards against circular inheritance with a visited set (max depth 10).
func findByInheritedMethod(
	methodIndex map[string]graph.NodeID,
	inheritanceMap map[string][]string,
	className, methodName string,
	skipSelf bool,
) graph.NodeID {
	visited := make(map[string]bool)
	if skipSelf {
		visited[className] = true
	}
	// BFS through inheritance chain.
	queue := inheritanceMap[className]
	for depth := 0; len(queue) > 0 && depth < 10; depth++ {
		var next []string
		for _, parent := range queue {
			if visited[parent] {
				continue
			}
			visited[parent] = true
			if id := methodIndex[parent+"."+methodName]; id != "" {
				return id
			}
			next = append(next, inheritanceMap[parent]...)
		}
		queue = next
	}
	return ""
}
