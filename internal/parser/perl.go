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

		case "subroutine_declaration_statement":
			p.handleSubDecl(g, child, src, filePath, fileNodeID, currentPkg)

		case "expression_statement":
			// Capture our $VAR / my $VAR at file scope.
			p.handleExprStmt(g, child, src, filePath, fileNodeID)
		}
	}
	return nil
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
	"strict": true, "warnings": true, "utf8": true, "feature": true,
	"constant": true, "vars": true, "base": true, "parent": true,
	"Exporter": true, "overload": true, "POSIX": true,
}

func isPerlPragma(name string) bool {
	return perlPragmas[name]
}
