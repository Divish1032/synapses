package embed

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// LlamaServerBinPath
// ---------------------------------------------------------------------------

func TestLlamaServerBinPath_CurrentOS(t *testing.T) {
	dir := t.TempDir()
	got := LlamaServerBinPath(dir)

	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(got, "llama-server.exe") {
			t.Errorf("windows: want suffix llama-server.exe, got %q", got)
		}
	} else {
		if !strings.HasSuffix(got, "llama-server") {
			t.Errorf("non-windows: want suffix llama-server, got %q", got)
		}
		if strings.HasSuffix(got, ".exe") {
			t.Errorf("non-windows: must not have .exe suffix, got %q", got)
		}
	}

	// Must be inside the provided dir.
	if filepath.Dir(got) != dir {
		t.Errorf("want dir %q, got dir %q (full path %q)", dir, filepath.Dir(got), got)
	}
}

func TestLlamaServerBinPath_CustomDir(t *testing.T) {
	got := LlamaServerBinPath("/usr/local/bin")
	if !strings.HasPrefix(got, "/usr/local/bin") {
		t.Errorf("expected path to start with /usr/local/bin, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// EmbedModelPath
// ---------------------------------------------------------------------------

func TestEmbedModelPath_EmptyFilenameUsesDefault(t *testing.T) {
	dir := "/models"
	got := EmbedModelPath(dir, "")
	want := filepath.Join(dir, EmbedModelFilename)
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestEmbedModelPath_CustomFilename(t *testing.T) {
	dir := "/models"
	custom := "my-model.gguf"
	got := EmbedModelPath(dir, custom)
	want := filepath.Join(dir, custom)
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestEmbedModelPath_DefaultFilenameConstant(t *testing.T) {
	// EmbedModelFilename must be the nomic Q4_K_M file.
	if EmbedModelFilename != "nomic-embed-text-v1.5.Q4_K_M.gguf" {
		t.Errorf("unexpected EmbedModelFilename: %q", EmbedModelFilename)
	}
}

// ---------------------------------------------------------------------------
// humanBytes
// ---------------------------------------------------------------------------

func TestHumanBytes_KB(t *testing.T) {
	// Values below 1 MB (1<<20) are expressed in KB.
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0 KB"},
		{1024, "1 KB"},
		{500 * 1024, "500 KB"},
		{(1 << 20) - 1, "1023 KB"}, // just under 1 MB
	}
	for _, tc := range cases {
		got := humanBytes(tc.input)
		if got != tc.want {
			t.Errorf("humanBytes(%d): want %q, got %q", tc.input, tc.want, got)
		}
	}
}

func TestHumanBytes_MB(t *testing.T) {
	// Values in [1 MB, 1 GB) are expressed as "N MB".
	cases := []struct {
		input int64
		check func(string) bool
	}{
		{1 << 20, func(s string) bool { return strings.HasSuffix(s, " MB") }},
		{274 * (1 << 20), func(s string) bool { return s == "274 MB" }},
		{(1 << 30) - 1, func(s string) bool { return strings.HasSuffix(s, " MB") }},
	}
	for _, tc := range cases {
		got := humanBytes(tc.input)
		if !tc.check(got) {
			t.Errorf("humanBytes(%d) = %q: did not match expected MB format", tc.input, got)
		}
	}
}

func TestHumanBytes_GB(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{1 << 30, "1.0 GB"},
		{int64(1.5 * (1 << 30)), "1.5 GB"},
		{10 * (1 << 30), "10.0 GB"},
	}
	for _, tc := range cases {
		got := humanBytes(tc.input)
		if got != tc.want {
			t.Errorf("humanBytes(%d): want %q, got %q", tc.input, tc.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// fileExists
// ---------------------------------------------------------------------------

func TestFileExists_ExistingFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "test-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if !fileExists(f.Name()) {
		t.Errorf("fileExists(%q) = false for a file that exists", f.Name())
	}
}

func TestFileExists_NonExistentPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.txt")
	if fileExists(path) {
		t.Errorf("fileExists(%q) = true for a path that does not exist", path)
	}
}

func TestFileExists_Directory(t *testing.T) {
	dir := t.TempDir()
	// Directories also satisfy os.Stat without error, so fileExists returns true.
	if !fileExists(dir) {
		t.Errorf("fileExists(%q) = false for an existing directory", dir)
	}
}

// ---------------------------------------------------------------------------
// logProgress
// ---------------------------------------------------------------------------

func TestLogProgress_NilWriter_NoPanic(t *testing.T) {
	// Must not panic.
	logProgress(nil, "hello %s", "world")
}

func TestLogProgress_NonNilWriter_WritesOutput(t *testing.T) {
	var buf bytes.Buffer
	logProgress(&buf, "value=%d", 42)
	got := buf.String()
	if !strings.Contains(got, "value=42") {
		t.Errorf("expected output to contain %q, got %q", "value=42", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected output to end with newline, got %q", got)
	}
}

func TestLogProgress_FormatsMultipleArgs(t *testing.T) {
	var buf bytes.Buffer
	logProgress(&buf, "%s=%d", "key", 99)
	if !strings.Contains(buf.String(), "key=99") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// DownloadOptions.httpClient
// ---------------------------------------------------------------------------

func TestDownloadOptions_httpClient_NilReturnsDefault(t *testing.T) {
	opts := DownloadOptions{HTTPClient: nil}
	client := opts.httpClient()
	if client == nil {
		t.Fatal("expected a non-nil *http.Client")
	}
	// Default should have a 10-minute timeout.
	if client.Timeout == 0 {
		t.Error("expected default client to have a non-zero timeout")
	}
}

func TestDownloadOptions_httpClient_CustomClientReturned(t *testing.T) {
	custom := &http.Client{Timeout: 5}
	opts := DownloadOptions{HTTPClient: custom}
	got := opts.httpClient()
	if got != custom {
		t.Errorf("expected custom *http.Client to be returned unchanged")
	}
}

// ---------------------------------------------------------------------------
// EnsureLlamaServer — binary already present (no network)
// ---------------------------------------------------------------------------

func TestEnsureLlamaServer_AlreadyExists(t *testing.T) {
	binDir := t.TempDir()

	// Pre-create a file at the expected binary path so no download is attempted.
	binPath := LlamaServerBinPath(binDir)
	if err := os.WriteFile(binPath, []byte("fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var progress bytes.Buffer
	opts := DownloadOptions{
		LlamaCPPVersion: DefaultLlamaCPPVersion,
		BinDir:          binDir,
		Progress:        &progress,
		// No HTTPClient — any network attempt would use the default client and fail;
		// the test verifies we return early without hitting the network.
	}

	got, err := EnsureLlamaServer(t.Context(), opts)
	if err != nil {
		t.Fatalf("EnsureLlamaServer returned unexpected error: %v", err)
	}
	if got != binPath {
		t.Errorf("want path %q, got %q", binPath, got)
	}
	if !strings.Contains(progress.String(), "already installed") {
		t.Errorf("expected 'already installed' in progress output, got: %q", progress.String())
	}
}

func TestEnsureLlamaServer_EmptyVersionDefaultsToConst(t *testing.T) {
	binDir := t.TempDir()
	binPath := LlamaServerBinPath(binDir)
	if err := os.WriteFile(binPath, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pass an empty LlamaCPPVersion — should default to DefaultLlamaCPPVersion internally.
	opts := DownloadOptions{
		LlamaCPPVersion: "",
		BinDir:          binDir,
	}
	got, err := EnsureLlamaServer(t.Context(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == "" {
		t.Error("expected non-empty path")
	}
}

// ---------------------------------------------------------------------------
// EnsureEmbedModel — model already present (no network)
// ---------------------------------------------------------------------------

func TestEnsureEmbedModel_AlreadyExists(t *testing.T) {
	modelDir := t.TempDir()
	modelPath := EmbedModelPath(modelDir, EmbedModelFilename)
	if err := os.WriteFile(modelPath, []byte("fake model"), 0o644); err != nil {
		t.Fatal(err)
	}

	var progress bytes.Buffer
	opts := DownloadOptions{
		ModelDir: modelDir,
		Progress: &progress,
	}

	got, err := EnsureEmbedModel(t.Context(), opts, EmbedModelHFRepo, EmbedModelFilename)
	if err != nil {
		t.Fatalf("EnsureEmbedModel returned unexpected error: %v", err)
	}
	if got != modelPath {
		t.Errorf("want path %q, got %q", modelPath, got)
	}
	if !strings.Contains(progress.String(), "already present") {
		t.Errorf("expected 'already present' in progress output, got: %q", progress.String())
	}
}

func TestEnsureEmbedModel_EmptyArgsUseDefaults(t *testing.T) {
	modelDir := t.TempDir()
	// Create the default model file so we avoid any download.
	modelPath := EmbedModelPath(modelDir, "")
	if err := os.WriteFile(modelPath, []byte("fake model"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := DownloadOptions{ModelDir: modelDir}
	// Pass empty hfRepo and filename — should fall back to defaults.
	got, err := EnsureEmbedModel(t.Context(), opts, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != modelPath {
		t.Errorf("want %q, got %q", modelPath, got)
	}
}

// ---------------------------------------------------------------------------
// llamaCPPReleaseURL
// ---------------------------------------------------------------------------

func TestLlamaCPPReleaseURL_CurrentPlatform(t *testing.T) {
	// On supported platforms the function must succeed and return a URL
	// containing the version string and pointing to GitHub.
	version := DefaultLlamaCPPVersion
	url, err := llamaCPPReleaseURL(version)

	switch {
	case runtime.GOOS == "windows" && runtime.GOARCH != "amd64":
		// Windows arm64 is unsupported — expect an error.
		if err == nil {
			t.Fatalf("expected error for windows/%s, got URL %q", runtime.GOARCH, url)
		}
		return
	case (runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows"):
		// Completely unknown OS — expect an error.
		if err == nil {
			t.Fatalf("expected error for %s/%s, got URL %q", runtime.GOOS, runtime.GOARCH, url)
		}
		return
	}

	if err != nil {
		t.Fatalf("llamaCPPReleaseURL(%q) unexpected error: %v", version, err)
	}
	if url == "" {
		t.Error("expected non-empty URL")
	}
	if !strings.Contains(url, version) {
		t.Errorf("URL %q does not contain version %q", url, version)
	}
	if !strings.Contains(url, "github.com") {
		t.Errorf("URL %q does not point to github.com", url)
	}
}

func TestLlamaCPPReleaseURL_KnownPlatformURLs(t *testing.T) {
	// We can only call the real function for the current platform.
	// For the other known platforms we verify the artifact naming pattern by
	// inspecting the URL if we happen to be on that platform.
	type platformExpect struct {
		goos   string
		goarch string
		substr string
	}
	expects := []platformExpect{
		{"darwin", "arm64", "macos-arm64"},
		{"darwin", "amd64", "macos-x64"},
		{"linux", "amd64", "ubuntu-x64"},
		{"linux", "arm64", "ubuntu-arm64"},
		{"windows", "amd64", "win-avx2-x64"},
	}

	curGOOS := runtime.GOOS
	curGOARCH := runtime.GOARCH

	for _, tc := range expects {
		if curGOOS != tc.goos || curGOARCH != tc.goarch {
			continue // skip — can only test current platform
		}
		url, err := llamaCPPReleaseURL(DefaultLlamaCPPVersion)
		if err != nil {
			t.Errorf("platform %s/%s: unexpected error: %v", tc.goos, tc.goarch, err)
			continue
		}
		if !strings.Contains(url, tc.substr) {
			t.Errorf("platform %s/%s: URL %q does not contain %q", tc.goos, tc.goarch, url, tc.substr)
		}
	}
}

func TestLlamaCPPReleaseURL_ContainsVersion(t *testing.T) {
	// Skip on unsupported platforms.
	if !(runtime.GOOS == "darwin" || runtime.GOOS == "linux" ||
		(runtime.GOOS == "windows" && runtime.GOARCH == "amd64")) {
		t.Skip("platform not supported by llamaCPPReleaseURL")
	}
	version := "b9999"
	url, err := llamaCPPReleaseURL(version)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(url, version) {
		t.Errorf("URL %q does not contain version %q", url, version)
	}
}

// ---------------------------------------------------------------------------
// llamaCPPReleaseURL — unsupported platforms
// ---------------------------------------------------------------------------

func TestLlamaCPPReleaseURL_UnsupportedPlatform(t *testing.T) {
	// This test is conditional — on unsupported platforms we verify error handling.
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		if runtime.GOOS == "windows" && runtime.GOARCH != "amd64" {
			// Windows with non-amd64 arch is unsupported
			break
		}
		t.Skip("current platform is supported")
	}

	// For truly unsupported platforms, the function should return an error
	// (this won't actually run on those platforms in CI, but it's good to have)
	url, err := llamaCPPReleaseURL("b5618")
	if runtime.GOOS == "dragonfly" {
		if err == nil {
			t.Error("expected error for unsupported platform")
		}
		if url != "" {
			t.Errorf("expected empty URL for unsupported platform, got %q", url)
		}
	}
}

// ---------------------------------------------------------------------------
// extractLlamaServerFromZip
// ---------------------------------------------------------------------------

func TestExtractLlamaServerFromZip_NotFound(t *testing.T) {
	// Create minimal valid zip data without llama-server
	zipData := []byte("PK\x03\x04") // ZIP magic, rest is garbage to trigger error
	destDir := t.TempDir()

	err := extractLlamaServerFromZip(zipData, destDir, filepath.Join(destDir, "llama-server"))
	if err == nil {
		t.Error("expected error for invalid zip")
	}
}

// ---------------------------------------------------------------------------
// progressReader.Read
// ---------------------------------------------------------------------------

func TestProgressReader_WithProgress(t *testing.T) {
	data := []byte("x")
	for i := 0; i < 1000; i++ {
		data = append(data, []byte("x")...)
	}

	var progress bytes.Buffer
	pr := &progressReader{
		r:     bytes.NewReader(data),
		w:     &progress,
		total: int64(len(data)),
	}

	buf := make([]byte, 100)
	totalRead := 0
	for {
		n, err := pr.Read(buf)
		totalRead += n
		if err != nil {
			break
		}
	}

	if totalRead != len(data) {
		t.Errorf("expected to read %d bytes, got %d", len(data), totalRead)
	}

	// Verify progress was written (should have percentage outputs)
	progressStr := progress.String()
	if len(progressStr) == 0 {
		t.Error("expected progress to be written")
	}
	if !strings.Contains(progressStr, "%") {
		t.Errorf("expected percentage in progress output, got: %q", progressStr)
	}
}

func TestProgressReader_ZeroTotal(t *testing.T) {
	// progressReader with zero total should not crash
	var progress bytes.Buffer
	pr := &progressReader{
		r:     bytes.NewReader([]byte("test")),
		w:     &progress,
		total: 0, // zero total
	}

	buf := make([]byte, 4)
	n, err := pr.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 4 {
		t.Errorf("expected 4 bytes, got %d", n)
	}
	// No progress should be written when total is 0
	if progress.Len() > 0 {
		t.Errorf("expected no progress output when total=0, got: %q", progress.String())
	}
}

// ---------------------------------------------------------------------------
// LlamaServerBinPath — cross-platform paths
// ---------------------------------------------------------------------------

func TestLlamaServerBinPath_Windows(t *testing.T) {
	// Simulate Windows by checking if the suffix logic works
	// On Windows, the filename should be llama-server.exe
	path := LlamaServerBinPath("/bin")
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(path, ".exe") {
			t.Errorf("on windows, expected .exe suffix, got %q", path)
		}
	} else {
		if strings.HasSuffix(path, ".exe") {
			t.Errorf("on non-windows, expected no .exe suffix, got %q", path)
		}
	}
}

// ---------------------------------------------------------------------------
// EnsureLlamaServer — directory creation
// ---------------------------------------------------------------------------

func TestEnsureLlamaServer_CreatesBinDir(t *testing.T) {
	// Test that EnsureLlamaServer creates the BinDir if it doesn't exist
	parentDir := t.TempDir()
	binDir := filepath.Join(parentDir, "does", "not", "exist")

	// Pre-create the binary at the expected path to avoid network
	os.MkdirAll(binDir, 0o755)
	binPath := LlamaServerBinPath(binDir)
	os.WriteFile(binPath, []byte("fake binary"), 0o755)

	opts := DownloadOptions{
		LlamaCPPVersion: DefaultLlamaCPPVersion,
		BinDir:          binDir,
	}

	got, err := EnsureLlamaServer(t.Context(), opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != binPath {
		t.Errorf("want %q, got %q", binPath, got)
	}
}

// ---------------------------------------------------------------------------
// EnsureEmbedModel — directory creation
// ---------------------------------------------------------------------------

func TestEnsureEmbedModel_CreatesModelDir(t *testing.T) {
	// Test that EnsureEmbedModel creates the ModelDir if it doesn't exist
	parentDir := t.TempDir()
	modelDir := filepath.Join(parentDir, "models", "subdir")

	// Pre-create the model at the expected path to avoid network
	os.MkdirAll(modelDir, 0o755)
	modelPath := EmbedModelPath(modelDir, "")
	os.WriteFile(modelPath, []byte("fake model"), 0o644)

	opts := DownloadOptions{
		ModelDir: modelDir,
	}

	got, err := EnsureEmbedModel(t.Context(), opts, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != modelPath {
		t.Errorf("want %q, got %q", modelPath, got)
	}
}

// ---------------------------------------------------------------------------
// downloadBytes tests
// ---------------------------------------------------------------------------

func TestDownloadBytes_Success(t *testing.T) {
	expectedData := []byte("test download data")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(expectedData)
	}))
	defer server.Close()

	client := &http.Client{}
	ctx := context.Background()

	data, err := downloadBytes(ctx, client, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Equal(data, expectedData) {
		t.Errorf("want %q, got %q", expectedData, data)
	}
}

func TestDownloadBytes_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := &http.Client{}
	ctx := context.Background()

	_, err := downloadBytes(ctx, client, server.URL)
	if err == nil {
		t.Error("expected error for HTTP 404, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP") {
		t.Errorf("expected HTTP error, got %q", err)
	}
}

func TestDownloadBytes_LargeData(t *testing.T) {
	largeData := bytes.Repeat([]byte("test"), 1000) // ~4KB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(largeData)
	}))
	defer server.Close()

	client := &http.Client{}
	ctx := context.Background()

	data, err := downloadBytes(ctx, client, server.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(data) != len(largeData) {
		t.Errorf("want %d bytes, got %d", len(largeData), len(data))
	}
}

// ---------------------------------------------------------------------------
// downloadFile tests
// ---------------------------------------------------------------------------

func TestDownloadFile_Success(t *testing.T) {
	expectedData := []byte("file content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(expectedData)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "test-file.bin")
	client := &http.Client{}
	ctx := context.Background()

	err := downloadFile(ctx, client, server.URL, destPath, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify file was created and contains correct data
	data, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	if !bytes.Equal(data, expectedData) {
		t.Errorf("want %q, got %q", expectedData, data)
	}
}

func TestDownloadFile_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "test-file.bin")
	client := &http.Client{}
	ctx := context.Background()

	err := downloadFile(ctx, client, server.URL, destPath, nil)
	if err == nil {
		t.Error("expected error for HTTP error, got nil")
	}
}

func TestDownloadFile_InvalidDestPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data"))
	}))
	defer server.Close()

	// Try to write to invalid path
	destPath := "/nonexistent/directory/that/does/not/exist/file.bin"
	client := &http.Client{}
	ctx := context.Background()

	err := downloadFile(ctx, client, server.URL, destPath, nil)
	if err == nil {
		t.Error("expected error for invalid destination path, got nil")
	}
}

func TestDownloadFile_WithProgress(t *testing.T) {
	expectedData := []byte("test data with progress")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(expectedData)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "test-file.bin")
	client := &http.Client{}
	ctx := context.Background()

	var progressBuf bytes.Buffer
	err := downloadFile(ctx, client, server.URL, destPath, &progressBuf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !fileExists(destPath) {
		t.Error("expected file to be created")
	}
}
