package parser_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
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

// ── IMP-IMPL-2: struct fields in metadata ─────────────────────────────────────

// structFieldsOf is a helper that finds a struct by name and returns the
// comma-separated "fields" metadata string (split into a slice for easy assertion).
func structFieldsOf(t *testing.T, g *graph.Graph, structName string) []string {
	t.Helper()
	nodes := g.FindByName(structName)
	if len(nodes) == 0 {
		t.Fatalf("struct %q not found", structName)
	}
	raw := nodes[0].Metadata["fields"]
	if raw == "" {
		return nil
	}
	// Split and trim — mirrors digest.go's rendering.
	var out []string
	for _, f := range splitComma(raw) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func TestGoParser_StructFields_SimpleTypes(t *testing.T) {
	src := `package config
type Config struct {
	Host    string
	Port    int
	Debug   bool
	Timeout float64
}
`
	g := parseGoSource(t, src)
	fields := structFieldsOf(t, g, "Config")
	want := []string{"Host string", "Port int", "Debug bool", "Timeout float64"}
	if len(fields) != len(want) {
		t.Fatalf("fields count: want %d, got %d (%v)", len(want), len(fields), fields)
	}
	for i, w := range want {
		if fields[i] != w {
			t.Errorf("field[%d]: want %q, got %q", i, w, fields[i])
		}
	}
}

func TestGoParser_StructFields_PointerAndSliceTypes(t *testing.T) {
	src := `package store
type Node struct {
	ID       string
	Children []*Node
	Parent   *Node
	Tags     []string
}
`
	g := parseGoSource(t, src)
	fields := structFieldsOf(t, g, "Node")
	// Must contain pointer and slice types with correct text.
	want := map[string]bool{
		"ID string":        true,
		"Children []*Node": true,
		"Parent *Node":     true,
		"Tags []string":    true,
	}
	for _, f := range fields {
		delete(want, f)
	}
	if len(want) > 0 {
		t.Errorf("missing fields: %v (got %v)", want, fields)
	}
}

func TestGoParser_StructFields_MultiNameDeclaration(t *testing.T) {
	// "X, Y int" — both names should appear as separate "Name type" entries.
	src := `package geom
type Point struct {
	X, Y int
	Z    float64
}
`
	g := parseGoSource(t, src)
	fields := structFieldsOf(t, g, "Point")
	wantSet := map[string]bool{"X int": true, "Y int": true, "Z float64": true}
	for _, f := range fields {
		delete(wantSet, f)
	}
	if len(wantSet) > 0 {
		t.Errorf("missing fields: %v (got %v)", wantSet, fields)
	}
}

func TestGoParser_StructFields_EmbeddedField(t *testing.T) {
	src := `package auth
import "sync"
type Service struct {
	sync.Mutex
	Name string
}
`
	g := parseGoSource(t, src)
	fields := structFieldsOf(t, g, "Service")
	// Must contain the embedded type and the named field.
	var hasName, hasEmbedded bool
	for _, f := range fields {
		if f == "Name string" {
			hasName = true
		}
		// Embedded field may appear as "sync.Mutex" or just "Mutex".
		if strings.Contains(f, "Mutex") {
			hasEmbedded = true
		}
	}
	if !hasName {
		t.Errorf("missing 'Name string' field; got %v", fields)
	}
	if !hasEmbedded {
		t.Errorf("missing embedded Mutex field; got %v", fields)
	}
}

func TestGoParser_StructFields_StructTagStripped(t *testing.T) {
	// Struct tags must NOT appear in the fields metadata.
	src := `package api
type User struct {
	ID   int    ` + "`json:\"id\"`" + `
	Name string ` + "`json:\"name\" db:\"user_name\"`" + `
}
`
	g := parseGoSource(t, src)
	fields := structFieldsOf(t, g, "User")
	for _, f := range fields {
		if strings.Contains(f, "`") || strings.Contains(f, "json:") {
			t.Errorf("struct tag leaked into fields: %q", f)
		}
	}
	wantSet := map[string]bool{"ID int": true, "Name string": true}
	for _, f := range fields {
		delete(wantSet, f)
	}
	if len(wantSet) > 0 {
		t.Errorf("missing fields: %v (got %v)", wantSet, fields)
	}
}

func TestGoParser_StructFields_Cap15(t *testing.T) {
	// Structs with >15 fields must only store the first 15.
	src := `package big
type Big struct {
	F01 int
	F02 int
	F03 int
	F04 int
	F05 int
	F06 int
	F07 int
	F08 int
	F09 int
	F10 int
	F11 int
	F12 int
	F13 int
	F14 int
	F15 int
	F16 int
	F17 int
}
`
	g := parseGoSource(t, src)
	fields := structFieldsOf(t, g, "Big")
	if len(fields) != 15 {
		t.Errorf("expected exactly 15 fields (cap), got %d: %v", len(fields), fields)
	}
}

func TestGoParser_StructFields_EmptyStruct(t *testing.T) {
	src := `package token
type Token struct{}
`
	g := parseGoSource(t, src)
	nodes := g.FindByName("Token")
	if len(nodes) == 0 {
		t.Fatal("Token struct not found")
	}
	// Empty struct should have no "fields" metadata key (or empty string).
	if raw := nodes[0].Metadata["fields"]; raw != "" {
		t.Errorf("empty struct should have no fields metadata, got %q", raw)
	}
}

// --- Sprint 23.7: entity signature extraction tests ---

func TestGoParser_StructSignature(t *testing.T) {
	g := parseGoSource(t, goSource)
	nodes := g.FindByName("AuthService")
	if len(nodes) == 0 {
		t.Fatal("AuthService not found")
	}
	sig := nodes[0].Metadata["signature"]
	if sig == "" {
		t.Fatal("AuthService should have a signature")
	}
	if sig != "type AuthService struct" {
		t.Errorf("struct signature = %q, want %q", sig, "type AuthService struct")
	}
}

func TestGoParser_InterfaceSignature(t *testing.T) {
	g := parseGoSource(t, goSource)
	nodes := g.FindByName("Authenticator")
	if len(nodes) == 0 {
		t.Fatal("Authenticator not found")
	}
	sig := nodes[0].Metadata["signature"]
	if sig == "" {
		t.Fatal("Authenticator should have a signature")
	}
	if sig != "type Authenticator interface" {
		t.Errorf("interface signature = %q, want %q", sig, "type Authenticator interface")
	}
}

func TestGoParser_TypeAliasSignature(t *testing.T) {
	src := `package api
type Handler func(w http.ResponseWriter, r *http.Request)
type UserID int64
`
	g := parseGoSource(t, src)

	handlerNodes := g.FindByName("Handler")
	if len(handlerNodes) == 0 {
		t.Fatal("Handler type not found")
	}
	handlerSig := handlerNodes[0].Metadata["signature"]
	if !strings.Contains(handlerSig, "func") {
		t.Errorf("Handler signature %q should contain 'func'", handlerSig)
	}

	idNodes := g.FindByName("UserID")
	if len(idNodes) == 0 {
		t.Fatal("UserID type not found")
	}
	idSig := idNodes[0].Metadata["signature"]
	if !strings.Contains(idSig, "UserID") || !strings.Contains(idSig, "int64") {
		t.Errorf("UserID signature %q should contain 'UserID' and 'int64'", idSig)
	}
}

func TestGoParser_GenericStructSignature(t *testing.T) {
	src := `package store
type Cache[K comparable, V any] struct {
	items map[K]V
}
`
	g := parseGoSource(t, src)
	nodes := g.FindByName("Cache")
	if len(nodes) == 0 {
		t.Fatal("Cache struct not found")
	}
	sig := nodes[0].Metadata["signature"]
	if sig == "" {
		t.Fatal("generic struct should have a signature")
	}
	if !strings.Contains(sig, "Cache") || !strings.Contains(sig, "struct") {
		t.Errorf("generic struct signature %q should contain 'Cache' and 'struct'", sig)
	}
}
