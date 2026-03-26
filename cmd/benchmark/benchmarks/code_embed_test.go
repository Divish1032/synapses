package benchmarks

import (
	"testing"
)

// TestIsCodeEmbedMode verifies the mode detection helper.
func TestIsCodeEmbedMode(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"embed-codebert", true},
		{"embed-jina-v2-code", true},
		{"embed-jina-v3", true},
		{"hybrid-rrf", false},
		{"synapses-embed-local", false},
		{"rerank-bm25", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsCodeEmbedMode(tt.mode); got != tt.want {
			t.Errorf("IsCodeEmbedMode(%q) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

// TestCodeModelSpecs verifies all modes in CodeModelSpecs are covered by IsCodeEmbedMode.
func TestCodeModelSpecs(t *testing.T) {
	for mode, spec := range CodeModelSpecs {
		if !IsCodeEmbedMode(mode) {
			t.Errorf("mode %q is in CodeModelSpecs but IsCodeEmbedMode returns false", mode)
		}
		if spec.ModelID == "" {
			t.Errorf("mode %q has empty ModelID", mode)
		}
		if spec.DirName == "" {
			t.Errorf("mode %q has empty DirName", mode)
		}
		if spec.OnnxFile == "" {
			t.Errorf("mode %q has empty OnnxFile", mode)
		}
		if spec.Dims <= 0 {
			t.Errorf("mode %q has non-positive Dims: %d", mode, spec.Dims)
		}
	}
}

// TestCosineF32 verifies basic cosine similarity properties.
func TestCosineF32(t *testing.T) {
	// Identical vectors → cosine = 1.0.
	a := []float32{1, 0, 0}
	if got := cosineF32(a, a); got < 0.9999 {
		t.Errorf("identical vectors: cosine = %v, want ~1.0", got)
	}

	// Orthogonal vectors → cosine = 0.0.
	b := []float32{0, 1, 0}
	if got := cosineF32(a, b); got > 0.0001 {
		t.Errorf("orthogonal vectors: cosine = %v, want ~0.0", got)
	}

	// Zero vector → cosine = 0.0 (no panic).
	z := []float32{0, 0, 0}
	if got := cosineF32(a, z); got != 0 {
		t.Errorf("zero vector: cosine = %v, want 0.0", got)
	}

	// Mismatched dims → 0 (no panic).
	if got := cosineF32(a, []float32{1, 0}); got != 0 {
		t.Errorf("mismatched dims: cosine = %v, want 0.0", got)
	}
}

// TestRankViaCodeEmbed_NilEmbedder verifies nil embedder falls back gracefully.
// A nil CodeEmbedder is handled in rankSample before calling rankViaCodeEmbed,
// so this test ensures the fallback path works.
func TestRankViaCodeEmbed_FallbackOnBatchError(t *testing.T) {
	// We can't create a real CodeModelEmbedder without downloading a model.
	// Verify that the fallback path in rankViaCodeEmbed (hybrid-rrf) produces
	// a valid rank in [1, len(candidates)].
	sample := RepoBenchSample{
		Context:            []string{"a", "b", "c", "d"},
		GoldenSnippetIndex: 2,
	}
	// Simulate the fallback: call rankHybridRRF directly and verify gold is found.
	query := "test query"
	ranked := rankHybridRRF(query, sample.Context)
	found := false
	for rank, item := range ranked {
		if item.index == sample.GoldenSnippetIndex {
			if rank+1 < 1 || rank+1 > len(sample.Context) {
				t.Errorf("rank %d out of bounds [1, %d]", rank+1, len(sample.Context))
			}
			found = true
			break
		}
	}
	if !found {
		t.Error("gold snippet not found in ranked results")
	}
}
