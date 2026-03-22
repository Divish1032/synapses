package parser

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	sitter "github.com/alexaandru/go-tree-sitter-bare"
	"github.com/alexaandru/go-sitter-forest/sql"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// SQLParser parses SQL (.sql) source files, extracting CREATE TABLE, VIEW,
// FUNCTION, PROCEDURE, TRIGGER, and INDEX declarations as graph nodes.
type SQLParser struct {
	language *sitter.Language
}

// NewSQLParser creates a ready-to-use SQLParser.
func NewSQLParser() *SQLParser {
	return &SQLParser{language: sitter.NewLanguage(sql.GetLanguage())}
}

// Extensions returns the file extensions handled by this parser.
func (p *SQLParser) Extensions() []string {
	return []string{".sql"}
}

func (p *SQLParser) TSLanguageForFile(_ string) *sitter.Language { return p.language }

// Parse extracts code entities from a single SQL file and merges them into the graph.
//
// Strategy: tree-sitter SQL grammars vary widely in their node types across
// versions. Instead of relying on specific statement types, we walk the AST
// recursively and inspect the source text of each node for CREATE keywords.
// This makes the parser resilient to grammar changes while still leveraging
// tree-sitter for accurate statement boundary detection.
func (p *SQLParser) Parse(g *graph.Graph, filePath string, src []byte) error {
	parser := sitter.NewParser()
	parser.SetLanguage(p.language)

	parseCtx, parseCancel := parseContext()
	defer parseCancel()
	tree, _ := parser.ParseString(parseCtx, nil, src)
	if tree != nil {
		defer tree.Close()
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

	lines := strings.Split(string(src), "\n")

	// Track which CREATE statements we have already emitted so we don't
	// produce duplicates when both a parent and child node match.
	emitted := make(map[string]bool)

	// Walk AST to find CREATE statements. We use a greedy top-down approach:
	// try to match at every node. When we find a CREATE match, emit the node
	// and stop recursing into that subtree (the entity has been captured).
	// We skip the root node itself (depth 0) since its text spans the entire
	// file and would greedily match only the first CREATE statement.
	var walk func(n sitter.Node, depth int)
	walk = func(n sitter.Node, depth int) {
		if n.IsNull() || depth > 50 {
			return
		}

		// At depth > 0, try to match CREATE statements.
		if depth > 0 {
			nodeText := string(src[n.StartByte():n.EndByte()])
			upperText := strings.ToUpper(strings.TrimSpace(nodeText))

			if strings.HasPrefix(upperText, "CREATE") {
				if info := sqlParseCreate(upperText, nodeText); info.name != "" && !emitted[info.name] {
					startLine := int(n.StartPoint().Row) + 1
					endLine := int(n.EndPoint().Row) + 1
					doc := extractLineDoc(lines, startLine, "--")
					lineCount := endLine - startLine + 1

					meta := make(map[string]string, 4)
					meta["kind"] = info.kind
					if doc != "" {
						meta["doc"] = doc
					}
					if lineCount > 0 {
						meta["line_count"] = strconv.Itoa(lineCount)
					}

					nodeID := g.MakeNodeID(filePath, info.name)
					if g.GetNode(nodeID) == nil {
						g.AddNode(&graph.Node{
							ID:       nodeID,
							Type:     info.nodeType,
							Name:     info.name,
							File:     filePath,
							Line:     startLine,
							Exported: true,
							Metadata: meta,
						})
						g.AddEdge(&graph.Edge{From: fileNodeID, To: nodeID, Type: graph.EdgeDefines})
					}
					emitted[info.name] = true
					return // Don't recurse into children of a matched CREATE statement.
				}
			}
		}

		for i := uint32(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), depth+1)
		}
	}
	walk(root, 0)

	return nil
}

// sqlCreateInfo holds parsed information about a CREATE statement.
type sqlCreateInfo struct {
	name     string
	kind     string         // "table", "view", "function", "procedure", "trigger", "index"
	nodeType graph.NodeType // NodeStruct, NodeFunction, or NodeVariable
}

// sqlNamePattern matches the first identifier (possibly schema-qualified).
// Handles backtick-quoted, double-quoted, bracket-quoted, and plain identifiers.
// The caller is responsible for stripping IF NOT EXISTS / CONCURRENTLY prefixes
// before applying this pattern.
var sqlNamePattern = regexp.MustCompile(
	`(?:` +
		"`[^`]+`" + // backtick-quoted
		`|` +
		`"[^"]+"` + // double-quoted
		`|` +
		`\[[^\]]+\]` + // bracket-quoted [dbo].[name]
		`|` +
		`[A-Za-z_][A-Za-z0-9_]*` + // plain identifier
		`)` +
		`(?:` +
		`\.` +
		`(?:` +
		"`[^`]+`" +
		`|` +
		`"[^"]+"` +
		`|` +
		`\[[^\]]+\]` +
		`|` +
		`[A-Za-z_][A-Za-z0-9_]*` +
		`)` +
		`)*`, // optional .schema.name segments
)

// sqlStripPrefixes is a regex that removes SQL noise keywords that appear
// between the CREATE <type> and the entity name.
var sqlStripPrefixes = regexp.MustCompile(`(?i)^(?:IF\s+NOT\s+EXISTS\s+|CONCURRENTLY\s+)`)

