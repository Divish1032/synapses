package mcp

import (
	"strings"
	"testing"
)

// ── computeContextConfidence unit tests ─────────────────────────────────────

func TestComputeContextConfidence_NoRecord(t *testing.T) {
	// No prior outcome data → optimistic default 0.75 regardless of staleness.
	conf := computeContextConfidence(0, false, false, false)
	if conf != 0.75 {
		t.Errorf("expected 0.75 for no-record entity, got %.2f", conf)
	}
}

func TestComputeContextConfidence_NoRecordWithStaleness(t *testing.T) {
	// No record + graph freshness warning → 0.75 - 0.10 = 0.65.
	conf := computeContextConfidence(0, false, true, false)
	if conf != 0.65 {
		t.Errorf("expected 0.65 for no-record + graphFreshWarning, got %.2f", conf)
	}

	// No record + both staleness flags → 0.75 - 0.10 - 0.05 = 0.60.
	conf = computeContextConfidence(0, false, true, true)
	if conf != 0.60 {
		t.Errorf("expected 0.60 for no-record + both flags, got %.2f", conf)
	}
}

func TestComputeContextConfidence_NeutralQuality(t *testing.T) {
	// qs=0 with a record: sigmoid(0/2.0)=0.5, scaled to 0.15+0.5*0.8 = 0.55.
	conf := computeContextConfidence(0, true, false, false)
	if conf != 0.55 {
		t.Errorf("expected 0.55 for qs=0 with record, got %.2f", conf)
	}
}

func TestComputeContextConfidence_PositiveQuality(t *testing.T) {
	// qs=+3: sigmoid(1.5)≈0.818, scaled to 0.15+0.818*0.8 ≈ 0.80.
	conf := computeContextConfidence(3.0, true, false, false)
	if conf < 0.78 || conf > 0.82 {
		t.Errorf("expected ~0.80 for qs=+3, got %.2f", conf)
	}
	// Strong positive quality should never trigger the low-confidence condition.
	if conf < 0.5 {
		t.Errorf("positive quality should not fall below 0.5, got %.2f", conf)
	}
}

func TestComputeContextConfidence_NegativeQuality_LowHintThreshold(t *testing.T) {
	// qs=-2 (established bad history): should produce confidence < 0.5 so that
	// ConfidenceHint fires. sigmoid(-1.0)≈0.269, scaled → 0.15+0.269*0.8 ≈ 0.37.
	conf := computeContextConfidence(-2.0, true, false, false)
	if conf >= 0.5 {
		t.Errorf("qs=-2 should produce confidence < 0.5, got %.2f", conf)
	}
}

func TestComputeContextConfidence_OneAbandonedSession(t *testing.T) {
	// qs=-0.8 (SignalWeightTaskAbandoned = -0.8): one session abandoned.
	// sigmoid(-0.4)≈0.401, scaled → 0.15+0.401*0.8 ≈ 0.47 < 0.5.
	conf := computeContextConfidence(-0.8, true, false, false)
	if conf >= 0.5 {
		t.Errorf("qs=-0.8 should produce confidence < 0.5, got %.2f", conf)
	}
}

func TestComputeContextConfidence_TwoCancellations(t *testing.T) {
	// qs=-1.0 (two task_cancelled signals, each -0.5): exact value 0.45.
	// sigmoid(-0.5)≈0.378, scaled → 0.15+0.378*0.8 = 0.452 → rounds to 0.45.
	conf := computeContextConfidence(-1.0, true, false, false)
	if conf != 0.45 {
		t.Errorf("expected 0.45 for qs=-1.0 (two cancellations), got %.2f", conf)
	}
}

func TestComputeContextConfidence_MildNegative_AboveHintThreshold(t *testing.T) {
	// qs=-0.2 (SignalWeightCorrectionDelayed = -0.2): mild negative signal.
	// sigmoid(-0.1)≈0.475, scaled → 0.15+0.475*0.8 ≈ 0.53 — above 0.5, no hint.
	conf := computeContextConfidence(-0.2, true, false, false)
	if conf < 0.5 {
		t.Errorf("qs=-0.2 should stay >= 0.5 (no hint fires), got %.2f", conf)
	}
}

func TestComputeContextConfidence_StalenessDowngrades(t *testing.T) {
	base := computeContextConfidence(0, true, false, false) // 0.55
	withFresh := computeContextConfidence(0, true, true, false)
	withBoth := computeContextConfidence(0, true, true, true)

	if withFresh >= base {
		t.Errorf("graphFreshWarning should lower confidence: base=%.2f withFresh=%.2f", base, withFresh)
	}
	if withBoth >= withFresh {
		t.Errorf("staleAnnotWarning should further lower confidence: withFresh=%.2f withBoth=%.2f", withFresh, withBoth)
	}
	// Exact values: 0.55 - 0.10 = 0.45, 0.45 - 0.05 = 0.40.
	if withFresh != 0.45 {
		t.Errorf("expected 0.45, got %.2f", withFresh)
	}
	if withBoth != 0.40 {
		t.Errorf("expected 0.40, got %.2f", withBoth)
	}
}

func TestComputeContextConfidence_Clamped(t *testing.T) {
	// Very high quality: must not exceed 1.0.
	conf := computeContextConfidence(20.0, true, false, false)
	if conf > 1.0 {
		t.Errorf("confidence must not exceed 1.0, got %.2f", conf)
	}
	// Very bad + max staleness: must not go below 0.0.
	conf = computeContextConfidence(-20.0, true, true, true)
	if conf < 0.0 {
		t.Errorf("confidence must not go below 0.0, got %.2f", conf)
	}
}

