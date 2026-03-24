package graph_test

// Tests for Sprint 16 #5: Cross-domain impact analysis.
//
// Covers:
//   - ImpactAnalysis populates CrossDomainImpact for DEPLOYS, CONSUMES, CONFIGURED_BY
//   - ImpactAnalysis populates CrossDomainImpact for DOCUMENTS/MENTIONS (reverse direction)
//   - CrossDomainImpact is empty when no cross-domain edges exist
//   - CrossDomainImpact deduplicates (same node reachable via two edges appears once)
//   - CrossDomainCategory returns correct category strings
//   - CrossDomainImpactForNode is the public wrapper (same result as ImpactAnalysis)
//   - CrossDomainImpact does not include the root itself
//   - MANUAL edges are included in cross-domain impact

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// buildImpactFixture builds a graph for cross-domain impact tests.
//
//	paymentSvc (code) --DEPLOYS--> paymentLambda (infra)
//	paymentSvc (code) --CONSUMES--> chargeAPI (api)
//	paymentSvc (code) --CONFIGURED_BY--> paymentConfig (config/custom)
//	readmeSection (docs) --DOCUMENTS--> paymentSvc (code)   [reverse: docs→code]
//	relatedSvc (code)    --MENTIONS-->  paymentSvc (code)   [reverse: code→code]
//	paymentSvc (code) --CALLS--> helperFunc (code)          [same-domain, not cross-domain]
func buildImpactFixture(t *testing.T) (*graph.Graph, map[string]graph.NodeID) {
	t.Helper()
	g := graph.New("testrepo")

	ids := map[string]graph.NodeID{
		"paymentSvc":    g.MakeNodeID("payment/service.go", "PaymentService"),
		"paymentLambda": g.MakeNodeID("infra/payment.tf", "aws_lambda_function.payment"),
		"chargeAPI":     g.MakeNodeID("api/charge.yaml", "/charge POST"),
		"paymentConfig": g.MakeNodeID("config/payment.yaml", "payment_config"),
		"readmeSection": g.MakeNodeID("docs/README.md", "Payment Service"),
		"relatedSvc":    g.MakeNodeID("billing/service.go", "BillingService"),
		"helperFunc":    g.MakeNodeID("payment/helper.go", "buildPayload"),
	}

	g.AddNode(&graph.Node{ID: ids["paymentSvc"], Type: graph.NodeFunction, Name: "PaymentService", File: "payment/service.go", Domain: graph.DomainCode})
	g.AddNode(&graph.Node{ID: ids["paymentLambda"], Type: graph.NodeFunction, Name: "aws_lambda_function.payment", File: "infra/payment.tf", Domain: graph.DomainInfra})
	g.AddNode(&graph.Node{ID: ids["chargeAPI"], Type: graph.NodeFunction, Name: "/charge POST", File: "api/charge.yaml", Domain: graph.DomainAPI})
	g.AddNode(&graph.Node{ID: ids["paymentConfig"], Type: graph.NodeFunction, Name: "payment_config", File: "config/payment.yaml", Domain: graph.DomainCustom})
	g.AddNode(&graph.Node{ID: ids["readmeSection"], Type: graph.NodeFunction, Name: "Payment Service", File: "docs/README.md", Domain: graph.DomainDocs})
	g.AddNode(&graph.Node{ID: ids["relatedSvc"], Type: graph.NodeFunction, Name: "BillingService", File: "billing/service.go", Domain: graph.DomainCode})
	g.AddNode(&graph.Node{ID: ids["helperFunc"], Type: graph.NodeFunction, Name: "buildPayload", File: "payment/helper.go", Domain: graph.DomainCode})

	// Forward cross-domain edges from paymentSvc.
	g.AddEdge(&graph.Edge{From: ids["paymentSvc"], To: ids["paymentLambda"], Type: graph.EdgeDeploys})
	g.AddEdge(&graph.Edge{From: ids["paymentSvc"], To: ids["chargeAPI"], Type: graph.EdgeConsumes})
	g.AddEdge(&graph.Edge{From: ids["paymentSvc"], To: ids["paymentConfig"], Type: graph.EdgeConfiguredBy})

	// Reverse cross-domain edges (target is paymentSvc).
	g.AddEdge(&graph.Edge{From: ids["readmeSection"], To: ids["paymentSvc"], Type: graph.EdgeDocuments})
	g.AddEdge(&graph.Edge{From: ids["relatedSvc"], To: ids["paymentSvc"], Type: graph.EdgeMentions})

	// Same-domain CALLS edge — must NOT appear in CrossDomainImpact.
	g.AddEdge(&graph.Edge{From: ids["paymentSvc"], To: ids["helperFunc"], Type: graph.EdgeCalls})

	return g, ids
}

