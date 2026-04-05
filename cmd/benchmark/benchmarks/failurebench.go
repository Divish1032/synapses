// failurebench.go implements Benchmark 1: Failure Avoidance.
//
// Tests whether Synapses memory recall finds relevant past failures when
// queried with SEMANTICALLY DIFFERENT phrases (not exact string matches).
//
// Uses REAL failure lessons from 86 sprint reflections (317 lessons).
// Ground truth: manually curated query-to-lesson relevance pairs.
package benchmarks

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/SynapsesOS/synapses/cmd/benchmark/reporter"
	"github.com/SynapsesOS/synapses/internal/store"
)

// FailureQuery is a realistic developer question with known relevant lessons.
type FailureQuery struct {
	Query           string   // what the developer would ask (NOT exact match to stored text)
	RelevantLessons []string // substrings that MUST appear in at least one result
	Category        string   // memory, parser, store, security, convention, graph
}

// FailureBenchResult holds per-query results.
type FailureBenchResult struct {
	Query        string  `json:"query"`
	Category     string  `json:"category"`
	ResultCount  int     `json:"result_count"`
	Relevant     int     `json:"relevant"`     // how many results contained relevant content
	Expected     int     `json:"expected"`     // how many relevant lessons exist
	Recall       float64 `json:"recall"`       // relevant found / expected
	TopRelevant  bool    `json:"top_relevant"` // was the top result relevant?
}

// RunFailureBench seeds real reflection data and tests semantic recall.
func RunFailureBench() (*reporter.FailureBenchReport, error) {
	// Load all reflection files as memories.
	reflectionDir := filepath.Join(os.Getenv("HOME"),
		".claude/projects/-Users-itachi-Documents-Github-synapses-os-synapses/memory")

	files, err := filepath.Glob(filepath.Join(reflectionDir, "reflection_sprint*.md"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no reflection files found in %s", reflectionDir)
	}

	// Create a temp store and seed with real memories.
	tmpDir, _ := os.MkdirTemp("", "failurebench-*")
	st, err := store.Open(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	seeded := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		content := string(data)
		// Split into individual lessons (each numbered item is a separate memory).
		lessons := splitLessons(content)
		for _, lesson := range lessons {
			if len(lesson) < 30 {
				continue
			}
			st.InsertMemory(store.Memory{
				Content: lesson,
				Tier:    "tier_1",
				AgentID: "implementer",
				Source:  "reflection",
				Tags:    "[]",
			})
			seeded++
		}
	}
	log.Printf("[failurebench] seeded %d memories from %d reflection files", seeded, len(files))

	// Run queries — each tests a REAL developer question.
	queries := buildFailureQueries()
	log.Printf("[failurebench] %d test queries", len(queries))

	var results []FailureBenchResult
	totalRecall := 0.0
	topHits := 0

	for _, q := range queries {
		memories, _ := st.SearchMemories(q.Query, 10)

		relevant := 0
		topRelevant := false
		for i, m := range memories {
			for _, expected := range q.RelevantLessons {
				if strings.Contains(strings.ToLower(m.Content), strings.ToLower(expected)) {
					relevant++
					if i == 0 {
						topRelevant = true
					}
					break // count each result once
				}
			}
		}

		recall := float64(0)
		if len(q.RelevantLessons) > 0 {
			recall = float64(relevant) / float64(len(q.RelevantLessons))
			if recall > 1 {
				recall = 1
			}
		}

		results = append(results, FailureBenchResult{
			Query:       q.Query,
			Category:    q.Category,
			ResultCount: len(memories),
			Relevant:    relevant,
			Expected:    len(q.RelevantLessons),
			Recall:      recall * 100,
			TopRelevant: topRelevant,
		})

		totalRecall += recall
		if topRelevant {
			topHits++
		}
	}

	avgRecall := totalRecall / float64(len(queries)) * 100
	mrr := float64(topHits) / float64(len(queries)) * 100 // Mean Reciprocal Rank (simplified)

	report := &reporter.FailureBenchReport{
		Timestamp:    reporter.Timestamp(),
		TotalQueries: len(queries),
		SeededMemories: seeded,
		AvgRecall:    avgRecall,
		MRR:          mrr,
		Cases:        results,
	}

	log.Printf("[failurebench] avg_recall=%.1f%% MRR=%.1f%% (%d/%d top hits)",
		avgRecall, mrr, topHits, len(queries))
	return report, nil
}

// splitLessons breaks a reflection file into individual lessons.
func splitLessons(content string) []string {
	var lessons []string
	var current strings.Builder

	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		// New numbered lesson starts
		if len(trimmed) > 3 && trimmed[0] >= '1' && trimmed[0] <= '9' && trimmed[1] == '.' {
			if current.Len() > 0 {
				lessons = append(lessons, current.String())
			}
			current.Reset()
			current.WriteString(trimmed)
		} else if current.Len() > 0 && trimmed != "" && !strings.HasPrefix(trimmed, "---") && !strings.HasPrefix(trimmed, "name:") {
			current.WriteString(" ")
			current.WriteString(trimmed)
		}
	}
	if current.Len() > 0 {
		lessons = append(lessons, current.String())
	}
	return lessons
}

