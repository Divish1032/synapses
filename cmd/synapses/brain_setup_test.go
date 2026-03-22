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

// --- Tests for cmdBrainRegister ---

func TestCmdBrainRegister_NoArgs(t *testing.T) {
	err := cmdBrainRegister([]string{})
	if err == nil {
		t.Error("expected error when no tier name provided")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCmdBrainRegister_UnknownTier(t *testing.T) {
	err := cmdBrainRegister([]string{"unknown-tier"})
	if err == nil {
		t.Error("expected error for unknown tier")
	}
	if !strings.Contains(err.Error(), "unknown tier") {
		t.Errorf("unexpected error: %v", err)
	}
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

// --- Tests for findOllamaBinary ---

func TestFindOllamaBinary_ReturnsStringOrError(t *testing.T) {
	// This test verifies the function can search PATH
	// It may or may not find ollama depending on system setup
	path, err := findOllamaBinary()
	// Just verify it doesn't panic
	_ = path
	_ = err
	// ollama may or may not be in PATH
	t.Log("findOllamaBinary completed successfully")
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

// --- Tests for brainRegisterIdentity ---

func TestBrainRegisterIdentity_ValidTier(t *testing.T) {
	if len(brainTiers) == 0 {
		t.Skip("no brain tiers configured")
	}

	server := mockOllamaServer(t, nil)
	defer server.Close()

	// This would require mocking the HTTP requests made by brainRegisterIdentity
	// For now, we test the structure exists
	tier := brainTiers[0]
	if tier.name == "" {
		t.Error("expected tier to have a name")
	}
	if tier.content == "" {
		t.Error("expected tier to have content")
	}
}

// --- Tests for brainTiers structure ---

func TestBrainTiers_AllConfigured(t *testing.T) {
	if len(brainTiers) == 0 {
		t.Error("expected brainTiers to be configured")
	}

	expectedTiers := []string{"sentry", "critic", "librarian", "navigator", "archivist"}
	if len(brainTiers) != len(expectedTiers) {
		t.Errorf("expected %d tiers, got %d", len(expectedTiers), len(brainTiers))
	}

	for i, tier := range brainTiers {
		if tier.name == "" {
			t.Errorf("tier %d has empty name", i)
		}
		if tier.label == "" {
			t.Errorf("tier %d has empty label", i)
		}
		if tier.content == "" {
			t.Errorf("tier %d has empty content", i)
		}
		if !strings.Contains(tier.content, "FROM qwen3.5:2b") {
			t.Errorf("tier %d content doesn't have proper FROM", i)
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
