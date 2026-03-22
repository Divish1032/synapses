package embed

import (
	"math"
	"testing"
)

func TestNormalizeL2_NaN(t *testing.T) {
	v := []float32{1.0, float32(math.NaN()), 3.0}
	if got := normalizeL2(v); got != nil {
		t.Errorf("expected nil for NaN element, got %v", got)
	}
}

func TestNormalizeL2_Inf(t *testing.T) {
	v := []float32{1.0, float32(math.Inf(1)), 3.0}
	if got := normalizeL2(v); got != nil {
		t.Errorf("expected nil for +Inf element, got %v", got)
	}
}

func TestNormalizeL2_NegInf(t *testing.T) {
	v := []float32{1.0, float32(math.Inf(-1)), 3.0}
	if got := normalizeL2(v); got != nil {
		t.Errorf("expected nil for -Inf element, got %v", got)
	}
}

func TestNormalizeL2_Empty(t *testing.T) {
	v := []float32{}
	got := normalizeL2(v)
	if got == nil {
		t.Fatal("expected non-nil (empty slice) for empty input, got nil")
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got length %d", len(got))
	}
}

func TestNormalizeL2_ZeroMagnitude(t *testing.T) {
	v := []float32{0, 0, 0}
	got := normalizeL2(v)
	if got == nil {
		t.Fatal("expected non-nil for zero-magnitude vector, got nil")
	}
	for i, x := range got {
		if x != 0 {
			t.Errorf("got[%d] = %v, want 0", i, x)
		}
	}
}

func TestNormalizeL2_AlreadyNormalized(t *testing.T) {
	// A unit vector: magnitude is exactly 1.
	v := []float32{0, 0, 1}
	got := normalizeL2(v)
	if got == nil {
		t.Fatal("expected non-nil for already-normalized vector, got nil")
	}
	// Should return the original slice unchanged (within tolerance).
	var sum float64
	for _, x := range got {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if math.Abs(norm-1.0) > 0.002 {
		t.Errorf("expected unit-length vector, got norm %v", norm)
	}
}

func TestNormalizeL2_NormalVector(t *testing.T) {
	v := []float32{3, 4, 0}
	got := normalizeL2(v)
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
	// Verify direction is preserved: v[0]/v[1] ratio should be 3/4.
	ratio := float64(got[0]) / float64(got[1])
	if math.Abs(ratio-0.75) > 1e-6 {
		t.Errorf("expected ratio 0.75, got %v", ratio)
	}
}

func TestNormalizeL2_MixedValidInvalid(t *testing.T) {
	// NaN appears at index 2; function should return nil at first bad element.
	v := []float32{1.0, 2.0, float32(math.NaN()), 4.0}
	if got := normalizeL2(v); got != nil {
		t.Errorf("expected nil for mixed valid/NaN vector, got %v", got)
	}

	// +Inf at the end.
	v2 := []float32{1.0, 2.0, 3.0, float32(math.Inf(1))}
	if got := normalizeL2(v2); got != nil {
		t.Errorf("expected nil for mixed valid/Inf vector, got %v", got)
	}
}
