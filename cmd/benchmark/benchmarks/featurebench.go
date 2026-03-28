// featurebench.go implements the FeatureBench benchmark runner.
// It measures whether giving Claude Code access to Synapses MCP tools improves
// its ability to implement features (pass@1 delta on FeatureBench tasks).
//
// Unlike swebench.go which uses the Go agent loop + Anthropic API directly,
// this runner shells out to `claude -p` so it works with Max subscriptions
// (OAuth) without requiring an API key. Synapses tools are loaded via .mcp.json.
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

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// FeatureBenchOptions configures the FeatureBench run.
type FeatureBenchOptions struct {
	Split       string   // HF split: "lite", "fast", "full"
	TaskIDs     []string // optional: specific task IDs
	Level       int      // 0 = all, 1 or 2
	ReposDir    string   // where repos are cloned
	Limit       int      // max tasks (0 = all)
	Mode        string   // "baseline" or "synapses"
	Model       string   // Claude model (passed via ANTHROPIC_MODEL env)
	Timeout     int      // seconds per task (default 1200 = 20min)
	SynapsesBin string   // path to synapses binary (for init + index)
	OutputDir   string   // where to write predictions JSONL
	Debug       bool     // dump raw stream-json to file for inspection
}

// FeatureBenchTask is one task from the FeatureBench dataset.
type FeatureBenchTask struct {
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

// FeatureBenchTaskResult is the outcome of running Claude on one task.
type FeatureBenchTaskResult struct {
	InstanceID string            `json:"instance_id"`
	Repo       string            `json:"repo"`
	Mode       string            `json:"mode"`
	ModelPatch string            `json:"model_patch"`
	ToolCalls  map[string]int    `json:"tool_calls,omitempty"`
	Turns      int               `json:"turns"`
	Error      string            `json:"error,omitempty"`
	Duration   string            `json:"duration"`
	Task       *FeatureBenchTask `json:"-"` // for metadata output
}

// FeatureBenchPrediction is the JSONL format that fb eval expects.
type FeatureBenchPrediction struct {
	InstanceID   string                 `json:"instance_id"`
	ModelPatch   string                 `json:"model_patch"`
	TaskMetadata map[string]interface{} `json:"task_metadata,omitempty"`
}

// RunFeatureBench runs the FeatureBench benchmark.
func RunFeatureBench(opts FeatureBenchOptions) ([]FeatureBenchTaskResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 1200 // 20 minutes
	}

	// Find claude binary.
	claudeBin, err := findClaudeBin()
	if err != nil {
		return nil, fmt.Errorf("claude CLI not found: %w", err)
	}
	log.Printf("FeatureBench: using claude at %s", claudeBin)

	// Find synapses binary.
	if opts.SynapsesBin == "" {
		opts.SynapsesBin = findSynapsesBin()
	}

	tasks, err := loadFeatureBenchData(opts)
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}

	if opts.Limit > 0 && opts.Limit < len(tasks) {
		tasks = tasks[:opts.Limit]
	}

	log.Printf("FeatureBench: running %d tasks in %s mode (model=%s, timeout=%ds)",
		len(tasks), opts.Mode, opts.Model, opts.Timeout)

	// Open predictions file for incremental writes (each task appended immediately).
	var predPath string
	var predFile *os.File
	if opts.OutputDir != "" {
		predPath = filepath.Join(opts.OutputDir,
			fmt.Sprintf("featurebench_%s_%s.jsonl", opts.Mode, time.Now().UTC().Format("2006-01-02_15-04-05")))
		os.MkdirAll(filepath.Dir(predPath), 0o755)
		var err error
		predFile, err = os.Create(predPath)
		if err != nil {
			log.Printf("WARNING: cannot create predictions file: %v", err)
		} else {
			defer predFile.Close()
		}
	}

	var results []FeatureBenchTaskResult
	for i, task := range tasks {
		log.Printf("[%d/%d] %s (%s @ %s)", i+1, len(tasks),
			task.InstanceID, task.Repo, task.BaseCommit[:minInt(8, len(task.BaseCommit))])

		result := runFeatureBenchTask(claudeBin, task, opts)
		results = append(results, result)

		if result.Error != "" {
			log.Printf("  ERROR: %s", result.Error)
		} else {
			patchLines := strings.Count(result.ModelPatch, "\n")
			mcpCalls := countMCPToolCalls(result.ToolCalls)
			log.Printf("  Done: %d turns, %d tool calls (%d MCP), %d patch lines, %s",
				result.Turns, countToolCalls(result.ToolCalls), mcpCalls, patchLines, result.Duration)
		}

		// Incremental write: append this result to JSONL immediately.
		if predFile != nil {
			pred := FeatureBenchPrediction{
				InstanceID: result.InstanceID,
				ModelPatch: result.ModelPatch,
			}
			if result.Task != nil {
				pred.TaskMetadata = map[string]interface{}{
					"image_name":    result.Task.ImageName,
					"repo_settings": json.RawMessage(result.Task.RepoSettings),
				}
			}
			if err := json.NewEncoder(predFile).Encode(pred); err != nil {
				log.Printf("WARNING: failed to write prediction: %v", err)
			}
		}
	}

	if predPath != "" {
		log.Printf("Predictions written to %s", predPath)
	}

	return results, nil
}

