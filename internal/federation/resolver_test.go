package federation_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// createSiblingStore creates a temp store with some nodes, simulating a sibling
// project's index. Returns the DB path.
func createSiblingStore(t *testing.T, repoID string, nodes []*graph.Node) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open sibling store: %v", err)
	}
	defer st.Close()

	g := graph.New(repoID)
	for _, n := range nodes {
		g.AddNode(n)
	}
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("save sibling graph: %v", err)
	}
	return dbPath
}

// createSiblingWithDefaultPath creates a sibling store at the path that
// store.DefaultPath would derive for projectDir. This makes the resolver
// able to discover it via SiblingDBPath.
func createSiblingWithDefaultPath(t *testing.T, projectDir string, repoID string, nodes []*graph.Node) {
	t.Helper()
	dbPath, err := federation.SiblingDBPath(projectDir)
	if err != nil {
		t.Fatalf("SiblingDBPath: %v", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store at default path: %v", err)
	}
	defer st.Close()

	g := graph.New(repoID)
	for _, n := range nodes {
		g.AddNode(n)
	}
	if err := st.SaveGraph(g); err != nil {
		t.Fatalf("save graph: %v", err)
	}
}

// sampleNodesFor returns nodes with IDs prefixed by repoID so SaveGraph persists them.
func sampleNodesFor(repoID string) []*graph.Node {
	return []*graph.Node{
		{ID: graph.NodeID(repoID + "::pkg/auth.go::AuthService"), Name: "AuthService", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 10, Exported: true},
		{ID: graph.NodeID(repoID + "::pkg/auth.go::Validate"), Name: "Validate", Type: graph.NodeMethod, File: "pkg/auth.go", Line: 50, Exported: true},
		{ID: graph.NodeID(repoID + "::pkg/db.go::Connect"), Name: "Connect", Type: graph.NodeFunction, File: "pkg/db.go", Line: 5, Exported: true},
	}
}

// ── Status tests ────────────────────────────────────────────────────────────

func TestResolverStatus_NotFound(t *testing.T) {
	r := federation.NewResolver([]config.FederationEntry{
		{Path: "/nonexistent/path/to/project", Alias: "missing"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Status != "not_found" {
		t.Errorf("expected not_found, got %q", statuses[0].Status)
	}
	if statuses[0].Alias != "missing" {
		t.Errorf("expected alias 'missing', got %q", statuses[0].Alias)
	}
}

func TestResolverStatus_NotIndexed(t *testing.T) {
	// Directory exists but no store DB.
	dir := t.TempDir()
	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "empty"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Status != "not_indexed" {
		t.Errorf("expected not_indexed, got %q", statuses[0].Status)
	}
}

func TestResolverStatus_Indexed(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling-project", sampleNodesFor("sibling-project"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Status != "indexed" {
		t.Errorf("expected indexed, got %q", statuses[0].Status)
	}
	if statuses[0].NodeCount != 3 {
		t.Errorf("expected 3 nodes, got %d", statuses[0].NodeCount)
	}
	if statuses[0].Alias != "sibling" {
		t.Errorf("expected alias 'sibling', got %q", statuses[0].Alias)
	}
}

func TestResolverStatus_MultipleSiblings(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	createSiblingWithDefaultPath(t, dir1, "proj-a", sampleNodesFor("proj-a"))
	// dir2 has no store — not indexed

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir1, Alias: "a"},
		{Path: dir2, Alias: "b"},
		{Path: "/nonexistent", Alias: "c"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status()
	if len(statuses) != 3 {
		t.Fatalf("expected 3 statuses, got %d", len(statuses))
	}
	if statuses[0].Status != "indexed" {
		t.Errorf("a: expected indexed, got %q", statuses[0].Status)
	}
	if statuses[1].Status != "not_indexed" {
		t.Errorf("b: expected not_indexed, got %q", statuses[1].Status)
	}
	if statuses[2].Status != "not_found" {
		t.Errorf("c: expected not_found, got %q", statuses[2].Status)
	}
}

// ── EntityExists tests ──────────────────────────────────────────────────────

func TestEntityExists_Found(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	if !r.EntityExists("sibling", "AuthService") {
		t.Error("expected AuthService to exist")
	}
}

func TestEntityExists_NotFound(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	if r.EntityExists("sibling", "NonExistent") {
		t.Error("expected NonExistent to not exist")
	}
}

func TestEntityExists_UnknownAlias(t *testing.T) {
	r := federation.NewResolver(nil, t.TempDir())
	defer r.Close()

	// Should return false for unknown alias — fail-open.
	if r.EntityExists("unknown", "anything") {
		t.Error("expected false for unknown alias")
	}
}

func TestEntityExists_BrokenStore(t *testing.T) {
	// Point to a directory that exists but has no valid store.
	dir := t.TempDir()

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "broken"},
	}, t.TempDir())
	defer r.Close()

	// Fail-open: returns false, no panic.
	if r.EntityExists("broken", "anything") {
		t.Error("expected false for unindexed sibling")
	}
}

// ── FindEntities tests ──────────────────────────────────────────────────────

func TestFindEntities_CrossProject(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	results := r.FindEntities("AuthService", nil, 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result group, got %d", len(results))
	}
	if results[0].Alias != "sibling" {
		t.Errorf("expected alias 'sibling', got %q", results[0].Alias)
	}
	if len(results[0].Results) != 1 {
		t.Errorf("expected 1 match, got %d", len(results[0].Results))
	}
}

