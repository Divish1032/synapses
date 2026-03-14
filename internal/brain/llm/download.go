package llm

// download.go — GGUF model auto-download from HuggingFace.
//
// Downloads a GGUF file from a public HuggingFace repository to the local
// model directory (~/.synapses/models/ by default). Uses atomic writes
// (download to .tmp, rename on success) so interrupted downloads never leave
// a corrupt file behind.
//
// No authentication is required for public HuggingFace repositories.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// HFBaseURL is the HuggingFace resolve endpoint for file downloads.
const HFBaseURL = "https://huggingface.co"

// DownloadConfig holds parameters for a GGUF download.
type DownloadConfig struct {
	// Repo is the HuggingFace repo, e.g. "divish/sil-coder"
	Repo string
	// Filename is the GGUF file name within the repo, e.g. "sil-coder-Q5_K_M.gguf"
	Filename string
	// DestDir is the local directory to save to. Created if it doesn't exist.
	DestDir string
	// Progress is an optional writer for progress messages. May be nil.
	Progress io.Writer
}

// DestPath returns the full local path where the GGUF will be saved.
func (d DownloadConfig) DestPath() string {
	return filepath.Join(d.DestDir, d.Filename)
}

// URL returns the HuggingFace download URL for this file.
func (d DownloadConfig) URL() string {
	return fmt.Sprintf("%s/%s/resolve/main/%s", HFBaseURL, d.Repo, d.Filename)
}

// GGUFExists returns true if the GGUF file already exists on disk.
func GGUFExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

// DownloadGGUF downloads a GGUF model file from HuggingFace if it doesn't
// already exist locally. Returns the local path on success.
//
// Progress messages are written to cfg.Progress (if non-nil) in the format:
//
//	Downloading sil-coder-Q5_K_M.gguf from huggingface.co/divish/sil-coder
//	 500 MB / 6.5 GB (7%)
//	 ...
//	Download complete: /Users/you/.synapses/models/sil-coder-Q5_K_M.gguf
func DownloadGGUF(ctx context.Context, cfg DownloadConfig) (string, error) {
	dest := cfg.DestPath()

	// Fast path: already downloaded.
	if GGUFExists(dest) {
		logf(cfg.Progress, "Model already exists: %s\n", dest)
		return dest, nil
	}

	if cfg.Repo == "" {
		return "", fmt.Errorf("download: hf_repo is not configured — run: brain config hf-repo <username/repo>")
	}

	// Create destination directory.
	if err := os.MkdirAll(cfg.DestDir, 0o755); err != nil {
		return "", fmt.Errorf("download: create model dir %q: %w", cfg.DestDir, err)
	}

	url := cfg.URL()
	logf(cfg.Progress, "Downloading %s\n  from %s\n  to   %s\n", cfg.Filename, url, dest)

	// HTTP GET with context.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("download: build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("download: file not found on HuggingFace — check hf_repo=%q and hf_filename=%q", cfg.Repo, cfg.Filename)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: unexpected HTTP %d from %s", resp.StatusCode, url)
	}

	totalBytes := resp.ContentLength // -1 if unknown

	// Atomic write: download to .tmp file, rename on success.
	tmpPath := dest + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return "", fmt.Errorf("download: create temp file: %w", err)
	}

	// Wrap body with progress reporter.
	reader := &progressReader{
		r:     resp.Body,
		total: totalBytes,
		w:     cfg.Progress,
		name:  cfg.Filename,
	}

	if _, err := io.Copy(f, reader); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("download: write %q: %w", tmpPath, err)
	}
	f.Close()

	// Atomic rename.
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("download: rename to %q: %w", dest, err)
	}

	logf(cfg.Progress, "\nDownload complete: %s\n", dest)
	return dest, nil
}

// progressReader wraps an io.Reader and prints download progress.
type progressReader struct {
	r         io.Reader
	total     int64
	read      int64
	w         io.Writer
	name      string
	lastPrint int64
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.read += int64(n)

	// Print every 50 MB.
	if p.w != nil && p.read-p.lastPrint >= 50*1024*1024 {
		p.lastPrint = p.read
		if p.total > 0 {
			pct := p.read * 100 / p.total
			logf(p.w, "  %s / %s (%d%%)\n",
				humanBytes(p.read), humanBytes(p.total), pct)
		} else {
			logf(p.w, "  %s downloaded\n", humanBytes(p.read))
		}
	}
	return n, err
}

// humanBytes formats bytes as a human-readable string.
func humanBytes(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%d MB", b/MB)
	default:
		return fmt.Sprintf("%d KB", b/KB)
	}
}

// logf writes a formatted message to w if w is non-nil.
func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	// Ensure lines end with \n.
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprint(w, msg)
}
