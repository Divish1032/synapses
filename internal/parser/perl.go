package parser

import (
	"path/filepath"
	"strings"

	perlg "github.com/alexaandru/go-sitter-forest/perl"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// PerlParser parses Perl source files (.pl, .pm, .t).
//
// Extracts:
//   - package declarations → NodePackage
//   - subroutine declarations → NodeFunction (exported if name doesn't start with _)
//   - our/my variable declarations at file scope → NodeVariable
//   - use statements (imports) → NodePackage edges
type PerlParser struct{}

// NewPerlParser returns a new Perl parser.
func NewPerlParser() *PerlParser { return &PerlParser{} }

// Language returns the language name.
func (p *PerlParser) Language() string { return "perl" }

// Extensions returns the file extensions handled by this parser.
func (p *PerlParser) Extensions() []string { return []string{".pl", ".pm", ".t"} }

// TSLanguageForFile returns the tree-sitter language for this parser.
func (p *PerlParser) TSLanguageForFile(_ string) *sitter.Language {
	return sitter.NewLanguage(perlg.GetLanguage())
}

// Parse parses a Perl file into the graph.
func (p *PerlParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(sitter.NewLanguage(perlg.GetLanguage()))
	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, err := parser.ParseString(parseCtx, nil, src)
	if err != nil || tree == nil {
		return err
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.IsNull() {
		return nil
	}

	fileName := filepath.Base(filePath)
	fileNodeID := g.MakeNodeID(filePath, fileName)
	if g.GetNode(fileNodeID) == nil {
		g.AddNode(&graph.Node{
			ID:   fileNodeID,
			Type: graph.NodeFile,
			Name: fileName,
			File: filePath,
			Line: 1,
		})
	}

	// Track current package context for method attribution.
	currentPkg := ""

	// Pass 1 (top-level): extract package declarations, use/import statements,
	// and top-level variable declarations. These never appear nested.
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		switch nodeType(child) {
		case "package_statement":
			currentPkg = perlExtractPackageName(child, src)
			if currentPkg == "" {
				continue
			}
			startLine := int(child.StartPoint().Row) + 1
			pkgID := g.MakeNodeID(filePath, "pkg_"+currentPkg)
			if g.GetNode(pkgID) == nil {
				g.AddNode(&graph.Node{
					ID:       pkgID,
					Type:     graph.NodePackage,
					Name:     currentPkg,
					File:     filePath,
					Line:     startLine,
					Exported: true,
					Metadata: map[string]string{"kind": "package"},
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: pkgID, Type: graph.EdgeDefines})
			}

		case "use_statement":
			importPkg := perlExtractUseName(child, src)
			if importPkg == "" || isPerlPragma(importPkg) {
				continue
			}
			startLine := int(child.StartPoint().Row) + 1
			importID := g.MakeNodeID(filePath, "use_"+importPkg)
			if g.GetNode(importID) == nil {
				g.AddNode(&graph.Node{
					ID:       importID,
					Type:     graph.NodePackage,
					Name:     importPkg,
					File:     filePath,
					Line:     startLine,
					Exported: false,
					Metadata: map[string]string{"kind": "import"},
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: importID, Type: graph.EdgeImports})
			}

		case "expression_statement":
			// Capture our $VAR / my $VAR at file scope.
			p.handleExprStmt(g, child, src, filePath, fileNodeID)
		}
	}

	// Pass 2 (recursive): extract subroutine declarations at any depth.
	// The tree-sitter Perl grammar sometimes absorbs multiple subs into one
	// large subroutine_declaration_statement when a forward declaration
	// (sub foo;) causes a parse error — walking recursively recovers all subs.
	p.walkSubDecls(g, root, src, filePath, fileNodeID, &currentPkg)
	// Sprint 28: collect call sites from function/method calls.
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		FuncTypes: map[string]bool{
			"subroutine_declaration_statement": true,
		},
		CallTypes: map[string]bool{
			"function_call_expression":           true,
			"ambiguous_function_call_expression": true,
			"method_call_expression":             true,
		},
		NameExtractor: func(n sitter.Node, src []byte) string {
			// Extract sub name from bareword child.
			// Return "sub_"+qualifiedName to match the node ID key used during creation.
			for i := uint32(0); i < n.ChildCount(); i++ {
				child := n.Child(i)
				if !child.IsNull() && nodeType(child) == "bareword" {
					name := childText(child, src)
					if currentPkg != "" {
						return "sub_" + currentPkg + "::" + name
					}
					return "sub_" + name
				}
			}
			return ""
		},
		AliasedCalleeExtractor: func(n sitter.Node, src []byte) (string, string) {
			nt := nodeType(n)
			switch nt {
			case "function_call_expression", "ambiguous_function_call_expression":
				// function_call_expression / ambiguous_function_call_expression:
				// both have a "function" child with the callee name.
				for i := uint32(0); i < n.ChildCount(); i++ {
					child := n.Child(i)
					if !child.IsNull() && nodeType(child) == "function" {
						name := childText(child, src)
						// Qualified: Foo::helper → alias="Foo", callee="helper"
						if idx := strings.LastIndex(name, "::"); idx >= 0 {
							return name[:idx], name[idx+2:]
						}
						return "", name
					}
				}
			case "method_call_expression":
				// method_call_expression: receiver -> method(args)
				var method string
				for i := uint32(0); i < n.ChildCount(); i++ {
					child := n.Child(i)
					if !child.IsNull() && nodeType(child) == "method" {
						method = childText(child, src)
					}
				}
				if method != "" {
					return "self", method // treat as self-call for resolver
				}
			}
			return "", ""
		},
		IsBuiltin: isPerlBuiltin,
	})

	return nil
}

