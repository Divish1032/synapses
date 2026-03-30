// Package mcp — SDLC auto-detection from agent tool-call patterns.
//
// Sprint 27.1: Infers the project SDLC phase (planning/development/testing/
// review/deployment) by analyzing the stream of tool calls and file signals.
// Pure heuristic — no LLM required. Runs every checkInterval tool calls.
//
// An explicit SetSDLCPhase call from the agent always overrides auto-detection
// for the remainder of the session.
package mcp

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/SynapsesOS/synapses/internal/brain"
)

// sdlcDetector accumulates tool-call signals and periodically infers the SDLC phase.
type sdlcDetector struct {
	mu sync.Mutex

	// Counters reset after each detection cycle.
	callCount int
	toolHits  map[string]int // tool name → invocation count since last detect
	fileKinds map[string]int // "test"/"config"/"code"/"doc" → count

	// Sticky state across detection cycles.
	lastPhase      brain.SDLCPhase
	lastConfidence float64
	explicitlySet  bool // true once the agent calls SetSDLCPhase
	checkInterval  int  // how many calls between detection runs (default 5)
}

// newSDLCDetector creates a detector with default settings.
func newSDLCDetector() *sdlcDetector {
	return &sdlcDetector{
		toolHits:      make(map[string]int),
		fileKinds:     make(map[string]int),
		checkInterval: 5,
	}
}

// recordCall is invoked after every MCP tool call (inside the ledger wrapper).
// It increments counters and, every checkInterval calls, runs detection.
// Returns the detected phase + quality mode when a transition fires, or empty
// strings when no change occurred or detection was not triggered.
func (d *sdlcDetector) recordCall(toolName string, entities, files []string) (phase brain.SDLCPhase, mode brain.QualityMode, changed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.explicitlySet {
		return "", "", false
	}

	d.callCount++
	d.toolHits[toolName]++

	// Classify file paths into categories.
	for _, f := range files {
		kind := classifyFile(f)
		d.fileKinds[kind]++
	}
	// Also classify entity names that look like file paths.
	for _, e := range entities {
		if strings.Contains(e, "/") || strings.Contains(e, ".") {
			kind := classifyFile(e)
			d.fileKinds[kind]++
		}
	}

	if d.callCount < d.checkInterval {
		return "", "", false
	}

	// --- Run detection ---
	phase, confidence := d.detect()
	d.resetCounters()

	if phase == "" || phase == brain.PhaseUnknown {
		return "", "", false
	}

	// Hysteresis: only transition if new confidence exceeds old by margin.
	if phase == d.lastPhase {
		return "", "", false
	}
	if confidence <= d.lastConfidence-0.1 {
		return "", "", false
	}

	d.lastPhase = phase
	d.lastConfidence = confidence

	mode = phaseToQualityMode(phase)
	return phase, mode, true
}

// markExplicit records that the agent explicitly set the SDLC phase.
// Auto-detection is suppressed for the rest of the session.
func (d *sdlcDetector) markExplicit() {
	d.mu.Lock()
	d.explicitlySet = true
	d.mu.Unlock()
}

// reset clears all state — called on new session_init.
func (d *sdlcDetector) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.callCount = 0
	d.toolHits = make(map[string]int)
	d.fileKinds = make(map[string]int)
	d.lastPhase = ""
	d.lastConfidence = 0
	d.explicitlySet = false
}

// phaseOrder defines deterministic iteration order for tie-breaking.
// Development is first so it wins ties (it's the baseline).
var phaseOrder = []brain.SDLCPhase{
	brain.PhaseDevelopment,
	brain.PhaseTesting,
	brain.PhaseReview,
	brain.PhasePlanning,
	brain.PhaseDeployment,
}

// detect scores each phase and returns the highest. Must be called under d.mu.
// Tie-breaking: deterministic via phaseOrder (development wins ties).
func (d *sdlcDetector) detect() (brain.SDLCPhase, float64) {
	scores := [5]float64{
		d.scoreDevelopment(),
		d.scoreTesting(),
		d.scoreReview(),
		d.scorePlanning(),
		d.scoreDeployment(),
	}

	var bestPhase brain.SDLCPhase
	var bestScore float64
	for i, phase := range phaseOrder {
		if scores[i] > bestScore {
			bestScore = scores[i]
			bestPhase = phase
		}
	}

	if bestScore < 0.15 {
		return "", 0 // signal too weak
	}

	// Confidence based on signal concentration.
	total := 0.0
	for _, s := range scores {
		total += float64(s)
	}
	confidence := 0.0
	if total > 0 {
		concentration := bestScore / total
		switch {
		case concentration > 0.8:
			confidence = 0.85
		case concentration > 0.6:
			confidence = 0.7
		case concentration > 0.4:
			confidence = 0.5
		default:
			confidence = 0.3
		}
	}

	return bestPhase, confidence
}

