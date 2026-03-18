package federation_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// createSiblingWithDefaultPath creates a sibling store at the path that
// store.DefaultPath would derive for projectDir. This makes the resolver
// able to discover it via SiblingDBPath.
func createSiblingWithDefaultPath(t *testing.T, projectDir, repoID string, nodes []*graph.Node) {
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

func sampleNodesFor(repoID string) []*graph.Node {
	return []*graph.Node{
		{ID: graph.NodeID(repoID + "::pkg/auth.go::AuthService"), Name: "AuthService", Type: graph.NodeFunction, File: "pkg/auth.go", Line: 10, Exported: true},
		{ID: graph.NodeID(repoID + "::pkg/auth.go::Server.Validate"), Name: "Server.Validate", Type: graph.NodeMethod, File: "pkg/auth.go", Line: 50, Exported: true},
		{ID: graph.NodeID(repoID + "::pkg/db.go::Connect"), Name: "Connect", Type: graph.NodeFunction, File: "pkg/db.go", Line: 5, Exported: true},
	}
}

func bg() context.Context { return context.Background() }

// ── Status tests ────────────────────────────────────────────────────────────

func TestResolverStatus_NotFound(t *testing.T) {
	r := federation.NewResolver([]config.FederationEntry{
		{Path: "/nonexistent/path/to/project", Alias: "missing"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status(bg())
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
	dir := t.TempDir()
	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "empty"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status(bg())
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

	statuses := r.Status(bg())
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

func TestResolverStatus_Stale(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "old", sampleNodesFor("old"))

	// Override nowFunc to simulate 48 hours in the future.
	original := federation.ExportNowFunc()
	federation.SetNowFunc(func() time.Time {
		return time.Now().Add(48 * time.Hour)
	})
	defer federation.SetNowFunc(original)

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "old"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status(bg())
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Status != "stale" {
		t.Errorf("expected stale, got %q", statuses[0].Status)
	}
}

func TestResolverStatus_Incompatible(t *testing.T) {
	dir := t.TempDir()
	// Create a DB file that has no 'nodes' or 'meta' tables — just junk.
	dbPath, err := federation.SiblingDBPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Create a valid SQLite DB but with no synapses tables.
	db, err := store.Open(filepath.Join(t.TempDir(), "dummy.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Write an empty SQLite file at the expected path.
	if err := os.WriteFile(dbPath, []byte("SQLite format 3\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "incompat"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status(bg())
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Status != "incompatible" {
		t.Errorf("expected incompatible, got %q (err: %s)", statuses[0].Status, statuses[0].Error)
	}
}

func TestResolverStatus_MultipleSiblings(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	createSiblingWithDefaultPath(t, dir1, "proj-a", sampleNodesFor("proj-a"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir1, Alias: "a"},
		{Path: dir2, Alias: "b"},
		{Path: "/nonexistent", Alias: "c"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status(bg())
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

func TestResolverStatus_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(bg())
	cancel() // immediately cancelled

	r := federation.NewResolver([]config.FederationEntry{
		{Path: "/a", Alias: "a"},
	}, t.TempDir())
	defer r.Close()

	statuses := r.Status(ctx)
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Error != "timeout" {
		t.Errorf("expected timeout error, got %q", statuses[0].Error)
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

	if !r.EntityExists(bg(), "sibling", "AuthService") {
		t.Error("expected AuthService to exist")
	}
}

func TestEntityExists_QualifiedName(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	// "Validate" should match "Server.Validate" (suffix after last dot).
	if !r.EntityExists(bg(), "sibling", "Validate") {
		t.Error("expected Validate to match Server.Validate via suffix matching")
	}
}

func TestEntityExists_NotFound(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	if r.EntityExists(bg(), "sibling", "NonExistent") {
		t.Error("expected NonExistent to not exist")
	}
}

func TestEntityExists_UnknownAlias(t *testing.T) {
	r := federation.NewResolver(nil, t.TempDir())
	defer r.Close()

	if r.EntityExists(bg(), "unknown", "anything") {
		t.Error("expected false for unknown alias")
	}
}

func TestEntityExists_BrokenStore_FailOpen(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "broken"},
	}, t.TempDir())
	defer r.Close()

	// Fail-open: returns false, no panic.
	if r.EntityExists(bg(), "broken", "anything") {
		t.Error("expected false for unindexed sibling")
	}
}

func TestEntityExists_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	ctx, cancel := context.WithCancel(bg())
	cancel()

	// Should return false when context is cancelled — fail-open.
	if r.EntityExists(ctx, "sibling", "AuthService") {
		t.Error("expected false when context cancelled")
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

	results := r.FindEntities(bg(), "AuthService", nil, 10)
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

func TestFindEntities_QualifiedSuffix(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "sibling"},
	}, t.TempDir())
	defer r.Close()

	// "Validate" should find "Server.Validate" via suffix matching.
	results := r.FindEntities(bg(), "Validate", nil, 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result group, got %d", len(results))
	}
	if len(results[0].Results) != 1 {
		t.Errorf("expected 1 match for suffix 'Validate', got %d", len(results[0].Results))
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

	results := r.FindEntities(bg(), "Connect", []string{"a"}, 10)
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

	results := r.FindEntities(bg(), "NonExistentEntity", nil, 10)
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

	results := r.FindEntities(bg(), "AuthService", nil, 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result group (only 'good'), got %d", len(results))
	}
	if results[0].Alias != "good" {
		t.Errorf("expected alias 'good', got %q", results[0].Alias)
	}
}

// ── GetDepsForEntity tests ──────────────────────────────────────────────────

func TestGetDepsForEntity_NoDeps(t *testing.T) {
	st := openTestStore(t)

	r := federation.NewResolver(nil, t.TempDir())
	defer r.Close()

	deps := r.GetDepsForEntity(bg(), "some::entity", st)
	if deps != nil {
		t.Errorf("expected nil deps, got %v", deps)
	}
}

func TestGetDepsForEntity_WithDeps(t *testing.T) {
	st := openTestStore(t)

	// Insert a cross-project dep.
	err := st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity:     "local::main.go::Handler",
		ToProject:      "core",
		ToEntity:       "AuthService",
		ToFile:         "pkg/auth.go",
		VerifiedCommit: "abc123",
		VerifiedAt:     time.Now().UTC().Format(time.RFC3339),
		DetectionTier:  "tier1",
	})
	if err != nil {
		t.Fatal(err)
	}

	r := federation.NewResolver(nil, t.TempDir())
	defer r.Close()

	deps := r.GetDepsForEntity(bg(), "local::main.go::Handler", st)
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].Project != "core" {
		t.Errorf("expected project 'core', got %q", deps[0].Project)
	}
	if deps[0].Entity != "AuthService" {
		t.Errorf("expected entity 'AuthService', got %q", deps[0].Entity)
	}
}

func TestGetDepsForEntity_NilStore(t *testing.T) {
	r := federation.NewResolver(nil, t.TempDir())
	defer r.Close()

	// Nil store should return nil — fail-open.
	deps := r.GetDepsForEntity(bg(), "entity", nil)
	if deps != nil {
		t.Errorf("expected nil for nil store, got %v", deps)
	}
}

func TestGetDepsForEntity_CancelledContext(t *testing.T) {
	st := openTestStore(t)

	r := federation.NewResolver(nil, t.TempDir())
	defer r.Close()

	ctx, cancel := context.WithCancel(bg())
	cancel()

	deps := r.GetDepsForEntity(ctx, "entity", st)
	if deps != nil {
		t.Errorf("expected nil for cancelled context, got %v", deps)
	}
}

// ── InvalidateCache tests ───────────────────────────────────────────────────

func TestInvalidateCache_ClearsState(t *testing.T) {
	r := federation.NewResolver(nil, t.TempDir())
	defer r.Close()

	r.InvalidateCache() // should not panic
}

func TestInvalidateCache_RetriesFailedStores(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver([]config.FederationEntry{
		{Path: dir, Alias: "retry"},
	}, t.TempDir())
	defer r.Close()

	// First call fails — no store exists.
	if r.EntityExists(bg(), "retry", "Foo") {
		t.Error("expected false on first attempt")
	}

	// Create the store now.
	createSiblingWithDefaultPath(t, dir, "retry", sampleNodesFor("retry"))

	// Without InvalidateCache, the error is cached — still fails.
	if r.EntityExists(bg(), "retry", "AuthService") {
		t.Error("expected false — error should still be cached")
	}

	// After cache reset, it should find the entity.
	r.InvalidateCache()
	if !r.EntityExists(bg(), "retry", "AuthService") {
		t.Error("expected true after InvalidateCache + store creation")
	}
}

// ── Aliases / HasAlias tests ────────────────────────────────────────────────

func TestAliases(t *testing.T) {
	r := federation.NewResolver([]config.FederationEntry{
		{Path: "/a", Alias: "alpha"},
		{Path: "/b", Alias: "beta"},
	}, t.TempDir())
	defer r.Close()

	aliases := r.Aliases()
	if len(aliases) != 2 || aliases[0] != "alpha" || aliases[1] != "beta" {
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
	writeFile(t, filepath.Join(dir, "synapses.json"), `{
		"version": "1",
		"federation": [
			{"path": "../a", "alias": "dup"},
			{"path": "../b", "alias": "dup"}
		]
	}`)

	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for duplicate federation alias")
	}
}

func TestConfigValidation_EmptyAlias(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{
		"version": "1",
		"federation": [{"path": "../a", "alias": ""}]
	}`)

	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for empty federation alias")
	}
}

func TestConfigValidation_WhitespaceAlias(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{
		"version": "1",
		"federation": [{"path": "../a", "alias": "has space"}]
	}`)

	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for alias with whitespace")
	}
}

func TestConfigValidation_EmptyPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{
		"version": "1",
		"federation": [{"path": "", "alias": "nopath"}]
	}`)

	_, err := config.Load(dir)
	if err == nil {
		t.Error("expected error for empty federation path")
	}
}

func TestConfigValidation_ValidFederation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{
		"version": "1",
		"federation": [
			{"path": "../a", "alias": "alpha"},
			{"path": "../b", "alias": "beta"}
		]
	}`)

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
	if len(cfg.Federation) != 2 {
		t.Errorf("expected 2 federation entries, got %d", len(cfg.Federation))
	}
	if !filepath.IsAbs(cfg.Federation[0].Path) {
		t.Errorf("expected absolute path, got %q", cfg.Federation[0].Path)
	}
}

