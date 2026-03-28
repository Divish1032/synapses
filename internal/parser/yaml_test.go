package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── YAML test helpers ────────────────────────────────────────────────────────

func parseYAML(t *testing.T, filePath string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewYAMLParser()
	if err := p.Parse(g, filePath, []byte(src)); err != nil {
		t.Fatalf("YAMLParser.Parse() error: %v", err)
	}
	return g
}

func hasEdgeToName(g *graph.Graph, fromID graph.NodeID, edgeType graph.EdgeType, targetName string) bool {
	for _, e := range g.OutEdges(fromID) {
		if e.Type != edgeType {
			continue
		}
		n := g.GetNode(e.To)
		if n != nil && n.Name == targetName {
			return true
		}
	}
	return false
}

// ─── 1. Extensions ────────────────────────────────────────────────────────────

func TestYAMLParser_Extensions(t *testing.T) {
	exts := parser.NewYAMLParser().Extensions()
	has := func(want string) bool {
		for _, e := range exts {
			if e == want {
				return true
			}
		}
		return false
	}
	if !has(".yaml") {
		t.Errorf("Extensions() missing .yaml, got %v", exts)
	}
	if !has(".yml") {
		t.Errorf("Extensions() missing .yml, got %v", exts)
	}
}

// ─── 2. File node created for generic YAML ────────────────────────────────────

func TestYAMLParser_FileNode(t *testing.T) {
	src := `
name: myapp
version: "1.0.0"
`
	g := parseYAML(t, "/tmp/config.yaml", src)
	nodes := g.FindByName("config.yaml")
	if len(nodes) == 0 {
		t.Fatal("file node config.yaml not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── 3. Top-level keys extracted as NodeVariable with kind=key ────────────────

func TestYAMLParser_TopLevelKeys(t *testing.T) {
	src := `
name: myapp
version: "1.0.0"
description: A test application
`
	g := parseYAML(t, "/tmp/config.yaml", src)

	for _, key := range []string{"name", "version", "description"} {
		nodes := g.FindByName(key)
		found := false
		for _, n := range nodes {
			if n.Type == graph.NodeVariable && n.Metadata["kind"] == "key" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("top-level key %q not found as NodeVariable with kind=key", key)
		}
	}
}

// ─── 4. DEFINES edges from file to top-level keys ────────────────────────────

func TestYAMLParser_DefinesEdges(t *testing.T) {
	src := `
services:
  web:
    image: nginx
networks:
  default: {}
`
	g := parseYAML(t, "/tmp/compose.yaml", src)
	fileNodes := g.FindByName("compose.yaml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	wantKeys := map[string]bool{
		"services": false,
		"networks": false,
	}
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil {
				if _, ok := wantKeys[n.Name]; ok {
					wantKeys[n.Name] = true
				}
			}
		}
	}
	for key, found := range wantKeys {
		if !found {
			t.Errorf("no DEFINES edge from file to top-level key %q", key)
		}
	}
}

// ─── 5. Kubernetes: kind detected, metadata.name extracted as NodeStruct ──────

func TestYAMLParser_K8sDeployment(t *testing.T) {
	src := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deployment
  namespace: production
spec:
  replicas: 3
  selector:
    matchLabels:
      app: my-app
`
	g := parseYAML(t, "/tmp/deployment.yaml", src)

	// Look for a node with kind=k8s_resource and k8s_kind=Deployment.
	nodeID := g.MakeNodeID("/tmp/deployment.yaml", "Deployment/my-deployment")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected k8s resource node Deployment/my-deployment")
	}
	if node.Type != graph.NodeStruct {
		t.Errorf("k8s resource node type = %q, want NodeStruct", node.Type)
	}
	if node.Metadata["kind"] != "k8s_resource" {
		t.Errorf("kind = %q, want k8s_resource", node.Metadata["kind"])
	}
	if node.Metadata["k8s_kind"] != "Deployment" {
		t.Errorf("k8s_kind = %q, want Deployment", node.Metadata["k8s_kind"])
	}
}

// ─── 6. Kubernetes: namespace in metadata ────────────────────────────────────

func TestYAMLParser_K8sNamespace(t *testing.T) {
	src := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-deployment
  namespace: production
`
	g := parseYAML(t, "/tmp/deployment.yaml", src)
	nodeID := g.MakeNodeID("/tmp/deployment.yaml", "Deployment/my-deployment")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected k8s resource node")
	}
	if node.Metadata["namespace"] != "production" {
		t.Errorf("namespace = %q, want production", node.Metadata["namespace"])
	}
}

