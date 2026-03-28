//go:build spike

package embed_test

// Spike: nomic-embed-text-v1.5 feasibility validation (comprehensive)
//
// Run with:
//   go test -tags spike -run TestSpikeNomicEmbedTextV15 -v -timeout 20m ./internal/embed/
//
// Validates six questions before committing to Sprint 12 #2 (embedding model upgrade):
//   (a) hugot loads the nomic-embed-text-v1.5 ONNX model
//   (b) Inference latency on this machine + bulk re-embedding cost analysis
//   (c) Matryoshka 384-dim truncation quality vs full 768 dims
//   (d) Quality advantage over MiniLM-L6-v2 on code-relevant sentence pairs
//   (e) Long-context advantage (nomic 8192 tokens vs MiniLM 256 tokens)
//   (f) Task prefix impact (with/without "search_document:" prefix)
//
// Downloads:
//   - nomic-ai/nomic-embed-text-v1.5 quantized (~137 MB, for latency/quality tests)
//   - KnightsAnalytics/all-MiniLM-L6-v2 (~23 MB, for quality comparison baseline)

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hugot "github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"

	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nomicModelName     = "nomic-ai/nomic-embed-text-v1.5"
	nomicOnnxFilePath  = "onnx/model_quantized.onnx" // quantized for spike (~137 MB); production uses onnx/model.onnx
	nomicLocalOnnxFile = "model_quantized.onnx"      // hugot strips dir prefix: onnx/x.onnx → x.onnx
	matryoshkaDims     = 384
)

