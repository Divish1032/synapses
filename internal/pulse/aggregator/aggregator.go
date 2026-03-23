// Package aggregator pre-computes daily rollup metrics for fast dashboard queries.
// It runs on a configurable interval and writes to the daily_rollups table.
package aggregator

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
)

// Aggregator periodically rolls up raw events into daily summaries.
type Aggregator struct {
	store        *pulsestore.Store
	interval     time.Duration
	stopCh       chan struct{}
	stopOnce     sync.Once
	wg           sync.WaitGroup
	// P2-20: track last vacuum time so it runs at most once per day.
	lastVacuumDay atomic.Value // stores string "YYYY-MM-DD"
	// Bug 27 — ROI-E7: DB file path for size metric.
	dbPath string
	// Bug 28 — ROI-E8: optional function to read the collector drop rate.
	collDropRate func() float64
}

// New creates an Aggregator that rolls up at the given interval.
func New(st *pulsestore.Store, intervalSec int) *Aggregator {
	if intervalSec <= 0 {
		intervalSec = 3600
	}
	return &Aggregator{
		store:    st,
		interval: time.Duration(intervalSec) * time.Second,
		stopCh:   make(chan struct{}),
	}
}

// NewWithOptions creates an Aggregator with extended options.
// dbPath is used to measure DB file size (Bug 27 — ROI-E7).
// collDropRate is an optional function returning the collector drop rate (Bug 28 — ROI-E8).
func NewWithOptions(st *pulsestore.Store, intervalSec int, dbPath string, collDropRate func() float64) *Aggregator {
	a := New(st, intervalSec)
	a.dbPath = dbPath
	a.collDropRate = collDropRate
	return a
}

// Start begins the rollup loop. It runs an immediate rollup, then repeats
// on the configured interval.
func (a *Aggregator) Start() {
	a.wg.Add(1)
	go a.loop()
}

// Stop signals the loop to exit and waits for completion.
// Safe to call multiple times.
func (a *Aggregator) Stop() {
	a.stopOnce.Do(func() { close(a.stopCh) })
	a.wg.Wait()
}

func (a *Aggregator) loop() {
	defer a.wg.Done()

	// Immediate rollup on start.
	a.rollup()

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.rollup()
		case <-a.stopCh:
			return
		}
	}
}

// buildDayMetrics constructs the standard per-day metric map from a summary and
// pipeline counters. Used by both rollup() and backfillMissedDays() to ensure
// consistent keys across all daily_rollups rows.
//
// file_changes_count equals reparseCount — both count reparse_events rows for
// the day, so no separate CountFileChanges query is needed.
//
// staleEmbeddings should be the current live count for today, or 0 for historical
// backfill days (the value is point-in-time and cannot be reconstructed).
// p3Metrics holds pre-computed P3 agent behavior counts for a single day.
type p3Metrics struct {
	// Phase 3 (existing)
	guardCircuitBreaks   int
	rateLimitRejections  int
	recallHits           int
	recallMisses         int
	validationViolations int
	// Phase 3B additions
	errorCount            int
	brainCostUSD          float64
	agentLLMCostUSD       float64
	truncatedDeliveries   int
	bfsCacheHits          int
	validatePlanCount     int
	memoryWrites          int
	safetyCheckHits       int
	safetyCheckMisses     int
	memoriesStaled        int
	avgSessionDurationMs  float64
	resumedSessions       int
	workflowAdherenceRate float64
	tasksPerHour          float64
	avgTaskCompletionMs   float64
	replanCount           int
	// Bug 8 — DQ-G.5: session abandonment rate.
	abandonmentRate float64
	// Bug 23 — ROI-C5: token savings from Anthropic cache reads.
	cacheTokenSavings int
	// Bug 24 — ROI-C7: context freshness score (0.0–1.0).
	contextFreshnessScore float64
	// Bug 25 — ROI-D5: fraction of entities with memory coverage.
	entityMemoryCoverage float64
	// Bug 68 — COV-9: config reload count for the day.
	configReloads int
	// Bug 69 — COV-12: store persistence operation count and avg latency.
	persistOps   int
	avgPersistMs float64
	// Phase 5: coverage completeness & self-refining metrics.
	crossSessionRecallHits int
	uptimePct              float64
	avgRebuildMs           float64
	federationDetections   int
	memoryInvalidations    int
	watcherViolations      int
	crossProjectAlerts     int
	deliverySuccessRate    float64
	concurrentAgentsMax    int
	graphFreshnessScore    float64
	cleanSessionRate       float64
	tokenBudgetHitRate     float64
	bfsCacheHitRateP5      float64
}

