// contextbench_llm.go implements the LLM-powered ContextBench runner.
// Instead of a heuristic pipeline, it shells out to `claude -p` with (optional)
// Synapses MCP tools, asks Claude to identify relevant files and line ranges,
// then scores against gold context.
//
// Two modes:
//   - baseline: Claude + Read/Grep/Glob/Bash (no Synapses) — what Claude can do alone
//   - synapses: Claude + Synapses MCP tools — measures Synapses value-add
//
// Uses `claude -p` with Max subscription (no API key required).
package benchmarks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// LLMContextBenchOptions configures the LLM-powered ContextBench run.
type LLMContextBenchOptions struct {
	DataFile    string   // path to contextbench.jsonl
	ReposDir    string   // where repos are cloned
	Limit       int      // max tasks (0 = all)
	Languages   []string // language filter (empty = all)
	Sources     []string // source filter (empty = all)
	Mode        string   // "baseline" or "synapses"
	Model       string   // Claude model (default: claude-sonnet-4-6)
	Timeout     int      // seconds per task (default: 180)
	SynapsesBin string   // path to synapses binary
	OutputDir   string   // where to write debug output
	Debug       bool     // dump raw stream-json
	BothModes   bool     // run baseline + synapses, compute delta
	DaemonPort  string   // daemon HTTP port (default: "11435")
}

// LLMContextBenchTaskResult holds per-task metrics for LLM mode.
type LLMContextBenchTaskResult struct {
	InstanceID      string         `json:"instance_id"`
	Repo            string         `json:"repo"`
	Language        string         `json:"language"`
	Mode            string         `json:"mode"`
	Precision       float64        `json:"precision"`
	Recall          float64        `json:"recall"`
	F1              float64        `json:"f1"`
	FilePrecision   float64        `json:"file_precision"`
	FileRecall      float64        `json:"file_recall"`
	FileF1          float64        `json:"file_f1"`
	GoldLines       int            `json:"gold_lines"`
	HitLines        int            `json:"hit_lines"`
	RetrievedLines  int            `json:"retrieved_lines"`
	GoldFiles       int            `json:"gold_files"`
	HitFiles        int            `json:"hit_files"`
	RetrievedFiles  int            `json:"retrieved_files"`
	ToolCalls       map[string]int `json:"tool_calls"`
	Turns           int            `json:"turns"`
	InputTokens     int            `json:"input_tokens"`
	OutputTokens    int            `json:"output_tokens"`
	CostEstimateUSD float64        `json:"cost_estimate_usd"`
	Duration        string         `json:"duration"`
	Error           string         `json:"error,omitempty"`
}

// retrievedRange is a file + line range identified by Claude.
type retrievedRange struct {
	File      string
	StartLine int
	EndLine   int
}

// RunLLMContextBench runs the LLM-powered ContextBench evaluation.
func RunLLMContextBench(opts LLMContextBenchOptions) (*reporter.LLMContextBenchResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 180
	}
	if opts.DaemonPort == "" {
		opts.DaemonPort = "11435"
	}
	if opts.Model == "" {
		opts.Model = "claude-sonnet-4-6"
	}

	claudeBin, err := findClaudeBin()
	if err != nil {
		return nil, fmt.Errorf("claude CLI not found: %w", err)
	}
	if opts.SynapsesBin == "" {
		opts.SynapsesBin = findSynapsesBin()
	}

	tasks, err := loadContextBenchTasks(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("load dataset: %w", err)
	}
	tasks = filterTasks(tasks, opts.Languages, opts.Sources)
	if opts.Limit > 0 && len(tasks) > opts.Limit {
		tasks = tasks[:opts.Limit]
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no tasks after filtering")
	}

	// Sort by repo for index reuse.
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Repo != tasks[j].Repo {
			return tasks[i].Repo < tasks[j].Repo
		}
		return tasks[i].BaseCommit < tasks[j].BaseCommit
	})

	log.Printf("[llm-contextbench] %d tasks, mode=%s, model=%s, timeout=%ds",
		len(tasks), opts.Mode, opts.Model, opts.Timeout)

	var results []LLMContextBenchTaskResult
	prevRepo := ""
	for i, task := range tasks {
		// Cleanup: when switching repos, remove old cloned repo to free disk + memory.
		// The stdio MCP subprocess exits when Claude exits, so no daemon cleanup needed.
		if task.Repo != prevRepo && prevRepo != "" {
			cleanupPrevRepo(opts.ReposDir, prevRepo)
		}
		prevRepo = task.Repo

		log.Printf("[llm-contextbench] task %d/%d: %s", i+1, len(tasks), task.InstanceID)

		tr := runLLMContextBenchTask(claudeBin, task, opts)
		results = append(results, tr)

		if tr.Error != "" {
			log.Printf("  ERROR: %s", tr.Error)
		} else {
			log.Printf("  → F1=%.1f%% FileF1=%.1f%% (lines=%d/%d files=%d/%d turns=%d cost=$%.4f)",
				tr.F1*100, tr.FileF1*100,
				tr.HitLines, tr.GoldLines, tr.HitFiles, tr.GoldFiles,
				tr.Turns, tr.CostEstimateUSD)
		}
	}

	// Cleanup last repo.
	if prevRepo != "" {
		cleanupPrevRepo(opts.ReposDir, prevRepo)
	}

	return buildLLMContextBenchReport(opts.Mode, opts.Model, results), nil
}

