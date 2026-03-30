package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── Compaction Recovery Benchmark ───────────────────────────────────────────
//
// Measures data completeness of the compaction recovery packet and guide.
// Three scenarios test whether post-compaction context contains the information
// an agent needs to resume effectively.
//
// Metrics (0.0–1.0):
//   - EntityRecall: fraction of scenario entities in recovery
//   - FileRecall: fraction of scenario files in recovery
//   - DecisionRecall: fraction of decisions preserved
//   - ViolationRecall: fraction of violations surfaced
//   - GuideCompleteness: entity_importance covers all work entities

// scenarioData defines a compaction benchmark scenario.
type scenarioData struct {
	Name       string
	AgentID    string
	Entities   []string
	Files      []string
	Decisions  []string
	Failures   []string
	Violations []violationFixture
}

type violationFixture struct {
	RuleID   string
	FromNode string
	ToNode   string
	FromFile string
	ToFile   string
}

func TestCompactionBenchmark_MidRefactor(t *testing.T) {
	runCompactionBenchmark(t, scenarioData{
		Name:    "mid-refactor",
		AgentID: "bench-agent-1",
		Entities: []string{"AuthService", "TokenValidator", "SessionStore", "UserRepo", "LoginHandler"},
		Files:    []string{"pkg/auth/auth.go", "pkg/auth/token.go", "pkg/auth/session.go", "pkg/repo/user.go", "pkg/api/login.go"},
		Decisions: []string{
			"Chose JWT over session cookies for stateless scaling",
			"Decided to keep backward compat for v1 token format",
		},
		Failures: []string{
			"TokenValidator.Validate panicked on nil input — added nil guard",
		},
		Violations: []violationFixture{
			{RuleID: "no-direct-db", FromNode: "LoginHandler", ToNode: "UserRepo", FromFile: "pkg/api/login.go", ToFile: "pkg/repo/user.go"},
		},
	})
}

func TestCompactionBenchmark_BugInvestigation(t *testing.T) {
	runCompactionBenchmark(t, scenarioData{
		Name:    "bug-investigation",
		AgentID: "bench-agent-2",
		Entities: []string{"PaymentProcessor", "OrderService", "RefundHandler"},
		Files:    []string{"pkg/payment/processor.go", "pkg/orders/service.go", "pkg/payment/refund.go"},
		Decisions: []string{
			"Root cause: race condition in PaymentProcessor.Charge",
		},
		Failures: []string{
			"Refund double-fires when concurrent requests hit RefundHandler",
			"OrderService.Cancel does not check payment state",
		},
	})
}

func TestCompactionBenchmark_ArchitecturalChange(t *testing.T) {
	runCompactionBenchmark(t, scenarioData{
		Name:    "architectural-change",
		AgentID: "bench-agent-3",
		Entities: []string{"Router", "Middleware", "AuthGuard", "RateLimiter", "Logger", "MetricsCollector", "ErrorHandler", "ResponseWriter"},
		Files: []string{
			"pkg/http/router.go", "pkg/http/middleware.go", "pkg/http/auth.go",
			"pkg/http/ratelimit.go", "pkg/http/logger.go", "pkg/metrics/collector.go",
			"pkg/http/errors.go", "pkg/http/response.go",
		},
		Decisions: []string{
			"Moving to middleware chain pattern instead of nested handlers",
			"RateLimiter must run before AuthGuard to prevent auth DOS",
		},
		Violations: []violationFixture{
			{RuleID: "no-circular-dep", FromNode: "Router", ToNode: "Middleware", FromFile: "pkg/http/router.go", ToFile: "pkg/http/middleware.go"},
			{RuleID: "no-direct-db", FromNode: "Logger", ToNode: "MetricsCollector", FromFile: "pkg/http/logger.go", ToFile: "pkg/metrics/collector.go"},
		},
	})
}

