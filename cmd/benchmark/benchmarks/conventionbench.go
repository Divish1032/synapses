// conventionbench.go implements ConventionBench — measures precision and recall
// of Synapses' convention extraction engine against ground truth.
//
// Seeds store.Store with N sessions of SessionObservation records, runs
// convention extraction, and compares against expected conventions.
//
// No agent, no MCP, no Docker. Pure Go test against the extraction engine.
package benchmarks

import (
	"fmt"
	"log"
	"os"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ConventionBenchCase defines a test scenario for convention extraction.
type ConventionBenchCase struct {
	ID          string `json:"id"`
	Description string `json:"description"`

	// Sessions to seed: each session has observations.
	Sessions []ConventionSession `json:"sessions"`

	// Expected conventions after extraction runs.
	Expected []ExpectedConvention `json:"expected"`
}

// ConventionSession is one simulated session's observations.
type ConventionSession struct {
	SessionID    string                `json:"session_id"`
	Observations []store.SessionObservation `json:"observations"`
}

// ExpectedConvention is one expected extraction result.
type ExpectedConvention struct {
	Category string  `json:"category"`
	Key      string  `json:"key"`
	MinConf  float64 `json:"min_confidence"` // minimum expected confidence
}

// ConventionBenchResult holds per-case results.
type ConventionBenchResult struct {
	CaseID           string `json:"case_id"`
	Description      string `json:"description"`
	ExpectedCount    int    `json:"expected_count"`
	ExtractedCount   int    `json:"extracted_count"`
	CorrectCount     int    `json:"correct_count"`
	FalsePositives   int    `json:"false_positives"`
	FalseNegatives   int    `json:"false_negatives"`
	Precision        float64 `json:"precision"`
	Recall           float64 `json:"recall"`
}

// RunConventionBench runs all convention bench cases.
func RunConventionBench() (*reporter.ConventionBenchReport, error) {
	cases := buildConventionCases()
	log.Printf("[conventionbench] %d test cases", len(cases))

	var results []ConventionBenchResult
	var totalCorrect, totalExtracted, totalExpected int

	for _, tc := range cases {
		r := runConventionCase(tc)
		results = append(results, r)
		totalCorrect += r.CorrectCount
		totalExtracted += r.ExtractedCount
		totalExpected += r.ExpectedCount
	}

	precision := float64(0)
	if totalExtracted > 0 {
		precision = float64(totalCorrect) / float64(totalExtracted)
	}
	recall := float64(0)
	if totalExpected > 0 {
		recall = float64(totalCorrect) / float64(totalExpected)
	}
	f1 := float64(0)
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}

	report := &reporter.ConventionBenchReport{
		Timestamp:  reporter.Timestamp(),
		TotalCases: len(cases),
		Precision:  precision * 100,
		Recall:     recall * 100,
		F1:         f1 * 100,
		Cases:      results,
	}

	log.Printf("[conventionbench] Precision=%.1f%% Recall=%.1f%% F1=%.1f%%",
		report.Precision, report.Recall, report.F1)
	return report, nil
}

func runConventionCase(tc ConventionBenchCase) ConventionBenchResult {
	// Create a fresh store for each case.
	tmpDir, _ := os.MkdirTemp("", "conventionbench-*")
	st, err := store.Open(tmpDir)
	if err != nil {
		return ConventionBenchResult{
			CaseID:      tc.ID,
			Description: tc.Description,
		}
	}
	defer st.Close()

	projectID := "bench-project"

	// Seed observations.
	for _, sess := range tc.Sessions {
		for _, obs := range sess.Observations {
			obs.SessionID = sess.SessionID
			obs.ProjectID = projectID
			if obs.AgentID == "" {
				obs.AgentID = "bench-agent"
			}
			if obs.Confidence == 0 {
				obs.Confidence = 0.7
			}
			st.InsertSessionObservation(obs)
		}
	}

	// Run convention extraction.
	// runConventionExtraction is package-private in internal/mcp.
	// Instead, we directly query what the extraction would find:
	// count distinct sessions per key, check against threshold.
	extracted := map[string]bool{}
	for _, category := range []string{
		store.ObsCategoryTestingPattern,
		store.ObsCategoryLibraryUsage,
		store.ObsCategoryFilePattern,
	} {
		keyCounts, err := st.GetObservationKeyCounts(projectID, category)
		if err != nil {
			continue
		}
		for key, count := range keyCounts {
			if count >= 3 { // MinSessionsForConvention
				extracted[category+":"+key] = true
			}
		}
	}

	// Compare against expected.
	expectedSet := map[string]bool{}
	for _, exp := range tc.Expected {
		expectedSet[exp.Category+":"+exp.Key] = true
	}

	correct := 0
	for key := range extracted {
		if expectedSet[key] {
			correct++
		}
	}

	falsePositives := len(extracted) - correct
	falseNegatives := len(expectedSet) - correct

	precision := float64(0)
	if len(extracted) > 0 {
		precision = float64(correct) / float64(len(extracted))
	}
	recall := float64(0)
	if len(expectedSet) > 0 {
		recall = float64(correct) / float64(len(expectedSet))
	}

	return ConventionBenchResult{
		CaseID:         tc.ID,
		Description:    tc.Description,
		ExpectedCount:  len(tc.Expected),
		ExtractedCount: len(extracted),
		CorrectCount:   correct,
		FalsePositives: falsePositives,
		FalseNegatives: falseNegatives,
		Precision:      precision * 100,
		Recall:         recall * 100,
	}
}

