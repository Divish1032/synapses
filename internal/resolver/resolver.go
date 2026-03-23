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
	// nil means no instantiation data (e.g. pure Go project) — tie-breaking skipped.
	instantiated := g.GetInstantiatedTypes()

	// Track edges added in this batch. Existing edges checked via g.HasEdge.
	type edgeKey struct{ from, to graph.NodeID }
	seen := make(map[edgeKey]bool)
	resolved := 0

	for _, site := range sites {
		var targetID graph.NodeID

		if site.PkgAlias != "" {
			// Qualified call: pkg.Func() or var.Method()
			// The parser cannot distinguish these at AST time, so we try
			// the import-based resolution first and fall through to
			// method resolution if the alias is not an import.
			aliases, ok := importMap[site.CallerFile]
			if ok {
				if importPath, found := aliases[site.PkgAlias]; found {
					shortPkg := path.Base(importPath)
					targetID = findInPackage(pkgIndex, shortPkg, site.FuncName, instantiated)
				}
			}

			// Second try: var type map — Python/Java obj.method() where obj has a
			// known declared type (e.g. repo: Repository = ... or Repository repo = ...).
			// Resolve the type name then look for TypeName.method across all packages.
			if targetID == "" {
				varTypes := g.GetVarTypes(site.CallerFile)
				if typeName, hasType := varTypes[site.PkgAlias]; hasType {
					targetID = findByTypedMethod(methodIndex, typeName, site.FuncName)
				}
			}

			// Fallback: alias was not an import or a typed var — treat as var.Method().
			// Search the caller's own package and all imported packages
			// for a method matching ".FuncName" (e.g. Graph.CarveEgoGraph).
			if targetID == "" {
				callerNode := g.GetNode(site.CallerID)
				if callerNode != nil {
					targetID = findInPackage(pkgIndex, callerNode.Package, site.FuncName, instantiated)
				}
				if targetID == "" {
					if aliases, ok := importMap[site.CallerFile]; ok {
						sortedPaths := make([]string, 0, len(aliases))
						for _, p := range aliases {
							sortedPaths = append(sortedPaths, p)
						}
						sort.Strings(sortedPaths)
						for _, importPath := range sortedPaths {
							shortPkg := path.Base(importPath)
							if id := findInPackage(pkgIndex, shortPkg, site.FuncName, instantiated); id != "" {
								targetID = id
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
			targetID = findInPackage(pkgIndex, callerNode.Package, site.FuncName, instantiated)

			// 2. Fallback: search all packages imported by the caller's file.
			//    This handles Python/TypeScript `from X import Y` style calls where
			//    the symbol is imported directly (no qualifier) from another module.
			if targetID == "" {
				if aliases, ok := importMap[site.CallerFile]; ok {
					sortedPaths := make([]string, 0, len(aliases))
					for _, p := range aliases {
						sortedPaths = append(sortedPaths, p)
					}
					sort.Strings(sortedPaths)
					for _, importPath := range sortedPaths {
						shortPkg := path.Base(importPath)
						if id := findInPackage(pkgIndex, shortPkg, site.FuncName, instantiated); id != "" {
							targetID = id
							break
						}
					}
				}
			}
		}

		if targetID == "" {
			continue
		}

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

	return resolved
}

// buildLookupTables builds the import map and package index in a single
// AllNodes() pass, avoiding redundant full-graph scans. Previously these were
// two separate functions (buildImportMap + buildPackageIndex) with two passes.
//
// importMap: absFilePath → {packageAlias → importPath}
// pkgIndex:  shortPkgName → []*Node (functions and methods)
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
	return importMap, pkgIndex
}

// findInPackage returns the NodeID of a function or method named `name` in
// package `pkg`, or "" if not found.
// Methods are stored as "ReceiverType.MethodName", so we match on the suffix.
//
// RTA tie-breaking: when multiple candidates match and instantiated is non-nil,
// prefer candidates whose receiver type appears in the instantiated set. This
// reduces false-positive CALLS edges in codebases with deep class hierarchies
// (Java/TypeScript) where many classes share the same method name. Falls back
// to the first candidate if no instantiated match exists (no regressions).
func findInPackage(idx map[string][]*graph.Node, pkg, name string, instantiated map[string]bool) graph.NodeID {
	suffix := "." + name
	candidates := idx[pkg]

	if len(instantiated) == 0 {
		// Fast path: no RTA data, original CHA behavior.
		for _, n := range candidates {
			if n.Name == name || strings.HasSuffix(n.Name, suffix) {
				return n.ID
			}
		}
		return ""
	}

	// RTA path: prefer candidates whose receiver type is in the instantiated set.
	var first graph.NodeID // first match (fallback if no instantiated candidate)
	for _, n := range candidates {
		if n.Name != name && !strings.HasSuffix(n.Name, suffix) {
			continue
		}
		if first == "" {
			first = n.ID
		}
		// For method nodes ("ReceiverType.MethodName"), check if the receiver type
		// is in the instantiated set.
		dot := strings.IndexByte(n.Name, '.')
		if dot > 0 && instantiated[n.Name[:dot]] {
			return n.ID // prefer instantiated receiver
		}
	}
	return first // fallback: first match (CHA behavior when no RTA preference found)
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
