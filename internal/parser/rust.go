package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/rust"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// extractRustDeclInfo walks the Rust AST collecting metadata for function,
// struct, enum, trait, type alias, and macro declarations.
// Methods inside impl blocks are qualified as Type.method_name.
func extractRustDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n sitter.Node, implType string, depth int)
	walk = func(n sitter.Node, implType string, depth int) {
		if n.IsNull() || depth > 8 {
			return
		}
		switch n.Type() {
		case "function_item":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if implType != "" {
					qualName = implType + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"block"}),
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "struct_item", "enum_item", "trait_item":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "type_item", "const_item", "static_item":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "macro_definition":
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		case "impl_item":
			// Determine the impl target type.
			typeNode := n.ChildByFieldName("type")
			typeName := ""
			if !typeNode.IsNull() {
				typeName = string(src[typeNode.StartByte():typeNode.EndByte()])
			}
			// Walk the impl body with the type context.
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), typeName, depth+1)
				}
			}
			return
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), implType, depth+1)
		}
	}
	walk(root, "", 0)
	return result
}

// isRustPub checks if a Rust item has the `pub` visibility modifier.
func isRustPub(n sitter.Node, src []byte) bool {
	if n.IsNull() {
		return false
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		ct := child.Type()
		if ct == "visibility_modifier" {
			return true
		}
		// Stop at first non-attribute, non-visibility child.
		if ct != "attribute_item" && ct != "line_comment" && ct != "block_comment" {
			break
		}
	}
	return false
}

// RustParser parses Rust (.rs) source files.
type RustParser struct {
	language *sitter.Language
}