func runFeatureBenchTask(claudeBin string, task FeatureBenchTask, opts FeatureBenchOptions) FeatureBenchTaskResult {
	start := time.Now()
	result := FeatureBenchTaskResult{
		InstanceID: task.InstanceID,
		Repo:       task.Repo,
		Mode:       opts.Mode,
		Task:       &task,
		ToolCalls:  make(map[string]int),
	}

	// 1. Ensure repo is checked out.
	repoDir, err := ensureRepo(opts.ReposDir, task.Repo, task.BaseCommit)
	if err != nil {
		result.Error = fmt.Sprintf("repo setup: %v", err)
		result.Duration = time.Since(start).String()
		return result
	}

	// Reset any leftover changes.
	gitReset(repoDir)

	// 2. Apply corruption patches (the code the agent must recreate).
	if task.Patch != "" {
		if err := gitApplyPatch(repoDir, task.Patch); err != nil {
			result.Error = fmt.Sprintf("apply patch: %v", err)
			result.Duration = time.Since(start).String()
			return result
		}
	}
	if task.TestPatch != "" {
		if err := gitApplyPatch(repoDir, task.TestPatch); err != nil {
			result.Error = fmt.Sprintf("apply test_patch: %v", err)
			result.Duration = time.Since(start).String()
			return result
		}
	}

	// 3. CRITICAL: Commit corruption as the new baseline.
	// fb eval applies corruption FIRST, then applies model_patch on top.
	// So model_patch must be relative to the corrupted state, not base_commit.
	if err := gitCommitAll(repoDir, "corruption baseline"); err != nil {
		result.Error = fmt.Sprintf("commit corruption: %v", err)
		result.Duration = time.Since(start).String()
		return result
	}

	// 4. Mode-specific setup.
	if opts.Mode == "synapses" {
		if err := setupSynapses(opts.SynapsesBin, repoDir); err != nil {
			log.Printf("  WARNING: synapses setup failed: %v (continuing without)", err)
		}
	} else {
		// Baseline: remove any .mcp.json and .claude/ that might exist
		os.Remove(filepath.Join(repoDir, ".mcp.json"))
		os.RemoveAll(filepath.Join(repoDir, ".claude"))
	}

	// 5. Run claude -p.
	allowedTools := "Bash Read Write Edit Grep Glob"
	disallowedTools := ""
	var systemPrompt string
	if opts.Mode == "synapses" {
		// Add Synapses MCP tools.
		allowedTools += " mcp__synapses__*"
		// Remove Grep and Glob so Claude is FORCED to use mcp__synapses__search
		// for codebase exploration. Without this, Claude always prefers built-in tools.
		disallowedTools = "Grep,Glob"
		systemPrompt = synapsesSystemPrompt
	}

	streamJSON, err := runClaudeCode(claudeBin, repoDir, task.ProblemStatement, allowedTools, disallowedTools, opts.Model, systemPrompt, opts.Timeout)
	if err != nil {
		result.Error = fmt.Sprintf("claude: %v", err)
		result.Duration = time.Since(start).String()
		// Still try to capture patch even on error.
		result.ModelPatch, _ = gitDiffExcludeBenchFiles(repoDir)
		return result
	}

	// 6. Debug: dump raw stream-json if requested.
	if opts.Debug && opts.OutputDir != "" {
		debugPath := filepath.Join(opts.OutputDir,
			fmt.Sprintf("stream_%s_%s.json", opts.Mode, task.InstanceID))
		_ = os.WriteFile(debugPath, []byte(streamJSON), 0o644)
		log.Printf("  Debug stream written to %s", debugPath)
	}

	// 7. Parse stats from stream output.
	result.ToolCalls, result.Turns = parseStreamStats(streamJSON)

	// 8. Capture patch — exclude benchmark artifacts (.mcp.json, .claude/, synapses.json).
	// This diff is relative to corruption commit (step 3), so it contains ONLY agent changes.
	result.ModelPatch, err = gitDiffExcludeBenchFiles(repoDir)
	if err != nil {
		result.Error = fmt.Sprintf("git diff: %v", err)
	}

	// 9. Reset for next task — force back to the base commit.
	resetToCommit(repoDir, task.BaseCommit)

	result.Duration = time.Since(start).String()
	return result
}

