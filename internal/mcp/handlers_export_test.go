package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestHandleExportKnowledge_Basic verifies the tool returns valid JSON with
// the expected envelope fields and summary counts (inline, small export).
func TestHandleExportKnowledge_Basic(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "export handler test memory",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})
	_, _ = srv.store.RememberEpisode(store.Episode{
		AgentID:       "export-test-agent",
		EpisodeType:   "decision",
		Outcome:       "success",
		Decision:      "export handler test episode",
		AffectedFiles: "[]", AffectedNodes: "[]", Tags: "[]",
	})

	result, err := srv.handleExportKnowledge(context.Background(), callTool(map[string]any{
		"format": "json",
	}))
	if err != nil {
		t.Fatalf("handleExportKnowledge: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}

	if len(result.Content) == 0 {
		t.Fatal("no content in result")
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	raw := tc.Text

	// Preamble ends before the first '{' — find the JSON object.
	jsonStart := strings.Index(raw, "{")
	if jsonStart < 0 {
		t.Fatalf("no JSON object in result: %q", raw)
	}
	jsonBody := raw[jsonStart:]

	var exp struct {
		Version string `json:"version"`
		TTLNote string `json:"ttl_note"`
		Summary struct {
			MemoryCount  int `json:"memory_count"`
			EpisodeCount int `json:"episode_count"`
		} `json:"summary"`
		Memories []interface{} `json:"memories"`
		Episodes []interface{} `json:"episodes"`
	}
	if err := json.Unmarshal([]byte(jsonBody), &exp); err != nil {
		preview := jsonBody
		if len(preview) > 200 {
			preview = preview[:200]
		}
		t.Fatalf("unmarshal export JSON: %v\nbody: %q", err, preview)
	}
	if exp.Version != "1" {
		t.Errorf("Version = %q, want \"1\"", exp.Version)
	}
	if exp.TTLNote == "" {
		t.Error("TTLNote is empty in exported JSON")
	}
	if exp.Summary.MemoryCount < 1 {
		t.Errorf("MemoryCount = %d, want ≥1", exp.Summary.MemoryCount)
	}
	if exp.Summary.EpisodeCount < 1 {
		t.Errorf("EpisodeCount = %d, want ≥1", exp.Summary.EpisodeCount)
	}
	// Slices must be JSON arrays (not null) — verify the raw JSON.
	if strings.Contains(jsonBody, `"memories": null`) {
		t.Error(`"memories" is null in JSON, want []`)
	}
	if strings.Contains(jsonBody, `"episodes": null`) {
		t.Error(`"episodes" is null in JSON, want []`)
	}
}

// TestHandleExportKnowledge_OutputPath verifies the tool writes to a file
// when output_path is provided and returns only the summary (not the full JSON).
func TestHandleExportKnowledge_OutputPath(t *testing.T) {
	srv := newTestServer(t)

	_, _ = srv.store.InsertMemory(store.Memory{
		Tier:      store.TierProject,
		Content:   "file output test memory",
		Source:    store.SourceManual,
		ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	})

	outPath := filepath.Join(t.TempDir(), "export.json")

	result, err := srv.handleExportKnowledge(context.Background(), callTool(map[string]any{
		"output_path": outPath,
	}))
	if err != nil {
		t.Fatalf("handleExportKnowledge: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}

	// The response should mention the file path, not contain the full JSON.
	tc, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(tc.Text, outPath) {
		t.Errorf("response does not mention output path: %q", tc.Text)
	}
	if strings.Contains(tc.Text, `"version"`) {
		t.Error("inline JSON leaked into output_path response — should be summary only")
	}

	// The file must exist and contain valid JSON.
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output file is not valid JSON: %v", err)
	}
	if parsed["version"] != "1" {
		t.Errorf("version in file = %v, want \"1\"", parsed["version"])
	}
	// Arrays must be [] not null.
	if parsed["memories"] == nil {
		t.Error(`"memories" is null in file, want []`)
	}
}

// TestHandleExportKnowledge_OutputPath_CreatesDir verifies parent dirs are
// created automatically when they don't exist.
func TestHandleExportKnowledge_OutputPath_CreatesDir(t *testing.T) {
	srv := newTestServer(t)

	outPath := filepath.Join(t.TempDir(), "subdir", "nested", "export.json")

	result, err := srv.handleExportKnowledge(context.Background(), callTool(map[string]any{
		"output_path": outPath,
	}))
	if err != nil {
		t.Fatalf("handleExportKnowledge: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error creating nested dirs: %v", result.Content)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("output file not created at nested path: %v", err)
	}
}

// TestHandleExportKnowledge_DefaultFormat verifies omitting format defaults to json.
func TestHandleExportKnowledge_DefaultFormat(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleExportKnowledge(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("handleExportKnowledge: %v", err)
	}
	if result.IsError {
		t.Errorf("unexpected error with empty format: %v", result.Content)
	}
}

// TestHandleExportKnowledge_UnsupportedFormat verifies an unknown format returns an error.
func TestHandleExportKnowledge_UnsupportedFormat(t *testing.T) {
	srv := newTestServer(t)

	result, err := srv.handleExportKnowledge(context.Background(), callTool(map[string]any{
		"format": "xml",
	}))
	if err != nil {
		t.Fatalf("handleExportKnowledge: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for unsupported format, got success")
	}
}

// TestHandleExportKnowledge_NilStore verifies a helpful error is returned
// when the store is nil.
func TestHandleExportKnowledge_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil

	result, err := srv.handleExportKnowledge(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("handleExportKnowledge: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when store is nil")
	}
}

// TestHandleExportKnowledge_AtomicWrite verifies that the output file is
// complete and valid even if we check it immediately after the call (no
// partial-write window where the file is incomplete JSON).
func TestHandleExportKnowledge_AtomicWrite(t *testing.T) {
	srv := newTestServer(t)

	for i := 0; i < 5; i++ {
		_, _ = srv.store.InsertMemory(store.Memory{
			Tier:      store.TierProject,
			Content:   "atomic write test memory " + string(rune('A'+i)),
			Source:    store.SourceManual,
			ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		})
	}

	outPath := filepath.Join(t.TempDir(), "atomic.json")

	_, err := srv.handleExportKnowledge(context.Background(), callTool(map[string]any{
		"output_path": outPath,
	}))
	if err != nil {
		t.Fatalf("handleExportKnowledge: %v", err)
	}

	// Temp file must be gone (rename completed).
	if _, err := os.Stat(outPath + ".tmp"); err == nil {
		t.Error("temp file still exists after export — rename did not complete")
	}

	// File must be parseable immediately.
	data, _ := os.ReadFile(outPath)
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output file not valid JSON immediately after write: %v", err)
	}
}