func buildDayMetrics(sum *pulsestore.Summary, reparseCount int, reparseDurationMs float64, staleEmbeddings int, p3 p3Metrics) map[string]float64 {
	return map[string]float64{
		"tokens_saved":       float64(sum.TokensSaved),
		"tokens_delivered":   float64(sum.TokensDelivered),
		"baseline_tokens":    float64(sum.BaselineTokens),
		"tool_calls":         float64(sum.TotalToolCalls),
		"context_deliveries": float64(sum.ContextDeliveries),
		"cost_saved_usd":     sum.CostSavedUSD,
		"sessions":           float64(sum.Sessions),
		"tasks_completed":    float64(sum.TasksCompleted),
		// Summable components — rates are recomputed at query time from these.
		"cache_hits":           float64(sum.CacheHits),
		"brain_enriched_count": float64(sum.BrainEnrichedCount),
		"total_latency_ms":     sum.TotalLatencyMs,
		// Legacy rate metrics kept for backward compat with older dashboards.
		"savings_pct":           sum.SavingsPct,
		"compression":           sum.CompressionRatio,
		"cache_hit_rate":        sum.CacheHitRate,
		"brain_enrichment_rate": sum.BrainEnrichRate,
		"avg_latency_ms":        sum.AvgLatencyMs,
		// P2-10: incremental reparse counters.
		"total_reparses":            float64(reparseCount),
		"total_reparse_duration_ms": reparseDurationMs,
		// P2-12: file change count (reparse_events rows == file changes).
		"file_changes_count": float64(reparseCount),
		// P2-16: stale embeddings (point-in-time; 0 for historical backfill).
		"stale_embeddings": float64(staleEmbeddings),
		// P3: agent behavior metrics.
		"guard_circuit_breaks":  float64(p3.guardCircuitBreaks),
		"rate_limit_rejections": float64(p3.rateLimitRejections),
		"recall_hits":           float64(p3.recallHits),
		"recall_misses":         float64(p3.recallMisses),
		"validation_violations": float64(p3.validationViolations),
		// Phase 3B: cost rollups
		"error_count":        float64(p3.errorCount),
		"brain_cost_usd":     p3.brainCostUSD,
		"agent_llm_cost_usd": p3.agentLLMCostUSD,
		// Phase 3B: context delivery quality
		"truncated_deliveries": float64(p3.truncatedDeliveries),
		"bfs_cache_hits":       float64(p3.bfsCacheHits),
		// Phase 3B: validation workflow
		"validate_plan_count": float64(p3.validatePlanCount),
		// Phase 3B: memory health
		"memory_writes":               float64(p3.memoryWrites),
		"safety_check_hits":           float64(p3.safetyCheckHits),
		"safety_check_misses":         float64(p3.safetyCheckMisses),
		"memory_anchor_invalidations": float64(p3.memoriesStaled),
		// Phase 3B: session analytics
		"avg_session_duration_ms": p3.avgSessionDurationMs,
		"resumed_sessions":        float64(p3.resumedSessions),
		"workflow_adherence_rate": p3.workflowAdherenceRate,
		// Phase 3B: task productivity
		"tasks_per_hour":         p3.tasksPerHour,
		"avg_task_completion_ms": p3.avgTaskCompletionMs,
		"replan_count":           float64(p3.replanCount),
		// Bug 8 — DQ-G.5: session abandonment rate.
		"abandonment_rate": p3.abandonmentRate,
		// Bug 23 — ROI-C5: Anthropic cache token savings.
		"cache_token_savings": float64(p3.cacheTokenSavings),
		// Bug 24 — ROI-C7: context freshness score.
		"context_freshness_score": p3.contextFreshnessScore,
		// Bug 25 — ROI-D5: entity memory coverage fraction.
		"entity_memory_coverage": p3.entityMemoryCoverage,
		// Bug 68 — COV-9: config reloads.
		"config_reloads": float64(p3.configReloads),
		// Bug 69 — COV-12: persistence metrics.
		"persist_ops":    float64(p3.persistOps),
		"avg_persist_ms": p3.avgPersistMs,
		// Phase 5: coverage completeness & self-refining.
		"cross_session_recall_hits": float64(p3.crossSessionRecallHits),
		"uptime_pct":               p3.uptimePct,
		"avg_rebuild_ms":           p3.avgRebuildMs,
		"federation_detections":    float64(p3.federationDetections),
		"memory_invalidations":     float64(p3.memoryInvalidations),
		"watcher_violations":       float64(p3.watcherViolations),
		"cross_project_alerts":     float64(p3.crossProjectAlerts),
		"delivery_success_rate":    p3.deliverySuccessRate,
		"concurrent_agents_max":    float64(p3.concurrentAgentsMax),
		"graph_freshness_score_p5": p3.graphFreshnessScore,
		"clean_session_rate":       p3.cleanSessionRate,
		"token_budget_hit_rate":    p3.tokenBudgetHitRate,
		"bfs_cache_hit_rate_p5":    p3.bfsCacheHitRateP5,
	}
}

