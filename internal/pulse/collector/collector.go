// Package collector implements an async event buffer that decouples ingestion
// from SQLite writes. Events are accepted into a ring buffer and batch-flushed
// to the store on a configurable interval, ensuring <1ms overhead on the
// ingestion hot path.
package collector

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

// event wraps a typed event for the ring buffer.
type event struct {
	kind string // "tool_call", "context_delivery", "brain_usage", "session", "session_model", "agent_llm_usage",
	// "parse_event", "reparse_event", "graph_snapshot", "embedding_event", "index_event",
	// "guard_event", "memory_op", "validation_event", "search_event",
	// "config_reload", "persistence_event", "enrichment_event", "rule_eval_event",
	// "federation_event", "skill_execution", "tool_sequence", "heartbeat"
	data interface{}
}

// sessionPayload wraps a session event with its ID.
type sessionPayload struct {
	ID           string
	AgentID      string
	ProjectID    string
	Event        string
	AgentVersion string // Bug 16 — DQ-C.6
}

// sessionModelPayload carries the model/provider for Option A (session_init reports model).
type sessionModelPayload struct {
	SessionID string
	AgentID   string
	ProjectID string
	Model     string
	Provider  string
}

// Collector buffers analytics events and batch-writes them to the store.
type Collector struct {
	store *pulsestore.Store
	// Ring buffer: fixed-size array with head/tail indices.
	ring  []event
	head  int // index of oldest event
	tail  int // index of next write slot
	count int // number of events in the buffer
	mu       sync.Mutex
	cap      int
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
	// P2-17: high-water mark — peak buffer depth since last flush.
	highWaterMark atomic.Int64
	// P2-19: events dropped due to full buffer (ring buffer overflow).
	dropped        atomic.Int64
	lastLoggedDrops atomic.Int64 // last dropped count we logged — avoids repeated warnings
	enqueued       atomic.Int64
	// P5 — DQ-Integrity.1: write errors during batch persistence.
	writeErrors atomic.Int64
	// earlyFlushRunning prevents concurrent early-flush goroutines. Only one
	// early-flush goroutine is allowed at a time; additional triggers are skipped.
	earlyFlushRunning atomic.Int32
	// P12-7: optional callback invoked after each flush with the batch size.
	OnFlush func(count int)
}