// findCrossDomainRef searches for a node by name in the CrossDomainImpact list.
func findCrossDomainRef(refs []graph.CrossDomainRef, name string) (graph.CrossDomainRef, bool) {
	for _, r := range refs {
		if r.Name == name {
			return r, true
		}
	}
	return graph.CrossDomainRef{}, false
}

func TestImpactAnalysisCrossDomainForwardEdges(t *testing.T) {
	g, ids := buildImpactFixture(t)

	result, err := g.ImpactAnalysis(ids["paymentSvc"], 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}

	// DEPLOYS → infra
	ref, ok := findCrossDomainRef(result.CrossDomainImpact, "aws_lambda_function.payment")
	if !ok {
		t.Error("expected DEPLOYS target in CrossDomainImpact")
	} else {
		if ref.EdgeType != graph.EdgeDeploys {
			t.Errorf("expected EdgeType=DEPLOYS, got %q", ref.EdgeType)
		}
		if ref.Category != "infra" {
			t.Errorf("expected category=infra, got %q", ref.Category)
		}
	}

	// CONSUMES → api
	ref, ok = findCrossDomainRef(result.CrossDomainImpact, "/charge POST")
	if !ok {
		t.Error("expected CONSUMES target in CrossDomainImpact")
	} else if ref.Category != "api" {
		t.Errorf("expected category=api, got %q", ref.Category)
	}

	// CONFIGURED_BY → config
	ref, ok = findCrossDomainRef(result.CrossDomainImpact, "payment_config")
	if !ok {
		t.Error("expected CONFIGURED_BY target in CrossDomainImpact")
	} else if ref.Category != "config" {
		t.Errorf("expected category=config, got %q", ref.Category)
	}
}

func TestImpactAnalysisCrossDomainReverseEdges(t *testing.T) {
	g, ids := buildImpactFixture(t)

	result, err := g.ImpactAnalysis(ids["paymentSvc"], 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}

	// DOCUMENTS (reverse): readmeSection --DOCUMENTS--> paymentSvc
	ref, ok := findCrossDomainRef(result.CrossDomainImpact, "Payment Service")
	if !ok {
		t.Error("expected DOCUMENTS source in CrossDomainImpact (reverse direction)")
	} else if ref.Category != "docs" {
		t.Errorf("expected category=docs, got %q", ref.Category)
	}

	// MENTIONS (reverse): relatedSvc --MENTIONS--> paymentSvc
	ref, ok = findCrossDomainRef(result.CrossDomainImpact, "BillingService")
	if !ok {
		t.Error("expected MENTIONS source in CrossDomainImpact (reverse direction)")
	} else if ref.Category != "related" {
		t.Errorf("expected category=related, got %q", ref.Category)
	}
}

func TestImpactAnalysisSameDomainNotInCrossDomain(t *testing.T) {
	g, ids := buildImpactFixture(t)

	result, err := g.ImpactAnalysis(ids["paymentSvc"], 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}

	// CALLS edge to helperFunc must NOT appear in CrossDomainImpact.
	if _, ok := findCrossDomainRef(result.CrossDomainImpact, "buildPayload"); ok {
		t.Error("same-domain CALLS target must not appear in CrossDomainImpact")
	}
}

func TestImpactAnalysisRootNotInCrossDomain(t *testing.T) {
	g, ids := buildImpactFixture(t)

	result, err := g.ImpactAnalysis(ids["paymentSvc"], 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}

	// Root itself must not appear in CrossDomainImpact.
	if _, ok := findCrossDomainRef(result.CrossDomainImpact, "PaymentService"); ok {
		t.Error("root entity must not appear in CrossDomainImpact")
	}
}

