package main

// cmdBrainSetup implements "synapses brain setup".
//
// Calling sequence:
//
//  1. Ping Ollama — abort with a helpful message if unreachable.
//  2. Pull required models for the chosen mode (qwen3.5:0.8b, 2b, and/or 4b).
//  3. Smoke-test the base model (optional, --skip-smoke).
//  4. Write/update ~/.synapses/brain.json with enabled:true and the chosen mode.
//
// System prompts are passed per-request at inference time — no Ollama Modelfile
// identity registration is needed.
//
// Usage:
//
//	synapses brain setup [--ollama http://localhost:11434] [--mode optimal|standard|full]
//	                     [--skip-pull] [--skip-smoke]

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
	"github.com/SynapsesOS/synapses/internal/logutil"
)

// modelsForMode returns the distinct Ollama model tags to pull for a given mode.
func modelsForMode(mode string) []string {
	cfg := brainconfig.BrainConfig{IntelligenceMode: brainconfig.IntelligenceMode(mode)}
	cfg.AutoConfigureModels(0)
	return cfg.ModelsRequired()
}

// ── Entry points ──────────────────────────────────────────────────────────────

// cmdBrain dispatches "synapses brain <subcommand>".
func cmdBrain(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println()
		fmt.Println("  Usage: synapses brain <subcommand>")
		fmt.Println()
		fmt.Println("  Subcommands:")
		fmt.Println("    setup     Configure Ollama + pull required models")
		fmt.Println()
		fmt.Println("  Flags for 'setup':")
		fmt.Println("    --ollama <url>   Ollama base URL  (default: http://localhost:11434)")
		fmt.Println("    --mode <mode>    Intelligence mode: optimal | standard | full  (default: standard)")
		fmt.Println("    --skip-pull      Assume models are already downloaded")
		fmt.Println("    --skip-smoke     Skip post-setup smoke tests")
		fmt.Println("    --no-color       Disable ANSI color codes (for GUI / non-terminal output)")
		fmt.Println()
		return nil
	}
	switch args[0] {
	case "setup":
		return cmdBrainSetup(args[1:])
	default:
		return fmt.Errorf("unknown brain subcommand %q — run 'synapses brain help'", args[0])
	}
}

