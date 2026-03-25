package embed

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// llamaServerSHA256 maps (version, GOOS/GOARCH) to the expected SHA-256 hash
// of the downloaded zip artifact. Update these when bumping DefaultLlamaCPPVersion.
var llamaServerSHA256 = map[string]string{
	"b5618:darwin/arm64":  "b9c5548e43e712b7528c1c553e0f5ab670e36eba50e81a98d0ccb449501da7c4",
	"b5618:darwin/amd64":  "3025e9469e9c743881abc5f11875356c84ffa97c5c5a4cff1d60b9ca508553df",
	"b5618:linux/amd64":   "cb14b8a80d045cb20a8c6c7f0efd234f255b504e368de59401a290bbea9967df",
	"b5618:windows/amd64": "ae87ccf08c0f548597abf9c304c121c71fc09bbe757bfae7b2ea78e3d47d2c9a",
}

// embedModelSHA256 maps model filenames to expected SHA-256 hashes.
var embedModelSHA256 = map[string]string{
	"nomic-embed-text-v1.5.Q4_K_M.gguf": "d4e388894e09cf3816e8b0896d81d265b55e7a9fff9ab03fe8bf4ef5e11295ac",
}

// verifyOrLogSHA256 checks download data against an expected hash. If expected is
// empty, logs the hash for pinning. Returns error on mismatch.
func verifyOrLogSHA256(data []byte, expected string, label string, progress io.Writer) error {
	actual := sha256.Sum256(data)
	actualHex := hex.EncodeToString(actual[:])
	if expected == "" {
		return fmt.Errorf("%s integrity check failed: no expected sha256 hash configured (actual: %s) — pin this hash before shipping", label, actualHex)
	}
	if subtle.ConstantTimeCompare([]byte(actualHex), []byte(expected)) != 1 {
		return fmt.Errorf("%s integrity check failed: expected sha256 %s, got %s", label, expected, actualHex)
	}
	logProgress(progress, "SHA-256 verified: %s", actualHex[:16])
	return nil
}

const (
	// DefaultLlamaCPPVersion is the pinned llama.cpp release used for binary downloads.
	// Update this constant to pull newer builds.
	DefaultLlamaCPPVersion = "b5618"

	// EmbedModelHFRepo is the HuggingFace repository for nomic-embed-text.
	EmbedModelHFRepo = "nomic-ai/nomic-embed-text-v1.5-GGUF"

	// EmbedModelFilename is the Q4_K_M quantisation — best quality/size tradeoff
	// (~274 MB). Runs at ~5 ms/embed on Apple Silicon M2.
	EmbedModelFilename = "nomic-embed-text-v1.5.Q4_K_M.gguf"
)

// DownloadOptions configures a binary/model download.
type DownloadOptions struct {
	// LlamaCPPVersion is the llama.cpp release tag, e.g. "b5618".
	LlamaCPPVersion string
	// BinDir is the directory to install the llama-server binary.
	BinDir string
	// ModelDir is the directory to save the GGUF model file.
	ModelDir string
	// Progress receives human-readable progress lines (may be nil).
	Progress io.Writer
	// HTTPClient is used for downloads (nil → default with 10-min timeout).
	HTTPClient *http.Client
}

// LlamaServerBinPath returns the expected path of the llama-server binary.
func LlamaServerBinPath(binDir string) string {
	name := "llama-server"
	if runtime.GOOS == "windows" {
		name = "llama-server.exe"
	}
	return filepath.Join(binDir, name)
}

// EmbedModelPath returns the expected path of the embedding GGUF model.
func EmbedModelPath(modelDir, filename string) string {
	if filename == "" {
		filename = EmbedModelFilename
	}
	return filepath.Join(modelDir, filename)
}

