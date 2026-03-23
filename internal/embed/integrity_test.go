package embed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyModelIntegrity_CorruptFile_ReturnsError(t *testing.T) {
	if builtinModelSHA256 == "" {
		t.Skip("builtinModelSHA256 is empty — integrity enforcement disabled")
	}

	dir := t.TempDir()
	onnxPath := filepath.Join(dir, builtinModelFileQuantized)

	if err := os.WriteFile(onnxPath, []byte("not a real onnx model"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyModelIntegrity(onnxPath, builtinModelFileQuantized)
	if err == nil {
		t.Fatal("expected error for corrupt model, got nil")
	}

	// The corrupt file should have been removed.
	if _, statErr := os.Stat(onnxPath); !os.IsNotExist(statErr) {
		t.Errorf("corrupt model file should have been removed, but still exists")
	}
}

func TestVerifyModelIntegrity_MissingFile_ReturnsError(t *testing.T) {
	err := verifyModelIntegrity("/nonexistent/path/"+builtinModelFileQuantized, builtinModelFileQuantized)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestVerifyModelIntegrity_EmptyFile_ReturnsError(t *testing.T) {
	if builtinModelSHA256 == "" {
		t.Skip("builtinModelSHA256 is empty — integrity enforcement disabled")
	}

	dir := t.TempDir()
	onnxPath := filepath.Join(dir, builtinModelFileQuantized)

	if err := os.WriteFile(onnxPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyModelIntegrity(onnxPath, builtinModelFileQuantized)
	if err == nil {
		t.Fatal("expected error for empty model, got nil")
	}

	if _, statErr := os.Stat(onnxPath); !os.IsNotExist(statErr) {
		t.Errorf("corrupt model file should have been removed, but still exists")
	}
}

func TestSelectOnnxVariant_ReturnsTwoStrings(t *testing.T) {
	// selectOnnxVariant should always return non-empty file and path.
	modelFile, repoPath := selectOnnxVariant()
	if modelFile == "" {
		t.Error("selectOnnxVariant returned empty modelFile")
	}
	if repoPath == "" {
		t.Error("selectOnnxVariant returned empty repoPath")
	}
	// One of the two known variants must be returned.
	if modelFile != builtinModelFileQuantized && modelFile != builtinModelFileFP32 {
		t.Errorf("unexpected modelFile %q", modelFile)
	}
}

func TestSafeModelEvent_PanicRecovery(t *testing.T) {
	e := &BuiltinEmbedder{
		modelsDir: t.TempDir(),
		done:      make(chan struct{}),
	}
	e.OnModelEvent = func(eventType string) {
		panic("test panic in model event callback")
	}
	// Should not panic — safeModelEvent recovers.
	e.safeModelEvent("test_event")
}

func TestVerifyModelIntegrity_FP32_TOFU_FirstUse(t *testing.T) {
	// FP32 variant with no hardcoded hash: first use should succeed (TOFU)
	// and create a sidecar .sha256 file for future verification.
	dir := t.TempDir()
	onnxPath := filepath.Join(dir, builtinModelFileFP32)
	if err := os.WriteFile(onnxPath, []byte("fp32 model content"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyModelIntegrity(onnxPath, builtinModelFileFP32)
	if err != nil {
		t.Fatalf("expected TOFU first-use to succeed, got: %v", err)
	}

	// Sidecar file should exist.
	sidecar := onnxPath + ".sha256"
	stored, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("sidecar file not created: %v", err)
	}
	if strings.TrimSpace(string(stored)) == "" {
		t.Fatal("sidecar file is empty")
	}
}

func TestVerifyModelIntegrity_FP32_TOFU_SubsequentLoad(t *testing.T) {
	// After TOFU stores the hash, subsequent loads verify against it.
	dir := t.TempDir()
	onnxPath := filepath.Join(dir, builtinModelFileFP32)
	content := []byte("fp32 model content v2")
	if err := os.WriteFile(onnxPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	// First call: stores hash via TOFU.
	if err := verifyModelIntegrity(onnxPath, builtinModelFileFP32); err != nil {
		t.Fatalf("TOFU first-use failed: %v", err)
	}

	// Second call with same content: should pass.
	if err := verifyModelIntegrity(onnxPath, builtinModelFileFP32); err != nil {
		t.Fatalf("TOFU verification of unchanged file failed: %v", err)
	}
}

func TestVerifyModelIntegrity_FP32_TOFU_TamperDetected(t *testing.T) {
	// If the model file changes after TOFU, verification should fail.
	dir := t.TempDir()
	onnxPath := filepath.Join(dir, builtinModelFileFP32)
	if err := os.WriteFile(onnxPath, []byte("original content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First call: stores hash.
	if err := verifyModelIntegrity(onnxPath, builtinModelFileFP32); err != nil {
		t.Fatalf("TOFU first-use failed: %v", err)
	}

	// Tamper with the file.
	if err := os.WriteFile(onnxPath, []byte("tampered content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second call: should detect tampering.
	err := verifyModelIntegrity(onnxPath, builtinModelFileFP32)
	if err == nil {
		t.Fatal("expected TOFU to detect tampering, got nil")
	}
	if !strings.Contains(err.Error(), "TOFU") {
		t.Fatalf("expected TOFU error, got: %v", err)
	}
}
