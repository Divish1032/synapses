// Package aggregator pre-computes daily rollup metrics for fast dashboard queries.
// It runs on a configurable interval and writes to the daily_rollups table.
package aggregator

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
)

// Aggregator periodically rolls up raw events into daily summaries.
type Aggregator struct {
	store        *pulsestore.Store
	interval     time.Duration
	stopCh       chan struct{}
	wg           sync.WaitGroup
	// P2-20: track last vacuum time so it runs at most once per day.
	lastVacuumDay atomic.Value // stores string "YYYY-MM-DD"
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

// Start begins the rollup loop. It runs an immediate rollup, then repeats
// on the configured interval.
func (a *Aggregator) Start() {
	a.wg.Add(1)
	go a.loop()
}

// Stop signals the loop to exit and waits for completion.
func (a *Aggregator) Stop() {
	close(a.stopCh)
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
	guardCircuitBreaks  int
	rateLimitRejections int
	recallHits          int
	recallMisses        int
	validationViolations int
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
	}
}

// rollup computes today's metrics and writes them to daily_rollups, then prunes
// old events beyond the retention window.
// Uses calendar-day boundaries via created_date column instead of date() function.
func (a *Aggregator) rollup() {
	today := time.Now().UTC().Format("2006-01-02")

	// P2-18: backfill rollups for any days missed due to daemon downtime.
	a.backfillMissedDays(today)

	sum, err := a.store.GetSummaryForDay(today)
	if err != nil {
		log.Printf("pulse aggregator: summary error: %v", err)
		return
	}

	// P2-10: reparse daily counts. reparseCount also serves as file_changes_count
	// (same underlying table) — no separate CountFileChanges query needed.
	reparseCount, reparseDurationMs, _ := a.store.CountReparses(today)

	// P2-16: stale embedding count.
	staleEmbeddings := a.store.CountStaleEmbeddings()

	// P3: agent behavior counts for today.
	p3 := p3Metrics{
		guardCircuitBreaks:   a.store.CountGuardEvents(today, "loop_circuit_break"),
		rateLimitRejections:  a.store.CountGuardEvents(today, "rate_limit"),
		recallHits:           a.store.CountMemoryOps(today, "recall_hit"),
		recallMisses:         a.store.CountMemoryOps(today, "recall_miss"),
		validationViolations: a.store.CountValidationViolations(today),
	}

	metrics := buildDayMetrics(sum, reparseCount, reparseDurationMs, staleEmbeddings, p3)

	rollupOK := true
	for metric, value := range metrics {
		if err := a.store.UpsertDailyRollup(today, metric, value); err != nil {
			log.Printf("pulse aggregator: upsert %s: %v", metric, err)
			rollupOK = false
		}
	}

	// Automatic pruning: remove events older than 90 days.
	// Only prune if the rollup succeeded — otherwise raw data is still needed.
	if rollupOK {
		if deleted, err := a.store.PruneOldEvents(90); err != nil {
			log.Printf("pulse aggregator: prune error: %v", err)
		} else if deleted > 0 {
			log.Printf("pulse aggregator: pruned %d old events", deleted)
		}

		// P2-20: run VACUUM at most once per day to reclaim space freed by DELETE.
		// VACUUM must NOT be inside a transaction — call outside PruneOldEvents.
		lastVac, _ := a.lastVacuumDay.Load().(string)
		if lastVac != today {
			if err := a.store.Vacuum(); err != nil {
				log.Printf("pulse aggregator: vacuum error: %v", err)
			} else {
				a.lastVacuumDay.Store(today)
			}
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
			log.Printf("pulse aggregator: backfill summary for %s: %v", day, err)
			continue
		}
		reparseCount, reparseDurationMs, _ := a.store.CountReparses(day)
		// stale_embeddings and P3 counts are point-in-time; use 0 for historical
		// backfill days since live counts cannot be reconstructed after the fact.
		backfillP3 := p3Metrics{
			guardCircuitBreaks:   a.store.CountGuardEvents(day, "loop_circuit_break"),
			rateLimitRejections:  a.store.CountGuardEvents(day, "rate_limit"),
			recallHits:           a.store.CountMemoryOps(day, "recall_hit"),
			recallMisses:         a.store.CountMemoryOps(day, "recall_miss"),
			validationViolations: a.store.CountValidationViolations(day),
		}
		metrics := buildDayMetrics(sum, reparseCount, reparseDurationMs, 0, backfillP3)
		for metric, value := range metrics {
			if err := a.store.UpsertDailyRollup(day, metric, value); err != nil {
				log.Printf("pulse aggregator: backfill upsert %s for %s: %v", metric, day, err)
			}
		}
		log.Printf("pulse aggregator: backfilled rollup for %s", day)
	}
}

// RollupNow triggers an immediate rollup (useful for CLI).
func (a *Aggregator) RollupNow() {
	a.rollup()
}
