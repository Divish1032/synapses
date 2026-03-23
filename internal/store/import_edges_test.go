package store_test

import (
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

// ── test helper to expose SaveCallSites via the exported store API ────────────

func init() {
	// Ensure store.Store has the SaveCallSites method used above.
	var _ interface{ SaveCallSites([]graph.CallSite) error } = (*store.Store)(nil)
}
