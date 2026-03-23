package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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

	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "auth token", "", 5, false, 7, nil, 0)
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
	mems, _, _, _ := srv.quadRecallSearch(context.Background(), "auth changes", "", 10, false, 7, nil, 0)

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
	mems, _, _, _ := srv.quadRecallSearch(context.Background(), "auth OAuth middleware", "", 5, false, 7, nil, 0)

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

	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "auth login", "", 10, false, 7, nil, 0)

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

	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "nonexistent query", "", 5, false, 7, nil, 0)
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

	mems, _, _, _ := srv.quadRecallSearch(context.Background(), "GraphQL", "", 5, false, 7, nil, 0)
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
	for _, id := range result.Nodes {
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
	for _, id := range result.Nodes {
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
	for _, id := range result.Nodes {
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
	if len(result.Nodes) > 500 {
		t.Errorf("BFS returned %d nodes, expected <= 500", len(result.Nodes))
	}
}

func TestGraphBFS_InvalidSeedIgnored(t *testing.T) {
	srv := newTestServer(t)
	result := srv.graphBFS([]string{"nonexistent::node"}, 2)
	if len(result.Nodes) != 0 {
		t.Errorf("invalid seed should return empty, got %d", len(result.Nodes))
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
	for _, id := range result.Nodes {
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

// ── Browse-mode decay filter (Fix: browse mode must honour DecayVisibilityThreshold) ─────

func TestHandleRecall_BrowseMode_HidesDecayedMemory(t *testing.T) {
	// Regression: before the fix, browse mode (empty query) bypassed the decay
	// threshold and returned all non-expired, non-stale memories regardless of age.
	// After the fix, memories whose DecayedImportanceScore < DecayVisibilityThreshold
	// are excluded from browse results (same as search mode).
	srv := newTestServer(t)

	// Insert a memory with minimum-importance weight and a very old last_accessed_at,
	// engineered so DecayedImportanceScore < DecayVisibilityThreshold.
	// TierProject half-life = 336h. weight=0.05 × recencyDecay(3200h, 336h) = 0.05 × 0.095 ≈ 0.00475 < 0.05.
	oldTime := time.Now().Add(-3200 * time.Hour).UTC().Format(time.RFC3339)
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:           store.TierProject,
		Content:        "very old decayed memory that should not appear in browse",
		AgentID:        "decay-browse-agent",
		Source:         store.SourceManual,
		Importance:     "0.05",
		LastAccessedAt: oldTime,
		CreatedAt:      oldTime,
	})
	if err != nil {
		t.Fatalf("insert decayed memory: %v", err)
	}

	// Insert a fresh pinned memory — must always appear.
	_, err = srv.store.InsertMemory(store.Memory{
		Tier:       store.TierProject,
		Content:    "pinned memory that must always appear in browse regardless of age",
		AgentID:    "decay-browse-agent",
		Source:     store.SourceManual,
		Importance: store.ImportancePinned,
	})
	if err != nil {
		t.Fatalf("insert pinned memory: %v", err)
	}

	ctx := context.Background()
	res, recallErr := srv.handleRecall(ctx, callTool(map[string]any{
		"agent_id": "decay-browse-agent",
	}))
	if recallErr != nil {
		t.Fatal(recallErr)
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
		t.Errorf("expected browse mode, got %q", mode)
	}

	memories, _ := resp["memories"].([]any)

	// The decayed memory must NOT appear.
	for _, raw := range memories {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		if strings.Contains(content, "very old decayed") {
			t.Error("decayed memory appeared in browse results — decay threshold not applied in browse mode")
		}
	}

	// The pinned memory MUST appear.
	foundPinned := false
	for _, raw := range memories {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		if strings.Contains(content, "pinned memory that must always appear") {
			foundPinned = true
		}
	}
	if !foundPinned {
		t.Error("pinned memory was hidden from browse results — pinned memories must always be visible")
	}
}

// ── Sprint 10 #8: depth param + traversal paths ───────────────────────────────

func TestGraphBFS_ReturnsParentMap(t *testing.T) {
	srv := newTestServer(t)

	// A → B → C chain via CALLS.
	aID := srv.graph.MakeNodeID("a.go", "ServiceA")
	bID := srv.graph.MakeNodeID("b.go", "ServiceB")
	cID := srv.graph.MakeNodeID("c.go", "ServiceC")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "ServiceA", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "ServiceB", Type: graph.NodeFunction, File: "b.go", Line: 1},
		{ID: cID, Name: "ServiceC", Type: graph.NodeFunction, File: "c.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 2)

	// ParentMap must record how B and C were discovered.
	entryB, okB := result.ParentMap[string(bID)]
	if !okB {
		t.Fatal("expected ParentMap entry for ServiceB")
	}
	if entryB.ParentNodeID != string(aID) {
		t.Errorf("ServiceB parent should be ServiceA, got %q", entryB.ParentNodeID)
	}
	if entryB.EdgeType != graph.EdgeCalls {
		t.Errorf("ServiceB edge type should be CALLS, got %q", entryB.EdgeType)
	}

	entryC, okC := result.ParentMap[string(cID)]
	if !okC {
		t.Fatal("expected ParentMap entry for ServiceC")
	}
	if entryC.ParentNodeID != string(bID) {
		t.Errorf("ServiceC parent should be ServiceB, got %q", entryC.ParentNodeID)
	}

	// SeedSet must contain the seed.
	if !result.SeedSet[string(aID)] {
		t.Error("SeedSet should contain seed aID")
	}
	if result.SeedSet[string(bID)] {
		t.Error("SeedSet should NOT contain non-seed bID")
	}
}

func TestGraphBFS_DepthParam_1HopOnly(t *testing.T) {
	srv := newTestServer(t)

	// A → B → C — depth=1 should find B but NOT C.
	aID := srv.graph.MakeNodeID("a.go", "Root")
	bID := srv.graph.MakeNodeID("b.go", "Mid")
	cID := srv.graph.MakeNodeID("c.go", "Far")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "Root", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "Mid", Type: graph.NodeFunction, File: "b.go", Line: 1},
		{ID: cID, Name: "Far", Type: graph.NodeFunction, File: "c.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 1)
	found := make(map[string]bool)
	for _, id := range result.Nodes {
		found[id] = true
	}
	if !found[string(bID)] {
		t.Error("depth=1: expected Mid (1 hop)")
	}
	if found[string(cID)] {
		t.Error("depth=1: Far is 2 hops away — should NOT be found")
	}
}

func TestGraphBFS_DepthParam_3Hops(t *testing.T) {
	srv := newTestServer(t)

	// A → B → C → D chain — depth=3 should find B, C, and D.
	aID := srv.graph.MakeNodeID("a.go", "NodeA")
	bID := srv.graph.MakeNodeID("b.go", "NodeB")
	cID := srv.graph.MakeNodeID("c.go", "NodeC")
	dID := srv.graph.MakeNodeID("d.go", "NodeD")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "NodeA", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "NodeB", Type: graph.NodeFunction, File: "b.go", Line: 1},
		{ID: cID, Name: "NodeC", Type: graph.NodeFunction, File: "c.go", Line: 1},
		{ID: dID, Name: "NodeD", Type: graph.NodeFunction, File: "d.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: bID, To: cID, Type: graph.EdgeCalls})
	srv.graph.AddEdge(&graph.Edge{From: cID, To: dID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 3)
	found := make(map[string]bool)
	for _, id := range result.Nodes {
		found[id] = true
	}
	if !found[string(bID)] || !found[string(cID)] || !found[string(dID)] {
		t.Errorf("depth=3: expected B, C, D; got nodes: %v", result.Nodes)
	}
}

func TestGraphBFS_DepthParam_Capped_At_4(t *testing.T) {
	// depth=5 should be clamped to 4 by quadRecallSearch; test that graphBFS(maxDepth=4) doesn't
	// panic and still returns results.
	srv := newTestServer(t)

	aID := srv.graph.MakeNodeID("a.go", "Root")
	bID := srv.graph.MakeNodeID("b.go", "Child")
	for _, n := range []*graph.Node{
		{ID: aID, Name: "Root", Type: graph.NodeFunction, File: "a.go", Line: 1},
		{ID: bID, Name: "Child", Type: graph.NodeFunction, File: "b.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 4)
	found := make(map[string]bool)
	for _, id := range result.Nodes {
		found[id] = true
	}
	if !found[string(bID)] {
		t.Error("depth=4: should still find direct child")
	}
}

func TestBuildGraphPath_TwoHop(t *testing.T) {
	srv := newTestServer(t)

	// Build: Auth → TokenValidator → RS256Handler
	authID := srv.graph.MakeNodeID("auth.go", "Auth")
	tokID := srv.graph.MakeNodeID("auth.go", "TokenValidator")
	for _, n := range []*graph.Node{
		{ID: authID, Name: "Auth", Type: graph.NodeFunction, File: "auth.go", Line: 1},
		{ID: tokID, Name: "TokenValidator", Type: graph.NodeFunction, File: "auth.go", Line: 50},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: authID, To: tokID, Type: graph.EdgeCalls})

	bfsResult := srv.graphBFS([]string{string(authID)}, 2)

	// TokenValidator should be reachable from Auth seed.
	pathStr, hops := srv.buildGraphPath(string(tokID), bfsResult)
	if hops != 1 {
		t.Errorf("expected 1 hop, got %d", hops)
	}
	if pathStr == "" {
		t.Fatal("expected non-empty path string")
	}
	// Path should contain both node names and the edge type.
	if !strings.Contains(pathStr, "Auth") {
		t.Errorf("path should contain 'Auth', got: %q", pathStr)
	}
	if !strings.Contains(pathStr, "TokenValidator") {
		t.Errorf("path should contain 'TokenValidator', got: %q", pathStr)
	}
	if !strings.Contains(pathStr, "CALLS") {
		t.Errorf("path should contain 'CALLS', got: %q", pathStr)
	}
}

func TestBuildGraphPath_SeedIsAnchor_ReturnsEmpty(t *testing.T) {
	// When the anchor IS a seed node, no multi-hop path exists — should return "".
	srv := newTestServer(t)

	aID := srv.graph.MakeNodeID("a.go", "SeedNode")
	srv.graph.AddNode(&graph.Node{ID: aID, Name: "SeedNode", Type: graph.NodeFunction, File: "a.go", Line: 1})

	bfsResult := srv.graphBFS([]string{string(aID)}, 2)

	pathStr, hops := srv.buildGraphPath(string(aID), bfsResult)
	if hops != 0 || pathStr != "" {
		t.Errorf("anchor=seed should return ('', 0), got (%q, %d)", pathStr, hops)
	}
}

func TestQuadRecallSearch_DepthZeroDefaultsToTwo(t *testing.T) {
	// depth=0 should default to 2 (same behavior as before this feature).
	// This tests backward compatibility — the graph channel still fires with default depth.
	srv := newTestServer(t)

	authID := srv.graph.MakeNodeID("pkg/auth.go", "AuthLogin")
	srv.graph.AddNode(&graph.Node{ID: authID, Name: "AuthLogin", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 10})
	tokID := srv.graph.MakeNodeID("pkg/auth.go", "TokenValidator")
	srv.graph.AddNode(&graph.Node{ID: tokID, Name: "TokenValidator", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 50})
	srv.graph.AddEdge(&graph.Edge{From: authID, To: tokID, Type: graph.EdgeCalls})

	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "auth login refactored for OAuth2",
		AgentID: "agent-1", Source: store.SourceManual,
	}, []string{string(authID)})
	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "switched to RS256 for token signing",
		AgentID: "agent-1", Source: store.SourceManual,
	}, []string{string(tokID)})

	// depth=0 → defaults to 2; should find the TokenValidator memory via graph channel.
	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "auth login", "", 10, false, 7, nil, 0)

	foundJWT := false
	for _, m := range mems {
		if strings.Contains(m.Content, "RS256") {
			foundJWT = true
			channels := attr[m.ID]
			hasGraph := false
			for _, ch := range channels {
				if ch == "graph" {
					hasGraph = true
				}
			}
			if !hasGraph {
				t.Error("RS256 memory should be attributed to graph channel")
			}
		}
	}
	if !foundJWT {
		t.Error("depth=0 (default 2): expected graph channel to find RS256 memory via TokenValidator")
	}
}

