// daemon_serve.go — "synapses daemon serve" subcommand.
//
// Runs ONE singleton daemon per machine. Serves multiple projects
// within a single process. Each project gets its own graph, store,
// file watcher, and Unix socket (for stdio proxy backward compat).
//
// Architecture (after singleton refactor):
//
//	┌─────────────┐   ┌───────────┐
//	│ Claude Code  │   │  Cursor   │    (stdio MCP — backward compat)
//	└──────┬───────┘   └─────┬─────┘
//	       │ stdio             │ stdio
//	       ▼                   ▼
//	┌────────────┐    ┌────────────┐
//	│ mcp proxy  │    │ mcp proxy  │   ← "synapses start --path <repo>"
//	└──────┬─────┘    └──────┬─────┘
//	       │ Unix socket       │
//	       └───────────────────┘
//	                   │
//	┌──────────────────▼──────────────────────┐
//	│       synapses singleton daemon          │  ← ONE per machine
//	│  HTTP 127.0.0.1:11435                    │
//	│  GET  /api/admin/health                  │
//	│  GET  /api/admin/projects                │
//	│  POST /api/admin/projects                │
//	│  GET  /api/admin/pulse/summary[?days=N]  │  analytics
//	│  POST|GET|DELETE /mcp?project=<path>     │  HTTP MCP transport
//	│  POST /v1/tools/{name}?project=<path>    │  REST tool API (B29)
//	│  ~/.synapses/daemons/<hash>.sock per-proj │  stdio proxy compat
//	└─────────────────────────────────────────┘
//
// PID:     ~/.synapses/daemon.pid  (singleton)
// Sockets: ~/.synapses/daemons/<sha256(path)[:16]>.sock  (one per registered project)
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/SynapsesOS/synapses/internal/brain"
	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/contextfile"
	"github.com/SynapsesOS/synapses/internal/embed"
	"github.com/SynapsesOS/synapses/internal/federation"
	"github.com/SynapsesOS/synapses/internal/logutil"
	"github.com/SynapsesOS/synapses/internal/lsp"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/namematcher"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"

	"golang.org/x/net/netutil"
)

// DaemonHTTPPort is the fixed port for the singleton daemon HTTP server.
// Port 11434 is Ollama's default — we use 11435 to avoid conflicts.
const DaemonHTTPPort = "11435"

// restSessionCounter generates unique per-request session IDs for REST API calls.
// Each POST /v1/tools/{name} request gets an isolated "rest-N" session context.
var restSessionCounter atomic.Int64

// restRateLimiter is a global token-bucket rate limiter for the REST tool API.
// Prevents abuse since each REST request gets a fresh session (bypassing
// per-session MCP rate limits). 50 requests/sec burst, refills at 10/sec.
var restRateLimiter = newTokenBucket(10, 50)

// perCallerBuckets holds a per-caller-IP token bucket as a secondary
// rate limiter. Entries idle >60s are evicted lazily on each access.
// Keyed by IP address only (not IP:port) so all connections from the same
// host share one bucket, preventing port-cycling bypass.
var perCallerBuckets sync.Map // key: string (IP only), value: *perCallerEntry

type perCallerEntry struct {
	bucket   *tokenBucket
	lastSeen time.Time
	mu       sync.Mutex
}

// callerIP extracts the IP address from a "host:port" RemoteAddr string.
// Falls back to the full string if parsing fails (should not happen on loopback).
func callerIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr // defensive fallback
	}
	return host
}

// callerBucket returns (or creates) the per-IP bucket for the given remote address,
// evicting entries that have been idle for more than 60 seconds.
func callerBucket(remoteAddr string) *tokenBucket {
	ip := callerIP(remoteAddr)
	now := time.Now()
	v, _ := perCallerBuckets.LoadOrStore(ip, &perCallerEntry{
		bucket:   newTokenBucket(5, 20),
		lastSeen: now,
	})
	entry := v.(*perCallerEntry)
	entry.mu.Lock()
	entry.lastSeen = now
	entry.mu.Unlock()

	// Lazy eviction: sweep for entries idle >60s.
	// For a loopback-only daemon, the number of distinct IPs is tiny (1-2),
	// so this sweep is O(1) in practice. Hard cap at 1000 entries prevents
	// unbounded growth when bound to 0.0.0.0.
	var mapSize int
	perCallerBuckets.Range(func(k, val interface{}) bool {
		mapSize++
		e := val.(*perCallerEntry)
		e.mu.Lock()
		idle := now.Sub(e.lastSeen)
		e.mu.Unlock()
		if idle > 60*time.Second {
			perCallerBuckets.Delete(k)
			mapSize--
		}
		return true
	})
	if mapSize > 1000 {
		// Emergency eviction: delete oldest entries to stay under cap.
		perCallerBuckets.Range(func(k, val interface{}) bool {
			if mapSize <= 1000 {
				return false
			}
			perCallerBuckets.Delete(k)
			mapSize--
			return true
		})
	}
	return entry.bucket
}

// adminProjectsRateLimiter rate-limits POST/DELETE /api/admin/projects (2/sec burst 10).
var adminProjectsRateLimiter = newTokenBucket(2, 10)

// tokenBucket is a simple token-bucket rate limiter.
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	lastRefill time.Time
}

func newTokenBucket(refillRate, maxTokens float64) *tokenBucket {
	return &tokenBucket{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

func (tb *tokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	if elapsed > 0 {
		// Only refill forward — clamp negative elapsed (NTP clock skew) to zero
		// to prevent draining tokens when the system clock jumps backward.
		tb.tokens += elapsed * tb.refillRate
		if tb.tokens > tb.maxTokens {
			tb.tokens = tb.maxTokens
		}
		tb.lastRefill = now
	}
	if tb.tokens < 1 {
		return false
	}
	tb.tokens--
	return true
}

// DaemonHTTPAddr returns the address the singleton daemon binds to.
// Override with SYNAPSES_BIND_ADDR env var (e.g. "0.0.0.0:11435" for Docker).
var DaemonHTTPAddr = daemonHTTPAddr()

func daemonHTTPAddr() string {
	if addr := os.Getenv("SYNAPSES_BIND_ADDR"); addr != "" {
		return addr
	}
	return "127.0.0.1:" + DaemonHTTPPort
}

// canonicalPath resolves a path to its absolute, symlink-free form.
// This ensures that /project and /symlink-to-project map to the same daemon.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// If symlink resolution fails (e.g., path doesn't exist yet),
		// fall back to the absolute path.
		return abs, nil
	}
	return resolved, nil
}

// ── path helpers ──────────────────────────────────────────────────────────────

func daemonDir() (string, error) {
	base, err := synapsesHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "daemons")
	return dir, os.MkdirAll(dir, 0o700)
}

func projectHash(absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return fmt.Sprintf("%x", h[:8]) // 16-char hex
}

// daemonSocketPath returns the per-project Unix socket path.
// Used by both the daemon (to create the socket) and the proxy (to connect).
func daemonSocketPath(absPath string) (string, error) {
	dir, err := daemonDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, projectHash(absPath)+".sock"), nil
}

// singletonPIDPath returns the path to the singleton daemon PID file.
func singletonPIDPath() (string, error) {
	base, err := synapsesHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "daemon.pid"), nil
}

// singletonLogPath returns the singleton daemon log path.
func singletonLogPath() (string, error) {
	base, err := synapsesHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "daemon.log"), nil
}

// rotateLogIfNeeded performs simple size-based log rotation at daemon startup.
// If the log file exceeds 50 MB, it is renamed to <logPath>.1 and a fresh
// file is created on the next open. At most one backup is kept (~100 MB max).
func rotateLogIfNeeded(logPath string) {
	const maxLogBytes = 50 * 1024 * 1024 // 50 MiB
	info, err := os.Stat(logPath)
	if err != nil || info.Size() < maxLogBytes {
		return
	}
	_ = os.Rename(logPath, logPath+".1")
}

// IsSingletonDaemonRunning checks if the singleton daemon is up by hitting
// the health endpoint. Returns true if the daemon responds within 2 seconds.
func IsSingletonDaemonRunning() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + DaemonHTTPAddr + "/api/admin/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// cleanStaleSingletonPID removes the PID file if the process is not alive.
func cleanStaleSingletonPID() {
	pidPath, err := singletonPIDPath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	lines := strings.SplitN(string(data), "\n", 2)
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || !processAlive(pid) {
		os.Remove(pidPath)
		return
	}
	if len(lines) >= 2 {
		if startNanos, parseErr := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); parseErr == nil && startNanos > 0 {
			if procStart := processStartTime(pid); procStart > 0 {
				recorded := time.Unix(0, startNanos)
				actual := time.Unix(0, procStart)
				if actual.Sub(recorded).Abs() > 2*time.Second {
					os.Remove(pidPath)
				}
			}
		}
	}
}

// isAddrInUse returns true when err indicates the bind address is already in use.
func isAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EADDRINUSE
	}
	return strings.Contains(err.Error(), "address already in use")
}

// daemonPort extracts the port number from DaemonHTTPAddr (e.g. "127.0.0.1:11435" → "11435").
func daemonPort() string {
	_, port, err := net.SplitHostPort(DaemonHTTPAddr)
	if err != nil {
		return DaemonHTTPPort
	}
	return port
}

// ── Auth token ────────────────────────────────────────────────────────────────

// authTokenPath returns the path to the daemon auth token file (~/.synapses/auth_token).
func authTokenPath() (string, error) {
	base, err := synapsesHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "auth_token"), nil
}

// loadOrCreateAuthToken reads the auth token from disk, creating one if absent or invalid.
// The token is 32 random bytes encoded as a 64-character hex string.
// File is written with mode 0600 (owner read/write only).
//
// The function ensures ~/.synapses exists before writing: synapsesHome() returns
// the path without creating it, so a fresh install would fail without MkdirAll.
func loadOrCreateAuthToken() (string, error) {
	path, err := authTokenPath()
	if err != nil {
		return "", err
	}
	// Ensure the parent directory exists. synapsesHome() returns the path
	// without creating it, so we must do it ourselves for robustness.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create auth token dir: %w", err)
	}
	// Try to read an existing valid token.
	if data, err := os.ReadFile(path); err == nil {
		// Verify file permissions — a world-readable token file is a security risk.
		if info, statErr := os.Stat(path); statErr == nil {
			if perm := info.Mode().Perm(); perm != 0o600 {
				// Fix permissions and warn; do not abort so the daemon can still start.
				if chmodErr := os.Chmod(path, 0o600); chmodErr == nil {
					logutil.Warn("synapses: auth token file had insecure permissions %04o — corrected to 0600\n", perm)
				} else {
					logutil.Warn("synapses: auth token file has insecure permissions %04o and could not be corrected: %v\n", perm, chmodErr)
				}
			}
		}
		token := strings.TrimSpace(string(data))
		if len(token) == 64 {
			if _, hexErr := hex.DecodeString(token); hexErr == nil {
				return token, nil
			}
		}
		// File exists but is invalid (truncated, corrupted, non-hex) — regenerate.
	}
	// Generate a new 32-byte random token.
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate auth token: %w", err)
	}
	token := fmt.Sprintf("%x", b)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("write auth token: %w", err)
	}
	return token, nil
}

