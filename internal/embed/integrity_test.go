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

func TestVerifyModelIntegrity_FP32_FailsClosed(t *testing.T) {
	// FP32 variant: hash not yet captured → fail-closed (refuse to use).
	// This forces fallback to the verified quantized variant in ensureModel().
	dir := t.TempDir()
	onnxPath := filepath.Join(dir, builtinModelFileFP32)
	if err := os.WriteFile(onnxPath, []byte("any content"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyModelIntegrity(onnxPath, builtinModelFileFP32)
	if err == nil {
		t.Fatal("expected error for fp32 variant with no hardcoded hash (fail-closed), got nil")
	}
	if !strings.Contains(err.Error(), "no expected hash") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
