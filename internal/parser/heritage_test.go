package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// --- Java heritage tests ---

func TestJavaParser_HeritageImplements(t *testing.T) {
	src := `
public interface Serializable {}
public interface Comparable {}

public class User implements Serializable, Comparable {
}
`
	g := graph.New("test")
	if err := parser.NewJavaParser().Parse(g, "User.java", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("User")
	if len(nodes) == 0 {
		t.Fatal("class 'User' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi != "Serializable,Comparable" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Serializable,Comparable")
	}
}

func TestJavaParser_HeritageExtends(t *testing.T) {
	src := `
public class Base {}

public class Child extends Base {
}
`
	g := graph.New("test")
	if err := parser.NewJavaParser().Parse(g, "Child.java", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Child")
	if len(nodes) == 0 {
		t.Fatal("class 'Child' not found")
	}
	he := nodes[0].Metadata["heritage_extends"]
	if he != "Base" {
		t.Errorf("heritage_extends = %q, want %q", he, "Base")
	}
}

func TestJavaParser_HeritageExtendsAndImplements(t *testing.T) {
	src := `
public class AbstractService {}
public interface Logging {}

public class UserService extends AbstractService implements Logging {
}
`
	g := graph.New("test")
	if err := parser.NewJavaParser().Parse(g, "UserService.java", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("UserService")
	if len(nodes) == 0 {
		t.Fatal("class 'UserService' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	he := nodes[0].Metadata["heritage_extends"]
	if hi != "Logging" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Logging")
	}
	if he != "AbstractService" {
		t.Errorf("heritage_extends = %q, want %q", he, "AbstractService")
	}
}

func TestJavaParser_HeritageNoInheritance(t *testing.T) {
	src := `public class Standalone {}`
	g := graph.New("test")
	if err := parser.NewJavaParser().Parse(g, "Standalone.java", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Standalone")
	if len(nodes) == 0 {
		t.Fatal("class 'Standalone' not found")
	}
	if nodes[0].Metadata["heritage_implements"] != "" {
		t.Errorf("unexpected heritage_implements = %q", nodes[0].Metadata["heritage_implements"])
	}
}

func TestJavaParser_HeritageEnum(t *testing.T) {
	src := `
public interface Displayable {}

public enum Status implements Displayable {
    ACTIVE, INACTIVE
}
`
	g := graph.New("test")
	if err := parser.NewJavaParser().Parse(g, "Status.java", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Status")
	if len(nodes) == 0 {
		t.Fatal("enum 'Status' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi != "Displayable" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Displayable")
	}
}

func TestJavaParser_HeritageRecord(t *testing.T) {
	src := `
public interface Validatable {}

public record Point(int x, int y) implements Validatable {
}
`
	g := graph.New("test")
	if err := parser.NewJavaParser().Parse(g, "Point.java", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Point")
	if len(nodes) == 0 {
		t.Fatal("record 'Point' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi != "Validatable" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Validatable")
	}
}

// --- C# heritage tests ---

func TestCSharpParser_Heritage(t *testing.T) {
	src := `
public interface IDisposable {}
public interface ICloneable {}

public class Resource : IDisposable, ICloneable {
}
`
	g := graph.New("test")
	if err := parser.NewCSharpParser().Parse(g, "Resource.cs", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Resource")
	if len(nodes) == 0 {
		t.Fatal("class 'Resource' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi == "" {
		t.Error("heritage_implements is empty, expected base types")
	}
}

func TestCSharpParser_HeritageNoInheritance(t *testing.T) {
	src := `public class Plain {}`
	g := graph.New("test")
	if err := parser.NewCSharpParser().Parse(g, "Plain.cs", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Plain")
	if len(nodes) == 0 {
		t.Fatal("class 'Plain' not found")
	}
	if nodes[0].Metadata["heritage_implements"] != "" {
		t.Errorf("unexpected heritage_implements = %q", nodes[0].Metadata["heritage_implements"])
	}
}

// --- Kotlin heritage tests ---

func TestKotlinParser_Heritage(t *testing.T) {
	src := `
interface Greetable {
    fun greet(): String
}

class Greeter : Greetable {
    override fun greet(): String = "Hello"
}
`
	g := graph.New("test")
	if err := parser.NewKotlinParser().Parse(g, "Greeter.kt", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Greeter")
	if len(nodes) == 0 {
		t.Fatal("class 'Greeter' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi == "" {
		t.Error("heritage_implements is empty, expected Greetable")
	}
}

func TestKotlinParser_HeritageNoInheritance(t *testing.T) {
	src := `class Plain {}`
	g := graph.New("test")
	if err := parser.NewKotlinParser().Parse(g, "Plain.kt", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Plain")
	if len(nodes) == 0 {
		t.Fatal("class 'Plain' not found")
	}
	if nodes[0].Metadata["heritage_implements"] != "" {
		t.Errorf("unexpected heritage_implements = %q", nodes[0].Metadata["heritage_implements"])
	}
}

// --- Existing Java source already has "implements" — verify ---

func TestJavaParser_ExistingSourceHeritage(t *testing.T) {
	// Mirrors the existing javaSource from languages_test.go.
	src := `
package com.example.auth;

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
`
	g := graph.New("test")
	if err := parser.NewJavaParser().Parse(g, "AuthService.java", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("AuthService")
	if len(nodes) == 0 {
		t.Fatal("class 'AuthService' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi != "Authenticator" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Authenticator")
	}
}
