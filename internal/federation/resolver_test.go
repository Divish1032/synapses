package federation_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── helpers ─────────────────────────────────────────────────────────────────

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

func newResolver(entries []config.FederationEntry) *federation.Resolver {
	return federation.NewResolver(entries, "/tmp")
}

func newResolverStale(entries []config.FederationEntry) *federation.Resolver {
	return federation.NewResolverWithClock(entries, "/tmp", func() time.Time {
		return time.Now().Add(48 * time.Hour)
	})
}

// ── Status tests ────────────────────────────────────────────────────────────

func TestResolverStatus_NotFound(t *testing.T) {
	r := newResolver([]config.FederationEntry{
		{Path: "/nonexistent/path/to/project", Alias: "missing"},
	})
	defer r.Close()

	statuses := r.Status(bg())
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].Status != "not_found" {
		t.Errorf("expected not_found, got %q", statuses[0].Status)
	}
}

func TestResolverStatus_NotIndexed(t *testing.T) {
	dir := t.TempDir()
	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "empty"}})
	defer r.Close()

	statuses := r.Status(bg())
	if statuses[0].Status != "not_indexed" {
		t.Errorf("expected not_indexed, got %q", statuses[0].Status)
	}
}

func TestResolverStatus_Indexed(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling-project", sampleNodesFor("sibling-project"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "sibling"}})
	defer r.Close()

	statuses := r.Status(bg())
	if statuses[0].Status != "indexed" {
		t.Errorf("expected indexed, got %q", statuses[0].Status)
	}
	if statuses[0].NodeCount != 3 {
		t.Errorf("expected 3 nodes, got %d", statuses[0].NodeCount)
	}
}

func TestResolverStatus_Stale(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "old", sampleNodesFor("old"))

	// Clock is 48h in the future — store appears stale.
	r := newResolverStale([]config.FederationEntry{{Path: dir, Alias: "old"}})
	defer r.Close()

	statuses := r.Status(bg())
	if statuses[0].Status != "stale" {
		t.Errorf("expected stale, got %q", statuses[0].Status)
	}
}

func TestResolverStatus_Incompatible(t *testing.T) {
	dir := t.TempDir()
	dbPath, err := federation.SiblingDBPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Write a valid SQLite header but no synapses tables.
	if err := os.WriteFile(dbPath, []byte("SQLite format 3\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "incompat"}})
	defer r.Close()

	statuses := r.Status(bg())
	if statuses[0].Status != "incompatible" {
		t.Errorf("expected incompatible, got %q (err: %s)", statuses[0].Status, statuses[0].Error)
	}
}

func TestResolverStatus_MultipleSiblings(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	createSiblingWithDefaultPath(t, dir1, "proj-a", sampleNodesFor("proj-a"))

	r := newResolver([]config.FederationEntry{
		{Path: dir1, Alias: "a"},
		{Path: dir2, Alias: "b"},
		{Path: "/nonexistent", Alias: "c"},
	})
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
	cancel()

	r := newResolver([]config.FederationEntry{{Path: "/a", Alias: "a"}})
	defer r.Close()

	statuses := r.Status(ctx)
	if statuses[0].Error != "timeout" {
		t.Errorf("expected timeout error, got %q", statuses[0].Error)
	}
}

// ── EntityExists tests ──────────────────────────────────────────────────────

func TestEntityExists_Found(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "sibling"}})
	defer r.Close()

	if !r.EntityExists(bg(), "sibling", "AuthService") {
		t.Error("expected AuthService to exist")
	}
}

func TestEntityExists_QualifiedName(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "sibling"}})
	defer r.Close()

	// "Validate" should match "Server.Validate" via suffix matching.
	if !r.EntityExists(bg(), "sibling", "Validate") {
		t.Error("expected Validate to match Server.Validate via suffix matching")
	}
}

func TestEntityExists_NotFound(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "sibling"}})
	defer r.Close()

	if r.EntityExists(bg(), "sibling", "NonExistent") {
		t.Error("expected NonExistent to not exist")
	}
}

func TestEntityExists_UnknownAlias(t *testing.T) {
	r := newResolver(nil)
	defer r.Close()
	if r.EntityExists(bg(), "unknown", "anything") {
		t.Error("expected false for unknown alias")
	}
}

