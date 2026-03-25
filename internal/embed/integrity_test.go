package embed

import (
	"os"
	"path/filepath"
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

func TestVerifyModelIntegrity_FP32_PinnedHash_IsNonEmpty(t *testing.T) {
	// Guard: builtinModelSHA256FP32 must remain non-empty so GPU users get
	// cryptographic verification rather than TOFU trust-on-first-use.
	if builtinModelSHA256FP32 == "" {
		t.Fatal("builtinModelSHA256FP32 is empty — GPU users will not get pinned-hash verification; pin the hash before shipping")
	}
	// Verify it looks like a hex-encoded SHA-256 (64 lowercase hex chars).
	if len(builtinModelSHA256FP32) != 64 {
		t.Fatalf("builtinModelSHA256FP32 length %d != 64 — must be a full SHA-256 hex digest", len(builtinModelSHA256FP32))
	}
	for _, c := range builtinModelSHA256FP32 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("builtinModelSHA256FP32 contains non-hex character %q", c)
		}
	}
}

func TestVerifyModelIntegrity_FP32_PinnedHash_CorruptFile_ReturnsError(t *testing.T) {
	// FP32 variant now has a pinned hash. Fake content must fail verification
	// and the corrupt file must be removed so the next Embed() re-downloads.
	dir := t.TempDir()
	onnxPath := filepath.Join(dir, builtinModelFileFP32)
	if err := os.WriteFile(onnxPath, []byte("not a real fp32 model"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyModelIntegrity(onnxPath, builtinModelFileFP32)
	if err == nil {
		t.Fatal("expected error for corrupt fp32 model, got nil")
	}

	// The corrupt file should have been removed.
	if _, statErr := os.Stat(onnxPath); !os.IsNotExist(statErr) {
		t.Errorf("corrupt fp32 model file should have been removed, but still exists")
	}
}

func TestVerifyModelIntegrity_FP32_PinnedHash_EmptyFile_ReturnsError(t *testing.T) {
	// An empty fp32 file must fail verification with the pinned hash.
	dir := t.TempDir()
	onnxPath := filepath.Join(dir, builtinModelFileFP32)
	if err := os.WriteFile(onnxPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyModelIntegrity(onnxPath, builtinModelFileFP32)
	if err == nil {
		t.Fatal("expected error for empty fp32 model, got nil")
	}

	// The empty file should have been removed.
	if _, statErr := os.Stat(onnxPath); !os.IsNotExist(statErr) {
		t.Errorf("empty fp32 model file should have been removed, but still exists")
	}
}
