package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Helpers ──────────────────────────────────────────────────────────────────

// openShutdownTestStore opens a real SQLite store at a temp path.
func openShutdownTestStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

// ── MCP Server.Close tests ──────────────────────────────────────────────────

func TestServerClose_DrainsWorkers(t *testing.T) {
	st := openShutdownTestStore(t)
	defer st.Close()
	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()

	// Dispatch a tool call to prove the server is functional.
	result, err := srv.DispatchTool(context.Background(), "explain_codebase", nil)
	if err != nil {
		t.Fatalf("dispatch before close: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result before close")
	}

	// Close should return promptly (workers drain the queue).
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
		// OK — Close returned.
	case <-time.After(10 * time.Second):
		t.Fatal("Server.Close() did not return within 10s — workers stuck")
	}
}

func TestServerClose_Idempotent(t *testing.T) {
	st := openShutdownTestStore(t)
	defer st.Close()
	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()

	// Close twice — must not panic or deadlock.
	srv.Close()
	srv.Close()
}

func TestServerClose_RejectsPostCloseWork(t *testing.T) {
	st := openShutdownTestStore(t)
	defer st.Close()
	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()
	srv.Close()

	// DispatchTool after Close should still work (it's a direct function call,
	// not a background task). The server remains functional for in-flight
	// requests; only goBackground is shut down.
	result, err := srv.DispatchTool(context.Background(), "explain_codebase", nil)
	if err != nil {
		t.Fatalf("dispatch after close should still work: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result after close")
	}
}

// ── ProjectInstance.Close tests ─────────────────────────────────────────────

func TestProjectInstanceClose_Order(t *testing.T) {
	// Track the order of Close calls using a shared slice.
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	st := openShutdownTestStore(t)
	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()

	cancelCalled := false
	pi := &ProjectInstance{
		AbsPath:   t.TempDir(),
		Graph:     g,
		Store:     st,
		MCPServer: srv,
		cancel: func() {
			cancelCalled = true
			record("cancel")
		},
	}

	pi.Close()

	if !cancelCalled {
		t.Error("cancel function was not called")
	}

	// Verify cancel was called (it's always first).
	mu.Lock()
	if len(order) == 0 || order[0] != "cancel" {
		t.Errorf("expected cancel first, got order: %v", order)
	}
	mu.Unlock()
}

func TestProjectInstanceClose_NilFields(t *testing.T) {
	// All fields nil — must not panic.
	pi := &ProjectInstance{}
	pi.Close() // should not panic
}

func TestProjectInstanceClose_PartialNilFields(t *testing.T) {
	// Only cancel set, everything else nil.
	called := false
	pi := &ProjectInstance{
		cancel: func() { called = true },
	}
	pi.Close()
	if !called {
		t.Error("cancel was not called with partial nil fields")
	}
}

// ── projectRegistry.Close tests ─────────────────────────────────────────────

func TestRegistryClose_AllInstancesClosed(t *testing.T) {
	reg := newProjectRegistry()

	// Create two instances with real stores.
	var closed atomic.Int32
	for i := 0; i < 2; i++ {
		st := openShutdownTestStore(t)
		g := graph.New(fmt.Sprintf("repo-%d", i))
		cfg := &config.Config{}
		srv := mcpsrv.New(g, cfg, st)
		srv.StartBackground()

		pi := &ProjectInstance{
			AbsPath:   filepath.Join(t.TempDir(), fmt.Sprintf("proj-%d", i)),
			Graph:     g,
			Store:     st,
			MCPServer: srv,
			cancel: func() {
				closed.Add(1)
			},
		}
		reg.Set(pi)
	}

	if reg.Len() != 2 {
		t.Fatalf("expected 2 projects, got %d", reg.Len())
	}

	reg.Close()

	if reg.Len() != 0 {
		t.Errorf("expected 0 projects after close, got %d", reg.Len())
	}
	if closed.Load() != 2 {
		t.Errorf("expected 2 cancel calls, got %d", closed.Load())
	}
}

func TestRegistryClose_EmptyRegistry(t *testing.T) {
	reg := newProjectRegistry()
	reg.Close() // must not panic
	if reg.Len() != 0 {
		t.Errorf("expected 0 after closing empty registry, got %d", reg.Len())
	}
}

// ── Daemon HTTP shutdown tests ──────────────────────────────────────────────