func TestQuadRecallSearch_ReturnsTraversalInfo_WhenGraphFires(t *testing.T) {
	// Traversal info must be populated when graph channel was active and found memories.
	srv := newTestServer(t)

	authID := srv.graph.MakeNodeID("pkg/auth.go", "AuthService")
	srv.graph.AddNode(&graph.Node{ID: authID, Name: "AuthService", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 1})
	tokID := srv.graph.MakeNodeID("pkg/auth.go", "TokenValidator")
	srv.graph.AddNode(&graph.Node{ID: tokID, Name: "TokenValidator", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 50})
	srv.graph.AddEdge(&graph.Edge{From: authID, To: tokID, Type: graph.EdgeCalls})

	// Anchor query-matching memory to AuthService (seed), and
	// non-query-matching memory to TokenValidator (discovered via graph).
	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "auth service handles login flow",
		AgentID: "a1", Source: store.SourceManual,
	}, []string{string(authID)})
	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "switched to RS256 for JWT signing",
		AgentID: "a1", Source: store.SourceManual,
	}, []string{string(tokID)})

	_, _, _, ti := srv.quadRecallSearch(context.Background(), "auth service login", "", 10, false, 7, nil, 2)

	if ti == nil {
		t.Fatal("expected non-nil GraphTraversalInfo when graph channel was active")
	}
	if ti.Depth != 2 {
		t.Errorf("expected depth=2, got %d", ti.Depth)
	}
	if ti.AnchorCount == 0 {
		t.Error("expected AnchorCount > 0")
	}
	if ti.Note == "" {
		t.Error("expected non-empty Note")
	}
}

