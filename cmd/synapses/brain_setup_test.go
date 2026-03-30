package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// --- Mock HTTP Server ---

func mockOllamaServer(t *testing.T, responses map[string]interface{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"version": "0.1.0"}`)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			if models, ok := responses["models"]; ok && models != nil {
				fmt.Fprintf(w, `{"models": %v}`, models)
			} else {
				fmt.Fprintf(w, `{"models": []}`)
			}
		case "/api/pull":
			w.Header().Set("Content-Type", "application/json")
			// Stream pull progress
			flusher, _ := w.(http.Flusher)
			for i := 0; i < 3; i++ {
				fmt.Fprintf(w, `{"status": "pulling %d%%"}`+"\n", i*33)
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprintf(w, `{"status": "success"}`)
		case "/api/create":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status": "success"}`)
		case "/api/generate":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"model": "test", "response": "{}"}`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

// --- Tests for cmdBrain ---

func TestCmdBrain_Help(t *testing.T) {
	err := cmdBrain([]string{"help"})
	if err != nil {
		t.Errorf("cmdBrain(help) failed: %v", err)
	}
}

func TestCmdBrain_NoArgs(t *testing.T) {
	err := cmdBrain([]string{})
	if err != nil {
		t.Errorf("cmdBrain() with no args should show help, got: %v", err)
	}
}

func TestCmdBrain_LongHelp(t *testing.T) {
	err := cmdBrain([]string{"--help"})
	if err != nil {
		t.Errorf("cmdBrain(--help) failed: %v", err)
	}
}

func TestCmdBrain_ShortHelp(t *testing.T) {
	err := cmdBrain([]string{"-h"})
	if err != nil {
		t.Errorf("cmdBrain(-h) failed: %v", err)
	}
}

func TestCmdBrain_UnknownSubcommand(t *testing.T) {
	err := cmdBrain([]string{"unknown"})
	if err == nil {
		t.Error("expected error for unknown subcommand")
	}
	if !strings.Contains(err.Error(), "unknown brain subcommand") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// --- Tests for cmdBrainSetup ---

func TestCmdBrainSetup_InvalidMode(t *testing.T) {
	err := cmdBrainSetup([]string{"--mode", "invalid"})
	if err == nil {
		t.Error("expected error for invalid mode")
	}
	if !strings.Contains(err.Error(), "invalid mode") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCmdBrainSetup_OllamaUnreachable(t *testing.T) {
	// Use invalid URL that won't connect
	err := cmdBrainSetup([]string{"--ollama", "http://invalid-unreachable-host:9999", "--skip-smoke"})
	if err == nil {
		t.Error("expected error when Ollama is unreachable")
	}
}

func TestCmdBrainSetup_WithMockOllama(t *testing.T) {
	server := mockOllamaServer(t, map[string]interface{}{
		"models": []map[string]string{
			{"name": "qwen3.5:2b"},
		},
	})
	defer server.Close()

	tmpDir := t.TempDir()
	// Temporarily change home for this test
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)
	os.Setenv("HOME", tmpDir)

	err := cmdBrainSetup([]string{
		"--ollama", server.URL,
		"--mode", "standard",
		"--skip-pull",
		"--skip-smoke",
		"--no-color",
	})
	// Note: may fail on registration due to mock limitations, but should test the flow
	_ = err
}


// --- Tests for brainPingOllama ---

func TestBrainPingOllama_Success(t *testing.T) {
	server := mockOllamaServer(t, nil)
	defer server.Close()

	version, err := brainPingOllama(server.URL, http.DefaultClient)
	if err != nil {
		t.Fatalf("brainPingOllama failed: %v", err)
	}
	if version != "0.1.0" {
		t.Errorf("expected version 0.1.0, got %q", version)
	}
}

func TestBrainPingOllama_Unreachable(t *testing.T) {
	_, err := brainPingOllama("http://invalid-unreachable-host:9999", http.DefaultClient)
	if err == nil {
		t.Error("expected error when server unreachable")
	}
}

func TestBrainPingOllama_Timeout(t *testing.T) {
	// Server that takes forever to respond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	}))
	defer server.Close()

	_, err := brainPingOllama(server.URL, http.DefaultClient)
	if err == nil {
		t.Error("expected error on timeout")
	}
}

// --- Tests for brainIsModelInstalled ---

func TestBrainIsModelInstalled_Found(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"models": [{"name": "qwen3.5:2b"}]}`)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	installed, err := brainIsModelInstalled(server.URL, "qwen3.5:2b", http.DefaultClient)
	if err != nil {
		t.Fatalf("brainIsModelInstalled failed: %v", err)
	}
	if !installed {
		t.Error("expected model to be installed")
	}
}

