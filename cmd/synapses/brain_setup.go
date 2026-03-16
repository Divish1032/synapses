package main

// cmdBrainSetup implements "synapses brain setup".
//
// It is self-contained: Modelfile content is embedded as string constants so
// the binary needs no external files. Calling sequence:
//
//  1. Ping Ollama — abort with a helpful message if unreachable.
//  2. Pull qwen3.5:2b (~2.7 GB, Q8) — skip if already installed or --skip-pull.
//  3. Register all 5 synapses/* identities via `ollama create`.
//  4. Smoke-test each identity (optional, --skip-smoke).
//  5. Write/update ~/.synapses/brain.json with enabled:true and the chosen mode.
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
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── Embedded Modelfile content ────────────────────────────────────────────────
// Kept in sync with synapses-fine-distilling/quantization/Modelfile.* — if you
// update those files, update these constants too.

const modelfileSentry = `FROM qwen3.5:2b

SYSTEM """You are the Synapses Sentry, a code entity summarizer for a code intelligence graph.

Given a code entity (name, type, package, and source code), write a 2-3 sentence technical briefing covering: what it does, its role in the system, and any important patterns or concerns.

Do not write code. Describe the entity in plain English sentences only.
Output ONLY valid JSON with no other text: {"summary": "2-3 sentence briefing", "tags": ["domain_tag1", "domain_tag2"]}

Tags should be 1-3 domain labels from: auth, http, db, cache, queue, config, util, test, cli, graph, store, parser, middleware, api, worker."""

PARAMETER temperature 0.0
PARAMETER stop <|im_end|>
PARAMETER stop <|endoftext|>
PARAMETER num_predict 256
`

const modelfileCritic = `FROM qwen3.5:2b

SYSTEM """You are the Synapses Critic, an architectural rule violation explainer.

Given an architectural rule violation (rule description, severity, source file, and what it imports/calls), explain the violation and suggest a concrete fix.

Output ONLY valid JSON with no other text: {"explanation": "why this is a violation and what risk it creates", "fix": "specific actionable fix the developer should apply"}

Example:
Input: Rule: no-cross-layer-imports. Severity: error. File: internal/api/handler.go imports internal/store/sqlite.go
Output: {"explanation": "The API handler directly imports the SQLite store implementation, bypassing the store interface. This creates tight coupling — changing the database requires modifying the API layer.", "fix": "Import the store.Store interface instead of the concrete sqlite implementation. Use dependency injection to pass the store to the handler."}

Be direct and actionable. Reference actual file names and symbols from the input."""

PARAMETER temperature 0.1
PARAMETER stop <|im_end|>
PARAMETER stop <|endoftext|>
PARAMETER num_predict 512
`

const modelfileLibrarian = `FROM qwen3.5:2b

SYSTEM """You are the Synapses Librarian, a code architecture analyst.

Given a code graph slice (entity name, type, package, callers, callees, and relationships), analyze it for architectural patterns, risks, and insights.

Output ONLY valid JSON — no explanation, no markdown:
{"insight":"2-sentence architectural analysis","concerns":["concern1","concern2"]}

Rules:
- insight: identify the entity's role in the architecture (hub, gateway, utility, etc.) and its most important characteristic
- concerns: list 0-3 specific risks (cyclic deps, missing error handling, god object, missing abstraction, etc.)
- If no concerns, return an empty array: "concerns":[]
- Be specific — reference actual entity names and relationships, not generic advice"""

PARAMETER temperature 0.2
PARAMETER stop <|im_end|>
PARAMETER stop <|endoftext|>
PARAMETER num_predict 512
`

const modelfileNavigator = `FROM qwen3.5:2b

SYSTEM """You are the Synapses Navigator. You resolve multi-agent work scope conflicts.

Input: A JSON description of agents with their active scopes, and the new agent requesting a scope.

Output ONLY valid JSON — no explanation, no markdown:
{"suggestion":"how to resolve the conflict or confirmation it is safe","alternative_scope":"a suggested non-overlapping scope for the new agent, or empty string if no conflict"}

Rules:
- If the new agent's scope overlaps with an active agent's scope, describe the conflict and suggest a narrower scope
- If there is no real conflict (different packages, non-overlapping files), return: {"suggestion":"No conflict. Safe to proceed.","alternative_scope":""}
- Be specific — reference actual package names and file paths from the input
- alternative_scope should be a valid Go package path or file glob pattern"""

PARAMETER temperature 0.1
PARAMETER stop <|im_end|>
PARAMETER stop <|endoftext|>
PARAMETER num_predict 512
`