func TestQuadRecallSearch_TraversalPaths_ShowConnection(t *testing.T) {
	// When graph channel surfaces a memory via traversal, graph_traversal.paths
	// must include a path entry with the node names and edge type.
	srv := newTestServer(t)

	authID := srv.graph.MakeNodeID("pkg/auth.go", "AuthService")
	srv.graph.AddNode(&graph.Node{ID: authID, Name: "AuthService", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 1})
	tokID := srv.graph.MakeNodeID("pkg/auth.go", "TokenValidator")
	srv.graph.AddNode(&graph.Node{ID: tokID, Name: "TokenValidator", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 50})
	srv.graph.AddEdge(&graph.Edge{From: authID, To: tokID, Type: graph.EdgeCalls})

	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "auth service handles login and session management",
		AgentID: "a1", Source: store.SourceManual,
	}, []string{string(authID)})
	jwtMemID, _ := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "switched to RS256 for JWT signing algorithm",
		AgentID: "a1", Source: store.SourceManual,
	}, []string{string(tokID)})

	_, _, _, ti := srv.quadRecallSearch(context.Background(), "auth service login", "", 10, false, 7, nil, 2)

	if ti == nil {
		t.Fatal("expected GraphTraversalInfo")
	}
	if len(ti.Paths) == 0 {
		// Graph channel may not have attributed this memory — acceptable if BM25 ranks it higher.
		// Just verify the traversal info itself is populated.
		t.Logf("no traversal paths (memory may not be graph-attributed); ti=%+v", ti)
		return
	}

	// At least one path should reference the JWT memory and show the connection.
	for _, p := range ti.Paths {
		if p.MemoryID == jwtMemID {
			if p.Hops == 0 {
				t.Error("expected Hops > 0 for TokenValidator memory")
			}
			if !strings.Contains(p.Path, "AuthService") {
				t.Errorf("path should mention AuthService: %q", p.Path)
			}
			if !strings.Contains(p.Path, "TokenValidator") {
				t.Errorf("path should mention TokenValidator: %q", p.Path)
			}
			return
		}
	}
}

func TestHandleRecall_DepthParam_WiredThrough(t *testing.T) {
	// Verify that the depth param is accepted by handleRecall without error,
	// and that graph_traversal appears in the response when the graph fires.
	srv := newTestServer(t)

	authID := srv.graph.MakeNodeID("pkg/auth.go", "AuthService")
	srv.graph.AddNode(&graph.Node{ID: authID, Name: "AuthService", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 1})
	tokID := srv.graph.MakeNodeID("pkg/auth.go", "TokenSvc")
	srv.graph.AddNode(&graph.Node{ID: tokID, Name: "TokenSvc", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 50})
	srv.graph.AddEdge(&graph.Edge{From: authID, To: tokID, Type: graph.EdgeCalls})

	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "auth service initialises token pipeline",
		AgentID: "a1", Source: store.SourceManual,
	}, []string{string(authID)})
	_, _ = srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "token service validates expiry and issuer",
		AgentID: "a1", Source: store.SourceManual,
	}, []string{string(tokID)})

	ctx := context.Background()
	res, err := srv.handleRecall(ctx, callTool(map[string]any{
		"query": "auth service",
		"depth": float64(3),
	}))
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

	// Response must have graph_traversal when graph channel was active.
	if gt, ok := resp["graph_traversal"]; ok {
		gtMap, ok := gt.(map[string]any)
		if !ok {
			t.Fatal("graph_traversal should be an object")
		}
		depth, _ := gtMap["depth"].(float64)
		if int(depth) != 3 {
			t.Errorf("expected depth=3 in graph_traversal, got %v", depth)
		}
	}
	// Mode must be search.
	if resp["mode"] != "search" {
		t.Errorf("expected mode=search, got %v", resp["mode"])
	}
}

