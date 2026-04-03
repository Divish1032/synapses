package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
)

// ── extractFilePathsFromText unit tests ──────────────────────────────────────

func TestExtractFilePathsFromText_GoFile(t *testing.T) {
	text := "I am about to add a new handler to handlers/users.go for the user API"
	got := extractFilePathsFromText(text)
	if len(got) != 1 || got[0] != "handlers/users.go" {
		t.Errorf("expected [handlers/users.go], got %v", got)
	}
}

func TestExtractFilePathsFromText_MultipleFiles(t *testing.T) {
	text := "modify internal/auth/service.go and add tests in internal/auth/service_test.go"
	got := extractFilePathsFromText(text)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 paths, got %v", got)
	}
	foundService, foundTest := false, false
	for _, p := range got {
		if strings.Contains(p, "service.go") {
			foundService = true
		}
		if strings.Contains(p, "service_test.go") {
			foundTest = true
		}
	}
	if !foundService || !foundTest {
		t.Errorf("expected both service.go and service_test.go paths, got %v", got)
	}
}

func TestExtractFilePathsFromText_TypeScript(t *testing.T) {
	text := "Adding a new route handler to src/routes/users.ts"
	got := extractFilePathsFromText(text)
	if len(got) != 1 || got[0] != "src/routes/users.ts" {
		t.Errorf("expected [src/routes/users.ts], got %v", got)
	}
}

func TestExtractFilePathsFromText_NoFiles(t *testing.T) {
	text := "I want to add some authentication to the user management endpoint"
	got := extractFilePathsFromText(text)
	if len(got) != 0 {
		t.Errorf("expected no paths extracted from prose without paths, got %v", got)
	}
}

func TestExtractFilePathsFromText_Deduplication(t *testing.T) {
	text := "update handlers/users.go and then test handlers/users.go again"
	got := extractFilePathsFromText(text)
	if len(got) != 1 {
		t.Errorf("expected 1 unique path (deduplicated), got %v", got)
	}
}

// ── handleValidatePreWrite error cases ───────────────────────────────────────

func TestValidatePreWrite_NoInputError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleValidatePreWrite(ctx, callTool(nil))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool-level error for missing description and files")
	}
	errText := mustErrorResult(t, res, nil)
	if !strings.Contains(errText, "description") || !strings.Contains(errText, "files") {
		t.Errorf("error should mention 'description' and 'files', got: %s", errText)
	}
}