func TestComputeContextConfidence_TwoDecimalPrecision(t *testing.T) {
	// Results must be rounded to exactly 2 decimal places.
	for _, qs := range []float64{-3.7, -1.0, 0.0, 0.3, 1.3, 4.5} {
		conf := computeContextConfidence(qs, true, false, false)
		rounded := float64(int64(conf*100+0.5)) / 100
		if conf != rounded {
			t.Errorf("qs=%.1f: result %.6f is not rounded to 2 decimals", qs, conf)
		}
	}
}

// ── serializeCompact: confidence line appears in output ──────────────────────

func TestSerializeCompact_ConfidenceHighValue(t *testing.T) {
	dc := newTestDC()
	dc.Confidence = 0.87
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "confidence:0.87") {
		t.Errorf("expected 'confidence:0.87' in compact output, got:\n%s", out)
	}
	// No hint at 0.87.
	if strings.Contains(out, "⚠ confidence:") {
		t.Errorf("should not show warning prefix for high confidence, got:\n%s", out)
	}
}

func TestSerializeCompact_ConfidenceLowHint(t *testing.T) {
	dc := newTestDC()
	dc.Confidence = 0.37
	dc.ConfidenceHint = "Low confidence (0.37): prior deliveries frequently followed by corrections."
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "⚠ confidence:0.37") {
		t.Errorf("expected '⚠ confidence:0.37' prefix in low-confidence compact output, got:\n%s", out)
	}
	if !strings.Contains(out, "prior deliveries") {
		t.Errorf("expected confidence hint text in output, got:\n%s", out)
	}
}

func TestSerializeCompact_ConfidenceAbsentWhenZero(t *testing.T) {
	// Confidence=0 with no hint (zero value — unset) must not appear in compact output.
	dc := newTestDC()
	dc.Confidence = 0
	dc.ConfidenceHint = ""
	out := serializeCompact(dc, "full")
	if strings.Contains(out, "confidence:") {
		t.Errorf("confidence:0 with no hint should be suppressed (unset sentinel), got:\n%s", out)
	}
}

func TestSerializeCompact_ConfidenceZeroWithHint(t *testing.T) {
	// Confidence=0.0 from formula (extreme qs + both staleness) — hint IS set.
	// Must appear in compact output; suppressing it would silently hide the worst signal.
	dc := newTestDC()
	dc.Confidence = 0.0
	dc.ConfidenceHint = "Low confidence (0.00): prior context deliveries for this entity were frequently followed by corrections or session abandonment. Context is not suppressed — agent decides. Consider depth=4 or a different entry point."
	out := serializeCompact(dc, "full")
	if !strings.Contains(out, "⚠ confidence:0.00") {
		t.Errorf("confidence:0.00 with hint must appear in compact output, got:\n%s", out)
	}
}

func TestSerializeCompact_ConfidenceZeroWithHintInSummary(t *testing.T) {
	// Same as above but at summary detail level.
	dc := newTestDC()
	dc.Confidence = 0.0
	dc.ConfidenceHint = "Low confidence (0.00): corrections pattern."
	out := serializeCompact(dc, "summary")
	if !strings.Contains(out, "⚠ confidence:0.00") {
		t.Errorf("confidence:0.00 with hint must appear in summary level, got:\n%s", out)
	}
}

// TestSerializeCompact_ConfidenceInSummaryLevel verifies confidence is present
// even in the minimal "summary" detail level (fix for early-return gap).
func TestSerializeCompact_ConfidenceInSummaryLevel(t *testing.T) {
	dc := newTestDC()
	dc.Confidence = 0.75
	out := serializeCompact(dc, "summary")
	if !strings.Contains(out, "confidence:0.75") {
		t.Errorf("confidence must appear in summary level output, got:\n%s", out)
	}
	// Summary should NOT include Calls: section.
	if strings.Contains(out, "Calls:") {
		t.Error("summary level should not include Calls: section")
	}
}

func TestSerializeCompact_LowConfidenceInSummaryLevel(t *testing.T) {
	dc := newTestDC()
	dc.Confidence = 0.37
	dc.ConfidenceHint = "Low confidence (0.37): corrections pattern."
	out := serializeCompact(dc, "summary")
	if !strings.Contains(out, "⚠ confidence:0.37") {
		t.Errorf("low confidence hint must appear in summary level, got:\n%s", out)
	}
}

// ── Integration: confidence present in JSON get_context response ─────────────

func TestGetContext_ConfidenceInJSONResponse(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	res, err := s.handleGetContext(ctx, callTool(map[string]any{
		"entity": "AuthLogin",
		"format": "json",
	}))
	m := mustResult(t, res, err)

	// Confidence field must be present.
	confRaw, ok := m["confidence"]
	if !ok {
		t.Fatal("confidence field missing from get_context JSON response")
	}
	conf, ok := confRaw.(float64)
	if !ok {
		t.Fatalf("confidence is not a float64: %T", confRaw)
	}
	// New entity with no pulse data → default 0.75.
	if conf != 0.75 {
		t.Errorf("new entity with no quality data should default to 0.75, got %.2f", conf)
	}
	// confidence_hint must be absent for 0.75 (not low).
	if _, exists := m["confidence_hint"]; exists {
		t.Error("confidence_hint should be absent for non-low confidence score")
	}
}
