package mcp

// Tests for semantic tool discovery (Sprint 12 #5).
// Covers: semantic path taken when embedder+embeddings ready, keyword fallback
// when embedder absent, keyword fallback on embed error, EmbedToolCatalog
// correctness, normalizeVec and dotProduct math helpers.

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/embed"
)

// ── mock embedder for semantic tests ─────────────────────────────────────────

// indexedEmbedder returns a deterministic distinct vector per call index, so
// different tool descriptions produce different embeddings and ranking is
// meaningful even without a real model.
type indexedEmbedder struct {
	callIdx   atomic.Int32
	failAfter int32 // embed returns error once callIdx >= failAfter (0 = never fail)
	model     string
}

func (e *indexedEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	idx := e.callIdx.Add(1) - 1
	if e.failAfter > 0 && idx >= e.failAfter {
		return nil, errors.New("embed: forced test error")
	}
	// Build a 4-dimensional unit vector that is unique per index.
	// Packs index as angle θ = idx * (π/4) in the first two dimensions.
	theta := float64(idx) * (math.Pi / 4)
	return []float32{
		float32(math.Cos(theta)),
		float32(math.Sin(theta)),
		0,
		0,
	}, nil
}

func (e *indexedEmbedder) Model() string {
	if e.model == "" {
		return "test-indexed"
	}
	return e.model
}

func (e *indexedEmbedder) WarmUp(_ context.Context) error { return nil }
func (e *indexedEmbedder) Close() error                   { return nil }

var _ embed.Embedder = (*indexedEmbedder)(nil)

// constEmbedder always returns the same fixed vector regardless of input.
// Used when we want all tools to have equal similarity so we can test
// that the semantic path is taken without caring about ranking order.
type constEmbedder struct {
	vec []float32
	err error
}

func (e *constEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return e.vec, e.err
}
func (e *constEmbedder) WarmUp(_ context.Context) error { return nil }
func (e *constEmbedder) Model() string                  { return "test-const" }
func (e *constEmbedder) Close() error                   { return nil }

var _ embed.Embedder = (*constEmbedder)(nil)

// ── helper: build server with pre-embedded tool catalog ───────────────────────

// newSemanticServer returns a test server with the tool catalog already embedded
// using the given embedder. EmbedToolCatalog is called synchronously so the
// test doesn't need to wait for the background goroutine.
func newSemanticServer(t *testing.T, emb embed.Embedder) *Server {
	t.Helper()
	s := newTestServer(t)
	s.memoryEmbedder = emb
	s.EmbedToolCatalog(context.Background(), emb)
	return s
}

// ── normalizeVec ──────────────────────────────────────────────────────────────

func TestNormalizeVec_ProducesUnitVector(t *testing.T) {
	v := []float32{3, 4}
	u := normalizeVec(v)
	mag := math.Sqrt(float64(u[0]*u[0] + u[1]*u[1]))
	if math.Abs(mag-1.0) > 1e-6 {
		t.Errorf("expected unit vector, got magnitude %v", mag)
	}
}

func TestNormalizeVec_ZeroVector_Unchanged(t *testing.T) {
	v := []float32{0, 0, 0}
	u := normalizeVec(v)
	for i, x := range u {
		if x != v[i] {
			t.Errorf("zero vector should be unchanged, got %v", u)
			break
		}
	}
}

func TestNormalizeVec_SingleElement(t *testing.T) {
	v := []float32{5}
	u := normalizeVec(v)
	if math.Abs(float64(u[0])-1.0) > 1e-6 {
		t.Errorf("expected 1.0, got %v", u[0])
	}
}

func TestNormalizeVec_DoesNotMutateInput(t *testing.T) {
	v := []float32{3, 4}
	orig := []float32{3, 4}
	normalizeVec(v)
	if v[0] != orig[0] || v[1] != orig[1] {
		t.Error("normalizeVec must not mutate input slice")
	}
}

// ── dotProduct ────────────────────────────────────────────────────────────────

func TestDotProduct_OrthogonalVectors(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	if dotProduct(a, b) != 0 {
		t.Errorf("expected 0 for orthogonal vectors, got %v", dotProduct(a, b))
	}
}

