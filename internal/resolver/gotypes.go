package resolver

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/logutil"
)

// loadGoPackagesTimeout is the maximum time allowed for packages.Load.
// gin and similar repos with many transitive deps can hang indefinitely
// if modules are not cached; 30 s is generous for cached local repos.
const loadGoPackagesTimeout = 30 * time.Second

// loadGoPackages loads all packages under root with full type information.
// Shared by ResolveGoTypesCallEdges and ResolveGoTypesImplementsEdges so the
// expensive packages.Load call only happens once when both are needed.
//
// GOPROXY=off prevents go list from downloading missing modules — it fails
// fast instead of hanging for minutes waiting for network. Repos with all
// deps cached locally still work fine; only uncached deps return errors
// which are already handled as non-fatal package errors below.
func loadGoPackages(root string) (*token.FileSet, []*packages.Package, error) {
	fset := token.NewFileSet()
	ctx, cancel := context.WithTimeout(context.Background(), loadGoPackagesTimeout)
	defer cancel()

	// Inherit current env but override GOPROXY to prevent network downloads.
	// Filter out any existing GOPROXY/GONOSUMCHECK so our values win.
	raw := os.Environ()
	env := make([]string, 0, len(raw)+2)
	for _, e := range raw {
		if !strings.HasPrefix(e, "GOPROXY=") && !strings.HasPrefix(e, "GONOSUMCHECK=") {
			env = append(env, e)
		}
	}
	env = append(env, "GOPROXY=off", "GONOSUMCHECK=*")

	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo,
		Dir:     root,
		Fset:    fset,
		Context: ctx,
		Env:     env,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, nil, fmt.Errorf("go/packages load: %w", err)
	}
	return fset, pkgs, nil
}

// ResolveGoTypesCallEdges performs a type-checked CALLS resolution pass for
// Go files using golang.org/x/tools/go/packages. It supplements the
// tree-sitter resolver with cross-package, interface-dispatch, and
// closure-aware edges that structural analysis cannot see.
//
// Returns the number of new CALLS edges added. Package-level type errors are
// logged to stderr but do not abort the run — partial results are returned.
// Returns an error only if packages.Load itself fails (e.g. no go.mod found).
func ResolveGoTypesCallEdges(g *graph.Graph, root string) (int, error) {
	fset, pkgs, err := loadGoPackages(root)
	if err != nil {
		return 0, err
	}
	return resolveGoTypesCalls(g, fset, pkgs)
}

// ResolveGoTypesImplementsEdges uses go/types structural matching to detect
// which named types implement which interfaces, including cross-package pairs
// (e.g. http.Handler, io.Reader, error) that the same-package heuristic in
// ResolveImplementsEdges cannot see.
//
// Both T and *T are checked against every interface so pointer-receiver
// implementations (the dominant Go pattern) are captured correctly.
//
// Only interfaces and structs already in the graph are matched — stdlib types
// that were never indexed are naturally excluded, preventing edge explosion.
//
// Returns the number of new IMPLEMENTS edges added.
func ResolveGoTypesImplementsEdges(g *graph.Graph, root string) (int, error) {
	fset, pkgs, err := loadGoPackages(root)
	if err != nil {
		return 0, err
	}
	return resolveGoTypesImplements(g, fset, pkgs)
}

// ResolveGoTypesBoth performs a single packages.Load and then runs both the
// CALLS and IMPLEMENTS resolvers. Use this instead of calling the two
// individual functions separately to avoid loading packages twice.
//
// Returns (callsAdded, implementsAdded, error).
func ResolveGoTypesBoth(g *graph.Graph, root string) (int, int, error) {
	fset, pkgs, err := loadGoPackages(root)
	if err != nil {
		return 0, 0, err
	}
	calls, err := resolveGoTypesCalls(g, fset, pkgs)
	if err != nil {
		return calls, 0, err
	}
	impls, err := resolveGoTypesImplements(g, fset, pkgs)
	return calls, impls, err
}

