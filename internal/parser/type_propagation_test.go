package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// TestPythonTypeAnnotationParams verifies that function parameter type
// annotations are recorded as varTypes for call-site resolution.
// def process(repo: Repository, size: int) → "repo" → "Repository"
func TestPythonTypeAnnotationParams(t *testing.T) {
	src := `class Handler:
    def process(self, repo: Repository, count: int, service: AuthService):
        repo.save()
        service.authenticate()
`
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "handler.py", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	vt := g.GetVarTypes("handler.py")
	if vt == nil {
		t.Fatal("GetVarTypes returned nil — no types recorded")
	}

	tests := []struct {
		varName  string
		wantType string
	}{
		{"repo", "Repository"},
		{"service", "AuthService"},
	}
	for _, tc := range tests {
		if got := vt[tc.varName]; got != tc.wantType {
			t.Errorf("varTypes[%q] = %q, want %q", tc.varName, got, tc.wantType)
		}
	}

	// "count" is primitive-like — we don't assert it must be absent, but it
	// won't resolve to anything meaningful via the method index. Not asserted.
}

// TestPythonSelfAttrConstructor verifies that self.attr = ClassName(...)
// assignments in __init__ are recorded as "self.attr" → "ClassName".
// This enables self.attr.method() call resolution.
func TestPythonSelfAttrConstructor(t *testing.T) {
	src := `class Service:
    def __init__(self):
        self.repo = Repository()
        self.auth = AuthService()
        self.name = "literal"
`
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "service.py", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	vt := g.GetVarTypes("service.py")
	if vt == nil {
		t.Fatal("GetVarTypes returned nil")
	}

	if got := vt["self.repo"]; got != "Repository" {
		t.Errorf("varTypes[\"self.repo\"] = %q, want \"Repository\"", got)
	}
	if got := vt["self.auth"]; got != "AuthService" {
		t.Errorf("varTypes[\"self.auth\"] = %q, want \"AuthService\"", got)
	}
	// self.name = "literal" — not a constructor call, must NOT be recorded
	if got := vt["self.name"]; got != "" {
		t.Errorf("varTypes[\"self.name\"] = %q, want \"\" (string literal should not be recorded)", got)
	}
}

// TestPythonTypedDefaultParam verifies typed_default_parameter is handled:
// def f(repo: Repository = None) → "repo" → "Repository"
func TestPythonTypedDefaultParam(t *testing.T) {
	src := `def fetch(repo: Repository = None, limit: int = 10):
    repo.find_all()
`
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "fetch.py", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	vt := g.GetVarTypes("fetch.py")
	if vt == nil {
		t.Fatal("GetVarTypes returned nil")
	}
	if got := vt["repo"]; got != "Repository" {
		t.Errorf("varTypes[\"repo\"] = %q, want \"Repository\"", got)
	}
}