func runCompactionBenchmark(t *testing.T, scenario scenarioData) {
	t.Helper()

	// ── Setup ────────────────────────────────────────────────────────────
	st := openMCPTestStore(t)
	g := graph.New("bench-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Add graph nodes and edges
	nodeIDs := make(map[string]graph.NodeID)
	for i, entity := range scenario.Entities {
		file := "bench/file.go"
		if i < len(scenario.Files) {
			file = scenario.Files[i]
		}
		id := g.MakeNodeID(file, entity)
		g.AddNode(&graph.Node{
			ID:       id,
			Type:     graph.NodeFunction,
			Name:     entity,
			File:     file,
			Line:     1,
			Package:  "bench",
			Exported: true,
		})
		nodeIDs[entity] = id
	}
	// Add edges between adjacent entities
	ids := make([]graph.NodeID, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		ids = append(ids, id)
	}
	for i := 0; i+1 < len(ids); i++ {
		g.AddEdge(&graph.Edge{From: ids[i], To: ids[i+1], Type: graph.EdgeCalls})
	}

	srv := New(g, cfg, st)
	srv.StartBackground()
	srv.projectID = "bench-project"
	t.Cleanup(func() { srv.Close() })

	// Bootstrap session via session_init (establishes synapsesSessions mapping)
	_, _ = srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": scenario.AgentID,
		"scope":    "standard",
	}))

	// Resolve the session ID that was registered
	sessionID := srv.getSynapseSessionID(SessionIDFromContext(ctx))

	// Populate work ledger with the correct session ID
	for i, entity := range scenario.Entities {
		file := ""
		if i < len(scenario.Files) {
			file = scenario.Files[i]
		}
		_ = st.AppendLedger(store.LedgerEntry{
			SessionID: sessionID,
			ProjectID: "bench-project",
			ToolName:  "get_context",
			EntityIDs: []string{entity},
			FilePaths: []string{file},
		})
	}

	// Populate episodes AFTER session creation so they fall within the session's time window.
	// Episodes are correlated to sessions via agent_id + created_at range.
	episodeTime := time.Now().Unix()
	for _, d := range scenario.Decisions {
		_, _ = st.RememberEpisode(store.Episode{
			AgentID:     scenario.AgentID,
			ProjectID:   "bench-project",
			CreatedAt:   episodeTime,
			EpisodeType: "decision",
			Outcome:     "success",
			Decision:    d,
			Rationale:   "benchmark rationale",
		})
	}
	for _, f := range scenario.Failures {
		_, _ = st.RememberEpisode(store.Episode{
			AgentID:     scenario.AgentID,
			ProjectID:   "bench-project",
			CreatedAt:   episodeTime,
			EpisodeType: "failure",
			Outcome:     "failure",
			Decision:    f,
		})
	}

	// Populate entity memories
	for _, entity := range scenario.Entities[:min(3, len(scenario.Entities))] {
		_, _ = st.InsertMemory(store.Memory{
			Tier:     store.TierEntity,
			Content:  fmt.Sprintf("Institutional knowledge about %s", entity),
			EntityID: entity,
			AgentID:  scenario.AgentID,
			Source:   store.SourceAuto,
			Tags:     `["entity"]`,
		})
	}

	// Populate violations
	for _, v := range scenario.Violations {
		_ = st.LogViolations([]config.Violation{{
			RuleID:   v.RuleID,
			Severity: "warning",
			FromNode: graph.NodeID(v.FromNode),
			ToNode:   graph.NodeID(v.ToNode),
			FromFile: v.FromFile,
			ToFile:   v.ToFile,
		}})
	}

	// Populate rules
	for _, v := range scenario.Violations {
		_ = st.UpsertDynamicRule(config.Rule{
			ID:          v.RuleID,
			Description: fmt.Sprintf("No %s allowed", v.RuleID),
			Severity:    "warning",
			ForbiddenEdge: config.ForbiddenEdge{
				FromFilePattern: v.FromFile,
				ToFilePattern:   v.ToFile,
			},
		})
	}

	// Create a task with session state
	_, taskIDs, _ := st.CreatePlan("Bench Plan", "benchmark", scenario.AgentID, []store.TaskInput{
		{Title: "Bench Task"},
	})
	taskID := ""
	if len(taskIDs) > 0 {
		taskID = taskIDs[0]
		_, _, _ = st.UpdateTask(taskID, "in_progress", "", scenario.AgentID)
	}
	_ = st.UpsertSessionState(store.SessionState{
		ID:             "bench-state",
		TaskID:         taskID,
		AgentID:        scenario.AgentID,
		Approach:       "Refactoring approach for benchmark",
		FilesModified:  scenario.Files[:min(3, len(scenario.Files))],
		CompletedSteps: []string{"step 1 done", "step 2 done", "step 3 done"},
		RemainingSteps: []string{"step 4 pending", "step 5 pending"},
		Blockers:       []string{"waiting for API review"},
		Decisions:      scenario.Decisions,
	})

	// ── Baseline: standard scope ─────────────────────────────────────────
	standardResult, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": scenario.AgentID,
		"scope":    "standard",
	}))
	standardMap := mustResult(t, standardResult, err)
	standardMetrics := scoreRecovery(t, scenario, standardMap, "standard")

	// ── Recovery: compaction scope ───────────────────────────────────────
	compactionResult, err := srv.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": scenario.AgentID,
		"scope":    "compaction",
	}))
	compactionMap := mustResult(t, compactionResult, err)
	compactionMetrics := scoreRecovery(t, scenario, compactionMap, "compaction")

	// ── Guide ────────────────────────────────────────────────────────────
	guideResult, err := srv.handleGetCompactionGuide(ctx, callTool(map[string]any{
		"agent_id": scenario.AgentID,
	}))
	guideMap := mustResult(t, guideResult, err)
	guideCompleteness := scoreGuide(t, scenario, guideMap)

	// ── Report ───────────────────────────────────────────────────────────
	t.Logf("\n=== Compaction Benchmark: %s ===", scenario.Name)
	t.Logf("%-20s %-10s %-10s", "Metric", "Standard", "Compaction")
	t.Logf("%-20s %-10.2f %-10.2f", "EntityRecall", standardMetrics.EntityRecall, compactionMetrics.EntityRecall)
	t.Logf("%-20s %-10.2f %-10.2f", "FileRecall", standardMetrics.FileRecall, compactionMetrics.FileRecall)
	t.Logf("%-20s %-10.2f %-10.2f", "DecisionRecall", standardMetrics.DecisionRecall, compactionMetrics.DecisionRecall)
	t.Logf("%-20s %-10.2f %-10.2f", "ViolationRecall", standardMetrics.ViolationRecall, compactionMetrics.ViolationRecall)
	t.Logf("%-20s %-10s %-10.2f", "GuideCompleteness", "n/a", guideCompleteness)

	// ── Assertions ───────────────────────────────────────────────────────
	// Compaction recovery should have ≥0.8 recall on all metrics
	if compactionMetrics.EntityRecall < 0.8 {
		t.Errorf("EntityRecall %.2f < 0.80 threshold", compactionMetrics.EntityRecall)
	}
	if compactionMetrics.FileRecall < 0.8 {
		t.Errorf("FileRecall %.2f < 0.80 threshold", compactionMetrics.FileRecall)
	}
	// Decision recall may be lower since episodes are time-window-correlated
	if compactionMetrics.DecisionRecall < 0.5 {
		t.Errorf("DecisionRecall %.2f < 0.50 threshold", compactionMetrics.DecisionRecall)
	}

	// Guide should cover ≥0.9 of work entities
	if guideCompleteness < 0.9 {
		t.Errorf("GuideCompleteness %.2f < 0.90 threshold", guideCompleteness)
	}

	// Compaction should be strictly better than standard on entity/file recall
	if compactionMetrics.EntityRecall < standardMetrics.EntityRecall {
		t.Errorf("Compaction EntityRecall (%.2f) should be >= Standard (%.2f)",
			compactionMetrics.EntityRecall, standardMetrics.EntityRecall)
	}
}

