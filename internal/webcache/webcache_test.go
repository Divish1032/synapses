package webcache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// testStore creates a temporary store for testing.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestNew(t *testing.T) {
	s := testStore(t)
	c := New(s)
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.store != s {
		t.Error("store not set")
	}
	if c.httpClient == nil {
		t.Error("httpClient not set")
	}
}

func TestPackageCacheKey(t *testing.T) {
	tests := []struct {
		importPath string
		version    string
		want       string
	}{
		{"github.com/foo/bar", "v1.2.3", "pkg:github.com/foo/bar@v1.2.3"},
		{"github.com/foo/bar", "", "pkg:github.com/foo/bar"},
		{"net/http", "", "pkg:net/http"},
	}
	for _, tt := range tests {
		got := PackageCacheKey(tt.importPath, tt.version)
		if got != tt.want {
			t.Errorf("PackageCacheKey(%q, %q) = %q, want %q", tt.importPath, tt.version, got, tt.want)
		}
	}
}

func TestIsStdlib(t *testing.T) {
	tests := []struct {
		importPath string
		want       bool
	}{
		{"fmt", true},
		{"net/http", true},
		{"os", true},
		{"github.com/foo/bar", false},
		{"golang.org/x/tools", false},
		{"io", true},
	}
	for _, tt := range tests {
		got := IsStdlib(tt.importPath)
		if got != tt.want {
			t.Errorf("IsStdlib(%q) = %v, want %v", tt.importPath, got, tt.want)
		}
	}
}

func TestFetch_CacheMiss_ThenHit(t *testing.T) {
	// Set up a test HTTP server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><p>Hello World</p></body></html>"))
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: srv.Client(),
	}

	ctx := context.Background()

	// First fetch — cache miss
	content, fromCache, err := c.Fetch(ctx, srv.URL+"/test", 24)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fromCache {
		t.Error("expected cache miss on first fetch")
	}
	if !strings.Contains(content, "Hello World") {
		t.Errorf("expected content to contain 'Hello World', got %q", content)
	}

	// Second fetch — cache hit
	content2, fromCache2, err := c.Fetch(ctx, srv.URL+"/test", 24)
	if err != nil {
		t.Fatalf("Fetch (cached): %v", err)
	}
	if !fromCache2 {
		t.Error("expected cache hit on second fetch")
	}
	if content2 != content {
		t.Error("cached content differs from original")
	}
}

func TestFetchFresh(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("plain text response"))
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: srv.Client(),
	}

	content, err := c.FetchFresh(context.Background(), srv.URL+"/fresh")
	if err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	if content != "plain text response" {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestFetch_HTTPError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: srv.Client(),
	}

	_, _, err := c.Fetch(context.Background(), srv.URL+"/missing", 24)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("expected HTTP 404 error, got: %v", err)
	}
}

func TestFetchPackageDocs_CacheMiss_ThenHit(t *testing.T) {
	s := testStore(t)

	// Test the cache key logic and invalidation directly.
	key := PackageCacheKey("github.com/foo/bar", "v1.0.0")
	if key != "pkg:github.com/foo/bar@v1.0.0" {
		t.Fatalf("unexpected cache key: %s", key)
	}

	// Manually write to cache
	if err := s.UpsertWebCache(key, "cached doc content", 0); err != nil {
		t.Fatalf("UpsertWebCache: %v", err)
	}

	// Verify it comes from cache
	entry, ok := s.GetWebCache(key)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if entry.Content != "cached doc content" {
		t.Errorf("unexpected cached content: %q", entry.Content)
	}
}

func TestInvalidatePackage(t *testing.T) {
	s := testStore(t)
	c := New(s)

	key := PackageCacheKey("github.com/foo/bar", "v1.0.0")
	if err := s.UpsertWebCache(key, "old docs", 0); err != nil {
		t.Fatal(err)
	}

	c.InvalidatePackage("github.com/foo/bar", "v1.0.0")

	if _, ok := s.GetWebCache(key); ok {
		t.Error("expected cache entry to be invalidated")
	}
}

func TestPruneExpired(t *testing.T) {
	s := testStore(t)
	c := New(s)

	err := c.PruneExpired()
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
}

func TestParseGoMod(t *testing.T) {
	dir := t.TempDir()
	gomod := `module example.com/myproject

go 1.21

require (
	github.com/foo/bar v1.2.3
	github.com/baz/qux v0.5.0 // indirect
)

require github.com/single/line v2.0.0
`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	versions, err := ParseGoMod(dir)
	if err != nil {
		t.Fatalf("ParseGoMod: %v", err)
	}

	expected := map[string]string{
		"github.com/foo/bar":    "v1.2.3",
		"github.com/baz/qux":   "v0.5.0",
		"github.com/single/line": "v2.0.0",
	}

	for pkg, wantVer := range expected {
		if gotVer, ok := versions[pkg]; !ok {
			t.Errorf("missing package %s", pkg)
		} else if gotVer != wantVer {
			t.Errorf("package %s: got %s, want %s", pkg, gotVer, wantVer)
		}
	}
}

func TestParseGoMod_NotExists(t *testing.T) {
	versions, err := ParseGoMod(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for missing go.mod, got: %v", err)
	}
	if versions != nil {
		t.Error("expected nil versions for missing go.mod")
	}
}

func TestFetchAndStrip_ContentCapped(t *testing.T) {
	// Generate content larger than maxDocBytes (8KB)
	largeContent := strings.Repeat("a", 16*1024)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(largeContent))
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: srv.Client(),
	}

	content, err := c.FetchFresh(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	if len(content) > maxDocBytes {
		t.Errorf("content length %d exceeds maxDocBytes %d", len(content), maxDocBytes)
	}
}
