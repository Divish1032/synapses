package brain

import (
	"sync"
	"testing"
)

// --- Tier-specific validators ---

func TestValidateIngestResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		summary string
		want    bool
	}{
		{"valid summary", "AuthService handles authentication for the API layer.", true},
		{"empty string", "", false},
		{"whitespace only", "   \n\t  ", false},
		{"too short", "hello", false},
		{"exactly 10 chars", "1234567890", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateIngestResponse(tc.summary); got != tc.want {
				t.Errorf("validateIngestResponse(%q) = %v, want %v", tc.summary, got, tc.want)
			}
		})
	}
}

func TestValidateEnrichResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		insight  string
		concerns []string
		want     bool
	}{
		{"valid insight", "AuthService is a hub.", nil, true},
		{"empty insight", "", nil, false},
		{"whitespace insight", "   ", nil, false},
		{"insight with concerns", "Hub node.", []string{"high coupling"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateEnrichResponse(tc.insight, tc.concerns); got != tc.want {
				t.Errorf("validateEnrichResponse(%q, %v) = %v, want %v", tc.insight, tc.concerns, got, tc.want)
			}
		})
	}
}

func TestValidateGuardianResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		explanation string
		fix         string
		want        bool
	}{
		{"both present", "This violates the rule.", "Use the interface instead.", true},
		{"empty explanation", "", "fix it", false},
		{"empty fix", "violation", "", false},
		{"both empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateGuardianResponse(tc.explanation, tc.fix); got != tc.want {
				t.Errorf("validateGuardianResponse(%q, %q) = %v, want %v", tc.explanation, tc.fix, got, tc.want)
			}
		})
	}
}

func TestValidateCoordinateResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		suggestion string
		want       bool
	}{
		{"valid", "No conflict. Safe to proceed.", true},
		{"empty", "", false},
		{"whitespace", "   ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateCoordinateResponse(tc.suggestion); got != tc.want {
				t.Errorf("validateCoordinateResponse(%q) = %v, want %v", tc.suggestion, got, tc.want)
			}
		})
	}
}

// --- brainStats ---

func TestBrainStats_RecordAndSnapshot(t *testing.T) {
	t.Parallel()
	var s brainStats

	s.record("ingest", true, 100)
	s.record("ingest", true, 200)
	s.record("ingest", false, 50)
	s.record("enrich", true, 3000)
	s.record("guardian", true, 500)
	s.record("orchestrate", false, 800)
	s.record("archivist", true, 2000)

	snap := s.snapshot()

	assertInt := func(key string, want int64) {
		t.Helper()
		got, ok := snap[key]
		if !ok {
			t.Errorf("missing key %q in snapshot", key)
			return
		}
		if got.(int64) != want {
			t.Errorf("%s = %v, want %d", key, got, want)
		}
	}

	assertInt("ingest_calls", 3)
	assertInt("ingest_success", 2)
	assertInt("enrich_calls", 1)
	assertInt("enrich_success", 1)
	assertInt("guardian_calls", 1)
	assertInt("guardian_success", 1)
	assertInt("orchestrate_calls", 1)
	assertInt("orchestrate_success", 0)
	assertInt("archivist_calls", 1)
	assertInt("archivist_success", 1)

	// Average latency: (100+200+50)/3 = 116
	avgIngest := snap["ingest_avg_ms"].(int64)
	if avgIngest < 100 || avgIngest > 120 {
		t.Errorf("ingest_avg_ms = %d, want ~116", avgIngest)
	}
}

func TestBrainStats_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	var s brainStats
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(tier string) {
			defer wg.Done()
			s.record(tier, true, 10)
		}([]string{"ingest", "enrich", "guardian", "orchestrate", "archivist"}[i%5])
	}
	wg.Wait()

	snap := s.snapshot()
	total := snap["ingest_calls"].(int64) + snap["enrich_calls"].(int64) +
		snap["guardian_calls"].(int64) + snap["orchestrate_calls"].(int64) +
		snap["archivist_calls"].(int64)
	if total != 100 {
		t.Errorf("total calls = %d, want 100 (race condition?)", total)
	}
}
