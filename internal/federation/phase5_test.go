package federation_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Brain mock ──────────────────────────────────────────────────────────────

type mockBrain struct {
	summaries map[string]string // "projectID::nodeID" → summary
	available bool
}

func (m *mockBrain) Summary(projectID, nodeID string) string {
	return m.summaries[projectID+"::"+nodeID]
}
func (m *mockBrain) Available() bool { return m.available }

// ── SearchEpisodes tests ────────────────────────────────────────────────────

func TestSearchEpisodes_Found(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	if err := os.MkdirAll(sibDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create sibling store with episodes.
	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Validate(token string) error"}, File: "auth.go"},
	})

	// Add episodes to the sibling store.
	dbPath, _ := federation.SiblingDBPath(sibDir)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.RememberEpisode(store.Episode{
		AgentID:   "agent-1",
		Decision:  "AuthService rewrite driven by compliance requirements",
		Rationale: "Legal flagged session token storage",
		Tags:      `["auth","compliance"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	results := r.SearchEpisodes(context.Background(), "auth compliance", nil, 5)
	if len(results) == 0 {
		t.Fatal("expected cross-project episodes, got none")
	}
	if results[0].Alias != "core" {
		t.Errorf("expected alias 'core', got %q", results[0].Alias)
	}
	if results[0].Episode.Decision == "" {
		t.Error("expected non-empty decision")
	}
}

func TestSearchEpisodes_LabeledWithSource(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	if err := os.MkdirAll(sibDir, 0o755); err != nil {
		t.Fatal(err)
	}

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Validate(token string) error"}, File: "auth.go"},
	})

	dbPath, _ := federation.SiblingDBPath(sibDir)
	st, _ := store.Open(dbPath)
	st.RememberEpisode(store.Episode{
		AgentID:  "agent-1",
		Decision: "Token validation updated",
		Tags:     `["auth"]`,
	})
	st.Close()

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	results := r.SearchEpisodes(context.Background(), "token validation", []string{"core"}, 5)
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	// Verify the alias label.
	if results[0].Alias != "core" {
		t.Errorf("expected alias 'core', got %q", results[0].Alias)
	}
}

func TestSearchEpisodes_NoResults(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	if err := os.MkdirAll(sibDir, 0o755); err != nil {
		t.Fatal(err)
	}

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	// No episodes in store → empty results.
	results := r.SearchEpisodes(context.Background(), "nonexistent thing", nil, 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestSearchEpisodes_SiblingUnavailable(t *testing.T) {
	dir := t.TempDir()

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: filepath.Join(dir, "nonexistent")},
	}, dir)
	defer r.Close()

	// Broken sibling → empty results, no panic.
	results := r.SearchEpisodes(context.Background(), "auth", nil, 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results from broken sibling, got %d", len(results))
	}
}

func TestSearchEpisodes_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := r.SearchEpisodes(ctx, "auth", nil, 5)
	if len(results) != 0 {
		t.Errorf("expected 0 results from cancelled context, got %d", len(results))
	}
}

// ── SearchMemoriesForEntity tests ───────────────────────────────────────────

func TestSearchMemoriesForEntity_Found(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::AuthService", Name: "AuthService", Type: graph.NodeStruct, File: "auth.go"},
	})

	dbPath, _ := federation.SiblingDBPath(sibDir)
	st, _ := store.Open(dbPath)
	st.RememberEpisode(store.Episode{
		AgentID:  "agent-1",
		Decision: "AuthService rewrite driven by compliance requirements",
		Tags:     `["auth"]`,
	})
	st.Close()

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	hints := r.SearchMemoriesForEntity(context.Background(), "AuthService", nil)
	if len(hints) == 0 {
		t.Fatal("expected memory hints, got none")
	}
	if hints[0].Alias != "core" {
		t.Errorf("expected alias 'core', got %q", hints[0].Alias)
	}
	if hints[0].Query != "AuthService" {
		t.Errorf("expected query 'AuthService', got %q", hints[0].Query)
	}
}

func TestSearchMemoriesForEntity_NotFound(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	hints := r.SearchMemoriesForEntity(context.Background(), "TotallyUnrelated", nil)
	if len(hints) != 0 {
		t.Errorf("expected 0 hints for unrelated entity, got %d", len(hints))
	}
}

// ── Brain summary tests ─────────────────────────────────────────────────────

func TestGetEntitySummary_BrainSummaryExists(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Validate(token string) error"}, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	projectID := r.SiblingProjectID("core")
	brain := &mockBrain{
		summaries: map[string]string{
			projectID + "::core-repo::auth.go::Validate": "Validates JWT tokens against the auth provider. Returns error for expired or malformed tokens.",
		},
		available: true,
	}
	r.SetBrain(brain)

	summary := r.GetEntitySummary(context.Background(), "core", "Validate")
	if summary == "" {
		t.Fatal("expected brain summary, got empty")
	}
	if summary == "func Validate(token string) error" {
		t.Error("expected brain summary, got raw signature instead")
	}
}

func TestGetEntitySummary_NoBrainSummary_FallsBackToSignature(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Validate(token string) error"}, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	brain := &mockBrain{
		summaries: map[string]string{}, // no summaries
		available: true,
	}
	r.SetBrain(brain)

	summary := r.GetEntitySummary(context.Background(), "core", "Validate")
	if summary != "func Validate(token string) error" {
		t.Errorf("expected raw signature fallback, got %q", summary)
	}
}

func TestGetEntitySummary_NoBrain_FallsBackToSignature(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{
		{ID: "core-repo::auth.go::Validate", Name: "Validate", Type: graph.NodeFunction,
			Metadata: map[string]string{"signature": "func Validate(token string) error"}, File: "auth.go"},
	})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()
	// No brain set → raw signature.

	summary := r.GetEntitySummary(context.Background(), "core", "Validate")
	if summary != "func Validate(token string) error" {
		t.Errorf("expected raw signature, got %q", summary)
	}
}

func TestGetEntitySummary_EntityNotFound(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	createSiblingWithDefaultPath(t, sibDir, "core-repo", []*graph.Node{})

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	summary := r.GetEntitySummary(context.Background(), "core", "NonExistent")
	if summary != "" {
		t.Errorf("expected empty summary for missing entity, got %q", summary)
	}
}

// ── Brain drift summary tests ───────────────────────────────────────────────

func TestBrainDriftSummary_BrainAvailable(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()

	brain := &mockBrain{available: true}
	r.SetBrain(brain)

	// Brain available but BrainDriftSummary currently returns structural diff.
	summary := r.BrainDriftSummary(context.Background(),
		"func Validate(token string) error",
		"func Validate(token string, opts ...Option) (bool, error)",
		"Validate",
	)
	if summary == "" {
		t.Fatal("expected non-empty drift summary")
	}
	// Should contain structural diff info.
	if summary == "Signature changed" {
		t.Error("expected detailed structural diff, got generic message")
	}
}

func TestBrainDriftSummary_BrainUnavailable(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()
	// No brain set.

	summary := r.BrainDriftSummary(context.Background(),
		"func Validate(token string) error",
		"func Validate(token string, opts ...Option) (bool, error)",
		"Validate",
	)
	if summary == "" {
		t.Fatal("expected non-empty fallback drift summary")
	}
}

func TestBrainDriftSummary_ParamAdded(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()

	summary := r.BrainDriftSummary(context.Background(),
		"func Login(user string) error",
		"func Login(user string, password string) error",
		"Login",
	)
	if summary == "" {
		t.Fatal("expected non-empty summary")
	}
	// Should mention added param.
	if !contains(summary, "password") && !contains(summary, "Params") {
		t.Errorf("expected mention of added param, got %q", summary)
	}
}

func TestBrainDriftSummary_ReturnTypeChanged(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()

	summary := r.BrainDriftSummary(context.Background(),
		"func Validate(token string) error",
		"func Validate(token string) (bool, error)",
		"Validate",
	)
	if summary == "" {
		t.Fatal("expected non-empty summary")
	}
	// Should mention return type change.
	if !contains(summary, "Returns") && !contains(summary, "bool") {
		t.Errorf("expected mention of return type change, got %q", summary)
	}
}

// ── SiblingProjectID tests ──────────────────────────────────────────────────

func TestSiblingProjectID_Valid(t *testing.T) {
	dir := t.TempDir()
	sibDir := filepath.Join(dir, "core")
	os.MkdirAll(sibDir, 0o755)

	r := federation.NewResolver([]config.FederationEntry{
		{Alias: "core", Path: sibDir},
	}, dir)
	defer r.Close()

	id := r.SiblingProjectID("core")
	if id == "" {
		t.Fatal("expected non-empty project ID")
	}
	// Should be consistent.
	if r.SiblingProjectID("core") != id {
		t.Error("expected consistent project ID")
	}
}

func TestSiblingProjectID_UnknownAlias(t *testing.T) {
	dir := t.TempDir()
	r := federation.NewResolver(nil, dir)
	defer r.Close()

	id := r.SiblingProjectID("nonexistent")
	if id != "" {
		t.Errorf("expected empty project ID for unknown alias, got %q", id)
	}
}

// contains is defined in tracker_test.go
