package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDevLinkStateRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	state := &devLinkState{
		Linked:   true,
		Source:   "/usr/local/src/synapses/build/synapses",
		LinkedAt: "2026-03-29T10:00:00Z",
	}
	if err := writeDevLinkState(state); err != nil {
		t.Fatal(err)
	}

	loaded, err := readDevLinkState()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Linked {
		t.Error("Linked = false, want true")
	}
	if loaded.Source != state.Source {
		t.Errorf("Source = %q, want %q", loaded.Source, state.Source)
	}
}

func TestDevLinkStateNotExists(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	state, err := readDevLinkState()
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
	if state.Linked {
		t.Error("default should be not linked")
	}
}

func TestCmdDevLink(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Create a fake binary to link
	fakeBin := filepath.Join(tmp, "my-synapses")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\necho test"), 0o755)

	if err := cmdDevLink([]string{fakeBin}); err != nil {
		t.Fatal(err)
	}

	// Verify dev_link.json was created
	data, err := os.ReadFile(filepath.Join(tmp, ".synapses", "dev_link.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state devLinkState
	json.Unmarshal(data, &state)
	if !state.Linked {
		t.Error("Linked = false after link")
	}

	// Verify binary was copied
	dest := filepath.Join(tmp, ".synapses", "bin", "synapses")
	destData, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(destData) != "#!/bin/sh\necho test" {
		t.Error("binary content mismatch")
	}
}

func TestCmdDevUnlink(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	binDir := filepath.Join(tmp, ".synapses", "bin")
	os.MkdirAll(binDir, 0o755)

	// Create backup and current binary
	os.WriteFile(filepath.Join(binDir, "synapses.app-backup"), []byte("original"), 0o755)
	os.WriteFile(filepath.Join(binDir, "synapses"), []byte("custom"), 0o755)

	// Write link state
	writeDevLinkState(&devLinkState{Linked: true, Source: "/custom"})

	if err := cmdDevUnlink(nil); err != nil {
		t.Fatal(err)
	}

	// Verify binary was restored
	data, _ := os.ReadFile(filepath.Join(binDir, "synapses"))
	if string(data) != "original" {
		t.Errorf("binary = %q, want %q", string(data), "original")
	}

	// Verify backup was removed
	if _, err := os.Stat(filepath.Join(binDir, "synapses.app-backup")); !os.IsNotExist(err) {
		t.Error("backup should be removed after unlink")
	}

	// Verify state was cleared
	state, _ := readDevLinkState()
	if state.Linked {
		t.Error("should be unlinked")
	}
}

func TestCmdDevLink_MissingBinary(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	err := cmdDevLink([]string{"/nonexistent/binary"})
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestCmdDevLink_NoArgs(t *testing.T) {
	err := cmdDevLink(nil)
	if err == nil {
		t.Error("expected error for no args")
	}
}