// New creates a Collector with the given buffer capacity and flush interval.
func New(st *pulsestore.Store, capacity int, flushIntervalMs int) *Collector {
	if capacity <= 0 {
		capacity = 1000
	}
	if flushIntervalMs <= 0 {
		flushIntervalMs = 500
	}
	return &Collector{
		store:    st,
		ring:     make([]event, capacity),
		cap:      capacity,
		interval: time.Duration(flushIntervalMs) * time.Millisecond,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the background flush goroutine.
func (c *Collector) Start() {
	c.wg.Add(1)
	go c.flushLoop()
}

// Stop signals the flush loop to exit and waits for a final flush.
func (c *Collector) Stop() {
	close(c.stopCh)
	c.wg.Wait()
}

// RecordToolCall enqueues a tool call event.
func (c *Collector) RecordToolCall(ev pulsetypes.ToolCallEvent) {
	c.enqueue(event{kind: "tool_call", data: ev})
}

// RecordContextDelivery enqueues a context delivery event.
func (c *Collector) RecordContextDelivery(ev pulsetypes.ContextDeliveryEvent) {
	c.enqueue(event{kind: "context_delivery", data: ev})
}

// RecordBrainUsage enqueues a brain usage event.
func (c *Collector) RecordBrainUsage(ev pulsetypes.BrainUsageEvent) {
	c.enqueue(event{kind: "brain_usage", data: ev})
}

// RecordSessionEvent enqueues a session lifecycle event.
func (c *Collector) RecordSessionEvent(id, agentID, projectID, eventType string) {
	c.enqueue(event{kind: "session", data: sessionPayload{
		ID: id, AgentID: agentID, ProjectID: projectID, Event: eventType,
	}})
}

// RecordSessionEventFull enqueues a session lifecycle event with agent version (Bug 16 — DQ-C.6).
func (c *Collector) RecordSessionEventFull(id, agentID, projectID, eventType, agentVersion string) {
	c.enqueue(event{kind: "session", data: sessionPayload{
		ID: id, AgentID: agentID, ProjectID: projectID, Event: eventType, AgentVersion: agentVersion,
	}})
}

// RecordOutcomeSignal enqueues an intent alignment outcome signal (R29).
func (c *Collector) RecordOutcomeSignal(ev pulsetypes.OutcomeSignalEvent) {
	c.enqueue(event{kind: "outcome_signal", data: ev})
}

// RecordSessionModel enqueues a model/provider update for an existing session.
// Called when an agent reports its model via session_init (Option A).
func (c *Collector) RecordSessionModel(sessionID, agentID, projectID, model, provider string) {
	c.enqueue(event{kind: "session_model", data: sessionModelPayload{
		SessionID: sessionID, AgentID: agentID, ProjectID: projectID,
		Model: model, Provider: provider,
	}})
}

// RecordAgentLLMUsage enqueues an agent-reported LLM usage event (Option B).
func (c *Collector) RecordAgentLLMUsage(ev pulsetypes.AgentLLMUsageEvent) {
	c.enqueue(event{kind: "agent_llm_usage", data: ev})
}

// RecordParseEvent enqueues a per-file parse event (P2-2).
func (c *Collector) RecordParseEvent(ev pulsetypes.ParseEvent) {
	c.enqueue(event{kind: "parse_event", data: ev})
}

// RecordReparseEvent enqueues an incremental reparse event (P2-3).
func (c *Collector) RecordReparseEvent(ev pulsetypes.ReparseEvent) {
	c.enqueue(event{kind: "reparse_event", data: ev})
}

// RecordGraphSnapshot enqueues a graph topology snapshot (P2-7).
func (c *Collector) RecordGraphSnapshot(ev pulsetypes.GraphSnapshotEvent) {
	c.enqueue(event{kind: "graph_snapshot", data: ev})
}

// RecordEmbeddingEvent enqueues an embedding batch event (P2-6).
func (c *Collector) RecordEmbeddingEvent(ev pulsetypes.EmbeddingEvent) {
	c.enqueue(event{kind: "embedding_event", data: ev})
}

// RecordIndexEvent enqueues a full-index completion event (P2-8).
func (c *Collector) RecordIndexEvent(ev pulsetypes.IndexEvent) {
	c.enqueue(event{kind: "index_event", data: ev})
}

// RecordGuardEvent enqueues a loop-guard or rate-limiter block event (P3-2/P3-3).
func (c *Collector) RecordGuardEvent(ev pulsetypes.GuardEvent) {
	c.enqueue(event{kind: "guard_event", data: ev})
}

// RecordMemoryOp enqueues a recall hit/miss or memory write event (P3-4).
func (c *Collector) RecordMemoryOp(ev pulsetypes.MemoryOperationEvent) {
	c.enqueue(event{kind: "memory_op", data: ev})
}

// RecordValidationEvent enqueues a validate_plan or verify_implementation outcome event (P3-5).
func (c *Collector) RecordValidationEvent(ev pulsetypes.ValidationEvent) {
	c.enqueue(event{kind: "validation_event", data: ev})
}

// RecordSearchEvent enqueues a search or find_entity analytics event (P4-8).
func (c *Collector) RecordSearchEvent(ev pulsetypes.SearchEvent) {
	c.enqueue(event{kind: "search_event", data: ev})
}

// RecordConfigReload enqueues a configuration hot-reload event (Bug 68 — COV-9).
func (c *Collector) RecordConfigReload(ev pulsetypes.ConfigReloadEvent) {
	c.enqueue(event{kind: "config_reload", data: ev})
}

// RecordPersistenceEvent enqueues a store write duration/size event (Bug 69 — COV-12).
func (c *Collector) RecordPersistenceEvent(ev pulsetypes.PersistenceEvent) {
	c.enqueue(event{kind: "persistence_event", data: ev})
}

// RecordEnrichmentEvent enqueues a code enrichment pass outcome event (Bug 70 — COV-Subsys).
func (c *Collector) RecordEnrichmentEvent(ev pulsetypes.EnrichmentEvent) {
	c.enqueue(event{kind: "enrichment_event", data: ev})
}

// RecordRuleEvalEvent enqueues an architecture rule evaluation event (Bug 71 — COV-Subsys).
func (c *Collector) RecordRuleEvalEvent(ev pulsetypes.RuleEvalEvent) {
	c.enqueue(event{kind: "rule_eval_event", data: ev})
}

// RecordFederationEvent enqueues a federation detection event (P5 — COV-8).
func (c *Collector) RecordFederationEvent(ev pulsetypes.FederationDetectEvent) {
	c.enqueue(event{kind: "federation_event", data: ev})
}

// RecordSkillExecution enqueues a skill execution event (P5 — COV-15).
func (c *Collector) RecordSkillExecution(ev pulsetypes.SkillExecutionEvent) {
	c.enqueue(event{kind: "skill_execution", data: ev})
}

// RecordToolSequenceEntry enqueues a tool call sequence entry (P5 — SA-C1).
func (c *Collector) RecordToolSequenceEntry(sessionID, toolName string, position int, success bool) {
	c.enqueue(event{kind: "tool_sequence", data: pulsetypes.ToolSequenceEntry{
		SessionID: sessionID, ToolName: toolName, Position: position, Success: success,
	}})
}

// RecordHeartbeat enqueues a system uptime heartbeat tick (P5 — ROI-E1).
func (c *Collector) RecordHeartbeat() {
	c.enqueue(event{kind: "heartbeat", data: nil})
}

// Dropped returns the number of events dropped due to buffer overflow (P2-19).
func (c *Collector) Dropped() int64 {
	return c.dropped.Load()
}

// HighWaterMark returns the peak buffer depth since the collector started (P2-17).
func (c *Collector) HighWaterMark() int64 {
	return c.highWaterMark.Load()
}

// DropRate returns the fraction of enqueued events that were dropped (Bug 28 — ROI-E8).
// Returns 0.0 if no events have been dropped or the high-water mark is zero.
func (c *Collector) DropRate() float64 {
	enqueued := c.enqueued.Load()
	if enqueued <= 0 {
		return 0.0
	}
	dropped := c.dropped.Load()
	if dropped <= 0 {
		return 0.0
	}
	return float64(dropped) / float64(enqueued)
}

// WriteErrors returns the total number of batch-write errors since collector start (P5 — DQ-Integrity.1).
func (c *Collector) WriteErrors() int64 {
	return c.writeErrors.Load()
}

func (c *Collector) enqueue(ev event) {
	c.enqueued.Add(1)
	c.mu.Lock()

	// O(1) ring buffer enqueue — drop the oldest event when full.
	if c.count == c.cap {
		c.head = (c.head + 1) % c.cap
		c.dropped.Add(1)
	} else {
		c.count++
	}
	c.ring[c.tail] = ev
	c.tail = (c.tail + 1) % c.cap

	// P2-17: update high-water mark.
	if depth := int64(c.count); depth > c.highWaterMark.Load() {
		c.highWaterMark.Store(depth)
	}

	// If buffer is at 80% capacity, trigger an early flush in the background.
	// Only allow one concurrent early-flush goroutine to prevent unbounded goroutine spawning.
	if c.count >= c.cap*80/100 && c.earlyFlushRunning.CompareAndSwap(0, 1) {
		batch := c.drainLocked()
		c.mu.Unlock()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer c.earlyFlushRunning.Store(0)
			c.writeBatch(batch)
		}()
		return
	}
	c.mu.Unlock()
}

// drainLocked reads all events from the ring buffer and resets it. Caller must hold mu.
func (c *Collector) drainLocked() []event {
	if c.count == 0 {
		return nil
	}
	batch := make([]event, c.count)
	if c.head < c.tail {
		copy(batch, c.ring[c.head:c.tail])
	} else {
		n := copy(batch, c.ring[c.head:])
		copy(batch[n:], c.ring[:c.tail])
	}
	c.head = 0
	c.tail = 0
	c.count = 0
	return batch
}

func (c *Collector) flushLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.flush()
		case <-c.stopCh:
			c.flush() // Final flush
			return
		}
	}
}

