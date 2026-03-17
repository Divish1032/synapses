package mcp

// White-box tests for digest.go (serializeCompact, writeNodeHeader,
// getRootSummary, getDepSummary) and brain_tools.go nil-brain paths.
// Uses package mcp for direct access to unexported types/functions.

import (
	"strings"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── writeNodeHeader ───────────────────────────────────────────────────────────

func TestWriteNodeHeader_BasicNoSummary(t *testing.T) {
	var b strings.Builder
	n := &graph.Node{Name: "Login", Type: graph.NodeFunction, File: "auth/auth.go", Line: 42}
	writeNodeHeader(&b, n, "")
	out := b.String()
	if !strings.Contains(out, "[Login]") {
		t.Errorf("expected [Login] in header, got %q", out)
	}
	if !strings.Contains(out, "auth.go:42") {
		t.Errorf("expected auth.go:42 in header, got %q", out)
	}
	if strings.Contains(out, "Summary:") {
		t.Error("should not write Summary: when summary is empty")
	}
}

func TestWriteNodeHeader_WithSummary(t *testing.T) {
	var b strings.Builder
	n := &graph.Node{Name: "Logout", Type: graph.NodeFunction, File: "auth/auth.go", Line: 10}
	writeNodeHeader(&b, n, "handles user logout")
	out := b.String()
	if !strings.Contains(out, "Summary: handles user logout") {
		t.Errorf("expected Summary line, got %q", out)
	}
}

func TestWriteNodeHeader_ZeroLine(t *testing.T) {
	var b strings.Builder
	n := &graph.Node{Name: "Foo", Type: graph.NodeFunction, File: "foo.go", Line: 0}
	writeNodeHeader(&b, n, "")
	out := b.String()
	// With Line=0, no :0 should appear
	if strings.Contains(out, ":0") {
		t.Errorf("should not have :0 for zero line, got %q", out)
	}
}

func TestWriteNodeHeader_WithComplexity(t *testing.T) {
	var b strings.Builder
	n := &graph.Node{
		Name: "ComplexFunc", Type: graph.NodeFunction, File: "svc.go", Line: 5,
		Metadata: map[string]string{"complexity": "12"},
	}
	writeNodeHeader(&b, n, "")
	out := b.String()
	if !strings.Contains(out, "complexity:12") {
		t.Errorf("expected complexity in header, got %q", out)
	}
}

func TestWriteNodeHeader_ComplexityZeroSkipped(t *testing.T) {
	var b strings.Builder
	n := &graph.Node{
		Name: "Simple", Type: graph.NodeFunction, File: "s.go", Line: 1,
		Metadata: map[string]string{"complexity": "0"},
	}
	writeNodeHeader(&b, n, "")
	out := b.String()
	if strings.Contains(out, "complexity") {
		t.Errorf("complexity:0 should be skipped, got %q", out)
	}
}

// ── OF-H1: domain badge in writeNodeHeader ────────────────────────────────────

func TestWriteNodeHeader_DomainBadge_NonCode(t *testing.T) {
	var b strings.Builder
	n := &graph.Node{
		Name:   "GetUsers",
		Type:   graph.NodeType("endpoint"),
		File:   "api/openapi.yaml",
		Line:   10,
		Domain: graph.DomainAPI,
	}
	writeNodeHeader(&b, n, "")
	out := b.String()
	if !strings.Contains(out, "domain:api") {
		t.Errorf("expected 'domain:api' badge for api domain node, got %q", out)
	}
}

func TestWriteNodeHeader_DomainBadge_CodeHidden(t *testing.T) {
	var b strings.Builder
	n := &graph.Node{
		Name:   "Login",
		Type:   graph.NodeFunction,
		File:   "auth/auth.go",
		Line:   5,
		Domain: graph.DomainCode,
	}
	writeNodeHeader(&b, n, "")
	out := b.String()
	if strings.Contains(out, "domain:") {
		t.Errorf("domain:code should be hidden (zero noise for code nodes), got %q", out)
	}
}

func TestWriteNodeHeader_DomainBadge_EmptyHidden(t *testing.T) {
	var b strings.Builder
	n := &graph.Node{
		Name: "main",
		Type: graph.NodeFunction,
		File: "main.go",
		Line: 1,
		// Domain intentionally empty — treated as "code".
	}
	writeNodeHeader(&b, n, "")
	out := b.String()
	if strings.Contains(out, "domain:") {
		t.Errorf("empty domain should be hidden, got %q", out)
	}
}

func TestWriteNodeHeader_DomainBadge_AllNonCodeDomains(t *testing.T) {
	nonCodeDomains := []graph.DomainType{
		graph.DomainInfra,
		graph.DomainAPI,
		graph.DomainDocs,
		graph.DomainIssues,
		graph.DomainCustom,
	}
	for _, dom := range nonCodeDomains {
		dom := dom
		t.Run(string(dom), func(t *testing.T) {
			var b strings.Builder
			n := &graph.Node{Name: "X", Type: graph.NodeFunction, File: "x.txt", Line: 1, Domain: dom}
			writeNodeHeader(&b, n, "")
			out := b.String()
			want := "domain:" + string(dom)
			if !strings.Contains(out, want) {
				t.Errorf("expected %q badge, got %q", want, out)
			}
		})
	}
}

// ── getRootSummary ────────────────────────────────────────────────────────────

func TestGetRootSummary_NilPacket(t *testing.T) {
	n := &graph.Node{Name: "Foo"}
	if s := getRootSummary(n, nil); s != "" {
		t.Errorf("expected empty for nil packet, got %q", s)
	}
}

func TestGetRootSummary_FromPacket(t *testing.T) {
	n := &graph.Node{Name: "Foo"}
	pkt := &brain.ContextPacket{RootSummary: "authenticates the user"}
	if s := getRootSummary(n, pkt); s != "authenticates the user" {
		t.Errorf("expected brain summary, got %q", s)
	}
}

func TestGetRootSummary_FromNodeDoc(t *testing.T) {
	n := &graph.Node{Name: "Foo", Metadata: map[string]string{"doc": "ast doc comment"}}
	if s := getRootSummary(n, nil); s != "ast doc comment" {
		t.Errorf("expected ast doc, got %q", s)
	}
}

func TestGetRootSummary_DocTruncatedAt250(t *testing.T) {
	longDoc := strings.Repeat("a", 300)
	n := &graph.Node{Name: "Foo", Metadata: map[string]string{"doc": longDoc}}
	s := getRootSummary(n, nil)
	if len(s) > 254 { // 250 + "…"
		t.Errorf("expected truncation at 250 chars, got len %d", len(s))
	}
	if !strings.HasSuffix(s, "…") {
		t.Errorf("expected trailing ellipsis for truncated doc")
	}
}

func TestGetRootSummary_PacketOverridesDoc(t *testing.T) {
	n := &graph.Node{Name: "Foo", Metadata: map[string]string{"doc": "ast doc"}}
	pkt := &brain.ContextPacket{RootSummary: "brain summary"}
	if s := getRootSummary(n, pkt); s != "brain summary" {
		t.Errorf("brain should override AST doc, got %q", s)
	}
}

// ── getDepSummary ─────────────────────────────────────────────────────────────

func TestGetDepSummary_NilPacket(t *testing.T) {
	if s := getDepSummary("Foo", nil); s != "" {
		t.Errorf("expected empty for nil packet, got %q", s)
	}
}

func TestGetDepSummary_EmptyMap(t *testing.T) {
	pkt := &brain.ContextPacket{}
	if s := getDepSummary("Foo", pkt); s != "" {
		t.Errorf("expected empty for empty map, got %q", s)
	}
}

func TestGetDepSummary_Found(t *testing.T) {
	pkt := &brain.ContextPacket{
		DependencySummaries: map[string]string{"Foo": "foo does things"},
	}
	if s := getDepSummary("Foo", pkt); s != "foo does things" {
		t.Errorf("expected dep summary, got %q", s)
	}
}

func TestGetDepSummary_NotFound(t *testing.T) {
	pkt := &brain.ContextPacket{
		DependencySummaries: map[string]string{"Bar": "bar does stuff"},
	}
	if s := getDepSummary("Foo", pkt); s != "" {
		t.Errorf("expected empty for missing key, got %q", s)
	}
}

// ── serializeCompact ──────────────────────────────────────────────────────────

func newTestDC() *directionalContext {
	root := &graph.Node{
		ID: "root", Name: "AuthLogin", Type: graph.NodeFunction,
		File: "pkg/auth/auth.go", Line: 10,
	}
	callee := &graph.Node{
		ID: "callee", Name: "ValidateToken", Type: graph.NodeFunction,
		File: "pkg/auth/token.go", Line: 5,
	}
	caller := &graph.Node{
		ID: "caller", Name: "HandleRequest", Type: graph.NodeFunction,
		File: "pkg/api/handler.go", Line: 20,
	}
	return &directionalContext{
		Root: root,
		Callees: []graph.CarvedNode{
			{Node: callee},
		},
		Callers: []graph.CarvedNode{
			{Node: caller},
		},
	}
}

func TestSerializeCompact_FullLevel(t *testing.T) {
	dc := newTestDC()
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "[AuthLogin]") {
		t.Error("expected root entity name")
	}
	if !strings.Contains(out, "Calls:") {
		t.Error("expected Calls: section")
	}
	if !strings.Contains(out, "Called by:") {
		t.Error("expected Called by: section")
	}
	if !strings.Contains(out, "[ValidateToken]") {
		t.Error("expected callee detail block in full level")
	}
}

