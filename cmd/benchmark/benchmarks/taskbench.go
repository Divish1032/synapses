// taskbench.go implements the end-to-end TaskBench benchmark.
// Measures Synapses' complete product value on real coding tasks (SWE-bench).
//
// Two modes:
//   - baseline: Claude + Read/Write/Edit/Grep/Glob/Bash (no Synapses)
//   - synapses: Claude + Synapses MCP tools (search, get_context, get_impact, etc.)
//
// Metrics: resolve rate (pass@1), turns, tokens, cost, tool usage.
// Default uses `claude -p` (Max subscription, no API key).
// Eval via SWE-bench Docker test harness.
package benchmarks

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// TaskBenchOptions configures the TaskBench run.
type TaskBenchOptions struct {
	DataFile    string   // optional JSONL override (skips HF download)
	ReposDir    string   // where repos are cloned
	Limit       int      // max tasks (0 = all)
	Mode        string   // "baseline" or "synapses"
	Model       string   // Claude model (default: claude-sonnet-4-6)
	Timeout     int      // per-task timeout in seconds (default: 600)
	OutputDir   string   // where to write results + predictions
	Debug       bool     // dump raw stream-json per task
	BothModes   bool     // run baseline + synapses, compute delta
	Eval        bool     // run Docker eval (default: true)
	DaemonPort  string   // Synapses daemon port (default: "11435")
	InstanceIDs []string // optional instance ID filter
}

// TaskBenchTaskResult holds per-task metrics.
type TaskBenchTaskResult struct {
	InstanceID          string         `json:"instance_id"`
	Repo                string         `json:"repo"`
	Mode                string         `json:"mode"`
	ModelPatch          string         `json:"model_patch"`
	Resolved            bool           `json:"resolved"`
	ToolCalls           map[string]int `json:"tool_calls"`
	Turns               int            `json:"turns"`
	InputTokens         int            `json:"input_tokens"`
	OutputTokens        int            `json:"output_tokens"`
	CacheCreationTokens int            `json:"cache_creation_tokens"`
	CacheReadTokens     int            `json:"cache_read_tokens"`
	TotalTokens         int            `json:"total_tokens"`
	CostUSD             float64        `json:"cost_usd"`
	MCPConnected        bool           `json:"mcp_connected"`
	Duration            string         `json:"duration"`
	Error               string         `json:"error,omitempty"`
}

// RunTaskBench runs the TaskBench benchmark.
func RunTaskBench(opts TaskBenchOptions) ([]TaskBenchTaskResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 600
	}
	if opts.Model == "" {
		opts.Model = "claude-sonnet-4-6"
	}
	if opts.DaemonPort == "" {
		opts.DaemonPort = "11435"
	}

	claudeBin, err := findClaudeBin()
	if err != nil {
		return nil, fmt.Errorf("claude CLI not found: %w", err)
	}
	log.Printf("[taskbench] using claude at %s", claudeBin)

	// Load tasks.
	tasks, err := loadTaskBenchData(opts)
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}
	if opts.Limit > 0 && len(tasks) > opts.Limit {
		tasks = tasks[:opts.Limit]
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no tasks after filtering")
	}

	// Sort by repo for sequential processing.
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].Repo < tasks[j].Repo
	})

	log.Printf("[taskbench] %d tasks, mode=%s, model=%s, timeout=%ds, eval=%v",
		len(tasks), opts.Mode, opts.Model, opts.Timeout, opts.Eval)

	// Open predictions file for incremental writes.
	var predFile *os.File
	if opts.OutputDir != "" {
		predPath := filepath.Join(opts.OutputDir,
			fmt.Sprintf("taskbench_%s_predictions.jsonl", opts.Mode))
		os.MkdirAll(opts.OutputDir, 0o755)
		predFile, err = os.Create(predPath)
		if err != nil {
			log.Printf("WARNING: cannot create predictions file: %v", err)
		} else {
			defer predFile.Close()
		}
	}

	var results []TaskBenchTaskResult
	prevRepo := ""

	for i, task := range tasks {
		// Cleanup previous repo when switching to free disk + memory.
		if task.Repo != prevRepo && prevRepo != "" {
			cleanupPrevRepo(opts.ReposDir, prevRepo)
		}
		prevRepo = task.Repo

		// In synapses mode, restart daemon between tasks to free memory.
		// Each task at a different commit creates a new project in the daemon;
		// without cleanup, memory grows unbounded (~1GB per project).
		if opts.Mode == "synapses" && i > 0 {
			restartDaemonForBench()
		}

		log.Printf("[taskbench] task %d/%d: %s (%s @ %s)",
			i+1, len(tasks), task.InstanceID, task.Repo, task.BaseCommit[:minStr(8, len(task.BaseCommit))])

		tr := runTaskBenchTask(claudeBin, task, opts)
		results = append(results, tr)

		if tr.Error != "" {
			log.Printf("  ERROR: %s", tr.Error)
		} else {
			patchLines := strings.Count(tr.ModelPatch, "\n")
			mcpTag := ""
			if opts.Mode == "synapses" {
				if tr.MCPConnected {
					mcpTag = " [MCP:ok]"
				} else {
					mcpTag = " [MCP:FAILED]"
				}
			}
			log.Printf("  → turns=%d patch=%d lines cost=$%.4f tokens=%d %s%s",
				tr.Turns, patchLines, tr.CostUSD, tr.TotalTokens, tr.Duration, mcpTag)
		}

		// Write prediction incrementally.
		if predFile != nil && tr.ModelPatch != "" {
			pred := map[string]string{
				"instance_id":        tr.InstanceID,
				"model_name_or_path": "synapses-taskbench",
				"model_patch":        tr.ModelPatch,
			}
			json.NewEncoder(predFile).Encode(pred)
		}
	}

	// Cleanup last repo.
	if prevRepo != "" {
		cleanupPrevRepo(opts.ReposDir, prevRepo)
	}

	return results, nil
}