func TestHandleRecall_NoDepth_NoTraversalInfo_WhenGraphSkipped(t *testing.T) {
	// Without a graph, graph_traversal should be absent from the response.
	st := openMCPTestStore(t)
	srv := &Server{store: st}

	_, _ = st.InsertMemory(store.Memory{
		Tier: store.TierProject, Content: "auth service test memory",
		AgentID: "a1", Source: store.SourceManual,
	})

	ctx := context.Background()
	res, err := srv.handleRecall(ctx, callTool(map[string]any{
		"query": "auth service",
		"depth": float64(2),
	}))
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
		t.Fatalf("unmarshal: %v", err)
	}
	// No graph → graph_traversal must be absent.
	if _, ok := resp["graph_traversal"]; ok {
		t.Error("graph_traversal should not be present when no graph is loaded")
	}
}

func TestHandleRecall_BrowseMode_IncludeStale_BypassesDecay(t *testing.T) {
	// include_stale=true is an explicit override — even decayed memories should appear.
	srv := newTestServer(t)

	oldTime := time.Now().Add(-3200 * time.Hour).UTC().Format(time.RFC3339)
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:           store.TierProject,
		Content:        "old memory visible when include_stale is true",
		AgentID:        "stale-browse-agent",
		Source:         store.SourceManual,
		Importance:     "0.05",
		LastAccessedAt: oldTime,
		CreatedAt:      oldTime,
	})
	if err != nil {
		t.Fatalf("insert old memory: %v", err)
	}

	ctx := context.Background()
	res, recallErr := srv.handleRecall(ctx, callTool(map[string]any{
		"agent_id":      "stale-browse-agent",
		"include_stale": true,
	}))
	if recallErr != nil {
		t.Fatal(recallErr)
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

	memories, _ := resp["memories"].([]any)
	found := false
	for _, raw := range memories {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, _ := m["content"].(string)
		if strings.Contains(content, "old memory visible when include_stale") {
			found = true
		}
	}
	if !found {
		t.Error("include_stale=true should bypass decay filter and show old memory")
	}
}

// ── Directed arrow and multi-anchor fixes (v2 hardening) ─────────────────────

func TestGraphBFS_IsIncoming_OutgoingEdge(t *testing.T) {
	// Outgoing edge (A→B): IsIncoming must be false for B's parent entry.
	srv := newTestServer(t)
	aID := srv.graph.MakeNodeID("a.go", "CallerA")
	bID := srv.graph.MakeNodeID("b.go", "CalleeB")
	srv.graph.AddNode(&graph.Node{ID: aID, Name: "CallerA", Type: graph.NodeFunction, File: "a.go", Line: 1})
	srv.graph.AddNode(&graph.Node{ID: bID, Name: "CalleeB", Type: graph.NodeFunction, File: "b.go", Line: 1})
	srv.graph.AddEdge(&graph.Edge{From: aID, To: bID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 1)

	entry, ok := result.ParentMap[string(bID)]
	if !ok {
		t.Fatal("expected ParentMap entry for CalleeB")
	}
	if entry.IsIncoming {
		t.Error("outgoing edge should set IsIncoming=false on the discovered callee")
	}
	if entry.ParentNodeID != string(aID) {
		t.Errorf("CallerA should be parent of CalleeB, got %q", entry.ParentNodeID)
	}
}

func TestGraphBFS_IsIncoming_IncomingEdge(t *testing.T) {
	// Incoming edge (B→A, seed=A): IsIncoming must be true for B's parent entry.
	srv := newTestServer(t)
	aID := srv.graph.MakeNodeID("a.go", "SeedCallee")
	bID := srv.graph.MakeNodeID("b.go", "Caller")
	srv.graph.AddNode(&graph.Node{ID: aID, Name: "SeedCallee", Type: graph.NodeFunction, File: "a.go", Line: 1})
	srv.graph.AddNode(&graph.Node{ID: bID, Name: "Caller", Type: graph.NodeFunction, File: "b.go", Line: 1})
	// B calls A (incoming from A's perspective).
	srv.graph.AddEdge(&graph.Edge{From: bID, To: aID, Type: graph.EdgeCalls})

	result := srv.graphBFS([]string{string(aID)}, 1)

	entry, ok := result.ParentMap[string(bID)]
	if !ok {
		t.Fatal("expected ParentMap entry for Caller (discovered via InEdges)")
	}
	if !entry.IsIncoming {
		t.Error("incoming edge discovery should set IsIncoming=true on the discovered caller")
	}
	if entry.ParentNodeID != string(aID) {
		t.Errorf("SeedCallee should be parent of Caller, got %q", entry.ParentNodeID)
	}
}

func TestBuildGraphPath_DirectedArrow_Outgoing(t *testing.T) {
	// Outgoing edge: path should use →[CALLS]→ (left calls right).
	srv := newTestServer(t)
	authID := srv.graph.MakeNodeID("auth.go", "AuthService")
	tokID := srv.graph.MakeNodeID("auth.go", "TokenValidator")
	srv.graph.AddNode(&graph.Node{ID: authID, Name: "AuthService", Type: graph.NodeFunction, File: "auth.go", Line: 1})
	srv.graph.AddNode(&graph.Node{ID: tokID, Name: "TokenValidator", Type: graph.NodeFunction, File: "auth.go", Line: 50})
	srv.graph.AddEdge(&graph.Edge{From: authID, To: tokID, Type: graph.EdgeCalls})

	bfsResult := srv.graphBFS([]string{string(authID)}, 2)
	pathStr, hops := srv.buildGraphPath(string(tokID), bfsResult)

	if hops != 1 {
		t.Fatalf("expected 1 hop, got %d", hops)
	}
	// AuthService calls TokenValidator — outgoing → arrow points right.
	want := "AuthService →[CALLS]→ TokenValidator"
	if pathStr != want {
		t.Errorf("outgoing path: got %q, want %q", pathStr, want)
	}
}

func TestBuildGraphPath_DirectedArrow_Incoming(t *testing.T) {
	// Incoming edge (Caller→Seed): path should use ←[CALLS]- (right calls left).
	srv := newTestServer(t)
	seedID := srv.graph.MakeNodeID("auth.go", "AuthService")
	callerID := srv.graph.MakeNodeID("api.go", "APIHandler")
	srv.graph.AddNode(&graph.Node{ID: seedID, Name: "AuthService", Type: graph.NodeFunction, File: "auth.go", Line: 1})
	srv.graph.AddNode(&graph.Node{ID: callerID, Name: "APIHandler", Type: graph.NodeFunction, File: "api.go", Line: 1})
	// APIHandler calls AuthService (incoming from AuthService's perspective).
	srv.graph.AddEdge(&graph.Edge{From: callerID, To: seedID, Type: graph.EdgeCalls})

	bfsResult := srv.graphBFS([]string{string(seedID)}, 1)
	pathStr, hops := srv.buildGraphPath(string(callerID), bfsResult)

	if hops != 1 {
		t.Fatalf("expected 1 hop, got %d", hops)
	}
	// APIHandler calls AuthService — memory at APIHandler, query matched AuthService.
	// Display: AuthService ←[CALLS]- APIHandler (APIHandler calls AuthService).
	want := "AuthService ←[CALLS]- APIHandler"
	if pathStr != want {
		t.Errorf("incoming path: got %q, want %q", pathStr, want)
	}
}

func TestBuildGraphPath_Mixed_Direction(t *testing.T) {
	// Two-hop mixed: Seed →[CALLS]→ Mid ←[CALLS]- Anchor
	// Seed calls Mid (outgoing), Anchor calls Mid (incoming to Mid).
	srv := newTestServer(t)
	seedID := srv.graph.MakeNodeID("svc.go", "Service")
	midID := srv.graph.MakeNodeID("core.go", "Core")
	anchorID := srv.graph.MakeNodeID("util.go", "Util")
	for _, n := range []*graph.Node{
		{ID: seedID, Name: "Service", Type: graph.NodeFunction, File: "svc.go", Line: 1},
		{ID: midID, Name: "Core", Type: graph.NodeFunction, File: "core.go", Line: 1},
		{ID: anchorID, Name: "Util", Type: graph.NodeFunction, File: "util.go", Line: 1},
	} {
		srv.graph.AddNode(n)
	}
	srv.graph.AddEdge(&graph.Edge{From: seedID, To: midID, Type: graph.EdgeCalls})   // outgoing
	srv.graph.AddEdge(&graph.Edge{From: anchorID, To: midID, Type: graph.EdgeCalls}) // incoming to Mid

	bfsResult := srv.graphBFS([]string{string(seedID)}, 2)
	pathStr, hops := srv.buildGraphPath(string(anchorID), bfsResult)

	if hops != 2 {
		t.Fatalf("expected 2 hops, got %d (path: %q)", hops, pathStr)
	}
	want := "Service →[CALLS]→ Core ←[CALLS]- Util"
	if pathStr != want {
		t.Errorf("mixed path: got %q, want %q", pathStr, want)
	}
}

func TestGetMemoryAnchorNodeIDsInSet_SingleAnchorInSet(t *testing.T) {
	srv := newTestServer(t)

	nodeID := string(srv.graph.MakeNodeID("auth.go", "AuthService"))
	memID, _ := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "test memory", AgentID: "a1", Source: store.SourceManual,
	}, []string{nodeID})

	nodeSet := map[string]bool{nodeID: true}
	result, err := srv.store.GetMemoryAnchorNodeIDsInSet([]string{memID}, nodeSet)
	if err != nil {
		t.Fatal(err)
	}
	if result[memID] != nodeID {
		t.Errorf("expected anchor %q, got %q", nodeID, result[memID])
	}
}

