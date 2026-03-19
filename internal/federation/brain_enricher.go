package federation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// BrainEnricher provides brain-enhanced cross-project entity summaries
// and drift explanations. Optional — when brain is unavailable, falls
// back to raw entity signatures and structural diffs.
type BrainEnricher struct {
	resolver *Resolver

	// mu guards brain and generate. A dedicated mutex is used rather
	// than the resolver's mu to keep lock scope narrow: SetBrain/SetGenerate
	// are init-time setters that don't need the full store lock, and readers
	// snapshot these fields quickly before releasing mu — no overlap with
	// store operations.
	mu       sync.RWMutex
	brain    BrainSummaryProvider
	generate func(ctx context.Context, prompt string) (string, error)
}

func newBrainEnricher(r *Resolver) *BrainEnricher {
	return &BrainEnricher{
		resolver: r,
	}
}

// SetBrain attaches a brain summary provider for cross-project summarization.
// Optional — when nil, cross-project summaries use raw entity signatures.
// Thread-safe: may be called concurrently with readers.
func (b *BrainEnricher) SetBrain(brain BrainSummaryProvider) {
	b.mu.Lock()
	b.brain = brain
	b.mu.Unlock()
}

// SetBrainGenerate attaches an LLM generate function for brain-enhanced
// drift summaries. When set and brain is available, BrainDriftSummary
// feeds the structural diff to the LLM for a natural-language explanation.
// Typically wired to the brain's ingestor LLM client.
// Thread-safe: may be called concurrently with readers.
func (b *BrainEnricher) SetBrainGenerate(fn func(ctx context.Context, prompt string) (string, error)) {
	b.mu.Lock()
	b.generate = fn
	b.mu.Unlock()
}

// GetEntitySummary returns a brain-generated summary for a sibling entity.
// Falls back to the entity's raw signature if brain is unavailable or has
// no summary. This is the cross-project context summarization path.
func (b *BrainEnricher) GetEntitySummary(ctx context.Context, alias, entityName string) string {
	if ctx.Err() != nil {
		return ""
	}

	// Try brain summary first (zero LLM calls — reads from brain.sqlite).
	b.mu.RLock()
	brain := b.brain
	b.mu.RUnlock()
	if brain != nil {
		projectID := b.resolver.SiblingProjectID(alias)
		if projectID != "" {
			// Brain summaries are indexed by nodeID. We need to find the
			// entity's nodeID in the sibling store first.
			st := b.resolver.getStore(alias)
			if st != nil {
				results, err := st.FindNodesByNameCtx(ctx, entityName, 1)
				if err == nil && len(results) > 0 {
					summary := brain.Summary(projectID, results[0].ID)
					if summary != "" {
						return summary
					}
				}
			}
		}
	}

	// Fallback: raw entity signature from sibling store.
	st := b.resolver.getStore(alias)
	if st == nil {
		return ""
	}
	results, err := st.FindNodesByNameCtx(ctx, entityName, 1)
	if err != nil || len(results) == 0 {
		return ""
	}
	if results[0].Signature != "" {
		return results[0].Signature
	}
	return results[0].Name
}

// driftSummaryPrompt is the prompt template for brain-enhanced drift summaries.
const driftSummaryPrompt = `Given this function signature change:
Old: %s
New: %s
Structural diff: %s

Summarize in ONE sentence what changed and how it affects callers. Focus on whether existing callers need updating. Output only the summary sentence, no other text.`

// BrainDriftSummary generates a brain-enhanced drift summary for a changed
// entity. When brain is available and generate is set, produces a
// human-readable natural-language explanation. When unavailable, uses
// structuralSignatureDiff (the existing graph-based heuristic).
func (b *BrainEnricher) BrainDriftSummary(ctx context.Context, oldSig, newSig, entityName string) string {
	structural := structuralSignatureDiff(oldSig, newSig)

	// If brain generate is unavailable, return structural diff.
	b.mu.RLock()
	bg := b.generate
	brain := b.brain
	b.mu.RUnlock()
	if bg == nil || brain == nil || !brain.Available() {
		return structural
	}

	// Feed the structural diff to the brain for natural-language explanation.
	// Use a short timeout — brain enhancement is best-effort.
	brainCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	prompt := fmt.Sprintf(driftSummaryPrompt, oldSig, newSig, structural)
	response, err := bg(brainCtx, prompt)
	if err != nil || response == "" {
		return structural // fail-open: brain error → structural diff
	}

	// Clean up the response — remove quotes, trim whitespace.
	response = strings.TrimSpace(response)
	response = strings.Trim(response, `"'`)
	response = strings.TrimSpace(response)

	// Validate: response should be a reasonable prose sentence, not code or garbage.
	if !isValidDriftSummary(response) {
		return structural
	}

	return response
}

// isValidDriftSummary checks that a brain-generated drift summary looks like
// a natural-language sentence, not code, JSON, or garbage. Returns false
// if the response should be discarded in favor of the structural diff.
func isValidDriftSummary(s string) bool {
	if len(s) < 10 || len(s) > 500 {
		return false
	}
	// Must contain at least one space (sentences have spaces).
	if !strings.Contains(s, " ") {
		return false
	}
	// Reject responses that look like code.
	codePrefixes := []string{"{", "func ", "def ", "fn ", "class ", "import ", "package ", "```"}
	for _, prefix := range codePrefixes {
		if strings.HasPrefix(s, prefix) {
			return false
		}
	}
	// Reject responses that are just the signatures echoed back.
	if strings.HasPrefix(s, "Old:") || strings.HasPrefix(s, "New:") {
		return false
	}
	return true
}