// rollup computes today's metrics and writes them to daily_rollups, then prunes
// old events beyond the retention window.
// Uses calendar-day boundaries via created_date column instead of date() function.
func (a *Aggregator) rollup() {
	today := time.Now().UTC().Format("2006-01-02")

	// P2-18: backfill rollups for any days missed due to daemon downtime.
	a.backfillMissedDays(today)

	// G3 idempotency: if the full rollup already completed for today (e.g.,
	// daemon restarted mid-day), skip the batch metrics phase and jump
	// directly to the per-dimension rollups that are individually idempotent.
	// The sentinel metric "rollup_completed" marks a successful full pass.
	if val, readErr := a.store.ReadDailyRollup(today, "rollup_completed"); readErr == nil && val > 0 {
		// Batch metrics already computed. Re-run per-dimension rollups
		// (cheap, idempotent) and prune to handle late-arriving events.
		up := a.store.UpsertDailyRollup
		a.rollupPerProject(today, up)
		a.rollupPerTool(today, up)
		a.rollupPerAgent(today, up)
		a.rollupPerLanguage(today, up)
		a.rollupSearchMetrics(today, up)
		a.rollupPerToolErrors(today, up)
		// Prune is also idempotent (date-based, doesn't double-delete).
		if deleted, err := a.store.PruneOldEvents(90); err == nil && deleted > 0 {
			logutil.Info("pulse aggregator: pruned %d old events\n", deleted)
		}
		return
	}

	sum, err := a.store.GetSummaryForDay(today)
	if err != nil {
		logutil.Warn("pulse aggregator: summary error: %v\n", err)
		return
	}

	// P2-10: reparse daily counts. reparseCount also serves as file_changes_count
	// (same underlying table) — no separate CountFileChanges query needed.
	reparseCount, reparseDurationMs, _ := a.store.CountReparses(today)

	// P2-16: stale embedding count.
	staleEmbeddings := a.store.CountStaleEmbeddings()

	// P3: agent behavior counts for today.
	truncatedDeliveries, _ := a.store.CountTruncatedDeliveries(today)
	p3 := p3Metrics{
		guardCircuitBreaks:    a.store.CountGuardEvents(today, "loop_circuit_break"),
		rateLimitRejections:   a.store.CountGuardEvents(today, "rate_limit"),
		recallHits:            a.store.CountMemoryOps(today, "recall_hit"),
		recallMisses:          a.store.CountMemoryOps(today, "recall_miss"),
		validationViolations:  a.store.CountValidationViolations(today),
		errorCount:            a.store.CountToolErrors(today),
		brainCostUSD:          a.store.SumBrainCostForDay(today),
		agentLLMCostUSD:       a.store.SumAgentLLMCostForDay(today),
		truncatedDeliveries:   truncatedDeliveries,
		bfsCacheHits:          a.store.CountBFSCacheHitsForDay(today),
		validatePlanCount:     a.store.CountValidationCalls(today, "validate_plan"),
		memoryWrites:          a.store.CountMemoryOps(today, "write"),
		safetyCheckHits:       a.store.CountMemoryOps(today, "safety_hit"),
		safetyCheckMisses:     a.store.CountMemoryOps(today, "safety_miss"),
		memoriesStaled:        a.store.SumMemoriesStaled(today),
		avgSessionDurationMs:  a.store.AvgSessionDurationMs(today),
		resumedSessions:       a.store.CountResumedSessions(today),
		workflowAdherenceRate: a.store.GetWorkflowAdherenceRate(today),
		tasksPerHour:          a.store.GetTasksPerHour(today),
		avgTaskCompletionMs:   a.store.GetAvgTaskCompletionMs(today),
		replanCount:           a.store.CountOutcomeSignals(today, "replan"),
		// Bug 8 — DQ-G.5: session abandonment rate.
		abandonmentRate: a.store.GetAbandonmentRate(today),
		// Bug 23 — ROI-C5: Anthropic cache read token savings.
		cacheTokenSavings: a.store.SumCacheTokenSavings(today),
		// Bug 24 — ROI-C7: context freshness score.
		contextFreshnessScore: a.store.GetContextFreshnessScore(today),
		// Bug 25 — ROI-D5: entity memory coverage.
		entityMemoryCoverage: a.store.GetEntityMemoryCoverage(today),
		// Bug 68 — COV-9: config reloads.
		configReloads: a.store.CountConfigReloads(today),
		// Bug 69 — COV-12: persistence metrics.
		persistOps:   a.store.CountPersistOps(today),
		avgPersistMs: a.store.AvgPersistMs(today),
		// Phase 5: coverage completeness & self-refining.
		crossSessionRecallHits: a.store.CountCrossSessionRecallHits(today),
		uptimePct:              a.store.GetUptimePctForDay(today),
		avgRebuildMs:           a.store.AvgRebuildMs(today),
		federationDetections:   a.store.CountFederationDetections(today, 0),
		memoryInvalidations:    a.store.SumMemoryInvalidations(today),
		watcherViolations:      a.store.CountWatcherViolations(today),
		crossProjectAlerts:     a.store.CountCrossProjectImpactAlerts(today),
		deliverySuccessRate:    a.store.GetDeliverySuccessRateForDay(today),
		concurrentAgentsMax:    a.store.GetConcurrentAgentsMax(today),
		graphFreshnessScore:    a.store.GetGraphFreshnessScoreP5(today),
		cleanSessionRate:       a.store.GetCleanSessionRate(today),
		tokenBudgetHitRate:     a.store.GetTokenBudgetHitRate(today),
		bfsCacheHitRateP5:      a.store.GetBFSCacheHitRate(1),
	}

	metrics := buildDayMetrics(sum, reparseCount, reparseDurationMs, staleEmbeddings, p3)

	// Bug 26 — ROI-E6: embedding coverage (point-in-time, not in p3Metrics).
	metrics["embedding_coverage_pct"] = a.store.GetEmbeddingCoveragePct()

	// Bug 27 — ROI-E7: DB file size in bytes.
	if a.dbPath != "" {
		metrics["db_size_bytes"] = float64(a.store.DBSizeBytes(a.dbPath))
	}

	// Bug 28 — ROI-E8: collector drop rate (provided by caller).
	if a.collDropRate != nil {
		metrics["collector_drop_rate"] = a.collDropRate()
	}

	// P12-6: batch all metric upserts into a single transaction to reduce
	// fsync overhead from ~80 individual commits to 1.
	// Falls back to individual upserts if BeginBatch fails (e.g. DB locked).
	rollupOK := true
	commit, batchErr := a.store.BeginBatch()
	if batchErr != nil {
		logutil.Warn("pulse aggregator: begin batch (fallback to individual): %v\n", batchErr)
		// Fallback: write each metric individually (pre-P12-6 behavior).
		for metric, value := range metrics {
			if err := a.store.UpsertDailyRollup(today, metric, value); err != nil {
				logutil.Warn("pulse aggregator: upsert %s: %v\n", metric, err)
				rollupOK = false
			}
		}
	} else {
		for metric, value := range metrics {
			if err := a.store.UpsertDailyRollupTx(today, metric, value); err != nil {
				logutil.Warn("pulse aggregator: upsert %s: %v\n", metric, err)
				rollupOK = false
			}
		}
		if err := commit(rollupOK); err != nil {
			logutil.Warn("pulse aggregator: commit batch: %v\n", err)
			rollupOK = false
		}
	}

	// P5 — ROI-E1: heartbeat tick on every successful rollup cycle.
	if rollupOK {
		if hbErr := a.store.InsertHeartbeat(); hbErr != nil {
			logutil.Warn("pulse aggregator: heartbeat insert: %v\n", hbErr)
		}
	}

	// Per-dimension rollups + sentinel in a single transaction for crash-safety.
	// BeginBatch holds the store mutex; UpsertDailyRollupTx executes within the
	// held transaction. If we crash mid-rollup, the uncommitted transaction is
	// rolled back and the sentinel is never written — restart re-runs everything.
	dimCommit, dimBatchErr := a.store.BeginBatch()
	if dimBatchErr != nil {
		// Fallback: run per-dimension rollups individually (pre-transaction behavior).
		logutil.Warn("pulse aggregator: per-dimension begin batch (fallback to individual): %v\n", dimBatchErr)
		up := a.store.UpsertDailyRollup
		a.rollupPerProject(today, up)
		a.rollupPerTool(today, up)
		a.rollupPerAgent(today, up)
		a.rollupPerLanguage(today, up)
		a.rollupSearchMetrics(today, up)
		a.rollupPerToolErrors(today, up)
		if rollupOK {
			_ = a.store.UpsertDailyRollup(today, "rollup_completed", 1)
		}
	} else {
		upTx := a.store.UpsertDailyRollupTx
		a.rollupPerProject(today, upTx)
		a.rollupPerTool(today, upTx)
		a.rollupPerAgent(today, upTx)
		a.rollupPerLanguage(today, upTx)
		a.rollupSearchMetrics(today, upTx)
		a.rollupPerToolErrors(today, upTx)

		peakRate := a.store.GetPeakReparseRate(today)
		if peakRate > 0 {
			if err := a.store.UpsertDailyRollupTx(today, "peak_reparse_rate_per_min", float64(peakRate)); err != nil {
				logutil.Warn("pulse aggregator: peak reparse rate upsert: %v\n", err)
			}
		}
		if fcr, fcrErr := a.store.GetFirstContextRightRate(1); fcrErr == nil {
			if err := a.store.UpsertDailyRollupTx(today, "first_context_right_rate", fcr); err != nil {
				logutil.Warn("pulse aggregator: first_context_right_rate upsert: %v\n", err)
			}
		}
		if rollupOK {
			_ = a.store.UpsertDailyRollupTx(today, "rollup_completed", 1)
		}
		if err := dimCommit(rollupOK); err != nil {
			logutil.Warn("pulse aggregator: per-dimension commit: %v\n", err)
		}
	}

	// Automatic pruning: remove events older than 90 days.
	// Only prune if the rollup succeeded — otherwise raw data is still needed.
	if rollupOK {
		if deleted, err := a.store.PruneOldEvents(90); err != nil {
			logutil.Warn("pulse aggregator: prune error: %v\n", err)
		} else if deleted > 0 {
			logutil.Info("pulse aggregator: pruned %d old events\n", deleted)
		}

		// P2-20: run VACUUM at most once per day to reclaim space freed by DELETE.
		// VACUUM must NOT be inside a transaction — call outside PruneOldEvents.
		lastVac, _ := a.lastVacuumDay.Load().(string)
		if lastVac != today {
			if err := a.store.Vacuum(); err != nil {
				logutil.Warn("pulse aggregator: vacuum error: %v\n", err)
			} else {
				a.lastVacuumDay.Store(today)
			}
		}
	}
}

