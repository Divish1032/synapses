package embed

import (
	"math"
	"testing"
)

// ── Matryoshka truncation tests ──────────────────────────────────────────────
// These tests verify the truncation + normalization logic that Embed() applies
// to raw pipeline output. They run in package embed (not embed_test) so they
// can access the unexported matryoshkaDims constant and l2Normalize function.

func TestMatryoshkaTruncation_768to384(t *testing.T) {
	// Simulate a 768-dim raw embedding from nomic-embed-text-v1.5.
	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = float32(i+1) * 0.001
	}

	if len(vec) > matryoshkaDims {
		vec = vec[:matryoshkaDims]
	}

	if len(vec) != 384 {
		t.Errorf("expected 384 dims after truncation, got %d", len(vec))
	}

	// Verify only the leading dimensions survived.
	if vec[0] != 0.001 {
		t.Errorf("vec[0] = %f, want 0.001", vec[0])
	}
	if vec[383] != 0.384 {
		t.Errorf("vec[383] = %f, want 0.384", vec[383])
	}
}

func TestMatryoshkaTruncation_NoTruncationWhen384(t *testing.T) {
	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = float32(i+1) * 0.01
	}
	original := make([]float32, len(vec))
	copy(original, vec)

	if len(vec) > matryoshkaDims {
		vec = vec[:matryoshkaDims]
	}

	if len(vec) != 384 {
		t.Errorf("expected 384 dims (no truncation needed), got %d", len(vec))
	}
	for i := range vec {
		if vec[i] != original[i] {
			t.Errorf("vec[%d] changed: got %f, want %f", i, vec[i], original[i])
			break
		}
	}
}

func TestMatryoshkaTruncation_ShorterThan384Unchanged(t *testing.T) {
	// If a pipeline somehow returns fewer than 384 dims, truncation should be a no-op.
	vec := make([]float32, 128)
	for i := range vec {
		vec[i] = float32(i)
	}

	if len(vec) > matryoshkaDims {
		vec = vec[:matryoshkaDims]
	}

	if len(vec) != 128 {
		t.Errorf("expected 128 dims (unchanged), got %d", len(vec))
	}
}

func TestMatryoshkaDimsConstant(t *testing.T) {
	// Guard against accidental changes to the constant.
	if matryoshkaDims != 384 {
		t.Errorf("matryoshkaDims = %d, want 384", matryoshkaDims)
	}
}

// ── l2Normalize tests ────────────────────────────────────────────────────────

func TestL2Normalize_KnownVector(t *testing.T) {
	// 3-4-5 right triangle: norm=5, so [3/5, 4/5] = [0.6, 0.8].
	vec := l2Normalize([]float32{3, 4})
	if math.Abs(float64(vec[0])-0.6) > 1e-6 || math.Abs(float64(vec[1])-0.8) > 1e-6 {
		t.Errorf("expected [0.6, 0.8], got [%f, %f]", vec[0], vec[1])
	}

	// Verify unit length.
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if math.Abs(sumSq-1.0) > 1e-6 {
		t.Errorf("expected unit length, got magnitude %.6f", math.Sqrt(sumSq))
	}
}

func TestL2Normalize_AlreadyUnit(t *testing.T) {
	// [1, 0] is already unit length — l2Normalize should return it unchanged
	// (the early-return path for norm in [0.999, 1.001]).
	in := []float32{1, 0}
	out := l2Normalize(in)
	if out[0] != 1 || out[1] != 0 {
		t.Errorf("expected unchanged [1, 0], got [%f, %f]", out[0], out[1])
	}
	// Verify same slice (no allocation for already-unit vectors).
	if &out[0] != &in[0] {
		t.Error("expected same backing array for already-unit vector")
	}
}

func TestL2Normalize_NearlyUnit(t *testing.T) {
	// A vector with norm 1.0005 is within the [0.999, 1.001] tolerance
	// and should be returned unchanged (same slice, no allocation).
	v := float32(math.Sqrt(1.0005 / 2.0))
	in := []float32{v, v}
	out := l2Normalize(in)
	if &out[0] != &in[0] {
		t.Error("expected same backing array for near-unit vector")
	}
}

func TestL2Normalize_ZeroVector(t *testing.T) {
	in := []float32{0, 0, 0}
	out := l2Normalize(in)
	if len(out) != 3 {
		t.Fatalf("expected length 3, got %d", len(out))
	}
	for i, v := range out {
		if v != 0 {
			t.Errorf("out[%d] = %f, want 0", i, v)
		}
	}
	// Should return same slice (zero-magnitude early return).
	if &out[0] != &in[0] {
		t.Error("expected same backing array for zero vector")
	}
}

func TestL2Normalize_Empty(t *testing.T) {
	out := l2Normalize([]float32{})
	if len(out) != 0 {
		t.Errorf("expected empty slice, got len=%d", len(out))
	}
}

func TestL2Normalize_NilSlice(t *testing.T) {
	out := l2Normalize(nil)
	if out != nil {
		t.Errorf("expected nil, got len=%d", len(out))
	}
}

func TestL2Normalize_SingleElement(t *testing.T) {
	out := l2Normalize([]float32{5.0})
	if math.Abs(float64(out[0])-1.0) > 1e-6 {
		t.Errorf("expected 1.0, got %f", out[0])
	}
}

func TestL2Normalize_NegativeValues(t *testing.T) {
	// [-3, -4]: norm=5, normalized=[-0.6, -0.8]
	out := l2Normalize([]float32{-3, -4})
	if math.Abs(float64(out[0])+0.6) > 1e-6 || math.Abs(float64(out[1])+0.8) > 1e-6 {
		t.Errorf("expected [-0.6, -0.8], got [%f, %f]", out[0], out[1])
	}
}

// ── Combined truncation + normalization ──────────────────────────────────────

func TestTruncateThenNormalize_ProducesUnitVector(t *testing.T) {
	// Full 768-dim vector with known non-unit-length values.
	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = float32(i+1) * 0.001
	}

	// Apply the same two-step pipeline as Embed().
	if len(vec) > matryoshkaDims {
		vec = vec[:matryoshkaDims]
	}
	vec = l2Normalize(vec)

	if len(vec) != 384 {
		t.Fatalf("expected 384 dims, got %d", len(vec))
	}

	// Verify unit length.
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if math.Abs(sumSq-1.0) > 1e-4 {
		t.Errorf("expected unit length after truncate+normalize, got magnitude %.6f", math.Sqrt(sumSq))
	}
}

func TestTruncateThenNormalize_DiscardedDimsDoNotAffectResult(t *testing.T) {
	// Two 768-dim vectors identical in first 384 dims, different in last 384.
	// After truncation they must produce identical normalized output.
	a := make([]float32, 768)
	b := make([]float32, 768)
	for i := 0; i < 384; i++ {
		a[i] = float32(i+1) * 0.001
		b[i] = float32(i+1) * 0.001
	}
	for i := 384; i < 768; i++ {
		a[i] = 0.0  // zeros in tail
		b[i] = 99.0 // large values in tail
	}

	// Truncate + normalize both.
	if len(a) > matryoshkaDims {
		a = a[:matryoshkaDims]
	}
	a = l2Normalize(a)

	if len(b) > matryoshkaDims {
		b = b[:matryoshkaDims]
	}
	b = l2Normalize(b)

	for i := range a {
		if math.Abs(float64(a[i])-float64(b[i])) > 1e-7 {
			t.Errorf("dim %d differs: a=%f, b=%f — tail dims leaked into result", i, a[i], b[i])
			break
		}
	}
}
