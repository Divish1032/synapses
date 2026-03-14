// onboard.go — "synapses onboard" interactive setup wizard
// Guides the user through installing and configuring all four SynapsesOS legs.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func cmdOnboard(args []string) error {
	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════╗")
	fmt.Println("  ║       Welcome to Synapses OS Setup           ║")
	fmt.Println("  ╚══════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("  This wizard will help you install and configure")
	fmt.Println("  Synapses OS and wire it into your AI coding agent.")
	fmt.Println()

	r := bufio.NewReader(os.Stdin)

	// ── Step 1: detect what's installed ────────────────────────────────────
	fmt.Println("  Step 1 of 5 — Checking installed components")
	fmt.Println("  ──────────────────────────────────────────────")

	hasOllama := binaryExists("ollama")

	printInstalled("synapses (core + brain + pulse + web-cache)", true)
	printInstalled("ollama (local LLM runtime — optional)", hasOllama)
	fmt.Println()
	fmt.Println("  Note: brain, pulse, and web intelligence are now built into")
	fmt.Println("  synapses — no separate sidecars needed.")
	fmt.Println()

	// ── Step 2: Ollama setup (optional — enables AI brain enrichment) ─────
	fmt.Println("  Step 2 of 5 — Configure AI brain (optional)")
	fmt.Println("  ──────────────────────────────────────────────")
	if !hasOllama {
		fmt.Println("  \033[33m!\033[0m Ollama is not installed.")
		fmt.Println("    Visit https://ollama.com to install it.")
		fmt.Println("    Once installed, set brain.enabled:true in synapses.json to activate.")
		fmt.Println()
	} else {
		fmt.Println("  \033[32m✓\033[0m Ollama detected. To enable AI enrichment:")
		fmt.Println("    Set \"brain\": {\"enabled\": true} in your synapses.json")
		fmt.Println()
	}

	// ── Step 4: write synapses.json ────────────────────────────────────────
	fmt.Println("  Step 3 of 5 — Write project configuration")
	fmt.Println("  ──────────────────────────────────────────────")

	repoPath := "."
	if len(args) > 0 {
		repoPath = args[0]
	}
	absPath, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	if err := writeOnboardSynapsesJSON(absPath); err != nil {
		fmt.Printf("  \033[31m✗\033[0m Could not write synapses.json: %v\n", err)
	} else {
		fmt.Printf("  \033[32m✓\033[0m synapses.json written (%s)\n", filepath.Join(absPath, "synapses.json"))
	}
	fmt.Println()

	// ── Step 4: start services ────────────────────────────────────────────
	fmt.Println("  Step 4 of 5 — Start services")
	fmt.Println("  ──────────────────────────────────────────────")

	if err := ensureDirs(); err != nil {
		return err
	}

	installOnLogin := prompt(r, "  Auto-start daemon at login? [Y/n]: ")
	fmt.Println()
	if installOnLogin {
		daemonInstall() //nolint:errcheck
	}

	// ── Step 5: wire into AI agent ────────────────────────────────────────
	fmt.Println("  Step 5 of 5 — Wire into AI agent")
	fmt.Println("  ──────────────────────────────────────────────")
	fmt.Println()

	if prompt(r, "  Wire Synapses into Claude Code? [Y/n]: ") {
		fmt.Println()
		if err := runCmd("synapses", "mcp-setup", "--agent", "claude"); err != nil {
			fmt.Printf("  \033[33m!\033[0m mcp-setup failed: %v\n", err)
			fmt.Printf("    Run manually: claude mcp add synapses -- synapses start --path %s\n", absPath)
		}
	}

	otherAgents := []struct{ Name, Flag string }{
		{"Cursor", "cursor"},
		{"Windsurf", "windsurf"},
		{"Zed", "zed"},
		{"Gemini CLI", "gemini"},
	}
	for _, a := range otherAgents {
		if prompt(r, fmt.Sprintf("  Wire into %s? [y/N]: ", a.Name)) {
			runCmd("synapses", "mcp-setup", "--agent", a.Flag) //nolint:errcheck
		}
	}
	fmt.Println()

	// ── Done ─────────────────────────────────────────────────────────────
	fmt.Println("  ╔══════════════════════════════════════════════╗")
	fmt.Println("  ║           Synapses OS is ready!              ║")
	fmt.Println("  ╚══════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("  Useful commands:")
	fmt.Println("    synapses daemon status       — check what's running")
	fmt.Println("    synapses daemon start        — start all sidecars")
	fmt.Println("    synapses daemon stop         — stop all sidecars")
	fmt.Println("    synapses daemon logs --service brain")
	fmt.Println("    synapses doctor              — full health diagnostics")
	fmt.Println("    synapses index --path .      — (re)index this project")
	fmt.Println()
	fmt.Println("  VS Code: install the Synapses extension for a full control panel.")
	fmt.Println()

	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func binaryExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func printInstalled(name string, found bool) {
	if found {
		fmt.Printf("  \033[32m✓\033[0m %s\n", name)
	} else {
		fmt.Printf("  \033[31m✗\033[0m %s  (not installed)\n", name)
	}
}

// prompt shows a question and returns true if the user answered Y/y or just pressed Enter.
// If the question ends with [y/N] the default is No.
func prompt(r *bufio.Reader, question string) bool {
	fmt.Print(question)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if strings.Contains(question, "[y/N]") {
		return line == "y" || line == "yes"
	}
	return line == "" || line == "y" || line == "yes"
}

func runCmd(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func activeSidecars() []Sidecar {
	return allSidecars
}

func writeOnboardSynapsesJSON(root string) error {
	// Create the directory if it doesn't exist (handles `synapses init --path /new/dir`).
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	cfgPath := filepath.Join(root, "synapses.json")

	// Load existing config if present.
	existing := map[string]interface{}{}
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(data, &existing)
	}

	// Brain is now in-process — add a disabled-by-default entry as a hint.
	if _, hasBrain := existing["brain"]; !hasBrain {
		existing["brain"] = map[string]interface{}{
			"enabled": false,
			// Set enabled:true and ollama_url to activate AI enrichment.
		}
	}
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, append(data, '\n'), 0o644)
}