// upsertFunc is the signature shared by UpsertDailyRollup and UpsertDailyRollupTx.
// Accepting it as a parameter lets each per-dimension rollup method work both
// inside and outside a store transaction, eliminating the duplicated Tx variants.
type upsertFunc func(day, metric string, value float64) error

// rollupPerProject writes per-project rollups for a given day (Bug 21 — DQ-G.2).
// Each project gets its own daily_rollups rows keyed as "project:<id>:<metric>".
func (a *Aggregator) rollupPerProject(day string, upsert upsertFunc) {
	projects := a.store.GetProjectsForDay(day)
	for _, projectID := range projects {
		sum, err := a.store.GetSummaryForDayProject(day, projectID)
		if err != nil || sum == nil {
			continue
		}
		key := func(metric string) string {
			return fmt.Sprintf("project:%s:%s", projectID, metric)
		}
		entries := map[string]float64{
			key("tool_calls"):           float64(sum.TotalToolCalls),
			key("context_deliveries"):   float64(sum.ContextDeliveries),
			key("tokens_saved"):         float64(sum.TokensSaved),
			key("tokens_delivered"):     float64(sum.TokensDelivered),
			key("baseline_tokens"):      float64(sum.BaselineTokens),
			key("cost_saved_usd"):       sum.CostSavedUSD,
			key("sessions"):             float64(sum.Sessions),
			key("tasks_completed"):      float64(sum.TasksCompleted),
			key("cache_hits"):           float64(sum.CacheHits),
			key("brain_enriched_count"): float64(sum.BrainEnrichedCount),
			key("total_latency_ms"):     sum.TotalLatencyMs,
		}
		for metric, value := range entries {
			if err := upsert(day, metric, value); err != nil {
				logutil.Warn("pulse aggregator: per-project upsert %s: %v\n", metric, err)
			}
		}
	}
}