func TestSerializeCompact_SummaryLevel(t *testing.T) {
	dc := newTestDC()
	out := serializeCompact(dc, "summary")
	if !strings.Contains(out, "[AuthLogin]") {
		t.Error("expected root entity name in summary")
	}
	// Summary level should NOT include callee blocks
	if strings.Contains(out, "Calls:") {
		t.Error("summary level should not include Calls: section")
	}
}

func TestSerializeCompact_NeighborsLevel(t *testing.T) {
	dc := newTestDC()
	out := serializeCompact(dc, "neighbors")
	if !strings.Contains(out, "Calls:") {
		t.Error("neighbors level should include Calls: section")
	}
	// neighbors level should NOT include callee detail blocks
	if strings.Contains(out, "[ValidateToken]") {
		t.Error("neighbors level should not include callee detail blocks")
	}
}

func TestSerializeCompact_WithWarnings(t *testing.T) {
	dc := newTestDC()
	dc.ContextPacket = &brain.ContextPacket{
		GraphWarnings: []string{"high complexity"},
		Concerns:      []string{"missing tests"},
	}
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "⚠") {
		t.Error("expected warning symbol in output")
	}
}

func TestSerializeCompact_WithInsight(t *testing.T) {
	dc := newTestDC()
	dc.ContextPacket = &brain.ContextPacket{
		Insight: "this function is performance-critical",
	}
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "Insight:") {
		t.Error("expected Insight: in full level")
	}
}

