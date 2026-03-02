package graph_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

// buildFixture returns a small but representative graph used across tests.
//
//	auth.go
//	  └─DEFINES─► AuthService (struct)
//	  └─DEFINES─► Login       (func)
//	Login ─CALLS─► AuthService
//
//	db.go
//	  └─DEFINES─► UserRepo    (struct)
//	auth.go ─IMPORTS─► db.go
func buildFixture(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")

	authFile := g.MakeNodeID("auth.go", "auth.go")
	dbFile := g.MakeNodeID("db.go", "db.go")
	svcID := g.MakeNodeID("auth.go", "AuthService")
	loginID := g.MakeNodeID("auth.go", "Login")
	repoID := g.MakeNodeID("db.go", "UserRepo")

	g.AddNode(&graph.Node{ID: authFile, Type: graph.NodeFile, Name: "auth.go", File: "auth.go"})
	g.AddNode(&graph.Node{ID: dbFile, Type: graph.NodeFile, Name: "db.go", File: "db.go"})
	g.AddNode(&graph.Node{ID: svcID, Type: graph.NodeStruct, Name: "AuthService", File: "auth.go", Exported: true})
	g.AddNode(&graph.Node{ID: loginID, Type: graph.NodeFunction, Name: "Login", File: "auth.go", Exported: true})
	g.AddNode(&graph.Node{ID: repoID, Type: graph.NodeStruct, Name: "UserRepo", File: "db.go", Exported: true})

	g.AddEdge(&graph.Edge{From: authFile, To: svcID, Type: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: authFile, To: loginID, Type: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: loginID, To: svcID, Type: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: authFile, To: dbFile, Type: graph.EdgeImports})
	g.AddEdge(&graph.Edge{From: dbFile, To: repoID, Type: graph.EdgeDefines})

	return g
}

func TestNodeCount(t *testing.T) {
	g := buildFixture(t)
	if got := g.NodeCount(); got != 5 {
		t.Errorf("NodeCount() = %d, want 5", got)
	}
}

func TestEdgeCount(t *testing.T) {
	g := buildFixture(t)
	if got := g.EdgeCount(); got != 5 {
		t.Errorf("EdgeCount() = %d, want 5", got)
	}
}

func TestFaninFanout(t *testing.T) {
	g := buildFixture(t)
	svcID := g.MakeNodeID("auth.go", "AuthService")

	// AuthService is called by Login and defined by auth.go → fanin = 2
	if got := g.Fanin(svcID); got != 2 {
		t.Errorf("Fanin(AuthService) = %d, want 2", got)
	}
	// AuthService has no outgoing edges → fanout = 0
	if got := g.Fanout(svcID); got != 0 {
		t.Errorf("Fanout(AuthService) = %d, want 0", got)
	}
}

func TestAddEdge_DropsDanglingRef(t *testing.T) {
	g := graph.New("test")
	g.AddNode(&graph.Node{ID: "a::x::Foo", Type: graph.NodeFunction, Name: "Foo"})

	// "Bar" does not exist — edge must be silently dropped.
	g.AddEdge(&graph.Edge{From: "a::x::Foo", To: "a::x::Bar", Type: graph.EdgeCalls})

	if got := g.EdgeCount(); got != 0 {
		t.Errorf("dangling edge not dropped: EdgeCount() = %d, want 0", got)
	}
}

func TestFindByName_ExactMatch(t *testing.T) {
	g := buildFixture(t)
	nodes := g.FindByName("AuthService")
	if len(nodes) != 1 {
		t.Fatalf("FindByName(AuthService) returned %d results, want 1", len(nodes))
	}
	if nodes[0].Name != "AuthService" {
		t.Errorf("got name %q, want AuthService", nodes[0].Name)
	}
}

func TestFindByName_CaseInsensitive(t *testing.T) {
	g := buildFixture(t)
	nodes := g.FindByName("authservice")
	if len(nodes) != 1 {
		t.Errorf("case-insensitive match failed: got %d results, want 1", len(nodes))
	}
}

func TestFindByPattern_Substring(t *testing.T) {
	g := buildFixture(t)
	nodes := g.FindByPattern("Service")
	if len(nodes) == 0 {
		t.Error("FindByPattern(Service) returned no results")
	}
	for _, n := range nodes {
		if !containsCI(n.Name, "service") {
			t.Errorf("unexpected match: %q does not contain 'service'", n.Name)
		}
	}
}

func TestRemoveFile(t *testing.T) {
	g := buildFixture(t)
	g.RemoveFile("auth.go")

	// Nodes belonging to auth.go must be gone.
	nodesAfter := g.AllNodes()
	for _, n := range nodesAfter {
		if n.File == "auth.go" {
			t.Errorf("node %q still present after RemoveFile(auth.go)", n.Name)
		}
	}

	// Edges that pointed to or from an auth.go node must be gone.
	edges := g.AllEdges()
	for _, e := range edges {
		fromNode := g.GetNode(e.From)
		toNode := g.GetNode(e.To)
		if fromNode == nil || toNode == nil {
			t.Errorf("dangling edge %s→%s after RemoveFile", e.From, e.To)
		}
	}
}

func TestProjectIdentity_Counts(t *testing.T) {
	g := buildFixture(t)
	id := g.ProjectIdentity()

	if id.Summary.Files != 2 {
		t.Errorf("Summary.Files = %d, want 2", id.Summary.Files)
	}
	if id.Summary.Functions != 1 {
		t.Errorf("Summary.Functions = %d, want 1", id.Summary.Functions)
	}
	if id.Summary.Structs != 2 {
		t.Errorf("Summary.Structs = %d, want 2", id.Summary.Structs)
	}
	if id.Summary.Edges != 5 {
		t.Errorf("Summary.Edges = %d, want 5", id.Summary.Edges)
	}
}