// ── Store.NodeExistsByName / FindNodesByName tests ──────────────────────────

func TestNodeExistsByName_ExactMatch(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::main.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "main.go"})

	exists, err := st.NodeExistsByName("Foo")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected Foo to exist")
	}
}

func TestNodeExistsByName_CaseInsensitive(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::main.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "main.go"})

	exists, err := st.NodeExistsByName("foo")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected case-insensitive match for 'foo'")
	}
}

func TestNodeExistsByName_QualifiedSuffix(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::main.go::Store.Close", Name: "Store.Close", Type: graph.NodeMethod, File: "main.go"})

	// "Close" should match "Store.Close" via suffix.
	exists, err := st.NodeExistsByName("Close")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected 'Close' to match 'Store.Close' via suffix")
	}
}

func TestNodeExistsByName_NotFound(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::main.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "main.go"})

	exists, err := st.NodeExistsByName("Bar")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("expected Bar to not exist")
	}
}

func TestFindNodesByName_MultipleMatches(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test",
		&graph.Node{ID: "test::a.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "a.go"},
		&graph.Node{ID: "test::b.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "b.go"},
		&graph.Node{ID: "test::c.go::Bar", Name: "Bar", Type: graph.NodeFunction, File: "c.go"},
	)

	results, err := st.FindNodesByName("Foo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results for Foo, got %d", len(results))
	}
}