func TestEntityExists_BrokenStore_FailOpen(t *testing.T) {
	dir := t.TempDir()
	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "broken"}})
	defer r.Close()

	if r.EntityExists(bg(), "broken", "anything") {
		t.Error("expected false for unindexed sibling")
	}
}

func TestEntityExists_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "sibling"}})
	defer r.Close()

	ctx, cancel := context.WithCancel(bg())
	cancel()

	if r.EntityExists(ctx, "sibling", "AuthService") {
		t.Error("expected false when context cancelled")
	}
}

// ── FindEntities tests ──────────────────────────────────────────────────────

func TestFindEntities_CrossProject(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "sibling"}})
	defer r.Close()

	results := r.FindEntities(bg(), "AuthService", nil, 10)
	if len(results) != 1 || results[0].Alias != "sibling" || len(results[0].Results) != 1 {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestFindEntities_QualifiedSuffix(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "sibling"}})
	defer r.Close()

	results := r.FindEntities(bg(), "Validate", nil, 10)
	if len(results) != 1 || len(results[0].Results) != 1 {
		t.Fatalf("expected suffix match for 'Validate', got %+v", results)
	}
}

func TestFindEntities_FilterByAlias(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	createSiblingWithDefaultPath(t, dir1, "a", sampleNodesFor("a"))
	createSiblingWithDefaultPath(t, dir2, "b", sampleNodesFor("b"))

	r := newResolver([]config.FederationEntry{
		{Path: dir1, Alias: "a"},
		{Path: dir2, Alias: "b"},
	})
	defer r.Close()

	results := r.FindEntities(bg(), "Connect", []string{"a"}, 10)
	if len(results) != 1 || results[0].Alias != "a" {
		t.Fatalf("expected 1 result group from 'a', got %+v", results)
	}
}

func TestFindEntities_NoMatch(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "sibling", sampleNodesFor("sibling"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "sibling"}})
	defer r.Close()

	results := r.FindEntities(bg(), "NonExistentEntity", nil, 10)
	if len(results) != 0 {
		t.Errorf("expected 0 result groups, got %d", len(results))
	}
}

func TestFindEntities_BrokenSibling_Skipped(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "good", sampleNodesFor("good"))

	r := newResolver([]config.FederationEntry{
		{Path: "/nonexistent/broken", Alias: "broken"},
		{Path: dir, Alias: "good"},
	})
	defer r.Close()

	results := r.FindEntities(bg(), "AuthService", nil, 10)
	if len(results) != 1 || results[0].Alias != "good" {
		t.Fatalf("expected only 'good' result, got %+v", results)
	}
}

// ── GetDepsForEntity tests ──────────────────────────────────────────────────

func TestGetDepsForEntity_NoDeps(t *testing.T) {
	st := openTestStore(t)
	r := newResolver(nil)
	defer r.Close()

	deps := r.GetDepsForEntity(bg(), "some::entity", st)
	if deps != nil {
		t.Errorf("expected nil deps, got %v", deps)
	}
}

func TestGetDepsForEntity_WithDeps(t *testing.T) {
	st := openTestStore(t)
	err := st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "core", ToEntity: "AuthService",
		ToFile: "pkg/auth.go", VerifiedCommit: "abc123",
		VerifiedAt: time.Now().UTC().Format(time.RFC3339), DetectionTier: "tier1",
	})
	if err != nil {
		t.Fatal(err)
	}

	r := newResolver(nil)
	defer r.Close()

	deps := r.GetDepsForEntity(bg(), "local::main.go::Handler", st)
	if len(deps) != 1 || deps[0].Project != "core" || deps[0].Entity != "AuthService" {
		t.Fatalf("unexpected deps: %+v", deps)
	}
}

func TestGetDepsForEntity_NilStore(t *testing.T) {
	r := newResolver(nil)
	defer r.Close()
	if deps := r.GetDepsForEntity(bg(), "entity", nil); deps != nil {
		t.Errorf("expected nil for nil store, got %v", deps)
	}
}

