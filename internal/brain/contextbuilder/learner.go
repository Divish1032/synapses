// Package contextbuilder — this file implements the Learner, which records
// co-occurrence patterns from agent decisions into brain.sqlite.
//
// The Learner is pure Go (no LLM calls). It tracks which entities agents edit
// together, incrementing co-occurrence counters in context_patterns. These
// counters are used by the Builder to populate PatternHints.
package contextbuilder

import (
	"fmt"

	"github.com/SynapsesOS/synapses/internal/brain/store"
)

// DecisionInput carries the data needed to update co-occurrence patterns.
// It mirrors brain.DecisionRequest but uses no pkg/brain import.
type DecisionInput struct {
	AgentID         string
	Phase           string
	EntityName      string
	Action          string   // "edit"|"test"|"review"|"fix_violation"
	RelatedEntities []string // entities touched in the same work session
	Outcome         string   // "success"|"violation"|"reverted"|""
	Notes           string
}

// Learner records agent decisions and updates co-occurrence patterns.
type Learner struct {
	store *store.Store
}

// NewLearner creates a Learner backed by the given store.
func NewLearner(st *store.Store) *Learner {
	return &Learner{store: st}
}

// RecordDecision persists the decision in the audit log and updates co-occurrence
// patterns for all (entityName, relatedEntity) pairs, both directions.
//
// Example: editing "AuthService" alongside "TokenCache" and "UserRepo" records:
//   - AuthService → TokenCache (and TokenCache → AuthService)
//   - AuthService → UserRepo   (and UserRepo → AuthService)
//
// This is a best-effort operation — individual pattern errors are aggregated
// and returned, but the log write always proceeds independently.
func (l *Learner) RecordDecision(req DecisionInput) error {
	// 1. Persist the audit log entry.
	l.store.LogDecision(
		req.AgentID, req.Phase, req.EntityName, req.Action,
		req.RelatedEntities, req.Outcome, req.Notes,
	)

	// 2. Update co-occurrence patterns (only when there are related entities).
	if req.EntityName == "" || len(req.RelatedEntities) == 0 {
		return nil
	}

	reason := buildReason(req)
	var errs []string

	for _, rel := range req.RelatedEntities {
		if rel == "" || rel == req.EntityName {
			continue
		}
		if err := l.store.UpsertPattern(req.EntityName, rel, reason); err != nil {
			errs = append(errs, err.Error())
		}
		if err := l.store.UpsertPattern(rel, req.EntityName, reason); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("pattern upsert errors: %v", errs)
	}
	return nil
}

// buildReason constructs a short reason label from the decision context.
func buildReason(req DecisionInput) string {
	if req.Action != "" && req.Phase != "" {
		return fmt.Sprintf("%s during %s", req.Action, req.Phase)
	}
	if req.Action != "" {
		return req.Action
	}
	if req.Phase != "" {
		return req.Phase
	}
	return ""
}