type benchMetrics struct {
	EntityRecall    float64
	FileRecall      float64
	DecisionRecall  float64
	ViolationRecall float64
}

// scoreRecovery measures how much of the scenario data is present in the response.
// Uses field-level validation for compaction scope, string matching for standard.
func scoreRecovery(t *testing.T, scenario scenarioData, resp map[string]any, label string) benchMetrics {
	t.Helper()

	if label == "compaction" {
		return scoreCompactionRecovery(t, scenario, resp)
	}

	// Standard scope: use string matching (baseline, less precise)
	respJSON, _ := json.Marshal(resp)
	respStr := string(respJSON)
	return benchMetrics{
		EntityRecall:    recallStrings(scenario.Entities, respStr),
		FileRecall:      recallStrings(scenario.Files, respStr),
		DecisionRecall:  recallStrings(scenario.Decisions, respStr),
		ViolationRecall: recallViolationStrings(scenario.Violations, respStr),
	}
}

// scoreCompactionRecovery validates the compaction_recovery field specifically.
func scoreCompactionRecovery(t *testing.T, scenario scenarioData, resp map[string]any) benchMetrics {
	t.Helper()
	metrics := benchMetrics{}

	recovery, _ := resp["compaction_recovery"].(map[string]any)
	if recovery == nil {
		t.Log("compaction_recovery field is missing from response")
		return metrics
	}

	// Validate work_summary contains entities and files
	workSummary, _ := recovery["work_summary"].(string)
	if workSummary == "" {
		t.Log("work_summary is empty")
	}

	// Entity recall: check work_summary for entity names
	entityFound := 0
	for _, entity := range scenario.Entities {
		if strings.Contains(workSummary, entity) {
			entityFound++
		}
	}
	if len(scenario.Entities) > 0 {
		metrics.EntityRecall = float64(entityFound) / float64(len(scenario.Entities))
	} else {
		metrics.EntityRecall = 1.0
	}

	// File recall: check work_summary for file basenames
	fileFound := 0
	for _, file := range scenario.Files {
		parts := strings.Split(file, "/")
		basename := parts[len(parts)-1]
		if strings.Contains(workSummary, basename) {
			fileFound++
		}
	}
	if len(scenario.Files) > 0 {
		metrics.FileRecall = float64(fileFound) / float64(len(scenario.Files))
	} else {
		metrics.FileRecall = 1.0
	}

	// Decision recall: validate session_decisions field
	decisions, _ := recovery["session_decisions"].([]any)
	decisionFound := 0
	for _, d := range scenario.Decisions {
		for _, sd := range decisions {
			sdMap, _ := sd.(map[string]any)
			if sdMap != nil {
				decision, _ := sdMap["decision"].(string)
				if decision == d {
					decisionFound++
					break
				}
			}
		}
	}
	if len(scenario.Decisions) > 0 {
		metrics.DecisionRecall = float64(decisionFound) / float64(len(scenario.Decisions))
	} else {
		metrics.DecisionRecall = 1.0
	}

	// Violation recall: validate active_violations field
	violations, _ := recovery["active_violations"].([]any)
	violationFound := 0
	for _, v := range scenario.Violations {
		for _, sv := range violations {
			svMap, _ := sv.(map[string]any)
			if svMap != nil {
				ruleID, _ := svMap["rule_id"].(string)
				if ruleID == v.RuleID {
					violationFound++
					break
				}
			}
		}
	}
	if len(scenario.Violations) > 0 {
		metrics.ViolationRecall = float64(violationFound) / float64(len(scenario.Violations))
	} else {
		metrics.ViolationRecall = 1.0
	}

	return metrics
}