func TestDotProduct_ParallelUnitVectors(t *testing.T) {
	a := []float32{1, 0}
	if got := dotProduct(a, a); math.Abs(float64(got)-1.0) > 1e-6 {
		t.Errorf("expected 1.0 for identical unit vectors, got %v", got)
	}
}

func TestDotProduct_LengthMismatch_ReturnsZero(t *testing.T) {
	if dotProduct([]float32{1, 2}, []float32{1}) != 0 {
		t.Error("expected 0 for mismatched lengths")
	}
}

func TestDotProduct_Empty_ReturnsZero(t *testing.T) {
	if dotProduct([]float32{}, []float32{}) != 0 {
		t.Error("expected 0 for empty slices")
	}
}

// ── EmbedToolCatalog ──────────────────────────────────────────────────────────

func TestEmbedToolCatalog_EmbeddsAllTools(t *testing.T) {
	s := newTestServer(t)
	emb := &indexedEmbedder{}
	s.EmbedToolCatalog(context.Background(), emb)

	s.toolEmbedsMu.RLock()
	n := len(s.toolEmbeds)
	s.toolEmbedsMu.RUnlock()

	if n != len(toolCatalog) {
		t.Errorf("expected %d tool embeddings, got %d", len(toolCatalog), n)
	}
}

func TestEmbedToolCatalog_NilEmbedder_NoOp(t *testing.T) {
	s := newTestServer(t)
	s.EmbedToolCatalog(context.Background(), nil) // must not panic

	s.toolEmbedsMu.RLock()
	n := len(s.toolEmbeds)
	s.toolEmbedsMu.RUnlock()

	if n != 0 {
		t.Errorf("expected 0 embeddings for nil embedder, got %d", n)
	}
}

func TestEmbedToolCatalog_EmbedError_AbortsSilently(t *testing.T) {
	s := newTestServer(t)
	// Force error on first embed call.
	emb := &indexedEmbedder{failAfter: 1}
	s.EmbedToolCatalog(context.Background(), emb) // must not panic

	s.toolEmbedsMu.RLock()
	n := len(s.toolEmbeds)
	s.toolEmbedsMu.RUnlock()

	// Abort on first error means toolEmbeds stays nil.
	if n != 0 {
		t.Errorf("partial embedding should leave toolEmbeds empty, got %d", n)
	}
}

func TestEmbedToolCatalog_VectorsAreNormalized(t *testing.T) {
	s := newTestServer(t)
	emb := &indexedEmbedder{}
	s.EmbedToolCatalog(context.Background(), emb)

	s.toolEmbedsMu.RLock()
	embeds := s.toolEmbeds
	s.toolEmbedsMu.RUnlock()

	for i, v := range embeds {
		var mag float64
		for _, x := range v {
			mag += float64(x) * float64(x)
		}
		mag = math.Sqrt(mag)
		if math.Abs(mag-1.0) > 1e-5 {
			t.Errorf("tool[%d] embedding not unit-length: mag=%v", i, mag)
		}
	}
}

// ── handleDiscoverTools — semantic path ───────────────────────────────────────

func TestDiscoverTools_SemanticPath_SearchModeField(t *testing.T) {
	// All tools get the same embedding → all have equal similarity.
	// We only verify that search_mode="semantic" is returned.
	emb := &constEmbedder{vec: []float32{1, 0, 0, 0}}
	s := newSemanticServer(t, emb)

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "find a function"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	mode, _ := m["search_mode"].(string)
	if mode != "semantic" {
		t.Errorf("expected search_mode=semantic, got %q", mode)
	}
}

func TestDiscoverTools_SemanticPath_MatchesHaveSimilarityScore(t *testing.T) {
	emb := &constEmbedder{vec: []float32{1, 0, 0, 0}}
	s := newSemanticServer(t, emb)

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "explore code"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	matches, _ := m["matches"].([]any)
	if len(matches) == 0 {
		t.Fatal("expected at least one match")
	}
	for i, match := range matches {
		mm, _ := match.(map[string]any)
		if _, ok := mm["similarity_score"]; !ok {
			t.Errorf("matches[%d] missing similarity_score field", i)
		}
	}
}

