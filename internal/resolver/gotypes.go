package resolver

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"

	"golang.org/x/tools/go/packages"

	"github.com/synapses/synapses/internal/graph"
)

// ResolveGoTypesCallEdges performs a type-checked CALLS resolution pass for
// Go files using golang.org/x/tools/go/packages. It supplements the
// tree-sitter resolver with cross-package, interface-dispatch, and
// closure-aware edges that structural analysis cannot see.
//
// Returns the number of new CALLS edges added. Package-level type errors are
// logged to stderr but do not abort the run — partial results are returned.
// Returns an error only if packages.Load itself fails (e.g. no go.mod found).
func ResolveGoTypesCallEdges(g *graph.Graph, root string) (int, error) {
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo,
		Dir:  root,
		Fset: fset,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return 0, fmt.Errorf("go/packages load: %w", err)
	}

	// Build a (absFilePath, 1-based line) → NodeID lookup from all
	// function and method nodes already in the graph.
	type posKey struct {
		file string
		line int
	}
	posToNode := make(map[posKey]graph.NodeID, g.NodeCount())
	for _, n := range g.AllNodes() {
		switch n.Type {
		case graph.NodeFunction, graph.NodeMethod:
			posToNode[posKey{n.File, n.Line}] = n.ID
		}
	}

	// Pre-populate dedup set from existing CALLS edges so we never create
	// duplicates of what the tree-sitter resolver already emitted.
	type edgeKey struct{ from, to graph.NodeID }
	seen := make(map[edgeKey]bool, g.EdgeCount())
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeCalls {
			seen[edgeKey{e.From, e.To}] = true
		}
	}

	added := 0
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil || pkg.Syntax == nil {
			continue
		}
		// Log type errors per package but keep going — partial type info
		// still yields better coverage than tree-sitter alone.
		for _, pe := range pkg.Errors {
			fmt.Fprintf(os.Stderr, "synapses/gotypes: %s: %v\n", pkg.PkgPath, pe)
		}

		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}

				// Find the graph node for the enclosing function/method.
				callerPos := fset.Position(fd.Pos())
				callerID := posToNode[posKey{callerPos.Filename, callerPos.Line}]
				if callerID == "" {
					continue // not indexed (e.g. vendor, generated)
				}

				// Walk the body for every call expression.
				ast.Inspect(fd.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}

					// Extract the identifier that names the callee.
					var ident *ast.Ident
					switch callFun := call.Fun.(type) {
					case *ast.Ident:
						ident = callFun
					case *ast.SelectorExpr:
						ident = callFun.Sel
					}
					if ident == nil {
						return true
					}

					// Resolve via type info — this is where go/types shines:
					// it resolves across packages, through interfaces, etc.
					obj, ok := pkg.TypesInfo.Uses[ident]
					if !ok {
						return true
					}
					calleeFn, ok := obj.(*types.Func)
					if !ok {
						return true
					}

					calleePos := fset.Position(calleeFn.Pos())
					if !calleePos.IsValid() {
						return true // built-in or synthetic function
					}
					calleeID := posToNode[posKey{calleePos.Filename, calleePos.Line}]
					if calleeID == "" {
						return true // callee not in this project's graph
					}

					key := edgeKey{callerID, calleeID}
					if !seen[key] {
						seen[key] = true
						g.AddEdge(&graph.Edge{
							From: callerID,
							To:   calleeID,
							Type: graph.EdgeCalls,
						})
						added++
					}
					return true
				})
			}
		}
	}
	return added, nil
}
