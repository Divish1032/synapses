package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── Tokenizer tests ──────────────────────────────────────────────────────────

func TestTokenize_BasicTokens(t *testing.T) {
	tokens, err := tokenize(`NODES WHERE package="auth" AND fanin > 5`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Expect: NODES, WHERE, package, =, "auth", AND, fanin, >, 5, EOF
	expectedKinds := []tokenKind{tokWord, tokWord, tokWord, tokOp, tokString, tokWord, tokWord, tokOp, tokNumber, tokEOF}
	if len(tokens) != len(expectedKinds) {
		t.Fatalf("expected %d tokens, got %d: %v", len(expectedKinds), len(tokens), tokens)
	}
	for i, k := range expectedKinds {
		if tokens[i].kind != k {
			t.Errorf("token[%d]: expected kind %d, got %d (value %q)", i, k, tokens[i].kind, tokens[i].value)
		}
	}
	// Check string value strips quotes.
	if tokens[3].value != "=" {
		t.Errorf("expected operator =, got %q", tokens[3].value)
	}
	if tokens[4].value != "auth" {
		t.Errorf("expected string value auth, got %q", tokens[4].value)
	}
}

func TestTokenize_TwoCharOps(t *testing.T) {
	tests := []struct {
		input    string
		wantOp   string
	}{
		{"x != y", "!="},
		{"x >= 5", ">="},
		{"x <= 5", "<="},
	}
	for _, tt := range tests {
		tokens, err := tokenize(tt.input)
		if err != nil {
			t.Fatalf("tokenize(%q): %v", tt.input, err)
		}
		found := false
		for _, tok := range tokens {
			if tok.kind == tokOp && tok.value == tt.wantOp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tokenize(%q): op %q not found in tokens %v", tt.input, tt.wantOp, tokens)
		}
	}
}

func TestTokenize_UnterminatedString(t *testing.T) {
	_, err := tokenize(`NODES WHERE package="auth`)
	if err == nil {
		t.Fatal("expected error for unterminated string, got nil")
	}
}

func TestTokenize_QuotedStringWithEscape(t *testing.T) {
	tokens, err := tokenize(`NODES WHERE name="foo\"bar"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tok := range tokens {
		if tok.kind == tokString {
			if tok.value != `foo"bar` {
				t.Errorf("expected unescaped string foo\"bar, got %q", tok.value)
			}
			return
		}
	}
	t.Fatal("string token not found")
}

func TestTokenize_UnexpectedCharacter(t *testing.T) {
	_, err := tokenize("NODES WHERE package @ auth")
	if err == nil {
		t.Fatal("expected error for unexpected character @, got nil")
	}
}

// ── Parser tests ─────────────────────────────────────────────────────────────

func TestParseGraphQuery_SimpleStringCondition(t *testing.T) {
	q, err := parseGraphQuery(`NODES WHERE package="auth"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(q.conditions))
	}
	c := q.conditions[0]
	if c.field != gqfPackage {
		t.Errorf("expected field package, got %q", c.field)
	}
	if c.op != "=" {
		t.Errorf("expected op =, got %q", c.op)
	}
	if c.sval != "auth" {
		t.Errorf("expected sval auth, got %q", c.sval)
	}
	if c.isNum {
		t.Error("expected isNum=false for string field")
	}
}

func TestParseGraphQuery_NumericCondition(t *testing.T) {
	q, err := parseGraphQuery(`NODES WHERE fanin > 5`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(q.conditions))
	}
	c := q.conditions[0]
	if c.field != gqfFanIn {
		t.Errorf("expected field fanin, got %q", c.field)
	}
	if c.op != ">" {
		t.Errorf("expected op >, got %q", c.op)
	}
	if c.ival != 5 {
		t.Errorf("expected ival 5, got %d", c.ival)
	}
	if !c.isNum {
		t.Error("expected isNum=true for numeric field")
	}
}

func TestParseGraphQuery_MultipleConditions(t *testing.T) {
	q, err := parseGraphQuery(`NODES WHERE package="auth" AND fanin > 5 AND type="function"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.conditions) != 3 {
		t.Fatalf("expected 3 conditions, got %d", len(q.conditions))
	}
}

