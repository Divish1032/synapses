// daemon.go — "synapses daemon" subcommand
// Manages the singleton daemon and any external background services.
// Uses ~/.synapses/pids/ for PID tracking and ~/.synapses/logs/ for output.
//
// Usage:
//
//	synapses daemon start            # start all configured sidecars
//	synapses daemon start --service brain
//	synapses daemon stop             # stop all
//	synapses daemon stop --service pulse
//	synapses daemon restart
//	synapses daemon status           # show running/stopped state
//	synapses daemon logs --service brain
//	synapses daemon install          # register as OS login service (launchd / systemd)
//	synapses daemon uninstall        # remove OS login service
package main

import (
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// daemonLabel is the launchd/systemd service identifier for the main daemon process.
const daemonLabel = "com.synapses.daemon"

// Sidecar describes one managed background service.
type Sidecar struct {
	Name   string
	Binary string
	Args   []string
	Port   string
}

// allSidecars lists external managed services. Brain and pulse are in-process;
// web intelligence is now built into the core via the Go webcache module.
var allSidecars = []Sidecar{}

// ── path helpers ──────────────────────────────────────────────────────────────

func synapsesHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".synapses"), nil
}

func ensureDirs() error {
	base, err := synapsesHome()
	if err != nil {
		return err
	}
	for _, sub := range []string{"pids", "logs"} {
		if err := os.MkdirAll(filepath.Join(base, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func pidFilePath(name string) string {
	base, err := synapsesHome()
	if err != nil {
		log.Printf("warning: could not determine synapses home: %v; falling back to /tmp", err)
		base = "/tmp/.synapses"
	}
	return filepath.Join(base, "pids", name+".pid")
}

func logFilePath(name string) string {
	base, err := synapsesHome()
	if err != nil {
		log.Printf("warning: could not determine synapses home: %v; falling back to /tmp", err)
		base = "/tmp/.synapses"
	}
	return filepath.Join(base, "logs", name+".log")
}

// ── PID helpers ───────────────────────────────────────────────────────────────

func readPID(name string) (int, error) {
	data, err := os.ReadFile(pidFilePath(name))
	if err != nil {
		return 0, err
	}
	// PID file format: "<pid>\n<start_unix_nanos>" (second line optional for compat).
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	return strconv.Atoi(line)
}

// readPIDWithStart reads both PID and start timestamp from the PID file.
func readPIDWithStart(name string) (int, int64, error) {
	data, err := os.ReadFile(pidFilePath(name))
	if err != nil {
		return 0, 0, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	var startNanos int64
	if len(parts) > 1 {
		startNanos, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}
	return pid, startNanos, nil
}

func writePID(name string, pid int) error {
	// Write PID and current timestamp so we can detect PID recycling on read.
	content := fmt.Sprintf("%d\n%d", pid, time.Now().UnixNano())
	return os.WriteFile(pidFilePath(name), []byte(content), 0o600)
}

func removePID(name string) { os.Remove(pidFilePath(name)) }

// serviceRunning returns the PID and whether it is alive.
// Guards against PID recycling by comparing the process start time
// against the timestamp recorded in the PID file.
func serviceRunning(name string) (int, bool) {
	pid, startNanos, err := readPIDWithStart(name)
	if err != nil {
		return 0, false
	}
	if !processAlive(pid) {
		removePID(name) // stale
		return 0, false
	}
	// If we have a recorded start time, verify the process hasn't been recycled.
	if startNanos > 0 {
		if procStart := processStartTime(pid); procStart > 0 {
			// Allow 2-second tolerance for clock granularity differences.
			recorded := time.Unix(0, startNanos)
			actual := time.Unix(0, procStart)
			if actual.Sub(recorded).Abs() > 2*time.Second {
				removePID(name) // PID was recycled
				return 0, false
			}
		}
	}
	return pid, true
}

// ── start / stop ──────────────────────────────────────────────────────────────

func startSidecar(s Sidecar, quiet bool) error {
	if pid, running := serviceRunning(s.Name); running {
		if !quiet {
			fmt.Printf("  \033[32m✓\033[0m %-8s already running  (pid %d)\n", s.Name, pid)
		}
		return nil
	}

	binPath, err := exec.LookPath(s.Binary)
	if err != nil {
		if !quiet {
			fmt.Printf("  \033[33m!\033[0m %-8s not found in PATH — skipping\n", s.Name)
		}
		return nil
	}

	lf, err := os.OpenFile(logFilePath(s.Name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log for %s: %w", s.Name, err)
	}

	cmd := exec.Command(binPath, s.Args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = detachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("start %s: %w", s.Name, err)
	}
	lf.Close()

	pid := cmd.Process.Pid
	// Reap the child process in the background so it doesn't become a zombie.
	// Without Wait(), the OS keeps a process table entry until the parent exits.
	go cmd.Wait() //nolint:errcheck

	if err := writePID(s.Name, pid); err != nil {
		// Kill the orphaned child since we can't track it without a PID file.
		cmd.Process.Kill() //nolint:errcheck
		return fmt.Errorf("write pid for %s: %w", s.Name, err)
	}
	if !quiet {
		fmt.Printf("  \033[32m✓\033[0m %-8s started  (pid %d, log: %s)\n", s.Name, pid, logFilePath(s.Name))
	}
	return nil
}

func stopSidecar(name string, quiet bool) error {
	pid, running := serviceRunning(name)
	if !running {
		if !quiet {
			fmt.Printf("  \033[33m!\033[0m %-8s not running\n", name)
		}
		return nil
	}

	if err := killProcess(pid); err != nil {
		removePID(name)
		return nil
	}

	// Wait up to 5 s for the process to exit.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if processAlive(pid) {
		forceKillProcess(pid) //nolint:errcheck
	}
	removePID(name)

	if !quiet {
		fmt.Printf("  \033[32m✓\033[0m %-8s stopped\n", name)
	}
	return nil
}

// ── cmdDaemon entry point ─────────────────────────────────────────────────────

func cmdDaemon(args []string) error {
	if len(args) == 0 {
		printDaemonUsage()
		return nil
	}

	sub := args[0]
	rest := args[1:]

	// Parse --service NAME flag for logs subcommand.
	target := ""
	for i := 0; i < len(rest); i++ {
		if rest[i] == "--service" && i+1 < len(rest) {
			target = rest[i+1]
			i++
		}
	}

	if err := ensureDirs(); err != nil {
		return fmt.Errorf("ensure dirs: %w", err)
	}

	switch sub {
	case "serve":
		return cmdDaemonServe(rest)
	case "install":
		return daemonInstall(os.Stdout)
	case "uninstall":
		return daemonUninstall()
	case "logs":
		return daemonLogs(target)
	default:
		return fmt.Errorf("unknown daemon subcommand %q — run 'synapses daemon' for usage", sub)
	}
}

func resolveSidecars(name string) ([]Sidecar, error) {
	if name == "" {
		return allSidecars, nil
	}
	for _, s := range allSidecars {
		if s.Name == name {
			return []Sidecar{s}, nil
		}
	}
	return nil, fmt.Errorf("unknown service %q (no external sidecars registered)", name)
}

// ── subcommand implementations ────────────────────────────────────────────────

func daemonStart(targets []Sidecar, quiet bool) error {
	if !quiet {
		fmt.Println()
		fmt.Println("  Starting Synapses services...")
		fmt.Println("  ─────────────────────────────────")
	}
	for _, s := range targets {
		if err := startSidecar(s, quiet); err != nil {
			fmt.Printf("  \033[31m✗\033[0m %-8s error: %v\n", s.Name, err)
		}
	}
	if !quiet {
		fmt.Println()
	}
	return nil
}

func daemonStop(targets []Sidecar, quiet bool) error {
	if !quiet {
		fmt.Println()
		fmt.Println("  Stopping Synapses services...")
		fmt.Println("  ─────────────────────────────────")
	}
	for _, s := range targets {
		if err := stopSidecar(s.Name, quiet); err != nil {
			fmt.Printf("  \033[31m✗\033[0m %-8s error: %v\n", s.Name, err)
		}
	}
	if !quiet {
		fmt.Println()
	}
	return nil
}

func daemonStatus() error {
	fmt.Println()
	fmt.Println("  Synapses service status")
	fmt.Println("  ─────────────────────────────────────────")
	for _, s := range allSidecars {
		pid, running := serviceRunning(s.Name)
		if running {
			fmt.Printf("  \033[32m●\033[0m %-8s  running  pid=%-7d  port=%s\n", s.Name, pid, s.Port)
		} else {
			fmt.Printf("  \033[31m○\033[0m %-8s  stopped\n", s.Name)
		}
	}
	fmt.Println()
	return nil
}

func daemonLogs(name string) error {
	if name == "" {
		return fmt.Errorf("specify a service: synapses daemon logs --service brain")
	}
	// Validate name against known services to prevent path traversal.
	allowed := false
	for _, s := range allSidecars {
		if s.Name == name {
			allowed = true
			break
		}
	}
	if name == "daemon" {
		allowed = true
	}
	if !allowed {
		return fmt.Errorf("unknown service %q", name)
	}
	lines := tailFile(logFilePath(name), 200)
	if len(lines) == 0 {
		return fmt.Errorf("no log file for %s at %s", name, logFilePath(name))
	}
	fmt.Print(strings.Join(lines, "\n"))
	return nil
}

// daemonWait blocks until the singleton daemon responds to health checks or
// the timeout expires. Useful in scripts and CI to gate further commands on
// daemon readiness: `synapses daemon wait --timeout 60`.
func daemonWait(args []string) error {
	fs := flag.NewFlagSet("daemon wait", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 30*time.Second, "Maximum time to wait")
	if err := fs.Parse(args); err != nil {
		return err
	}
	deadline := time.Now().Add(*timeout)
	interval := 250 * time.Millisecond
	const maxInterval = 2 * time.Second
	fmt.Printf("Waiting for Synapses daemon at %s", DaemonHTTPAddr)
	for {
		if IsSingletonDaemonRunning() {
			fmt.Println(" ✓ ready")
			return nil
		}
		if time.Now().After(deadline) {
			fmt.Println()
			return fmt.Errorf("daemon at %s did not become healthy within %s", DaemonHTTPAddr, *timeout)
		}
		fmt.Print(".")
		time.Sleep(interval)
		if interval < maxInterval {
			interval = interval * 3 / 2
		}
	}
}

// ── OS init system integration ────────────────────────────────────────────────

func daemonInstall(w io.Writer) error {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(w)
	case "linux":
		return installSystemd(w)
	default:
		fmt.Fprintf(w, "  Auto-start not supported on %s.\n", runtime.GOOS)
		fmt.Fprintln(w, "  Start manually: synapses daemon start")
		return nil
	}
}

func daemonUninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "linux":
		return uninstallSystemd()
	default:
		return fmt.Errorf("not supported on %s", runtime.GOOS)
	}
}

// ── launchd (macOS) ───────────────────────────────────────────────────────────

func launchdAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func launchdPlist(s Sidecar) (string, error) {
	binPath, err := exec.LookPath(s.Binary)
	if err != nil {
		return "", fmt.Errorf("look up %s binary: %w", s.Binary, err)
	}
	logPath := logFilePath(s.Name)
	label := "com.synapses." + s.Name
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, html.EscapeString(label), html.EscapeString(binPath), html.EscapeString(logPath), html.EscapeString(logPath)), nil
}

// daemonSelfPlist returns the launchd plist XML for the daemon process itself.
// It uses KeepAlive so launchd auto-restarts the daemon on crash.
func daemonSelfPlist(binPath, logPath, homeDir string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>daemon</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
  <key>Sockets</key>
  <dict>
    <key>SynapsesHTTP</key>
    <dict>
      <key>SockServiceName</key>
      <string>%s</string>
      <key>SockType</key>
      <string>stream</string>
      <key>SockFamily</key>
      <string>IPv4</string>
      <key>SockNodeName</key>
      <string>127.0.0.1</string>
    </dict>
  </dict>
</dict>
</plist>
`, html.EscapeString(daemonLabel), html.EscapeString(binPath), html.EscapeString(homeDir), html.EscapeString(logPath), html.EscapeString(logPath), DaemonHTTPPort)
}

func installLaunchd(w io.Writer) error {
	agentsDir, err := launchdAgentsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Installing Synapses launch agents (macOS launchd)...")
	fmt.Fprintln(w, "  ────────────────────────────────────────────────────")

	for _, s := range allSidecars {
		if _, err := exec.LookPath(s.Binary); err != nil {
			fmt.Fprintf(w, "  \033[33m!\033[0m %-8s binary not found — skipping\n", s.Name)
			continue
		}
		label := "com.synapses." + s.Name
		plistPath := filepath.Join(agentsDir, label+".plist")

		plistContent, err := launchdPlist(s)
		if err != nil {
			return fmt.Errorf("generate plist for %s: %w", s.Name, err)
		}
		if err := os.WriteFile(plistPath, []byte(plistContent), 0o644); err != nil {
			return fmt.Errorf("write plist for %s: %w", s.Name, err)
		}
		// Unload first in case it's already registered with old config.
		exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck

		if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
			fmt.Fprintf(w, "  \033[31m✗\033[0m %-8s launchctl load failed: %s\n", s.Name, strings.TrimSpace(string(out)))
		} else {
			fmt.Fprintf(w, "  \033[32m✓\033[0m %-8s installed and will start at login\n", s.Name)
		}
	}

	// Install the daemon itself so launchd auto-restarts it on crash.
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	logPath := logFilePath("daemon")
	plistPath := filepath.Join(agentsDir, daemonLabel+".plist")
	plistContent := daemonSelfPlist(binPath, logPath, homeDir)

	if err := os.WriteFile(plistPath, []byte(plistContent), 0o644); err != nil {
		return fmt.Errorf("write daemon plist: %w", err)
	}
	exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck
	if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
		fmt.Fprintf(w, "  \033[31m✗\033[0m %-8s launchctl load failed: %s\n", "daemon", strings.TrimSpace(string(out)))
	} else {
		fmt.Fprintf(w, "  \033[32m✓\033[0m %-8s installed — daemon will auto-start at login and restart on crash\n", "daemon")
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Run 'synapses start' to start the daemon now.")
	fmt.Fprintln(w)
	return nil
}

func uninstallLaunchd() error {
	agentsDir, err := launchdAgentsDir()
	if err != nil {
		return err
	}
	fmt.Println()
	for _, s := range allSidecars {
		label := "com.synapses." + s.Name
		plistPath := filepath.Join(agentsDir, label+".plist")
		exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck
		os.Remove(plistPath)
		fmt.Printf("  \033[32m✓\033[0m %-8s uninstalled\n", s.Name)
	}

	// Uninstall the daemon plist.
	daemonPlistPath := filepath.Join(agentsDir, daemonLabel+".plist")
	exec.Command("launchctl", "unload", daemonPlistPath).Run() //nolint:errcheck
	os.Remove(daemonPlistPath)
	fmt.Printf("  \033[32m✓\033[0m %-8s uninstalled\n", "daemon")

	fmt.Println()
	return nil
}

// sanitizeUnitValue strips newlines to prevent directive injection in systemd unit files.
func sanitizeUnitValue(s string) string {
	return strings.NewReplacer("\n", "", "\r", "").Replace(s)
}

// ── systemd (Linux) ───────────────────────────────────────────────────────────

func systemdUserDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func systemdUnit(s Sidecar) (string, error) {
	binPath, err := exec.LookPath(s.Binary)
	if err != nil {
		return "", fmt.Errorf("look up %s binary: %w", s.Binary, err)
	}
	logPath := logFilePath(s.Name)
	return fmt.Sprintf(`[Unit]
Description=Synapses %s sidecar
After=network.target

[Service]
Type=simple
ExecStart=%s serve
Restart=on-failure
RestartSec=5
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, sanitizeUnitValue(s.Name), sanitizeUnitValue(binPath), sanitizeUnitValue(logPath), sanitizeUnitValue(logPath)), nil
}

