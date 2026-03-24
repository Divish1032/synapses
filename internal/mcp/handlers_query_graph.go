package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// query_graph — constrained DSL for direct graph node filtering.
//
// Grammar:
//
//	query  := "NODES" "WHERE" predicate
//	predicate := comparison ("AND" comparison)*
//	comparison := field operator value
//	field  := "package" | "type" | "domain" | "file" | "name" | "exported" | "fanin" | "fanout"
//	operator := "=" | "!=" | ">" | ">=" | "<" | "<="
//	value  := quoted-string | unquoted-word | integer | bool
//
// Constraints:
//   - Read-only, 1000 node cap, 500 ms timeout.
//   - Only AND connectors supported (no OR, no nested parens).
//   - Numeric comparisons (>, >=, <, <=) only for fanin and fanout fields.

const (
	queryGraphNodeCap = 1000
	queryGraphTimeout = 500 * time.Millisecond
)

// unescapeStringLiteral converts raw content between double-quotes into the
// intended string value by processing backslash escape sequences one character
// at a time. Supported: \\ → \, \" → ". Any other \X passes through unchanged.
// This avoids the overlapping-pattern bug that strings.ReplaceAll chains have
// when the input contains sequences like \\" (should give \, not \").
func unescapeStringLiteral(raw string) string {
	if !strings.ContainsRune(raw, '\\') {
		return raw // fast path: nothing to unescape
	}
	var b strings.Builder
	b.Grow(len(raw))
	i := 0
	for i < len(raw) {
		if raw[i] == '\\' && i+1 < len(raw) {
			switch raw[i+1] {
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			default:
				b.WriteByte('\\')
				b.WriteByte(raw[i+1])
			}
			i += 2
			continue
		}
		b.WriteByte(raw[i])
		i++
	}
	return b.String()
}

// ── Tokenizer ────────────────────────────────────────────────────────────────

type tokenKind int

const (
	tokWord     tokenKind = iota // bareword identifier or keyword
	tokString                    // double-quoted string literal
	tokNumber                    // integer literal
	tokOp                        // operator: = != > >= < <=
	tokEOF
)

type token struct {
	kind  tokenKind
	value string // raw lexeme
}

// tokenize breaks a query string into tokens.
// Returns an error on unterminated string literals.
func tokenize(s string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(s) {
		// Skip whitespace.
		if unicode.IsSpace(rune(s[i])) {
			i++
			continue
		}

		// Double-quoted string literal.
		if s[i] == '"' {
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++ // skip escape character
				}
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated string literal starting at position %d", i)
			}
			// Unescape backslash sequences character by character so that overlapping
			// patterns (e.g. \\") are handled correctly: \\ → \, \" → ".
			raw := unescapeStringLiteral(s[i+1 : j])
			tokens = append(tokens, token{kind: tokString, value: raw})
			i = j + 1
			continue
		}

		// Two-character operators: !=, >=, <=.
		if i+1 < len(s) {
			two := s[i : i+2]
			if two == "!=" || two == ">=" || two == "<=" {
				tokens = append(tokens, token{kind: tokOp, value: two})
				i += 2
				continue
			}
		}

		// Single-character operators: =, >, <.
		if s[i] == '=' || s[i] == '>' || s[i] == '<' {
			tokens = append(tokens, token{kind: tokOp, value: string(s[i])})
			i++
			continue
		}

		// Number literal (positive integer).
		if unicode.IsDigit(rune(s[i])) {
			j := i
			for j < len(s) && unicode.IsDigit(rune(s[j])) {
				j++
			}
			tokens = append(tokens, token{kind: tokNumber, value: s[i:j]})
			i = j
			continue
		}

		// Bareword: identifier, keyword, boolean, unquoted value.
		if unicode.IsLetter(rune(s[i])) || s[i] == '_' || s[i] == '.' || s[i] == '/' || s[i] == '-' {
			j := i
			for j < len(s) && (unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])) ||
				s[j] == '_' || s[j] == '.' || s[j] == '/' || s[j] == '-') {
				j++
			}
			tokens = append(tokens, token{kind: tokWord, value: s[i:j]})
			i = j
			continue
		}

		return nil, fmt.Errorf("unexpected character %q at position %d", s[i], i)
	}
	tokens = append(tokens, token{kind: tokEOF})
	return tokens, nil
}