func TestParseGraphQuery_ExportedBoolean(t *testing.T) {
	q, err := parseGraphQuery(`NODES WHERE exported=true`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c := q.conditions[0]
	if c.field != gqfExported {
		t.Errorf("expected field exported, got %q", c.field)
	}
	if c.sval != "true" {
		t.Errorf("expected sval true, got %q", c.sval)
	}
}

func TestParseGraphQuery_InvalidField(t *testing.T) {
	_, err := parseGraphQuery(`NODES WHERE badfield="foo"`)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}

func TestParseGraphQuery_NumericOpOnStringField(t *testing.T) {
	_, err := parseGraphQuery(`NODES WHERE package > 5`)
	if err == nil {
		t.Fatal("expected error for numeric op on string field, got nil")
	}
}

func TestParseGraphQuery_MissingWhere(t *testing.T) {
	_, err := parseGraphQuery(`NODES package="auth"`)
	if err == nil {
		t.Fatal("expected error for missing WHERE, got nil")
	}
}

func TestParseGraphQuery_MissingNODES(t *testing.T) {
	_, err := parseGraphQuery(`WHERE package="auth"`)
	if err == nil {
		t.Fatal("expected error for missing NODES prefix, got nil")
	}
}

func TestParseGraphQuery_EmptyQuery(t *testing.T) {
	_, err := parseGraphQuery(`NODES WHERE`)
	if err == nil {
		t.Fatal("expected error for empty WHERE clause, got nil")
	}
}

func TestParseGraphQuery_NonIntegerNumericValue(t *testing.T) {
	_, err := parseGraphQuery(`NODES WHERE fanin > abc`)
	if err == nil {
		t.Fatal("expected error for non-integer numeric value, got nil")
	}
}

func TestParseGraphQuery_TrailingJunk(t *testing.T) {
	_, err := parseGraphQuery(`NODES WHERE type="function" OR type="method"`)
	if err == nil {
		t.Fatal("expected error for OR operator (unsupported), got nil")
	}
}

func TestParseGraphQuery_ExportedWithOrderingOp(t *testing.T) {
	_, err := parseGraphQuery(`NODES WHERE exported > true`)
	if err == nil {
		t.Fatal("expected error for ordering op on exported field, got nil")
	}
}

func TestParseGraphQuery_AllFields(t *testing.T) {
	queries := []string{
		`NODES WHERE package="auth"`,
		`NODES WHERE type="function"`,
		`NODES WHERE domain="infra"`,
		`NODES WHERE file="main.go"`,
		`NODES WHERE name="Foo"`,
		`NODES WHERE exported=false`,
		`NODES WHERE fanin = 0`,
		`NODES WHERE fanout >= 3`,
	}
	for _, q := range queries {
		if _, err := parseGraphQuery(q); err != nil {
			t.Errorf("parseGraphQuery(%q) unexpected error: %v", q, err)
		}
	}
}

// ── Condition matching tests ─────────────────────────────────────────────────

func makeTestNode(name, pkg, typ, domain, file string, exported bool) *graph.Node {
	return &graph.Node{
		ID:       graph.NodeID("repo::" + file + "::" + name),
		Name:     name,
		Package:  pkg,
		Type:     graph.NodeType(typ),
		Domain:   graph.DomainType(domain),
		File:     file,
		Exported: exported,
	}
}

func TestMatchCondition_StringEquals(t *testing.T) {
	n := makeTestNode("Foo", "auth", "function", "code", "auth.go", true)
	c := graphQueryCondition{field: gqfPackage, op: "=", sval: "auth"}
	if !matchCondition(n, 0, 0, c) {
		t.Error("expected match for package=auth")
	}
	c.sval = "payment"
	if matchCondition(n, 0, 0, c) {
		t.Error("expected no match for package=payment")
	}
}

func TestMatchCondition_StringNotEquals(t *testing.T) {
	n := makeTestNode("Foo", "auth", "function", "code", "auth.go", true)
	c := graphQueryCondition{field: gqfPackage, op: "!=", sval: "payment"}
	if !matchCondition(n, 0, 0, c) {
		t.Error("expected match for package!=payment")
	}
}

func TestMatchCondition_FanIn(t *testing.T) {
	n := makeTestNode("Foo", "auth", "function", "code", "auth.go", true)
	c := graphQueryCondition{field: gqfFanIn, op: ">", ival: 5, isNum: true}
	if matchCondition(n, 3, 0, c) {
		t.Error("expected no match for fanin=3 > 5")
	}
	if !matchCondition(n, 10, 0, c) {
		t.Error("expected match for fanin=10 > 5")
	}
}

func TestMatchCondition_FanOut(t *testing.T) {
	n := makeTestNode("Foo", "auth", "function", "code", "auth.go", true)
	tests := []struct {
		op     string
		ival   int
		fanout int
		want   bool
	}{
		{"=", 5, 5, true},
		{"=", 5, 4, false},
		{"!=", 5, 4, true},
		{">=", 5, 5, true},
		{">=", 5, 4, false},
		{"<=", 5, 5, true},
		{"<=", 5, 6, false},
		{"<", 5, 4, true},
		{"<", 5, 5, false},
	}
	for _, tt := range tests {
		c := graphQueryCondition{field: gqfFanOut, op: tt.op, ival: tt.ival, isNum: true}
		got := matchCondition(n, 0, tt.fanout, c)
		if got != tt.want {
			t.Errorf("fanout=%d %s %d: expected %v, got %v", tt.fanout, tt.op, tt.ival, tt.want, got)
		}
	}
}

func TestMatchCondition_Exported(t *testing.T) {
	n := makeTestNode("Foo", "auth", "function", "code", "auth.go", true)
	c := graphQueryCondition{field: gqfExported, op: "=", sval: "true"}
	if !matchCondition(n, 0, 0, c) {
		t.Error("expected match for exported=true on exported node")
	}
	c.sval = "false"
	if matchCondition(n, 0, 0, c) {
		t.Error("expected no match for exported=false on exported node")
	}
}

func TestMatchCondition_Domain_DefaultCode(t *testing.T) {
	// Node with empty domain should be treated as "code"
	n := makeTestNode("Foo", "pkg", "function", "", "foo.go", false)
	c := graphQueryCondition{field: gqfDomain, op: "=", sval: "code"}
	if !matchCondition(n, 0, 0, c) {
		t.Error("expected empty domain to match 'code'")
	}
}

func TestMatchCondition_CaseInsensitive(t *testing.T) {
	n := makeTestNode("Foo", "Auth", "function", "code", "auth.go", true)
	c := graphQueryCondition{field: gqfPackage, op: "=", sval: "auth"}
	if !matchCondition(n, 0, 0, c) {
		t.Error("expected case-insensitive match for package")
	}
}

func TestMatchAllConditions_MultipleAND(t *testing.T) {
	n := makeTestNode("Foo", "auth", "function", "code", "auth.go", true)
	conditions := []graphQueryCondition{
		{field: gqfPackage, op: "=", sval: "auth"},
		{field: gqfType, op: "=", sval: "function"},
		{field: gqfFanIn, op: ">", ival: 2, isNum: true},
	}
	if !matchAllConditions(n, 5, 3, conditions) {
		t.Error("expected all conditions to match")
	}
	// Flip one condition to fail.
	conditions[2].ival = 10
	if matchAllConditions(n, 5, 3, conditions) {
		t.Error("expected match to fail when fanin condition fails")
	}
}

// ── Handler integration tests ────────────────────────────────────────────────

// buildTestGraphForQuery creates a small graph with known topology for handler tests.
func buildTestGraphForQuery() *graph.Graph {
	g := graph.New("testrepo")

	addNode := func(name, pkg, typ, domain string, exported bool) {
		n := &graph.Node{
			ID:       g.MakeNodeID(pkg+".go", name),
			Name:     name,
			Package:  pkg,
			Type:     graph.NodeType(typ),
			Domain:   graph.DomainType(domain),
			File:     pkg + ".go",
			Exported: exported,
		}
		g.AddNode(n)
	}

	addEdge := func(fromName, fromPkg, toName, toPkg string) {
		from := g.MakeNodeID(fromPkg+".go", fromName)
		to := g.MakeNodeID(toPkg+".go", toName)
		g.AddEdge(&graph.Edge{From: from, To: to, Type: graph.EdgeCalls})
	}

	// auth package: 2 functions
	addNode("Login", "auth", "function", "code", true)
	addNode("Logout", "auth", "function", "code", true)
	addNode("hashPassword", "auth", "function", "code", false)
	// payment package: 1 function
	addNode("ProcessPayment", "payment", "function", "code", true)
	// infra domain: 1 node
	addNode("prod-cluster", "infra", "variable", "infra", false)

	// Edges: Login → hashPassword (fanout Login=1, fanin hashPassword=1)
	addEdge("Login", "auth", "hashPassword", "auth")
	// ProcessPayment → Login (fanin Login=1, fanout ProcessPayment=1)
	addEdge("ProcessPayment", "payment", "Login", "auth")

	return g
}

func callQueryGraphTool(t *testing.T, g *graph.Graph, queryStr string) map[string]interface{} {
	t.Helper()
	srv := New(g, nil, nil)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"query": queryStr}
	result, err := srv.handleQueryGraph(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		// Return error in a parseable form for error-path tests.
		text := extractText(t, result)
		return map[string]interface{}{"_error": text}
	}
	text := extractText(t, result)
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal result: %v\nraw: %s", err, text)
	}
	return out
}

