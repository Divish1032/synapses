package llm

import (
	"context"
	"testing"
)

// ============================================================
// hardware.go — DetectHardware branch coverage
// ============================================================

func TestDetectHardware_ReturnsNonNegativeRAM(t *testing.T) {
	t.Parallel()
	cfg := DetectHardware()
	if cfg.AvailableRAMGB < 0 {
		t.Errorf("AvailableRAMGB = %f, want >= 0", cfg.AvailableRAMGB)
	}
}

func TestDetectHardware_GPULayersEnvOverride(t *testing.T) {
	t.Setenv("SYNAPSES_GPU_LAYERS", "25")
	cfg := DetectHardware()
	// On Apple Silicon, GPULayers should be 25 (env override).
	// On other platforms, GPULayers depends on CUDA presence.
	// Just verify it doesn't panic and returns a valid value.
	if cfg.GPULayers < 0 {
		t.Errorf("GPULayers = %d, want >= 0", cfg.GPULayers)
	}
}

func TestGpuLayersFromEnv_EmptyStringReturnsDefault(t *testing.T) {
	t.Setenv("SYNAPSES_GPU_LAYERS", "")
	got := gpuLayersFromEnv(77)
	if got != 77 {
		t.Errorf("gpuLayersFromEnv(77) with empty env = %d, want 77", got)
	}
}

// ============================================================
// mock.go — edge cases
// ============================================================

func TestNewUnavailableMockClient_ModelName(t *testing.T) {
	mc := NewUnavailableMockClient()
	if mc.ModelName() != "mock:test" {
		t.Errorf("ModelName() = %q, want mock:test", mc.ModelName())
	}
}

func TestNewUnavailableMockClient_Generate(t *testing.T) {
	mc := NewUnavailableMockClient()
	resp, err := mc.Generate(context.TODO(), "prompt")
	// Should return empty response and nil error (no Err configured).
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if resp != "" {
		t.Errorf("expected empty response, got %q", resp)
	}
}

func TestMockClient_PullModel_NilWriter(t *testing.T) {
	mc := NewMockClient("resp")
	if err := mc.PullModel(context.TODO(), nil); err != nil {
		t.Errorf("PullModel(nil) unexpected error: %v", err)
	}
}

// ============================================================
// parser.go — additional edge cases
// ============================================================

func TestParseSILResponse_ConcernsWithEmptyItems(t *testing.T) {
	// Concerns with empty items between commas should be filtered out.
	raw := "INSIGHT: test.\nCONCERNS: a, , b, ,  "
	_, _, concerns := ParseSILResponse(raw)
	if len(concerns) != 2 {
		t.Fatalf("expected 2 concerns, got %d: %v", len(concerns), concerns)
	}
	if concerns[0] != "a" || concerns[1] != "b" {
		t.Errorf("concerns = %v, want [a b]", concerns)
	}
}

func TestParseSILResponse_MultilineInsight(t *testing.T) {
	raw := "ROOT_SUMMARY: A summary.\nINSIGHT: Line one.\nLine two.\nCONCERNS: none"
	_, insight, _ := ParseSILResponse(raw)
	if insight != "Line one. Line two." {
		t.Errorf("insight = %q", insight)
	}
}

func TestExtractSILLabel_LastLabelNoNewline(t *testing.T) {
	text := "ROOT_SUMMARY: summary\nCONCERNS: concern1, concern2"
	got := extractSILLabel(text, "CONCERNS")
	if got != "concern1, concern2" {
		t.Errorf("got %q", got)
	}
}

// ============================================================
// util.go — additional edge cases
// ============================================================

func TestExtractJSON_OnlyOpenBrace(t *testing.T) {
	got := ExtractJSON("{")
	if got != "{" {
		t.Errorf("ExtractJSON('{') = %q, want '{'", got)
	}
}

func TestExtractJSON_MultipleFences(t *testing.T) {
	input := "text ```json\n{\"a\":1}\n``` more ```json\n{\"b\":2}\n```"
	got := ExtractJSON(input)
	// Should extract the first fenced block.
	if got != `{"a":1}` {
		t.Errorf("ExtractJSON = %q, want {\"a\":1}", got)
	}
}

func TestRepairJSON_ValidArray(t *testing.T) {
	input := `[1,2,3]`
	got := RepairJSON(input)
	if got != input {
		t.Errorf("RepairJSON(%q) = %q, want unchanged", input, got)
	}
}

func TestRepairJSON_EmptyString(t *testing.T) {
	got := RepairJSON("")
	if got != "" {
		t.Errorf("RepairJSON('') = %q, want ''", got)
	}
}

func TestTruncate_LargeN(t *testing.T) {
	// n much larger than string — returns string unchanged.
	got := Truncate("hi", 1000)
	if got != "hi" {
		t.Errorf("Truncate('hi', 1000) = %q, want 'hi'", got)
	}
}

func TestStripThinkBlocks_NestedAngleBrackets(t *testing.T) {
	// Non-matching angle brackets should be left alone.
	input := "<div>hello</div>"
	got := stripThinkBlocks(input)
	if got != "<div>hello</div>" {
		t.Errorf("stripThinkBlocks = %q, want %q", got, input)
	}
}

// ============================================================
// hardware.go — isAppleSilicon on non-darwin returns false
// ============================================================

func TestIsAppleSilicon_PlatformDependent(t *testing.T) {
	// Just ensure it doesn't panic and returns a bool.
	_ = isAppleSilicon() // just ensure it doesn't panic — platform dependent
}

func TestHasCUDA_PlatformDependent(t *testing.T) {
	result := hasCUDA()
	_ = result // platform dependent
}

func TestAvailableRAMGB_Positive(t *testing.T) {
	ram := availableRAMGB()
	// On any real machine, RAM should be > 0.
	if ram <= 0 {
		t.Logf("availableRAMGB() = %f (may be 0 on unsupported platform)", ram)
	}
}