// checkMCPConnected parses the stream-json output and returns true
// if the synapses MCP server was configured and available.
// Claude CLI 2.1+ reports "pending" at init (async connection), so we
// accept both "connected" and "pending" in the init message. We also
// check whether any mcp__synapses tool calls appear in the output —
// if they do, MCP definitely worked regardless of init status.
func checkMCPConnected(streamJSON string) bool {
	// Fast path: if any mcp__synapses tool call appears, MCP was connected.
	if strings.Contains(streamJSON, "mcp__synapses") {
		return true
	}

	// Check init message for MCP server presence.
	idx := strings.Index(streamJSON, "\n")
	firstLine := streamJSON
	if idx > 0 {
		firstLine = streamJSON[:idx]
	}
	if !strings.Contains(firstLine, "mcp_servers") {
		return false
	}

	var msg struct {
		MCPServers []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"mcp_servers"`
	}
	if json.Unmarshal([]byte(firstLine), &msg) != nil {
		return false
	}
	for _, s := range msg.MCPServers {
		if s.Name == "synapses" && (s.Status == "connected" || s.Status == "pending") {
			return true
		}
	}
	return false
}

// cleanupPrevRepo removes the cloned repo directory to free disk space.
func cleanupPrevRepo(reposDir, repo string) {
	safeName := strings.ReplaceAll(repo, "/", "_")
	dir := filepath.Join(reposDir, safeName)
	if _, err := os.Stat(dir); err == nil {
		log.Printf("[llm-contextbench] cleaning up %s", safeName)
		os.RemoveAll(dir)
	}
}

func runLLMContextBenchTask(claudeBin string, task ContextBenchTask, opts LLMContextBenchOptions) LLMContextBenchTaskResult {
	start := time.Now()
	result := LLMContextBenchTaskResult{
		InstanceID: task.InstanceID,
		Repo:       task.Repo,
		Language:   task.Language,
		Mode:       opts.Mode,
		ToolCalls:  make(map[string]int),
	}

	// 1. Parse gold context.
	goldBlocks, err := parseGoldContext(task.GoldContextRaw)
	if err != nil {
		result.Error = "parse gold: " + err.Error()
		result.Duration = time.Since(start).String()
		return result
	}
	if len(goldBlocks) == 0 {
		result.Error = "empty gold context"
		result.Duration = time.Since(start).String()
		return result
	}

	goldLines := make(map[string]bool)
	for _, b := range goldBlocks {
		for line := b.StartLine; line <= b.EndLine; line++ {
			goldLines[fmt.Sprintf("%s:%d", b.File, line)] = true
		}
	}
	result.GoldLines = len(goldLines)

	goldFileSet := make(map[string]bool)
	for _, b := range goldBlocks {
		goldFileSet[b.File] = true
	}
	result.GoldFiles = len(goldFileSet)

	// 2. Ensure repo is cloned and checked out.
	repoDir, err := ensureRepo(opts.ReposDir, task.Repo, task.BaseCommit)
	if err != nil {
		result.Error = "repo setup: " + err.Error()
		result.Duration = time.Since(start).String()
		return result
	}
	gitReset(repoDir)

	// Resolve symlinks so daemon path matches .mcp.json project= path.
	// On macOS /tmp → /private/tmp; mismatched paths break MCP connection.
	if real, err := filepath.EvalSymlinks(repoDir); err == nil {
		repoDir = real
	}

	// 3. Mode-specific setup.
	if opts.Mode == "synapses" {
		if err := setupLLMContextBenchSynapses(repoDir, opts.DaemonPort); err != nil {
			log.Printf("  WARNING: synapses setup failed: %v", err)
		}
	} else {
		// Baseline: clean any MCP artifacts.
		os.Remove(filepath.Join(repoDir, ".mcp.json"))
		os.Remove(filepath.Join(repoDir, "synapses.json"))
		os.RemoveAll(filepath.Join(repoDir, ".claude"))
	}

	// 4. Build prompt.
	prompt := buildLLMContextPrompt(task.ProblemStatement)

	// 5. Configure tools.
	allowedTools := "Read Grep Glob Bash"
	disallowedTools := ""
	systemPrompt := ""

	if opts.Mode == "synapses" {
		allowedTools = "Read Bash mcp__synapses__*"
		disallowedTools = "Grep,Glob"
		systemPrompt = llmContextSynapsesPrompt
	}

	// 6. Run Claude with MCP verification.
	// In synapses mode, verify MCP connected successfully. Retry up to 2 times
	// if it fails — the stdio MCP server may need more time on large repos.
	var streamJSON string
	maxAttempts := 1
	if opts.Mode == "synapses" {
		maxAttempts = 3
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var err error
		streamJSON, err = runLLMContextBenchClaude(claudeBin, repoDir, prompt, allowedTools, disallowedTools, opts.Model, systemPrompt, opts.Timeout, opts.Mode == "synapses")
		if err != nil {
			log.Printf("  Claude error (attempt %d): %v", attempt, err)
		}

		// Verify MCP connection in synapses mode.
		if opts.Mode == "synapses" {
			mcpOK := checkMCPConnected(streamJSON)
			if mcpOK {
				if attempt > 1 {
					log.Printf("  MCP connected on attempt %d", attempt)
				}
				break
			}
			if attempt < maxAttempts {
				log.Printf("  MCP connection failed (attempt %d/%d), retrying...", attempt, maxAttempts)
				time.Sleep(5 * time.Second) // give daemon time to settle
				continue
			}
			// All retries exhausted — mark as error.
			result.Error = "MCP connection failed after 3 attempts — synapses tools unavailable"
			result.Duration = time.Since(start).String()
			log.Printf("  MCP FAILED after %d attempts — skipping task", maxAttempts)
			resetToCommit(repoDir, task.BaseCommit)
			return result
		}
		break // baseline mode — no verification needed
	}

	// 7. Debug output.
	if opts.Debug && opts.OutputDir != "" {
		debugPath := filepath.Join(opts.OutputDir,
			fmt.Sprintf("llm_cb_%s_%s.json", opts.Mode, task.InstanceID))
		_ = os.WriteFile(debugPath, []byte(streamJSON), 0o644)
	}

	// 8. Parse stats.
	stats := parseStreamStats(streamJSON)
	result.ToolCalls = stats.ToolCalls
	result.Turns = stats.Turns
	result.InputTokens = stats.InputTokens
	result.OutputTokens = stats.OutputTokens
	result.CostEstimateUSD = estimateCost(opts.Model, stats.InputTokens, stats.OutputTokens)

	// 9. Extract Claude's context identification.
	ranges := parseLLMContextOutput(streamJSON)

	// 10. Build retrieved line set.
	retrievedLines := make(map[string]bool)
	for _, r := range ranges {
		for line := r.StartLine; line <= r.EndLine; line++ {
			retrievedLines[fmt.Sprintf("%s:%d", r.File, line)] = true
		}
	}
	result.RetrievedLines = len(retrievedLines)

	// 11. Compute line-level F1.
	result.Precision, result.Recall, result.F1, result.HitLines = computeContextF1(retrievedLines, goldLines)

	// 12. Compute file-level F1.
	retrievedFileSet := make(map[string]bool)
	for _, r := range ranges {
		retrievedFileSet[r.File] = true
	}
	result.RetrievedFiles = len(retrievedFileSet)

	fileHits := 0
	for f := range retrievedFileSet {
		if goldFileSet[f] {
			fileHits++
		}
	}
	result.HitFiles = fileHits
	if len(retrievedFileSet) > 0 {
		result.FilePrecision = float64(fileHits) / float64(len(retrievedFileSet))
	}
	if len(goldFileSet) > 0 {
		result.FileRecall = float64(fileHits) / float64(len(goldFileSet))
	}
	if result.FilePrecision+result.FileRecall > 0 {
		result.FileF1 = 2 * result.FilePrecision * result.FileRecall / (result.FilePrecision + result.FileRecall)
	}

	// 13. Cleanup.
	resetToCommit(repoDir, task.BaseCommit)

	result.Duration = time.Since(start).String()
	return result
}

// ─── Claude Runner ──────────────────────────────────────────────────────────

// runLLMContextBenchClaude runs claude -p with explicit MCP config if needed.
func runLLMContextBenchClaude(claudeBin, repoDir, prompt, allowedTools, disallowedTools, model, systemPrompt string, timeoutSecs int, withMCP bool) (string, error) {
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
	if withMCP {
		mcpConfig := filepath.Join(repoDir, ".mcp.json")
		if _, err := os.Stat(mcpConfig); err == nil {
			args = append(args, "--mcp-config", mcpConfig)
		}
	}

	cmd := exec.Command(claudeBin, args...)
	cmd.Dir = repoDir
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

	cmd.Stderr = os.Stderr

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
			return string(out), fmt.Errorf("claude exited: %w", err)
		}
		return string(out), nil
	case <-time.After(time.Duration(timeoutSecs) * time.Second):
		_ = cmd.Process.Kill()
		return string(out), fmt.Errorf("timeout after %ds", timeoutSecs)
	}
}

// ─── Synapses Setup ─────────────────────────────────────────────────────────

// setupLLMContextBenchSynapses writes .mcp.json + .claude/settings.json.
// Uses stdio transport: Claude Code spawns `synapses start --path <dir>` as a
// subprocess. This is the standard connection method and works across all
// Claude Code versions (no HTTP/SSE/streamableHttp compatibility issues).
func setupLLMContextBenchSynapses(repoDir, port string) error {
	synapsesBin := findSynapsesBin()

	// Pre-warm: run `synapses start` once to register the project and wait
	// for indexing. This ensures the daemon socket is ready before Claude starts.
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

	// .mcp.json → stdio subprocess
	mcpJSON := fmt.Sprintf(`{
  "mcpServers": {
    "synapses": {
      "type": "stdio",
      "command": %q,
      "args": ["start", "--path", %q]
    }
  }
}`, synapsesBin, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, ".mcp.json"), []byte(mcpJSON), 0o644); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}

	// .claude/settings.json — auto-approve all tools
	claudeDir := filepath.Join(repoDir, ".claude")
	os.MkdirAll(claudeDir, 0o755)
	settingsJSON := `{
  "permissions": {
    "allow": [
      "mcp__synapses__*",
      "Bash(*)",
      "Read(*)",
      "Grep(*)",
      "Glob(*)"
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}

	return nil
}

// ─── Prompt ─────────────────────────────────────────────────────────────────

func buildLLMContextPrompt(problemStatement string) string {
	return fmt.Sprintf(`You are analyzing a bug report for a codebase. Your job is to identify which files and specific line ranges contain code relevant to understanding and fixing this issue.

## Problem

%s

## Instructions

1. Explore the codebase to find ALL code relevant to this problem
2. Look for: the buggy code, related functions/classes, callers, test files, type definitions
3. Be thorough — check callers, callees, imports, and related test files
4. When done, output your findings in this EXACT format:

RELEVANT_CONTEXT_START
file: path/to/file.py
lines: 10-25
reason: Contains the function that handles X

file: path/to/other.py
lines: 100-150
reason: Test that verifies the behavior
RELEVANT_CONTEXT_END

IMPORTANT:
- Use paths relative to the repository root
- Every file entry MUST have a lines field with a range (e.g. lines: 42-60)
- Include 5-15 file entries — be thorough but focused
- Include test files if they test the relevant functionality
- The RELEVANT_CONTEXT_START/END markers are required`, problemStatement)
}

const llmContextSynapsesPrompt = `You have access to Synapses MCP tools that provide structural code intelligence. These tools have already indexed this entire codebase.

CRITICAL — START WITH THIS TOOL:

Call mcp__synapses__get_context with mode="investigate" and pass the problem description as the "problem" parameter. This single call returns ranked code blocks WITH actual source code, affected files, and test files. Example:

  mcp__synapses__get_context(mode="investigate", problem="<paste the problem statement>", target="<main entity>", max_blocks=10, include_tests=true)

This gives you everything in one call: relevant code blocks with source, affected file list, and test coverage.

THEN for any gaps:
- Use mcp__synapses__search to find additional symbols
- Use Read to examine specific files from the affected_files list
- Use mcp__synapses__get_impact for deeper blast radius analysis

The investigate mode combines search + dependency graph + impact analysis + source code reading into one response. Always start there.`

// ─── Output Parser ──────────────────────────────────────────────────────────

var (
	contextBlockFileRe  = regexp.MustCompile(`(?i)^file:\s*(.+)$`)
	contextBlockLinesRe = regexp.MustCompile(`(?i)^lines:\s*(\d+)\s*-\s*(\d+)$`)
	// Fallback: "path/to/file.ext:NN-MM" or "path/to/file.ext lines NN-MM"
	fallbackFileLineRe = regexp.MustCompile(`([\w/.\-]+\.\w{1,4})\s*(?::|lines\s+)(\d+)\s*-\s*(\d+)`)
)

// parseLLMContextOutput extracts file+line ranges from Claude's stream-json output.
// Looks for the RELEVANT_CONTEXT_START/END markers first, then falls back to
// scanning all text for file:line patterns.
func parseLLMContextOutput(streamJSON string) []retrievedRange {
	// Extract all assistant text from the stream.
	var allText strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(streamJSON))
	scanner.Buffer(make([]byte, 0), 2*1024*1024)

	for scanner.Scan() {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Type != "assistant" {
			continue
		}
		for _, c := range msg.Message.Content {
			if c.Type == "text" && c.Text != "" {
				allText.WriteString(c.Text)
				allText.WriteString("\n")
			}
		}
	}

	text := allText.String()

	// Strategy 1: Look for RELEVANT_CONTEXT_START/END markers.
	if ranges := parseContextMarkers(text); len(ranges) > 0 {
		return ranges
	}

	// Strategy 2: Fallback — scan all text for file:line-range patterns.
	return parseContextFallback(text)
}

func parseContextMarkers(text string) []retrievedRange {
	startIdx := strings.Index(text, "RELEVANT_CONTEXT_START")
	if startIdx < 0 {
		return nil
	}
	endIdx := strings.Index(text[startIdx:], "RELEVANT_CONTEXT_END")
	if endIdx < 0 {
		endIdx = len(text) - startIdx
	}
	block := text[startIdx : startIdx+endIdx]

	var ranges []retrievedRange
	var currentFile string

	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)

		if m := contextBlockFileRe.FindStringSubmatch(line); len(m) == 2 {
			currentFile = strings.TrimSpace(m[1])
			continue
		}

		if m := contextBlockLinesRe.FindStringSubmatch(line); len(m) == 3 && currentFile != "" {
			start, _ := strconv.Atoi(m[1])
			end, _ := strconv.Atoi(m[2])
			if start > 0 && end >= start && end-start < 500 {
				ranges = append(ranges, retrievedRange{
					File:      currentFile,
					StartLine: start,
					EndLine:   end,
				})
			}
		}
	}

	return ranges
}