func TestFindNodesByName_QualifiedSuffix(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test",
		&graph.Node{ID: "test::a.go::Server.Handle", Name: "Server.Handle", Type: graph.NodeMethod, File: "a.go"},
		&graph.Node{ID: "test::b.go::Handle", Name: "Handle", Type: graph.NodeFunction, File: "b.go"},
	)

	results, err := st.FindNodesByName("Handle", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results (exact + suffix), got %d", len(results))
	}
}

func TestFindNodesByName_NoMatch(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::a.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "a.go"})

	results, err := st.FindNodesByName("NotThere", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// ── Store cross_project_deps CRUD tests ─────────────────────────────────────

func TestCrossProjectDeps_UpsertAndGet(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)

	dep := store.CrossProjectDep{
		FromEntity:     "local::main.go::Handler",
		ToProject:      "core",
		ToEntity:       "AuthService",
		ToFile:         "pkg/auth.go",
		VerifiedCommit: "abc123",
		VerifiedAt:     now,
		DetectionTier:  "tier1",
	}
	if err := st.UpsertCrossProjectDep(dep); err != nil {
		t.Fatal(err)
	}

	deps, err := st.GetCrossProjectDeps("local::main.go::Handler")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].ToProject != "core" || deps[0].ToEntity != "AuthService" {
		t.Errorf("unexpected dep: %+v", deps[0])
	}
}

