package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// util.go — stripThinkBlocks, ExtractJSON, Truncate
// ============================================================

func TestStripThinkBlocks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no think block",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "single think block",
			input: "<think>internal reasoning</think>actual answer",
			want:  "actual answer",
		},
		{
			name:  "think block with newlines",
			input: "<think>\nmultiline\nreasoning\n</think>\nfinal answer",
			want:  "final answer",
		},
		{
			name:  "multiple think blocks",
			input: "<think>first</think>middle<think>second</think>end",
			want:  "middleend",
		},
		{
			name:  "only think block",
			input: "<think>only this</think>",
			want:  "",
		},
		{
			name:  "leading/trailing whitespace after strip",
			input: "  <think>hidden</think>  result  ",
			want:  "result",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripThinkBlocks(tc.input)
			if got != tc.want {
				t.Errorf("stripThinkBlocks(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain JSON object",
			input: `{"key": "value"}`,
			want:  `{"key": "value"}`,
		},
		{
			name:  "JSON in backtick-json fence",
			input: "```json\n{\"key\": \"value\"}\n```",
			want:  `{"key": "value"}`,
		},
		{
			name:  "JSON in plain backtick fence",
			input: "```\n{\"key\": \"value\"}\n```",
			want:  `{"key": "value"}`,
		},
		{
			name:  "JSON with preamble text before brace",
			input: "Here is the result: {\"score\": 42}",
			want:  `{"score": 42}`,
		},
		{
			name:  "JSON with trailing text after closing brace",
			input: `{"a": "b"} some trailing text`,
			want:  `{"a": "b"}`,
		},
		{
			name:  "no JSON object at all",
			input: "just plain text with no braces",
			want:  "just plain text with no braces",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "nested JSON object",
			input: `{"outer": {"inner": 1}}`,
			want:  `{"outer": {"inner": 1}}`,
		},
		{
			name:  "fence with leading whitespace",
			input: "  ```json\n{\"x\": 1}\n```  ",
			want:  `{"x": 1}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractJSON(tc.input)
			if got != tc.want {
				t.Errorf("ExtractJSON(%q) = %q; want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		n     int
		want  string
	}{
		{
			name:  "string shorter than limit",
			input: "hello",
			n:     10,
			want:  "hello",
		},
		{
			name:  "string exactly at limit",
			input: "hello",
			n:     5,
			want:  "hello",
		},
		{
			name:  "string longer than limit",
			input: "hello world",
			n:     5,
			want:  "hello...",
		},
		{
			name:  "empty string",
			input: "",
			n:     5,
			want:  "",
		},
		{
			name:  "limit zero",
			input: "hello",
			n:     0,
			want:  "...",
		},
		{
			name:  "single character over limit",
			input: "ab",
			n:     1,
			want:  "a...",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Truncate(tc.input, tc.n)
			if got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q; want %q", tc.input, tc.n, got, tc.want)
			}
		})
	}
}

// ============================================================
// parser.go — ParseSILResponse, extractSILLabel, isSILLabelLine
// ============================================================

func TestIsSILLabelLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"ROOT_SUMMARY: foo", true},
		{"INSIGHT: bar", true},
		{"CONCERNS: none", true},
		{"ROOT_SUMMARY:", true},
		{"INSIGHT:", true},
		{"CONCERNS:", true},
		{"just text", false},
		{"", false},
		{"ROOT_SUMMARYFOO:", false},
		{"INSIGHT_EXTRA:", false},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			got := isSILLabelLine(strings.ToUpper(tc.line))
			if got != tc.want {
				t.Errorf("isSILLabelLine(%q) = %v; want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestExtractSILLabel(t *testing.T) {
	t.Run("label present single line", func(t *testing.T) {
		text := "ROOT_SUMMARY: This is the summary.\nINSIGHT: The insight here."
		got := extractSILLabel(text, "ROOT_SUMMARY")
		if got != "This is the summary." {
			t.Errorf("got %q", got)
		}
	})

	t.Run("label missing returns empty", func(t *testing.T) {
		text := "INSIGHT: only insight"
		got := extractSILLabel(text, "ROOT_SUMMARY")
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("label with multi-word value stops at next label", func(t *testing.T) {
		text := "INSIGHT: First sentence here.\nCONCERNS: concern1, concern2"
		got := extractSILLabel(text, "INSIGHT")
		if got != "First sentence here." {
			t.Errorf("got %q", got)
		}
	})

	t.Run("case insensitive label detection", func(t *testing.T) {
		text := "root_summary: lowercase label test"
		got := extractSILLabel(text, "ROOT_SUMMARY")
		if got != "lowercase label test" {
			t.Errorf("got %q", got)
		}
	})
}

func TestParseSILResponse(t *testing.T) {
	t.Run("all three labels present", func(t *testing.T) {
		raw := "ROOT_SUMMARY: Manages the data pipeline.\nINSIGHT: Acts as a mediator layer.\nCONCERNS: high coupling, missing tests"
		rootSummary, insight, concerns := ParseSILResponse(raw)
		if rootSummary != "Manages the data pipeline." {
			t.Errorf("rootSummary = %q", rootSummary)
		}
		if insight != "Acts as a mediator layer." {
			t.Errorf("insight = %q", insight)
		}
		if len(concerns) != 2 {
			t.Fatalf("len(concerns) = %d; want 2", len(concerns))
		}
		if concerns[0] != "high coupling" {
			t.Errorf("concerns[0] = %q", concerns[0])
		}
		if concerns[1] != "missing tests" {
			t.Errorf("concerns[1] = %q", concerns[1])
		}
	})

	t.Run("CONCERNS: none yields nil slice", func(t *testing.T) {
		raw := "ROOT_SUMMARY: Root node.\nINSIGHT: Key insight.\nCONCERNS: none"
		_, _, concerns := ParseSILResponse(raw)
		if concerns != nil {
			t.Errorf("expected nil concerns, got %v", concerns)
		}
	})

	t.Run("CONCERNS: NONE case insensitive yields nil slice", func(t *testing.T) {
		raw := "ROOT_SUMMARY: Root node.\nINSIGHT: Key insight.\nCONCERNS: NONE"
		_, _, concerns := ParseSILResponse(raw)
		if concerns != nil {
			t.Errorf("expected nil concerns, got %v", concerns)
		}
	})

	t.Run("only ROOT_SUMMARY present", func(t *testing.T) {
		raw := "ROOT_SUMMARY: Just a summary."
		rootSummary, insight, concerns := ParseSILResponse(raw)
		if rootSummary != "Just a summary." {
			t.Errorf("rootSummary = %q", rootSummary)
		}
		if insight != "" {
			t.Errorf("insight should be empty, got %q", insight)
		}
		if concerns != nil {
			t.Errorf("concerns should be nil, got %v", concerns)
		}
	})

	t.Run("only INSIGHT present", func(t *testing.T) {
		raw := "INSIGHT: Just an insight."
		rootSummary, insight, concerns := ParseSILResponse(raw)
		if rootSummary != "" {
			t.Errorf("rootSummary should be empty, got %q", rootSummary)
		}
		if insight != "Just an insight." {
			t.Errorf("insight = %q", insight)
		}
		if concerns != nil {
			t.Errorf("concerns should be nil, got %v", concerns)
		}
	})

	t.Run("empty string returns all empty", func(t *testing.T) {
		rootSummary, insight, concerns := ParseSILResponse("")
		if rootSummary != "" || insight != "" || concerns != nil {
			t.Errorf("expected all empty, got rootSummary=%q insight=%q concerns=%v",
				rootSummary, insight, concerns)
		}
	})

	t.Run("no labels returns all empty", func(t *testing.T) {
		raw := "This is plain text with no SIL labels at all."
		rootSummary, insight, concerns := ParseSILResponse(raw)
		if rootSummary != "" || insight != "" || concerns != nil {
			t.Errorf("expected all empty for unlabeled text, got rootSummary=%q insight=%q concerns=%v",
				rootSummary, insight, concerns)
		}
	})

	t.Run("with think block prefix", func(t *testing.T) {
		raw := "<think>I need to analyze this carefully.</think>\nROOT_SUMMARY: Clean entry point.\nINSIGHT: Coordinates startup."
		rootSummary, insight, _ := ParseSILResponse(raw)
		if rootSummary != "Clean entry point." {
			t.Errorf("rootSummary = %q", rootSummary)
		}
		if insight != "Coordinates startup." {
			t.Errorf("insight = %q", insight)
		}
	})

	t.Run("only think block returns empty", func(t *testing.T) {
		raw := "<think>all internal</think>"
		rootSummary, insight, concerns := ParseSILResponse(raw)
		if rootSummary != "" || insight != "" || concerns != nil {
			t.Errorf("expected all empty after stripping think, got rootSummary=%q insight=%q concerns=%v",
				rootSummary, insight, concerns)
		}
	})

	t.Run("concerns with comma-separated items", func(t *testing.T) {
		raw := "INSIGHT: Important.\nCONCERNS: no error handling, untested edge cases, circular dependency"
		_, _, concerns := ParseSILResponse(raw)
		if len(concerns) != 3 {
			t.Fatalf("len(concerns) = %d; want 3", len(concerns))
		}
		if concerns[2] != "circular dependency" {
			t.Errorf("concerns[2] = %q", concerns[2])
		}
	})

	t.Run("whitespace-only string returns empty", func(t *testing.T) {
		rootSummary, insight, concerns := ParseSILResponse("   \n\t  ")
		if rootSummary != "" || insight != "" || concerns != nil {
			t.Errorf("expected all empty for whitespace input")
		}
	})
}

// ============================================================
// hardware.go — gpuLayersFromEnv (and smoke tests for others)
// ============================================================

func TestGpuLayersFromEnv(t *testing.T) {
	t.Run("env set to 42 returns 42", func(t *testing.T) {
		t.Setenv("SYNAPSES_GPU_LAYERS", "42")
		got := gpuLayersFromEnv(0)
		if got != 42 {
			t.Errorf("gpuLayersFromEnv(0) = %d; want 42", got)
		}
	})

	t.Run("env set to 0 returns 0", func(t *testing.T) {
		t.Setenv("SYNAPSES_GPU_LAYERS", "0")
		got := gpuLayersFromEnv(99)
		if got != 0 {
			t.Errorf("gpuLayersFromEnv(99) with env=0 = %d; want 0", got)
		}
	})

	t.Run("env unset returns defaultVal", func(t *testing.T) {
		os.Unsetenv("SYNAPSES_GPU_LAYERS")
		got := gpuLayersFromEnv(7)
		if got != 7 {
			t.Errorf("gpuLayersFromEnv(7) with no env = %d; want 7", got)
		}
	})

	t.Run("env set to invalid value returns defaultVal", func(t *testing.T) {
		t.Setenv("SYNAPSES_GPU_LAYERS", "not-a-number")
		got := gpuLayersFromEnv(5)
		if got != 5 {
			t.Errorf("gpuLayersFromEnv(5) with invalid env = %d; want 5", got)
		}
	})

	t.Run("env set to negative value is returned as-is", func(t *testing.T) {
		t.Setenv("SYNAPSES_GPU_LAYERS", "-1")
		got := gpuLayersFromEnv(10)
		if got != -1 {
			t.Errorf("gpuLayersFromEnv(10) with env=-1 = %d; want -1", got)
		}
	})
}

func TestDetectHardware_Smoke(t *testing.T) {
	// Smoke test: DetectHardware should not panic and should return a valid struct.
	cfg := DetectHardware()
	// GPULayers must be >= 0 if we have a GPU, or 0 for CPU-only.
	if cfg.GPULayers < 0 {
		t.Errorf("GPULayers should be >= 0, got %d", cfg.GPULayers)
	}
	// AvailableRAMGB should be >= 0.
	if cfg.AvailableRAMGB < 0 {
		t.Errorf("AvailableRAMGB should be >= 0, got %f", cfg.AvailableRAMGB)
	}
	// HasMetal and HasCUDA are mutually exclusive.
	if cfg.HasMetal && cfg.HasCUDA {
		t.Error("HasMetal and HasCUDA should not both be true")
	}
}

func TestIsAppleSilicon_Smoke(t *testing.T) {
	// Should not panic; result is platform-dependent.
	_ = isAppleSilicon()
}

func TestHasCUDA_Smoke(t *testing.T) {
	// Should not panic; result depends on nvidia-smi presence.
	_ = hasCUDA()
}

func TestAvailableRAMGB_Smoke(t *testing.T) {
	// Should not panic and return a non-negative value.
	ram := availableRAMGB()
	if ram < 0 {
		t.Errorf("availableRAMGB() = %f; want >= 0", ram)
	}
}

func TestDetectNvidiaVRAMGB_Smoke(t *testing.T) {
	// Should not panic; returns 0 if nvidia-smi is not present.
	vram := detectNvidiaVRAMGB()
	if vram < 0 {
		t.Errorf("detectNvidiaVRAMGB() = %f; want >= 0", vram)
	}
}

// ============================================================
// mock.go — MockClient
// ============================================================

func TestMockClient_Generate(t *testing.T) {
	t.Run("returns configured response", func(t *testing.T) {
		mc := NewMockClient("expected response")
		got, err := mc.Generate(context.Background(), "any prompt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "expected response" {
			t.Errorf("got %q; want %q", got, "expected response")
		}
	})

	t.Run("returns configured error", func(t *testing.T) {
		mc := NewMockClient("")
		mc.Err = errors.New("mock error")
		_, err := mc.Generate(context.Background(), "any prompt")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if err.Error() != "mock error" {
			t.Errorf("got error %q; want %q", err.Error(), "mock error")
		}
	})
}

func TestMockClient_Available(t *testing.T) {
	t.Run("available client returns true", func(t *testing.T) {
		mc := NewMockClient("resp")
		if !mc.Available(context.Background()) {
			t.Error("expected Available() = true")
		}
	})

	t.Run("unavailable client returns false", func(t *testing.T) {
		mc := NewUnavailableMockClient()
		if mc.Available(context.Background()) {
			t.Error("expected Available() = false")
		}
	})
}

func TestMockClient_ModelName(t *testing.T) {
	mc := NewMockClient("resp")
	if mc.ModelName() != "mock:test" {
		t.Errorf("ModelName() = %q; want %q", mc.ModelName(), "mock:test")
	}
}

func TestMockClient_ModelPulled(t *testing.T) {
	t.Run("available client reports model pulled", func(t *testing.T) {
		mc := NewMockClient("resp")
		if !mc.ModelPulled(context.Background()) {
			t.Error("expected ModelPulled() = true")
		}
	})

	t.Run("unavailable client reports model not pulled", func(t *testing.T) {
		mc := NewUnavailableMockClient()
		if mc.ModelPulled(context.Background()) {
			t.Error("expected ModelPulled() = false")
		}
	})
}

func TestMockClient_PullModel(t *testing.T) {
	mc := NewMockClient("resp")
	var buf bytes.Buffer
	if err := mc.PullModel(context.Background(), &buf); err != nil {
		t.Errorf("PullModel() unexpected error: %v", err)
	}
}

func TestMockClient_ImplementsLLMClient(t *testing.T) {
	// Compile-time interface check via type assertion at runtime.
	var _ LLMClient = NewMockClient("test")
	var _ LLMClient = NewUnavailableMockClient()
}

// ============================================================
// ollama.go — OllamaClient via httptest
// ============================================================

// ollamaSuccessResponse builds a valid ollamaResponse JSON body.
func ollamaSuccessBody(response string) string {
	b, _ := json.Marshal(ollamaResponse{Response: response, Done: true})
	return string(b)
}

func TestNewOllamaClient(t *testing.T) {
	t.Run("default timeout applied when zero", func(t *testing.T) {
		c := NewOllamaClient("http://localhost:11434", "llama3", 0)
		if c.httpClient.Timeout != 3000*time.Millisecond {
			t.Errorf("expected 3000ms timeout, got %v", c.httpClient.Timeout)
		}
	})

	t.Run("custom timeout applied", func(t *testing.T) {
		c := NewOllamaClient("http://localhost:11434", "llama3", 5000)
		if c.httpClient.Timeout != 5000*time.Millisecond {
			t.Errorf("expected 5000ms timeout, got %v", c.httpClient.Timeout)
		}
	})

	t.Run("trailing slash stripped from baseURL", func(t *testing.T) {
		c := NewOllamaClient("http://localhost:11434/", "m", 1000)
		if c.baseURL != "http://localhost:11434" {
			t.Errorf("baseURL = %q; want %q", c.baseURL, "http://localhost:11434")
		}
	})
}

func TestOllamaClient_WithThinking(t *testing.T) {
	c := NewOllamaClient("http://localhost:11434", "qwen3:latest", 1000)
	c2 := c.WithThinking(true)
	if c2 != c {
		t.Error("WithThinking should return the same client for chaining")
	}
	if !c.think {
		t.Error("WithThinking(true) should set think=true")
	}
	c.WithThinking(false)
	if c.think {
		t.Error("WithThinking(false) should set think=false")
	}
}

func TestOllamaClient_ModelName(t *testing.T) {
	c := NewOllamaClient("http://localhost:11434", "mymodel:v1", 1000)
	if c.ModelName() != "mymodel:v1" {
		t.Errorf("ModelName() = %q; want %q", c.ModelName(), "mymodel:v1")
	}
}

func TestOllamaClient_Generate_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaSuccessBody("hello from ollama"))
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	got, err := c.Generate(context.Background(), "say hello")
	if err != nil {
		t.Fatalf("Generate() unexpected error: %v", err)
	}
	if got != "hello from ollama" {
		t.Errorf("Generate() = %q; want %q", got, "hello from ollama")
	}
}

func TestOllamaClient_Generate_StripThinkBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaSuccessBody("<think>internal reasoning</think>clean answer"))
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "qwen3:latest", 5000)
	got, err := c.Generate(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "clean answer" {
		t.Errorf("expected think blocks stripped, got %q", got)
	}
}

func TestOllamaClient_Generate_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_, err := c.Generate(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention HTTP 500, got %q", err.Error())
	}
}

func TestOllamaClient_Generate_JSONDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "not valid json{{{{")
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_, err := c.Generate(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error should mention decode, got %q", err.Error())
	}
}

func TestOllamaClient_Generate_ErrorInResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(ollamaResponse{Error: "model not found"})
		fmt.Fprint(w, string(b))
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_, err := c.Generate(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error when response body contains error field")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error should contain 'model not found', got %q", err.Error())
	}
}

func TestOllamaClient_Generate_Qwen3ThinkField(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = readAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaSuccessBody("ok"))
	}))
	defer srv.Close()

	t.Run("qwen3 model with think=true sends think field", func(t *testing.T) {
		capturedBody = nil
		c := NewOllamaClient(srv.URL, "qwen3:latest", 5000).WithThinking(true)
		_, _ = c.Generate(context.Background(), "prompt")
		var req ollamaRequest
		if err := json.Unmarshal(capturedBody, &req); err != nil {
			t.Fatalf("failed to unmarshal request: %v", err)
		}
		if req.Think == nil {
			t.Error("expected think field to be set for qwen3 model")
		} else if !*req.Think {
			t.Error("expected think=true for qwen3 model with thinking enabled")
		}
	})

	t.Run("non-qwen3 model does not send think field", func(t *testing.T) {
		capturedBody = nil
		c := NewOllamaClient(srv.URL, "llama3", 5000)
		_, _ = c.Generate(context.Background(), "prompt")
		var req ollamaRequest
		if err := json.Unmarshal(capturedBody, &req); err != nil {
			t.Fatalf("failed to unmarshal request: %v", err)
		}
		if req.Think != nil {
			t.Errorf("expected think field to be nil for non-qwen3 model, got %v", *req.Think)
		}
	})
}

func TestOllamaClient_Available_True(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"models":[]}`)
		}
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	if !c.Available(context.Background()) {
		t.Error("expected Available() = true when server returns 200")
	}
}