func parseContextFallback(text string) []retrievedRange {
	var ranges []retrievedRange
	seen := make(map[string]bool) // deduplicate

	for _, m := range fallbackFileLineRe.FindAllStringSubmatch(text, -1) {
		if len(m) == 4 {
			file := m[1]
			start, _ := strconv.Atoi(m[2])
			end, _ := strconv.Atoi(m[3])
			if start > 0 && end >= start && end-start < 500 {
				key := fmt.Sprintf("%s:%d-%d", file, start, end)
				if !seen[key] {
					seen[key] = true
					ranges = append(ranges, retrievedRange{
						File:      file,
						StartLine: start,
						EndLine:   end,
					})
				}
			}
		}
	}

	return ranges
}

// ─── Scoring ────────────────────────────────────────────────────────────────

// computeContextF1 computes precision, recall, F1, and hit count between
// retrieved and gold line sets. Shared by heuristic and LLM contextbench.
func computeContextF1(retrieved, gold map[string]bool) (precision, recall, f1 float64, hits int) {
	for line := range retrieved {
		if gold[line] {
			hits++
		}
	}
	if len(retrieved) > 0 {
		precision = float64(hits) / float64(len(retrieved))
	}
	if len(gold) > 0 {
		recall = float64(hits) / float64(len(gold))
	}
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	return
}

