package parser_test

import (
	"testing"

	"github.com/synapses/synapses/internal/graph"
	"github.com/synapses/synapses/internal/parser"
)

// ─── shared helpers ───────────────────────────────────────────────────────────

func assertFileNode(t *testing.T, g *graph.Graph, wantName string) {
	t.Helper()
	nodes := g.FindByName(wantName)
	if len(nodes) == 0 {
		t.Fatalf("file node %q not found", wantName)
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

func assertNode(t *testing.T, g *graph.Graph, name string, wantType graph.NodeType) *graph.Node {
	t.Helper()
	nodes := g.FindByName(name)
	if len(nodes) == 0 {
		t.Fatalf("node %q not found", name)
	}
	if nodes[0].Type != wantType {
		t.Errorf("node %q: type = %q, want %q", name, nodes[0].Type, wantType)
	}
	return nodes[0]
}

func assertDefinesEdge(t *testing.T, g *graph.Graph, fileNodeID graph.NodeID, entityName string) {
	t.Helper()
	for _, e := range g.OutEdges(fileNodeID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil && n.Name == entityName {
				return
			}
		}
	}
	t.Errorf("no DEFINES edge from file to %q", entityName)
}

func assertNoCrash(t *testing.T, p parser.LanguageParser, ext, src string) {
	t.Helper()
	g := graph.New("testrepo")
	if err := p.Parse(g, "empty"+ext, []byte(src)); err != nil {
		t.Fatalf("Parse() on minimal %s input returned error: %v", ext, err)
	}
	if g.NodeCount() == 0 {
		t.Errorf("Parse() produced zero nodes; expected at least a file node")
	}
}

func hasExtension(exts []string, want string) bool {
	for _, e := range exts {
		if e == want {
			return true
		}
	}
	return false
}

// ─── Java ─────────────────────────────────────────────────────────────────────

const javaSource = `package com.example.auth;

import com.example.db.UserRepo;

public interface Authenticator {
    boolean login(String user);
}

public class AuthService implements Authenticator {
    private UserRepo repo;

    public boolean login(String user) {
        return true;
    }
}

public enum Status {
    ACTIVE, INACTIVE
}
`

func parseJava(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "auth/AuthService.java", []byte(src)); err != nil {
		t.Fatalf("JavaParser.Parse() error: %v", err)
	}
	return g
}

func TestJavaParser_Extensions(t *testing.T) {
	exts := parser.NewJavaParser().Extensions()
	if !hasExtension(exts, ".java") {
		t.Errorf("Extensions() = %v, missing .java", exts)
	}
}

func TestJavaParser_FileNode(t *testing.T) {
	assertFileNode(t, parseJava(t, javaSource), "AuthService.java")
}

func TestJavaParser_ExtractsInterface(t *testing.T) {
	n := assertNode(t, parseJava(t, javaSource), "Authenticator", graph.NodeInterface)
	if !n.Exported {
		t.Error("Authenticator should be exported (starts with uppercase)")
	}
}

func TestJavaParser_ExtractsClass(t *testing.T) {
	n := assertNode(t, parseJava(t, javaSource), "AuthService", graph.NodeStruct)
	if !n.Exported {
		t.Error("AuthService should be exported")
	}
}

func TestJavaParser_ExtractsEnum(t *testing.T) {
	assertNode(t, parseJava(t, javaSource), "Status", graph.NodeStruct)
}

func TestJavaParser_ExtractsMethod(t *testing.T) {
	assertNode(t, parseJava(t, javaSource), "login", graph.NodeMethod)
}

func TestJavaParser_DefinesEdges(t *testing.T) {
	g := parseJava(t, javaSource)
	fileID := g.FindByName("AuthService.java")[0].ID
	for _, name := range []string{"Authenticator", "AuthService", "Status", "login"} {
		assertDefinesEdge(t, g, fileID, name)
	}
}

func TestJavaParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewJavaParser(), ".java", "")
}

// ─── Kotlin ──────────────────────────────────────────────────────────────────

const kotlinSource = `class AuthService {
    fun login(user: String): Boolean = true
    fun logout(user: String) {}
}

object AuthConfig {
    const val timeout = 30
}

fun createService(): AuthService = AuthService()
`

func parseKotlin(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "auth/Auth.kt", []byte(src)); err != nil {
		t.Fatalf("KotlinParser.Parse() error: %v", err)
	}
	return g
}