func TestGetDepsForEntity_CancelledContext(t *testing.T) {
	st := openTestStore(t)
	r := newResolver(nil)
	defer r.Close()

	ctx, cancel := context.WithCancel(bg())
	cancel()
	if deps := r.GetDepsForEntity(ctx, "entity", st); deps != nil {
		t.Errorf("expected nil for cancelled context")
	}
}

// ── GetEntityContext tests ──────────────────────────────────────────────────

func TestGetEntityContext_Found(t *testing.T) {
	dir := t.TempDir()
	// Create sibling with edges so BFS has something to carve.
	nodes := []*graph.Node{
		{ID: "s::a.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "a.go", Line: 1, Exported: true},
		{ID: "s::a.go::Bar", Name: "Bar", Type: graph.NodeFunction, File: "a.go", Line: 10, Exported: true},
	}
	dbPath, _ := federation.SiblingDBPath(dir)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New("s")
	for _, n := range nodes {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: "s::a.go::Foo", To: "s::a.go::Bar", Type: graph.EdgeCalls})
	st.SaveGraph(g)
	st.Close()

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "s"}})
	defer r.Close()

	fc := r.GetEntityContext(bg(), "Foo", "s", 2)
	if fc == nil {
		t.Fatal("expected non-nil FederatedContext")
	}
	if fc.Alias != "s" || fc.Entity != "Foo" {
		t.Errorf("unexpected context: %+v", fc)
	}
	if fc.NodeCount == 0 {
		t.Error("expected at least 1 node in carved context")
	}
}

func TestGetEntityContext_NotFound(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "s", sampleNodesFor("s"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "s"}})
	defer r.Close()

	fc := r.GetEntityContext(bg(), "NonExistent", "s", 2)
	if fc != nil {
		t.Errorf("expected nil for non-existent entity, got %+v", fc)
	}
}

func TestGetEntityContext_UnknownAlias(t *testing.T) {
	r := newResolver(nil)
	defer r.Close()

	fc := r.GetEntityContext(bg(), "Foo", "unknown", 2)
	if fc != nil {
		t.Errorf("expected nil for unknown alias")
	}
}

func TestGetEntityContext_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	createSiblingWithDefaultPath(t, dir, "s", sampleNodesFor("s"))

	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "s"}})
	defer r.Close()

	ctx, cancel := context.WithCancel(bg())
	cancel()
	fc := r.GetEntityContext(ctx, "AuthService", "s", 2)
	if fc != nil {
		t.Error("expected nil for cancelled context")
	}
}

// ── InvalidateCache tests ───────────────────────────────────────────────────

func TestInvalidateCache_ClearsState(t *testing.T) {
	r := newResolver(nil)
	defer r.Close()
	r.InvalidateCache() // should not panic
}

func TestInvalidateCache_RetriesFailedStores(t *testing.T) {
	dir := t.TempDir()
	r := newResolver([]config.FederationEntry{{Path: dir, Alias: "retry"}})
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

// ── CheckDrift tests ────────────────────────────────────────────────────────

func TestCheckDrift_SameHead_NoDrift(t *testing.T) {
	sibDir := t.TempDir()
	initGitRepoExt(t, sibDir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Validate(token string) bool { return true }",
	})
	head := getHeadExt(t, sibDir)
	createSiblingWithDefaultPath(t, sibDir, "sibling", sampleNodesFor("sibling"))

	localStore := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	localStore.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "sibling",
		ToEntity: "Validate", ToFile: "pkg/auth.go",
		VerifiedCommit: head, VerifiedAt: now, DetectionTier: "tier1",
	})

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sibling"}})
	defer r.Close()

	alerts := r.CheckDrift(bg(), localStore)
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts (same HEAD), got %d: %+v", len(alerts), alerts)
	}
}

