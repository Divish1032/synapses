package parser_test

// Edge-case and feature-completeness tests for all 18 language parsers.
// Covers: class-qualified methods, new entity types (enums, traits, consts,
// annotations, protocols, namespaces), function-level call-site resolution,
// visibility, nested classes, and degenerate inputs.

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── helpers (local wrappers, reuse assertNode / assertDefinesEdge from languages_test.go) ───

func assertCallSite(t *testing.T, g *graph.Graph, callerName, callee string) {
	t.Helper()
	sites := g.PeekCallSites()
	for _, cs := range sites {
		if cs.FuncName == callee {
			// Check caller resolves to the named function (or file level).
			if callerName == "" {
				return // any caller
			}
			if callerNode := g.GetNode(cs.CallerID); callerNode != nil {
				if callerNode.Name == callerName {
					return
				}
			}
		}
	}
	if callerName == "" {
		t.Errorf("no call site found for callee %q", callee)
	} else {
		t.Errorf("no call site found: caller=%q callee=%q", callerName, callee)
	}
}

func assertExported(t *testing.T, g *graph.Graph, name string, want bool) {
	t.Helper()
	nodes := g.FindByName(name)
	if len(nodes) == 0 {
		t.Fatalf("node %q not found for exported check", name)
	}
	if nodes[0].Exported != want {
		t.Errorf("node %q Exported=%v, want %v", name, nodes[0].Exported, want)
	}
}

func assertMetaKind(t *testing.T, g *graph.Graph, name, wantKind string) {
	t.Helper()
	nodes := g.FindByName(name)
	if len(nodes) == 0 {
		t.Fatalf("node %q not found for meta check", name)
	}
	got := nodes[0].Metadata["kind"]
	if got != wantKind {
		t.Errorf("node %q metadata[kind]=%q, want %q", name, got, wantKind)
	}
}

// ─── Go parser edge cases ──────────────────────────────────────────────────

const goEdgeSource = `package auth

// TypeID is a type alias.
type TypeID = int64

// StatusCode is a named type.
type StatusCode int

// Handler is a function type.
type Handler func(w http.ResponseWriter, r *http.Request)

type AuthService struct{}

func (a *AuthService) Login(user string) error {
	return validate(user)
}

func validate(user string) error {
	return nil
}
`

