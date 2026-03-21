// Package collector implements an async event buffer that decouples ingestion
// from SQLite writes. Events are accepted into a ring buffer and batch-flushed
// to the store on a configurable interval, ensuring <1ms overhead on the
// ingestion hot path.
package collector

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

// event wraps a typed event for the ring buffer.
type event struct {
	kind string // "tool_call", "context_delivery", "brain_usage", "session", "session_model", "agent_llm_usage",
	// "parse_event", "reparse_event", "graph_snapshot", "embedding_event", "index_event",
	// "guard_event", "memory_op", "validation_event"
	data interface{}
}

// sessionPayload wraps a session event with its ID.
type sessionPayload struct {
	ID        string
	AgentID   string
	ProjectID string
	Event     string
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
	store    *pulsestore.Store
	buf      []event
	mu       sync.Mutex
	cap      int
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
	// P2-17: high-water mark — peak buffer depth since last flush.
	highWaterMark atomic.Int64
	// P2-19: events dropped due to full buffer (ring buffer overflow).
	dropped atomic.Int64
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
		buf:      make([]event, 0, capacity),
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

// Dropped returns the number of events dropped due to buffer overflow (P2-19).
func (c *Collector) Dropped() int64 {
	return c.dropped.Load()
}

// HighWaterMark returns the peak buffer depth since the collector started (P2-17).
func (c *Collector) HighWaterMark() int64 {
	return c.highWaterMark.Load()
}

func (c *Collector) enqueue(ev event) {
	c.mu.Lock()

	// P2-19: true bounded ring buffer — drop the oldest event when full.
	if len(c.buf) >= c.cap {
		// Remove oldest event to make room (ring buffer semantics).
		c.buf = c.buf[1:]
		c.dropped.Add(1)
	}

	c.buf = append(c.buf, ev)

	// P2-17: update high-water mark.
	if depth := int64(len(c.buf)); depth > c.highWaterMark.Load() {
		c.highWaterMark.Store(depth)
	}

	// If buffer is at 80% capacity, trigger an early flush in the background.
	if len(c.buf) >= c.cap*80/100 {
		batch := c.drainLocked()
		c.mu.Unlock()
		go c.writeBatch(batch)
		return
	}
	c.mu.Unlock()
}

// drainLocked swaps the buffer and returns the old batch. Caller must hold mu.
func (c *Collector) drainLocked() []event {
	batch := c.buf
	c.buf = make([]event, 0, c.cap)
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
	if len(c.buf) == 0 {
		c.mu.Unlock()
		return
	}
	batch := c.drainLocked()
	c.mu.Unlock()

	c.writeBatch(batch)
}

func (c *Collector) writeBatch(batch []event) {
	// Wrap the entire batch in a single transaction (1 fsync instead of N).
	commit, txErr := c.store.BeginBatch()
	if txErr != nil {
		log.Printf("pulse collector: begin batch tx: %v", txErr)
		// Fall back to non-transactional writes.
		c.writeBatchNoTx(batch)
		return
	}

	// Panic recovery: if a type assertion or other bug panics, we must
	// rollback and release the mutex to avoid a permanent deadlock.
	ok := true
	defer func() {
		if r := recover(); r != nil {
			log.Printf("pulse collector: panic in writeBatch: %v", r)
			ok = false
		}
		if err := commit(ok); err != nil {
			log.Printf("pulse collector: commit/rollback error: %v", err)
			if ok {
				// Commit failed — fall back to individual writes.
				c.writeBatchNoTx(batch)
			}
		}
	}()

	for _, ev := range batch {
		if err := c.dispatchTx(ev); err != nil {
			log.Printf("pulse collector: write error (%s): %v", ev.kind, err)
			ok = false
			// Break immediately — the deferred commit(ok=false) rolls back the
			// entire transaction, so continuing would just burn CPU for nothing.
			break
		}
	}
}

// dispatchTx writes a single event using the in-transaction Tx methods.
// Caller must hold the open transaction (via BeginBatch).
func (c *Collector) dispatchTx(ev event) error {
	switch ev.kind {
	case "tool_call":
		tc, _ := ev.data.(pulsetypes.ToolCallEvent)
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
			log.Printf("pulse collector: update session stats: %v", serr)
		}
	case "context_delivery":
		cd, _ := ev.data.(pulsetypes.ContextDeliveryEvent)
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
		costSaved := c.computeCostSaved(tokensSaved)
		sessionID := cd.SessionID
		if sessionID == "" {
			sessionID = agentID + ":" + cd.ProjectID + ":" + time.Now().UTC().Format("2006-01-02")
		}
		if serr := c.store.AddSessionTokensSavedTx(sessionID, agentID, cd.ProjectID, tokensSaved, costSaved); serr != nil {
			log.Printf("pulse collector: add session tokens saved: %v", serr)
		}
	case "brain_usage":
		bu, _ := ev.data.(pulsetypes.BrainUsageEvent)
		return c.store.InsertBrainUsageTx(bu)
	case "session":
		sp, _ := ev.data.(sessionPayload)
		return c.store.UpsertSessionTx(sp.ID, sp.AgentID, sp.ProjectID, sp.Event)
	case "outcome_signal":
		os, _ := ev.data.(pulsetypes.OutcomeSignalEvent)
		return c.store.InsertOutcomeSignalTx(os)
	case "session_model":
		sp, _ := ev.data.(sessionModelPayload)
		return c.store.UpdateSessionModelTx(sp.SessionID, sp.AgentID, sp.ProjectID, sp.Model, sp.Provider)
	case "agent_llm_usage":
		au, _ := ev.data.(pulsetypes.AgentLLMUsageEvent)
		return c.store.InsertAgentLLMUsageTx(au)
	case "parse_event":
		pe, _ := ev.data.(pulsetypes.ParseEvent)
		return c.store.InsertParseEventTx(pe)
	case "reparse_event":
		re, _ := ev.data.(pulsetypes.ReparseEvent)
		return c.store.InsertReparseEventTx(re)
	case "graph_snapshot":
		gs, _ := ev.data.(pulsetypes.GraphSnapshotEvent)
		return c.store.InsertGraphSnapshotTx(gs)
	case "embedding_event":
		ee, _ := ev.data.(pulsetypes.EmbeddingEvent)
		return c.store.InsertEmbeddingEventTx(ee)
	case "index_event":
		ie, _ := ev.data.(pulsetypes.IndexEvent)
		return c.store.InsertIndexEventTx(ie)
	case "guard_event":
		ge, _ := ev.data.(pulsetypes.GuardEvent)
		return c.store.InsertGuardEventTx(ge)
	case "memory_op":
		mo, _ := ev.data.(pulsetypes.MemoryOperationEvent)
		return c.store.InsertMemoryOpTx(mo)
	case "validation_event":
		ve, _ := ev.data.(pulsetypes.ValidationEvent)
		return c.store.InsertValidationEventTx(ve)
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
			log.Printf("pulse collector: write error (%s): %v", ev.kind, err)
		}
	}
}