// ── Parser ───────────────────────────────────────────────────────────────────

// graphQueryField enumerates the node attributes usable in WHERE clauses.
type graphQueryField string

const (
	gqfPackage  graphQueryField = "package"
	gqfType     graphQueryField = "type"
	gqfDomain   graphQueryField = "domain"
	gqfFile     graphQueryField = "file"
	gqfName     graphQueryField = "name"
	gqfExported graphQueryField = "exported"
	gqfFanIn    graphQueryField = "fanin"
	gqfFanOut   graphQueryField = "fanout"
)

var validGraphQueryFields = map[string]graphQueryField{
	"package":  gqfPackage,
	"type":     gqfType,
	"domain":   gqfDomain,
	"file":     gqfFile,
	"name":     gqfName,
	"exported": gqfExported,
	"fanin":    gqfFanIn,
	"fanout":   gqfFanOut,
}

// numericFields are the only fields that support ordering operators (>, >=, <, <=).
var numericGraphQueryFields = map[graphQueryField]bool{
	gqfFanIn:  true,
	gqfFanOut: true,
}

// graphQueryCondition is a single field = value comparison.
type graphQueryCondition struct {
	field graphQueryField
	op    string // "=", "!=", ">", ">=", "<", "<="
	sval  string // populated for string/bool fields
	ival  int    // populated for numeric fields
	isNum bool   // true when the value was parsed as an integer
}

// graphQuery is the parsed representation of a query_graph DSL query.
type graphQuery struct {
	conditions []graphQueryCondition
}

// parseGraphQuery parses a full DSL query string into a graphQuery.
func parseGraphQuery(raw string) (*graphQuery, error) {
	tokens, err := tokenize(raw)
	if err != nil {
		return nil, err
	}

	pos := 0
	peek := func() token { return tokens[pos] }
	consume := func() token {
		t := tokens[pos]
		pos++
		return t
	}
	expectWord := func(want string) error {
		t := consume()
		if t.kind != tokWord || !strings.EqualFold(t.value, want) {
			return fmt.Errorf("expected %q, got %q", want, t.value)
		}
		return nil
	}

	// Expect "NODES WHERE".
	if err := expectWord("NODES"); err != nil {
		return nil, fmt.Errorf("query must start with NODES: %w", err)
	}
	if err := expectWord("WHERE"); err != nil {
		return nil, fmt.Errorf("expected WHERE after NODES: %w", err)
	}

	var conditions []graphQueryCondition

	for {
		// Parse field.
		t := consume()
		if t.kind != tokWord {
			return nil, fmt.Errorf("expected field name, got %q", t.value)
		}
		fieldKey := strings.ToLower(t.value)
		field, ok := validGraphQueryFields[fieldKey]
		if !ok {
			return nil, fmt.Errorf("unknown field %q — valid fields: package, type, domain, file, name, exported, fanin, fanout", t.value)
		}

		// Parse operator.
		opTok := consume()
		if opTok.kind != tokOp {
			return nil, fmt.Errorf("expected operator after field %q, got %q", field, opTok.value)
		}
		op := opTok.value

		// Validate operator for field type.
		if (op == ">" || op == ">=" || op == "<" || op == "<=") && !numericGraphQueryFields[field] {
			return nil, fmt.Errorf("operator %q only supported for numeric fields (fanin, fanout); field %q is a string", op, field)
		}

		// Parse value.
		valTok := consume()
		var cond graphQueryCondition
		cond.field = field
		cond.op = op

		switch {
		case numericGraphQueryFields[field]:
			// Numeric fields require an integer value.
			var numStr string
			switch valTok.kind {
			case tokNumber:
				numStr = valTok.value
			case tokWord:
				// Accept bareword that is all digits (edge case).
				numStr = valTok.value
			default:
				return nil, fmt.Errorf("field %q requires an integer value, got %q", field, valTok.value)
			}
			n, perr := strconv.Atoi(numStr)
			if perr != nil {
				return nil, fmt.Errorf("field %q requires an integer value, %q is not a valid integer", field, numStr)
			}
			cond.ival = n
			cond.isNum = true

		case field == gqfExported:
			// Boolean field: accept true/false (case-insensitive).
			var rawVal string
			switch valTok.kind {
			case tokWord:
				rawVal = strings.ToLower(valTok.value)
			case tokString:
				rawVal = strings.ToLower(valTok.value)
			default:
				return nil, fmt.Errorf("field exported requires true or false, got %q", valTok.value)
			}
			if rawVal != "true" && rawVal != "false" {
				return nil, fmt.Errorf("field exported requires true or false, got %q", rawVal)
			}
			if op != "=" && op != "!=" {
				return nil, fmt.Errorf("field exported only supports = and != operators")
			}
			cond.sval = rawVal

		default:
			// String fields accept quoted or unquoted values.
			switch valTok.kind {
			case tokString, tokWord:
				cond.sval = valTok.value
			default:
				return nil, fmt.Errorf("field %q requires a string value, got token %q", field, valTok.value)
			}
		}

		conditions = append(conditions, cond)

		// Check for AND or EOF.
		next := peek()
		if next.kind == tokEOF {
			break
		}
		if next.kind == tokWord && strings.EqualFold(next.value, "AND") {
			consume() // consume AND
			continue
		}
		return nil, fmt.Errorf("expected AND or end of query, got %q", next.value)
	}

	if len(conditions) == 0 {
		return nil, fmt.Errorf("WHERE clause must contain at least one condition")
	}

	return &graphQuery{conditions: conditions}, nil
}

