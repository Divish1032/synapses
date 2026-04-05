// continuitybench.go implements Benchmark 4: Session Continuity.
//
// Measures whether warm Synapses context (from prior sessions) covers
// the entities/files that a REAL task actually needs. Not "does DB
// return data" but "does the returned data cover what the task requires?"
//
// Ground truth: for each task, the required entities are known.
// Warm data: realistic exploration from prior sessions.
// Test: does memory recall + convention delivery cover the required context?
package benchmarks

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ContinuityScenario defines a multi-session scenario.
type ContinuityScenario struct {
	ID          string
	Description string

	// Prior sessions: what the agent explored and learned.
	PriorMemories []string // memories from sessions 1-3
	PriorFailures []store.Episode
	PriorObservations []store.SessionObservation

	// Current task: what the agent needs to do NOW.
	TaskDescription string
	TaskQuery       string   // what the agent would search for
	RequiredContext []string // substrings that MUST appear in recalled content
}

// ContinuityResult holds per-scenario results.
type ContinuityResult struct {
	ID              string  `json:"id"`
	Description     string  `json:"description"`
	RequiredCount   int     `json:"required_count"`
	CoveredWarm     int     `json:"covered_warm"`
	CoveredCold     int     `json:"covered_cold"`
	CoverageWarm    float64 `json:"coverage_warm"`
	CoverageCold    float64 `json:"coverage_cold"`
	Delta           float64 `json:"delta"`
}

// RunContinuityBench tests session continuity value.
func RunContinuityBench() (*reporter.MemoryBenchReport, error) {
	scenarios := buildContinuityScenarios()
	log.Printf("[continuitybench] %d scenarios", len(scenarios))

	var results []ContinuityResult
	var totalWarmCoverage, totalColdCoverage float64

	for _, sc := range scenarios {
		r := runContinuityScenario(sc)
		results = append(results, r)
		totalWarmCoverage += r.CoverageWarm
		totalColdCoverage += r.CoverageCold
		log.Printf("  %s: warm=%.0f%% cold=%.0f%% delta=+%.0f%%",
			sc.ID, r.CoverageWarm, r.CoverageCold, r.Delta)
	}

	avgWarm := totalWarmCoverage / float64(len(scenarios))
	avgCold := totalColdCoverage / float64(len(scenarios))

	report := &reporter.MemoryBenchReport{
		Timestamp:    reporter.Timestamp(),
		TotalCases:   len(scenarios),
		TotalWarm:    int(avgWarm),
		TotalCold:    int(avgCold),
		DeliveryRate: avgWarm - avgCold,
		Cases:        results,
	}

	log.Printf("[continuitybench] avg_warm=%.1f%% avg_cold=%.1f%% delta=+%.1f%%",
		avgWarm, avgCold, avgWarm-avgCold)
	return report, nil
}

func runContinuityScenario(sc ContinuityScenario) ContinuityResult {
	projectID := "bench-project"
	result := ContinuityResult{
		ID:            sc.ID,
		Description:   sc.Description,
		RequiredCount: len(sc.RequiredContext),
	}

	// ── Warm store: seeded with prior session data ──
	warmDir, _ := os.MkdirTemp("", "continuity-warm-*")
	warmSt, err := store.Open(warmDir)
	if err != nil {
		return result
	}
	defer warmSt.Close()

	for _, content := range sc.PriorMemories {
		warmSt.InsertMemory(store.Memory{
			Content: content,
			Tier:    "tier_1",
			AgentID: "prior-session",
			Source:  "agent_save",
			Tags:    "[]",
		})
	}
	for _, ep := range sc.PriorFailures {
		ep.ProjectID = projectID
		warmSt.RememberEpisode(ep)
	}
	for i, obs := range sc.PriorObservations {
		obs.ProjectID = projectID
		obs.SessionID = fmt.Sprintf("prior-%d", i/3)
		obs.CreatedAt = time.Now().Unix()
		warmSt.InsertSessionObservation(obs)
	}

	// ── Cold store: empty ──
	coldDir, _ := os.MkdirTemp("", "continuity-cold-*")
	coldSt, err := store.Open(coldDir)
	if err != nil {
		return result
	}
	defer coldSt.Close()

	// ── Query both stores with the task query ──
	warmMems, _ := warmSt.SearchMemories(sc.TaskQuery, 10)
	coldMems, _ := coldSt.SearchMemories(sc.TaskQuery, 10)

	warmEps, _ := warmSt.RecallEpisodes(sc.TaskQuery, projectID, "", "", "", 10, 0)
	coldEps, _ := coldSt.RecallEpisodes(sc.TaskQuery, projectID, "", "", "", 10, 0)

	// Build combined recall text.
	var warmText strings.Builder
	for _, m := range warmMems {
		warmText.WriteString(m.Content)
		warmText.WriteString(" ")
	}
	for _, e := range warmEps {
		warmText.WriteString(e.Decision)
		warmText.WriteString(" ")
		warmText.WriteString(e.Rationale)
		warmText.WriteString(" ")
	}

	var coldText strings.Builder
	for _, m := range coldMems {
		coldText.WriteString(m.Content)
		coldText.WriteString(" ")
	}
	for _, e := range coldEps {
		coldText.WriteString(e.Decision)
		coldText.WriteString(" ")
		coldText.WriteString(e.Rationale)
		coldText.WriteString(" ")
	}

	// Check coverage: how many required context items appear in recalled text?
	warmStr := strings.ToLower(warmText.String())
	coldStr := strings.ToLower(coldText.String())

	for _, req := range sc.RequiredContext {
		reqLower := strings.ToLower(req)
		if strings.Contains(warmStr, reqLower) {
			result.CoveredWarm++
		}
		if strings.Contains(coldStr, reqLower) {
			result.CoveredCold++
		}
	}

	if result.RequiredCount > 0 {
		result.CoverageWarm = float64(result.CoveredWarm) / float64(result.RequiredCount) * 100
		result.CoverageCold = float64(result.CoveredCold) / float64(result.RequiredCount) * 100
	}
	result.Delta = result.CoverageWarm - result.CoverageCold

	return result
}