// isValidProjectPath reports whether absPath is a legitimate project root.
// A path is considered legitimate if it contains a `.git` entry (directory
// for normal repos, regular file for git worktrees) or a `synapses.json`
// regular file as a direct child. This prevents a malicious browser page from
// registering `/`, `/etc`, or other sensitive system directories as projects
// via the REST API, which would cause the daemon to index and expose their
// contents.
//
// Validation rules:
//   - `.git`:         accepted as a directory OR regular file (git worktrees
//     use a plain text file pointing at the shared git dir)
//   - `synapses.json`: accepted only as a regular file, NOT a directory.
//     Accepting a directory named `synapses.json` would allow an attacker
//     with local filesystem access to manufacture a fake marker by running
//     `mkdir synapses.json`. A real config file must be a regular file.
//
// Returns a non-nil error (suitable for use in HTTP responses) when the path
// fails validation.
func isValidProjectPath(absPath string) error {
	// Check for .git — valid as directory (normal repo) or regular file (worktree).
	if info, err := os.Stat(filepath.Join(absPath, ".git")); err == nil {
		if info.IsDir() || info.Mode().IsRegular() {
			return nil
		}
	}
	// Check for synapses.json — must be a regular file, not a directory.
	if info, err := os.Stat(filepath.Join(absPath, "synapses.json")); err == nil {
		if info.Mode().IsRegular() {
			return nil
		}
	}
	return fmt.Errorf("path %q is not a valid project root (missing .git or synapses.json)", absPath)
}

// isCORSAllowedOrigin reports whether origin is in the explicit allowlist of
// browser origins permitted to make cross-origin requests to the REST API.
// The allowlist covers:
//   - tauri://localhost    — Synapses desktop app
//   - http(s)://localhost  — local browser dev tools
//   - http(s)://127.0.0.1 — explicit loopback variant
//
// Wildcard (*) is intentionally not used — it would allow any webpage to
// silently call the API via the trusted loopback connection (loopback is
// exempt from bearer-token auth, so CORS wildcard = unauthenticated access
// from any browser tab on the user's machine).
//
// Non-browser clients (curl, MCP stdio proxy) send no Origin header and are
// unaffected by CORS headers entirely.
func isCORSAllowedOrigin(origin string) bool {
	if origin == "tauri://localhost" || origin == "https://tauri.localhost" {
		return true
	}
	for _, prefix := range []string{
		"http://localhost", "https://localhost",
		"http://127.0.0.1", "https://127.0.0.1",
	} {
		if origin == prefix || strings.HasPrefix(origin, prefix+":") {
			return true
		}
	}
	return false
}

// ── Security middleware (Phase -1) ───────────────────────────────────────────

// hostGuard blocks DNS rebinding attacks by validating the Host header.
// Only requests to 127.0.0.1, localhost, or ::1 are allowed.
// This is the same mitigation used by Jupyter Notebook, webpack-dev-server,
// and Chrome DevTools after their DNS rebinding CVEs.
func hostGuard(next http.Handler) http.Handler {
	allowed := map[string]bool{
		"127.0.0.1": true, "localhost": true, "::1": true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !allowed[host] {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// trustedOrigins is the set of origins allowed to perform mutations.
// Non-browser clients (curl, Go HTTP) send no Origin header and are always allowed.
// Browsers ALWAYS send Origin on cross-origin POST/PUT/DELETE.
var trustedOrigins = map[string]bool{
	"http://127.0.0.1:11435":  true,
	"http://localhost:11435":  true,
	"tauri://localhost":       true,
	"https://tauri.localhost": true,
}

// mutationGuard blocks CSRF by validating Origin and CSRF token on mutations.
// GET/HEAD/OPTIONS are always allowed (read-only).
// For POST/PUT/DELETE: Origin must be trusted (or absent), AND a valid
// X-CSRF-Token header must be present.
func mutationGuard(csrfToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}
		// Check Origin header (present on cross-origin requests from browsers).
		origin := r.Header.Get("Origin")
		if origin != "" && !trustedOrigins[origin] {
			http.Error(w, "Forbidden: untrusted origin", http.StatusForbidden)
			return
		}
		// CSRF token check: require X-CSRF-Token on mutations.
		// Exempt: /mcp (MCP protocol uses its own session management).
		// /v1/tools/ requires CSRF only when Origin header is present (browser context);
		// non-browser REST clients (CLI, MCP proxies) don't send Origin.
		needCSRF := !strings.HasPrefix(r.URL.Path, "/mcp")
		if strings.HasPrefix(r.URL.Path, "/v1/tools/") && origin == "" {
			needCSRF = false // non-browser REST client — Bearer auth suffices
		}
		if needCSRF {
			provided := r.Header.Get("X-CSRF-Token")
			if csrfToken == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(csrfToken)) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid CSRF token"}) //nolint:errcheck
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// generateCSRFToken creates a random 32-byte hex-encoded CSRF token.
func generateCSRFToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// setSecurityHeaders adds CSP and other security headers to responses.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"connect-src 'self'",
		"img-src 'self' data:",
		"frame-ancestors 'none'",
	}, "; "))
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// restToolsHandler returns the HTTP handler for POST /v1/tools/{name}?project=<path>.
// projectInit is called to lazily initialize a project that is not yet registered.
// Extracted from cmdDaemonServe to enable HTTP-level testing.
func restToolsHandler(reg *projectRegistry, projectInit func(string) (*ProjectInstance, error), wg *sync.WaitGroup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Per-caller rate limit (20 burst, 5/sec refill) applied first to
		// prevent a single caller from starving others on the shared bucket.
		if !callerBucket(r.RemoteAddr).Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded, try again later"}) //nolint:errcheck
			return
		}
		// Global rate limit — coarse abuse guard across all callers.
		if !restRateLimiter.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded, try again later"}) //nolint:errcheck
			return
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			json.NewEncoder(w).Encode(map[string]string{"error": "method not allowed, use POST"}) //nolint:errcheck
			return
		}

		// Extract tool name — everything after "/v1/tools/".
		toolName := strings.TrimPrefix(r.URL.Path, "/v1/tools/")
		if toolName == "" || strings.Contains(toolName, "/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid tool path; use /v1/tools/{name}"}) //nolint:errcheck
			return
		}

		// Resolve project.
		projectPath := r.URL.Query().Get("project")
		if projectPath == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing ?project= query parameter"}) //nolint:errcheck
			return
		}
		if decoded, err := url.QueryUnescape(projectPath); err == nil {
			projectPath = decoded
		}
		absPath, err := canonicalPath(projectPath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid project path: " + mcpsrv.StripInternalPaths(err.Error())}) //nolint:errcheck
			return
		}
		if err := isValidProjectPath(absPath); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid project path: " + mcpsrv.StripInternalPaths(err.Error())}) //nolint:errcheck
			return
		}

		type restPIResult struct {
			pi  *ProjectInstance
			err error
		}
		restCh := make(chan restPIResult, 1)
		go func() {
			pi, err := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
				return projectInit(absPath)
			})
			restCh <- restPIResult{pi, err}
		}()

		var pi *ProjectInstance
		select {
		case res := <-restCh:
			if res.err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": "init project: " + mcpsrv.StripInternalPaths(res.err.Error())}) //nolint:errcheck
				return
			}
			pi = res.pi
		case <-time.After(60 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "project still loading — retry in a few seconds"}) //nolint:errcheck
			return
		}
		if wg != nil {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				saveKnownProject(p)
			}(absPath)
		} else {
			// wg is nil (e.g. in tests or misconfigured call sites).
			// Run synchronously to ensure the project is always persisted.
			saveKnownProject(absPath)
		}

		// Parse request body as tool arguments. Empty or absent body → empty args.
		// Cap at 1 MiB to prevent unbounded memory allocation from malformed requests.
		// Use io.LimitReader regardless of Content-Length: handles chunked transfer
		// encoding (Content-Length == -1) correctly without skipping the body.
		args := make(map[string]interface{})
		limited := io.LimitReader(r.Body, 1<<20) // 1 MiB
		if decodeErr := json.NewDecoder(limited).Decode(&args); decodeErr != nil && decodeErr != io.EOF {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON body: " + mcpsrv.StripInternalPaths(decodeErr.Error())}) //nolint:errcheck
			return
		}
		// Reject deeply nested JSON to prevent memory amplification attacks.
		// A 1 MiB JSON body decoded into map[string]interface{} can amplify
		// to 5-10 MiB heap per request with deep nesting.
		if jsonDepth(args) > 10 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "JSON nesting too deep (max 10)"}) //nolint:errcheck
			return
		}

		// Inject a per-request session ID so handlers that use SessionIDFromContext
		// get an isolated context. Each REST call is stateless — no session is shared
		// across calls. The session ID is "rest-N" where N is a monotonic counter.
		sessionID := fmt.Sprintf("rest-%d", restSessionCounter.Add(1))
		ctx := mcpsrv.WithSessionID(r.Context(), sessionID)

		result, dispatchErr := pi.MCPServer.DispatchTool(ctx, toolName, args)
		if dispatchErr != nil {
			w.Header().Set("Content-Type", "application/json")
			if _, ok := dispatchErr.(*mcpsrv.ErrUnknownTool); ok {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"error": mcpsrv.StripInternalPaths(dispatchErr.Error())}) //nolint:errcheck
			} else {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": mcpsrv.StripInternalPaths(dispatchErr.Error())}) //nolint:errcheck
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result) //nolint:errcheck
	}
}