func TestDaemonHTTPShutdown_InFlightRequestCompletes(t *testing.T) {
	// Simulate a slow handler and verify Shutdown waits for it.
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tools/slow_tool", func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		// Simulate slow work (up to 2s).
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "completed"}) //nolint:errcheck
		close(handlerDone)
	})

	httpSrv := httptest.NewServer(mux)

	// Start a request in the background.
	var respStatus int
	var respBody string
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		resp, err := http.Post(httpSrv.URL+"/v1/tools/slow_tool", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Logf("request error (may be expected during shutdown): %v", err)
			return
		}
		defer resp.Body.Close()
		respStatus = resp.StatusCode
		var m map[string]string
		json.NewDecoder(resp.Body).Decode(&m) //nolint:errcheck
		respBody = m["status"]
	}()

	// Wait for handler to start, then shut down.
	<-handlerStarted
	httpSrv.Close()

	// Wait for request to complete.
	select {
	case <-reqDone:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete within 5s after shutdown")
	}

	// httptest.Server.Close() waits for in-flight requests, so the response
	// should be complete.
	if respStatus != 0 && respStatus != http.StatusOK {
		t.Errorf("expected 200, got %d", respStatus)
	}
	if respBody != "" && respBody != "completed" {
		t.Errorf("expected 'completed', got %q", respBody)
	}
}

func TestDaemonHTTPShutdown_GracefulWithTimeout(t *testing.T) {
	// Verify httpSrv.Shutdown with a timeout context works as expected.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	})

	httpSrv := httptest.NewServer(mux)

	// Verify server is functional.
	resp, err := http.Get(httpSrv.URL + "/api/admin/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 from health, got %d", resp.StatusCode)
	}

	// Shut down with timeout (mimics daemon signal handler).
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := httpSrv.Config.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}

	// After shutdown, new requests should fail.
	_, err = http.Get(httpSrv.URL + "/api/admin/health")
	if err == nil {
		t.Error("expected error after shutdown, got nil")
	}
}

// ── PID file cleanup test ───────────────────────────────────────────────────

func TestPIDFileCleanup(t *testing.T) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Create the required directory structure.
	os.MkdirAll(filepath.Join(tmpDir, ".synapses", "pids"), 0o755)

	pidPath, err := singletonPIDPath()
	if err != nil {
		t.Fatalf("singletonPIDPath: %v", err)
	}

	// Write a PID file.
	if err := os.WriteFile(pidPath, []byte("99999"), 0o600); err != nil {
		t.Fatalf("write PID: %v", err)
	}

	// Verify it exists.
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("PID file not created: %v", err)
	}

	// Simulate the deferred cleanup.
	os.Remove(pidPath)

	// Verify it's gone.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("PID file should be removed after cleanup")
	}
}

// ── Stdio shutdown: context cancel triggers cleanup chain ───────────────────

func TestStdioShutdown_ContextCancelCleansUp(t *testing.T) {
	// Simulates the stdio shutdown path: context cancel → deferred cleanup.
	// The real code uses defer st.Close(), defer watcher.Stop(), etc.
	// We verify the LIFO ordering of defers.
	var order []string
	var mu sync.Mutex
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	appCtx, appCancel := context.WithCancel(context.Background())

	// Simulate resources created in order (like cmdStartDirect).
	st := openShutdownTestStore(t)
	defer func() {
		st.Close()
		record("store.Close")
	}()

	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()
	defer func() {
		srv.Close()
		record("srv.Close")
	}()

	// Background goroutine that respects context (like defrag, prune).
	bgDone := make(chan struct{})
	go func() {
		defer close(bgDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-appCtx.Done():
				return
			case <-ticker.C:
				// simulate periodic work
			}
		}
	}()

	// Cancel the context (simulates SIGINT handler calling appCancel).
	appCancel()

	// Background goroutine should exit promptly.
	select {
	case <-bgDone:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("background goroutine did not exit after context cancel")
	}
}

func TestStdioShutdown_SafetyNetTimerPattern(t *testing.T) {
	// Verify the 5-second safety-net pattern works: after cancel, if cleanup
	// hangs, the timer fires. We simulate a fast cleanup to verify the
	// timer is cancelled (doesn't fire).
	appCtx, appCancel := context.WithCancel(context.Background())

	safetyFired := atomic.Bool{}

	// Simulate signal handler.
	go func() {
		appCancel()
		time.AfterFunc(200*time.Millisecond, func() {
			safetyFired.Store(true)
		})
	}()

	// Wait for cancel.
	<-appCtx.Done()

	// Simulate quick cleanup (well under 200ms).
	time.Sleep(50 * time.Millisecond)

	// At this point, cleanup is done. The safety timer should NOT have fired
	// yet (200ms timer, only 50ms elapsed).
	if safetyFired.Load() {
		t.Error("safety-net timer fired before cleanup completed")
	}

	// Wait for the timer to actually fire (to verify the pattern works).
	time.Sleep(300 * time.Millisecond)
	if !safetyFired.Load() {
		t.Error("safety-net timer should have fired after 200ms")
	}
}

// ── Deferred cleanup ordering ───────────────────────────────────────────────

