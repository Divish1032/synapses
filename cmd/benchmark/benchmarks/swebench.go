// swebench.go implements the SWE-bench Verified benchmark runner.
// It measures whether giving an LLM access to Synapses MCP tools improves
// its ability to solve real coding tasks (Pass@1 delta).
package benchmarks

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// SWEBenchOptions configures the SWE-bench benchmark run.
type SWEBenchOptions struct {
	DataFile string // path to JSONL dataset
	ReposDir string // directory where repos are cloned
	Limit    int    // max tasks to run (0 = all)
	Mode     string // "baseline" or "synapses"
	Model    string // Claude model name
	MaxTurns int    // max agent loop turns
	Endpoint string // Synapses daemon endpoint (synapses mode only)
}

// SWEBenchTask is one task from the SWE-bench dataset.
type SWEBenchTask struct {
	InstanceID       string `json:"instance_id"`
	Repo             string `json:"repo"`
	BaseCommit       string `json:"base_commit"`
	ProblemStatement string `json:"problem_statement"`
	Patch            string `json:"patch,omitempty"`      // gold patch (for reference)
	TestPatch        string `json:"test_patch,omitempty"` // test changes (for eval)
}

// SWEBenchTaskResult is the outcome of running the agent on one task.
type SWEBenchTaskResult struct {
	InstanceID     string          `json:"instance_id"`
	Repo           string          `json:"repo"`
	Mode           string          `json:"mode"`
	GeneratedPatch string          `json:"generated_patch"`
	Pass           bool            `json:"pass"`            // set by evaluator (manual or Docker)
	Stats          agent.AgentStats `json:"stats"`
	Error          string          `json:"error,omitempty"`
	Duration       string          `json:"duration"`
}

// RunSWEBench runs the SWE-bench benchmark in the specified mode.
func RunSWEBench(mcpClient *agent.SynapsesClient, opts SWEBenchOptions) (*reporter.SWEBenchResult, error) {
	tasks, err := loadSWEBenchData(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}

	if opts.Limit > 0 && opts.Limit < len(tasks) {
		tasks = tasks[:opts.Limit]
	}

	mode := agent.AgentMode(opts.Mode)
	cfg := agent.DefaultAgentConfig()
	if opts.Model != "" {
		cfg.Model = opts.Model
	}
	if opts.MaxTurns > 0 {
		cfg.MaxTurns = opts.MaxTurns
	}

	log.Printf("SWE-bench: running %d tasks in %s mode (model=%s, max_turns=%d)",
		len(tasks), opts.Mode, cfg.Model, cfg.MaxTurns)

	var results []SWEBenchTaskResult
	toolUsage := make(map[string]int)

	for i, task := range tasks {
		log.Printf("[%d/%d] %s (%s @ %s)", i+1, len(tasks), task.InstanceID, task.Repo, task.BaseCommit[:8])

		result := runSWEBenchTask(cfg, mode, task, mcpClient, opts)
		results = append(results, result)

		// Aggregate tool usage.
		for name, count := range result.Stats.ToolCalls {
			toolUsage[name] += count
		}

		if result.Error != "" {
			log.Printf("  ERROR: %s", result.Error)
		} else {
			patchLines := strings.Count(result.GeneratedPatch, "\n")
			log.Printf("  Done: %d turns, %d tool calls, %d patch lines, %s",
				result.Stats.TotalTurns, totalToolCalls(result.Stats.ToolCalls),
				patchLines, result.Duration)
		}
	}

	// Build reporter result.
	return buildSWEBenchResult(opts, results, toolUsage), nil
}

func runSWEBenchTask(cfg agent.AgentConfig, mode agent.AgentMode,
	task SWEBenchTask, mcpClient *agent.SynapsesClient, opts SWEBenchOptions) SWEBenchTaskResult {

	start := time.Now()
	result := SWEBenchTaskResult{
		InstanceID: task.InstanceID,
		Repo:       task.Repo,
		Mode:       opts.Mode,
	}

	// 1. Ensure repo is checked out at the correct commit.
	repoDir, err := ensureRepo(opts.ReposDir, task.Repo, task.BaseCommit)
	if err != nil {
		result.Error = fmt.Sprintf("repo setup: %v", err)
		result.Duration = time.Since(start).String()
		return result
	}

	// Reset any leftover changes from previous tasks.
	resetCmd := exec.Command("git", "checkout", ".")
	resetCmd.Dir = repoDir
	_ = resetCmd.Run()

	// 2. For synapses mode, wait for indexing.
	if mode == agent.ModeSynapses {
		projClient := mcpClient.WithProject(repoDir)
		if err := waitForIndex(projClient, repoDir); err != nil {
			log.Printf("  WARNING: index wait failed: %v (continuing anyway)", err)
		}
	}

	// 3. Build system prompt.
	systemPrompt := buildSystemPrompt(task, mode)

	// 4. Build task prompt.
	taskPrompt := fmt.Sprintf("Please fix the following issue in the %s repository.\n\n"+
		"## Issue\n\n%s\n\n"+
		"Use the available tools to explore the codebase, understand the problem, "+
		"and make the necessary changes using write_file. When you are done making "+
		"all changes, respond with a summary of what you changed and why.",
		task.Repo, task.ProblemStatement)

	// 5. Choose tools and executor.
	var tools []agent.ToolDef
	var executor agent.ToolExecutor

	if mode == agent.ModeSynapses {
		tools = agent.SynapsesTools()
		projClient := mcpClient.WithProject(repoDir)
		executor = &agent.SynapsesExecutor{
			BaselineExecutor: agent.BaselineExecutor{RepoDir: repoDir},
			Client:           projClient,
			TaskID:           task.InstanceID,
		}
	} else {
		tools = agent.BaselineTools()
		executor = &agent.BaselineExecutor{RepoDir: repoDir}
	}

	// 6. Patch extractor: git diff HEAD in the repo.
	patchExtractor := func() (string, error) {
		cmd := exec.Command("git", "diff", "HEAD")
		cmd.Dir = repoDir
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git diff: %w", err)
		}
		return string(out), nil
	}

	// 7. Run agent loop.
	agentResult, err := agent.RunAgent(cfg, systemPrompt, taskPrompt, tools, executor, patchExtractor)
	if err != nil {
		result.Error = fmt.Sprintf("agent: %v", err)
		result.Duration = time.Since(start).String()
		return result
	}

	result.GeneratedPatch = agentResult.Patch
	result.Stats = agentResult.Stats
	if agentResult.Error != "" {
		result.Error = agentResult.Error
	}
	result.Duration = time.Since(start).String()

	// 8. Reset repo for next task.
	resetCmd2 := exec.Command("git", "checkout", ".")
	resetCmd2.Dir = repoDir
	_ = resetCmd2.Run()

	return result
}