// daemonSelfSystemdUnit returns the systemd unit file for the daemon process itself.
func daemonSelfSystemdUnit(binPath, logPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Synapses MCP daemon
After=network.target
Requires=synapses.socket

[Service]
Type=simple
ExecStart=%s daemon serve
Restart=on-failure
RestartSec=5
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, sanitizeUnitValue(binPath), sanitizeUnitValue(logPath), sanitizeUnitValue(logPath))
}

// daemonSocketUnit returns the systemd socket unit for socket activation.
func daemonSocketUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Synapses MCP daemon socket

[Socket]
ListenStream=127.0.0.1:%s

[Install]
WantedBy=sockets.target
`, DaemonHTTPPort)
}

func installSystemd(w io.Writer) error {
	svcDir, err := systemdUserDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Installing Synapses systemd user services (Linux)...")
	fmt.Fprintln(w, "  ────────────────────────────────────────────────────")

	for _, s := range allSidecars {
		if _, err := exec.LookPath(s.Binary); err != nil {
			fmt.Fprintf(w, "  \033[33m!\033[0m %-8s binary not found — skipping\n", s.Name)
			continue
		}
		unitName := "synapses-" + s.Name + ".service"
		unitPath := filepath.Join(svcDir, unitName)

		unitContent, err := systemdUnit(s)
		if err != nil {
			return fmt.Errorf("generate unit for %s: %w", s.Name, err)
		}
		if err := os.WriteFile(unitPath, []byte(unitContent), 0o644); err != nil {
			return fmt.Errorf("write unit for %s: %w", s.Name, err)
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run() //nolint:errcheck
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
			fmt.Fprintf(w, "  \033[31m✗\033[0m %-8s systemctl failed: %s\n", s.Name, strings.TrimSpace(string(out)))
		} else {
			fmt.Fprintf(w, "  \033[32m✓\033[0m %-8s installed and started\n", s.Name)
		}
	}

	// Install the daemon socket unit (socket activation) and service unit.
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve binary path: %w", err)
	}
	logPath := logFilePath("daemon")

	// Write socket unit first — holds the port during daemon restarts.
	socketUnitName := "synapses.socket"
	socketUnitPath := filepath.Join(svcDir, socketUnitName)
	if err := os.WriteFile(socketUnitPath, []byte(daemonSocketUnit()), 0o644); err != nil {
		return fmt.Errorf("write socket unit: %w", err)
	}

	// Write service unit.
	unitName := "synapses-daemon.service"
	unitPath := filepath.Join(svcDir, unitName)
	if err := os.WriteFile(unitPath, []byte(daemonSelfSystemdUnit(binPath, logPath)), 0o644); err != nil {
		return fmt.Errorf("write daemon unit: %w", err)
	}

	exec.Command("systemctl", "--user", "daemon-reload").Run() //nolint:errcheck

	// Enable and start the socket (which will start the service on first connection).
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", socketUnitName).CombinedOutput(); err != nil {
		fmt.Fprintf(w, "  \033[31m✗\033[0m %-8s systemctl socket failed: %s\n", "daemon", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
		fmt.Fprintf(w, "  \033[31m✗\033[0m %-8s systemctl failed: %s\n", "daemon", strings.TrimSpace(string(out)))
	} else {
		fmt.Fprintf(w, "  \033[32m✓\033[0m %-8s installed with socket activation — auto-restarts on crash, port stays open\n", "daemon")
	}

	fmt.Fprintln(w)
	return nil
}

func uninstallSystemd() error {
	svcDir, err := systemdUserDir()
	if err != nil {
		return err
	}
	fmt.Println()
	for _, s := range allSidecars {
		unitName := "synapses-" + s.Name + ".service"
		exec.Command("systemctl", "--user", "disable", "--now", unitName).Run() //nolint:errcheck
		os.Remove(filepath.Join(svcDir, unitName))
		fmt.Printf("  \033[32m✓\033[0m %-8s uninstalled\n", s.Name)
	}

	// Uninstall the daemon service unit.
	daemonUnitName := "synapses-daemon.service"
	exec.Command("systemctl", "--user", "disable", "--now", daemonUnitName).Run() //nolint:errcheck
	os.Remove(filepath.Join(svcDir, daemonUnitName))
	fmt.Printf("  \033[32m✓\033[0m %-8s uninstalled\n", "daemon")

	// Uninstall the daemon socket unit.
	socketUnitName := "synapses.socket"
	exec.Command("systemctl", "--user", "disable", "--now", socketUnitName).Run() //nolint:errcheck
	os.Remove(filepath.Join(svcDir, socketUnitName))
	fmt.Printf("  \033[32m✓\033[0m %-8s uninstalled\n", "socket")

	exec.Command("systemctl", "--user", "daemon-reload").Run() //nolint:errcheck
	fmt.Println()
	return nil
}

func printDaemonUsage() {
	fmt.Print(`
  synapses daemon — low-level daemon control

  Usage:
    synapses daemon serve       Run the MCP daemon in foreground (used by launchd/systemd)
    synapses daemon install     Register as login service (auto-start on boot)
    synapses daemon uninstall   Remove login service registration
    synapses daemon logs        Tail daemon log
`)
}