func TestCheckDrift_HeadChanged_NoAffectedFiles(t *testing.T) {
	sibDir := t.TempDir()
	oldHead := initGitRepoExt(t, sibDir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Validate(token string) bool { return true }",
		"pkg/db.go":   "package db\nfunc Connect() {}",
	})
	// Change db.go (not auth.go which is the dep file).
	commitChangeExt(t, sibDir, "pkg/db.go", "package db\nfunc Connect(ctx context.Context) {}", "change db")
	createSiblingWithDefaultPath(t, sibDir, "sibling", sampleNodesFor("sibling"))

	localStore := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	localStore.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "sibling",
		ToEntity: "Validate", ToFile: "pkg/auth.go",
		VerifiedCommit: oldHead, VerifiedAt: now, DetectionTier: "tier1",
	})

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sibling"}})
	defer r.Close()

	alerts := r.CheckDrift(bg(), localStore)
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts (dep file unchanged), got %d: %+v", len(alerts), alerts)
	}
}

func TestCheckDrift_SignatureChanged_Alert(t *testing.T) {
	sibDir := t.TempDir()
	oldHead := initGitRepoExt(t, sibDir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Validate(token string) bool { return true }",
	})
	commitChangeExt(t, sibDir, "pkg/auth.go",
		"package auth\nfunc Validate(token string, opts ...Option) bool { return true }",
		"add opts param")
	createSiblingWithDefaultPath(t, sibDir, "sibling", sampleNodesFor("sibling"))

	localStore := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	localStore.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "sibling",
		ToEntity: "Validate", ToFile: "pkg/auth.go",
		VerifiedCommit: oldHead, VerifiedAt: now, DetectionTier: "tier1",
	})

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sibling"}})
	defer r.Close()

	alerts := r.CheckDrift(bg(), localStore)
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d: %+v", len(alerts), alerts)
	}
	if alerts[0].Change != "signature_changed" {
		t.Errorf("expected signature_changed, got %q", alerts[0].Change)
	}
	if alerts[0].Severity != "breaking" {
		t.Errorf("expected breaking, got %q", alerts[0].Severity)
	}
	if alerts[0].Entity != "Validate" {
		t.Errorf("expected entity Validate, got %q", alerts[0].Entity)
	}
}

func TestCheckDrift_EntityRemoved_Alert(t *testing.T) {
	sibDir := t.TempDir()
	oldHead := initGitRepoExt(t, sibDir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Validate(token string) bool { return true }",
	})
	// Remove the function from the file (but keep the file).
	commitChangeExt(t, sibDir, "pkg/auth.go", "package auth\n// Validate was removed\n", "remove validate")
	createSiblingWithDefaultPath(t, sibDir, "sibling", sampleNodesFor("sibling"))

	localStore := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	localStore.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "sibling",
		ToEntity: "Validate", ToFile: "pkg/auth.go",
		VerifiedCommit: oldHead, VerifiedAt: now, DetectionTier: "tier1",
	})

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sibling"}})
	defer r.Close()

	alerts := r.CheckDrift(bg(), localStore)
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d: %+v", len(alerts), alerts)
	}
	if alerts[0].Change != "removed" {
		t.Errorf("expected removed, got %q", alerts[0].Change)
	}
}

func TestCheckDrift_FileChangedEntityUntouched_NoAlert(t *testing.T) {
	sibDir := t.TempDir()
	oldHead := initGitRepoExt(t, sibDir, map[string]string{
		"pkg/auth.go": "package auth\n\nfunc Validate(token string) bool { return true }\n\nfunc Other() {}\n",
	})
	// Change Other() but not Validate().
	commitChangeExt(t, sibDir, "pkg/auth.go",
		"package auth\n\nfunc Validate(token string) bool { return true }\n\nfunc Other(ctx context.Context) {}\n",
		"change other")
	createSiblingWithDefaultPath(t, sibDir, "sibling", sampleNodesFor("sibling"))

	localStore := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	localStore.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "sibling",
		ToEntity: "Validate", ToFile: "pkg/auth.go",
		VerifiedCommit: oldHead, VerifiedAt: now, DetectionTier: "tier1",
	})

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sibling"}})
	defer r.Close()

	alerts := r.CheckDrift(bg(), localStore)
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts (entity untouched), got %d: %+v", len(alerts), alerts)
	}
}

