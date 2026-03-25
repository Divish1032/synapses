package embed

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// hashFileSHA256
// ---------------------------------------------------------------------------

func TestHashFileSHA256_KnownContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.bin")
	content := []byte("hello synapses")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := hashFileSHA256(path)
	if err != nil {
		t.Fatalf("hashFileSHA256: %v", err)
	}

	want := sha256.Sum256(content)
	wantHex := hex.EncodeToString(want[:])
	if got != wantHex {
		t.Errorf("hash mismatch: got %s, want %s", got, wantHex)
	}
}

func TestHashFileSHA256_MissingFile(t *testing.T) {
	_, err := hashFileSHA256("/nonexistent/path/file.bin")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// ---------------------------------------------------------------------------
// writeSHA256Sidecar
// ---------------------------------------------------------------------------

func TestWriteSHA256Sidecar_WritesCorrectHash(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "llama-server")
	content := []byte("fake binary content for test")
	if err := os.WriteFile(binPath, content, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	if err := writeSHA256Sidecar(binPath); err != nil {
		t.Fatalf("writeSHA256Sidecar: %v", err)
	}

	// Sidecar must exist.
	sidecarPath := binPath + ".sha256"
	sidecarBytes, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}

	// Sidecar content must be the correct 64-char hex digest.
	got := strings.TrimSpace(string(sidecarBytes))
	if len(got) != 64 {
		t.Errorf("sidecar should be 64 hex chars, got %d: %q", len(got), got)
	}
	want := sha256.Sum256(content)
	wantHex := hex.EncodeToString(want[:])
	if got != wantHex {
		t.Errorf("sidecar hash mismatch: got %s, want %s", got, wantHex)
	}
}

func TestWriteSHA256Sidecar_NoTmpFileLeftOnSuccess(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(binPath, []byte("content"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := writeSHA256Sidecar(binPath); err != nil {
		t.Fatalf("writeSHA256Sidecar: %v", err)
	}

	// The temp file must not be left behind.
	tmpPath := binPath + ".sha256.tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("temp sidecar file was left behind after successful write")
	}
}

// ---------------------------------------------------------------------------
// verifyBinarySidecar
// ---------------------------------------------------------------------------

func TestVerifyBinarySidecar_NoSidecar_SkipsCheck(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(binPath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	// No sidecar file — must not return an error (backwards compat).
	if err := verifyBinarySidecar(binPath); err != nil {
		t.Errorf("expected no error when sidecar absent, got: %v", err)
	}
}

func TestVerifyBinarySidecar_MatchingHash_Passes(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "llama-server")
	content := []byte("trusted binary")
	if err := os.WriteFile(binPath, content, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	// Write the correct sidecar.
	if err := writeSHA256Sidecar(binPath); err != nil {
		t.Fatalf("writeSHA256Sidecar: %v", err)
	}

	if err := verifyBinarySidecar(binPath); err != nil {
		t.Errorf("expected no error for matching hash, got: %v", err)
	}
}

func TestVerifyBinarySidecar_Corrupted_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(binPath, []byte("original content"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	// Write the sidecar for the original content.
	if err := writeSHA256Sidecar(binPath); err != nil {
		t.Fatalf("writeSHA256Sidecar: %v", err)
	}

	// Simulate post-installation corruption.
	if err := os.WriteFile(binPath, []byte("corrupted content!"), 0o755); err != nil {
		t.Fatalf("overwrite binary: %v", err)
	}

	err := verifyBinarySidecar(binPath)
	if err == nil {
		t.Fatal("expected error for corrupted binary, got nil")
	}
	// Error message must guide the user to the fix.
	if !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("error should mention 'corrupted', got: %v", err)
	}
	if !strings.Contains(err.Error(), "brain setup") {
		t.Errorf("error should mention 'brain setup', got: %v", err)
	}
}

func TestVerifyBinarySidecar_MalformedSidecar_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(binPath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	// Write a sidecar with invalid content (not 64 hex chars).
	sidecarPath := binPath + ".sha256"
	if err := os.WriteFile(sidecarPath, []byte("not-a-valid-sha256"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	err := verifyBinarySidecar(binPath)
	if err == nil {
		t.Fatal("expected error for malformed sidecar, got nil")
	}
	if !strings.Contains(err.Error(), "malformed") {
		t.Errorf("error should mention 'malformed', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Server.Start integration: binary corrupted scenario
// ---------------------------------------------------------------------------

func TestServerStart_CorruptedBinary_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "llama-server")
	modelPath := filepath.Join(dir, "model.gguf")

	// Create a plausible binary and model so the os.Stat checks pass.
	if err := os.WriteFile(binPath, []byte("original binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if err := os.WriteFile(modelPath, []byte("model data"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}

	// Write a valid sidecar for the original binary.
	if err := writeSHA256Sidecar(binPath); err != nil {
		t.Fatalf("writeSHA256Sidecar: %v", err)
	}

	// Corrupt the binary after the sidecar is written.
	if err := os.WriteFile(binPath, []byte("tampered!"), 0o755); err != nil {
		t.Fatalf("corrupt binary: %v", err)
	}

	s := New(modelPath, 19999, binPath)
	err := s.Start(t.Context())
	if err == nil {
		t.Fatal("Start should fail for corrupted binary")
	}
	if !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("error should mention 'corrupted', got: %v", err)
	}
}
