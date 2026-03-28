// tools.go defines tool schemas and executors for the SWE-bench agent.
// Baseline tools operate on a checked-out git repo (read/grep/list/write).
// Synapses tools extend baseline with Synapses MCP tools (search, context, impact).
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ToolDef matches the Claude API tool schema.
type ToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// ToolExecutor executes a tool by name and returns the text result.
type ToolExecutor interface {
	Execute(toolName string, input json.RawMessage) (string, error)
}

// ── Baseline executor ────────────────────────────────────────────────────────

// BaselineExecutor provides file-level tools on a checked-out repo.
type BaselineExecutor struct {
	RepoDir string
}

// Execute dispatches baseline tool calls (read_file, grep_search, etc.).
func (e *BaselineExecutor) Execute(toolName string, input json.RawMessage) (string, error) {
	switch toolName {
	case "read_file":
		return e.readFile(input)
	case "grep_search":
		return e.grepSearch(input)
	case "list_directory":
		return e.listDirectory(input)
	case "write_file":
		return e.writeFile(input)
	default:
		return "", fmt.Errorf("unknown tool: %s", toolName)
	}
}

func (e *BaselineExecutor) readFile(input json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	abs := e.safePath(args.Path)
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", args.Path, err)
	}
	return string(data), nil
}

func (e *BaselineExecutor) grepSearch(input json.RawMessage) (string, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	searchDir := e.RepoDir
	if args.Path != "" {
		searchDir = e.safePath(args.Path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "grep", "-rn", "--include=*.py", "--include=*.java",
		"--include=*.go", "--include=*.ts", "--include=*.js", "--include=*.rs", "--include=*.rb",
		"-m", "50", args.Pattern, searchDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// grep returns 1 when no matches — not an error.
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
			return "No matches found.", nil
		}
		return string(out), nil
	}
	return string(out), nil
}

func (e *BaselineExecutor) listDirectory(input json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	dir := e.safePath(args.Path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", args.Path, err)
	}
	var sb strings.Builder
	for _, entry := range entries {
		if entry.IsDir() {
			sb.WriteString(entry.Name() + "/\n")
		} else {
			sb.WriteString(entry.Name() + "\n")
		}
	}
	return sb.String(), nil
}

func (e *BaselineExecutor) writeFile(input json.RawMessage) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	abs := e.safePath(args.Path)
	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", args.Path, err)
	}
	return fmt.Sprintf("Successfully wrote %s", args.Path), nil
}

// safePath resolves a relative path within the repo dir, preventing escapes.
func (e *BaselineExecutor) safePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(e.RepoDir, p)
}

// ── Synapses executor ────────────────────────────────────────────────────────

// SynapsesExecutor extends BaselineExecutor with Synapses MCP tools.
type SynapsesExecutor struct {
	BaselineExecutor
	Client *SynapsesClient
	TaskID string
}

// Execute dispatches Synapses-augmented tool calls, falling back to baseline tools.
func (e *SynapsesExecutor) Execute(toolName string, input json.RawMessage) (string, error) {
	switch toolName {
	case "synapses_search":
		return e.synapsesSearch(input)
	case "synapses_get_context":
		return e.synapsesGetContext(input)
	case "synapses_get_impact":
		return e.synapsesGetImpact(input)
	case "synapses_prepare_context":
		return e.synapsesPrepareContext(input)
	default:
		// Fall through to baseline tools.
		return e.BaselineExecutor.Execute(toolName, input)
	}
}

func (e *SynapsesExecutor) synapsesSearch(input json.RawMessage) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	res, err := e.Client.Search(e.TaskID, args.Query)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func (e *SynapsesExecutor) synapsesGetContext(input json.RawMessage) (string, error) {
	var args struct {
		Entity string `json:"entity"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	return e.Client.GetContextJSON(e.TaskID, args.Entity, "full")
}

func (e *SynapsesExecutor) synapsesGetImpact(input json.RawMessage) (string, error) {
	var args struct {
		Entity string `json:"entity"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	res, err := e.Client.GetImpact(e.TaskID, args.Entity)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

func (e *SynapsesExecutor) synapsesPrepareContext(input json.RawMessage) (string, error) {
	var args struct {
		Entity string `json:"entity"`
		Intent string `json:"intent"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	res, err := e.Client.PrepareContext(e.TaskID, args.Entity, args.Intent)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// ── Tool definitions ─────────────────────────────────────────────────────────

// BaselineTools returns tool definitions for the baseline agent mode.
func BaselineTools() []ToolDef {
	return []ToolDef{
		{
			Name:        "read_file",
			Description: "Read the contents of a file at the given path relative to the repository root.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path relative to the repository root",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "grep_search",
			Description: "Search for a pattern in the repository using grep. Returns matching lines with file paths and line numbers.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "Search pattern (basic regex)",
					},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Optional subdirectory to search in (default: entire repo)",
					},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "list_directory",
			Description: "List the files and subdirectories in a directory.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Directory path relative to the repository root (default: root)",
					},
				},
				"required": []string{},
			},
		},
		{
			Name:        "write_file",
			Description: "Write content to a file. Creates the file if it doesn't exist, or overwrites it.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "File path relative to the repository root",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "Content to write to the file",
					},
				},
				"required": []string{"path", "content"},
			},
		},
	}
}

// SynapsesTools returns baseline + Synapses MCP tool definitions.
func SynapsesTools() []ToolDef {
	tools := BaselineTools()
	return append(tools,
		ToolDef{
			Name:        "synapses_search",
			Description: "Search the codebase using Synapses semantic search. Returns relevant code entities (functions, classes, methods) matching the query with file locations.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Natural language or code search query",
					},
				},
				"required": []string{"query"},
			},
		},
		ToolDef{
			Name:        "synapses_get_context",
			Description: "Get structural code context for an entity — its callers, callees, related functions, and cross-domain connections. Use this to understand how a function/class fits into the codebase.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"entity": map[string]interface{}{
						"type":        "string",
						"description": "Entity name (function, class, or method name)",
					},
				},
				"required": []string{"entity"},
			},
		},
		ToolDef{
			Name:        "synapses_get_impact",
			Description: "Get blast-radius analysis for an entity — what other parts of the codebase would be affected if this entity changes. Returns tiered impact (direct callers, transitive callers, related tests).",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"entity": map[string]interface{}{
						"type":        "string",
						"description": "Entity name to analyze impact for",
					},
				},
				"required": []string{"entity"},
			},
		},
		ToolDef{
			Name:        "synapses_prepare_context",
			Description: "Get curated context for an entity and a specific intent. Returns focused context including relevant code snippets, documentation, and architectural patterns.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"entity": map[string]interface{}{
						"type":        "string",
						"description": "Entity name to get context for",
					},
					"intent": map[string]interface{}{
						"type":        "string",
						"description": "What you're trying to do (e.g., 'fix bug', 'understand behavior', 'add feature')",
					},
				},
				"required": []string{"entity", "intent"},
			},
		},
	)
}