// authMiddleware enforces bearer-token authentication for non-localhost clients.
//
// Localhost connections (RemoteAddr resolving to a loopback IP) are always
// allowed — they represent trusted local tools (Claude Code, Cursor, Tauri app)
// that have no need to manage tokens.  Non-localhost connections must present
// a valid "Authorization: Bearer <token>" header.
//
// The /api/admin/health endpoint is always exempt so that liveness checks
// (IsSingletonDaemonRunning, Tauri health poll) never require credentials.
//
// OPTIONS (CORS preflight) requests are always forwarded — the caller is
// expected to have already set CORS headers before calling this middleware,
// so the preflight response will include them even without auth.
//
// If token is empty, auth is disabled entirely (daemon logs a warning at
// startup; localhost-only binding remains the primary protection).
//
// IMPORTANT: this middleware must be called AFTER CORS headers have been set
// on the ResponseWriter.  The canonical composition in cmdDaemonServe is:
//
//	finalHandler (sets CORS headers) → authMiddleware → mux
//
// This ensures that 401 rejections carry the Access-Control-Allow-* headers
// a browser needs to surface the auth error rather than a generic CORS error.
func authMiddleware(token string, next http.Handler) http.Handler {
	if token == "" || os.Getenv("SYNAPSES_NO_AUTH") == "1" {
		return next // disabled — no-op pass-through
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health endpoint is always exempt.
		if r.URL.Path == "/api/admin/health" {
			next.ServeHTTP(w, r)
			return
		}
		// OPTIONS preflight: always forward.  The outer CORS handler sets
		// the required headers before reaching this middleware, so the
		// preflight response is already correct.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		// Determine the client IP.
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		// Loopback (127.x.x.x, ::1) → trusted, no token needed.
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			next.ServeHTTP(w, r)
			return
		}
		// Non-localhost: require a valid Bearer token.
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			logutil.Warn("synapses: auth: missing token from %s %s\n", r.RemoteAddr, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", `Bearer realm="synapses"`)
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing Authorization header; use Bearer token"}) //nolint:errcheck
			return
		}
		provided := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			logutil.Warn("synapses: auth: invalid token from %s %s\n", r.RemoteAddr, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", `Bearer realm="synapses"`)
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid Bearer token"}) //nolint:errcheck
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── connSession: per-connection MCP session (used for Unix socket serving) ───

// connSession implements the mcp-go ClientSession, SessionWithLogging, and
// SessionWithClientInfo interfaces. Each proxy connection gets its own session.
type connSession struct {
	id            string
	notifications chan mcp.JSONRPCNotification
	initialized   atomic.Bool
	loggingLevel  atomic.Value
	clientInfo    atomic.Value
}

func (s *connSession) SessionID() string                                   { return s.id }
func (s *connSession) NotificationChannel() chan<- mcp.JSONRPCNotification { return s.notifications }
func (s *connSession) Initialize() {
	s.loggingLevel.Store(mcp.LoggingLevelError)
	s.initialized.Store(true)
}
func (s *connSession) Initialized() bool { return s.initialized.Load() }
func (s *connSession) GetClientInfo() mcp.Implementation {
	if v := s.clientInfo.Load(); v != nil {
		if ci, ok := v.(mcp.Implementation); ok {
			return ci
		}
	}
	return mcp.Implementation{}
}
func (s *connSession) SetClientInfo(ci mcp.Implementation) { s.clientInfo.Store(ci) }
func (s *connSession) SetLogLevel(level mcp.LoggingLevel)  { s.loggingLevel.Store(level) }
func (s *connSession) GetLogLevel() mcp.LoggingLevel {
	if v := s.loggingLevel.Load(); v != nil {
		return v.(mcp.LoggingLevel)
	}
	return mcp.LoggingLevelError
}

// ── cmdDaemonServe: the singleton daemon entry point ─────────────────────────

func cmdDaemonServe(args []string) error {
	// Structured lifecycle event — MUST be the very first output so that
	// an empty daemon.log means the binary never executed (OS-level issue).
	daemonStartedAt := time.Now()
	emitLifecycleEvent("daemon_starting", map[string]any{
		"version": version,
		"pid":     os.Getpid(),
		"go":      runtime.Version(),
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
		"addr":    DaemonHTTPAddr,
	})

	// Catch panics so they appear in daemon.log as structured JSON instead
	// of causing a silent exit with an empty log.
	defer func() {
		if r := recover(); r != nil {
			emitLifecycleEvent("daemon_panic", map[string]any{
				"error": fmt.Sprint(r),
				"stack": string(debug.Stack()),
			})
			// Re-panic so the process exits with a non-zero status.
			panic(r)
		}
	}()

	// No flags — the singleton daemon serves all projects.
	// Projects are registered on-demand via the admin API.

	// ── Singleton check ──────────────────────────────────────────────────────
	cleanStaleSingletonPID()
	if IsSingletonDaemonRunning() {
		return fmt.Errorf("singleton daemon already running at %s", DaemonHTTPAddr)
	}

	pidPath, err := singletonPIDPath()
	if err != nil {
		return fmt.Errorf("pid path: %w", err)
	}
	pidContent := fmt.Sprintf("%d\n%d", os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(pidPath, []byte(pidContent), 0o600); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	defer os.Remove(pidPath)

	// ── saveKnownProject WaitGroup ────────────────────────────────────────────
	var saveKnownProjectWg sync.WaitGroup

	// ── App context for graceful shutdown ────────────────────────────────────
	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()

	// ── Project registry ─────────────────────────────────────────────────────
	reg := newProjectRegistry()
	defer reg.Close()

	// ── Pulse (shared across all projects) ───────────────────────────────────
	var sharedPulse *pulse.Client
	if pulseDBPath, err := pulse.DefaultDBPath(); err == nil {
		if pulseCli, err := pulse.New(pulseDBPath); err == nil {
			sharedPulse = pulseCli
			defer pulseCli.Close()
			logutil.Info("synapses: pulse analytics enabled (in-process, db: %s)\n", pulseDBPath)
		}
	}

	// ── Hibernate system ─────────────────────────────────────────────────────
	// Wire the wake function so GetOrSet can auto-wake hibernated projects.
	reg.SetWakeFunc(func(absPath string) (*ProjectInstance, error) {
		return wakeProjectInstance(appCtx, absPath, sharedPulse, reg)
	})
	// Start the hibernation sweeper goroutine.
	go startHibernationSweeper(appCtx, reg)

	// ── Eager project warming ────────────────────────────────────────────────
	// Pre-initialize projects that were registered before the last shutdown.
	// Hibernated projects are loaded as tombstones (no full init).
	// Runs in background so it doesn't block HTTP server startup.
	go func() {
		entries := loadKnownProjectsWithState()
		if len(entries) == 0 {
			return
		}
		logutil.Info("synapses daemon: loading %d known project(s)\n", len(entries))
		for _, entry := range entries {
			path := entry.Path
			if _, err := os.Stat(path); os.IsNotExist(err) {
				logutil.Info("synapses daemon: removing stale project %s\n", path)
				removeKnownProject(path)
				continue
			}
			if err := isValidProjectPath(path); err != nil {
				logutil.Info("synapses daemon: skipping invalid project %s: %v\n", path, err)
				removeKnownProject(path)
				continue
			}

			if entry.State == "hibernated" {
				// Restore as tombstone — no full init, just sentinel watcher.
				tomb := &HibernatedProject{
					AbsPath:      path,
					HibernatedAt: time.Now(),
					sentinelStop: make(chan struct{}),
				}
				go runSentinelWatcher(tomb)
				reg.Hibernate(path, tomb)
				logutil.Info("synapses daemon: restored hibernated %s\n", filepath.Base(path))
				continue
			}

			// Warm init (default for "warm" or unrecognized states).
			_, err := reg.GetOrSet(path, func() (*ProjectInstance, error) {
				return initProjectInstance(appCtx, path, sharedPulse, reg)
			})
			if err != nil {
				logutil.Warn("synapses daemon: warm %s failed: %v\n", path, err)
			} else {
				logutil.Info("synapses daemon: warmed %s\n", path)
			}
		}
	}()

	// ── HTTP MCP router ───────────────────────────────────────────────────────
	// /mcp?project=<absPath>  → per-project StreamableHTTPServer
	// /api/admin/*            → daemon management API
	mux := http.NewServeMux()

	// Admin: health
	mux.HandleFunc("/api/admin/health", func(w http.ResponseWriter, r *http.Request) {
		// Health endpoint is exempt from auth for liveness checks.
		// Only return project_count (not paths) to avoid information disclosure.
		// Full project details available at /api/admin/projects (auth-protected).
		projects := reg.All()
		overallStatus := "ok"

		// BUG-019: include per-project watcher liveness so Tauri app and
		// monitoring can detect watcher deaths (previously only surfaced
		// in session_init warnings).
		watcherDead := 0
		for _, pi := range projects {
			if pi.Watcher != nil && !pi.Watcher.IsAlive() {
				watcherDead++
			}
		}
		if watcherDead > 0 {
			overallStatus = "degraded"
		}

		// BUG-020: aggregate background queue stats across projects.
		var totalBgDepth int
		var totalBgDrops int64
		for _, pi := range projects {
			if pi.MCPServer != nil {
				depth, drops := pi.MCPServer.BackgroundQueueStats()
				totalBgDepth += depth
				totalBgDrops += drops
			}
		}

		snap := ActiveSnapshot()
		hibernatedPaths := reg.HibernatedPaths()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":               overallStatus,
			"project_count":        len(projects),
			"hibernated_count":     len(hibernatedPaths),
			"watchers_dead":        watcherDead,
			"bg_queue_depth":       totalBgDepth,
			"bg_queue_drops":       totalBgDrops,
			"indexing_progress":    snap,
			"indexing_in_progress": snap.State == "indexing",
		})
	})

	// Admin: list projects
	mux.HandleFunc("/api/admin/projects", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			projects := reg.All()
			type projectInfo struct {
				Path   string `json:"path"`
				Hash   string `json:"hash"`
				Socket string `json:"socket"`
			}
			infos := make([]projectInfo, 0, len(projects))
			for _, p := range projects {
				sock, _ := daemonSocketPath(p.AbsPath)
				infos = append(infos, projectInfo{
					Path:   p.AbsPath,
					Hash:   projectHash(p.AbsPath),
					Socket: sock,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"projects": infos})

		case http.MethodPost:
			if !adminProjectsRateLimiter.Allow() {
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			var req struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			absPath, err := canonicalPath(req.Path)
			if err != nil {
				http.Error(w, "invalid path: "+mcpsrv.StripInternalPaths(err.Error()), http.StatusBadRequest)
				return
			}
			if err := isValidProjectPath(absPath); err != nil {
				http.Error(w, "invalid project path: "+mcpsrv.StripInternalPaths(err.Error()), http.StatusBadRequest)
				return
			}
			sockPath, err := daemonSocketPath(absPath)
			if err != nil {
				http.Error(w, "socket path error: "+mcpsrv.StripInternalPaths(err.Error()), http.StatusInternalServerError)
				return
			}
			// GetOrSet: lazy-initialize the project if not already registered.
			_, initErr := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
				return initProjectInstance(appCtx, absPath, sharedPulse, reg)
			})
			if initErr != nil {
				http.Error(w, "init project: "+mcpsrv.StripInternalPaths(initErr.Error()), http.StatusInternalServerError)
				return
			}
			// Persist for eager warming on next daemon restart.
			saveKnownProjectWg.Add(1)
			go func(p string) {
				defer saveKnownProjectWg.Done()
				saveKnownProject(p)
			}(absPath)

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "ok",
				"path":   absPath,
				"socket": sockPath,
			})

		case http.MethodDelete:
			if !adminProjectsRateLimiter.Allow() {
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			var req struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			absPath, err := canonicalPath(req.Path)
			if err != nil {
				http.Error(w, "invalid path: "+mcpsrv.StripInternalPaths(err.Error()), http.StatusBadRequest)
				return
			}
			if err := isValidProjectPath(absPath); err != nil {
				http.Error(w, "invalid project path: "+mcpsrv.StripInternalPaths(err.Error()), http.StatusBadRequest)
				return
			}
			reg.Delete(absPath)
			go removeKnownProject(absPath)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// pulseGuard returns true (and writes 503) if sharedPulse is nil.
	pulseGuard := func(w http.ResponseWriter) bool {
		if sharedPulse == nil {
			http.Error(w, `{"error":"pulse analytics unavailable"}`, http.StatusServiceUnavailable)
			return true
		}
		return false
	}

	// Admin: pulse analytics summary
	// P12-3: supports ?sections=summary,tools,timeline to avoid computing all 50+ fields.
	// When sections is omitted, returns the full summary (backward compatible).
	mux.HandleFunc("/api/admin/pulse/summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if projectFilter := r.URL.Query().Get("project"); projectFilter != "" {
			json.NewEncoder(w).Encode(sharedPulse.GetSummaryForProject(days, projectFilter))
			return
		}
		// P12-3: sectioned response.
		if sectionsParam := r.URL.Query().Get("sections"); sectionsParam != "" {
			sectionSet := make(map[string]bool)
			for _, s := range strings.Split(sectionsParam, ",") {
				s = strings.TrimSpace(s)
				if s != "" {
					sectionSet[s] = true
				}
			}
			json.NewEncoder(w).Encode(sharedPulse.GetSummarySectioned(days, sectionSet))
			return
		}
		json.NewEncoder(w).Encode(sharedPulse.GetSummary(days))
	})

	// Admin: pulse — per-tool stats (P4-1 / Task P4-1)
	mux.HandleFunc("/api/admin/pulse/tools", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sharedPulse.GetToolStatsRaw(days))
	})

	// Admin: pulse — daily timeline (P4-1 / Task P4-1)
	mux.HandleFunc("/api/admin/pulse/timeline", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 14
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sharedPulse.GetTimelineRaw(days))
	})

	// Admin: pulse — brain LLM cost stats (P4-1 / Task P4-1)
	mux.HandleFunc("/api/admin/pulse/brain", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		brainResp := struct {
			Stats      interface{} `json:"stats"`
			CostByTier interface{} `json:"cost_by_tier,omitempty"`
		}{
			Stats:      sharedPulse.GetBrainCostStats(days),
			CostByTier: sharedPulse.GetCostByTier(days),
		}
		json.NewEncoder(w).Encode(brainResp)
	})

	// Admin: pulse — graph snapshot (P4-7)
	mux.HandleFunc("/api/admin/pulse/graph", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sharedPulse.GetLatestGraphSnapshot())
	})

	// Admin: pulse — search analytics (P4-8)
	mux.HandleFunc("/api/admin/pulse/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sharedPulse.GetSearchStats(days))
	})

	// Admin: pulse — per-tool timeline (Bug 54)
	// GET /api/admin/pulse/tools/{name}/timeline?days=N
	mux.HandleFunc("/api/admin/pulse/tools/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		// Path: /api/admin/pulse/tools/{name}/timeline
		path := strings.TrimPrefix(r.URL.Path, "/api/admin/pulse/tools/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 || parts[1] != "timeline" {
			http.NotFound(w, r)
			return
		}
		toolName := parts[0]
		if toolName == "" {
			http.Error(w, "missing tool name", http.StatusBadRequest)
			return
		}
		days := 14
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sharedPulse.GetToolTimeline(toolName, days))
	})

	// Admin: pulse — session detail (Bug 55)
	// GET /api/admin/pulse/sessions/{id}
	mux.HandleFunc("/api/admin/pulse/sessions/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		sessionID := strings.TrimPrefix(r.URL.Path, "/api/admin/pulse/sessions/")
		if sessionID == "" {
			http.Error(w, "missing session id", http.StatusBadRequest)
			return
		}
		detail := sharedPulse.GetSessionDetail(sessionID)
		if detail == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(detail)
	})

	// Admin: pulse — raw data export (Bug 56)
	// GET /api/admin/pulse/export?days=N
	mux.HandleFunc("/api/admin/pulse/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		data := sharedPulse.ExportRawData(days)
		if data == nil {
			http.Error(w, "export failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})

	// GET /v1/health — Sprint 18 #2: full daemon health endpoint.
	// Returns daemon uptime, per-project graph/memory sizes, brain availability,
	// federation status, embedding model status, and pulse collector metrics.
	// Handler logic lives in buildHealthHandler (defined below) so it can be
	// unit-tested independently of the full daemon setup.
	mux.HandleFunc("/v1/health", buildHealthHandler(reg, sharedPulse, daemonStartedAt))

	// GET /api/admin/pulse/monthly?year=YYYY&month=MM — P5 Item 20: monthly ROI report.
	mux.HandleFunc("/api/admin/pulse/monthly", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		now := time.Now().UTC()
		year, month := now.Year(), int(now.Month())
		if y := r.URL.Query().Get("year"); y != "" {
			if n, err := strconv.Atoi(y); err == nil {
				year = n
			}
		}
		if m := r.URL.Query().Get("month"); m != "" {
			if n, err := strconv.Atoi(m); err == nil && n >= 1 && n <= 12 {
				month = n
			}
		}
		report := sharedPulse.GetMonthlyROIReport(year, month)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(report)
	})

	// P12-7: SSE real-time event stream.
	// GET /api/admin/pulse/stream
	const maxSSEClients = 16
	var sseClientCount atomic.Int32
	mux.HandleFunc("/api/admin/pulse/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		if sseClientCount.Add(1) > int32(maxSSEClients) {
			sseClientCount.Add(-1)
			http.Error(w, "too many SSE clients", http.StatusServiceUnavailable)
			return
		}
		defer sseClientCount.Add(-1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := sharedPulse.SubscribeSSE()
		defer sharedPulse.UnsubscribeSSE(ch)

		rc := http.NewResponseController(w)

		// Send initial keepalive so the client knows the connection is established.
		fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()

		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				data, _ := json.Marshal(ev)
				// Set write deadline to detect stale connections, then clear
				// it after successful write to avoid expiring idle streams.
				// SetWriteDeadline error is intentionally ignored — not all
				// ResponseWriter implementations support deadlines, and the
				// subsequent Fprintf will catch stale connections regardless.
				_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
					return // stale client
				}
				_ = rc.SetWriteDeadline(time.Time{}) // clear deadline
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	// P12-1: per-agent drill-down endpoint.
	// GET /api/admin/pulse/agents/{id}?days=N
	mux.HandleFunc("/api/admin/pulse/agents/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		agentID := strings.TrimPrefix(r.URL.Path, "/api/admin/pulse/agents/")
		if agentID == "" {
			http.Error(w, "missing agent id", http.StatusBadRequest)
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		detail := sharedPulse.GetAgentDrillDown(agentID, days)
		if detail == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(detail)
	})

	// P12-2: week-over-week comparison endpoint.
	// GET /api/admin/pulse/wow
	mux.HandleFunc("/api/admin/pulse/wow", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		wow := sharedPulse.GetWeekOverWeek()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(wow)
	})

	// P12-8: selective data cleanup.
	// DELETE /api/admin/pulse/data?agent_id=X or ?project_id=X
	mux.HandleFunc("/api/admin/pulse/data", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		agentID := r.URL.Query().Get("agent_id")
		projectID := r.URL.Query().Get("project_id")
		if agentID == "" && projectID == "" {
			http.Error(w, "missing agent_id or project_id query parameter", http.StatusBadRequest)
			return
		}
		var deleted int64
		if agentID != "" {
			logutil.Info("pulse: DELETE by agent_id=%q (remote=%s)\n", agentID, r.RemoteAddr)
			deleted = sharedPulse.DeleteByAgent(agentID)
		} else {
			logutil.Info("pulse: DELETE by project_id=%q (remote=%s)\n", projectID, r.RemoteAddr)
			deleted = sharedPulse.DeleteByProject(projectID)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":       "ok",
			"rows_deleted": deleted,
		})
	})

	// P12-9: validate-to-verify funnel rate.
	// GET /api/admin/pulse/funnel/validate-verify?days=N
	mux.HandleFunc("/api/admin/pulse/funnel/validate-verify", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		rate := sharedPulse.GetValidateToVerifyRate(days)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"days":        days,
			"verify_rate": rate,
		})
	})

	// P12-10: declining tools.
	// GET /api/admin/pulse/tools/declining?days=N&min_calls=M
	mux.HandleFunc("/api/admin/pulse/tools/declining", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 30
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 365 {
				days = n
			}
		}
		minCalls := 10
		if m := r.URL.Query().Get("min_calls"); m != "" {
			if n, err := strconv.Atoi(m); err == nil && n > 0 {
				minCalls = n
			}
		}
		tools := sharedPulse.GetDecliningTools(days, minCalls)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tools)
	})

	// GET /api/admin/pulse/effectiveness?days=N&project=P
	mux.HandleFunc("/api/admin/pulse/effectiveness", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if pulseGuard(w) {
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		projectID := r.URL.Query().Get("project")
		fcr := sharedPulse.GetFirstContextRightRate(days)
		entities := sharedPulse.FetchEffectiveness(projectID, 2)
		trend := sharedPulse.GetRecentEffectivenessTrend(days, "")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"days":                     days,
			"first_context_right_rate": fcr,
			"entity_effectiveness":     entities,
			"trend":                    trend,
		})
	})

	// MCP: route to per-project StreamableHTTPServer
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		// Panic recovery: a panic in any tool handler or project init must NOT
		// crash the entire daemon. Log it, return 500, and keep serving.
		defer func() {
			if rv := recover(); rv != nil {
				stack := debug.Stack()
				logutil.Error("panic in /mcp handler: %v\n%s", rv, stack)
				// Best-effort error response. If headers were already sent
				// (e.g. SSE stream in progress), this will be a no-op since
				// WriteHeader only takes effect on the first call.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
					"jsonrpc": "2.0",
					"error":   "internal error (recovered)",
				})
			}
		}()

		// Origin check: reject cross-origin browser requests to /mcp.
		// Native MCP clients (CLI, proxies) send no Origin header and are allowed.
		// The mutationGuard only covers POST/PUT/DELETE; this check also covers GET (SSE).
		if origin := r.Header.Get("Origin"); origin != "" && !trustedOrigins[origin] {
			http.Error(w, "Forbidden: untrusted origin", http.StatusForbidden)
			return
		}

		projectPath := r.URL.Query().Get("project")
		if projectPath == "" {
			http.Error(w, "missing ?project=<abs-path> query parameter", http.StatusBadRequest)
			return
		}

		// Decode URL-encoded path
		if decoded, err := url.QueryUnescape(projectPath); err == nil {
			projectPath = decoded
		}

		absPath, err := canonicalPath(projectPath)
		if err != nil {
			http.Error(w, "invalid project path: "+mcpsrv.StripInternalPaths(err.Error()), http.StatusBadRequest)
			return
		}
		if err := isValidProjectPath(absPath); err != nil {
			http.Error(w, "invalid project path: "+mcpsrv.StripInternalPaths(err.Error()), http.StatusBadRequest)
			return
		}

		// Project init with timeout: large repos (225K+ nodes) can take
		// minutes to load. Use a 60s deadline so the HTTP client gets a
		// 503 instead of hanging indefinitely. The singleflight in GetOrSet
		// continues in the background — the next request will find it warm.
		type piResult struct {
			pi  *ProjectInstance
			err error
		}
		ch := make(chan piResult, 1)
		go func() {
			pi, err := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
				return initProjectInstance(appCtx, absPath, sharedPulse, reg)
			})
			ch <- piResult{pi, err}
		}()

		var pi *ProjectInstance
		select {
		case res := <-ch:
			if res.err != nil {
				http.Error(w, "init project: "+mcpsrv.StripInternalPaths(res.err.Error()), http.StatusInternalServerError)
				return
			}
			pi = res.pi
		case <-time.After(60 * time.Second):
			w.Header().Set("Retry-After", "10")
			http.Error(w, "project still loading — retry in a few seconds", http.StatusServiceUnavailable)
			return
		}
		saveKnownProjectWg.Add(1)
		go func(p string) {
			defer saveKnownProjectWg.Done()
			saveKnownProject(p)
		}(absPath)

		// Strip the ?project= param before forwarding so the MCP server
		// doesn't see unknown query parameters.
		r2 := r.Clone(r.Context())
		q := r2.URL.Query()
		q.Del("project")
		r2.URL.RawQuery = q.Encode()

		pi.HTTPHandler.ServeHTTP(w, r2)
	})

	// REST API: POST /v1/tools/{name}?project=<abs-path>
	// Thin HTTP wrapper around MCP tool handlers. Same Go functions as the MCP path.
	// Request body: JSON object of tool arguments (empty body treated as {}).
	// Response: {"content":[...],"isError":bool} on success.
	// HTTP status: 200 ok, 400 bad request, 404 unknown tool, 405 method not allowed,
	//              500 internal error.
	// Note: auth (bearer token) ships in Sprint 6.2. Until then, localhost-only
	// binding (127.0.0.1) limits exposure to local processes only.
	mux.HandleFunc("/v1/tools/", restToolsHandler(reg, func(absPath string) (*ProjectInstance, error) {
		return initProjectInstance(appCtx, absPath, sharedPulse, reg)
	}, &saveKnownProjectWg))

	// ── Phase 0: Admin management endpoints (web console) ────────────────────
	registerAdminEndpoints(mux, reg, func(absPath string) (*ProjectInstance, error) {
		return initProjectInstance(appCtx, absPath, sharedPulse, reg)
	}, appCancel)

	// ── Serve web console at root ────────────────────────────────────────────
	// Lookup order (Gitea/MinIO pattern):
	//   1. ~/.synapses/console/  — disk override for hotfixes & dev
	//   2. //go:embed             — production default baked into binary
	//
	// SPA fallback: if the path doesn't match an existing file in dist/,
	// serve index.html so client-side routing works.
	// Root path returns a simple JSON health response (no UI — UI is in the Tauri app).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "service": "synapses", "ui": "use SynapsesOS app"}) //nolint:errcheck
	})

	// ── Auth token ────────────────────────────────────────────────────────────
	// Generated on first start, persisted at ~/.synapses/auth_token.
	// Required only for non-localhost connections; localhost is always trusted.
	if os.Getenv("SYNAPSES_NO_AUTH") == "1" {
		logutil.Warn("synapses: WARNING — SYNAPSES_NO_AUTH=1 is set, authentication is DISABLED. Do not use in production.\n")
	}

	authToken, authErr := loadOrCreateAuthToken()
	if authErr != nil {
		// Non-fatal: log a warning and continue. Localhost-only binding
		// (127.0.0.1) is the primary protection; auth is defence-in-depth.
		logutil.Warn("synapses: could not load/create auth token: %v\n", authErr)
		authToken = ""
	} else {
		tokenPath, _ := authTokenPath()
		logutil.Info("synapses: auth token stored at %s\n", tokenPath)
	}

	// ── CSRF token (per-daemon-session, in-memory only) ──────────────────────
	csrfToken, csrfErr := generateCSRFToken()
	if csrfErr != nil {
		logutil.Warn("synapses: could not generate CSRF token: %v\n", csrfErr)
		csrfToken = "" // mutationGuard will reject all mutations if token is empty (fail-closed)
	}

	// CSRF token endpoint — fetched once by the web console on load.
	// Restricted to GET; requires trusted Origin (or no Origin for same-origin).
	// Non-browser callers (no Origin/Sec-Fetch-Site header) from loopback are
	// trusted (consistent with authMiddleware). Non-loopback non-browser callers
	// must present a Bearer token.
	mux.HandleFunc("/api/admin/csrf-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		origin := r.Header.Get("Origin")
		secFetch := r.Header.Get("Sec-Fetch-Site")
		isBrowserRequest := origin != "" || secFetch == "same-origin"
		if isBrowserRequest {
			if origin != "" && !trustedOrigins[origin] {
				http.Error(w, "Forbidden: untrusted origin", http.StatusForbidden)
				return
			}
		} else {
			// Non-browser: loopback callers (CLI tools) are trusted without a token.
			// Non-loopback callers must present a valid Bearer token.
			// SYNAPSES_NO_AUTH=1 disables token checks (for Docker/CI where
			// requests arrive from the bridge network, not loopback).
			noAuth := os.Getenv("SYNAPSES_NO_AUTH") == "1"
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			isLoopback := func() bool {
				ip := net.ParseIP(host)
				return ip != nil && ip.IsLoopback()
			}()
			if !isLoopback && !noAuth {
				authHeader := r.Header.Get("Authorization")
				if !strings.HasPrefix(authHeader, "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authHeader, "Bearer ")), []byte(authToken)) != 1 {
					w.Header().Set("WWW-Authenticate", `Bearer realm="synapses"`)
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": csrfToken}) //nolint:errcheck
	})

	// ── HTTP server ───────────────────────────────────────────────────────────
	// Layer order (outermost → innermost):
	//
	//   hostGuard → finalHandler (CORS + CSP + OPTIONS + write deadline)
	//     └─ mutationGuard (Origin + CSRF on POST/PUT/DELETE)
	//       └─ authMiddleware → mux
	//
	// CORS headers are set BEFORE auth/mutation checks run. This guarantees
	// that rejection responses carry the Access-Control-Allow-* headers a
	// browser needs to surface the real error rather than an opaque CORS error.
	authProtected := authMiddleware(authToken, mux)
	mutProtected := mutationGuard(csrfToken, authProtected)
	finalHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers on all responses (CSP, X-Frame-Options, nosniff).
		setSecurityHeaders(w)

		// CORS: reflect origin only for known-safe origins (explicit allowlist).
		// Wildcard (*) is intentionally not used — see isCORSAllowedOrigin.
		if origin := r.Header.Get("Origin"); origin != "" {
			if isCORSAllowedOrigin(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-CSRF-Token")
				w.Header().Set("Vary", "Origin")
			}
			// Disallowed origin: no CORS headers set → browser blocks the request.
		}
		if r.Method == http.MethodOptions {
			// Always 204 for OPTIONS. If origin is disallowed, ACAO was not set
			// above, so the browser will block the follow-up actual request.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Per-request write deadline for non-SSE endpoints. The server-level
		// WriteTimeout is 0 (required for SSE/MCP streams), so we set a 60s
		// deadline on regular REST/admin endpoints to prevent slow-client
		// resource exhaustion.
		if !strings.HasPrefix(r.URL.Path, "/mcp") && !strings.HasPrefix(r.URL.Path, "/api/admin/pulse/stream") && !strings.HasPrefix(r.URL.Path, "/api/admin/ollama/pull") {
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
		}
		mutProtected.ServeHTTP(w, r)
	})
	secureHandler := hostGuard(finalHandler)
	httpSrv := &http.Server{
		Addr:           DaemonHTTPAddr,
		Handler:        secureHandler,
		ReadTimeout:    60 * time.Second,
		WriteTimeout:   0, // SSE streams can be indefinite
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 * 1024,
	}

	logutil.Info("synapses %s singleton daemon starting on %s\n", version, DaemonHTTPAddr)

	// ── Background self-update check (every 6 hours, silent) ─────────────────
	startSelfUpdateLoop(appCtx)

	// ── Signal handling ──────────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintln(os.Stderr) // visual separator
		logutil.Info("synapses daemon: received %s, shutting down\n", sig)
		emitLifecycleEvent("daemon_stopping", map[string]any{
			"signal":      sig.String(),
			"uptime_secs": int(time.Since(daemonStartedAt).Seconds()),
			"projects":    len(reg.All()),
		})
		appCancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		httpSrv.Shutdown(shutCtx) //nolint:errcheck
		saveKnownProjectWg.Wait()
	}()

	// ── Start HTTP server ─────────────────────────────────────────────────────
	// Try socket activation first (launchd on macOS, systemd on Linux).
	// If the OS supervisor provides a pre-opened listener, use it.
	// This keeps port 11435 available even during daemon restarts.
	listenSource := "tcp"
	ln, err := trySocketActivation()
	if err != nil {
		logutil.Warn("socket activation: %v (falling back to TCP)\n", err)
	}
	if ln == nil {
		// Fallback: direct TCP bind.
		ln, err = net.Listen("tcp", DaemonHTTPAddr)
		if err != nil {
			if isAddrInUse(err) {
				return fmt.Errorf(
					"port %s is already in use.\n\n"+
						"This usually means:\n"+
						"  • A previous Synapses daemon is still running (check: lsof -i :%s)\n"+
						"  • Another app is occupying port %s\n\n"+
						"To fix:\n"+
						"  1. If a Synapses daemon is running: synapses daemon stop (or kill the process)\n"+
						"  2. If the PID file is stale: rm ~/.synapses/daemon.pid\n"+
						"  3. If another app owns the port: stop that app or change DaemonHTTPAddr\n\n"+
						"Original error: %w",
					DaemonHTTPAddr, daemonPort(), daemonPort(), err)
			}
			return fmt.Errorf("http listen: %w", err)
		}
	} else {
		listenSource = "socket_activation"
		logutil.Info("synapses daemon: using socket activation listener\n")
	}

	// BUG-028: cap concurrent connections to prevent FD exhaustion from
	// misbehaving clients (e.g. many SSE streams from a single process).
	const maxConns = 512
	ln = netutil.LimitListener(ln, maxConns)
	emitLifecycleEvent("daemon_ready", map[string]any{
		"addr":   DaemonHTTPAddr,
		"source": listenSource,
	})
	httpSrv.Addr = "" // already bound via listener
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	logutil.Info("synapses daemon: stopped\n")
	return nil
}