// sqlParseCreate examines the uppercase and original text of a CREATE statement
// and returns the entity name, kind, and node type. Returns an empty sqlCreateInfo
// if the statement doesn't match any known CREATE pattern.
// sqlNormalizeSpace collapses runs of whitespace to a single space and trims.
var sqlMultiSpace = regexp.MustCompile(`\s+`)

func sqlNormalizeSpace(s string) string {
	return strings.TrimSpace(sqlMultiSpace.ReplaceAllString(s, " "))
}

func sqlParseCreate(upperText, originalText string) sqlCreateInfo {
	// Normalize multi-space so "CREATE  TABLE" matches "CREATE TABLE".
	upperText = sqlNormalizeSpace(upperText)
	originalText = sqlNormalizeSpace(originalText)

	type createPattern struct {
		prefix   string
		kind     string
		nodeType graph.NodeType
	}

	patterns := []createPattern{
		// OR REPLACE variants (must come before non-OR-REPLACE).
		{"CREATE OR REPLACE FUNCTION", "function", graph.NodeFunction},
		{"CREATE OR REPLACE PROCEDURE", "procedure", graph.NodeFunction},
		{"CREATE OR REPLACE TRIGGER", "trigger", graph.NodeFunction},
		{"CREATE OR REPLACE VIEW", "view", graph.NodeStruct},
		{"CREATE OR REPLACE TYPE", "type", graph.NodeStruct},
		{"CREATE OR REPLACE AGGREGATE", "aggregate", graph.NodeFunction},
		// Standard variants.
		{"CREATE UNLOGGED TABLE", "table", graph.NodeStruct},
		{"CREATE GLOBAL TEMPORARY TABLE", "table", graph.NodeStruct},
		{"CREATE GLOBAL TEMP TABLE", "table", graph.NodeStruct},
		{"CREATE LOCAL TEMPORARY TABLE", "table", graph.NodeStruct},
		{"CREATE LOCAL TEMP TABLE", "table", graph.NodeStruct},
		{"CREATE TEMPORARY TABLE", "table", graph.NodeStruct},
		{"CREATE TEMP TABLE", "table", graph.NodeStruct},
		{"CREATE TABLE", "table", graph.NodeStruct},
		{"CREATE MATERIALIZED VIEW", "view", graph.NodeStruct},
		{"CREATE VIEW", "view", graph.NodeStruct},
		{"CREATE FUNCTION", "function", graph.NodeFunction},
		{"CREATE PROCEDURE", "procedure", graph.NodeFunction},
		{"CREATE TRIGGER", "trigger", graph.NodeFunction},
		{"CREATE UNIQUE INDEX", "index", graph.NodeVariable},
		{"CREATE INDEX", "index", graph.NodeVariable},
		// Schema and extension definitions.
		{"CREATE SCHEMA", "schema", graph.NodeStruct},
		{"CREATE EXTENSION", "extension", graph.NodePackage},
		{"CREATE AGGREGATE", "aggregate", graph.NodeFunction},
		// Type and sequence definitions.
		{"CREATE TYPE", "type", graph.NodeStruct},
		{"CREATE SEQUENCE", "sequence", graph.NodeVariable},
		{"CREATE DOMAIN", "domain", graph.NodeStruct},
	}

	for _, pat := range patterns {
		if !strings.HasPrefix(upperText, pat.prefix) {
			continue
		}

		// Extract the remainder after the prefix from the original text.
		rest := strings.TrimSpace(originalText[len(pat.prefix):])
		if rest == "" {
			continue
		}

		name := sqlExtractName(rest)
		if name == "" {
			continue
		}

		// For functions/procedures, strip trailing parentheses and parameters.
		if pat.kind == "function" || pat.kind == "procedure" {
			if idx := strings.Index(name, "("); idx != -1 {
				name = name[:idx]
			}
		}

		return sqlCreateInfo{
			name:     name,
			kind:     pat.kind,
			nodeType: pat.nodeType,
		}
	}

	return sqlCreateInfo{}
}

// sqlExtractName extracts the entity name (possibly schema-qualified) from the
// text following a CREATE <TYPE> prefix. Strips IF NOT EXISTS, CONCURRENTLY,
// and handles quoted identifiers.
func sqlExtractName(rest string) string {
	// Strip noise keywords between the type keyword and the name.
	cleaned := sqlStripPrefixes.ReplaceAllString(rest, "")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		return ""
	}
	loc := sqlNamePattern.FindStringIndex(cleaned)
	if loc == nil {
		return ""
	}
	raw := cleaned[loc[0]:loc[1]]
	return sqlUnquoteName(raw)
}

// sqlUnquoteName strips quoting characters from a SQL identifier.
// Handles backtick, double-quote, and bracket quoting, including
// schema-qualified names like `myschema`.`users` or [dbo].[users].
func sqlUnquoteName(name string) string {
	parts := strings.Split(name, ".")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		// Backtick-quoted.
		if strings.HasPrefix(part, "`") && strings.HasSuffix(part, "`") && len(part) > 1 {
			part = part[1 : len(part)-1]
		}
		// Double-quoted.
		if strings.HasPrefix(part, `"`) && strings.HasSuffix(part, `"`) && len(part) > 1 {
			part = part[1 : len(part)-1]
		}
		// Bracket-quoted.
		if strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]") && len(part) > 1 {
			part = part[1 : len(part)-1]
		}
		parts[i] = part
	}
	return strings.Join(parts, ".")
}
