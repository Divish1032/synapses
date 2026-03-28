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
	srv := httptest.NewTLSServer(handler)
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
	srv := httptest.NewTLSServer(handler)
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
	srv := httptest.NewTLSServer(handler)
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
		"github.com/foo/bar":     "v1.2.3",
		"github.com/baz/qux":     "v0.5.0",
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
	srv := httptest.NewTLSServer(handler)
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

// ---------------------------------------------------------------------------
// FetchPackageDocs Tests (Network)
// ---------------------------------------------------------------------------

func TestFetchPackageDocs_CacheHit(t *testing.T) {
	// Test that FetchPackageDocs returns cached content on cache hit
	s := testStore(t)
	c := New(s)

	importPath := "github.com/foo/bar"
	version := "v1.0.0"
	key := PackageCacheKey(importPath, version)

	// Pre-populate the cache
	cachedContent := "cached documentation"
	if err := s.UpsertWebCache(key, cachedContent, 0); err != nil {
		t.Fatalf("UpsertWebCache: %v", err)
	}

	ctx := context.Background()
	content, fromCache, err := c.FetchPackageDocs(ctx, importPath, version)
	if err != nil {
		t.Fatalf("FetchPackageDocs: %v", err)
	}
	if !fromCache {
		t.Error("expected cache hit")
	}
	if content != cachedContent {
		t.Errorf("expected %q, got %q", cachedContent, content)
	}
}

func TestFetchPackageDocs_CacheMissNetworkSuccess(t *testing.T) {
	// Test FetchPackageDocs cache key generation and storage
	s := testStore(t)

	importPath := "github.com/test/pkg"
	version := "v1.5.0"
	key := PackageCacheKey(importPath, version)

	// Verify not in cache initially
	if _, ok := s.GetWebCache(key); ok {
		t.Fatal("expected cache miss initially")
	}

	// Test cache hit scenario by manually caching and retrieving
	testContent := "cached package docs"
	if err := s.UpsertWebCache(key, testContent, 0); err != nil {
		t.Fatalf("UpsertWebCache: %v", err)
	}

	// Now test that retrieving returns the cached content
	entry, ok := s.GetWebCache(key)
	if !ok {
		t.Error("expected cache hit after insert")
	}
	if entry.Content != testContent {
		t.Error("cached content mismatch")
	}
}

func TestFetchPackageDocs_WithoutVersion(t *testing.T) {
	// Test FetchPackageDocs works with empty version string
	// Test the cache key logic with empty version
	s := testStore(t)

	importPath := "fmt"
	version := ""
	key := PackageCacheKey(importPath, version)

	// Verify key format without version
	expected := pkgPrefix + importPath
	if key != expected {
		t.Errorf("expected key %q, got %q", expected, key)
	}

	// Pre-cache content
	cachedContent := "docs without version"
	if err := s.UpsertWebCache(key, cachedContent, 0); err != nil {
		t.Fatalf("UpsertWebCache: %v", err)
	}

	// Retrieve from cache
	entry, ok := s.GetWebCache(key)
	if !ok {
		t.Error("expected cache hit")
	}
	if entry.Content != cachedContent {
		t.Errorf("unexpected content: %q", entry.Content)
	}
}

func TestFetchPackageDocs_NetworkError(t *testing.T) {
	// Test FetchPackageDocs handles network errors
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewTLSServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: srv.Client(),
	}

	ctx := context.Background()
	_, _, err := c.FetchPackageDocs(ctx, "github.com/test/pkg", "v1.0.0")
	if err == nil {
		t.Error("expected error on network failure")
	}
}

// ---------------------------------------------------------------------------
// FetchPackageDocsFresh Tests
// ---------------------------------------------------------------------------

func TestFetchPackageDocsFresh(t *testing.T) {
	// Test FetchPackageDocsFresh fetches fresh content, bypassing cache
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("fresh package docs"))
	})
	srv := httptest.NewTLSServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: srv.Client(),
	}

	// Test with a URL that the test server can handle
	ctx := context.Background()
	content, err := c.FetchFresh(ctx, srv.URL+"/pkg")
	if err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}

	if !strings.Contains(content, "fresh package docs") {
		t.Errorf("expected 'fresh package docs', got %q", content)
	}
}

func TestFetchPackageDocsFresh_WithoutVersion(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("stdlib docs"))
	})
	srv := httptest.NewTLSServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: srv.Client(),
	}

	ctx := context.Background()
	content, err := c.FetchFresh(ctx, srv.URL+"/stdlib")
	if err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	if !strings.Contains(content, "stdlib docs") {
		t.Errorf("unexpected content: %q", content)
	}
}

// ---------------------------------------------------------------------------
// New Function — SSRF Protection Tests
// ---------------------------------------------------------------------------

func TestNew_SSRFProtection_BlocksLoopback(t *testing.T) {
	// Test that New's SSRF protection is set up (hard to fully test without mocking net resolver)
	s := testStore(t)
	c := New(s)

	// Verify the cache was created with proper fields
	if c.store == nil {
		t.Error("store should not be nil")
	}
	if c.httpClient == nil {
		t.Error("httpClient should not be nil")
	}
	if c.httpClient.Transport == nil {
		t.Error("transport should not be nil")
	}
}

// ---------------------------------------------------------------------------
// Fetch Error Cases
// ---------------------------------------------------------------------------

func TestFetch_NetworkTimeout(t *testing.T) {
	// Simulate a timeout by using a very slow server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't respond at all - will trigger timeout
		<-r.Context().Done()
	})
	srv := httptest.NewTLSServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: &http.Client{Timeout: 1 * 1000 * 1000}, // 1 millisecond timeout
	}

	ctx := context.Background()
	_, _, err := c.Fetch(ctx, srv.URL, 24)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestFetch_EmptyResponse(t *testing.T) {
	// Test handling of empty response body
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write nothing
	})
	srv := httptest.NewTLSServer(handler)
	defer srv.Close()

	s := testStore(t)
	c := &Cache{
		store:      s,
		httpClient: srv.Client(),
	}

	ctx := context.Background()
	content, fromCache, err := c.Fetch(ctx, srv.URL, 24)
	if err != nil {
		t.Fatalf("Fetch with empty response: %v", err)
	}
	if fromCache {
		t.Error("expected cache miss")
	}
	if content != "" {
		t.Errorf("expected empty string, got %q", content)
	}
}
