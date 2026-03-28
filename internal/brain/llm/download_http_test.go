package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hijackTransport redirects all HTTP requests to a fixed target host,
// allowing DownloadGGUF (which uses the hardcoded HFBaseURL const) to be
// tested against a local httptest.Server.
type hijackTransport struct {
	target string // "host:port"
	base   http.RoundTripper
}

func (h *hijackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme = "http"
	u.Host = h.target
	req2 := req.Clone(req.Context())
	req2.URL = &u
	req2.Host = h.target
	return h.base.RoundTrip(req2)
}

// withHijackedHTTP installs a transport hijacker, runs f, then restores.
func withHijackedHTTP(t *testing.T, srv *httptest.Server, f func()) {
	t.Helper()
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	host := strings.TrimPrefix(srv.URL, "http://")
	http.DefaultTransport = &hijackTransport{target: host, base: orig}
	f()
}

// ============================================================
// DownloadGGUF — HTTP 404 (file not found on HuggingFace)
// ============================================================

func TestDownloadGGUF_HTTP404_FileNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 404
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := DownloadConfig{
		Repo:     "owner/repo",
		Filename: "model.gguf",
		DestDir:  dir,
	}

	withHijackedHTTP(t, srv, func() {
		_, err := DownloadGGUF(context.Background(), cfg)
		if err == nil {
			t.Fatal("expected error for 404 response")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention 'not found', got: %v", err)
		}
	})
}

// ============================================================
// DownloadGGUF — HTTP 500 (unexpected status)
// ============================================================

func TestDownloadGGUF_HTTPNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden) // 403
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := DownloadConfig{
		Repo:     "owner/repo",
		Filename: "model.gguf",
		DestDir:  dir,
	}

	withHijackedHTTP(t, srv, func() {
		_, err := DownloadGGUF(context.Background(), cfg)
		if err == nil {
			t.Fatal("expected error for non-200 response")
		}
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("error should mention 403, got: %v", err)
		}
	})
}

// ============================================================
// DownloadGGUF — HTTP 200 + actual download (success path)
// ============================================================

func TestDownloadGGUF_HTTPSuccess_DownloadsFile(t *testing.T) {
	fileContent := []byte("fake gguf file content for testing atomic write")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "47")
		w.WriteHeader(http.StatusOK)
		w.Write(fileContent)
	}))
	defer srv.Close()

	// Compute the SHA-256 of the test content for verification.
	h := sha256.Sum256(fileContent)
	expectedHash := hex.EncodeToString(h[:])

	dir := t.TempDir()
	filename := "model.gguf"
	cfg := DownloadConfig{
		Repo:     "owner/repo",
		Filename: filename,
		DestDir:  dir,
		SHA256:   expectedHash,
	}

	withHijackedHTTP(t, srv, func() {
		got, err := DownloadGGUF(context.Background(), cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(dir, filename)
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		// File should exist on disk.
		if !GGUFExists(got) {
			t.Error("expected downloaded file to exist on disk")
		}
	})
}

// ============================================================
// DownloadGGUF — Hash mismatch (corrupt or tampered download)
// ============================================================

func TestDownloadGGUF_HashMismatch_ReturnsErrorAndCleansUp(t *testing.T) {
	fileContent := []byte("this is the actual download content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(fileContent)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := DownloadConfig{
		Repo:     "owner/repo",
		Filename: "model.gguf",
		DestDir:  dir,
		SHA256:   "0000000000000000000000000000000000000000000000000000000000000000", // wrong hash
	}

	withHijackedHTTP(t, srv, func() {
		_, err := DownloadGGUF(context.Background(), cfg)
		if err == nil {
			t.Fatal("expected error for hash mismatch, got nil")
		}
		if !strings.Contains(err.Error(), "integrity check failed") {
			t.Errorf("error should mention integrity check failure: %v", err)
		}
		// Verify the corrupt file was removed — neither .tmp nor final should exist.
		finalPath := filepath.Join(dir, "model.gguf")
		tmpPath := finalPath + ".tmp"
		if _, statErr := os.Stat(finalPath); statErr == nil {
			t.Error("final file should not exist after hash mismatch")
		}
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			t.Error(".tmp file should not exist after hash mismatch")
		}
	})
}

// ============================================================
// DownloadGGUF — MkdirAll creates nested directories
// ============================================================

func TestDownloadGGUF_CreatesDestDir(t *testing.T) {
	fileContent := []byte("dummy model content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(fileContent)
	}))
	defer srv.Close()

	// Compute the SHA-256 of the test content for verification.
	h := sha256.Sum256(fileContent)
	expectedHash := hex.EncodeToString(h[:])

	// Use a deeply nested directory that doesn't exist yet.
	base := t.TempDir()
	dir := filepath.Join(base, "a", "b", "c", "models")
	cfg := DownloadConfig{
		Repo:     "owner/repo",
		Filename: "model.gguf",
		DestDir:  dir,
		SHA256:   expectedHash,
	}

	withHijackedHTTP(t, srv, func() {
		_, err := DownloadGGUF(context.Background(), cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Verify that the dir was created.
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Errorf("expected dest dir to be created, stat error: %v", statErr)
		}
	})
}