// EnsureLlamaServer checks whether the llama-server binary exists at binDir;
// if not, downloads and extracts it from the GitHub release.
// Returns the binary path.
func EnsureLlamaServer(ctx context.Context, opts DownloadOptions) (string, error) {
	if opts.LlamaCPPVersion == "" {
		opts.LlamaCPPVersion = DefaultLlamaCPPVersion
	}
	if err := os.MkdirAll(opts.BinDir, 0o755); err != nil {
		return "", fmt.Errorf("create bin dir: %w", err)
	}

	binPath := LlamaServerBinPath(opts.BinDir)
	if fileExists(binPath) {
		logProgress(opts.Progress, "llama-server: already installed at %s", binPath)
		// Opportunistically backfill the SHA-256 sidecar if the binary exists but
		// the sidecar does not. This activates pre-execution integrity checks for
		// binaries installed before this feature shipped without requiring a full
		// re-download.
		if !fileExists(binPath + ".sha256") {
			if err := writeSHA256Sidecar(binPath); err != nil {
				logProgress(opts.Progress, "warning: failed to write SHA-256 sidecar for llama-server: %v", err)
			}
		}
		return binPath, nil
	}

	url, err := llamaCPPReleaseURL(opts.LlamaCPPVersion)
	if err != nil {
		return "", err
	}

	logProgress(opts.Progress, "Downloading llama-server %s (%s/%s)…",
		opts.LlamaCPPVersion, runtime.GOOS, runtime.GOARCH)

	data, err := downloadBytes(ctx, opts.httpClient(), url)
	if err != nil {
		return "", fmt.Errorf("download llama.cpp release: %w", err)
	}

	// Verify SHA-256 integrity when a pinned hash is available.
	hashKey := opts.LlamaCPPVersion + ":" + runtime.GOOS + "/" + runtime.GOARCH
	if err := verifyOrLogSHA256(data, llamaServerSHA256[hashKey], "llama-server", opts.Progress); err != nil {
		return "", err
	}

	logProgress(opts.Progress, "Extracting llama-server from zip (%d MB)…", len(data)/1024/1024)
	if err := extractLlamaServerFromZip(data, opts.BinDir, binPath); err != nil {
		return "", fmt.Errorf("extract llama-server: %w", err)
	}

	// Make executable.
	if err := os.Chmod(binPath, 0o755); err != nil {
		return "", err
	}

	// Write SHA-256 sidecar so Server.Start() can verify integrity before exec.
	// Non-fatal: a missing sidecar degrades to a warning, not a startup failure.
	if err := writeSHA256Sidecar(binPath); err != nil {
		logProgress(opts.Progress, "warning: failed to write SHA-256 sidecar for llama-server: %v", err)
	}

	logProgress(opts.Progress, "llama-server installed: %s", binPath)
	return binPath, nil
}

// hashFileSHA256 returns the hex-encoded SHA-256 digest of the file at path.
// Streams the file to avoid loading large binaries into memory.
func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeSHA256Sidecar computes the SHA-256 of binPath and writes it atomically
// to binPath+".sha256". The atomic rename prevents a partial write from leaving
// a corrupt sidecar that would block future startups.
func writeSHA256Sidecar(binPath string) error {
	digest, err := hashFileSHA256(binPath)
	if err != nil {
		return err
	}
	sidecarPath := binPath + ".sha256"
	// Atomic write: temp file in same directory + rename.
	tmp := sidecarPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(digest), 0o644); err != nil {
		return fmt.Errorf("write sidecar temp: %w", err)
	}
	if err := os.Rename(tmp, sidecarPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename sidecar: %w", err)
	}
	return nil
}

// EnsureEmbedModel checks whether the embedding GGUF model exists at modelDir;
// if not, downloads it from HuggingFace.
// Returns the model path.
func EnsureEmbedModel(ctx context.Context, opts DownloadOptions, hfRepo, filename string) (string, error) {
	if hfRepo == "" {
		hfRepo = EmbedModelHFRepo
	}
	if filename == "" {
		filename = EmbedModelFilename
	}
	if err := os.MkdirAll(opts.ModelDir, 0o755); err != nil {
		return "", fmt.Errorf("create model dir: %w", err)
	}

	modelPath := EmbedModelPath(opts.ModelDir, filename)
	if fileExists(modelPath) {
		logProgress(opts.Progress, "Embedding model: already present at %s", modelPath)
		return modelPath, nil
	}

	url := fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s", hfRepo, filename)
	logProgress(opts.Progress, "Downloading %s from huggingface.co/%s", filename, hfRepo)
	logProgress(opts.Progress, "(~274 MB — this is a one-time download)")

	tmpPath := modelPath + ".tmp"
	if err := downloadFile(ctx, opts.httpClient(), url, tmpPath, opts.Progress); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("download embedding model: %w", err)
	}

	// Verify SHA-256 integrity — stream the file through the hash to avoid
	// loading the entire model (~274 MB) into memory.
	hashFile, readErr := os.Open(tmpPath)
	if readErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("open downloaded model for verification: %w", readErr)
	}
	h := sha256.New()
	if _, readErr = io.Copy(h, hashFile); readErr != nil {
		hashFile.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("hash downloaded model: %w", readErr)
	}
	hashFile.Close()
	actualHex := hex.EncodeToString(h.Sum(nil))
	expected := embedModelSHA256[filename]
	if expected == "" {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("embed model integrity check failed: no expected sha256 hash configured (actual: %s) — pin this hash before shipping", actualHex)
	}
	if subtle.ConstantTimeCompare([]byte(actualHex), []byte(expected)) != 1 {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("embed model integrity check failed: expected sha256 %s, got %s", expected, actualHex)
	}
	logProgress(opts.Progress, "SHA-256 verified: %s", actualHex[:16])

	if err := os.Rename(tmpPath, modelPath); err != nil {
		return "", err
	}
	logProgress(opts.Progress, "Embedding model saved: %s", modelPath)
	return modelPath, nil
}