func TestCheckDrift_NotAGitRepo_FallbackWorks(t *testing.T) {
	sibDir := t.TempDir() // no git init
	createSiblingWithDefaultPath(t, sibDir, "sibling", sampleNodesFor("sibling"))

	localStore := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	localStore.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "sibling",
		ToEntity: "AuthService", ToFile: "pkg/auth.go",
		VerifiedCommit: "fakecommit", VerifiedAt: now, DetectionTier: "tier1",
	})

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sibling"}})
	defer r.Close()

	// Should not panic — falls back to signature comparison.
	alerts := r.CheckDrift(bg(), localStore)
	// Fallback can't compare signatures without stored old sig, so no alerts expected.
	_ = alerts
}

func TestCheckDrift_NilStore(t *testing.T) {
	r := newResolver(nil)
	defer r.Close()

	alerts := r.CheckDrift(bg(), nil)
	if alerts != nil {
		t.Errorf("expected nil for nil store")
	}
}

func TestCheckDrift_CancelledContext(t *testing.T) {
	r := newResolver(nil)
	defer r.Close()

	ctx, cancel := context.WithCancel(bg())
	cancel()

	alerts := r.CheckDrift(ctx, openTestStore(t))
	if alerts != nil {
		t.Errorf("expected nil for cancelled context")
	}
}

func TestCheckDrift_CachedResults(t *testing.T) {
	sibDir := t.TempDir()
	oldHead := initGitRepoExt(t, sibDir, map[string]string{
		"pkg/auth.go": "package auth\nfunc Validate(token string) bool { return true }",
	})
	commitChangeExt(t, sibDir, "pkg/auth.go",
		"package auth\nfunc Validate(token string, opts ...Option) bool { return true }",
		"change validate")
	createSiblingWithDefaultPath(t, sibDir, "sibling", sampleNodesFor("sibling"))

	localStore := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	localStore.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "sibling",
		ToEntity: "Validate", ToFile: "pkg/auth.go",
		VerifiedCommit: oldHead, VerifiedAt: now, DetectionTier: "tier1",
	})

	r := newResolver([]config.FederationEntry{{Path: sibDir, Alias: "sibling"}})
	defer r.Close()

	alerts1 := r.CheckDrift(bg(), localStore)
	alerts2 := r.CheckDrift(bg(), localStore)

	if len(alerts1) != len(alerts2) {
		t.Errorf("cached results differ: %d vs %d", len(alerts1), len(alerts2))
	}
}

// ── drift test helpers ──────────────────────────────────────────────────────

func initGitRepoExt(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	runGitExt(t, dir, "init")
	runGitExt(t, dir, "config", "user.email", "test@test.com")
	runGitExt(t, dir, "config", "user.name", "Test")
	for path, content := range files {
		full := filepath.Join(dir, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	runGitExt(t, dir, "add", ".")
	runGitExt(t, dir, "commit", "-m", "initial")
	return getHeadExt(t, dir)
}

func commitChangeExt(t *testing.T, dir, filePath, newContent, msg string) string {
	t.Helper()
	full := filepath.Join(dir, filePath)
	os.MkdirAll(filepath.Dir(full), 0o755)
	os.WriteFile(full, []byte(newContent), 0o644)
	runGitExt(t, dir, "add", filePath)
	runGitExt(t, dir, "commit", "-m", msg)
	return getHeadExt(t, dir)
}

func getHeadExt(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD: %v", err)
	}
	s := string(out)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func runGitExt(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// ── Aliases / HasAlias ──────────────────────────────────────────────────────

func TestAliases(t *testing.T) {
	r := newResolver([]config.FederationEntry{
		{Path: "/a", Alias: "alpha"},
		{Path: "/b", Alias: "beta"},
	})
	defer r.Close()
	aliases := r.Aliases()
	if len(aliases) != 2 || aliases[0] != "alpha" || aliases[1] != "beta" {
		t.Errorf("unexpected: %v", aliases)
	}
}

func TestHasAlias(t *testing.T) {
	r := newResolver([]config.FederationEntry{{Path: "/a", Alias: "alpha"}})
	defer r.Close()
	if !r.HasAlias("alpha") {
		t.Error("expected true")
	}
	if r.HasAlias("beta") {
		t.Error("expected false")
	}
}

// ── Config validation ───────────────────────────────────────────────────────

func TestConfigValidation_DuplicateAlias(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{"version":"1","federation":[{"path":"../a","alias":"dup"},{"path":"../b","alias":"dup"}]}`)
	if _, err := config.Load(dir); err == nil {
		t.Error("expected error for duplicate alias")
	}
}

func TestConfigValidation_EmptyAlias(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{"version":"1","federation":[{"path":"../a","alias":""}]}`)
	if _, err := config.Load(dir); err == nil {
		t.Error("expected error for empty alias")
	}
}

func TestConfigValidation_WhitespaceAlias(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{"version":"1","federation":[{"path":"../a","alias":"has space"}]}`)
	if _, err := config.Load(dir); err == nil {
		t.Error("expected error for whitespace alias")
	}
}

func TestConfigValidation_EmptyPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{"version":"1","federation":[{"path":"","alias":"nopath"}]}`)
	if _, err := config.Load(dir); err == nil {
		t.Error("expected error for empty path")
	}
}

func TestConfigValidation_ValidFederation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "synapses.json"), `{"version":"1","federation":[{"path":"../a","alias":"alpha"},{"path":"../b","alias":"beta"}]}`)
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("expected valid config: %v", err)
	}
	if len(cfg.Federation) != 2 {
		t.Errorf("expected 2 entries, got %d", len(cfg.Federation))
	}
	if !filepath.IsAbs(cfg.Federation[0].Path) {
		t.Errorf("expected absolute path, got %q", cfg.Federation[0].Path)
	}
}

