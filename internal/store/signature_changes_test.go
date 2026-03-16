package store_test

// Tests for Store.GetSignatureChanges — verifies that SaveGraph correctly
// tracks prev_signature and GetSignatureChanges returns only actually-changed
// exported entities.

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

func makeGraph(t *testing.T, nodes []*graph.Node) *graph.Graph {
	t.Helper()
	g := graph.New("test-repo")
	for _, n := range nodes {
		g.AddNode(n)
	}
	return g
}

// TestGetSignatureChanges_ChangedSignature verifies the common case: an
// exported function whose signature changes between two SaveGraph calls.
func TestGetSignatureChanges_ChangedSignature(t *testing.T) {
	st := openTestStore(t)

	g1 := graph.New("test-repo")
	id := g1.MakeNodeID("pkg/api/api.go", "Handle")
	g1.AddNode(&graph.Node{
		ID: id, Name: "Handle", Type: graph.NodeFunction,
		File: "pkg/api/api.go", Package: "api", Exported: true, Line: 10,
		Metadata: map[string]string{"signature": "func Handle(w http.ResponseWriter)"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	// Second save: signature changed — new param added.
	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: id, Name: "Handle", Type: graph.NodeFunction,
		File: "pkg/api/api.go", Package: "api", Exported: true, Line: 10,
		Metadata: map[string]string{"signature": "func Handle(w http.ResponseWriter, r *http.Request)"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	changes, err := st.GetSignatureChanges("pkg/api/api.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	c := changes[0]
	if c.Name != "Handle" {
		t.Errorf("expected Name=Handle, got %q", c.Name)
	}
	if c.OldSig != "func Handle(w http.ResponseWriter)" {
		t.Errorf("unexpected OldSig: %q", c.OldSig)
	}
	if c.NewSig != "func Handle(w http.ResponseWriter, r *http.Request)" {
		t.Errorf("unexpected NewSig: %q", c.NewSig)
	}
}

// TestGetSignatureChanges_UnchangedSignature verifies that an exported function
// whose signature did NOT change produces no entries.
func TestGetSignatureChanges_UnchangedSignature(t *testing.T) {
	st := openTestStore(t)

	g1 := graph.New("test-repo")
	id := g1.MakeNodeID("pkg/svc/svc.go", "Run")
	g1.AddNode(&graph.Node{
		ID: id, Name: "Run", Type: graph.NodeFunction,
		File: "pkg/svc/svc.go", Package: "svc", Exported: true, Line: 1,
		Metadata: map[string]string{"signature": "func Run() error"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	// Second save: same signature.
	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: id, Name: "Run", Type: graph.NodeFunction,
		File: "pkg/svc/svc.go", Package: "svc", Exported: true, Line: 1,
		Metadata: map[string]string{"signature": "func Run() error"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	changes, err := st.GetSignatureChanges("pkg/svc/svc.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected 0 changes for unchanged signature, got %d", len(changes))
	}
}

// TestGetSignatureChanges_NewNode verifies that a brand-new exported node
// (not present in the prior SaveGraph) does NOT appear as a change.
func TestGetSignatureChanges_NewNode(t *testing.T) {
	st := openTestStore(t)

	// First save: empty graph.
	if err := st.SaveGraph(graph.New("test-repo")); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	// Second save: new node added.
	g2 := graph.New("test-repo")
	id := g2.MakeNodeID("pkg/new/new.go", "New")
	g2.AddNode(&graph.Node{
		ID: id, Name: "New", Type: graph.NodeFunction,
		File: "pkg/new/new.go", Package: "new", Exported: true, Line: 1,
		Metadata: map[string]string{"signature": "func New() *Client"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	changes, err := st.GetSignatureChanges("pkg/new/new.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected 0 changes for new node (no prior signature), got %d", len(changes))
	}
}

// TestGetSignatureChanges_UnexportedIgnored verifies that unexported entities
// are never returned even if their signature changed.
func TestGetSignatureChanges_UnexportedIgnored(t *testing.T) {
	st := openTestStore(t)

	g1 := graph.New("test-repo")
	id := g1.MakeNodeID("pkg/internal/helper.go", "helper")
	g1.AddNode(&graph.Node{
		ID: id, Name: "helper", Type: graph.NodeFunction,
		File: "pkg/internal/helper.go", Package: "internal", Exported: false, Line: 1,
		Metadata: map[string]string{"signature": "func helper(x int)"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: id, Name: "helper", Type: graph.NodeFunction,
		File: "pkg/internal/helper.go", Package: "internal", Exported: false, Line: 1,
		Metadata: map[string]string{"signature": "func helper(x int, y string)"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	changes, err := st.GetSignatureChanges("pkg/internal/helper.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected 0 changes for unexported entity, got %d", len(changes))
	}
}

// TestGetSignatureChanges_StructChanged verifies struct type changes are tracked.
func TestGetSignatureChanges_StructChanged(t *testing.T) {
	st := openTestStore(t)

	g1 := graph.New("test-repo")
	id := g1.MakeNodeID("pkg/config/config.go", "Config")
	g1.AddNode(&graph.Node{
		ID: id, Name: "Config", Type: graph.NodeStruct,
		File: "pkg/config/config.go", Package: "config", Exported: true, Line: 5,
		Metadata: map[string]string{"signature": "type Config struct { Host string }"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: id, Name: "Config", Type: graph.NodeStruct,
		File: "pkg/config/config.go", Package: "config", Exported: true, Line: 5,
		Metadata: map[string]string{"signature": "type Config struct { Host string; Port int }"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	changes, err := st.GetSignatureChanges("pkg/config/config.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change for struct, got %d", len(changes))
	}
	if changes[0].NodeType != "struct" {
		t.Errorf("expected type=struct, got %q", changes[0].NodeType)
	}
}

// TestGetSignatureChanges_WrongFileReturnsEmpty verifies that querying a
// different file returns nothing even if another file has changes.
func TestGetSignatureChanges_WrongFileReturnsEmpty(t *testing.T) {
	st := openTestStore(t)

	g1 := graph.New("test-repo")
	id := g1.MakeNodeID("pkg/a/a.go", "Func")
	g1.AddNode(&graph.Node{
		ID: id, Name: "Func", Type: graph.NodeFunction,
		File: "pkg/a/a.go", Package: "a", Exported: true, Line: 1,
		Metadata: map[string]string{"signature": "func Func() int"},
	})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{
		ID: id, Name: "Func", Type: graph.NodeFunction,
		File: "pkg/a/a.go", Package: "a", Exported: true, Line: 1,
		Metadata: map[string]string{"signature": "func Func() string"},
	})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	// Query a different file — should return nothing.
	changes, err := st.GetSignatureChanges("pkg/b/b.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected 0 changes for unrelated file, got %d", len(changes))
	}
}

// TestGetSignatureChanges_EmptyStore verifies graceful return on empty DB.
func TestGetSignatureChanges_EmptyStore(t *testing.T) {
	st := openTestStore(t)
	changes, err := st.GetSignatureChanges("any/file.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges on empty store: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected 0 changes on empty store, got %d", len(changes))
	}
}

// TestGetSignatureChanges_MultipleChangesInFile verifies multiple changed
// entities in a single file are all returned.
func TestGetSignatureChanges_MultipleChangesInFile(t *testing.T) {
	st := openTestStore(t)

	g1 := graph.New("test-repo")
	idA := g1.MakeNodeID("pkg/multi/multi.go", "FuncA")
	idB := g1.MakeNodeID("pkg/multi/multi.go", "FuncB")
	g1.AddNode(&graph.Node{ID: idA, Name: "FuncA", Type: graph.NodeFunction, File: "pkg/multi/multi.go", Package: "multi", Exported: true, Line: 1, Metadata: map[string]string{"signature": "func FuncA() int"}})
	g1.AddNode(&graph.Node{ID: idB, Name: "FuncB", Type: graph.NodeFunction, File: "pkg/multi/multi.go", Package: "multi", Exported: true, Line: 5, Metadata: map[string]string{"signature": "func FuncB() string"}})
	if err := st.SaveGraph(g1); err != nil {
		t.Fatalf("SaveGraph 1: %v", err)
	}

	g2 := graph.New("test-repo")
	g2.AddNode(&graph.Node{ID: idA, Name: "FuncA", Type: graph.NodeFunction, File: "pkg/multi/multi.go", Package: "multi", Exported: true, Line: 1, Metadata: map[string]string{"signature": "func FuncA(n int) int"}})
	g2.AddNode(&graph.Node{ID: idB, Name: "FuncB", Type: graph.NodeFunction, File: "pkg/multi/multi.go", Package: "multi", Exported: true, Line: 5, Metadata: map[string]string{"signature": "func FuncB(s string) string"}})
	if err := st.SaveGraph(g2); err != nil {
		t.Fatalf("SaveGraph 2: %v", err)
	}

	changes, err := st.GetSignatureChanges("pkg/multi/multi.go")
	if err != nil {
		t.Fatalf("GetSignatureChanges: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(changes))
	}
}

// TestGetSignatureChanges_UsesStoreNotExposedToPublic is a compile-time check
// that SignatureChange fields are accessible.
func TestGetSignatureChanges_FieldsAccessible(t *testing.T) {
	var sc store.SignatureChange
	_ = sc.NodeID
	_ = sc.Name
	_ = sc.NodeType
	_ = sc.File
	_ = sc.Line
	_ = sc.OldSig
	_ = sc.NewSig
}
