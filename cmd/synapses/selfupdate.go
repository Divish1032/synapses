// selfupdate.go — background self-update check for the synapses daemon.
//
// At startup the daemon spawns a goroutine that checks GitHub Releases for a
// newer version and, if found, downloads and replaces the running binary.
// The replacement takes effect on the next daemon restart (the Tauri app is
// notified via the /api/admin/health response).
//
// Library: github.com/creativeprojects/go-selfupdate
// The dependency is added to go.mod with: go get github.com/creativeprojects/go-selfupdate
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"
)

const (
	updateCheckInterval = 6 * time.Hour
	updateGitHubOwner   = "SynapsesOS"
	updateGitHubRepo    = "synapses"
)

// pendingUpdateVersion is set by checkAndApplySelfUpdate once a newer binary has
// been downloaded and placed on disk. The Tauri app polls /api/admin/health and
// shows a "restart to update" banner when this is non-empty.
// TODO: assigned by the go-selfupdate implementation — currently always "".
var pendingUpdateVersion string //nolint:unused

// startSelfUpdateLoop checks for a newer daemon release on GitHub every 6 hours.
// It runs as a background goroutine and never blocks the main daemon.
// If an update is found, it downloads and replaces the binary atomically.
// The daemon continues running the old binary until it is restarted.
func startSelfUpdateLoop(ctx context.Context) {
	go func() {
		// Initial delay — let the daemon fully start before hitting the network.
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}

		for {
			checkAndApplySelfUpdate()

			select {
			case <-ctx.Done():
				return
			case <-time.After(updateCheckInterval):
			}
		}
	}()
}

func checkAndApplySelfUpdate() {
	// Guard: skip if running in dev (version == "dev") to avoid replacing dev builds.
	if version == "dev" {
		return
	}

	// TODO: implement using github.com/creativeprojects/go-selfupdate
	//   1. Run: go get github.com/creativeprojects/go-selfupdate
	//   2. Replace this stub with:
	//      updater, _ := selfupdate.NewUpdater(selfupdate.Config{})
	//      release, found, err := updater.DetectLatest(ctx, selfupdate.NewRepositorySlug(updateGitHubOwner, updateGitHubRepo))
	//      if err != nil || !found { return }
	//      if release.GreaterThan(version) {
	//          if err := updater.UpdateSelf(ctx, version, selfupdate.NewRepositorySlug(...)); err == nil {
	//              pendingUpdateVersion = release.Version()
	//          }
	//      }
	_ = runtime.GOOS
	_ = runtime.GOARCH
}

// applySelfUpdateFromPath atomically replaces the running executable with newBinary.
// It writes to a .new temp file, then renames over the original.
// On failure it cleans up and leaves the original intact.
func applySelfUpdateFromPath(newBinary string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}

	// Read new binary
	data, err := os.ReadFile(newBinary)
	if err != nil {
		return fmt.Errorf("read new binary: %w", err)
	}

	// Write to a temp file in the same directory (ensures same filesystem for rename)
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("write temp binary: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename new binary into place: %w", err)
	}

	return nil
}
