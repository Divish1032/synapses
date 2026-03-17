package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	vueg "github.com/alexaandru/go-sitter-forest/vue"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// VueParser parses Vue Single-File Components (.vue files).
//
// Extracts:
//   - A component node (NodeStruct, kind=component) named after the filename
//   - Script block content re-parsed via JS or TS sub-parser for functions,
//     classes, imports, variables, etc.
//   - Style block metadata (scoped flag, language)
//
// The Vue grammar provides raw_text for script/style content rather than
// a parsed AST, so script content is delegated to the JS/TS sub-parsers.
type VueParser struct {
	language *sitter.Language
	jsParser  *JavaScriptParser
	tsParser  *TypeScriptParser
}

// NewVueParser creates a ready-to-use VueParser.
func NewVueParser() *VueParser {
	return &VueParser{
		language: sitter.NewLanguage(vueg.GetLanguage()),
		jsParser:  NewJavaScriptParser(),
		tsParser:  NewTypeScriptParser(),
	}
}

// Extensions returns the file extensions handled by this parser.
func (p *VueParser) Extensions() []string {
	return []string{".vue"}
}

// Parse extracts code entities from a single Vue SFC file and merges them
// into the provided graph.
func (p *VueParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	// Parse the Vue file with tree-sitter to get the SFC structure.
	vueParser := sitter.NewParser()
	vueParser.SetLanguage(p.language)
	tree, _ := vueParser.ParseString(context.Background(), nil, src)
	root := tree.RootNode()

	// Derive component name from filename (e.g. MyComponent.vue → MyComponent).
	baseName := filepath.Base(filePath)
	componentName := strings.TrimSuffix(baseName, filepath.Ext(baseName))

	// Create the file node.
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: baseName,
		File: filePath,
		Line: 1,
	})

	// Walk top-level children of the document root to find template, script,
	// and style elements.
	var (
		scriptLang      = "js"   // default script language
		scriptSetup     = false  // true when <script setup> is present
		scriptContent   []byte
		scriptStartLine int // 1-based line number where raw_text begins

		styleLang   = "css" // default style language
		styleScoped = false  // true when <style scoped>
		styleLine   = 1
		hasStyle    = false
	)

	for i := uint32(0); i < root.ChildCount(); i++ {
		child := root.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "script_element":
			scriptLang, scriptSetup, scriptContent, scriptStartLine = p.extractScriptInfo(child, src)

		case "style_element":
			hasStyle = true
			styleLine = int(child.StartPoint().Row) + 1
			styleLang, styleScoped = p.extractStyleInfo(child, src)
		}
	}

	// Build component metadata.
	componentMeta := map[string]string{
		"kind": "component",
		"lang": scriptLang,
	}
	if scriptSetup {
		componentMeta["setup"] = "true"
	}

	// Create the component node (NodeStruct, kind=component).
	componentNodeID := g.MakeNodeID(filePath, componentName)
	g.AddNode(&graph.Node{
		ID:       componentNodeID,
		Type:     graph.NodeStruct,
		Name:     componentName,
		File:     filePath,
		Line:     1,
		Exported: true,
		Metadata: componentMeta,
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: componentNodeID, Type: graph.EdgeDefines})

	// Re-parse script content using the appropriate sub-parser.
	if len(scriptContent) > 0 {
		p.parseScriptBlock(g, filePath, scriptContent, scriptLang, scriptStartLine, fileNodeID)
	}

	// Create a style node if a <style> block is present.
	if hasStyle {
		styleMeta := map[string]string{
			"kind": "style",
			"lang": styleLang,
		}
		if styleScoped {
			styleMeta["scoped"] = "true"
		}
		styleNodeID := g.MakeNodeID(filePath, componentName+".style")
		g.AddNode(&graph.Node{
			ID:       styleNodeID,
			Type:     graph.NodeVariable,
			Name:     componentName + ".style",
			File:     filePath,
			Line:     styleLine,
			Exported: false,
			Metadata: styleMeta,
		})
		g.AddEdge(&graph.Edge{From: componentNodeID, To: styleNodeID, Type: graph.EdgeContains})
	}

	return nil
}

// extractScriptInfo reads the <script> element node and returns:
//   - lang: "js" or "ts"
//   - setup: true if <script setup>
//   - content: the raw text bytes of the script body
//   - startLine: 1-based line number of the raw_text node in the .vue file
func (p *VueParser) extractScriptInfo(scriptElem sitter.Node, src []byte) (lang string, setup bool, content []byte, startLine int) {
	lang = "js"
	startLine = int(scriptElem.StartPoint().Row) + 1

	// Examine the start_tag for lang and setup attributes.
	for i := uint32(0); i < scriptElem.ChildCount(); i++ {
		child := scriptElem.Child(i)
		if child.IsNull() {
			continue
		}

		switch child.Type() {
		case "start_tag":
			// Check for lang attribute.
			langVal := vueGetAttribute(child, src, "lang")
			if langVal == "ts" || langVal == "tsx" {
				lang = "ts"
			}
			// Check for setup attribute (boolean).
			setupVal := vueGetAttribute(child, src, "setup")
			if setupVal != "" {
				setup = true
			}

		case "raw_text":
			startLine = int(child.StartPoint().Row) + 1
			content = src[child.StartByte():child.EndByte()]
		}
	}
	return
}