// buildHealthHandler returns the http.HandlerFunc for GET /v1/health.
//
// Extracted from the cmdDaemonServe inline closure so it can be unit-tested
// independently of the full daemon setup.
//
// Latency design: two I/O-bound operations dominate per-project health checks:
//
//   - brain.HealthCheck makes a network call; impl.Available() has its own
//     internal 2-second timeout that ignores the caller's context, so a
//     sequential loop over N projects would block for up to 2s × N.
//   - federation.Status opens SQLite files and performs disk I/O per sibling;
//     with a shared context the first slow project drains the time budget for
//     all others.
//
// Both are parallelised: each project runs in its own goroutine, and within a
// project the brain check and federation check run concurrently.  Overall
// handler latency is max(individual latencies) instead of the sum.  Each
// project also gets its own 3-second federation context so that a slow project
// cannot starve its siblings.
func buildHealthHandler(reg *projectRegistry, sharedPulse *pulse.Client, daemonStartedAt time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Pulse data is best-effort — respond even when pulse is disabled.
		var snap map[string]interface{}
		if sharedPulse != nil {
			snap = sharedPulse.GetHealthSnapshot()
		} else {
			snap = map[string]interface{}{"status": "ok"}
		}

		// Daemon uptime.
		snap["uptime_secs"] = int(time.Since(daemonStartedAt).Seconds())

		// Collect per-project health in parallel.
		projects := reg.All()
		snap["project_count"] = len(projects)

		type projectResult struct {
			nodes, edges, memories int
			watcherDead            bool
			brainModel             string   // empty when brain unavailable
			ollamaModels           []string // installed Ollama models (from first client that returns any)
			fedHealthy, fedStale   int
		}

		results := make([]projectResult, len(projects))
		var outerWg sync.WaitGroup

		// Cap outer parallelism: each project's federation check may open up to
		// FederationParallelism (8) SQLite files simultaneously.  Without a cap,
		// a daemon with many registered projects could exhaust file descriptors.
		// 16 projects × 8 FD each × 2 (brain+fed) = 256 FDs — well within the
		// typical ulimit of 1024.
		const maxHealthParallel = 16
		sem := make(chan struct{}, maxHealthParallel)

		for i, pi := range projects {
			i, pi := i, pi
			outerWg.Add(1)
			sem <- struct{}{} // acquire slot before launching goroutine
			go func() {
				defer func() { <-sem }() // release slot
				defer outerWg.Done()
				pr := &results[i]

				// Cheap reads — safe from any goroutine (graph/store are thread-safe).
				if pi.Graph != nil {
					pr.nodes = pi.Graph.NodeCount()
					pr.edges = pi.Graph.EdgeCount()
				}
				if pi.Store != nil {
					pr.memories = pi.Store.CountEmbeddableMemories()
				}
				if pi.Watcher != nil && !pi.Watcher.IsAlive() {
					pr.watcherDead = true
				}

				// Brain check and federation check are I/O-bound; run concurrently
				// so neither blocks the other.  They write to different struct fields
				// (pr.brainModel vs pr.fedHealthy/fedStale) — no data race.
				var innerWg sync.WaitGroup
				if pi.BrainClient != nil {
					innerWg.Add(1)
					go func() {
						defer innerWg.Done()
						// impl.Available() uses context.Background() internally and
						// cannot be cancelled from outside.  Running in a goroutine
						// prevents it from lengthening the sequential chain.
						if model, _ := pi.BrainClient.HealthCheck(r.Context()); model != "" {
							pr.brainModel = model
						}
						// Best-effort: list installed Ollama models for diagnostics.
						// Run here so it's parallel with federation (same 3s budget).
						mctx, mcancel := context.WithTimeout(r.Context(), 3*time.Second)
						pr.ollamaModels = pi.BrainClient.ListInstalledModels(mctx)
						mcancel()
					}()
				}
				if pi.FederationResolver != nil {
					innerWg.Add(1)
					go func() {
						defer innerWg.Done()
						// Per-project context: a slow project exhausts only its own
						// 3-second budget, not the budgets of all other projects.
						pCtx, pCancel := context.WithTimeout(r.Context(), 3*time.Second)
						defer pCancel()
						for _, es := range pi.FederationResolver.Status(pCtx) {
							if es.Status == "indexed" {
								pr.fedHealthy++
							} else {
								pr.fedStale++
							}
						}
					}()
				}
				innerWg.Wait()
			}()
		}
		outerWg.Wait()

		// Aggregate results.
		var totalNodes, totalEdges, totalMemories, watchersDead, fedHealthy, fedStale int
		brainModelSet := make(map[string]struct{})
		ollamaModelSet := make(map[string]struct{})
		for _, pr := range results {
			totalNodes += pr.nodes
			totalEdges += pr.edges
			totalMemories += pr.memories
			if pr.watcherDead {
				watchersDead++
			}
			if pr.brainModel != "" {
				brainModelSet[pr.brainModel] = struct{}{}
			}
			for _, m := range pr.ollamaModels {
				ollamaModelSet[m] = struct{}{}
			}
			fedHealthy += pr.fedHealthy
			fedStale += pr.fedStale
		}

		// Deterministic brain model output: sort distinct names so the response
		// is stable across calls even when multiple projects use different models.
		brainAvailable := len(brainModelSet) > 0
		if brainAvailable {
			models := make([]string, 0, len(brainModelSet))
			for m := range brainModelSet {
				models = append(models, m)
			}
			sort.Strings(models)
			snap["brain_model"] = strings.Join(models, ",")
		}

		// Expose installed Ollama models collected in parallel above.
		if len(ollamaModelSet) > 0 {
			installed := make([]string, 0, len(ollamaModelSet))
			for m := range ollamaModelSet {
				installed = append(installed, m)
			}
			sort.Strings(installed)
			snap["ollama_installed_models"] = installed
		}

		if watchersDead > 0 {
			snap["status"] = "degraded"
		}
		snap["total_nodes"] = totalNodes
		snap["total_edges"] = totalEdges
		snap["total_memories"] = totalMemories
		snap["watchers_dead"] = watchersDead
		snap["brain_available"] = brainAvailable
		snap["federation_healthy"] = fedHealthy
		snap["federation_stale"] = fedStale

		// Last index time and embedding model status from pulse events.
		if sharedPulse != nil {
			if t := sharedPulse.GetLastIndexTime(); t != "" {
				snap["last_index_time"] = t
			}
			snap["embedding_status"] = sharedPulse.GetLatestEmbeddingModelStatus()
		} else {
			snap["embedding_status"] = "none"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap)
	}
}