// llamaCPPReleaseURL returns the GitHub release download URL for the current platform.
func llamaCPPReleaseURL(version string) (string, error) {
	var artifact string
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		artifact = fmt.Sprintf("llama-%s-bin-macos-arm64.zip", version)
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		artifact = fmt.Sprintf("llama-%s-bin-macos-x64.zip", version)
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		artifact = fmt.Sprintf("llama-%s-bin-ubuntu-x64.zip", version)
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		artifact = fmt.Sprintf("llama-%s-bin-win-cpu-x64.zip", version)
	default:
		return "", fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf(
		"https://github.com/ggerganov/llama.cpp/releases/download/%s/%s",
		version, artifact,
	), nil
}

// allowedLibPrefixes lists the shared library name prefixes that may be
// extracted from the llama.cpp release zip. Any library not matching one of
// these prefixes is silently skipped to reduce supply-chain risk.
var allowedLibPrefixes = []string{
	"libllama", "libggml", "libcommon", "llama", "ggml",
}

// isAllowedLib returns true if base matches the allowlist of shared libraries.
func isAllowedLib(base string) bool {
	if !strings.HasSuffix(base, ".dylib") && !strings.HasSuffix(base, ".so") && !strings.Contains(base, ".so.") {
		return false
	}
	lower := strings.ToLower(base)
	for _, pfx := range allowedLibPrefixes {
		if strings.HasPrefix(lower, pfx) {
			return true
		}
	}
	return false
}

// extractLlamaServerFromZip finds and extracts the llama-server binary and any
// accompanying shared libraries (.dylib/.so) from the zip bytes into destDir.
// destPath is the final path for the llama-server binary itself.
func extractLlamaServerFromZip(data []byte, destDir, destPath string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}

	target := "llama-server"
	if runtime.GOOS == "windows" {
		target = "llama-server.exe"
	}

	found := false
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(f.Name)
		isBinary := strings.EqualFold(base, target)
		isLib := isAllowedLib(base)
		if !isBinary && !isLib {
			continue
		}

		outPath := filepath.Join(destDir, base)
		if isBinary {
			outPath = destPath
			found = true
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(outPath)
		if err != nil {
			rc.Close()
			return err
		}
		const maxExtractBytes = 200 << 20 // 200 MiB
		lr := io.LimitReader(rc, maxExtractBytes)
		n, err := io.Copy(out, lr)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
		if n == maxExtractBytes {
			return fmt.Errorf("extracted file %q exceeds 200 MiB size cap", base)
		}
	}
	if !found {
		return fmt.Errorf("llama-server not found in zip (files: %d)", len(r.File))
	}
	return nil
}

// downloadBytes fetches a URL and returns the body as []byte.
func downloadBytes(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	// maxDownloadBytes caps peak memory at ~1 GB (zip in memory + extraction).
	// Streaming extraction would reduce this but actual artifacts are <200 MB.
	// Acceptable given the cap prevents unbounded growth.
	const maxDownloadBytes = 512 << 20
	return io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
}

// downloadFile streams a URL to a local file, writing progress to w.
func downloadFile(ctx context.Context, client *http.Client, url, destPath string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()

	const downloadCap = int64(1 << 30)
	cappedTotal := resp.ContentLength
	if cappedTotal <= 0 || cappedTotal > downloadCap {
		cappedTotal = downloadCap
	}
	pr := &progressReader{r: io.LimitReader(resp.Body, downloadCap), total: cappedTotal, w: w}
	_, err = io.Copy(out, pr)
	return err
}

// progressReader reports download progress.
type progressReader struct {
	r        io.Reader
	w        io.Writer
	total    int64
	received int64
	lastPct  int
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.received += int64(n)
	if p.w != nil && p.total > 0 {
		pct := int(p.received * 100 / p.total)
		if pct/10 > p.lastPct/10 {
			p.lastPct = pct
			fmt.Fprintf(p.w, "  %s / %s (%d%%)\n",
				humanBytes(p.received), humanBytes(p.total), pct)
		}
	}
	return n, err
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", b/1024)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func logProgress(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", args...)
	}
}

func (opts DownloadOptions) httpClient() *http.Client {
	if opts.HTTPClient != nil {
		return opts.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}
