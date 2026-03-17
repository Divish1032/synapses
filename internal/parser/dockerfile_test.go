package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Dockerfile test helpers ─────────────────────────────────────────────────

const dockerfileMultiStage = `# Build stage
FROM golang:1.21 AS builder
ARG VERSION=1.0
ENV CGO_ENABLED=0
WORKDIR /app
COPY . .
RUN go build -o myapp

# Runtime stage
FROM alpine:3.18
LABEL maintainer="dev@example.com"
COPY --from=builder /app/myapp /usr/local/bin/
EXPOSE 8080
CMD ["myapp"]
`

func parseDockerfile(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewDockerfileParser()
	if err := p.Parse(g, "/tmp/Dockerfile", []byte(src)); err != nil {
		t.Fatalf("DockerfileParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestDockerfileParser_Extensions(t *testing.T) {
	exts := parser.NewDockerfileParser().Extensions()
	want := map[string]bool{".dockerfile": true}
	for _, e := range exts {
		delete(want, e)
	}
	if len(want) > 0 {
		t.Errorf("missing extensions: %v", want)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestDockerfileParser_FileNode(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)
	fileID := g.MakeNodeID("/tmp/Dockerfile", "/tmp/Dockerfile")
	if n := g.GetNode(fileID); n == nil {
		t.Fatal("expected file node")
	} else if n.Type != graph.NodeFile {
		t.Errorf("file node type = %q, want %q", n.Type, graph.NodeFile)
	}
}

// ─── FROM stages (multi-stage build) ────────────────────────────────────────

func TestDockerfileParser_ExtractsStage(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)

	// Named stage: "builder"
	builderID := g.MakeNodeID("/tmp/Dockerfile", "builder")
	builderNode := g.GetNode(builderID)
	if builderNode == nil {
		t.Fatal("expected builder stage node")
	}
	if builderNode.Type != graph.NodeStruct {
		t.Errorf("builder type = %q, want %q", builderNode.Type, graph.NodeStruct)
	}
	if builderNode.Metadata["kind"] != "stage" {
		t.Errorf("builder kind = %q, want %q", builderNode.Metadata["kind"], "stage")
	}
	if !builderNode.Exported {
		t.Error("builder should be exported")
	}

	// Unnamed stage: "alpine" (image name used as stage name)
	alpineID := g.MakeNodeID("/tmp/Dockerfile", "alpine")
	alpineNode := g.GetNode(alpineID)
	if alpineNode == nil {
		t.Fatal("expected alpine stage node")
	}
	if alpineNode.Metadata["kind"] != "stage" {
		t.Errorf("alpine kind = %q, want %q", alpineNode.Metadata["kind"], "stage")
	}
}

func TestDockerfileParser_StageDocComment(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)
	builderID := g.MakeNodeID("/tmp/Dockerfile", "builder")
	builderNode := g.GetNode(builderID)
	if builderNode == nil {
		t.Fatal("expected builder stage node")
	}
	if builderNode.Metadata["doc"] != "Build stage" {
		t.Errorf("builder doc = %q, want %q", builderNode.Metadata["doc"], "Build stage")
	}
}

// ─── Base image imports ─────────────────────────────────────────────────────

func TestDockerfileParser_BaseImageImport(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)

	// golang:1.21 should have an import node.
	golangID := g.MakeNodeID("golang:1.21", "golang:1.21")
	golangNode := g.GetNode(golangID)
	if golangNode == nil {
		t.Fatal("expected golang:1.21 import node")
	}
	if golangNode.Type != graph.NodePackage {
		t.Errorf("golang import type = %q, want %q", golangNode.Type, graph.NodePackage)
	}

	// alpine:3.18 should have an import node.
	alpineImportID := g.MakeNodeID("alpine:3.18", "alpine:3.18")
	alpineImportNode := g.GetNode(alpineImportID)
	if alpineImportNode == nil {
		t.Fatal("expected alpine:3.18 import node")
	}
}

// ─── COPY --from edges ─────────────────────────────────────────────────────

