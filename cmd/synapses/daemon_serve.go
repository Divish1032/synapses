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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
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
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/parser"
	"github.com/SynapsesOS/synapses/internal/pulse"
	"github.com/SynapsesOS/synapses/internal/resolver"
	"github.com/SynapsesOS/synapses/internal/scout"
	"github.com/SynapsesOS/synapses/internal/skills"
	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/SynapsesOS/synapses/internal/watcher"
	"github.com/SynapsesOS/synapses/internal/webcache"
)

// DaemonHTTPPort is the fixed port for the singleton daemon HTTP server.
// Port 11434 is Ollama's default — we use 11435 to avoid conflicts.
const DaemonHTTPPort = "11435"

// restSessionCounter generates unique per-request session IDs for REST API calls.
// Each POST /v1/tools/{name} request gets an isolated "rest-N" session context.
var restSessionCounter atomic.Int64

// DaemonHTTPAddr is the loopback address the singleton daemon binds to.
const DaemonHTTPAddr = "127.0.0.1:" + DaemonHTTPPort

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
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !processAlive(pid) {
		os.Remove(pidPath)
	}
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
		token := strings.TrimSpace(string(data))
		if len(token) == 64 {
			return token, nil
		}
		// File exists but is invalid (truncated, corrupted) — regenerate.
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
	if origin == "tauri://localhost" {
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
	if token == "" {
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
			fmt.Fprintf(os.Stderr, "synapses: auth: missing token from %s %s\n", r.RemoteAddr, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", `Bearer realm="synapses"`)
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing Authorization header; use Bearer token"}) //nolint:errcheck
			return
		}
		provided := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			fmt.Fprintf(os.Stderr, "synapses: auth: invalid token from %s %s\n", r.RemoteAddr, r.URL.Path)
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

func (s *connSession) SessionID() string                                  { return s.id }
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
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}
	defer os.Remove(pidPath)

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
			fmt.Fprintf(os.Stderr, "synapses: pulse analytics enabled (in-process, db: %s)\n", pulseDBPath)
		}
	}

	// ── HTTP MCP router ───────────────────────────────────────────────────────
	// /mcp?project=<absPath>  → per-project StreamableHTTPServer
	// /api/admin/*            → daemon management API
	mux := http.NewServeMux()

	// Admin: health
	mux.HandleFunc("/api/admin/health", func(w http.ResponseWriter, r *http.Request) {
		// Health endpoint is exempt from auth for liveness checks.
		// Only return project_count (not paths) to avoid information disclosure.
		// Full project details available at /api/admin/projects (auth-protected).
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":            "ok",
			"version":           version,
			"project_count":     len(reg.All()),
			"indexing_progress": ActiveSnapshot(),
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
			var req struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			absPath, err := canonicalPath(req.Path)
			if err != nil {
				http.Error(w, "invalid path: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := isValidProjectPath(absPath); err != nil {
				http.Error(w, "invalid project path: "+err.Error(), http.StatusBadRequest)
				return
			}
			sockPath, err := daemonSocketPath(absPath)
			if err != nil {
				http.Error(w, "socket path error: "+err.Error(), http.StatusInternalServerError)
				return
			}
			// GetOrSet: lazy-initialize the project if not already registered.
			_, initErr := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
				return initProjectInstance(appCtx, absPath, sharedPulse, reg)
			})
			if initErr != nil {
				http.Error(w, "init project: "+initErr.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "ok",
				"path":   absPath,
				"socket": sockPath,
			})

		case http.MethodDelete:
			var req struct {
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			absPath, _ := canonicalPath(req.Path)
			reg.Delete(absPath)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Admin: pulse analytics summary
	mux.HandleFunc("/api/admin/pulse/summary", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		days := 7
		if d := r.URL.Query().Get("days"); d != "" {
			if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
				days = n
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sharedPulse.GetSummary(days))
	})

	// MCP: route to per-project StreamableHTTPServer
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
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
			http.Error(w, "invalid project path: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := isValidProjectPath(absPath); err != nil {
			http.Error(w, "invalid project path: "+err.Error(), http.StatusBadRequest)
			return
		}

		pi, initErr := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
			return initProjectInstance(appCtx, absPath, sharedPulse, reg)
		})
		if initErr != nil {
			http.Error(w, "init project: "+initErr.Error(), http.StatusInternalServerError)
			return
		}

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
	mux.HandleFunc("/v1/tools/", func(w http.ResponseWriter, r *http.Request) {
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
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid project path: " + err.Error()}) //nolint:errcheck
			return
		}
		if err := isValidProjectPath(absPath); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid project path: " + err.Error()}) //nolint:errcheck
			return
		}

		pi, initErr := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
			return initProjectInstance(appCtx, absPath, sharedPulse, reg)
		})
		if initErr != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "init project: " + initErr.Error()}) //nolint:errcheck
			return
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
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON body: " + decodeErr.Error()}) //nolint:errcheck
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
				json.NewEncoder(w).Encode(map[string]string{"error": dispatchErr.Error()}) //nolint:errcheck
			} else {
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": dispatchErr.Error()}) //nolint:errcheck
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result) //nolint:errcheck
	})

	// ── Auth token ────────────────────────────────────────────────────────────
	// Generated on first start, persisted at ~/.synapses/auth_token.
	// Required only for non-localhost connections; localhost is always trusted.
	authToken, authErr := loadOrCreateAuthToken()
	if authErr != nil {
		// Non-fatal: log a warning and continue. Localhost-only binding
		// (127.0.0.1) is the primary protection; auth is defence-in-depth.
		fmt.Fprintf(os.Stderr, "synapses: warning: could not load/create auth token: %v\n", authErr)
		authToken = ""
	} else {
		tokenPath, _ := authTokenPath()
		fmt.Fprintf(os.Stderr, "synapses: auth token stored at %s\n", tokenPath)
	}

	// ── HTTP server ───────────────────────────────────────────────────────────
	// Layer order (outermost → innermost):
	//
	//   finalHandler (CORS headers + OPTIONS)
	//     └─ authProtected (authMiddleware → mux)
	//
	// CORS headers are set BEFORE the auth check runs.  This guarantees that
	// 401 rejections carry the Access-Control-Allow-* headers a browser needs
	// to surface the auth error; without this ordering, auth failures look like
	// opaque CORS errors to the caller.
	authProtected := authMiddleware(authToken, mux)
	finalHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS: reflect origin only for known-safe origins (explicit allowlist).
		// Wildcard (*) is intentionally not used — see isCORSAllowedOrigin.
		//
		// Layer order: CORS headers set here (outermost) so that 401 rejections
		// from authMiddleware carry the correct ACAO header, letting browsers
		// surface the auth error rather than an opaque CORS error.
		//
		// Non-browser clients (curl, MCP stdio proxy) send no Origin header;
		// they bypass this block entirely and are unaffected.
		if origin := r.Header.Get("Origin"); origin != "" {
			if isCORSAllowedOrigin(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
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
		authProtected.ServeHTTP(w, r)
	})
	httpSrv := &http.Server{
		Addr:         DaemonHTTPAddr,
		Handler:      finalHandler,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // SSE streams can be indefinite
		IdleTimeout:  120 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "synapses %s singleton daemon starting on %s\n", version, DaemonHTTPAddr)

	// ── Background self-update check (every 6 hours, silent) ─────────────────
	startSelfUpdateLoop(appCtx)

	// ── Signal handling ──────────────────────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nsynapses daemon: received %s, shutting down\n", sig)
		appCancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		httpSrv.Shutdown(shutCtx) //nolint:errcheck
	}()

	// ── Start HTTP server ─────────────────────────────────────────────────────
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	fmt.Fprintf(os.Stderr, "synapses daemon: stopped\n")
	return nil
}

