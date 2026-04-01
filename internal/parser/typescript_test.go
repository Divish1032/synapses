package parser_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

const tsSource = `
import { useState } from 'react';
import type { User } from './types';

export interface AuthService {
  login(user: string): Promise<void>;
  logout(): void;
}

export type AuthResult = {
  token: string;
  user: User;
};

export class AuthClient implements AuthService {
  async login(user: string): Promise<void> {}
  logout(): void {}
}

export function createAuthClient(): AuthClient {
  return new AuthClient();
}
`

const tsxSource = `
import React from 'react';
import { AuthClient } from './auth';

export interface LoginProps {
  onSuccess: () => void;
}

export const LoginForm: React.FC<LoginProps> = ({ onSuccess }) => {
  const client = new AuthClient();
  return <div />;
};

export function LoginPage() {
  return <LoginForm onSuccess={() => {}} />;
}
`

func parseTS(t *testing.T, filePath, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, filePath, []byte(src)); err != nil {
		t.Fatalf("Parse(%s) error: %v", filePath, err)
	}
	return g
}

func TestTypeScriptParser_Extensions(t *testing.T) {
	p := parser.NewTypeScriptParser()
	exts := p.Extensions()
	has := func(want string) bool {
		for _, e := range exts {
			if e == want {
				return true
			}
		}
		return false
	}
	if !has(".ts") {
		t.Error("Extensions() missing .ts")
	}
	if !has(".tsx") {
		t.Error("Extensions() missing .tsx")
	}
}

func TestTypeScriptParser_FileNode(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)
	nodes := g.FindByName("service.ts")
	if len(nodes) == 0 {
		t.Fatal("file node 'service.ts' not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

func TestTypeScriptParser_ExtractsImports(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)

	reactNodes := g.FindByPattern("react")
	if len(reactNodes) == 0 {
		t.Error("import 'react' not found")
	}
	typesNodes := g.FindByPattern("types")
	if len(typesNodes) == 0 {
		t.Error("import './types' not found")
	}
}

func TestTypeScriptParser_ExtractsInterface(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)
	nodes := g.FindByName("AuthService")
	if len(nodes) == 0 {
		t.Fatal("interface 'AuthService' not found")
	}
	if nodes[0].Type != graph.NodeInterface {
		t.Errorf("type = %q, want NodeInterface", nodes[0].Type)
	}
	if !nodes[0].Exported {
		t.Error("AuthService should be marked exported")
	}
}

func TestTypeScriptParser_ExtractsTypeAlias(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)
	nodes := g.FindByName("AuthResult")
	if len(nodes) == 0 {
		t.Fatal("type alias 'AuthResult' not found")
	}
	if nodes[0].Type != graph.NodeInterface {
		t.Errorf("type alias type = %q, want NodeInterface", nodes[0].Type)
	}
}

func TestTypeScriptParser_ExtractsClass(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)
	nodes := g.FindByName("AuthClient")
	if len(nodes) == 0 {
		t.Fatal("class 'AuthClient' not found")
	}
	if nodes[0].Type != graph.NodeStruct {
		t.Errorf("class type = %q, want NodeStruct", nodes[0].Type)
	}
	if !nodes[0].Exported {
		t.Error("AuthClient should be marked exported")
	}
}

func TestTypeScriptParser_ExtractsFunctions(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)
	nodes := g.FindByName("createAuthClient")
	if len(nodes) == 0 {
		t.Fatal("function 'createAuthClient' not found")
	}
	if nodes[0].Type != graph.NodeFunction {
		t.Errorf("type = %q, want NodeFunction", nodes[0].Type)
	}
}