// emitLifecycleEvent writes a structured JSON event to stdout (which is
// daemon.log when running as a detached process). These events form a
// diagnostic ladder: empty log = binary never ran, "daemon_starting" without
// "daemon_ready" = crash during init, etc.
func emitLifecycleEvent(event string, fields map[string]any) {
	rec := map[string]any{
		"time":  time.Now().UTC().Format(time.RFC3339),
		"event": event,
	}
	for k, v := range fields {
		rec[k] = v
	}
	data, err := json.Marshal(rec)
	if err != nil {
		// Fallback: at least write something.
		fmt.Fprintf(os.Stdout, `{"time":"%s","event":"%s","error":"marshal failed"}`+"\n",
			time.Now().UTC().Format(time.RFC3339), event)
		return
	}
	fmt.Fprintln(os.Stdout, string(data))
}

// ── initProjectInstance: bootstrap one project in the singleton daemon ────────

// initProjectInstance loads/builds all resources for one project and starts
// its per-project Unix socket listener (for stdio proxy backward compat)
// and registers an HTTP MCP handler for the project.
func initProjectInstance(appCtx context.Context, absPath string, sharedPulse *pulse.Client, reg *projectRegistry) (*ProjectInstance, error) {
	projCtx, projCancel := context.WithCancel(appCtx)

	cfgDir, found := config.FindConfigDir(absPath)
	if found && cfgDir != absPath {
		logutil.InfoP(projectHash(absPath), "synapses: using config from %s\n", cfgDir)
	}
	cfg, err := config.Load(cfgDir)
	if err != nil {
		projCancel()
		return nil, fmt.Errorf("load config: %w", err)
	}

	// Auto-detect knowledge mode: if no synapses.json AND no supported source
	// files in the project directory, default to knowledge mode.
	if cfg.Mode == "" && !found {
		if !hasSourceFiles(absPath) {
			cfg.Mode = "knowledge"
			logutil.InfoP(projectHash(absPath), "synapses: no source files detected, starting in knowledge mode\n")
		}
	}

	dbPath, err := store.DefaultPath(absPath)
	if err != nil {
		projCancel()
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		projCancel()
		return nil, fmt.Errorf("open store: %w", err)
	}
	// Prune stale operational data at startup and then daily.
	// Prevents unbounded growth of tool_calls, events, agent_messages, episodes,
	// and sessions tables during long daemon uptime (weeks/months).
	// PruneOldSessions is also triggered on each session_init (hourly debounce),
	// but including it here ensures sessions are cleaned up even when no agents
	// connect for days (e.g. a paused project still running in the background).
	go func() {
		// Check ctx before the initial prune — don't run if already shutting down.
		if projCtx.Err() != nil {
			return
		}
		st.PruneStaleData(projCtx, 30)
		st.PruneOldSessions(90 * 24 * time.Hour) //nolint:errcheck
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				logutil.InfoP(projectHash(absPath), "synapses: daily prune running (30-day retention, 90-day sessions)\n")
				st.PruneStaleData(projCtx, 30)
				st.PruneOldSessions(90 * 24 * time.Hour) //nolint:errcheck
			case <-projCtx.Done():
				return
			}
		}
	}()

	// ── Knowledge mode: skip graph, parsing, watcher, federation ──────────
	if cfg.Mode == "knowledge" {
		mcpsrv.Version = version
		srv := mcpsrv.NewKnowledge(cfg, st)
		srv.SetProjectID(pathProjectID(absPath))
		srv.SetProjectPath(absPath)
		srv.SetProjectRegistry(&registryAdapter{reg: reg})
		srv.SetUpdateChecker(getPendingUpdateVersion)
		srv.StartBackground()

		if sharedPulse != nil {
			srv.SetPulseClient(sharedPulse)
		}

		loadAndSetPrompts(srv, absPath)

		httpHandler := mcpserver.NewStreamableHTTPServer(srv.MCPServer())
		startProjectSocket(projCtx, srv, absPath, reg)

		// Wire embedder into knowledge-mode server so rank_candidates is usable.
		knowledgeEmbedder := createMemoryEmbedder(cfg)
		if knowledgeEmbedder != nil {
			srv.SetMemoryEmbedder(knowledgeEmbedder)
			go func() {
				if err := knowledgeEmbedder.WarmUp(projCtx); err != nil {
					logutil.Warn("synapses: knowledge-mode embedder warmup: %v\n", err)
				}
			}()
		}

		logutil.Info("synapses: project ready — %s (knowledge mode)\n", filepath.Base(absPath))

		return &ProjectInstance{
			AbsPath:     absPath,
			Store:       st,
			MCPServer:   srv,
			HTTPHandler: httpHandler,
			cancel:      projCancel,
		}, nil
	}

	// Plugin security: per-machine opt-in for external parser commands.
	var pluginCheck *parser.PluginChecker
	if len(cfg.Plugins) > 0 {
		sHome, homeErr := synapsesHome()
		if homeErr != nil {
			logutil.Warn("cannot determine synapses home: %v (plugins disabled)\n", homeErr)
			cfg.Plugins = nil // fail-closed: cannot verify plugins → disable them
		} else {
			pluginCheck = parser.NewPluginChecker(sHome)
		}
	}

	g, err := loadOrBuildGraphWithStore(absPath, st, false, cfg.Plugins, pluginCheck, sharedPulse, pathProjectID(absPath))
	if err != nil {
		st.Close()
		projCancel()
		return nil, err
	}
	// Set graph root so search/investigate handlers can read source files.
	g.SetRoot(absPath)

	// Federation.
	for _, linkedPath := range cfg.Linked {
		if mergeErr := mergeLinkedProject(g, linkedPath); mergeErr != nil {
			logutil.WarnP(projectHash(absPath), "synapses: skipping linked project %s: %v\n",
				linkedPath, mergeErr)
		}
	}
	if len(cfg.Linked) > 0 {
		if sites, err := st.LoadCallSites(); err == nil && len(sites) > 0 {
			for _, cs := range sites {
				g.AddCallSite(cs)
			}
			if n := resolver.ResolveCallEdges(g); n > 0 {
				logutil.InfoP(projectHash(absPath), "synapses: resolved %d cross-project CALLS edges\n",
					n)
			}
		}
	}

	applyGoTypesIfEnabled(g, absPath, cfg)
	applyTSTypesIfEnabled(g, absPath, cfg)
	enrichMetricsIfEnabled(g, absPath, cfg)
	analyzeDataFlowIfEnabled(g, cfg)

	// D1: Activate FlatGraph SoA CSR fast path for BFS/PPR traversal.
	// EnableFlatGraph builds the cache-friendly adjacency representation that
	// eliminates pointer-chasing through g.outEdges on every PPR hop.
	if cfg.UseFlatGraph {
		g.EnableFlatGraph()
		logutil.InfoP(projectHash(absPath), "synapses: FlatGraph CSR enabled (%d nodes)\n", g.NodeCount())
	}

	// Context file (auto-injected session context).
	writeCtxFile := func() {
		identity := g.ProjectIdentity()
		var tasks []store.Task
		if pending, err := st.GetPendingTasks("", ""); err == nil {
			tasks = pending
		}
		_ = contextfile.Write(absPath, identity, tasks)
	}
	go writeCtxFile()
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeCtxFile()
			case <-projCtx.Done():
				return
			}
		}
	}()

	// Background index rebuild — respects project context cancellation.
	go func() {
		blob, err := g.RebuildIndex()
		if err == nil && len(blob) > 0 {
			// Check context before writing to store — project may have been torn down.
			if projCtx.Err() == nil {
				_ = st.SaveIndexSnapshot(blob)
			}
		}
	}()
	// Background idle-defrag.
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				idx := g.Index()
				if idx == nil || !idx.Ready() || idx.TombstoneRatio() < 0.15 {
					continue
				}
				blob, err := g.RebuildIndex()
				if err == nil && len(blob) > 0 {
					_ = st.SaveIndexSnapshot(blob)
				}
			case <-projCtx.Done():
				return
			}
		}
	}()

	// MCP server.
	mcpsrv.Version = version
	srv := mcpsrv.New(g, cfg, st)
	srv.SetProjectID(pathProjectID(absPath))
	srv.SetProjectRegistry(&registryAdapter{reg: reg})
	srv.SetUpdateChecker(getPendingUpdateVersion)
	srv.StartBackground()

	// LSP Manager — type-system-backed confidence upgrades for security findings
	// (Sprint 28.5). Verifiers degrade to no-op when the binary is absent so no
	// config is required. A maintenance goroutine trims idle processes and purges
	// stale cache entries; it closes the manager on project context cancellation.
	lspMgr := lsp.NewManager(lsp.Options{})
	lspMgr.Register(lsp.NewGoplsVerifier(lsp.GoplsVerifierOptions{ProjectRoot: absPath}))
	lspMgr.Register(lsp.NewTsserverVerifier(lsp.TsserverVerifierOptions{ProjectRoot: absPath}))
	lspMgr.Register(lsp.NewPyrightVerifier(lsp.PyrightVerifierOptions{ProjectRoot: absPath}))
	srv.SetLSPManager(lspMgr)
	go func() {
		trimTicker := time.NewTicker(time.Minute)
		purgeTicker := time.NewTicker(10 * time.Minute)
		defer trimTicker.Stop()
		defer purgeTicker.Stop()
		for {
			select {
			case <-trimTicker.C:
				lspMgr.TrimIdle()
			case <-purgeTicker.C:
				lspMgr.PurgeExpired()
			case <-projCtx.Done():
				lspMgr.Close()
				return
			}
		}
	}()

	// Skills / prompts.
	loadAndSetPrompts(srv, absPath)

	// Federation resolver: cross-project drift detection + dependency tracking.
	// Only created when federation entries are configured in synapses.json.
	// The dependency tracker is wired into the watcher later (after watcher creation).
	var fedResolver *federation.Resolver
	if len(cfg.Federation) > 0 {
		fedResolver = federation.NewResolver(cfg.Federation, cfgDir)
		srv.SetFederationResolver(fedResolver)
		go func() {
			<-projCtx.Done()
			fedResolver.Close()
		}()
	}

	// Brain.
	brainCli := brain.NewInProcess(cfg.Brain.ToBrainConfig())
	if cfg.Brain.Enabled {
		srv.SetBrainClient(brainCli)
		if fedResolver != nil {
			fedResolver.SetBrain(brainCli)
			fedResolver.SetBrainGenerate(brainCli.Generate)
		}
		go func() {
			go fetchTopNSummaries(projCtx, brainCli, g, st, 20)
			bulkIngestToBrain(projCtx, brainCli, g, pathProjectID(absPath))
			fetchAndWriteBackSummaries(projCtx, brainCli, g, st)
		}()
	}

	srv.SetProjectPath(absPath)

	// Tech stack detection (no longer enriched via scout).
	go func() {
		entries := scout.DetectTechStack(absPath)
		if len(entries) > 0 {
			srv.SetTechStack(entries)
		}
	}()

	// Pulse.
	if sharedPulse != nil {
		srv.SetPulseClient(sharedPulse)
	}

	// Node embeddings (semantic search) — prefer external endpoint, fall back to builtin.
	var nodeEmbedder embed.Embedder
	if cfg.EmbeddingEndpoint != "" {
		nodeEmbedder = embed.NewClient(cfg.EmbeddingEndpoint, "")
	}

	// Memory embeddings (recall vector search).
	memEmbedder := createMemoryEmbedder(cfg)
	if memEmbedder != nil {
		// Wire model download events to pulse for first-startup observability.
		if be, ok := memEmbedder.(*embed.BuiltinEmbedder); ok && sharedPulse != nil {
			pc := sharedPulse
			be.OnModelEvent = func(eventType string) {
				logutil.Info("synapses: embed model event: %s\n", eventType)
				pc.RecordLifecycleEvent(eventType, 0, "")
			}
		}
		srv.SetMemoryEmbedder(memEmbedder)
		go func() {
			if err := memEmbedder.WarmUp(projCtx); err != nil {
				logutil.Warn("synapses: embedder warmup: %v\n", err)
			}
		}()
		go embedAllMemories(projCtx, memEmbedder, st, sharedPulse)

		// Use builtin embedder for node embeddings when no external endpoint is set.
		if nodeEmbedder == nil {
			nodeEmbedder = memEmbedder
		}
	}

	// Wire node embedder for semantic search and kick off background embedding.
	if nodeEmbedder != nil {
		srv.SetEmbedClient(nodeEmbedder)
		go embedAllNodes(projCtx, nodeEmbedder, g, st)
	}

	// File watcher.
	var fw *watcher.Watcher
	w := parser.NewWalker()
	for _, p := range cfg.Plugins {
		w.RegisterPlugin(p.Extensions, p.Command, pluginCheck)
	}
	fw2, err := watcher.New(g, w, st)
	if err == nil {
		if startErr := fw2.Start(absPath); startErr == nil {
			fw = fw2
			fw.SetConfig(cfg)
			fw.SetProjectID(pathProjectID(absPath))
			if sharedPulse != nil {
				fw.SetPulseClient(sharedPulse) // P2-3: wire pulse for reparse events
			}
			srv.SetChangeSource(fw)
			fw.SetPacketInvalidator(srv)
			fw.SetBrainClient(brainCli)
			// Wire cross-domain name matcher: runs after each reindex to create MENTIONS edges.
			nm := namematcher.New(brainCli)
			nm.PrimeCrossDomain(g) // prime flag from already-loaded graph
			fw.SetNameMatcher(nm)
			// D1: Keep FlatGraph CSR in sync after each graph rebuild.
			if cfg.UseFlatGraph {
				fw.SetAfterRebuildHook(func() { g.EnableFlatGraph() })
			}
			// Eager re-embed: when watcher marks embeddings stale, immediately
			// queue them for re-embedding instead of waiting for the 60s retry loop.
			if memEmbedder != nil {
				fw.SetOnStaleEmbeddings(func(memoryIDs []string) {
					for _, memID := range memoryIDs {
						content, ok := st.GetMemoryContent(memID)
						if !ok || content == "" {
							continue
						}
						srv.QueueEmbedMemory(projCtx, memEmbedder, st, memID, content)
					}
				})
			}
			// Wire federation dependency tracker into the watcher so
			// cross-project imports are detected on every file re-parse.
			var fedTracker *federation.DeterministicDetector
			fw.SetConfigChangeHandler(func(newCfg *config.Config) {
				newBrain := brain.NewInProcess(newCfg.Brain.ToBrainConfig())
				srv.SetBrainClient(newBrain)
				fw.SetBrainClient(newBrain)
				fw.SetNameMatcher(namematcher.New(newBrain))
				if fedTracker != nil {
					fedTracker.Rebuild(newCfg.Federation)
				}
			})
			if fedResolver != nil {
				fedTracker = federation.NewDeterministicDetector(cfg.Federation, fedResolver)
				fw.SetCrossProjectTracker(fedTracker)
				// Tier 2: brain-enhanced cross-project detection for languages
				// Tier 1 doesn't cover well (Python, Ruby, Java, etc.).
				if cfg.Brain.Enabled {
					aliases := fedResolver.Aliases()
					brainDet := federation.NewBrainDetector(brainCli.Generate, fedResolver, aliases)
					brainAdapter := federation.NewBrainTrackerAdapter(brainDet, fedTracker)
					if brainAdapter != nil {
						fw.SetBrainCrossProjectTracker(brainAdapter)
					}
				}
			}
		}
	}

	// HTTP MCP handler for this project (used via /mcp?project=<path>).
	httpHandler := mcpserver.NewStreamableHTTPServer(srv.MCPServer())
	startProjectSocket(projCtx, srv, absPath, reg)

	identity := g.ProjectIdentity()
	logutil.Info("synapses: project ready — %s (%d nodes, %d edges)\n",
		identity.RepoID,
		identity.Summary.Files+identity.Summary.Functions+
			identity.Summary.Structs+identity.Summary.Interfaces,
		identity.Summary.Edges)

	return &ProjectInstance{
		AbsPath:            absPath,
		Graph:              g,
		Store:              st,
		MCPServer:          srv,
		HTTPHandler:        httpHandler,
		BrainClient:        brainCli,
		Watcher:            fw,
		MemoryEmbedder:     memEmbedder,
		FederationResolver: fedResolver,
		cancel:             projCancel,
	}, nil
}

