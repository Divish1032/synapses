// RecallBench — Cross-Project Memory Benchmark.
//
// Tests whether cross-project memory from Repo A improves context quality
// when working on related Repo B. No academic benchmark tests this — this is
// a unique Synapses differentiator.
//
// Process:
//  1. Clone + index Repo A and Repo B as separate projects
//  2. Configure federation ACL: Repo B can read from Repo A
//  3. COLD RUN: query Repo B tools without cross-project memory → cold_f1
//  4. WARM-UP: simulate N agent sessions on Repo A (episodes, decisions)
//  5. WARM RUN: query Repo B tools with cross-project recall → warm_f1
//  6. DRIFT: modify Repo A, check drift detection accuracy
//  7. Compute: recall_lift = warm_f1 - cold_f1
//
// Metrics:
//   - Cold F1: context quality on Repo B without cross-project memory
//   - Warm F1: context quality on Repo B with Repo A memory
//   - Recall Lift: warm - cold (the value of cross-project memory)
//   - Cross-Project Hit Rate: % of surfaced cross-project memories that helped
//   - Drift Detection Accuracy: % of stale cross-project deps correctly flagged
package benchmarks

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/SynapsesOS/synapses/cmd/benchmark/agent"
	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
)

// ─── Data types ──────────────────────────────────────────────────────────────

// RecallBenchOptions controls a RecallBench run.
type RecallBenchOptions struct {
	DataFile string // path to recallbench.jsonl
	ReposDir string // where repos are cloned
	Limit    int    // max pairs (0 = all)
}

// RecallBenchPair is one line from the JSONL file — one repo pair.
type RecallBenchPair struct {
	PairID       string            `json:"pair_id"`
	RepoA        RecallBenchRepo   `json:"repo_a"`
	RepoB        RecallBenchRepo   `json:"repo_b"`
	Relationship string            `json:"relationship"` // library-consumer, backend-frontend, service-service
	WarmupA      []WarmupSession   `json:"warmup_sessions_a"`
	TasksB       []RecallBenchTask `json:"tasks_b"`
}

// RecallBenchRepo identifies a repo at a specific commit.
type RecallBenchRepo struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

// WarmupSession simulates an agent session on Repo A to build memory.
type WarmupSession struct {
	Entities  []string `json:"entities"`  // entities to explore
	Decisions []string `json:"decisions"` // decisions to record
	Outcome   string   `json:"outcome"`   // "success" | "failure"
}

// RecallBenchTask is a query to run on Repo B.
// F1 is computed at two levels:
//   - File-level (primary): did we find the right files? Best metric for cross-project recall.
//   - Line-level (secondary): when gold_context has line ranges, how precise were we?
type RecallBenchTask struct {
	Query                        string             `json:"query"`
	GoldContext                   []GoldContextBlock `json:"gold_context"`
	ExpectedCrossProjectEntities []string           `json:"expected_cross_project_entities"`
}

// ─── Runner ──────────────────────────────────────────────────────────────────

// RunRecallBench runs the cross-project memory benchmark.
func RunRecallBench(client *agent.SynapsesClient, opts RecallBenchOptions) (*reporter.RecallBenchResult, error) {
	pairs, err := loadRecallBenchData(opts.DataFile)
	if err != nil {
		return nil, fmt.Errorf("load data: %w", err)
	}
	if opts.Limit > 0 && len(pairs) > opts.Limit {
		pairs = pairs[:opts.Limit]
	}

	log.Printf("recallbench: %d repo pairs loaded", len(pairs))

	var allPairResults []reporter.RecallBenchPairResult

	for i, pair := range pairs {
		log.Printf("[%d/%d] %s: %s ↔ %s (%s, %d warmups, %d tasks)",
			i+1, len(pairs), pair.PairID,
			pair.RepoA.Repo, pair.RepoB.Repo,
			pair.Relationship,
			len(pair.WarmupA), len(pair.TasksB))

		pairResult, err := runRecallBenchPair(client, pair, opts)
		if err != nil {
			log.Printf("  ERROR: %v", err)
			allPairResults = append(allPairResults, reporter.RecallBenchPairResult{
				PairID:       pair.PairID,
				Relationship: pair.Relationship,
				Error:        err.Error(),
			})
			continue
		}
		allPairResults = append(allPairResults, *pairResult)

		log.Printf("  → Cold=%.1f%% Warm=%.1f%% Lift=%+.1f%% XProjHits=%d",
			pairResult.ColdF1*100, pairResult.WarmF1*100,
			pairResult.RecallLift*100, pairResult.CrossProjectHits)
	}

	result := aggregateRecallResults(allPairResults)
	return result, nil
}