func TestHandleQueryGraph_PackageFilter(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE package="auth"`)
	count := int(out["count"].(float64))
	if count != 3 {
		t.Errorf("expected 3 auth nodes, got %d", count)
	}
}

func TestHandleQueryGraph_DomainInfra(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE domain="infra"`)
	count := int(out["count"].(float64))
	if count != 1 {
		t.Errorf("expected 1 infra node, got %d", count)
	}
}

func TestHandleQueryGraph_ExportedTrue(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE exported=true`)
	count := int(out["count"].(float64))
	// Login, Logout, ProcessPayment, prod-cluster is NOT exported... Login, Logout, ProcessPayment = 3 exported
	if count != 3 {
		t.Errorf("expected 3 exported nodes, got %d", count)
	}
}

func TestHandleQueryGraph_FaninGreaterThanZero(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE fanin > 0`)
	count := int(out["count"].(float64))
	// Login has fanin=1 (from ProcessPayment), hashPassword has fanin=1 (from Login) → 2
	if count != 2 {
		t.Errorf("expected 2 nodes with fanin > 0, got %d", count)
	}
}

func TestHandleQueryGraph_MultipleConditions(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE package="auth" AND exported=true`)
	count := int(out["count"].(float64))
	// Login and Logout in auth and exported → 2
	if count != 2 {
		t.Errorf("expected 2 auth+exported nodes, got %d", count)
	}
}

func TestHandleQueryGraph_NoMatch(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE package="nonexistent"`)
	count := int(out["count"].(float64))
	if count != 0 {
		t.Errorf("expected 0 matches, got %d", count)
	}
	if out["truncated"].(bool) {
		t.Error("truncated should be false for empty results")
	}
}

