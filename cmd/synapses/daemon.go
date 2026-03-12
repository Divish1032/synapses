// daemon.go — "synapses daemon" subcommand
// Manages brain, scout, and pulse as background services.
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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Sidecar describes one managed background service.
type Sidecar struct {
	Name   string
	Binary string
	Args   []string
	Port   string
}

var allSidecars = []Sidecar{
	{Name: "brain", Binary: "brain", Args: []string{"serve"}, Port: "11435"},
	{Name: "scout", Binary: "scout", Args: []string{"serve"}, Port: "11436"},
	{Name: "pulse", Binary: "pulse", Args: []string{"serve"}, Port: "11437"},
}

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
	base, _ := synapsesHome()
	return filepath.Join(base, "pids", name+".pid")
}

func logFilePath(name string) string {
	base, _ := synapsesHome()
	return filepath.Join(base, "logs", name+".log")
}

// ── PID helpers ───────────────────────────────────────────────────────────────

func readPID(name string) (int, error) {
	data, err := os.ReadFile(pidFilePath(name))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func writePID(name string, pid int) error {
	return os.WriteFile(pidFilePath(name), []byte(strconv.Itoa(pid)), 0o644)
}

func removePID(name string) { os.Remove(pidFilePath(name)) }

// serviceRunning returns the PID and whether it is alive.
func serviceRunning(name string) (int, bool) {
	pid, err := readPID(name)
	if err != nil {
		return 0, false
	}
	if processAlive(pid) {
		return pid, true
	}
	removePID(name) // stale
	return 0, false
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

	lf, err := os.OpenFile(logFilePath(s.Name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log for %s: %w", s.Name, err)
	}

	cmd := exec.Command(binPath, s.Args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = detachedSysProcAttr()
	// Pass pulse URL to brain so it can emit usage telemetry.
	// Only set if brain hasn't already configured pulse_url in its config file.
	if s.Name == "brain" {
		cmd.Env = append(os.Environ(), "BRAIN_PULSE_URL=http://localhost:11437")
	}

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

	// Parse --service NAME and --quiet flags from rest.
	target := ""
	quiet := false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--service":
			if i+1 < len(rest) {
				target = rest[i+1]
				i++
			}
		case "--quiet", "-q":
			quiet = true
		}
	}

	if err := ensureDirs(); err != nil {
		return fmt.Errorf("ensure dirs: %w", err)
	}

	targets, err := resolveSidecars(target)
	if err != nil {
		return err
	}

	switch sub {
	case "serve":
		// "synapses daemon serve -path <repo>" — run the MCP server as a
		// long-lived daemon process listening on a Unix socket. This is the
		// heavy server; proxies connect to it via the socket.
		return cmdDaemonServe(rest)
	case "start":
		return daemonStart(targets, quiet)
	case "stop":
		return daemonStop(targets, quiet)
	case "restart":
		_ = daemonStop(targets, true)
		time.Sleep(500 * time.Millisecond)
		return daemonStart(targets, quiet)
	case "status":
		return daemonStatus()
	case "logs":
		return daemonLogs(target)
	case "install":
		return daemonInstall()
	case "uninstall":
		return daemonUninstall()
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
	return nil, fmt.Errorf("unknown service %q (valid: brain, scout, pulse)", name)
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
	data, err := os.ReadFile(logFilePath(name))
	if err != nil {
		return fmt.Errorf("no log file for %s at %s", name, logFilePath(name))
	}
	lines := strings.Split(string(data), "\n")
	start := 0
	if len(lines) > 200 {
		start = len(lines) - 200
	}
	fmt.Print(strings.Join(lines[start:], "\n"))
	return nil
}

// ── OS init system integration ────────────────────────────────────────────────

func daemonInstall() error {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd()
	case "linux":
		return installSystemd()
	default:
		fmt.Printf("  Auto-start not supported on %s.\n", runtime.GOOS)
		fmt.Println("  Start manually: synapses daemon start")
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

func launchdPlist(s Sidecar) string {
	binPath, _ := exec.LookPath(s.Binary)
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
`, label, binPath, logPath, logPath)
}

func installLaunchd() error {
	agentsDir, err := launchdAgentsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("  Installing Synapses launch agents (macOS launchd)...")
	fmt.Println("  ────────────────────────────────────────────────────")

	for _, s := range allSidecars {
		if _, err := exec.LookPath(s.Binary); err != nil {
			fmt.Printf("  \033[33m!\033[0m %-8s binary not found — skipping\n", s.Name)
			continue
		}
		label := "com.synapses." + s.Name
		plistPath := filepath.Join(agentsDir, label+".plist")

		if err := os.WriteFile(plistPath, []byte(launchdPlist(s)), 0o644); err != nil {
			return fmt.Errorf("write plist for %s: %w", s.Name, err)
		}
		// Unload first in case it's already registered with old config.
		exec.Command("launchctl", "unload", plistPath).Run() //nolint:errcheck

		if out, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
			fmt.Printf("  \033[31m✗\033[0m %-8s launchctl load failed: %s\n", s.Name, strings.TrimSpace(string(out)))
		} else {
			fmt.Printf("  \033[32m✓\033[0m %-8s installed and will start at login\n", s.Name)
		}
	}
	fmt.Println()
	fmt.Println("  Run 'synapses daemon start' to start them now.")
	fmt.Println()
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
	fmt.Println()
	return nil
}

// ── systemd (Linux) ───────────────────────────────────────────────────────────

func systemdUserDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func systemdUnit(s Sidecar) string {
	binPath, _ := exec.LookPath(s.Binary)
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
`, s.Name, binPath, logPath, logPath)
}

func installSystemd() error {
	svcDir, err := systemdUserDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("  Installing Synapses systemd user services (Linux)...")
	fmt.Println("  ────────────────────────────────────────────────────")

	for _, s := range allSidecars {
		if _, err := exec.LookPath(s.Binary); err != nil {
			fmt.Printf("  \033[33m!\033[0m %-8s binary not found — skipping\n", s.Name)
			continue
		}
		unitName := "synapses-" + s.Name + ".service"
		unitPath := filepath.Join(svcDir, unitName)

		if err := os.WriteFile(unitPath, []byte(systemdUnit(s)), 0o644); err != nil {
			return fmt.Errorf("write unit for %s: %w", s.Name, err)
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run() //nolint:errcheck
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", unitName).CombinedOutput(); err != nil {
			fmt.Printf("  \033[31m✗\033[0m %-8s systemctl failed: %s\n", s.Name, strings.TrimSpace(string(out)))
		} else {
			fmt.Printf("  \033[32m✓\033[0m %-8s installed and started\n", s.Name)
		}
	}
	fmt.Println()
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
	exec.Command("systemctl", "--user", "daemon-reload").Run() //nolint:errcheck
	fmt.Println()
	return nil
}

func printDaemonUsage() {
	fmt.Print(`
  synapses daemon — manage background services

  Usage:
    synapses daemon serve -path <repo>   Run MCP server as a daemon (Unix socket)
    synapses daemon start                Start all sidecars (brain, scout, pulse)
    synapses daemon start --service X    Start a single sidecar
    synapses daemon stop                 Stop all sidecars
    synapses daemon stop  --service X    Stop a single sidecar
    synapses daemon restart              Restart all
    synapses daemon status               Show running / stopped state
    synapses daemon logs --service X     Tail last 200 lines of a sidecar log
    synapses daemon install              Register as login service (launchd/systemd)
    synapses daemon uninstall            Remove login service registration

  The 'serve' command is automatically invoked by 'synapses start' (the proxy).
  You rarely need to call it manually.

  Services:  brain (port 11435)  scout (port 11436)  pulse (port 11437)
`)
}