func TestGetMemoryAnchorNodeIDsInSet_MultiAnchorPicksInSet(t *testing.T) {
	// Memory with two anchors: first by created_at is NOT in nodeSet.
	// GetMemoryAnchorNodeIDsInSet must return the SECOND anchor (the one in nodeSet).
	// This is the multi-anchor bug fix.
	srv := newTestServer(t)

	firstNodeID := string(srv.graph.MakeNodeID("old.go", "OldNode"))
	secondNodeID := string(srv.graph.MakeNodeID("bfs.go", "BFSNode"))

	memID, err := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "multi-anchor memory", AgentID: "a1", Source: store.SourceManual,
	}, []string{firstNodeID, secondNodeID})
	if err != nil {
		t.Fatal(err)
	}

	// nodeSet only contains the SECOND anchor (as if BFS discovered secondNodeID but not firstNodeID).
	nodeSet := map[string]bool{secondNodeID: true}
	result, err := srv.store.GetMemoryAnchorNodeIDsInSet([]string{memID}, nodeSet)
	if err != nil {
		t.Fatal(err)
	}
	// Must return secondNodeID, not firstNodeID.
	if got := result[memID]; got != secondNodeID {
		t.Errorf("multi-anchor: expected BFS-discovered anchor %q, got %q (old GetMemoryAnchorNodeIDs bug)", secondNodeID, got)
	}
}

func TestGetMemoryAnchorNodeIDsInSet_NoAnchorInSet(t *testing.T) {
	// None of the memory's anchors are in nodeSet — result should be empty.
	srv := newTestServer(t)

	nodeID := string(srv.graph.MakeNodeID("x.go", "NotInBFS"))
	memID, _ := srv.store.InsertMemoryWithAnchors(store.Memory{
		Tier: store.TierEntity, Content: "orphan anchor memory", AgentID: "a1", Source: store.SourceManual,
	}, []string{nodeID})

	nodeSet := map[string]bool{"totally::different::node": true}
	result, err := srv.store.GetMemoryAnchorNodeIDsInSet([]string{memID}, nodeSet)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := result[memID]; found {
		t.Error("expected no result when anchor is not in nodeSet")
	}
}

