package brain_test

// Modelfile content validation tests.
//
// These tests parse all 5 Ollama Modelfiles in synapses-fine-distilling/quantization/
// and verify they are structurally correct and semantically consistent with the
// runtime configuration. A Modelfile that is missing a required field, has the
// wrong temperature, or omits the stop token will cause Ollama to misbehave at
// runtime — these bugs are invisible until the model is actually loaded.
//
// Tests run as part of the normal `go test ./internal/brain/...` suite — no Ollama required.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// modelfileSpec defines what we require from each Modelfile.
type modelfileSpec struct {
	// file is the filename in the quantization/ directory.
	file string
	// fromSubstring is a required substring of the FROM line.
	// For FT models: the GGUF filename. For base models: the Ollama model tag.
	fromSubstring string
	// requireSystemJSON is true when the SYSTEM block must instruct the model to
	// output ONLY valid JSON. FT models rely on this for deterministic output.
	requireSystemJSON bool
	// maxTemperature is the highest acceptable temperature PARAMETER value.
	// Sentry must be 0.0 (deterministic). Others 0.0–0.4.
	maxTemperature float64
	// requireStopImEnd is true when <|im_end|> stop token must be present.
	// Required for all Qwen-family models.
	requireStopImEnd bool
	// minNumPredict is the minimum acceptable num_predict value.
	// Prevents accidental truncation of outputs.
	minNumPredict int
	// maxNumPredict is the maximum acceptable value (prevents runaway generation).
	maxNumPredict int
	// isChatMode signals that this model uses /api/chat — no special Modelfile
	// check needed, but we verify the SYSTEM block is present (Ollama applies
	// SYSTEM blocks only via /api/chat, not /api/generate).
	requireSystem bool
}

// All 5 Modelfiles now use base qwen3.5:2b — no fine-tuned GGUFs.
var modelfileSpecs = []modelfileSpec{
	{
		file:              "Modelfile.sentry",
		fromSubstring:     "qwen3.5:2b",
		requireSystemJSON: true,
		maxTemperature:    0.0, // must be exactly 0: deterministic classifier
		requireStopImEnd:  true,
		minNumPredict:     64,
		maxNumPredict:     256,
		requireSystem:     true,
	},
	{
		file:              "Modelfile.librarian",
		fromSubstring:     "qwen3.5:2b",
		requireSystemJSON: true,
		maxTemperature:    0.3,
		requireStopImEnd:  true,
		minNumPredict:     256,
		maxNumPredict:     1024,
		requireSystem:     true,
	},
	{
		file:              "Modelfile.critic",
		fromSubstring:     "qwen3.5:2b",
		requireSystemJSON: true,
		maxTemperature:    0.2,
		requireStopImEnd:  true,
		minNumPredict:     256,
		maxNumPredict:     1024,
		requireSystem:     true,
	},
	{
		file:              "Modelfile.navigator",
		fromSubstring:     "qwen3.5:2b",
		requireSystemJSON: true,
		maxTemperature:    0.3,
		requireStopImEnd:  true,
		minNumPredict:     256,
		maxNumPredict:     1024,
		requireSystem:     true,
	},
	{
		file:              "Modelfile.archivist",
		fromSubstring:     "qwen3.5:2b",
		requireSystemJSON: true,
		maxTemperature:    0.4,
		requireStopImEnd:  true,
		minNumPredict:     512,
		maxNumPredict:     2048,
		requireSystem:     true,
	},
}

// quantizationDir returns the absolute path to synapses-fine-distilling/quantization/.
// Uses runtime.Caller to locate relative to this test file.
func quantizationDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = .../synapses-os/synapses/internal/brain/modelfile_validation_test.go
	// Go up 3 levels to repo root: brain → internal → synapses → synapses-os
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	dir := filepath.Join(repoRoot, "synapses-fine-distilling", "quantization")
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	return abs
}

// parsedModelfile holds extracted fields from a Modelfile.
type parsedModelfile struct {
	From          string
	SystemLines   []string // raw lines inside SYSTEM block
	Parameters    map[string]string
	HasSystemJSON bool // SYSTEM block contains "json" keyword
}

