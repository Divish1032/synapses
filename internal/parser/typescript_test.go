package parser_test

import (
	"testing"

	"github.com/synapses/synapses/internal/graph"
	"github.com/synapses/synapses/internal/parser"
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