func TestOllamaClient_Available_False(t *testing.T) {
	// Point to a server that immediately refuses connections.
	c := NewOllamaClient("http://127.0.0.1:1", "llama3", 500)
	if c.Available(context.Background()) {
		t.Error("expected Available() = false when server is unreachable")
	}
}

func TestOllamaClient_Available_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	if c.Available(context.Background()) {
		t.Error("expected Available() = false for non-200 status")
	}
}

func TestOllamaClient_ModelPulled_InList(t *testing.T) {
	// Model matches with :latest suffix appended by Ollama.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"name":"llama3:latest"},{"name":"mistral:7b"}]}`)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	if !c.ModelPulled(context.Background()) {
		t.Error("expected ModelPulled() = true when model is in list with :latest suffix")
	}
}

func TestOllamaClient_ModelPulled_ExactMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"name":"llama3:8b"}]}`)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3:8b", 5000)
	if !c.ModelPulled(context.Background()) {
		t.Error("expected ModelPulled() = true for exact name match")
	}
}

func TestOllamaClient_ModelPulled_NotInList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"name":"mistral:7b"}]}`)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	if c.ModelPulled(context.Background()) {
		t.Error("expected ModelPulled() = false when model is not in list")
	}
}

func TestOllamaClient_ModelPulled_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	if c.ModelPulled(context.Background()) {
		t.Error("expected ModelPulled() = false on HTTP error")
	}
}