func TestKotlinParser_Extensions(t *testing.T) {
	exts := parser.NewKotlinParser().Extensions()
	if !hasExtension(exts, ".kt") || !hasExtension(exts, ".kts") {
		t.Errorf("Extensions() = %v, missing .kt or .kts", exts)
	}
}

func TestKotlinParser_FileNode(t *testing.T) {
	assertFileNode(t, parseKotlin(t, kotlinSource), "Auth.kt")
}

func TestKotlinParser_ExtractsClass(t *testing.T) {
	assertNode(t, parseKotlin(t, kotlinSource), "AuthService", graph.NodeStruct)
}

func TestKotlinParser_ExtractsObject(t *testing.T) {
	assertNode(t, parseKotlin(t, kotlinSource), "AuthConfig", graph.NodeStruct)
}

func TestKotlinParser_ExtractsFunction(t *testing.T) {
	assertNode(t, parseKotlin(t, kotlinSource), "createService", graph.NodeFunction)
}

func TestKotlinParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewKotlinParser(), ".kt", "")
}

// ─── Scala ───────────────────────────────────────────────────────────────────

const scalaSource = `import scala.concurrent.Future

trait Authenticator {
  def login(user: String): Future[Boolean]
}

class AuthService extends Authenticator {
  def login(user: String): Future[Boolean] = Future.successful(true)
  def logout(user: String): Unit = ()
}

object AuthFactory {
  def create(): AuthService = new AuthService()
}
`

func parseScala(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewScalaParser()
	if err := p.Parse(g, "auth/Auth.scala", []byte(src)); err != nil {
		t.Fatalf("ScalaParser.Parse() error: %v", err)
	}
	return g
}

func TestScalaParser_Extensions(t *testing.T) {
	exts := parser.NewScalaParser().Extensions()
	if !hasExtension(exts, ".scala") {
		t.Errorf("Extensions() = %v, missing .scala", exts)
	}
}

func TestScalaParser_FileNode(t *testing.T) {
	assertFileNode(t, parseScala(t, scalaSource), "Auth.scala")
}

func TestScalaParser_ExtractsTrait(t *testing.T) {
	assertNode(t, parseScala(t, scalaSource), "Authenticator", graph.NodeInterface)
}

func TestScalaParser_ExtractsClass(t *testing.T) {
	assertNode(t, parseScala(t, scalaSource), "AuthService", graph.NodeStruct)
}

func TestScalaParser_ExtractsObject(t *testing.T) {
	assertNode(t, parseScala(t, scalaSource), "AuthFactory", graph.NodeStruct)
}

func TestScalaParser_ExtractsFunction(t *testing.T) {
	assertNode(t, parseScala(t, scalaSource), "login", graph.NodeFunction)
}

func TestScalaParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewScalaParser(), ".scala", "")
}

// ─── Groovy ──────────────────────────────────────────────────────────────────

const groovySource = `class AuthService {
    boolean login(String user) { return true }
    void logout(String user) {}
}
`

func parseGroovy(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewGroovyParser()
	if err := p.Parse(g, "auth/Auth.groovy", []byte(src)); err != nil {
		t.Fatalf("GroovyParser.Parse() error: %v", err)
	}
	return g
}

func TestGroovyParser_Extensions(t *testing.T) {
	exts := parser.NewGroovyParser().Extensions()
	if !hasExtension(exts, ".groovy") || !hasExtension(exts, ".gradle") {
		t.Errorf("Extensions() = %v, missing .groovy or .gradle", exts)
	}
}

func TestGroovyParser_FileNode(t *testing.T) {
	assertFileNode(t, parseGroovy(t, groovySource), "Auth.groovy")
}

func TestGroovyParser_ExtractsClass(t *testing.T) {
	assertNode(t, parseGroovy(t, groovySource), "AuthService", graph.NodeStruct)
}

func TestGroovyParser_ExtractsMethod(t *testing.T) {
	assertNode(t, parseGroovy(t, groovySource), "login", graph.NodeMethod)
}

func TestGroovyParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewGroovyParser(), ".groovy", "")
}

// ─── Rust ────────────────────────────────────────────────────────────────────

