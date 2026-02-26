package parser_test

import (
	"testing"

	"github.com/synapses/synapses/internal/graph"
	"github.com/synapses/synapses/internal/parser"
)

// goSource is a representative Go file covering all constructs the parser
// is expected to extract.
const goSource = `package auth

import (
	"fmt"
	"github.com/example/db"
)

type AuthService struct {
	repo db.UserRepo
}

type Authenticator interface {
	Login(user string) error
	Logout(user string)
}

func NewAuthService() *AuthService {
	return &AuthService{}
}

func (a *AuthService) Login(user string) error {
	fmt.Println(user)
	return nil
}

func (a *AuthService) Logout(user string) {
	fmt.Println(user)
}
`

func parseGoSource(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "auth/service.go", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	return g
}

func TestGoParser_ExtractsFileNode(t *testing.T) {
	g := parseGoSource(t, goSource)
	nodes := g.FindByName("service.go")
	if len(nodes) == 0 {
		t.Fatal("file node 'service.go' not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want %q", nodes[0].Type, graph.NodeFile)
	}
}

func TestGoParser_ExtractsStruct(t *testing.T) {
	g := parseGoSource(t, goSource)
	nodes := g.FindByName("AuthService")
	if len(nodes) == 0 {
		t.Fatal("struct node 'AuthService' not found")
	}
	if nodes[0].Type != graph.NodeStruct {
		t.Errorf("node type = %q, want %q", nodes[0].Type, graph.NodeStruct)
	}
	if !nodes[0].Exported {
		t.Error("AuthService should be marked exported")
	}
}

func TestGoParser_ExtractsInterface(t *testing.T) {
	g := parseGoSource(t, goSource)
	nodes := g.FindByName("Authenticator")
	if len(nodes) == 0 {
		t.Fatal("interface node 'Authenticator' not found")
	}
	if nodes[0].Type != graph.NodeInterface {
		t.Errorf("node type = %q, want %q", nodes[0].Type, graph.NodeInterface)
	}
}

func TestGoParser_ExtractsFunctions(t *testing.T) {
	g := parseGoSource(t, goSource)
	nodes := g.FindByName("NewAuthService")
	if len(nodes) == 0 {
		t.Fatal("function node 'NewAuthService' not found")
	}
	if nodes[0].Type != graph.NodeFunction {
		t.Errorf("node type = %q, want %q", nodes[0].Type, graph.NodeFunction)
	}
}

func TestGoParser_ExtractsMethods(t *testing.T) {
	g := parseGoSource(t, goSource)

	// Methods are stored as "ReceiverType.MethodName"
	tests := []string{"AuthService.Login", "AuthService.Logout"}
	for _, name := range tests {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("method node %q not found", name)
			continue
		}
		if nodes[0].Type != graph.NodeMethod {
			t.Errorf("%s: type = %q, want %q", name, nodes[0].Type, graph.NodeMethod)
		}
	}
}

func TestGoParser_ExtractsImports(t *testing.T) {
	g := parseGoSource(t, goSource)

	// Both "fmt" and "github.com/example/db" should become NodePackage nodes.
	fmtNodes := g.FindByName("fmt")
	if len(fmtNodes) == 0 {
		t.Error("import node 'fmt' not found")
	}
	dbNodes := g.FindByPattern("example/db")
	if len(dbNodes) == 0 {
		t.Error("import node for 'github.com/example/db' not found")
	}
}

func TestGoParser_FileDefinesEdges(t *testing.T) {
	g := parseGoSource(t, goSource)

	fileNodes := g.FindByName("service.go")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	outEdges := g.OutEdges(fileID)

	// File should have DEFINES edges to AuthService, Authenticator, NewAuthService.
	definesTargets := make(map[string]bool)
	for _, e := range outEdges {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil {
				definesTargets[n.Name] = true
			}
		}
	}

	for _, expected := range []string{"AuthService", "Authenticator", "NewAuthService"} {
		if !definesTargets[expected] {
			t.Errorf("missing DEFINES edge from file to %q", expected)
		}
	}
}

func TestGoParser_ImportEdges(t *testing.T) {
	g := parseGoSource(t, goSource)

	fileNodes := g.FindByName("service.go")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	outEdges := g.OutEdges(fileID)

	importCount := 0
	for _, e := range outEdges {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 2 {
		t.Errorf("expected ≥2 IMPORTS edges, got %d", importCount)
	}
}

func TestGoParser_LineNumbers(t *testing.T) {
	g := parseGoSource(t, goSource)
	nodes := g.FindByName("AuthService")
	if len(nodes) == 0 {
		t.Fatal("AuthService not found")
	}
	if nodes[0].Line <= 0 {
		t.Errorf("line number not set: got %d", nodes[0].Line)
	}
}

func TestGoParser_EmptyFile(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "empty.go", []byte(`package main`)); err != nil {
		t.Fatalf("Parse() on minimal file returned error: %v", err)
	}
	// Should at least produce a file node and a package import node.
	if g.NodeCount() == 0 {
		t.Error("empty file produced no nodes")
	}
}
