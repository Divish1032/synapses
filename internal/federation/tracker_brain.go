// Package federation — tracker_brain.go provides Tier 2 brain-enhanced
// cross-project dependency detection. When brain (Ollama) is available,
// it uses LLM-powered detection for ambiguous languages that Tier 1's
// deterministic import matching can't handle well (Python, dynamic imports,
// transitive dependencies, aliased imports that don't match module names).
//
// Key design decisions:
//   - Anti-hallucination: EVERY brain-detected dep is validated against the
//     sibling store. If the entity doesn't exist there → discarded silently.
//   - Zero false positives: validation makes hallucination a false-negative
//     (missed dep) rather than a false-positive (phantom dep).
//   - Fail-open: brain unavailable → Tier 1 only. Still good for Go/TS/Rust.
//   - The brain prompt is designed to produce structured JSON output that can
//     be parsed deterministically.
package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/SynapsesOS/synapses/internal/store"
)

// BrainDetectedDep is a single dependency detected by the brain LLM.
type BrainDetectedDep struct {
	TargetProject string  `json:"target_project"` // federation alias
	TargetEntity  string  `json:"target_entity"`  // function/class/struct name
	ImportPath    string  `json:"import_path"`     // the import or URL reference
	Confidence    float64 `json:"confidence"`      // 0.0–1.0
}

// BrainDetector provides Tier 2 dependency detection using LLM analysis.
// It wraps a brain generate function and a resolver for validation.
type BrainDetector struct {
	// Generate calls the brain LLM with a prompt and returns the response.
	// This decouples tracker_brain from the brain package — any LLM backend
	// that implements this signature works.
	Generate func(ctx context.Context, prompt string) (string, error)

	// resolver validates detected deps against sibling stores.
	resolver *Resolver

	// aliases lists the federation aliases to look for.
	aliases []string
}

// NewBrainDetector creates a Tier 2 detector. generate is the LLM function
// (typically brain.Client's Generate or a wrapper). If generate is nil,
// the detector is disabled and DetectDeps returns nil.
func NewBrainDetector(generate func(ctx context.Context, prompt string) (string, error), resolver *Resolver, aliases []string) *BrainDetector {
	return &BrainDetector{
		Generate: generate,
		resolver: resolver,
		aliases:  aliases,
	}
}

// crossProjectPromptSuffix is appended to the file content to ask the brain
// for cross-project dependency detection. The structured JSON output format
// enables deterministic parsing.
const crossProjectPromptSuffix = `
Analyze this source code file for cross-project dependencies to these sibling projects: %s

For each external dependency you find (imports, function calls, API references to the listed projects), output a JSON line:
{"target_project": "<alias>", "target_entity": "<function or class or struct name>", "import_path": "<the import path or module>", "confidence": <0.0-1.0>}

Rules:
- Only include dependencies you are confident about (confidence >= 0.7)
- Only look for references to the listed sibling projects
- Output one JSON object per line, no other text
- If no cross-project dependencies found, output nothing

Source code:
`

// DetectDeps uses the brain LLM to detect cross-project dependencies in
// source code that Tier 1's deterministic matching may have missed.
// Returns only validated deps (entity exists in sibling store).
//
// The detection flow:
//  1. Build prompt with file content + sibling aliases
//  2. Call brain LLM
//  3. Parse JSON lines from response
//  4. Filter by confidence threshold (>= 0.7)
//  5. Validate each dep against sibling store (anti-hallucination)
//  6. Return only validated deps
func (bd *BrainDetector) DetectDeps(ctx context.Context, fileContent string, maxCodeLen int) []RawCrossDep {
	if bd == nil || bd.Generate == nil || bd.resolver == nil {
		return nil
	}
	if len(bd.aliases) == 0 || fileContent == "" {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}

	// Truncate code to avoid blowing up the context window.
	if maxCodeLen <= 0 {
		maxCodeLen = 2000
	}
	code := filterSecretLines(fileContent)
	if len(code) > maxCodeLen {
		code = code[:maxCodeLen]
	}

	prompt := fmt.Sprintf(crossProjectPromptSuffix, strings.Join(bd.aliases, ", ")) + code

	response, err := bd.Generate(ctx, prompt)
	if err != nil {
		return nil // fail-open
	}

	raw := parseBrainDeps(response)
	return bd.validateBrainDeps(ctx, raw)
}

// parseBrainDeps extracts BrainDetectedDep entries from the LLM response.
// Expects one JSON object per line. Malformed lines are silently skipped.
func parseBrainDeps(response string) []BrainDetectedDep {
	var deps []BrainDetectedDep
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}

		var dep BrainDetectedDep
		if err := json.Unmarshal([]byte(line), &dep); err != nil {
			continue // malformed JSON → skip
		}

		// Confidence threshold filter.
		if math.IsNaN(dep.Confidence) || dep.Confidence < 0.7 {
			continue
		}

		// Basic sanity: must have target project and entity.
		if dep.TargetProject == "" || dep.TargetEntity == "" {
			continue
		}

		deps = append(deps, dep)
	}
	return deps
}