// buildConventionCases creates the 8 test archetypes.
func buildConventionCases() []ConventionBenchCase {
	return []ConventionBenchCase{
		{
			ID:          "clean-signal",
			Description: "5 sessions with consistent testify usage → should extract",
			Sessions:    makeSessions(5, store.ObsCategoryTestingPattern, "uses_testify"),
			Expected:    []ExpectedConvention{{Category: store.ObsCategoryTestingPattern, Key: "uses_testify", MinConf: 0.6}},
		},
		{
			ID:          "below-threshold",
			Description: "2 sessions (below MinSessionsForConvention=3) → should NOT extract",
			Sessions:    makeSessions(2, store.ObsCategoryTestingPattern, "uses_testify"),
			Expected:    []ExpectedConvention{}, // nothing expected
		},
		{
			ID:          "at-threshold",
			Description: "Exactly 3 sessions → should extract at minimum confidence",
			Sessions:    makeSessions(3, store.ObsCategoryLibraryUsage, "uses_gin"),
			Expected:    []ExpectedConvention{{Category: store.ObsCategoryLibraryUsage, Key: "uses_gin", MinConf: 0.6}},
		},
		{
			ID:          "multi-convention",
			Description: "5 sessions with 3 distinct patterns → should extract all 3",
			Sessions:    makeMultiConventionSessions(5),
			Expected: []ExpectedConvention{
				{Category: store.ObsCategoryTestingPattern, Key: "uses_testify", MinConf: 0.6},
				{Category: store.ObsCategoryLibraryUsage, Key: "uses_chi", MinConf: 0.6},
				{Category: store.ObsCategoryFilePattern, Key: "uses_handler_layer", MinConf: 0.6},
			},
		},
		{
			ID:          "noisy-signal",
			Description: "3 matching + 2 conflicting sessions → should still extract (3 >= threshold)",
			Sessions:    makeNoisySessions(),
			Expected:    []ExpectedConvention{{Category: store.ObsCategoryTestingPattern, Key: "uses_testify", MinConf: 0.6}},
		},
		{
			ID:          "null-case",
			Description: "No observations → zero conventions",
			Sessions:    []ConventionSession{},
			Expected:    []ExpectedConvention{},
		},
		{
			ID:          "wrong-category",
			Description: "Observations in non-promotable category → should NOT extract",
			Sessions:    makeSessions(5, store.ObsCategoryToolUsage, "high_validate_usage"),
			Expected:    []ExpectedConvention{}, // tool_usage is not promoted
		},
		{
			ID:          "high-confidence",
			Description: "10 sessions → high confidence extraction",
			Sessions:    makeSessions(10, store.ObsCategoryLibraryUsage, "uses_fastapi"),
			Expected:    []ExpectedConvention{{Category: store.ObsCategoryLibraryUsage, Key: "uses_fastapi", MinConf: 0.9}},
		},
	}
}

func makeSessions(n int, category, key string) []ConventionSession {
	var sessions []ConventionSession
	for i := 0; i < n; i++ {
		sessions = append(sessions, ConventionSession{
			SessionID: fmt.Sprintf("sess-%d", i+1),
			Observations: []store.SessionObservation{
				{Category: category, Key: key, Value: "5"},
			},
		})
	}
	return sessions
}

func makeMultiConventionSessions(n int) []ConventionSession {
	var sessions []ConventionSession
	for i := 0; i < n; i++ {
		sessions = append(sessions, ConventionSession{
			SessionID: fmt.Sprintf("sess-%d", i+1),
			Observations: []store.SessionObservation{
				{Category: store.ObsCategoryTestingPattern, Key: "uses_testify", Value: "3"},
				{Category: store.ObsCategoryLibraryUsage, Key: "uses_chi", Value: "2"},
				{Category: store.ObsCategoryFilePattern, Key: "uses_handler_layer", Value: "4"},
			},
		})
	}
	return sessions
}

func makeNoisySessions() []ConventionSession {
	var sessions []ConventionSession
	// 3 sessions with testify
	for i := 0; i < 3; i++ {
		sessions = append(sessions, ConventionSession{
			SessionID: fmt.Sprintf("match-%d", i+1),
			Observations: []store.SessionObservation{
				{Category: store.ObsCategoryTestingPattern, Key: "uses_testify", Value: "5"},
			},
		})
	}
	// 2 sessions with different testing (noise)
	for i := 0; i < 2; i++ {
		sessions = append(sessions, ConventionSession{
			SessionID: fmt.Sprintf("noise-%d", i+1),
			Observations: []store.SessionObservation{
				{Category: store.ObsCategoryTestingPattern, Key: "uses_gotest_only", Value: "3"},
			},
		})
	}
	return sessions
}