const rustSource = `use std::collections::HashMap;

pub struct AuthService {
    sessions: HashMap<String, String>,
}

pub enum AuthError {
    InvalidCredentials,
    Expired,
}

pub trait Authenticator {
    fn login(&self, user: &str) -> Result<(), AuthError>;
}

pub fn create_service() -> AuthService {
    AuthService { sessions: HashMap::new() }
}
`

func parseRust(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "src/auth.rs", []byte(src)); err != nil {
		t.Fatalf("RustParser.Parse() error: %v", err)
	}
	return g
}

func TestRustParser_Extensions(t *testing.T) {
	exts := parser.NewRustParser().Extensions()
	if !hasExtension(exts, ".rs") {
		t.Errorf("Extensions() = %v, missing .rs", exts)
	}
}

func TestRustParser_FileNode(t *testing.T) {
	assertFileNode(t, parseRust(t, rustSource), "auth.rs")
}

func TestRustParser_ExtractsStruct(t *testing.T) {
	n := assertNode(t, parseRust(t, rustSource), "AuthService", graph.NodeStruct)
	if !n.Exported {
		t.Error("AuthService should be exported (starts with uppercase)")
	}
}

func TestRustParser_ExtractsEnum(t *testing.T) {
	assertNode(t, parseRust(t, rustSource), "AuthError", graph.NodeStruct)
}

func TestRustParser_ExtractsTrait(t *testing.T) {
	assertNode(t, parseRust(t, rustSource), "Authenticator", graph.NodeInterface)
}

func TestRustParser_ExtractsFunction(t *testing.T) {
	assertNode(t, parseRust(t, rustSource), "create_service", graph.NodeFunction)
}

func TestRustParser_DefinesEdges(t *testing.T) {
	g := parseRust(t, rustSource)
	fileID := g.FindByName("auth.rs")[0].ID
	for _, name := range []string{"AuthService", "AuthError", "Authenticator", "create_service"} {
		assertDefinesEdge(t, g, fileID, name)
	}
}

func TestRustParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewRustParser(), ".rs", "")
}

// ─── C ───────────────────────────────────────────────────────────────────────

const cSource = `#include <stdio.h>
#include "auth.h"

struct AuthSession {
    int id;
    char* user;
};

typedef struct AuthSession AuthSession;

int login(const char* user) {
    return 1;
}

void logout(const char* user) {
}
`

func parseC(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "src/auth.c", []byte(src)); err != nil {
		t.Fatalf("CParser.Parse() error: %v", err)
	}
	return g
}

func TestCParser_Extensions(t *testing.T) {
	exts := parser.NewCParser().Extensions()
	if !hasExtension(exts, ".c") || !hasExtension(exts, ".h") {
		t.Errorf("Extensions() = %v, missing .c or .h", exts)
	}
}

func TestCParser_FileNode(t *testing.T) {
	assertFileNode(t, parseC(t, cSource), "auth.c")
}

func TestCParser_ExtractsStruct(t *testing.T) {
	assertNode(t, parseC(t, cSource), "AuthSession", graph.NodeStruct)
}

func TestCParser_ExtractsFunction(t *testing.T) {
	assertNode(t, parseC(t, cSource), "login", graph.NodeFunction)
}

func TestCParser_ImportEdges(t *testing.T) {
	g := parseC(t, cSource)
	fileID := g.FindByName("auth.c")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 1 {
		t.Errorf("expected ≥1 IMPORTS edges from #include directives, got %d", importCount)
	}
}

func TestCParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewCParser(), ".c", "")
}

// ─── C++ ─────────────────────────────────────────────────────────────────────

const cppSource = `#include <string>

namespace auth {

class AuthService {
public:
    bool login(const std::string& user);
    void logout(const std::string& user);
};

struct Config {
    int timeout;
};

} // namespace auth
`

func parseCpp(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "src/auth.cpp", []byte(src)); err != nil {
		t.Fatalf("CppParser.Parse() error: %v", err)
	}
	return g
}

func TestCppParser_Extensions(t *testing.T) {
	exts := parser.NewCppParser().Extensions()
	if !hasExtension(exts, ".cpp") || !hasExtension(exts, ".hpp") {
		t.Errorf("Extensions() = %v, missing .cpp or .hpp", exts)
	}
}

func TestCppParser_FileNode(t *testing.T) {
	assertFileNode(t, parseCpp(t, cppSource), "auth.cpp")
}