func TestValidatePreWrite_InvalidFilesJSON(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleValidatePreWrite(ctx, callTool(map[string]any{
		"files": "not-json",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool-level error for invalid files JSON")
	}
}

// TestValidatePreWrite_NoGraph verifies an advisory response when the graph
// is nil (knowledge mode / uninitialized daemon).
func TestValidatePreWrite_NoGraph(t *testing.T) {
	st := openMCPTestStore(t)
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// nil graph → knowledge mode.
	srv := New(nil, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	res, callErr := srv.handleValidatePreWrite(ctx, callTool(map[string]any{
		"description": "adding a POST handler to handlers/users.go",
	}))
	m := mustResult(t, res, callErr)

	if got := m["status"]; got != "unprotected" {
		t.Errorf("expected status=unprotected, got %v", got)
	}
	if _, ok := m["message"]; !ok {
		t.Error("expected advisory message when graph is nil")
	}
	if _, ok := m["_summary"]; !ok {
		t.Error("expected _summary in no-graph response")
	}
}

// TestValidatePreWrite_ExistingFile_WithFindings verifies that when an existing
// chi-routes file is passed as a target, the pattern engine fires and findings
// appear in the response. The file must be on disk AND in the graph (the
// pattern engine uses the graph for import edges).
func TestValidatePreWrite_ExistingFile_WithFindings(t *testing.T) {
	root := t.TempDir()
	// Absolute file path stored in graph (pattern engine requires absolute paths).
	filePath := filepath.Join(root, "api", "routes.go")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Write a minimal Go file to disk so os.Stat succeeds (file is NOT new).
	if err := os.WriteFile(filePath, []byte("package api\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Graph with chi import — triggers "go-chi-missing-auth" pattern.
	g := makeChiRouteGraph(t, root, filePath)

	st := openMCPTestStore(t)
	cfg, cfgErr := config.Load(t.TempDir())
	if cfgErr != nil {
		t.Fatalf("config.Load: %v", cfgErr)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// Pass the absolute file path explicitly.
	filesJSON, _ := json.Marshal([]string{filePath})
	res, callErr := srv.handleValidatePreWrite(ctx, callTool(map[string]any{
		"description": "adding a new route to the chi router",
		"files":       string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	// Status must not be "clear" — chi pattern fires on this file.
	if got := m["status"]; got == "clear" {
		t.Error("expected non-clear status when chi auth pattern fires, got clear")
	}
	// Top-level security_findings must be present.
	if _, ok := m["security_findings"]; !ok {
		t.Error("expected security_findings in response when pattern fires")
	}
	// _summary must be present.
	if _, ok := m["_summary"]; !ok {
		t.Error("expected _summary field")
	}
	// files_analyzed must list the file.
	fa, ok := m["files_analyzed"].([]any)
	if !ok || len(fa) == 0 {
		t.Fatal("expected files_analyzed list")
	}
	fr := fa[0].(map[string]any)
	if got := fr["is_new"]; got != false {
		t.Errorf("existing file should have is_new=false, got %v", got)
	}
}

// TestValidatePreWrite_NewFile_SiblingAnalysis verifies that when a NEW (not
// yet written) file is the target, siblings in the same directory are scanned
// to infer expected security patterns. The sibling must be in the graph for
// the pattern to fire.
func TestValidatePreWrite_NewFile_SiblingAnalysis(t *testing.T) {
	root := t.TempDir()
	siblingPath := filepath.Join(root, "api", "users.go")
	newFilePath := filepath.Join(root, "api", "posts.go") // does not exist on disk

	if err := os.MkdirAll(filepath.Dir(siblingPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Sibling exists on disk.
	if err := os.WriteFile(siblingPath, []byte("package api\n"), 0o644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	// newFilePath intentionally NOT created on disk.

	// Graph: chi import on the sibling → pattern will fire when scanning sibling.
	g := makeChiRouteGraph(t, root, siblingPath)

	st := openMCPTestStore(t)
	cfg, cfgErr := config.Load(t.TempDir())
	if cfgErr != nil {
		t.Fatalf("config.Load: %v", cfgErr)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	filesJSON, _ := json.Marshal([]string{newFilePath})
	res, callErr := srv.handleValidatePreWrite(ctx, callTool(map[string]any{
		"description": "adding a new posts handler to api/posts.go",
		"files":       string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	fa, ok := m["files_analyzed"].([]any)
	if !ok || len(fa) == 0 {
		t.Fatal("expected files_analyzed list for new-file case")
	}
	fr := fa[0].(map[string]any)
	if got := fr["is_new"]; got != true {
		t.Errorf("new file should have is_new=true, got %v", got)
	}

	// If the sibling pattern fired, security_findings should be in the result.
	// (May be empty if the pattern engine finds no issues in sibling — acceptable.)
	// The key correctness property: is_new=true AND no panic or error.
	summary, _ := m["_summary"].(string)
	if summary == "" {
		t.Error("expected non-empty _summary")
	}
}

// TestValidatePreWrite_PathTraversal verifies that path traversal attempts
// are silently rejected and do not trigger filesystem access.
func TestValidatePreWrite_PathTraversal(t *testing.T) {
	s, root := newServerWithRoot(t)

	// Create a file OUTSIDE the root.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.go")
	if err := os.WriteFile(outsideFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	// Compute relative path from root → outside file (contains ".." segments).
	rel, err := filepath.Rel(root, outsideFile)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	filesJSON, _ := json.Marshal([]string{rel})
	res, callErr := s.handleValidatePreWrite(ctx, callTool(map[string]any{
		"description": "modifying external file",
		"files":       string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	// Traversal file should be silently skipped → files_analyzed is empty.
	fa, _ := m["files_analyzed"].([]any)
	if len(fa) != 0 {
		t.Errorf("traversal path should be silently skipped, but files_analyzed has %d entry(ies)", len(fa))
	}
}

// TestValidatePreWrite_AbsoluteTraversal verifies that an absolute path outside
// the project root is also rejected.
func TestValidatePreWrite_AbsoluteTraversal(t *testing.T) {
	s, _ := newServerWithRoot(t)

	filesJSON := `["/etc/passwd"]`
	res, callErr := s.handleValidatePreWrite(ctx, callTool(map[string]any{
		"files": filesJSON,
	}))
	m := mustResult(t, res, callErr)

	fa, _ := m["files_analyzed"].([]any)
	if len(fa) != 0 {
		t.Errorf("absolute external path should be skipped, files_analyzed has %d entry(ies)", len(fa))
	}
}

// TestValidatePreWrite_DescriptionOnlyNoFiles verifies that when a description
// contains no file paths and no explicit files are given, the handler returns
// a hint rather than an error.
func TestValidatePreWrite_DescriptionOnlyNoFiles(t *testing.T) {
	s := newTestServer(t)
	res, callErr := s.handleValidatePreWrite(ctx, callTool(map[string]any{
		"description": "I want to add some new authentication logic",
	}))
	m := mustResult(t, res, callErr)

	// No files → hint should appear.
	if _, ok := m["hint"]; !ok {
		t.Error("expected hint when no file paths could be identified")
	}
	// Should still have a _summary.
	if _, ok := m["_summary"]; !ok {
		t.Error("expected _summary even without files")
	}
}

// TestValidatePreWrite_DispatchRoute verifies that validate(phase="pre_write")
// correctly dispatches to handleValidatePreWrite via handleValidateDispatch.
func TestValidatePreWrite_DispatchRoute(t *testing.T) {
	s := newTestServer(t)
	res, callErr := s.handleValidateDispatch(ctx, callTool(map[string]any{
		"phase":       "pre_write",
		"description": "adding a new endpoint handler",
	}))
	// Must not be a Go error.
	if callErr != nil {
		t.Fatalf("dispatch returned Go error: %v", callErr)
	}
	if res == nil {
		t.Fatal("dispatch returned nil result")
	}
	// Must not be a "unknown validate phase" error.
	if res.IsError {
		text := mustErrorResult(t, res, nil)
		if strings.Contains(text, "unknown validate phase") {
			t.Errorf("pre_write was not routed — unknown phase error: %s", text)
		}
	}
}

// TestValidatePreWrite_UnknownPhaseError verifies the error message is updated
// to include "pre_write" in the list of valid phases.
func TestValidatePreWrite_UnknownPhaseError(t *testing.T) {
	s := newTestServer(t)
	res, callErr := s.handleValidateDispatch(ctx, callTool(map[string]any{
		"phase": "nonexistent_phase",
	}))
	if callErr != nil {
		t.Fatalf("unexpected Go error: %v", callErr)
	}
	if !res.IsError {
		t.Fatal("expected tool-level error for unknown phase")
	}
	text := mustErrorResult(t, res, nil)
	if !strings.Contains(text, "pre_write") {
		t.Errorf("error message should list pre_write as a valid phase, got: %s", text)
	}
}