func TestOllamaClient_ModelPulled_EmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[]}`)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	if c.ModelPulled(context.Background()) {
		t.Error("expected ModelPulled() = false for empty model list")
	}
}

func TestOllamaClient_PullModel_Success(t *testing.T) {
	// Stream two NDJSON events followed by EOF.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"pulling manifest"}`)
		fmt.Fprintln(w, `{"status":"downloading","total":1000,"completed":1000}`)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	var buf bytes.Buffer
	if err := c.PullModel(context.Background(), &buf); err != nil {
		t.Fatalf("PullModel() unexpected error: %v", err)
	}
}

func TestOllamaClient_PullModel_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	var buf bytes.Buffer
	err := c.PullModel(context.Background(), &buf)
	if err == nil {
		t.Fatal("expected error for non-200 pull response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should mention 403, got %q", err.Error())
	}
}

func TestOllamaClient_PullModel_EventError(t *testing.T) {
	// Server streams a pull error event.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"error":"access denied"}`)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	var buf bytes.Buffer
	err := c.PullModel(context.Background(), &buf)
	if err == nil {
		t.Fatal("expected error from pull event error field")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error should mention 'access denied', got %q", err.Error())
	}
}

func TestOllamaClient_ProbeLatency_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ollamaSuccessBody("ready"))
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	dur, err := c.ProbeLatency(context.Background(), 10*time.Second)
	if err != nil {
		t.Fatalf("ProbeLatency() unexpected error: %v", err)
	}
	if dur <= 0 {
		t.Errorf("ProbeLatency() returned non-positive duration: %v", dur)
	}
}

