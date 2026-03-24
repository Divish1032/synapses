package resolver_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

func TestResolveTerraformRefs_CrossFile(t *testing.T) {
	g := graph.New("test-repo")
	p := parser.NewTerraformParser()

	// vpc.tf: defines the VPC resource
	vpc := []byte(`resource "aws_vpc" "main" { cidr_block = "10.0.0.0/16" }`)
	if err := p.Parse(g, "/repo/vpc.tf", vpc); err != nil {
		t.Fatalf("vpc.tf parse: %v", err)
	}

	// compute.tf: references the VPC from vpc.tf
	compute := []byte(`
resource "aws_instance" "web" {
  ami           = data.aws_ami.ubuntu.id
  vpc_id        = aws_vpc.main.id
  instance_type = var.instance_type
}
`)
	if err := p.Parse(g, "/repo/compute.tf", compute); err != nil {
		t.Fatalf("compute.tf parse: %v", err)
	}

	// Before resolution: no DEPENDS_ON edges between cross-file resources.
	preDeps := 0
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeDependsOn {
			preDeps++
		}
	}

	// Run resolver.
	n := resolver.ResolveTerraformRefs(g)
	if n == 0 {
		t.Error("ResolveTerraformRefs: expected at least 1 cross-file DEPENDS_ON edge")
	}

	// Verify the specific cross-file edge: aws_instance.web → aws_vpc.main
	nodes := g.AllNodes()
	nameToID := make(map[string]graph.NodeID)
	for _, nd := range nodes {
		nameToID[nd.Name] = nd.ID
	}

	webID, hasWeb := nameToID["aws_instance.web"]
	vpcID, hasVPC := nameToID["aws_vpc.main"]
	if !hasWeb {
		t.Fatal("node aws_instance.web not found")
	}
	if !hasVPC {
		t.Fatal("node aws_vpc.main not found")
	}

	if !g.HasEdge(webID, vpcID, graph.EdgeDependsOn) {
		t.Errorf("expected DEPENDS_ON edge: aws_instance.web → aws_vpc.main (cross-file)")
	}

	// var.instance_type should NOT produce a DEPENDS_ON edge (builtin namespace).
	varID, hasVar := nameToID["var.instance_type"]
	if hasVar && g.HasEdge(webID, varID, graph.EdgeDependsOn) {
		t.Error("unexpected DEPENDS_ON to var.instance_type (builtin namespace should be ignored)")
	}

	_ = preDeps // informational
}

func TestResolveTerraformRefs_SameFile(t *testing.T) {
	g := graph.New("test-repo")
	p := parser.NewTerraformParser()

	src := []byte(`
resource "aws_vpc" "main" { cidr_block = "10.0.0.0/16" }
resource "aws_subnet" "pub" { vpc_id = aws_vpc.main.id }
`)
	if err := p.Parse(g, "/repo/net.tf", []byte(src)); err != nil {
		t.Fatalf("parse: %v", err)
	}

	n := resolver.ResolveTerraformRefs(g)
	if n == 0 {
		t.Error("expected at least 1 DEPENDS_ON edge for same-file resource ref")
	}

	nodes := g.AllNodes()
	nameToID := make(map[string]graph.NodeID)
	for _, nd := range nodes {
		nameToID[nd.Name] = nd.ID
	}
	subnetID := nameToID["aws_subnet.pub"]
	vpcID := nameToID["aws_vpc.main"]
	if !g.HasEdge(subnetID, vpcID, graph.EdgeDependsOn) {
		t.Error("expected DEPENDS_ON: aws_subnet.pub → aws_vpc.main")
	}
}

func TestResolveTerraformRefs_NoSelfLoop(t *testing.T) {
	g := graph.New("test-repo")
	p := parser.NewTerraformParser()

	src := []byte(`resource "aws_instance" "web" { tags = { Name = "web" } }`)
	if err := p.Parse(g, "/repo/ec2.tf", []byte(src)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	resolver.ResolveTerraformRefs(g)
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeDependsOn && e.From == e.To {
			t.Errorf("self-loop DEPENDS_ON on %q", e.From)
		}
	}
}

func TestResolveTerraformRefs_DrainIsIdempotent(t *testing.T) {
	g := graph.New("test-repo")
	p := parser.NewTerraformParser()

	src := []byte(`
resource "aws_vpc" "main" {}
resource "aws_subnet" "pub" { vpc_id = aws_vpc.main.id }
`)
	if err := p.Parse(g, "/repo/net.tf", []byte(src)); err != nil {
		t.Fatalf("parse: %v", err)
	}

	n1 := resolver.ResolveTerraformRefs(g)
	n2 := resolver.ResolveTerraformRefs(g) // second call: refs already drained
	if n2 != 0 {
		t.Errorf("second ResolveTerraformRefs call: expected 0 new edges, got %d", n2)
	}
	_ = n1
}

func TestResolveTerraformRefs_BreaksCycles(t *testing.T) {
	g := graph.New("test-repo")

	// Manually add two infra resource nodes that reference each other.
	aID := graph.NodeID("node-a")
	bID := graph.NodeID("node-b")
	g.AddNode(&graph.Node{
		ID:     aID,
		Name:   "aws_instance.a",
		Domain: graph.DomainInfra,
		Metadata: map[string]string{
			"kind": "resource",
		},
	})
	g.AddNode(&graph.Node{
		ID:     bID,
		Name:   "aws_instance.b",
		Domain: graph.DomainInfra,
		Metadata: map[string]string{
			"kind": "resource",
		},
	})

	// A depends on B AND B depends on A — a cycle.
	g.AddTerraformRef(graph.TerraformRef{
		FromID:   aID,
		FromFile: "/repo/a.tf",
		RefName:  "aws_instance.b",
	})
	g.AddTerraformRef(graph.TerraformRef{
		FromID:   bID,
		FromFile: "/repo/b.tf",
		RefName:  "aws_instance.a",
	})

	resolver.ResolveTerraformRefs(g)

	// Count surviving DEPENDS_ON edges.
	depCount := 0
	for _, e := range g.AllEdges() {
		if e.Type == graph.EdgeDependsOn {
			depCount++
		}
	}
	if depCount != 1 {
		t.Errorf("expected exactly 1 DEPENDS_ON edge after cycle-breaking, got %d", depCount)
	}
}