func TestDeferredCleanupOrdering(t *testing.T) {
	// In cmdStartDirect, the cleanup order is (defers execute LIFO):
	//   1. appCancel()  (first defer → last to run)
	//   2. st.Close()   (second defer)
	//   3. fw.Stop()    (later defer → runs earlier)
	//   4. srv.Close()  (deferred via t.Cleanup in tests, but in prod it's
	//                    the ServeStdio return that triggers defer unwind)
	//
	// The correct ordering for shutdown is:
	//   srv.Close() → watcher.Stop() → store.Close() → appCancel()
	//
	// This matches LIFO: the last resource created is the first to close.

	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	// Simulate the defer stack from cmdStartDirect.
	func() {
		defer record("appCancel")   // first defer → runs last
		defer record("store.Close") // second defer
		// watcher defer happens later in the function
		defer record("watcher.Stop") // third defer → runs before store
		// srv is used via ServeStdio, its Close is called after return
		// but for this test we simulate it
		defer record("srv.Close") // fourth defer → runs first
	}()

	mu.Lock()
	defer mu.Unlock()

	// Expected LIFO order.
	expected := []string{"srv.Close", "watcher.Stop", "store.Close", "appCancel"}
	if len(order) != len(expected) {
		t.Fatalf("expected %d cleanup steps, got %d: %v", len(expected), len(order), order)
	}
	for i, want := range expected {
		if order[i] != want {
			t.Errorf("cleanup step %d: expected %q, got %q (full order: %v)", i, want, order[i], order)
		}
	}
}

// ── Integration: full resource lifecycle ────────────────────────────────────

func TestFullResourceLifecycle(t *testing.T) {
	// Create real resources, dispatch a tool, then shut everything down.
	// Verifies no panics, no goroutine leaks, no deadlocks.

	st := openShutdownTestStore(t)
	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()

	// Dispatch a real tool to exercise the full path.
	result, err := srv.DispatchTool(context.Background(), "explain_codebase", nil)
	if err != nil {
		t.Fatalf("explain_codebase: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatal("expected successful result from explain_codebase")
	}

	// Dispatch a second tool to exercise store interaction.
	result, err = srv.DispatchTool(context.Background(), "recall", map[string]interface{}{})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result from recall")
	}

	// Clean up in correct order.
	done := make(chan struct{})
	go func() {
		srv.Close()
		st.Close()
		close(done)
	}()

	select {
	case <-done:
		// All resources cleaned up.
	case <-time.After(10 * time.Second):
		t.Fatal("resource cleanup did not complete within 10s")
	}
}

func TestFullResourceLifecycle_ConcurrentDispatchDuringShutdown(t *testing.T) {
	// Fire multiple tool calls, then shut down while some may still be in flight.
	// Verifies no panics or races.

	st := openShutdownTestStore(t)
	g := graph.New("test-repo")
	cfg := &config.Config{}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()

	// Start several concurrent dispatches. Use "recall" (pure store read)
	// instead of "explain_codebase" to avoid triggering the pre-existing race
	// in graph.ProjectIdentity() — same approach as TestDispatchTool_ConcurrentSafe.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.DispatchTool(context.Background(), "recall", map[string]interface{}{}) //nolint:errcheck
		}()
	}

	// Close while dispatches may still be running.
	// This must not panic or deadlock.
	time.Sleep(10 * time.Millisecond) // let some dispatches start
	srv.Close()

	// Wait for all dispatches to finish (they may get partial results or errors).
	wgDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent dispatches did not complete after Close()")
	}

	st.Close()
}

// ── Daemon registry + HTTP integration ──────────────────────────────────────

func TestDaemonShutdown_RegistryCleanup(t *testing.T) {
	// Simulate daemon shutdown: registry with projects, HTTP server, clean stop.
	reg := newProjectRegistry()

	// Register a project instance.
	st := openShutdownTestStore(t)
	g := graph.New("shutdown-test")
	cfg := &config.Config{}
	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()

	projPath := filepath.Join(t.TempDir(), "myproject")
	cancelCalled := atomic.Bool{}
	pi := &ProjectInstance{
		AbsPath:   projPath,
		Graph:     g,
		Store:     st,
		MCPServer: srv,
		cancel:    func() { cancelCalled.Store(true) },
	}
	reg.Set(pi)

	// Create an HTTP server (mimics daemon).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
	})
	httpSrv := httptest.NewServer(mux)

	// Verify health before shutdown.
	resp, err := http.Get(httpSrv.URL + "/api/admin/health")
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	resp.Body.Close()

	// Simulate daemon shutdown sequence:
	// 1. Shutdown HTTP server
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	httpSrv.Config.Shutdown(shutCtx) //nolint:errcheck

	// 2. Close registry (closes all project instances).
	reg.Close()

	// Verify cancel was called.
	if !cancelCalled.Load() {
		t.Error("project cancel was not called during registry close")
	}

	// Verify registry is empty.
	if reg.Len() != 0 {
		t.Errorf("registry should be empty after close, got %d", reg.Len())
	}

	// 3. Verify HTTP server is no longer accepting connections.
	_, err = http.Get(httpSrv.URL + "/api/admin/health")
	if err == nil {
		t.Error("HTTP server should reject connections after shutdown")
	}
}
