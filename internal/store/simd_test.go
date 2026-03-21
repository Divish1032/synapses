package store

import (
	"math"
	"testing"
)

func TestNormalizeVec_UnitLength(t *testing.T) {
	t.Parallel()
	vec := normalizeVec([]float32{3, 4}) // magnitude = 5
	if vec == nil {
		t.Fatal("expected non-nil normalized vector")
	}
	mag := float64(vec[0])*float64(vec[0]) + float64(vec[1])*float64(vec[1])
	if math.Abs(mag-1.0) > 1e-5 {
		t.Errorf("expected unit magnitude, got %f", math.Sqrt(mag))
	}
}

func TestNormalizeVec_ZeroMagnitude(t *testing.T) {
	t.Parallel()
	if got := normalizeVec([]float32{0, 0, 0}); got != nil {
		t.Errorf("expected nil for zero-magnitude vector, got %v", got)
	}
}

func TestNormalizeVec_Empty(t *testing.T) {
	t.Parallel()
	if got := normalizeVec(nil); got != nil {
		t.Errorf("expected nil for nil input, got %v", got)
	}
	if got := normalizeVec([]float32{}); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}

func TestNormalizeVec_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	orig := []float32{3, 4}
	_ = normalizeVec(orig)
	if orig[0] != 3 || orig[1] != 4 {
		t.Errorf("normalizeVec mutated input: got {%f, %f}", orig[0], orig[1])
	}
}

func TestDotSimilarity_NormalizedVectors(t *testing.T) {
	t.Parallel()
	a := normalizeVec([]float32{1, 0})
	b := normalizeVec([]float32{1, 1})
	got := dotSimilarity(a, b)
	want := float32(1.0 / math.Sqrt(2)) // cos(45°) ≈ 0.707
	if math.Abs(float64(got-want)) > 1e-5 {
		t.Errorf("dotSimilarity = %f, want ≈ %f", got, want)
	}
}

func TestDotSimilarity_IdenticalVectors(t *testing.T) {
	t.Parallel()
	a := normalizeVec([]float32{3, 4})
	got := dotSimilarity(a, a)
	if math.Abs(float64(got)-1.0) > 1e-5 {
		t.Errorf("dotSimilarity of identical normalized vectors = %f, want 1.0", got)
	}
}

func TestDotSimilarity_OrthogonalVectors(t *testing.T) {
	t.Parallel()
	a := normalizeVec([]float32{1, 0})
	b := normalizeVec([]float32{0, 1})
	got := dotSimilarity(a, b)
	if math.Abs(float64(got)) > 1e-5 {
		t.Errorf("expected 0 for orthogonal vectors, got %f", got)
	}
}

func TestDotSimilarity_DimensionMismatch(t *testing.T) {
	t.Parallel()
	if got := dotSimilarity([]float32{1, 0}, []float32{1, 0, 0}); got != 0 {
		t.Errorf("expected 0 for dim mismatch, got %f", got)
	}
}

func TestDotSimilarity_Empty(t *testing.T) {
	t.Parallel()
	if got := dotSimilarity(nil, nil); got != 0 {
		t.Errorf("expected 0 for nil input, got %f", got)
	}
	if got := dotSimilarity([]float32{}, []float32{}); got != 0 {
		t.Errorf("expected 0 for empty input, got %f", got)
	}
}

func TestDotSimilarity_MatchesCosineSimilarity(t *testing.T) {
	t.Parallel()
	// When both vectors are pre-normalized, dot product should equal cosine similarity.
	raw := [][]float32{
		{1, 2, 3},
		{4, 5, 6},
		{-1, 0.5, 2},
	}
	for i := 0; i < len(raw); i++ {
		for j := i; j < len(raw); j++ {
			na := normalizeVec(raw[i])
			nb := normalizeVec(raw[j])
			dot := dotSimilarity(na, nb)
			cos := cosineSimilarity(raw[i], raw[j])
			if math.Abs(float64(dot-cos)) > 1e-4 {
				t.Errorf("dot(%v,%v) = %f, cosine = %f, diff too large",
					raw[i], raw[j], dot, cos)
			}
		}
	}
}
