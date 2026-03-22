// selfupdate.go — background self-update check and CLI update command.
//
// The daemon spawns a goroutine that checks GitHub Releases for a newer
// version every 6 hours. If found, it records the update state (version,
// changelog URL, asset URL). The binary is NOT downloaded automatically
// — the user must run `synapses update`.
//
// Update state is persisted at ~/.synapses/update_state.json so the web
// console, CLI, and MCP can show "update available" banners.
//
// Zero external dependencies — uses only Go stdlib.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

const (
	updateCheckInterval = 6 * time.Hour
	updateGitHubOwner   = "SynapsesOS"
	updateGitHubRepo    = "synapses"
	updateHTTPTimeout   = 30 * time.Second
	downloadHTTPTimeout = 10 * time.Minute
	// Minimum time between checks — avoids re-checking on rapid daemon restarts.
	updateMinCheckAge = 1 * time.Hour
)

// ── Update state ────────────────────────────────────────────────────────────

// UpdateState is persisted at ~/.synapses/update_state.json.
// Read by the web console, CLI, and MCP session_init.
type UpdateState struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	UpdateAvailable bool   `json:"update_available"`
	ChangelogURL    string `json:"changelog_url"`
	AssetURL        string `json:"asset_url"`
	AssetName       string `json:"asset_name"`
	CheckedAt       string `json:"checked_at"`
	Error           string `json:"error,omitempty"`
}

// pendingUpdate stores the latest version string atomically for fast reads.
var pendingUpdate atomic.Value // stores string

// updateMu prevents concurrent update checks / downloads.
var updateMu sync.Mutex

func getPendingUpdateVersion() string {
	v, _ := pendingUpdate.Load().(string)
	return v
}

func getUpdateState() *UpdateState {
	home, err := synapsesHome()
	if err != nil {
		return nil
	}
	return loadUpdateState(filepath.Join(home, "update_state.json"))
}

// ── GitHub API types ────────────────────────────────────────────────────────