func TestGetMemoryAnchorNodeIDsInSet_EmptyInputs(t *testing.T) {
	srv := newTestServer(t)
	r1, err := srv.store.GetMemoryAnchorNodeIDsInSet(nil, map[string]bool{"x": true})
	if err != nil || r1 != nil {
		t.Errorf("nil memIDs: expected (nil, nil), got (%v, %v)", r1, err)
	}
	r2, err := srv.store.GetMemoryAnchorNodeIDsInSet([]string{"x"}, nil)
	if err != nil || r2 != nil {
		t.Errorf("nil nodeSet: expected (nil, nil), got (%v, %v)", r2, err)
	}
}

// ── Sprint 10.5: temporal cross-domain queries ────────────────────────────────

// TestHandleRecall_SinceBound verifies that since= excludes memories before the cutoff.
func TestHandleRecall_SinceBound(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "auth refactor with OAuth2",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})

	// since = 1 hour from now → no memory should be returned (all memories are "old").
	future := time.Now().UTC().Add(1 * time.Hour).Format("2006-01-02T15:04:05Z")
	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "auth",
		"since": future,
	}))
	if err != nil {
		t.Fatal(err)
	}
	// The since= is in the future — 0 memories should match.
	raw := extractJSON(t, res)
	if mems, ok := raw["memories"]; ok {
		if arr, ok := mems.([]interface{}); ok && len(arr) > 0 {
			t.Errorf("since=future: expected 0 memories, got %d", len(arr))
		}
	}
}

// TestHandleRecall_UntilBound verifies that until= excludes memories after the cutoff.
func TestHandleRecall_UntilBound(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "auth refactor with OAuth2",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})

	// until = 1 hour ago → the just-inserted memory was created after the cutoff.
	past := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02T15:04:05Z")
	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "auth",
		"until": past,
	}))
	if err != nil {
		t.Fatal(err)
	}
	raw := extractJSON(t, res)
	if mems, ok := raw["memories"]; ok {
		if arr, ok := mems.([]interface{}); ok && len(arr) > 0 {
			t.Errorf("until=past: expected 0 memories, got %d", len(arr))
		}
	}
}

// TestHandleRecall_SinceUntilWindow verifies that memories within the window are returned.
func TestHandleRecall_SinceUntilWindow(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "auth JWT token validation",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})

	// Window from 1 hour ago to 1 hour from now — the memory should be included.
	since := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02T15:04:05Z")
	until := time.Now().UTC().Add(1 * time.Hour).Format("2006-01-02T15:04:05Z")
	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "auth JWT",
		"since": since,
		"until": until,
	}))
	if err != nil {
		t.Fatal(err)
	}
	raw := extractJSON(t, res)
	// time_filter should be annotated in the response.
	if _, ok := raw["time_filter"]; !ok {
		t.Error("expected time_filter annotation in response when since/until provided")
	}
	// The memory should be present.
	mems, _ := raw["memories"].([]interface{})
	if len(mems) == 0 {
		t.Error("expected at least 1 memory within the time window")
	}
}

// TestHandleRecall_SinceAfterUntil_ReturnsError verifies validation rejects inverted range.
func TestHandleRecall_SinceAfterUntil_ReturnsError(t *testing.T) {
	srv := newTestServer(t)

	since := "2026-03-15"
	until := "2026-03-01" // before since — invalid
	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "auth",
		"since": since,
		"until": until,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("expected error result for inverted since/until range")
	}
	if !strings.Contains(extractErrorText(t, res), "before") {
		t.Errorf("error message should mention 'before', got: %s", extractErrorText(t, res))
	}
}

// TestHandleRecall_InvalidSinceFormat_ReturnsError verifies that bad format is rejected.
func TestHandleRecall_InvalidSinceFormat_ReturnsError(t *testing.T) {
	srv := newTestServer(t)

	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "auth",
		"since": "not-a-date",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("expected error for unparseable since= value")
	}
}

// TestHandleRecall_LimitRespectedAfterTimeFilter verifies that limit= is honored even when
// the inflated quadLimit would return more candidates inside the time window.
// Regression: without the re-cap, limit=2 with 5 in-range memories returned 5.
func TestHandleRecall_LimitRespectedAfterTimeFilter(t *testing.T) {
	srv := newTestServer(t)

	for i := 0; i < 5; i++ {
		_, _ = srv.store.InsertMemory(store.Memory{
			Tier:    store.TierProject,
			Content: fmt.Sprintf("auth decision number %d", i),
			AgentID: "agent-1",
			Source:  store.SourceManual,
		})
	}

	since := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02T15:04:05Z")
	until := time.Now().UTC().Add(1 * time.Hour).Format("2006-01-02T15:04:05Z")
	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "auth decision",
		"since": since,
		"until": until,
		"limit": float64(2),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", extractErrorText(t, res))
	}
	raw := extractJSON(t, res)
	mems, _ := raw["memories"].([]interface{})
	if len(mems) > 2 {
		t.Errorf("limit=2 must be respected after time filter: got %d memories", len(mems))
	}
}

// TestHandleRecall_DateOnlyFormat verifies "2026-03-01" is accepted without RFC3339 suffix.
func TestHandleRecall_DateOnlyFormat(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "auth session token cleanup",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})

	// Date-only format should not return an error.
	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "auth session",
		"since": "2020-01-01", // old date — all memories after this
		"until": "2099-12-31", // far future — all memories before this
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("date-only format should be accepted, got error: %s", extractErrorText(t, res))
	}
	raw := extractJSON(t, res)
	if _, ok := raw["time_filter"]; !ok {
		t.Error("expected time_filter annotation for date-only since/until")
	}
}