func TestDockerfileParser_CopyFromEdge(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)

	// alpine stage should have a CALLS edge to builder (via COPY --from=builder).
	alpineID := g.MakeNodeID("/tmp/Dockerfile", "alpine")
	builderID := g.MakeNodeID("/tmp/Dockerfile", "builder")

	edges := g.OutEdges(alpineID)
	found := false
	for _, e := range edges {
		if e.To == builderID && e.Type == graph.EdgeCalls {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected CALLS edge from alpine stage to builder stage (COPY --from)")
	}
}

func TestDockerfileParser_CopyFromNumericIndex(t *testing.T) {
	src := `FROM golang:1.21 AS builder
RUN go build -o myapp

FROM alpine:3.18
COPY --from=0 /app/myapp /usr/local/bin/
`
	g := parseDockerfile(t, src)

	alpineID := g.MakeNodeID("/tmp/Dockerfile", "alpine")
	builderID := g.MakeNodeID("/tmp/Dockerfile", "builder")

	edges := g.OutEdges(alpineID)
	found := false
	for _, e := range edges {
		if e.To == builderID && e.Type == graph.EdgeCalls {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected CALLS edge from alpine to builder via numeric index COPY --from=0")
	}
}

// ─── ARG declarations ───────────────────────────────────────────────────────

func TestDockerfileParser_ArgDeclaration(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)

	argID := g.MakeNodeID("/tmp/Dockerfile", "VERSION")
	argNode := g.GetNode(argID)
	if argNode == nil {
		t.Fatal("expected VERSION arg node")
	}
	if argNode.Type != graph.NodeVariable {
		t.Errorf("arg type = %q, want %q", argNode.Type, graph.NodeVariable)
	}
	if argNode.Metadata["kind"] != "arg" {
		t.Errorf("arg kind = %q, want %q", argNode.Metadata["kind"], "arg")
	}
	if !argNode.Exported {
		t.Error("arg should be exported")
	}
}

func TestDockerfileParser_ArgNoDefault(t *testing.T) {
	src := `FROM alpine:3.18
ARG BUILD_DATE
`
	g := parseDockerfile(t, src)

	argID := g.MakeNodeID("/tmp/Dockerfile", "BUILD_DATE")
	argNode := g.GetNode(argID)
	if argNode == nil {
		t.Fatal("expected BUILD_DATE arg node")
	}
	if argNode.Metadata["kind"] != "arg" {
		t.Errorf("arg kind = %q, want %q", argNode.Metadata["kind"], "arg")
	}
}

// ─── ENV declarations ───────────────────────────────────────────────────────

func TestDockerfileParser_EnvDeclaration(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)

	envID := g.MakeNodeID("/tmp/Dockerfile", "CGO_ENABLED")
	envNode := g.GetNode(envID)
	if envNode == nil {
		t.Fatal("expected CGO_ENABLED env node")
	}
	if envNode.Type != graph.NodeVariable {
		t.Errorf("env type = %q, want %q", envNode.Type, graph.NodeVariable)
	}
	if envNode.Metadata["kind"] != "env" {
		t.Errorf("env kind = %q, want %q", envNode.Metadata["kind"], "env")
	}
}

func TestDockerfileParser_EnvSpaceSeparated(t *testing.T) {
	src := `FROM alpine:3.18
ENV APP_HOME /opt/app
`
	g := parseDockerfile(t, src)

	envID := g.MakeNodeID("/tmp/Dockerfile", "APP_HOME")
	envNode := g.GetNode(envID)
	if envNode == nil {
		t.Fatal("expected APP_HOME env node")
	}
	if envNode.Metadata["kind"] != "env" {
		t.Errorf("env kind = %q, want %q", envNode.Metadata["kind"], "env")
	}
}

// ─── EXPOSE ports ───────────────────────────────────────────────────────────

func TestDockerfileParser_ExposePort(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)

	fileID := g.MakeNodeID("/tmp/Dockerfile", "/tmp/Dockerfile")
	fileNode := g.GetNode(fileID)
	if fileNode == nil {
		t.Fatal("expected file node")
	}
	expose := fileNode.Metadata["expose"]
	if expose == "" {
		t.Fatal("expected expose metadata on file node")
	}
	if expose != "8080" {
		t.Errorf("expose = %q, want %q", expose, "8080")
	}
}

