package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── HCL test helpers ─────────────────────────────────────────────────────────

const hclSource = `
# Web server instance
# Runs the main application
resource "aws_instance" "web" {
  ami           = "ami-12345"
  instance_type = "t2.micro"
}

# Ubuntu AMI lookup
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]
}

# VPC module
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "3.0.0"
}

# AWS region
variable "region" {
  type    = string
  default = "us-east-1"
}

# Public IP of the web server
output "ip" {
  value = aws_instance.web.public_ip
}

locals {
  # Environment name
  env_name = "production"
  # Application name
  app_name = "myapp"
}

provider "aws" {
  region = var.region
}

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 4.0"
    }
  }
}
`

func parseHCL(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	if err := p.Parse(g, "/tmp/main.tf", []byte(src)); err != nil {
		t.Fatalf("HCLParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ───────────────────────────────────────────────────────────────

func TestHCLParser_Extensions(t *testing.T) {
	exts := parser.NewHCLParser().Extensions()
	want := map[string]bool{".tf": true, ".tfvars": true, ".hcl": true}
	for _, e := range exts {
		delete(want, e)
	}
	if len(want) > 0 {
		t.Errorf("Extensions() missing: %v", want)
	}
}

// ─── File node ────────────────────────────────────────────────────────────────

func TestHCLParser_FileNode(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("main.tf")
	if len(nodes) == 0 {
		t.Fatal("file node main.tf not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Resource extraction ──────────────────────────────────────────────────────

func TestHCLParser_ExtractsResource(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("aws_instance.web")
	if len(nodes) == 0 {
		t.Fatal("expected aws_instance.web resource node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("resource type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "resource" {
		t.Errorf("resource kind = %q, want 'resource'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("resource should be exported")
	}
}

func TestHCLParser_ResourceDocComment(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("aws_instance.web")
	if len(nodes) == 0 {
		t.Fatal("expected aws_instance.web resource node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc == "" {
		t.Error("resource should have a doc comment")
	}
	if doc != "Web server instance Runs the main application" {
		t.Errorf("resource doc = %q, want 'Web server instance Runs the main application'", doc)
	}
}

// ─── Data source extraction ──────────────────────────────────────────────────

func TestHCLParser_ExtractsData(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("data.aws_ami.ubuntu")
	if len(nodes) == 0 {
		t.Fatal("expected data.aws_ami.ubuntu data node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("data type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "data" {
		t.Errorf("data kind = %q, want 'data'", n.Metadata["kind"])
	}
}

func TestHCLParser_DataDocComment(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("data.aws_ami.ubuntu")
	if len(nodes) == 0 {
		t.Fatal("expected data.aws_ami.ubuntu data node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc != "Ubuntu AMI lookup" {
		t.Errorf("data doc = %q, want 'Ubuntu AMI lookup'", doc)
	}
}

// ─── Module extraction ───────────────────────────────────────────────────────

func TestHCLParser_ExtractsModule(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("module.vpc")
	if len(nodes) == 0 {
		t.Fatal("expected module.vpc module node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("module type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "module" {
		t.Errorf("module kind = %q, want 'module'", n.Metadata["kind"])
	}
}

func TestHCLParser_ModuleSourceImport(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("module.vpc")
	if len(nodes) == 0 {
		t.Fatal("expected module.vpc module node")
	}
	moduleID := nodes[0].ID

	found := false
	for _, e := range g.OutEdges(moduleID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "terraform-aws-modules/vpc/aws" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected import edge for module source terraform-aws-modules/vpc/aws")
	}
}

func TestHCLParser_ModuleDocComment(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("module.vpc")
	if len(nodes) == 0 {
		t.Fatal("expected module.vpc module node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc != "VPC module" {
		t.Errorf("module doc = %q, want 'VPC module'", doc)
	}
}

// ─── Variable extraction ─────────────────────────────────────────────────────

func TestHCLParser_ExtractsVariable(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("var.region")
	if len(nodes) == 0 {
		t.Fatal("expected var.region variable node")
	}
	n := nodes[0]
	if n.Type != graph.NodeVariable {
		t.Errorf("variable type = %q, want NodeVariable", n.Type)
	}
	if n.Metadata["kind"] != "variable" {
		t.Errorf("variable kind = %q, want 'variable'", n.Metadata["kind"])
	}
}

func TestHCLParser_VariableDocComment(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("var.region")
	if len(nodes) == 0 {
		t.Fatal("expected var.region variable node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc != "AWS region" {
		t.Errorf("variable doc = %q, want 'AWS region'", doc)
	}
}

// ─── Output extraction ───────────────────────────────────────────────────────

func TestHCLParser_ExtractsOutput(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("output.ip")
	if len(nodes) == 0 {
		t.Fatal("expected output.ip output node")
	}
	n := nodes[0]
	if n.Type != graph.NodeVariable {
		t.Errorf("output type = %q, want NodeVariable", n.Type)
	}
	if n.Metadata["kind"] != "output" {
		t.Errorf("output kind = %q, want 'output'", n.Metadata["kind"])
	}
}

func TestHCLParser_OutputDocComment(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("output.ip")
	if len(nodes) == 0 {
		t.Fatal("expected output.ip output node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc != "Public IP of the web server" {
		t.Errorf("output doc = %q, want 'Public IP of the web server'", doc)
	}
}

// ─── Locals extraction ───────────────────────────────────────────────────────

func TestHCLParser_ExtractsLocals(t *testing.T) {
	g := parseHCL(t, hclSource)
	for _, name := range []string{"local.env_name", "local.app_name"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s local node", name)
			continue
		}
		n := nodes[0]
		if n.Type != graph.NodeVariable {
			t.Errorf("%s: type = %q, want NodeVariable", name, n.Type)
		}
		if n.Metadata["kind"] != "local" {
			t.Errorf("%s: kind = %q, want 'local'", name, n.Metadata["kind"])
		}
	}
}

func TestHCLParser_LocalDocComment(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("local.env_name")
	if len(nodes) == 0 {
		t.Fatal("expected local.env_name node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc != "Environment name" {
		t.Errorf("local doc = %q, want 'Environment name'", doc)
	}
}

// ─── Provider extraction ─────────────────────────────────────────────────────

func TestHCLParser_ExtractsProvider(t *testing.T) {
	g := parseHCL(t, hclSource)
	nodes := g.FindByName("provider.aws")
	if len(nodes) == 0 {
		t.Fatal("expected provider.aws provider node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("provider type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "provider" {
		t.Errorf("provider kind = %q, want 'provider'", n.Metadata["kind"])
	}
}

// ─── Terraform block (metadata only, no graph node) ──────────────────────────

func TestHCLParser_TerraformBlockNoNode(t *testing.T) {
	g := parseHCL(t, hclSource)
	// terraform blocks should not produce a graph node.
	nodes := g.FindByName("terraform")
	for _, n := range nodes {
		if n.Metadata != nil && n.Metadata["kind"] == "terraform" {
			t.Error("terraform block should not produce a graph node")
		}
	}
}

// ─── DEFINES edges ───────────────────────────────────────────────────────────

func TestHCLParser_DefinesEdges(t *testing.T) {
	g := parseHCL(t, hclSource)
	fileNodes := g.FindByName("main.tf")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	wantNames := map[string]bool{
		"aws_instance.web":     false,
		"data.aws_ami.ubuntu":  false,
		"module.vpc":           false,
		"var.region":           false,
		"output.ip":            false,
		"local.env_name":       false,
		"local.app_name":       false,
		"provider.aws":         false,
	}

	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil {
				if _, ok := wantNames[n.Name]; ok {
					wantNames[n.Name] = true
				}
			}
		}
	}

	for name, found := range wantNames {
		if !found {
			t.Errorf("no DEFINES edge from file to %s", name)
		}
	}
}

// ─── Empty file ──────────────────────────────────────────────────────────────

func TestHCLParser_EmptyFile(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	if err := p.Parse(g, "empty.tf", []byte("")); err != nil {
		t.Fatalf("Parse() on empty .tf returned error: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Error("Parse() produced zero nodes; expected at least a file node")
	}
}

// ─── .tfvars file (attributes only, no blocks) ──────────────────────────────

func TestHCLParser_TfvarsFile(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	src := []byte(`
region = "us-west-2"
instance_type = "t3.micro"
`)
	if err := p.Parse(g, "/tmp/terraform.tfvars", src); err != nil {
		t.Fatalf("Parse() on .tfvars returned error: %v", err)
	}
	// Should have at least the file node and no crashes.
	nodes := g.FindByName("terraform.tfvars")
	if len(nodes) == 0 {
		t.Fatal("file node terraform.tfvars not found")
	}
}

// ─── Multiple resources ──────────────────────────────────────────────────────

func TestHCLParser_MultipleResources(t *testing.T) {
	src := `
resource "aws_instance" "web" {
  ami = "ami-12345"
}

resource "aws_s3_bucket" "logs" {
  bucket = "my-logs"
}

resource "aws_security_group" "allow_ssh" {
  name = "allow_ssh"
}
`
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	if err := p.Parse(g, "/tmp/resources.tf", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	for _, name := range []string{"aws_instance.web", "aws_s3_bucket.logs", "aws_security_group.allow_ssh"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s resource node", name)
			continue
		}
		if nodes[0].Metadata["kind"] != "resource" {
			t.Errorf("%s: kind = %q, want 'resource'", name, nodes[0].Metadata["kind"])
		}
	}
}

// ─── Module with local source ────────────────────────────────────────────────

func TestHCLParser_ModuleLocalSource(t *testing.T) {
	src := `
module "network" {
  source = "./modules/network"
}
`
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	if err := p.Parse(g, "/tmp/main.tf", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	nodes := g.FindByName("module.network")
	if len(nodes) == 0 {
		t.Fatal("expected module.network node")
	}
	moduleID := nodes[0].ID

	found := false
	for _, e := range g.OutEdges(moduleID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "./modules/network" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected import edge for local module source ./modules/network")
	}
}

// ─── Module with git source ─────────────────────────────────────────────────

func TestHCLParser_ModuleGitSource(t *testing.T) {
	src := `
module "consul" {
  source = "git::https://example.com/vpc.git?ref=v1.2.0"
}
`
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	if err := p.Parse(g, "/tmp/main.tf", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	nodes := g.FindByName("module.consul")
	if len(nodes) == 0 {
		t.Fatal("expected module.consul node")
	}
	moduleID := nodes[0].ID

	found := false
	for _, e := range g.OutEdges(moduleID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "git::https://example.com/vpc.git?ref=v1.2.0" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected import edge for git module source")
	}
}

// ─── Doc comments with // style ─────────────────────────────────────────────

func TestHCLParser_SlashSlashDocComment(t *testing.T) {
	src := `
// Database instance
resource "aws_db_instance" "main" {
  engine = "postgres"
}
`
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	if err := p.Parse(g, "/tmp/db.tf", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	nodes := g.FindByName("aws_db_instance.main")
	if len(nodes) == 0 {
		t.Fatal("expected aws_db_instance.main node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc != "Database instance" {
		t.Errorf("doc = %q, want 'Database instance'", doc)
	}
}

// ─── Multiple locals in one block ────────────────────────────────────────────

func TestHCLParser_MultipleLocalsBlocks(t *testing.T) {
	src := `
locals {
  foo = "bar"
}

locals {
  baz = "qux"
}
`
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	if err := p.Parse(g, "/tmp/locals.tf", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	for _, name := range []string{"local.foo", "local.baz"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s local node", name)
		}
	}
}

// ─── Complex HCL file ───────────────────────────────────────────────────────

func TestHCLParser_ComplexFile(t *testing.T) {
	src := `
terraform {
  required_version = ">= 1.0"
  backend "s3" {
    bucket = "my-state"
  }
}

# Cloud provider configuration
provider "google" {
  project = "my-project"
  region  = "us-central1"
}

variable "project_id" {
  type        = string
  description = "The GCP project ID"
}

variable "zone" {
  type    = string
  default = "us-central1-a"
}

resource "google_compute_instance" "vm" {
  name         = "test-vm"
  machine_type = "e2-medium"
  zone         = var.zone
}

data "google_compute_image" "debian" {
  family  = "debian-11"
  project = "debian-cloud"
}

module "gke" {
  source     = "terraform-google-modules/kubernetes-engine/google"
  project_id = var.project_id
}

output "instance_ip" {
  value = google_compute_instance.vm.network_interface.0.network_ip
}

locals {
  region      = "us-central1"
  environment = "staging"
  labels = {
    team = "platform"
  }
}
`
	g := graph.New("testrepo")
	p := parser.NewHCLParser()
	if err := p.Parse(g, "/tmp/gcp.tf", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	checks := []struct {
		name     string
		nodeType graph.NodeType
		kind     string
	}{
		{"provider.google", graph.NodeStruct, "provider"},
		{"var.project_id", graph.NodeVariable, "variable"},
		{"var.zone", graph.NodeVariable, "variable"},
		{"google_compute_instance.vm", graph.NodeStruct, "resource"},
		{"data.google_compute_image.debian", graph.NodeStruct, "data"},
		{"module.gke", graph.NodeStruct, "module"},
		{"output.instance_ip", graph.NodeVariable, "output"},
		{"local.region", graph.NodeVariable, "local"},
		{"local.environment", graph.NodeVariable, "local"},
		{"local.labels", graph.NodeVariable, "local"},
	}

	for _, tc := range checks {
		nodes := g.FindByName(tc.name)
		if len(nodes) == 0 {
			t.Errorf("expected %s node", tc.name)
			continue
		}
		n := nodes[0]
		if n.Type != tc.nodeType {
			t.Errorf("%s: type = %q, want %q", tc.name, n.Type, tc.nodeType)
		}
		if n.Metadata["kind"] != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.name, n.Metadata["kind"], tc.kind)
		}
	}
}
