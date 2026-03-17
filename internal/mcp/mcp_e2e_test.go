package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestE2E_SessionInitReturnsValidJSON(t *testing.T) {
	g := graph.New("test-repo")
	srv := New(g, &config.Config{}, nil)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := srv.handleSessionInit(context.Background(), req)
	if err != nil {
		t.Fatalf("session_init error: %v", err)
	}

	text := result.Content[0].(mcp.TextContent).Text

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v\nraw: %s", err, text)
	}

	for _, key := range []string{"project_identity", "working_state", "scale_guidance"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("missing top-level key %q in session_init response", key)
		}
	}
}

func TestE2E_SessionInitThenGetContext(t *testing.T) {
	g := graph.New("test-repo")

	authSvc := &graph.Node{
		ID:      "test-repo::internal/auth/service.go::AuthService",
		Name:    "AuthService",
		Type:    graph.NodeFunction,
		File:    "internal/auth/service.go",
		Package: "internal/auth",
	}
	validateToken := &graph.Node{
		ID:      "test-repo::internal/auth/token.go::ValidateToken",
		Name:    "ValidateToken",
		Type:    graph.NodeFunction,
		File:    "internal/auth/token.go",
		Package: "internal/auth",
	}
	g.AddNode(authSvc)
	g.AddNode(validateToken)
	g.AddEdge(&graph.Edge{From: authSvc.ID, To: validateToken.ID, Type: graph.EdgeCalls})

	srv := New(g, &config.Config{}, nil)

	// Call session_init first (as protocol requires).
	initReq := mcp.CallToolRequest{}
	initReq.Params.Arguments = map[string]interface{}{}
	if _, err := srv.handleSessionInit(context.Background(), initReq); err != nil {
		t.Fatalf("session_init error: %v", err)
	}

	// Now call get_context for AuthService.
	ctxReq := mcp.CallToolRequest{}
	ctxReq.Params.Arguments = map[string]interface{}{"entity": "AuthService"}

	result, err := srv.handleGetContext(context.Background(), ctxReq)
	if err != nil {
		t.Fatalf("get_context error: %v", err)
	}

	text := result.Content[0].(mcp.TextContent).Text

	if !strings.Contains(text, "AuthService") {
		t.Errorf("get_context response should contain 'AuthService', got: %s", text)
	}
	if !strings.Contains(text, "ValidateToken") {
		t.Errorf("get_context response should contain neighbor 'ValidateToken', got: %s", text)
	}
}

func TestE2E_GetContextWithEdges(t *testing.T) {
	g := graph.New("test-repo")

	caller := &graph.Node{
		ID:   "test-repo::cmd/api/handler.go::HandleRequest",
		Name: "HandleRequest", Type: graph.NodeFunction,
		File: "cmd/api/handler.go", Package: "cmd/api",
	}
	middle := &graph.Node{
		ID:   "test-repo::internal/service/svc.go::ProcessData",
		Name: "ProcessData", Type: graph.NodeFunction,
		File: "internal/service/svc.go", Package: "internal/service",
	}
	callee := &graph.Node{
		ID:   "test-repo::internal/repo/store.go::SaveRecord",
		Name: "SaveRecord", Type: graph.NodeFunction,
		File: "internal/repo/store.go", Package: "internal/repo",
	}

	g.AddNode(caller)
	g.AddNode(middle)
	g.AddNode(callee)
	g.AddEdge(&graph.Edge{From: caller.ID, To: middle.ID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: middle.ID, To: callee.ID, Type: graph.EdgeCalls})

	srv := New(g, &config.Config{}, nil)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"entity": "ProcessData", "format": "json"}

	result, err := srv.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("get_context error: %v", err)
	}

	text := result.Content[0].(mcp.TextContent).Text

	if !strings.Contains(text, "HandleRequest") {
		t.Errorf("response should contain caller 'HandleRequest', got: %s", text)
	}
	if !strings.Contains(text, "SaveRecord") {
		t.Errorf("response should contain callee 'SaveRecord', got: %s", text)
	}
}
