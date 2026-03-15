// Package collector implements an async event buffer that decouples ingestion
// from SQLite writes. Events are accepted into a ring buffer and batch-flushed
// to the store on a configurable interval, ensuring <1ms overhead on the
// ingestion hot path.
package collector

import (
	"log"
	"sync"
	"time"

	pulsestore "github.com/SynapsesOS/synapses/internal/pulse/pstore"
	pulsetypes "github.com/SynapsesOS/synapses/internal/pulse/types"
)

// event wraps a typed event for the ring buffer.
type event struct {
	kind string // "tool_call", "context_delivery", "brain_usage", "session", "session_model", "agent_llm_usage"
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

func (c *Collector) enqueue(ev event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buf = append(c.buf, ev)

	// If buffer is at 80% capacity, trigger an early flush in the background.
	if len(c.buf) >= c.cap*80/100 {
		batch := c.drainLocked()
		go c.writeBatch(batch)
	}
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
	for _, ev := range batch {
		var err error
		switch ev.kind {
		case "tool_call":
			tc := ev.data.(pulsetypes.ToolCallEvent)
			err = c.store.InsertToolCall(tc)
			// Increment the session tool_call counter for every real tool invocation.
			if err == nil {
				agentID := tc.AgentID
				if agentID == "" {
					agentID = "default"
				}
				sessionID := agentID + ":" + tc.ProjectID + ":" + time.Now().UTC().Format("2006-01-02")
				if serr := c.store.UpdateSessionStats(sessionID, agentID, tc.ProjectID, 0, 0); serr != nil {
					log.Printf("pulse collector: update session stats: %v", serr)
				}
			}
		case "context_delivery":
			cd := ev.data.(pulsetypes.ContextDeliveryEvent)
			err = c.store.InsertContextDelivery(cd)
			// Accumulate token savings separately — does NOT increment tool_calls
			// (that is handled above in the tool_call branch so the counter
			// reflects actual tool invocations, not just context deliveries).
			if err == nil {
				agentID := cd.AgentID
				if agentID == "" {
					agentID = "default"
				}
				tokensSaved := cd.BaselineTokens - cd.ResponseTokens
				if tokensSaved < 0 {
					tokensSaved = 0
				}
				costSaved := c.computeCostSaved(tokensSaved)
				sessionID := agentID + ":" + cd.ProjectID + ":" + time.Now().UTC().Format("2006-01-02")
				if serr := c.store.AddSessionTokensSaved(sessionID, agentID, cd.ProjectID, tokensSaved, costSaved); serr != nil {
					log.Printf("pulse collector: add session tokens saved: %v", serr)
				}
			}
		case "brain_usage":
			err = c.store.InsertBrainUsage(ev.data.(pulsetypes.BrainUsageEvent))
		case "session":
			sp := ev.data.(sessionPayload)
			err = c.store.UpsertSession(sp.ID, sp.AgentID, sp.ProjectID, sp.Event)
		case "outcome_signal":
			err = c.store.InsertOutcomeSignal(ev.data.(pulsetypes.OutcomeSignalEvent))
		case "session_model":
			sp := ev.data.(sessionModelPayload)
			err = c.store.UpdateSessionModel(sp.SessionID, sp.AgentID, sp.ProjectID, sp.Model, sp.Provider)
		case "agent_llm_usage":
			err = c.store.InsertAgentLLMUsage(ev.data.(pulsetypes.AgentLLMUsageEvent))
		}
		if err != nil {
			log.Printf("pulse collector: write error (%s): %v", ev.kind, err)
		}
	}
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
