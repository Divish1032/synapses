// Package resolver performs a post-parse cross-file CALLS edge resolution pass.
//
// The Go parser (and other language parsers) collect raw call sites during
// AST traversal but cannot resolve cross-file targets at that time because not
// all nodes exist yet. This package drains those call sites after all files are
// parsed and links them to their target nodes via CALLS edges.
package resolver

import (
	"path"
	"strings"

	"github.com/Divish1032/synapses/internal/graph"
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

	// Build lookup tables once.
	importMap := buildImportMap(g)   // absFilePath → {alias → importPath}
	pkgIndex := buildPackageIndex(g) // shortPkgName → []*Node

	type edgeKey struct{ from, to graph.NodeID }
	seen := make(map[edgeKey]bool)

	// Pre-populate seen from existing CALLS edges so that a second call to
	// ResolveCallEdges (e.g. after MergeFrom for cross-project resolution)
	// never creates duplicate edges in the in-memory graph.
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls {
			seen[edgeKey{e.From, e.To}] = true
		}
	}
	resolved := 0

	for _, site := range sites {
		var targetID graph.NodeID

		if site.PkgAlias != "" {
			// Qualified call: pkg.Func()
			// Only resolve if the alias matches a known import (not a variable).
			aliases, ok := importMap[site.CallerFile]
			if !ok {
				continue
			}
			importPath, ok := aliases[site.PkgAlias]
			if !ok {
				continue // operand is a local variable, not a package — skip
			}
			shortPkg := path.Base(importPath)
			targetID = findInPackage(pkgIndex, shortPkg, site.FuncName)
		} else {
			// Direct call: Func()
			// Look in the caller's own package.
			callerNode := g.GetNode(site.CallerID)
			if callerNode == nil {
				continue
			}
			targetID = findInPackage(pkgIndex, callerNode.Package, site.FuncName)
		}

		if targetID == "" {
			continue
		}

		key := edgeKey{site.CallerID, targetID}
		if seen[key] {
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

// buildImportMap builds a map of absFilePath → {packageAlias → importPath}
// by following IMPORTS edges from every NodeFile node.
// The alias defaults to the last path segment of the import path.
func buildImportMap(g *graph.Graph) map[string]map[string]string {
	result := make(map[string]map[string]string)
	for _, n := range g.AllNodes() {
		if n.Type != graph.NodeFile {
			continue
		}
		aliases := make(map[string]string)
		for _, e := range g.OutEdges(n.ID) {
			if e.Type != graph.EdgeImports {
				continue
			}
			pkgNode := g.GetNode(e.To)
			if pkgNode == nil || pkgNode.Type != graph.NodePackage {
				continue
			}
			// Use last path segment as the alias (matches Go's default convention).
			alias := path.Base(pkgNode.Name)
			aliases[alias] = pkgNode.Name
		}
		result[n.File] = aliases
	}
	return result
}

// buildPackageIndex groups NodeFunction and NodeMethod nodes by their short
// package name (node.Package), enabling fast lookup by (pkg, name).
func buildPackageIndex(g *graph.Graph) map[string][]*graph.Node {
	result := make(map[string][]*graph.Node)
	for _, n := range g.AllNodes() {
		switch n.Type {
		case graph.NodeFunction, graph.NodeMethod:
			result[n.Package] = append(result[n.Package], n)
		}
	}
	return result
}

// findInPackage returns the NodeID of a function or method named `name` in
// package `pkg`, or "" if not found.
// Methods are stored as "ReceiverType.MethodName", so we match on the suffix.
func findInPackage(idx map[string][]*graph.Node, pkg, name string) graph.NodeID {
	suffix := "." + name
	for _, n := range idx[pkg] {
		if n.Name == name || strings.HasSuffix(n.Name, suffix) {
			return n.ID
		}
	}
	return ""
}