// BrainTrackerAdapter implements watcher.BrainCrossProjectTracker by
// combining BrainDetector (LLM detection) with DeterministicDetector
// (entity resolution + dedup). This is the concrete type wired into the watcher.
type BrainTrackerAdapter struct {
	brain    *BrainDetector
	detector *DeterministicDetector
}

// NewBrainTrackerAdapter creates a tracker adapter for the watcher.
// Returns nil if brain or detector is nil.
func NewBrainTrackerAdapter(brain *BrainDetector, detector *DeterministicDetector) *BrainTrackerAdapter {
	if brain == nil || detector == nil {
		return nil
	}
	return &BrainTrackerAdapter{brain: brain, detector: detector}
}

// DetectAndStoreBrain implements watcher.BrainCrossProjectTracker.
// Reads the file, calls brain for detection, resolves and deduplicates
// against Tier 1 deps, then stores any new deps.
func (a *BrainTrackerAdapter) DetectAndStoreBrain(ctx context.Context, filePath string, localStore *store.Store) {
	if a == nil || localStore == nil || ctx.Err() != nil {
		return
	}

	// Only run for languages Tier 1 doesn't cover well.
	lang := langFromExt(filePath)
	switch lang {
	case "go", "typescript", "rust":
		return // Tier 1 handles these well enough
	}
	// Python, Ruby, Java, etc. — brain can help.
	if lang == "" {
		return // unknown extension, skip
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return // fail-open
	}

	brainDeps := a.brain.DetectDeps(ctx, string(content), 2000)
	if len(brainDeps) == 0 {
		return
	}

	// Get existing Tier 1 deps for dedup.
	fromEntity := "file:" + filePath
	existingDeps, _ := localStore.GetCrossProjectDeps(fromEntity)

	// Resolve and deduplicate.
	resolved := a.detector.ResolveBrainDeps(ctx, brainDeps, existingDeps, filePath)
	if len(resolved) == 0 {
		return
	}

	// Store the new Tier 2 deps.
	a.detector.StoreDeps(resolved, localStore)
}

// validateBrainDeps checks each brain-detected dep against the sibling store.
// Only deps where the entity actually exists are returned.
// This is the anti-hallucination gate — the brain may claim a dep exists,
// but we only trust it if the sibling's graph confirms it.
// filterSecretLines removes lines that match common secret patterns before
// sending file content to the LLM.
func filterSecretLines(content string) string {
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if looksLikeSecret(line) {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

// secretPatterns are case-insensitive substrings that indicate a line contains
// or assigns a secret value. Covers environment variables, config files, code
// constants, and common provider-specific key names.
var secretPatterns = []string{
	// Generic assignment patterns
	"API_KEY=", "APIKEY=", "API_KEY:", "APIKEY:",
	"SECRET=", "SECRET:", "SECRET_KEY", "SECRETKEY",
	"PASSWORD=", "PASSWORD:", "PASSWD=", "PASSWD:",
	"TOKEN=", "TOKEN:", "_TOKEN=", "_TOKEN:",
	"PRIVATE_KEY", "PRIVATEKEY",
	"CREDENTIALS=", "CREDENTIALS:",

	// AWS
	"AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN",

	// Database
	"DATABASE_URL=", "DATABASE_URL:", "DB_PASSWORD", "DB_PASS",
	"MONGO_URI=", "REDIS_URL=", "REDIS_PASSWORD",

	// OAuth / social
	"CLIENT_SECRET", "GITHUB_TOKEN", "SLACK_TOKEN", "SLACK_WEBHOOK",
	"DISCORD_TOKEN", "OPENAI_API_KEY",

	// PEM blocks
	"-----BEGIN RSA", "-----BEGIN EC", "-----BEGIN PRIVATE",
	"-----BEGIN OPENSSH", "-----BEGIN PGP",

	// Generic bearer/auth
	"BEARER ", "AUTHORIZATION:",

	// Common prefixes for API keys
	"SK_LIVE_", "SK_TEST_", "PK_LIVE_", "PK_TEST_",
	"GHPAT_", "GHP_", "GHO_", "GHU_", "GHS_", "GHR_",
	"XOXB-", "XOXP-", "XOXA-",
	"SG.", // SendGrid
}

// looksLikeSecret returns true if the line likely contains a secret value.
func looksLikeSecret(line string) bool {
	upper := strings.ToUpper(strings.TrimSpace(line))
	for _, pat := range secretPatterns {
		if strings.Contains(upper, pat) {
			return true
		}
	}
	return false
}

func (bd *BrainDetector) validateBrainDeps(ctx context.Context, raw []BrainDetectedDep) []RawCrossDep {
	var valid []RawCrossDep
	for _, dep := range raw {
		if ctx.Err() != nil {
			break
		}

		// Check if the entity actually exists in the sibling store.
		if !bd.resolver.EntityExists(ctx, dep.TargetProject, dep.TargetEntity) {
			continue // hallucination or stale reference → discard silently
		}

		valid = append(valid, RawCrossDep{
			FromImport:    dep.ImportPath,
			ToProject:     dep.TargetProject,
			ToEntity:      dep.TargetEntity,
			DetectionTier: "brain",
		})
	}
	return valid
}