func TestTypeScriptParser_TSX_ExtractsArrowFunction(t *testing.T) {
	g := parseTS(t, "components/login.tsx", tsxSource)
	// LoginForm is an exported arrow function.
	nodes := g.FindByName("LoginForm")
	if len(nodes) == 0 {
		t.Fatal("arrow function 'LoginForm' not found in TSX")
	}
	if nodes[0].Type != graph.NodeFunction {
		t.Errorf("type = %q, want NodeFunction", nodes[0].Type)
	}
	if !nodes[0].Exported {
		t.Error("LoginForm should be marked exported")
	}
}

func TestTypeScriptParser_TSX_ExtractsRegularFunction(t *testing.T) {
	g := parseTS(t, "components/login.tsx", tsxSource)
	nodes := g.FindByName("LoginPage")
	if len(nodes) == 0 {
		t.Fatal("function 'LoginPage' not found in TSX")
	}
}

func TestTypeScriptParser_TSX_ExtractsInterface(t *testing.T) {
	g := parseTS(t, "components/login.tsx", tsxSource)
	nodes := g.FindByName("LoginProps")
	if len(nodes) == 0 {
		t.Fatal("interface 'LoginProps' not found in TSX")
	}
	if nodes[0].Type != graph.NodeInterface {
		t.Errorf("type = %q, want NodeInterface", nodes[0].Type)
	}
}

func TestTypeScriptParser_DefinesEdges(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)

	fileNodes := g.FindByName("service.ts")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	outEdges := g.OutEdges(fileNodes[0].ID)

	targets := make(map[string]bool)
	for _, e := range outEdges {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil {
				targets[n.Name] = true
			}
		}
	}

	for _, expected := range []string{"AuthService", "AuthResult", "AuthClient", "createAuthClient"} {
		if !targets[expected] {
			t.Errorf("missing DEFINES edge from file to %q", expected)
		}
	}
}

func TestTypeScriptParser_ImportEdges(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)

	fileNodes := g.FindByName("service.ts")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	outEdges := g.OutEdges(fileNodes[0].ID)

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

func TestTypeScriptParser_EmptyFile(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewTypeScriptParser()
	if err := p.Parse(g, "empty.ts", []byte("")); err != nil {
		t.Fatalf("Parse() on empty file returned error: %v", err)
	}
	// At minimum a file node should be created.
	if g.NodeCount() == 0 {
		t.Error("empty file produced no nodes")
	}
}