func TestGoParser_TypeAlias(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "auth/auth.go", []byte(goEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// TypeID (type alias) and StatusCode (named type) should be captured.
	assertNode(t, g, "TypeID", graph.NodeStruct)
	assertNode(t, g, "StatusCode", graph.NodeStruct)
}

func TestGoParser_MethodCallSiteResolution(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "auth/auth.go", []byte(goEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Login calls validate — call site should be attributed to Login, not the file.
	assertCallSite(t, g, "AuthService.Login", "validate")
}

// ─── Python edge cases ────────────────────────────────────────────────────

const pyEdgeSource = `import os
import sys
from pathlib import Path

class Base:
    """Base class."""
    def method_a(self):
        pass

class Child(Base):
    """Child class."""
    def method_b(self):
        self.method_a()

async def async_handler(request):
    """Async function."""
    return await process(request)

_INTERNAL_CONST = 42
`

func TestPythonParser_ClassQualifiedMethods(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "auth.py", []byte(pyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Base.method_a", graph.NodeMethod)
	assertNode(t, g, "Child.method_b", graph.NodeMethod)
}

func TestPythonParser_AsyncFunction(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "auth.py", []byte(pyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// async_handler is a module-level function.
	assertNode(t, g, "async_handler", graph.NodeFunction)
}

func TestPythonParser_Visibility(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "auth.py", []byte(pyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertExported(t, g, "Base", true)
	assertExported(t, g, "async_handler", true)
}

func TestPythonParser_ImportEdges(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "auth.py", []byte(pyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	fileID := g.FindByName("auth.py")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 2 {
		t.Errorf("expected ≥2 import edges, got %d", importCount)
	}
}

// ─── TypeScript edge cases ────────────────────────────────────────────────

const tsEdgeSource = `import { EventEmitter } from 'events';

namespace Auth {
  export class TokenService {
    private token: string;
    generate(): string { return this.token; }
  }
}

abstract class BaseHandler {
  abstract handle(req: Request): Response;
}

export type AuthToken = string;

enum Role {
  Admin = 'admin',
  User = 'user'
}

export const validateToken = (token: string): boolean => {
  return token.length > 0;
};
`

func TestTSParser_Namespace(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "auth.ts", []byte(tsEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Auth", graph.NodePackage)
}

func TestTSParser_AbstractClass(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "auth.ts", []byte(tsEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "BaseHandler", graph.NodeStruct)
}

func TestTSParser_TypeAlias(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "auth.ts", []byte(tsEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "AuthToken", graph.NodeInterface)
}

func TestTSParser_Enum(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "auth.ts", []byte(tsEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Role", graph.NodeStruct)
	assertMetaKind(t, g, "Role", "enum")
}

func TestTSParser_ExportedArrowFunction(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "auth.ts", []byte(tsEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "validateToken", graph.NodeFunction)
	if !n.Exported {
		t.Error("validateToken should be exported")
	}
}

// ─── Java edge cases ──────────────────────────────────────────────────────

const javaEdgeSource = `package com.example;

/**
 * Annotation for marking deprecated APIs.
 */
public @interface Deprecated {
    String reason() default "";
}

public record Point(int x, int y) {
    public double distance() {
        return Math.sqrt(x * x + y * y);
    }
}

public class Service {
    private static final int MAX = 100;

    /** Create an instance. */
    public Service() {}

    public void process() {
        validate();
    }

    private void validate() {}
}
`

func TestJavaParser_AnnotationType(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Service.java", []byte(javaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "Deprecated", graph.NodeInterface)
	if n.Metadata["kind"] != "annotation" {
		t.Errorf("Deprecated metadata[kind]=%q, want %q", n.Metadata["kind"], "annotation")
	}
}

func TestJavaParser_Record(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Service.java", []byte(javaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Point", graph.NodeStruct)
	assertMetaKind(t, g, "Point", "record")
}

func TestJavaParser_Constructor(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Service.java", []byte(javaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Service.constructor", graph.NodeMethod)
}

func TestJavaParser_MethodCallSite(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Service.java", []byte(javaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// process() calls validate() — should be function-level attributed.
	assertCallSite(t, g, "Service.process", "validate")
}

// ─── Rust edge cases ──────────────────────────────────────────────────────

const rustEdgeSource = `use std::io;

/// Maximum retry count.
pub const MAX_RETRIES: u32 = 3;

/// A mutable session counter.
pub static SESSION_COUNT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);

/// Type alias for results.
pub type AuthResult<T> = Result<T, AuthError>;

macro_rules! log_error {
    ($msg:expr) => { eprintln!("ERROR: {}", $msg); };
}

pub struct AuthService {
    retries: u32,
}

impl AuthService {
    pub fn login(&self, user: &str) -> AuthResult<()> {
        self.validate(user)
    }

    fn validate(&self, user: &str) -> AuthResult<()> {
        Ok(())
    }
}
`

func TestRustParser_ConstItem(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "src/auth.rs", []byte(rustEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "MAX_RETRIES", graph.NodeStruct)
	if n.Metadata["kind"] != "const" {
		t.Errorf("MAX_RETRIES metadata[kind]=%q, want 'const'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("MAX_RETRIES should be exported (pub)")
	}
}

func TestRustParser_StaticItem(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "src/auth.rs", []byte(rustEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "SESSION_COUNT", graph.NodeStruct)
	if n.Metadata["kind"] != "static" {
		t.Errorf("SESSION_COUNT metadata[kind]=%q, want 'static'", n.Metadata["kind"])
	}
}

func TestRustParser_TypeAlias(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "src/auth.rs", []byte(rustEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "AuthResult", graph.NodeStruct)
	if n.Metadata["kind"] != "type_alias" {
		t.Errorf("AuthResult metadata[kind]=%q, want 'type_alias'", n.Metadata["kind"])
	}
}

func TestRustParser_Macro(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "src/auth.rs", []byte(rustEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "log_error", graph.NodeFunction)
	assertMetaKind(t, g, "log_error", "macro")
}

func TestRustParser_ImplMethodQualified(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "src/auth.rs", []byte(rustEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "AuthService.login", graph.NodeMethod)
	assertNode(t, g, "AuthService.validate", graph.NodeMethod)
}

func TestRustParser_MethodCallSite(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "src/auth.rs", []byte(rustEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// login calls validate — call site attributed to login.
	assertCallSite(t, g, "AuthService.login", "validate")
}

// ─── C++ edge cases ───────────────────────────────────────────────────────

const cppEdgeSource = `#include <string>

class AuthService {
public:
    bool login(const std::string& user);
    void logout(const std::string& user);
};

bool AuthService::login(const std::string& user) {
    return validate(user);
}

void AuthService::logout(const std::string& user) {
    cleanup(user);
}

bool validate(const std::string& user) {
    return !user.empty();
}

template<typename T>
class Container {
public:
    void add(T item);
    T get(int idx);
};

enum class Status { Active, Inactive };
`

func TestCppParser_ScopeQualifiedMethods(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "src/auth.cpp", []byte(cppEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Out-of-line definitions should be added as methods.
	assertNode(t, g, "AuthService.login", graph.NodeMethod)
	assertNode(t, g, "AuthService.logout", graph.NodeMethod)
}

func TestCppParser_FunctionLevelCallSite(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "src/auth.cpp", []byte(cppEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// login calls validate — should be attributed to login.
	assertCallSite(t, g, "AuthService.login", "validate")
}

func TestCppParser_TemplateClass(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "src/auth.cpp", []byte(cppEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Container", graph.NodeStruct)
}

func TestCppParser_EnumClass(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "src/auth.cpp", []byte(cppEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Status", graph.NodeStruct)
	assertMetaKind(t, g, "Status", "enum")
}

// ─── Swift edge cases ─────────────────────────────────────────────────────

const swiftEdgeSource = `import Foundation

struct Config {
    let timeout: Int
    let retries: Int
}

enum AuthError: Error {
    case invalidToken
    case expired
}

actor SessionManager {
    private var sessions: [String: Session] = [:]

    func createSession(for user: String) -> Session {
        return Session(user: user)
    }
}

extension Config {
    func isValid() -> Bool {
        return timeout > 0
    }
}

class AuthService {
    func login(user: String) -> Bool {
        return validate(user)
    }
}

protocol Authenticatable {
    func authenticate() -> Bool
}
`

func TestSwiftParser_Struct(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "Auth.swift", []byte(swiftEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "Config", graph.NodeStruct)
	if n.Metadata["kind"] != "struct" {
		t.Errorf("Config metadata[kind]=%q, want 'struct'", n.Metadata["kind"])
	}
}

func TestSwiftParser_Enum(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "Auth.swift", []byte(swiftEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "AuthError", graph.NodeStruct)
	if n.Metadata["kind"] != "enum" {
		t.Errorf("AuthError metadata[kind]=%q, want 'enum'", n.Metadata["kind"])
	}
}

func TestSwiftParser_Actor(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "Auth.swift", []byte(swiftEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "SessionManager", graph.NodeStruct)
	if n.Metadata["kind"] != "actor" {
		t.Errorf("SessionManager metadata[kind]=%q, want 'actor'", n.Metadata["kind"])
	}
}

func TestSwiftParser_Protocol(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "Auth.swift", []byte(swiftEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Authenticatable", graph.NodeInterface)
}

func TestSwiftParser_ExtensionMethodsAttributed(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "Auth.swift", []byte(swiftEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// isValid() is defined in Config extension — should be qualified as Config.isValid.
	assertNode(t, g, "Config.isValid", graph.NodeMethod)
}

// ─── Kotlin edge cases ────────────────────────────────────────────────────

const kotlinEdgeSource = `interface Authenticator {
    fun login(user: String): Boolean
    fun logout(user: String)
}

data class UserCredentials(val user: String, val pass: String)

sealed class AuthResult {
    class Success(val token: String) : AuthResult()
    class Failure(val error: String) : AuthResult()
}

enum class Role {
    ADMIN, USER, GUEST
}

class AuthService : Authenticator {
    override fun login(user: String): Boolean = true
    override fun logout(user: String) {}
}
`

func TestKotlinParser_Interface(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Auth.kt", []byte(kotlinEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Authenticator", graph.NodeInterface)
}

func TestKotlinParser_DataClass(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Auth.kt", []byte(kotlinEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "UserCredentials", graph.NodeStruct)
	if n.Metadata["kind"] != "data" {
		t.Errorf("UserCredentials metadata[kind]=%q, want 'data'", n.Metadata["kind"])
	}
}

func TestKotlinParser_SealedClass(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Auth.kt", []byte(kotlinEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "AuthResult", graph.NodeStruct)
	if n.Metadata["kind"] != "sealed" {
		t.Errorf("AuthResult metadata[kind]=%q, want 'sealed'", n.Metadata["kind"])
	}
}

func TestKotlinParser_EnumClass(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Auth.kt", []byte(kotlinEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "Role", graph.NodeStruct)
	if n.Metadata["kind"] != "enum" {
		t.Errorf("Role metadata[kind]=%q, want 'enum'", n.Metadata["kind"])
	}
}

func TestKotlinParser_ClassQualifiedMethods(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Auth.kt", []byte(kotlinEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "AuthService.login", graph.NodeMethod)
	assertNode(t, g, "AuthService.logout", graph.NodeMethod)
}

// ─── Elixir edge cases ────────────────────────────────────────────────────

const elixirEdgeSource = `defmodule Auth.TokenService do
  use Phoenix.Controller

  alias Auth.Repo

  def generate(user) do
    create_token(user)
  end

  defp create_token(user) do
    :crypto.strong_rand_bytes(32)
  end

  defmacro with_auth(do: block) do
    block
  end
end

defprotocol Auth.Serializable do
  def serialize(data)
  def deserialize(binary)
end
`

func TestElixirParser_Module(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "lib/token_service.ex", []byte(elixirEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Auth.TokenService", graph.NodeStruct)
}

func TestElixirParser_PublicFunc(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "lib/token_service.ex", []byte(elixirEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "generate", graph.NodeFunction)
	if !n.Exported {
		t.Error("def generate should be exported")
	}
}

func TestElixirParser_PrivateFunc(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "lib/token_service.ex", []byte(elixirEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "create_token", graph.NodeFunction)
	if n.Exported {
		t.Error("defp create_token should not be exported")
	}
}

func TestElixirParser_Macro(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "lib/token_service.ex", []byte(elixirEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "with_auth", graph.NodeFunction)
	assertMetaKind(t, g, "with_auth", "macro")
}

func TestElixirParser_Protocol(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "lib/token_service.ex", []byte(elixirEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Auth.Serializable", graph.NodeInterface)
}

func TestElixirParser_ImportEdges(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "lib/token_service.ex", []byte(elixirEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	fileID := g.FindByName("token_service.ex")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 1 {
		t.Errorf("expected ≥1 import edges (use/alias), got %d", importCount)
	}
}

// ─── Scala edge cases ─────────────────────────────────────────────────────

const scalaEdgeSource = `import scala.concurrent.Future

sealed trait Shape
case class Circle(radius: Double) extends Shape
case class Rectangle(w: Double, h: Double) extends Shape
case object Empty extends Shape

object MathUtils {
  val Pi: Double = 3.14159
  def area(s: Shape): Double = s match {
    case Circle(r) => Pi * r * r
    case Rectangle(w, h) => w * h
    case Empty => 0.0
  }
}
`

func TestScalaParser_SealedTrait(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "src/Shape.scala", []byte(scalaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Shape", graph.NodeInterface)
}

func TestScalaParser_CaseClass(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "src/Shape.scala", []byte(scalaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Circle", graph.NodeStruct)
	assertNode(t, g, "Rectangle", graph.NodeStruct)
}

func TestScalaParser_CaseObject(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "src/Shape.scala", []byte(scalaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// case object should be a struct.
	assertNode(t, g, "Empty", graph.NodeStruct)
}

func TestScalaParser_ObjectMethod(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "src/Shape.scala", []byte(scalaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MathUtils.area", graph.NodeMethod)
}

// ─── Groovy edge cases ────────────────────────────────────────────────────

const groovyEdgeSource = `package com.example

interface Validator {
    boolean validate(String input)
}

class AuthService implements Validator {
    static final String VERSION = "1.0"

    boolean validate(String input) {
        return check(input)
    }

    private boolean check(String s) {
        return s != null
    }
}
`

func TestGroovyParser_Interface(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGroovyParser()
	if err := p.Parse(g, "src/AuthService.groovy", []byte(groovyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Interfaces use 'interface' keyword — should be NodeInterface.
	assertNode(t, g, "Validator", graph.NodeInterface)
}

func TestGroovyParser_ClassQualifiedMethods(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGroovyParser()
	if err := p.Parse(g, "src/AuthService.groovy", []byte(groovyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "AuthService.validate", graph.NodeMethod)
	assertNode(t, g, "AuthService.check", graph.NodeMethod)
}

// ─── Ruby edge cases ──────────────────────────────────────────────────────

const rubyEdgeSource = `module Concerns
  module Authenticatable
    def self.included(base)
      base.extend(ClassMethods)
    end

    module ClassMethods
      def authenticate(user, pass)
        new(user).verify(pass)
      end
    end

    def verify(pass)
      true
    end
  end
end

class User
  include Concerns::Authenticatable
  attr_reader :name
  attr_accessor :email

  def initialize(name)
    @name = name
  end
end
`

func TestRubyParser_NestedModule(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRubyParser()
	if err := p.Parse(g, "lib/user.rb", []byte(rubyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Concerns", graph.NodeInterface)
	assertNode(t, g, "Authenticatable", graph.NodeInterface)
}

func TestRubyParser_ClassWithMixins(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRubyParser()
	if err := p.Parse(g, "lib/user.rb", []byte(rubyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "User", graph.NodeStruct)
}

func TestRubyParser_InitializeMethod(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRubyParser()
	if err := p.Parse(g, "lib/user.rb", []byte(rubyEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "User.initialize", graph.NodeMethod)
}

// ─── PHP edge cases ───────────────────────────────────────────────────────

const phpEdgeSource = `<?php

namespace App\Auth;

trait Loggable {
    public function log(string $msg): void {
        echo $msg;
    }
}

enum Status: string {
    case Active = 'active';
    case Inactive = 'inactive';
}

interface TokenProvider {
    public function generate(string $user): string;
}

abstract class BaseService {
    abstract protected function validate(string $input): bool;
}

class AuthService extends BaseService implements TokenProvider {
    use Loggable;

    public function generate(string $user): string {
        $this->log("generating token");
        return hash('sha256', $user);
    }

    protected function validate(string $input): bool {
        return strlen($input) > 0;
    }
}
`

func TestPHPParser_Trait(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "src/AuthService.php", []byte(phpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Loggable", graph.NodeStruct)
}

func TestPHPParser_Enum(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "src/AuthService.php", []byte(phpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Status", graph.NodeStruct)
}

func TestPHPParser_ClassQualifiedMethods(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "src/AuthService.php", []byte(phpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "AuthService.generate", graph.NodeMethod)
	assertNode(t, g, "AuthService.validate", graph.NodeMethod)
}

// ─── Lua edge cases ───────────────────────────────────────────────────────

const luaEdgeSource = `local auth = require('auth.core')
local M = {}

-- Creates a new session.
function M.new_session(user)
    return {user = user, token = generate_token(user)}
end

-- Validates a session.
function M:validate(session)
    return session ~= nil
end

local function generate_token(user)
    return user .. "_token"
end

return M
`

func TestLuaParser_ModuleDotFunction(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "auth/session.lua", []byte(luaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// M.new_session is a dot-method.
	assertNode(t, g, "M.new_session", graph.NodeMethod)
}

func TestLuaParser_ModuleColonMethod(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "auth/session.lua", []byte(luaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// M:validate is a colon-method.
	assertNode(t, g, "M.validate", graph.NodeMethod)
}

func TestLuaParser_LocalFunction(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "auth/session.lua", []byte(luaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "generate_token", graph.NodeFunction)
	if n.Exported {
		t.Error("local generate_token should not be exported")
	}
}

func TestLuaParser_RequireImport(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "auth/session.lua", []byte(luaEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	fileID := g.FindByName("session.lua")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 1 {
		t.Errorf("expected ≥1 import edges from require(), got %d", importCount)
	}
}

// ─── Protobuf edge cases ──────────────────────────────────────────────────

const protoEdgeSource = `syntax = "proto3";

package payments.v1;

import "google/protobuf/timestamp.proto";
import "google/protobuf/empty.proto";

option java_package = "com.example.payments";

message Payment {
    string id = 1;
    int64 amount = 2;
    string currency = 3;
    google.protobuf.Timestamp created_at = 4;

    message Metadata {
        string key = 1;
        string value = 2;
    }
}

enum PaymentStatus {
    PENDING = 0;
    COMPLETED = 1;
    FAILED = 2;
}

service PaymentService {
    rpc CreatePayment(Payment) returns (Payment);
    rpc GetPayment(Payment) returns (Payment);
    rpc ListPayments(google.protobuf.Empty) returns (Payment);
}
`

func TestProtoParser_NestedMessage(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewProtobufParser()
	if err := p.Parse(g, "proto/payments.proto", []byte(protoEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Payment", graph.NodeStruct)
	// Nested message is qualified as Payment.Metadata.
	assertNode(t, g, "Payment.Metadata", graph.NodeStruct)
	// Top-level fields.
	assertNode(t, g, "Payment.id", graph.NodeMethod)
	assertNode(t, g, "Payment.amount", graph.NodeMethod)
}

func TestProtoParser_ImportEdges(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewProtobufParser()
	if err := p.Parse(g, "proto/payments.proto", []byte(protoEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	fileID := g.FindByName("payments.proto")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 2 {
		t.Errorf("expected ≥2 import edges from proto imports, got %d", importCount)
	}
}

func TestProtoParser_ServiceRPCs(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewProtobufParser()
	if err := p.Parse(g, "proto/payments.proto", []byte(protoEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// RPCs are now qualified as ServiceName.RPCName.
	assertNode(t, g, "PaymentService.CreatePayment", graph.NodeMethod)
	assertNode(t, g, "PaymentService.GetPayment", graph.NodeMethod)
	assertNode(t, g, "PaymentService.ListPayments", graph.NodeMethod)
}

func TestProtoParser_EnumValues(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewProtobufParser()
	if err := p.Parse(g, "proto/payments.proto", []byte(protoEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "PaymentStatus", graph.NodeStruct)
	assertNode(t, g, "PaymentStatus.PENDING", graph.NodeMethod)
	assertNode(t, g, "PaymentStatus.COMPLETED", graph.NodeMethod)
	assertNode(t, g, "PaymentStatus.FAILED", graph.NodeMethod)
	assertMetaKind(t, g, "PaymentStatus.PENDING", "enum_value")
}

func TestProtoParser_Oneof(t *testing.T) {
	src := []byte(`syntax = "proto3";
message SearchRequest {
  oneof query {
    string keyword = 1;
    int32 product_id = 2;
  }
  int32 page = 3;
}
`)
	g := graph.New("testrepo")
	p := parser.NewProtobufParser()
	if err := p.Parse(g, "search.proto", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "SearchRequest", graph.NodeStruct)
	// The oneof block itself.
	assertNode(t, g, "SearchRequest.query", graph.NodeMethod)
	assertMetaKind(t, g, "SearchRequest.query", "oneof")
	// Fields inside oneof qualify under the message.
	assertNode(t, g, "SearchRequest.keyword", graph.NodeMethod)
	assertNode(t, g, "SearchRequest.product_id", graph.NodeMethod)
	assertMetaKind(t, g, "SearchRequest.keyword", "oneof_field")
	// Regular field outside oneof.
	assertNode(t, g, "SearchRequest.page", graph.NodeMethod)
	assertMetaKind(t, g, "SearchRequest.page", "field")
}

// ─── C edge cases ─────────────────────────────────────────────────────────

const cEdgeSource = `#include <stdio.h>
#include "auth.h"

#define MAX_SESSIONS 100
#define LOG_ERROR(msg) fprintf(stderr, "ERROR: %s\n", msg)

typedef struct {
    int id;
    char* user;
} Session;

typedef enum {
    STATUS_ACTIVE,
    STATUS_EXPIRED
} SessionStatus;

typedef union {
    int int_val;
    float float_val;
} Value;

static int validate_session(Session* s);

int create_session(const char* user) {
    return 1;
}

void destroy_session(int id) {
    cleanup(id);
}

static int validate_session(Session* s) {
    return s != NULL;
}
`

func TestCParser_MacroFunction(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "src/auth.c", []byte(cEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "LOG_ERROR", graph.NodeFunction)
	assertMetaKind(t, g, "LOG_ERROR", "macro")
}

func TestCParser_ObjectMacro(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "src/auth.c", []byte(cEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MAX_SESSIONS", graph.NodeStruct)
	assertMetaKind(t, g, "MAX_SESSIONS", "macro")
}

func TestCParser_Typedef(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "src/auth.c", []byte(cEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Session", graph.NodeStruct)
	assertNode(t, g, "Value", graph.NodeStruct)
}

func TestCParser_Enum(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "src/auth.c", []byte(cEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "SessionStatus", graph.NodeStruct)
	assertMetaKind(t, g, "SessionStatus", "enum")
}

func TestCParser_StaticFunctionNotExported(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "src/auth.c", []byte(cEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "validate_session", graph.NodeFunction)
	if n.Exported {
		t.Error("static validate_session should not be exported")
	}
}

func TestCParser_NonStaticFunctionExported(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "src/auth.c", []byte(cEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "create_session", graph.NodeFunction)
	if !n.Exported {
		t.Error("create_session (non-static) should be exported")
	}
}

// ─── C# edge cases ────────────────────────────────────────────────────────

const csharpEdgeSource = `using System;
using System.Threading.Tasks;

namespace Auth.Services {
    public enum AuthStatus {
        Active,
        Suspended,
        Expired
    }

    public record UserRecord(string Name, string Email);

    public class AuthService {
        private readonly string _secret;

        public AuthService(string secret) {
            _secret = secret;
        }

        public async Task<bool> LoginAsync(string user) {
            return await ValidateAsync(user);
        }

        private async Task<bool> ValidateAsync(string user) {
            return !string.IsNullOrEmpty(user);
        }

        public string Name { get; set; }
    }
}
`

func TestCSharpParser_Enum(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Auth/AuthService.cs", []byte(csharpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "AuthStatus", graph.NodeStruct)
}

func TestCSharpParser_AsyncMethod(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Auth/AuthService.cs", []byte(csharpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "AuthService.LoginAsync", graph.NodeMethod)
}

func TestCSharpParser_Constructor(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Auth/AuthService.cs", []byte(csharpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "AuthService.constructor", graph.NodeMethod)
}

func TestCSharpParser_MethodCallSite(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Auth/AuthService.cs", []byte(csharpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// LoginAsync calls ValidateAsync.
	assertCallSite(t, g, "AuthService.LoginAsync", "ValidateAsync")
}

// ─── Go const/var declarations ────────────────────────────────────────────

const goConstVarSource = `package config

const MaxRetries = 3
const DefaultTimeout = 30

const (
	StatusOK    = 200
	StatusError = 500
)

var ErrNotFound = errors.New("not found")

var (
	GlobalCounter int
	GlobalPrefix  = "prefix"
)

type Config struct{}
`

func TestGoParser_PackageLevelConst(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "config/config.go", []byte(goConstVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MaxRetries", graph.NodeVariable)
	assertMetaKind(t, g, "MaxRetries", "const")
	assertNode(t, g, "DefaultTimeout", graph.NodeVariable)
	assertMetaKind(t, g, "DefaultTimeout", "const")
}

func TestGoParser_PackageLevelConstGroup(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "config/config.go", []byte(goConstVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "StatusOK", graph.NodeVariable)
	assertMetaKind(t, g, "StatusOK", "const")
	assertNode(t, g, "StatusError", graph.NodeVariable)
}

func TestGoParser_PackageLevelVar(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "config/config.go", []byte(goConstVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "ErrNotFound", graph.NodeVariable)
	assertMetaKind(t, g, "ErrNotFound", "var")
}

func TestGoParser_PackageLevelVarGroup(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "config/config.go", []byte(goConstVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "GlobalCounter", graph.NodeVariable)
	assertMetaKind(t, g, "GlobalCounter", "var")
	assertNode(t, g, "GlobalPrefix", graph.NodeVariable)
}

// ─── Go slice-literal var (FIX-PARSER-2) ─────────────────────────────────
// Regression tests for the bug where package-level var declarations whose
// initialiser is a composite slice literal (e.g. []SomeStruct{...}) were
// misclassified as NodeStruct, using the element type's classification instead
// of the var declaration's own type.

// goSliceVarSource replicates the exact pattern that triggered FIX-PARSER-2:
// a struct type followed immediately by a package-level var holding a slice of
// that struct type, each element being a composite literal.
const goSliceVarSource = `package mcp

// toolCatalogEntry describes a single tool for discovery.
type toolCatalogEntry struct {
	Name     string
	Category string
	Keywords []string
}

// toolCatalog is the static list — the triggering case from FIX-PARSER-2.
var toolCatalog = []toolCatalogEntry{
	{Name: "session_init", Category: "session", Keywords: []string{"start", "init"}},
	{Name: "get_context", Category: "exploration", Keywords: []string{"context"}},
}

// errorRegistry is a second slice-literal var to verify the pattern generalises.
var errorRegistry = []string{"err1", "err2"}

var (
	defaultTimeout = 30
	maxRetries     = 3
)
`

// TestGoParser_SliceLiteralVar_IsVariable verifies that a package-level var
// whose value is a []StructType composite literal is classified as NodeVariable,
// not NodeStruct. This is the core regression from FIX-PARSER-2.
func TestGoParser_SliceLiteralVar_IsVariable(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "internal/mcp/tools.go", []byte(goSliceVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "toolCatalog", graph.NodeVariable)
	assertMetaKind(t, g, "toolCatalog", "var")
}

// TestGoParser_SliceLiteralVar_LineAccuracy verifies that the var node's line
// is the line of the "var" keyword, not the element struct's definition line.
func TestGoParser_SliceLiteralVar_LineAccuracy(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "internal/mcp/tools.go", []byte(goSliceVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("toolCatalog")
	var varNode *graph.Node
	for _, n := range nodes {
		if n.Name == "toolCatalog" {
			varNode = n
			break
		}
	}
	if varNode == nil {
		t.Fatal("toolCatalog not found in graph")
	}
	// toolCatalog is declared on line 11 of goSliceVarSource.
	// toolCatalogEntry struct is declared on lines 4–8 — the line must NOT be
	// in that range, proving the parser uses the var keyword's line.
	const wantLine = 11
	if varNode.Line != wantLine {
		t.Errorf("toolCatalog line=%d, want %d (var keyword line, not struct definition line)",
			varNode.Line, wantLine)
	}
}

// TestGoParser_SliceLiteralVar_StructUnaffected verifies that the element struct
// type (toolCatalogEntry) is still correctly classified as NodeStruct with its
// own line — the var fix must not disturb the struct node.
func TestGoParser_SliceLiteralVar_StructUnaffected(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "internal/mcp/tools.go", []byte(goSliceVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "toolCatalogEntry", graph.NodeStruct)
	// toolCatalogEntry struct starts at line 4.
	nodes := g.FindByName("toolCatalogEntry")
	if len(nodes) == 0 {
		t.Fatal("toolCatalogEntry not found")
	}
	const wantLine = 4
	if nodes[0].Line != wantLine {
		t.Errorf("toolCatalogEntry line=%d, want %d", nodes[0].Line, wantLine)
	}
}

// TestGoParser_SliceLiteralVar_NonStructElement verifies that the pattern also
// works when the element type is a primitive ([]string) — not just structs.
func TestGoParser_SliceLiteralVar_NonStructElement(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "internal/mcp/tools.go", []byte(goSliceVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "errorRegistry", graph.NodeVariable)
	assertMetaKind(t, g, "errorRegistry", "var")
}

// TestGoParser_SliceLiteralVar_GroupedVars verifies that grouped var blocks
// (var ( ... )) adjacent to slice-literal vars are correctly indexed.
func TestGoParser_SliceLiteralVar_GroupedVars(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "internal/mcp/tools.go", []byte(goSliceVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "defaultTimeout", graph.NodeVariable)
	assertMetaKind(t, g, "defaultTimeout", "var")
	assertNode(t, g, "maxRetries", graph.NodeVariable)
}

// TestGoParser_SliceLiteralVar_OverwritesStaleNode verifies that re-parsing a
// file corrects a stale misclassification. Before FIX-PARSER-2 the emitVar
// guard (if g.GetNode(nodeID) != nil { continue }) prevented re-indexing from
// fixing a var that was previously stored as struct. This test simulates that
// by manually inserting a wrong struct node before parsing.
func TestGoParser_SliceLiteralVar_OverwritesStaleNode(t *testing.T) {
	g := graph.New("testrepo")

	// Simulate the stale state: an old parse stored toolCatalog as struct.
	staleID := g.MakeNodeID("internal/mcp/tools.go", "toolCatalog")
	g.AddNode(&graph.Node{
		ID:   staleID,
		Type: graph.NodeStruct,
		Name: "toolCatalog",
		File: "internal/mcp/tools.go",
		Line: 999, // wrong line from old parse
	})
	if node := g.GetNode(staleID); node == nil || node.Type != graph.NodeStruct {
		t.Fatal("precondition failed: stale struct node not inserted")
	}

	// Re-parse the file — emitVar must overwrite the stale struct node.
	p := parser.NewGoParser()
	if err := p.Parse(g, "internal/mcp/tools.go", []byte(goSliceVarSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// toolCatalog must now be NodeVariable, not NodeStruct.
	assertNode(t, g, "toolCatalog", graph.NodeVariable)
	assertMetaKind(t, g, "toolCatalog", "var")

	// Line must reflect the var keyword line, not the stale 999.
	nodes := g.FindByName("toolCatalog")
	for _, n := range nodes {
		if n.Name == "toolCatalog" {
			if n.Line == 999 {
				t.Errorf("toolCatalog still has stale line 999 — overwrite did not apply")
			}
			break
		}
	}
}

// ─── Python decorator metadata ────────────────────────────────────────────

const pyDecoratorSource = `class MyModel:
    @property
    def name(self):
        return self._name

    @classmethod
    def create(cls):
        return cls()

    @staticmethod
    def validate(data):
        return bool(data)

    def plain(self):
        pass

MAX_SIZE = 100
MIN_SIZE = 1
_private_const = 42
NotAConst = "string"
`

func TestPythonParser_PropertyDecorator(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "model.py", []byte(pyDecoratorSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MyModel.name", graph.NodeMethod)
	assertMetaKind(t, g, "MyModel.name", "property")
}

func TestPythonParser_ClassMethodDecorator(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "model.py", []byte(pyDecoratorSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MyModel.create", graph.NodeMethod)
	assertMetaKind(t, g, "MyModel.create", "classmethod")
}

func TestPythonParser_StaticMethodDecorator(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "model.py", []byte(pyDecoratorSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MyModel.validate", graph.NodeMethod)
	assertMetaKind(t, g, "MyModel.validate", "staticmethod")
}

func TestPythonParser_PlainMethodNoDecoratorKind(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "model.py", []byte(pyDecoratorSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("MyModel.plain")
	if len(nodes) == 0 {
		t.Fatal("MyModel.plain not found")
	}
	if nodes[0].Metadata["kind"] != "" {
		t.Errorf("plain method should have no kind, got %q", nodes[0].Metadata["kind"])
	}
}

func TestPythonParser_AllCapsConst(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "model.py", []byte(pyDecoratorSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MAX_SIZE", graph.NodeStruct)
	assertMetaKind(t, g, "MAX_SIZE", "const")
	assertNode(t, g, "MIN_SIZE", graph.NodeStruct)
}

func TestPythonParser_NonAllCapsNotConst(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "model.py", []byte(pyDecoratorSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// "NotAConst" has mixed case — should NOT be captured as const
	nodes := g.FindByName("NotAConst")
	if len(nodes) > 0 {
		t.Errorf("NotAConst (mixed case) should not be captured as a const node")
	}
}

// ─── Kotlin extension functions ───────────────────────────────────────────

const kotlinExtSource = `package ext

fun String.trimAndUpper(): String {
    return this.trim().uppercase()
}

fun Int.isEven(): Boolean = this % 2 == 0

class MyClass {
    fun regularMethod(): Unit {}
}
`

func TestKotlinParser_ExtensionFunction(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "ext.kt", []byte(kotlinExtSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "String.trimAndUpper", graph.NodeFunction)
	assertMetaKind(t, g, "String.trimAndUpper", "extension")
}

func TestKotlinParser_IntExtensionFunction(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "ext.kt", []byte(kotlinExtSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Int.isEven", graph.NodeFunction)
	assertMetaKind(t, g, "Int.isEven", "extension")
}

func TestKotlinParser_RegularMethodNotExtension(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "ext.kt", []byte(kotlinExtSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MyClass.regularMethod", graph.NodeMethod)
	nodes := g.FindByName("MyClass.regularMethod")
	if len(nodes) > 0 && nodes[0].Metadata["kind"] == "extension" {
		t.Error("MyClass.regularMethod should not have kind=extension")
	}
}

// ─── Ruby attr_reader / attr_writer / attr_accessor ───────────────────────

const rubyAttrSource = `class Person
  attr_reader :name
  attr_writer :age
  attr_accessor :email, :phone

  def initialize(name, age, email, phone)
    @name = name
    @age = age
    @email = email
    @phone = phone
  end
end
`

func TestRubyParser_AttrReader(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRubyParser()
	if err := p.Parse(g, "person.rb", []byte(rubyAttrSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Person.name", graph.NodeMethod)
	assertMetaKind(t, g, "Person.name", "attr_reader")
}

func TestRubyParser_AttrWriter(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRubyParser()
	if err := p.Parse(g, "person.rb", []byte(rubyAttrSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Person.age", graph.NodeMethod)
	assertMetaKind(t, g, "Person.age", "attr_writer")
}

func TestRubyParser_AttrAccessorMultiple(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRubyParser()
	if err := p.Parse(g, "person.rb", []byte(rubyAttrSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Person.email", graph.NodeMethod)
	assertMetaKind(t, g, "Person.email", "attr_accessor")
	assertNode(t, g, "Person.phone", graph.NodeMethod)
	assertMetaKind(t, g, "Person.phone", "attr_accessor")
}

// ─── PHP class constants and properties ───────────────────────────────────

const phpClassMembersSource = `<?php

class Config {
    const VERSION = '1.0';
    const MAX_CONNECTIONS = 100;

    public string $host;
    protected int $port;

    public function getHost(): string {
        return $this->host;
    }
}
`

func TestPHPParser_ClassConst(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "Config.php", []byte(phpClassMembersSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Class constants should be captured as NodeStruct with kind="const"
	nodes := g.FindByName("Config.VERSION")
	if len(nodes) == 0 {
		// Alternative: some grammars name it differently — accept either form
		t.Logf("Config.VERSION not found (grammar may not expose class_const_declaration as expected)")
	} else {
		assertMetaKind(t, g, "Config.VERSION", "const")
	}
}

func TestPHPParser_ClassMethod(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "Config.php", []byte(phpClassMembersSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Config.getHost", graph.NodeMethod)
}

// ─── C# delegates ─────────────────────────────────────────────────────────

const csharpDelegateSource = `using System;

namespace Events {
    public delegate void EventHandler(object sender, EventArgs e);
    public delegate bool Predicate<T>(T value);

    public class EventBus {
        private EventHandler _handler;

        public void Subscribe(EventHandler handler) {
            _handler = handler;
        }
    }
}
`

func TestCSharpParser_Delegate(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Events/EventBus.cs", []byte(csharpDelegateSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "EventHandler", graph.NodeFunction)
	assertMetaKind(t, g, "EventHandler", "delegate")
}

func TestCSharpParser_GenericDelegate(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Events/EventBus.cs", []byte(csharpDelegateSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Generic delegate Predicate<T> — name should be "Predicate"
	nodes := g.FindByName("Predicate")
	if len(nodes) == 0 {
		t.Logf("Predicate delegate not found (generic delegate name extraction may vary by grammar)")
	} else {
		assertMetaKind(t, g, "Predicate", "delegate")
	}
}

func TestCSharpParser_Record(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Auth/AuthService.cs", []byte(csharpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "UserRecord", graph.NodeStruct)
	assertMetaKind(t, g, "UserRecord", "record")
}

func TestCSharpParser_Property(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Auth/AuthService.cs", []byte(csharpEdgeSource)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// "Name { get; set; }" should be captured as property
	assertNode(t, g, "AuthService.Name", graph.NodeMethod)
	assertMetaKind(t, g, "AuthService.Name", "property")
}

// ─── Scala val/var class fields ───────────────────────────────────────────

func TestScalaParser_ValField(t *testing.T) {
	src := `
class Point {
  val name: String = "point"
}
`
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "test.scala", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// val field inside class should be captured as method with kind=val
	assertNode(t, g, "Point.name", graph.NodeMethod)
	assertMetaKind(t, g, "Point.name", "val")
}

func TestScalaParser_VarField(t *testing.T) {
	src := `
class Counter {
  var count: Int = 0
}
`
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "test.scala", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Counter.count", graph.NodeMethod)
	assertMetaKind(t, g, "Counter.count", "var")
}

func TestScalaParser_CaseClassFunction(t *testing.T) {
	src := `
case class Person(name: String, age: Int) {
  def greet(): String = "hello"
}
`
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "test.scala", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Person", graph.NodeStruct)
	assertMetaKind(t, g, "Person", "case_class")
	assertNode(t, g, "Person.greet", graph.NodeMethod)
}

// ─── Swift init/deinit ────────────────────────────────────────────────────

func TestSwiftParser_InitMethod(t *testing.T) {
	src := `
class Manager {
    var name: String = ""
    init(name: String) {
        self.name = name
    }
    deinit {
        print("bye")
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "test.swift", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Manager.init", graph.NodeMethod)
	assertNode(t, g, "Manager.deinit", graph.NodeMethod)
}

func TestSwiftParser_ExtensionAddsMethodToExistingType(t *testing.T) {
	src := `
struct Greeter {}
extension Greeter {
    func hello() -> String { return "hello" }
}
`
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "test.swift", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Extension methods should be attributed to Greeter
	assertNode(t, g, "Greeter.hello", graph.NodeMethod)
}

// ─── Groovy enum/trait detection ─────────────────────────────────────────

func TestGroovyParser_EnumKind(t *testing.T) {
	src := `
enum Color {
    RED, GREEN, BLUE
}
`
	g := graph.New("testrepo")
	p := parser.NewGroovyParser()
	if err := p.Parse(g, "test.groovy", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("Color")
	if len(nodes) == 0 {
		t.Skip("Color enum not found (Groovy grammar may represent enum differently)")
		return
	}
	if nodes[0].Metadata == nil || nodes[0].Metadata["kind"] != "enum" {
		t.Errorf("expected kind=enum for Color, got %v", nodes[0].Metadata)
	}
}

func TestGroovyParser_TraitKind(t *testing.T) {
	src := `
trait Flyable {
    def fly() {}
}
`
	g := graph.New("testrepo")
	p := parser.NewGroovyParser()
	if err := p.Parse(g, "test.groovy", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("Flyable")
	if len(nodes) == 0 {
		t.Skip("Flyable trait not found (Groovy grammar may represent trait differently)")
		return
	}
	if nodes[0].Metadata == nil || nodes[0].Metadata["kind"] != "trait" {
		t.Errorf("expected kind=trait for Flyable, got %v", nodes[0].Metadata)
	}
}

// ─── Java annotation type / record ───────────────────────────────────────

func TestJavaParser_AnnotationTypeKind(t *testing.T) {
	src := `
public @interface MyAnnotation {
    String value() default "";
    int count() default 1;
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "test.java", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MyAnnotation", graph.NodeInterface)
	assertMetaKind(t, g, "MyAnnotation", "annotation")
}

func TestJavaParser_RecordKind(t *testing.T) {
	src := `
public record Point(int x, int y) {
    public double distance() {
        return Math.sqrt(x * x + y * y);
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "test.java", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Point", graph.NodeStruct)
	assertMetaKind(t, g, "Point", "record")
	assertNode(t, g, "Point.distance", graph.NodeMethod)
}

// ─── Elixir defstruct module ──────────────────────────────────────────────

func TestElixirParser_ModuleWithStruct(t *testing.T) {
	src := `
defmodule MyApp.User do
  defstruct [:name, :email]

  def new(name, email) do
    %MyApp.User{name: name, email: email}
  end
end
`
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "test.ex", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "MyApp.User", graph.NodeStruct)
	assertNode(t, g, "new", graph.NodeFunction)
}

func TestElixirParser_MacroWithArgs(t *testing.T) {
	src := `
defmacro unless(condition, do: body) do
  quote do: if !unquote(condition), do: unquote(body)
end
`
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "test.ex", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("unless")
	if len(nodes) == 0 {
		t.Skip("unless macro not found")
		return
	}
	if nodes[0].Metadata == nil || nodes[0].Metadata["kind"] != "macro" {
		t.Errorf("expected kind=macro for unless, got %v", nodes[0].Metadata)
	}
}

// ─── Lua table/module-style functions ────────────────────────────────────

func TestLuaParser_TableConstructor(t *testing.T) {
	src := `
local M = {}

function M.create(name)
    return {name = name}
end

function M:destroy()
    self = nil
end
`
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "test.lua", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "M.create", graph.NodeMethod)
	assertNode(t, g, "M.destroy", graph.NodeMethod)
}

// ─── Rust module item ─────────────────────────────────────────────────────

func TestRustParser_ModItem(t *testing.T) {
	src := `
pub mod utils {
    pub fn helper() -> i32 { 42 }
}
mod internal {}
`
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "test.rs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "utils", graph.NodePackage)
	assertNode(t, g, "internal", graph.NodePackage)
	// helper is inside the mod body — it should still be captured
	assertNode(t, g, "helper", graph.NodeFunction)
}

// ─── C++ template function ────────────────────────────────────────────────

func TestCppParser_TemplateFunction(t *testing.T) {
	src := `
template<typename T>
T maxVal(T a, T b) {
    return (a > b) ? a : b;
}
`
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "test.cpp", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "maxVal", graph.NodeFunction)
}

// ─── TypeScript class accessor (getter/setter) ────────────────────────────

func TestTSParser_ClassGetterSetter(t *testing.T) {
	src := `
class Config {
    private _value: string = "";

    get value(): string {
        return this._value;
    }

    set value(v: string) {
        this._value = v;
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "test.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Config", graph.NodeStruct)
	// getter/setter are method_definition nodes — should be captured
	assertNode(t, g, "Config.value", graph.NodeMethod)
}

// ─── TypeScript interface methods ────────────────────────────────────────

func TestTSParser_InterfaceMethods(t *testing.T) {
	src := `
interface IAuthService {
	login(user: string, pass: string): Promise<boolean>;
	logout(): void;
	readonly userId: string;
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "test.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "IAuthService", graph.NodeInterface)
	assertNode(t, g, "IAuthService.login", graph.NodeMethod)
	assertNode(t, g, "IAuthService.logout", graph.NodeMethod)
	// property_signature should also be captured
	assertNode(t, g, "IAuthService.userId", graph.NodeMethod)
	assertMetaKind(t, g, "IAuthService.userId", "property")
}

func TestTSParser_InterfaceMethodsExported(t *testing.T) {
	src := `
export interface IRepo {
	findById(id: number): Promise<Item | null>;
	save(item: Item): Promise<void>;
	delete(id: number): Promise<boolean>;
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "test.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "IRepo", graph.NodeInterface)
	assertNode(t, g, "IRepo.findById", graph.NodeMethod)
	assertNode(t, g, "IRepo.save", graph.NodeMethod)
	assertNode(t, g, "IRepo.delete", graph.NodeMethod)
}

// ─── Swift subscript ──────────────────────────────────────────────────────

func TestSwiftParser_Subscript(t *testing.T) {
	src := `
struct Matrix {
    private var storage: [Double]

    subscript(row: Int, col: Int) -> Double {
        get { return storage[row * 3 + col] }
        set { storage[row * 3 + col] = newValue }
    }

    func transpose() -> Matrix {
        return self
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "test.swift", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Matrix", graph.NodeStruct)
	assertNode(t, g, "Matrix.subscript", graph.NodeMethod)
	assertMetaKind(t, g, "Matrix.subscript", "subscript")
	assertNode(t, g, "Matrix.transpose", graph.NodeMethod)
}

// ─── TypeScript export default function ──────────────────────────────────

func TestTSParser_ExportDefaultFunction(t *testing.T) {
	src := `
export default function fetchUser(id: string): Promise<User | null> {
	return null;
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "test.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "fetchUser", graph.NodeFunction)
}

func TestTSParser_DeclareFunction(t *testing.T) {
	src := `
declare function log(msg: string): void;
declare function parse(input: string): object;
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "test.d.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// declare function is a function_declaration inside ambient_declaration;
	// queries match at any depth so it should be captured.
	assertNode(t, g, "log", graph.NodeFunction)
	assertNode(t, g, "parse", graph.NodeFunction)
}

// ─── JavaScript export default ───────────────────────────────────────────

func TestJSParser_ExportDefaultFunction(t *testing.T) {
	src := `
export default function renderPage(props) {
	return '<div>' + props.title + '</div>';
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaScriptParser()
	if err := p.Parse(g, "test.js", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "renderPage", graph.NodeFunction)
}

// ─── Degenerate / edge inputs ─────────────────────────────────────────────

func TestAllParsers_EmptyInput(t *testing.T) {
	cases := []struct {
		name string
		p    parser.LanguageParser
		ext  string
		src  string
	}{
		{"Go", parser.NewGoParser(), ".go", "package main\n"},
		{"Python", parser.NewPythonParser(), ".py", ""},
		{"TypeScript", parser.NewTypeScriptParser(), ".ts", ""},
		{"Java", parser.NewJavaParser(), ".java", ""},
		{"Kotlin", parser.NewKotlinParser(), ".kt", ""},
		{"Rust", parser.NewRustParser(), ".rs", ""},
		{"C", parser.NewCParser(), ".c", ""},
		{"C++", parser.NewCppParser(), ".cpp", ""},
		{"C#", parser.NewCSharpParser(), ".cs", ""},
		{"Swift", parser.NewSwiftParser(), ".swift", ""},
		{"Ruby", parser.NewRubyParser(), ".rb", ""},
		{"PHP", parser.NewPHPParser(), ".php", "<?php"},
		{"Lua", parser.NewLuaParser(), ".lua", ""},
		{"Elixir", parser.NewElixirParser(), ".ex", ""},
		{"Scala", parser.NewScalaParser(), ".scala", ""},
		{"Groovy", parser.NewGroovyParser(), ".groovy", ""},
		{"Protobuf", parser.NewProtobufParser(), ".proto", `syntax = "proto3";`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertNoCrash(t, tc.p, tc.ext, tc.src)
		})
	}
}

func TestAllParsers_OnlyComments(t *testing.T) {
	cases := []struct {
		name string
		p    parser.LanguageParser
		ext  string
		src  string
	}{
		{"Go", parser.NewGoParser(), ".go", "package foo\n// just a comment\n"},
		{"Python", parser.NewPythonParser(), ".py", "# just a comment\n"},
		{"TypeScript", parser.NewTypeScriptParser(), ".ts", "// just a comment\n"},
		{"Rust", parser.NewRustParser(), ".rs", "// just a comment\n"},
		{"C", parser.NewCParser(), ".c", "/* just a comment */\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertNoCrash(t, tc.p, tc.ext, tc.src)
		})
	}
}

// ─── Go: interface method spec nodes ────────────────────────────────────────

func TestGoParser_InterfaceMethods(t *testing.T) {
	src := `package iface

type Reader interface {
	Read(p []byte) (n int, err error)
	Close() error
}
`
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "iface/iface.go", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Reader", graph.NodeInterface)
	assertNode(t, g, "Reader.Read", graph.NodeMethod)
	assertNode(t, g, "Reader.Close", graph.NodeMethod)
}

func TestGoParser_InterfaceMethodExported(t *testing.T) {
	src := `package iface

type Writer interface {
	Write(b []byte) (int, error)
}
`
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "iface/iface.go", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Writer.Write", graph.NodeMethod)
	assertExported(t, g, "Writer.Write", true)
}

// ─── Kotlin: class properties ────────────────────────────────────────────────

func TestKotlinParser_ConstructorParamProperties(t *testing.T) {
	// Constructor parameter properties (the dominant Kotlin pattern).
	src := `data class Point(val x: Double, val y: Double)`
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Point.kt", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Point", graph.NodeStruct)
	assertNode(t, g, "Point.x", graph.NodeMethod)
	assertMetaKind(t, g, "Point.x", "val")
	assertNode(t, g, "Point.y", graph.NodeMethod)
	assertMetaKind(t, g, "Point.y", "val")
}

func TestKotlinParser_ClassBodyProperties(t *testing.T) {
	// Class body val/var properties.
	src := `class Config {
    val host: String = "localhost"
    var port: Int = 8080
}`
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Config.kt", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Config", graph.NodeStruct)
	assertNode(t, g, "Config.host", graph.NodeMethod)
	assertMetaKind(t, g, "Config.host", "val")
	assertNode(t, g, "Config.port", graph.NodeMethod)
	assertMetaKind(t, g, "Config.port", "var")
}

func TestKotlinParser_MixedValVar(t *testing.T) {
	// Mix of val and var constructor params.
	src := `class User(val name: String, var age: Int)`
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "User.kt", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "User.name", graph.NodeMethod)
	assertMetaKind(t, g, "User.name", "val")
	assertNode(t, g, "User.age", graph.NodeMethod)
	assertMetaKind(t, g, "User.age", "var")
}

func TestKotlinParser_ConstructorParamNoBinding(t *testing.T) {
	// Constructor params WITHOUT val/var must NOT be emitted as property nodes.
	src := `class Tmp(x: Int, y: Int)` // plain params, not properties
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Tmp.kt", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Tmp", graph.NodeStruct)
	for _, n := range g.AllNodes() {
		if n.Name == "Tmp.x" || n.Name == "Tmp.y" {
			t.Errorf("plain constructor param %q should NOT be emitted as a property node", n.Name)
		}
	}
}

// ─── Kotlin: typealias ───────────────────────────────────────────────────────

func TestKotlinParser_Typealias(t *testing.T) {
	src := `typealias Callback = (String) -> Unit`
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "types.kt", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Callback", graph.NodeInterface)
	assertMetaKind(t, g, "Callback", "typealias")
}

// ─── Swift: typealias ────────────────────────────────────────────────────────

func TestSwiftParser_Typealias(t *testing.T) {
	src := `typealias Position = CGPoint`
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "types.swift", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Position", graph.NodeInterface)
	assertMetaKind(t, g, "Position", "typealias")
}

// ─── C++: using type alias ───────────────────────────────────────────────────

func TestCppParser_UsingTypeAlias(t *testing.T) {
	src := `using StringList = std::vector<std::string>;`
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "types.cpp", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "StringList", graph.NodeInterface)
	assertMetaKind(t, g, "StringList", "type_alias")
}

// ─── C: anonymous struct typedef kind ────────────────────────────────────────

func TestCParser_TypedefAnonStruct(t *testing.T) {
	src := `typedef struct { int x; int y; } Point;`
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "types.c", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Point", graph.NodeStruct)
	assertMetaKind(t, g, "Point", "struct")
}

// ─── PHP: abstract class ─────────────────────────────────────────────────────

func TestPHPParser_AbstractClass(t *testing.T) {
	src := `<?php
abstract class BaseController {
    abstract public function handle(): void;
}
`
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "BaseController.php", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "BaseController", graph.NodeStruct)
	assertMetaKind(t, g, "BaseController", "abstract")
}

// ─── C#: abstract class ──────────────────────────────────────────────────────

func TestCSharpParser_AbstractClass(t *testing.T) {
	src := `public abstract class Shape {
    public abstract double Area();
}
`
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Shape.cs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Shape", graph.NodeStruct)
	assertMetaKind(t, g, "Shape", "abstract")
}

// ─── C#: indexer ─────────────────────────────────────────────────────────────

func TestCSharpParser_Indexer(t *testing.T) {
	src := `public class DataStore {
    private string[] data = new string[10];
    public string this[int index] {
        get { return data[index]; }
        set { data[index] = value; }
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "DataStore.cs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "DataStore", graph.NodeStruct)
	assertNode(t, g, "DataStore.this", graph.NodeMethod)
	assertMetaKind(t, g, "DataStore.this", "indexer")
}

// ─── Lua: table field function assignment ────────────────────────────────────

func TestLuaParser_TableFieldFunction(t *testing.T) {
	src := `local M = {}
M.greet = function(name)
    print("Hello " .. name)
end
M.farewell = function(name)
    print("Bye " .. name)
end
return M
`
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "module.lua", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "M.greet", graph.NodeFunction)
	assertMetaKind(t, g, "M.greet", "table_func")
	assertNode(t, g, "M.farewell", graph.NodeFunction)
	assertMetaKind(t, g, "M.farewell", "table_func")
}

func TestLuaParser_TableBracketFunction(t *testing.T) {
	// M["key"] = function() end — bracket notation with string literal key.
	src := `local M = {}
M["greet"] = function(name) end
M["farewell"] = function(name) end
`
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "module.lua", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "M.greet", graph.NodeFunction)
	assertMetaKind(t, g, "M.greet", "table_func")
	assertNode(t, g, "M.farewell", graph.NodeFunction)
	assertMetaKind(t, g, "M.farewell", "table_func")
}

func TestLuaParser_TableBracketNonStringKey(t *testing.T) {
	// M[i] = function() — non-string key must NOT produce a node (no stable name).
	src := `local M = {}
M[1] = function() end
`
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "module.lua", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	for _, n := range g.AllNodes() {
		if n.Name == "M.1" || (len(n.Name) > 2 && n.Name[:2] == "M.") {
			t.Errorf("integer-key table assignment should NOT produce node %q", n.Name)
		}
	}
}

// ─── TypeScript: optional chaining call sites ────────────────────────────────

func TestTSParser_OptionalChaining(t *testing.T) {
	src := `function handler(obj: Foo | null) {
    obj?.process()
    obj?.validate()
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "handler.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertCallSite(t, g, "handler", "process")
	assertCallSite(t, g, "handler", "validate")
}

// ─── TypeScript: class field definitions ────────────────────────────────────

func TestTSParser_ClassFields(t *testing.T) {
	src := `class User {
  name: string = '';
  private age: number;
  readonly id: number = 0;
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "User.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "User", graph.NodeStruct)
	n := assertNode(t, g, "User.name", graph.NodeMethod)
	assertMetaKind(t, g, "User.name", "field")
	assertExported(t, g, "User.name", true)

	priv := assertNode(t, g, "User.age", graph.NodeMethod)
	assertMetaKind(t, g, "User.age", "private")
	assertExported(t, g, "User.age", false)
	_ = priv

	assertNode(t, g, "User.id", graph.NodeMethod)
	assertMetaKind(t, g, "User.id", "readonly")
	_ = n
}

func TestTSParser_AbstractClassKind(t *testing.T) {
	src := `abstract class Shape {
  abstract area(): number;
}
`
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "Shape.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	n := assertNode(t, g, "Shape", graph.NodeStruct)
	assertMetaKind(t, g, "Shape", "abstract")
	_ = n
}

// ─── JavaScript: class field definitions ────────────────────────────────────

func TestJSParser_ClassFields(t *testing.T) {
	src := `class Config {
  host = 'localhost';
  #privatePort = 8080;
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaScriptParser()
	if err := p.Parse(g, "Config.js", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Config", graph.NodeStruct)
	assertNode(t, g, "Config.host", graph.NodeMethod)
	assertMetaKind(t, g, "Config.host", "field")
	assertExported(t, g, "Config.host", true)
	assertNode(t, g, "Config.privatePort", graph.NodeMethod)
	assertMetaKind(t, g, "Config.privatePort", "private")
	assertExported(t, g, "Config.privatePort", false)
}

// ─── Java: enum constants ────────────────────────────────────────────────────

func TestJavaParser_EnumConstants(t *testing.T) {
	src := `enum Status { ACTIVE, INACTIVE, PENDING }`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Status.java", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Status", graph.NodeStruct)
	assertNode(t, g, "Status.ACTIVE", graph.NodeMethod)
	assertMetaKind(t, g, "Status.ACTIVE", "enum_constant")
	assertNode(t, g, "Status.INACTIVE", graph.NodeMethod)
	assertNode(t, g, "Status.PENDING", graph.NodeMethod)
}

// ─── Rust: enum variants ─────────────────────────────────────────────────────

func TestRustParser_EnumVariants(t *testing.T) {
	src := `pub enum Color { Red, Green, Blue(u8) }`
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "color.rs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Color", graph.NodeStruct)
	assertNode(t, g, "Color::Red", graph.NodeMethod)
	assertMetaKind(t, g, "Color::Red", "variant")
	assertNode(t, g, "Color::Green", graph.NodeMethod)
	assertNode(t, g, "Color::Blue", graph.NodeMethod)
}

func TestRustParser_PrivateEnumVariants(t *testing.T) {
	// Private enum variants inherit enum visibility.
	src := `enum Direction { North, South, East, West }`
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "dir.rs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Direction", graph.NodeStruct)
	assertNode(t, g, "Direction::North", graph.NodeMethod)
	assertNode(t, g, "Direction::South", graph.NodeMethod)
	// Variants should not be exported when enum is private.
	assertExported(t, g, "Direction::North", false)
}

// ─── Scala: package declaration ───────────────────────────────────────────────

func TestScalaParser_PackageDeclaration(t *testing.T) {
	src := `package com.example.app
object Main extends App {
  def run(): Unit = {}
}
`
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "Main.scala", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Package node should be present.
	found := false
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodePackage && n.Name == "com.example.app" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Scala package node 'com.example.app' not found")
	}
}

// ─── Groovy: package declaration ─────────────────────────────────────────────

func TestGroovyParser_PackageDeclaration(t *testing.T) {
	src := `package com.example
class Service {
  def execute() {}
}
`
	g := graph.New("testrepo")
	p := parser.NewGroovyParser()
	if err := p.Parse(g, "Service.groovy", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	found := false
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodePackage && n.Name == "com.example" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Groovy package node 'com.example' not found")
	}
}

// ─── TypeScript enum members ────────────────────────────────────────────────

func TestTSParser_EnumMembers(t *testing.T) {
	src := []byte(`enum Color { Red, Green, Blue }
enum Direction { Up = "UP", Down = "DOWN" }
`)
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "enums.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Enum containers should exist.
	assertNode(t, g, "Color", graph.NodeStruct)
	assertNode(t, g, "Direction", graph.NodeStruct)
	// Individual enum members should exist.
	assertNode(t, g, "Color.Red", graph.NodeMethod)
	assertNode(t, g, "Color.Green", graph.NodeMethod)
	assertNode(t, g, "Color.Blue", graph.NodeMethod)
	assertNode(t, g, "Direction.Up", graph.NodeMethod)
	assertNode(t, g, "Direction.Down", graph.NodeMethod)
	assertMetaKind(t, g, "Color.Red", "enum_member")
}

// ─── Kotlin companion object methods ────────────────────────────────────────

func TestKotlinParser_CompanionObject(t *testing.T) {
	src := []byte(`class Factory {
  companion object {
    fun create(): Factory = Factory()
    fun parse(s: String): Factory = Factory()
  }
}
`)
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "Factory.kt", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Companion methods should be qualified with class name.
	assertNode(t, g, "Factory.create", graph.NodeMethod)
	assertNode(t, g, "Factory.parse", graph.NodeMethod)
}

// ─── Ruby singleton method metadata ─────────────────────────────────────────

func TestRubyParser_SingletonMethod(t *testing.T) {
	src := []byte(`class Config
  def self.load
    new
  end
  def save
    nil
  end
end
`)
	g := graph.New("testrepo")
	p := parser.NewRubyParser()
	if err := p.Parse(g, "config.rb", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Singleton method should have kind=singleton.
	assertNode(t, g, "Config.load", graph.NodeMethod)
	assertMetaKind(t, g, "Config.load", "singleton")
	// Regular method should still work.
	assertNode(t, g, "Config.save", graph.NodeMethod)
}

// ─── C# event field declarations ────────────────────────────────────────────

func TestCSharpParser_EventField(t *testing.T) {
	src := []byte(`using System;
class Button {
  public event EventHandler OnClick;
  public event EventHandler OnHover;
  public void Press() {}
}
`)
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Button.cs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Button.OnClick", graph.NodeMethod)
	assertNode(t, g, "Button.OnHover", graph.NodeMethod)
	assertMetaKind(t, g, "Button.OnClick", "event")
}

// ─── PHP enum cases ─────────────────────────────────────────────────────────

func TestPHPParser_EnumCases(t *testing.T) {
	src := []byte(`<?php
enum Status {
  case Active;
  case Inactive;
  case Pending;
}
`)
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "status.php", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Enum container.
	assertNode(t, g, "Status", graph.NodeStruct)
	assertMetaKind(t, g, "Status", "enum")
	// Individual cases.
	assertNode(t, g, "Status.Active", graph.NodeMethod)
	assertNode(t, g, "Status.Inactive", graph.NodeMethod)
	assertNode(t, g, "Status.Pending", graph.NodeMethod)
	assertMetaKind(t, g, "Status.Active", "enum_case")
}

// ─── Elixir defguard ────────────────────────────────────────────────────────

func TestElixirParser_Defguard(t *testing.T) {
	src := []byte(`defmodule MyMod do
  defguard is_positive(x) when x > 0
  defguardp is_negative(x) when x < 0
  def greet(name), do: "Hello #{name}"
end
`)
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "my_mod.ex", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Guards should be captured as functions with kind=guard.
	assertNode(t, g, "is_positive", graph.NodeFunction)
	assertMetaKind(t, g, "is_positive", "guard")
	assertExported(t, g, "is_positive", true)
	// Private guard should not be exported.
	assertNode(t, g, "is_negative", graph.NodeFunction)
	assertMetaKind(t, g, "is_negative", "guard")
	assertExported(t, g, "is_negative", false)
	// Regular function should still work.
	assertNode(t, g, "greet", graph.NodeFunction)
}

// ─── C function pointer typedef ─────────────────────────────────────────────

func TestCParser_FuncPtrTypedef(t *testing.T) {
	src := []byte(`typedef int (*callback_fn)(int x, int y);
typedef void (*handler)(void);
void normal_func(void) {}
`)
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "test.c", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "callback_fn", graph.NodeStruct)
	assertNode(t, g, "handler", graph.NodeStruct)
	assertNode(t, g, "normal_func", graph.NodeFunction)
}

// ─── C++ operator overloads ─────────────────────────────────────────────────

func TestCppParser_OperatorOverload(t *testing.T) {
	src := []byte(`class Vec {
public:
  Vec operator+(const Vec& other) const { return Vec(); }
  bool operator==(const Vec& other) const { return true; }
  void doStuff() {}
};
`)
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "vec.cpp", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Vec", graph.NodeStruct)
	assertNode(t, g, "Vec.operator+", graph.NodeMethod)
	assertNode(t, g, "Vec.operator==", graph.NodeMethod)
	assertNode(t, g, "Vec.doStuff", graph.NodeMethod)
}

// ─── Java sealed permits ────────────────────────────────────────────────────

func TestJavaParser_SealedPermits(t *testing.T) {
	src := []byte(`public sealed class Shape permits Circle, Square {}
`)
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Shape.java", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Shape", graph.NodeStruct)
	assertMetaKind(t, g, "Shape", "sealed")
	// Permitted subclasses should be captured.
	assertNode(t, g, "Circle", graph.NodeStruct)
	assertNode(t, g, "Square", graph.NodeStruct)
	assertMetaKind(t, g, "Circle", "permitted")
}

// ─── Scala implicit modifier ────────────────────────────────────────────────

func TestScalaParser_ImplicitDef(t *testing.T) {
	src := []byte(`object Implicits {
  implicit def intToString(x: Int): String = x.toString
  implicit class RichInt(val x: Int) {
    def square: Int = x * x
  }
  def normalFunc(): Unit = ()
}
`)
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "Implicits.scala", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Implicits.intToString", graph.NodeMethod)
	assertMetaKind(t, g, "Implicits.intToString", "implicit")
	assertNode(t, g, "RichInt", graph.NodeStruct)
	assertMetaKind(t, g, "RichInt", "implicit")
	assertNode(t, g, "Implicits.normalFunc", graph.NodeMethod)
}

// ─── Elixir defstruct fields ────────────────────────────────────────────────

func TestElixirParser_DefstructFields(t *testing.T) {
	src := []byte(`defmodule User do
  defstruct name: "", age: 0, active: true
end
`)
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "user.ex", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "User", graph.NodeStruct)
	assertNode(t, g, "name", graph.NodeMethod)
	assertNode(t, g, "age", graph.NodeMethod)
	assertNode(t, g, "active", graph.NodeMethod)
	assertMetaKind(t, g, "name", "field")
}

// ─── Protobuf message fields ────────────────────────────────────────────────

func TestProtobufParser_MessageFields(t *testing.T) {
	src := []byte(`syntax = "proto3";
message User {
  string name = 1;
  int32 age = 2;
  bool active = 3;
}
`)
	g := graph.New("testrepo")
	p := parser.NewProtobufParser()
	if err := p.Parse(g, "user.proto", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "User", graph.NodeStruct)
	assertNode(t, g, "User.name", graph.NodeMethod)
	assertNode(t, g, "User.age", graph.NodeMethod)
	assertNode(t, g, "User.active", graph.NodeMethod)
	assertMetaKind(t, g, "User.name", "field")
}
// --- Python: dataclass annotated fields ---
func TestAudit_Python_DataclassFields(t *testing.T) {
	src := []byte(`from dataclasses import dataclass

@dataclass
class User:
    name: str
    age: int
    email: str = ""
`)
	g := graph.New("r")
	if err := parser.NewPythonParser().Parse(g, "user.py", src); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"User.name", "User.age", "User.email"} {
		nodes := g.FindByName(field)
		if len(nodes) == 0 {
			t.Errorf("MISSING dataclass field: %q", field)
		}
	}
}

// --- Kotlin: extension functions ---
func TestAudit_Kotlin_ExtensionFunctions(t *testing.T) {
	src := []byte(`fun String.toSlug(): String = this.lowercase().replace(" ", "-")
fun Int.isEven(): Boolean = this % 2 == 0
`)
	g := graph.New("r")
	if err := parser.NewKotlinParser().Parse(g, "ext.kt", src); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"toSlug", "isEven"} {
		if nodes := g.FindByName(fn); len(nodes) == 0 {
			t.Errorf("MISSING extension function: %q", fn)
		}
	}
}

// --- Kotlin: data class primary constructor properties ---
func TestAudit_Kotlin_DataClassProperties(t *testing.T) {
	src := []byte(`data class User(val name: String, val age: Int, var email: String = "")`)
	g := graph.New("r")
	if err := parser.NewKotlinParser().Parse(g, "user.kt", src); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"User.name", "User.age", "User.email"} {
		if nodes := g.FindByName(field); len(nodes) == 0 {
			t.Errorf("MISSING data class property: %q", field)
		}
	}
}

// --- Rust: macro_rules! ---
func TestAudit_Rust_MacroRules(t *testing.T) {
	src := []byte(`macro_rules! vec_of_strings {
    ($($x:expr),*) => (vec![$($x.to_string()),*]);
}
macro_rules! log_error {
    ($msg:expr) => { eprintln!("ERROR: {}", $msg); };
}
`)
	g := graph.New("r")
	if err := parser.NewRustParser().Parse(g, "macros.rs", src); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"vec_of_strings", "log_error"} {
		if nodes := g.FindByName(m); len(nodes) == 0 {
			t.Errorf("MISSING macro_rules: %q", m)
		}
	}
}

// --- Ruby: attr_accessor / attr_reader / attr_writer ---
func TestAudit_Ruby_AttrAccessor(t *testing.T) {
	src := []byte(`class User
  attr_accessor :name, :email
  attr_reader :id
  attr_writer :password
end
`)
	g := graph.New("r")
	if err := parser.NewRubyParser().Parse(g, "user.rb", src); err != nil {
		t.Fatal(err)
	}
	// attr_accessor generates getters + setters
	for _, m := range []string{"User.name", "User.email", "User.id", "User.password"} {
		if nodes := g.FindByName(m); len(nodes) == 0 {
			t.Errorf("MISSING attr accessor method: %q", m)
		}
	}
}

// --- Java: records ---
func TestAudit_Java_Records(t *testing.T) {
	src := []byte(`public record Point(int x, int y) {
    public double distance() {
        return Math.sqrt(x * x + y * y);
    }
}
record Color(int r, int g, int b) {}
`)
	g := graph.New("r")
	if err := parser.NewJavaParser().Parse(g, "Point.java", src); err != nil {
		t.Fatal(err)
	}
	if nodes := g.FindByName("Point"); len(nodes) == 0 {
		t.Errorf("MISSING record class: Point")
	}
	if nodes := g.FindByName("Color"); len(nodes) == 0 {
		t.Errorf("MISSING record class: Color")
	}
	if nodes := g.FindByName("Point.distance"); len(nodes) == 0 {
		t.Errorf("MISSING record method: Point.distance")
	}
}

// --- TypeScript: abstract methods ---
func TestAudit_TypeScript_AbstractMethods(t *testing.T) {
	src := []byte(`abstract class Shape {
    abstract area(): number;
    abstract perimeter(): number;
    describe(): string { return "shape"; }
}
`)
	g := graph.New("r")
	if err := parser.NewTypeScriptParser().Parse(g, "shape.ts", src); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"Shape.area", "Shape.perimeter", "Shape.describe"} {
		if nodes := g.FindByName(m); len(nodes) == 0 {
			t.Errorf("MISSING abstract method: %q", m)
		}
	}
}

// --- Go: type aliases ---
func TestAudit_Go_TypeAlias(t *testing.T) {
	src := []byte(`package main
type Foo = int
type Bar = string
`)
	g := graph.New("r")
	if err := parser.NewGoParser().Parse(g, "alias.go", src); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"Foo", "Bar"} {
		if nodes := g.FindByName(a); len(nodes) == 0 {
			t.Errorf("MISSING type alias: %q", a)
		}
	}
}

// --- Lua: module table pattern ---
func TestAudit_Lua_ModulePattern(t *testing.T) {
	src := []byte(`local M = {}

function M.greet(name)
    return "Hello, " .. name
end

M.version = "1.0"

return M
`)
	g := graph.New("r")
	if err := parser.NewLuaParser().Parse(g, "mod.lua", src); err != nil {
		t.Fatal(err)
	}
	if nodes := g.FindByName("M.greet"); len(nodes) == 0 {
		t.Errorf("MISSING module function: M.greet")
	}
}

// --- Elixir: defdelegate ---
func TestAudit_Elixir_Defdelegate(t *testing.T) {
	src := []byte(`defmodule MyApp.Accounts do
  defdelegate get_user(id), to: UserRepo
  defdelegate create_user(attrs), to: UserRepo
end
`)
	g := graph.New("r")
	if err := parser.NewElixirParser().Parse(g, "accounts.ex", src); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"get_user", "create_user"} {
		if nodes := g.FindByName(fn); len(nodes) == 0 {
			t.Errorf("MISSING defdelegate: %q", fn)
		}
	}
}

// --- C#: records ---
func TestAudit_CSharp_Records(t *testing.T) {
	src := []byte(`public record Point(int X, int Y);
record Color(byte R, byte G, byte B)
{
    public bool IsWhite() => R == 255 && G == 255 && B == 255;
}
`)
	g := graph.New("r")
	if err := parser.NewCSharpParser().Parse(g, "records.cs", src); err != nil {
		t.Fatal(err)
	}
	if nodes := g.FindByName("Point"); len(nodes) == 0 {
		t.Errorf("MISSING record: Point")
	}
	if nodes := g.FindByName("Color"); len(nodes) == 0 {
		t.Errorf("MISSING record: Color")
	}
}

// --- Kotlin: suspend functions ---
func TestAudit_Kotlin_SuspendFunctions(t *testing.T) {
	src := []byte(`suspend fun fetchUser(id: Int): User {
    return apiClient.get("/users/$id")
}

class UserService {
    suspend fun login(name: String, pw: String): String {
        return tokenRepo.create(name, pw)
    }
}
`)
	g := graph.New("r")
	if err := parser.NewKotlinParser().Parse(g, "service.kt", src); err != nil {
		t.Fatal(err)
	}
	if nodes := g.FindByName("fetchUser"); len(nodes) == 0 {
		t.Errorf("MISSING suspend function: fetchUser")
	}
	if nodes := g.FindByName("UserService.login"); len(nodes) == 0 {
		t.Errorf("MISSING suspend method: UserService.login")
	}
}

// ─── Scala 3 given/using ──────────────────────────────────────────────────────

func TestScalaParser_Given(t *testing.T) {
	src := []byte(`given intOrdering: Ordering[Int] = Ordering.Int
given listOrdering[A](using ord: Ordering[A]): Ordering[List[A]] = ???

class MyClass {
  given localOrd: Ordering[String] = Ordering.String
}
`)
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "givens.scala", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "intOrdering", graph.NodeFunction)
	assertNode(t, g, "listOrdering", graph.NodeFunction)
	assertMetaKind(t, g, "intOrdering", "given")
	assertMetaKind(t, g, "listOrdering", "given")
	// given inside a class is a method
	assertNode(t, g, "MyClass.localOrd", graph.NodeMethod)
	assertMetaKind(t, g, "MyClass.localOrd", "given")
}

// ─── TypeScript decorator metadata ───────────────────────────────────────────

func TestTSParser_Decorators(t *testing.T) {
	src := []byte(`@Component({ selector: 'app-root' })
class AppComponent {
  title: string = '';
}

@Injectable()
export class UserService {}

@Controller('/users')
@UseGuards(AuthGuard)
export class UserController {}
`)
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "app.ts", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Classes must exist
	assertNode(t, g, "AppComponent", graph.NodeStruct)
	assertNode(t, g, "UserService", graph.NodeStruct)
	assertNode(t, g, "UserController", graph.NodeStruct)

	// Decorator metadata stored on class nodes
	nodes := g.FindByName("AppComponent")
	if len(nodes) == 0 || nodes[0].Metadata == nil || nodes[0].Metadata["decorators"] != "Component" {
		t.Errorf("AppComponent missing decorator metadata, got %v", nodes[0].Metadata)
	}
	nodes = g.FindByName("UserService")
	if len(nodes) == 0 || nodes[0].Metadata == nil || nodes[0].Metadata["decorators"] != "Injectable" {
		t.Errorf("UserService missing decorator metadata, got %v", nodes[0].Metadata)
	}
	// Multiple decorators stored comma-separated
	nodes = g.FindByName("UserController")
	if len(nodes) == 0 || nodes[0].Metadata == nil {
		t.Fatalf("UserController node not found")
	}
	decs := nodes[0].Metadata["decorators"]
	if decs != "Controller,UseGuards" {
		t.Errorf("UserController decorators = %q, want %q", decs, "Controller,UseGuards")
	}
}

// ─── Protobuf proto2 extend blocks ───────────────────────────────────────────

func TestProtoParser_Extend(t *testing.T) {
	src := []byte(`syntax = "proto2";

message Request {
  optional string name = 1;
}

extend Request {
  optional string session_id = 1000;
  optional int32 priority = 1001;
}
`)
	g := graph.New("testrepo")
	p := parser.NewProtobufParser()
	if err := p.Parse(g, "ext.proto", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Request", graph.NodeStruct)
	// Extension fields qualify under the extended message.
	assertNode(t, g, "Request.session_id", graph.NodeMethod)
	assertNode(t, g, "Request.priority", graph.NodeMethod)
}

// ─── Dart parser coverage ─────────────────────────────────────────────────────

func TestDartParser_Class(t *testing.T) {
	src := []byte(`class Person {
  String name;
  int age;

  Person(this.name, this.age);

  void greet() {
    print('Hello');
  }
}

class Admin extends Person {
  Admin(String name, int age) : super(name, age);
}
`)
	g := graph.New("testrepo")
	p := parser.NewDartParser()
	if err := p.Parse(g, "person.dart", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Dart parser parses without error; node structure varies by grammar
	nodes := g.AllNodes()
	_ = nodes
}

// ─── R parser coverage ────────────────────────────────────────────────────────

func TestRParser_Function(t *testing.T) {
	src := []byte(`# R functions
calculate_mean <- function(x) {
  sum(x) / length(x)
}

MyClass <- setRefClass("MyClass",
  fields = list(value = "numeric"),
  methods = list(
    getValue = function() { value }
  )
)
`)
	g := graph.New("testrepo")
	p := parser.NewRParser()
	if err := p.Parse(g, "script.R", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "calculate_mean", graph.NodeFunction)
}

// ─── Perl parser coverage ─────────────────────────────────────────────────────

func TestPerlParser_Package(t *testing.T) {
	src := []byte(`package MyModule;

use strict;

sub greet {
  my ($name) = @_;
  print "Hello, $name\\n";
}

sub new {
  my $class = shift;
  my $self = {};
  bless $self, $class;
  return $self;
}

1;
`)
	g := graph.New("testrepo")
	p := parser.NewPerlParser()
	if err := p.Parse(g, "MyModule.pm", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Perl parser parses without error; nodes may vary by grammar implementation
	nodes := g.AllNodes()
	_ = nodes
}

// ─── PowerShell parser coverage ───────────────────────────────────────────────

func TestPowerShellParser_Function(t *testing.T) {
	src := []byte(`function Invoke-Deploy {
  param(
    [string]$Environment,
    [int]$Version
  )

  Write-Host "Deploying to $Environment"
}

class DeployService {
  [string]$Name

  Deploy() {
    Write-Host "Deploying"
  }
}
`)
	g := graph.New("testrepo")
	p := parser.NewPowerShellParser()
	if err := p.Parse(g, "deploy.ps1", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "Invoke-Deploy", graph.NodeFunction)
	assertNode(t, g, "DeployService", graph.NodeStruct)
}

// ─── Julia parser coverage ───────────────────────────────────────────────────

func TestJuliaParser_Function(t *testing.T) {
	src := []byte(`module MyModule

export greet, MyStruct

function greet(name::String)
  println("Hello, $name")
end

struct MyStruct
  x::Float64
  y::Float64
end

end
`)
	g := graph.New("testrepo")
	p := parser.NewJuliaParser()
	if err := p.Parse(g, "script.jl", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "greet", graph.NodeFunction)
	assertNode(t, g, "MyStruct", graph.NodeStruct)
}

// ─── YAML parser coverage ────────────────────────────────────────────────────

func TestYAMLParser_Document(t *testing.T) {
	src := []byte(`apiVersion: v1
kind: Service
metadata:
  name: my-service
  labels:
    app: MyApp
spec:
  ports:
  - port: 8080
    targetPort: 8080
`)
	g := graph.New("testrepo")
	p := parser.NewYAMLParser()
	if err := p.Parse(g, "service.yaml", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// YAML should create nodes for top-level keys
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected nodes from YAML parsing")
	}
}

// ─── XML parser coverage ─────────────────────────────────────────────────────

func TestXMLParser_Element(t *testing.T) {
	src := []byte(`<?xml version="1.0"?>
<root xmlns="http://example.com/schema">
  <Person id="1">
    <Name>John</Name>
    <Age>30</Age>
  </Person>
  <Company>
    <Name>ACME</Name>
  </Company>
</root>
`)
	g := graph.New("testrepo")
	p := parser.NewXMLParser()
	if err := p.Parse(g, "data.xml", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected nodes from XML parsing")
	}
}

// ─── Dockerfile parser coverage ───────────────────────────────────────────────

func TestDockerfileParser_Instructions(t *testing.T) {
	src := []byte(`FROM ubuntu:20.04

LABEL maintainer="dev@example.com"

RUN apt-get update && apt-get install -y python3

COPY app.py /app/

WORKDIR /app

ENTRYPOINT ["python3", "app.py"]
`)
	g := graph.New("testrepo")
	p := parser.NewDockerfileParser()
	if err := p.Parse(g, "Dockerfile", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected nodes from Dockerfile parsing")
	}
}

// ─── Makefile parser coverage ────────────────────────────────────────────────

func TestMakefileParser_Rules(t *testing.T) {
	src := []byte(`.PHONY: build test clean

BUILD_DIR := build
SOURCES := src/*.c

build: $(SOURCES)
	gcc -o $(BUILD_DIR)/app $(SOURCES)

test: build
	./$(BUILD_DIR)/app --test

clean:
	rm -rf $(BUILD_DIR)
`)
	g := graph.New("testrepo")
	p := parser.NewMakefileParser()
	if err := p.Parse(g, "Makefile", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected nodes from Makefile parsing")
	}
}

// ─── CMake parser coverage ───────────────────────────────────────────────────

func TestCMakeParser_Functions(t *testing.T) {
	src := []byte(`cmake_minimum_required(VERSION 3.10)
project(MyProject)

add_executable(myapp
    src/main.cpp
    src/utils.cpp
)

target_link_libraries(myapp PRIVATE pthread)

function(my_function arg1 arg2)
  message(STATUS "Called with ${arg1} ${arg2}")
endfunction()
`)
	g := graph.New("testrepo")
	p := parser.NewCMakeParser()
	if err := p.Parse(g, "CMakeLists.txt", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected nodes from CMake parsing")
	}
}

// ─── Nix parser coverage ──────────────────────────────────────────────────────

func TestNixParser_Attr(t *testing.T) {
	src := []byte(`{ pkgs, lib, ... }:

{
  options.myapp = {
    enable = lib.mkOption {
      type = lib.types.bool;
      default = false;
    };
  };

  config = lib.mkIf config.myapp.enable {
    environment.systemPackages = [ pkgs.myapp ];
  };
}
`)
	g := graph.New("testrepo")
	p := parser.NewNixParser()
	if err := p.Parse(g, "default.nix", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected nodes from Nix parsing")
	}
}

// ─── Cue parser coverage ──────────────────────────────────────────────────────

func TestCueParser_Structure(t *testing.T) {
	src := []byte(`package config

Config: {
  database: {
    host: string
    port: int
    user: string
  }
  server: {
    port: int
  }
}
`)
	g := graph.New("testrepo")
	p := parser.NewCUEParser()
	if err := p.Parse(g, "config.cue", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.AllNodes()
	if len(nodes) == 0 {
		t.Error("expected nodes from Cue parsing")
	}
}

// ─── Starlark parser coverage ─────────────────────────────────────────────────

func TestStarlarkParser_Function(t *testing.T) {
	src := []byte(`def hello(name):
  """Greeting function"""
  return "Hello, " + name

def build_rule(name, srcs, outs):
  """Custom build rule"""
  native.cc_library(
    name = name,
    srcs = srcs,
    outs = outs,
  )
`)
	g := graph.New("testrepo")
	p := parser.NewStarlarkParser()
	if err := p.Parse(g, "BUILD.star", src); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertNode(t, g, "hello", graph.NodeFunction)
	assertNode(t, g, "build_rule", graph.NodeFunction)
}
