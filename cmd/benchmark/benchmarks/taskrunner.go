// taskrunner.go provides shared infrastructure for benchmark runners that shell
// out to `claude -p`. Includes dataset loading, git helpers, Synapses Docker
// setup, Claude Code integration, and stream output parsing.
//
// Used by: CompactionBench.
package benchmarks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ─── Task Data Types ────────────────────────────────────────────────────────

// TaskOptions configures dataset loading for task-based benchmarks.
type TaskOptions struct {
	Split    string   // HF split: "lite", "fast", "full"
	TaskIDs  []string // optional: specific task IDs
	Level    int      // 0 = all, 1 or 2
	ReposDir string   // where repos are cloned
}

// BenchTask is one task from the task-based benchmark dataset.
type BenchTask struct {
	InstanceID       string          `json:"instance_id"`
	Repo             string          `json:"repo"`
	BaseCommit       string          `json:"base_commit"`
	ProblemStatement string          `json:"problem_statement"`
	ImageName        string          `json:"image_name"`
	RepoSettings     json.RawMessage `json:"repo_settings"`
	Patch            string          `json:"patch"`
	TestPatch        string          `json:"test_patch"`
	FailToPass       []string        `json:"FAIL_TO_PASS"`
	PassToPass       []string        `json:"PASS_TO_PASS"`
}

// ─── Dataset Loading ────────────────────────────────────────────────────────

func loadBenchTasks(opts TaskOptions) ([]BenchTask, error) {
	// Find the Python script relative to the benchmark binary or CWD.
	scriptPath := findScript("load_bench_tasks.py")

	args := []string{scriptPath, "--split", opts.Split}
	if len(opts.TaskIDs) > 0 {
		args = append(args, "--task-ids", strings.Join(opts.TaskIDs, ","))
	}
	if opts.Level > 0 {
		args = append(args, "--level", fmt.Sprintf("%d", opts.Level))
	}

	cmd := exec.Command("python3", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("python3 %s: %w", scriptPath, err)
	}

	var tasks []BenchTask
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0), 10*1024*1024) // 10MB line buffer
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var task BenchTask
		if err := json.Unmarshal([]byte(line), &task); err != nil {
			log.Printf("WARNING: skip malformed line: %v", err)
			continue
		}
		tasks = append(tasks, task)
	}

	return tasks, scanner.Err()
}

func findScript(name string) string {
	// Try relative to executable, then CWD.
	exe, _ := os.Executable()
	if exe != "" {
		p := filepath.Join(filepath.Dir(exe), "scripts", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Try CWD/cmd/benchmark/scripts/
	candidates := []string{
		filepath.Join("cmd", "benchmark", "scripts", name),
		filepath.Join("scripts", name),
		name,
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return name // fallback, will fail with clear error
}

// ─── Claude Code System Prompt ──────────────────────────────────────────────

// synapsesSystemPrompt is injected via --append-system-prompt in Synapses mode.
const synapsesSystemPrompt = `Synapses MCP tools are available with indexed codebase. Use mcp__synapses__search for symbol lookup, mcp__synapses__get_context for callers/callees/dependencies, mcp__synapses__get_impact before editing to check blast radius.`

// ─── Claude Code Integration ────────────────────────────────────────────────

func findClaudeBin() (string, error) {
	// Check PATH first.
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}

	// macOS: Claude Desktop bundles claude-code.
	if runtime.GOOS == "darwin" {
		home, _ := os.UserHomeDir()
		pattern := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code", "*", "claude.app", "Contents", "MacOS", "claude")
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			// Use the latest version (last match after sort).
			return matches[len(matches)-1], nil
		}
	}

	return "", fmt.Errorf("claude binary not found; install Claude Code or add to PATH")
}

func findSynapsesBin() string {
	if p, err := exec.LookPath("synapses"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".synapses", "bin", "synapses")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return "synapses" // fallback
}

func runClaudeCode(claudeBin, repoDir, prompt, allowedTools, disallowedTools, model, systemPrompt string, timeoutSecs int) (string, error) {
	args := []string{
		"-p", prompt,
		"--allowedTools", allowedTools,
		"--output-format", "stream-json",
		"--verbose",
	}
	if disallowedTools != "" {
		args = append(args, "--disallowedTools", disallowedTools)
	}
	if systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}

	cmd := exec.Command(claudeBin, args...)
	cmd.Dir = repoDir

	// Set model via env if specified.
	cmd.Env = append(os.Environ(),
		"DISABLE_TELEMETRY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	)
	if model != "" {
		cmd.Env = append(cmd.Env,
			fmt.Sprintf("ANTHROPIC_MODEL=%s", model),
			fmt.Sprintf("ANTHROPIC_DEFAULT_SONNET_MODEL=%s", model),
		)
	}

	// Capture stdout (stream-json), stderr goes to os.Stderr for debugging.
	cmd.Stderr = os.Stderr

	// Set timeout via context.
	done := make(chan error, 1)
	var out []byte

	go func() {
		var err error
		out, err = cmd.Output()
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			// Still return output — partial results are useful.
			return string(out), fmt.Errorf("claude exited: %w", err)
		}
		return string(out), nil
	case <-time.After(time.Duration(timeoutSecs) * time.Second):
		_ = cmd.Process.Kill()
		return string(out), fmt.Errorf("timeout after %ds", timeoutSecs)
	}
}