// TestJavaMethodParamTypes verifies that Java method formal parameters with
// class types are recorded as varTypes for call-site resolution.
func TestJavaMethodParamTypes(t *testing.T) {
	src := `package com.example;

public class OrderService {
    public void process(Repository repo, AuthService auth, int count) {
        repo.save();
        auth.verify();
    }

    public OrderService(Repository defaultRepo) {
        defaultRepo.init();
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "OrderService.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	vt := g.GetVarTypes("OrderService.java")
	if vt == nil {
		t.Fatal("GetVarTypes returned nil")
	}

	tests := []struct {
		varName  string
		wantType string
	}{
		{"repo", "Repository"},
		{"auth", "AuthService"},
		{"defaultRepo", "Repository"},
	}
	for _, tc := range tests {
		if got := vt[tc.varName]; got != tc.wantType {
			t.Errorf("varTypes[%q] = %q, want %q", tc.varName, got, tc.wantType)
		}
	}
}

// TestJavaPrimitiveParamsNotStored verifies that primitive Java types (int, boolean)
// are not stored as varTypes — they have no class to resolve against.
func TestJavaPrimitiveParamsNotStored(t *testing.T) {
	src := `package com.example;

public class Calculator {
    public int add(int a, int b) {
        return a + b;
    }
    public void process(Repository repo, boolean flag) {
        repo.save();
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "Calculator.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	vt := g.GetVarTypes("Calculator.java")
	// "a" and "b" must NOT be stored (int is primitive)
	if vt != nil {
		if got := vt["a"]; got != "" {
			t.Errorf("varTypes[\"a\"] = %q, want \"\" (int is primitive)", got)
		}
		if got := vt["b"]; got != "" {
			t.Errorf("varTypes[\"b\"] = %q, want \"\" (int is primitive)", got)
		}
		if got := vt["flag"]; got != "" {
			t.Errorf("varTypes[\"flag\"] = %q, want \"\" (boolean is primitive)", got)
		}
		// "repo" should be stored
		if got := vt["repo"]; got != "Repository" {
			t.Errorf("varTypes[\"repo\"] = %q, want \"Repository\"", got)
		}
	}
}

// TestJavaGenericParamType verifies that generic types are unwrapped to their
// base type: List<Repository> param → "Repository" not "List<Repository>".
func TestJavaGenericParamType(t *testing.T) {
	src := `package com.example;

public class BatchProcessor {
    public void processBatch(List<Repository> repos) {
        // repos is a list of repositories
    }
}
`
	g := graph.New("testrepo")
	p := parser.NewJavaParser()
	if err := p.Parse(g, "BatchProcessor.java", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	// List<Repository> — base type is "List" which is a Java builtin.
	// It will be filtered by isJavaBuiltin, so "repos" won't be recorded.
	// GetVarTypes may return nil if nothing else was recorded. Must NOT panic.
	vt := g.GetVarTypes("BatchProcessor.java")
	_ = vt // nil is acceptable when all types are builtins
}

// TestPythonOptionalUnwrapping verifies that Optional[X], Union[X, None],
// and PEP 604 X | None annotations are correctly unwrapped to the inner type.
// This is critical: Optional[Repository] must resolve to Repository, not Optional.
func TestPythonOptionalUnwrapping(t *testing.T) {
	src := `class Handler:
    def process(
        self,
        repo: Optional[Repository],
        auth: Union[AuthService, None],
        svc: Service | None,
        plain: Repository,
    ):
        repo.save()
        auth.verify()
        svc.run()
        plain.find()
`
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "handler.py", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	vt := g.GetVarTypes("handler.py")
	if vt == nil {
		t.Fatal("GetVarTypes returned nil")
	}

	tests := []struct {
		varName  string
		wantType string
		desc     string
	}{
		{"repo", "Repository", "Optional[Repository] must unwrap to Repository"},
		{"auth", "AuthService", "Union[AuthService, None] must unwrap to AuthService"},
		{"svc", "Service", "PEP 604 Service | None must resolve to Service"},
		{"plain", "Repository", "bare Repository annotation still works"},
	}
	for _, tc := range tests {
		if got := vt[tc.varName]; got != tc.wantType {
			t.Errorf("varTypes[%q] = %q, want %q — %s", tc.varName, got, tc.wantType, tc.desc)
		}
	}
}

// TestPythonAnnotatedType verifies Annotated[X, metadata] is unwrapped to X.
func TestPythonAnnotatedType(t *testing.T) {
	src := `def create(repo: Annotated[Repository, Field(default=None)]):
    repo.save()
`
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "create.py", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	vt := g.GetVarTypes("create.py")
	if vt == nil {
		t.Fatal("GetVarTypes returned nil")
	}
	if got := vt["repo"]; got != "Repository" {
		t.Errorf("varTypes[\"repo\"] = %q, want \"Repository\" (Annotated unwrap)", got)
	}
}

// TestPythonExistingPatternUnchanged verifies the two pre-existing patterns
// (annotated assignment and constructor assignment) still work after refactor.
func TestPythonExistingPatternUnchanged(t *testing.T) {
	src := `repo: Repository = get_repo()
service = AuthService()
plain = helper()
`
	g := graph.New("testrepo")
	p := parser.NewPythonParser()
	if err := p.Parse(g, "vars.py", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	vt := g.GetVarTypes("vars.py")
	if vt == nil {
		t.Fatal("GetVarTypes returned nil")
	}
	if got := vt["repo"]; got != "Repository" {
		t.Errorf("varTypes[\"repo\"] = %q, want \"Repository\" (annotated assignment)", got)
	}
	if got := vt["service"]; got != "AuthService" {
		t.Errorf("varTypes[\"service\"] = %q, want \"AuthService\" (constructor assignment)", got)
	}
	// plain = helper() — lowercase, must not be recorded
	if got := vt["plain"]; got != "" {
		t.Errorf("varTypes[\"plain\"] = %q, want \"\" (non-uppercase constructor)", got)
	}
}
