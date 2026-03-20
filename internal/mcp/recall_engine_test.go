package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── quadRecallSearch integration tests ────────────────────────────────────────

func TestQuadRecallSearch_BM25Channel(t *testing.T) {
	srv := newTestServer(t)

	// Insert a memory that matches "auth".
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierEntity,
		Content: "AuthService handles token validation",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	mems, attr := srv.quadRecallSearch(context.Background(), "auth token", 5, false, 7)
	if len(mems) == 0 {
		t.Fatal("expected at least 1 memory from BM25 channel")
	}
	// Should be attributed to bm25 and/or temporal.
	if attr == nil {
		t.Fatal("expected non-nil attribution")
	}
	found := false
	for _, m := range mems {
		if m.Content == "AuthService handles token validation" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected to find auth memory in results")
	}
}

func TestQuadRecallSearch_TemporalChannel(t *testing.T) {
	srv := newTestServer(t)

	// Insert a recent memory with content that does NOT match query text.
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "deployment pipeline was updated to use Docker",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Query for "auth" — BM25 won't find "Docker" memory, but temporal should.
	mems, _ := srv.quadRecallSearch(context.Background(), "auth changes", 10, false, 7)

	// Temporal channel returns recent memories regardless of text match.
	foundDocker := false
	for _, m := range mems {
		if m.Content == "deployment pipeline was updated to use Docker" {
			foundDocker = true
		}
	}
	if !foundDocker {
		t.Error("expected temporal channel to surface recent non-matching memory")
	}
}

func TestQuadRecallSearch_TemporalDoesNotOverwhelmRelevant(t *testing.T) {
	srv := newTestServer(t)

	// Insert 2 relevant memories (match "auth").
	for _, content := range []string{
		"AuthService refactored for OAuth2 support",
		"auth middleware now validates JWT expiry correctly",
	} {
		_, _ = srv.store.InsertMemory(store.Memory{
			Tier: store.TierProject, Content: content,
			AgentID: "agent-1", Source: store.SourceManual,
		})
	}

	// Insert 10 irrelevant recent memories (no text overlap with "auth").
	for i := 0; i < 10; i++ {
		content := fmt.Sprintf("unrelated topic %d about infrastructure deployment pipeline number %d", i, i*100)
		_, _ = srv.store.InsertMemory(store.Memory{
			Tier: store.TierProject, Content: content,
			AgentID: "agent-1", Source: store.SourceManual,
		})
	}

	// Query for "auth" with limit=5.
	mems, _ := srv.quadRecallSearch(context.Background(), "auth OAuth middleware", 5, false, 7)

	// The 2 auth-relevant memories should rank in the top 3 (multi-channel boost).
	authCount := 0
	for i, m := range mems {
		if i >= 3 {
			break
		}
		if strings.Contains(m.Content, "auth") || strings.Contains(m.Content, "Auth") ||
			strings.Contains(m.Content, "OAuth") || strings.Contains(m.Content, "JWT") {
			authCount++
		}
	}
	if authCount < 2 {
		t.Errorf("expected at least 2 auth-relevant memories in top 3, got %d", authCount)
		for i, m := range mems {
			t.Logf("  rank %d: %s", i+1, m.Content)
		}
	}
}

func TestQuadRecallSearch_GraphChannel(t *testing.T) {
	srv := newTestServer(t)

	// Build a small graph: AuthLogin → TokenValidator (CALLS edge).
	authID := srv.graph.MakeNodeID("pkg/auth.go", "AuthLogin")
	srv.graph.AddNode(&graph.Node{
		ID:   authID,
		Name: "AuthLogin",
		Type: graph.NodeFunction,
		File: "pkg/auth.go",
		Line: 10,
	})
	tokID := srv.graph.MakeNodeID("pkg/auth.go", "TokenValidator")
	srv.graph.AddNode(&graph.Node{
		ID:   tokID,
		Name: "TokenValidator",
		Type: graph.NodeFunction,
		File: "pkg/auth.go",
		Line: 50,
	})
	srv.graph.AddEdge(&graph.Edge{
		From: authID,
		To:   tokID,
		Type: graph.EdgeCalls,
	})

	// Insert memory anchored to AuthLogin (matches "auth" query).
	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierEntity,
		Content: "auth login was refactored for OAuth2",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	}, []string{string(authID)})

	// Insert memory anchored to TokenValidator (no text match for "auth").
	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier:    store.TierEntity,
		Content: "switched to RS256 for JWT signing",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	}, []string{string(tokID)})

	mems, attr := srv.quadRecallSearch(context.Background(), "auth login", 10, false, 7)

	// Graph channel should find the TokenValidator memory via BFS from AuthLogin.
	foundJWT := false
	for _, m := range mems {
		if m.Content == "switched to RS256 for JWT signing" {
			foundJWT = true
			// Check it came from graph channel.
			channels := attr[m.ID]
			hasGraph := false
			for _, ch := range channels {
				if ch == "graph" {
					hasGraph = true
				}
			}
			if !hasGraph {
				t.Error("JWT memory should be attributed to graph channel")
			}
		}
	}
	if !foundJWT {
		t.Error("expected graph channel to find structurally-related JWT memory")
	}
}

