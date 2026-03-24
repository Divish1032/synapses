package namematcher

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// openTestStore creates a temporary store for testing.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("openTestStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// makeNode creates a graph node for testing.
func makeNode(id, name string, domain graph.DomainType, typ graph.NodeType, file string) *graph.Node {
	return &graph.Node{
		ID:     graph.NodeID(id),
		Name:   name,
		Domain: domain,
		Type:   typ,
		File:   file,
	}
}

// ── normalizeEntityName ────────────────────────────────────────────────────

func TestNormalizeEntityName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"PaymentService", "paymentservice"},
		{"payment-service", "paymentservice"},
		{"payment_service", "paymentservice"},
		{"payment.service", "paymentservice"},
		{"payment/service", "paymentservice"},
		{"camelCase", "camelcase"},
		{"PascalCase", "pascalcase"},
		{"ALLCAPS", "allcaps"},
		{"mixed-Case_value", "mixedcasevalue"},
		{"", ""},
	}
	for _, tc := range cases {
		got := normalizeEntityName(tc.input)
		if got != tc.want {
			t.Errorf("normalizeEntityName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ── isGenericName ──────────────────────────────────────────────────────────

func TestIsGenericName(t *testing.T) {
	blocked := []string{"main", "handler", "config", "client", "server", "service",
		"manager", "utils", "util", "helpers", "helper", "types", "type",
		"model", "models", "errors", "error", "test", "init", "new", "run",
		"start", "stop", "close", "open", "get", "set", "create", "delete",
		"update", "list", "index", "default", "base", "common", "core", "data",
		"api", "app", "db", "logger", "log", "context", "ctx", "request",
		"response", "result", "router", "options", "opts", "params",
	}
	for _, name := range blocked {
		if !isGenericName(name) {
			t.Errorf("isGenericName(%q) = false, want true", name)
		}
	}

	specific := []string{"paymentservice", "userprofile", "ordermanager", "invoicehandler"}
	for _, name := range specific {
		if isGenericName(name) {
			t.Errorf("isGenericName(%q) = true, want false", name)
		}
	}
}

// ── scoreMatch ────────────────────────────────────────────────────────────

func TestScoreMatch_ExactBoost(t *testing.T) {
	// Exact case-insensitive name match — gets exactNameBoost
	a := makeNode("a1", "PaymentService", graph.DomainCode, graph.NodeStruct, "/src/payment.go")
	b := makeNode("b1", "PaymentService", graph.DomainInfra, "resource", "/infra/payment.tf")
	exact := scoreMatch(a, b)

	// Normalized-only match (different original name, same normalized form) — no boost
	bDash := makeNode("b2", "payment-service", graph.DomainInfra, "resource", "/other/payment.tf")
	normalized := scoreMatch(a, bDash)

	if exact <= normalized {
		t.Errorf("exact match score (%v) should be higher than normalized-only (%v)", exact, normalized)
	}
}

func TestScoreMatch_SameDirBoost(t *testing.T) {
	dir := "/src/payments"
	a := makeNode("a1", "PaymentService", graph.DomainCode, graph.NodeStruct, filepath.Join(dir, "service.go"))
	b := makeNode("b1", "paymentservice", graph.DomainInfra, "resource", filepath.Join(dir, "infra.tf"))

	scoreWithDir := scoreMatch(a, b)

	bOther := makeNode("b2", "paymentservice", graph.DomainInfra, "resource", "/other/dir/infra.tf")
	scoreWithout := scoreMatch(a, bOther)

	if scoreWithDir <= scoreWithout {
		t.Errorf("same dir score (%v) should be higher than different dir (%v)", scoreWithDir, scoreWithout)
	}
}

func TestScoreMatch_Capped(t *testing.T) {
	// All boosts applied — should not exceed 1.0
	dir := "/src"
	a := makeNode("a1", "PaymentService", graph.DomainCode, graph.NodeStruct, filepath.Join(dir, "payment.go"))
	b := makeNode("b1", "PaymentService", graph.DomainInfra, "resource", filepath.Join(dir, "payment.tf"))

	score := scoreMatch(a, b)
	if score > 1.0 {
		t.Errorf("score %v exceeds 1.0", score)
	}
	if score < baseConfidence {
		t.Errorf("score %v is below base confidence %v", score, baseConfidence)
	}
}

// ── orderEdge ─────────────────────────────────────────────────────────────

func TestOrderEdge_CodeToInfra(t *testing.T) {
	code := makeNode("c1", "Svc", graph.DomainCode, graph.NodeStruct, "")
	infra := makeNode("i1", "svc", graph.DomainInfra, "resource", "")

	from, to := orderEdge(code, infra)
	if from.Domain != graph.DomainCode {
		t.Errorf("expected from domain=code, got %s", from.Domain)
	}
	if to.Domain != graph.DomainInfra {
		t.Errorf("expected to domain=infra, got %s", to.Domain)
	}
}

func TestOrderEdge_InfraToCodeReversed(t *testing.T) {
	code := makeNode("c1", "Svc", graph.DomainCode, graph.NodeStruct, "")
	infra := makeNode("i1", "svc", graph.DomainInfra, "resource", "")

	// Pass infra first — should still return code as "from"
	from, to := orderEdge(infra, code)
	if from.Domain != graph.DomainCode {
		t.Errorf("expected from domain=code, got %s", from.Domain)
	}
	if to.Domain != graph.DomainInfra {
		t.Errorf("expected to domain=infra, got %s", to.Domain)
	}
}

// ── RunAsync ──────────────────────────────────────────────────────────────

func TestRunAsync_NilGraph(t *testing.T) {
	m := New(nil)
	st := openTestStore(t)
	// Should not panic
	m.RunAsync(context.Background(), nil, st)
}

func TestRunAsync_NilStore(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	// Should not panic
	m.RunAsync(context.Background(), g, nil)
}

func TestRunAsync_EmptyGraph(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)
	// Should not panic and create no edges
	m.RunAsync(context.Background(), g, st)
	edges, err := st.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges, got %d", len(edges))
	}
}

func TestRunAsync_SameDomainSkipped(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)

	// Two code-domain nodes with the same name — should NOT be matched
	n1 := makeNode("code:PaymentService:1", "PaymentService", graph.DomainCode, graph.NodeStruct, "/src/a.go")
	n2 := makeNode("code:PaymentService:2", "PaymentService", graph.DomainCode, graph.NodeStruct, "/src/b.go")
	g.AddNode(n1)
	g.AddNode(n2)

	m.RunAsync(context.Background(), g, st)

	edges, err := st.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for same-domain pair, got %d", len(edges))
	}
}