func (c *Collector) flush() {
	c.mu.Lock()
	if c.count == 0 {
		c.mu.Unlock()
		return
	}
	batch := c.drainLocked()
	c.mu.Unlock()

	// Log a warning if events were dropped since the last flush.
	if dropped := c.dropped.Load(); dropped > 0 && dropped > c.lastLoggedDrops.Load() {
		logutil.Warn("pulse collector: %d events dropped (ring buffer full, cap=%d)\n", dropped, c.cap)
		c.lastLoggedDrops.Store(dropped)
	}

	c.writeBatch(batch)

	// P12-7: notify SSE subscribers after flush.
	if c.OnFlush != nil {
		c.OnFlush(len(batch))
	}
}

func (c *Collector) writeBatch(batch []event) {
	// Wrap the entire batch in a single transaction (1 fsync instead of N).
	commit, txErr := c.store.BeginBatch()
	if txErr != nil {
		logutil.Warn("pulse collector: begin batch tx: %v\n", txErr)
		// Fall back to non-transactional writes.
		c.writeBatchNoTx(batch)
		return
	}

	// Panic recovery: if a type assertion or other bug panics, we must
	// rollback and release the mutex to avoid a permanent deadlock.
	ok := true
	defer func() {
		if r := recover(); r != nil {
			logutil.Warn("pulse collector: panic in writeBatch: %v\n", r)
			ok = false
		}
		if err := commit(ok); err != nil {
			logutil.Warn("pulse collector: commit/rollback error: %v\n", err)
			if ok {
				// Commit failed — fall back to individual writes.
				c.writeBatchNoTx(batch)
			}
		}
	}()

	for _, ev := range batch {
		if err := c.dispatchTx(ev); err != nil {
			logutil.Warn("pulse collector: write error (%s): %v\n", ev.kind, err)
			c.writeErrors.Add(1)
			// P6-7: skip the bad event and continue processing the rest of the
			// batch. Only unrecoverable errors (disk full, DB locked) should
			// abort the entire transaction — single-row constraint or type
			// errors should not kill the whole flush.
			errMsg := err.Error()
			if strings.Contains(errMsg, "disk") || strings.Contains(errMsg, "readonly") ||
				strings.Contains(errMsg, "SQLITE_FULL") || strings.Contains(errMsg, "database is locked") {
				ok = false
				break
			}
			// Non-fatal: log and skip.
			continue
		}
	}
}