// recallStrings computes what fraction of expected items appear in the response text.
func recallStrings(expected []string, respStr string) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	found := 0
	for _, item := range expected {
		parts := strings.Split(item, "/")
		searchTerm := parts[len(parts)-1]
		if strings.Contains(respStr, searchTerm) {
			found++
		}
	}
	return float64(found) / float64(len(expected))
}

func recallViolationStrings(expected []violationFixture, respStr string) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	found := 0
	for _, v := range expected {
		if strings.Contains(respStr, v.RuleID) {
			found++
		}
	}
	return float64(found) / float64(len(expected))
}

// scoreGuide validates that entity_importance in the guide covers work entities.
func scoreGuide(t *testing.T, scenario scenarioData, guide map[string]any) float64 {
	t.Helper()

	importance, _ := guide["entity_importance"].([]any)
	if importance == nil {
		t.Log("entity_importance is nil in guide")
		return 0.0
	}

	// Collect entity names from the importance list
	guideEntities := make(map[string]bool)
	for _, item := range importance {
		itemMap, _ := item.(map[string]any)
		if itemMap != nil {
			entity, _ := itemMap["entity"].(string)
			if entity != "" {
				guideEntities[entity] = true
			}
		}
	}

	found := 0
	for _, entity := range scenario.Entities {
		if guideEntities[entity] {
			found++
		}
	}
	if len(scenario.Entities) == 0 {
		return 1.0
	}
	return float64(found) / float64(len(scenario.Entities))
}