func TestQuadRecallSearch_EmptyResults(t *testing.T) {
	srv := newTestServer(t)

	mems, attr := srv.quadRecallSearch(context.Background(), "nonexistent query", 5, false, 7)
	// Temporal channel may return recent memories, but store is empty.
	if len(mems) != 0 {
		t.Errorf("expected 0 memories from empty store, got %d", len(mems))
	}
	if attr != nil && len(attr) > 0 {
		t.Errorf("expected nil/empty attribution, got %v", attr)
	}
}

func TestQuadRecallSearch_NoGraph_GracefulDegradation(t *testing.T) {
	// Create a server without a graph (knowledge mode).
	st := openMCPTestStore(t)
	srv := &Server{store: st}

	_, _ = st.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "project uses GraphQL for API layer",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})

	mems, _ := srv.quadRecallSearch(context.Background(), "GraphQL", 5, false, 7)
	if len(mems) == 0 {
		t.Error("expected BM25/temporal results even without graph")
	}
}

// ── graphBFS tests ────────────────────────────────────────────────────────────

func TestGraphBFS_TwoHops(t *testing.T) {
	srv := newTestServer(t)

	// A → B → C chain.
	aID := srv.graph.MakeNodeID("a.go", "FuncA")
	bID := srv.graph.MakeNodeID("b.go", "FuncB")
	cID := srv.graph.MakeNodeID("c.go", "FuncC")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "FuncA", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "FuncB", Type: graph.NodeFunction, File: "b.go", Line: 1},
		{ID: cID, Name: "FuncC", Type: graph.NodeFunction, File: "c.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 2)
	// Should find B (depth 1) and C (depth 2), but not A (seed excluded).
	found := make(map[string]bool)
	for _, id := range result {
		found[id] = true
	}
	if !found[string(bID)] {
		t.Error("expected FuncB at depth 1")
	}
	if !found[string(cID)] {
		t.Error("expected FuncC at depth 2")
	}
	if found[string(aID)] {
		t.Error("seed node should be excluded from results")
	}
}

func TestGraphBFS_RespectsEdgeTypeFilter(t *testing.T) {
	srv := newTestServer(t)

	aID := srv.graph.MakeNodeID("a.go", "FuncA")
	bID := srv.graph.MakeNodeID("b.go", "FuncB")
	cID := srv.graph.MakeNodeID("c.go", "FuncC")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "FuncA", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "FuncB", Type: graph.NodeFunction, File: "b.go", Line: 1},
		{ID: cID, Name: "FuncC", Type: graph.NodeFunction, File: "c.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	// A → B via CALLS (allowed), B → C via DEFINES (not allowed at any depth).
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeDefines})

	result := srv.graphBFS([]string{string(aID)}, 2)
	found := make(map[string]bool)
	for _, id := range result {
		found[id] = true
	}
	if !found[string(bID)] {
		t.Error("expected FuncB via CALLS edge")
	}
	if found[string(cID)] {
		t.Error("FuncC should NOT be reachable via DEFINES edge")
	}
}