func TestFindEntities_FilterByAlias(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	createSiblingWithDefaultPath(t, dir1, "a", sampleNodesFor("a"))
	createSiblingWithDefaultPath(t, dir2, "b", sampleNodesFor("b"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir1, Alias: "a"},
		{Path: dir2, Alias: "b"},
	}, t.TempDir())
	defer r.Close()

	// Search only in alias "a".
	results := r.FindEntities("Connect", []string{"a"}, 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result group (only 'a'), got %d", len(results))
	}
	if results[0].Alias != "a" {
		t.Errorf("expected alias 'a', got %q", results[0].Alias)
	}
}

func TestFindEntities_NoMatch(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	results := r.FindEntities("NonExistentEntity", nil, 10)
	if len(results) != 0 {
		t.Errorf("expected 0 result groups, got %d", len(results))
	}
}

func TestFindEntities_BrokenSibling_Skipped(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "good", sampleNodesFor("good"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: "/nonexistent/broken", Alias: "broken"},
		{Path: dir, Alias: "good"},
	}, t.TempDir())
	defer r.Close()

	// Broken sibling should be silently skipped.
	results := r.FindEntities("AuthService", nil, 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result group (only 'good'), got %d", len(results))
	}
	if results[0].Alias != "good" {
		t.Errorf("expected alias 'good', got %q", results[0].Alias)
	}
}

// ── InvalidateCache tests ───────────────────────────────────────────────────

func TestInvalidateCache_ClearsState(t *testing.T) {
	r := federation.NewResolver(nil, t.TempDir())
	defer r.Close()

	// Should not panic even with empty resolver.
	r.InvalidateCache()
}

// ── Aliases / HasAlias tests ────────────────────────────────────────────────

func TestAliases(t *testing.T) {
	r := federation.NewResolver([]config.FederationEntry{
		{Path: "/a", Alias: "alpha"},
		{Path: "/b", Alias: "beta"},
	}, t.TempDir())
	defer r.Close()

	aliases := r.Aliases()
	if len(aliases) != 2 {
		t.Fatalf("expected 2 aliases, got %d", len(aliases))
	}
	if aliases[0] != "alpha" || aliases[1] != "beta" {
		t.Errorf("unexpected aliases: %v", aliases)
	}
}

func TestHasAlias(t *testing.T) {
	r := federation.NewResolver([]config.FederationEntry{
		{Path: "/a", Alias: "alpha"},
	}, t.TempDir())
	defer r.Close()

	if !r.HasAlias("alpha") {
		t.Error("expected HasAlias('alpha') = true")
	}
	if r.HasAlias("beta") {
		t.Error("expected HasAlias('beta') = false")
	}
}

// ── Config validation tests ─────────────────────────────────────────────────

func TestConfigValidation_DuplicateAlias(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"version": "1",
		"federation": [
			{"path": "../a", "alias": "dup"},
			{"path": "../b", "alias": "dup"}
		]
	}`
	cfgPath := filepath.Join(dir, "synapses.json")
	if err := writeFile(cfgPath, cfgJSON); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for duplicate federation alias")
	}
}

func TestConfigValidation_EmptyAlias(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"version": "1",
		"federation": [
			{"path": "../a", "alias": ""}
		]
	}`
	cfgPath := filepath.Join(dir, "synapses.json")
	if err := writeFile(cfgPath, cfgJSON); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for empty federation alias")
	}
}

func TestConfigValidation_WhitespaceAlias(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"version": "1",
		"federation": [
			{"path": "../a", "alias": "has space"}
		]
	}`
	cfgPath := filepath.Join(dir, "synapses.json")
	if err := writeFile(cfgPath, cfgJSON); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for alias with whitespace")
	}
}

func TestConfigValidation_ValidFederation(t *testing.T) {
	dir := t.TempDir()
	cfgJSON := `{
		"version": "1",
		"federation": [
			{"path": "../a", "alias": "alpha"},
			{"path": "../b", "alias": "beta"}
		]
	}`
	cfgPath := filepath.Join(dir, "synapses.json")
	if err := writeFile(cfgPath, cfgJSON); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if len(cfg.Federation) != 2 {
		t.Errorf("expected 2 federation entries, got %d", len(cfg.Federation))
	}
	// Paths should be resolved to absolute.
	if !filepath.IsAbs(cfg.Federation[0].Path) {
		t.Errorf("expected absolute path, got %q", cfg.Federation[0].Path)
	}
}

// ── Store.NodeExistsByName / FindNodesByName tests ──────────────────────────

func TestNodeExistsByName(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	g := graph.New("test")
	g.AddNode(&graph.Node{ID: "test::main.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "main.go"})
	if err := st.SaveGraph(g); err != nil {
		t.Fatal(err)
	}

	exists, err := st.NodeExistsByName("Foo")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected Foo to exist")
	}

	exists, err = st.NodeExistsByName("Bar")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("expected Bar to not exist")
	}

	// Case insensitive.
	exists, err = st.NodeExistsByName("foo")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected case-insensitive match for 'foo'")
	}
}

func TestFindNodesByName(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	g := graph.New("test")
	g.AddNode(&graph.Node{ID: "test::a.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "a.go"})
	g.AddNode(&graph.Node{ID: "test::b.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "b.go"})
	g.AddNode(&graph.Node{ID: "test::c.go::Bar", Name: "Bar", Type: graph.NodeFunction, File: "c.go"})
	if err := st.SaveGraph(g); err != nil {
		t.Fatal(err)
	}

	results, err := st.FindNodesByName("Foo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results for Foo, got %d", len(results))
	}

	results, err = st.FindNodesByName("NotThere", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for NotThere, got %d", len(results))
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
