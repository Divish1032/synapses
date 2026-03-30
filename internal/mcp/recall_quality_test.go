package mcp

import (
	"testing"
	"time"
)

func TestRecallFootprintRing_PushAndEffectiveness(t *testing.T) {
	t.Parallel()
	r := &recallFootprintRing{}

	// Push 3 footprints with results.
	r.push(recallFootprint{RecallID: "r1", EntityIDs: []string{"A"}, ResultCount: 2, Timestamp: time.Now()})
	r.push(recallFootprint{RecallID: "r2", EntityIDs: []string{"B"}, ResultCount: 1, Timestamp: time.Now()})
	r.push(recallFootprint{RecallID: "r3", EntityIDs: []string{"C"}, ResultCount: 0, Timestamp: time.Now()}) // no results

	rate, total, actedOn := r.effectiveness()
	if total != 2 {
		t.Errorf("expected 2 recalls with results, got %d", total)
	}
	if actedOn != 0 {
		t.Errorf("expected 0 acted-on, got %d", actedOn)
	}
	if rate != 0 {
		t.Errorf("expected rate 0, got %f", rate)
	}
}

func TestRecallFootprintRing_CheckActedOn_StrongSignal(t *testing.T) {
	t.Parallel()
	r := &recallFootprintRing{}

	now := time.Now()
	r.push(recallFootprint{RecallID: "r1", EntityIDs: []string{"AuthService"}, ResultCount: 1, Timestamp: now.Add(-30 * time.Second)})

	fp, weight := r.checkActedOn([]string{"AuthService"}, nil, now)
	if fp == nil {
		t.Fatal("expected acted-on match, got nil")
	}
	if weight != 1.0 {
		t.Errorf("expected strong weight 1.0, got %f", weight)
	}
	if !fp.ActedOn {
		t.Error("expected footprint marked as acted-on")
	}

	// Second check should not match (already acted on).
	fp2, _ := r.checkActedOn([]string{"AuthService"}, nil, now)
	if fp2 != nil {
		t.Error("expected no match on second check (already acted-on)")
	}

	rate, total, actedOn := r.effectiveness()
	if total != 1 || actedOn != 1 {
		t.Errorf("expected 1/1, got %d/%d", actedOn, total)
	}
	if rate != 1.0 {
		t.Errorf("expected rate 1.0, got %f", rate)
	}
}

func TestRecallFootprintRing_CheckActedOn_WeakSignal(t *testing.T) {
	t.Parallel()
	r := &recallFootprintRing{}

	now := time.Now()
	r.push(recallFootprint{RecallID: "r1", FilePaths: []string{"/src/auth.go"}, ResultCount: 1, Timestamp: now.Add(-3 * time.Minute)})

	fp, weight := r.checkActedOn(nil, []string{"/src/auth.go"}, now)
	if fp == nil {
		t.Fatal("expected weak match, got nil")
	}
	if weight != 0.5 {
		t.Errorf("expected weak weight 0.5, got %f", weight)
	}
}

func TestRecallFootprintRing_CheckActedOn_Expired(t *testing.T) {
	t.Parallel()
	r := &recallFootprintRing{}

	now := time.Now()
	r.push(recallFootprint{RecallID: "r1", EntityIDs: []string{"X"}, ResultCount: 1, Timestamp: now.Add(-6 * time.Minute)})

	fp, _ := r.checkActedOn([]string{"X"}, nil, now)
	if fp != nil {
		t.Error("expected no match beyond 5-minute window")
	}
}

func TestRecallFootprintRing_Overflow(t *testing.T) {
	t.Parallel()
	r := &recallFootprintRing{}

	// Push more than maxRecallFootprints
	for i := 0; i < maxRecallFootprints+3; i++ {
		r.push(recallFootprint{RecallID: "r", ResultCount: 1, Timestamp: time.Now()})
	}
	if r.count != maxRecallFootprints {
		t.Errorf("expected count capped at %d, got %d", maxRecallFootprints, r.count)
	}
}

func TestOverlaps(t *testing.T) {
	t.Parallel()
	if overlaps(nil, []string{"a"}) {
		t.Error("nil a should not overlap")
	}
	if overlaps([]string{"a"}, nil) {
		t.Error("nil b should not overlap")
	}
	if !overlaps([]string{"a", "b"}, []string{"b", "c"}) {
		t.Error("expected overlap on 'b'")
	}
	if overlaps([]string{"a"}, []string{"b"}) {
		t.Error("expected no overlap")
	}
}

func TestExtractEntityTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		query    string
		expected int
	}{
		{"how does AuthService work?", 1},       // AuthService is PascalCase
		{"repo::file::Foo bar baz", 1},           // full NodeID
		{"all lowercase words here", 0},          // no entities
		{"com.example.Foo and Bar", 2},           // dotted + PascalCase
		{"AuthService PaymentService TokenValidator UserRepo SessionManager ExtraOne", 5}, // capped at 5
	}
	for _, tt := range tests {
		tokens := extractEntityTokens(tt.query)
		if len(tokens) != tt.expected {
			t.Errorf("extractEntityTokens(%q) = %d tokens, want %d (got %v)", tt.query, len(tokens), tt.expected, tokens)
		}
	}
}