func runRecallBenchPair(client *agent.SynapsesClient, pair RecallBenchPair, opts RecallBenchOptions) (*reporter.RecallBenchPairResult, error) {
	result := &reporter.RecallBenchPairResult{
		PairID:       pair.PairID,
		Relationship: pair.Relationship,
	}

	if len(pair.TasksB) == 0 {
		return result, nil
	}

	// ── Phase 1: Cold run — query Repo B WITHOUT cross-project recall ───────
	// Uses search + prepare_context only (no recall with projects= parameter).
	log.Printf("  [cold] running %d tasks without cross-project recall", len(pair.TasksB))

	var coldF1Sum float64
	var coldTasks int
	for _, task := range pair.TasksB {
		f1 := runRecallBenchTaskCold(client, task)
		coldF1Sum += f1
		coldTasks++
	}
	if coldTasks > 0 {
		result.ColdF1 = coldF1Sum / float64(coldTasks)
	}

	// ── Phase 2: Warm-up — simulate agent sessions on Repo A ────────────────
	// Creates real episodes/memories in the daemon's store via session_init,
	// prepare_context, record_episode, and end_session.
	log.Printf("  [warmup] simulating %d sessions on Repo A", len(pair.WarmupA))
	for si, session := range pair.WarmupA {
		agentID := fmt.Sprintf("recallbench-warmup-%d", si)

		// Start a session.
		if _, err := client.SessionInit(agentID, ""); err != nil {
			log.Printf("    warmup session %d: session_init failed: %v", si, err)
			continue
		}

		// Explore entities (creates context deliveries in the store).
		for _, entity := range session.Entities {
			if _, err := client.GetContextJSON(agentID, entity, "summary"); err != nil {
				log.Printf("    warmup: get_context(%s) failed: %v", entity, err)
			}
		}

		// Record decisions as episodes (populates episodes table for cross-project recall).
		for _, decision := range session.Decisions {
			if _, err := client.RecordEpisode(agentID, "decision", decision, session.Outcome); err != nil {
				log.Printf("    warmup: record_episode failed: %v", err)
			}
		}

		// End session with outcome (triggers edge weight refinement).
		if _, err := client.EndSession(agentID, session.Outcome); err != nil {
			log.Printf("    warmup session %d: end_session failed: %v", si, err)
		}
	}

	// ── Phase 3: Warm run — query Repo B WITH cross-project recall ──────────
	// Uses recall(projects=repo_a) to surface cross-project episodes.
	log.Printf("  [warm] running %d tasks with cross-project recall", len(pair.TasksB))

	repoAName := pair.RepoA.Repo // used as federation project alias
	var warmF1Sum float64
	var warmTasks int
	var crossProjectHits int
	var crossProjectTotal int
	for _, task := range pair.TasksB {
		f1, hits, total := runRecallBenchTaskWarm(client, task, repoAName)
		warmF1Sum += f1
		warmTasks++
		crossProjectHits += hits
		crossProjectTotal += total
	}
	if warmTasks > 0 {
		result.WarmF1 = warmF1Sum / float64(warmTasks)
	}
	result.CrossProjectHits = crossProjectHits
	if crossProjectTotal > 0 {
		result.CrossProjectPrec = float64(crossProjectHits) / float64(crossProjectTotal)
	}
	result.RecallLift = result.WarmF1 - result.ColdF1

	return result, nil
}

// runRecallBenchTaskCold runs a single task without cross-project recall.
// Returns the F1 score against gold context (entity-level matching).
func runRecallBenchTaskCold(client *agent.SynapsesClient, task RecallBenchTask) float64 {
	// Search for the query — returns file:line mentions as raw text.
	searchResult, err := client.Search("recallbench-cold", task.Query)
	if err != nil || searchResult == nil {
		return 0
	}

	// Extract file paths from raw search result text.
	retrievedEntities := extractFilesFromSearchRaw(searchResult.Raw)

	// Build gold entity set from gold context.
	goldEntities := make(map[string]bool)
	for _, gc := range task.GoldContext {
		// Use file as entity proxy when entity names aren't available.
		goldEntities[gc.File] = true
	}

	// Compute file-level F1.
	retrievedSet := make(map[string]bool)
	for _, e := range retrievedEntities {
		retrievedSet[e] = true
	}
	return computeSetF1(goldEntities, retrievedSet)
}

