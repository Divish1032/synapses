package benchmarks

import (
	"context"
	"math"
	"os"
	"path/filepath"

	"github.com/SynapsesOS/synapses/internal/embed"
)

// LocalEmbedder wraps the builtin nomic-embed-text-v1.5 ONNX model for
// direct in-process embedding — no HTTP round-trips, full pool throughput.
type LocalEmbedder struct {
	e *embed.BuiltinEmbedder
}

// NewLocalEmbedder creates and warms up a local embedder using the model
// already downloaded at ~/.synapses/models/.
func NewLocalEmbedder(poolSize int) (*LocalEmbedder, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	modelsDir := filepath.Join(homeDir, ".synapses", "models")
	var e *embed.BuiltinEmbedder
	if poolSize > 0 {
		e = embed.NewBuiltinEmbedderWithPoolSize(modelsDir, poolSize)
	} else {
		e = embed.NewBuiltinEmbedder(modelsDir)
	}
	// Warm up synchronously so first sample doesn't pay model-load cost.
	if err := e.WarmUp(context.Background()); err != nil {
		return nil, err
	}
	return &LocalEmbedder{e: e}, nil
}

// Close releases ONNX resources.
func (l *LocalEmbedder) Close() {
	if l.e != nil {
		l.e.Close() //nolint:errcheck
	}
}

// embedBatchSafe embeds texts in small batches to bound peak memory usage.
// batchSize=1 is sequential (safest); larger values increase throughput up to
// the point where the ONNX runtime runs out of memory.
const localEmbedBatchSize = 4

// rankViaLocalEmbed ranks candidates by cosine similarity to the query using
// the local nomic-embed ONNX model. Texts are embedded in small batches
// (localEmbedBatchSize) to avoid OOM on large candidate lists.
func rankViaLocalEmbed(emb *LocalEmbedder, query string, sample RepoBenchSample) (int, error) {
	ctx := context.Background()
	candidates := sample.Context

	// Embed all texts in small batches.
	all := make([]string, 1+len(candidates))
	all[0] = query
	copy(all[1:], candidates)

	vecs := make([][]float32, len(all))
	for start := 0; start < len(all); start += localEmbedBatchSize {
		end := start + localEmbedBatchSize
		if end > len(all) {
			end = len(all)
		}
		batch := all[start:end]
		bvecs, err := emb.e.EmbedBatch(ctx, batch)
		if err != nil {
			// Fallback to hybrid-rrf on embed failure.
			ranked := rankHybridRRF(query, candidates)
			for rank, item := range ranked {
				if item.index == sample.GoldenSnippetIndex {
					return rank + 1, nil
				}
			}
			return len(candidates), nil
		}
		copy(vecs[start:end], bvecs)
	}

	queryVec := vecs[0]
	type scored struct {
		index int
		score float64
	}
	scores := make([]scored, len(candidates))
	for i, candVec := range vecs[1:] {
		scores[i] = scored{index: i, score: float64(cosineF32local(queryVec, candVec))}
	}

	// Sort descending by score.
	for i := 1; i < len(scores); i++ {
		for j := i; j > 0 && scores[j].score > scores[j-1].score; j-- {
			scores[j], scores[j-1] = scores[j-1], scores[j]
		}
	}
	for rank, s := range scores {
		if s.index == sample.GoldenSnippetIndex {
			return rank + 1, nil
		}
	}
	return len(candidates), nil
}

func cosineF32local(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(normA) * math.Sqrt(normB)))
}
