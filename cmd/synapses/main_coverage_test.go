package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/store"
)

// goRepoDir2 creates a minimal Go module directory for indexing tests.
// Named goRepoDir2 to avoid collision with any helper in main_test.go.
func goRepoDir2(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n\nfunc helper() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// indexedRepoDir2 creates a minimal Go module, indexes it, and returns the dir.
func indexedRepoDir2(t *testing.T) string {
	t.Helper()
	dir := goRepoDir2(t)
	if err := cmdIndex([]string{"--path", dir}); err != nil {
		t.Fatalf("cmdIndex failed: %v", err)
	}
	return dir
}

// ── buildGraph ────────────────────────────────────────────────────────────────

func TestBuildGraph_BasicGoRepoCov(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatalf("buildGraph returned error: %v", err)
	}
	if g == nil {
		t.Fatal("buildGraph returned nil graph")
	}
	if g.NodeCount() == 0 {
		t.Error("expected at least one node in graph")
	}
}

func TestBuildGraph_WithStoreCov(t *testing.T) {
	dir := goRepoDir2(t)
	dbPath, err := store.DefaultPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	g, err := buildGraph(dir, st, nil)
	if err != nil {
		t.Fatalf("buildGraph with store returned error: %v", err)
	}
	if g == nil {
		t.Fatal("buildGraph returned nil graph")
	}
}

// ── loadOrBuildGraph ──────────────────────────────────────────────────────────

func TestLoadOrBuildGraph_NoCacheCov(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := loadOrBuildGraph(dir, false)
	if err != nil {
		t.Fatalf("loadOrBuildGraph returned error: %v", err)
	}
	if g == nil {
		t.Fatal("expected non-nil graph")
	}
}

func TestLoadOrBuildGraph_ForceReindexCov(t *testing.T) {
	dir := goRepoDir2(t)
	// First build to populate cache.
	if _, err := loadOrBuildGraph(dir, false); err != nil {
		t.Fatal(err)
	}
	// Force reindex.
	g, err := loadOrBuildGraph(dir, true)
	if err != nil {
		t.Fatalf("loadOrBuildGraph force reindex returned error: %v", err)
	}
	if g == nil {
		t.Fatal("expected non-nil graph")
	}
}

// ── cmdIndex ──────────────────────────────────────────────────────────────────

func TestCmdIndex_BasicGoRepoCov(t *testing.T) {
	dir := goRepoDir2(t)
	if err := cmdIndex([]string{"--path", dir}); err != nil {
		t.Errorf("cmdIndex returned error: %v", err)
	}
}

func TestCmdIndex_ForceReindexCov(t *testing.T) {
	dir := goRepoDir2(t)
	// Index once to build cache.
	if err := cmdIndex([]string{"--path", dir}); err != nil {
		t.Fatalf("first index failed: %v", err)
	}
	// Force reindex.
	if err := cmdIndex([]string{"--path", dir, "--reindex"}); err != nil {
		t.Errorf("force reindex returned error: %v", err)
	}
}