func TestCppParser_ExtractsClass(t *testing.T) {
	assertNode(t, parseCpp(t, cppSource), "AuthService", graph.NodeStruct)
}

func TestCppParser_ExtractsStruct(t *testing.T) {
	assertNode(t, parseCpp(t, cppSource), "Config", graph.NodeStruct)
}

func TestCppParser_ExtractsNamespace(t *testing.T) {
	assertNode(t, parseCpp(t, cppSource), "auth", graph.NodeStruct)
}

func TestCppParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewCppParser(), ".cpp", "")
}

// ─── C# ──────────────────────────────────────────────────────────────────────

const csharpSource = `using System;

namespace Auth {
    public interface IAuthenticator {
        bool Login(string user);
    }

    public class AuthService : IAuthenticator {
        public bool Login(string user) { return true; }
        public void Logout(string user) {}
    }
}
`

func parseCSharp(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Auth/AuthService.cs", []byte(src)); err != nil {
		t.Fatalf("CSharpParser.Parse() error: %v", err)
	}
	return g
}

func TestCSharpParser_Extensions(t *testing.T) {
	exts := parser.NewCSharpParser().Extensions()
	if !hasExtension(exts, ".cs") {
		t.Errorf("Extensions() = %v, missing .cs", exts)
	}
}

func TestCSharpParser_FileNode(t *testing.T) {
	assertFileNode(t, parseCSharp(t, csharpSource), "AuthService.cs")
}

func TestCSharpParser_ExtractsInterface(t *testing.T) {
	assertNode(t, parseCSharp(t, csharpSource), "IAuthenticator", graph.NodeInterface)
}

func TestCSharpParser_ExtractsClass(t *testing.T) {
	assertNode(t, parseCSharp(t, csharpSource), "AuthService", graph.NodeStruct)
}

func TestCSharpParser_ExtractsMethod(t *testing.T) {
	assertNode(t, parseCSharp(t, csharpSource), "Login", graph.NodeMethod)
}

func TestCSharpParser_ImportEdge(t *testing.T) {
	g := parseCSharp(t, csharpSource)
	fileID := g.FindByName("AuthService.cs")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 1 {
		t.Errorf("expected ≥1 IMPORTS edges from using directives, got %d", importCount)
	}
}

func TestCSharpParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewCSharpParser(), ".cs", "")
}

// ─── Swift ───────────────────────────────────────────────────────────────────

const swiftSource = `import Foundation

protocol Authenticator {
    func login(user: String) -> Bool
}

struct AuthConfig {
    let timeout: Int
}

class AuthService: Authenticator {
    func login(user: String) -> Bool { return true }
}

func createService() -> AuthService {
    return AuthService()
}
`

func parseSwift(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewSwiftParser()
	if err := p.Parse(g, "Sources/Auth.swift", []byte(src)); err != nil {
		t.Fatalf("SwiftParser.Parse() error: %v", err)
	}
	return g
}

func TestSwiftParser_Extensions(t *testing.T) {
	exts := parser.NewSwiftParser().Extensions()
	if !hasExtension(exts, ".swift") {
		t.Errorf("Extensions() = %v, missing .swift", exts)
	}
}

func TestSwiftParser_FileNode(t *testing.T) {
	assertFileNode(t, parseSwift(t, swiftSource), "Auth.swift")
}

func TestSwiftParser_ExtractsProtocol(t *testing.T) {
	assertNode(t, parseSwift(t, swiftSource), "Authenticator", graph.NodeInterface)
}

func TestSwiftParser_ExtractsStruct(t *testing.T) {
	assertNode(t, parseSwift(t, swiftSource), "AuthConfig", graph.NodeStruct)
}

func TestSwiftParser_ExtractsClass(t *testing.T) {
	assertNode(t, parseSwift(t, swiftSource), "AuthService", graph.NodeStruct)
}

func TestSwiftParser_ExtractsFunction(t *testing.T) {
	assertNode(t, parseSwift(t, swiftSource), "createService", graph.NodeFunction)
}

func TestSwiftParser_ImportEdge(t *testing.T) {
	g := parseSwift(t, swiftSource)
	fileID := g.FindByName("Auth.swift")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 1 {
		t.Errorf("expected ≥1 IMPORTS edges from import declarations, got %d", importCount)
	}
}

func TestSwiftParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewSwiftParser(), ".swift", "")
}

// ─── Ruby ────────────────────────────────────────────────────────────────────

const rubySource = `module Authentication
  class AuthService
    def login(user)
      true
    end

    def logout(user)
    end

    def self.create
      new
    end
  end
end
`

func parseRuby(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewRubyParser()
	if err := p.Parse(g, "lib/auth_service.rb", []byte(src)); err != nil {
		t.Fatalf("RubyParser.Parse() error: %v", err)
	}
	return g
}

func TestRubyParser_Extensions(t *testing.T) {
	exts := parser.NewRubyParser().Extensions()
	if !hasExtension(exts, ".rb") {
		t.Errorf("Extensions() = %v, missing .rb", exts)
	}
}

func TestRubyParser_FileNode(t *testing.T) {
	assertFileNode(t, parseRuby(t, rubySource), "auth_service.rb")
}

func TestRubyParser_ExtractsModule(t *testing.T) {
	assertNode(t, parseRuby(t, rubySource), "Authentication", graph.NodeInterface)
}

func TestRubyParser_ExtractsClass(t *testing.T) {
	assertNode(t, parseRuby(t, rubySource), "AuthService", graph.NodeStruct)
}

func TestRubyParser_ExtractsMethod(t *testing.T) {
	assertNode(t, parseRuby(t, rubySource), "login", graph.NodeMethod)
}

func TestRubyParser_ExtractsSingletonMethod(t *testing.T) {
	assertNode(t, parseRuby(t, rubySource), "create", graph.NodeMethod)
}

func TestRubyParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewRubyParser(), ".rb", "")
}

// ─── PHP ─────────────────────────────────────────────────────────────────────

const phpSource = `<?php

interface Authenticator {
    public function login(string $user): bool;
}

class AuthService implements Authenticator {
    public function login(string $user): bool {
        return true;
    }

    public function logout(string $user): void {}
}

function create_service(): AuthService {
    return new AuthService();
}
`

func parsePHP(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "src/AuthService.php", []byte(src)); err != nil {
		t.Fatalf("PHPParser.Parse() error: %v", err)
	}
	return g
}

func TestPHPParser_Extensions(t *testing.T) {
	exts := parser.NewPHPParser().Extensions()
	if !hasExtension(exts, ".php") {
		t.Errorf("Extensions() = %v, missing .php", exts)
	}
}

func TestPHPParser_FileNode(t *testing.T) {
	assertFileNode(t, parsePHP(t, phpSource), "AuthService.php")
}

func TestPHPParser_ExtractsInterface(t *testing.T) {
	assertNode(t, parsePHP(t, phpSource), "Authenticator", graph.NodeInterface)
}

func TestPHPParser_ExtractsClass(t *testing.T) {
	assertNode(t, parsePHP(t, phpSource), "AuthService", graph.NodeStruct)
}

func TestPHPParser_ExtractsFunction(t *testing.T) {
	assertNode(t, parsePHP(t, phpSource), "create_service", graph.NodeFunction)
}

func TestPHPParser_ExtractsMethod(t *testing.T) {
	assertNode(t, parsePHP(t, phpSource), "login", graph.NodeMethod)
}

func TestPHPParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewPHPParser(), ".php", "<?php")
}

// ─── Lua ─────────────────────────────────────────────────────────────────────

// Lua uses isExported (uppercase first letter) for function_declaration;
// local functions are always Exported=false regardless of name.
const luaSource = `function Login(user)
    return true
end

function Logout(user)
end

local function validate(user)
    return user ~= nil
end
`

func parseLua(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewLuaParser()
	if err := p.Parse(g, "auth/auth.lua", []byte(src)); err != nil {
		t.Fatalf("LuaParser.Parse() error: %v", err)
	}
	return g
}

func TestLuaParser_Extensions(t *testing.T) {
	exts := parser.NewLuaParser().Extensions()
	if !hasExtension(exts, ".lua") {
		t.Errorf("Extensions() = %v, missing .lua", exts)
	}
}

func TestLuaParser_FileNode(t *testing.T) {
	assertFileNode(t, parseLua(t, luaSource), "auth.lua")
}

func TestLuaParser_ExtractsGlobalFunction(t *testing.T) {
	g := parseLua(t, luaSource)
	n := assertNode(t, g, "Login", graph.NodeFunction)
	if !n.Exported {
		t.Error("Login (global, uppercase) should be marked exported")
	}
}