func (d *sdlcDetector) scorePlanning() float64 {
	var score float64
	score += float64(d.toolHits["create_plan"]) * 0.3
	score += float64(d.toolHits["get_plans"]) * 0.2
	score += float64(d.toolHits["rules"]) * 0.15
	score += float64(d.toolHits["upsert_rule"]) * 0.2
	score += float64(d.toolHits["upsert_adr"]) * 0.25
	score += float64(d.toolHits["get_adrs"]) * 0.15
	score += float64(d.toolHits["search"]) * 0.05 // broad exploration
	score += float64(d.fileKinds["doc"]) * 0.1
	return score
}

func (d *sdlcDetector) scoreDevelopment() float64 {
	var score float64
	score += float64(d.toolHits["get_context"]) * 0.15
	score += float64(d.toolHits["find_entity"]) * 0.15
	score += float64(d.toolHits["search"]) * 0.1
	score += float64(d.toolHits["link_task_nodes"]) * 0.2
	score += float64(d.toolHits["update_task"]) * 0.1
	score += float64(d.fileKinds["code"]) * 0.1
	return score
}

func (d *sdlcDetector) scoreTesting() float64 {
	var score float64
	score += float64(d.toolHits["validate"]) * 0.2
	score += float64(d.toolHits["verify_implementation"]) * 0.25
	score += float64(d.toolHits["get_impact"]) * 0.1
	score += float64(d.fileKinds["test"]) * 0.25
	return score
}

func (d *sdlcDetector) scoreReview() float64 {
	var score float64
	score += float64(d.toolHits["get_impact"]) * 0.2
	score += float64(d.toolHits["get_context"]) * 0.05 // lower weight — shared with dev
	score += float64(d.toolHits["get_call_chain"]) * 0.15
	score += float64(d.toolHits["annotate"]) * 0.15
	score += float64(d.toolHits["annotate_node"]) * 0.15
	// Review = reads many, edits none. Penalize if code files are touched.
	if d.fileKinds["code"] == 0 && d.toolHits["get_context"]+d.toolHits["get_impact"] > 2 {
		score += 0.2
	}
	return score
}

func (d *sdlcDetector) scoreDeployment() float64 {
	var score float64
	score += float64(d.fileKinds["config"]) * 0.25
	score += float64(d.toolHits["validate"]) * 0.1
	return score
}

func (d *sdlcDetector) resetCounters() {
	d.callCount = 0
	d.toolHits = make(map[string]int)
	d.fileKinds = make(map[string]int)
}

// classifyFile categorises a file path into test/config/code/doc.
func classifyFile(path string) string {
	base := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))
	dir := strings.ToLower(filepath.Dir(path))

	// Test files
	if strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, ".test.jsx") ||
		strings.HasSuffix(base, "_test.py") ||
		strings.HasSuffix(base, "_spec.rb") ||
		strings.HasSuffix(base, "_test.rs") ||
		strings.Contains(dir, "test") ||
		strings.Contains(dir, "__tests__") {
		return "test"
	}

	// Config/deploy files
	switch base {
	case "dockerfile", "docker-compose.yml", "docker-compose.yaml",
		"makefile", ".goreleaser.yml", ".goreleaser.yaml",
		"package.json", "tsconfig.json", "cargo.toml",
		"jenkinsfile", "procfile":
		return "config"
	}
	switch ext {
	case ".yml", ".yaml", ".toml", ".ini", ".cfg", ".env":
		return "config"
	}
	if strings.Contains(dir, ".github") || strings.Contains(dir, "/ci/") ||
		strings.HasPrefix(dir, "ci/") || strings.Contains(dir, "/deploy/") ||
		strings.HasPrefix(dir, "deploy/") || strings.Contains(dir, "/infra/") ||
		strings.HasPrefix(dir, "infra/") {
		return "config"
	}

	// Documentation
	switch ext {
	case ".md", ".mdx", ".rst", ".txt", ".adoc":
		return "doc"
	}

	return "code"
}

// phaseToQualityMode maps an auto-detected phase to its default quality mode.
func phaseToQualityMode(phase brain.SDLCPhase) brain.QualityMode {
	switch phase {
	case brain.PhaseTesting, brain.PhaseReview, brain.PhaseDeployment:
		return brain.QualityEnterprise
	case brain.PhasePlanning:
		return brain.QualityQuick
	default: // development
		return brain.QualityStandard
	}
}