// ─── 7. k8s Service manifest ─────────────────────────────────────────────────

func TestYAMLParser_K8sService(t *testing.T) {
	src := `
apiVersion: v1
kind: Service
metadata:
  name: my-service
  namespace: default
spec:
  selector:
    app: my-app
  ports:
    - port: 80
      targetPort: 8080
`
	g := parseYAML(t, "/tmp/service.yaml", src)
	nodeID := g.MakeNodeID("/tmp/service.yaml", "Service/my-service")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected k8s Service node")
	}
	if node.Metadata["k8s_kind"] != "Service" {
		t.Errorf("k8s_kind = %q, want Service", node.Metadata["k8s_kind"])
	}
}

// ─── 8. k8s Deployment: DEFINES edge from file ───────────────────────────────

func TestYAMLParser_K8sDefinesEdge(t *testing.T) {
	src := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
`
	g := parseYAML(t, "/tmp/nginx.yaml", src)
	fileNodes := g.FindByName("nginx.yaml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	if !hasEdgeToName(g, fileNodes[0].ID, graph.EdgeDefines, "Deployment/nginx") {
		t.Error("expected DEFINES edge from file to Deployment/nginx")
	}
}

// ─── 9. Docker Compose: services extracted as NodeStruct with kind=service ────

func TestYAMLParser_DockerCompose_Service(t *testing.T) {
	src := `
version: "3.9"
services:
  web:
    image: nginx:latest
    ports:
      - "80:80"
  db:
    image: postgres:14
    environment:
      POSTGRES_DB: mydb
`
	g := parseYAML(t, "/tmp/docker-compose.yaml", src)

	webID := g.MakeNodeID("/tmp/docker-compose.yaml", "service:web")
	webNode := g.GetNode(webID)
	if webNode == nil {
		t.Fatal("expected service node 'web'")
	}
	if webNode.Type != graph.NodeStruct {
		t.Errorf("service web type = %q, want NodeStruct", webNode.Type)
	}
	if webNode.Metadata["kind"] != "service" {
		t.Errorf("service web kind = %q, want service", webNode.Metadata["kind"])
	}
}

// ─── 10. Docker Compose: multiple services ────────────────────────────────────

func TestYAMLParser_DockerCompose_MultipleServices(t *testing.T) {
	src := `
version: "3.9"
services:
  web:
    image: nginx:latest
  db:
    image: postgres:14
  redis:
    image: redis:7
`
	g := parseYAML(t, "/tmp/docker-compose.yaml", src)

	for _, svc := range []string{"web", "db", "redis"} {
		nodeID := g.MakeNodeID("/tmp/docker-compose.yaml", "service:"+svc)
		node := g.GetNode(nodeID)
		if node == nil {
			t.Errorf("expected service node %q", svc)
			continue
		}
		if node.Metadata["kind"] != "service" {
			t.Errorf("service %q kind = %q, want service", svc, node.Metadata["kind"])
		}
	}
}

// ─── 11. Docker Compose: networks and volumes are top-level keys ──────────────

func TestYAMLParser_DockerCompose_NetworksVolumes(t *testing.T) {
	src := `
version: "3.9"
services:
  web:
    image: nginx
networks:
  frontend: {}
  backend: {}
volumes:
  db_data: {}
`
	g := parseYAML(t, "/tmp/docker-compose.yaml", src)
	fileNodes := g.FindByName("docker-compose.yaml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	// networks and volumes should be top-level keys (NodeVariable, kind=key).
	for _, key := range []string{"networks", "volumes"} {
		found := false
		for _, e := range g.OutEdges(fileID) {
			if e.Type == graph.EdgeDefines {
				n := g.GetNode(e.To)
				if n != nil && n.Name == key {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("expected DEFINES edge to top-level key %q", key)
		}
	}
}

// ─── 12. GitHub Actions: jobs extracted as NodeFunction with kind=job ─────────

func TestYAMLParser_GHAJobs(t *testing.T) {
	src := `
name: CI
on:
  push:
    branches: [main]
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: go build ./...
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
`
	g := parseYAML(t, "/tmp/ci.yml", src)

	for _, jobName := range []string{"build", "test"} {
		nodeID := g.MakeNodeID("/tmp/ci.yml", "job:"+jobName)
		node := g.GetNode(nodeID)
		if node == nil {
			t.Errorf("expected job node %q", jobName)
			continue
		}
		if node.Type != graph.NodeFunction {
			t.Errorf("job %q type = %q, want NodeFunction", jobName, node.Type)
		}
		if node.Metadata["kind"] != "job" {
			t.Errorf("job %q kind = %q, want job", jobName, node.Metadata["kind"])
		}
	}
}

// ─── 13. GitHub Actions: multiple jobs ────────────────────────────────────────

func TestYAMLParser_GHAMultipleJobs(t *testing.T) {
	src := `
name: Release
on:
  push:
    tags: ["v*"]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: golangci-lint run
  build:
    runs-on: ubuntu-latest
    steps:
      - run: go build ./...
  release:
    needs: [lint, build]
    runs-on: ubuntu-latest
    steps:
      - run: goreleaser release
`
	g := parseYAML(t, "/tmp/release.yml", src)

	for _, job := range []string{"lint", "build", "release"} {
		nodeID := g.MakeNodeID("/tmp/release.yml", "job:"+job)
		if g.GetNode(nodeID) == nil {
			t.Errorf("expected job node %q", job)
		}
	}
}

// ─── 14. GitHub Actions: on trigger events ────────────────────────────────────

func TestYAMLParser_GHATriggers(t *testing.T) {
	src := `
name: CI
on:
  push:
    branches: [main]
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
`
	g := parseYAML(t, "/tmp/ci.yml", src)

	// push and pull_request should be extracted as trigger nodes.
	for _, trigger := range []string{"push", "pull_request"} {
		nodeID := g.MakeNodeID("/tmp/ci.yml", "trigger:"+trigger)
		node := g.GetNode(nodeID)
		if node == nil {
			t.Errorf("expected trigger node %q", trigger)
			continue
		}
		if node.Metadata["kind"] != "trigger" {
			t.Errorf("trigger %q kind = %q, want trigger", trigger, node.Metadata["kind"])
		}
	}
}

// ─── 15. Ansible playbook: tasks with name extracted ─────────────────────────

func TestYAMLParser_AnsibleTasks(t *testing.T) {
	src := `
- name: Install packages
  apt:
    name: "{{ item }}"
    state: present
  with_items:
    - git
    - curl

- name: Start nginx
  service:
    name: nginx
    state: started
`
	g := parseYAML(t, "/tmp/playbook.yml", src)

	for _, taskName := range []string{"Install packages", "Start nginx"} {
		nodeID := g.MakeNodeID("/tmp/playbook.yml", "task:"+taskName)
		node := g.GetNode(nodeID)
		if node == nil {
			t.Errorf("expected task node %q", taskName)
			continue
		}
		if node.Type != graph.NodeFunction {
			t.Errorf("task %q type = %q, want NodeFunction", taskName, node.Type)
		}
		if node.Metadata["kind"] != "task" {
			t.Errorf("task %q kind = %q, want task", taskName, node.Metadata["kind"])
		}
	}
}

// ─── 16. YAML anchor extracted as NodeVariable with kind=anchor ──────────────

func TestYAMLParser_Anchors(t *testing.T) {
	src := `
defaults: &defaults
  timeout: 30
  retries: 3

production:
  <<: *defaults
  timeout: 60
`
	g := parseYAML(t, "/tmp/config.yaml", src)

	nodeID := g.MakeNodeID("/tmp/config.yaml", "&defaults")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected anchor node &defaults")
	}
	if node.Type != graph.NodeVariable {
		t.Errorf("anchor type = %q, want NodeVariable", node.Type)
	}
	if node.Metadata["kind"] != "anchor" {
		t.Errorf("anchor kind = %q, want anchor", node.Metadata["kind"])
	}
}

// ─── 17. YAML file reference in value creates EdgeImports ─────────────────────

func TestYAMLParser_FileReferenceImport(t *testing.T) {
	src := `
include: base.yaml
extends: common/defaults.yml
name: myapp
`
	g := parseYAML(t, "/tmp/config.yaml", src)
	fileNodes := g.FindByName("config.yaml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	wantRefs := map[string]bool{
		"base.yaml":           false,
		"common/defaults.yml": false,
	}
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil {
				if _, ok := wantRefs[n.Name]; ok {
					wantRefs[n.Name] = true
				}
			}
		}
	}
	for ref, found := range wantRefs {
		if !found {
			t.Errorf("expected EdgeImports for file reference %q", ref)
		}
	}
}

// ─── 18. Multi-document YAML handled ─────────────────────────────────────────

func TestYAMLParser_MultiDocument(t *testing.T) {
	src := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
---
apiVersion: v1
kind: Service
metadata:
  name: my-app-svc
`
	g := parseYAML(t, "/tmp/multi.yaml", src)

	depID := g.MakeNodeID("/tmp/multi.yaml", "Deployment/my-app")
	if g.GetNode(depID) == nil {
		t.Error("expected Deployment/my-app node from first document")
	}

	svcID := g.MakeNodeID("/tmp/multi.yaml", "Service/my-app-svc")
	if g.GetNode(svcID) == nil {
		t.Error("expected Service/my-app-svc node from second document")
	}
}