func runTaskBenchTask(claudeBin string, task BenchTask, opts TaskBenchOptions) TaskBenchTaskResult {
	start := time.Now()
	result := TaskBenchTaskResult{
		InstanceID: task.InstanceID,
		Repo:       task.Repo,
		Mode:       opts.Mode,
		ToolCalls:  make(map[string]int),
	}

	// 1. Ensure repo is checked out at base_commit.
	repoDir, err := ensureRepo(opts.ReposDir, task.Repo, task.BaseCommit)
	if err != nil {
		result.Error = fmt.Sprintf("repo setup: %v", err)
		result.Duration = time.Since(start).String()
		return result
	}
	gitReset(repoDir)

	// Resolve symlinks (macOS /tmp → /private/tmp).
	if real, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = real
	}

	// 2. Apply test patch (ground truth tests the agent must satisfy).
	if task.TestPatch != "" {
		if err := gitApplyPatch(repoDir, task.TestPatch); err != nil {
			result.Error = fmt.Sprintf("apply test_patch: %v", err)
			result.Duration = time.Since(start).String()
			return result
		}
	}
	if err := gitCommitAll(repoDir, "test baseline"); err != nil {
		result.Error = fmt.Sprintf("commit test baseline: %v", err)
		result.Duration = time.Since(start).String()
		return result
	}

	// 3. Mode-specific setup.
	if opts.Mode == "synapses" {
		setupTaskBenchSynapses(repoDir, opts.DaemonPort)
	} else {
		// Baseline: ensure no MCP artifacts.
		os.Remove(filepath.Join(repoDir, ".mcp.json"))
		os.Remove(filepath.Join(repoDir, "synapses.json"))
		os.RemoveAll(filepath.Join(repoDir, ".claude"))
	}

	// 4. Build prompt.
	prompt := buildTaskBenchPrompt(task)

	// 5. Configure tools.
	allowedTools := "Bash Read Write Edit Grep Glob"
	disallowedTools := ""
	systemPrompt := ""

	if opts.Mode == "synapses" {
		allowedTools = "Bash Read Write Edit mcp__synapses__*"
		disallowedTools = "Grep,Glob"
		systemPrompt = taskBenchSynapsesPrompt
	}

	// 6. Run Claude with MCP verification + retry.
	var streamJSON string
	maxAttempts := 1
	if opts.Mode == "synapses" {
		maxAttempts = 3
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		streamJSON, err = runClaudeCode(claudeBin, repoDir, prompt, allowedTools,
			disallowedTools, opts.Model, systemPrompt, opts.Timeout)
		if err != nil {
			log.Printf("  Claude error (attempt %d): %v", attempt, err)
		}

		if opts.Mode == "synapses" {
			if checkMCPConnected(streamJSON) {
				result.MCPConnected = true
				if attempt > 1 {
					log.Printf("  MCP connected on attempt %d", attempt)
				}
				break
			}
			if attempt < maxAttempts {
				log.Printf("  MCP failed (attempt %d/%d), retrying...", attempt, maxAttempts)
				// Reset repo state before retry — Claude may have made changes.
				resetToCommit(repoDir, "HEAD")
				time.Sleep(5 * time.Second)
				continue
			}
			result.Error = "MCP connection failed after 3 attempts"
			result.Duration = time.Since(start).String()
			resetToCommit(repoDir, task.BaseCommit)
			return result
		}
		break // baseline: no MCP verification needed
	}

	// 7. Debug output.
	if opts.Debug && opts.OutputDir != "" {
		debugPath := filepath.Join(opts.OutputDir,
			fmt.Sprintf("taskbench_%s_%s.json", opts.Mode, task.InstanceID))
		os.WriteFile(debugPath, []byte(streamJSON), 0o644)
	}

	// 8. Parse stats.
	stats := parseStreamStats(streamJSON)
	result.ToolCalls = stats.ToolCalls
	result.Turns = stats.Turns
	result.InputTokens = stats.InputTokens
	result.OutputTokens = stats.OutputTokens
	result.CacheCreationTokens = stats.CacheCreationTokens
	result.CacheReadTokens = stats.CacheReadTokens
	result.TotalTokens = stats.InputTokens + stats.OutputTokens + stats.CacheCreationTokens + stats.CacheReadTokens
	// Use Claude Code's own cost tracking (accurate), fall back to estimate.
	if stats.TotalCostUSD > 0 {
		result.CostUSD = stats.TotalCostUSD
	} else {
		result.CostUSD = estimateCost(opts.Model, stats.InputTokens+stats.CacheCreationTokens+stats.CacheReadTokens, stats.OutputTokens)
	}

	// 9. Capture model patch — diff relative to test baseline commit.
	result.ModelPatch, _ = gitDiffExcludeBenchFiles(repoDir)

	// 10. Reset for next task.
	resetToCommit(repoDir, task.BaseCommit)

	result.Duration = time.Since(start).String()
	return result
}

