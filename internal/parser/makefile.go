package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	makeg "github.com/alexaandru/go-sitter-forest/make"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// MakefileParser parses Makefile source files.
// It extracts variable assignments, build targets (rules), define directives,
// and include directives from GNU Make and compatible Makefiles.
type MakefileParser struct {
	language *sitter.Language
}

// NewMakefileParser creates a ready-to-use MakefileParser.
func NewMakefileParser() *MakefileParser {
	return &MakefileParser{language: sitter.NewLanguage(makeg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *MakefileParser) Extensions() []string {
	return []string{".mk"}
}

// Filenames returns the exact base filenames handled by this parser.
// These are files that have no extension (e.g. "Makefile") or use
// standard naming conventions for GNU Make.
func (p *MakefileParser) Filenames() []string {
	return []string{"Makefile", "GNUmakefile", "makefile"}
}

// Parse extracts code entities from a single Makefile and merges them into
// the graph.
//
// Extracts:
//   - File node         -> NodeFile
//   - Variable assignments -> NodeVariable with kind="variable"
//   - Rules/targets     -> NodeFunction with kind="target"
//   - Define directives -> NodeFunction with kind="define"
//   - Include directives -> EdgeImports to NodePackage
func (p *MakefileParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	root := tree.RootNode()
	if root.IsNull() {
		return nil
	}

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lines := strings.Split(string(src), "\n")

	// Track .PHONY targets so we can mark them in metadata.
	phonyTargets := p.collectPhonyTargets(root, src)

	// Walk top-level children.
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "variable_assignment":
			p.handleVariable(g, child, src, filePath, fileNodeID, lines)

		case "rule":
			p.handleRule(g, child, src, filePath, fileNodeID, lines, phonyTargets)

		case "define_directive":
			p.handleDefine(g, child, src, filePath, fileNodeID, lines)

		case "include_directive":
			p.handleInclude(g, child, src, filePath, fileNodeID)
		}
	}

	return nil
}

// collectPhonyTargets scans the AST for .PHONY rules and returns a set of
// target names declared as phony.
func (p *MakefileParser) collectPhonyTargets(root sitter.Node, src []byte) map[string]bool {
	phony := make(map[string]bool)
	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() || child.Type() != "rule" {
			continue
		}

		// Check if this rule's target is .PHONY.
		targetsNode := firstChildOfType(child, "targets")
		if targetsNode.IsNull() {
			continue
		}

		isPhony := false
		for j := uint32(0); j < targetsNode.ChildCount(); j++ {
			tc := targetsNode.Child(j)
			if !tc.IsNull() && tc.Type() == "word" {
				name := strings.TrimSpace(childText(tc, src))
				if name == ".PHONY" {
					isPhony = true
					break
				}
			}
		}

		if !isPhony {
			continue
		}

		// Collect the prerequisites as phony target names.
		prereqsNode := firstChildOfType(child, "prerequisites")
		if prereqsNode.IsNull() {
			continue
		}
		for j := uint32(0); j < prereqsNode.ChildCount(); j++ {
			pc := prereqsNode.Child(j)
			if !pc.IsNull() && pc.Type() == "word" {
				name := strings.TrimSpace(childText(pc, src))
				if name != "" {
					phony[name] = true
				}
			}
		}
	}
	return phony
}

// handleVariable processes a variable_assignment node, creating a NodeVariable.
func (p *MakefileParser) handleVariable(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	// Extract the variable name from the first "word" child.
	nameNode := firstChildOfType(n, "word")
	if nameNode.IsNull() {
		return
	}
	name := strings.TrimSpace(childText(nameNode, src))
	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	meta := map[string]string{"kind": "variable"}
	if doc != "" {
		meta["doc"] = doc
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeVariable,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleRule processes a rule node, creating a NodeFunction for each target.
// Skips .PHONY rules entirely and other special targets starting with ".".
func (p *MakefileParser) handleRule(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
	phonyTargets map[string]bool,
) {
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	targetsNode := firstChildOfType(n, "targets")
	if targetsNode.IsNull() {
		return
	}

	// Extract prerequisite names for metadata.
	var prereqs []string
	prereqsNode := firstChildOfType(n, "prerequisites")
	if !prereqsNode.IsNull() {
		for j := uint32(0); j < prereqsNode.ChildCount(); j++ {
			pc := prereqsNode.Child(j)
			if !pc.IsNull() && pc.Type() == "word" {
				prereqName := strings.TrimSpace(childText(pc, src))
				if prereqName != "" {
					prereqs = append(prereqs, prereqName)
				}
			}
		}
	}

	for j := uint32(0); j < targetsNode.ChildCount(); j++ {
		tc := targetsNode.Child(j)
		if tc.IsNull() || tc.Type() != "word" {
			continue
		}
		name := strings.TrimSpace(childText(tc, src))
		if name == "" {
			continue
		}

		// Skip .PHONY entirely — it's a directive, not a real target.
		if name == ".PHONY" {
			return
		}

		// Skip internal/special targets starting with "." (e.g. .DEFAULT, .SUFFIXES).
		if strings.HasPrefix(name, ".") {
			continue
		}

		nodeID := g.MakeNodeID(filePath, name)
		if g.GetNode(nodeID) != nil {
			continue
		}

		meta := map[string]string{"kind": "target"}
		if doc != "" {
			meta["doc"] = doc
		}
		if phonyTargets[name] {
			meta["phony"] = "true"
		}
		if len(prereqs) > 0 {
			meta["prerequisites"] = strings.Join(prereqs, ", ")
		}

		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeFunction,
			Name:     name,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// handleDefine processes a define_directive node, creating a NodeFunction.
func (p *MakefileParser) handleDefine(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	lines []string,
) {
	startLine := int(n.StartPoint().Row) + 1
	doc := extractLineDoc(lines, startLine, "#")

	// In the make grammar, define_directive has children:
	// "define" keyword, then the name as a "word" child.
	// Find the "word" child that appears after the "define" keyword.
	var name string
	seenDefine := false
	for j := uint32(0); j < n.ChildCount(); j++ {
		child := n.Child(j)
		if child.IsNull() {
			continue
		}
		text := strings.TrimSpace(childText(child, src))
		if text == "define" {
			seenDefine = true
			continue
		}
		if seenDefine && child.Type() == "word" {
			name = text
			break
		}
	}

	if name == "" {
		return
	}

	nodeID := g.MakeNodeID(filePath, name)
	if g.GetNode(nodeID) != nil {
		return
	}

	meta := map[string]string{"kind": "define"}
	if doc != "" {
		meta["doc"] = doc
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: true,
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// handleInclude processes an include_directive node, creating an import edge
// for the included file.
func (p *MakefileParser) handleInclude(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// Extract the full text and parse out the included path(s).
	// include_directive text looks like: "include foo.mk bar.mk" or "-include optional.mk"
	text := strings.TrimSpace(childText(n, src))

	// Strip the include keyword prefix.
	for _, prefix := range []string{"-include", "sinclude", "include"} {
		if strings.HasPrefix(text, prefix) {
			text = strings.TrimSpace(strings.TrimPrefix(text, prefix))
			break
		}
	}

	if text == "" {
		return
	}

	// Handle multiple included files separated by whitespace.
	paths := strings.Fields(text)
	for _, importPath := range paths {
		importPath = strings.TrimSpace(importPath)
		if importPath == "" {
			continue
		}

		importNodeID := g.MakeNodeID(importPath, importPath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    filepath.Base(importPath),
			Package: importPath,
			File:    filePath,
			Line:    int(n.StartPoint().Row) + 1,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}
}