func buildSystemPrompt(task SWEBenchTask, mode agent.AgentMode) string {
	var sb strings.Builder
	sb.WriteString("You are an expert software engineer tasked with fixing a bug in an open source repository.\n\n")
	sb.WriteString("Repository: " + task.Repo + "\n")
	sb.WriteString("Commit: " + task.BaseCommit + "\n\n")

	sb.WriteString("## Available Tools\n\n")
	sb.WriteString("- read_file: Read file contents\n")
	sb.WriteString("- grep_search: Search for patterns in code\n")
	sb.WriteString("- list_directory: List directory contents\n")
	sb.WriteString("- write_file: Write/modify files to apply your fix\n")

	if mode == agent.ModeSynapses {
		sb.WriteString("\n## Synapses Code Intelligence Tools\n\n")
		sb.WriteString("You also have access to Synapses, a code intelligence engine that understands the codebase structure:\n")
		sb.WriteString("- synapses_search: Semantic code search — find relevant functions, classes, and methods by natural language query\n")
		sb.WriteString("- synapses_get_context: Get structural context — callers, callees, and related code for any entity\n")
		sb.WriteString("- synapses_get_impact: Blast-radius analysis — understand what code is affected by changes to an entity\n")
		sb.WriteString("- synapses_prepare_context: Get curated context for a specific entity and intent\n\n")
		sb.WriteString("Use Synapses tools to efficiently navigate the codebase. Start with synapses_search to find relevant code, ")
		sb.WriteString("then use synapses_get_context or synapses_get_impact to understand relationships.\n")
	}

	sb.WriteString("\n## Instructions\n\n")
	sb.WriteString("1. Read the issue carefully and understand the bug\n")
	sb.WriteString("2. Explore the codebase to find the relevant code\n")
	sb.WriteString("3. Understand the root cause\n")
	sb.WriteString("4. Make a minimal, targeted fix using write_file\n")
	sb.WriteString("5. Verify your fix addresses the issue\n\n")
	sb.WriteString("Make the smallest change that correctly fixes the bug. Do not refactor or change unrelated code.\n")

	return sb.String()
}

func buildSWEBenchResult(opts SWEBenchOptions, results []SWEBenchTaskResult, toolUsage map[string]int) *reporter.SWEBenchResult {
	r := &reporter.SWEBenchResult{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Mode:       opts.Mode,
		Model:      opts.Model,
		TotalTasks: len(results),
		ToolUsage:  toolUsage,
	}

	var totalTurns, totalTokens int
	synapsesContrib := 0

	for _, res := range results {
		if res.GeneratedPatch != "" {
			r.PatchCount++
		}
		if res.Pass {
			r.PassCount++
		}
		totalTurns += res.Stats.TotalTurns
		totalTokens += res.Stats.InputTokens + res.Stats.OutputTokens
		if res.Stats.SynapsesUsed && res.Pass {
			synapsesContrib++
		}
	}

	if r.TotalTasks > 0 {
		r.PassRate = float64(r.PassCount) / float64(r.TotalTasks) * 100
		r.PatchRate = float64(r.PatchCount) / float64(r.TotalTasks) * 100
		r.AvgTurns = float64(totalTurns) / float64(r.TotalTasks)
		r.AvgTokens = totalTokens / r.TotalTasks
	}
	if r.PassCount > 0 {
		r.ToolContribRate = float64(synapsesContrib) / float64(r.PassCount) * 100
	}

	// Convert results to interface slice for JSON.
	for _, res := range results {
		r.Tasks = append(r.Tasks, res)
	}

	return r
}

// ── Data loading ─────────────────────────────────────────────────────────────

func loadSWEBenchData(path string) ([]SWEBenchTask, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tasks []SWEBenchTask
	for lineNum, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		var task SWEBenchTask
		if err := json.Unmarshal([]byte(line), &task); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum+1, err)
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func totalToolCalls(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// WritePredictions writes the SWE-bench prediction JSONL for Docker evaluation.
func WritePredictions(results []SWEBenchTaskResult, outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, r := range results {
		pred := map[string]string{
			"instance_id":        r.InstanceID,
			"model_name_or_path": "synapses-swe-agent",
			"model_patch":        r.GeneratedPatch,
		}
		if err := enc.Encode(pred); err != nil {
			return err
		}
	}
	return nil
}

// ── Exported types for predictions file ──────────────────────────────────────

// ExportPredictions returns the list of task results for external use.
func ExportPredictions(results []SWEBenchTaskResult, dir string) error {
	return WritePredictions(results, filepath.Join(dir, "predictions.jsonl"))
}