func TestImpactAnalysisCrossDomainEmpty(t *testing.T) {
	g := graph.New("testrepo")
	nodeID := g.MakeNodeID("main.go", "simpleFunc")
	g.AddNode(&graph.Node{ID: nodeID, Type: graph.NodeFunction, Name: "simpleFunc", File: "main.go", Domain: graph.DomainCode})

	result, err := g.ImpactAnalysis(nodeID, 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}
	if len(result.CrossDomainImpact) != 0 {
		t.Errorf("expected empty CrossDomainImpact, got %d entries", len(result.CrossDomainImpact))
	}
}

func TestImpactAnalysisCrossDomainDeduplication(t *testing.T) {
	// Same node reachable via both MENTIONS (forward) and MENTIONS (reverse) — or via two different edge types.
	// Deduplication must ensure it appears only once.
	g := graph.New("testrepo")

	codeID := g.MakeNodeID("svc.go", "MyService")
	otherID := g.MakeNodeID("other.go", "OtherService")

	g.AddNode(&graph.Node{ID: codeID, Type: graph.NodeFunction, Name: "MyService", File: "svc.go", Domain: graph.DomainCode})
	g.AddNode(&graph.Node{ID: otherID, Type: graph.NodeFunction, Name: "OtherService", File: "other.go", Domain: graph.DomainCode})

	// Forward MENTIONS: codeID → otherID
	g.AddEdge(&graph.Edge{From: codeID, To: otherID, Type: graph.EdgeMentions})
	// Reverse MENTIONS: otherID → codeID (OtherService mentions MyService back)
	g.AddEdge(&graph.Edge{From: otherID, To: codeID, Type: graph.EdgeMentions})

	result, err := g.ImpactAnalysis(codeID, 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}

	count := 0
	for _, r := range result.CrossDomainImpact {
		if r.Name == "OtherService" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("OtherService appears %d times in CrossDomainImpact, want exactly 1", count)
	}
}

func TestCrossDomainImpactForNode(t *testing.T) {
	g, ids := buildImpactFixture(t)

	// CrossDomainImpactForNode must return the same cross-domain refs as ImpactAnalysis.
	direct := g.CrossDomainImpactForNode(ids["paymentSvc"])
	via, err := g.ImpactAnalysis(ids["paymentSvc"], 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}

	if len(direct) != len(via.CrossDomainImpact) {
		t.Errorf("CrossDomainImpactForNode returned %d refs, ImpactAnalysis had %d",
			len(direct), len(via.CrossDomainImpact))
	}
}

func TestCrossDomainCategoryMapping(t *testing.T) {
	cases := []struct {
		et       graph.EdgeType
		expected string
	}{
		{graph.EdgeDeploys, "infra"},
		{graph.EdgeConsumes, "api"},
		{graph.EdgeConfiguredBy, "config"},
		{graph.EdgeDocuments, "docs"},
		{graph.EdgeMentions, "related"},
		{graph.EdgeManual, "related"},
	}
	for _, c := range cases {
		got := graph.CrossDomainCategory(c.et)
		if got != c.expected {
			t.Errorf("CrossDomainCategory(%q) = %q, want %q", c.et, got, c.expected)
		}
	}
}

func TestImpactAnalysisManualEdge(t *testing.T) {
	g := graph.New("testrepo")

	svcID := g.MakeNodeID("svc.go", "AuthService")
	configID := g.MakeNodeID("config/auth.yaml", "auth_settings")

	g.AddNode(&graph.Node{ID: svcID, Type: graph.NodeFunction, Name: "AuthService", File: "svc.go", Domain: graph.DomainCode})
	g.AddNode(&graph.Node{ID: configID, Type: graph.NodeFunction, Name: "auth_settings", File: "config/auth.yaml", Domain: graph.DomainCustom})

	// MANUAL edge: user-defined cross-domain link.
	g.AddEdge(&graph.Edge{From: svcID, To: configID, Type: graph.EdgeManual})

	result, err := g.ImpactAnalysis(svcID, 3)
	if err != nil {
		t.Fatalf("ImpactAnalysis: %v", err)
	}

	ref, ok := findCrossDomainRef(result.CrossDomainImpact, "auth_settings")
	if !ok {
		t.Fatal("expected MANUAL target in CrossDomainImpact")
	}
	if ref.Category != "related" {
		t.Errorf("expected category=related for MANUAL edge, got %q", ref.Category)
	}
}