func TestProjectIdentity_KeyEntities(t *testing.T) {
	g := buildFixture(t)
	id := g.ProjectIdentity()
	if len(id.KeyEntities) == 0 {
		t.Error("KeyEntities is empty")
	}
	// AuthService should be near the top: fanin=2 (DEFINES + CALLS incoming).
	found := false
	for _, e := range id.KeyEntities {
		if e.Name == "AuthService" {
			found = true
			break
		}
	}
	if !found {
		t.Error("AuthService not in KeyEntities despite high connectivity")
	}
}

func TestAllEdges_NoDuplicates(t *testing.T) {
	g := buildFixture(t)
	edges := g.AllEdges()
	seen := make(map[[3]string]struct{})
	for _, e := range edges {
		key := [3]string{string(e.From), string(e.To), string(e.Type)}
		if _, dup := seen[key]; dup {
			t.Errorf("duplicate edge: %s →[%s]→ %s", e.From, e.Type, e.To)
		}
		seen[key] = struct{}{}
	}
}

// containsCI is a case-insensitive contains helper for tests.
func containsCI(s, sub string) bool {
	sLower, subLower := toLower(s), toLower(sub)
	for i := 0; i <= len(sLower)-len(subLower); i++ {
		if sLower[i:i+len(subLower)] == subLower {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// --- Stable UUID tests ---

func TestAddNode_AssignsStableID(t *testing.T) {
	g := graph.New("repo")
	id := g.MakeNodeID("svc.go", "Serve")
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: "Serve", File: "svc.go"})

	nodes := g.NodesForFile("svc.go")
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].StableID == "" {
		t.Error("AddNode should assign a non-empty StableID")
	}
}

func TestAddNode_PreservesExistingStableID(t *testing.T) {
	g := graph.New("repo")
	id := g.MakeNodeID("svc.go", "Serve")
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: "Serve", File: "svc.go", StableID: "fixed-uuid"})

	nodes := g.NodesForFile("svc.go")
	if nodes[0].StableID != "fixed-uuid" {
		t.Errorf("StableID = %q, want %q", nodes[0].StableID, "fixed-uuid")
	}
}

func TestMigrateStableID_ExactMatch(t *testing.T) {
	g := graph.New("repo")
	id := g.MakeNodeID("svc.go", "Serve")
	g.AddNode(&graph.Node{
		ID: id, Type: graph.NodeFunction, Name: "Serve", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"signature": "func() error"},
	})

	nodes := g.NodesForFile("svc.go")
	original := nodes[0].StableID
	if original == "" {
		t.Fatal("expected non-empty StableID after AddNode")
	}

	// Simulate re-parse: snapshot → remove → add new node → migrate.
	g.SnapshotFileStableIDs("svc.go")
	g.RemoveFile("svc.go")

	id2 := g.MakeNodeID("svc.go", "Serve")
	g.AddNode(&graph.Node{
		ID: id2, Type: graph.NodeFunction, Name: "Serve", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"signature": "func() error"},
	})

	for _, n := range g.NodesForFile("svc.go") {
		g.MigrateStableID(n)
	}
	g.ClearFileSnapshot("svc.go")

	nodes = g.NodesForFile("svc.go")
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node after re-parse, got %d", len(nodes))
	}
	if nodes[0].StableID != original {
		t.Errorf("StableID after re-parse = %q, want %q (original)", nodes[0].StableID, original)
	}
}

func TestMigrateStableID_RenameViaSignature(t *testing.T) {
	g := graph.New("repo")
	id := g.MakeNodeID("svc.go", "OldName")
	g.AddNode(&graph.Node{
		ID: id, Type: graph.NodeFunction, Name: "OldName", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"signature": "func(ctx context.Context) error"},
	})
	original := g.NodesForFile("svc.go")[0].StableID

	g.SnapshotFileStableIDs("svc.go")
	g.RemoveFile("svc.go")

	// Re-parsed with new name but same signature → Tier 2 match.
	id2 := g.MakeNodeID("svc.go", "NewName")
	g.AddNode(&graph.Node{
		ID: id2, Type: graph.NodeFunction, Name: "NewName", Package: "svc", File: "svc.go",
		Metadata: map[string]string{"signature": "func(ctx context.Context) error"},
	})
	for _, n := range g.NodesForFile("svc.go") {
		g.MigrateStableID(n)
	}
	g.ClearFileSnapshot("svc.go")

	if got := g.NodesForFile("svc.go")[0].StableID; got != original {
		t.Errorf("renamed node StableID = %q, want %q (tier-2 migration)", got, original)
	}
}

func TestMigrateStableID_NoMatchGetsNewUUID(t *testing.T) {
	g := graph.New("repo")
	id := g.MakeNodeID("svc.go", "Serve")
	g.AddNode(&graph.Node{ID: id, Type: graph.NodeFunction, Name: "Serve", Package: "svc", File: "svc.go"})
	original := g.NodesForFile("svc.go")[0].StableID

	g.SnapshotFileStableIDs("svc.go")
	g.RemoveFile("svc.go")

	// Completely different function — should get a fresh UUID.
	id2 := g.MakeNodeID("svc.go", "Stop")
	g.AddNode(&graph.Node{ID: id2, Type: graph.NodeFunction, Name: "Stop", Package: "svc", File: "svc.go"})
	for _, n := range g.NodesForFile("svc.go") {
		g.MigrateStableID(n)
	}
	g.ClearFileSnapshot("svc.go")

	newID := g.NodesForFile("svc.go")[0].StableID
	if newID == "" {
		t.Error("new node should have a non-empty StableID")
	}
	if newID == original {
		t.Error("completely different function should not reuse the original StableID")
	}
}
