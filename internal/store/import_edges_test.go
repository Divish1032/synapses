package store_test

import (
	"slices"
	"sort"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

func TestImportEdges_RoundTrip(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	if err := st.UpdateImportEdgesForFile("a.go", []string{"store", "graph", "config"}); err != nil {
		t.Fatalf("UpdateImportEdgesForFile: %v", err)
	}
	if err := st.UpdateImportEdgesForFile("b.go", []string{"store"}); err != nil {
		t.Fatalf("UpdateImportEdgesForFile: %v", err)
	}

	loaded, err := st.LoadAllImportEdges()
	if err != nil {
		t.Fatalf("LoadAllImportEdges: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("want 2 files, got %d", len(loaded))
	}
	aPkgs := loaded["a.go"]
	sort.Strings(aPkgs)
	want := []string{"config", "graph", "store"}
	if !slices.Equal(aPkgs, want) {
		t.Errorf("a.go pkgs: want %v, got %v", want, aPkgs)
	}
	if !slices.Equal(loaded["b.go"], []string{"store"}) {
		t.Errorf("b.go pkgs: want [store], got %v", loaded["b.go"])
	}
}

func TestImportEdges_UpdateReplacesPreviousEntries(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_ = st.UpdateImportEdgesForFile("a.go", []string{"store", "graph"})
	// Update to a different set.
	if err := st.UpdateImportEdgesForFile("a.go", []string{"config"}); err != nil {
		t.Fatalf("second UpdateImportEdgesForFile: %v", err)
	}

	loaded, err := st.LoadAllImportEdges()
	if err != nil {
		t.Fatalf("LoadAllImportEdges: %v", err)
	}
	pkgs := loaded["a.go"]
	if len(pkgs) != 1 || pkgs[0] != "config" {
		t.Errorf("want [config] after update, got %v", pkgs)
	}
}

func TestImportEdges_EmptyPkgSkipped(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)

	_ = st.UpdateImportEdgesForFile("a.go", []string{"", "store", ""})

	loaded, err := st.LoadAllImportEdges()
	if err != nil {
		t.Fatalf("LoadAllImportEdges: %v", err)
	}
	if !slices.Equal(loaded["a.go"], []string{"store"}) {
		t.Errorf("want [store] (empty strings skipped), got %v", loaded["a.go"])
	}
}

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

// ── test helper to expose SaveCallSites via the exported store API ────────────

func init() {
	// Ensure store.Store has the SaveCallSites method used above.
	var _ interface{ SaveCallSites([]graph.CallSite) error } = (*store.Store)(nil)
}