// ── Store query method tests ────────────────────────────────────────────────

func TestNodeExistsByName_ExactMatch(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::main.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "main.go"})
	assertExists(t, st, "Foo", true)
}

func TestNodeExistsByName_CaseInsensitive(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::main.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "main.go"})
	assertExists(t, st, "foo", true)
}

func TestNodeExistsByName_QualifiedSuffix(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::main.go::Store.Close", Name: "Store.Close", Type: graph.NodeMethod, File: "main.go"})
	assertExists(t, st, "Close", true)
}

func TestNodeExistsByName_NotFound(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::main.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "main.go"})
	assertExists(t, st, "Bar", false)
}

func TestNodeExistsByName_LikeWildcardEscaped(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test",
		&graph.Node{ID: "test::a.go::_init", Name: "_init", Type: graph.NodeFunction, File: "a.go"},
		&graph.Node{ID: "test::b.go::Xinit", Name: "Xinit", Type: graph.NodeFunction, File: "b.go"},
	)
	// Searching "_init" should NOT match "Xinit" — underscore must be escaped.
	results, err := st.FindNodesByName("_init", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result (only _init), got %d", len(results))
	}
	if len(results) == 1 && results[0].Name != "_init" {
		t.Errorf("expected _init, got %q", results[0].Name)
	}
}

func TestNodeExistsByName_PercentEscaped(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test",
		&graph.Node{ID: "test::a.go::X%Y", Name: "X%Y", Type: graph.NodeFunction, File: "a.go"},
		&graph.Node{ID: "test::b.go::XabcY", Name: "XabcY", Type: graph.NodeFunction, File: "b.go"},
	)
	// Searching "X%Y" should only match literal "X%Y", not "XabcY".
	exists, err := st.NodeExistsByName("X%Y")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected X%Y to exist via exact match")
	}
	// FindNodesByName should find exactly 1 (the exact match) — not 2 via LIKE wildcard.
	results, err := st.FindNodesByName("X%Y", 10)
	if err != nil {
		t.Fatal(err)
	}
	// Both found: exact match on "X%Y" and suffix LIKE "%.X%Y" (escaped).
	// The exact match clause finds "X%Y". The LIKE clause with escaping won't match "XabcY".
	if len(results) != 1 {
		t.Errorf("expected 1 result (only X%%Y), got %d: %+v", len(results), results)
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
		t.Errorf("expected 2, got %d", len(results))
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
		t.Errorf("expected 2 (exact + suffix), got %d", len(results))
	}
}