// dispatchTx writes a single event using the in-transaction Tx methods.
// Caller must hold the open transaction (via BeginBatch).
func (c *Collector) dispatchTx(ev event) error {
	switch ev.kind {
	case "tool_call":
		tc, ok := ev.data.(pulsetypes.ToolCallEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		if err := c.store.InsertToolCallTx(tc); err != nil {
			return err
		}
		agentID := tc.AgentID
		if agentID == "" {
			agentID = "default"
		}
		sessionID := tc.SessionID
		if sessionID == "" {
			sessionID = agentID + ":" + tc.ProjectID + ":" + time.Now().UTC().Format("2006-01-02")
		}
		if serr := c.store.UpdateSessionStatsTx(sessionID, agentID, tc.ProjectID, 0, 0); serr != nil {
			logutil.Warn("pulse collector: update session stats: %v\n", serr)
		}
	case "context_delivery":
		cd, ok := ev.data.(pulsetypes.ContextDeliveryEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		if err := c.store.InsertContextDeliveryTx(cd); err != nil {
			return err
		}
		agentID := cd.AgentID
		if agentID == "" {
			agentID = "default"
		}
		tokensSaved := cd.BaselineTokens - cd.ResponseTokens
		if tokensSaved < 0 {
			tokensSaved = 0
		}
		// Bug 1 — DQ-H.3: look up the session model for accurate cost pricing.
		sessionID := cd.SessionID
		if sessionID == "" {
			sessionID = agentID + ":" + cd.ProjectID + ":" + time.Now().UTC().Format("2006-01-02")
		}
		model := c.store.GetSessionModel(sessionID)
		if model == "" {
			model = "claude-sonnet-4-6"
		}
		costSaved := c.computeCostSaved(tokensSaved, model)
		if serr := c.store.AddSessionTokensSavedTx(sessionID, agentID, cd.ProjectID, tokensSaved, costSaved); serr != nil {
			logutil.Warn("pulse collector: add session tokens saved: %v\n", serr)
		}
	case "brain_usage":
		bu, ok := ev.data.(pulsetypes.BrainUsageEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertBrainUsageTx(bu)
	case "session":
		sp, ok := ev.data.(sessionPayload)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		// Bug 16 — DQ-C.6: pass agent version through to the store.
		return c.store.UpsertSessionWithVersionTx(sp.ID, sp.AgentID, sp.ProjectID, sp.Event, sp.AgentVersion)
	case "outcome_signal":
		os, ok := ev.data.(pulsetypes.OutcomeSignalEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		if err := c.store.InsertOutcomeSignalTx(os); err != nil {
			return err
		}
		// Sprint 15 #2: recompute quality score WITHIN the same transaction so
		// the SELECT in UpdateEntityQualityScore sees the just-inserted signal.
		// Calling this after the insert (not before) is what makes the score
		// correct — the call-site fire-and-forget pattern in task_tools.go and
		// context_signals.go ran before the flush and always missed the new signal.
		c.store.UpdateEntityQualityScore(os.Entity, os.ProjectID)
		return nil
	case "session_model":
		sp, ok := ev.data.(sessionModelPayload)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.UpdateSessionModelTx(sp.SessionID, sp.AgentID, sp.ProjectID, sp.Model, sp.Provider)
	case "agent_llm_usage":
		au, ok := ev.data.(pulsetypes.AgentLLMUsageEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertAgentLLMUsageTx(au)
	case "parse_event":
		pe, ok := ev.data.(pulsetypes.ParseEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertParseEventTx(pe)
	case "reparse_event":
		re, ok := ev.data.(pulsetypes.ReparseEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertReparseEventTx(re)
	case "graph_snapshot":
		gs, ok := ev.data.(pulsetypes.GraphSnapshotEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertGraphSnapshotTx(gs)
	case "embedding_event":
		ee, ok := ev.data.(pulsetypes.EmbeddingEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertEmbeddingEventTx(ee)
	case "index_event":
		ie, ok := ev.data.(pulsetypes.IndexEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertIndexEventTx(ie)
	case "guard_event":
		ge, ok := ev.data.(pulsetypes.GuardEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertGuardEventTx(ge)
	case "memory_op":
		mo, ok := ev.data.(pulsetypes.MemoryOperationEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertMemoryOpTx(mo)
	case "validation_event":
		ve, ok := ev.data.(pulsetypes.ValidationEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertValidationEventTx(ve)
	case "search_event":
		se, ok := ev.data.(pulsetypes.SearchEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertSearchEventTx(se)
	case "config_reload":
		cr, ok := ev.data.(pulsetypes.ConfigReloadEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertConfigReloadEventTx(cr)
	case "persistence_event":
		pe, ok := ev.data.(pulsetypes.PersistenceEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertPersistenceEventTx(pe)
	case "enrichment_event":
		ee, ok := ev.data.(pulsetypes.EnrichmentEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertEnrichmentEventTx(ee)
	case "rule_eval_event":
		re, ok := ev.data.(pulsetypes.RuleEvalEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertRuleEvalEventTx(re)
	case "federation_event":
		fe, ok := ev.data.(pulsetypes.FederationDetectEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertFederationEventTx(fe)
	case "skill_execution":
		se, ok := ev.data.(pulsetypes.SkillExecutionEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertSkillExecutionTx(se)
	case "tool_sequence":
		ts, ok := ev.data.(pulsetypes.ToolSequenceEntry)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertToolSequenceEntryTx(ts.SessionID, ts.ToolName, ts.Position, ts.Success)
	case "heartbeat":
		return c.store.InsertHeartbeatTx()
	}
	return nil
}

// writeBatchNoTx is the fallback for when transaction creation fails.
// Each event is written individually; errors are logged but never halt the loop
// (unlike dispatchTx, partial progress is acceptable here since there's no
// transaction to roll back).
func (c *Collector) writeBatchNoTx(batch []event) {
	for _, ev := range batch {
		if err := c.dispatchNoTx(ev); err != nil {
			logutil.Warn("pulse collector: write error (%s): %v\n", ev.kind, err)
			c.writeErrors.Add(1)
		}
	}
}

// dispatchNoTx writes a single event using the non-transactional store methods.
// Used as the BeginBatch fallback path; each call acquires its own store mutex.
func (c *Collector) dispatchNoTx(ev event) error {
	switch ev.kind {
	case "tool_call":
		tc, ok := ev.data.(pulsetypes.ToolCallEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		if err := c.store.InsertToolCall(tc); err != nil {
			return err
		}
		agentID := tc.AgentID
		if agentID == "" {
			agentID = "default"
		}
		sessionID := tc.SessionID
		if sessionID == "" {
			sessionID = agentID + ":" + tc.ProjectID + ":" + time.Now().UTC().Format("2006-01-02")
		}
		if serr := c.store.UpdateSessionStats(sessionID, agentID, tc.ProjectID, 0, 0); serr != nil {
			logutil.Warn("pulse collector: update session stats: %v\n", serr)
		}
	case "context_delivery":
		cd, ok := ev.data.(pulsetypes.ContextDeliveryEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		if err := c.store.InsertContextDelivery(cd); err != nil {
			return err
		}
		agentID := cd.AgentID
		if agentID == "" {
			agentID = "default"
		}
		tokensSaved := cd.BaselineTokens - cd.ResponseTokens
		if tokensSaved < 0 {
			tokensSaved = 0
		}
		// Bug 1 — DQ-H.3: look up the session model for accurate cost pricing.
		sessionID := cd.SessionID
		if sessionID == "" {
			sessionID = agentID + ":" + cd.ProjectID + ":" + time.Now().UTC().Format("2006-01-02")
		}
		model := c.store.GetSessionModel(sessionID)
		if model == "" {
			model = "claude-sonnet-4-6"
		}
		costSaved := c.computeCostSaved(tokensSaved, model)
		if serr := c.store.AddSessionTokensSaved(sessionID, agentID, cd.ProjectID, tokensSaved, costSaved); serr != nil {
			logutil.Warn("pulse collector: add session tokens saved: %v\n", serr)
		}
	case "brain_usage":
		bu, ok := ev.data.(pulsetypes.BrainUsageEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertBrainUsage(bu)
	case "session":
		sp, ok := ev.data.(sessionPayload)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		// Bug 16 — DQ-C.6: pass agent version through to the store.
		return c.store.UpsertSessionWithVersion(sp.ID, sp.AgentID, sp.ProjectID, sp.Event, sp.AgentVersion)
	case "outcome_signal":
		os, ok := ev.data.(pulsetypes.OutcomeSignalEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		if err := c.store.InsertOutcomeSignal(os); err != nil {
			return err
		}
		// Sprint 15 #2: recompute quality score immediately after the signal
		// write so the SELECT aggregation sees the new row.
		c.store.UpdateEntityQualityScore(os.Entity, os.ProjectID)
		return nil
	case "session_model":
		sp, ok := ev.data.(sessionModelPayload)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.UpdateSessionModel(sp.SessionID, sp.AgentID, sp.ProjectID, sp.Model, sp.Provider)
	case "agent_llm_usage":
		au, ok := ev.data.(pulsetypes.AgentLLMUsageEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertAgentLLMUsage(au)
	case "parse_event":
		pe, ok := ev.data.(pulsetypes.ParseEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertParseEvent(pe)
	case "reparse_event":
		re, ok := ev.data.(pulsetypes.ReparseEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertReparseEvent(re)
	case "graph_snapshot":
		gs, ok := ev.data.(pulsetypes.GraphSnapshotEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertGraphSnapshot(gs)
	case "embedding_event":
		ee, ok := ev.data.(pulsetypes.EmbeddingEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertEmbeddingEvent(ee)
	case "index_event":
		ie, ok := ev.data.(pulsetypes.IndexEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertIndexEvent(ie)
	case "guard_event":
		ge, ok := ev.data.(pulsetypes.GuardEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertGuardEvent(ge)
	case "memory_op":
		mo, ok := ev.data.(pulsetypes.MemoryOperationEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertMemoryOp(mo)
	case "validation_event":
		ve, ok := ev.data.(pulsetypes.ValidationEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertValidationEvent(ve)
	case "search_event":
		se, ok := ev.data.(pulsetypes.SearchEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertSearchEvent(se)
	case "config_reload":
		cr, ok := ev.data.(pulsetypes.ConfigReloadEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertConfigReloadEvent(cr)
	case "persistence_event":
		pe, ok := ev.data.(pulsetypes.PersistenceEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertPersistenceEvent(pe)
	case "enrichment_event":
		ee, ok := ev.data.(pulsetypes.EnrichmentEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertEnrichmentEvent(ee)
	case "rule_eval_event":
		re, ok := ev.data.(pulsetypes.RuleEvalEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertRuleEvalEvent(re)
	case "federation_event":
		fe, ok := ev.data.(pulsetypes.FederationDetectEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertFederationEvent(fe)
	case "skill_execution":
		se, ok := ev.data.(pulsetypes.SkillExecutionEvent)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertSkillExecution(se)
	case "tool_sequence":
		ts, ok := ev.data.(pulsetypes.ToolSequenceEntry)
		if !ok {
			logutil.Warn("pulse collector: type assertion failed for %s\n", ev.kind)
			return nil
		}
		return c.store.InsertToolSequenceEntry(ts.SessionID, ts.ToolName, ts.Position, ts.Success)
	case "heartbeat":
		return c.store.InsertHeartbeat()
	}
	return nil
}

// computeCostSaved estimates the USD value of tokensSaved using the provided
// model's pricing (Bug 1 — DQ-H.3). Falls back to gpt-4o rates if the model
// is not found in the pricing table.
func (c *Collector) computeCostSaved(tokensSaved int, model string) float64 {
	if tokensSaved <= 0 {
		return 0
	}
	inputPer1M, _, found := c.store.GetPricing(model)
	if !found || inputPer1M <= 0 {
		// Fall back to gpt-4o as canonical high-value agent baseline.
		inputPer1M, _, found = c.store.GetPricing("gpt-4o")
		if !found || inputPer1M <= 0 {
			return 0
		}
	}
	return float64(tokensSaved) / 1_000_000.0 * inputPer1M
}

// Len returns the current buffer length (for diagnostics).
func (c *Collector) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}