func TestCrossProjectDeps_UpsertOverwrite(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)

	dep := store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "core", ToEntity: "AuthService",
		ToFile: "pkg/auth.go", VerifiedCommit: "abc123", VerifiedAt: now, DetectionTier: "tier1",
	}
	if err := st.UpsertCrossProjectDep(dep); err != nil {
		t.Fatal(err)
	}

	// Update the commit.
	dep.VerifiedCommit = "def456"
	if err := st.UpsertCrossProjectDep(dep); err != nil {
		t.Fatal(err)
	}

	deps, err := st.GetCrossProjectDeps("local::main.go::Handler")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep after upsert, got %d", len(deps))
	}
	if deps[0].VerifiedCommit != "def456" {
		t.Errorf("expected updated commit 'def456', got %q", deps[0].VerifiedCommit)
	}
}

func TestCrossProjectDeps_GetByProject(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)

	for _, e := range []string{"A", "B"} {
		st.UpsertCrossProjectDep(store.CrossProjectDep{
			FromEntity: "local::x.go::" + e, ToProject: "core", ToEntity: e,
			ToFile: "f.go", VerifiedCommit: "aaa", VerifiedAt: now, DetectionTier: "tier1",
		})
	}
	st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::y.go::C", ToProject: "other", ToEntity: "C",
		ToFile: "g.go", VerifiedCommit: "bbb", VerifiedAt: now, DetectionTier: "tier1",
	})

	deps, err := st.GetCrossProjectDepsByProject("core")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 {
		t.Errorf("expected 2 deps for project 'core', got %d", len(deps))
	}
}

func TestCrossProjectDeps_Delete(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)

	st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "core", ToEntity: "Auth",
		ToFile: "a.go", VerifiedCommit: "aaa", VerifiedAt: now, DetectionTier: "tier1",
	})

	if err := st.DeleteCrossProjectDeps("local::main.go::Handler"); err != nil {
		t.Fatal(err)
	}

	deps, err := st.GetCrossProjectDeps("local::main.go::Handler")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 deps after delete, got %d", len(deps))
	}
}

func TestCrossProjectDeps_UpdateVerifiedCommit(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)

	st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "core", ToEntity: "Auth",
		ToFile: "a.go", VerifiedCommit: "old", VerifiedAt: now, DetectionTier: "tier1",
	})

	if err := st.UpdateVerifiedCommit("core", "Auth", "new"); err != nil {
		t.Fatal(err)
	}

	deps, err := st.GetCrossProjectDeps("local::main.go::Handler")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].VerifiedCommit != "new" {
		t.Errorf("expected commit 'new', got %q", deps[0].VerifiedCommit)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func saveNodes(t *testing.T, st *store.Store, repoID string, nodes ...*graph.Node) {
	t.Helper()
	g := graph.New(repoID)
	for _, n := range nodes {
		g.AddNode(n)
	}
	if err := st.SaveGraph(g); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