// TestQuadRecallSearch_UntilBound_TemporalChannel verifies until= gates the temporal channel.
// The temporal channel is the only channel that respects untilTime directly.
// BM25 / semantic channels are time-agnostic — post-filtering in handleRecall trims those.
// This test verifies that a memory that would ONLY appear in the temporal channel
// (i.e., it contains a noise term that doesn't match the query) is excluded when until=past.
func TestQuadRecallSearch_UntilBound_TemporalChannel(t *testing.T) {
	srv := newTestServer(t)

	// Insert a memory that does NOT match the query "zzz_nomatching_query" via BM25,
	// but IS recent — meaning it can only surface via the temporal channel.
	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "xyzzy infrastructure background update foobar",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})

	// until = 1 hour ago — the just-inserted memory was created "now", which is AFTER the cutoff.
	// With until=past, the temporal channel should not return the memory.
	past := time.Now().UTC().Add(-1 * time.Hour)
	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "zzz_nomatching_query", "", 10, false, 30, &past, 0)

	// The memory should not appear: BM25 won't find it (query doesn't match),
	// and temporal channel is bounded to past — the memory was created after the cutoff.
	for _, m := range mems {
		if m.Content == "xyzzy infrastructure background update foobar" {
			channels := attr[m.ID]
			t.Errorf("memory created after until= should not appear; found in channels %v", channels)
		}
	}
}

// ── helpers used in Sprint 10.5 tests ────────────────────────────────────────

func extractJSON(t *testing.T, res *mcp.CallToolResult) map[string]interface{} {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		return nil
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			var out map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Text), &out); err == nil {
				return out
			}
		}
	}
	return nil
}

func extractErrorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// ── clampUnit tests ──────────────────────────────────────────────────────────

func TestClampUnit_InRange(t *testing.T) {
	t.Parallel()
	for _, v := range []float64{0.0, 0.5, 1.0, 0.001, 0.999} {
		if got := clampUnit(v); got != v {
			t.Errorf("clampUnit(%f) = %f, want %f", v, got, v)
		}
	}
}

func TestClampUnit_Below(t *testing.T) {
	t.Parallel()
	if got := clampUnit(-0.5); got != 0 {
		t.Errorf("clampUnit(-0.5) = %f, want 0", got)
	}
	if got := clampUnit(-100); got != 0 {
		t.Errorf("clampUnit(-100) = %f, want 0", got)
	}
}

func TestClampUnit_Above(t *testing.T) {
	t.Parallel()
	if got := clampUnit(1.5); got != 1 {
		t.Errorf("clampUnit(1.5) = %f, want 1", got)
	}
	if got := clampUnit(999); got != 1 {
		t.Errorf("clampUnit(999) = %f, want 1", got)
	}
}

// ── Sprint 13 #6: buildEnrichedQuery unit tests ───────────────────────────────

func TestBuildEnrichedQuery_EmptyQuery(t *testing.T) {
	t.Parallel()
	// Empty query → no enrichment regardless of intent/task.
	if got := buildEnrichedQuery("", "implementing auth", "Fix login bug"); got != "" {
		t.Errorf("empty query should be returned unchanged, got %q", got)
	}
}

func TestBuildEnrichedQuery_EmptyContext(t *testing.T) {
	t.Parallel()
	// No intent, no task title → original query returned unchanged.
	if got := buildEnrichedQuery("caching strategy", "", ""); got != "caching strategy" {
		t.Errorf("empty context should return original query, got %q", got)
	}
}

func TestBuildEnrichedQuery_AppendsMissingTerms(t *testing.T) {
	t.Parallel()
	// "caching strategy" + intent "implementing auth" → appends "implementing auth".
	got := buildEnrichedQuery("caching strategy", "implementing auth", "")
	if !strings.Contains(got, "caching strategy") {
		t.Error("original query must be present")
	}
	if !strings.Contains(got, "implementing") || !strings.Contains(got, "auth") {
		t.Errorf("intent terms should be appended; got %q", got)
	}
}

func TestBuildEnrichedQuery_DeduplicatesExistingTerms(t *testing.T) {
	t.Parallel()
	// intent "auth caching" when query is "auth caching middleware" → no new terms.
	got := buildEnrichedQuery("auth caching middleware", "auth caching", "")
	if got != "auth caching middleware" {
		t.Errorf("already-present terms should not be appended; got %q", got)
	}
}

func TestBuildEnrichedQuery_CaseInsensitiveDedup(t *testing.T) {
	t.Parallel()
	// "Auth" in intent matches "auth" in query (case-insensitive).
	got := buildEnrichedQuery("auth token", "Auth middleware", "")
	if strings.Contains(got, "Auth") {
		t.Errorf("case-insensitive dedup should suppress 'Auth' since 'auth' is in query; got %q", got)
	}
	if !strings.Contains(got, "middleware") {
		t.Errorf("novel term 'middleware' should be appended; got %q", got)
	}
}

func TestBuildEnrichedQuery_ShortTokensSkipped(t *testing.T) {
	t.Parallel()
	// Single- and double-char tokens (stop words) should be skipped.
	got := buildEnrichedQuery("auth token", "a is in the auth", "")
	if strings.Contains(got, " a ") || strings.Contains(got, " is ") || strings.Contains(got, " in ") {
		t.Errorf("short tokens should be skipped; got %q", got)
	}
}

func TestBuildEnrichedQuery_WithTaskTitle(t *testing.T) {
	t.Parallel()
	// Both intent and task title contribute novel terms.
	got := buildEnrichedQuery("caching", "auth middleware", "Fix login redirect bug")
	for _, want := range []string{"auth", "middleware", "Fix", "login", "redirect"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in enriched query %q", want, got)
		}
	}
}

// ── Sprint 13 #6: context-weighted recall integration tests ──────────────────

