// Package aggregator pre-computes daily rollup metrics for fast dashboard queries.
// It runs on a configurable interval and writes to the daily_rollups table.
package aggregator

import (
	"log"
	"sync"
	"time"

	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
)

// Aggregator periodically rolls up raw events into daily summaries.
type Aggregator struct {
	store    *pulsestore.Store
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
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

// rollup computes today's metrics and writes them to daily_rollups.
// Uses calendar-day boundaries (WHERE date(created_at) = today) rather than
// a rolling 24-hour window so each day's rollup represents exactly midnight→midnight.
func (a *Aggregator) rollup() {
	today := time.Now().UTC().Format("2006-01-02")

	sum, err := a.store.GetSummaryForDay(today)
	if err != nil {
		log.Printf("pulse aggregator: summary error: %v", err)
		return
	}

	metrics := map[string]float64{
		"tokens_saved":          float64(sum.TokensSaved),
		"tokens_delivered":      float64(sum.TokensDelivered),
		"baseline_tokens":       float64(sum.BaselineTokens),
		"tool_calls":            float64(sum.TotalToolCalls),
		"savings_pct":           sum.SavingsPct,
		"compression":           sum.CompressionRatio,
		"cache_hit_rate":        sum.CacheHitRate,
		"brain_enrichment_rate": sum.BrainEnrichRate,
		"avg_latency_ms":        sum.AvgLatencyMs,
		"context_deliveries":    float64(sum.ContextDeliveries),
		"cost_saved_usd":        sum.CostSavedUSD,
		"sessions":              float64(sum.Sessions),
		"tasks_completed":       float64(sum.TasksCompleted),
	}

	for metric, value := range metrics {
		if err := a.store.UpsertDailyRollup(today, metric, value); err != nil {
			log.Printf("pulse aggregator: upsert %s: %v", metric, err)
		}
	}
}

// RollupNow triggers an immediate rollup (useful for CLI).
func (a *Aggregator) RollupNow() {
	a.rollup()
}