func TestTypeScriptParser_HeritageImplements(t *testing.T) {
	// The existing tsSource has: class AuthClient implements AuthService
	g := parseTS(t, "auth/service.ts", tsSource)
	nodes := g.FindByName("AuthClient")
	if len(nodes) == 0 {
		t.Fatal("class 'AuthClient' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi != "AuthService" {
		t.Errorf("heritage_implements = %q, want %q", hi, "AuthService")
	}
}

func TestTypeScriptParser_HeritageExtends(t *testing.T) {
	src := `
class Base {
  greet(): string { return "hi"; }
}

class Child extends Base {
  wave(): void {}
}
`
	g := parseTS(t, "app.ts", src)
	nodes := g.FindByName("Child")
	if len(nodes) == 0 {
		t.Fatal("class 'Child' not found")
	}
	he := nodes[0].Metadata["heritage_extends"]
	if he != "Base" {
		t.Errorf("heritage_extends = %q, want %q", he, "Base")
	}
}

func TestTypeScriptParser_HeritageExtendsAndImplements(t *testing.T) {
	src := `
interface Loggable {
  log(): void;
}

class Base {}

class Service extends Base implements Loggable {
  log(): void {}
}
`
	g := parseTS(t, "app.ts", src)
	nodes := g.FindByName("Service")
	if len(nodes) == 0 {
		t.Fatal("class 'Service' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	he := nodes[0].Metadata["heritage_extends"]
	if hi != "Loggable" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Loggable")
	}
	if he != "Base" {
		t.Errorf("heritage_extends = %q, want %q", he, "Base")
	}
}

func TestTypeScriptParser_HeritageMultipleImplements(t *testing.T) {
	src := `
interface Readable { read(): void; }
interface Writable { write(): void; }

class Stream implements Readable, Writable {
  read(): void {}
  write(): void {}
}
`
	g := parseTS(t, "stream.ts", src)
	nodes := g.FindByName("Stream")
	if len(nodes) == 0 {
		t.Fatal("class 'Stream' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi != "Readable,Writable" {
		t.Errorf("heritage_implements = %q, want %q", hi, "Readable,Writable")
	}
}

func TestTypeScriptParser_HeritageAbstractClass(t *testing.T) {
	src := `
abstract class Base {
  abstract greet(): string;
}

class Impl extends Base {
  greet(): string { return "hi"; }
}
`
	g := parseTS(t, "app.ts", src)
	nodes := g.FindByName("Impl")
	if len(nodes) == 0 {
		t.Fatal("class 'Impl' not found")
	}
	he := nodes[0].Metadata["heritage_extends"]
	if he != "Base" {
		t.Errorf("heritage_extends = %q, want %q", he, "Base")
	}
}

func TestTypeScriptParser_NoHeritage(t *testing.T) {
	src := `class Standalone { foo(): void {} }`
	g := parseTS(t, "app.ts", src)
	nodes := g.FindByName("Standalone")
	if len(nodes) == 0 {
		t.Fatal("class 'Standalone' not found")
	}
	if nodes[0].Metadata["heritage_implements"] != "" {
		t.Errorf("unexpected heritage_implements = %q", nodes[0].Metadata["heritage_implements"])
	}
	if nodes[0].Metadata["heritage_extends"] != "" {
		t.Errorf("unexpected heritage_extends = %q", nodes[0].Metadata["heritage_extends"])
	}
}

func TestTypeScriptParser_HeritageGenericType(t *testing.T) {
	src := `
interface Comparable<T> { compareTo(other: T): number; }

class User implements Comparable<User> {
  compareTo(other: User): number { return 0; }
}
`
	g := parseTS(t, "app.ts", src)
	nodes := g.FindByName("User")
	if len(nodes) == 0 {
		t.Fatal("class 'User' not found")
	}
	hi := nodes[0].Metadata["heritage_implements"]
	if hi != "Comparable" {
		t.Errorf("heritage_implements = %q, want %q (generic stripped)", hi, "Comparable")
	}
}

// --- Sprint 23.7: entity signature extraction tests ---

func TestTypeScriptParser_ClassSignature(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)
	nodes := g.FindByName("AuthClient")
	if len(nodes) == 0 {
		t.Fatal("AuthClient class not found")
	}
	var classNode *graph.Node
	for i := range nodes {
		if nodes[i].Type == graph.NodeStruct {
			classNode = nodes[i]
			break
		}
	}
	if classNode == nil {
		t.Fatal("AuthClient should be a NodeStruct")
	}
	sig := classNode.Metadata["signature"]
	if sig == "" {
		t.Fatal("AuthClient should have a signature")
	}
	if !strings.Contains(sig, "AuthClient") || !strings.Contains(sig, "implements") {
		t.Errorf("class signature %q should contain 'AuthClient' and 'implements'", sig)
	}
}

func TestTypeScriptParser_InterfaceSignature(t *testing.T) {
	g := parseTS(t, "auth/service.ts", tsSource)
	nodes := g.FindByName("AuthService")
	if len(nodes) == 0 {
		t.Fatal("AuthService interface not found")
	}
	var ifaceNode *graph.Node
	for i := range nodes {
		if nodes[i].Type == graph.NodeInterface {
			ifaceNode = nodes[i]
			break
		}
	}
	if ifaceNode == nil {
		t.Fatal("AuthService should be a NodeInterface")
	}
	sig := ifaceNode.Metadata["signature"]
	if sig == "" {
		t.Fatal("AuthService interface should have a signature")
	}
	if !strings.Contains(sig, "AuthService") {
		t.Errorf("interface signature %q should contain 'AuthService'", sig)
	}
}