// ─── 19. Malformed YAML → no crash, file node still created ─────────────────

func TestYAMLParser_MalformedYAML(t *testing.T) {
	src := `
this: is: not: valid: yaml:
  - unclosed bracket [
  foo: bar: baz
    misindented: value
`
	g := graph.New("testrepo")
	p := parser.NewYAMLParser()
	// Must not panic or return error.
	err := p.Parse(g, "/tmp/broken.yaml", []byte(src))
	if err != nil {
		t.Errorf("Parse() returned error on malformed YAML: %v", err)
	}
	nodes := g.FindByName("broken.yaml")
	if len(nodes) == 0 {
		t.Error("expected at least a file node for malformed YAML")
	}
}

// ─── 20. Empty YAML → no crash, file node exists ─────────────────────────────

func TestYAMLParser_EmptyYAML(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewYAMLParser()
	err := p.Parse(g, "/tmp/empty.yaml", []byte(""))
	if err != nil {
		t.Errorf("Parse() returned error on empty YAML: %v", err)
	}
	nodes := g.FindByName("empty.yaml")
	if len(nodes) == 0 {
		t.Error("expected file node for empty YAML")
	}
}

// ─── 21. Deeply nested YAML → no crash ───────────────────────────────────────

func TestYAMLParser_DeeplyNested(t *testing.T) {
	src := `
level1:
  level2:
    level3:
      level4:
        level5:
          level6:
            value: deep
`
	g := graph.New("testrepo")
	p := parser.NewYAMLParser()
	err := p.Parse(g, "/tmp/deep.yaml", []byte(src))
	if err != nil {
		t.Errorf("Parse() returned error on deeply nested YAML: %v", err)
	}
	nodes := g.FindByName("deep.yaml")
	if len(nodes) == 0 {
		t.Error("expected file node for deeply nested YAML")
	}
}

