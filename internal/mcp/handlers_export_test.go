package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestHandleExportKnowledge_Basic verifies the tool returns valid JSON with
// the expected envelope fields and summary counts.
func TestHandleExportKnowledge_Basic(t *testing.T) {
	srv := newTestServer(t)

	// Pre-populate: one memory and one episode.
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

	// The result text has a preamble followed by JSON. Extract the JSON part.
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
		Version   string `json:"version"`
		ProjectID string `json:"project_id"`
		Summary   struct {
			MemoryCount  int `json:"memory_count"`
			EpisodeCount int `json:"episode_count"`
		} `json:"summary"`
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
	if exp.Summary.MemoryCount < 1 {
		t.Errorf("MemoryCount = %d, want ≥1", exp.Summary.MemoryCount)
	}
	if exp.Summary.EpisodeCount < 1 {
		t.Errorf("EpisodeCount = %d, want ≥1", exp.Summary.EpisodeCount)
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
// when the store is nil (daemon not started).
func TestHandleExportKnowledge_NilStore(t *testing.T) {
	srv := newTestServer(t)
	srv.store = nil // simulate no-store state

	result, err := srv.handleExportKnowledge(context.Background(), callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("handleExportKnowledge: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when store is nil")
	}
}

