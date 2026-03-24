package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/skills"
)

func TestActivePrompts_InjectedInGetContext(t *testing.T) {
	g := graph.New("test-repo")
	n := graph.Node{
		ID:      "test-repo::internal/auth/auth.go::AuthService",
		Name:    "AuthService",
		Type:    graph.NodeFunction,
		File:    "internal/auth/auth.go",
		Package: "internal/auth",
	}
	g.AddNode(&n)

	srv := New(g, &config.Config{}, nil)
	t.Cleanup(func() { srv.Close() })
	srv.SetPromptTemplates([]skills.PromptTemplate{
		{ID: "go-guide", FilePattern: "**/*.go", Body: "Use fmt.Errorf wrapping.", Source: "builtin"},
		{ID: "ts-guide", FilePattern: "**/*.ts", Body: "TypeScript only.", Source: "builtin"},
	})

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"entity": "AuthService", "format": "json"}

	result, err := srv.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := result.Content[0].(mcp.TextContent).Text
	var dc directionalContext
	if err := json.Unmarshal([]byte(text), &dc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(dc.ActivePrompts) != 1 {
		t.Errorf("expected 1 active prompt (go-guide), got %d: %+v", len(dc.ActivePrompts), dc.ActivePrompts)
	}
	if dc.ActivePrompts[0].ID != "go-guide" {
		t.Errorf("wrong prompt ID: %q", dc.ActivePrompts[0].ID)
	}
	if !strings.Contains(dc.ActivePrompts[0].Body, "fmt.Errorf") {
		t.Errorf("prompt body missing expected text: %q", dc.ActivePrompts[0].Body)
	}
}

func TestActivePrompts_NoneWhenNoTemplates(t *testing.T) {
	g := graph.New("test-repo")
	g.AddNode(&graph.Node{
		ID: "test-repo::main.go::main", Name: "main",
		Type: graph.NodeFunction, File: "main.go",
	})

	srv := New(g, &config.Config{}, nil)
	t.Cleanup(func() { srv.Close() })
	// No SetPromptTemplates call

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"entity": "main", "format": "json"}

	result, err := srv.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	var dc directionalContext
	if err := json.Unmarshal([]byte(text), &dc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dc.ActivePrompts) != 0 {
		t.Errorf("expected no active prompts, got %d", len(dc.ActivePrompts))
	}
}

func TestAutoLoadPrompts_InSessionInit(t *testing.T) {
	g := graph.New("test-repo")
	srv := New(g, &config.Config{}, nil)
	t.Cleanup(func() { srv.Close() })
	srv.SetPromptTemplates([]skills.PromptTemplate{
		{ID: "project-wide", AutoLoad: true, Body: "Always use X.", Source: "project"},
		{ID: "entity-specific", FilePattern: "**/*.go", Body: "Go only.", Source: "builtin"},
	})

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}
	result, err := srv.handleSessionInit(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "project-wide") {
		t.Errorf("session_init should include auto-load prompt 'project-wide', got: %s", text[:200])
	}
	// entity-specific (no auto_load) should NOT appear
	if strings.Contains(text, "\"entity-specific\"") {
		t.Errorf("entity-specific prompt should not appear in session_init")
	}
}
