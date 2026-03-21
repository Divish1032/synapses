package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	matlabg "github.com/alexaandru/go-sitter-forest/matlab"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// MATLABParser parses MATLAB/Octave source files (.m).
//
// Extracts:
//   - classdef declarations → NodeStruct (with superclass edges)
//   - properties blocks → NodeVariable per property
//   - methods blocks → NodeFunction/NodeMethod per function_definition
//   - top-level function_definition → NodeFunction
type MATLABParser struct{}

func NewMATLABParser() *MATLABParser { return &MATLABParser{} }

func (p *MATLABParser) Language() string       { return "matlab" }
func (p *MATLABParser) Extensions() []string { return []string{".m"} }

func (p *MATLABParser) TSLanguageForFile(_ string) *sitter.Language {
	return sitter.NewLanguage(matlabg.GetLanguage())
}

func (p *MATLABParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(sitter.NewLanguage(matlabg.GetLanguage()))
	tree, err := parser.ParseString(context.Background(), nil, src)
	if err != nil || tree == nil {
		return err
	}
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

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "class_definition":
			p.handleClass(g, child, src, filePath, fileNodeID)
		case "function_definition":
			p.handleTopLevelFunc(g, child, src, filePath, fileNodeID, "")
		}
	}
	return nil
}

// handleClass processes a classdef block.
func (p *MATLABParser) handleClass(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	className := ""
	var superclasses []string
	startLine := int(n.StartPoint().Row) + 1

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "identifier":
			if className == "" {
				className = childText(child, src)
			}
		case "superclasses":
			// superclasses: < identifier (& identifier)*
			for j := uint32(0); j < child.ChildCount(); j++ {
				sc := child.Child(j)
				if !sc.IsNull() && sc.Type() == "property_name" {
					superclasses = append(superclasses, childText(sc, src))
				}
			}
		case "properties":
			if className != "" {
				classID := g.MakeNodeID(filePath, "class_"+className)
				p.handleProperties(g, child, src, filePath, classID, className)
			}
		case "methods":
			if className != "" {
				classID := g.MakeNodeID(filePath, "class_"+className)
				p.handleMethods(g, child, src, filePath, fileNodeID, classID, className)
			}
		}
	}

	if className == "" {
		return
	}

	meta := map[string]string{"kind": "classdef"}
	if len(superclasses) > 0 {
		meta["superclasses"] = strings.Join(superclasses, ", ")
	}
	classID := g.MakeNodeID(filePath, "class_"+className)
	if g.GetNode(classID) == nil {
		g.AddNode(&graph.Node{
			ID:       classID,
			Type:     graph.NodeStruct,
			Name:     className,
			File:     filePath,
			Line:     startLine,
			Exported: true,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: classID, Type: graph.EdgeDefines})
	}

	// Add implements edges for superclasses.
	for _, sc := range superclasses {
		scID := g.MakeNodeID(filePath, "superclass_"+sc)
		if g.GetNode(scID) == nil {
			g.AddNode(&graph.Node{
				ID:       scID,
				Type:     graph.NodeStruct,
				Name:     sc,
				File:     filePath,
				Line:     startLine,
				Exported: true,
				Metadata: map[string]string{"kind": "superclass"},
			})
		}
		g.AddEdge(&graph.Edge{From: classID, To: scID, Type: graph.EdgeImplements})
	}
}

// handleProperties emits NodeVariable for each property in a properties block.
func (p *MATLABParser) handleProperties(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	classNodeID graph.NodeID,
	className string,
) {
	// Check access attributes.
	isPrivate := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		attr := n.Child(i)
		if attr.IsNull() || attr.Type() != "attributes" {
			continue
		}
		attrText := strings.ToLower(childText(attr, src))
		if strings.Contains(attrText, "private") {
			isPrivate = true
		}
	}

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "property" {
			continue
		}
		propName := ""
		for j := uint32(0); j < child.ChildCount(); j++ {
			pn := child.Child(j)
			if !pn.IsNull() && pn.Type() == "identifier" {
				propName = childText(pn, src)
				break
			}
		}
		if propName == "" {
			continue
		}
		startLine := int(child.StartPoint().Row) + 1
		nodeID := g.MakeNodeID(filePath, className+"_prop_"+propName)
		if g.GetNode(nodeID) != nil {
			continue
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     graph.NodeVariable,
			Name:     propName,
			File:     filePath,
			Line:     startLine,
			Exported: !isPrivate,
			Metadata: map[string]string{"kind": "property", "class": className},
		})
		g.AddEdge(&graph.Edge{From: classNodeID, To: nodeID, Type: graph.EdgeDefines})
	}
}

// handleMethods processes a methods block, emitting NodeMethod for each function.
func (p *MATLABParser) handleMethods(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	classNodeID graph.NodeID,
	className string,
) {
	isStatic := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		attr := n.Child(i)
		if attr.IsNull() || attr.Type() != "attributes" {
			continue
		}
		if strings.Contains(strings.ToLower(childText(attr, src)), "static") {
			isStatic = true
		}
	}

	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() || child.Type() != "function_definition" {
			continue
		}
		name := matlabFuncName(child, src)
		if name == "" {
			continue
		}
		startLine := int(child.StartPoint().Row) + 1
		fullName := className + "." + name
		nodeID := g.MakeNodeID(filePath, "method_"+fullName)
		if g.GetNode(nodeID) != nil {
			continue
		}
		meta := map[string]string{"kind": "method", "class": className}
		if isStatic {
			meta["static"] = "true"
		}
		nodeType := graph.NodeMethod
		if isStatic {
			nodeType = graph.NodeFunction
		}
		g.AddNode(&graph.Node{
			ID:       nodeID,
			Type:     nodeType,
			Name:     fullName,
			File:     filePath,
			Line:     startLine,
			Exported: !strings.HasPrefix(name, "_"),
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: classNodeID, To: nodeID, Type: graph.EdgeDefines})
		_ = fileNodeID
	}
}

// handleTopLevelFunc processes a standalone function_definition.
func (p *MATLABParser) handleTopLevelFunc(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	className string,
) {
	name := matlabFuncName(n, src)
	if name == "" {
		return
	}
	startLine := int(n.StartPoint().Row) + 1
	nodeID := g.MakeNodeID(filePath, "func_"+name)
	if g.GetNode(nodeID) != nil {
		return
	}
	meta := map[string]string{"kind": "function"}
	if className != "" {
		meta["class"] = className
	}
	g.AddNode(&graph.Node{
		ID:       nodeID,
		Type:     graph.NodeFunction,
		Name:     name,
		File:     filePath,
		Line:     startLine,
		Exported: !strings.HasPrefix(name, "_"),
		Metadata: meta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
}

// matlabFuncName extracts the function name from a function_definition node.
// Structure: function [output =] NAME (args) block end
func matlabFuncName(n sitter.Node, src []byte) string {
	// The name identifier comes after optional function_output.
	seenOutput := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		switch child.Type() {
		case "function_output":
			seenOutput = true
		case "identifier":
			if seenOutput || i > 0 {
				return childText(child, src)
			}
		}
	}
	// Fallback: first identifier that isn't the keyword.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if !child.IsNull() && child.Type() == "identifier" {
			return childText(child, src)
		}
	}
	return ""
}