// NewRustParser creates a ready-to-use RustParser.
func NewRustParser() *RustParser {
	return &RustParser{language: sitter.NewLanguage(rust.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *RustParser) Extensions() []string {
	return []string{".rs"}
}

func (p *RustParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single Rust file and merges them into the graph.
func (p *RustParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	tree, _ := parser.ParseString(context.Background(), nil, src)
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
	declInfo := extractRustDeclInfo(root, src)

	// --- use declarations (scoped_identifier) ---
	useQuery := `(use_declaration argument: (scoped_identifier) @use_path)`
	if err := runQuery(lang, root, src, useQuery, func(captures map[string]string, _ int) {
		usePath := captures["use_path"]
		if usePath == "" {
			return
		}
		importNodeID := g.MakeNodeID(usePath, usePath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    usePath,
			Package: usePath,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	}); err != nil {
		return err
	}

	// --- use declarations with use_list: use std::{io, fs} ---
	useListQuery := `(use_declaration argument: (use_as_clause path: (scoped_identifier) @use_path))`
	_ = runQuery(lang, root, src, useListQuery, func(captures map[string]string, _ int) {
		usePath := captures["use_path"]
		if usePath == "" {
			return
		}
		importNodeID := g.MakeNodeID(usePath, usePath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    usePath,
			Package: usePath,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- use with simple identifier: use crate_name; ---
	useSimpleQuery := `(use_declaration argument: (identifier) @use_path)`
	_ = runQuery(lang, root, src, useSimpleQuery, func(captures map[string]string, _ int) {
		usePath := captures["use_path"]
		if usePath == "" {
			return
		}
		importNodeID := g.MakeNodeID(usePath, usePath)
		g.AddNode(&graph.Node{
			ID:      importNodeID,
			Type:    graph.NodePackage,
			Name:    usePath,
			Package: usePath,
			File:    filePath,
		})
		g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
	})

	// --- All declarations via AST walk (handles impl blocks for class-qualification) ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectRustCallSites(g, lang, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks the AST to extract functions, structs, enums,
// traits, type aliases, macros, and impl methods with proper qualification.
func (p *RustParser) extractAllDeclarations(
	g *graph.Graph,
	root sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, implType string)
	walk = func(n sitter.Node, implType string) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "function_item":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			nodeType := graph.NodeFunction
			if implType != "" {
				qualName = implType + "." + name
				nodeType = graph.NodeMethod
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     nodeType,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isRustPub(n, src),
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Link type → method.
			if implType != "" {
				typeID := g.MakeNodeID(filePath, implType)
				if g.GetNode(typeID) != nil {
					g.AddEdge(&graph.Edge{From: typeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}

		case "struct_item":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isRustPub(n, src),
				Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "enum_item":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "enum"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isRustPub(n, src),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			// Walk enum_variant_list to emit individual variant nodes.
			enumPub := isRustPub(n, src)
			for i := uint32(0); i < n.ChildCount(); i++ {
				vl := n.Child(i)
				if vl.IsNull() || vl.Type() != "enum_variant_list" {
					continue
				}
				for j := uint32(0); j < vl.ChildCount(); j++ {
					v := vl.Child(j)
					if v.IsNull() || v.Type() != "enum_variant" {
						continue
					}
					vNameNode := v.ChildByFieldName("name")
					if vNameNode.IsNull() {
						vNameNode = firstChildOfType(v, "identifier")
					}
					if vNameNode.IsNull() {
						continue
					}
					variantName := string(src[vNameNode.StartByte():vNameNode.EndByte()])
					qualName := name + "::" + variantName
					varID := g.MakeNodeID(filePath, qualName)
					if g.GetNode(varID) != nil {
						continue
					}
					varMeta := map[string]string{"kind": "variant"}
					g.AddNode(&graph.Node{
						ID:       varID,
						Type:     graph.NodeMethod,
						Name:     qualName,
						File:     filePath,
						Line:     int(v.StartPoint().Row) + 1,
						Exported: enumPub,
						Metadata: varMeta,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: varID, Type: graph.EdgeDefines})
					g.AddEdge(&graph.Edge{From: nodeID, To: varID, Type: graph.EdgeDefines})
				}
			}

		case "trait_item":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeInterface,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isRustPub(n, src),
				Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "type_item":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "type_alias"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isRustPub(n, src),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "const_item", "static_item":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			kind := "const"
			if n.Type() == "static_item" {
				kind = "static"
			}
			meta["kind"] = kind
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isRustPub(n, src),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "macro_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "macro"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true, // macro_rules! are always usable where imported
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "mod_item":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodePackage,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isRustPub(n, src),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

		case "impl_item":
			// Determine the type being implemented.
			typeNode := n.ChildByFieldName("type")
			typeName := ""
			if !typeNode.IsNull() {
				typeName = string(src[typeNode.StartByte():typeNode.EndByte()])
			}
			// Walk body with type context.
			if body := n.ChildByFieldName("body"); !body.IsNull() {
				for i := uint32(0); i < body.ChildCount(); i++ {
					walk(body.Child(i), typeName)
				}
			}
			return
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), implType)
		}
	}
	walk(root, "")
}

// collectRustCallSites performs a depth-first AST walk to collect call sites
// with function-level caller resolution.
func collectRustCallSites(g *graph.Graph, _ *sitter.Language, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{
			"impl_item": true,
		},
		FuncTypes: map[string]bool{
			"function_item": true,
		},
		CallTypes: map[string]bool{
			"call_expression": true,
		},
		NameExtractor: func(n sitter.Node, src []byte) string {
			if n.Type() == "impl_item" {
				// For impl blocks, the "class" name is the type being implemented.
				if typeNode := n.ChildByFieldName("type"); !typeNode.IsNull() {
					return string(src[typeNode.StartByte():typeNode.EndByte()])
				}
				return ""
			}
			if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		CalleeExtractor: func(n sitter.Node, src []byte) string {
			fn := n.ChildByFieldName("function")
			if fn.IsNull() {
				return ""
			}
			switch fn.Type() {
			case "identifier":
				return string(src[fn.StartByte():fn.EndByte()])
			case "field_expression":
				if field := fn.ChildByFieldName("field"); !field.IsNull() {
					return string(src[field.StartByte():field.EndByte()])
				}
			case "scoped_identifier":
				if nameNode := fn.ChildByFieldName("name"); !nameNode.IsNull() {
					return string(src[nameNode.StartByte():nameNode.EndByte()])
				}
			}
			return ""
		},
		IsBuiltin: isRustBuiltin,
	})
}

// isRustBuiltin returns true for Rust stdlib primitives/functions that should
// not generate CALLS edges.
func isRustBuiltin(name string) bool {
	switch name {
	case "println", "print", "eprintln", "eprint", "format", "write", "writeln",
		"vec", "panic", "todo", "unimplemented", "unreachable",
		"assert", "assert_eq", "assert_ne", "debug_assert",
		"Some", "None", "Ok", "Err",
		"clone", "to_string", "to_owned", "into", "from", "as_ref", "as_mut",
		"unwrap", "expect", "unwrap_or", "unwrap_or_else", "unwrap_or_default",
		"map", "and_then", "or_else", "filter", "collect", "iter", "into_iter",
		"push", "pop", "len", "is_empty", "contains", "get", "insert", "remove",
		"String", "Vec", "Box", "Rc", "Arc", "Cell", "RefCell",
		"HashMap", "HashSet", "BTreeMap", "BTreeSet",
		"Display", "Debug", "Default", "Clone", "Copy", "Send", "Sync":
		return true
	}
	return false
}