// ── Evaluator ────────────────────────────────────────────────────────────────

// graphNodeResult is the shape of a single node returned by query_graph.
type graphNodeResult struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Package  string `json:"package,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Exported bool   `json:"exported"`
	FanIn    int    `json:"fanin"`
	FanOut   int    `json:"fanout"`
}

// matchCondition tests whether a node (with pre-computed fanin/fanout) satisfies
// a single condition. Returns false if the condition is not satisfied.
func matchCondition(n *graph.Node, fanin, fanout int, c graphQueryCondition) bool {
	if c.isNum {
		var actual int
		switch c.field {
		case gqfFanIn:
			actual = fanin
		case gqfFanOut:
			actual = fanout
		}
		switch c.op {
		case "=":
			return actual == c.ival
		case "!=":
			return actual != c.ival
		case ">":
			return actual > c.ival
		case ">=":
			return actual >= c.ival
		case "<":
			return actual < c.ival
		case "<=":
			return actual <= c.ival
		}
		return false
	}

	// String / boolean fields.
	var actual string
	switch c.field {
	case gqfPackage:
		actual = n.Package
	case gqfType:
		actual = string(n.Type)
	case gqfDomain:
		d := string(n.Domain)
		if d == "" {
			d = string(graph.DomainCode) // default domain
		}
		actual = d
	case gqfFile:
		actual = n.File
	case gqfName:
		actual = n.Name
	case gqfExported:
		if n.Exported {
			actual = "true"
		} else {
			actual = "false"
		}
	default:
		return false
	}

	wantLower := strings.ToLower(c.sval)
	actualLower := strings.ToLower(actual)

	// file and package use substring (contains) matching so users can write
	// short names without knowing the internal storage format:
	//   file:    NODES WHERE file="login.go"    matches "internal/auth/login.go"
	//   package: NODES WHERE package="auth"     matches "com.example.auth" (Java)
	// Exact equality for these fields requires knowing the full stored value,
	// which users cannot discover without a successful query first.
	if c.field == gqfFile || c.field == gqfPackage {
		switch c.op {
		case "=":
			return strings.Contains(actualLower, wantLower)
		case "!=":
			return !strings.Contains(actualLower, wantLower)
		}
		return false
	}

	// All other string fields (name, type, domain, exported): case-insensitive
	// exact match. Parser blocks >, >=, <, <= for string fields — only = and
	// != reach here.
	switch c.op {
	case "=":
		return actualLower == wantLower
	case "!=":
		return actualLower != wantLower
	}
	return false
}

// matchAllConditions returns true when the node satisfies every condition (AND semantics).
func matchAllConditions(n *graph.Node, fanin, fanout int, conditions []graphQueryCondition) bool {
	for _, c := range conditions {
		if !matchCondition(n, fanin, fanout, c) {
			return false
		}
	}
	return true
}

// ── Handler ──────────────────────────────────────────────────────────────────

// handleQueryGraph implements the query_graph MCP tool.
// It parses the DSL query, iterates the in-memory graph, and returns matching nodes.
// Hard limits: 1000 nodes, 500 ms wall time.
func (s *Server) handleQueryGraph(
	_ context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	if s.graph == nil {
		return mcp.NewToolResultError(
			"No graph loaded — run 'synapses index' first."), nil
	}

	raw, err := stringArgLimited(req, "query", 10*1024) // 10 KB cap — tokenizer is O(len)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if raw == "" {
		return mcp.NewToolResultError(
			"query is required. Example: NODES WHERE package=\"auth\" AND fanin > 5"), nil
	}

	q, err := parseGraphQuery(raw)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf(
			"query parse error: %s\n\nExamples:\n"+
				"  NODES WHERE package=\"auth\" AND fanin > 5\n"+
				"  NODES WHERE type=\"function\" AND exported=true\n"+
				"  NODES WHERE domain=\"infra\"\n"+
				"  NODES WHERE fanout >= 10 AND fanin = 0", err)), nil
	}

	// Start the 500 ms deadline timer BEFORE the graph snapshots so that
	// AllNodes + AllEdges are counted against the budget.
	deadline := time.NewTimer(queryGraphTimeout)
	defer deadline.Stop()

	// Snapshot all nodes under one read lock.
	allNodes := s.graph.AllNodes()
	if len(allNodes) == 0 {
		return mcp.NewToolResultError(
			"Graph is empty — run 'synapses index' first and verify that parsing completed."), nil
	}

	// Build degree maps from a single AllEdges snapshot (one read lock).
	// This avoids 2N per-node lock acquisitions (InEdges/OutEdges each lock),
	// which under concurrent reindexing can block the timeout from firing.
	// Pattern mirrors the stats command in main.go.
	allEdges := s.graph.AllEdges()
	fanInMap := make(map[graph.NodeID]int, len(allNodes))
	fanOutMap := make(map[graph.NodeID]int, len(allNodes))
	for _, e := range allEdges {
		fanInMap[e.To]++
		fanOutMap[e.From]++
	}

	results := make([]graphNodeResult, 0) // never nil — serializes as [] not null
	truncated := false
	timedOut := false
	matchedTotal := 0 // total nodes passing WHERE — may exceed queryGraphNodeCap

	for _, n := range allNodes {
		// Check the deadline in a non-blocking select every iteration.
		// Since degree lookups are now O(1) map ops (no locks), this check
		// fires reliably within one iteration of the 500 ms budget.
		select {
		case <-deadline.C:
			timedOut = true
		default:
		}
		if timedOut {
			truncated = true
			break
		}

		fanin := fanInMap[n.ID]
		fanout := fanOutMap[n.ID]

		if !matchAllConditions(n, fanin, fanout, q.conditions) {
			continue
		}
		matchedTotal++

		// After the cap is hit, keep counting matches but stop collecting results.
		// This gives callers an accurate matchedTotal so they can gauge truncation severity.
		if len(results) < queryGraphNodeCap {
			domain := string(n.Domain)
			if domain == "" {
				domain = string(graph.DomainCode)
			}
			results = append(results, graphNodeResult{
				ID:       string(n.ID),
				Name:     n.Name,
				Type:     string(n.Type),
				Package:  n.Package,
				File:     n.File,
				Line:     n.Line,
				Domain:   domain,
				Exported: n.Exported,
				FanIn:    fanin,
				FanOut:   fanout,
			})
			if len(results) >= queryGraphNodeCap {
				truncated = true
				// Don't break — keep iterating to get accurate matchedTotal.
			}
		}
	}

	out := map[string]interface{}{
		"query":         raw,
		"nodes":         results,
		"count":         len(results),
		"matched_total": matchedTotal, // total passing WHERE; may exceed count when truncated
		"total_nodes":   len(allNodes),
		"truncated":     truncated,
		"timed_out":     timedOut,
		"node_cap":      queryGraphNodeCap,
		"timeout_ms":    queryGraphTimeout.Milliseconds(),
	}
	if truncated && !timedOut {
		out["hint"] = fmt.Sprintf(
			"Result capped at %d of %d matching nodes. Narrow your query with additional AND conditions.",
			queryGraphNodeCap, matchedTotal)
	}
	if timedOut {
		out["hint"] = fmt.Sprintf(
			"Query timed out after %dms. Narrow your query with additional AND conditions "+
				"(e.g. add package= or type= to reduce the scan set).", queryGraphTimeout.Milliseconds())
	}

	return jsonResult(out)
}