func TestRunAsync_GenericNameSkipped(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)

	// "handler" is a generic name — should not be matched even across domains
	n1 := makeNode("code:Handler:1", "handler", graph.DomainCode, graph.NodeStruct, "/src/a.go")
	n2 := makeNode("infra:Handler:1", "handler", graph.DomainInfra, "resource", "/infra/a.tf")
	g.AddNode(n1)
	g.AddNode(n2)

	m.RunAsync(context.Background(), g, st)

	edges, err := st.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for generic name, got %d", len(edges))
	}
}

func TestRunAsync_ShortNameSkipped(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)

	// "pay" is < 4 chars after normalization — should not be matched
	n1 := makeNode("code:pay:1", "pay", graph.DomainCode, graph.NodeStruct, "/src/a.go")
	n2 := makeNode("infra:pay:1", "pay", graph.DomainInfra, "resource", "/infra/a.tf")
	g.AddNode(n1)
	g.AddNode(n2)

	m.RunAsync(context.Background(), g, st)

	edges, err := st.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for short name, got %d", len(edges))
	}
}

func TestRunAsync_CreatesEdge(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)

	// Exact cross-domain match — should create MENTIONS edge
	n1 := makeNode("code:PaymentService:1", "PaymentService", graph.DomainCode, graph.NodeStruct, "/src/payment.go")
	n2 := makeNode("infra:PaymentService:1", "PaymentService", graph.DomainInfra, "resource", "/infra/payment.tf")
	g.AddNode(n1)
	g.AddNode(n2)

	m.RunAsync(context.Background(), g, st)

	edges, err := st.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(edges))
	}
	e := edges[0]
	if e.Relation != string(graph.EdgeMentions) {
		t.Errorf("expected relation MENTIONS, got %s", e.Relation)
	}
	if e.Confidence < minConfidence {
		t.Errorf("expected confidence >= %v, got %v", minConfidence, e.Confidence)
	}
	if e.CreatedBy != "namematcher" {
		t.Errorf("expected created_by=namematcher, got %s", e.CreatedBy)
	}
}

func TestRunAsync_MinConfidence(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)

	// Normalized-only match with no boosts — baseConfidence (0.65) >= minConfidence (0.6)
	// so it will be created. This validates the scoring threshold works correctly.
	n1 := makeNode("code:OrderProcessor:1", "OrderProcessor", graph.DomainCode, graph.NodeStruct, "/src/order.go")
	n2 := makeNode("docs:order-processor:1", "order-processor", graph.DomainDocs, "section", "/docs/arch.md")
	g.AddNode(n1)
	g.AddNode(n2)

	m.RunAsync(context.Background(), g, st)

	edges, err := st.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	// base (0.65) >= min (0.6) — should create an edge
	if len(edges) != 1 {
		t.Errorf("expected 1 edge for base confidence match, got %d", len(edges))
	}
}

func TestRunAsync_Idempotent(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)

	n1 := makeNode("code:PaymentService:1", "PaymentService", graph.DomainCode, graph.NodeStruct, "/src/payment.go")
	n2 := makeNode("infra:PaymentService:1", "PaymentService", graph.DomainInfra, "resource", "/infra/payment.tf")
	g.AddNode(n1)
	g.AddNode(n2)

	// Run twice — should upsert, not duplicate
	m.RunAsync(context.Background(), g, st)
	m.RunAsync(context.Background(), g, st)

	edges, err := st.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Errorf("expected 1 edge after idempotent run, got %d", len(edges))
	}
}

func TestRunAsync_ContextCancellation(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)

	// Add many cross-domain nodes to give cancellation a chance to fire
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("uniqueentityname%04d", i)
		g.AddNode(makeNode(
			"code:"+name+":1", name, graph.DomainCode, graph.NodeStruct, "/src/a.go",
		))
		g.AddNode(makeNode(
			"infra:"+name+":1", name, graph.DomainInfra, "resource", "/infra/a.tf",
		))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Should not panic; may create 0 or some edges depending on timing
	m.RunAsync(ctx, g, st)
}

func TestRunAsync_StructuralNodeTypesSkipped(t *testing.T) {
	m := New(nil)
	g := graph.New("test-repo")
	st := openTestStore(t)

	// File and package nodes should be skipped even with good names
	n1 := makeNode("file:PaymentService:1", "PaymentService", graph.DomainCode, graph.NodeFile, "/src/payment.go")
	n2 := makeNode("infra:PaymentService:1", "PaymentService", graph.DomainInfra, "resource", "/infra/payment.tf")
	g.AddNode(n1)
	g.AddNode(n2)

	m.RunAsync(context.Background(), g, st)

	edges, err := st.LoadManualEdges()
	if err != nil {
		t.Fatalf("LoadManualEdges: %v", err)
	}
	if len(edges) != 0 {
		t.Errorf("expected 0 edges for file node type, got %d", len(edges))
	}
}