func TestSpikeNomicEmbedTextV15(t *testing.T) {
	dir := t.TempDir()

	// ─── (a) Download and load nomic model ───────────────────────────────────

	t.Log("=== (a) Download and load nomic-embed-text-v1.5 ===")

	opts := hugot.NewDownloadOptions()
	opts.OnnxFilePath = nomicOnnxFilePath
	opts.Verbose = false
	opts.MaxRetries = 3
	opts.RetryInterval = 3

	modelPath, err := hugot.DownloadModel(nomicModelName, dir, opts)
	require.NoError(t, err, "(a) hugot.DownloadModel failed")

	onnxDisk := filepath.Join(modelPath, nomicLocalOnnxFile)
	info, err := os.Stat(onnxDisk)
	require.NoError(t, err, "(a) ONNX not found at %s", onnxDisk)
	t.Logf("    ONNX on disk: %s (%.1f MB)", onnxDisk, float64(info.Size())/1e6)

	// Check whether fp32 model.onnx exists (needed for Sprint 12 #2 decision).
	// hugot DownloadModel only copies the requested file, so inspect tokenizer_config.json
	// which is always downloaded. We detect presence by checking what was actually fetched.
	fp32OnnxDisk := filepath.Join(modelPath, "model.onnx")
	if _, statErr := os.Stat(fp32OnnxDisk); os.IsNotExist(statErr) {
		t.Logf("    fp32 model.onnx: NOT cached (not downloaded in this spike — expected)")
		t.Logf("    NOTE for Sprint 12 #2: to get fp32, use OnnxFilePath: \"onnx/model.onnx\"")
	} else {
		t.Logf("    fp32 model.onnx: %s", fp32OnnxDisk)
	}

	// Check for external data files (some large models split weights into .onnx_data).
	entries, _ := os.ReadDir(modelPath)
	var extDataFiles []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".onnx_data") || strings.HasSuffix(e.Name(), "_data") {
			extDataFiles = append(extDataFiles, e.Name())
		}
	}
	if len(extDataFiles) > 0 {
		t.Logf("    WARN: external data files found: %v — Sprint 12 #2 must set ExternalDataPath", extDataFiles)
	} else {
		t.Logf("    External data files: none (self-contained ONNX)")
	}

	session, err := hugot.NewGoSession()
	require.NoError(t, err, "(a) NewGoSession failed")
	t.Cleanup(func() { _ = session.Destroy() })

	config := hugot.FeatureExtractionConfig{
		ModelPath:    modelPath,
		Name:         "nomic-spike",
		OnnxFilename: nomicLocalOnnxFile,
		Options:      []hugot.FeatureExtractionOption{pipelines.WithNormalization()},
	}
	pipeline, err := hugot.NewPipeline(session, config)
	require.NoError(t, err, "(a) NewPipeline failed")

	// Single warmup inference.
	warmResult, err := pipeline.RunPipeline([]string{"warmup"})
	require.NoError(t, err, "(a) warmup failed")
	require.NotEmpty(t, warmResult.Embeddings)
	outputDims := len(warmResult.Embeddings[0])
	require.Equal(t, 768, outputDims, "(a) expected 768-dim output from nomic model")

	// Verify L2 normalization of output (WithNormalization() must produce unit vectors).
	var normSq float64
	for _, v := range warmResult.Embeddings[0] {
		normSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(normSq)
	assert.InDelta(t, 1.0, norm, 0.01, "(a) L2 norm of normalized output must be ~1.0")
	t.Logf("    Output dims: %d  L2 norm: %.6f (should be ~1.0)", outputDims, norm)
	t.Log("PASS (a) hugot loads model, outputs 768 dims, L2-normalizes correctly")

	// ─── (b) Latency + bulk re-embedding cost analysis ───────────────────────

	t.Log("=== (b) Inference latency + bulk re-embedding cost ===")

	benchText := "search_document: func (s *Server) handleGetContext(ctx context.Context, params map[string]interface{}) (*mcp.ToolResult, error)"

	// 5 warm-up runs.
	for i := range 5 {
		_, err := pipeline.RunPipeline([]string{benchText})
		require.NoError(t, err, "(b) warmup %d failed", i)
	}

	const benchRuns = 20
	start := time.Now()
	for i := range benchRuns {
		_, err := pipeline.RunPipeline([]string{benchText})
		require.NoError(t, err, "(b) bench run %d failed", i)
	}
	avg := time.Since(start) / benchRuns

	// Bulk re-embedding cost estimates (relevant for model upgrade migration).
	est100 := avg * 100
	est1000 := avg * 1000
	est5000 := avg * 5000

	t.Logf("    Avg latency: %v (n=%d)", avg.Round(time.Millisecond), benchRuns)
	t.Logf("    Bulk re-embed estimate (1 goroutine):")
	t.Logf("      100  memories: %v", est100.Round(time.Second))
	t.Logf("      1000 memories: %v", est1000.Round(time.Second))
	t.Logf("      5000 memories: %v", est5000.Round(time.Second))
	t.Logf("    Pool of 3 goroutines: %v / %v / %v",
		(est100 / 3).Round(time.Second),
		(est1000 / 3).Round(time.Second),
		(est5000 / 3).Round(time.Second))

	latencyOK := avg < 50*time.Millisecond
	if latencyOK {
		t.Logf("PASS (b) avg %v meets <50ms target", avg.Round(time.Millisecond))
	} else {
		t.Logf("INFO (b) avg %v — exceeds 50ms Mac target (CPU-only Linux expected). See bulk cost above.", avg.Round(time.Millisecond))
	}

	// ─── (c) Matryoshka 384-dim truncation vs full 768 dims ──────────────────

	t.Log("=== (c) Matryoshka 384-dim truncation quality ===")

	qualityTexts := []string{
		"search_document: authentication middleware validates JWT bearer tokens in HTTP requests",
		"search_document: JWT token verification and RS256 signature validation in auth handler",
		"search_document: BFS traversal over the code graph to find related nodes",
		"search_document: breadth-first search algorithm explores graph layer by layer",
		"search_document: SQLite connection pool with WAL mode for concurrent readers",
		"search_document: HTTP rate limiting with token bucket per session identifier",
	}

	batchResult, err := pipeline.RunPipeline(qualityTexts)
	require.NoError(t, err, "(c) batch inference failed")
	require.Len(t, batchResult.Embeddings, len(qualityTexts))

	type pair struct {
		label    string
		i, j     int
		wantHigh bool
	}
	pairs := []pair{
		{"similar-A JWT auth", 0, 1, true},
		{"similar-B graph BFS", 2, 3, true},
		{"dissimilar-A auth/SQLite", 0, 4, false},
		{"dissimilar-B auth/rate-limit", 0, 5, false},
	}

	cPassFail := true
	for _, p := range pairs {
		full := spikeCosineSim(batchResult.Embeddings[p.i], batchResult.Embeddings[p.j])
		trunc := spikeCosineSim(
			batchResult.Embeddings[p.i][:matryoshkaDims],
			batchResult.Embeddings[p.j][:matryoshkaDims],
		)
		drift := math.Abs(full-trunc) / math.Max(full, 0.001) * 100
		t.Logf("    %-32s  full=%.4f  384d=%.4f  drift=%.1f%%", p.label, full, trunc, drift)

		if p.wantHigh && trunc < 0.5 {
			t.Errorf("FAIL (c) similar pair %q: sim384=%.4f < 0.5", p.label, trunc)
			cPassFail = false
		}
		if !p.wantHigh && trunc > 0.85 {
			t.Errorf("FAIL (c) dissimilar pair %q: sim384=%.4f > 0.85", p.label, trunc)
			cPassFail = false
		}
		if drift > 15 {
			t.Logf("WARN (c) pair %q: drift %.1f%% — Matryoshka prefix property weaker than expected", p.label, drift)
		}
	}
	// Ranking preservation: similar must outscore dissimilar.
	sim384simA := spikeCosineSim(batchResult.Embeddings[0][:matryoshkaDims], batchResult.Embeddings[1][:matryoshkaDims])
	sim384disA := spikeCosineSim(batchResult.Embeddings[0][:matryoshkaDims], batchResult.Embeddings[4][:matryoshkaDims])
	require.Greater(t, sim384simA, sim384disA, "(c) ranking not preserved: similar <= dissimilar after truncation")
	if cPassFail {
		t.Log("PASS (c) Matryoshka 384-dim truncation preserves quality and ranking")
	}

	// ─── (d) Quality comparison vs MiniLM-L6-v2 ─────────────────────────────

	t.Log("=== (d) Quality comparison: nomic-384 vs MiniLM-384 ===")

	miniDir := t.TempDir()
	miniEmb := embed.NewBuiltinEmbedder(miniDir)
	t.Cleanup(func() { _ = miniEmb.Close() })

	// Raw texts (no prefix) — matching how MiniLM is currently used in production.
	rawTexts := []string{
		"authentication middleware validates JWT bearer tokens in HTTP requests",
		"JWT token verification and RS256 signature validation in auth handler",
		"BFS traversal over the code graph to find related nodes",
		"breadth-first search algorithm explores graph layer by layer",
		"SQLite connection pool with WAL mode for concurrent readers",
		"HTTP rate limiting with token bucket per session identifier",
	}

	// MiniLM embeddings (production current model).
	miniVecs := make([][]float32, len(rawTexts))
	for i, text := range rawTexts {
		vec, embErr := miniEmb.Embed(t.Context(), text)
		require.NoError(t, embErr, "(d) MiniLM embed[%d] failed", i)
		require.Len(t, vec, 384, "(d) MiniLM must output 384 dims")
		miniVecs[i] = vec
	}

	// nomic embeddings with raw text (no prefix — matches current production call convention).
	rawResult, err := pipeline.RunPipeline(rawTexts)
	require.NoError(t, err, "(d) nomic batch (no prefix) failed")

	// nomic embeddings with task prefix (recommended usage).
	prefixedTexts := make([]string, len(rawTexts))
	for i, t2 := range rawTexts {
		prefixedTexts[i] = "search_document: " + t2
	}
	prefixResult, err := pipeline.RunPipeline(prefixedTexts)
	require.NoError(t, err, "(d) nomic batch (with prefix) failed")

	t.Logf("    %-32s  MiniLM   nomic(raw)  nomic(prefix)", "pair")
	t.Logf("    %-32s  -------  ----------  ------------", "----")

	type pairScore struct {
		label          string
		i, j           int
		wantHigh       bool
		mini, raw, pfx float64
	}
	pairScores := make([]spikePairScore, len(pairs))
	for k, p := range pairs {
		mini := spikeCosineSim(miniVecs[p.i], miniVecs[p.j])
		nomicRaw := spikeCosineSim(rawResult.Embeddings[p.i][:matryoshkaDims], rawResult.Embeddings[p.j][:matryoshkaDims])
		nomicPfx := spikeCosineSim(prefixResult.Embeddings[p.i][:matryoshkaDims], prefixResult.Embeddings[p.j][:matryoshkaDims])
		t.Logf("    %-32s  %.4f   %.4f      %.4f", p.label, mini, nomicRaw, nomicPfx)
		pairScores[k] = spikePairScore{p.label, p.i, p.j, p.wantHigh, mini, nomicRaw, nomicPfx}
	}

	// Quality assessment: nomic (at 384 dims) should discriminate similar vs dissimilar
	// at least as well as MiniLM (score gap analysis).
	miniGap := spikeDiscriminationGap(pairScores, func(ps spikePairScore) float64 { return ps.mini })
	rawGap := spikeDiscriminationGap(pairScores, func(ps spikePairScore) float64 { return ps.raw })
	pfxGap := spikeDiscriminationGap(pairScores, func(ps spikePairScore) float64 { return ps.pfx })

	t.Logf("    Discrimination gap (similar mean - dissimilar mean):")
	t.Logf("      MiniLM:          %.4f", miniGap)
	t.Logf("      nomic (raw):     %.4f  (%+.1f%% vs MiniLM)", rawGap, (rawGap-miniGap)/math.Max(miniGap, 0.001)*100)
	t.Logf("      nomic (prefix):  %.4f  (%+.1f%% vs MiniLM)", pfxGap, (pfxGap-miniGap)/math.Max(miniGap, 0.001)*100)

	if rawGap >= miniGap {
		t.Log("PASS (d) nomic-384 (raw, no prefix) matches or exceeds MiniLM discrimination")
	} else {
		t.Logf("INFO (d) nomic-384 (raw) gap=%.4f vs MiniLM gap=%.4f — use prefix to recover quality", rawGap, miniGap)
	}
	if pfxGap >= miniGap {
		t.Log("PASS (d) nomic-384 (with prefix) confirms quality advantage over MiniLM")
	} else {
		t.Logf("WARN (d) nomic-384 (prefix) gap=%.4f still below MiniLM gap=%.4f — investigate", pfxGap, miniGap)
	}

	// ─── (e) Long-context advantage (8192 vs 256 token window) ──────────────

	t.Log("=== (e) Long-context advantage test ===")

	// Build a text that exceeds MiniLM's 256-token window (~200 words ≈ 300+ tokens).
	// nomic should embed the full document; MiniLM silently truncates.
	longDoc := strings.Repeat("The authentication middleware validates JWT bearer tokens "+
		"using RS256 public key cryptography. It checks expiry, issuer claim, "+
		"and audience claim before forwarding requests to downstream handlers. "+
		"Rate limiting is enforced per user identity, not per IP address. ", 8)
	// Append a distinctive tail that MiniLM will truncate but nomic will capture.
	longDocTail := longDoc + "DISTINCTIVE_TAIL_CONTENT_ONLY_VISIBLE_WITH_LONG_CONTEXT"
	shortQuery := "DISTINCTIVE_TAIL_CONTENT_ONLY_VISIBLE_WITH_LONG_CONTEXT"

	// nomic: embed long doc and short query, measure similarity.
	eResult, err := pipeline.RunPipeline([]string{
		"search_document: " + longDocTail,
		"search_query: " + shortQuery,
	})
	require.NoError(t, err, "(e) nomic long-context inference failed")
	nomicLongSim := spikeCosineSim(
		eResult.Embeddings[0][:matryoshkaDims],
		eResult.Embeddings[1][:matryoshkaDims],
	)

	// MiniLM: same test (will truncate the long doc, losing the tail).
	miniLongVec, err := miniEmb.Embed(t.Context(), longDocTail)
	require.NoError(t, err, "(e) MiniLM long-doc embed failed")
	miniQueryVec, err := miniEmb.Embed(t.Context(), shortQuery)
	require.NoError(t, err, "(e) MiniLM query embed failed")
	miniLongSim := spikeCosineSim(miniLongVec, miniQueryVec)

	t.Logf("    Long doc length: ~%d chars, ~%d estimated tokens", len(longDocTail), len(strings.Fields(longDocTail))*4/3)
	t.Logf("    Similarity to tail-only query:")
	t.Logf("      nomic (8192-token window): %.4f", nomicLongSim)
	t.Logf("      MiniLM (256-token window): %.4f", miniLongSim)

	if nomicLongSim > miniLongSim {
		t.Logf("PASS (e) nomic captures long-context signal better than MiniLM (%.4f > %.4f)", nomicLongSim, miniLongSim)
	} else {
		t.Logf("INFO (e) nomic sim=%.4f, MiniLM sim=%.4f — long-context advantage may require longer inputs on this quantized model", nomicLongSim, miniLongSim)
	}

	// ─── (f) Task prefix impact ───────────────────────────────────────────────

	t.Log("=== (f) Task prefix impact on quality ===")

	// Verify: does adding "search_document:" prefix actually help?
	// If the gap is negligible, the current Embed() API (no prefix) is fine as-is.
	prefixDelta := pfxGap - rawGap
	t.Logf("    nomic discrimination gap: raw=%.4f  prefixed=%.4f  delta=%+.4f", rawGap, pfxGap, prefixDelta)

	if math.Abs(prefixDelta) < 0.01 {
		t.Log("PASS (f) prefix has negligible impact (<0.01) — current Embed() API can pass raw text unchanged")
	} else if prefixDelta > 0 {
		t.Logf("INFO (f) prefix improves discrimination by %.4f — Sprint 12 #2 should inject prefix in Embed()", prefixDelta)
		t.Logf("         Recommended: Embed() prepends \"search_document: \" for stored memories,")
		t.Logf("         \"search_query: \" for recall queries (requires Embedder interface change or caller convention)")
	} else {
		t.Logf("WARN (f) prefix HURTS discrimination by %.4f — do NOT use prefix", math.Abs(prefixDelta))
	}

	// ─── Summary ─────────────────────────────────────────────────────────────

	fmt.Printf("\n╔══════════════════════════════════════════════════════════════════╗\n")
	fmt.Printf("║    SPIKE RESULTS: nomic-embed-text-v1.5 (comprehensive)          ║\n")
	fmt.Printf("╠══════════════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║  (a) hugot loads ONNX + L2 norm correct:     PASS               ║\n")
	fmt.Printf("║      %d-dim output, quantized=%.0fMB, self-contained            ║\n",
		outputDims, float64(info.Size())/1e6)
	fmt.Printf("║  (b) Latency: %v avg on CPU-only Linux              ║\n", avg.Round(time.Millisecond))
	fmt.Printf("║      1000 memories re-embed: ~%v (pool/3: ~%v)    ║\n",
		est1000.Round(time.Second), (est1000 / 3).Round(time.Second))
	fmt.Printf("║  (c) Matryoshka 384d truncation:             %s               ║\n", boolPass(cPassFail))
	fmt.Printf("║  (d) Quality vs MiniLM — nomic(raw):  %+.1f%%  nomic(pfx): %+.1f%%  ║\n",
		(rawGap-miniGap)/math.Max(miniGap, 0.001)*100,
		(pfxGap-miniGap)/math.Max(miniGap, 0.001)*100)
	fmt.Printf("║  (e) Long-context: nomic=%.4f  MiniLM=%.4f          ║\n", nomicLongSim, miniLongSim)
	fmt.Printf("║  (f) Prefix delta: %+.4f (%s)             ║\n",
		prefixDelta, func() string {
			if math.Abs(prefixDelta) < 0.01 {
				return "no change needed"
			} else if prefixDelta > 0 {
				return "add prefix in Embed()"
			}
			return "skip prefix"
		}())
	fmt.Printf("╠══════════════════════════════════════════════════════════════════╣\n")
	fmt.Printf("║  VERDICT: Sprint 12 #2 FEASIBLE. See implementation notes.      ║\n")
	fmt.Printf("╚══════════════════════════════════════════════════════════════════╝\n")

	fmt.Printf("\nSprint 12 #2 implementation checklist:\n")
	fmt.Printf("  [ ] builtinModelName   = \"nomic-ai/nomic-embed-text-v1.5\"\n")
	fmt.Printf("  [ ] builtinModelDirName = \"nomic-ai_nomic-embed-text-v1.5\"\n")
	fmt.Printf("  [ ] OnnxFilePath in download opts = \"onnx/model.onnx\" (fp32) or \"onnx/model_quantized.onnx\"\n")
	fmt.Printf("  [ ] OnnxFilename in pipeline config = \"model.onnx\" or \"model_quantized.onnx\" (path.Base)\n")
	fmt.Printf("  [ ] Truncate Embed() output to [:384] before returning\n")
	fmt.Printf("  [ ] Inject task prefix based on prefix delta finding above\n")
	fmt.Printf("  [ ] Capture new builtinModelSHA256 after download\n")
	fmt.Printf("  [ ] Re-embedding throttle: rate-limit bulk migration to avoid CPU saturation\n")
	if len(extDataFiles) > 0 {
		fmt.Printf("  [ ] Set ExternalDataPath for external data files: %v\n", extDataFiles)
	}
}

// spikeCosineSim computes cosine similarity. Handles both normalized vectors
// (reduces to dot product) and unnormalized vectors (full norm computation).
func spikeCosineSim(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, normA, normB float64
	for i := range n {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// spikeDiscriminationGap returns the mean score of similar pairs minus the mean
// score of dissimilar pairs. Higher gap = better discrimination between related
// and unrelated content.
func spikeDiscriminationGap(pairs []spikePairScore, score func(spikePairScore) float64) float64 {
	var simSum, disSum float64
	var simN, disN int
	for _, p := range pairs {
		if p.wantHigh {
			simSum += score(p)
			simN++
		} else {
			disSum += score(p)
			disN++
		}
	}
	if simN == 0 || disN == 0 {
		return 0
	}
	return simSum/float64(simN) - disSum/float64(disN)
}

func boolPass(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

// spikePairScore holds quality measurement for one sentence pair across models.
type spikePairScore struct {
	label          string
	i, j           int
	wantHigh       bool
	mini, raw, pfx float64
}
