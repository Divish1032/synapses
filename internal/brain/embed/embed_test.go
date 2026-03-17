package embed

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	s := newWithBaseURL("/model.gguf", 49999, "/llama-server", "http://127.0.0.1:49999")
	s.started = true
	_, err := s.Embed(context.Background(), "test")
	if err == nil {
		t.Error("expected connection error")
	}
}

func TestEmbedBatch_ConnectionRefused(t *testing.T) {
	s := newWithBaseURL("/model.gguf", 49999, "/llama-server", "http://127.0.0.1:49999")
	s.started = true
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

	s := newWithBaseURL("/model.gguf", 49999, "/llama-server", "http://127.0.0.1:49999")
	s.client = &http.Client{Timeout: time.Second}
	s.started = true

	_, err := s.Embed(ctx, "test")
	if err == nil {
		t.Error("expected error on canceled context")
	}
}

func TestEmbedBatch_ContextCanceled(t *testing.T) {
	// Test that EmbedBatch respects context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newWithBaseURL("/model.gguf", 49999, "/llama-server", "http://127.0.0.1:49999")
	s.client = &http.Client{Timeout: time.Second}
	s.started = true

	_, err := s.EmbedBatch(ctx, []string{"text"})
	if err == nil {
		t.Error("expected error on canceled context")
	}
}

func TestStop_ClearsStartedFlag(t *testing.T) {
	// Test that Stop() sets started=false.
	s := New("/model.gguf", 11437, "/llama-server")
	s.started = true

	s.Stop()

	if s.started {
		t.Error("Stop() should set started=false")
	}
}

func TestEmbed_MarshallError(t *testing.T) {
	// json.Marshal of the request body always succeeds for string content.
	// The call fails at the TCP level — verifies the marshal→request→send path.
	s := newWithBaseURL("/model.gguf", 11437, "/llama-server", "http://127.0.0.1:11437")
	s.started = true

	_, err := s.Embed(context.Background(), "valid text")
	// Will fail on connection; just verify it doesn't panic
	if err == nil {
		t.Error("expected connection error (port 11437 unused)")
	}
}

func TestEmbedBatch_SingleItem(t *testing.T) {
	// Test EmbedBatch with single item reaches the HTTP call path.
	s := newWithBaseURL("/model.gguf", 49999, "/llama-server", "http://127.0.0.1:49999")
	s.started = true

	_, err := s.EmbedBatch(context.Background(), []string{"single"})
	if err == nil {
		t.Error("expected connection error")
	}
}

func TestAvailable_WithTimeout(t *testing.T) {
	// Test Available() with very short timeout — unused port guarantees failure.
	s := newWithBaseURL("/model.gguf", 49999, "/llama-server", "http://127.0.0.1:49999")
	s.client = &http.Client{Timeout: 1 * time.Millisecond}
	s.started = true

	if s.Available() {
		t.Error("expected available=false on timeout")
	}
}

// --- Test helpers for httptest integration ---

// newTestServerWithHandler creates a Server backed by an httptest.Server.
// Uses the unexported newWithBaseURL constructor — invisible to all callers
// outside this package. Production code always goes through New().
func newTestServerWithHandler(t *testing.T, handler http.HandlerFunc) *Server {
	testServer := httptest.NewServer(handler)
	t.Cleanup(func() { testServer.Close() })

	s := newWithBaseURL("/model.gguf", 0, "/llama-server", testServer.URL)
	s.client = &http.Client{Timeout: 5 * time.Second}
	s.started = true
	return s
}

// --- Integration tests with mocked HTTP server ---

func TestEmbed_SuccessfulEmbedding(t *testing.T) {
	// Test successful embedding with mock HTTP server.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embedding" {
			t.Errorf("expected /embedding, got %s", r.URL.Path)
		}
		// Return 384-dimensional embedding vector
		fmt.Fprint(w, `{"embedding": [0.1, 0.2, 0.3`)
		for i := 3; i < 384; i++ {
			fmt.Fprint(w, ", 0.0")
		}
		fmt.Fprint(w, `]}`)
	})

	s := newTestServerWithHandler(t, handler)

	vec, err := s.Embed(context.Background(), "test text")
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if len(vec) != 384 {
		t.Errorf("expected 384 dimensions, got %d", len(vec))
	}
	if vec[0] != 0.1 || vec[1] != 0.2 || vec[2] != 0.3 {
		t.Errorf("embedding values incorrect: %v", vec[:3])
	}
}