func TestSerializeCompact_WithADRs(t *testing.T) {
	dc := newTestDC()
	dc.ADRs = []brain.ADR{{ID: "adr-1", Title: "Use JWT", Status: "accepted"}}
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "[ADR]") {
		t.Error("expected [ADR] entry in output")
	}
}

func TestSerializeCompact_WithPrinciples(t *testing.T) {
	dc := newTestDC()
	dc.Principles = []string{"keep it simple", "no direct DB access"}
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "Laws:") {
		t.Error("expected Laws: section from principles")
	}
}

func TestSerializeCompact_TruncatedNote(t *testing.T) {
	dc := newTestDC()
	dc.Truncated = true
	dc.TruncatedCount = 7
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "7 additional nodes omitted") {
		t.Error("expected truncation notice in output")
	}
}

func TestSerializeCompact_NoCallers(t *testing.T) {
	root := &graph.Node{ID: "root", Name: "Standalone", Type: graph.NodeFunction, File: "s.go", Line: 1}
	dc := &directionalContext{Root: root, Callees: nil, Callers: nil}
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "Called by: (none)") {
		t.Errorf("standalone node should show 'Called by: (none)', got: %q", out)
	}
}

func TestSerializeCompact_WithAnnotations(t *testing.T) {
	dc := newTestDC()
	dc.Annotations = map[string][]store.Annotation{
		"root": {{Note: "handles login flow"}},
	}
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "handles login flow") {
		t.Error("expected annotation note in output")
	}
}