func TestDockerfileParser_ExposeMultiplePorts(t *testing.T) {
	src := `FROM alpine:3.18
EXPOSE 8080
EXPOSE 443
`
	g := parseDockerfile(t, src)

	fileID := g.MakeNodeID("/tmp/Dockerfile", "/tmp/Dockerfile")
	fileNode := g.GetNode(fileID)
	if fileNode == nil {
		t.Fatal("expected file node")
	}
	expose := fileNode.Metadata["expose"]
	if expose != "8080,443" {
		t.Errorf("expose = %q, want %q", expose, "8080,443")
	}
}

// ─── LABEL metadata ─────────────────────────────────────────────────────────

func TestDockerfileParser_LabelMetadata(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)

	fileID := g.MakeNodeID("/tmp/Dockerfile", "/tmp/Dockerfile")
	fileNode := g.GetNode(fileID)
	if fileNode == nil {
		t.Fatal("expected file node")
	}
	maintainer := fileNode.Metadata["label.maintainer"]
	if maintainer == "" {
		t.Fatal("expected label.maintainer metadata on file node")
	}
	if maintainer != "dev@example.com" {
		t.Errorf("label.maintainer = %q, want %q", maintainer, "dev@example.com")
	}
}

// ─── Edge counts ────────────────────────────────────────────────────────────

func TestDockerfileParser_DefinesEdges(t *testing.T) {
	g := parseDockerfile(t, dockerfileMultiStage)

	fileID := g.MakeNodeID("/tmp/Dockerfile", "/tmp/Dockerfile")
	edges := g.OutEdges(fileID)

	definesCount := 0
	for _, e := range edges {
		if e.Type == graph.EdgeDefines {
			definesCount++
		}
	}

	// Expected DEFINES edges: builder stage, alpine stage, VERSION arg, CGO_ENABLED env.
	if definesCount < 4 {
		t.Errorf("DEFINES edge count = %d, want >= 4", definesCount)
	}
}

// ─── Empty / minimal Dockerfile ─────────────────────────────────────────────

func TestDockerfileParser_EmptyFile(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewDockerfileParser()
	if err := p.Parse(g, "/tmp/Dockerfile", []byte("")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDockerfileParser_SingleFrom(t *testing.T) {
	src := `FROM ubuntu:22.04
`
	g := parseDockerfile(t, src)

	ubuntuID := g.MakeNodeID("/tmp/Dockerfile", "ubuntu")
	if g.GetNode(ubuntuID) == nil {
		t.Error("expected ubuntu stage node")
	}

	importID := g.MakeNodeID("ubuntu:22.04", "ubuntu:22.04")
	if g.GetNode(importID) == nil {
		t.Error("expected ubuntu:22.04 import node")
	}
}

// ─── Three-stage build ──────────────────────────────────────────────────────

func TestDockerfileParser_ThreeStages(t *testing.T) {
	src := `FROM node:18 AS deps
COPY package.json .

FROM node:18 AS build
COPY --from=deps /app/node_modules ./node_modules
RUN npm run build

FROM nginx:alpine AS runtime
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
`
	g := parseDockerfile(t, src)

	// Verify all three stages exist.
	for _, name := range []string{"deps", "build", "runtime"} {
		id := g.MakeNodeID("/tmp/Dockerfile", name)
		if g.GetNode(id) == nil {
			t.Errorf("expected stage %q", name)
		}
	}

	// Verify COPY --from edges.
	buildID := g.MakeNodeID("/tmp/Dockerfile", "build")
	depsID := g.MakeNodeID("/tmp/Dockerfile", "deps")
	edges := g.OutEdges(buildID)
	foundDeps := false
	for _, e := range edges {
		if e.To == depsID && e.Type == graph.EdgeCalls {
			foundDeps = true
		}
	}
	if !foundDeps {
		t.Error("expected CALLS edge from build to deps")
	}

	runtimeID := g.MakeNodeID("/tmp/Dockerfile", "runtime")
	edges = g.OutEdges(runtimeID)
	foundBuild := false
	for _, e := range edges {
		if e.To == buildID && e.Type == graph.EdgeCalls {
			foundBuild = true
		}
	}
	if !foundBuild {
		t.Error("expected CALLS edge from runtime to build")
	}
}