func TestOllamaClient_ProbeLatency_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_, err := c.ProbeLatency(context.Background(), 10*time.Second)
	if err == nil {
		t.Fatal("expected error for non-200 probe response")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should mention 502, got %q", err.Error())
	}
}

func TestOllamaClient_ProbeLatency_BodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(ollamaResponse{Error: "context length exceeded"})
		fmt.Fprint(w, string(b))
	}))
	defer srv.Close()

	c := NewOllamaClient(srv.URL, "llama3", 5000)
	_, err := c.ProbeLatency(context.Background(), 10*time.Second)
	if err == nil {
		t.Fatal("expected error for response body error field")
	}
	if !strings.Contains(err.Error(), "context length exceeded") {
		t.Errorf("error should mention probe error, got %q", err.Error())
	}
}

func TestListInstalledModels_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"name":"llama3:latest"},{"name":"mistral:7b"},{"name":"qwen3:4b"}]}`)
	}))
	defer srv.Close()

	names, err := ListInstalledModels(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("ListInstalledModels() unexpected error: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 models, got %d", len(names))
	}
	if names[0] != "llama3:latest" {
		t.Errorf("names[0] = %q; want %q", names[0], "llama3:latest")
	}
	if names[2] != "qwen3:4b" {
		t.Errorf("names[2] = %q; want %q", names[2], "qwen3:4b")
	}
}

func TestListInstalledModels_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[]}`)
	}))
	defer srv.Close()

	names, err := ListInstalledModels(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty list, got %v", names)
	}
}