func TestSerializeCompact_WithDocumentation(t *testing.T) {
	dc := newTestDC()
	dc.Documentation = []graph.CarvedNode{
		{
			Node: &graph.Node{
				ID:   "test::docs.md::docs.md § Architecture",
				Type: graph.NodeSection,
				Name: "docs.md § Architecture",
				File: "docs.md",
				Line: 5,
				Metadata: map[string]string{
					"title": "Architecture",
					"depth": "2",
				},
				Domain: graph.DomainDocs,
			},
			Relevance: 0.6,
			Hop:       1,
		},
	}
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "📖 Docs:") {
		t.Error("expected 📖 Docs section in output")
	}
	if !strings.Contains(out, "\"Architecture\"") {
		t.Error("expected section title in output")
	}
	if !strings.Contains(out, "docs.md") {
		t.Error("expected file name in output")
	}
}

func TestSerializeCompact_WithDocumentation_SummaryLevel(t *testing.T) {
	dc := newTestDC()
	dc.Documentation = []graph.CarvedNode{
		{
			Node: &graph.Node{
				ID:   "test::docs.md::docs.md § API",
				Type: graph.NodeSection,
				Name: "docs.md § API",
				File: "docs.md",
				Line: 1,
				Metadata: map[string]string{
					"title": "API",
					"depth": "1",
				},
				Domain: graph.DomainDocs,
			},
			Relevance: 0.5,
			Hop:       1,
		},
	}
	// Documentation should appear even at summary level.
	out := serializeCompact(dc, "summary")
	if !strings.Contains(out, "📖 Docs:") {
		t.Error("expected 📖 Docs section at summary level")
	}
}

func TestSerializeCompact_NoDocumentation(t *testing.T) {
	dc := newTestDC()
	dc.Documentation = nil
	out := serializeCompact(dc, "full")
	if strings.Contains(out, "📖 Docs:") {
		t.Error("📖 Docs should not appear when no documentation")
	}
}

// ── brain_tools.go: nil-brain paths ──────────────────────────────────────────

func TestHandleUpsertADR_NoBrain(t *testing.T) {
	s := newTestServer(t)
	// No brain client configured — should return a graceful error.
	req := callTool(map[string]any{
		"id":       "adr-1",
		"title":    "Use SQLite",
		"decision": "We will use SQLite for local storage",
	})
	result, err := s.handleUpsertADR(ctx, req)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	// Should return an error JSON mentioning brain not configured.
	tc := result.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "brain not configured") {
		t.Errorf("expected brain-not-configured error, got: %q", tc.Text)
	}
}

func TestHandleGetADRs_NoBrain(t *testing.T) {
	s := newTestServer(t)
	req := callTool(map[string]any{})
	result, err := s.handleGetADRs(ctx, req)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	tc := result.Content[0].(mcp.TextContent)
	if !strings.Contains(tc.Text, "brain not configured") {
		t.Errorf("expected brain-not-configured error, got: %q", tc.Text)
	}
}

func TestHandleUpsertADR_MissingArgs(t *testing.T) {
	s := newTestServer(t)
	// Even if brain were configured, missing args should fail gracefully.
	// Without a brain client, gets brain-not-configured first.
	req := callTool(map[string]any{"id": "adr-1"})
	result, err := s.handleUpsertADR(ctx, req)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestIngestWebContent_NoBrain(t *testing.T) {
	s := newTestServer(t)
	// Should not panic when brain is nil.
	s.ingestWebContent("https://example.com", "Test Article", strings.Repeat("x", 300))
}

func TestIngestWebContent_ShortContent(t *testing.T) {
	s := newTestServer(t)
	// Content under 200 chars — should no-op without panic.
	s.ingestWebContent("https://example.com", "Test", "short")
}

func TestGetBrainClient_NoBrain(t *testing.T) {
	s := newTestServer(t)
	bc := s.getBrainClient()
	if bc != nil {
		t.Error("expected nil brain client for unconfigured server")
	}
}