// ─── Dataset Loading ────────────────────────────────────────────────────────

func loadFeatureBenchData(opts FeatureBenchOptions) ([]FeatureBenchTask, error) {
	// Find the Python script relative to the benchmark binary or CWD.
	scriptPath := findScript("load_featurebench.py")

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

	var tasks []FeatureBenchTask
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0), 10*1024*1024) // 10MB line buffer
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var task FeatureBenchTask
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

// synapsesSystemPrompt is injected via --append-system-prompt in Synapses mode.
// It MUST be strong enough to override Claude's default behavior of using Bash/Grep.
const synapsesSystemPrompt = `You have access to Synapses MCP tools that provide structural code intelligence (call graphs, dependency trees, symbol search). These tools have already indexed this entire codebase.

CRITICAL WORKFLOW — You MUST follow this sequence:

Step 1: BEFORE reading any files, call mcp__synapses__search with a query describing what you need to find. This returns functions, types, and files with exact locations.

Step 2: For each relevant result, call mcp__synapses__get_context to understand its callers, callees, imports, and dependency graph.

Step 3: Call mcp__synapses__get_impact before making changes to understand what depends on the code you're modifying.

Step 4: Only THEN use Read/Edit/Bash to make your changes.

These tools give you the dependency graph — who calls what, what imports what, what would break if you change something. This is impossible to get from grep/find alone. Use them.`

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

func parseStreamStats(streamJSON string) (toolCalls map[string]int, turns int) {
	toolCalls = make(map[string]int)
	scanner := bufio.NewScanner(strings.NewReader(streamJSON))
	scanner.Buffer(make([]byte, 0), 1024*1024) // 1MB per line

	for scanner.Scan() {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if msg.Type == "assistant" {
			turns++
			for _, c := range msg.Message.Content {
				if c.Type == "tool_use" && c.Name != "" {
					toolCalls[c.Name]++
				}
			}
		}
	}
	return
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
	// The daemon indexes on first request for a project.
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
	// Diff against HEAD (corruption commit), excluding benchmark artifacts.
	// Use --no-ext-diff to avoid external diff tools, and strip index lines
	// since fb eval runs in a fresh git init container with different object hashes.
	cmd := exec.Command("git", "diff", "HEAD", "--no-ext-diff",
		"--", ".", ":!.mcp.json", ":!.claude", ":!synapses.json")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	// Strip "index <hash>..<hash>" lines — fb eval's container has a fresh
	// git init so these hashes are meaningless and can cause apply failures.
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

// ─── Output ─────────────────────────────────────────────────────────────────
// Predictions are now written incrementally in RunFeatureBench (no batch function needed).

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

// minInt is already defined in contextbench.go (same package).

// BuildFeatureBenchReport aggregates task results into a reporter-compatible struct.
func BuildFeatureBenchReport(mode, model string, results []FeatureBenchTaskResult) *reporter.FeatureBenchReport {
	r := &reporter.FeatureBenchReport{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Mode:       mode,
		Model:      model,
		TotalTasks: len(results),
		ToolUsage:  make(map[string]int),
	}

	totalTurns := 0
	for _, tr := range results {
		if tr.ModelPatch != "" {
			r.PatchCount++
		}
		totalTurns += tr.Turns
		for name, count := range tr.ToolCalls {
			r.ToolUsage[name] += count
		}
		r.Tasks = append(r.Tasks, tr)
	}

	if len(results) > 0 {
		r.PatchRate = float64(r.PatchCount) / float64(len(results)) * 100
		r.AvgTurns = float64(totalTurns) / float64(len(results))
	}

	return r
}
