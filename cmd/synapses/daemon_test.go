package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── Helper functions ────────────────────────────────────────────────────────

// tempSynapsesHome sets up a temporary HOME directory for testing
func tempSynapsesHome(t *testing.T) (string, func()) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)

	// Create required .synapses subdirectories
	synapsesDir := filepath.Join(tmpDir, ".synapses")
	if err := os.MkdirAll(filepath.Join(synapsesDir, "pids"), 0o755); err != nil {
		t.Fatalf("failed to create pids dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(synapsesDir, "logs"), 0o755); err != nil {
		t.Fatalf("failed to create logs dir: %v", err)
	}

	cleanup := func() {
		os.Setenv("HOME", origHome)
	}
	return tmpDir, cleanup
}

// ── Tests for resolveSidecars ───────────────────────────────────────────────

func TestResolveSidecars_Empty(t *testing.T) {
	// Save original allSidecars
	origSidecars := allSidecars
	defer func() { allSidecars = origSidecars }()

	// Test with empty sidecar list
	allSidecars = []Sidecar{}
	result, err := resolveSidecars("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty sidecar list, got %d sidecars", len(result))
	}
}

func TestResolveSidecars_WithName(t *testing.T) {
	// Save original allSidecars
	origSidecars := allSidecars
	defer func() { allSidecars = origSidecars }()

	// Setup test sidecars
	allSidecars = []Sidecar{
		{Name: "brain", Binary: "synapses-brain", Args: []string{}, Port: "11435"},
		{Name: "pulse", Binary: "synapses-pulse", Args: []string{}, Port: "11436"},
	}

	// Test resolving specific sidecar
	result, err := resolveSidecars("brain")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 sidecar, got %d", len(result))
	}
	if result[0].Name != "brain" {
		t.Errorf("expected brain sidecar, got %s", result[0].Name)
	}
}

func TestResolveSidecars_NameNotFound(t *testing.T) {
	// Save original allSidecars
	origSidecars := allSidecars
	defer func() { allSidecars = origSidecars }()

	allSidecars = []Sidecar{
		{Name: "brain", Binary: "synapses-brain", Args: []string{}, Port: "11435"},
	}

	result, err := resolveSidecars("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent sidecar")
	}
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d sidecars", len(result))
	}
}

// ── Tests for PID management ────────────────────────────────────────────────

func TestWriteAndReadPID(t *testing.T) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer func() {
		os.Setenv("HOME", origHome)
	}()

	// Create required dirs - must match synapsesHome() structure
	os.MkdirAll(filepath.Join(tmpDir, ".synapses", "pids"), 0o755)

	serviceName := "test_service"
	pidValue := 12345

	// Write PID
	if err := writePID(serviceName, pidValue); err != nil {
		t.Fatalf("writePID failed: %v", err)
	}

	// Verify file was created
	pidPath := pidFilePath(serviceName)
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file not created: %v", err)
	}

	// Read PID back
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("readFile failed: %v", err)
	}

	// PID file format: "<pid>\n<start_unix_nanos>"
	lines := strings.SplitN(string(data), "\n", 2)
	if strings.TrimSpace(lines[0]) != "12345" {
		t.Errorf("expected PID 12345, got %s", lines[0])
	}
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
		t.Error("expected start timestamp on second line")
	}
}

func TestRemovePID(t *testing.T) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer func() {
		os.Setenv("HOME", origHome)
	}()

	// Create required dirs - must match synapsesHome() structure
	os.MkdirAll(filepath.Join(tmpDir, ".synapses", "pids"), 0o755)

	serviceName := "test_service"

	// Write PID first
	if err := writePID(serviceName, 12345); err != nil {
		t.Fatalf("writePID failed: %v", err)
	}

	pidPath := pidFilePath(serviceName)
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("PID file should exist before removal: %v", err)
	}

	// Remove PID
	removePID(serviceName)

	// Verify file was deleted
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("PID file should not exist after removal")
	}
}

// ── Tests for daemonStatus ──────────────────────────────────────────────────

func TestDaemonStatus_NoServices(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// Save and restore allSidecars
	origSidecars := allSidecars
	defer func() { allSidecars = origSidecars }()

	allSidecars = []Sidecar{}

	// Should not error with no services
	err := daemonStatus()
	if err != nil {
		t.Fatalf("daemonStatus failed: %v", err)
	}
}

func TestDaemonStatus_WithServices(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// Save and restore allSidecars
	origSidecars := allSidecars
	defer func() { allSidecars = origSidecars }()

	allSidecars = []Sidecar{
		{Name: "brain", Binary: "test_brain", Args: []string{}, Port: "11435"},
	}

	// Write a fake PID to simulate running service
	writePID("brain", 99999)

	err := daemonStatus()
	if err != nil {
		t.Fatalf("daemonStatus failed: %v", err)
	}
}

// ── Tests for daemonLogs ────────────────────────────────────────────────────

func TestDaemonLogs_NoLogFile(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// Try to read logs for a service that doesn't have a log file
	err := daemonLogs("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent log file")
	}
}

func TestDaemonLogs_WithContent(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// Create a log file with content
	logPath := logFilePath("daemon")

	// Ensure parent directory exists (should already be created by tempSynapsesHome)
	os.MkdirAll(filepath.Dir(logPath), 0o755)

	logContent := "test log line 1\ntest log line 2\n"
	if err := os.WriteFile(logPath, []byte(logContent), 0o644); err != nil {
		t.Fatalf("failed to create log file: %v", err)
	}

	// Read the logs
	err := daemonLogs("daemon")
	if err != nil {
		t.Fatalf("daemonLogs failed: %v", err)
	}
}

// ── Tests for daemonStart and daemonStop ────────────────────────────────────

func TestDaemonStart_EmptyServices(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// Start with no services configured
	err := daemonStart([]Sidecar{}, false)
	if err != nil {
		t.Fatalf("daemonStart failed: %v", err)
	}
}

func TestDaemonStop_EmptyServices(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// Stop with no services configured
	err := daemonStop([]Sidecar{}, false)
	if err != nil {
		t.Fatalf("daemonStop failed: %v", err)
	}
}

// ── Tests for cmdDaemon ─────────────────────────────────────────────────────

func TestCmdDaemon_NoArgs(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// Call with no args should print usage and return no error
	err := cmdDaemon([]string{})
	if err != nil {
		t.Fatalf("cmdDaemon with no args failed: %v", err)
	}
}

func TestCmdDaemon_RemovedSubcommands(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// status, start, stop were removed — they should return unknown subcommand errors.
	for _, sub := range []string{"status", "start", "stop", "restart", "wait"} {
		err := cmdDaemon([]string{sub})
		if err == nil {
			t.Errorf("cmdDaemon %s should return error (removed subcommand)", sub)
		}
	}
}

func TestCmdDaemon_InvalidCommand(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// Save and restore allSidecars
	origSidecars := allSidecars
	defer func() { allSidecars = origSidecars }()

	allSidecars = []Sidecar{}

	err := cmdDaemon([]string{"invalid"})
	if err == nil {
		t.Error("expected error for invalid command")
	}
}

func TestCmdDaemon_LogsWithServiceFlag(t *testing.T) {
	_, cleanup := tempSynapsesHome(t)
	defer cleanup()

	// logs with --service flag should not panic (may error if no log file).
	_ = cmdDaemon([]string{"logs", "--service", "brain"})
}