func TestHandleQueryGraph_EmptyResultIsArray(t *testing.T) {
	// Verifies that nodes field is [] (not null) when no nodes match.
	// null would break agent JSON parsing expecting an array.
	g := buildTestGraphForQuery()
	srv := New(g, nil, nil)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"query": `NODES WHERE package="doesnotexist"`}
	result, err := srv.handleQueryGraph(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := extractText(t, result)
	// Raw JSON should contain "nodes": [] not "nodes": null
	if !strings.Contains(text, `"nodes": []`) {
		t.Errorf("expected nodes to serialize as [] not null, got: %s", text)
	}
}

func TestHandleQueryGraph_ParseError(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE badfield="foo"`)
	if _, hasErr := out["_error"]; !hasErr {
		t.Error("expected error result for unknown field")
	}
}

func TestHandleQueryGraph_EmptyQuery(t *testing.T) {
	g := buildTestGraphForQuery()
	srv := New(g, nil, nil)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"query": ""}
	result, err := srv.handleQueryGraph(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for empty query")
	}
}

func TestHandleQueryGraph_NilGraph(t *testing.T) {
	srv := New(nil, nil, nil)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"query": `NODES WHERE package="auth"`}
	result, err := srv.handleQueryGraph(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result when graph is nil")
	}
}

func TestHandleQueryGraph_ResultShape(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE name="Login"`)
	nodes, ok := out["nodes"].([]interface{})
	if !ok || len(nodes) == 0 {
		t.Fatal("expected at least one node in result")
	}
	node := nodes[0].(map[string]interface{})
	// Verify expected fields are present.
	for _, field := range []string{"id", "name", "type", "package", "file", "domain", "fanin", "fanout"} {
		if _, exists := node[field]; !exists {
			t.Errorf("expected field %q in node result", field)
		}
	}
	if node["name"] != "Login" {
		t.Errorf("expected name Login, got %q", node["name"])
	}
}

func TestHandleQueryGraph_FanoutCondition(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE fanout >= 1`)
	count := int(out["count"].(float64))
	// Login (fanout=1 → hashPassword), ProcessPayment (fanout=1 → Login) → 2
	if count != 2 {
		t.Errorf("expected 2 nodes with fanout >= 1, got %d", count)
	}
}

func TestHandleQueryGraph_TypeFilter(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE type="variable"`)
	count := int(out["count"].(float64))
	if count != 1 {
		t.Errorf("expected 1 variable node (prod-cluster), got %d", count)
	}
}

func TestHandleQueryGraph_DomainCodeDefault(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE domain="code"`)
	count := int(out["count"].(float64))
	// 4 code nodes: Login, Logout, hashPassword, ProcessPayment
	if count != 4 {
		t.Errorf("expected 4 code-domain nodes, got %d", count)
	}
}

func TestHandleQueryGraph_TruncatedAndNotTruncated(t *testing.T) {
	g := buildTestGraphForQuery()
	out := callQueryGraphTool(t, g, `NODES WHERE type="function"`)
	// 4 function nodes — well under 1000 cap
	if out["truncated"].(bool) {
		t.Error("should not be truncated for small result set")
	}
	if out["timed_out"].(bool) {
		t.Error("should not be timed_out for small result set")
	}
}

// ── Benchmark ────────────────────────────────────────────────────────────────

func BenchmarkParseGraphQuery(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = parseGraphQuery(`NODES WHERE package="auth" AND fanin > 5 AND exported=true`)
	}
}

func BenchmarkHandleQueryGraph_SmallGraph(b *testing.B) {
	g := buildTestGraphForQuery()
	srv := New(g, nil, nil)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"query": `NODES WHERE type="function"`}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = srv.handleQueryGraph(context.Background(), req)
	}
}
