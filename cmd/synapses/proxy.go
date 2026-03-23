// proxy.go — thin MCP proxy for "synapses start".
//
// Instead of running the full MCP server in-process, this proxy:
//  1. Ensures the singleton daemon is running (starts it if not).
//  2. Registers the project with the daemon via HTTP API.
//  3. Connects to the project's per-project Unix socket.
//  4. Bridges stdin ↔ socket bidirectionally (zero protocol awareness).
//  5. Exits when stdin closes (client disconnects) — daemon stays alive.
//
// The proxy is stateless and disposable. Crash it, kill it, spawn 100 of them
// — the daemon is unaffected.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// cmdStartProxy is the new implementation of "synapses start". It ensures the
// singleton daemon is running, registers the project, then bridges stdio to
// the per-project Unix socket.
func cmdStartProxy(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	repoPath := fs.String("path", ".", "Path to the repository root")
	direct := fs.Bool("direct", false, "Run MCP server directly on stdio (legacy mode, no daemon)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Legacy mode: run the old direct-stdio server for debugging/testing.
	if *direct {
		return cmdStartDirect(args)
	}

	absPath, err := canonicalPath(*repoPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	// ── Auto-create synapses.json if no project marker exists ───────────────
	// The user explicitly ran "synapses start" in this directory — that is
	// clear intent to use it as a project root.  Create a minimal config so
	// the daemon's security check (isValidProjectPath) passes without
	// requiring a .git directory.
	if err := ensureProjectMarker(absPath); err != nil {
		return fmt.Errorf("ensure project marker: %w", err)
	}

	// ── Ensure singleton daemon is running ───────────────────────────────────
	if err := ensureSingletonDaemon(absPath); err != nil {
		return fmt.Errorf("ensure daemon: %w", err)
	}

	// ── Check for binary version mismatch ────────────────────────────────────
	if err := checkSingletonDaemonVersion(); err != nil {
		logutil.Error("synapses proxy: %v — restarting daemon\n", err)
		if err := restartSingletonDaemon(absPath); err != nil {
			return fmt.Errorf("restart daemon: %w", err)
		}
	}

	// ── Register project with daemon ─────────────────────────────────────────
	sockPath, err := registerProjectWithDaemon(absPath)
	if err != nil {
		return fmt.Errorf("register project: %w", err)
	}

	// ── Wait for per-project socket to appear ────────────────────────────────
	if err := waitForSocket(sockPath, 30*time.Second); err != nil {
		return fmt.Errorf("socket not ready: %w", err)
	}

	// ── Connect to per-project socket ─────────────────────────────────────────
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to daemon socket %s: %w", sockPath, err)
	}
	defer conn.Close()

	// ── Bridge stdin ↔ socket ────────────────────────────────────────────────
	stdinDone := make(chan struct{})
	sockDone := make(chan struct{})

	go func() {
		defer close(stdinDone)
		io.Copy(conn, os.Stdin) //nolint:errcheck
		if uc, ok := conn.(*net.UnixConn); ok {
			uc.CloseWrite() //nolint:errcheck
		}
	}()

	go func() {
		defer close(sockDone)
		io.Copy(os.Stdout, conn) //nolint:errcheck
	}()

	<-sockDone
	conn.Close()
	<-stdinDone
	return nil
}

// ensureSingletonDaemon checks if the singleton daemon is running and starts
// it if not. Uses the HTTP health endpoint as the source of truth.
func ensureSingletonDaemon(absPath string) error {
	if IsSingletonDaemonRunning() {
		return nil
	}

	cleanStaleSingletonPID()

	logutil.Info("synapses proxy: starting singleton daemon\n")

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own binary: %w", err)
	}

	logPath, err := singletonLogPath()
	if err != nil {
		return fmt.Errorf("log path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	rotateLogIfNeeded(logPath)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log %s: %w", logPath, err)
	}

	cmd := exec.Command(self, "daemon", "serve")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = detachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	logFile.Close()
	go cmd.Wait() //nolint:errcheck

	// Wait for daemon to be ready.
	deadline := time.Now().Add(120 * time.Second)
	backoff := 200 * time.Millisecond
	for time.Now().Before(deadline) {
		time.Sleep(backoff)
		if IsSingletonDaemonRunning() {
			logutil.Info("synapses proxy: daemon ready\n")
			return nil
		}
		// Check if daemon process died.
		pidPath, _ := singletonPIDPath()
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			// PID file gone — daemon exited (or another started and exited).
			// Try one more health check (race: another proxy started it).
			if IsSingletonDaemonRunning() {
				return nil
			}
			// Report log tail.
			logData, _ := os.ReadFile(logPath)
			if len(logData) > 0 {
				lines := strings.Split(string(logData), "\n")
				tail := lines
				if len(tail) > 10 {
					tail = tail[len(tail)-10:]
				}
				return fmt.Errorf("daemon failed to start:\n%s", strings.Join(tail, "\n"))
			}
			return fmt.Errorf("daemon failed to start (no log output)")
		}
		if backoff < 2*time.Second {
			backoff = backoff * 3 / 2
		}
	}
	return fmt.Errorf("timed out waiting for daemon at %s (120s)", DaemonHTTPAddr)
}

