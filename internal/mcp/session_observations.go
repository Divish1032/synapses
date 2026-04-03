package mcp

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/store"
)

// extractSessionObservations derives structured signals from a completed session
// and returns them as SessionObservation records ready for store insertion.
//
// This is the Sprint 29.1 observation pipeline — a Tier 1 server-side capture
// that runs automatically at end_session without any agent initiative. The
// convention extraction engine (Sprint 29.2) aggregates these observations
// across sessions to identify project-wide patterns.
//
// Four categories are extracted:
//   - tool_usage:      which tools were used and how heavily
//   - testing_pattern: test files touched, inferred testing conventions
//   - library_usage:   framework/library imports detected in touched files
//   - approach_outcome: session success/failure signal
//   - file_pattern:    architectural layer usage (handler/service/repo)
func extractSessionObservations(
	agentID, sessionID, projectID string,
	sessSummary *sessionSummary,
	retro *store.ToolCallSummary,
	g *graph.Graph,
) []store.SessionObservation {
	if sessionID == "" || agentID == "" {
		return nil
	}

	now := time.Now().UTC().Unix()
	base := store.SessionObservation{
		SessionID: sessionID,
		ProjectID: projectID,
		AgentID:   agentID,
		CreatedAt: now,
	}

	var obs []store.SessionObservation

	// ── Category: tool_usage ─────────────────────────────────────────────────
	if retro != nil && retro.TotalCalls > 0 {
		obs = append(obs, toolUsageObservations(base, retro)...)
	}

	// ── Category: testing_pattern + file_pattern ────────────────────────────
	if sessSummary != nil && len(sessSummary.FilesTouched) > 0 {
		obs = append(obs, testingPatternObservations(base, sessSummary.FilesTouched)...)
		obs = append(obs, filePatternObservations(base, sessSummary.FilesTouched)...)
	}

	// ── Category: library_usage ──────────────────────────────────────────────
	if g != nil && sessSummary != nil && len(sessSummary.FilesTouched) > 0 {
		obs = append(obs, libraryUsageObservations(base, sessSummary.FilesTouched, g)...)
	}

	// ── Category: approach_outcome ───────────────────────────────────────────
	obs = append(obs, approachOutcomeObservation(base, retro, sessSummary))

	return obs
}

// ── Tool usage ────────────────────────────────────────────────────────────────

func toolUsageObservations(base store.SessionObservation, retro *store.ToolCallSummary) []store.SessionObservation {
	base.Category = store.ObsCategoryToolUsage

	// Build a quick lookup: tool name → count.
	toolCounts := make(map[string]int, len(retro.TopTools))
	for _, tc := range retro.TopTools {
		toolCounts[tc.ToolName] = tc.Count
	}

	var obs []store.SessionObservation

	// validate usage.
	validateCount := toolCounts["validate"]
	switch {
	case validateCount >= 5:
		obs = append(obs, ob(base, "heavy_validate_usage", strconv.Itoa(validateCount), 0.8))
	case validateCount >= 1:
		obs = append(obs, ob(base, "some_validate_usage", strconv.Itoa(validateCount), 0.6))
	case retro.TotalCalls >= 10:
		// Many tool calls but no validate → a meaningful absence signal.
		obs = append(obs, ob(base, "no_validate_usage", "", 0.6))
	}

	// memory tool usage (agent-initiated knowledge saves).
	memCount := toolCounts["memory"]
	if memCount >= 3 {
		obs = append(obs, ob(base, "frequent_memory_saves", strconv.Itoa(memCount), 0.7))
	} else if memCount >= 1 {
		obs = append(obs, ob(base, "some_memory_saves", strconv.Itoa(memCount), 0.5))
	}

	// get_impact usage (signals the agent reasons about blast radius).
	impactCount := toolCounts["get_impact"]
	if impactCount >= 3 {
		obs = append(obs, ob(base, "uses_impact_analysis", strconv.Itoa(impactCount), 0.7))
	}

	return obs
}

// ── Testing patterns ──────────────────────────────────────────────────────────

