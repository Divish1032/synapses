// compactionbench.go implements a two-phase benchmark that measures whether
// Synapses compaction recovery helps agents resume work after context loss.
//
// Phase 1: Agent explores codebase + plans (with Synapses MCP tools).
// [COMPACTION EVENT — new Claude session, Synapses state persists]
// Phase 2: Agent resumes and implements the solution.
//
// Three modes:
//   - baseline:       Phase 2 starts cold (only problem statement)
//   - synapses:       Phase 2 gets compaction_recovery injected from Synapses
//   - synapses+guide: Phase 2 gets compaction_recovery + compaction_guide
//
// Uses `claude -p` (Max subscription, no API key required).
// Reuses FeatureBench tasks as the workload.
package benchmarks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// CompactionBenchOptions configures the CompactionBench run.
type CompactionBenchOptions struct {
	Split       string   // HF split: "lite", "fast", "full"
	TaskIDs     []string // optional: specific task IDs
	Level       int      // 0 = all, 1 or 2
	ReposDir    string   // where repos are cloned
	Limit       int      // max tasks (0 = all)
	Mode        string   // "baseline", "synapses", or "synapses+guide"
	Model       string   // Claude model
	P1Timeout   int      // Phase 1 timeout in seconds (default 300 = 5min)
	P2Timeout   int      // Phase 2 timeout in seconds (default 900 = 15min)
	SynapsesBin string   // path to synapses binary
	OutputDir   string   // where to write results
	Debug       bool     // dump raw stream-json
	DaemonPort  string   // daemon HTTP port (default "11436")
}

// CompactionBenchTaskResult is the outcome of one two-phase task.
type CompactionBenchTaskResult struct {
	InstanceID string `json:"instance_id"`
	Repo       string `json:"repo"`
	Mode       string `json:"mode"`

	// Phase 1 metrics
	P1ToolCalls map[string]int `json:"p1_tool_calls,omitempty"`
	P1Turns     int            `json:"p1_turns"`
	P1Duration  string         `json:"p1_duration"`

	// Phase 2 metrics
	P2ToolCalls    map[string]int `json:"p2_tool_calls,omitempty"`
	P2Turns        int            `json:"p2_turns"`
	P2Duration     string         `json:"p2_duration"`
	P2TurnsToEdit  int            `json:"p2_turns_to_edit"` // turns before first Edit/Write
	P2SearchCalls  int            `json:"p2_search_calls"`  // re-exploration signal

	// Outcome
	ModelPatch     string `json:"model_patch"`
	PatchGenerated bool   `json:"patch_generated"`

	// Compaction context
	RecoveryTokens int `json:"recovery_tokens,omitempty"` // approx tokens in injected recovery

	Error string `json:"error,omitempty"`
}

// RunCompactionBench runs the two-phase compaction benchmark.
func RunCompactionBench(opts CompactionBenchOptions) ([]CompactionBenchTaskResult, error) {
	if opts.P1Timeout <= 0 {
		opts.P1Timeout = 300 // 5 minutes
	}
	if opts.P2Timeout <= 0 {
		opts.P2Timeout = 900 // 15 minutes
	}
	if opts.DaemonPort == "" {
		opts.DaemonPort = "11435"
	}

	claudeBin, err := findClaudeBin()
	if err != nil {
		return nil, fmt.Errorf("claude CLI not found: %w", err)
	}
	if opts.SynapsesBin == "" {
		opts.SynapsesBin = findSynapsesBin()
	}

	// Load FeatureBench tasks as workload.
	tasks, err := loadFeatureBenchData(FeatureBenchOptions{
		Split:    opts.Split,
		TaskIDs:  opts.TaskIDs,
		Level:    opts.Level,
		ReposDir: opts.ReposDir,
	})
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}
	if opts.Limit > 0 && opts.Limit < len(tasks) {
		tasks = tasks[:opts.Limit]
	}

	log.Printf("CompactionBench: running %d tasks in %s mode (model=%s, p1=%ds, p2=%ds)",
		len(tasks), opts.Mode, opts.Model, opts.P1Timeout, opts.P2Timeout)

	var results []CompactionBenchTaskResult
	for i, task := range tasks {
		log.Printf("[%d/%d] %s", i+1, len(tasks), task.InstanceID)
		result := runCompactionTask(claudeBin, task, opts)
		results = append(results, result)
		log.Printf("  P1: %d turns, %s | P2: %d turns (edit@%d), %s | patch=%v",
			result.P1Turns, result.P1Duration,
			result.P2Turns, result.P2TurnsToEdit, result.P2Duration,
			result.PatchGenerated)
	}

	return results, nil
}