func TestListInstalledModels_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := ListInstalledModels(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404, got %q", err.Error())
	}
}

func TestListInstalledModels_TrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"name":"phi3:mini"}]}`)
	}))
	defer srv.Close()

	// Pass URL with trailing slash — should be stripped.
	names, err := ListInstalledModels(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 1 || names[0] != "phi3:mini" {
		t.Errorf("unexpected names: %v", names)
	}
}

// ============================================================
// download.go — DownloadConfig methods, GGUFExists
// ============================================================

func TestDownloadConfig_DestPath(t *testing.T) {
	cfg := DownloadConfig{
		DestDir:  "/home/user/.synapses/models",
		Filename: "sil-coder-Q5_K_M.gguf",
	}
	want := filepath.Join("/home/user/.synapses/models", "sil-coder-Q5_K_M.gguf")
	got := cfg.DestPath()
	if got != want {
		t.Errorf("DestPath() = %q; want %q", got, want)
	}
}

func TestDownloadConfig_URL(t *testing.T) {
	cfg := DownloadConfig{
		Repo:     "divish/sil-coder",
		Filename: "sil-coder-Q5_K_M.gguf",
	}
	want := "https://huggingface.co/divish/sil-coder/resolve/main/sil-coder-Q5_K_M.gguf"
	got := cfg.URL()
	if got != want {
		t.Errorf("URL() = %q; want %q", got, want)
	}
}

func TestGGUFExists_ExistingFile(t *testing.T) {
	// Create a temporary file with content to simulate an existing GGUF.
	dir := t.TempDir()
	path := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(path, []byte("fake gguf content"), 0o644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if !GGUFExists(path) {
		t.Errorf("GGUFExists(%q) = false; want true for existing non-empty file", path)
	}
}

func TestGGUFExists_EmptyFile(t *testing.T) {
	// An empty file should not count as existing GGUF (size must be > 0).
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.gguf")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}
	if GGUFExists(path) {
		t.Errorf("GGUFExists(%q) = true; want false for empty file", path)
	}
}

func TestGGUFExists_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.gguf")
	if GGUFExists(path) {
		t.Errorf("GGUFExists(%q) = true; want false for missing file", path)
	}
}

// ============================================================
// LocalClient tests (no real model needed — tests error/no-op paths)
// ============================================================

func TestGgufModelName_ExtractsNameWithoutExtension(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/models/sil-coder-Q5_K_M.gguf", "sil-coder-Q5_K_M"},
		{"model.gguf", "model"},
		{"/a/b/c/my-model.gguf", "my-model"},
	}
	for _, tc := range cases {
		got := ggufModelName(tc.path)
		if got != tc.want {
			t.Errorf("ggufModelName(%q) = %q; want %q", tc.path, got, tc.want)
		}
	}
}

func TestNewLocalClient_InsufficientRAM(t *testing.T) {
	hw := HardwareConfig{AvailableRAMGB: 0.5} // below minRAMGB=3.0
	_, err := NewLocalClient("/nonexistent/model.gguf", hw)
	if err == nil {
		t.Fatal("expected error for insufficient RAM, got nil")
	}
	if !strings.Contains(err.Error(), "insufficient RAM") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestNewLocalClient_LoadFails_ReturnError(t *testing.T) {
	// Enough RAM, but model file doesn't exist → loadModel fails.
	hw := HardwareConfig{AvailableRAMGB: 16}
	_, err := NewLocalClient("/nonexistent/model.gguf", hw)
	if err == nil {
		t.Fatal("expected error when model file does not exist")
	}
}

func TestLocalClient_UnavailableClient_Generate(t *testing.T) {
	// Directly construct an unavailable client (model is nil).
	c := &LocalClient{available: false, modelName: "test"}
	_, err := c.Generate(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error from unavailable client, got nil")
	}
}

func TestLocalClient_Available_False(t *testing.T) {
	c := &LocalClient{available: false}
	if c.Available(context.Background()) {
		t.Error("expected Available=false for unloaded client")
	}
}

func TestLocalClient_ModelName(t *testing.T) {
	c := &LocalClient{modelName: "sil-coder"}
	if c.ModelName() != "sil-coder" {
		t.Errorf("expected sil-coder, got %q", c.ModelName())
	}
}

func TestLocalClient_ModelPulled_AlwaysTrue(t *testing.T) {
	c := &LocalClient{}
	if !c.ModelPulled(context.Background()) {
		t.Error("ModelPulled should always return true for local files")
	}
}

func TestLocalClient_PullModel_NoOp(t *testing.T) {
	c := &LocalClient{}
	if err := c.PullModel(context.Background(), nil); err != nil {
		t.Errorf("PullModel should be a no-op, got error: %v", err)
	}
}

func TestLocalClient_WithThinking(t *testing.T) {
	c := &LocalClient{}
	returned := c.WithThinking(true)
	if returned != c {
		t.Error("WithThinking should return the same client")
	}
	if !c.think {
		t.Error("expected think=true after WithThinking(true)")
	}
	c.WithThinking(false)
	if c.think {
		t.Error("expected think=false after WithThinking(false)")
	}
}

// ============================================================
// llm/download.go utility tests (humanBytes, logf)
// ============================================================

func TestHumanBytes_KB(t *testing.T) {
	got := humanBytes(512)
	if !strings.Contains(got, "KB") {
		t.Errorf("expected KB for 512 bytes, got %q", got)
	}
}

func TestHumanBytes_MB(t *testing.T) {
	got := humanBytes(2 * 1024 * 1024)
	if !strings.Contains(got, "MB") {
		t.Errorf("expected MB for 2MB, got %q", got)
	}
}

func TestHumanBytes_GB(t *testing.T) {
	got := humanBytes(2 * 1024 * 1024 * 1024)
	if !strings.Contains(got, "GB") {
		t.Errorf("expected GB for 2GB, got %q", got)
	}
}

func TestLogf_NilWriter_NoPanic(t *testing.T) {
	// Should not panic
	logf(nil, "test %s", "message")
}

func TestLogf_Writer_WritesMessage(t *testing.T) {
	var buf bytes.Buffer
	logf(&buf, "hello %s", "world")
	got := buf.String()
	if !strings.Contains(got, "hello world") {
		t.Errorf("expected 'hello world' in output, got %q", got)
	}
}

func TestLogf_AppendsNewline(t *testing.T) {
	var buf bytes.Buffer
	logf(&buf, "no newline")
	got := buf.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got %q", got)
	}
}

func TestProgressReader_Read_Progress(t *testing.T) {
	// Test progressReader.Read with a writer set (covers the Read method body)
	var buf bytes.Buffer
	data := make([]byte, 60*1024*1024) // 60 MB, enough to trigger print at 50 MB
	pr := &progressReader{
		r:     bytes.NewReader(data),
		w:     &buf,
		name:  "test",
		total: int64(len(data)),
	}
	tmp := make([]byte, 1024*1024) // read 1MB at a time
	for {
		_, err := pr.Read(tmp)
		if err != nil {
			break
		}
	}
	// Should have written at least one progress line
	if buf.Len() == 0 {
		t.Error("expected progress output, got none")
	}
}

// ============================================================
// Helpers
// ============================================================

// readAll is a test helper to read from an io.ReadCloser into a byte slice.
func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 512)
	for {
		n, err := r.Read(tmp)
		buf.Write(tmp[:n])
		if err != nil {
			break
		}
	}
	return buf.Bytes(), nil
}