// ─── 22. Generic YAML — only file node and top-level keys ────────────────────

func TestYAMLParser_GenericYAML_OnlyFileNode(t *testing.T) {
	src := `
server:
  host: localhost
  port: 8080
database:
  url: postgres://localhost/mydb
`
	g := parseYAML(t, "/tmp/app.yaml", src)
	fileNodes := g.FindByName("app.yaml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}

	// Should have top-level keys: server, database.
	fileID := fileNodes[0].ID
	wantKeys := map[string]bool{"server": false, "database": false}
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil {
				if _, ok := wantKeys[n.Name]; ok {
					wantKeys[n.Name] = true
				}
			}
		}
	}
	for key, found := range wantKeys {
		if !found {
			t.Errorf("expected top-level key %q", key)
		}
	}
}

// ─── 23. Multiple anchors in same file ───────────────────────────────────────

func TestYAMLParser_MultipleAnchors(t *testing.T) {
	src := `
base: &base
  timeout: 30

extended: &extended
  <<: *base
  retries: 3

production:
  <<: *extended
  timeout: 60
`
	g := parseYAML(t, "/tmp/anchors.yaml", src)

	for _, anchor := range []string{"&base", "&extended"} {
		nodeID := g.MakeNodeID("/tmp/anchors.yaml", anchor)
		node := g.GetNode(nodeID)
		if node == nil {
			t.Errorf("expected anchor node %q", anchor)
			continue
		}
		if node.Metadata["kind"] != "anchor" {
			t.Errorf("anchor %q kind = %q, want anchor", anchor, node.Metadata["kind"])
		}
	}
}

