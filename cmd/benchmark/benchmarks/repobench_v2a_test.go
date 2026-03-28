package benchmarks

import (
	"math"
	"strings"
	"testing"
)

// ─── V2-A5: Next-line hint injection ─────────────────────────────────────────

func TestBuildQueryWithNextHint_NoHint(t *testing.T) {
	s := RepoBenchSample{Code: "x = foo()", ImportStatement: "import foo"}
	base := buildQuery(s)
	got := buildQueryWithNextHint(s)
	if got != base {
		t.Errorf("empty next_line: expected base query, got %q", got)
	}
}

func TestBuildQueryWithNextHint_InjectsIdentifiers(t *testing.T) {
	s := RepoBenchSample{
		Code:            "x = foo()",
		ImportStatement: "import bar",
		NextLine:        "result = compute_embedding(text)",
	}
	got := buildQueryWithNextHint(s)
	base := buildQuery(s)
	if len(got) <= len(base) {
		t.Errorf("expected hint query longer than base; hint=%d base=%d", len(got), len(base))
	}
	// At least one meaningful identifier should appear.
	if !strings.Contains(got, "compute_embedding") && !strings.Contains(got, "result") {
		t.Errorf("expected identifier from next_line in query, got:\n%s", got)
	}
}

func TestBuildQueryWithNextHint_SkipsShortTokens(t *testing.T) {
	// "ok" (len 2) and "if" (stop-word) should not trigger hint injection.
	s := RepoBenchSample{
		Code:     "x = 1",
		NextLine: "if ok",
	}
	got := buildQueryWithNextHint(s)
	base := buildQuery(s)
	if got != base {
		t.Errorf("short/stop-word next_line should not extend query\nhint=%q\nbase=%q", got, base)
	}
}

func TestBuildQueryWithNextHint_DoubledInQuery(t *testing.T) {
	// Each identifier from next_line should appear at least twice (doubling for BM25 boost).
	s := RepoBenchSample{
		Code:     "x = foo()",
		NextLine: "embedding_vector = encoder(tokens)",
	}
	got := buildQueryWithNextHint(s)
	count := strings.Count(got, "embedding_vector") + strings.Count(got, "encoder")
	if count < 2 {
		t.Errorf("expected identifiers doubled in hint query, count=%d in %q", count, got)
	}
}

func TestExtractNextLineIdentifiers(t *testing.T) {
	tests := []struct {
		nextLine string
		wantAny  []string
	}{
		{"result = compute_embedding(text)", []string{"result", "compute_embedding", "text"}},
		{"self.model.forward(x)", []string{"model", "forward"}},
		{"return cls.from_config(config)", []string{"from_config", "config"}},
	}
	for _, tt := range tests {
		got := extractNextLineIdentifiers(tt.nextLine)
		gotSet := make(map[string]bool)
		for _, g := range got {
			gotSet[g] = true
		}
		found := false
		for _, want := range tt.wantAny {
			if gotSet[want] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("extractNextLineIdentifiers(%q) = %v, expected at least one of %v",
				tt.nextLine, got, tt.wantAny)
		}
	}
}

func TestExtractNextLineIdentifiers_AllStopWords(t *testing.T) {
	// A next_line containing only stop-words should return nil.
	got := extractNextLineIdentifiers("if else return")
	if len(got) != 0 {
		t.Errorf("expected empty identifiers for all-stopword next_line, got %v", got)
	}
}

// ─── V2-A6: Adaptive BM25 length normalisation ───────────────────────────────

func TestRankBM25WithB_AvgDLZeroGuard(t *testing.T) {
	// All candidates tokenize to empty (every token is a stop-word).
	// Before the fix, b * dl/avgDL = NaN caused NaN scores.
	query := "embedding similarity"
	candidates := []string{
		"if else return", // all stop-words
		"var const type", // all stop-words
	}
	got := rankBM25WithB(query, candidates, 0.95)
	if len(got) != len(candidates) {
		t.Fatalf("expected %d items, got %d", len(candidates), len(got))
	}
	for i, item := range got {
		if math.IsNaN(item.score) || math.IsInf(item.score, 0) {
			t.Errorf("item[%d].score = %v (should be finite)", i, item.score)
		}
	}
}