// resolveGoTypesCalls is the inner implementation used by both
// ResolveGoTypesCallEdges and ResolveGoTypesBoth.
func resolveGoTypesCalls(g *graph.Graph, fset *token.FileSet, pkgs []*packages.Package) (int, error) {
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

	type edgeKey struct{ from, to graph.NodeID }
	seen := make(map[edgeKey]bool)
	added := 0

	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil || pkg.Syntax == nil {
			continue
		}
		for _, pe := range pkg.Errors {
			logutil.Warn("synapses/gotypes: %s: %v\n", pkg.PkgPath, pe)
		}
		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				callerPos := fset.Position(fd.Pos())
				callerID := posToNode[posKey{callerPos.Filename, callerPos.Line}]
				if callerID == "" {
					continue
				}
				ast.Inspect(fd.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
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
						return true
					}
					calleeID := posToNode[posKey{calleePos.Filename, calleePos.Line}]
					if calleeID == "" {
						return true
					}
					key := edgeKey{callerID, calleeID}
					if !seen[key] && !g.HasEdge(callerID, calleeID, graph.EdgeCalls) {
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

// resolveGoTypesImplements is the inner implementation used by both
// ResolveGoTypesImplementsEdges and ResolveGoTypesBoth.
func resolveGoTypesImplements(g *graph.Graph, fset *token.FileSet, pkgs []*packages.Package) (int, error) {
	// Build position-keyed lookup for structs and name-keyed lookup for interfaces.
	type posKey struct {
		file string
		line int
	}
	posToStruct := make(map[posKey]graph.NodeID)
	// Interfaces keyed by both "pkgpath.Name" and short "Name" for flexibility.
	ifaceByQual := make(map[string]graph.NodeID)
	ifaceByName := make(map[string]graph.NodeID)

	for _, n := range g.AllNodes() {
		switch n.Type {
		case graph.NodeStruct:
			posToStruct[posKey{n.File, n.Line}] = n.ID
		case graph.NodeInterface:
			// Short name (may collide across packages — used as fallback only).
			ifaceByName[n.Name] = n.ID
			// Qualified name built from Package path + Name avoids collisions.
			if n.Package != "" {
				ifaceByQual[n.Package+"."+n.Name] = n.ID
			}
		}
	}

	// Collect all interface types found in the loaded packages.
	type ifaceEntry struct {
		typ  *types.Interface
		id   graph.NodeID
		name string
	}
	var ifaces []ifaceEntry

	for _, pkg := range pkgs {
		if pkg.Types == nil {
			continue
		}
		scope := pkg.Types.Scope()
		for _, objName := range scope.Names() {
			obj := scope.Lookup(objName)
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			iface, ok := tn.Type().Underlying().(*types.Interface)
			if !ok || iface.NumMethods() == 0 {
				// Skip empty interfaces (interface{} / any) — every type satisfies
				// them and adding those edges would flood the graph with noise.
				continue
			}
			// Match to graph node: try qualified key first, then short name.
			qual := pkg.PkgPath + "." + objName
			id, found := ifaceByQual[qual]
			if !found {
				id, found = ifaceByName[objName]
			}
			if !found {
				continue // interface not in our graph (stdlib, vendor, etc.)
			}
			ifaces = append(ifaces, ifaceEntry{typ: iface, id: id, name: objName})
		}
	}

	if len(ifaces) == 0 {
		return 0, nil
	}

	// For each named struct type, check T and *T against all collected interfaces.
	seen := make(map[string]bool)
	added := 0

	for _, pkg := range pkgs {
		if pkg.Types == nil {
			continue
		}
		scope := pkg.Types.Scope()
		for _, objName := range scope.Names() {
			obj := scope.Lookup(objName)
			tn, ok := obj.(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			// Only check concrete types (not interfaces themselves).
			if _, isIface := named.Underlying().(*types.Interface); isIface {
				continue
			}

			// Find the graph node by source position.
			pos := fset.Position(tn.Pos())
			if !pos.IsValid() {
				continue
			}
			structID := posToStruct[posKey{pos.Filename, pos.Line}]
			if structID == "" {
				continue
			}

			ptrType := types.NewPointer(named)

			for _, iface := range ifaces {
				// Check T implements iface or *T implements iface.
				if !types.Implements(named, iface.typ) && !types.Implements(ptrType, iface.typ) {
					continue
				}
				edgeKey := string(structID) + "->" + string(iface.id)
				if seen[edgeKey] || g.HasEdge(structID, iface.id, graph.EdgeImplements) {
					continue
				}
				seen[edgeKey] = true
				g.AddEdge(&graph.Edge{
					From: structID,
					To:   iface.id,
					Type: graph.EdgeImplements,
				})
				added++
			}
		}
	}
	return added, nil
}

// goTypesPackageName returns the short package name from a PkgPath, e.g.
// "github.com/foo/bar/v2" → "bar". Used for display/logging only.
func goTypesPackageName(pkgPath string) string {
	if idx := strings.LastIndexByte(pkgPath, '/'); idx >= 0 {
		return pkgPath[idx+1:]
	}
	return pkgPath
}