func testingPatternObservations(base store.SessionObservation, files []string) []store.SessionObservation {
	base.Category = store.ObsCategoryTestingPattern

	var goTests, tsTests, pyTests, javaTests, rustTests int
	for _, f := range files {
		name := filepath.Base(f)
		switch {
		case strings.HasSuffix(name, "_test.go"):
			goTests++
		case strings.HasSuffix(name, ".test.ts"),
			strings.HasSuffix(name, ".spec.ts"),
			strings.HasSuffix(name, ".test.js"),
			strings.HasSuffix(name, ".spec.js"):
			tsTests++
		case strings.HasPrefix(name, "test_") && strings.HasSuffix(name, ".py"),
			strings.HasSuffix(name, "_test.py"):
			pyTests++
		case strings.HasSuffix(name, "Test.java"),
			strings.HasSuffix(name, "Tests.java"),
			strings.HasSuffix(name, "Spec.java"):
			javaTests++
		case strings.HasSuffix(name, ".rs") && strings.Contains(f, "test"):
			rustTests++
		}
	}

	var obs []store.SessionObservation
	if goTests > 0 {
		conf := confidenceByCount(goTests)
		obs = append(obs, ob(base, "go_test_files_touched", strconv.Itoa(goTests), conf))
	}
	if tsTests > 0 {
		obs = append(obs, ob(base, "ts_test_files_touched", strconv.Itoa(tsTests), confidenceByCount(tsTests)))
	}
	if pyTests > 0 {
		obs = append(obs, ob(base, "py_test_files_touched", strconv.Itoa(pyTests), confidenceByCount(pyTests)))
	}
	if javaTests > 0 {
		obs = append(obs, ob(base, "java_test_files_touched", strconv.Itoa(javaTests), confidenceByCount(javaTests)))
	}
	if rustTests > 0 {
		obs = append(obs, ob(base, "rust_test_files_touched", strconv.Itoa(rustTests), confidenceByCount(rustTests)))
	}

	return obs
}

// ── File / architecture patterns ──────────────────────────────────────────────

// knownLayerDirs maps directory name fragments to logical architectural layers.
// Multiple fragments may map to the same layer — the first match wins.
var knownLayerDirs = []struct {
	fragment string
	layer    string
}{
	{"handler", "handler"},
	{"handlers", "handler"},
	{"controller", "handler"},
	{"controllers", "handler"},
	{"route", "handler"},
	{"routes", "handler"},
	{"service", "service"},
	{"services", "service"},
	{"usecase", "service"},
	{"usecases", "service"},
	{"domain", "service"},
	{"repo", "repository"},
	{"repos", "repository"},
	{"repository", "repository"},
	{"repositories", "repository"},
	{"store", "repository"},
	{"storage", "repository"},
	{"db", "repository"},
	{"database", "repository"},
	{"middleware", "middleware"},
	{"model", "model"},
	{"models", "model"},
	{"entity", "model"},
	{"entities", "model"},
}

func filePatternObservations(base store.SessionObservation, files []string) []store.SessionObservation {
	base.Category = store.ObsCategoryFilePattern

	layerSet := make(map[string]bool)
	for _, f := range files {
		// Walk each path component to detect layer directories.
		parts := strings.Split(filepath.ToSlash(f), "/")
		for _, part := range parts {
			low := strings.ToLower(part)
			for _, kl := range knownLayerDirs {
				if low == kl.fragment {
					layerSet[kl.layer] = true
					break
				}
			}
		}
	}

	var obs []store.SessionObservation

	// Layered architecture: handler + service + repository all touched.
	if layerSet["handler"] && layerSet["service"] && layerSet["repository"] {
		obs = append(obs, ob(base, "layered_architecture_touched", "handler+service+repository", 0.8))
	} else if layerSet["handler"] && layerSet["service"] {
		obs = append(obs, ob(base, "handler_service_touched", "handler+service", 0.6))
	} else if layerSet["handler"] && layerSet["repository"] {
		obs = append(obs, ob(base, "handler_repository_touched", "handler+repository", 0.5))
	}

	// Middleware usage.
	if layerSet["middleware"] {
		obs = append(obs, ob(base, "middleware_files_touched", "", 0.7))
	}

	return obs
}

// ── Library / framework detection ────────────────────────────────────────────

