package store_test

import (
	"fmt"
	"slices"
	"sort"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

func TestLoadCallSitesForFiles_ScopedLoad(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	sites := []graph.CallSite{
		{CallerID: "repo::a.go::Foo", CallerFile: "a.go", PkgAlias: "b", FuncName: "Bar"},
		{CallerID: "repo::b.go::Bar", CallerFile: "b.go", PkgAlias: "c", FuncName: "Baz"},
		{CallerID: "repo::c.go::Baz", CallerFile: "c.go", PkgAlias: "", FuncName: "inner"},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	// Load only for a.go and c.go — b.go should not appear.
	loaded, err := st.LoadCallSitesForFiles([]string{"a.go", "c.go"})
	if err != nil {
		t.Fatalf("LoadCallSitesForFiles: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("want 2 call sites, got %d: %v", len(loaded), loaded)
	}
	for _, cs := range loaded {
		if cs.CallerFile == "b.go" {
			t.Errorf("b.go should not be in scoped result")
		}
	}
}

func TestLoadCallSitesForFiles_EmptyFilesReturnsNil(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	loaded, err := st.LoadCallSitesForFiles([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded != nil {
		t.Errorf("want nil for empty files, got %v", loaded)
	}
}

func TestLoadCallSitesForFiles_FallbackWhenTooManyFiles(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Seed one call site so LoadCallSites returns something meaningful.
	sites := []graph.CallSite{
		{CallerID: "repo::a.go::Foo", CallerFile: "a.go", PkgAlias: "b", FuncName: "Bar"},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	// Build a files slice that exceeds the 900-entry threshold.
	files := make([]string, 901)
	for i := range files {
		files[i] = "dummy.go"
	}
	files[0] = "a.go"

	loaded, err := st.LoadCallSitesForFiles(files)
	if err != nil {
		t.Fatalf("LoadCallSitesForFiles fallback: %v", err)
	}
	// At least the seeded site should be returned (fallback to full load).
	if len(loaded) == 0 {
		t.Error("expected at least one call site from full-load fallback")
	}
}

func TestLoadCallSitesForFiles_RoundTrip(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	sites := []graph.CallSite{
		{CallerID: "repo::a.go::Foo", CallerFile: "a.go", PkgAlias: "store", FuncName: "Open"},
		{CallerID: "repo::a.go::Bar", CallerFile: "a.go", PkgAlias: "graph", FuncName: "New"},
		{CallerID: "repo::b.go::Baz", CallerFile: "b.go", PkgAlias: "store", FuncName: "Save"},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	loaded, err := st.LoadCallSitesForFiles([]string{"a.go"})
	if err != nil {
		t.Fatalf("LoadCallSitesForFiles: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("want 2 call sites for a.go, got %d: %v", len(loaded), loaded)
	}
	var funcs []string
	for _, cs := range loaded {
		funcs = append(funcs, cs.FuncName)
	}
	sort.Strings(funcs)
	if !slices.Equal(funcs, []string{"New", "Open"}) {
		t.Errorf("want [New Open], got %v", funcs)
	}
}

func TestLoadCallerFilesForPkgAliases_Basic(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// a.go calls something from package "models" (unresolved — no CALLS edge yet)
	// b.go calls something from package "store"
	sites := []graph.CallSite{
		{CallerID: "repo::a.go::Handler", CallerFile: "a.go", PkgAlias: "models", FuncName: "NewUser"},
		{CallerID: "repo::b.go::Fetcher", CallerFile: "b.go", PkgAlias: "store", FuncName: "Open"},
		{CallerID: "repo::c.go::Init", CallerFile: "c.go", PkgAlias: "models", FuncName: "FindAll"},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	// When "models" package adds a new function, a.go and c.go must be found.
	files, err := st.LoadCallerFilesForPkgAliases([]string{"models"})
	if err != nil {
		t.Fatalf("LoadCallerFilesForPkgAliases: %v", err)
	}
	sort.Strings(files)
	if !slices.Equal(files, []string{"a.go", "c.go"}) {
		t.Errorf("want [a.go c.go], got %v", files)
	}

	// b.go must NOT appear when querying for "models".
	for _, f := range files {
		if f == "b.go" {
			t.Errorf("b.go (alias 'store') must not appear for alias 'models'")
		}
	}
}

func TestLoadCallerFilesForPkgAliases_MultipleAliases(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	sites := []graph.CallSite{
		{CallerID: "repo::x.go::A", CallerFile: "x.go", PkgAlias: "models", FuncName: "Foo"},
		{CallerID: "repo::y.go::B", CallerFile: "y.go", PkgAlias: "user", FuncName: "Bar"},
		{CallerID: "repo::z.go::C", CallerFile: "z.go", PkgAlias: "other", FuncName: "Baz"},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	// Query with both aliases: package name "models" and filename stem "user".
	files, err := st.LoadCallerFilesForPkgAliases([]string{"models", "user"})
	if err != nil {
		t.Fatalf("LoadCallerFilesForPkgAliases: %v", err)
	}
	sort.Strings(files)
	if !slices.Equal(files, []string{"x.go", "y.go"}) {
		t.Errorf("want [x.go y.go], got %v", files)
	}
}

func TestLoadCallerFilesForPkgAliases_EmptyAliasesReturnsNil(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	files, err := st.LoadCallerFilesForPkgAliases([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files != nil {
		t.Errorf("want nil for empty aliases, got %v", files)
	}

	files, err = st.LoadCallerFilesForPkgAliases([]string{""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files != nil {
		t.Errorf("want nil for blank alias, got %v", files)
	}
}

func TestLoadCallerFilesForPkgAliases_DeduplicatesResults(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// a.go has two call sites both with alias "models" — should appear once.
	sites := []graph.CallSite{
		{CallerID: "repo::a.go::F1", CallerFile: "a.go", PkgAlias: "models", FuncName: "Alpha"},
		{CallerID: "repo::a.go::F2", CallerFile: "a.go", PkgAlias: "models", FuncName: "Beta"},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	files, err := st.LoadCallerFilesForPkgAliases([]string{"models"})
	if err != nil {
		t.Fatalf("LoadCallerFilesForPkgAliases: %v", err)
	}
	if len(files) != 1 || files[0] != "a.go" {
		t.Errorf("want exactly [a.go] (deduplicated), got %v", files)
	}
}

func TestLoadCallerFilesForPkgAliases_FallbackOver900(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	// Seed call sites across 3 files with distinct pkg_alias values.
	sites := []graph.CallSite{
		{CallerID: "repo::a.go::F1", CallerFile: "a.go", PkgAlias: "alpha", FuncName: "Do"},
		{CallerID: "repo::b.go::F2", CallerFile: "b.go", PkgAlias: "beta", FuncName: "Run"},
		{CallerID: "repo::c.go::F3", CallerFile: "c.go", PkgAlias: "gamma", FuncName: "Exec"},
	}
	if err := st.SaveCallSites(sites); err != nil {
		t.Fatalf("SaveCallSites: %v", err)
	}

	// Build an alias slice that exceeds the 900-entry SQLite threshold.
	// Include the three real aliases among 901 total entries.
	aliases := make([]string, 901)
	for i := range aliases {
		aliases[i] = fmt.Sprintf("fake_%d", i)
	}
	aliases[0] = "alpha"
	aliases[1] = "beta"
	aliases[2] = "gamma"

	files, err := st.LoadCallerFilesForPkgAliases(aliases)
	if err != nil {
		t.Fatalf("LoadCallerFilesForPkgAliases fallback: %v", err)
	}
	sort.Strings(files)

	// The fallback returns ALL caller files, so all 3 must appear.
	want := []string{"a.go", "b.go", "c.go"}
	if !slices.Equal(files, want) {
		t.Errorf("want %v, got %v", want, files)
	}
}

// ── test helper to expose SaveCallSites via the exported store API ────────────

func init() {
	// Ensure store.Store has the required methods.
	var _ interface{ SaveCallSites([]graph.CallSite) error } = (*store.Store)(nil)
	var _ interface {
		LoadCallerFilesForPkgAliases([]string) ([]string, error)
	} = (*store.Store)(nil)
}