func runCompactionTask(claudeBin string, task FeatureBenchTask, opts CompactionBenchOptions) CompactionBenchTaskResult {
	result := CompactionBenchTaskResult{
		InstanceID:  task.InstanceID,
		Repo:        task.Repo,
		Mode:        opts.Mode,
		P1ToolCalls: make(map[string]int),
		P2ToolCalls: make(map[string]int),
	}

	// 1. Repo setup (same as FeatureBench).
	repoDir, err := ensureRepo(opts.ReposDir, task.Repo, task.BaseCommit)
	if err != nil {
		result.Error = fmt.Sprintf("repo setup: %v", err)
		return result
	}
	gitReset(repoDir)

	// Apply corruption patches.
	if task.Patch != "" {
		if err := gitApplyPatch(repoDir, task.Patch); err != nil {
			result.Error = fmt.Sprintf("apply patch: %v", err)
			return result
		}
	}
	if task.TestPatch != "" {
		if err := gitApplyPatch(repoDir, task.TestPatch); err != nil {
			result.Error = fmt.Sprintf("apply test_patch: %v", err)
			return result
		}
	}
	if err := gitCommitAll(repoDir, "corruption baseline"); err != nil {
		result.Error = fmt.Sprintf("commit corruption: %v", err)
		return result
	}

	// 2. Setup Synapses (for all modes — Phase 1 always uses MCP).
	// Try Docker first; fall back to local daemon on port DaemonPort.
	if err := setupSynapses(opts.SynapsesBin, repoDir); err != nil {
		log.Printf("  Docker setup failed: %v — trying local daemon", err)
		// Fall back: write .mcp.json pointing to local daemon.
		if setupErr := setupSynapsesLocal(repoDir, opts.DaemonPort); setupErr != nil {
			log.Printf("  WARNING: local synapses setup also failed: %v (continuing without)", setupErr)
		}
	}

	// ── PHASE 1: Explore and Plan ────────────────────────────────────────

	p1Start := time.Now()
	p1Prompt := fmt.Sprintf(`You are working on this task:
%s

Phase 1: EXPLORE AND PLAN ONLY. Do NOT make any code changes yet.
1. Search the codebase to understand the relevant files and entities
2. Identify the root cause or implementation approach
3. Use remember() to save your key decisions and findings
4. Use save_session_state() to checkpoint your approach, files identified, and remaining steps

When done exploring, output "PHASE 1 COMPLETE" and stop.`, task.ProblemStatement)

	p1Tools := "Bash Read mcp__synapses__*"
	p1System := `You are in Phase 1 of a two-phase task. EXPLORE ONLY — do NOT edit files.
Use Synapses MCP tools to search and understand the codebase structure.
Save your findings via remember() and save_session_state() so they persist.`

	p1Stream, err := runClaudeCode(claudeBin, repoDir, p1Prompt, p1Tools, "Write,Edit", opts.Model, p1System, opts.P1Timeout)
	if err != nil {
		log.Printf("  Phase 1 error: %v", err)
	}
	result.P1ToolCalls, result.P1Turns = parseStreamStats(p1Stream)
	result.P1Duration = time.Since(p1Start).Truncate(time.Second).String()

	if opts.Debug && opts.OutputDir != "" {
		debugPath := filepath.Join(opts.OutputDir, fmt.Sprintf("stream_p1_%s_%s.json", opts.Mode, task.InstanceID))
		_ = os.WriteFile(debugPath, []byte(p1Stream), 0o644)
	}

	// ── COMPACTION EVENT ─────────────────────────────────────────────────
	// Claude process exited (context lost). Synapses daemon still running.
	// No action needed — the session kill IS the compaction simulation.

	// ── PHASE 2: Resume and Implement ────────────────────────────────────

	// Pre-fetch compaction recovery from Synapses (for synapses/synapses+guide modes).
	var recoveryContext, guideContext string
	if opts.Mode == "synapses" || opts.Mode == "synapses+guide" {
		recoveryContext = fetchCompactionRecovery(repoDir, opts.DaemonPort)
		result.RecoveryTokens = len(recoveryContext) * 2 / 7 // rough token estimate
	}
	if opts.Mode == "synapses+guide" {
		guideContext = fetchCompactionGuide(repoDir, opts.DaemonPort)
	}

	// Build Phase 2 prompt based on mode.
	p2Prompt := buildPhase2Prompt(task.ProblemStatement, opts.Mode, recoveryContext, guideContext)

	p2Tools := "Bash Read Write Edit Grep Glob"
	p2DisallowedTools := ""
	p2System := ""

	if opts.Mode == "synapses" || opts.Mode == "synapses+guide" {
		p2Tools += " mcp__synapses__*"
		p2DisallowedTools = "Grep,Glob"
		p2System = synapsesSystemPrompt
	}

	p2Start := time.Now()
	p2Stream, err := runClaudeCode(claudeBin, repoDir, p2Prompt, p2Tools, p2DisallowedTools, opts.Model, p2System, opts.P2Timeout)
	if err != nil {
		log.Printf("  Phase 2 error: %v", err)
	}
	result.P2ToolCalls, result.P2Turns = parseStreamStats(p2Stream)
	result.P2Duration = time.Since(p2Start).Truncate(time.Second).String()
	result.P2TurnsToEdit = turnsToFirstEdit(p2Stream)
	result.P2SearchCalls = countSearchCalls(p2Stream)

	if opts.Debug && opts.OutputDir != "" {
		debugPath := filepath.Join(opts.OutputDir, fmt.Sprintf("stream_p2_%s_%s.json", opts.Mode, task.InstanceID))
		_ = os.WriteFile(debugPath, []byte(p2Stream), 0o644)
	}

	// Extract patch.
	result.ModelPatch, _ = gitDiffExcludeBenchFiles(repoDir)
	result.PatchGenerated = result.ModelPatch != ""

	// Cleanup.
	gitReset(repoDir)
	os.Remove(filepath.Join(repoDir, ".mcp.json"))
	os.RemoveAll(filepath.Join(repoDir, ".claude"))
	os.Remove(filepath.Join(repoDir, "synapses.json"))

	return result
}