// rollupPerAgent writes per-agent rollups for a given day (P8-4).
// Each agent gets daily_rollups rows keyed as "agent:<id>:<metric>".
func (a *Aggregator) rollupPerAgent(day string, upsert upsertFunc) {
	agents := a.store.GetAgentsForDay(day)
	for _, agentID := range agents {
		sum, err := a.store.GetSummaryForDayAgent(day, agentID)
		if err != nil || sum == nil {
			continue
		}
		key := func(metric string) string {
			return fmt.Sprintf("agent:%s:%s", agentID, metric)
		}
		entries := map[string]float64{
			key("tool_calls"):         float64(sum.TotalToolCalls),
			key("context_deliveries"): float64(sum.ContextDeliveries),
			key("tokens_saved"):       float64(sum.TokensSaved),
			key("sessions"):           float64(sum.Sessions),
		}
		for metric, value := range entries {
			if err := upsert(day, metric, value); err != nil {
				logutil.Warn("pulse aggregator: per-agent upsert %s: %v\n", metric, err)
			}
		}
	}
}

// rollupPerLanguage writes per-language parse stats for a given day (P9-10).
// Each language gets daily_rollups rows keyed as "lang:<name>:<metric>".
func (a *Aggregator) rollupPerLanguage(day string, upsert upsertFunc) {
	stats := a.store.GetLanguageStatsForDay(day)
	for _, ls := range stats {
		key := func(metric string) string {
			return fmt.Sprintf("lang:%s:%s", ls.Language, metric)
		}
		entries := map[string]float64{
			key("parse_count"):    float64(ls.ParseCount),
			key("avg_duration_ms"): ls.AvgDurationMs,
			key("error_count"):    float64(ls.ErrorCount),
		}
		for metric, value := range entries {
			if err := upsert(day, metric, value); err != nil {
				logutil.Warn("pulse aggregator: per-language upsert %s: %v\n", metric, err)
			}
		}
	}
}