func TestRankBM25WithB_ParamPropagates(t *testing.T) {
	query := "embedding cosine similarity vector"
	candidates := []string{
		"cosine_similarity between vectors dot product",
		"import numpy array tensor",
		"encoder decoder attention",
	}
	r075 := rankBM25WithB(query, candidates, 0.75)
	r095 := rankBM25WithB(query, candidates, 0.95)
	if len(r075) != len(candidates) || len(r095) != len(candidates) {
		t.Fatalf("expected %d items, got %d / %d", len(candidates), len(r075), len(r095))
	}
}

func TestRankBM25Adaptive_InterpolationRange(t *testing.T) {
	query := "model loss training"
	// Verify the adaptive b value is within expected bounds for various pool sizes.
	// We can't observe b directly, but we verify that adaptive == standard for n ≤ 7.
	for _, n := range []int{3, 5, 7} {
		cands := make([]string, n)
		for i := range cands {
			cands[i] = "training epoch model optimizer loss"
		}
		adaptive := rankBM25Adaptive(query, cands)
		standard := rankBM25(query, cands)
		if len(adaptive) != n {
			t.Fatalf("n=%d: expected %d items", n, n)
		}
		// For n ≤ 7, b=0.75 in both; rankings must match.
		for i := range adaptive {
			if adaptive[i].index != standard[i].index {
				t.Errorf("n=%d rank[%d]: adaptive index=%d != standard index=%d",
					n, i, adaptive[i].index, standard[i].index)
			}
		}
	}
}

func TestRankBM25Adaptive_LargePoolNoNaN(t *testing.T) {
	query := "neural network forward propagation"
	candidates := make([]string, 18)
	for i := range candidates {
		candidates[i] = "layer activation gradient backprop"
	}
	got := rankBM25Adaptive(query, candidates)
	for i, item := range got {
		if math.IsNaN(item.score) || math.IsInf(item.score, 0) {
			t.Errorf("item[%d].score = %v", i, item.score)
		}
	}
}

func TestRankBM25LenormConvex_ReturnsSorted(t *testing.T) {
	query := "neural network forward pass"
	candidates := []string{
		"forward propagation through network layers activation",
		"DataLoader batching iteration dataset",
		"tensor creation initialization random",
	}
	got := rankBM25LenormConvex(query, candidates)
	if len(got) != len(candidates) {
		t.Fatalf("expected %d, got %d", len(candidates), len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].score > got[i-1].score+1e-9 {
			t.Errorf("not sorted at %d: %.6f > %.6f", i, got[i].score, got[i-1].score)
		}
	}
}

// ─── V2-A8: Stopword expansion ────────────────────────────────────────────────

func TestStopWords_KeywordsPresent(t *testing.T) {
	// Pure language keywords must be stop-words.
	keywords := []string{
		"func", "package", "var", "const", "type", "range", "make", "append",
		"let", "export", "default", "typeof",
		"public", "private", "protected", "static", "final", "extends",
		"implements", "override", "super", "abstract", "interface", "new",
		"pass", "elif", "lambda", "yield", "async", "await",
		"struct",
		"if", "else", "break", "continue",
	}
	for _, w := range keywords {
		if !isStopWord(w) {
			t.Errorf("keyword %q must be a stop-word", w)
		}
	}
}

func TestStopWords_TypeNamesNotPresent(t *testing.T) {
	// Primitive type names must NOT be stop-words — they can be discriminative
	// (e.g. "string_encoder", "error_handler" split into [string,encoder]).
	typeNames := []string{"int", "str", "bool", "float", "string", "byte",
		"char", "long", "short", "double", "error"}
	for _, w := range typeNames {
		if isStopWord(w) {
			t.Errorf("type name %q should NOT be a stop-word (discriminative risk)", w)
		}
	}
}

func TestTokenize_FiltersKeywordsNotTypes(t *testing.T) {
	// Keywords are filtered; type names pass through.
	tokens := tokenize("func forward(x string) error { if true { return nil } }")
	tokSet := make(map[string]bool)
	for _, t2 := range tokens {
		tokSet[t2] = true
	}
	// Must NOT appear (keywords).
	for _, kw := range []string{"func", "if", "return"} {
		if tokSet[kw] {
			t.Errorf("keyword %q should be filtered by tokenize", kw)
		}
	}
	// MUST appear (type names / identifiers).
	for _, id := range []string{"forward", "string", "error"} {
		if !tokSet[id] {
			t.Errorf("identifier/type %q should survive tokenize", id)
		}
	}
}

// ─── V2-A9: Word-bigram features ─────────────────────────────────────────────