func TestDiscoverTools_SemanticPath_ReturnsTop3(t *testing.T) {
	emb := &constEmbedder{vec: []float32{1, 0, 0, 0}}
	s := newSemanticServer(t, emb)

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "session start"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	matches, _ := m["matches"].([]any)
	if len(matches) > 3 {
		t.Errorf("expected at most 3 matches, got %d", len(matches))
	}
}

func TestDiscoverTools_SemanticPath_RankingByCosineSimilarity(t *testing.T) {
	// Use a controlled embedder: query vector = [1,0,0,0].
	// Tool 0 embedding = [1,0,0,0] (cosine=1.0, best).
	// Tool 1 embedding = [0,1,0,0] (cosine=0.0, worst).
	// All other tools get zero-like embeddings for simplicity.
	queryVec := []float32{1, 0, 0, 0}
	bestToolIdx := 0

	vecs := make([][]float32, len(toolCatalog))
	vecs[bestToolIdx] = queryVec
	for i := range vecs {
		if vecs[i] == nil {
			vecs[i] = []float32{0, 1, 0, 0}
		}
	}

	s := newTestServer(t)
	// The query embedder returns queryVec.
	s.memoryEmbedder = &constEmbedder{vec: queryVec}

	// Embed each tool via controlled sequence; model must match memoryEmbedder.Model()
	// so the model-consistency check in handleDiscoverTools passes.
	s.toolEmbedsMu.Lock()
	s.toolEmbeds = make([][]float32, len(toolCatalog))
	for i, v := range vecs {
		s.toolEmbeds[i] = normalizeVec(v)
	}
	s.toolEmbedModel = s.memoryEmbedder.Model()
	s.toolEmbedsMu.Unlock()

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "find function"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	mode, _ := m["search_mode"].(string)
	if mode != "semantic" {
		t.Fatalf("expected semantic mode, got %q", mode)
	}
	matches, _ := m["matches"].([]any)
	if len(matches) == 0 {
		t.Fatal("expected matches")
	}
	top := matches[0].(map[string]any)
	// Tool at bestToolIdx should rank first — it has cosine 1.0 with the query.
	wantName := toolCatalog[bestToolIdx].Name
	gotName, _ := top["name"].(string)
	if gotName != wantName {
		t.Errorf("expected top match to be %q (cosine=1.0), got %q", wantName, gotName)
	}
}

func TestDiscoverTools_KeywordFallback_NoEmbedder(t *testing.T) {
	// No embedder set → keyword path.
	s := newTestServer(t)

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "blast radius callers"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	mode, _ := m["search_mode"].(string)
	if mode != "keyword" {
		t.Errorf("expected keyword fallback, got %q", mode)
	}
}

func TestDiscoverTools_KeywordFallback_EmbedError(t *testing.T) {
	// Tool catalog is embedded (ready), but query embedding fails.
	// Should fall back to keyword path without error.
	emb := &constEmbedder{vec: []float32{1, 0, 0, 0}}
	s := newSemanticServer(t, emb)

	// Replace the embedder with one that fails on query embed.
	s.memoryEmbedder = &constEmbedder{err: errors.New("query embed failed")}

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "blast radius callers"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	mode, _ := m["search_mode"].(string)
	if mode != "keyword" {
		t.Errorf("expected keyword fallback on embed error, got %q", mode)
	}
}

func TestDiscoverTools_KeywordFallback_ToolEmbedsNotReady(t *testing.T) {
	// Embedder present but toolEmbeds not set (e.g., startup race).
	s := newTestServer(t)
	s.memoryEmbedder = &constEmbedder{vec: []float32{1, 0, 0, 0}}
	// toolEmbeds deliberately left nil (zero-value).

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "impact blast radius"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	mode, _ := m["search_mode"].(string)
	if mode != "keyword" {
		t.Errorf("expected keyword fallback when embeddings not ready, got %q", mode)
	}
}

func TestDiscoverTools_EmptyQuery_ReturnsCategories(t *testing.T) {
	// Empty query returns category overview regardless of embedder.
	emb := &constEmbedder{vec: []float32{1, 0, 0, 0}}
	s := newSemanticServer(t, emb)

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	if _, ok := m["categories"]; !ok {
		t.Error("empty query should return 'categories' key")
	}
}