// rollupPerTool writes per-tool rollups for a given day (Bug 22 — DQ-G.3).
// Each tool gets a daily_rollups row keyed as "tool:<name>:calls".
func (a *Aggregator) rollupPerTool(day string, upsert upsertFunc) {
	toolCounts := a.store.GetTopToolsForDay(day)
	for toolName, count := range toolCounts {
		metric := fmt.Sprintf("tool:%s:calls", toolName)
		if err := upsert(day, metric, float64(count)); err != nil {
			logutil.Warn("pulse aggregator: per-tool upsert %s: %v\n", metric, err)
		}
	}
}


// backfillMissedDays computes and inserts rollups for any days between the last
// recorded rollup and today (exclusive) that are missing — handles daemon downtime
// gaps where rollup never ran. (P2-18)
func (a *Aggregator) backfillMissedDays(today string) {
	lastDay, err := a.store.GetLastRollupDay()
	if err != nil || lastDay == "" || lastDay >= today {
		return // no gaps or unable to determine
	}

	gaps, err := a.store.GetRollupGapDays(lastDay, today)
	if err != nil || len(gaps) == 0 {
		return
	}

	for _, day := range gaps {
		sum, err := a.store.GetSummaryForDay(day)
		if err != nil {
			logutil.Warn("pulse aggregator: backfill summary for %s: %v\n", day, err)
			continue
		}
		reparseCount, reparseDurationMs, _ := a.store.CountReparses(day)
		// stale_embeddings and P3 counts are point-in-time; use 0 for historical
		// backfill days since live counts cannot be reconstructed after the fact.
		// Rate/average metrics use 0.0 for historical backfill — they can't be reconstructed.
		backfillTruncated, _ := a.store.CountTruncatedDeliveries(day)
		backfillP3 := p3Metrics{
			guardCircuitBreaks:    a.store.CountGuardEvents(day, "loop_circuit_break"),
			rateLimitRejections:   a.store.CountGuardEvents(day, "rate_limit"),
			recallHits:            a.store.CountMemoryOps(day, "recall_hit"),
			recallMisses:          a.store.CountMemoryOps(day, "recall_miss"),
			validationViolations:  a.store.CountValidationViolations(day),
			errorCount:            a.store.CountToolErrors(day),
			brainCostUSD:          a.store.SumBrainCostForDay(day),
			agentLLMCostUSD:       a.store.SumAgentLLMCostForDay(day),
			truncatedDeliveries:   backfillTruncated,
			bfsCacheHits:          a.store.CountBFSCacheHitsForDay(day),
			validatePlanCount:     a.store.CountValidationCalls(day, "validate_plan"),
			memoryWrites:          a.store.CountMemoryOps(day, "write"),
			safetyCheckHits:       a.store.CountMemoryOps(day, "safety_hit"),
			safetyCheckMisses:     a.store.CountMemoryOps(day, "safety_miss"),
			memoriesStaled:        a.store.SumMemoriesStaled(day),
			resumedSessions:       a.store.CountResumedSessions(day),
			replanCount:           a.store.CountOutcomeSignals(day, "replan"),
			// Bug 8 — DQ-G.5: abandonment rate from raw data.
			abandonmentRate: a.store.GetAbandonmentRate(day),
			// Bug 23 — ROI-C5: cache token savings from raw data.
			cacheTokenSavings: a.store.SumCacheTokenSavings(day),
			// Bug 68 — COV-9: config reloads from raw data.
			configReloads: a.store.CountConfigReloads(day),
			// Bug 69 — COV-12: persistence metrics from raw data.
			persistOps:   a.store.CountPersistOps(day),
			avgPersistMs: a.store.AvgPersistMs(day),
			// P6-9: reconstruct session metrics from raw data instead of hardcoding 0.0.
			avgSessionDurationMs:  a.store.AvgSessionDurationMs(day),
			workflowAdherenceRate: a.store.GetWorkflowAdherenceRate(day),
			tasksPerHour:          a.store.GetTasksPerHour(day),
			avgTaskCompletionMs:   a.store.GetAvgTaskCompletionMs(day),
			// Bug 24 — ROI-C7: context freshness; 0.0 for backfill.
			contextFreshnessScore: 0.0,
			// Bug 25 — ROI-D5: entity memory coverage; 0.0 for backfill.
			entityMemoryCoverage: 0.0,
			// Phase 5: reconstructable counts from raw data.
			crossSessionRecallHits: a.store.CountCrossSessionRecallHits(day),
			uptimePct:              a.store.GetUptimePctForDay(day),
			avgRebuildMs:           a.store.AvgRebuildMs(day),
			federationDetections:   a.store.CountFederationDetections(day, 0),
			memoryInvalidations:    a.store.SumMemoryInvalidations(day),
			watcherViolations:      a.store.CountWatcherViolations(day),
			crossProjectAlerts:     a.store.CountCrossProjectImpactAlerts(day),
			deliverySuccessRate:    a.store.GetDeliverySuccessRateForDay(day),
			concurrentAgentsMax:    a.store.GetConcurrentAgentsMax(day),
			// Point-in-time metrics: 0.0 for historical backfill.
			graphFreshnessScore: 0.0,
			cleanSessionRate:    a.store.GetCleanSessionRate(day),
			tokenBudgetHitRate:  a.store.GetTokenBudgetHitRate(day),
			bfsCacheHitRateP5:   0.0,
		}
		metrics := buildDayMetrics(sum, reparseCount, reparseDurationMs, 0, backfillP3)
		for metric, value := range metrics {
			if err := a.store.UpsertDailyRollup(day, metric, value); err != nil {
				logutil.Warn("pulse aggregator: backfill upsert %s for %s: %v\n", metric, day, err)
			}
		}
		// Bug 21/22: also backfill per-project and per-tool for missed days.
		bfUp := a.store.UpsertDailyRollup
		a.rollupPerProject(day, bfUp)
		a.rollupPerTool(day, bfUp)
		// P8-4: backfill per-agent for missed days.
		a.rollupPerAgent(day, bfUp)
		// P9-9/P9-10: backfill per-language and peak rate for missed days.
		peakRate := a.store.GetPeakReparseRate(day)
		if peakRate > 0 {
			_ = a.store.UpsertDailyRollup(day, "peak_reparse_rate_per_min", float64(peakRate))
		}
		a.rollupPerLanguage(day, bfUp)
		// P12-4/P12-5: backfill search metrics and per-tool errors.
		a.rollupSearchMetrics(day, bfUp)
		a.rollupPerToolErrors(day, bfUp)
		logutil.Info("pulse aggregator: backfilled rollup for %s\n", day)
	}
}

// rollupSearchMetrics writes search effectiveness rollup metrics for a day (P12-4).
func (a *Aggregator) rollupSearchMetrics(day string, upsert upsertFunc) {
	zeroRate := a.store.GetSearchZeroResultRate(day)
	avgLatency := a.store.GetSearchAvgLatencyMs(day)
	// Only write if there were any searches (avoid polluting rollups with zeros).
	if zeroRate > 0 || avgLatency > 0 {
		_ = upsert(day, "search_zero_result_rate", zeroRate)
		_ = upsert(day, "search_avg_latency_ms", avgLatency)
	}
}

// rollupPerToolErrors writes per-tool error rates for a day (P12-5).
// Each tool with errors gets a daily_rollups row keyed as "tool:<name>:error_rate".
func (a *Aggregator) rollupPerToolErrors(day string, upsert upsertFunc) {
	rates := a.store.GetToolErrorRates(day)
	for _, r := range rates {
		metric := fmt.Sprintf("tool:%s:error_rate", r.ToolName)
		if err := upsert(day, metric, r.ErrorRate); err != nil {
			logutil.Warn("pulse aggregator: per-tool error rate upsert %s: %v\n", metric, err)
		}
	}
}