// serveProjectSocket accepts MCP sessions on the per-project Unix socket.
// This provides backward compatibility for "synapses start" stdio proxies.
// reg and absPath are used to dynamically look up the registry entry for
// activeConns tracking. If reg is nil, connection tracking is disabled.
func serveProjectSocket(ctx context.Context, srv *mcpsrv.Server, listener net.Listener, reg *projectRegistry, absPath string) {
	// Limit concurrent connections per socket to prevent FD exhaustion.
	listener = netutil.LimitListener(listener, 64)

	defer func() {
		listener.Close()
		// Remove the socket file on exit.
		if ul, ok := listener.(*net.UnixListener); ok {
			_ = ul.Close()
			// Retrieve socket path via the addr.
			if addr := listener.Addr(); addr != nil {
				os.Remove(addr.String())
			}
		}
	}()

	// Stop accepting when context is cancelled.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	var connID atomic.Int64
	var wg sync.WaitGroup

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			continue
		}

		id := connID.Add(1)
		sessionID := fmt.Sprintf("sock-%d", id)
		wg.Add(1)
		go func(c net.Conn, sid string) {
			defer wg.Done()
			defer c.Close()
			// Track active proxy connections for hibernation sweeper.
			// Dynamic lookup ensures tracking works even when the socket was
			// started before the registry entry existed (cold init path).
			if reg != nil {
				if entry := reg.getEntry(absPath); entry != nil {
					entry.activeConns.Add(1)
					defer func() {
						// Re-lookup on close: entry may have been replaced by
						// hibernate/wake cycle. Decrement whichever entry exists.
						if e := reg.getEntry(absPath); e != nil {
							e.activeConns.Add(-1)
						}
					}()
				}
			}
			if err := serveMCPConn(ctx, srv.MCPServer(), srv, c, sid); err != nil {
				if !strings.Contains(err.Error(), "EOF") &&
					!strings.Contains(err.Error(), "use of closed") {
					logutil.Error("synapses socket session %s error: %v\n", sid, err)
				}
			}
		}(conn, sessionID)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// serveMCPConn handles one MCP session over a Unix socket connection.
