// proxy.go — thin MCP proxy for "synapses start".
//
// Instead of running the full MCP server in-process, this proxy:
//  1. Auto-starts the daemon if not already running for the project.
//  2. Connects to the daemon's Unix socket.
//  3. Bridges stdin ↔ socket bidirectionally (zero protocol awareness).
//  4. Exits when stdin closes (client disconnects) — daemon stays alive.
//
// The proxy is stateless and disposable. Crash it, kill it, spawn 100 of them
// — the daemon is unaffected. This eliminates the zombie/duplicate-server
// class of bugs entirely.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cmdStartProxy is the new implementation of "synapses start". It launches a
// lightweight proxy that connects to the daemon via Unix socket, auto-starting
// the daemon if needed. The .mcp.json config is unchanged — it still calls
// "synapses start -path <repo>".
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

	sockPath, err := daemonSocketPath(absPath)
	if err != nil {
		return fmt.Errorf("socket path: %w", err)
	}

	// ── Ensure daemon is running ─────────────────────────────────────────────
	if err := ensureDaemon(absPath, sockPath); err != nil {
		return fmt.Errorf("ensure daemon: %w", err)
	}

	// ── Check for binary version mismatch ────────────────────────────────────
	if err := checkDaemonVersion(absPath); err != nil {
		// Version mismatch — restart the daemon with the current binary.
		fmt.Fprintf(os.Stderr, "synapses proxy: %v — restarting daemon\n", err)
		if err := restartDaemon(absPath, sockPath); err != nil {
			return fmt.Errorf("restart daemon: %w", err)
		}
	}

	// ── Connect to daemon socket ─────────────────────────────────────────────
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect to daemon at %s: %w", sockPath, err)
	}
	defer conn.Close()

	// ── Bridge stdin ↔ socket ────────────────────────────────────────────────
	// The proxy is a dumb bidirectional byte pipe. It does not parse or
	// understand the MCP/JSON-RPC protocol. This keeps it trivially simple
	// and immune to protocol changes.
	//
	// Two shutdown paths:
	//   - stdin EOF (client disconnected): half-close socket write side, then
	//     wait for daemon to finish sending responses before exiting.
	//   - socket EOF (daemon died/stopped): close the connection to unblock
	//     the stdin→socket goroutine, then exit.
	stdinDone := make(chan struct{})
	sockDone := make(chan struct{})

	// stdin → socket
	go func() {
		defer close(stdinDone)
		io.Copy(conn, os.Stdin)
		// Half-close: tell daemon no more input, but keep reading responses.
		if uc, ok := conn.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
	}()

	// socket → stdout
	go func() {
		defer close(sockDone)
		io.Copy(os.Stdout, conn)
	}()

	// Wait for socket→stdout to finish. This covers both cases:
	//   - Normal: stdin EOF → half-close → daemon sends remaining responses → socket EOF
	//   - Daemon crash: socket EOF immediately
	// If socket→stdout finishes while stdin→socket is still blocked on stdin,
	// closing the connection unblocks the io.Copy.
	<-sockDone
	conn.Close() // unblock stdin→socket goroutine if still running
	<-stdinDone  // wait for it to exit cleanly
	return nil
}

// ensureDaemon checks if the daemon is running for the given project and
// starts it if not. Uses socket liveness (not PID files) as the source of
// truth. Waits for the daemon to become ready (socket accepting connections).
func ensureDaemon(absPath, sockPath string) error {
	// Fast path: daemon already running.
	conn, err := net.DialTimeout("unix", sockPath, 1*time.Second)
	if err == nil {
		conn.Close()
		return nil
	}

	// Clean up stale socket/PID files from crashed daemons.
	cleanStaleDaemonFiles(absPath)

	// Start the daemon as a detached background process.
	fmt.Fprintf(os.Stderr, "synapses proxy: starting daemon for %s\n", absPath)

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own binary: %w", err)
	}

	logPath := filepath.Join(func() string { d, _ := daemonDir(); return d }(), projectHash(absPath)+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log %s: %w", logPath, err)
	}

	cmd := exec.Command(self, "daemon", "serve", "-path", absPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = detachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	logFile.Close()
	// Reap the daemon process in the background to prevent zombie accumulation.
	go cmd.Wait() //nolint:errcheck

	// Wait for the daemon socket to appear (it creates the socket after init).
	// Large repos may take 30+ seconds for cold start.
	deadline := time.Now().Add(120 * time.Second)
	backoff := 200 * time.Millisecond
	for time.Now().Before(deadline) {
		time.Sleep(backoff)
		conn, err := net.DialTimeout("unix", sockPath, 1*time.Second)
		if err == nil {
			conn.Close()
			fmt.Fprintf(os.Stderr, "synapses proxy: daemon ready\n")
			return nil
		}

		// Check if daemon process died during startup.
		pidPath, _ := daemonPIDPath(absPath)
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			// PID file gone — daemon exited. This could mean:
			// (a) Init failed, or
			// (b) Another daemon was already running and ours exited with
			//     "already running" — a race between two proxies.
			// Try connecting one more time before giving up (handles case b).
			retryConn, retryErr := net.DialTimeout("unix", sockPath, 2*time.Second)
			if retryErr == nil {
				retryConn.Close()
				fmt.Fprintf(os.Stderr, "synapses proxy: daemon ready (started by another proxy)\n")
				return nil
			}
			// Genuinely failed — report the log tail.
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

		// Exponential backoff up to 2s.
		if backoff < 2*time.Second {
			backoff = backoff * 3 / 2
		}
	}
	return fmt.Errorf("timed out waiting for daemon socket at %s (120s)", sockPath)
}

// checkDaemonVersion reads the daemon's version file and compares it to the
// current binary's version. Returns an error if they differ.
func checkDaemonVersion(absPath string) error {
	verPath, err := daemonVersionPath(absPath)
	if err != nil {
		return nil // can't check — assume OK
	}
	data, err := os.ReadFile(verPath)
	if err != nil {
		return nil // no version file — old daemon, let it be
	}
	daemonVer := strings.TrimSpace(string(data))
	if daemonVer != version {
		return fmt.Errorf("version mismatch: daemon=%s, proxy=%s", daemonVer, version)
	}
	return nil
}

// restartDaemon sends SIGTERM to the running daemon, waits for it to exit,
// then starts a new daemon with the current binary.
func restartDaemon(absPath, sockPath string) error {
	// Read daemon PID and send SIGTERM.
	pidPath, _ := daemonPIDPath(absPath)
	if pidPath != "" {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				killProcess(pid)

				// Wait for old daemon to die (socket disappears).
				deadline := time.Now().Add(15 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(sockPath); os.IsNotExist(err) {
						break
					}
					time.Sleep(300 * time.Millisecond)
				}

				// Force kill if still alive.
				if processAlive(pid) {
					forceKillProcess(pid)
					time.Sleep(500 * time.Millisecond)
				}
			}
		}
	}

	// Clean up stale files and start fresh.
	cleanStaleDaemonFiles(absPath)
	return ensureDaemon(absPath, sockPath)
}
