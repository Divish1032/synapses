// Package resolver performs a post-parse cross-file CALLS edge resolution pass.
//
// The Go parser (and other language parsers) collect raw call sites during
// AST traversal but cannot resolve cross-file targets at that time because not
// all nodes exist yet. This package drains those call sites after all files are
// parsed and links them to their target nodes via CALLS edges.
package resolver

import (
	"path"
	"sort"
	"strings"

	"github.com/SynapsesOS/synapses/internal/graph"
)

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
	importMap, pkgIndex := buildLookupTables(g)
	methodIndex := buildMethodIndex(pkgIndex) // derived from pkgIndex, no graph scan

	// RTA: collect which types are explicitly instantiated across the project.
	// nil means no instantiation data (e.g. pure Go project) — CHA behavior used.
	instantiated := g.GetInstantiatedTypes()

	// Track edges added in this batch. Existing edges checked via g.HasEdge.
	type edgeKey struct{ from, to graph.NodeID }
	seen := make(map[edgeKey]bool)
	resolved := 0

	for _, site := range sites {
		// targets holds all resolved NodeIDs for this call site. Multiple targets
		// occur when RTA finds several instantiated classes with the same method.
		var targets []graph.NodeID

		if site.PkgAlias != "" {
			// Qualified call: pkg.Func() or var.Method()
			// The parser cannot distinguish these at AST time, so we try
			// the import-based resolution first and fall through to
			// method resolution if the alias is not an import.
			aliases, ok := importMap[site.CallerFile]
			if ok {
				if importPath, found := aliases[site.PkgAlias]; found {
					shortPkg := path.Base(importPath)
					targets = findInPackage(pkgIndex, shortPkg, site.FuncName, instantiated)
				}
			}

			// Second try: var type map — Python/Java obj.method() where obj has a
			// known declared type (e.g. repo: Repository = ... or Repository repo = ...).
			// Resolve the type name then look for TypeName.method across all packages.
			// This path is already precise (exact type known) — single target only.
			if len(targets) == 0 {
				varTypes := g.GetVarTypes(site.CallerFile)
				if typeName, hasType := varTypes[site.PkgAlias]; hasType {
					if id := findByTypedMethod(methodIndex, typeName, site.FuncName); id != "" {
						targets = []graph.NodeID{id}
					}
				}
			}

			// Fallback: alias was not an import or a typed var — treat as var.Method().
			// Search the caller's own package and all imported packages
			// for a method matching ".FuncName" (e.g. Graph.CarveEgoGraph).
			if len(targets) == 0 {
				callerNode := g.GetNode(site.CallerID)
				if callerNode != nil {
					targets = findInPackage(pkgIndex, callerNode.Package, site.FuncName, instantiated)
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
							if ids := findInPackage(pkgIndex, shortPkg, site.FuncName, instantiated); len(ids) > 0 {
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
			targets = findInPackage(pkgIndex, callerNode.Package, site.FuncName, instantiated)

			// 2. Fallback: search all packages imported by the caller's file.
			//    This handles Python/TypeScript `from X import Y` style calls where
			//    the symbol is imported directly (no qualifier) from another module.
			if len(targets) == 0 {
				if aliases, ok := importMap[site.CallerFile]; ok {
					sortedPaths := make([]string, 0, len(aliases))
					for _, p := range aliases {
						sortedPaths = append(sortedPaths, p)
					}
					sort.Strings(sortedPaths)
					for _, importPath := range sortedPaths {
						shortPkg := path.Base(importPath)
						if ids := findInPackage(pkgIndex, shortPkg, site.FuncName, instantiated); len(ids) > 0 {
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
func buildLookupTables(g *graph.Graph) (map[string]map[string]string, map[string][]*graph.Node) {
	importMap := make(map[string]map[string]string)
	pkgIndex := make(map[string][]*graph.Node)

	for _, n := range g.AllNodes() {
		switch n.Type {
		case graph.NodeFile:
			aliases := make(map[string]string)
			for _, e := range g.OutEdges(n.ID) {
				if e.Type != graph.EdgeImports {
					continue
				}
				pkgNode := g.GetNode(e.To)
				if pkgNode == nil || pkgNode.Type != graph.NodePackage {
					continue
				}
				alias := path.Base(pkgNode.Name)
				aliases[alias] = pkgNode.Name
			}
			importMap[n.File] = aliases
		case graph.NodeFunction, graph.NodeMethod:
			pkgIndex[n.Package] = append(pkgIndex[n.Package], n)
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

	return importMap, pkgIndex
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
func findInPackage(idx map[string][]*graph.Node, pkg, name string, instantiated map[string]bool) []graph.NodeID {
	suffix := "." + name
	candidates := idx[pkg]

	var instantiatedMatches []graph.NodeID
	var first graph.NodeID

	for _, n := range candidates {
		if n.Name != name && !strings.HasSuffix(n.Name, suffix) {
			continue
		}
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
				// Use first match only — functions are uniquely named within a package.
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
		// CHA fallback: no instantiated receiver found — return first match to
		// avoid losing the edge entirely (better a false positive than a false
		// negative for blast-radius analysis).
		if first != "" {
			return []graph.NodeID{first}
		}
		return nil
	}

	// CHA fast path: no instantiation data (pure Go project, etc.).
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