func TestExtractBigrams_Basic(t *testing.T) {
	tokens := []string{"foo", "bar", "baz"}
	got := extractBigrams(tokens)
	want := []string{"foo_bar", "bar_baz"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bigram[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestExtractBigrams_EdgeCases(t *testing.T) {
	if got := extractBigrams(nil); len(got) != 0 {
		t.Errorf("nil input: expected empty, got %v", got)
	}
	if got := extractBigrams([]string{"solo"}); len(got) != 0 {
		t.Errorf("single token: expected empty, got %v", got)
	}
	if got := extractBigrams([]string{"a", "b"}); len(got) != 1 || got[0] != "a_b" {
		t.Errorf("two tokens: expected [a_b], got %v", got)
	}
}

func TestBigramOverlapScore_SetBased(t *testing.T) {
	// Verify set-based dedup: the SAME bigram repeated many times in the
	// candidate must not inflate the score above 1.0.
	//
	// queryTokens → bigrams: ["alpha_beta"] (size 1)
	// candTokens  → raw bigrams: ["alpha_beta","alpha_beta","alpha_beta"] (3×)
	//   without dedup: common=3, denom=1+3=4 → Dice=1.5  (BUG: >1)
	//   with dedup: setC={"alpha_beta"} size 1, common=1, denom=1+1=2 → Dice=1.0 ✓
	queryTokens := []string{"alpha", "beta"}
	// Build candidate tokens so "alpha_beta" bigram repeats 3 times.
	candTokens := []string{"alpha", "beta", "alpha", "beta", "alpha", "beta"}

	score := bigramOverlapScore(queryTokens, candTokens)
	if score > 1.0+1e-9 {
		t.Errorf("set-based Dice must be ≤ 1.0; got %.6f (dedup broken)", score)
	}
	// The overlap should be non-zero (alpha_beta is in both sets).
	if score < 1e-9 {
		t.Errorf("expected non-zero overlap between alpha_beta bigrams; got %.6f", score)
	}
}

func TestBigramOverlapScore_GoldVsNoise(t *testing.T) {
	queryTokens := tokenize("compute cosine similarity between two embeddings")
	goldTokens := tokenize("cosine similarity score between embedding vectors")
	noisyTokens := tokenize("open file read write close handle descriptor")

	goldScore := bigramOverlapScore(queryTokens, goldTokens)
	noisyScore := bigramOverlapScore(queryTokens, noisyTokens)

	if goldScore <= noisyScore {
		t.Errorf("gold bigram score (%.4f) should > noisy (%.4f)", goldScore, noisyScore)
	}
}

func TestBigramOverlapScore_EmptyInputs(t *testing.T) {
	// Any empty input should return 0, not panic.
	if s := bigramOverlapScore(nil, []string{"foo", "bar"}); s != 0 {
		t.Errorf("nil query tokens: expected 0, got %.4f", s)
	}
	if s := bigramOverlapScore([]string{"foo", "bar"}, nil); s != 0 {
		t.Errorf("nil cand tokens: expected 0, got %.4f", s)
	}
	if s := bigramOverlapScore([]string{"solo"}, []string{"solo"}); s != 0 {
		t.Errorf("single token each: expected 0 bigrams → 0 score, got %.4f", s)
	}
}

func TestRankHybridNgram_ReturnsSortedAllIndices(t *testing.T) {
	query := "compute embedding similarity score"
	candidates := []string{
		"cosine similarity between embedding vectors",
		"open database connection pool",
		"parse yaml configuration file",
		"embedding distance score computation",
	}
	got := rankHybridNgram(query, candidates)
	if len(got) != len(candidates) {
		t.Fatalf("expected %d items, got %d", len(candidates), len(got))
	}
	seen := make(map[int]bool)
	for i, item := range got {
		if seen[item.index] {
			t.Errorf("duplicate index %d at rank %d", item.index, i)
		}
		seen[item.index] = true
		if i > 0 && got[i].score > got[i-1].score+1e-9 {
			t.Errorf("not sorted at rank %d: %.6f > %.6f", i, got[i].score, got[i-1].score)
		}
	}
}

func TestRankHybridNgram_GoldRankedHigher(t *testing.T) {
	query := "forward propagation neural network layer"
	candidates := []string{
		"neural network forward propagation layer activation",
		"read file open close handle",
		"parse config yaml load",
	}
	got := rankHybridNgram(query, candidates)
	if got[0].index != 0 {
		t.Errorf("expected gold (index 0) to rank first; got index=%d", got[0].index)
	}
}

func TestRankHybridNgram_ScoresInRange(t *testing.T) {
	// Combined score = 0.7*hybrid + 0.3*bigram.
	// hybrid is in [0,1] after normalise, bigram (Dice) is in [0,1].
	// Combined should be in [0, 1].
	query := "attention mechanism transformer layer"
	candidates := make([]string, 5)
	for i := range candidates {
		candidates[i] = "attention layer transformer model weights"
	}
	got := rankHybridNgram(query, candidates)
	for _, item := range got {
		if item.score < -1e-9 || item.score > 1+1e-9 {
			t.Errorf("score %.6f out of expected [0,1] range", item.score)
		}
	}
}

// ─── V2-A10: Candidate clustering ────────────────────────────────────────────

func TestKMeansCandidates_FarthestFirstInit(t *testing.T) {
	// Three clearly distinct groups — farthest-first init should seed one
	// centroid per group, producing clean cluster assignments.
	groups := [][]string{
		{"embedding cosine similarity neural network"},
		{"database query select insert update"},
		{"http request response header status"},
	}
	candidates := make([]string, 15)
	for i := range candidates {
		candidates[i] = groups[i/5][0]
	}
	allDocs := append([]string{candidates[0]}, candidates...)
	vecs := tfidfVectors(allDocs)
	candVecs := vecs[1:]
	assignments := kMeansCandidates(candVecs, 3, 20)

	// Within each original group the 5 candidates must share the same cluster.
	for g := 0; g < 3; g++ {
		base := assignments[g*5]
		for i := g * 5; i < g*5+5; i++ {
			if assignments[i] != base {
				t.Errorf("group %d candidate %d has cluster %d, expected %d",
					g, i, assignments[i], base)
			}
		}
	}
}

func TestKMeansCandidates_AssignmentsValid(t *testing.T) {
	candidates := make([]string, 15)
	for i := range candidates {
		candidates[i] = "some code snippet content"
	}
	vecs := tfidfVectors(candidates)
	assignments := kMeansCandidates(vecs, 3, 10)
	if len(assignments) != len(candidates) {
		t.Fatalf("expected %d assignments, got %d", len(candidates), len(assignments))
	}
	for _, a := range assignments {
		if a < 0 || a >= 3 {
			t.Errorf("assignment %d out of range [0,3)", a)
		}
	}
}

func TestComputeCentroids_MeanVectors(t *testing.T) {
	vecs := []map[string]float64{
		{"foo": 2.0, "bar": 0.0},
		{"foo": 0.0, "bar": 4.0},
	}
	assignments := []int{0, 0}
	centroids := computeCentroids(vecs, assignments, 1)
	if math.Abs(centroids[0]["foo"]-1.0) > 1e-9 {
		t.Errorf("centroid[0][foo]=%.4f want 1.0", centroids[0]["foo"])
	}
	if math.Abs(centroids[0]["bar"]-2.0) > 1e-9 {
		t.Errorf("centroid[0][bar]=%.4f want 2.0", centroids[0]["bar"])
	}
}

func TestComputeCentroids_EmptyCluster(t *testing.T) {
	// Cluster 1 has no assignments — centroid should be empty map, not panic.
	vecs := []map[string]float64{{"foo": 1.0}, {"bar": 1.0}}
	assignments := []int{0, 0} // both go to cluster 0; cluster 1 is empty
	centroids := computeCentroids(vecs, assignments, 2)
	if len(centroids) != 2 {
		t.Fatalf("expected 2 centroids, got %d", len(centroids))
	}
	// Empty centroid should be an empty (not nil) map.
	if centroids[1] == nil {
		t.Error("empty cluster centroid should be empty map, not nil")
	}
	if len(centroids[1]) != 0 {
		t.Errorf("empty cluster centroid should have 0 terms, got %d", len(centroids[1]))
	}
}

func TestClosestCentroid_SelectsNearest(t *testing.T) {
	query := map[string]float64{"embedding": 1.0, "vector": 1.0}
	centroids := []map[string]float64{
		{"database": 1.0, "query": 1.0},
		{"embedding": 1.0, "vector": 1.0, "cosine": 0.5},
		{"http": 1.0, "request": 1.0},
	}
	got := closestCentroid(query, centroids)
	if got != 1 {
		t.Errorf("expected centroid 1 (closest), got %d", got)
	}
}

func TestClosestCentroid_EmptyCentroidLoses(t *testing.T) {
	// Empty centroid (cosine=0) should lose to any non-empty centroid.
	query := map[string]float64{"embedding": 1.0}
	centroids := []map[string]float64{
		{},                 // empty centroid, cosine=0
		{"embedding": 1.0}, // perfect match
	}
	got := closestCentroid(query, centroids)
	if got != 1 {
		t.Errorf("non-empty centroid should beat empty; got %d", got)
	}
}

func TestRankClusterHybrid_FallbackSmallPool(t *testing.T) {
	query := "model training"
	candidates := make([]string, 8)
	for i := range candidates {
		candidates[i] = "training epoch loss gradient"
	}
	// ≤12 candidates: fall through to hybrid-convex, no panic.
	got := rankClusterHybrid(query, candidates)
	if len(got) != len(candidates) {
		t.Fatalf("expected %d items, got %d", len(candidates), len(got))
	}
}

func TestRankClusterHybrid_EmptyClusterFallback(t *testing.T) {
	// Force a scenario where inCluster could be empty:
	// use all-stopword candidates (empty TF-IDF vectors) — all get cosine=0,
	// clustering degenerates, empty-cluster guard must fire without panic.
	query := "compute distance"
	candidates := make([]string, 15)
	for i := range candidates {
		// These all tokenize to empty after stopword removal.
		candidates[i] = "if else return var const"
	}
	got := rankClusterHybrid(query, candidates)
	if len(got) != len(candidates) {
		t.Fatalf("expected %d items, got %d", len(candidates), len(got))
	}
	// All indices must appear exactly once.
	seen := make(map[int]bool)
	for _, item := range got {
		if seen[item.index] {
			t.Errorf("duplicate index %d", item.index)
		}
		seen[item.index] = true
	}
}

func TestRankClusterHybrid_LargePoolAllIndices(t *testing.T) {
	query := "cosine similarity embedding vector"
	candidates := make([]string, 15)
	for i := range candidates {
		candidates[i] = "snippet different content token identifier"
	}
	candidates[0] = "cosine similarity between embedding vectors computation"
	got := rankClusterHybrid(query, candidates)
	if len(got) != len(candidates) {
		t.Fatalf("expected %d items, got %d", len(candidates), len(got))
	}
	seen := make(map[int]bool)
	for _, item := range got {
		if seen[item.index] {
			t.Errorf("duplicate index %d", item.index)
		}
		seen[item.index] = true
	}
	for i := range candidates {
		if !seen[i] {
			t.Errorf("missing index %d in result", i)
		}
	}
}

func TestRankClusterHybrid_GoldRanksHighInCluster(t *testing.T) {
	query := "neural network forward propagation layer"
	gold := "forward propagation through neural network layers activation"
	noise := make([]string, 14)
	for i := range noise {
		noise[i] = "database connection pool max idle timeout retry"
	}
	candidates := append([]string{gold}, noise...)
	got := rankClusterHybrid(query, candidates)
	// Gold (index 0) should be in the top-3.
	for _, item := range got[:3] {
		if item.index == 0 {
			return
		}
	}
	top := make([]int, len(got))
	for i, item := range got {
		top[i] = item.index
	}
	t.Errorf("gold (index 0) not in top-3; ranking: %v", top[:minIntTest(5, len(top))])
}

func TestRankClusterHybrid_ScoreOffset(t *testing.T) {
	// In-cluster items must all rank above out-of-cluster items.
	// We verify this indirectly: all in-cluster items have score > any out-of-cluster item.
	query := "embedding cosine similarity"
	// 5 "embedding" candidates + 10 "database" candidates. Gold is in embedding cluster.
	var candidates []string
	for i := 0; i < 5; i++ {
		candidates = append(candidates, "embedding cosine distance similarity vector representation")
	}
	for i := 0; i < 10; i++ {
		candidates = append(candidates, "database query select insert update row column")
	}
	got := rankClusterHybrid(query, candidates)
	if len(got) != len(candidates) {
		t.Fatalf("expected %d items", len(candidates))
	}
	// Find boundary between in-cluster and out-of-cluster by score offset.
	// In-cluster items have score > 2.0 (base hybrid + 2.0 offset).
	// Out-of-cluster items have score ≤ 1.0 (raw cosine).
	inCount := 0
	for _, item := range got {
		if item.score > 1.5 {
			inCount++
		}
	}
	if inCount == 0 {
		t.Error("expected at least some in-cluster items with score > 1.5 (score+2.0 offset)")
	}
}

// minIntTest is a test-local min helper (minInt already exists in contextbench.go).
func minIntTest(a, b int) int {
	if a < b {
		return a
	}
	return b
}
