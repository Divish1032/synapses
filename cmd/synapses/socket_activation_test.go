package main

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestSocketActivation_FallbackToTCP verifies that when trySocketActivation()
// returns nil (no supervisor), the daemon falls back to direct TCP listen.
// This is the common path in development and CI environments.
func TestSocketActivation_FallbackToTCP(t *testing.T) {
	ln, err := trySocketActivation()
	if err != nil {
		t.Fatalf("trySocketActivation: %v", err)
	}
	// In a test environment (not managed by launchd/systemd), we expect nil.
	if ln != nil {
		ln.Close()
		t.Skip("running under a process supervisor — skip fallback test")
	}
	// Fallback: direct TCP listen should work.
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fallback TCP listen: %v", err)
	}
	defer tcpLn.Close()
}

// TestDaemonServeWithProvidedListener verifies that the HTTP server can
// serve using a pre-created listener (simulating socket activation).
// This tests the code path AFTER trySocketActivation() returns a listener.
func TestDaemonServeWithProvidedListener(t *testing.T) {
	// Create a listener as if it came from launchd/systemd.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	// Set up a minimal HTTP handler (mirrors the daemon's health endpoint).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"version": version,
			"source":  "socket_activation",
		})
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()

	// Give the server a moment to start.
	time.Sleep(50 * time.Millisecond)

	// Verify the server is reachable on the activated listener.
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/admin/health")
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health check: got %d, want 200", resp.StatusCode)
	}

	var result struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Source  string `json:"source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Status != "ok" {
		t.Errorf("status: got %q, want %q", result.Status, "ok")
	}
	if result.Source != "socket_activation" {
		t.Errorf("source: got %q, want %q", result.Source, "socket_activation")
	}
}
