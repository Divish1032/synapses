//go:build llamacpp

package llm

// local_cgo.go — CGo implementation of LocalClient using godeps/gollama.
//
// Build requirements:
//   CGO_ENABLED=1
//   godeps/gollama in go.mod (add with: go get github.com/godeps/gollama)
//
// Platform notes:
//   macOS (Apple Silicon): Requires Xcode command-line tools for Metal.
//   Linux (NVIDIA):        Requires CUDA toolkit for CuBLAS acceleration.
//   Linux/Windows (CPU):   Uses AVX-512/AVX2 SIMD automatically via llama.cpp.
//
// godeps/gollama is the maintained fork of the abandoned go-skynet/go-llama.cpp.
// The llama.cpp library is embedded as a C/C++ submodule — no separate
// installation is needed beyond the Go dependency.
//
// API has three levels:
//   1. llama.LoadModel(path, ModelOptions...)   → *llama.Model
//   2. model.NewContext(ContextOptions...)       → *llama.Context
//   3. ctx.Generate(prompt, GenerateOptions...) → string, error

import (
	"context"
	"fmt"
	"strings"

	llama "github.com/godeps/gollama"
)

// silSystemPrompt is the system instruction baked into every SIL inference call.
// MUST match SYSTEM_PROMPT in synapses-fine-distilling/grpo_train.py exactly —
// the model was trained with this exact system prompt.
const silSystemPrompt = `You are SIL (Synapses Intelligence Layer), a specialized code graph analyst embedded in a developer's IDE.

Given a code subgraph JSON with nodes (id, name, type, file, package, line) and edges (from, to, type), produce a structured Context Brief in EXACTLY this format — no deviations:

<think>
Analyze step by step:
1. Which node has the highest connectivity (most edges)? That is the Gravity Center.
2. What architectural pattern does the edge structure reveal (e.g. hub-and-spoke, pipeline, layered)?
3. Are there structural concerns: cycles, orphaned nodes, unusually high fan-in/fan-out?
</think>

ROOT_SUMMARY: <one sentence — what does the root node do? max 25 words>
INSIGHT: <one sentence — what architectural role does this subgraph play? max 25 words>
CONCERNS: <comma-separated concerns, or "none">

Rules:
- ROOT_SUMMARY and INSIGHT must each be a single sentence under 25 words.
- CONCERNS lists at most 3 items.
- If the graph has fewer than 2 edges, output CONCERNS: insufficient context — recommend get_impact
- Never invent node names not present in the input JSON.`

// applyQwen3ChatTemplate wraps systemPrompt and userMsg in the Qwen3/Qwen3.5
// chat template format required by the GGUF model. gollama's Generate API is a
// raw-completion API that does not apply the model's embedded chat template, so
// we format it manually here.
func applyQwen3ChatTemplate(systemPrompt, userMsg string) string {
	var b strings.Builder
	b.WriteString("<|im_start|>system\n")
	b.WriteString(systemPrompt)
	b.WriteString("\n<|im_end|>\n")
	b.WriteString("<|im_start|>user\n")
	b.WriteString(userMsg)
	b.WriteString("\n<|im_end|>\n")
	b.WriteString("<|im_start|>assistant\n")
	return b.String()
}

// loadModel loads the GGUF file and configures hardware acceleration.
// Called once by NewLocalClient.
func (c *LocalClient) loadModel() error {
	// --- Level 1: load model weights ---
	modelOpts := []llama.ModelOption{
		llama.WithMMap(true), // memory-map weights; reduces cold-start time
	}

	if c.hw.HasMetal || c.hw.HasCUDA {
		// Offload the configured number of transformer layers to the GPU.
		// Apple Silicon: GPULayers=99 (all layers, unified memory).
		// NVIDIA: auto-tuned by DetectHardware based on VRAM.
		modelOpts = append(modelOpts, llama.WithGPULayers(c.hw.GPULayers))
	}
	// CPU fallback: no GPU option; llama.cpp auto-detects AVX-512/AVX2.

	model, err := llama.LoadModel(c.modelPath, modelOpts...)
	if err != nil {
		c.available = false
		return err
	}
	c.model = model

	// --- Level 2: create inference context ---
	llamaCtx, err := model.NewContext(
		llama.WithContext(c.contextSize), // token context window size
	)
	if err != nil {
		c.available = false
		return fmt.Errorf("create inference context: %w", err)
	}
	c.llamaCtx = llamaCtx

	return nil
}

// generate runs a single inference call and returns the decoded text.
// Called under c.mu, so single-threaded access to the context is guaranteed.
func (c *LocalClient) generate(_ context.Context, prompt string) (string, error) {
	llamaCtx, ok := c.llamaCtx.(*llama.Context)
	if !ok || llamaCtx == nil {
		return "", fmt.Errorf("local LLM: inference context is nil")
	}

	// Apply Qwen3.5 chat template with the SIL system prompt.
	// The SIL model was trained with this system prompt; it must be present on
	// every inference call for the model to produce correct structured output.
	// gollama's Generate API is raw-completion only — we format the template here.
	fullPrompt := applyQwen3ChatTemplate(silSystemPrompt, prompt)

	// --- Level 3: generate ---
	result, err := llamaCtx.Generate(fullPrompt,
		llama.WithMaxTokens(512),   // match grpo_train max_completion_length
		llama.WithTemperature(0.1), // low temp for deterministic code graph analysis
		llama.WithTopP(0.9),
		llama.WithRepeatPenalty(1.1),
	)
	if err != nil {
		return "", fmt.Errorf("local LLM generate: %w", err)
	}

	return strings.TrimSpace(result), nil
}