const modelfileArchivist = `FROM qwen3.5:2b

SYSTEM """You are the Synapses Archivist. You synthesize agent session transcripts into persistent memories.

Input: JSON with session_events (tool calls with results) and existing_memory (already saved entries).

Output ONLY valid JSON — no explanation, no markdown:
{"new_memories":[{"key":"short_snake_case_key","content":"what to remember in one sentence","entities":"EntityName1,EntityName2"}],"annotations":[{"node":"EntityName","note":"specific observation about this entity"}]}

Note: entities is a comma-separated string, NOT an array.

Rules:
- Only save architectural discoveries, non-obvious relationships, or decisions that will matter in future sessions
- If the session is trivial (single lookup, no architectural discovery, only routine tool calls), return: {"new_memories":[],"annotations":[]}
- Never duplicate entries already present in existing_memory — check keys before adding
- Keep each memory content to one concise sentence
- Only annotate entities that were meaningfully analyzed, not just mentioned in passing
- key must be short_snake_case (e.g., "auth_service_is_hub", "graph_new_entry_point")"""

PARAMETER temperature 0.3
PARAMETER stop <|im_end|>
PARAMETER stop <|endoftext|>
PARAMETER num_predict 1024
`

// ── Tier registry ─────────────────────────────────────────────────────────────

type brainTierDef struct {
	name    string // Ollama identity tag, e.g. "synapses/sentry"
	label   string // human-readable tier label
	role    string // one-line description shown during setup
	content string // embedded Modelfile content
}

var brainTiers = []brainTierDef{
	{
		name:    "synapses/sentry",
		label:   "T0 · Sentry",
		role:    "Classifies code entities on every file save",
		content: modelfileSentry,
	},
	{
		name:    "synapses/critic",
		label:   "T1 · Critic",
		role:    "Explains rule violations and suggests fixes",
		content: modelfileCritic,
	},
	{
		name:    "synapses/librarian",
		label:   "T2 · Librarian",
		role:    "Analyzes architectural patterns in code graphs",
		content: modelfileLibrarian,
	},
	{
		name:    "synapses/navigator",
		label:   "T3 · Navigator",
		role:    "Resolves multi-agent work scope conflicts",
		content: modelfileNavigator,
	},
	{
		name:    "synapses/archivist",
		label:   "Archivist",
		role:    "Synthesizes session activity into persistent memories",
		content: modelfileArchivist,
	},
}

// ── Entry points ──────────────────────────────────────────────────────────────