func buildContinuityScenarios() []ContinuityScenario {
	return []ContinuityScenario{
		{
			ID:          "auth-refactor",
			Description: "Prior sessions explored auth system; new task modifies auth middleware",
			PriorMemories: []string{
				"Authentication uses gorilla/sessions for session management. Chosen in Sprint 14 for simplicity.",
				"PKCE flow chosen over implicit grant for OAuth — user requirement from security review.",
				"JWT middleware chain in middleware.go handles token validation before reaching handlers.",
				"auth.go has session middleware at line 47. All /api/* routes use AuthMiddleware.",
			},
			PriorFailures: []store.Episode{
				{AgentID: "prior", EpisodeType: "failure", Outcome: "failure",
					Decision:  "Tried jwt-go v3 for token validation",
					Rationale: "jwt-go v3 API incompatible with echo middleware — v4 required",
					Tags:      `["auth","jwt"]`, Importance: 0.9},
			},
			TaskDescription: "Add refresh token rotation to the auth middleware",
			TaskQuery:       "authentication middleware token session",
			RequiredContext: []string{"gorilla/sessions", "PKCE", "jwt", "middleware", "AuthMiddleware"},
		},
		{
			ID:          "database-migration",
			Description: "Prior sessions worked with DB layer; new task adds a migration",
			PriorMemories: []string{
				"Database uses SQLite with WAL mode. Store layer at internal/store/store.go.",
				"All DB access goes through the Store struct — handlers never import database/sql directly.",
				"Migration pattern: add column with ALTER TABLE, backfill in Go, then add NOT NULL constraint.",
				"FTS5 tables need explicit sync triggers — see memories_fts, episodes_fts examples.",
			},
			TaskDescription: "Add a new 'tags' column to the tasks table",
			TaskQuery:       "database migration table column SQLite",
			RequiredContext: []string{"SQLite", "WAL", "Store", "ALTER TABLE", "FTS5"},
		},
		{
			ID:          "test-conventions",
			Description: "Prior sessions established testing patterns; new task needs new tests",
			PriorMemories: []string{
				"This project uses table-driven tests with testify assertions — observed in 15+ test files.",
				"Always run go tests with -p 1 -parallel 1 to avoid RAM exhaustion on large packages.",
				"Store tests need per-test temp dirs — use t.TempDir(), not shared directories.",
				"Integration tests need format=json for parseable output.",
			},
			PriorObservations: []store.SessionObservation{
				{AgentID: "p", Category: "testing_pattern", Key: "uses_testify", Value: "15", Confidence: 0.8},
				{AgentID: "p", Category: "testing_pattern", Key: "uses_testify", Value: "12", Confidence: 0.8},
				{AgentID: "p", Category: "testing_pattern", Key: "uses_testify", Value: "18", Confidence: 0.8},
			},
			TaskDescription: "Write tests for the new convention extraction feature",
			TaskQuery:       "testing patterns conventions assertions",
			RequiredContext: []string{"testify", "table-driven", "-p 1", "TempDir"},
		},
		{
			ID:          "api-endpoint",
			Description: "Prior sessions explored API patterns; new task adds an endpoint",
			PriorMemories: []string{
				"MCP handlers follow the pattern: validate params → call store → format response → return.",
				"All handler functions take (ctx context.Context, req mcp.CallToolRequest) as params.",
				"Response format uses labeled KV with NL annotations — never raw JSON or code.",
				"Tool descriptions must be under 200 tokens — full docs go in MCP Resources.",
			},
			TaskDescription: "Add a new 'query_graph' MCP tool",
			TaskQuery:       "MCP handler tool endpoint response format",
			RequiredContext: []string{"handler", "validate", "store", "response", "KV"},
		},
		{
			ID:          "cold-start-irrelevant",
			Description: "New project, no prior sessions — cold and warm should be equal (both zero)",
			PriorMemories: []string{}, // nothing
			TaskDescription: "Set up a new React frontend",
			TaskQuery:       "React component state management hooks",
			RequiredContext: []string{"React", "component"}, // nothing will match
		},
	}
}
