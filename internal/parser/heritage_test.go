package parser_test

import (
	"strings"
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

func TestKotlinParser_HeritageConstructorInvocation(t *testing.T) {
	// Common Kotlin pattern: class extends via constructor invocation (parens).
	src := `
open class Base

class Child : Base() {
}
`
	g := graph.New("test")
	if err := parser.NewKotlinParser().Parse(g, "Child.kt", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Child")
	if len(nodes) == 0 {
		t.Fatal("class 'Child' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi != "Base" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Base")
	}
}

func TestKotlinParser_HeritageMultiple(t *testing.T) {
	// Class extends base + implements interface.
	src := `
open class Base
interface Loggable

class Service : Base(), Loggable {
}
`
	g := graph.New("test")
	if err := parser.NewKotlinParser().Parse(g, "Service.kt", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Service")
	if len(nodes) == 0 {
		t.Fatal("class 'Service' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi == "" {
		t.Fatal("heritage_implements is empty")
	}
	// Should contain both Base and Loggable.
	if !containsStr(hi, "Base") {
		t.Errorf("heritage_implements = %q, missing Base", hi)
	}
	if !containsStr(hi, "Loggable") {
		t.Errorf("heritage_implements = %q, missing Loggable", hi)
	}
}

func containsStr(csv, target string) bool {
	for _, s := range strings.Split(csv, ",") {
		if strings.TrimSpace(s) == target {
			return true
		}
	}
	return false
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

func TestKotlinParser_HeritageGeneric(t *testing.T) {
	src := `
interface Comparable<T>

class User : Comparable<User> {
}
`
	g := graph.New("test")
	if err := parser.NewKotlinParser().Parse(g, "User.kt", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("User")
	if len(nodes) == 0 {
		t.Fatal("class 'User' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	// Should be "Comparable", NOT "Comparable,User" (type arg shouldn't leak).
	if hi != "Comparable" {
		t.Errorf("heritage_implements = %q, want %q (generic type arg should not leak)", hi, "Comparable")
	}
}

func TestJavaParser_HeritageGenericExtends(t *testing.T) {
	src := `
public class Base<T> {}

public class Child extends Base<String> {
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
	// Should be "Base", NOT "Base,String".
	if he != "Base" {
		t.Errorf("heritage_extends = %q, want %q (generic type arg should not leak)", he, "Base")
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

// --- Python heritage tests ---

func TestPythonParser_HeritageExtends(t *testing.T) {
	src := `
class Base:
    pass

class Child(Base):
    pass

class Multi(Base, Mixin):
    pass

class Dotted(module.Parent):
    pass

class Builtin(object):
    pass
`
	g := graph.New("test")
	if err := parser.NewPythonParser().Parse(g, "test.py", []byte(src)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		want string
	}{
		{"Child", "Base"},
		{"Multi", "Base,Mixin"},
		{"Dotted", "Parent"},
		{"Builtin", ""},     // object is filtered out
		{"Base", ""},        // no parent
	}
	for _, tt := range tests {
		nodes := g.FindByName(tt.name)
		if len(nodes) == 0 {
			t.Errorf("class %q not found", tt.name)
			continue
		}
		got := nodes[0].Metadata["heritage_extends"]
		if got != tt.want {
			t.Errorf("class %s: heritage_extends = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// --- Ruby heritage tests ---

func TestRubyParser_HeritageExtends(t *testing.T) {
	src := `
class Handler < BaseHandler
  def process
  end
end

class Simple
  def go
  end
end
`
	g := graph.New("test")
	if err := parser.NewRubyParser().Parse(g, "test.rb", []byte(src)); err != nil {
		t.Fatal(err)
	}

	nodes := g.FindByName("Handler")
	if len(nodes) == 0 {
		t.Fatal("class 'Handler' not found")
	}
	he := nodes[0].Metadata["heritage_extends"]
	if he != "BaseHandler" {
		t.Errorf("Handler: heritage_extends = %q, want %q", he, "BaseHandler")
	}

	nodes = g.FindByName("Simple")
	if len(nodes) == 0 {
		t.Fatal("class 'Simple' not found")
	}
	he = nodes[0].Metadata["heritage_extends"]
	if he != "" {
		t.Errorf("Simple: heritage_extends = %q, want empty", he)
	}
}

// --- Rust scoped use list tests ---

func TestRustParser_ScopedUseList(t *testing.T) {
	src := `
use crate::{body, extract};
use self::future;
use std::io;

fn main() {}
`
	g := graph.New("test")
	p := parser.NewRustParser()
	if err := p.Parse(g, "test.rs", []byte(src)); err != nil {
		t.Fatal(err)
	}

	wantImports := []string{"crate::body", "crate::extract", "self::future", "std::io"}
	for _, want := range wantImports {
		found := false
		for _, n := range g.AllNodes() {
			if n.Name == want {
				found = true
				break
			}
		}
		if !found {
			// Check if it exists with different qualification
			var names []string
			for _, n := range g.AllNodes() {
				if n.Type == "package" {
					names = append(names, n.Name)
				}
			}
			t.Errorf("import %q not found; got packages: %v", want, names)
		}
	}
}

// --- PHP heritage tests ---

func TestPHPParser_HeritageExtends(t *testing.T) {
	src := `<?php
class ServiceProvider {}
class EventServiceProvider extends ServiceProvider {}
`
	g := graph.New("test")
	if err := parser.NewPHPParser().Parse(g, "test.php", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("EventServiceProvider")
	if len(nodes) == 0 {
		t.Fatal("class 'EventServiceProvider' not found")
	}
	he := nodes[0].Metadata["heritage_extends"]
	if he != "ServiceProvider" {
		t.Errorf("heritage_extends = %q, want %q", he, "ServiceProvider")
	}

	// Base class should have no heritage
	nodes = g.FindByName("ServiceProvider")
	if len(nodes) == 0 {
		t.Fatal("class 'ServiceProvider' not found")
	}
	if he := nodes[0].Metadata["heritage_extends"]; he != "" {
		t.Errorf("ServiceProvider: heritage_extends = %q, want empty", he)
	}
}

func TestPHPParser_HeritageImplements(t *testing.T) {
	src := `<?php
interface Loggable {}
interface Cacheable {}
class UserService implements Loggable, Cacheable {}
`
	g := graph.New("test")
	if err := parser.NewPHPParser().Parse(g, "test.php", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("UserService")
	if len(nodes) == 0 {
		t.Fatal("class 'UserService' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if !strings.Contains(hi, "Loggable") || !strings.Contains(hi, "Cacheable") {
		t.Errorf("heritage_implements = %q, want to contain Loggable and Cacheable", hi)
	}
}

func TestPHPParser_HeritageExtendsAndImplements(t *testing.T) {
	src := `<?php
class Base {}
interface Serializable {}
class Child extends Base implements Serializable {}
`
	g := graph.New("test")
	if err := parser.NewPHPParser().Parse(g, "test.php", []byte(src)); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("Child")
	if len(nodes) == 0 {
		t.Fatal("class 'Child' not found")
	}
	he := nodes[0].Metadata["heritage_extends"]
	hi := nodes[0].Metadata["heritage_implements"]
	if he != "Base" {
		t.Errorf("heritage_extends = %q, want %q", he, "Base")
	}
	if hi != "Serializable" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Serializable")
	}
}