// extractStyleInfo reads the <style> element node and returns:
//   - lang: css language (defaults to "css")
//   - scoped: true if scoped attribute is present
func (p *VueParser) extractStyleInfo(styleElem sitter.Node, src []byte) (lang string, scoped bool) {
	lang = "css"

	for i := uint32(0); i < styleElem.ChildCount(); i++ {
		child := styleElem.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "start_tag" {
			// Check for lang attribute.
			langVal := vueGetAttribute(child, src, "lang")
			if langVal != "" && langVal != "true" {
				lang = langVal
			}
			// Check for scoped attribute (boolean).
			scopedVal := vueGetAttribute(child, src, "scoped")
			if scopedVal != "" {
				scoped = true
			}
		}
	}
	return
}

// parseScriptBlock delegates script content to the JS or TS sub-parser,
// then adjusts the line numbers of newly added nodes by the script's start
// line offset within the .vue file.
func (p *VueParser) parseScriptBlock(
	g *graph.Graph,
	filePath string,
	scriptContent []byte,
	lang string,
	scriptStartLine int,
	fileNodeID graph.NodeID,
) {
	// Snapshot node IDs before sub-parse to identify newly added nodes.
	before := make(map[graph.NodeID]bool)
	for _, n := range g.NodesForFile(filePath) {
		before[n.ID] = true
	}

	// Choose sub-parser and run it.
	// The sub-parser will try to add a file node for filePath — that's fine,
	// AddNode is idempotent (it will not overwrite the existing file node).
	var parseErr error
	if lang == "ts" {
		parseErr = p.tsParser.Parse(g, filePath, scriptContent)
	} else {
		parseErr = p.jsParser.Parse(g, filePath, scriptContent)
	}
	if parseErr != nil {
		// Non-fatal: the component node is still useful even without script detail.
		return
	}

	// Adjust line numbers for all newly added nodes.
	// The sub-parser produced line numbers relative to the script block (1-based).
	// We need to shift them by (scriptStartLine - 1) to get .vue-file-relative lines.
	lineOffset := scriptStartLine - 1
	if lineOffset <= 0 {
		return
	}

	for _, n := range g.NodesForFile(filePath) {
		if before[n.ID] {
			continue // skip pre-existing nodes (file node, component node, style node)
		}
		if n.Type == graph.NodeFile {
			continue // never adjust the file node's line
		}
		n.Line += lineOffset
	}

	// Re-connect newly added nodes to our file node via DEFINES edges if the
	// sub-parser connected them to a duplicate file node. The sub-parser uses
	// the same fileNodeID (MakeNodeID(filePath, filePath)), so edges are already
	// correct — no additional wiring needed.
	_ = fileNodeID
}

// vueGetAttribute searches a start_tag node's attribute children for an
// attribute with the given name and returns its value.
//
// Boolean attributes (e.g. "setup", "scoped") have no value child — "true"
// is returned in that case.  An empty string means the attribute was not found.
func vueGetAttribute(tag sitter.Node, src []byte, attrName string) string {
	for i := uint32(0); i < tag.ChildCount(); i++ {
		child := tag.Child(i)
		if child.IsNull() || child.Type() != "attribute" {
			continue
		}

		// Walk attribute children looking for attribute_name that matches.
		nameFound := false
		for j := uint32(0); j < child.ChildCount(); j++ {
			gc := child.Child(j)
			if gc.IsNull() {
				continue
			}
			if gc.Type() == "attribute_name" {
				name := string(src[gc.StartByte():gc.EndByte()])
				if name == attrName {
					nameFound = true
				}
			}
		}
		if !nameFound {
			continue
		}

		// Attribute found — now look for its value.
		for k := uint32(0); k < child.ChildCount(); k++ {
			vc := child.Child(k)
			if vc.IsNull() {
				continue
			}
			if vc.Type() == "quoted_attribute_value" {
				txt := string(src[vc.StartByte():vc.EndByte()])
				// Strip surrounding quotes.
				txt = strings.Trim(txt, `"'`)
				return txt
			}
			// Plain attribute value (unquoted).
			if vc.Type() == "attribute_value" {
				return string(src[vc.StartByte():vc.EndByte()])
			}
		}
		// Boolean attribute — present but with no value.
		return "true"
	}
	return ""
}