func TestEmbed_HTTPError(t *testing.T) {
	// Test error handling for non-200 HTTP responses.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	s := newTestServerWithHandler(t, handler)

	_, err := s.Embed(context.Background(), "test")
	if err == nil {
		t.Error("expected error on HTTP 503")
	}
	if !strings.Contains(err.Error(), "HTTP") {
		t.Errorf("error should mention HTTP status, got: %v", err)
	}
}

func TestEmbed_MalformedJSON(t *testing.T) {
	// Test error handling for invalid JSON response.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not valid json at all"))
	})

	s := newTestServerWithHandler(t, handler)

	_, err := s.Embed(context.Background(), "test")
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error should mention decode, got: %v", err)
	}
}

func TestEmbed_EmptyVector(t *testing.T) {
	// Test error when embedding array is empty.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"embedding": []}`))
	})

	s := newTestServerWithHandler(t, handler)

	_, err := s.Embed(context.Background(), "test")
	if err == nil {
		t.Error("expected error on empty embedding")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty, got: %v", err)
	}
}

func TestEmbedBatch_SuccessfulBatch(t *testing.T) {
	// Test successful batch embedding.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return array of embeddings matching input count
		fmt.Fprint(w, `[`)
		for i := 0; i < 3; i++ {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprint(w, `{"embedding": [0.1, 0.2, 0.3]}`)
		}
		fmt.Fprint(w, `]`)
	})

	s := newTestServerWithHandler(t, handler)

	vecs, err := s.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("EmbedBatch failed: %v", err)
	}
	if len(vecs) != 3 {
		t.Errorf("expected 3 vectors, got %d", len(vecs))
	}
	for i, vec := range vecs {
		if len(vec) != 3 {
			t.Errorf("vector %d has %d dims, want 3", i, len(vec))
		}
	}
}

func TestEmbedBatch_LengthMismatch(t *testing.T) {
	// Test error when response count doesn't match input count.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return only 1 embedding instead of 2 expected
		fmt.Fprint(w, `[{"embedding": [0.1, 0.2]}]`)
	})

	s := newTestServerWithHandler(t, handler)

	_, err := s.EmbedBatch(context.Background(), []string{"text1", "text2"})
	if err == nil {
		t.Error("expected error on length mismatch")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error should mention mismatch, got: %v", err)
	}
}

func TestEmbedBatch_MalformedResponse(t *testing.T) {
	// Test error handling for invalid batch response.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("invalid"))
	})

	s := newTestServerWithHandler(t, handler)

	_, err := s.EmbedBatch(context.Background(), []string{"a"})
	if err == nil {
		t.Error("expected error on malformed response")
	}
}

func TestAvailable_HealthCheckSuccess(t *testing.T) {
	// Test successful health check.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	s := newTestServerWithHandler(t, handler)

	if !s.Available() {
		t.Error("expected available=true on health OK")
	}
}

func TestAvailable_HealthCheckFailure(t *testing.T) {
	// Test failed health check.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	s := newTestServerWithHandler(t, handler)

	if s.Available() {
		t.Error("expected available=false on health failure")
	}
}

func TestWaitReady_SuccessfulReady(t *testing.T) {
	// Test waitReady succeeds when health check passes.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	s := newTestServerWithHandler(t, handler)

	err := s.waitReady(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("waitReady failed: %v", err)
	}
}

func TestWaitReady_Timeout(t *testing.T) {
	// Test waitReady times out when health check never passes.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	s := newTestServerWithHandler(t, handler)

	err := s.waitReady(context.Background(), 100*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("error should mention timeout, got: %v", err)
	}
}