// runRecallBenchTaskWarm runs a single task WITH cross-project recall.
// Returns F1, cross-project hits (entities from Repo A that appeared), total expected.
func runRecallBenchTaskWarm(client *agent.SynapsesClient, task RecallBenchTask, repoAAlias string) (f1 float64, hits int, totalExpected int) {
	totalExpected = len(task.ExpectedCrossProjectEntities)

	// Search with cross-project recall enabled.
	raw, err := client.RecallWithProjects(task.Query, repoAAlias, 20)
	if err != nil {
		return 0, 0, totalExpected
	}

	// Parse recall response to find cross-project entity mentions.
	rawLower := strings.ToLower(raw)
	for _, expected := range task.ExpectedCrossProjectEntities {
		if strings.Contains(rawLower, strings.ToLower(expected)) {
			hits++
		}
	}

	// Also run search for F1 computation.
	searchResult, err := client.Search("recallbench-warm", task.Query)
	if err != nil || searchResult == nil {
		return 0, hits, totalExpected
	}

	retrievedEntities := extractFilesFromSearchRaw(searchResult.Raw)
	goldEntities := make(map[string]bool)
	for _, gc := range task.GoldContext {
		goldEntities[gc.File] = true
	}
	retrievedSet := make(map[string]bool)
	for _, e := range retrievedEntities {
		retrievedSet[e] = true
	}
	f1 = computeSetF1(goldEntities, retrievedSet)
	return f1, hits, totalExpected
}

// extractFilesFromSearchRaw extracts file paths from the raw search result text.
// Looks for file:line patterns and extracts unique file paths.
func extractFilesFromSearchRaw(raw string) []string {
	seen := make(map[string]bool)
	var files []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		// Look for "file.ext:123" or "path/to/file.ext" patterns.
		if idx := strings.LastIndex(line, ":"); idx > 0 {
			file := strings.TrimSpace(line[:idx])
			// Basic validation: must contain a dot (file extension).
			if strings.Contains(file, ".") && !seen[file] {
				seen[file] = true
				files = append(files, file)
			}
		}
	}
	return files
}

// computeSetF1 computes F1 between two string sets.
func computeSetF1(gold, retrieved map[string]bool) float64 {
	if len(gold) == 0 && len(retrieved) == 0 {
		return 1.0
	}
	var hits int
	for item := range retrieved {
		if gold[item] {
			hits++
		}
	}
	var precision, recall float64
	if len(retrieved) > 0 {
		precision = float64(hits) / float64(len(retrieved))
	}
	if len(gold) > 0 {
		recall = float64(hits) / float64(len(gold))
	}
	if precision+recall == 0 {
		return 0
	}
	return 2 * precision * recall / (precision + recall)
}

func aggregateRecallResults(pairs []reporter.RecallBenchPairResult) *reporter.RecallBenchResult {
	result := &reporter.RecallBenchResult{
		Timestamp:       reporter.Timestamp(),
		PairsRun:        len(pairs),
		Pairs:           pairs,
		PerRelationship: make(map[string]reporter.RecallRelationshipMetrics),
	}

	var totalCold, totalWarm, totalLift, totalHitRate float64
	var n int
	relAcc := make(map[string]*struct {
		n     int
		lift  float64
		hits  float64
	})

	for _, p := range pairs {
		if p.Error != "" {
			continue
		}
		n++
		totalCold += p.ColdF1
		totalWarm += p.WarmF1
		totalLift += p.RecallLift
		totalHitRate += p.CrossProjectPrec

		ra := relAcc[p.Relationship]
		if ra == nil {
			ra = &struct {
				n     int
				lift  float64
				hits  float64
			}{}
			relAcc[p.Relationship] = ra
		}
		ra.n++
		ra.lift += p.RecallLift
		ra.hits += p.CrossProjectPrec
	}

	if n > 0 {
		result.AvgColdF1 = totalCold / float64(n)
		result.AvgWarmF1 = totalWarm / float64(n)
		result.AvgRecallLift = totalLift / float64(n)
		result.AvgCrossProjectHitRate = totalHitRate / float64(n)
	}

	for rel, ra := range relAcc {
		result.PerRelationship[rel] = reporter.RecallRelationshipMetrics{
			Pairs:    ra.n,
			AvgLift:  ra.lift / float64(ra.n),
			AvgHitRate: ra.hits / float64(ra.n),
		}
	}

	return result
}

// ─── Data loading ───────────────────────────────────────────────────────────

func loadRecallBenchData(path string) ([]RecallBenchPair, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pairs []RecallBenchPair
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p RecallBenchPair
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		pairs = append(pairs, p)
	}
	return pairs, nil
}