func TestLuaParser_ExtractsLocalFunction(t *testing.T) {
	g := parseLua(t, luaSource)
	n := assertNode(t, g, "validate", graph.NodeFunction)
	if n.Exported {
		t.Error("validate (local function) should not be exported")
	}
}

func TestLuaParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewLuaParser(), ".lua", "")
}

// ─── Elixir ──────────────────────────────────────────────────────────────────

const elixirSource = `defmodule Auth.AuthService do
  def login(user) do
    {:ok, user}
  end

  def logout(user) do
    :ok
  end

  defp validate(user) do
    true
  end
end
`

func parseElixir(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "lib/auth_service.ex", []byte(src)); err != nil {
		t.Fatalf("ElixirParser.Parse() error: %v", err)
	}
	return g
}

func TestElixirParser_Extensions(t *testing.T) {
	exts := parser.NewElixirParser().Extensions()
	if !hasExtension(exts, ".ex") || !hasExtension(exts, ".exs") {
		t.Errorf("Extensions() = %v, missing .ex or .exs", exts)
	}
}

func TestElixirParser_FileNode(t *testing.T) {
	assertFileNode(t, parseElixir(t, elixirSource), "auth_service.ex")
}

func TestElixirParser_ExtractsModule(t *testing.T) {
	g := parseElixir(t, elixirSource)
	n := assertNode(t, g, "Auth.AuthService", graph.NodeStruct)
	if !n.Exported {
		t.Error("Elixir modules are always exported")
	}
}

func TestElixirParser_DefinesModuleEdge(t *testing.T) {
	g := parseElixir(t, elixirSource)
	fileID := g.FindByName("auth_service.ex")[0].ID
	assertDefinesEdge(t, g, fileID, "Auth.AuthService")
}

func TestElixirParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewElixirParser(), ".ex", "")
}

// ─── Protocol Buffers ────────────────────────────────────────────────────────

const protoSource = `syntax = "proto3";

package auth;

message LoginRequest {
    string user = 1;
    string password = 2;
}

message LoginResponse {
    string token = 1;
}

enum Status {
    ACTIVE = 0;
    INACTIVE = 1;
}

service AuthService {
    rpc Login(LoginRequest) returns (LoginResponse);
    rpc Logout(LoginRequest) returns (LoginResponse);
}
`

func parseProto(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewProtobufParser()
	if err := p.Parse(g, "proto/auth.proto", []byte(src)); err != nil {
		t.Fatalf("ProtobufParser.Parse() error: %v", err)
	}
	return g
}

func TestProtobufParser_Extensions(t *testing.T) {
	exts := parser.NewProtobufParser().Extensions()
	if !hasExtension(exts, ".proto") {
		t.Errorf("Extensions() = %v, missing .proto", exts)
	}
}

func TestProtobufParser_FileNode(t *testing.T) {
	assertFileNode(t, parseProto(t, protoSource), "auth.proto")
}

func TestProtobufParser_ExtractsMessages(t *testing.T) {
	g := parseProto(t, protoSource)
	assertNode(t, g, "LoginRequest", graph.NodeStruct)
	assertNode(t, g, "LoginResponse", graph.NodeStruct)
}

func TestProtobufParser_ExtractsEnum(t *testing.T) {
	assertNode(t, parseProto(t, protoSource), "Status", graph.NodeStruct)
}

func TestProtobufParser_ExtractsService(t *testing.T) {
	assertNode(t, parseProto(t, protoSource), "AuthService", graph.NodeInterface)
}

func TestProtobufParser_ExtractsRPCs(t *testing.T) {
	g := parseProto(t, protoSource)
	assertNode(t, g, "Login", graph.NodeMethod)
	assertNode(t, g, "Logout", graph.NodeMethod)
}

func TestProtobufParser_DefinesEdges(t *testing.T) {
	g := parseProto(t, protoSource)
	fileID := g.FindByName("auth.proto")[0].ID
	for _, name := range []string{"LoginRequest", "LoginResponse", "Status", "AuthService", "Login", "Logout"} {
		assertDefinesEdge(t, g, fileID, name)
	}
}

func TestProtobufParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewProtobufParser(), ".proto", `syntax = "proto3";`)
}
