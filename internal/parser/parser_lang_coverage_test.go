package parser_test

// Coverage tests for language-specific parser branches that aren't exercised
// by the existing language tests:
// - C / C++ with full constructs (function defs, string/system includes, struct, class, namespace)
// - Elixir def without parentheses (identifier path in extractElixirDeclInfo)
// - Go with switch/select/binary expressions (countComplexity branches)
// - Java private method (isJavaPublic false branch)
// - JavaScript/Kotlin/Groovy/CSharp richer source

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ── C++ full-construct source ─────────────────────────────────────────────────

const cppFull = `
#include "myheader.h"
#include <iostream>

namespace myns {

class AuthService {
public:
    bool login(const std::string& user);
};

struct Point {
    int x;
    int y;
};

}

int square(int x) {
    return x * x;
}

int add(int a, int b) {
    return a + b;
}
`

func TestCppParser_StringInclude(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "src/auth.cpp", []byte(cppFull)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Should have a file node.
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from C++ parse")
	}
}

func TestCppParser_FunctionDef(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "src/math.cpp", []byte(cppFull)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// square and add should be extracted as functions.
	nodes := g.FindByName("square")
	if len(nodes) == 0 {
		t.Error("expected function 'square' to be extracted")
	}
}

func TestCppParser_ClassAndStruct(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCppParser()
	if err := p.Parse(g, "src/auth.cpp", []byte(cppFull)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	nodes := g.FindByName("AuthService")
	if len(nodes) == 0 {
		t.Error("expected struct/class 'AuthService' from C++ parse")
	}
}

// ── C full-construct source ───────────────────────────────────────────────────

const cFull = `
#include "config.h"
#include <stdio.h>

struct Options {
    int verbose;
    int timeout;
};

enum Status {
    OK = 0,
    ERROR = 1
};

int compute(int x, int y) {
    return x + y;
}

void printResult(int r) {
    printf("%d\n", r);
}
`

func TestCParser_StringIncludeAndFunction(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCParser()
	if err := p.Parse(g, "src/util.c", []byte(cFull)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from C parse")
	}
	// Functions should be extracted.
	nodes := g.FindByName("compute")
	if len(nodes) == 0 {
		t.Error("expected function 'compute' from C parse")
	}
}

// ── Elixir: def without parentheses (identifier path) ────────────────────────

const elixirNoArgs = `
defmodule Simple do
  # A simple function without arguments
  def hello do
    :world
  end

  def greet(name) do
    "Hello #{name}"
  end
end
`

func TestElixirParser_DefWithoutArgs(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewElixirParser()
	if err := p.Parse(g, "lib/simple.ex", []byte(elixirNoArgs)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from Elixir parse")
	}
}

// ── Go: switch, select, binary expressions (countComplexity branches) ─────────

const goComplexSource = `package p

// HasComplexLogic uses if/for/switch/select and binary expressions.
func HasComplexLogic(x int, a, b bool, ch chan int) string {
	// if_statement branch
	if x > 0 {
		_ = x
	}

	// for_statement branch
	for i := 0; i < x; i++ {
		_ = i
	}

	// switch with expression_case
	switch x {
	case 1:
		return "one"
	case 2:
		return "two"
	default:
		_ = x
	}

	// type switch with type_case
	var v interface{} = x
	switch v.(type) {
	case int:
		return "int"
	case string:
		return "string"
	}

	// binary expression with &&
	if a && b {
		return "both"
	}

	// binary expression with ||
	if a || b {
		return "either"
	}

	_ = ch
	return "other"
}

// SelectFunc uses a select statement with communication_case.
func SelectFunc(ch chan int) {
	select {
	case v := <-ch:
		_ = v
	default:
	}
}
`

func TestGoParser_ComplexityBranches(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "pkg/complex.go", []byte(goComplexSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	nodes := g.FindByName("HasComplexLogic")
	if len(nodes) == 0 {
		t.Fatal("expected function 'HasComplexLogic' from Go parse")
	}
	// Complexity should be > 1 due to if/for/switch/binary branches.
	n := nodes[0]
	if n.Metadata["complexity"] == "" {
		t.Error("expected 'complexity' metadata to be set")
	}
}

func TestGoParser_SelectComplexity(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "pkg/select.go", []byte(goComplexSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	nodes := g.FindByName("SelectFunc")
	if len(nodes) == 0 {
		t.Fatal("expected function 'SelectFunc' from Go parse")
	}
}

// ── Java: private method (isJavaPublic false branch) ──────────────────────────

const javaPrivateSource = `
package com.example;

public class Service {
    private String secret;

    private void internalMethod() {
        // private method — isJavaPublic returns false
    }

    protected int protectedMethod() {
        return 42;
    }
}
`

func TestJavaParser_PrivateMethod(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Service.java", []byte(javaPrivateSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from Java parse")
	}
}

// ── JavaScript: export default class, arrow in class ─────────────────────────

const jsRichSource = `
import { helper } from './helper.js';
import utils from './utils.js';

/**
 * Auth service class.
 */
class AuthService {
  constructor() {
    this.token = null;
  }

  async login(user, pass) {
    return helper(user, pass);
  }
}

const validate = (token) => token != null;

export default AuthService;
export { validate };
`

func TestJavaScriptParser_RichSource(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaScriptParser()
	if err := p.Parse(g, "src/auth.js", []byte(jsRichSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from rich JavaScript source")
	}
}

// ── JavaScript: export const arrow function ────────────────────────────────────

const jsExportArrowSource = `
// Exported arrow functions (export const foo = () => ...)
export const validate = (token) => token != null;

export const hashPassword = (pass) => {
  return pass.split('').reverse().join('');
};

export const createUser = function(name, email) {
  return { name, email };
};

export class UserService {
  constructor() {
    this.users = [];
  }
  login(user) {
    return this.users.includes(user);
  }
}
`

func TestJavaScriptParser_ExportArrow(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewJavaScriptParser()
	if err := p.Parse(g, "src/utils.js", []byte(jsExportArrowSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from JS export arrow source")
	}
	// validate should be extracted as an exported arrow function.
	nodes := g.FindByName("validate")
	if len(nodes) == 0 {
		t.Error("expected 'validate' exported arrow function to be extracted")
	}
}

// ── Kotlin: data class, companion object ─────────────────────────────────────

const kotlinRichSource = `
package com.example

data class User(val name: String, val email: String)

class AuthService {
    companion object {
        fun create(): AuthService = AuthService()
    }

    fun login(user: String, pass: String): Boolean {
        return user.isNotEmpty() && pass.isNotEmpty()
    }

    private fun hash(pass: String): String = pass
}

interface Authenticator {
    fun authenticate(user: String): Boolean
}
`

func TestKotlinParser_RichSource(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewKotlinParser()
	if err := p.Parse(g, "src/Auth.kt", []byte(kotlinRichSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from Kotlin parse")
	}
}

// ── CSharp: private members ───────────────────────────────────────────────────

const csharpPrivateSource = `
using System;

namespace MyApp {
    public class AuthService {
        private string secret;

        private void InternalMethod() {
            // private
        }

        protected int ProtectedMethod() {
            return 42;
        }
    }
}
`

func TestCSharpParser_PrivateMembers(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewCSharpParser()
	if err := p.Parse(g, "Auth.cs", []byte(csharpPrivateSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from C# parse")
	}
}

// ── Python: decorated definitions ─────────────────────────────────────────────

const pythonDecoratedSource = `
import functools

class MyService:
    @property
    def value(self):
        return self._value

    @staticmethod
    def create():
        return MyService()

    @classmethod
    def from_string(cls, s):
        return cls()

@functools.lru_cache(maxsize=128)
def cached_compute(n):
    return n * n

def plain_func(x):
    return x + 1
`

func TestPythonParser_DecoratedDefs(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "service.py", []byte(pythonDecoratedSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from Python decorated source")
	}
}

// ── Groovy: richer source ─────────────────────────────────────────────────────

const groovyRichSource = `
package com.example

class BuildService {
    private String name

    void build(String target) {
        println "Building ${target}"
    }

    protected String getName() {
        return name
    }
}

interface Buildable {
    void build(String target)
}
`

func TestGroovyParser_RichSource(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGroovyParser()
	if err := p.Parse(g, "Build.groovy", []byte(groovyRichSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from Groovy parse")
	}
}

// ── TypeScript: function calls for collectTSCallSites ──────────────────────────

const tsCallSiteSource = `
import { helper } from './helper';
import utils from './utils';

class AuthService {
  constructor(private db: Database) {}

  async login(user: string, pass: string): Promise<boolean> {
    const result = this.db.query(user);
    return validatePassword(pass, result);
  }

  logout(): void {
    this.db.close();
    cleanup();
  }
}

function validatePassword(pass: string, hash: string): boolean {
  return hash === pass;
}

export { AuthService, validatePassword };
`

func TestTypeScriptParser_CallSites(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "src/auth.ts", []byte(tsCallSiteSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from TypeScript parse")
	}
}

// ── Go: value receiver and doc comments ───────────────────────────────────────

const goValueReceiverSource = `package p

// Graph is the main graph structure.
type Graph struct {
	nodes map[string]int
}

// NodeCount returns the number of nodes.
func (g Graph) NodeCount() int {
	return len(g.nodes)
}

// AddNode adds a node to the graph.
func (g *Graph) AddNode(name string) {
	g.nodes[name] = 1
}
`

func TestGoParser_ValueReceiver(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "pkg/graph.go", []byte(goValueReceiverSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Methods on value receiver should be extracted.
	nodes := g.FindByName("Graph.NodeCount")
	if len(nodes) == 0 {
		t.Error("expected method 'Graph.NodeCount' to be extracted")
	}
}

// ── Go: 2-level selector calls (a.b.Method()) ────────────────────────────────

const goTwoLevelSelectorSource = `package p

type Graph struct{}

func (g *Graph) Initialize() {}
func (g *Graph) Query(q string) {}

type Server struct {
	graph *Graph
	db    *Graph
}

// Run uses 2-level selector calls like s.graph.Initialize()
func (s *Server) Run() {
	s.graph.Initialize()
	s.db.Query("select *")
}
`

func TestGoParser_TwoLevelSelector(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGoParser()
	if err := p.Parse(g, "pkg/server.go", []byte(goTwoLevelSelectorSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	nodes := g.FindByName("Server.Run")
	if len(nodes) == 0 {
		t.Error("expected method 'Server.Run' to be extracted")
	}
}

// ── Python: source with doc comments (triggers extractLineDoc) ────────────────

const pythonWithDocSource = `
# Authentication module

# Validates the user token.
def validate_token(token):
    return token is not None

# Compute hash.
def compute_hash(data):
    return hash(data)

class UserService:
    # Initialize the service.
    def __init__(self):
        self.users = {}
`

func TestPythonParser_WithDocComments(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "auth.py", []byte(pythonWithDocSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from Python source with doc comments")
	}
}

// ── Rust: richer source ────────────────────────────────────────────────────────

const rustRichSource = `
use std::collections::HashMap;

pub struct AuthService {
    users: HashMap<String, String>,
}

impl AuthService {
    pub fn new() -> Self {
        AuthService { users: HashMap::new() }
    }

    pub fn login(&self, user: &str, pass: &str) -> bool {
        self.users.get(user).map_or(false, |p| p == pass)
    }

    fn hash(pass: &str) -> String {
        pass.to_string()
    }
}

pub trait Authenticator {
    fn authenticate(&self, user: &str) -> bool;
}
`

func TestRustParser_RichSource(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewRustParser()
	if err := p.Parse(g, "src/auth.rs", []byte(rustRichSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from Rust parse")
	}
}

// ── PHP: richer source ─────────────────────────────────────────────────────────

const phpRichSource = `<?php
namespace App\Auth;

class AuthService {
    private string $secret;

    public function login(string $user, string $pass): bool {
        return $this->validate($user, $pass);
    }

    protected function validate(string $user, string $pass): bool {
        return !empty($user) && !empty($pass);
    }

    private function hash(string $pass): string {
        return password_hash($pass, PASSWORD_BCRYPT);
    }
}

interface Authenticator {
    public function authenticate(string $user): bool;
}
`

func TestPHPParser_RichSource(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPHPParser()
	if err := p.Parse(g, "src/Auth.php", []byte(phpRichSource)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Fatal("expected nodes from PHP parse")
	}
}