func serveMCPConn(ctx context.Context, mcpSrv *mcpserver.MCPServer, synSrv *mcpsrv.Server, conn net.Conn, sessionID string) error {
	session := &connSession{
		id:            sessionID,
		notifications: make(chan mcp.JSONRPCNotification, 100),
	}

	if err := mcpSrv.RegisterSession(ctx, session); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	defer mcpSrv.UnregisterSession(ctx, sessionID)
	defer synSrv.ClearSessionHashes(sessionID)
	defer synSrv.ClearSynapseSession(sessionID)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sessionCtx = mcpsrv.WithSessionID(sessionCtx, sessionID)
	sessionCtx = mcpSrv.WithContext(sessionCtx, session)

	// Unblock any blocked reads when context is cancelled by setting an
	// immediate read deadline. This is necessary because bufio.ReadSlice
	// blocks on the underlying conn and does not observe context cancellation.
	// Must be launched after sessionCtx is fully built to avoid a data race.
	go func() {
		<-sessionCtx.Done()
		conn.SetReadDeadline(time.Now())
	}()

	var writeMu sync.Mutex
	writeJSON := func(v interface{}) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		writeMu.Lock()
		_, err = conn.Write(data)
		writeMu.Unlock()
		return err
	}

	go func() {
		for {
			select {
			case notif := <-session.notifications:
				if err := writeJSON(notif); err != nil {
					return
				}
			case <-sessionCtx.Done():
				return
			}
		}
	}()

	const maxMsgBytes = 4 * 1024 * 1024 // 4 MiB per-message limit
	reader := bufio.NewReader(conn)
	for {
		if sessionCtx.Err() != nil {
			return sessionCtx.Err()
		}
		// readBoundedLine reads one '\n'-terminated line using ReadSlice so
		// memory allocation is bounded to maxMsgBytes + internal buffer size.
		// bufio.ReadString accumulates the full message before returning, which
		// would allow a single large message to OOM the daemon before the size
		// check fires. ReadSlice returns ErrBufferFull for each internal-buffer
		// chunk, letting us enforce the limit incrementally.
		var (
			lineBuf  []byte
			lineErr  error
			tooLarge bool
		)
		for {
			frag, sliceErr := reader.ReadSlice('\n')
			if !tooLarge {
				if len(lineBuf)+len(frag) > maxMsgBytes {
					tooLarge = true
					lineBuf = nil // release already-accumulated memory
				} else {
					lineBuf = append(lineBuf, frag...)
				}
			}
			// sliceErr == nil means delimiter found; anything else means keep
			// reading (ErrBufferFull) or a real connection error.
			if sliceErr == nil {
				lineErr = nil
				break
			}
			if sliceErr != bufio.ErrBufferFull {
				lineErr = sliceErr
				break
			}
		}
		if lineErr != nil {
			return lineErr
		}
		if tooLarge {
			writeJSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]interface{}{"code": -32600, "message": "message exceeds 4 MiB limit"},
			}) //nolint:errcheck
			continue
		}
		line := strings.TrimSpace(string(lineBuf))
		if line == "" {
			continue
		}
		var rawMsg json.RawMessage
		if err := json.Unmarshal([]byte(line), &rawMsg); err != nil {
			writeJSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]interface{}{"code": -32700, "message": "Parse error"},
			}) //nolint:errcheck
			continue
		}
		response := mcpSrv.HandleMessage(sessionCtx, rawMsg)
		if response != nil {
			if err := writeJSON(response); err != nil {
				return err
			}
		}
	}
}