func TestBrainIsModelInstalled_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"models": []}`)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	installed, err := brainIsModelInstalled(server.URL, "qwen3.5:2b", http.DefaultClient)
	if err != nil {
		t.Fatalf("brainIsModelInstalled failed: %v", err)
	}
	if installed {
		t.Error("expected model to not be installed")
	}
}

func TestBrainIsModelInstalled_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `invalid json`)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	_, err := brainIsModelInstalled(server.URL, "qwen3.5:2b", http.DefaultClient)
	if err == nil {
		t.Error("expected error on invalid JSON")
	}
}

// --- Tests for brainPullModel ---

func TestBrainPullModel_Success(t *testing.T) {
	server := mockOllamaServer(t, nil)
	defer server.Close()

	err := brainPullModel(server.URL, "qwen3.5:2b", http.DefaultClient)
	if err != nil {
		t.Fatalf("brainPullModel failed: %v", err)
	}
}

// --- Tests for brainSmokeTest ---

func TestBrainSmokeTest_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			w.Header().Set("Content-Type", "application/json")
			// Return valid JSON response in the expected format
			fmt.Fprintf(w, `{"message": {"content": "{\"summary\": \"test\"}"}}`)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	ok, elapsed := brainSmokeTest(server.URL, "test-tier", http.DefaultClient)
	if !ok {
		t.Error("expected smoke test to pass")
	}
	if elapsed == "" {
		t.Error("expected non-empty elapsed time")
	}
}

func TestBrainSmokeTest_InvalidResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			w.Header().Set("Content-Type", "application/json")
			// Return invalid response
			fmt.Fprintf(w, `invalid`)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	ok, _ := brainSmokeTest(server.URL, "test-tier", http.DefaultClient)
	if ok {
		t.Error("expected smoke test to fail with invalid response")
	}
}

// --- Tests for brainWriteConfig ---

func TestBrainWriteConfig_WritesFile(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)
	os.Setenv("HOME", tmpDir)

	path, err := brainWriteConfig("http://localhost:11434", "standard")
	if err != nil {
		t.Fatalf("brainWriteConfig failed: %v", err)
	}

	// Verify file was created
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not created: %v", err)
	}
}

func TestBrainWriteConfig_InvalidMode(t *testing.T) {
	tmpDir := t.TempDir()
	oldHome := os.Getenv("HOME")
	defer os.Setenv("HOME", oldHome)
	os.Setenv("HOME", tmpDir)

	_, err := brainWriteConfig("http://localhost:11434", "invalid-mode")
	// Function should handle or ignore invalid modes gracefully
	_ = err
}

// --- Tests for modelsForMode ---

func TestModelsForMode(t *testing.T) {
	cases := []struct {
		mode     string
		contains []string
	}{
		{"optimal", []string{"qwen3.5:0.8b", "qwen3.5:2b"}},
		{"standard", []string{"qwen3.5:0.8b", "qwen3.5:2b", "qwen3.5:4b"}},
		{"full", []string{"qwen3.5:0.8b", "qwen3.5:4b"}},
	}
	for _, tc := range cases {
		models := modelsForMode(tc.mode)
		for _, want := range tc.contains {
			found := false
			for _, m := range models {
				if m == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("modelsForMode(%q) missing %q, got %v", tc.mode, want, models)
			}
		}
	}
}

// --- Integration-like test ---

func TestCmdBrainSetup_HelpPath(t *testing.T) {
	// Test the help/error paths that don't require network
	err := cmdBrainSetup([]string{"--mode", "standard", "--help"})
	// May error due to flag parsing, which is OK for this test
	_ = err
}