// cmdBrain dispatches "synapses brain <subcommand>".
func cmdBrain(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println()
		fmt.Println("  Usage: synapses brain <subcommand>")
		fmt.Println()
		fmt.Println("  Subcommands:")
		fmt.Println("    setup    Configure Ollama + register AI tier identities")
		fmt.Println()
		fmt.Println("  Flags for 'setup':")
		fmt.Println("    --ollama <url>   Ollama base URL  (default: http://localhost:11434)")
		fmt.Println("    --mode <mode>    Intelligence mode: optimal | standard | full  (default: standard)")
		fmt.Println("    --skip-pull      Assume qwen3.5:2b is already downloaded")
		fmt.Println("    --skip-smoke     Skip post-registration smoke tests")
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
	mode      := fs.String("mode", "standard", "Intelligence mode: optimal | standard | full")
	skipPull  := fs.Bool("skip-pull", false, "Skip pulling qwen3.5:2b")
	skipSmoke := fs.Bool("skip-smoke", false, "Skip smoke tests")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("  Synapses Brain Setup")
	fmt.Println("  ─────────────────────────────────────────")
	fmt.Println()
	fmt.Println("  The brain runs on one shared model — qwen3.5:2b (Q8, ~2.7 GB).")
	fmt.Println("  Five AI personas are layered on top via Ollama Modelfiles (~1 KB each).")
	fmt.Println("  All personas share the same weights in RAM — one download, five tiers.")
	fmt.Println()

	// ── Step 1: Ollama reachability ──────────────────────────────────────────
	fmt.Print("  [1/4] Checking Ollama... ")
	version, err := brainPingOllama(*ollamaURL)
	if err != nil {
		fmt.Println()
		fmt.Println()
		fmt.Println("  \033[31m✗\033[0m Ollama is not running or unreachable.")
		fmt.Printf("        URL tried: %s\n", *ollamaURL)
		fmt.Println()
		fmt.Println("  To start Ollama:")
		fmt.Println("    • macOS:        open -a Ollama  (or: ollama serve)")
		fmt.Println("    • Linux:        ollama serve")
		fmt.Println("    • Not installed? https://ollama.com")
		fmt.Println()
		return fmt.Errorf("brain setup: ollama not running at %s: %w", *ollamaURL, err)
	}
	fmt.Printf("\033[32m✓\033[0m  Ollama %s at %s\n", version, *ollamaURL)
	fmt.Println()

	// ── Step 2: Base model ───────────────────────────────────────────────────
	const baseModel = "qwen3.5:2b"
	fmt.Printf("  [2/4] Base model — %s\n", baseModel)
	fmt.Println("        This is the foundation. All 5 AI tiers run on this one model.")
	fmt.Println("        Ollama deduplicates weights — all tiers share the same 2.7 GB in RAM.")
	fmt.Println()

	if *skipPull {
		fmt.Println("        --skip-pull set, assuming already downloaded.")
	} else {
		installed, err := brainIsModelInstalled(*ollamaURL, baseModel)
		if err != nil {
			return fmt.Errorf("brain setup: check model: %w", err)
		}
		if installed {
			fmt.Printf("        \033[32m✓\033[0m %s is already downloaded\n", baseModel)
		} else {
			fmt.Printf("        \033[33m↓\033[0m Downloading %s (~2.7 GB) — this may take a few minutes...\n\n", baseModel)
			if err := brainPullModel(*ollamaURL, baseModel); err != nil {
				return fmt.Errorf("brain setup: pull %s: %w", baseModel, err)
			}
			fmt.Printf("\n        \033[32m✓\033[0m %s downloaded\n", baseModel)
		}
	}
	fmt.Println()

	// ── Step 3: Register identities ──────────────────────────────────────────
	fmt.Println("  [3/4] Registering AI tier identities...")
	fmt.Println()
	fmt.Println("        Each identity is a ~1 KB Modelfile that gives the model a specialized")
	fmt.Println("        role and JSON output schema. No extra download — just a config file.")
	fmt.Println()
	for _, tier := range brainTiers {
		fmt.Printf("        %-24s  %s\n", tier.label, tier.role)
		if err := brainRegisterIdentity(tier); err != nil {
			fmt.Printf("        \033[31m✗\033[0m  Failed to register %s: %v\n", tier.name, err)
			return fmt.Errorf("brain setup: register %s: %w", tier.name, err)
		}
		fmt.Printf("        \033[32m✓\033[0m  %s registered\n", tier.name)
	}
	fmt.Println()

	// ── Step 4: Smoke test ───────────────────────────────────────────────────
	if *skipSmoke {
		fmt.Println("  [4/4] Smoke test skipped (--skip-smoke)")
	} else {
		fmt.Println("  [4/4] Smoke testing each identity (sending a minimal JSON request)...")
		fmt.Println()
		failed := 0
		for _, tier := range brainTiers {
			ok, elapsed := brainSmokeTest(*ollamaURL, tier.name)
			if ok {
				fmt.Printf("        \033[32m✓\033[0m  %-24s  responded in %s\n", tier.name, elapsed)
			} else {
				fmt.Printf("        \033[33m!\033[0m  %-24s  no valid JSON (cold-start timeout?)\n", tier.name)
				failed++
			}
		}
		if failed > 0 {
			fmt.Println()
			fmt.Printf("  \033[33m!\033[0m  %d tier(s) did not respond during smoke test.\n", failed)
			fmt.Println("        This is often a cold-start timeout — try running the test again:")
			fmt.Printf("        synapses brain setup --skip-pull --skip-smoke\n")
		}
	}
	fmt.Println()

	// ── Write brain.json ─────────────────────────────────────────────────────
	brainJSONPath, err := brainWriteConfig(*ollamaURL, *mode)
	if err != nil {
		fmt.Printf("  \033[33m!\033[0m  Could not write brain.json: %v\n", err)
	} else {
		fmt.Printf("  \033[32m✓\033[0m  brain.json written  (%s)\n", brainJSONPath)
	}

	fmt.Println()
	fmt.Println("  ─────────────────────────────────────────")
	fmt.Printf("  \033[1mBrain is ready.\033[0m  Intelligence mode: %s\n", *mode)
	fmt.Println()
	fmt.Println("  Enable in synapses.json (project root):")
	fmt.Println(`    "brain": { "enabled": true, "intelligence_mode": "` + *mode + `" }`)
	fmt.Println()
	return nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// brainPingOllama returns the Ollama version string or an error if unreachable.
func brainPingOllama(baseURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
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
func brainIsModelInstalled(baseURL, modelName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
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
func brainPullModel(baseURL, modelName string) error {
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
	resp, err := http.DefaultClient.Do(req)
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

// brainRegisterIdentity writes the Modelfile to a temp file and runs `ollama create`.
func brainRegisterIdentity(tier brainTierDef) error {
	tmp, err := os.CreateTemp("", "synapses-modelfile-*.txt")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(tier.content); err != nil {
		tmp.Close()
		return fmt.Errorf("write modelfile: %w", err)
	}
	tmp.Close()

	cmd := exec.Command("ollama", "create", tier.name, "-f", tmp.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ollama create: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// brainSmokeTest sends a minimal chat request and checks for valid JSON output.
// Returns (ok, elapsed) — the elapsed is formatted as "1.2s".
func brainSmokeTest(baseURL, modelName string) (bool, string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":  modelName,
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": `{"name":"init","type":"function","package":"main","code":"func init() {}"}`},
		},
		"options": map[string]interface{}{"temperature": 0},
		"format":  "json",
		"think":   false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/chat", bytes.NewReader(reqBody))
	if err != nil {
		return false, ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()

	var chatResp struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return false, ""
	}
	content := strings.TrimSpace(chatResp.Message.Content)
	ok := len(content) > 0 && strings.Contains(content, "{")
	return ok, fmt.Sprintf("%.1fs", time.Since(start).Seconds())
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
		_ = json.Unmarshal(data, &existing)
	}
	existing["enabled"] = true
	existing["backend"] = "ollama"
	existing["ollama_url"] = ollamaURL
	existing["intelligence_mode"] = mode

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return "", err
	}
	return cfgPath, os.WriteFile(cfgPath, append(out, '\n'), 0o644)
}