// parseModelfile reads and parses a Modelfile into structured fields.
func parseModelfile(path string) (*parsedModelfile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	result := &parsedModelfile{
		Parameters: make(map[string]string),
	}

	scanner := bufio.NewScanner(f)
	var inSystem bool
	var systemBuf strings.Builder

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r") // strip CRLF if present
		trimmed := strings.TrimSpace(line)

		// Skip comment lines.
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		upper := strings.ToUpper(trimmed)

		switch {
		case strings.HasPrefix(upper, "FROM "):
			result.From = strings.TrimSpace(trimmed[5:])

		case inSystem:
			// Check if this line closes the SYSTEM block.
			// Closing """ may appear:
			//   (a) alone on its own line: """
			//   (b) at the end of a content line: ...content"""
			if strings.HasSuffix(trimmed, `"""`) {
				// Content before the closing triple-quote (may be empty).
				content := trimmed[:len(trimmed)-3]
				if content != "" {
					systemBuf.WriteString(content + "\n")
				}
				inSystem = false
				full := systemBuf.String()
				result.SystemLines = strings.Split(full, "\n")
				lc := strings.ToLower(full)
				result.HasSystemJSON = strings.Contains(lc, "json")
			} else {
				systemBuf.WriteString(line + "\n")
			}

		case strings.HasPrefix(upper, "SYSTEM "):
			// SYSTEM """ or SYSTEM """content...
			// Strip the leading "SYSTEM " prefix (case-insensitive length = 7).
			rest := strings.TrimSpace(trimmed[7:])
			if strings.HasPrefix(rest, `"""`) {
				afterOpen := rest[3:] // content after opening """
				if strings.HasSuffix(afterOpen, `"""`) {
					// Single-line: SYSTEM """content"""
					content := afterOpen[:len(afterOpen)-3]
					result.SystemLines = strings.Split(content, "\n")
					lc := strings.ToLower(content)
					result.HasSystemJSON = strings.Contains(lc, "json")
				} else {
					// Multi-line opening: SYSTEM """first line of content
					inSystem = true
					if afterOpen != "" {
						systemBuf.WriteString(afterOpen + "\n")
					}
				}
			}

		case strings.HasPrefix(upper, "PARAMETER "):
			// PARAMETER <key> <value>
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 {
				key := strings.ToLower(parts[1])
				val := strings.Join(parts[2:], " ")
				// For multi-value params like stop, append.
				if existing, ok := result.Parameters[key]; ok {
					result.Parameters[key] = existing + "," + val
				} else {
					result.Parameters[key] = val
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	return result, nil
}

// TestParseModelfile_CRLF_LineEndings verifies that parseModelfile handles
// Windows-style CRLF line endings without corrupting the FROM value or
// failing to detect the SYSTEM block / PARAMETER values.
// This is a regression test — the \r was silently corrupting parsed values
// before the strings.TrimRight fix was applied.
func TestParseModelfile_CRLF_LineEndings(t *testing.T) {
	t.Parallel()
	// Write a minimal Modelfile with CRLF line endings to a temp file.
	content := "FROM qwen3.5:2b\r\n" +
		"SYSTEM \"\"\"You must output valid json.\r\n" +
		"\"\"\"\r\n" +
		"PARAMETER temperature 0.1\r\n" +
		"PARAMETER stop <|im_end|>\r\n" +
		"PARAMETER num_predict 256\r\n"

	f, err := os.CreateTemp(t.TempDir(), "Modelfile.crlf.*")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	mf, err := parseModelfile(f.Name())
	if err != nil {
		t.Fatalf("parseModelfile: %v", err)
	}

	if mf.From != "qwen3.5:2b" {
		t.Errorf("FROM = %q, want qwen3.5:2b — CRLF corrupted the value", mf.From)
	}
	if len(mf.SystemLines) == 0 {
		t.Error("SYSTEM block not parsed — CRLF may have broken block detection")
	}
	if !mf.HasSystemJSON {
		t.Error("HasSystemJSON = false, want true — SYSTEM contains 'json'")
	}
	if mf.Parameters["temperature"] != "0.1" {
		t.Errorf("temperature = %q, want 0.1", mf.Parameters["temperature"])
	}
	if !strings.Contains(mf.Parameters["stop"], "<|im_end|>") {
		t.Errorf("stop = %q, want <|im_end|>", mf.Parameters["stop"])
	}
}

// TestModelfiles_AllExist verifies all 5 Modelfiles are present.
func TestModelfiles_AllExist(t *testing.T) {
	dir := quantizationDir(t)
	for _, spec := range modelfileSpecs {
		path := filepath.Join(dir, spec.file)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("%s: file does not exist at %s", spec.file, path)
		}
	}
}

// TestModelfiles_FROM_PointsToCorrectBase verifies each FROM line references
// the expected GGUF file or base model tag.
func TestModelfiles_FROM_PointsToCorrectBase(t *testing.T) {
	dir := quantizationDir(t)
	for _, spec := range modelfileSpecs {
		spec := spec
		t.Run(spec.file, func(t *testing.T) {
			mf, err := parseModelfile(filepath.Join(dir, spec.file))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if mf.From == "" {
				t.Errorf("FROM line missing or empty")
				return
			}
			if !strings.Contains(mf.From, spec.fromSubstring) {
				t.Errorf("FROM = %q, want it to contain %q", mf.From, spec.fromSubstring)
			}
		})
	}
}

// TestModelfiles_SYSTEM_Present verifies all Modelfiles have a SYSTEM block.
// Without a SYSTEM block, the Modelfile identity is indistinguishable from the
// base model — the model receives no role instructions.
func TestModelfiles_SYSTEM_Present(t *testing.T) {
	dir := quantizationDir(t)
	for _, spec := range modelfileSpecs {
		spec := spec
		t.Run(spec.file, func(t *testing.T) {
			if !spec.requireSystem {
				return
			}
			mf, err := parseModelfile(filepath.Join(dir, spec.file))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(mf.SystemLines) == 0 {
				t.Errorf("SYSTEM block missing — model will receive no role instructions")
			}
		})
	}
}

// TestModelfiles_SYSTEM_ContainsJSONInstruction verifies that every Modelfile
// whose output is parsed as JSON instructs the model to emit JSON in its SYSTEM block.
// Without this, the model may produce prose instead of JSON, causing parse failures
// even when Ollama's format:"json" constraint is active.
func TestModelfiles_SYSTEM_ContainsJSONInstruction(t *testing.T) {
	dir := quantizationDir(t)
	for _, spec := range modelfileSpecs {
		spec := spec
		t.Run(spec.file, func(t *testing.T) {
			if !spec.requireSystemJSON {
				return
			}
			mf, err := parseModelfile(filepath.Join(dir, spec.file))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !mf.HasSystemJSON {
				t.Errorf("SYSTEM block does not mention 'json' — model may produce prose instead of JSON output")
			}
		})
	}
}

// TestModelfiles_Temperature_WithinBounds verifies temperature PARAMETER is set
// and within the acceptable range for each model's role.
func TestModelfiles_Temperature_WithinBounds(t *testing.T) {
	dir := quantizationDir(t)
	for _, spec := range modelfileSpecs {
		spec := spec
		t.Run(spec.file, func(t *testing.T) {
			mf, err := parseModelfile(filepath.Join(dir, spec.file))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			tempStr, ok := mf.Parameters["temperature"]
			if !ok {
				t.Errorf("PARAMETER temperature missing — model uses Ollama default (0.8) which is too high for JSON output")
				return
			}
			temp, err := strconv.ParseFloat(strings.TrimSpace(tempStr), 64)
			if err != nil {
				t.Errorf("PARAMETER temperature = %q: not a valid float: %v", tempStr, err)
				return
			}
			if temp < 0.0 {
				t.Errorf("temperature = %.2f, must be >= 0.0", temp)
			}
			if temp > spec.maxTemperature {
				t.Errorf("temperature = %.2f exceeds max %.2f for this role (higher = less deterministic JSON output)",
					temp, spec.maxTemperature)
			}
		})
	}
}

// TestModelfiles_StopToken_ImEnd verifies <|im_end|> stop token is present.
// All Qwen-family models use this token to signal end-of-turn. Without it,
// the model continues generating after the JSON object, corrupting the output.
func TestModelfiles_StopToken_ImEnd(t *testing.T) {
	dir := quantizationDir(t)
	for _, spec := range modelfileSpecs {
		spec := spec
		t.Run(spec.file, func(t *testing.T) {
			if !spec.requireStopImEnd {
				return
			}
			mf, err := parseModelfile(filepath.Join(dir, spec.file))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			stopVal := mf.Parameters["stop"]
			if !strings.Contains(stopVal, "<|im_end|>") {
				t.Errorf("PARAMETER stop does not include <|im_end|> — Qwen model may not stop after JSON object.\nGot stop = %q", stopVal)
			}
		})
	}
}

// TestModelfiles_NumPredict_WithinBounds verifies num_predict is set and within
// sensible bounds for each model's role.
func TestModelfiles_NumPredict_WithinBounds(t *testing.T) {
	dir := quantizationDir(t)
	for _, spec := range modelfileSpecs {
		spec := spec
		t.Run(spec.file, func(t *testing.T) {
			mf, err := parseModelfile(filepath.Join(dir, spec.file))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			npStr, ok := mf.Parameters["num_predict"]
			if !ok {
				t.Errorf("PARAMETER num_predict missing — model uses Ollama default which may truncate output")
				return
			}
			np, err := strconv.Atoi(strings.TrimSpace(npStr))
			if err != nil {
				t.Errorf("PARAMETER num_predict = %q: not a valid int: %v", npStr, err)
				return
			}
			if np < spec.minNumPredict {
				t.Errorf("num_predict = %d < min %d — JSON output may be truncated mid-object", np, spec.minNumPredict)
			}
			if np > spec.maxNumPredict {
				t.Errorf("num_predict = %d > max %d — runaway generation risk", np, spec.maxNumPredict)
			}
		})
	}
}

// TestModelfiles_AllUseBaseModel verifies that ALL Modelfiles use a bare Ollama
// tag (qwen3.5:2b) as their FROM, not a GGUF file path. All tiers now use the
// base model with system prompts — FT models were evaluated and dropped.
func TestModelfiles_AllUseBaseModel(t *testing.T) {
	dir := quantizationDir(t)
	allModels := []string{"Modelfile.sentry", "Modelfile.librarian", "Modelfile.critic", "Modelfile.navigator", "Modelfile.archivist"}
	for _, file := range allModels {
		file := file
		t.Run(file, func(t *testing.T) {
			mf, err := parseModelfile(filepath.Join(dir, file))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if strings.Contains(strings.ToLower(mf.From), ".gguf") {
				t.Errorf("FROM = %q — Navigator/Archivist must use base Ollama tag (not a GGUF) to avoid catastrophic forgetting", mf.From)
			}
		})
	}
}