// ─── Data Loading ───────────────────────────────────────────────────────────

func loadTaskBenchData(opts TaskBenchOptions) ([]BenchTask, error) {
	// If a JSONL file is provided, load directly.
	if opts.DataFile != "" {
		return loadTaskBenchJSONL(opts.DataFile, opts.InstanceIDs)
	}

	// Otherwise, load from HuggingFace via Python script.
	scriptPath := findScript("load_swebench.py")
	args := []string{scriptPath}
	if opts.Limit > 0 {
		args = append(args, "--limit", fmt.Sprintf("%d", opts.Limit))
	}
	if len(opts.InstanceIDs) > 0 {
		args = append(args, "--instance-ids", strings.Join(opts.InstanceIDs, ","))
	}

	cmd := exec.Command("python3", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("load_swebench.py: %w", err)
	}

	return parseTaskBenchJSONL(string(out), opts.InstanceIDs)
}

func loadTaskBenchJSONL(path string, instanceIDs []string) ([]BenchTask, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseTaskBenchJSONL(string(data), instanceIDs)
}

func parseTaskBenchJSONL(data string, instanceIDs []string) ([]BenchTask, error) {
	idFilter := make(map[string]bool)
	for _, id := range instanceIDs {
		if id != "" {
			idFilter[id] = true
		}
	}

	var tasks []BenchTask
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var task BenchTask
		if err := json.Unmarshal([]byte(line), &task); err != nil {
			log.Printf("WARNING: skip malformed line: %v", err)
			continue
		}
		if len(idFilter) > 0 && !idFilter[task.InstanceID] {
			continue
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// ─── Prompt ─────────────────────────────────────────────────────────────────

func buildTaskBenchPrompt(task BenchTask) string {
	return fmt.Sprintf(`You are an expert software engineer. Fix the following issue in the %s repository.

## Issue

%s

## Instructions

1. Understand the problem by reading the relevant code
2. Identify the root cause
3. Make the minimal, targeted fix using Write/Edit
4. Verify your fix makes sense — do NOT introduce regressions
5. Do NOT modify test files

Make the smallest change that correctly fixes the issue. Do not refactor unrelated code.`, task.Repo, task.ProblemStatement)
}

const taskBenchSynapsesPrompt = `Synapses MCP tools are available with indexed codebase. Start with mcp__synapses__get_context(mode="investigate", problem="<issue summary>") for ranked code with source. Then mcp__synapses__search for symbol lookup, mcp__synapses__get_impact before editing.`

// ─── Synapses Setup ─────────────────────────────────────────────────────────

func setupTaskBenchSynapses(repoDir, port string) {
	synapsesBin := findSynapsesBin()

	// Pre-warm: run `synapses start` once to register the project with the
	// daemon and wait for indexing. This ensures the per-project Unix socket
	// exists BEFORE Claude launches its MCP subprocess. Without this, Claude's
	// MCP handshake times out while the daemon is still indexing.
	log.Printf("  pre-warming Synapses for %s...", filepath.Base(repoDir))
	warmCmd := exec.Command(synapsesBin, "start", "--path", repoDir)
	warmCmd.Stdin = strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"method\":\"initialize\",\"id\":1,\"params\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"clientInfo\":{\"name\":\"bench-warmup\",\"version\":\"1.0\"}}}\n")
	warmDone := make(chan error, 1)
	go func() {
		out, err := warmCmd.CombinedOutput()
		_ = out
		warmDone <- err
	}()
	select {
	case <-warmDone:
		log.Printf("  pre-warm complete")
	case <-time.After(90 * time.Second):
		if warmCmd.Process != nil {
			warmCmd.Process.Kill()
		}
		log.Printf("  pre-warm timed out (90s) — continuing anyway")
	}

	// .mcp.json — stdio transport.
	mcpJSON := fmt.Sprintf(`{
  "mcpServers": {
    "synapses": {
      "type": "stdio",
      "command": %q,
      "args": ["start", "--path", %q]
    }
  }
}`, synapsesBin, repoDir)
	os.WriteFile(filepath.Join(repoDir, ".mcp.json"), []byte(mcpJSON), 0o644)

	// .claude/settings.json — auto-approve tools.
	claudeDir := filepath.Join(repoDir, ".claude")
	os.MkdirAll(claudeDir, 0o755)
	settingsJSON := `{
  "permissions": {
    "allow": [
      "mcp__synapses__*",
      "Bash(*)",
      "Read(*)",
      "Write(*)",
      "Edit(*)"
    ]
  }
}`
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settingsJSON), 0o644)
}