func TestCmdIndex_WithLinkedProject(t *testing.T) {
	// Primary project.
	primary := goRepoDir2(t)
	// Linked project (indexed separately).
	linked := goRepoDir2(t)
	if err := cmdIndex([]string{"--path", linked}); err != nil {
		t.Fatalf("index linked project: %v", err)
	}

	// Write a synapses.json with a linked entry.
	cfg := `{"linked":["` + linked + `"]}`
	if err := os.WriteFile(filepath.Join(primary, "synapses.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	// Index primary — federation path.
	if err := cmdIndex([]string{"--path", primary}); err != nil {
		t.Errorf("cmdIndex with linked project: %v", err)
	}
}

func TestCmdIndex_LinkedProjectNotIndexed(t *testing.T) {
	// Linked project exists but has not been indexed yet.
	primary := goRepoDir2(t)
	linked := goRepoDir2(t) // never indexed

	cfg := `{"linked":["` + linked + `"]}`
	if err := os.WriteFile(filepath.Join(primary, "synapses.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	// Should succeed (logs a warning but doesn't fail).
	if err := cmdIndex([]string{"--path", primary}); err != nil {
		t.Errorf("cmdIndex with unindexed linked project: %v", err)
	}
}

// ── cmdStatus ─────────────────────────────────────────────────────────────────

func TestCmdStatus_WithIndexCov(t *testing.T) {
	dir := indexedRepoDir2(t)
	if err := cmdStatus([]string{"--path", dir}); err != nil {
		t.Errorf("cmdStatus with index returned error: %v", err)
	}
}

// ── cmdDoctor ─────────────────────────────────────────────────────────────────

func TestCmdDoctor_WithIndexCov(t *testing.T) {
	dir := indexedRepoDir2(t)
	if err := cmdDoctor([]string{"--path", dir}); err != nil {
		t.Errorf("cmdDoctor with index returned error: %v", err)
	}
}

func TestCmdDoctor_WithBrainURLCov(t *testing.T) {
	dir := t.TempDir()
	// Write config with brain URL (unreachable — should show "unreachable" not error).
	cfg := `{"brain":{"url":"http://127.0.0.1:19999"}}`
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cmdDoctor([]string{"--path", dir}); err != nil {
		t.Errorf("cmdDoctor with brain URL returned error: %v", err)
	}
}

func TestCmdDoctor_WithScoutURL(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"scout":{"url":"http://127.0.0.1:19998"}}`
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cmdDoctor([]string{"--path", dir}); err != nil {
		t.Errorf("cmdDoctor with scout URL returned error: %v", err)
	}
}

func TestCmdDoctor_WithPulseURL(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"pulse":{"url":"http://127.0.0.1:19997"}}`
	if err := os.WriteFile(filepath.Join(dir, "synapses.json"), []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cmdDoctor([]string{"--path", dir}); err != nil {
		t.Errorf("cmdDoctor with pulse URL returned error: %v", err)
	}
}

// ── cmdQuery ──────────────────────────────────────────────────────────────────

func TestCmdQuery_EntityFoundCov(t *testing.T) {
	dir := indexedRepoDir2(t)
	if err := cmdQuery([]string{"--path", dir, "--entity", "main"}); err != nil {
		t.Errorf("cmdQuery for 'main' returned error: %v", err)
	}
}

func TestCmdQuery_EntityNotFoundCov(t *testing.T) {
	dir := indexedRepoDir2(t)
	err := cmdQuery([]string{"--path", dir, "--entity", "nonexistentXYZ123"})
	if err == nil {
		t.Error("expected error for missing entity")
	}
}

func TestCmdQuery_NoIndexCov(t *testing.T) {
	dir := t.TempDir()
	err := cmdQuery([]string{"--path", dir, "--entity", "main"})
	if err == nil {
		t.Error("expected error when no index exists")
	}
}

func TestCmdQuery_SuffixMatchCov(t *testing.T) {
	// Write a file with a method so suffix matching is exercised.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testmod\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := `package main

type MyStruct struct{}

func (m MyStruct) DoThing() {}

func main() {}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cmdIndex([]string{"--path", dir}); err != nil {
		t.Fatal(err)
	}
	// Query by suffix — "DoThing" should match "MyStruct.DoThing".
	if err := cmdQuery([]string{"--path", dir, "--entity", "DoThing"}); err != nil {
		t.Errorf("suffix match query returned error: %v", err)
	}
}

// ── analyzeDataFlowIfEnabled ──────────────────────────────────────────────────

func TestAnalyzeDataFlowIfEnabled_EmptyConfigCov(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	analyzeDataFlowIfEnabled(g, &config.Config{})
}

// ── enrichMetricsIfEnabled ────────────────────────────────────────────────────

func TestEnrichMetricsIfEnabled_DefaultDays(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{} // MetricsDays = 0 → defaults to 90
	enrichMetricsIfEnabled(g, dir, cfg)
}

func TestEnrichMetricsIfEnabled_WithCoverageProfile(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Write a fake (empty) coverage profile — path just needs to exist.
	profilePath := filepath.Join(dir, "coverage.out")
	if err := os.WriteFile(profilePath, []byte("mode: set\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CoverageProfile: profilePath}
	enrichMetricsIfEnabled(g, dir, cfg)
}

func TestEnrichMetricsIfEnabled_WithPprofProfile(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Non-existent pprof — enricher should be fail-silent.
	cfg := &config.Config{PprofProfile: filepath.Join(dir, "nonexistent.pprof")}
	enrichMetricsIfEnabled(g, dir, cfg)
}

func TestEnrichMetricsIfEnabled_CustomDays(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{MetricsDays: 30}
	enrichMetricsIfEnabled(g, dir, cfg)
}

// ── applyGoTypesIfEnabled ─────────────────────────────────────────────────────

func TestApplyGoTypesIfEnabled_DisabledCov(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{UseGoTypes: false}
	applyGoTypesIfEnabled(g, dir, cfg)
}

func TestApplyGoTypesIfEnabled_EnabledCov(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{UseGoTypes: true}
	// May succeed or fail depending on environment; should not panic.
	applyGoTypesIfEnabled(g, dir, cfg)
}

// ── applyTSTypesIfEnabled ─────────────────────────────────────────────────────

func TestApplyTSTypesIfEnabled_DisabledCov(t *testing.T) {
	dir := goRepoDir2(t)
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{UseTSTypes: false}
	applyTSTypesIfEnabled(g, dir, cfg)
}

func TestApplyTSTypesIfEnabled_EnabledCov(t *testing.T) {
	dir := t.TempDir()
	// Use a dir with no TS files — resolver should fail silently.
	g, err := buildGraph(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{UseTSTypes: true}
	applyTSTypesIfEnabled(g, dir, cfg)
}

// ── writeProjectCLAUDE ────────────────────────────────────────────────────────

func TestWriteProjectCLAUDE_CreatesFileCov(t *testing.T) {
	dir := t.TempDir()
	if err := writeProjectCLAUDE(dir); err != nil {
		t.Fatalf("writeProjectCLAUDE returned error: %v", err)
	}
	claudePath := filepath.Join(dir, ".claude", "CLAUDE.md")
	if _, err := os.Stat(claudePath); os.IsNotExist(err) {
		t.Error(".claude/CLAUDE.md was not created")
	}
}

func TestWriteProjectCLAUDE_IdempotentCov(t *testing.T) {
	dir := t.TempDir()
	if err := writeProjectCLAUDE(dir); err != nil {
		t.Fatal(err)
	}
	// Second call should not error and file should still be valid.
	if err := writeProjectCLAUDE(dir); err != nil {
		t.Fatalf("second writeProjectCLAUDE returned error: %v", err)
	}
}

func TestWriteProjectCLAUDE_UpdatesExistingSectionCov(t *testing.T) {
	dir := t.TempDir()
	clauDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(clauDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-populate with user content + existing Synapses section.
	existing := "# My Project\n\nSome notes.\n\n<!-- synapses:start -->\nold content\n<!-- synapses:end -->\n"
	if err := os.WriteFile(filepath.Join(clauDir, "CLAUDE.md"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectCLAUDE(dir); err != nil {
		t.Fatalf("writeProjectCLAUDE returned error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(clauDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "old content") {
		t.Error("old section content should have been replaced")
	}
	if !strings.Contains(content, "session_init") {
		t.Error("new section should contain session_init")
	}
	if !strings.Contains(content, "# My Project") {
		t.Error("user content should be preserved")
	}
}

func TestWriteProjectCLAUDE_MigratesRootCLAUDECov(t *testing.T) {
	dir := t.TempDir()
	// Write a root-level CLAUDE.md with a Synapses section that should migrate.
	rootContent := "# Root\n\n<!-- synapses:start -->\nlegacy section\n<!-- synapses:end -->\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(rootContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectCLAUDE(dir); err != nil {
		t.Fatalf("writeProjectCLAUDE returned error: %v", err)
	}
	// Root CLAUDE.md should no longer contain the Synapses section.
	rootData, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err == nil && strings.Contains(string(rootData), "<!-- synapses:start -->") {
		t.Error("root CLAUDE.md should not contain the Synapses section after migration")
	}
}

func TestWriteProjectCLAUDE_AppendToExistingContent(t *testing.T) {
	dir := t.TempDir()
	clauDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(clauDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-populate without any Synapses section.
	existing := "# My Project\n\nSome user notes.\n"
	if err := os.WriteFile(filepath.Join(clauDir, "CLAUDE.md"), []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeProjectCLAUDE(dir); err != nil {
		t.Fatalf("writeProjectCLAUDE returned error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(clauDir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "# My Project") {
		t.Error("user content should be preserved when appending")
	}
	if !strings.Contains(content, "session_init") {
		t.Error("Synapses section should be appended")
	}
}

// ── writeMCPConfig ────────────────────────────────────────────────────────────

func TestWriteMCPConfig_CreatesFileCov(t *testing.T) {
	dir := t.TempDir()
	mcpFile := filepath.Join(dir, ".mcp.json")
	if err := writeMCPConfig(mcpFile, dir); err != nil {
		t.Fatalf("writeMCPConfig returned error: %v", err)
	}
	if _, err := os.Stat(mcpFile); os.IsNotExist(err) {
		t.Error(".mcp.json was not created")
	}
}

func TestWriteMCPConfig_PreservesExistingServersCov(t *testing.T) {
	dir := t.TempDir()
	mcpFile := filepath.Join(dir, ".mcp.json")
	// Pre-populate with an existing server.
	existing := `{"mcpServers":{"other":{"type":"stdio","command":"other-tool"}}}`
	if err := os.WriteFile(mcpFile, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeMCPConfig(mcpFile, dir); err != nil {
		t.Fatalf("writeMCPConfig returned error: %v", err)
	}
	data, err := os.ReadFile(mcpFile)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, `"other"`) {
		t.Error("existing server 'other' should be preserved")
	}
	if !strings.Contains(content, `"synapses"`) {
		t.Error("synapses server should be added")
	}
}

func TestWriteMCPConfig_IdempotentCov(t *testing.T) {
	dir := t.TempDir()
	mcpFile := filepath.Join(dir, ".mcp.json")
	if err := writeMCPConfig(mcpFile, dir); err != nil {
		t.Fatal(err)
	}
	if err := writeMCPConfig(mcpFile, dir); err != nil {
		t.Fatalf("second writeMCPConfig returned error: %v", err)
	}
}

func TestWriteMCPConfig_MissingMcpServersKey(t *testing.T) {
	dir := t.TempDir()
	mcpFile := filepath.Join(dir, ".mcp.json")
	// JSON without mcpServers key — should add it.
	existing := `{"otherKey":"value"}`
	if err := os.WriteFile(mcpFile, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeMCPConfig(mcpFile, dir); err != nil {
		t.Fatalf("writeMCPConfig returned error: %v", err)
	}
	data, err := os.ReadFile(mcpFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"synapses"`) {
		t.Error("synapses server should be added")
	}
}

// ── smartReindex ──────────────────────────────────────────────────────────────

func TestSmartReindex_WithCachedGraphCov(t *testing.T) {
	dir := goRepoDir2(t)
	dbPath, err := store.DefaultPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Build graph and save it (mtimes saved by buildGraph).
	g, err := buildGraph(dir, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGraph(g); err != nil {
		t.Fatal(err)
	}

	// Now smartReindex should succeed.
	g2, err := smartReindex(dir, st, nil)
	if err != nil {
		t.Errorf("smartReindex returned error: %v", err)
		return
	}
	if g2 == nil {
		t.Error("smartReindex returned nil graph")
	}
}

// ── mergeLinkedProject ────────────────────────────────────────────────────────

func TestMergeLinkedProject_NotIndexedCov(t *testing.T) {
	g, err := buildGraph(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Linked project has no index.
	linked := t.TempDir()
	err = mergeLinkedProject(g, linked)
	if err == nil {
		t.Error("expected error when linked project not indexed")
	}
}

func TestMergeLinkedProject_IndexedCov(t *testing.T) {
	primary := t.TempDir()
	linked := goRepoDir2(t)

	// Index the linked project.
	if err := cmdIndex([]string{"--path", linked}); err != nil {
		t.Fatal(err)
	}

	g, err := buildGraph(primary, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := mergeLinkedProject(g, linked); err != nil {
		t.Errorf("mergeLinkedProject returned error: %v", err)
	}
}

// ── loadOrBuildGraphWithStore ─────────────────────────────────────────────────

func TestLoadOrBuildGraphWithStore_ForceReindexWithCacheCov(t *testing.T) {
	dir := goRepoDir2(t)
	dbPath, err := store.DefaultPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// First build to create cache.
	g, err := loadOrBuildGraphWithStore(dir, st, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g == nil {
		t.Fatal("expected non-nil graph")
	}

	// Force reindex with existing cache.
	g2, err := loadOrBuildGraphWithStore(dir, st, true, nil)
	if err != nil {
		t.Errorf("force reindex returned error: %v", err)
	}
	if g2 == nil {
		t.Error("expected non-nil graph after force reindex")
	}
}

// ── cmdReset additional coverage ──────────────────────────────────────────────

func TestCmdReset_SpecificPathWithIndexCov(t *testing.T) {
	dir := indexedRepoDir2(t)
	if err := cmdReset([]string{"--path", dir}); err != nil {
		t.Errorf("cmdReset returned error: %v", err)
	}
}