type githubRelease struct {
	TagName    string        `json:"tag_name"`
	Name       string        `json:"name"`
	HTMLURL    string        `json:"html_url"`
	Prerelease bool          `json:"prerelease"`
	Draft      bool          `json:"draft"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// ── Background loop ─────────────────────────────────────────────────────────

// startSelfUpdateLoop checks for a newer daemon release on GitHub every 6 hours.
// Respects the auto_check_updates preference in app_settings.json.
// It only checks and records state — never auto-downloads the binary.
func startSelfUpdateLoop(ctx context.Context) {
	go func() {
		// Initial delay — let the daemon fully start before hitting the network.
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}

		for {
			if isAutoCheckEnabled() {
				// Hydrate pendingUpdate from persisted state on first loop iteration
				// so that the web console and MCP see the update hint immediately
				// without waiting for a fresh network check.
				if existing := getUpdateState(); existing != nil && existing.UpdateAvailable {
					pendingUpdate.Store(existing.LatestVersion)
					// Skip network check if last check is recent enough.
					if isRecentCheck(existing.CheckedAt) {
						goto wait
					}
				}
				checkForUpdate()
			}
		wait:
			select {
			case <-ctx.Done():
				return
			case <-time.After(updateCheckInterval):
			}
		}
	}()
}

// isAutoCheckEnabled reads the auto_check_updates preference.
// Default: true (users must explicitly opt out).
func isAutoCheckEnabled() bool {
	home, err := synapsesHome()
	if err != nil {
		return true
	}
	data, err := os.ReadFile(filepath.Join(home, "app_settings.json"))
	if err != nil {
		return true // no settings file → default on
	}
	var settings map[string]interface{}
	if json.Unmarshal(data, &settings) != nil {
		return true
	}
	if v, ok := settings["auto_check_updates"]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return true
}

// isRecentCheck returns true if checkedAt is within updateMinCheckAge.
func isRecentCheck(checkedAt string) bool {
	if checkedAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, checkedAt)
	if err != nil {
		return false
	}
	return time.Since(t) < updateMinCheckAge
}

// checkForUpdate queries GitHub for the latest release and persists the result.
// Safe for concurrent calls (protected by updateMu).
func checkForUpdate() *UpdateState {
	if version == "dev" {
		return nil
	}

	updateMu.Lock()
	defer updateMu.Unlock()

	state := &UpdateState{
		CurrentVersion: version,
		CheckedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	release, err := fetchLatestRelease()
	if err != nil {
		state.Error = err.Error()
		logutil.Info("synapses: update check failed: %v\n", err)
		saveUpdateState(state)
		return state
	}

	latestVer := strings.TrimPrefix(release.TagName, "v")
	state.LatestVersion = latestVer
	state.ChangelogURL = release.HTMLURL

	if compareSemver(latestVer, version) > 0 {
		state.UpdateAvailable = true
		assetName := platformAssetName()
		for _, a := range release.Assets {
			if a.Name == assetName {
				state.AssetURL = a.BrowserDownloadURL
				state.AssetName = a.Name
				break
			}
		}
		pendingUpdate.Store(latestVer)
		logutil.Info("synapses: update available: %s → %s\n", version, latestVer)
	} else {
		state.UpdateAvailable = false
		pendingUpdate.Store("")
	}

	saveUpdateState(state)
	return state
}

// ── CLI command: synapses update ─────────────────────────────────────────────

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "Only check for updates, don't download")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if version == "dev" {
		fmt.Println("Running a dev build — update check skipped.")
		return nil
	}

	fmt.Printf("Current version: %s\n", version)
	fmt.Println("Checking for updates...")

	state := checkForUpdate()
	if state == nil {
		return fmt.Errorf("update check failed")
	}
	if state.Error != "" {
		return fmt.Errorf("update check: %s", state.Error)
	}

	if !state.UpdateAvailable {
		fmt.Printf("Already up to date (latest: %s).\n", state.LatestVersion)
		return nil
	}

	fmt.Printf("Update available: %s → %s\n", version, state.LatestVersion)
	fmt.Printf("Changelog: %s\n", state.ChangelogURL)

	if *checkOnly {
		return nil
	}

	if state.AssetURL == "" {
		return fmt.Errorf("no binary available for %s/%s — download manually from %s",
			runtime.GOOS, runtime.GOARCH, state.ChangelogURL)
	}

	fmt.Printf("Downloading %s...\n", state.AssetName)

	tmpDir, err := os.MkdirTemp("", "synapses-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, state.AssetName)
	if err := downloadFile(archivePath, state.AssetURL); err != nil {
		return fmt.Errorf("download: %w", err)
	}

	// Verify download size matches Content-Length (already checked in downloadFile).
	fmt.Println("Extracting...")
	binaryPath, err := extractBinary(archivePath, tmpDir)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	// Verify the extracted binary is a reasonable size (not truncated/empty).
	info, err := os.Stat(binaryPath)
	if err != nil || info.Size() < 1024*1024 {
		return fmt.Errorf("extracted binary is suspiciously small (%d bytes) — aborting", info.Size())
	}

	fmt.Println("Replacing binary...")
	if err := applySelfUpdateFromPath(binaryPath); err != nil {
		// Detect permission errors and give a helpful hint.
		if os.IsPermission(err) {
			exe, _ := os.Executable()
			return fmt.Errorf("permission denied updating %s — try: sudo synapses update", exe)
		}
		return fmt.Errorf("apply update: %w", err)
	}

	// Also update ~/.synapses/bin/ copy if it exists.
	if home, err := synapsesHome(); err == nil {
		binCopy := filepath.Join(home, "bin", "synapses")
		if _, statErr := os.Stat(binCopy); statErr == nil {
			if data, readErr := os.ReadFile(binaryPath); readErr == nil {
				_ = os.WriteFile(binCopy, data, 0o755)
			}
		}
	}

	// Clear update state.
	state.UpdateAvailable = false
	saveUpdateState(state)
	pendingUpdate.Store("")

	fmt.Printf("\nUpdated to %s.\n", state.LatestVersion)
	fmt.Println("The daemon will auto-restart on the next MCP connection.")
	fmt.Println("Or restart manually: synapses stop && synapses start -path <dir>")
	return nil
}

// ── GitHub API ──────────────────────────────────────────────────────────────

func fetchLatestRelease() (*githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest",
		updateGitHubOwner, updateGitHubRepo)

	client := &http.Client{Timeout: updateHTTPTimeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "synapses/"+version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("GitHub API rate limited (status %d) — will retry later", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB cap
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("parse release JSON: %w", err)
	}

	if release.Draft || release.Prerelease {
		return nil, fmt.Errorf("latest release is draft/prerelease — skipping")
	}

	return &release, nil
}

// ── Download + extract ──────────────────────────────────────────────────────

// downloadFile downloads url to dst with progress output. Verifies the download
// is complete by checking actual bytes written against Content-Length.
func downloadFile(dst, url string) error {
	client := &http.Client{Timeout: downloadHTTPTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}

	// Hash the download for integrity logging.
	hasher := sha256.New()
	multiWriter := io.MultiWriter(f, hasher)

	total := resp.ContentLength
	var written int64
	buf := make([]byte, 32*1024)
	lastPct := -1
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := multiWriter.Write(buf[:n]); wErr != nil {
				f.Close()
				return wErr
			}
			written += int64(n)
			if total > 0 {
				pct := int(written * 100 / total)
				if pct/10 != lastPct/10 {
					fmt.Printf("  %d%% (%d / %d MB)\n", pct, written/(1024*1024), total/(1024*1024))
					lastPct = pct
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			f.Close()
			return readErr
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close download file: %w", err)
	}

	// Verify completeness: if server declared Content-Length, bytes must match.
	if total > 0 && written != total {
		os.Remove(dst)
		return fmt.Errorf("download incomplete: got %d bytes, expected %d", written, total)
	}

	computedHash := hex.EncodeToString(hasher.Sum(nil))
	logutil.Info("synapses: downloaded %s (%d bytes, sha256:%s)\n",
		filepath.Base(dst), written, computedHash[:16])

	// Fetch the published .sha256 checksum file and verify integrity.
	// Fail closed: if checksum file is unavailable or malformed, refuse the download.
	checksumURL := url + ".sha256"
	checksumResp, checksumErr := client.Get(checksumURL)
	if checksumErr != nil {
		os.Remove(dst)
		return fmt.Errorf("integrity check: failed to fetch checksum file %s: %w", filepath.Base(checksumURL), checksumErr)
	}
	defer checksumResp.Body.Close()
	if checksumResp.StatusCode != http.StatusOK {
		os.Remove(dst)
		return fmt.Errorf("integrity check: checksum file %s returned HTTP %d — refusing unverified download", filepath.Base(checksumURL), checksumResp.StatusCode)
	}
	checksumBody, readErr := io.ReadAll(io.LimitReader(checksumResp.Body, 256))
	if readErr != nil {
		os.Remove(dst)
		return fmt.Errorf("integrity check: failed to read checksum file: %w", readErr)
	}
	expectedHash := strings.Fields(strings.TrimSpace(string(checksumBody)))
	if len(expectedHash) == 0 || len(expectedHash[0]) != 64 {
		os.Remove(dst)
		return fmt.Errorf("integrity check: malformed checksum file (expected 64-char hex hash)")
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(computedHash)), []byte(strings.ToLower(expectedHash[0]))) != 1 {
		os.Remove(dst)
		return fmt.Errorf("integrity check failed: expected sha256 %s, got %s", expectedHash[0], computedHash)
	}
	logutil.Info("synapses: sha256 checksum verified against %s\n", filepath.Base(checksumURL))
	return nil
}

// extractBinary pulls the "synapses" (or "synapses.exe") binary from an archive.
func extractBinary(archivePath, destDir string) (string, error) {
	binaryName := "synapses"
	if runtime.GOOS == "windows" {
		binaryName = "synapses.exe"
	}

	if strings.HasSuffix(archivePath, ".tar.gz") || strings.HasSuffix(archivePath, ".tgz") {
		return extractFromTarGz(archivePath, destDir, binaryName)
	}
	if strings.HasSuffix(archivePath, ".zip") {
		return extractFromZip(archivePath, destDir, binaryName)
	}
	return "", fmt.Errorf("unknown archive format: %s", filepath.Base(archivePath))
}

func extractFromTarGz(archivePath, destDir, binaryName string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip open: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("tar read: %w", err)
		}
		// Match the binary name (could be "synapses" or "./synapses" or "dist/synapses").
		base := filepath.Base(hdr.Name)
		if base == binaryName && hdr.Typeflag == tar.TypeReg {
			outPath := filepath.Join(destDir, binaryName)
			out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, 500<<20)); err != nil { // 500MB cap
				out.Close()
				return "", err
			}
			out.Close()
			return outPath, nil
		}
	}
	return "", fmt.Errorf("binary %q not found in archive", binaryName)
}

func extractFromZip(archivePath, destDir, binaryName string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("zip open: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		if filepath.Base(f.Name) == binaryName && !f.FileInfo().IsDir() {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			outPath := filepath.Join(destDir, binaryName)
			out, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				rc.Close()
				return "", err
			}
			if _, err := io.Copy(out, io.LimitReader(rc, 500<<20)); err != nil {
				out.Close()
				rc.Close()
				return "", err
			}
			out.Close()
			rc.Close()
			return outPath, nil
		}
	}
	return "", fmt.Errorf("binary %q not found in archive", binaryName)
}

// ── Binary replacement ──────────────────────────────────────────────────────

// applySelfUpdateFromPath atomically replaces the running executable with newBinary.
// It writes to a .new temp file, then renames over the original.
// On failure it cleans up and leaves the original intact.
func applySelfUpdateFromPath(newBinary string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine executable path: %w", err)
	}
	// Resolve symlinks so we replace the actual file, not the link.
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	data, err := os.ReadFile(newBinary)
	if err != nil {
		return fmt.Errorf("read new binary: %w", err)
	}

	// Write to a temp file in the same directory (ensures same filesystem for rename).
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		// Wrap permission errors for better CLI messaging.
		if os.IsPermission(err) {
			return &os.PathError{Op: "write", Path: exe, Err: os.ErrPermission}
		}
		return fmt.Errorf("write temp binary: %w", err)
	}

	// Atomic rename.
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		if os.IsPermission(err) {
			return &os.PathError{Op: "rename", Path: exe, Err: os.ErrPermission}
		}
		return fmt.Errorf("rename new binary into place: %w", err)
	}

	return nil
}

// ── Semver comparison ───────────────────────────────────────────────────────

// compareSemver returns >0 if a > b, 0 if equal, <0 if a < b.
// Handles "0.8.1", "0.8.1-rc1" (pre-release sorts lower).
func compareSemver(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	// Split off pre-release suffix.
	aParts, aPre := splitPrerelease(a)
	bParts, bPre := splitPrerelease(b)

	aVer := parseSemverParts(aParts)
	bVer := parseSemverParts(bParts)

	for i := 0; i < 3; i++ {
		ai, bi := 0, 0
		if i < len(aVer) {
			ai = aVer[i]
		}
		if i < len(bVer) {
			bi = bVer[i]
		}
		if ai != bi {
			return ai - bi
		}
	}

	// Equal version numbers: pre-release < release (e.g. 0.8.1-rc1 < 0.8.1).
	if aPre != "" && bPre == "" {
		return -1
	}
	if aPre == "" && bPre != "" {
		return 1
	}
	return strings.Compare(aPre, bPre)
}

func splitPrerelease(v string) (string, string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

func parseSemverParts(v string) []int {
	parts := strings.Split(v, ".")
	result := make([]int, len(parts))
	for i, p := range parts {
		n, _ := strconv.Atoi(p)
		result[i] = n
	}
	return result
}

// ── Platform asset name ─────────────────────────────────────────────────────

// platformAssetName returns the expected GitHub release asset name for the
// current OS/arch. Must match what the release workflow produces.
func platformAssetName() string {
	os_ := runtime.GOOS
	arch := runtime.GOARCH

	switch {
	case os_ == "darwin" && arch == "arm64":
		return "synapses_darwin_arm64.tar.gz"
	case os_ == "darwin" && arch == "amd64":
		return "synapses_darwin_x86_64.tar.gz"
	case os_ == "linux" && arch == "amd64":
		return "synapses_linux_amd64.tar.gz"
	case os_ == "linux" && arch == "arm64":
		return "synapses_linux_arm64.tar.gz"
	case os_ == "windows" && arch == "amd64":
		return "synapses_windows_x86_64.zip"
	default:
		return fmt.Sprintf("synapses_%s_%s.tar.gz", os_, arch)
	}
}

// ── State persistence ───────────────────────────────────────────────────────

func loadUpdateState(path string) *UpdateState {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var state UpdateState
	if json.Unmarshal(data, &state) != nil {
		return nil
	}
	return &state
}

// saveUpdateState persists state atomically (write temp + rename).
func saveUpdateState(state *UpdateState) {
	home, err := synapsesHome()
	if err != nil {
		return
	}
	path := filepath.Join(home, "update_state.json")
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	// Atomic write: temp file + rename prevents corrupt JSON on crash.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}
