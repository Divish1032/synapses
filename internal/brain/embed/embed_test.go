package embed

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	s := New("/path/to/model.gguf", 11437, "/path/to/llama-server")
	if s == nil {
		t.Fatal("New returned nil")
	}
	if s.modelPath != "/path/to/model.gguf" {
		t.Error("modelPath not set")
	}
	if s.port != 11437 {
		t.Error("port not set")
	}
	if s.llamaBin != "/path/to/llama-server" {
		t.Error("llamaBin not set")
	}
	if s.client == nil {
		t.Error("client not set")
	}
	if s.started {
		t.Error("should not be started")
	}
}

func TestEmbed_NotStarted(t *testing.T) {
	s := New("/model.gguf", 11437, "/llama-server")
	_, err := s.Embed(context.Background(), "test text")
	if err == nil {
		t.Fatal("expected error when not started")
	}
}

func TestEmbedBatch_NotStarted(t *testing.T) {
	s := New("/model.gguf", 11437, "/llama-server")
	_, err := s.EmbedBatch(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error when not started")
	}
}

func TestEmbedBatch_EmptyInput(t *testing.T) {
	s := New("/model.gguf", 11437, "/llama-server")
	s.started = true
	result, err := s.EmbedBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil for empty input")
	}
}

func TestAvailable_NotStarted(t *testing.T) {
	s := New("/model.gguf", 11437, "/llama-server")
	if s.Available() {
		t.Error("should not be available when not started")
	}
}

func TestStop_NotStarted(t *testing.T) {
	s := New("/model.gguf", 11437, "/llama-server")
	s.Stop()
	if s.started {
		t.Error("should remain not started")
	}
}

func TestStart_MissingBinary(t *testing.T) {
	s := New("/tmp/nonexistent-model.gguf", 11437, "/nonexistent/llama-server")
	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestStart_MissingModel(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New("/nonexistent/model.gguf", 11437, fakeBin)
	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestStart_AlreadyStarted(t *testing.T) {
	s := New("/model.gguf", 11437, "/llama-server")
	s.started = true
	err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("re-start should be no-op, got: %v", err)
	}
}

func TestEmbed_ConnectionRefused(t *testing.T) {
	s := &Server{
		started: true,
		client:  &http.Client{},
		port:    49999, // unlikely to have anything running
	}
	_, err := s.Embed(context.Background(), "test")
	if err == nil {
		t.Error("expected connection error")
	}
}

func TestEmbedBatch_ConnectionRefused(t *testing.T) {
	s := &Server{
		started: true,
		client:  &http.Client{},
		port:    49999,
	}
	_, err := s.EmbedBatch(context.Background(), []string{"a"})
	if err == nil {
		t.Error("expected connection error")
	}
}

func TestLlamaServerBinPath(t *testing.T) {
	dir := "/usr/local/bin"
	path := LlamaServerBinPath(dir)
	expected := filepath.Join(dir, "llama-server")
	if runtime.GOOS == "windows" {
		expected = filepath.Join(dir, "llama-server.exe")
	}
	if path != expected {
		t.Errorf("got %q, want %q", path, expected)
	}
}

func TestEmbedModelPath(t *testing.T) {
	path := EmbedModelPath("/models", "")
	if path != filepath.Join("/models", EmbedModelFilename) {
		t.Errorf("default filename: got %q", path)
	}
	path2 := EmbedModelPath("/models", "custom.gguf")
	if path2 != filepath.Join("/models", "custom.gguf") {
		t.Errorf("custom filename: got %q", path2)
	}
}

func TestLlamaCPPReleaseURL(t *testing.T) {
	url, err := llamaCPPReleaseURL("b5618")
	if err != nil {
		t.Fatal(err)
	}
	if url == "" {
		t.Error("expected non-empty URL")
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	if fileExists(f) {
		t.Error("should not exist")
	}
	os.WriteFile(f, []byte("x"), 0o644)
	if !fileExists(f) {
		t.Error("should exist")
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		b    int64
		want string
	}{
		{500, "0 KB"},
		{1024 * 1024, "1 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
		{274 * 1024 * 1024, "274 MB"},
	}
	for _, tt := range tests {
		got := humanBytes(tt.b)
		if got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.b, got, tt.want)
		}
	}
}

func TestLogProgress_Nil(t *testing.T) {
	logProgress(nil, "test %s", "msg") // should not panic
}

func TestDownloadOptions_HttpClient(t *testing.T) {
	opts := DownloadOptions{}
	c := opts.httpClient()
	if c == nil {
		t.Error("default client should not be nil")
	}
}

func TestProgressReader(t *testing.T) {
	data := make([]byte, 100)
	for i := range data {
		data[i] = 'a'
	}
	pr := &progressReader{
		r:     bytes.NewReader(data),
		w:     nil, // no output
		total: 100,
	}
	buf := make([]byte, 50)
	n, err := pr.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 50 {
		t.Errorf("read %d, want 50", n)
	}
	if pr.received != 50 {
		t.Errorf("received %d, want 50", pr.received)
	}
}

// --- Additional error-path and integration tests ---

// Note: The embed.Server hardcodes "127.0.0.1:port", so we can't easily use
// httptest servers without refactoring the Server to accept a base URL.
// For now, we test the error paths that don't require a running server.

func TestEmbed_ContextCanceled(t *testing.T) {
	// Test that Embed respects context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel

	s := &Server{
		started: true,
		client:  &http.Client{Timeout: time.Second},
		port:    49999,
	}

	_, err := s.Embed(ctx, "test")
	if err == nil {
		t.Error("expected error on canceled context")
	}
}

func TestEmbedBatch_ContextCanceled(t *testing.T) {
	// Test that EmbedBatch respects context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := &Server{
		started: true,
		client:  &http.Client{Timeout: time.Second},
		port:    49999,
	}

	_, err := s.EmbedBatch(ctx, []string{"text"})
	if err == nil {
		t.Error("expected error on canceled context")
	}
}

func TestStop_ClearsStartedFlag(t *testing.T) {
	// Test that Stop() sets started=false.
	s := &Server{
		modelPath: "/model.gguf",
		port:      11437,
		llamaBin:  "/llama-server",
		started:   true,
	}

	s.Stop()

	if s.started {
		t.Error("Stop() should set started=false")
	}
}

func TestEmbed_MarshallError(t *testing.T) {
	// Test json.Marshal of request body (normally succeeds, but verify path).
	s := &Server{
		started: true,
		client:  &http.Client{},
		port:    11437,
	}

	// Normal case should marshal without error
	_, err := s.Embed(context.Background(), "valid text")
	// Will fail on connection, but marshalling succeeds
	if err == nil {
		t.Error("expected connection error (port 11437 unused)")
	}
}

func TestEmbedBatch_EdgeCases(t *testing.T) {
	// Test EmbedBatch with single item.
	s := &Server{
		started: true,
		client:  &http.Client{},
		port:    49999,
	}

	_, err := s.EmbedBatch(context.Background(), []string{"single"})
	if err == nil {
		t.Error("expected connection error")
	}
}

func TestAvailable_WithTimeout(t *testing.T) {
	// Test Available() with very short timeout.
	s := &Server{
		started: true,
		client:  &http.Client{Timeout: 1 * time.Millisecond},
		port:    49999, // unused port → timeout
	}

	available := s.Available()
	if available {
		t.Error("expected available=false on timeout")
	}
}
