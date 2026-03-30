package parser

import (
	"path/filepath"
	"strings"

	"github.com/alexaandru/go-sitter-forest/ruby"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// RubyParser parses Ruby (.rb) source files.
type RubyParser struct {
	language *sitter.Language
}

// NewRubyParser creates a ready-to-use RubyParser.
func NewRubyParser() *RubyParser {
	return &RubyParser{language: sitter.NewLanguage(ruby.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
// .rbi files are Sorbet type stubs — they use Ruby syntax, so the same
// tree-sitter Ruby grammar handles them correctly.
func (p *RubyParser) Extensions() []string {
	return []string{".rb", ".rbi"}
}

// TSLanguageForFile returns the tree-sitter language for this parser.
func (p *RubyParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// extractRubyDeclInfo performs a pre-pass for metadata.
func extractRubyDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch nodeType(n) {
		case "method", "singleton_method":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if enclosingClass != "" {
					qualName = enclosingClass + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"body_statement"}),
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "class":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
				// Walk body with class context.
				if body := firstChildOfType(n, "body_statement"); !body.IsNull() {
					for i := uint32(0); i < body.ChildCount(); i++ {
						walk(body.Child(i), name)
					}
				}
				return
			}
		case "module":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
	return result
}

// Parse extracts code entities from a single Ruby file.
func (p *RubyParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree != nil {
		defer tree.Close()
	}
	if tree == nil {
		return nil
	}
	root := tree.RootNode()

	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lang := p.language
	declInfo := extractRubyDeclInfo(root, src)

	// --- require / require_relative via call nodes ---
	// Ruby require calls are generic `call` nodes: require 'foo' or require_relative 'bar'
	requireQuery := `(call method: (identifier) @method_name arguments: (argument_list (string (string_content) @req_path)))`
	_ = runQuery(lang, root, src, requireQuery, func(captures map[string]string, _ int) {
		methodName := captures["method_name"]
		reqPath := captures["req_path"]
		if reqPath == "" {
			return
		}
		if methodName != "require" && methodName != "require_relative" {
			return
		}
		importNodeID := g.MakeNodeID(reqPath, reqPath)
		g.AddNode(&graph.Node{ID: importNodeID, Type: graph.NodePackage, Name: reqPath, Package: reqPath, File: filePath})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// Derive module name from filename: "builder.rb" → "builder", "rack.rb" → "rack".
	moduleName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

	// --- All declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// Post-process: set Package on all nodes in this file that don't have it.
	for _, n := range g.FindByFile(filePath) {
		if n.Package == "" && n.Type != graph.NodeFile && n.Type != graph.NodePackage {
			n.Package = moduleName
		}
	}

	// --- Call sites ---
	collectRubyCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks Ruby AST with class qualification.
func (p *RubyParser) extractAllDeclarations(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, enclosingClass string)
	walk = func(n sitter.Node, enclosingClass string) {
		if n.IsNull() {
			return
		}
		switch nodeType(n) {
		case "class":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isRubyPublic(name), Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := firstChildOfType(n, "body_statement"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "module":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeInterface, Name: name, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isRubyPublic(name), Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if body := firstChildOfType(n, "body_statement"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), name)
				}
			}
			return

		case "method":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if enclosingClass != "" {
				qualName = enclosingClass + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isRubyPublic(name), Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "singleton_method":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if enclosingClass != "" {
				qualName = enclosingClass + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := buildLangMeta(declInfo[qualName])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "singleton"
			g.AddNode(&graph.Node{
				ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
				Line: int(n.StartPoint().Row) + 1, Exported: isRubyPublic(name), Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if enclosingClass != "" {
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "call":
			// Handle attr_reader :name, attr_writer :name, attr_accessor :name
			// These are `call` nodes with method = identifier (attr_reader etc.)
			// and arguments containing symbols.
			if enclosingClass == "" {
				break
			}
			methodNode := n.ChildByFieldName("method")
			if methodNode.IsNull() {
				break
			}
			methodName := string(src[methodNode.StartByte():methodNode.EndByte()])
			if methodName != "attr_reader" && methodName != "attr_writer" && methodName != "attr_accessor" {
				break
			}
			argsNode := n.ChildByFieldName("arguments")
			if argsNode.IsNull() {
				break
			}
			// Collect all simple_symbol children (e.g. :name → "name")
			for k := uint32(0); k < argsNode.ChildCount(); k++ {
				arg := argsNode.Child(k)
				if arg.IsNull() {
					continue
				}
				var attrName string
				if nodeType(arg) == "simple_symbol" {
					sym := string(src[arg.StartByte():arg.EndByte()])
					attrName = strings.TrimPrefix(sym, ":")
				}
				if attrName == "" {
					continue
				}
				qualName := enclosingClass + "." + attrName
				nodeID := g.MakeNodeID(filePath, qualName)
				if g.GetNode(nodeID) != nil {
					continue
				}
				meta := map[string]string{"kind": methodName}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
					Line:     int(n.StartPoint().Row) + 1,
					Exported: isRubyPublic(attrName),
					Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				classID := g.MakeNodeID(filePath, enclosingClass)
				if g.GetNode(classID) != nil {
					g.AddEdge(&graph.Edge{From: classID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), enclosingClass)
		}
	}
	walk(root, "")
}

// collectRubyCallSites collects call sites.
func collectRubyCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{"class": true, "module": true},
		FuncTypes:  map[string]bool{"method": true, "singleton_method": true},
		CallTypes:  map[string]bool{"call": true},
		NameExtractor: func(n sitter.Node, src []byte) string {
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		AliasedCalleeExtractor: func(n sitter.Node, src []byte) (string, string) {
			methodNode := n.ChildByFieldName("method")
			if methodNode.IsNull() {
				return "", ""
			}
			callee := string(src[methodNode.StartByte():methodNode.EndByte()])
			// Check for receiver: obj.method(args)
			recvNode := n.ChildByFieldName("receiver")
			if !recvNode.IsNull() {
				switch nodeType(recvNode) {
				case "identifier":
					recv := string(src[recvNode.StartByte():recvNode.EndByte()])
					if recv != "self" {
						return recv, callee
					}
				case "constant":
					// Class-qualified calls: Builder.use(), Logger.info()
					return string(src[recvNode.StartByte():recvNode.EndByte()]), callee
				case "scope_resolution":
					// Namespaced constant calls: Rack::Handler.get(), ActiveRecord::Base.establish_connection()
					return string(src[recvNode.StartByte():recvNode.EndByte()]), callee
				}
			}
			return "", callee
		},
		IsBuiltin: isRubyBuiltin,
	})
}

func isRubyPublic(name string) bool {
	return !strings.HasPrefix(name, "_")
}

func isRubyBuiltin(name string) bool {
	switch name {
	case "puts", "print", "p", "pp", "raise", "require", "require_relative",
		"include", "extend", "prepend", "attr_accessor", "attr_reader", "attr_writer",
		"private", "public", "protected", "module_function",
		"new", "initialize", "super", "self", "send", "respond_to?",
		"each", "map", "select", "reject", "reduce", "inject", "collect",
		"find", "detect", "any?", "all?", "none?", "count", "size", "length",
		"push", "pop", "shift", "unshift", "first", "last",
		"to_s", "to_i", "to_f", "to_a", "to_h", "to_sym",
		"nil?", "empty?", "blank?", "present?",
		"freeze", "dup", "clone", "tap", "then", "yield_self":
		return true
	}
	return false
}

// RBSParser parses Ruby RBS type signature files (.rbs).
// RBS files use a dedicated syntax (not Ruby) so we parse them line-by-line
// rather than using the tree-sitter Ruby grammar. We emit classes, modules,
// interfaces, and method signatures as graph nodes.
type RBSParser struct{}

// NewRBSParser creates a ready-to-use RBSParser.
func NewRBSParser() *RBSParser {
	return &RBSParser{}
}

// Extensions returns the file extensions handled by this parser.
func (p *RBSParser) Extensions() []string {
	return []string{".rbs"}
}

// Parse extracts code entities from a single RBS file.
// RBS syntax examples:
//
//	class Foo < Bar                  → NodeStruct
//	module Baz                       → NodeInterface
//	interface _Iterator[T]           → NodeInterface with kind="interface"
//	def initialize: (String) -> void → NodeMethod
//	def self.create: (Integer) -> Foo → NodeMethod with kind="singleton"
//	type alias_name = Integer        → NodeStruct with kind="alias"
func (p *RBSParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	fileNodeID := g.MakeNodeID(filePath, filePath)
	g.AddNode(&graph.Node{
		ID:   fileNodeID,
		Type: graph.NodeFile,
		Name: filepath.Base(filePath),
		File: filePath,
		Line: 1,
	})

	lines := strings.Split(string(src), "\n")
	var enclosingName string // current class/module name

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineNum := i + 1

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// class Foo or class Foo < Bar
		if strings.HasPrefix(trimmed, "class ") {
			rest := strings.TrimPrefix(trimmed, "class ")
			name := strings.Fields(rest)[0]
			name = strings.TrimSuffix(name, "[") // generics: Foo[T]
			if idx := strings.IndexByte(name, '['); idx != -1 {
				name = name[:idx]
			}
			enclosingName = name
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "class", "source": "rbs"}
				// Extract parent: class Foo < Bar
				if idx := strings.Index(rest, " < "); idx != -1 {
					meta["extends"] = strings.TrimSpace(rest[idx+3:])
				}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
					Line: lineNum, Exported: isExported(name), Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
			continue
		}

		// module Foo
		if strings.HasPrefix(trimmed, "module ") {
			rest := strings.TrimPrefix(trimmed, "module ")
			name := strings.Fields(rest)[0]
			if idx := strings.IndexByte(name, '['); idx != -1 {
				name = name[:idx]
			}
			enclosingName = name
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "module", "source": "rbs"}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeInterface, Name: name, File: filePath,
					Line: lineNum, Exported: isExported(name), Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
			continue
		}

		// interface _Foo
		if strings.HasPrefix(trimmed, "interface ") {
			rest := strings.TrimPrefix(trimmed, "interface ")
			name := strings.Fields(rest)[0]
			if idx := strings.IndexByte(name, '['); idx != -1 {
				name = name[:idx]
			}
			enclosingName = name
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "interface", "source": "rbs"}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeInterface, Name: name, File: filePath,
					Line: lineNum, Exported: true, Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
			continue
		}

		// type alias_name = SomeType
		if strings.HasPrefix(trimmed, "type ") {
			rest := strings.TrimPrefix(trimmed, "type ")
			tokens := strings.Fields(rest)
			if len(tokens) == 0 {
				continue
			}
			name := tokens[0]
			nodeID := g.MakeNodeID(filePath, name)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "alias", "source": "rbs"}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeStruct, Name: name, File: filePath,
					Line: lineNum, Exported: isExported(name), Metadata: meta,
				})
				g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
			continue
		}

		// def method_name: signature  or  def self.method_name: signature
		if strings.HasPrefix(trimmed, "def ") {
			rest := strings.TrimPrefix(trimmed, "def ")
			// Strip leading "self." for singleton methods
			isSingleton := false
			if strings.HasPrefix(rest, "self.") {
				rest = strings.TrimPrefix(rest, "self.")
				isSingleton = true
			}
			// Method name ends at ":" or whitespace
			name := rest
			if idx := strings.IndexAny(name, ": \t"); idx != -1 {
				name = name[:idx]
			}
			if name == "" {
				continue
			}
			qualName := name
			if enclosingName != "" {
				qualName = enclosingName + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "method", "source": "rbs"}
				if isSingleton {
					meta["kind"] = "singleton"
				}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
					Line: lineNum, Exported: isExported(name), Metadata: meta,
				})
				parentNodeID := fileNodeID
				if enclosingName != "" {
					if pn := g.GetNode(g.MakeNodeID(filePath, enclosingName)); pn != nil {
						parentNodeID = g.MakeNodeID(filePath, enclosingName)
					}
				}
				g.AddEdge(&graph.Edge{From: parentNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
			continue
		}

		// attr_accessor / attr_reader / attr_writer name: type
		if strings.HasPrefix(trimmed, "attr_") && enclosingName != "" {
			rest := trimmed
			for _, prefix := range []string{"attr_accessor ", "attr_reader ", "attr_writer "} {
				if strings.HasPrefix(rest, prefix) {
					rest = strings.TrimPrefix(rest, prefix)
					break
				}
			}
			// attr name ends at ":" or whitespace
			attrName := rest
			if idx := strings.IndexAny(attrName, ": \t"); idx != -1 {
				attrName = attrName[:idx]
			}
			if attrName == "" || attrName == rest {
				continue
			}
			qualName := enclosingName + "." + attrName
			nodeID := g.MakeNodeID(filePath, qualName)
			if g.GetNode(nodeID) == nil {
				meta := map[string]string{"kind": "attr", "source": "rbs"}
				g.AddNode(&graph.Node{
					ID: nodeID, Type: graph.NodeMethod, Name: qualName, File: filePath,
					Line: lineNum, Exported: isExported(attrName), Metadata: meta,
				})
				if pn := g.GetNode(g.MakeNodeID(filePath, enclosingName)); pn != nil {
					g.AddEdge(&graph.Edge{From: g.MakeNodeID(filePath, enclosingName), To: nodeID, Type: graph.EdgeDefines})
				} else {
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
			continue
		}

		// end — pop enclosing context (simplified: only one level deep)
		if trimmed == "end" {
			enclosingName = ""
		}
	}

	return nil
}
