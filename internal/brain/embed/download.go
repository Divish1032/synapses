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
	"b5618:darwin/arm64":  "", // TODO: pin hash after first verified download
	"b5618:darwin/amd64":  "",
	"b5618:linux/amd64":   "",
	"b5618:linux/arm64":   "",
	"b5618:windows/amd64": "",
}

// embedModelSHA256 maps model filenames to expected SHA-256 hashes.
var embedModelSHA256 = map[string]string{
	"nomic-embed-text-v1.5.Q4_K_M.gguf": "", // TODO: pin hash after first verified download
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
	logProgress(opts.Progress, "llama-server installed: %s", binPath)
	return binPath, nil
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

	// Verify SHA-256 integrity — always read back and check.
	tmpData, readErr := os.ReadFile(tmpPath)
	if readErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("read downloaded model for verification: %w", readErr)
	}
	if err := verifyOrLogSHA256(tmpData, embedModelSHA256[filename], "embed model", opts.Progress); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

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
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		artifact = fmt.Sprintf("llama-%s-bin-ubuntu-arm64.zip", version)
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		artifact = fmt.Sprintf("llama-%s-bin-win-avx2-x64.zip", version)
	default:
		return "", fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf(
		"https://github.com/ggerganov/llama.cpp/releases/download/%s/%s",
		version, artifact,
	), nil
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
		isLib := strings.HasSuffix(base, ".dylib") || strings.HasSuffix(base, ".so")
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
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
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

	pr := &progressReader{r: resp.Body, total: resp.ContentLength, w: w}
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