// ─── Report Builder ─────────────────────────────────────────────────────────

func buildLLMContextBenchReport(mode, model string, results []LLMContextBenchTaskResult) *reporter.LLMContextBenchResult {
	r := &reporter.LLMContextBenchResult{
		Timestamp: reporter.Timestamp(),
		Mode:      mode,
		Model:     model,
		TotalTasks: len(results),
		ToolUsage: make(map[string]int),
	}

	var totalP, totalR, totalF1 float64
	var totalFP, totalFR, totalFF1 float64
	var totalTurns int
	var totalCost float64

	for _, tr := range results {
		totalP += tr.Precision
		totalR += tr.Recall
		totalF1 += tr.F1
		totalFP += tr.FilePrecision
		totalFR += tr.FileRecall
		totalFF1 += tr.FileF1
		totalTurns += tr.Turns
		totalCost += tr.CostEstimateUSD

		for name, count := range tr.ToolCalls {
			r.ToolUsage[name] += count
		}
		r.Tasks = append(r.Tasks, tr)
	}

	n := float64(len(results))
	if n > 0 {
		r.AvgPrecision = totalP / n
		r.AvgRecall = totalR / n
		r.AvgF1 = totalF1 / n
		r.AvgFilePrecision = totalFP / n
		r.AvgFileRecall = totalFR / n
		r.AvgFileF1 = totalFF1 / n
		r.AvgTurns = float64(totalTurns) / n
		r.AvgCostUSD = totalCost / n
		r.TotalCostUSD = totalCost
	}

	// Per-language breakdown.
	type langAcc struct {
		p, r, f1, fp, fr, ff1 float64
		n                     int
	}
	langMetrics := make(map[string]*langAcc)
	for _, tr := range results {
		lm := langMetrics[tr.Language]
		if lm == nil {
			lm = &langAcc{}
			langMetrics[tr.Language] = lm
		}
		lm.p += tr.Precision
		lm.r += tr.Recall
		lm.f1 += tr.F1
		lm.fp += tr.FilePrecision
		lm.fr += tr.FileRecall
		lm.ff1 += tr.FileF1
		lm.n++
	}
	for lang, lm := range langMetrics {
		r.PerLanguage = append(r.PerLanguage, reporter.ContextBenchLangResult{
			Language:     lang,
			Tasks:        lm.n,
			AvgPrecision: lm.ff1 / float64(lm.n), // repurpose: file F1 in the lang summary
			AvgRecall:    lm.r / float64(lm.n),
			AvgF1:        lm.f1 / float64(lm.n),
		})
	}

	return r
}