func TestGraphBFS_Depth2OnlyCallsEdges(t *testing.T) {
	srv := newTestServer(t)

	aID := srv.graph.MakeNodeID("a.go", "FuncA")
	bID := srv.graph.MakeNodeID("b.go", "FuncB")
	cID := srv.graph.MakeNodeID("c.go", "FuncC")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "FuncA", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "FuncB", Type: graph.NodeFunction, File: "b.go", Line: 1},
		{ID: cID, Name: "FuncC", Type: graph.NodeFunction, File: "c.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	// A → B via IMPORTS (allowed at depth 1), B → C via IMPORTS (NOT allowed at depth 2).
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeImports})
	srv.graph.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeImports})

	result := srv.graphBFS([]string{string(aID)}, 2)
	found := make(map[string]bool)
	for _, id := range result {
		found[id] = true
	}
	if !found[string(bID)] {
		t.Error("expected FuncB via IMPORTS at depth 1")
	}
	if found[string(cID)] {
		t.Error("FuncC should NOT be reachable via IMPORTS at depth 2 (only CALLS allowed)")
	}
}

func TestGraphBFS_MaxNodesCap(t *testing.T) {
	srv := newTestServer(t)

	// Create a star graph: hub → 600 spokes. Cap is 500.
	hubID := srv.graph.MakeNodeID("hub.go", "Hub")
	srv.graph.AddNode(&graph.Node{ID: hubID, Name: "Hub", Type: graph.NodeFunction, File: "hub.go", Line: 1})

	for i := 0; i < 600; i++ {
		name := "Spoke" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		nID := srv.graph.MakeNodeID("spoke.go", name)
		srv.graph.AddNode(&graph.Node{ID: nID, Name: name, Type: graph.NodeFunction, File: "spoke.go", Line: i})
		srv.graph.AddEdge(&graph.Edge{From: hubID, To: nID, Type: graph.EdgeCalls})
	}

	result := srv.graphBFS([]string{string(hubID)}, 1)
	// Should be capped at 500 - 1 (seed excluded) = 499 max.
	if len(result) > 500 {
		t.Errorf("BFS returned %d nodes, expected <= 500", len(result))
	}
}

func TestGraphBFS_InvalidSeedIgnored(t *testing.T) {
	srv := newTestServer(t)
	result := srv.graphBFS([]string{"nonexistent::node"}, 2)
	if len(result) != 0 {
		t.Errorf("invalid seed should return empty, got %d", len(result))
	}
}

func TestGraphBFS_FollowsIncomingEdges(t *testing.T) {
	srv := newTestServer(t)

	aID := srv.graph.MakeNodeID("a.go", "Caller")
	bID := srv.graph.MakeNodeID("b.go", "Target")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "Caller", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "Target", Type: graph.NodeFunction, File: "b.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	// Caller → Target. BFS from Target should find Caller via incoming edge.
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(bID)}, 1)
	found := false
	for _, id := range result {
		if id == string(aID) {
			found = true
		}
	}
	if !found {
		t.Error("expected Caller found via incoming CALLS edge")
	}
}

// ── handleRecall integration: quad-channel produces results ───────────────────

func TestHandleRecall_QuadChannel_Integration(t *testing.T) {
	srv := newTestServer(t)

	// Insert test data.
	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "migration to PostgreSQL completed successfully",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})
	_, _ = srv.store.RememberEpisode(store.Episode{
		AgentID:   "agent-1",
		Decision:  "decided to use PostgreSQL for persistence",
		Rationale: "better JSON support",
	})

	ctx := context.Background()
	res, err := srv.handleRecall(ctx, callTool(map[string]any{
		"query": "PostgreSQL migration",
	}))
	if err != nil {
		t.Fatal(err)
	}

	// Parse response.
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text = tc.Text
		}
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// Should have both episodes and memories.
	episodes, _ := resp["episodes"].([]any)
	memories, _ := resp["memories"].([]any)
	if len(episodes) == 0 {
		t.Error("expected at least 1 episode")
	}
	if len(memories) == 0 {
		t.Error("expected at least 1 memory from quad-channel")
	}
}

func TestHandleRecall_BrowseMode_Unchanged(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "browse mode test memory",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})

	ctx := context.Background()
	// Empty query = browse mode, should NOT use quad-channel.
	res, err := srv.handleRecall(ctx, callTool(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}

	var text string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text = tc.Text
		}
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	mode, _ := resp["mode"].(string)
	if mode != "browse" {
		t.Errorf("empty query should be browse mode, got %q", mode)
	}
}