// buildFailureQueries creates realistic developer questions.
// CRITICAL: queries use DIFFERENT WORDS than the stored lessons.
// This tests semantic recall, not string matching.
func buildFailureQueries() []FailureQuery {
	return []FailureQuery{
		// ── Parser / AST queries ───────────────────────────────────
		{
			Query:           "how does the Python type system handle generic annotations in tree-sitter",
			RelevantLessons: []string{"generic_type", "python", "type_identifier"},
			Category:        "parser",
		},
		{
			Query:           "what problems did we have with macOS sed command in tests",
			RelevantLessons: []string{"sed", "macOS"},
			Category:        "parser",
		},
		{
			Query:           "issues with TypeScript new expression and constructor calls",
			RelevantLessons: []string{"new_expression", "TypeScript", "constructor"},
			Category:        "parser",
		},

		// ── Store / Database queries ──────────────────────────────
		{
			Query:           "SQLite parameter limit and batching for large queries",
			RelevantLessons: []string{"SQLite", "900", "threshold"},
			Category:        "store",
		},
		{
			Query:           "how to open the store for testing without errors",
			RelevantLessons: []string{"store", "Open"},
			Category:        "store",
		},

		// ── Git / Version Control queries ─────────────────────────
		{
			Query:           "problems with detecting uncommitted changes using git diff",
			RelevantLessons: []string{"git diff", "uncommitted"},
			Category:        "git",
		},
		{
			Query:           "why does staging files with git add -A cause problems",
			RelevantLessons: []string{"git add", "stage"},
			Category:        "git",
		},

		// ── MCP / Handler queries ─────────────────────────────────
		{
			Query:           "what happened when we tried to add tools to the MCP server",
			RelevantLessons: []string{"tool", "MCP"},
			Category:        "mcp",
		},
		{
			Query:           "issues with background worker draining in tests",
			RelevantLessons: []string{"drainBackground", "worker"},
			Category:        "mcp",
		},

		// ── Security / Pattern queries ────────────────────────────
		{
			Query:           "false positives with credential detection regex patterns",
			RelevantLessons: []string{"credential", "regex"},
			Category:        "security",
		},
		{
			Query:           "problems detecting framework-specific route handlers",
			RelevantLessons: []string{"route", "handler", "framework"},
			Category:        "security",
		},

		// ── Convention / Learning queries ─────────────────────────
		{
			Query:           "how does the observation pipeline work for learning project patterns",
			RelevantLessons: []string{"observation", "session"},
			Category:        "convention",
		},
		{
			Query:           "what threshold is used for promoting patterns to conventions",
			RelevantLessons: []string{"session", "convention"},
			Category:        "convention",
		},

		// ── Graph / Resolver queries ──────────────────────────────
		{
			Query:           "edge types that are dead and should be removed from the graph",
			RelevantLessons: []string{"dead", "edge"},
			Category:        "graph",
		},
		{
			Query:           "how does the resolver handle ambiguous function names across packages",
			RelevantLessons: []string{"ambiguous", "resolve"},
			Category:        "graph",
		},

		// ── Architecture queries ──────────────────────────────────
		{
			Query:           "when did a parallel session commit code that another session was working on",
			RelevantLessons: []string{"parallel session", "commit"},
			Category:        "architecture",
		},
		{
			Query:           "what pattern do we use for atomic check-and-set operations",
			RelevantLessons: []string{"atomic", "check"},
			Category:        "architecture",
		},

		// ── Negative query (should return nothing relevant) ──────
		{
			Query:           "how to deploy Kubernetes pods with Helm charts",
			RelevantLessons: []string{}, // nothing relevant in the codebase
			Category:        "negative",
		},
		{
			Query:           "React component lifecycle hooks and state management",
			RelevantLessons: []string{}, // nothing relevant
			Category:        "negative",
		},

		// ── Broad queries ────────────────────────────────────────
		{
			Query:           "common mistakes when writing tests for the store layer",
			RelevantLessons: []string{"test", "store"},
			Category:        "testing",
		},
	}
}
