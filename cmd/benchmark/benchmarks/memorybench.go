// memorybench.go implements MemoryBench — measures whether warm Synapses
// (with prior session data) delivers better context than cold start.
//
// Seeds store with memories, observations, and episodes, then queries
// the store to verify warm vs cold delivery. No agent needed.
package benchmarks

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
	"github.com/SynapsesOS/synapses/internal/store"
)

// MemoryBenchScenario defines one test scenario.
type MemoryBenchScenario struct {
	ID          string
	Description string
	Category    string // memory, failure, convention, dedup

	// Setup function seeds the store.
	Setup func(st *store.Store, projectID string) error

	// Verify function checks warm vs cold delivery.
	// Returns (warmHits, coldHits).
	Verify func(st *store.Store, coldSt *store.Store, projectID string) (int, int)
}

// MemoryBenchResult holds per-scenario results.
type MemoryBenchResult struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Category    string  `json:"category"`
	WarmHits    int     `json:"warm_hits"`
	ColdHits    int     `json:"cold_hits"`
	Delta       int     `json:"delta"`
	DeltaPct    float64 `json:"delta_pct"`
}

// RunMemoryBench runs all memory bench scenarios.
func RunMemoryBench() (*reporter.MemoryBenchReport, error) {
	scenarios := buildMemoryScenarios()
	log.Printf("[memorybench] %d scenarios", len(scenarios))

	var results []MemoryBenchResult
	var totalWarm, totalCold int

	for _, sc := range scenarios {
		r := runMemoryScenario(sc)
		results = append(results, r)
		totalWarm += r.WarmHits
		totalCold += r.ColdHits
		log.Printf("  %s: warm=%d cold=%d delta=%+d", sc.ID, r.WarmHits, r.ColdHits, r.Delta)
	}

	deliveryRate := float64(0)
	if totalWarm > 0 {
		deliveryRate = float64(totalWarm-totalCold) / float64(totalWarm) * 100
	}

	report := &reporter.MemoryBenchReport{
		Timestamp:    reporter.Timestamp(),
		TotalCases:   len(scenarios),
		TotalWarm:    totalWarm,
		TotalCold:    totalCold,
		DeliveryRate: deliveryRate,
		Cases:        results,
	}

	log.Printf("[memorybench] warm=%d cold=%d delivery_rate=%.1f%%",
		totalWarm, totalCold, deliveryRate)
	return report, nil
}

func runMemoryScenario(sc MemoryBenchScenario) MemoryBenchResult {
	projectID := "bench-project"

	// Warm store: seeded with data.
	warmDir, _ := os.MkdirTemp("", "memorybench-warm-*")
	warmSt, err := store.Open(warmDir)
	if err != nil {
		return MemoryBenchResult{ID: sc.ID, Description: sc.Description, Category: sc.Category}
	}
	defer warmSt.Close()

	if err := sc.Setup(warmSt, projectID); err != nil {
		return MemoryBenchResult{ID: sc.ID, Description: sc.Description, Category: sc.Category}
	}

	// Cold store: empty.
	coldDir, _ := os.MkdirTemp("", "memorybench-cold-*")
	coldSt, err := store.Open(coldDir)
	if err != nil {
		return MemoryBenchResult{ID: sc.ID, Description: sc.Description, Category: sc.Category}
	}
	defer coldSt.Close()

	warm, cold := sc.Verify(warmSt, coldSt, projectID)

	delta := warm - cold
	deltaPct := float64(0)
	if warm > 0 {
		deltaPct = float64(delta) / float64(warm) * 100
	}

	return MemoryBenchResult{
		ID:          sc.ID,
		Description: sc.Description,
		Category:    sc.Category,
		WarmHits:    warm,
		ColdHits:    cold,
		Delta:       delta,
		DeltaPct:    deltaPct,
	}
}