// dispatchNoTx writes a single event using the non-transactional store methods.
// Used as the BeginBatch fallback path; each call acquires its own store mutex.
func (c *Collector) dispatchNoTx(ev event) error {
	switch ev.kind {
	case "tool_call":
		tc, _ := ev.data.(pulsetypes.ToolCallEvent)
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
			log.Printf("pulse collector: update session stats: %v", serr)
		}
	case "context_delivery":
		cd, _ := ev.data.(pulsetypes.ContextDeliveryEvent)
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
		costSaved := c.computeCostSaved(tokensSaved)
		sessionID := cd.SessionID
		if sessionID == "" {
			sessionID = agentID + ":" + cd.ProjectID + ":" + time.Now().UTC().Format("2006-01-02")
		}
		if serr := c.store.AddSessionTokensSaved(sessionID, agentID, cd.ProjectID, tokensSaved, costSaved); serr != nil {
			log.Printf("pulse collector: add session tokens saved: %v", serr)
		}
	case "brain_usage":
		bu, _ := ev.data.(pulsetypes.BrainUsageEvent)
		return c.store.InsertBrainUsage(bu)
	case "session":
		sp, _ := ev.data.(sessionPayload)
		return c.store.UpsertSession(sp.ID, sp.AgentID, sp.ProjectID, sp.Event)
	case "outcome_signal":
		os, _ := ev.data.(pulsetypes.OutcomeSignalEvent)
		return c.store.InsertOutcomeSignal(os)
	case "session_model":
		sp, _ := ev.data.(sessionModelPayload)
		return c.store.UpdateSessionModel(sp.SessionID, sp.AgentID, sp.ProjectID, sp.Model, sp.Provider)
	case "agent_llm_usage":
		au, _ := ev.data.(pulsetypes.AgentLLMUsageEvent)
		return c.store.InsertAgentLLMUsage(au)
	case "parse_event":
		pe, _ := ev.data.(pulsetypes.ParseEvent)
		return c.store.InsertParseEvent(pe)
	case "reparse_event":
		re, _ := ev.data.(pulsetypes.ReparseEvent)
		return c.store.InsertReparseEvent(re)
	case "graph_snapshot":
		gs, _ := ev.data.(pulsetypes.GraphSnapshotEvent)
		return c.store.InsertGraphSnapshot(gs)
	case "embedding_event":
		ee, _ := ev.data.(pulsetypes.EmbeddingEvent)
		return c.store.InsertEmbeddingEvent(ee)
	case "index_event":
		ie, _ := ev.data.(pulsetypes.IndexEvent)
		return c.store.InsertIndexEvent(ie)
	case "guard_event":
		ge, _ := ev.data.(pulsetypes.GuardEvent)
		return c.store.InsertGuardEvent(ge)
	case "memory_op":
		mo, _ := ev.data.(pulsetypes.MemoryOperationEvent)
		return c.store.InsertMemoryOp(mo)
	case "validation_event":
		ve, _ := ev.data.(pulsetypes.ValidationEvent)
		return c.store.InsertValidationEvent(ve)
	}
	return nil
}

// computeCostSaved estimates the USD value of tokensSaved by pricing the saved
// tokens at gpt-4o input rates (the canonical high-value agent baseline).
// Falls back to 0 if the pricing table has no entry for the baseline model.
func (c *Collector) computeCostSaved(tokensSaved int) float64 {
	if tokensSaved <= 0 {
		return 0
	}
	const baselineModel = "gpt-4o"
	inputPer1M, _, found := c.store.GetPricing(baselineModel)
	if !found || inputPer1M <= 0 {
		return 0
	}
	return float64(tokensSaved) / 1_000_000.0 * inputPer1M
}

// Len returns the current buffer length (for diagnostics).
func (c *Collector) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buf)
}
