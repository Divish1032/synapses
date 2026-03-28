package parser

import (
	"path/filepath"
	"strings"

	solidityg "github.com/alexaandru/go-sitter-forest/solidity"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// isSolidityExported checks if a Solidity declaration has public or external visibility.
// It searches direct children for a "visibility" node containing "public" or "external".
func isSolidityExported(n sitter.Node, src []byte) bool {
	if n.IsNull() {
		return false
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "visibility" {
			vis := string(src[child.StartByte():child.EndByte()])
			return vis == "public" || vis == "external"
		}
	}
	return false
}

// extractSolidityDeclInfo walks the Solidity AST collecting metadata for
// contract, interface, library, function, modifier, event, and state variable declarations.
// Functions/modifiers/events inside contracts are qualified as ContractName.memberName.
func extractSolidityDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")

	var walk func(n sitter.Node, contractName string, depth int)
	walk = func(n sitter.Node, contractName string, depth int) {
		if n.IsNull() || depth > 10 {
			return
		}
		switch n.Type() {
		case "contract_declaration", "interface_declaration", "library_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
				// Walk children with contract context.
				for i := uint32(0); i < n.ChildCount(); i++ {
					walk(n.Child(i), name, depth+1)
				}
				return
			}

		case "function_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if contractName != "" {
					qualName = contractName + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"function_body", "block"}),
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}

		case "constructor_definition":
			if contractName != "" {
				qualName := contractName + ".constructor"
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"function_body", "block"}),
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}

		case "modifier_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if contractName != "" {
					qualName = contractName + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Signature: extractSigToBodyMulti(n, src, []string{"function_body", "block"}),
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}

		case "event_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if contractName != "" {
					qualName = contractName + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}

		case "state_variable_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if contractName != "" {
					qualName = contractName + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}

		case "struct_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if contractName != "" {
					qualName = contractName + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}

		case "enum_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if !nameNode.IsNull() {
				name := string(src[nameNode.StartByte():nameNode.EndByte()])
				qualName := name
				if contractName != "" {
					qualName = contractName + "." + name
				}
				sl := int(n.StartPoint().Row) + 1
				result[qualName] = declMeta{
					Doc:       extractLineDoc(lines, sl, "///"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		}

		// Default: walk children (unless already handled by contract/interface/library above).
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), contractName, depth+1)
		}
	}
	walk(root, "", 0)
	return result
}

// SolidityParser parses Solidity (.sol) source files.
type SolidityParser struct {
	language *sitter.Language
}