func TestDiscoverTools_SemanticPath_KnowledgeModeFilters(t *testing.T) {
	// In knowledge mode, only knowledge tools should be returned.
	emb := &constEmbedder{vec: []float32{1, 0, 0, 0}}
	s := newSemanticServer(t, emb)
	s.knowledgeMode = true

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "remember episode"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	matches, _ := m["matches"].([]any)
	for i, match := range matches {
		mm, _ := match.(map[string]any)
		name, _ := mm["name"].(string)
		if !knowledgeTools[name] {
			t.Errorf("matches[%d] name=%q is not a knowledge tool", i, name)
		}
	}
}

func TestDiscoverTools_SemanticPath_NegativeSimilarityFallsBackToKeyword(t *testing.T) {
	// When all tool-query cosine similarities are ≤ 0 (orthogonal or opposite),
	// the semantic path should fall through to keyword rather than returning
	// irrelevant results labeled "semantic".
	//
	// Setup: query vector = [1,0,0,0]; all tool embeddings = [-1,0,0,0].
	// dot([1,0,0,0], [-1,0,0,0]) = -1.0  → all scores negative.
	s := newTestServer(t)
	s.memoryEmbedder = &constEmbedder{vec: []float32{1, 0, 0, 0}} // query vec

	s.toolEmbedsMu.Lock()
	s.toolEmbeds = make([][]float32, len(toolCatalog))
	for i := range s.toolEmbeds {
		s.toolEmbeds[i] = normalizeVec([]float32{-1, 0, 0, 0})
	}
	s.toolEmbedModel = s.memoryEmbedder.Model() // model matches: "test-const"
	s.toolEmbedsMu.Unlock()

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "blast radius callers"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	mode, _ := m["search_mode"].(string)
	if mode != "keyword" {
		t.Errorf("expected keyword fallback when all similarities ≤ 0, got %q", mode)
	}
}

func TestDiscoverTools_KeywordFallback_ModelMismatch(t *testing.T) {
	// Tool embeddings were built with model "old-model" but the current embedder
	// reports "new-model". The model-consistency check must force keyword fallback
	// so we never compare vectors from different embedding spaces.
	s := newTestServer(t)
	s.memoryEmbedder = &constEmbedder{vec: []float32{1, 0, 0, 0}} // Model() = "test-const"

	// Manually inject embeddings built by a different model.
	s.toolEmbedsMu.Lock()
	s.toolEmbeds = make([][]float32, len(toolCatalog))
	for i := range s.toolEmbeds {
		s.toolEmbeds[i] = normalizeVec([]float32{1, 0, 0, 0})
	}
	s.toolEmbedModel = "old-model" // does NOT match memoryEmbedder.Model() = "test-const"
	s.toolEmbedsMu.Unlock()

	res, err := s.handleDiscoverTools(ctx, callTool(map[string]any{"query": "blast radius callers"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := mustResult(t, res, nil)
	mode, _ := m["search_mode"].(string)
	if mode != "keyword" {
		t.Errorf("expected keyword fallback on model mismatch, got %q", mode)
	}
}

// TestDiscoverTools_SetMemoryEmbedder_TriggersBackgroundEmbed verifies that
// SetMemoryEmbedder launches EmbedToolCatalog in background. We check that
// after a brief synchronization point (using the const embedder), toolEmbeds
// eventually becomes ready.
func TestDiscoverTools_SetMemoryEmbedder_TriggersBackgroundEmbed(t *testing.T) {
	s := newTestServer(t)
	emb := &constEmbedder{vec: []float32{1, 0, 0, 0}}
	s.SetMemoryEmbedder(emb)

	// Poll until EmbedToolCatalog goroutine finishes or timeout (3s).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.toolEmbedsMu.RLock()
		ready := len(s.toolEmbeds) == len(toolCatalog)
		s.toolEmbedsMu.RUnlock()
		if ready {
			return // success
		}
		time.Sleep(time.Millisecond)
	}
	t.Error("tool catalog not embedded within 3s after SetMemoryEmbedder call")
}
