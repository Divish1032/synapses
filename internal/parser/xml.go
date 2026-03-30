package parser

import (
	"path/filepath"
	"strings"

	xmlg "github.com/alexaandru/go-sitter-forest/xml"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// XMLParser parses XML files (.xml) using tree-sitter.
// It performs schema-aware extraction based on filename:
//   - pom.xml (Maven): project identity, dependencies, plugins, modules
//   - AndroidManifest.xml: activities, services, receivers, permissions
//   - *context.xml / *config.xml (Spring): beans, imports
//   - Generic XML: root element, direct children with id/name attributes
type XMLParser struct {
	language *sitter.Language
}

// NewXMLParser creates a ready-to-use XMLParser.
func NewXMLParser() *XMLParser {
	return &XMLParser{language: sitter.NewLanguage(xmlg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *XMLParser) Extensions() []string {
	return []string{".xml"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *XMLParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single XML file.
func (p *XMLParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
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

	// Find root element (first element child of document).
	rootElement := xmlFindFirstElement(root, src)
	if rootElement.IsNull() {
		return nil
	}

	base := filepath.Base(filePath)
	switch {
	case base == "pom.xml":
		p.parsePom(g, filePath, fileNodeID, rootElement, src)
	case base == "AndroidManifest.xml":
		p.parseAndroidManifest(g, filePath, fileNodeID, rootElement, src)
	case strings.Contains(strings.ToLower(base), "context.xml") || strings.Contains(strings.ToLower(base), "config.xml"):
		p.parseSpringContext(g, filePath, fileNodeID, rootElement, src)
	default:
		p.parseGeneric(g, filePath, fileNodeID, rootElement, src)
	}

	return nil
}

// ─── Schema-specific parsers ──────────────────────────────────────────────────

// parsePom handles Maven pom.xml files.
func (p *XMLParser) parsePom(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	rootElem sitter.Node,
	src []byte,
) {
	// Extract project identity from direct children.
	groupID := xmlFindDirectChildText(rootElem, "groupId", src)
	artifactID := xmlFindDirectChildText(rootElem, "artifactId", src)
	version := xmlFindDirectChildText(rootElem, "version", src)

	if groupID != "" || artifactID != "" {
		name := artifactID
		if name == "" {
			name = groupID
		}
		nodeID := g.MakeNodeID(filePath, "project_identity")
		meta := map[string]string{"kind": "project"}
		if groupID != "" {
			meta["group"] = groupID
		}
		if artifactID != "" {
			meta["artifact"] = artifactID
		}
		if version != "" {
			meta["version"] = version
		}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeStruct, Name: name,
			File: filePath, Line: int(rootElem.StartPoint().Row) + 1,
			Exported: true, Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
	}

	// Walk content children looking for <dependencies>, <plugins>, <modules>.
	content := xmlGetContent(rootElem, src)
	if content.IsNull() {
		return
	}
	for i := uint32(0); i < content.ChildCount(); i++ {
		child := content.Child(i)
		if child.IsNull() {
			continue
		}
		if nodeType(child) != "element" {
			continue
		}
		tagName := xmlElementTagName(child, src)
		switch tagName {
		case "dependencies":
			p.parsePomDependencies(g, filePath, fileNodeID, child, src)
		case "plugins":
			p.parsePomPlugins(g, filePath, fileNodeID, child, src)
		case "modules":
			p.parsePomModules(g, filePath, fileNodeID, child, src)
		case "build":
			// Recurse into <build> to find nested <plugins> and <dependencies>.
			buildContent := xmlGetContent(child, src)
			if !buildContent.IsNull() {
				for j := uint32(0); j < buildContent.ChildCount(); j++ {
					bc := buildContent.Child(j)
					if bc.IsNull() || nodeType(bc) != "element" {
						continue
					}
					switch xmlElementTagName(bc, src) {
					case "plugins":
						p.parsePomPlugins(g, filePath, fileNodeID, bc, src)
					case "dependencies":
						p.parsePomDependencies(g, filePath, fileNodeID, bc, src)
					}
				}
			}
		}
	}
}

func (p *XMLParser) parsePomDependencies(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	depsElem sitter.Node,
	src []byte,
) {
	depsContent := xmlGetContent(depsElem, src)
	if depsContent.IsNull() {
		return
	}
	for i := uint32(0); i < depsContent.ChildCount(); i++ {
		child := depsContent.Child(i)
		if child.IsNull() || nodeType(child) != "element" {
			continue
		}
		if xmlElementTagName(child, src) != "dependency" {
			continue
		}
		depContent := xmlGetContent(child, src)
		if depContent.IsNull() {
			continue
		}
		group := xmlFindDirectChildText(child, "groupId", src)
		artifact := xmlFindDirectChildText(child, "artifactId", src)
		ver := xmlFindDirectChildText(child, "version", src)

		name := artifact
		if name == "" {
			name = group
		}
		if name == "" {
			continue
		}
		uniqueName := "dep_" + group + "_" + artifact
		nodeID := g.MakeNodeID(filePath, uniqueName)
		meta := map[string]string{"kind": "dependency"}
		if group != "" {
			meta["group"] = group
		}
		if artifact != "" {
			meta["artifact"] = artifact
		}
		if ver != "" {
			meta["version"] = ver
		}
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeVariable, Name: name,
			File: filePath, Line: int(child.StartPoint().Row) + 1,
			Metadata: meta,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeContains})
	}
}

func (p *XMLParser) parsePomPlugins(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	pluginsElem sitter.Node,
	src []byte,
) {
	pluginsContent := xmlGetContent(pluginsElem, src)
	if pluginsContent.IsNull() {
		return
	}
	for i := uint32(0); i < pluginsContent.ChildCount(); i++ {
		child := pluginsContent.Child(i)
		if child.IsNull() || nodeType(child) != "element" {
			continue
		}
		if xmlElementTagName(child, src) != "plugin" {
			continue
		}
		artifact := xmlFindDirectChildText(child, "artifactId", src)
		group := xmlFindDirectChildText(child, "groupId", src)
		name := artifact
		if name == "" {
			name = group
		}
		if name == "" {
			continue
		}
		uniqueName := "plugin_" + group + "_" + artifact
		nodeID := g.MakeNodeID(filePath, uniqueName)
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeVariable, Name: name,
			File: filePath, Line: int(child.StartPoint().Row) + 1,
			Metadata: map[string]string{"kind": "plugin"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeContains})
	}
}

func (p *XMLParser) parsePomModules(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	modulesElem sitter.Node,
	src []byte,
) {
	modulesContent := xmlGetContent(modulesElem, src)
	if modulesContent.IsNull() {
		return
	}
	for i := uint32(0); i < modulesContent.ChildCount(); i++ {
		child := modulesContent.Child(i)
		if child.IsNull() || nodeType(child) != "element" {
			continue
		}
		if xmlElementTagName(child, src) != "module" {
			continue
		}
		modName := xmlGetElementText(child, src)
		if modName == "" {
			continue
		}
		nodeID := g.MakeNodeID(filePath, "module_"+modName)
		g.AddNode(&graph.Node{
			ID: nodeID, Type: graph.NodeVariable, Name: modName,
			File: filePath, Line: int(child.StartPoint().Row) + 1,
			Metadata: map[string]string{"kind": "module"},
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeContains})
	}
}

// parseAndroidManifest handles AndroidManifest.xml files.
func (p *XMLParser) parseAndroidManifest(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	rootElem sitter.Node,
	src []byte,
) {
	// walkManifestContent walks the CONTENT node of an element,
	// processing child elements up to maxDepth levels deep.
	var walkManifestContent func(contentNode sitter.Node, depth int)
	walkManifestContent = func(contentNode sitter.Node, depth int) {
		if contentNode.IsNull() || depth > 4 {
			return
		}
		for i := uint32(0); i < contentNode.ChildCount(); i++ {
			child := contentNode.Child(i)
			if child.IsNull() || nodeType(child) != "element" {
				continue
			}
			tagName := xmlElementTagName(child, src)
			attrs := xmlGetAttributes(child, src)
			androidName := xmlAndroidAttr(attrs, "name")
			startLine := int(child.StartPoint().Row) + 1

			switch tagName {
			case "activity", "service", "receiver", "provider":
				if androidName == "" {
					androidName = tagName
				}
				nodeID := g.MakeNodeID(filePath, tagName+"_"+androidName)
				meta := map[string]string{"kind": tagName}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeStruct, Name: androidName,
					File: filePath, Line: startLine,
					Exported: true, Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			case "permission":
				if androidName != "" {
					nodeID := g.MakeNodeID(filePath, "permission_"+androidName)
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeVariable, Name: androidName,
						File: filePath, Line: startLine,
						Metadata: map[string]string{"kind": "permission"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			case "uses-permission":
				if androidName != "" {
					nodeID := g.MakeNodeID(filePath, "uses_permission_"+androidName)
					g.AddNode(&graph.Node{
						ID: nodeID, Type: graph.NodeVariable, Name: androidName,
						File: filePath, Line: startLine,
						Metadata: map[string]string{"kind": "uses_permission"},
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

			// Recurse into this element's content.
			childContent := xmlGetContent(child, src)
			if !childContent.IsNull() {
				walkManifestContent(childContent, depth+1)
			}
		}
	}

	// Start walking from the root element's content.
	rootContent := xmlGetContent(rootElem, src)
	walkManifestContent(rootContent, 0)
}

// parseSpringContext handles Spring application context XML files.
func (p *XMLParser) parseSpringContext(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	rootElem sitter.Node,
	src []byte,
) {
	content := xmlGetContent(rootElem, src)
	if content.IsNull() {
		return
	}
	for i := uint32(0); i < content.ChildCount(); i++ {
		child := content.Child(i)
		if child.IsNull() || nodeType(child) != "element" {
			continue
		}
		tagName := xmlElementTagName(child, src)
		attrs := xmlGetAttributes(child, src)
		startLine := int(child.StartPoint().Row) + 1

		switch tagName {
		case "bean":
			beanID := attrs["id"]
			beanClass := attrs["class"]
			if beanID == "" {
				beanID = beanClass
			}
			if beanID == "" {
				continue
			}
			meta := map[string]string{"kind": "bean"}
			if beanClass != "" {
				meta["class"] = beanClass
			}
			nodeID := g.MakeNodeID(filePath, "bean_"+beanID)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: beanID,
				File: filePath, Line: startLine,
				Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
		case "import":
			resource := attrs["resource"]
			if resource == "" {
				continue
			}
			importNodeID := g.MakeNodeID(resource, resource)
			g.AddNode(&graph.Node{
				ID: importNodeID, Type: graph.NodePackage, Name: resource,
				Package: resource, File: filePath,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
		}
	}
}

// parseGeneric handles all other XML files with a generic fallback strategy.
func (p *XMLParser) parseGeneric(
	g *graph.Graph,
	filePath string,
	fileNodeID graph.NodeID,
	rootElem sitter.Node,
	src []byte,
) {
	tagName := xmlElementTagName(rootElem, src)
	if tagName == "" {
		tagName = "root"
	}

	// Emit root element as NodeStruct.
	rootNodeID := g.MakeNodeID(filePath, "xml_root_"+tagName)
	g.AddNode(&graph.Node{
		ID: rootNodeID, Type: graph.NodeStruct, Name: tagName,
		File: filePath, Line: int(rootElem.StartPoint().Row) + 1,
		Exported: true, Metadata: map[string]string{"kind": "xml_root"},
	})
	g.AddEdge(&graph.Edge{From: fileNodeID, To: rootNodeID, Type: graph.EdgeDefines})

	// Walk direct children, emitting nodes for semantic elements (those with id/name attrs
	// or known semantic tag names).
	content := xmlGetContent(rootElem, src)
	if content.IsNull() {
		return
	}

	seenTags := make(map[string]int) // tag → count for sampling
	for i := uint32(0); i < content.ChildCount(); i++ {
		child := content.Child(i)
		if child.IsNull() || nodeType(child) != "element" {
			continue
		}
		childTag := xmlElementTagName(child, src)
		attrs := xmlGetAttributes(child, src)
		startLine := int(child.StartPoint().Row) + 1

		// Check for semantic attributes.
		nameAttr := attrs["id"]
		if nameAttr == "" {
			nameAttr = attrs["name"]
		}

		// Known semantic tag names always get emitted.
		knownSemantic := map[string]bool{
			"bean": true, "service": true, "component": true, "resource": true,
		}

		if nameAttr != "" {
			// Has id/name attribute — emit as NodeStruct.
			uniqueName := childTag + "_" + nameAttr
			nodeID := g.MakeNodeID(filePath, uniqueName)
			meta := map[string]string{"kind": childTag}
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: nameAttr,
				File: filePath, Line: startLine,
				Exported: true, Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: rootNodeID, To: nodeID, Type: graph.EdgeContains})
		} else if knownSemantic[childTag] {
			text := xmlCleanElementName(child, childTag, src)
			nodeID := g.MakeNodeID(filePath, childTag+"_"+text)
			if g.GetNode(nodeID) == nil {
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeVariable, Name: text,
					File: filePath, Line: startLine,
					Metadata: map[string]string{"kind": childTag},
				})
				g.AddEdge(&graph.Edge{From: rootNodeID, To: nodeID, Type: graph.EdgeContains})
			}
		} else {
			// Walk one level deeper: if child has children with id/name attrs, emit them.
			childContent := xmlGetContent(child, src)
			emittedChildren := false
			if !childContent.IsNull() {
				for j := uint32(0); j < childContent.ChildCount(); j++ {
					gc := childContent.Child(j)
					if gc.IsNull() || nodeType(gc) != "element" {
						continue
					}
					gcAttrs := xmlGetAttributes(gc, src)
					gcName := gcAttrs["id"]
					if gcName == "" {
						gcName = gcAttrs["name"]
					}
					if gcName != "" {
						gcTag := xmlElementTagName(gc, src)
						gcLine := int(gc.StartPoint().Row) + 1
						gcNodeID := g.MakeNodeID(filePath, gcTag+"_"+gcName)
						if g.GetNode(gcNodeID) == nil {
							g.AddNode(&graph.Node{
								ID: gcNodeID, Type: graph.NodeStruct, Name: gcName,
								File: filePath, Line: gcLine,
								Exported: true, Metadata: map[string]string{"kind": gcTag},
							})
							g.AddEdge(&graph.Edge{From: rootNodeID, To: gcNodeID, Type: graph.EdgeContains})
							emittedChildren = true
						}
					}
				}
			}
			if !emittedChildren {
				// For repeating elements, sample first 3.
				seenTags[childTag]++
				if seenTags[childTag] <= 3 {
					name := xmlCleanElementName(child, childTag, src)
					nodeID := g.MakeNodeID(filePath, childTag+"_"+name+"_"+string(rune('0'+seenTags[childTag]-1)))
					if g.GetNode(nodeID) == nil {
						g.AddNode(&graph.Node{
							ID: nodeID, Type: graph.NodeVariable, Name: name,
							File: filePath, Line: startLine,
							Metadata: map[string]string{"kind": childTag},
						})
						g.AddEdge(&graph.Edge{From: rootNodeID, To: nodeID, Type: graph.EdgeContains})
					}
				}
			}
		}
	}
}

// xmlCleanElementName extracts a usable name from an element.
// Falls back to tag name if text content is multi-line or too long (container elements).
func xmlCleanElementName(elem sitter.Node, tagName string, src []byte) string {
	text := xmlGetElementText(elem, src)
	if text == "" || strings.ContainsAny(text, "\n\r") || len(text) > 100 {
		return tagName
	}
	return text
}

// ─── XML AST helper functions ─────────────────────────────────────────────────

// xmlFindFirstElement finds the first element child of a document node.
func xmlFindFirstElement(docNode sitter.Node, src []byte) sitter.Node {
	for i := uint32(0); i < docNode.ChildCount(); i++ {
		child := docNode.Child(i)
		if !child.IsNull() && nodeType(child) == "element" {
			return child
		}
	}
	return sitter.Node{}
}

// xmlElementTagName returns the tag name of an element node.
// It checks STag → Name, EmptyElemTag → Name.
func xmlElementTagName(elemNode sitter.Node, src []byte) string {
	if elemNode.IsNull() {
		return ""
	}
	// Try STag first.
	for i := uint32(0); i < elemNode.ChildCount(); i++ {
		child := elemNode.Child(i)
		if child.IsNull() {
			continue
		}
		ct := nodeType(child)
		if ct == "STag" || ct == "EmptyElemTag" {
			// Find Name child.
			for j := uint32(0); j < child.ChildCount(); j++ {
				gc := child.Child(j)
				if !gc.IsNull() && nodeType(gc) == "Name" {
					return childText(gc, src)
				}
			}
		}
	}
	return ""
}

// xmlGetContent returns the content node of an element.
func xmlGetContent(elemNode sitter.Node, src []byte) sitter.Node {
	if elemNode.IsNull() {
		return sitter.Node{}
	}
	for i := uint32(0); i < elemNode.ChildCount(); i++ {
		child := elemNode.Child(i)
		if !child.IsNull() && nodeType(child) == "content" {
			return child
		}
	}
	return sitter.Node{}
}

// xmlGetAttributes returns all attributes of an element as a map of name→value.
// Strips quotes from AttValue nodes.
func xmlGetAttributes(elemNode sitter.Node, src []byte) map[string]string {
	attrs := make(map[string]string)
	if elemNode.IsNull() {
		return attrs
	}
	// Attributes live inside STag or EmptyElemTag.
	for i := uint32(0); i < elemNode.ChildCount(); i++ {
		child := elemNode.Child(i)
		if child.IsNull() {
			continue
		}
		ct := nodeType(child)
		if ct != "STag" && ct != "EmptyElemTag" {
			continue
		}
		for j := uint32(0); j < child.ChildCount(); j++ {
			attr := child.Child(j)
			if attr.IsNull() || nodeType(attr) != "Attribute" {
				continue
			}
			var attrName, attrVal string
			for k := uint32(0); k < attr.ChildCount(); k++ {
				ac := attr.Child(k)
				if ac.IsNull() {
					continue
				}
				switch nodeType(ac) {
				case "Name":
					attrName = childText(ac, src)
				case "AttValue":
					attrVal = xmlStripAttValue(childText(ac, src))
				}
			}
			if attrName != "" {
				attrs[attrName] = attrVal
			}
		}
	}
	return attrs
}

// xmlAndroidAttr looks up android:name (or name) from an attribute map.
func xmlAndroidAttr(attrs map[string]string, key string) string {
	if v, ok := attrs["android:"+key]; ok {
		return v
	}
	return attrs[key]
}

// xmlStripAttValue removes surrounding quotes from an XML attribute value.
func xmlStripAttValue(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// xmlGetElementText returns the text content of a simple element like <tag>text</tag>.
// Returns "" if element has no text content or has nested elements.
func xmlGetElementText(elemNode sitter.Node, src []byte) string {
	content := xmlGetContent(elemNode, src)
	if content.IsNull() {
		return ""
	}
	return strings.TrimSpace(childText(content, src))
}

// xmlFindDirectChildText finds a direct child element with the given tag name
// and returns its text content.
func xmlFindDirectChildText(elemNode sitter.Node, tagName string, src []byte) string {
	content := xmlGetContent(elemNode, src)
	if content.IsNull() {
		return ""
	}
	for i := uint32(0); i < content.ChildCount(); i++ {
		child := content.Child(i)
		if child.IsNull() || nodeType(child) != "element" {
			continue
		}
		if xmlElementTagName(child, src) == tagName {
			return xmlGetElementText(child, src)
		}
	}
	return ""
}