func TestFindNodesByName_ContextCancellation(t *testing.T) {
	st := openTestStore(t)
	saveNodes(t, st, "test", &graph.Node{ID: "test::a.go::Foo", Name: "Foo", Type: graph.NodeFunction, File: "a.go"})

	ctx, cancel := context.WithCancel(bg())
	cancel()

	_, err := st.FindNodesByNameCtx(ctx, "Foo", 10)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

// ── Store cross_project_deps CRUD ───────────────────────────────────────────

func TestCrossProjectDeps_UpsertAndGet(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	err := st.UpsertCrossProjectDep(store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "core", ToEntity: "AuthService",
		ToFile: "pkg/auth.go", VerifiedCommit: "abc123", VerifiedAt: now, DetectionTier: "tier1",
	})
	if err != nil {
		t.Fatal(err)
	}
	deps, err := st.GetCrossProjectDeps("local::main.go::Handler")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].ToProject != "core" {
		t.Fatalf("unexpected: %+v", deps)
	}
}

func TestCrossProjectDeps_UpsertOverwrite(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	dep := store.CrossProjectDep{
		FromEntity: "local::main.go::Handler", ToProject: "core", ToEntity: "Auth",
		ToFile: "a.go", VerifiedCommit: "old", VerifiedAt: now, DetectionTier: "tier1",
	}
	st.UpsertCrossProjectDep(dep)
	dep.VerifiedCommit = "new"
	st.UpsertCrossProjectDep(dep)

	deps, _ := st.GetCrossProjectDeps("local::main.go::Handler")
	if len(deps) != 1 || deps[0].VerifiedCommit != "new" {
		t.Fatalf("expected updated commit, got %+v", deps)
	}
}

func TestCrossProjectDeps_GetByProject(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	st.UpsertCrossProjectDep(store.CrossProjectDep{FromEntity: "l::x::A", ToProject: "core", ToEntity: "A", ToFile: "f.go", VerifiedCommit: "a", VerifiedAt: now, DetectionTier: "tier1"})
	st.UpsertCrossProjectDep(store.CrossProjectDep{FromEntity: "l::x::B", ToProject: "core", ToEntity: "B", ToFile: "f.go", VerifiedCommit: "a", VerifiedAt: now, DetectionTier: "tier1"})
	st.UpsertCrossProjectDep(store.CrossProjectDep{FromEntity: "l::y::C", ToProject: "other", ToEntity: "C", ToFile: "g.go", VerifiedCommit: "b", VerifiedAt: now, DetectionTier: "tier1"})

	deps, _ := st.GetCrossProjectDepsByProject("core")
	if len(deps) != 2 {
		t.Errorf("expected 2 for 'core', got %d", len(deps))
	}
}

func TestCrossProjectDeps_Delete(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	st.UpsertCrossProjectDep(store.CrossProjectDep{FromEntity: "l::x::A", ToProject: "core", ToEntity: "A", ToFile: "a.go", VerifiedCommit: "a", VerifiedAt: now, DetectionTier: "tier1"})
	st.DeleteCrossProjectDeps("l::x::A")

	deps, _ := st.GetCrossProjectDeps("l::x::A")
	if len(deps) != 0 {
		t.Errorf("expected 0 after delete, got %d", len(deps))
	}
}

func TestCrossProjectDeps_UpdateVerifiedCommit(t *testing.T) {
	st := openTestStore(t)
	now := time.Now().UTC().Format(time.RFC3339)
	st.UpsertCrossProjectDep(store.CrossProjectDep{FromEntity: "l::x::A", ToProject: "core", ToEntity: "Auth", ToFile: "a.go", VerifiedCommit: "old", VerifiedAt: now, DetectionTier: "tier1"})
	st.UpdateVerifiedCommit("core", "Auth", "new")

	deps, _ := st.GetCrossProjectDeps("l::x::A")
	if len(deps) != 1 || deps[0].VerifiedCommit != "new" {
		t.Fatalf("expected 'new', got %+v", deps)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
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

func assertExists(t *testing.T, st *store.Store, name string, want bool) {
	t.Helper()
	got, err := st.NodeExistsByName(name)
	if err != nil {
		t.Fatalf("NodeExistsByName(%q): %v", name, err)
	}
	if got != want {
		t.Errorf("NodeExistsByName(%q) = %v, want %v", name, got, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
