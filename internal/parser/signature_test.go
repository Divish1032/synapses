package parser_test

// Sprint 23.7: Entity signature extraction tests.
// Verifies that struct/class/interface/trait declarations have signatures
// populated in Node.Metadata["signature"] for all 5 primary languages.

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

func findNodeByName(g *graph.Graph, name string, typ graph.NodeType) *graph.Node {
	for _, n := range g.FindByName(name) {
		if n.Type == typ {
			return n
		}
	}
	return nil
}

// ─── Python ───────────────────────────────────────────────────────────────────

const pySignatureSource = `
class AuthService(BaseService, Authenticatable):
    """Handles authentication."""
    def login(self, user: str) -> bool:
        pass

    def logout(self, user: str) -> None:
        pass

class SimpleService:
    pass
`

func TestPythonParser_ClassSignature(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "auth/service.py", []byte(pySignatureSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	n := findNodeByName(g, "AuthService", graph.NodeStruct)
	if n == nil {
		t.Fatal("AuthService class not found")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Fatal("AuthService should have a signature")
	}
	// Signature must include class name and parent classes.
	if !strings.Contains(sig, "AuthService") || !strings.Contains(sig, "BaseService") {
		t.Errorf("class signature %q should contain 'AuthService' and 'BaseService'", sig)
	}
}

func TestPythonParser_SimpleClassSignature(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "auth/service.py", []byte(pySignatureSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	n := findNodeByName(g, "SimpleService", graph.NodeStruct)
	if n == nil {
		t.Fatal("SimpleService class not found")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Fatal("SimpleService should have a signature")
	}
	if !strings.Contains(sig, "SimpleService") {
		t.Errorf("class signature %q should contain 'SimpleService'", sig)
	}
}

// ─── Java ─────────────────────────────────────────────────────────────────────

const javaSignatureSource = `
package com.example.auth;

public class UserController extends BaseController implements IUserController {
    private UserRepository repo;

    public String login(String user) {
        return null;
    }
}

public interface IUserController {
    String login(String user);
}

public enum Role {
    ADMIN, USER, GUEST
}
`

func TestJavaParser_ClassSignature(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "auth/UserController.java", []byte(javaSignatureSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	n := findNodeByName(g, "UserController", graph.NodeStruct)
	if n == nil {
		t.Fatal("UserController class not found")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Fatal("UserController should have a signature")
	}
	if !strings.Contains(sig, "UserController") || !strings.Contains(sig, "extends") {
		t.Errorf("class signature %q should contain 'UserController' and 'extends'", sig)
	}
}

func TestJavaParser_InterfaceSignature(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "auth/UserController.java", []byte(javaSignatureSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	n := findNodeByName(g, "IUserController", graph.NodeInterface)
	if n == nil {
		t.Fatal("IUserController interface not found")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Fatal("IUserController should have a signature")
	}
	if !strings.Contains(sig, "IUserController") {
		t.Errorf("interface signature %q should contain 'IUserController'", sig)
	}
}

func TestJavaParser_EnumSignature(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "auth/UserController.java", []byte(javaSignatureSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	n := findNodeByName(g, "Role", graph.NodeStruct)
	if n == nil {
		t.Fatal("Role enum not found")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Fatal("Role enum should have a signature")
	}
	if !strings.Contains(sig, "Role") {
		t.Errorf("enum signature %q should contain 'Role'", sig)
	}
}

// ─── Rust ─────────────────────────────────────────────────────────────────────

const rustSignatureSource = `
pub struct UserRepository<T: Database> {
    db: T,
}

pub enum AuthError {
    Unauthorized,
    Forbidden,
}

pub trait Authenticatable {
    fn login(&self, user: &str) -> Result<(), AuthError>;
    fn logout(&self, user: &str);
}
`

func TestRustParser_StructSignature(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "auth/repo.rs", []byte(rustSignatureSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	n := findNodeByName(g, "UserRepository", graph.NodeStruct)
	if n == nil {
		t.Fatal("UserRepository struct not found")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Fatal("UserRepository should have a signature")
	}
	if !strings.Contains(sig, "UserRepository") {
		t.Errorf("struct signature %q should contain 'UserRepository'", sig)
	}
}

func TestRustParser_EnumSignature(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "auth/repo.rs", []byte(rustSignatureSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	n := findNodeByName(g, "AuthError", graph.NodeStruct)
	if n == nil {
		t.Fatal("AuthError enum not found")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Fatal("AuthError enum should have a signature")
	}
	if !strings.Contains(sig, "AuthError") {
		t.Errorf("enum signature %q should contain 'AuthError'", sig)
	}
}

func TestRustParser_TraitSignature(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "auth/repo.rs", []byte(rustSignatureSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	n := findNodeByName(g, "Authenticatable", graph.NodeInterface)
	if n == nil {
		t.Fatal("Authenticatable trait not found")
	}
	sig := n.Metadata["signature"]
	if sig == "" {
		t.Fatal("Authenticatable trait should have a signature")
	}
	if !strings.Contains(sig, "Authenticatable") {
		t.Errorf("trait signature %q should contain 'Authenticatable'", sig)
	}
}
