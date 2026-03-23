package embed

import (
	"math"
	"testing"
)

func TestNormalizeL2Vec_NaN(t *testing.T) {
	v := []float32{1.0, float32(math.NaN()), 3.0}
	if got := normalizeL2Vec(v); got != nil {
		t.Errorf("expected nil for NaN element, got %v", got)
	}
}

func TestNormalizeL2Vec_Inf(t *testing.T) {
	v := []float32{1.0, float32(math.Inf(1)), 3.0}
	if got := normalizeL2Vec(v); got != nil {
		t.Errorf("expected nil for +Inf element, got %v", got)
	}
}

func TestNormalizeL2Vec_NegInf(t *testing.T) {
	v := []float32{1.0, float32(math.Inf(-1)), 3.0}
	if got := normalizeL2Vec(v); got != nil {
		t.Errorf("expected nil for -Inf element, got %v", got)
	}
}

func TestNormalizeL2Vec_Empty(t *testing.T) {
	v := []float32{}
	got := normalizeL2Vec(v)
	if got == nil {
		t.Fatal("expected non-nil (empty slice) for empty input, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got length %d", len(got))
	}
}

func TestNormalizeL2Vec_ZeroMagnitude(t *testing.T) {
	v := []float32{0, 0, 0}
	got := normalizeL2Vec(v)
	if got != nil {
		t.Fatalf("expected nil for zero-magnitude vector (model failure), got %v", got)
	}
}

func TestNormalizeL2Vec_NormalVector(t *testing.T) {
	v := []float32{3, 4, 0}
	got := normalizeL2Vec(v)
	if got == nil {
		t.Fatal("expected non-nil for normal vector, got nil")
	}
	var sum float64
	for _, x := range got {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if math.Abs(norm-1.0) > 1e-6 {
		t.Errorf("expected unit-length result, got norm %v", norm)
	}
}