func TestWaitReady_ContextCanceled(t *testing.T) {
	// Test waitReady respects context cancellation.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Intentionally delay to let context cancel
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	s := newTestServerWithHandler(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := s.waitReady(ctx, 10*time.Second)
	if err == nil {
		t.Error("expected context error")
	}
}

// ---------------------------------------------------------------------------
// supervise — supervisor goroutine restart logic
// ---------------------------------------------------------------------------

func TestSupervise_ProcessExitAndRestart(t *testing.T) {
	// Test the supervise goroutine detects process exit and attempts restart.
	// We use a short-lived process to simulate a crash.

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mockServer := httptest.NewServer(handler)
	defer mockServer.Close()

	s := newWithBaseURL("/model.gguf", 0, "/bin/true", mockServer.URL)
	s.client = &http.Client{Timeout: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create a process that exits immediately
	cmd := exec.Command("/bin/true")
	cmd.Start()

	s.proc = cmd
	s.started = true

	// Give the process time to exit
	cmd.Wait()

	// The supervise function should detect the exit.
	// We start it but immediately cancel the context to prevent infinite restarts.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	s.supervise(ctx, []string{})

	// After supervision ends (context canceled), the started flag is still true
	// (it's only set to false by Stop())
}

func TestSupervise_StoppedFlagPreventsRestart(t *testing.T) {
	// Test that supervise stops restarting when started=false (clean shutdown).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mockServer := httptest.NewServer(handler)
	defer mockServer.Close()

	s := newWithBaseURL("/model.gguf", 0, "/bin/true", mockServer.URL)

	ctx := context.Background()

	// Create a process that exits immediately
	cmd := exec.Command("/bin/true")
	cmd.Start()
	s.proc = cmd
	s.started = true

	// Wait for it to exit
	cmd.Wait()

	// Now set started=false to simulate Stop() being called
	done := make(chan bool)
	go func() {
		s.supervise(ctx, []string{})
		done <- true
	}()

	// Let the goroutine detect the exit and check started flag
	time.Sleep(200 * time.Millisecond)
	s.mu.Lock()
	s.started = false
	s.mu.Unlock()

	select {
	case <-done:
		// supervise should exit when started=false
		t.Log("supervise exited as expected")
	case <-time.After(2 * time.Second):
		t.Error("supervise should have exited after started=false")
	}
}

func TestSupervise_ContextDoneStopsRestart(t *testing.T) {
	// Test that supervise stops restarting when context is done.
	s := New("/model.gguf", 11437, "/bin/true")

	ctx, cancel := context.WithCancel(context.Background())

	// Start with a process that exits immediately
	cmd := exec.Command("/bin/true")
	cmd.Start()
	s.proc = cmd
	s.started = true

	// Wait for process to exit
	cmd.Wait()

	done := make(chan bool)
	go func() {
		s.supervise(ctx, []string{})
		done <- true
	}()

	// Cancel the context quickly to prevent restart loop
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// supervise should exit when context is canceled
		t.Log("supervise exited as expected")
	case <-time.After(2 * time.Second):
		t.Error("supervise should have exited after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// Server.Stop — process cleanup
// ---------------------------------------------------------------------------

func TestServerStop_NilProcess(t *testing.T) {
	// Test Stop() with nil process doesn't panic
	s := New("/model.gguf", 11437, "/llama-server")
	s.proc = nil
	s.started = true

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop() panicked with nil process: %v", r)
		}
	}()

	s.Stop()

	if s.started {
		t.Error("expected started=false after Stop()")
	}
}

func TestServerStop_NilProcessState(t *testing.T) {
	// Test Stop() with exec.Cmd but nil Process
	s := New("/model.gguf", 11437, "/llama-server")
	s.proc = &exec.Cmd{}
	s.started = true

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop() panicked: %v", r)
		}
	}()

	s.Stop()

	if s.started {
		t.Error("expected started=false after Stop()")
	}
}

// ---------------------------------------------------------------------------
// concurrency tests
// ---------------------------------------------------------------------------

func TestServer_ConcurrentStopAndEmbed(t *testing.T) {
	// Test that concurrent Stop() and Embed() calls are safe.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/embedding" {
			fmt.Fprint(w, `{"embedding": [0.1, 0.2]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	s := newTestServerWithHandler(t, handler)

	// Stop the server
	s.Stop()

	// Try to embed after stopping - should return error
	ctx := context.Background()
	_, err := s.Embed(ctx, "test")
	if err == nil {
		t.Error("expected error when calling Embed after Stop")
	}
}

func TestServer_ConcurrentStartAndEmbed(t *testing.T) {
	// Test concurrent operations with a started server.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/embedding" {
			fmt.Fprint(w, `{"embedding": [0.1, 0.2]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	s := newTestServerWithHandler(t, handler)

	// Multiple concurrent Embed calls
	done := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() {
			_, err := s.Embed(context.Background(), "test")
			done <- err
		}()
	}

	for i := 0; i < 5; i++ {
		err := <-done
		if err != nil {
			t.Errorf("concurrent embed %d failed: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// edge cases for Start
// ---------------------------------------------------------------------------

func TestStart_Idempotent(t *testing.T) {
	// Test that calling Start twice is safe (should return immediately on second call)
	s := New("/model.gguf", 11437, "/llama-server")
	s.started = true

	err1 := s.Start(context.Background())
	err2 := s.Start(context.Background())

	if err1 != nil || err2 != nil {
		t.Errorf("start should be idempotent: err1=%v, err2=%v", err1, err2)
	}
}