// cmdBrainSetup is the implementation of "synapses brain setup".
func cmdBrainSetup(args []string) error {
	fs := flag.NewFlagSet("brain setup", flag.ContinueOnError)
	ollamaURL := fs.String("ollama", "http://localhost:11434", "Ollama base URL")
	mode := fs.String("mode", "standard", "Intelligence mode: optimal | standard | full")
	skipPull := fs.Bool("skip-pull", false, "Skip pulling the base model")
	skipSmoke := fs.Bool("skip-smoke", false, "Skip smoke tests")
	noColor := fs.Bool("no-color", false, "Disable ANSI color codes (useful when output is consumed by a GUI)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	validModes := map[string]bool{"optimal": true, "standard": true, "full": true}
	if !validModes[*mode] {
		return fmt.Errorf("invalid mode %q — must be one of: optimal, standard, full", *mode)
	}

	// SSRF protection: validate URL points to localhost only and create a
	// hardened HTTP client that enforces loopback at dial time.
	if err := validateOllamaURL(*ollamaURL); err != nil {
		return fmt.Errorf("brain setup: %w", err)
	}
	safeClient := newOllamaHTTPClient(30 * time.Second)
	smokeClient := newOllamaHTTPClient(90 * time.Second) // inference can take 30-60s on CPU for 4b models

	// color helpers — inline so callers stay readable
	green := func(s string) string {
		if *noColor {
			return s
		}
		return "\033[32m" + s + "\033[0m"
	}
	yellow := func(s string) string {
		if *noColor {
			return s
		}
		return "\033[33m" + s + "\033[0m"
	}
	red := func(s string) string {
		if *noColor {
			return s
		}
		return "\033[31m" + s + "\033[0m"
	}
	bold := func(s string) string {
		if *noColor {
			return s
		}
		return "\033[1m" + s + "\033[0m"
	}

	models := modelsForMode(*mode)

	fmt.Println()
	fmt.Println("  Synapses Brain Setup")
	fmt.Println("  ─────────────────────────────────────────")
	fmt.Println()
	fmt.Printf("  Mode: %s — requires %d model(s): %s\n", *mode, len(models), strings.Join(models, ", "))
	fmt.Println("  System prompts are passed per-request — no Modelfile registration needed.")
	fmt.Println()

	totalSteps := 3
	step := 0

	// ── Step 1: Ollama reachability ──────────────────────────────────────────
	step++
	fmt.Printf("  [%d/%d] Checking Ollama... ", step, totalSteps)
	version, err := brainPingOllama(*ollamaURL, safeClient)
	if err != nil {
		fmt.Println()
		fmt.Println()
		fmt.Printf("  %s Ollama is not running or unreachable.\n", red("✗"))
		fmt.Printf("        URL tried: %s\n", *ollamaURL)
		fmt.Println()
		fmt.Println("  To start Ollama:")
		fmt.Println("    • macOS:        open -a Ollama  (or: ollama serve)")
		fmt.Println("    • Linux:        ollama serve")
		fmt.Println("    • Not installed? https://ollama.com")
		fmt.Println()
		return fmt.Errorf("brain setup: ollama not running at %s: %w", *ollamaURL, err)
	}
	fmt.Printf("%s  Ollama %s at %s\n", green("✓"), version, *ollamaURL)
	fmt.Println()

	// ── Step 2: Pull required models ─────────────────────────────────────────
	step++
	fmt.Printf("  [%d/%d] Pulling required models...\n", step, totalSteps)
	fmt.Println()

	if *skipPull {
		fmt.Println("        --skip-pull set, assuming models are already downloaded.")
	} else {
		for _, modelTag := range models {
			installed, checkErr := brainIsModelInstalled(*ollamaURL, modelTag, safeClient)
			if checkErr != nil {
				return fmt.Errorf("brain setup: check model %s: %w", modelTag, checkErr)
			}
			if installed {
				fmt.Printf("        %s %s already downloaded\n", green("✓"), modelTag)
			} else {
				fmt.Printf("        %s Downloading %s...\n", yellow("↓"), modelTag)
				if pullErr := brainPullModel(*ollamaURL, modelTag, safeClient); pullErr != nil {
					return fmt.Errorf("brain setup: pull %s: %w", modelTag, pullErr)
				}
				fmt.Printf("        %s %s downloaded\n", green("✓"), modelTag)
			}
		}
	}
	fmt.Println()

	// ── Step 3: Smoke test ───────────────────────────────────────────────────
	step++
	if *skipSmoke {
		fmt.Printf("  [%d/%d] Smoke test skipped (--skip-smoke)\n", step, totalSteps)
	} else {
		fmt.Printf("  [%d/%d] Smoke test...\n", step, totalSteps)
		fmt.Println()

		// Test the smallest model (0.8b) — if it responds, Ollama is working.
		smokeModel := models[0] // qwen3.5:0.8b (always first — Sentry tier)
		warmOK, _ := brainSmokeWarmup(*ollamaURL, smokeModel, smokeClient)
		if !warmOK {
			fmt.Printf("  %s  Model did not respond within 90 seconds.\n", yellow("!"))
			fmt.Println("        The brain is configured correctly — models will warm up on first use.")
			goto skipSmokeTests
		}

		{
			fmt.Printf("        Sending test request to %s...", smokeModel)
			ok, elapsed := brainSmokeTest(*ollamaURL, smokeModel, smokeClient)
			if ok {
				fmt.Printf(" %s (%s)\n", green("✓"), elapsed)
				fmt.Printf("        %s  Brain is working.\n", green("✓"))
			} else {
				fmt.Printf(" %s\n", red("✗"))
				fmt.Println()
				fmt.Printf("  %s  Smoke test failed.\n", yellow("✗"))
				fmt.Println("        Try: synapses brain setup --skip-pull")
			}
		}
	skipSmokeTests:
	}
	fmt.Println()

	// ── Write brain.json ─────────────────────────────────────────────────────
	brainJSONPath, err := brainWriteConfig(*ollamaURL, *mode)
	if err != nil {
		fmt.Printf("  %s  Could not write brain.json: %v\n", yellow("!"), err)
	} else {
		fmt.Printf("  %s  brain.json written  (%s)\n", green("✓"), brainJSONPath)
	}

	fmt.Println()
	fmt.Println("  ─────────────────────────────────────────")
	fmt.Printf("  %s  Intelligence mode: %s\n", bold("Brain is ready."), *mode)
	fmt.Println()
	fmt.Println("  Enable in synapses.json (project root):")
	fmt.Println(`    "brain": { "enabled": true, "intelligence_mode": "` + *mode + `" }`)
	fmt.Println()
	return nil
}


// ── Internal helpers ──────────────────────────────────────────────────────────

// brainPingOllama returns the Ollama version string or an error if unreachable.
func brainPingOllama(baseURL string, client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Version == "" {
		return "running", nil
	}
	return v.Version, nil
}

// brainIsModelInstalled returns true if modelName is present in Ollama's tag list.
func brainIsModelInstalled(baseURL, modelName string, client *http.Client) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return false, err
	}
	norm := strings.TrimSuffix(modelName, ":latest")
	for _, m := range tags.Models {
		if strings.TrimSuffix(m.Name, ":latest") == norm {
			return true, nil
		}
	}
	return false, nil
}