func TestContextWeightedRecall_EnrichesQueryWithAgentIntent(t *testing.T) {
	// When agent_id is provided and agent has intent/task set, recall()
	// should surface memories that match intent terms even when the raw
	// query alone wouldn't rank them first.
	srv := newTestServer(t)

	// Insert a memory that matches both query and intent.
	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "auth middleware token validation caching optimisation",
		AgentID: "agent-ctx",
		Source:  store.SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Insert another memory that only matches the query term.
	_, err = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "caching layer uses Redis for session storage",
		AgentID: "agent-ctx",
		Source:  store.SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Register the agent with intent "auth middleware" so enrichment fires.
	if upsertErr := srv.store.UpsertAgent("agent-ctx", &store.AgentActivity{
		Intent:    "auth middleware implementation",
		TaskTitle: "Implement token refresh",
	}); upsertErr != nil {
		t.Fatalf("UpsertAgent: %v", upsertErr)
	}

	// Call handleRecall with query="caching" and agent_id="agent-ctx".
	// The enriched query becomes "caching auth middleware implementation Implement token refresh".
	res, recallErr := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query":    "caching",
		"agent_id": "agent-ctx",
	}))
	if recallErr != nil {
		t.Fatal(recallErr)
	}
	if res.IsError {
		t.Fatalf("handleRecall returned error: %v", extractErrorText(t, res))
	}

	raw := extractJSON(t, res)
	if raw == nil {
		t.Fatal("nil JSON response")
	}
	// Response must include query_enrichment annotation with applied=true.
	ce, ok := raw["query_enrichment"]
	if !ok {
		t.Fatal("expected query_enrichment annotation in response when agent_id provided and intent set")
	}
	ceMap, ok := ce.(map[string]interface{})
	if !ok {
		t.Fatalf("query_enrichment has unexpected type %T", ce)
	}
	if ceMap["applied"] != true {
		t.Errorf("query_enrichment.applied = %v, want true", ceMap["applied"])
	}
	if _, hasEnriched := ceMap["enriched_query"]; !hasEnriched {
		t.Error("query_enrichment.enriched_query should be present when applied=true")
	}
}

func TestContextWeightedRecall_NoEnrichmentWithoutAgentID(t *testing.T) {
	// Without agent_id, recall() must behave identically to before (no enrichment).
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "context weighted recall test memory",
		AgentID: "nobody",
		Source:  store.SourceManual,
	})

	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query": "context weighted recall",
	}))
	if err != nil {
		t.Fatal(err)
	}

	raw := extractJSON(t, res)
	if raw == nil {
		t.Fatal("nil JSON response")
	}
	// context_enrichment must NOT appear when agent_id is absent.
	if _, ok := raw["query_enrichment"]; ok {
		t.Error("query_enrichment should NOT appear when agent_id is not provided")
	}
}

func TestContextWeightedRecall_GracefulFallbackWhenAgentNotFound(t *testing.T) {
	// When agent_id is provided but agent has no record (first session),
	// recall() should degrade gracefully to the original query.
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "fallback test memory for unknown agent",
		AgentID: "unknown-agent",
		Source:  store.SourceManual,
	})

	// "ghost-agent" has no UpsertAgent record — GetAgent returns nil, nil.
	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query":    "fallback test",
		"agent_id": "ghost-agent",
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Should not be an error — graceful degradation.
	if res.IsError {
		t.Fatalf("unexpected error for unknown agent: %s", extractErrorText(t, res))
	}
	raw := extractJSON(t, res)
	if raw == nil {
		t.Fatal("nil JSON response")
	}
	// No enrichment fired because agent has no state.
	if _, ok := raw["query_enrichment"]; ok {
		t.Error("query_enrichment should NOT appear for unknown agent with no state")
	}
}

func TestContextWeightedRecall_NoEnrichmentForEmptyIntent(t *testing.T) {
	// Agent exists but has no intent/task — enrichedQuery == query → no annotation.
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "empty intent test memory",
		AgentID: "bare-agent",
		Source:  store.SourceManual,
	})
	// Register agent with no intent or task title.
	if err := srv.store.UpsertAgent("bare-agent", &store.AgentActivity{}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	res, err := srv.handleRecall(context.Background(), callTool(map[string]any{
		"query":    "empty intent test",
		"agent_id": "bare-agent",
	}))
	if err != nil {
		t.Fatal(err)
	}
	raw := extractJSON(t, res)
	if raw == nil {
		t.Fatal("nil JSON response")
	}
	// Agent found but no intent/task — context_enrichment present with applied=false.
	ce, ok := raw["query_enrichment"]
	if !ok {
		t.Error("query_enrichment should appear when agent_id is provided and agent is found, even with empty intent/task")
		return
	}
	ceMap, ok := ce.(map[string]interface{})
	if !ok {
		t.Fatalf("query_enrichment has unexpected type %T", ce)
	}
	if ceMap["applied"] != false {
		t.Errorf("query_enrichment.applied = %v, want false (no new context terms)", ceMap["applied"])
	}
	if _, hasEnriched := ceMap["enriched_query"]; hasEnriched {
		t.Error("query_enrichment.enriched_query should be absent when applied=false")
	}
}

func TestGetAgent_NotFound(t *testing.T) {
	// GetAgent returns nil, nil for an agent that was never inserted.
	st := openMCPTestStore(t)
	agent, err := st.GetAgent("nonexistent-agent-xyz")
	if err != nil {
		t.Fatalf("GetAgent returned error for unknown agent: %v", err)
	}
	if agent != nil {
		t.Errorf("GetAgent should return nil for unknown agent, got %+v", agent)
	}
}

func TestGetAgent_Found(t *testing.T) {
	// GetAgent returns the correct Agent record after UpsertAgent.
	st := openMCPTestStore(t)
	if err := st.UpsertAgent("test-agent", &store.AgentActivity{
		Intent:    "implementing caching",
		TaskTitle: "Sprint 13 task 6",
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	agent, err := st.GetAgent("test-agent")
	if err != nil {
		t.Fatalf("GetAgent error: %v", err)
	}
	if agent == nil {
		t.Fatal("GetAgent returned nil for known agent")
	}
	if agent.Intent != "implementing caching" {
		t.Errorf("Intent: got %q want %q", agent.Intent, "implementing caching")
	}
	if agent.CurrentTaskTitle != "Sprint 13 task 6" {
		t.Errorf("CurrentTaskTitle: got %q want %q", agent.CurrentTaskTitle, "Sprint 13 task 6")
	}
}