// wellKnownLibraries maps an import path substring to a canonical observation key.
// Checked in order; first match per file wins for each library slot.
var wellKnownLibraries = []struct {
	contains string // substring of import path (case-insensitive)
	key      string // observation key
}{
	// Go testing
	{"testify", "uses_testify"},
	{"gomock", "uses_gomock"},
	{"mockery", "uses_mockery"},
	// Go HTTP frameworks
	{"/chi", "uses_chi_router"},
	{"gin-gonic", "uses_gin_router"},
	{"labstack/echo", "uses_echo_router"},
	{"gorilla/mux", "uses_gorilla_mux"},
	{"fasthttp", "uses_fasthttp"},
	// Go auth
	{"golang-jwt", "uses_golang_jwt"},
	{"dgrijalva/jwt", "uses_jwt_go"},
	// Python testing
	{"pytest", "uses_pytest"},
	{"unittest", "uses_unittest"},
	// Python HTTP
	{"fastapi", "uses_fastapi"},
	{"flask", "uses_flask"},
	{"django", "uses_django"},
	// TypeScript/JS testing
	{"jest", "uses_jest"},
	{"vitest", "uses_vitest"},
	{"mocha", "uses_mocha"},
	// TypeScript/JS HTTP
	{"express", "uses_express"},
	{"fastify", "uses_fastify"},
	{"next", "uses_nextjs"},
	// Java
	{"springframework", "uses_spring"},
	{"junit", "uses_junit"},
	{"mockito", "uses_mockito"},
	// Rust
	{"actix_web", "uses_actix_web"},
	{"axum", "uses_axum"},
	{"tokio", "uses_tokio"},
}

// maxLibraryFileScan caps the number of touched files scanned for library imports.
// OutEdgesForFile is O(total_nodes) — scanning an unbounded file list on large
// projects (50k+ nodes, 100+ touched files) would be prohibitive at end_session.
const maxLibraryFileScan = 20

func libraryUsageObservations(base store.SessionObservation, files []string, g *graph.Graph) []store.SessionObservation {
	base.Category = store.ObsCategoryLibraryUsage

	// Collect all IMPORTS edges from the touched files.
	// Use a set to avoid counting the same library twice across files.
	detected := make(map[string]bool)

	scanFiles := files
	if len(scanFiles) > maxLibraryFileScan {
		scanFiles = scanFiles[:maxLibraryFileScan]
	}

	for _, f := range scanFiles {
		edges := g.OutEdgesForFile(f)
		for _, e := range edges {
			if e.Type != graph.EdgeImports {
				continue
			}
			// The target of an IMPORTS edge is typically a NodeFile or NodePackage
			// whose ID or Name contains the import path. We check both.
			importPath := strings.ToLower(string(e.To))
			for _, lib := range wellKnownLibraries {
				if strings.Contains(importPath, lib.contains) {
					detected[lib.key] = true
					break
				}
			}
		}
	}

	var obs []store.SessionObservation
	for key := range detected {
		// Single-session library detection has medium confidence; 29.2 will raise
		// it once the pattern repeats across multiple sessions.
		obs = append(obs, ob(base, key, "", 0.6))
	}
	return obs
}

// ── Approach outcome ──────────────────────────────────────────────────────────

func approachOutcomeObservation(base store.SessionObservation, retro *store.ToolCallSummary, sess *sessionSummary) store.SessionObservation {
	base.Category = store.ObsCategoryApproachOutcome

	// A productive session: files were touched AND tasks were updated.
	if sess != nil && len(sess.FilesTouched) > 0 && len(sess.TasksUpdated) > 0 {
		return ob(base, "productive_session", "", 0.8)
	}
	// Files touched but no tasks updated — still useful work.
	if sess != nil && len(sess.FilesTouched) > 0 {
		return ob(base, "files_only_session", "", 0.6)
	}
	// High tool call volume with no files touched — exploration or read-only session.
	if retro != nil && retro.TotalCalls >= 5 {
		return ob(base, "read_only_session", "", 0.5)
	}
	// Minimal activity.
	return ob(base, "low_activity_session", "", 0.3)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// ob creates a SessionObservation from a base template.
func ob(base store.SessionObservation, key, value string, confidence float64) store.SessionObservation {
	o := base
	o.Key = key
	o.Value = value
	o.Confidence = confidence
	return o
}

// confidenceByCount maps a raw occurrence count to a confidence score.
// One occurrence = medium confidence; more = higher.
func confidenceByCount(n int) float64 {
	switch {
	case n >= 5:
		return 0.9
	case n >= 3:
		return 0.8
	case n >= 2:
		return 0.6
	default:
		return 0.4
	}
}