// fetchDaemonCSRFToken fetches the per-session CSRF token from the daemon.
// The /api/admin/csrf-token endpoint is GET-only and loopback-trusted.
func fetchDaemonCSRFToken(client *http.Client) (string, error) {
	resp, err := client.Get("http://" + DaemonHTTPAddr + "/api/admin/csrf-token")
	if err != nil {
		return "", fmt.Errorf("fetch csrf token: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode csrf token: %w", err)
	}
	return result.Token, nil
}

// registerProjectWithDaemon calls POST /api/admin/projects and returns the
// per-project Unix socket path.
func registerProjectWithDaemon(absPath string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	csrfToken, err := fetchDaemonCSRFToken(client)
	if err != nil {
		return "", fmt.Errorf("register project: %w", err)
	}

	body, _ := json.Marshal(map[string]string{"path": absPath})
	req, err := http.NewRequest(http.MethodPost, "http://"+DaemonHTTPAddr+"/api/admin/projects", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("register project: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrfToken)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("register project: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result struct {
		Socket string `json:"socket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Socket == "" {
		// Fallback: derive socket path locally.
		sockPath, err := daemonSocketPath(absPath)
		if err != nil {
			return "", fmt.Errorf("socket path fallback: %w", err)
		}
		return sockPath, nil
	}
	return result.Socket, nil
}

// waitForSocket polls until the Unix socket at sockPath accepts connections
// or the deadline is reached.
func waitForSocket(sockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 100 * time.Millisecond
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", sockPath, 1*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(backoff)
		if backoff < 1*time.Second {
			backoff = backoff * 3 / 2
		}
	}
	return fmt.Errorf("socket %s not ready after %s", sockPath, timeout)
}

// checkSingletonDaemonVersion calls the health endpoint and compares versions.
func checkSingletonDaemonVersion() error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + DaemonHTTPAddr + "/api/admin/health")
	if err != nil {
		return nil // can't check, assume OK
	}
	defer resp.Body.Close()
	var result struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	if result.Version != "" && result.Version != version && version != "dev" && result.Version != "dev" {
		return fmt.Errorf("version mismatch: daemon=%s, proxy=%s", result.Version, version)
	}
	return nil
}

// restartSingletonDaemon gracefully stops the running singleton daemon and
// starts a fresh one.
func restartSingletonDaemon(absPath string) error {
	pidPath, _ := singletonPIDPath()
	if pidPath != "" {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			if pid, err := parseInt(strings.TrimSpace(string(data))); err == nil {
				killProcess(pid) //nolint:errcheck

				// Wait for daemon to exit (health check fails).
				deadline := time.Now().Add(15 * time.Second)
				for time.Now().Before(deadline) {
					if !IsSingletonDaemonRunning() {
						break
					}
					time.Sleep(300 * time.Millisecond)
				}
				if processAlive(pid) {
					forceKillProcess(pid) //nolint:errcheck
					time.Sleep(500 * time.Millisecond)
				}
			}
		}
	}
	cleanStaleSingletonPID()
	return ensureSingletonDaemon(absPath)
}

// ensureProjectMarker creates a minimal synapses.json in absPath if the
// directory has no existing project marker (.git or synapses.json).
// This lets users run "synapses start" in any directory without manually
// creating config files, while preserving the daemon's security validation.
func ensureProjectMarker(absPath string) error {
	// Already a git repo (directory or worktree file)?
	if info, err := os.Stat(filepath.Join(absPath, ".git")); err == nil {
		if info.IsDir() || info.Mode().IsRegular() {
			return nil
		}
	}
	// Already has synapses.json?
	cfgPath := filepath.Join(absPath, "synapses.json")
	if info, err := os.Stat(cfgPath); err == nil && info.Mode().IsRegular() {
		return nil
	}
	// Create minimal config.
	logutil.Info("synapses proxy: creating %s\n", cfgPath)
	return os.WriteFile(cfgPath, []byte("{}\n"), 0o644)
}

// parseInt parses a non-negative integer string. It rejects negative values
// and empty strings to guard against corrupted PID files or port numbers.
func parseInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("not a non-negative integer: %s", s)
	}
	return n, nil
}
