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
					"title":        "Architecture",
					"depth":        "2",
					"body_preview": "The core data structure is FlatGraph which uses SoA layout.",
				},
				Domain: graph.DomainDocs,
			},
			Relevance: 0.6,
			Hop:       1,
		},
	}
	out := serializeCompact(dc, "full")
	// New format: one line per doc entry with body_preview.
	if !strings.Contains(out, "📖") {
		t.Error("expected 📖 doc line in output")
	}
	if !strings.Contains(out, "\"Architecture\"") {
		t.Error("expected section title in output")
	}
	if !strings.Contains(out, "docs.md") {
		t.Error("expected file name in output")
	}
	if !strings.Contains(out, "FlatGraph") {
		t.Error("expected body_preview content in output")
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
	if !strings.Contains(out, "📖") {
		t.Error("expected 📖 doc line at summary level")
	}
	if !strings.Contains(out, "\"API\"") {
		t.Error("expected section title at summary level")
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

// newSandwichDC builds a directionalContext with all sections populated
// so ordering assertions can check every position.
func newSandwichDC() *directionalContext {
	root := &graph.Node{
		ID: "root", Name: "AuthLogin", Type: graph.NodeFunction,
		File: "pkg/auth/auth.go", Line: 10,
		Metadata: map[string]string{
			"blame_author":  "alice",
			"blame_date":    "2026-03-01",
			"blame_subject": "refactor auth",
		},
	}
	return &directionalContext{
		Root: root,
		Enrichment: &contextEnrichment{
			RuleAlerts: []ruleAlert{
				{RuleID: "no-cross", Severity: "HIGH", FromNode: "a::b::X", ToNode: "c::d::Y", EdgeType: "CALLS"},
			},
		},
		QualityGaps: []store.QualityGap{
			{GapID: "GAP-1", Severity: "MEDIUM", Description: "missing tests"},
		},
		ContextPacket: &brain.ContextPacket{
			GraphWarnings: []string{"high fan-out"},
			Concerns:      []string{"no error handling"},
		},
		Callees: []graph.CarvedNode{
			{Node: &graph.Node{ID: "c1", Name: "ValidateToken", Type: graph.NodeFunction, File: "token.go", Line: 5}},
		},
		Callers: []graph.CarvedNode{
			{Node: &graph.Node{ID: "c2", Name: "HandleRequest", Type: graph.NodeFunction, File: "handler.go", Line: 20}},
		},
		Related: []graph.CarvedNode{
			{Node: &graph.Node{ID: "r1", Name: "AuthInterface", Type: graph.NodeInterface, File: "auth.go", Line: 1}},
		},
		ADRs:       []brain.ADR{{ID: "adr-1", Title: "Use JWT", Status: "accepted"}},
		Principles: []string{"keep it simple"},
		Annotations: map[string][]store.Annotation{
			"root": {{Note: "needs refactor"}},
		},
	}
}

// assertBefore is a test helper that fails if posA >= posB.
func assertBefore(t *testing.T, nameA string, posA int, nameB string, posB int) {
	t.Helper()
	if posA >= posB {
		t.Errorf("sandwich: %s (pos %d) must appear before %s (pos %d)", nameA, posA, nameB, posB)
	}
}

// TestSerializeCompact_SandwichOrdering_Full verifies the "Lost in the Middle"
// ordering at "full" detail level: safety → supplementary → actionable.
func TestSerializeCompact_SandwichOrdering_Full(t *testing.T) {
	dc := newSandwichDC()
	// Give Related a brain summary so it appears in output.
	dc.ContextPacket.DependencySummaries = map[string]string{
		"AuthInterface": "core auth contract",
	}
	dc.ContextPacket.Insight = "performance-critical path"

	out := serializeCompact(dc, "full")

	pos := func(marker string) int { return strings.Index(out, marker) }

	posViolation := pos("rule violation")
	posGap := pos("quality gap")
	posWarning := pos("high fan-out")
	posBlame := pos("@alice")
	posAnnotation := pos("needs refactor")
	posADR := pos("[ADR]")
	posLaws := pos("Laws:")
	posCalls := pos("Calls:")
	posCalledBy := pos("Called by:")
	posInsight := pos("Insight:")
	posRelated := pos("AuthInterface")
	// Callee detail block: second occurrence of ValidateToken (first is in "Calls:" line).
	posCalleeBlock := strings.LastIndex(out, "[ValidateToken]")

	// All sections must be present.
	for _, check := range []struct {
		name string
		p    int
	}{
		{"violation", posViolation}, {"gap", posGap}, {"warning", posWarning},
		{"blame", posBlame}, {"annotation", posAnnotation}, {"ADR", posADR},
		{"laws", posLaws}, {"calls", posCalls}, {"called by", posCalledBy},
		{"insight", posInsight}, {"related", posRelated}, {"callee block", posCalleeBlock},
	} {
		if check.p < 0 {
			t.Fatalf("section %q not found in output:\n%s", check.name, out)
		}
	}

	// === BEGINNING: safety-critical before supplementary ===
	assertBefore(t, "violations", posViolation, "blame", posBlame)
	assertBefore(t, "quality gaps", posGap, "blame", posBlame)
	assertBefore(t, "warnings", posWarning, "ADRs", posADR)

	// === MIDDLE: supplementary before actionable ===
	assertBefore(t, "blame", posBlame, "calls", posCalls)
	assertBefore(t, "annotations", posAnnotation, "calls", posCalls)
	assertBefore(t, "ADRs", posADR, "calls", posCalls)
	assertBefore(t, "laws", posLaws, "calls", posCalls)

	// === END: actionable items at the end ===
	assertBefore(t, "ADRs", posADR, "calls", posCalls)
	assertBefore(t, "ADRs", posADR, "called by", posCalledBy)

	// Full-level: related nodes (supplementary) before callee detail blocks (actionable).
	assertBefore(t, "related", posRelated, "callee block", posCalleeBlock)

	// Callee detail blocks are the last content section (before entity_hash).
	assertBefore(t, "insight", posInsight, "callee block", posCalleeBlock)
}

// TestSerializeCompact_SandwichOrdering_Neighbors verifies the sandwich pattern
// at "neighbors" detail level: warnings/safety first, supplementary middle,
// calls/called-by at end.
func TestSerializeCompact_SandwichOrdering_Neighbors(t *testing.T) {
	dc := newSandwichDC()
	out := serializeCompact(dc, "neighbors")

	pos := func(marker string) int { return strings.Index(out, marker) }

	posViolation := pos("rule violation")
	posGap := pos("quality gap")
	posWarning := pos("high fan-out")
	posBlame := pos("@alice")
	posADR := pos("[ADR]")
	posCalls := pos("Calls:")
	posCalledBy := pos("Called by:")

	for _, check := range []struct {
		name string
		p    int
	}{
		{"violation", posViolation}, {"gap", posGap}, {"warning", posWarning},
		{"blame", posBlame}, {"ADR", posADR}, {"calls", posCalls}, {"called by", posCalledBy},
	} {
		if check.p < 0 {
			t.Fatalf("section %q not found in neighbors output:\n%s", check.name, out)
		}
	}

	// Safety before supplementary.
	assertBefore(t, "violations", posViolation, "blame", posBlame)
	assertBefore(t, "quality gaps", posGap, "blame", posBlame)
	assertBefore(t, "warnings", posWarning, "ADRs", posADR)

	// Supplementary before actionable.
	assertBefore(t, "ADRs", posADR, "calls", posCalls)
	assertBefore(t, "ADRs", posADR, "called by", posCalledBy)

	// Callee detail blocks must NOT appear in neighbors level.
	if strings.Contains(out, "[ValidateToken]") {
		t.Error("neighbors level must not include callee detail blocks")
	}
}

// TestSerializeCompact_SandwichOrdering_Summary verifies violations/gaps
// appear before blame even in the summary level.
func TestSerializeCompact_SandwichOrdering_Summary(t *testing.T) {
	dc := newSandwichDC()
	out := serializeCompact(dc, "summary")

	posViolation := strings.Index(out, "rule violation")
	posGap := strings.Index(out, "quality gap")
	posBlame := strings.Index(out, "@alice")

	if posViolation < 0 || posGap < 0 || posBlame < 0 {
		t.Fatalf("expected violations, gaps, and blame in summary output:\n%s", out)
	}

	assertBefore(t, "violations", posViolation, "blame", posBlame)
	assertBefore(t, "quality gaps", posGap, "blame", posBlame)

	// Summary must NOT contain calls or callee blocks.
	if strings.Contains(out, "Calls:") {
		t.Error("summary must not contain Calls:")
	}
}

// TestSerializeCompact_NilMetadata verifies no panic when Root.Metadata is nil.
func TestSerializeCompact_NilMetadata(t *testing.T) {
	dc := &directionalContext{
		Root: &graph.Node{
			ID: "x", Name: "Foo", Type: graph.NodeFunction,
			File: "foo.go", Line: 1,
			// Metadata intentionally nil.
		},
		Enrichment: &contextEnrichment{
			RuleAlerts: []ruleAlert{
				{RuleID: "r1", Severity: "LOW", FromNode: "a", ToNode: "b", EdgeType: "CALLS"},
			},
		},
	}
	// Must not panic for any detail level.
	for _, level := range []string{"summary", "neighbors", "full"} {
		out := serializeCompact(dc, level)
		if !strings.Contains(out, "[Foo]") {
			t.Errorf("level %q: expected root entity name in output", level)
		}
		if !strings.Contains(out, "rule violation") {
			t.Errorf("level %q: expected rule violation in output", level)
		}
	}
}
