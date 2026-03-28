// claude_agent.go implements a minimal agent loop that calls the Anthropic
// Messages API with tool_use. It supports two modes: baseline (file-level tools
// only) and synapses (file tools + Synapses MCP tools). Used by the SWE-bench
// benchmark to measure the Pass@1 delta that Synapses provides.
package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// AgentConfig holds Claude API parameters.
type AgentConfig struct {
	APIKey      string  // Anthropic API key (env ANTHROPIC_API_KEY)
	Model       string  // e.g. "claude-sonnet-4-6"
	MaxTurns    int     // max agent loop iterations (default 25)
	MaxTokens   int     // max tokens per response (default 4096)
	Temperature float64 // 0.0 for deterministic
}

// DefaultAgentConfig returns sensible defaults. APIKey is read from env.
func DefaultAgentConfig() AgentConfig {
	return AgentConfig{
		APIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		Model:       "claude-sonnet-4-6",
		MaxTurns:    25,
		MaxTokens:   4096,
		Temperature: 0.0,
	}
}

// AgentMode selects the tool set.
type AgentMode string

const (
	ModeBaseline AgentMode = "baseline"
	ModeSynapses AgentMode = "synapses"
)

// AgentStats tracks resource usage for a single task.
type AgentStats struct {
	TotalTurns   int            `json:"total_turns"`
	ToolCalls    map[string]int `json:"tool_calls"`    // tool_name → count
	InputTokens  int            `json:"input_tokens"`  // cumulative
	OutputTokens int            `json:"output_tokens"` // cumulative
	SynapsesUsed bool           `json:"synapses_used"` // true if any synapses tool was called
	Duration     time.Duration  `json:"duration"`
}

// AgentResult is the outcome of running the agent on a single task.
type AgentResult struct {
	Patch string     `json:"patch"`
	Stats AgentStats `json:"stats"`
	Error string     `json:"error,omitempty"`
}

// ── Claude API types ─────────────────────────────────────────────────────────

type claudeRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
	System      string          `json:"system,omitempty"`
	Tools       []ToolDef       `json:"tools,omitempty"`
	Messages    []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`    // tool_use block
	Name  string          `json:"name,omitempty"`  // tool_use block
	Input json.RawMessage `json:"input,omitempty"` // tool_use block
	// tool_result fields
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"` // tool_result text
}

type claudeResponse struct {
	ID         string         `json:"id"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// synapsesToolNames lists tool names that count as "synapses" tools for stats.
var synapsesToolNames = map[string]bool{
	"synapses_search":          true,
	"synapses_get_context":     true,
	"synapses_get_impact":      true,
	"synapses_prepare_context": true,
}

// RunAgent executes the agent loop for one task.
// It sends the system+task prompt to Claude, handles tool_use responses,
// and returns the final patch (extracted via patchExtractor callback).
func RunAgent(cfg AgentConfig, systemPrompt, taskPrompt string,
	tools []ToolDef, executor ToolExecutor, patchExtractor func() (string, error)) (*AgentResult, error) {

	if cfg.APIKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	start := time.Now()
	stats := AgentStats{ToolCalls: make(map[string]int)}

	// Conversation history.
	messages := []claudeMessage{
		{Role: "user", Content: []contentBlock{{Type: "text", Text: taskPrompt}}},
	}

	httpClient := &http.Client{Timeout: 120 * time.Second}
	noToolTurns := 0

	for turn := 0; turn < cfg.MaxTurns; turn++ {
		stats.TotalTurns = turn + 1

		// Build request.
		reqBody := claudeRequest{
			Model:       cfg.Model,
			MaxTokens:   cfg.MaxTokens,
			Temperature: cfg.Temperature,
			System:      systemPrompt,
			Tools:       tools,
			Messages:    messages,
		}

		resp, err := callClaude(httpClient, cfg.APIKey, &reqBody)
		if err != nil {
			return &AgentResult{Stats: stats, Error: fmt.Sprintf("turn %d: %v", turn, err)}, nil
		}

		stats.InputTokens += resp.Usage.InputTokens
		stats.OutputTokens += resp.Usage.OutputTokens

		// Append assistant message.
		messages = append(messages, claudeMessage{Role: "assistant", Content: resp.Content})

		// Check for tool_use blocks.
		var toolUses []contentBlock
		for _, block := range resp.Content {
			if block.Type == "tool_use" {
				toolUses = append(toolUses, block)
			}
		}

		if len(toolUses) == 0 {
			noToolTurns++
		} else {
			noToolTurns = 0
		}

		// If stop_reason is end_turn or no tool calls for 3 turns, extract patch.
		if resp.StopReason == "end_turn" || noToolTurns >= 3 {
			patch, pErr := patchExtractor()
			if pErr != nil {
				return &AgentResult{Stats: stats, Error: fmt.Sprintf("patch extract: %v", pErr)}, nil
			}
			stats.Duration = time.Since(start)
			return &AgentResult{Patch: patch, Stats: stats}, nil
		}

		// Execute tool calls and build tool_result message.
		var toolResults []contentBlock
		for _, tu := range toolUses {
			stats.ToolCalls[tu.Name]++
			if synapsesToolNames[tu.Name] {
				stats.SynapsesUsed = true
			}

			result, execErr := executor.Execute(tu.Name, tu.Input)
			if execErr != nil {
				result = fmt.Sprintf("Error: %v", execErr)
			}

			// Truncate large outputs.
			if len(result) > 8000 {
				result = result[:8000] + "\n... [truncated]"
			}

			toolResults = append(toolResults, contentBlock{
				Type:      "tool_result",
				ToolUseID: tu.ID,
				Content:   result,
			})
		}

		messages = append(messages, claudeMessage{Role: "user", Content: toolResults})
	}

	// Exhausted max turns — still try to extract patch.
	patch, _ := patchExtractor()
	stats.Duration = time.Since(start)
	return &AgentResult{Patch: patch, Stats: stats, Error: "max turns exhausted"}, nil
}

// callClaude sends a single request to the Anthropic Messages API.
func callClaude(client *http.Client, apiKey string, reqBody *claudeRequest) (*claudeResponse, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	// Retry loop for rate limiting.
	for attempt := 0; attempt < 5; attempt++ {
		req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("http: %w", err)
		}
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}

		if resp.StatusCode == 429 || resp.StatusCode == 529 {
			wait := time.Duration(1<<uint(attempt)) * 2 * time.Second
			time.Sleep(wait)
			continue
		}

		var result claudeResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return nil, fmt.Errorf("unmarshal (status %d): %s", resp.StatusCode, string(respBody))
		}

		if result.Error != nil {
			return nil, fmt.Errorf("api error: %s: %s", result.Error.Type, result.Error.Message)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
		}

		return &result, nil
	}
	return nil, fmt.Errorf("rate limited after 5 retries")
}