// hasSourceFiles checks if the directory contains any files with extensions
// supported by Synapses parsers. Uses a bounded walk (max 4 levels, max 200
// entries) to handle standard layouts like Maven/Gradle (src/main/java/...)
// and Rust (src/lib.rs) without being too slow on huge repos.
func hasSourceFiles(dir string) bool {
	sourceExts := map[string]bool{
		".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
		".rs": true, ".java": true, ".kt": true, ".swift": true, ".c": true, ".cpp": true,
		".cs": true, ".rb": true, ".php": true, ".scala": true, ".dart": true,
		".vue": true, ".svelte": true, ".zig": true, ".lua": true, ".ex": true, ".erl": true,
	}
	checked := 0
	const maxChecked = 500
	const maxDepth = 6

	var walk func(path string, depth int) bool
	walk = func(path string, depth int) bool {
		if depth > maxDepth || checked >= maxChecked {
			return false
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return false
		}
		for _, e := range entries {
			checked++
			if checked > maxChecked {
				return false
			}
			if e.IsDir() {
				// Skip known non-source directories.
				name := e.Name()
				if name == "node_modules" || name == ".git" || name == "vendor" ||
					name == "__pycache__" || name == "build" || name == "target" ||
					name == "dist" || name == ".gradle" {
					continue
				}
				if walk(filepath.Join(path, name), depth+1) {
					return true
				}
				continue
			}
			ext := filepath.Ext(e.Name())
			if sourceExts[ext] {
				return true
			}
		}
		return false
	}
	return walk(dir, 0)
}

// jsonDepth returns the maximum nesting depth of a decoded JSON value.
// Used to reject excessively nested request bodies that amplify memory usage.
func jsonDepth(v interface{}) int {
	switch val := v.(type) {
	case map[string]interface{}:
		maxChild := 0
		for _, child := range val {
			if d := jsonDepth(child); d > maxChild {
				maxChild = d
			}
		}
		return 1 + maxChild
	case []interface{}:
		maxChild := 0
		for _, child := range val {
			if d := jsonDepth(child); d > maxChild {
				maxChild = d
			}
		}
		return 1 + maxChild
	default:
		return 0
	}
}