// isPerlBuiltin returns true for common Perl built-in functions.
func isPerlBuiltin(name string) bool {
	switch name {
	case "print", "say", "warn", "die", "croak", "confess",
		"push", "pop", "shift", "unshift", "splice",
		"chomp", "chop", "length", "substr", "index", "join", "split",
		"open", "close", "read", "write", "seek", "tell",
		"defined", "exists", "delete", "ref", "bless",
		"keys", "values", "each", "sort", "reverse", "map", "grep",
		"require", "eval", "local", "return", "my", "our",
		"chmod", "chown", "mkdir", "rmdir", "rename", "unlink",
		"scalar", "wantarray":
		return true
	}
	return false
}

// walkSubDecls recursively walks the AST and emits NodeFunction for every
// subroutine_declaration_statement found at any depth. This is necessary
// because the tree-sitter Perl grammar creates parse ERRORs for forward
// declarations (sub foo;), which causes subsequent subs to be absorbed into
// one large node. Walking recursively recovers all sub definitions.
//
// pkg is a pointer so that package_statement nodes encountered during the
// walk can update the current package context (needed for multi-package files).
func (p *PerlParser) walkSubDecls(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	pkg *string,
) {
	if n.IsNull() {
		return
	}
	switch nodeType(n) {
	case "package_statement":
		// Update current package context as we encounter new package declarations.
		if name := perlExtractPackageName(n, src); name != "" {
			*pkg = name
		}
		return
	case "subroutine_declaration_statement":
		p.handleSubDecl(g, n, src, filePath, fileNodeID, *pkg)
		// Recurse into the block child only: when the grammar absorbs multiple
		// subs into one node due to forward-declaration parse errors, the real
		// sub bodies live inside the block. We use the same pkg context.
		for i := uint32(0); i < n.ChildCount(); i++ {
			child := n.Child(i)
			if !child.IsNull() && nodeType(child) == "block" {
				p.walkSubDecls(g, child, src, filePath, fileNodeID, pkg)
			}
		}
		return
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		p.walkSubDecls(g, n.Child(i), src, filePath, fileNodeID, pkg)
	}
}

// handleSubDecl emits a NodeFunction for a sub declaration.
func (p *PerlParser) handleSubDecl(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	pkg string,
) {
	// sub NAME BLOCK  or  sub NAME SIGNATURE BLOCK
	name := ""
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && nodeType(child) == "bareword" {
			name = childText(child, src)
			break
		}
	}
	if name == "" {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	exported := !strings.HasPrefix(name, "_")

	// Build fully qualified name when we have a package context.
	displayName := name
	if pkg != "" {
		displayName = pkg + "::" + name
	}

	nodeID := g.MakeNodeID(filePath, "sub_"+displayName)
	if g.GetNode(nodeID) != nil {
		return
	}
	meta := map[string]string{"kind": "subroutine"}
	if pkg != "" {
		meta["package"] = pkg
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     displayName,
		Package:  pkg,
		File:     filePath,
		Line:     startLine,
		Exported: exported,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleExprStmt captures top-level our/my variable declarations.
func (p *PerlParser) handleExprStmt(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// expression_statement → assignment_expression → variable_declaration
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || nodeType(child) != "assignment_expression" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			vd := child.Child(j)
			if vd.IsNull() || nodeType(vd) != "variable_declaration" {
				continue
			}
			scope := ""
			for k := uint32(0); k < vd.ChildCount(); k++ {
				kw := vd.Child(k)
				if kw.IsNull() {
					continue
				}
				if nodeType(kw) == "our" || nodeType(kw) == "my" {
					scope = nodeType(kw)
				}
			}
			if scope == "" {
				continue
			}
			varName := perlExtractVarName(vd, src)
			if varName == "" {
				continue
			}
			startLine := int(n.StartPoint().Row) + 1
			nodeID := g.MakeNodeID(filePath, "var_"+varName)
			if g.GetNode(nodeID) != nil {
				continue
			}
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     varName,
				File:     filePath,
				Line:     startLine,
				Exported: scope == "our",
				Metadata: map[string]string{"kind": scope},
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		}
	}
}

// perlExtractPackageName extracts the package name from a package_statement node.
func perlExtractPackageName(n sitter.Node, src []byte) string {
	// package_statement: package keyword + package name + ;
	count := 0
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if nodeType(child) == "package" {
			count++
			if count == 2 { // first is keyword, second is name
				return childText(child, src)
			}
		}
	}
	return ""
}

// perlExtractUseName extracts the module name from a use_statement node.
func perlExtractUseName(n sitter.Node, src []byte) string {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && nodeType(child) == "package" {
			return childText(child, src)
		}
	}
	return ""
}

// perlExtractVarName extracts the variable name (without sigil) from a variable_declaration.
func perlExtractVarName(vd sitter.Node, src []byte) string {
	for i := uint32(0); i < vd.ChildCount(); i++ {
		child := vd.Child(i)
		if child.IsNull() {
			continue
		}
		typ := nodeType(child)
		if typ == "scalar" || typ == "array" || typ == "hash" {
			// Look for varname child.
			for j := uint32(0); j < child.ChildCount(); j++ {
				vn := child.Child(j)
				if !vn.IsNull() && nodeType(vn) == "varname" {
					return childText(vn, src)
				}
			}
		}
	}
	return ""
}

// isPerlPragma returns true for common Perl pragmas that aren't real modules.
var perlPragmas = map[string]bool{
	"utf8": true, "feature": true,
	"constant": true, "vars": true, "base": true, "parent": true,
	"overload": true, "POSIX": true,
}

func isPerlPragma(name string) bool {
	return perlPragmas[name]
}