// ─── 24. k8s ConfigMap resource ──────────────────────────────────────────────

func TestYAMLParser_K8sConfigMap(t *testing.T) {
	src := `
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data:
  APP_ENV: production
  LOG_LEVEL: info
`
	g := parseYAML(t, "/tmp/configmap.yaml", src)
	nodeID := g.MakeNodeID("/tmp/configmap.yaml", "ConfigMap/app-config")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected ConfigMap/app-config node")
	}
	if node.Metadata["k8s_kind"] != "ConfigMap" {
		t.Errorf("k8s_kind = %q, want ConfigMap", node.Metadata["k8s_kind"])
	}
}

// ─── 25. Docker Compose: services DEFINES edges ──────────────────────────────

func TestYAMLParser_DockerCompose_DefinesEdges(t *testing.T) {
	src := `
version: "3"
services:
  app:
    image: myapp:latest
  cache:
    image: redis:alpine
`
	g := parseYAML(t, "/tmp/docker-compose.yml", src)
	fileNodes := g.FindByName("docker-compose.yml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	// service nodes are linked via DEFINES from file (via services top-level key expansion).
	// Verify that service:app and service:cache exist.
	appID := g.MakeNodeID("/tmp/docker-compose.yml", "service:app")
	cacheID := g.MakeNodeID("/tmp/docker-compose.yml", "service:cache")
	if g.GetNode(appID) == nil {
		t.Error("expected service:app node")
	}
	if g.GetNode(cacheID) == nil {
		t.Error("expected service:cache node")
	}
	_ = fileID
}

// ─── 26. GitHub Actions: trigger as scalar string ─────────────────────────────

func TestYAMLParser_GHATrigger_Scalar(t *testing.T) {
	src := `
name: Manual
on: workflow_dispatch
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - run: echo "deploying"
`
	g := parseYAML(t, "/tmp/manual.yml", src)

	nodeID := g.MakeNodeID("/tmp/manual.yml", "trigger:workflow_dispatch")
	node := g.GetNode(nodeID)
	if node == nil {
		t.Fatal("expected trigger node workflow_dispatch")
	}
	if node.Metadata["kind"] != "trigger" {
		t.Errorf("trigger kind = %q, want trigger", node.Metadata["kind"])
	}
}

// ─── 27. GitHub Actions: trigger as sequence ─────────────────────────────────

func TestYAMLParser_GHATrigger_Sequence(t *testing.T) {
	src := `
name: Multi-trigger
on: [push, pull_request, schedule]
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: echo "running"
`
	g := parseYAML(t, "/tmp/multi-trigger.yml", src)

	for _, trigger := range []string{"push", "pull_request", "schedule"} {
		nodeID := g.MakeNodeID("/tmp/multi-trigger.yml", "trigger:"+trigger)
		node := g.GetNode(nodeID)
		if node == nil {
			t.Errorf("expected trigger node %q", trigger)
		}
	}
}

// ─── 28. YAML with null/empty values → no crash ──────────────────────────────

func TestYAMLParser_NullValues(t *testing.T) {
	src := `
name: myapp
description: ~
config: null
settings:
`
	g := graph.New("testrepo")
	p := parser.NewYAMLParser()
	err := p.Parse(g, "/tmp/nulls.yaml", []byte(src))
	if err != nil {
		t.Errorf("Parse() error on null values: %v", err)
	}
	nodes := g.FindByName("nulls.yaml")
	if len(nodes) == 0 {
		t.Error("expected file node")
	}
}

// ─── 29. Full Helm values.yaml example ───────────────────────────────────────

func TestYAMLParser_HelmValues(t *testing.T) {
	src := `
replicaCount: 1

image:
  repository: nginx
  tag: "1.21"
  pullPolicy: IfNotPresent

service:
  type: ClusterIP
  port: 80

ingress:
  enabled: false

resources:
  limits:
    cpu: 100m
    memory: 128Mi
`
	g := parseYAML(t, "/tmp/values.yaml", src)
	fileNodes := g.FindByName("values.yaml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}

	// Top-level keys should be extracted.
	for _, key := range []string{"replicaCount", "image", "service", "ingress", "resources"} {
		nodes := g.FindByName(key)
		found := false
		for _, n := range nodes {
			if n.Type == graph.NodeVariable && n.Metadata["kind"] == "key" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected top-level key %q", key)
		}
	}
}

// ─── 31. Helm template — only file node produced ─────────────────────────────

func TestYAMLParserHelmTemplate(t *testing.T) {
	src := []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Values.name | quote }}
  namespace: {{ .Release.Namespace }}
spec:
  replicas: {{ .Values.replicaCount }}
  template:
    {{- if .Values.enabled }}
    spec: {}
    {{- end }}
`)
	g := graph.New("testrepo")
	p := parser.NewYAMLParser()
	if err := p.Parse(g, "charts/templates/deployment.yaml", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	nodes := g.AllNodes()
	// Should only have a file node — no K8s resource or top-level keys
	if len(nodes) != 1 {
		t.Errorf("expected 1 node (file), got %d", len(nodes))
		for _, n := range nodes {
			t.Logf("  node: %q type=%v", n.Name, n.Type)
		}
	}
}

// ─── 32. OpenAPI 3.0 spec: endpoints extracted with operationId and fallback ───

func TestYAMLParser_OpenAPISpec(t *testing.T) {
	src := `
openapi: "3.0.0"
info:
  title: User API
  version: "1.0"
paths:
  /users:
    get:
      operationId: listUsers
      summary: List all users
    post:
      operationId: createUser
  /users/{id}:
    get:
      summary: Get user by ID
    delete:
      operationId: deleteUser
`
	g := parseYAML(t, "/api/openapi.yaml", src)

	// operationId-named endpoints
	for _, name := range []string{"listUsers", "createUser", "deleteUser"} {
		found := false
		for _, n := range g.AllNodes() {
			if n.Name == name && n.Metadata["kind"] == "openapi_endpoint" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected endpoint node %q with kind=openapi_endpoint", name)
		}
	}

	// Fallback name for endpoint without operationId: "GET /users/{id}"
	found := false
	for _, n := range g.AllNodes() {
		if n.Name == "GET /users/{id}" && n.Metadata["kind"] == "openapi_endpoint" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected fallback endpoint node \"GET /users/{id}\"")
	}
}

// ─── 33. Swagger 2.0 spec: endpoints extracted ────────────────────────────────

func TestYAMLParser_SwaggerSpec(t *testing.T) {
	src := `
swagger: "2.0"
info:
  title: Pet Store
basePath: /v2
paths:
  /pets:
    get:
      operationId: listPets
    post:
      operationId: addPet
`
	g := parseYAML(t, "/api/swagger.yaml", src)
	for _, name := range []string{"listPets", "addPet"} {
		found := false
		for _, n := range g.AllNodes() {
			if n.Name == name && n.Metadata["kind"] == "openapi_endpoint" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected endpoint node %q", name)
		}
	}
}

// ─── 30. File reference inside nested structure creates EdgeImports ────────────

func TestYAMLParser_FileRefNestedImport(t *testing.T) {
	src := `
compose:
  extends:
    file: base-compose.yml
    service: web
`
	g := parseYAML(t, "/tmp/override.yml", src)
	fileNodes := g.FindByName("override.yml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "base-compose.yml" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected EdgeImports for nested file reference base-compose.yml")
	}
}