func buildMemoryScenarios() []MemoryBenchScenario {
	return []MemoryBenchScenario{
		// ── Memory Recall ──────────────────────────────────────────
		{
			ID:          "mem-recall-exact",
			Description: "Exact keyword recall from 3 saved memories",
			Category:    "memory",
			Setup: func(st *store.Store, pid string) error {
				for i, content := range []string{
					"gorilla/sessions chosen for session management in Sprint 14",
					"jwt-go v3 incompatible with our middleware — use v4",
					"table-driven tests with testify assertions is the project convention",
				} {
					st.InsertMemory(store.Memory{
						Content:  content,
						Tier:     "tier_1",
						AgentID:  "test",
						Source:   "agent_save",
						Tags:     "[]",
						ID:       fmt.Sprintf("mem-%d", i),
					})
				}
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				warmResults, _ := warm.SearchMemories("jwt-go middleware", 5)
				coldResults, _ := cold.SearchMemories("jwt-go middleware", 5)
				return len(warmResults), len(coldResults)
			},
		},
		{
			ID:          "mem-recall-semantic",
			Description: "Semantic recall — query doesn't match exact words",
			Category:    "memory",
			Setup: func(st *store.Store, pid string) error {
				st.InsertMemory(store.Memory{
					Content: "PKCE flow chosen over implicit grant for OAuth security",
					Tier:    "tier_1",
					AgentID: "test",
					Source:  "agent_save",
					Tags:    "[]",
				})
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				warmResults, _ := warm.SearchMemories("authentication OAuth flow", 5)
				coldResults, _ := cold.SearchMemories("authentication OAuth flow", 5)
				return len(warmResults), len(coldResults)
			},
		},
		{
			ID:          "mem-empty-cold",
			Description: "Cold store returns zero memories",
			Category:    "memory",
			Setup: func(st *store.Store, pid string) error {
				st.InsertMemory(store.Memory{
					Content: "Redis chosen for caching layer",
					Tier:    "tier_1",
					AgentID: "test",
					Source:  "agent_save",
					Tags:    "[]",
				})
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				warmResults, _ := warm.SearchMemories("caching", 5)
				coldResults, _ := cold.SearchMemories("caching", 5)
				return len(warmResults), len(coldResults)
			},
		},

		// ── Failure Episode Recall ─────────────────────────────────
		{
			ID:          "episode-failure-recall",
			Description: "Failure episodes recalled for known-bad approach",
			Category:    "failure",
			Setup: func(st *store.Store, pid string) error {
				st.RememberEpisode(store.Episode{
					ProjectID:   pid,
					AgentID:     "test",
					EpisodeType: "failure",
					Outcome:     "failure",
					Decision:    "Tried jwt-go v3 for authentication middleware",
					Rationale:   "jwt-go v3 API incompatible with echo middleware chain",
					Tags:        `["auth","jwt","middleware"]`,
					Importance:  0.9,
				})
				st.RememberEpisode(store.Episode{
					ProjectID:   pid,
					AgentID:     "test",
					EpisodeType: "failure",
					Outcome:     "failure",
					Decision:    "Direct database import from handler package",
					Rationale:   "Violates service layer pattern — handlers must go through services",
					Tags:        `["architecture","layers"]`,
					Importance:  0.8,
				})
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				warmEps, _ := warm.RecallEpisodes("jwt middleware authentication", pid, "", "failure", "", 5, 0)
				coldEps, _ := cold.RecallEpisodes("jwt middleware authentication", pid, "", "failure", "", 5, 0)
				return len(warmEps), len(coldEps)
			},
		},
		{
			ID:          "episode-decision-recall",
			Description: "Decision episodes recalled for architecture choices",
			Category:    "failure",
			Setup: func(st *store.Store, pid string) error {
				st.RememberEpisode(store.Episode{
					ProjectID:   pid,
					AgentID:     "test",
					EpisodeType: "decision",
					Outcome:     "success",
					Decision:    "Use gorilla/sessions for session management",
					Rationale:   "Simple API, matches existing code patterns, no external dependency",
					Tags:        `["architecture","sessions"]`,
					Importance:  0.7,
				})
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				warmEps, _ := warm.RecallEpisodes("session management", pid, "", "", "", 5, 0)
				coldEps, _ := cold.RecallEpisodes("session management", pid, "", "", "", 5, 0)
				return len(warmEps), len(coldEps)
			},
		},

		// ── Convention Delivery ─────────────────────────────────────
		{
			ID:          "convention-delivery",
			Description: "Conventions from observations appear in warm store",
			Category:    "convention",
			Setup: func(st *store.Store, pid string) error {
				// Seed 5 sessions with consistent pattern
				for i := 0; i < 5; i++ {
					st.InsertSessionObservation(store.SessionObservation{
						SessionID:  fmt.Sprintf("s%d", i),
						ProjectID:  pid,
						AgentID:    "test",
						Category:   store.ObsCategoryTestingPattern,
						Key:        "uses_testify",
						Value:      "8",
						Confidence: 0.7,
						CreatedAt:  time.Now().Unix() - int64(i*3600),
					})
				}
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				// Check observation key counts
				warmCounts, _ := warm.GetObservationKeyCounts(pid, store.ObsCategoryTestingPattern)
				coldCounts, _ := cold.GetObservationKeyCounts(pid, store.ObsCategoryTestingPattern)
				warmN := 0
				for _, c := range warmCounts {
					if c >= 3 {
						warmN++
					}
				}
				coldN := 0
				for _, c := range coldCounts {
					if c >= 3 {
						coldN++
					}
				}
				return warmN, coldN
			},
		},

		// ── Exploration Dedup ───────────────────────────────────────
		{
			ID:          "exploration-dedup",
			Description: "Exploration log prevents re-reading same entities",
			Category:    "dedup",
			Setup: func(st *store.Store, pid string) error {
				// Seed observations about explored entities
				for i := 0; i < 3; i++ {
					st.InsertSessionObservation(store.SessionObservation{
						SessionID:  fmt.Sprintf("s%d", i),
						ProjectID:  pid,
						AgentID:    "test",
						Category:   store.ObsCategoryFilePattern,
						Key:        "uses_handler_layer",
						Value:      "5",
						Confidence: 0.8,
						CreatedAt:  time.Now().Unix() - int64(i*3600),
					})
				}
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				warmObs, _ := warm.GetObservationsByCategory(pid, store.ObsCategoryFilePattern, 100)
				coldObs, _ := cold.GetObservationsByCategory(pid, store.ObsCategoryFilePattern, 100)
				return len(warmObs), len(coldObs)
			},
		},

		// ── Multiple Memory Types ──────────────────────────────────
		{
			ID:          "combined-warm",
			Description: "Warm store has memories + episodes + observations (combined)",
			Category:    "memory",
			Setup: func(st *store.Store, pid string) error {
				st.InsertMemory(store.Memory{
					Content: "Authentication uses PKCE with gorilla/sessions",
					Tier:    "tier_1",
					AgentID: "test",
					Source:  "agent_save",
					Tags:    "[]",
				})
				st.RememberEpisode(store.Episode{
					ProjectID:   pid,
					AgentID:     "test",
					EpisodeType: "failure",
					Outcome:     "failure",
					Decision:    "jwt-go v3 doesn't work with our middleware",
					Tags:        "[]",
					Importance:  0.9,
				})
				for i := 0; i < 3; i++ {
					st.InsertSessionObservation(store.SessionObservation{
						SessionID:  fmt.Sprintf("s%d", i),
						ProjectID:  pid,
						AgentID:    "test",
						Category:   store.ObsCategoryLibraryUsage,
						Key:        "uses_echo",
						Value:      "3",
						Confidence: 0.7,
						CreatedAt:  time.Now().Unix(),
					})
				}
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				hits := 0
				// Memory
				mems, _ := warm.SearchMemories("authentication PKCE", 5)
				hits += len(mems)
				// Episodes
				eps, _ := warm.RecallEpisodes("jwt middleware", pid, "", "", "", 5, 0)
				hits += len(eps)
				// Conventions
				counts, _ := warm.GetObservationKeyCounts(pid, store.ObsCategoryLibraryUsage)
				for _, c := range counts {
					if c >= 3 {
						hits++
					}
				}

				coldHits := 0
				coldMems, _ := cold.SearchMemories("authentication PKCE", 5)
				coldHits += len(coldMems)
				coldEps, _ := cold.RecallEpisodes("jwt middleware", pid, "", "", "", 5, 0)
				coldHits += len(coldEps)

				return hits, coldHits
			},
		},

		// ── Null Cases ─────────────────────────────────────────────
		{
			ID:          "both-empty",
			Description: "Both warm and cold are empty → 0 hits each",
			Category:    "memory",
			Setup: func(st *store.Store, pid string) error {
				return nil // nothing seeded
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				warmMems, _ := warm.SearchMemories("anything", 5)
				coldMems, _ := cold.SearchMemories("anything", 5)
				return len(warmMems), len(coldMems)
			},
		},
		{
			ID:          "irrelevant-query",
			Description: "Query doesn't match any warm data → 0 hits",
			Category:    "memory",
			Setup: func(st *store.Store, pid string) error {
				st.InsertMemory(store.Memory{
					Content: "gorilla/sessions for session management",
					Tier:    "tier_1",
					AgentID: "test",
					Source:  "agent_save",
					Tags:    "[]",
				})
				return nil
			},
			Verify: func(warm, cold *store.Store, pid string) (int, int) {
				warmMems, _ := warm.SearchMemories("kubernetes deployment helm", 5)
				coldMems, _ := cold.SearchMemories("kubernetes deployment helm", 5)
				return len(warmMems), len(coldMems)
			},
		},
	}
}