type streamStats struct {
	ToolCalls               map[string]int
	Turns                   int
	InputTokens             int // uncached input tokens
	OutputTokens            int
	CacheCreationTokens     int     // tokens written to cache
	CacheReadTokens         int     // tokens read from cache
	TotalCostUSD            float64 // from Claude Code's own tracking
}

func parseStreamStats(streamJSON string) streamStats {
	stats := streamStats{ToolCalls: make(map[string]int)}
	scanner := bufio.NewScanner(strings.NewReader(streamJSON))
	scanner.Buffer(make([]byte, 0), 2*1024*1024) // 2MB per line

	for scanner.Scan() {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
				Usage struct {
					InputTokens             int `json:"input_tokens"`
					OutputTokens            int `json:"output_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens    int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			// Final result message has cumulative usage + cost.
			TotalCostUSD float64 `json:"total_cost_usd"`
			Usage        struct {
				InputTokens             int `json:"input_tokens"`
				OutputTokens            int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens    int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if msg.Type == "assistant" {
			stats.Turns++
			for _, c := range msg.Message.Content {
				if c.Type == "tool_use" && c.Name != "" {
					stats.ToolCalls[c.Name]++
				}
			}
		}
		// The final "result" message has cumulative totals — use those
		// instead of summing per-turn to avoid double-counting.
		if msg.Type == "result" {
			stats.InputTokens = msg.Usage.InputTokens
			stats.OutputTokens = msg.Usage.OutputTokens
			stats.CacheCreationTokens = msg.Usage.CacheCreationInputTokens
			stats.CacheReadTokens = msg.Usage.CacheReadInputTokens
			stats.TotalCostUSD = msg.TotalCostUSD
		}
	}
	return stats
}

// ─── Synapses Setup (Docker-isolated) ───────────────────────────────────────

const (
	daemonContainerName = "synapses-bench-daemon"
	daemonImage         = "synapses-daemon:bench"
	daemonHostPort      = "11436" // avoid conflict with user's local daemon on 11435
)

// ensureDaemonContainer starts the Docker container if not already running.
// It mounts the repos directory so the daemon can index them.
func ensureDaemonContainer(reposDir string) error {
	// Always recreate to ensure latest image is used.
	rm := exec.Command("docker", "rm", "-f", daemonContainerName)
	_ = rm.Run()

	// Start new container.
	// Mount repos at same path so project paths match between host and container.
	absRepos, _ := filepath.Abs(reposDir)
	cmd := exec.Command("docker", "run", "-d",
		"--name", daemonContainerName,
		"-p", daemonHostPort+":11435",
		"-v", absRepos+":"+absRepos, // same path inside container
		daemonImage,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run: %w\n%s", err, string(out))
	}
	log.Printf("  Synapses daemon container started (port %s)", daemonHostPort)

	// Wait for daemon to be ready.
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		resp, err := http.Get("http://127.0.0.1:" + daemonHostPort + "/api/admin/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				log.Printf("  Synapses daemon ready")
				return nil
			}
		}
	}
	return fmt.Errorf("daemon did not become ready in 30s")
}

func setupSynapses(synapsesBin, repoDir string) error {
	// 1. Ensure Docker daemon container is running.
	reposDir := filepath.Dir(repoDir)
	if err := ensureDaemonContainer(reposDir); err != nil {
		return fmt.Errorf("start daemon container: %w", err)
	}

	// 2. Write .mcp.json pointing to Docker daemon on port 11436.
	mcpJSON := fmt.Sprintf(`{
  "mcpServers": {
    "synapses": {
      "type": "http",
      "url": "http://127.0.0.1:%s/mcp?project=%s"
    }
  }
}`, daemonHostPort, url.QueryEscape(repoDir))

	if err := os.WriteFile(filepath.Join(repoDir, ".mcp.json"), []byte(mcpJSON), 0o644); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}

	// 3. Write synapses.json (minimal project config for daemon to recognize).
	synapsesJSON := `{"version": "1.0", "project": {"name": "bench-target"}}`
	if err := os.WriteFile(filepath.Join(repoDir, "synapses.json"), []byte(synapsesJSON), 0o644); err != nil {
		return fmt.Errorf("write synapses.json: %w", err)
	}

	// 4. Write .claude/settings.json to auto-approve Synapses tools.
	claudeDir := filepath.Join(repoDir, ".claude")
	os.MkdirAll(claudeDir, 0o755)

	settingsJSON := `{
  "permissions": {
    "allow": [
      "mcp__synapses__*",
      "Bash(*)",
      "Read(*)",
      "Write(*)",
      "Edit(*)",
      "Grep(*)",
      "Glob(*)"
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}

	// 5. Write CLAUDE.md guiding the agent to use Synapses tools.
	claudeMD := `# MANDATORY: Synapses Code Intelligence

You have Synapses MCP tools that give you structural code understanding (call graphs, dependency trees, symbol search). These are MORE ACCURATE than grep for navigating unfamiliar codebases.

## Required Workflow

1. **ALWAYS start with** ` + "`mcp__synapses__search`" + ` to find relevant functions, types, and symbols
2. **THEN use** ` + "`mcp__synapses__get_context`" + ` on key files to understand callers/callees and dependencies
3. **Use** ` + "`mcp__synapses__get_impact`" + ` before making changes to understand what will be affected
4. **ONLY fall back to** Grep/Bash for things Synapses cannot do (running tests, checking runtime output)

## Available Synapses Tools

- **mcp__synapses__search** — Find functions, types, variables by name or description. Returns file paths, line numbers, signatures.
- **mcp__synapses__get_context** — Get detailed context for a file or function: its callers, callees, imports, exports, and dependency graph.
- **mcp__synapses__get_impact** — Before changing a function, see everything that depends on it.

## Why This Matters

Synapses has already indexed this entire codebase into a dependency graph. Using ` + "`search`" + ` + ` + "`get_context`" + ` is 10x faster than grepping through files manually, and gives you structural relationships that text search cannot provide.
`
	if err := os.WriteFile(filepath.Join(claudeDir, "CLAUDE.md"), []byte(claudeMD), 0o644); err != nil {
		return fmt.Errorf("write CLAUDE.md: %w", err)
	}

	// 6. Pre-warm: trigger indexing by making a search request.
	warmURL := fmt.Sprintf("http://127.0.0.1:%s/v1/tools/search?project=%s",
		daemonHostPort, url.QueryEscape(repoDir))
	body := strings.NewReader(`{"query": "main"}`)
	resp, err := http.Post(warmURL, "application/json", body)
	if err != nil {
		log.Printf("  WARNING: pre-warm request failed: %v", err)
	} else {
		resp.Body.Close()
		log.Printf("  Pre-warm request sent (status %d), waiting for index...", resp.StatusCode)
	}

	// 7. Wait for indexing to complete by polling until search returns results.
	for i := 0; i < 120; i++ { // up to 2 minutes
		time.Sleep(time.Second)
		body := strings.NewReader(`{"query": "test"}`)
		resp, err := http.Post(warmURL, "application/json", body)
		if err != nil {
			continue
		}
		respBody := make([]byte, 4096)
		n, _ := resp.Body.Read(respBody)
		resp.Body.Close()
		if resp.StatusCode == 200 && n > 50 { // non-empty response means indexed
			log.Printf("  Synapses index ready")
			return nil
		}
	}

	log.Printf("  WARNING: index did not complete in 2min (continuing anyway)")
	return nil
}

// ─── Git Helpers ────────────────────────────────────────────────────────────

func resetToCommit(repoDir, commit string) {
	// Hard reset to the specified commit, discarding corruption commit + agent changes.
	cmd := exec.Command("git", "reset", "--hard", commit)
	cmd.Dir = repoDir
	_ = cmd.Run()

	cmd2 := exec.Command("git", "clean", "-fd")
	cmd2.Dir = repoDir
	_ = cmd2.Run()

	// Remove benchmark artifacts.
	os.Remove(filepath.Join(repoDir, ".mcp.json"))
	os.Remove(filepath.Join(repoDir, "synapses.json"))
	os.RemoveAll(filepath.Join(repoDir, ".claude"))
}

func gitReset(repoDir string) {
	// Force reset all changes including staged.
	cmd := exec.Command("git", "checkout", "-f", ".")
	cmd.Dir = repoDir
	_ = cmd.Run()

	cmd2 := exec.Command("git", "clean", "-fd")
	cmd2.Dir = repoDir
	_ = cmd2.Run()

	// Also remove any benchmark artifacts.
	os.Remove(filepath.Join(repoDir, ".mcp.json"))
	os.Remove(filepath.Join(repoDir, "synapses.json"))
	os.RemoveAll(filepath.Join(repoDir, ".claude"))
}

func gitApplyPatch(repoDir, patch string) error {
	cmd := exec.Command("git", "apply", "--allow-empty", "-")
	cmd.Dir = repoDir
	cmd.Stdin = strings.NewReader(patch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func gitCommitAll(repoDir, msg string) error {
	add := exec.Command("git", "add", "-A")
	add.Dir = repoDir
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, string(out))
	}
	commit := exec.Command("git", "commit", "--allow-empty", "-m", msg)
	commit.Dir = repoDir
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=bench", "GIT_AUTHOR_EMAIL=bench@local",
		"GIT_COMMITTER_NAME=bench", "GIT_COMMITTER_EMAIL=bench@local",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, string(out))
	}
	return nil
}

func gitDiffExcludeBenchFiles(repoDir string) (string, error) {
	cmd := exec.Command("git", "diff", "HEAD", "--no-ext-diff",
		"--", ".", ":!.mcp.json", ":!.claude", ":!synapses.json")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	// Strip "index <hash>..<hash>" lines.
	var cleaned strings.Builder
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "index ") {
			continue
		}
		cleaned.WriteString(line)
		cleaned.WriteByte('\n')
	}
	return cleaned.String(), nil
}

// ─── Utilities ──────────────────────────────────────────────────────────────

func countToolCalls(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func countMCPToolCalls(m map[string]int) int {
	n := 0
	for name, v := range m {
		if strings.HasPrefix(name, "mcp__") {
			n += v
		}
	}
	return n
}

// estimateCost computes the estimated USD cost based on Claude model pricing.
func estimateCost(model string, inputTokens, outputTokens int) float64 {
	// Pricing per million tokens (as of 2026).
	var inputPrice, outputPrice float64
	switch {
	case strings.Contains(model, "opus"):
		inputPrice, outputPrice = 15.0, 75.0
	case strings.Contains(model, "haiku"):
		inputPrice, outputPrice = 0.25, 1.25
	default: // sonnet and other models
		inputPrice, outputPrice = 3.0, 15.0
	}
	return (float64(inputTokens) * inputPrice / 1_000_000) +
		(float64(outputTokens) * outputPrice / 1_000_000)
}