// NewSolidityParser creates a ready-to-use SolidityParser.
func NewSolidityParser() *SolidityParser {
	return &SolidityParser{language: sitter.NewLanguage(solidityg.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *SolidityParser) Extensions() []string {
	return []string{".sol"}
}

// TSLanguageForFile returns the tree-sitter language for the given file.
func (p *SolidityParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single Solidity file and merges them into the graph.
func (p *SolidityParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	declInfo := extractSolidityDeclInfo(root, src)

	// --- Extract all declarations via AST walk ---
	p.extractAllDeclarations(g, root, src, filePath, fileNodeID, declInfo)

	// --- Call sites ---
	collectSolidityCallSites(g, root, src, filePath, fileNodeID)

	return nil
}

// extractAllDeclarations walks the AST to extract contracts, interfaces, libraries,
// functions, constructors, modifiers, events, state variables, structs, and enums
// with proper contract-qualification.
func (p *SolidityParser) extractAllDeclarations(
	g *graph.Graph,
	root sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
	declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node, contractName string, contractNodeID graph.NodeID)
	walk = func(n sitter.Node, contractName string, contractNodeID graph.NodeID) {
		if n.IsNull() {
			return
		}
		switch n.Type() {
		case "contract_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "contract"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true, // contracts are always externally visible
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

			// Extract inheritance relationships.
			p.extractInheritance(g, n, src, filePath, nodeID)

			// Extract import-like relationships from inheritance_specifier.
			// Walk contract body children with contract context.
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), name, nodeID)
			}
			return

		case "interface_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
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
				Exported: true,
				Metadata: buildLangMeta(declInfo[name]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

			// Extract inheritance for interfaces too (interface IERC20 is IERC165).
			p.extractInheritance(g, n, src, filePath, nodeID)

			// Walk children with interface context.
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), name, nodeID)
			}
			return

		case "library_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			nodeID := g.MakeNodeID(filePath, name)
			meta := buildLangMeta(declInfo[name])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "library"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     name,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})

			// Walk children with library context.
			for i := uint32(0); i < n.ChildCount(); i++ {
				walk(n.Child(i), name, nodeID)
			}
			return

		case "import_directive":
			p.extractImport(g, n, src, filePath, fileNodeID)

		case "function_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			nodeType := graph.NodeFunction
			if contractName != "" {
				qualName = contractName + "." + name
				nodeType = graph.NodeMethod
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     nodeType,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isSolidityExported(n, src),
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if contractName != "" && contractNodeID != "" {
				g.AddEdge(&graph.Edge{From: contractNodeID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "constructor_definition":
			if contractName == "" {
				break
			}
			qualName := contractName + ".constructor"
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeMethod,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true, // constructors are always callable
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if contractNodeID != "" {
				g.AddEdge(&graph.Edge{From: contractNodeID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "modifier_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if contractName != "" {
				qualName = contractName + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := buildLangMeta(declInfo[qualName])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "modifier"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeFunction,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isSolidityExported(n, src),
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if contractName != "" && contractNodeID != "" {
				g.AddEdge(&graph.Edge{From: contractNodeID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "event_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if contractName != "" {
				qualName = contractName + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := buildLangMeta(declInfo[qualName])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "event"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true, // events are always visible
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if contractName != "" && contractNodeID != "" {
				g.AddEdge(&graph.Edge{From: contractNodeID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "state_variable_declaration":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if contractName != "" {
				qualName = contractName + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeVariable,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: isSolidityExported(n, src),
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if contractName != "" && contractNodeID != "" {
				g.AddEdge(&graph.Edge{From: contractNodeID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "struct_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if contractName != "" {
				qualName = contractName + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true,
				Metadata: buildLangMeta(declInfo[qualName]),
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if contractName != "" && contractNodeID != "" {
				g.AddEdge(&graph.Edge{From: contractNodeID, To: nodeID, Type: graph.EdgeDefines})
			}

		case "enum_definition":
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if nameNode.IsNull() {
				break
			}
			name := string(src[nameNode.StartByte():nameNode.EndByte()])
			qualName := name
			if contractName != "" {
				qualName = contractName + "." + name
			}
			nodeID := g.MakeNodeID(filePath, qualName)
			meta := buildLangMeta(declInfo[qualName])
			if meta == nil {
				meta = make(map[string]string, 1)
			}
			meta["kind"] = "enum"
			g.AddNode(&graph.Node{
				ID:       nodeID,
				Type:     graph.NodeStruct,
				Name:     qualName,
				File:     filePath,
				Line:     int(n.StartPoint().Row) + 1,
				Exported: true,
				Metadata: meta,
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
			if contractName != "" && contractNodeID != "" {
				g.AddEdge(&graph.Edge{From: contractNodeID, To: nodeID, Type: graph.EdgeDefines})
			}
		}

		// Default: walk children (unless already handled by contract/interface/library above).
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), contractName, contractNodeID)
		}
	}
	walk(root, "", "")
}

// extractImport extracts an import path from an import_directive node and
// creates an IMPORTS edge from the file node to a package node.
func (p *SolidityParser) extractImport(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	fileNodeID graph.NodeID,
) {
	// import_directive contains a string child with the path.
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "string" || child.Type() == "string_literal" {
			raw := string(src[child.StartByte():child.EndByte()])
			// Strip quotes.
			importPath := strings.Trim(raw, "\"'")
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
			})
			g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
			return
		}
		// Also handle source_import or import_clause containing a string.
		if child.Type() == "source_import" || child.Type() == "import_clause" {
			for j := uint32(0); j < child.ChildCount(); j++ {
				gc := child.Child(j)
				if gc.IsNull() {
					continue
				}
				if gc.Type() == "string" || gc.Type() == "string_literal" {
					raw := string(src[gc.StartByte():gc.EndByte()])
					importPath := strings.Trim(raw, "\"'")
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
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
					return
				}
			}
		}
	}
}

// extractInheritance extracts inheritance relationships from a contract or interface
// declaration. Each base contract in the inheritance_specifier creates a CONTAINS edge
// from the base to the derived type.
func (p *SolidityParser) extractInheritance(
	g *graph.Graph,
	n sitter.Node,
	src []byte,
	filePath string,
	derivedNodeID graph.NodeID,
) {
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "inheritance_specifier" {
			// Walk inside to find user_defined_type or identifier nodes (base contract names).
			for j := uint32(0); j < child.ChildCount(); j++ {
				base := child.Child(j)
				if base.IsNull() {
					continue
				}
				baseType := base.Type()
				if baseType == "user_defined_type" || baseType == "identifier" {
					baseName := string(src[base.StartByte():base.EndByte()])
					if baseName == "" {
						continue
					}
					baseNodeID := g.MakeNodeID(filePath, baseName)
					// Ensure the base node exists (it may be defined in another file).
					if g.GetNode(baseNodeID) == nil {
						g.AddNode(&graph.Node{
							ID:       baseNodeID,
							Type:     graph.NodeInterface,
							Name:     baseName,
							File:     filePath,
							Line:     int(base.StartPoint().Row) + 1,
							Exported: true,
						})
					}
					g.AddEdge(&graph.Edge{From: baseNodeID, To: derivedNodeID, Type: graph.EdgeContains})
				}
			}
		}
	}
}

// collectSolidityCallSites performs a depth-first AST walk to collect call sites
// with function-level caller resolution.
func collectSolidityCallSites(g *graph.Graph, root sitter.Node, src []byte, filePath string, fileNodeID graph.NodeID) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: map[string]bool{
			"contract_declaration":  true,
			"interface_declaration": true,
			"library_declaration":   true,
		},
		FuncTypes: map[string]bool{
			"function_definition":    true,
			"constructor_definition": true,
			"modifier_definition":    true,
		},
		CallTypes: map[string]bool{
			"call_expression": true,
		},
		NameExtractor: func(n sitter.Node, src []byte) string {
			if n.Type() == "constructor_definition" {
				return "constructor"
			}
			nameNode := n.ChildByFieldName("name")
			if nameNode.IsNull() {
				nameNode = firstChildOfType(n, "identifier")
			}
			if !nameNode.IsNull() {
				return string(src[nameNode.StartByte():nameNode.EndByte()])
			}
			return ""
		},
		CalleeExtractor: func(n sitter.Node, src []byte) string {
			// call_expression: the function child is the callee.
			fn := n.ChildByFieldName("function")
			if fn.IsNull() {
				// Fallback: first child may be the callee.
				if n.ChildCount() > 0 {
					fn = n.Child(0)
				}
			}
			if fn.IsNull() {
				return ""
			}
			switch fn.Type() {
			case "identifier":
				return string(src[fn.StartByte():fn.EndByte()])
			case "member_expression", "member_access":
				// obj.method() — extract the method name (last identifier).
				if prop := fn.ChildByFieldName("property"); !prop.IsNull() {
					return string(src[prop.StartByte():prop.EndByte()])
				}
				if fn.ChildCount() > 0 {
					last := fn.Child(fn.ChildCount() - 1)
					if !last.IsNull() && last.Type() == "identifier" {
						return string(src[last.StartByte():last.EndByte()])
					}
				}
			}
			return ""
		},
		IsBuiltin: isSolidityBuiltin,
	})
}

// isSolidityBuiltin returns true for Solidity built-in functions/globals that
// should not generate CALLS edges.
func isSolidityBuiltin(name string) bool {
	switch name {
	case "require", "assert", "revert",
		"keccak256", "sha256", "ripemd160", "ecrecover",
		"abi", "encode", "encodePacked", "encodeWithSelector", "encodeWithSignature",
		"decode", "encodeCall",
		"addmod", "mulmod",
		"selfdestruct", "suicide",
		"blockhash", "gasleft",
		"msg", "block", "tx",
		"this", "super",
		"type", "new",
		"push", "pop", "length",
		"send", "transfer", "call", "delegatecall", "staticcall",
		"emit", "log0", "log1", "log2", "log3", "log4":
		return true
	}
	return false
}
