package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// devLinkState is persisted to ~/.synapses/dev_link.json.
type devLinkState struct {
	Linked   bool   `json:"linked"`
	Source   string `json:"source"`
	LinkedAt string `json:"linked_at"`
}

func cmdDev(args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, `Usage: synapses dev <subcommand>

SUBCOMMANDS:
  link <path>   Use a custom binary (e.g., from source build)
  unlink        Restore the app-bundled binary
  status        Show which binary is active
`)
		return nil
	}

	switch args[0] {
	case "link":
		return cmdDevLink(args[1:])
	case "unlink":
		return cmdDevUnlink(args[1:])
	case "status":
		return cmdDevStatus(args[1:])
	default:
		return fmt.Errorf("unknown dev subcommand %q — use link, unlink, or status", args[0])
	}
}

func cmdDevLink(args []string) error {
	fs := flag.NewFlagSet("dev link", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: synapses dev link <path-to-binary>")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return fmt.Errorf("missing path to binary — example: synapses dev link ./build/synapses")
	}

	srcPath, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("binary not found at %s: %w", srcPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory — provide the path to the binary file", srcPath)
	}

	binDir := synapsesDataDir("bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("create bin directory: %w", err)
	}

	dest := filepath.Join(binDir, "synapses")
	backup := filepath.Join(binDir, "synapses.app-backup")

	// Back up current binary if it exists and no backup exists yet
	if _, err := os.Stat(dest); err == nil {
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			data, err := os.ReadFile(dest)
			if err != nil {
				return fmt.Errorf("read current binary for backup: %w", err)
			}
			if err := os.WriteFile(backup, data, 0o755); err != nil {
				return fmt.Errorf("write backup: %w", err)
			}
		}
	}

	// Copy source binary to dest
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read source binary: %w", err)
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return fmt.Errorf("write binary: %w", err)
	}

	// Write link state
	state := devLinkState{
		Linked:   true,
		Source:   srcPath,
		LinkedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDevLinkState(&state); err != nil {
		return err
	}

	fmt.Printf("CLI now using custom binary from %s\n", srcPath)
	fmt.Println("Run 'synapses dev unlink' to restore the app-bundled binary.")
	return nil
}

func cmdDevUnlink(args []string) error {
	binDir := synapsesDataDir("bin")
	dest := filepath.Join(binDir, "synapses")
	backup := filepath.Join(binDir, "synapses.app-backup")

	if _, err := os.Stat(backup); os.IsNotExist(err) {
		return fmt.Errorf("no backup found at %s — nothing to restore", backup)
	}

	data, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return fmt.Errorf("write binary: %w", err)
	}
	os.Remove(backup)

	// Clear link state
	state := devLinkState{Linked: false}
	if err := writeDevLinkState(&state); err != nil {
		return err
	}

	fmt.Println("CLI restored to app-bundled binary.")
	return nil
}

func cmdDevStatus(args []string) error {
	state, err := readDevLinkState()
	if err != nil || !state.Linked {
		fmt.Println("Using: app-bundled binary (default)")
	} else {
		fmt.Printf("Using: custom binary (linked from %s)\n", state.Source)
		fmt.Printf("Linked at: %s\n", state.LinkedAt)
	}

	// Show app binary info if app is installed
	appBin := appBundledBinaryPath()
	if appBin != "" {
		if out, err := exec.Command(appBin, "version").Output(); err == nil {
			fmt.Printf("App binary: %s %s", appBin, string(out))
		} else {
			fmt.Printf("App binary: %s (version unknown)\n", appBin)
		}
	} else {
		fmt.Println("App binary: not found")
	}

	// Show active binary version
	activeBin := filepath.Join(synapsesDataDir("bin"), "synapses")
	if out, err := exec.Command(activeBin, "version").Output(); err == nil {
		fmt.Printf("Active binary: %s %s", activeBin, string(out))
	}

	return nil
}

func synapsesDataDir(sub string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("/tmp", ".synapses", sub)
	}
	return filepath.Join(home, ".synapses", sub)
}

func devLinkStatePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".synapses", "dev_link.json")
}

func readDevLinkState() (*devLinkState, error) {
	data, err := os.ReadFile(devLinkStatePath())
	if err != nil {
		return &devLinkState{}, err
	}
	var state devLinkState
	if err := json.Unmarshal(data, &state); err != nil {
		return &devLinkState{}, err
	}
	return &state, nil
}

func writeDevLinkState(state *devLinkState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal dev link state: %w", err)
	}
	data = append(data, '\n')
	path := devLinkStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// appBundledBinaryPath returns the path to the synapses binary inside the app
// bundle, or empty string if the app is not installed.
func appBundledBinaryPath() string {
	switch runtime.GOOS {
	case "darwin":
		path := "/Applications/Synapses.app/Contents/Resources/synapses"
		if _, err := os.Stat(path); err == nil {
			return path
		}
		// Try platform-specific name
		for _, name := range []string{
			"synapses-aarch64-apple-darwin",
			"synapses-x86_64-apple-darwin",
		} {
			p := filepath.Join("/Applications/Synapses.app/Contents/Resources", name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	case "linux":
		path := "/opt/synapsesos/synapses"
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}