// setupSynapsesLocal writes .mcp.json + synapses.json + .claude/ for a local daemon
// (no Docker). Used as fallback when Docker is unavailable.
func setupSynapsesLocal(repoDir, port string) error {
	if port == "" {
		port = "11435"
	}

	// .mcp.json → local daemon
	mcpJSON := fmt.Sprintf(`{
  "mcpServers": {
    "synapses": {
      "type": "http",
      "url": "http://127.0.0.1:%s/mcp?project=%s"
    }
  }
}`, port, url.QueryEscape(repoDir))
	if err := os.WriteFile(filepath.Join(repoDir, ".mcp.json"), []byte(mcpJSON), 0o644); err != nil {
		return fmt.Errorf("write .mcp.json: %w", err)
	}

	// synapses.json
	synJSON := `{"version": "1.0", "project": {"name": "compbench-target"}}`
	if err := os.WriteFile(filepath.Join(repoDir, "synapses.json"), []byte(synJSON), 0o644); err != nil {
		return fmt.Errorf("write synapses.json: %w", err)
	}

	// .claude/settings.json
	claudeDir := filepath.Join(repoDir, ".claude")
	os.MkdirAll(claudeDir, 0o755)
	settingsJSON := `{"permissions": {"allow": ["mcp__synapses__*", "Bash(*)", "Read(*)", "Write(*)", "Edit(*)"]}}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settingsJSON), 0o644); err != nil {
		return fmt.Errorf("write settings.json: %w", err)
	}

	// Pre-warm: trigger indexing
	warmURL := fmt.Sprintf("http://127.0.0.1:%s/v1/tools/search?project=%s",
		port, url.QueryEscape(repoDir))
	body := strings.NewReader(`{"query": "main"}`)
	resp, err := http.Post(warmURL, "application/json", body)
	if err != nil {
		return fmt.Errorf("pre-warm failed: %w", err)
	}
	resp.Body.Close()

	// Wait for indexing (poll for up to 60s)
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		resp, err := http.Post(warmURL, "application/json", strings.NewReader(`{"query": "test"}`))
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 && len(data) > 50 {
			log.Printf("  Local daemon indexed (%d bytes)", len(data))
			return nil
		}
	}
	log.Printf("  WARNING: pre-warm timed out, continuing anyway")
	return nil
}

// buildPhase2Prompt constructs the Phase 2 prompt based on mode.
func buildPhase2Prompt(problemStatement, mode, recoveryContext, guideContext string) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "You are working on this task:\n%s\n\n", problemStatement)

	switch mode {
	case "baseline":
		sb.WriteString("A previous session explored this codebase but was interrupted. ")
		sb.WriteString("You have no information about what was found. Start fresh.\n")
		sb.WriteString("Implement the solution.\n")

	case "synapses":
		sb.WriteString("A previous session explored this codebase but was interrupted (context compacted).\n")
		sb.WriteString("Your prior work context has been recovered from Synapses:\n\n")
		if recoveryContext != "" {
			sb.WriteString("## Compaction Recovery (from Synapses)\n")
			sb.WriteString(recoveryContext)
			sb.WriteString("\n\n")
		}
		sb.WriteString("Use this recovered context to resume work efficiently. ")
		sb.WriteString("You can also call session_init(scope=\"compaction\") for the latest state.\n")
		sb.WriteString("Implement the solution.\n")

	case "synapses+guide":
		sb.WriteString("A previous session explored this codebase but was interrupted (context compacted).\n")
		sb.WriteString("Your prior work context has been recovered from Synapses:\n\n")
		if recoveryContext != "" {
			sb.WriteString("## Compaction Recovery (from Synapses)\n")
			sb.WriteString(recoveryContext)
			sb.WriteString("\n\n")
		}
		if guideContext != "" {
			sb.WriteString("## Compaction Guide (from Synapses)\n")
			sb.WriteString(guideContext)
			sb.WriteString("\n\n")
		}
		sb.WriteString("Use this recovered context to resume work efficiently. Implement the solution.\n")
	}

	return sb.String()
}

// fetchCompactionRecovery calls session_init(scope="compaction") via the Synapses REST API.
func fetchCompactionRecovery(repoDir, port string) string {
	toolURL := fmt.Sprintf("http://127.0.0.1:%s/v1/tools/session_init?project=%s",
		port, url.QueryEscape(repoDir))
	body := strings.NewReader(`{"agent_id":"compbench","scope":"compaction"}`)
	resp, err := http.Post(toolURL, "application/json", body)
	if err != nil {
		log.Printf("  WARNING: compaction recovery fetch failed: %v", err)
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("  WARNING: compaction recovery returned %d", resp.StatusCode)
		return ""
	}

	// Extract the compaction_recovery field from the JSON response.
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return string(data) // return raw if unparseable
	}
	if recovery, ok := result["compaction_recovery"]; ok {
		recoveryJSON, _ := json.MarshalIndent(recovery, "", "  ")
		return string(recoveryJSON)
	}
	return string(data)
}

// fetchCompactionGuide calls get_compaction_guide via the Synapses REST API.
func fetchCompactionGuide(repoDir, port string) string {
	toolURL := fmt.Sprintf("http://127.0.0.1:%s/v1/tools/get_compaction_guide?project=%s",
		port, url.QueryEscape(repoDir))
	body := strings.NewReader(`{"agent_id":"compbench"}`)
	resp, err := http.Post(toolURL, "application/json", body)
	if err != nil {
		log.Printf("  WARNING: compaction guide fetch failed: %v", err)
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("  WARNING: compaction guide returned %d", resp.StatusCode)
		return ""
	}
	return string(data)
}

// turnsToFirstEdit counts how many assistant turns occur before the first Edit or Write tool call.
// Returns -1 if no edit was made.
func turnsToFirstEdit(streamJSON string) int {
	scanner := bufio.NewScanner(strings.NewReader(streamJSON))
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, len(buf))

	turnCount := 0
	for scanner.Scan() {
		line := scanner.Text()
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Type != "assistant" {
			continue
		}
		turnCount++
		for _, c := range msg.Message.Content {
			if c.Type == "tool_use" && (c.Name == "Edit" || c.Name == "Write") {
				return turnCount
			}
		}
	}
	return -1 // no edit made
}

// countSearchCalls counts search/exploration tool calls in Phase 2.
func countSearchCalls(streamJSON string) int {
	scanner := bufio.NewScanner(strings.NewReader(streamJSON))
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, len(buf))

	count := 0
	searchTools := map[string]bool{
		"Grep": true, "Glob": true, "Read": true,
		"mcp__synapses__search":      true,
		"mcp__synapses__get_context": true,
		"mcp__synapses__get_impact":  true,
	}
	for scanner.Scan() {
		line := scanner.Text()
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Type != "assistant" {
			continue
		}
		for _, c := range msg.Message.Content {
			if c.Type == "tool_use" && searchTools[c.Name] {
				count++
			}
		}
	}
	return count
}

// BuildCompactionBenchReport aggregates results into a report.
func BuildCompactionBenchReport(mode, model string, results []CompactionBenchTaskResult) *reporter.CompactionBenchReport {
	report := &reporter.CompactionBenchReport{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Mode:       mode,
		Model:      model,
		TotalTasks: len(results),
	}

	var totalP2Turns, totalP2TurnsToEdit, totalP2Search int
	editCount := 0
	for _, r := range results {
		if r.PatchGenerated {
			report.PatchCount++
		}
		totalP2Turns += r.P2Turns
		if r.P2TurnsToEdit > 0 {
			totalP2TurnsToEdit += r.P2TurnsToEdit
			editCount++
		}
		totalP2Search += r.P2SearchCalls

		// Aggregate tool usage
		if report.P2ToolUsage == nil {
			report.P2ToolUsage = make(map[string]int)
		}
		for tool, count := range r.P2ToolCalls {
			report.P2ToolUsage[tool] += count
		}

		report.Tasks = append(report.Tasks, r)
	}

	if len(results) > 0 {
		report.PatchRate = float64(report.PatchCount) / float64(len(results)) * 100
		report.AvgP2Turns = float64(totalP2Turns) / float64(len(results))
		report.AvgP2Search = float64(totalP2Search) / float64(len(results))
	}
	if editCount > 0 {
		report.AvgTurnsToEdit = float64(totalP2TurnsToEdit) / float64(editCount)
	}

	return report
}

