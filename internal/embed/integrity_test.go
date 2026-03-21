package embed

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyModelIntegrity_CorruptFile_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	onnxPath := filepath.Join(dir, "model.onnx")

	// Write garbage data — hash won't match.
	if err := os.WriteFile(onnxPath, []byte("not a real onnx model"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyModelIntegrity(onnxPath)
	if err == nil {
		t.Fatal("expected error for corrupt model, got nil")
	}

	// The corrupt file should have been removed.
	if _, statErr := os.Stat(onnxPath); !os.IsNotExist(statErr) {
		t.Errorf("corrupt model file should have been removed, but still exists")
	}
}

func TestVerifyModelIntegrity_MissingFile_ReturnsError(t *testing.T) {
	err := verifyModelIntegrity("/nonexistent/path/model.onnx")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestVerifyModelIntegrity_EmptyFile_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	onnxPath := filepath.Join(dir, "model.onnx")

	// Empty file — hash won't match.
	if err := os.WriteFile(onnxPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	err := verifyModelIntegrity(onnxPath)
	if err == nil {
		t.Fatal("expected error for empty model, got nil")
	}

	// The empty file should have been removed.
	if _, statErr := os.Stat(onnxPath); !os.IsNotExist(statErr) {
		t.Errorf("corrupt model file should have been removed, but still exists")
	}
}