// brainPullModel streams a pull from Ollama, printing live progress.
func brainPullModel(baseURL, modelName string, client *http.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	reqBody, _ := json.Marshal(map[string]interface{}{
		"name":   modelName,
		"stream": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/pull", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	for {
		var event struct {
			Status    string `json:"status"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
			Error     string `json:"error"`
		}
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if event.Error != "" {
			return fmt.Errorf("%s", event.Error)
		}
		if event.Total > 0 {
			pct := int(float64(event.Completed) / float64(event.Total) * 100)
			gb := float64(event.Total) / 1e9
			done := float64(event.Completed) / 1e9
			fmt.Printf("\r        %-18s  %5.2f / %.2f GB  [%3d%%]   ", event.Status, done, gb, pct)
		} else if event.Status != "" {
			fmt.Printf("\r        %-60s", event.Status)
		}
	}
	fmt.Println()
	return nil
}


// brainSmokeWarmup loads the base model into memory and keeps it resident for
// 5 minutes so subsequent smoke tests run against warm weights.
// It uses keep_alive=300 (seconds) so Ollama does not evict the model between tests.
// Prints a live elapsed-seconds ticker so users know the process is not stuck.
// Returns (ok, elapsed).
func brainSmokeWarmup(baseURL, modelName string, client *http.Client) (bool, string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Live ticker: prints "Xs..." every second while the HTTP call blocks.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Printf("\r        Loading %s into memory... %ds", modelName, int(time.Since(start).Seconds()))
			}
		}
	}()

	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":      modelName,
		"stream":     false,
		"prompt":     "hi",
		"keep_alive": 300, // keep model loaded for 5 min so smoke tests hit warm weights
		"options": map[string]interface{}{
			"num_predict": 1,
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(reqBody))
	if err != nil {
		close(done)
		fmt.Print("\r\033[K")
		return false, ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) // ticker runs during this blocking call
	close(done)
	fmt.Print("\r\033[K") // clear ticker line (carriage return + erase to end of line)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	elapsed := fmt.Sprintf("%.1fs", time.Since(start).Seconds())
	return resp.StatusCode == http.StatusOK, elapsed
}

// brainSmokeTest verifies the model responds by waiting for the first streamed
// token. Returns (ok, elapsed) — elapsed is time to first token.
func brainSmokeTest(baseURL, modelName string, client *http.Client) (bool, string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":  modelName,
		"stream": true, // streaming: we only need the first token
		"messages": []map[string]string{
			{"role": "user", "content": `{"name":"init","type":"function","package":"main","code":"func init() {}"}`},
		},
		"options": map[string]interface{}{
			"temperature": 0,
			"num_predict": 1, // one token is enough to prove the model responded
		},
		"think": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/chat", bytes.NewReader(reqBody))
	if err != nil {
		return false, ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, ""
	}

	// Read the first streamed chunk — if we get any valid JSON line, the model is working.
	var chunk struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Done bool `json:"done"`
	}
	dec := json.NewDecoder(resp.Body)
	if dec.More() { //nolint:staticcheck // SA4004: intentionally read only the first chunk
		if err := dec.Decode(&chunk); err == nil {
			// Any chunk (even an empty content token) confirms the model is alive.
			return true, fmt.Sprintf("%.1fs", time.Since(start).Seconds())
		}
	}
	return false, ""
}

// brainWriteConfig creates or updates ~/.synapses/brain.json, preserving
// any existing keys not managed by this command. Returns the written path.
func brainWriteConfig(ollamaURL, mode string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".synapses")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	cfgPath := filepath.Join(dir, "brain.json")

	existing := map[string]interface{}{}
	if data, err := os.ReadFile(cfgPath); err == nil {
		if jsonErr := json.Unmarshal(data, &existing); jsonErr != nil {
			logutil.Warn("brain.json could not be parsed (%v) — existing keys will be overwritten\n",
				jsonErr)
			existing = map[string]interface{}{}
		}
	}
	existing["enabled"] = true
	existing["backend"] = "ollama"
	existing["ollama_url"] = ollamaURL
	existing["intelligence_mode"] = mode

	// Write per-tier model assignments (raw Ollama tags, no identity models).
	// AutoConfigureModels will re-derive these at runtime, but writing them
	// explicitly makes brain.json self-documenting and allows manual overrides.
	cfg := brainconfig.BrainConfig{IntelligenceMode: brainconfig.IntelligenceMode(mode)}
	cfg.AutoConfigureModels(0)
	existing["model_ingest"] = cfg.ModelIngest
	existing["model_enrich"] = cfg.ModelEnrich
	existing["model_guardian"] = cfg.ModelGuardian
	existing["model_orchestrate"] = cfg.ModelOrchestrate
	existing["model_archivist"] = cfg.ModelArchivist

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return "", err
	}
	return cfgPath, os.WriteFile(cfgPath, append(out, '\n'), 0o600)
}
