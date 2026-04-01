package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Perl ────────────────────────────────────────────────────────────────────

func parsePerlSrc(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("")
	p := parser.NewPerlParser()
	if err := p.Parse(g, "test.pm", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	return g
}

func TestPerlParser_Extensions(t *testing.T) {
	p := parser.NewPerlParser()
	exts := p.Extensions()
	want := map[string]bool{".pl": true, ".pm": true, ".t": true}
	for _, e := range exts {
		if !want[e] {
			t.Errorf("unexpected extension %s", e)
		}
	}
	if len(exts) != 3 {
		t.Errorf("expected 3 extensions, got %d", len(exts))
	}
}

func TestPerlParser_PackageAndSubs(t *testing.T) {
	g := parsePerlSrc(t, `package MyModule;
use strict;
use warnings;
use Scalar::Util qw(blessed);

our $VERSION = '1.0';

sub new {
    my ($class, %args) = @_;
    return bless {}, $class;
}

sub greet {
    my ($self, $name) = @_;
    return "Hello, $name!";
}

sub _private {
    my ($self) = @_;
}
`)
	assertNode(t, g, "MyModule", graph.NodePackage)

	newFn := assertNode(t, g, "MyModule::new", graph.NodeFunction)
	if newFn != nil && !newFn.Exported {
		t.Error("new should be exported")
	}

	privateFn := assertNode(t, g, "MyModule::_private", graph.NodeFunction)
	if privateFn != nil && privateFn.Exported {
		t.Error("_private should not be exported")
	}

	versionVar := assertNode(t, g, "VERSION", graph.NodeVariable)
	if versionVar != nil && !versionVar.Exported {
		t.Error("our $VERSION should be exported")
	}
}

func TestPerlParser_UseImports(t *testing.T) {
	g := parsePerlSrc(t, `package Foo;
use Carp qw(croak);
use MIME::Base64;
use strict;
use warnings;
sub do_thing { }
`)
	assertNode(t, g, "Carp", graph.NodePackage)
	assertNode(t, g, "MIME::Base64", graph.NodePackage)
	// strict and warnings are real imports (ground truth expects them).
	assertNode(t, g, "strict", graph.NodePackage)
	assertNode(t, g, "warnings", graph.NodePackage)
}

func TestPerlParser_MultiplePackages(t *testing.T) {
	g := parsePerlSrc(t, `package Animal;
sub new { return bless {}, shift; }
sub speak { }

package Dog;
sub new { return bless {}, shift; }
sub bark { return "Woof!"; }
`)
	assertNode(t, g, "Animal", graph.NodePackage)
	assertNode(t, g, "Dog", graph.NodePackage)
	assertNode(t, g, "Animal::new", graph.NodeFunction)
	assertNode(t, g, "Dog::bark", graph.NodeFunction)
}

func TestPerlParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewPerlParser(), ".pm", "")
}

// ─── MATLAB ───────────────────────────────────────────────────────────────────

func parseMATLABSrc(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("")
	p := parser.NewMATLABParser()
	if err := p.Parse(g, "test.m", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	return g
}

func TestMATLABParser_Extensions(t *testing.T) {
	p := parser.NewMATLABParser()
	exts := p.Extensions()
	if len(exts) != 1 || exts[0] != ".m" {
		t.Errorf("expected [.m], got %v", exts)
	}
}

func TestMATLABParser_TopLevelFunctions(t *testing.T) {
	g := parseMATLABSrc(t, `function y = square(x)
    y = x .^ 2;
end

function result = add(a, b)
    result = a + b;
end

function _internal()
end
`)
	squareFn := assertNode(t, g, "square", graph.NodeFunction)
	if squareFn != nil && !squareFn.Exported {
		t.Error("square should be exported")
	}

	assertNode(t, g, "add", graph.NodeFunction)

	internalFn := assertNode(t, g, "_internal", graph.NodeFunction)
	if internalFn != nil && internalFn.Exported {
		t.Error("_internal should not be exported")
	}
}

func TestMATLABParser_ClassDef(t *testing.T) {
	g := parseMATLABSrc(t, `classdef Animal < handle
    properties
        Name
        Sound = 'generic'
    end
    properties (Access = private)
        secret = 42
    end
    methods
        function obj = Animal(name)
            obj.Name = name;
        end
        function speak(obj)
            fprintf('%s\n', obj.Sound);
        end
    end
    methods (Static)
        function result = create(name)
            result = Animal(name);
        end
    end
end
`)
	animalClass := assertNode(t, g, "Animal", graph.NodeStruct)
	if animalClass != nil && animalClass.Metadata["kind"] != "classdef" {
		t.Errorf("expected kind=classdef, got %q", animalClass.Metadata["kind"])
	}

	nameProp := assertNode(t, g, "Name", graph.NodeVariable)
	if nameProp != nil && !nameProp.Exported {
		t.Error("public property Name should be exported")
	}

	secretProp := assertNode(t, g, "secret", graph.NodeVariable)
	if secretProp != nil && secretProp.Exported {
		t.Error("private property secret should not be exported")
	}

	assertNode(t, g, "Animal.speak", graph.NodeMethod)

	createFn := assertNode(t, g, "Animal.create", graph.NodeFunction)
	if createFn != nil && createFn.Type != graph.NodeFunction {
		t.Errorf("static method should be NodeFunction, got %s", createFn.Type)
	}
}

func TestMATLABParser_Superclass(t *testing.T) {
	g := parseMATLABSrc(t, `classdef Dog < Animal
    methods
        function bark(obj)
            fprintf('Woof!\n');
        end
    end
end
`)
	dog := assertNode(t, g, "Dog", graph.NodeStruct)
	if dog != nil && dog.Metadata["superclasses"] != "Animal" {
		t.Errorf("expected superclass Animal, got %q", dog.Metadata["superclasses"])
	}
	// Superclass node should also exist.
	assertNode(t, g, "Animal", graph.NodeStruct)
}

func TestMATLABParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewMATLABParser(), ".m", "")
}