// ─── Eval ───────────────────────────────────────────────────────────────────

// EvalTaskBench runs SWE-bench Docker evaluation on predictions.
// Updates each result's Resolved field in-place.
func EvalTaskBench(results []TaskBenchTaskResult, outputDir string) error {
	predPath := filepath.Join(outputDir, "taskbench_predictions_eval.jsonl")

	// Write predictions JSONL.
	f, err := os.Create(predPath)
	if err != nil {
		return fmt.Errorf("create predictions: %w", err)
	}
	for _, r := range results {
		if r.ModelPatch == "" {
			continue
		}
		pred := map[string]string{
			"instance_id":        r.InstanceID,
			"model_name_or_path": "synapses-taskbench",
			"model_patch":        r.ModelPatch,
		}
		json.NewEncoder(f).Encode(pred)
	}
	f.Close()

	// Run eval script.
	scriptPath := findScript("eval_swebench.py")
	cmd := exec.Command("python3", scriptPath,
		"--predictions", predPath,
		"--run-id", "taskbench-eval")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("eval_swebench.py: %w (output: %s)", err, string(out))
	}

	// Parse results.
	var evalResult struct {
		Results []struct {
			InstanceID string `json:"instance_id"`
			Resolved   bool   `json:"resolved"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &evalResult); err != nil {
		return fmt.Errorf("parse eval results: %w", err)
	}

	// Update results in-place.
	resolvedMap := make(map[string]bool)
	for _, er := range evalResult.Results {
		resolvedMap[er.InstanceID] = er.Resolved
	}
	for i := range results {
		if resolved, ok := resolvedMap[results[i].InstanceID]; ok {
			results[i].Resolved = resolved
		}
	}

	return nil
}

// ─── Report Builder ─────────────────────────────────────────────────────────

// BuildTaskBenchReport aggregates per-task results into a report.
func BuildTaskBenchReport(mode, model string, results []TaskBenchTaskResult) *reporter.TaskBenchReport {
	r := &reporter.TaskBenchReport{
		Timestamp:  reporter.Timestamp(),
		Mode:       mode,
		Model:      model,
		TotalTasks: len(results),
		ToolUsage:  make(map[string]int),
	}

	var totalTurns int
	var totalCost float64

	for _, tr := range results {
		if tr.ModelPatch != "" {
			r.PatchCount++
		}
		if tr.Resolved {
			r.Resolved++
		}
		totalTurns += tr.Turns
		totalCost += tr.CostUSD

		for name, count := range tr.ToolCalls {
			r.ToolUsage[name] += count
		}
		r.Tasks = append(r.Tasks, tr)
	}

	n := float64(len(results))
	if n > 0 {
		r.ResolveRate = float64(r.Resolved) / n * 100
		r.PatchRate = float64(r.PatchCount) / n * 100
		r.AvgTurns = float64(totalTurns) / n
		r.AvgCostUSD = totalCost / n
		r.TotalCostUSD = totalCost
	}

	return r
}

// restartDaemonForBench kills and restarts the Synapses daemon to free memory.
func restartDaemonForBench() {
	synapsesBin := findSynapsesBin()
	stop := exec.Command(synapsesBin, "stop")
	stop.Run()
	time.Sleep(2 * time.Second)
	start := exec.Command(synapsesBin, "daemon", "serve")
	start.Stdout = nil
	start.Stderr = nil
	start.Start()
	// Wait for health.
	for attempt := 0; attempt < 30; attempt++ {
		time.Sleep(time.Second)
		resp, err := http.Get("http://127.0.0.1:11435/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
	}
	log.Printf("  WARNING: daemon restart may have failed")
}

func minStr(a, b int) int {
	if a < b {
		return a
	}
	return b
}