// ── initProjectInstance: bootstrap one project in the singleton daemon ────────

// initProjectInstance loads/builds all resources for one project and starts
// its per-project Unix socket listener (for stdio proxy backward compat)
// and registers an HTTP MCP handler for the project.
func initProjectInstance(appCtx context.Context, absPath string, sharedPulse *pulse.Client, reg *projectRegistry) (*ProjectInstance, error) {
	projCtx, projCancel := context.WithCancel(appCtx)

	cfgDir, found := config.FindConfigDir(absPath)
	if found && cfgDir != absPath {
		fmt.Fprintf(os.Stderr, "synapses [%s]: using config from %s\n", projectHash(absPath), cfgDir)
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
			fmt.Fprintf(os.Stderr, "synapses [%s]: no source files detected, starting in knowledge mode\n", projectHash(absPath))
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
	// Prevents unbounded growth of tool_calls, events, agent_messages, and
	// episodes tables during long daemon uptime (weeks/months).
	go func() {
		st.PruneStaleData(30)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				st.PruneStaleData(30)
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
		srv.StartBackground()

		if sharedPulse != nil {
			srv.SetPulseClient(sharedPulse)
		}

		// Skills / prompts (same as full mode).
		{
			var allPrompts []skills.PromptTemplate
			allPrompts = append(allPrompts, skills.BuiltinPrompts()...)
			if homeDir, err := os.UserHomeDir(); err == nil {
				userDir := filepath.Join(homeDir, ".synapses", "prompts")
				if pts, err := skills.LoadPromptDir(userDir, "user"); err == nil {
					allPrompts = append(allPrompts, pts...)
				}
			}
			projectDir := filepath.Join(absPath, ".synapses", "prompts")
			if pts, err := skills.LoadPromptDir(projectDir, "project"); err == nil {
				allPrompts = append(allPrompts, pts...)
			}
			allPrompts = skills.DeduplicatePrompts(allPrompts)
			if len(allPrompts) > 0 {
				srv.SetPromptTemplates(allPrompts)
			}
		}
		{
			allRecipes := skills.BuiltinRecipes()
			if homeDir, err := os.UserHomeDir(); err == nil {
				userDir := filepath.Join(homeDir, ".synapses", "skills")
				if rs, err := skills.LoadRecipeDir(userDir, "user"); err == nil {
					allRecipes = append(allRecipes, rs...)
				}
			}
			projectDir := filepath.Join(absPath, ".synapses", "skills")
			if rs, err := skills.LoadRecipeDir(projectDir, "project"); err == nil {
				allRecipes = append(allRecipes, rs...)
			}
			allRecipes = skills.DeduplicateRecipes(allRecipes)
			srv.SetSkillRecipes(allRecipes)
		}

		httpHandler := mcpserver.NewStreamableHTTPServer(srv.MCPServer())

		// Per-project Unix socket.
		sockPath, sockErr := daemonSocketPath(absPath)
		if sockErr == nil {
			os.Remove(sockPath)
			listener, listenErr := net.Listen("unix", sockPath)
			if listenErr == nil {
				os.Chmod(sockPath, 0o700) //nolint:errcheck
				go serveProjectSocket(projCtx, srv, listener)
			}
		}

		fmt.Fprintf(os.Stderr, "synapses: project ready — %s (knowledge mode)\n", filepath.Base(absPath))

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
			fmt.Fprintf(os.Stderr, "WARNING: cannot determine synapses home: %v (plugins disabled)\n", homeErr)
			cfg.Plugins = nil // fail-closed: cannot verify plugins → disable them
		} else {
			pluginCheck = parser.NewPluginChecker(sHome)
		}
	}

	g, err := loadOrBuildGraphWithStore(absPath, st, false, cfg.Plugins, pluginCheck)
	if err != nil {
		st.Close()
		projCancel()
		return nil, err
	}

	// Federation.
	for _, linkedPath := range cfg.Linked {
		if mergeErr := mergeLinkedProject(g, linkedPath); mergeErr != nil {
			fmt.Fprintf(os.Stderr, "synapses [%s]: skipping linked project %s: %v\n",
				projectHash(absPath), linkedPath, mergeErr)
		}
	}
	if len(cfg.Linked) > 0 {
		if sites, err := st.LoadCallSites(); err == nil && len(sites) > 0 {
			for _, cs := range sites {
				g.AddCallSite(cs)
			}
			if n := resolver.ResolveCallEdges(g); n > 0 {
				fmt.Fprintf(os.Stderr, "synapses [%s]: resolved %d cross-project CALLS edges\n",
					projectHash(absPath), n)
			}
		}
	}

	applyGoTypesIfEnabled(g, absPath, cfg)
	applyTSTypesIfEnabled(g, absPath, cfg)
	enrichMetricsIfEnabled(g, absPath, cfg)
	analyzeDataFlowIfEnabled(g, cfg)

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

	// Background index rebuild.
	go func() {
		blob, err := g.RebuildIndex()
		if err == nil && len(blob) > 0 {
			_ = st.SaveIndexSnapshot(blob)
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
	srv.StartBackground()

	// Skills / prompts.
	{
		var allPrompts []skills.PromptTemplate
		allPrompts = append(allPrompts, skills.BuiltinPrompts()...)
		if homeDir, err := os.UserHomeDir(); err == nil {
			userDir := filepath.Join(homeDir, ".synapses", "prompts")
			if pts, err := skills.LoadPromptDir(userDir, "user"); err == nil {
				allPrompts = append(allPrompts, pts...)
			}
		}
		projectDir := filepath.Join(absPath, ".synapses", "prompts")
		if pts, err := skills.LoadPromptDir(projectDir, "project"); err == nil {
			allPrompts = append(allPrompts, pts...)
		}
		allPrompts = skills.DeduplicatePrompts(allPrompts)
		if len(allPrompts) > 0 {
			srv.SetPromptTemplates(allPrompts)
		}
	}
	{
		allRecipes := skills.BuiltinRecipes()
		if homeDir, err := os.UserHomeDir(); err == nil {
			userDir := filepath.Join(homeDir, ".synapses", "skills")
			if rs, err := skills.LoadRecipeDir(userDir, "user"); err == nil {
				allRecipes = append(allRecipes, rs...)
			}
		}
		projectDir := filepath.Join(absPath, ".synapses", "skills")
		if rs, err := skills.LoadRecipeDir(projectDir, "project"); err == nil {
			allRecipes = append(allRecipes, rs...)
		}
		allRecipes = skills.DeduplicateRecipes(allRecipes)
		srv.SetSkillRecipes(allRecipes)
	}

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

	// Web doc cache: version-pinned Go package docs, cached locally in SQLite.
	wc := webcache.New(st)
	srv.SetWebCache(wc)
	srv.SetProjectPath(absPath)
	go webcache.IndexProjectImports(projCtx, absPath, g, wc, 20)

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

	// Node embeddings (semantic search).
	if cfg.EmbeddingEndpoint != "" {
		embedCli := embed.NewClient(cfg.EmbeddingEndpoint, "")
		srv.SetEmbedClient(embedCli)
		go embedAllNodes(projCtx, embedCli, g, st)
	}

	// Memory embeddings (recall vector search).
	memEmbedder := createMemoryEmbedder(cfg)
	if memEmbedder != nil {
		srv.SetMemoryEmbedder(memEmbedder)
		go embedAllMemories(projCtx, memEmbedder, st)
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
			srv.SetChangeSource(fw)
			fw.SetPacketInvalidator(srv)
			fw.SetBrainClient(brainCli)
			fw.SetConfigChangeHandler(func(newCfg *config.Config) {
				newBrain := brain.NewInProcess(newCfg.Brain.ToBrainConfig())
				srv.SetBrainClient(newBrain)
				fw.SetBrainClient(newBrain)
			})
			// Wire federation dependency tracker into the watcher so
			// cross-project imports are detected on every file re-parse.
			if fedResolver != nil {
				tracker := federation.NewDeterministicDetector(cfg.Federation, fedResolver)
				fw.SetCrossProjectTracker(tracker)
				// Tier 2: brain-enhanced cross-project detection for languages
				// Tier 1 doesn't cover well (Python, Ruby, Java, etc.).
				if cfg.Brain.Enabled {
					aliases := fedResolver.Aliases()
					brainDet := federation.NewBrainDetector(brainCli.Generate, fedResolver, aliases)
					brainAdapter := federation.NewBrainTrackerAdapter(brainDet, tracker)
					if brainAdapter != nil {
						fw.SetBrainCrossProjectTracker(brainAdapter)
					}
				}
			}
		}
	}

	// HTTP MCP handler for this project (used via /mcp?project=<path>).
	httpHandler := mcpserver.NewStreamableHTTPServer(srv.MCPServer())

	// Per-project Unix socket (for stdio proxy backward compat).
	sockPath, sockErr := daemonSocketPath(absPath)
	if sockErr == nil {
		os.Remove(sockPath) // clean stale socket
		listener, listenErr := net.Listen("unix", sockPath)
		if listenErr == nil {
			os.Chmod(sockPath, 0o700) //nolint:errcheck
			go serveProjectSocket(projCtx, srv, listener)
		}
	}

	identity := g.ProjectIdentity()
	fmt.Fprintf(os.Stderr, "synapses: project ready — %s (%d nodes, %d edges)\n",
		identity.RepoID,
		identity.Summary.Files+identity.Summary.Functions+
			identity.Summary.Structs+identity.Summary.Interfaces,
		identity.Summary.Edges)

	return &ProjectInstance{
		AbsPath:     absPath,
		Graph:       g,
		Store:       st,
		MCPServer:   srv,
		HTTPHandler: httpHandler,
		BrainClient: brainCli,
		Watcher:     fw,
		cancel:      projCancel,
	}, nil
}

// serveProjectSocket accepts MCP sessions on the per-project Unix socket.
// This provides backward compatibility for "synapses start" stdio proxies.
func serveProjectSocket(ctx context.Context, srv *mcpsrv.Server, listener net.Listener) {
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
			if err := serveMCPConn(ctx, srv.MCPServer(), srv, c, sid); err != nil {
				if !strings.Contains(err.Error(), "EOF") &&
					!strings.Contains(err.Error(), "use of closed") {
					fmt.Fprintf(os.Stderr, "synapses socket session %s error: %v\n", sid, err)
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

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sessionCtx = mcpsrv.WithSessionID(sessionCtx, sessionID)
	sessionCtx = mcpSrv.WithContext(sessionCtx, session)

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

	reader := bufio.NewReader(conn)
	for {
		if sessionCtx.Err() != nil {
			return sessionCtx.Err()
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
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
// supported by Synapses parsers. Scans top-level and one level deep to keep it fast.
func hasSourceFiles(dir string) bool {
	sourceExts := map[string]bool{
		".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
		".rs": true, ".java": true, ".kt": true, ".swift": true, ".c": true, ".cpp": true,
		".cs": true, ".rb": true, ".php": true, ".scala": true, ".dart": true,
		".vue": true, ".svelte": true, ".zig": true, ".lua": true, ".ex": true, ".erl": true,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			// Check one level deep.
			subEntries, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			for _, se := range subEntries {
				if !se.IsDir() {
					ext := filepath.Ext(se.Name())
					if sourceExts[ext] {
						return true
					}
				}
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
