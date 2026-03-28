package parser

import (
	"path/filepath"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/bash"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// BashParser parses Bash/Shell (.sh, .bash) source files.
type BashParser struct {
	language *sitter.Language
}

// NewBashParser creates a ready-to-use BashParser.
func NewBashParser() *BashParser {
	return &BashParser{language: sitter.NewLanguage(bash.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *BashParser) Extensions() []string {
	return []string{".sh", ".bash", ".zsh"}
}

func (p *BashParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// extractBashDeclInfo performs a pre-pass collecting metadata for function
// definitions. It extracts doc comments (# lines above functions) and line counts.
func extractBashDeclInfo(root sitter.Node, src []byte) map[string]declMeta {
	result := make(map[string]declMeta)
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "function_definition" {
			name := extractBashFuncName(n, src)
			if name != "" {
				sl := int(n.StartPoint().Row) + 1
				result[name] = declMeta{
					Doc:       extractLineDoc(lines, sl, "#"),
					LineCount: int(n.EndPoint().Row) - int(n.StartPoint().Row) + 1,
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return result
}

// extractBashFuncName extracts the function name from a function_definition node.
// Handles both `function foo() { ... }` and `foo() { ... }` styles.
// The bash grammar uses the "name" field for the function name.
func extractBashFuncName(n sitter.Node, src []byte) string {
	// Try the "name" field first (tree-sitter bash grammar uses this).
	if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
		return strings.TrimSpace(string(src[nameNode.StartByte():nameNode.EndByte()]))
	}
	// Fallback: look for a "word" child (the function name token).
	if wordNode := firstChildOfType(n, "word"); !wordNode.IsNull() {
		return strings.TrimSpace(string(src[wordNode.StartByte():wordNode.EndByte()]))
	}
	return ""
}

// Parse extracts code entities from a single Bash/Shell file.
func (p *BashParser) Parse(g *graph.Graph, filePath string, src []byte) error {
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

	declInfo := extractBashDeclInfo(root, src)

	// --- Function definitions ---
	// Extracts both `function foo() { ... }` and `foo() { ... }` styles.
	p.extractFunctions(g, root, src, filePath, fileNodeID, declInfo)

	// --- Source/dot imports ---
	// `source file.sh` and `. file.sh` create import edges.
	p.extractSourceImports(g, root, src, filePath, fileNodeID)

	// --- Aliases ---
	// `alias name='...'` creates a function node with kind="alias".
	p.extractAliases(g, root, src, filePath, fileNodeID)

	// --- Call sites ---
	collectBashCallSites(g, root, src, filePath, fileNodeID)

	return nil
}

// extractFunctions walks the AST extracting function definitions.
func (p *BashParser) extractFunctions(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID, declInfo map[string]declMeta,
) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "function_definition" {
			name := extractBashFuncName(n, src)
			if name != "" {
				nodeID := g.MakeNodeID(filePath, name)
				if g.GetNode(nodeID) == nil {
					g.AddNode(&graph.Node{
						ID:       nodeID,
						Type:     graph.NodeFunction,
						Name:     name,
						File:     filePath,
						Line:     int(n.StartPoint().Row) + 1,
						Exported: true, // Bash has no visibility modifiers
						Metadata: buildLangMeta(declInfo[name]),
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// extractSourceImports walks the AST looking for `source` and `.` commands
// that import other shell files.
func (p *BashParser) extractSourceImports(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "command" {
			cmdName := extractBashCommandName(n, src)
			if cmdName == "source" || cmdName == "." {
				// Extract the file path argument (first argument after the command name).
				importPath := extractBashFirstArg(n, src)
				if importPath != "" {
					// Strip quotes if present.
					importPath = strings.Trim(importPath, `"'`)
					importNodeID := g.MakeNodeID(importPath, importPath)
					g.AddNode(&graph.Node{
						ID:      importNodeID,
						Type:    graph.NodePackage,
						Name:    filepath.Base(importPath),
						Package: importPath,
						File:    filePath,
					})
					g.AddEdge(&graph.Edge{From: fileNodeID, To: importNodeID, Type: graph.EdgeImports})
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// extractAliases walks the AST looking for alias declarations.
// In the bash grammar, `alias ll='ls -la'` is parsed as a command
// with command_name "alias" and arguments. We also handle the
// `variable_assignment` form inside alias commands.
func (p *BashParser) extractAliases(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	lines := strings.Split(string(src), "\n")
	var walk func(n sitter.Node)
	walk = func(n sitter.Node) {
		if n.IsNull() {
			return
		}
		if n.Type() == "command" {
			cmdName := extractBashCommandName(n, src)
			if cmdName == "alias" {
				// Each argument after "alias" is a name=value assignment.
				// In the bash grammar these appear as children of the command node.
				for i := uint32(0); i < n.ChildCount(); i++ {
					child := n.Child(i)
					if child.IsNull() {
						continue
					}
					// Skip the command_name node itself.
					if child.Type() == "command_name" {
						continue
					}
					// The alias argument can be a word like "ll='ls -la'"
					// or a concatenation node. Extract the name before '='.
					text := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
					if eqIdx := strings.Index(text, "="); eqIdx > 0 {
						aliasName := text[:eqIdx]
						nodeID := g.MakeNodeID(filePath, aliasName)
						if g.GetNode(nodeID) == nil {
							sl := int(n.StartPoint().Row) + 1
							meta := map[string]string{"kind": "alias"}
							doc := extractLineDoc(lines, sl, "#")
							if doc != "" {
								meta["doc"] = doc
							}
							g.AddNode(&graph.Node{
								ID:       nodeID,
								Type:     graph.NodeFunction,
								Name:     aliasName,
								File:     filePath,
								Line:     sl,
								Exported: true,
								Metadata: meta,
							})
							g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
						}
					}
				}
			}
		}
		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
}

// extractBashCommandName returns the command name from a command node.
// In the bash grammar, command has a "name" field containing command_name,
// which itself contains a word child.
func extractBashCommandName(n sitter.Node, src []byte) string {
	// Try field-based access first.
	if nameNode := n.ChildByFieldName("name"); !nameNode.IsNull() {
		return strings.TrimSpace(string(src[nameNode.StartByte():nameNode.EndByte()]))
	}
	// Fallback: look for command_name child.
	if cmdNameNode := firstChildOfType(n, "command_name"); !cmdNameNode.IsNull() {
		return strings.TrimSpace(string(src[cmdNameNode.StartByte():cmdNameNode.EndByte()]))
	}
	return ""
}

// extractBashFirstArg returns the text of the first argument to a command,
// skipping the command_name node.
func extractBashFirstArg(n sitter.Node, src []byte) string {
	seenName := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		child := n.Child(i)
		if child.IsNull() {
			continue
		}
		if child.Type() == "command_name" {
			seenName = true
			continue
		}
		if seenName {
			text := strings.TrimSpace(string(src[child.StartByte():child.EndByte()]))
			if text != "" {
				return text
			}
		}
	}
	return ""
}

// collectBashCallSites collects call sites from command invocations within
// function bodies, with function-level caller resolution.
func collectBashCallSites(
	g *graph.Graph, root sitter.Node, src []byte,
	filePath string, fileNodeID graph.NodeID,
) {
	collectCallSitesWalk(g, root, src, filePath, fileNodeID, callSiteConfig{
		ClassTypes: nil, // Bash has no classes
		FuncTypes: map[string]bool{
			"function_definition": true,
		},
		CallTypes: map[string]bool{
			"command": true,
		},
		NameExtractor: func(n sitter.Node, s []byte) string {
			if n.Type() == "function_definition" {
				return extractBashFuncName(n, s)
			}
			return ""
		},
		CalleeExtractor: func(n sitter.Node, s []byte) string {
			cmdName := extractBashCommandName(n, s)
			return cmdName
		},
		IsBuiltin: isBashBuiltin,
	})
}

// isBashBuiltin returns true if the command name is a shell builtin or
// common utility that should not generate call-site edges.
func isBashBuiltin(name string) bool {
	switch name {
	case "echo", "cd", "ls", "cat", "grep", "sed", "awk", "find",
		"test", "[", "[[", "printf", "read", "export", "local",
		"declare", "readonly", "unset", "eval", "exec", "exit",
		"return", "shift", "trap", "wait", "kill", "true", "false",
		"set", "source", ".", "alias", "unalias",
		"if", "then", "else", "elif", "fi",
		"for", "while", "until", "do", "done",
		"case", "esac", "in",
		"break", "continue",
		"function",
		"mv", "cp", "rm", "mkdir", "rmdir", "touch", "chmod", "chown",
		"head", "tail", "sort", "uniq", "wc", "cut", "tr", "tee",
		"xargs", "basename", "dirname", "realpath", "readlink",
		"date", "sleep", "env", "which", "type", "command",
		"getopts", "let", "expr", "typeset", "builtin",
		"pushd", "popd", "dirs", "hash", "times", "umask",
		// Array / advanced builtins
		"mapfile", "readarray", "select", "compgen", "complete", "compopt",
		"coproc", "disown", "enable", "fc", "fg", "bg", "jobs", "suspend",
		"logout", "newgrp", "ulimit", "caller", "bind",
		// Common system commands treated as builtins in scripts
		"sudo", "su", "systemctl", "service", "apt", "apt-get", "yum",
		"brew", "curl", "wget", "git", "make", "npm", "yarn", "pip",
		"python", "python3", "node", "ruby", "go", "java", "docker",
		"ssh", "scp", "rsync", "tar", "gzip", "gunzip", "zip", "unzip",
		"ln", "stat", "file", "lsof", "ps", "pkill", "pgrep",
		"tput", "stty", "perl", "jq", "bc", "seq",
		// zsh-specific builtins
		"autoload", "zmodload", "zle", "zparseopts", "zcompdef",
		"zstyle", "zformat", "zftp", "zprof", "zpty",
		"add-zsh-hook", "add-zle-hook-widget",
		"vared", "sched", "zstat":
		return true
	}
	return false
}
