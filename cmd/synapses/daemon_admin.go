// daemon_admin.go — Phase 0 admin API endpoints for the web console.
//
// These endpoints replace Tauri IPC commands with standard REST so the
// web console (browser or Tauri) can manage the daemon.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/SynapsesOS/synapses/internal/logutil"
)

// registerAdminEndpoints adds the Phase 0 management API to mux.
// reg is the project registry (for reindex).
func registerAdminEndpoints(mux *http.ServeMux, reg *projectRegistry, initProject func(string) (*ProjectInstance, error), shutdownFn ...func()) {
	// shutdownFn is an optional graceful shutdown callback that replaces os.Exit(0).
	doShutdown := func() { logutil.Warn("synapses: shutdown requested but no graceful handler registered\n") } // safe fallback — never hard-exit
	if len(shutdownFn) > 0 && shutdownFn[0] != nil {
		doShutdown = shutdownFn[0]
	}

	// ── GET /api/admin/version — binary version info ─────────────────────────
	mux.HandleFunc("/api/admin/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"version":  version,
			"go":       runtime.Version(),
			"os":       runtime.GOOS,
			"arch":     runtime.GOARCH,
			"projects": len(reg.All()),
		}) //nolint:errcheck
	})

	// ── GET /api/admin/services — list in-process services ───────────────────
	mux.HandleFunc("/api/admin/services", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "use GET", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The daemon runs brain, scout, and pulse in-process. Report status.
		services := []map[string]interface{}{
			{
				"name":   "daemon",
				"port":   11435,
				"status": "healthy",
			},
		}
		json.NewEncoder(w).Encode(services) //nolint:errcheck
	})

	// ── POST /api/admin/services/{name}/restart — daemon restart ─────────────
	mux.HandleFunc("/api/admin/services/", func(w http.ResponseWriter, r *http.Request) {
		// Route: /api/admin/services/{name}/restart or /stop
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/admin/services/"), "/")
		if len(parts) < 2 || r.Method != http.MethodPost {
			http.Error(w, "use POST /api/admin/services/{name}/restart or /stop", http.StatusMethodNotAllowed)
			return
		}
		action := parts[1]
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "restart":
			// Daemon self-restart: spawn a new instance and exit.
			// The service manager (launchd/systemd) or the caller restarts us.
			json.NewEncoder(w).Encode(map[string]string{"status": "restarting"}) //nolint:errcheck
			go func() {
				time.Sleep(500 * time.Millisecond)
				logutil.Info("synapses: daemon restart requested via API\n")
				doShutdown()
			}()
		case "stop":
			json.NewEncoder(w).Encode(map[string]string{"status": "stopping"}) //nolint:errcheck
			go func() {
				time.Sleep(500 * time.Millisecond)
				logutil.Info("synapses: daemon stop requested via API\n")
				doShutdown()
			}()
		default:
			http.Error(w, "unknown action: "+action, http.StatusBadRequest)
		}
	})

	// ── GET /api/admin/agents/detect — detect installed AI agents ────────────
	mux.HandleFunc("/api/admin/agents/detect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		agents := detectInstalledAgents()
		json.NewEncoder(w).Encode(agents) //nolint:errcheck
	})

	// ── POST /api/admin/agents/connect — write MCP config for agent ──────────
	mux.HandleFunc("/api/admin/agents/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Agent       string `json:"agent"`
			ProjectPath string `json:"project_path"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Agent == "" || req.ProjectPath == "" {
			http.Error(w, "agent and project_path required", http.StatusBadRequest)
			return
		}
		absPath, err := canonicalPath(req.ProjectPath)
		if err != nil {
			http.Error(w, "invalid path: "+err.Error(), http.StatusBadRequest)
			return
		}
		results := connectAgents(absPath, []string{req.Agent})
		w.Header().Set("Content-Type", "application/json")
		if len(results) > 0 && results[0].Err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": results[0].Err.Error()}) //nolint:errcheck
			return
		}
		resp := map[string]interface{}{"status": "ok"}
		if len(results) > 0 {
			resp["files"] = results[0].Files
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})

	// ── GET /api/admin/agents/check — check MCP config for agent ─────────────
	mux.HandleFunc("/api/admin/agents/check", func(w http.ResponseWriter, r *http.Request) {
		editor := r.URL.Query().Get("editor")
		projectPath := r.URL.Query().Get("project_path")
		if editor == "" || projectPath == "" {
			http.Error(w, "editor and project_path query params required", http.StatusBadRequest)
			return
		}
		configured := checkMCPConfigured(editor, projectPath)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"configured": configured}) //nolint:errcheck
	})

	// ── GET /api/admin/config — read synapses.json + brain.json ──────────────
	mux.HandleFunc("/api/admin/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			home, _ := synapsesHome()
			result := map[string]interface{}{}
			// Read brain.json
			if data, err := os.ReadFile(filepath.Join(home, "brain.json")); err == nil {
				var brain interface{}
				if json.Unmarshal(data, &brain) == nil {
					result["brain"] = brain
				}
			}
			// Read app_settings.json
			if data, err := os.ReadFile(filepath.Join(home, "app_settings.json")); err == nil {
				var settings interface{}
				if json.Unmarshal(data, &settings) == nil {
					result["app_settings"] = settings
				}
			}
			json.NewEncoder(w).Encode(result) //nolint:errcheck

		case http.MethodPut:
			var body map[string]json.RawMessage
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
				http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}
			home, err := synapsesHome()
			if err != nil {
				http.Error(w, "cannot find data dir: "+err.Error(), http.StatusInternalServerError)
				return
			}
			// Write brain.json
			if raw, ok := body["brain"]; ok {
				var check interface{}
				if json.Unmarshal(raw, &check) != nil {
					http.Error(w, "brain: invalid JSON", http.StatusBadRequest)
					return
				}
				if err := os.WriteFile(filepath.Join(home, "brain.json"), raw, 0o644); err != nil {
					http.Error(w, "write brain.json: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			// Write app_settings.json
			if raw, ok := body["app_settings"]; ok {
				if err := os.WriteFile(filepath.Join(home, "app_settings.json"), raw, 0o644); err != nil {
					http.Error(w, "write app_settings.json: "+err.Error(), http.StatusInternalServerError)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck

		default:
			http.Error(w, "use GET or PUT", http.StatusMethodNotAllowed)
		}
	})

	// ── GET /api/admin/logs — tail daemon log ────────────────────────────────
	mux.HandleFunc("/api/admin/logs", func(w http.ResponseWriter, r *http.Request) {
		n := 100
		if v := r.URL.Query().Get("n"); v != "" {
			fmt.Sscanf(v, "%d", &n) //nolint:errcheck
		}
		if n <= 0 || n > 10000 {
			n = 100
		}
		logPath, err := singletonLogPath()
		if err != nil {
			http.Error(w, "log path: "+err.Error(), http.StatusInternalServerError)
			return
		}
		lines := tailFile(logPath, n)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"lines": lines}) //nolint:errcheck
	})

	// ── GET /api/admin/ollama — Ollama status + models ───────────────────────
	mux.HandleFunc("/api/admin/ollama", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ollamaURL := "http://localhost:11434"
		// Try to read from brain.json
		if home, err := synapsesHome(); err == nil {
			if data, err := os.ReadFile(filepath.Join(home, "brain.json")); err == nil {
				var cfg map[string]interface{}
				if json.Unmarshal(data, &cfg) == nil {
					if u, ok := cfg["ollama_url"].(string); ok && u != "" {
						ollamaURL = u
					}
				}
			}
		}
		client := &http.Client{Timeout: 3 * time.Second}
		result := map[string]interface{}{"running": false}

		// Check version
		if resp, err := client.Get(ollamaURL + "/api/version"); err == nil {
			defer resp.Body.Close()
			var ver map[string]interface{}
			if json.NewDecoder(resp.Body).Decode(&ver) == nil {
				result["running"] = true
				result["version"] = ver["version"]
			}
		}

		// List models
		if result["running"] == true {
			if resp, err := client.Get(ollamaURL + "/api/tags"); err == nil {
				defer resp.Body.Close()
				var tags map[string]interface{}
				if json.NewDecoder(resp.Body).Decode(&tags) == nil {
					if models, ok := tags["models"]; ok {
						result["models"] = models
					}
				}
			}
		}

		json.NewEncoder(w).Encode(result) //nolint:errcheck
	})

	// ── POST /api/admin/ollama/pull — pull model with SSE progress ───────────
	mux.HandleFunc("/api/admin/ollama/pull", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Model == "" {
			http.Error(w, "model field required", http.StatusBadRequest)
			return
		}

		ollamaURL := "http://localhost:11434"
		if home, err := synapsesHome(); err == nil {
			if data, err := os.ReadFile(filepath.Join(home, "brain.json")); err == nil {
				var cfg map[string]interface{}
				if json.Unmarshal(data, &cfg) == nil {
					if u, ok := cfg["ollama_url"].(string); ok && u != "" {
						ollamaURL = u
					}
				}
			}
		}

		// Stream pull progress as SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		bodyJSON, _ := json.Marshal(map[string]interface{}{"model": req.Model, "stream": true})
		ollamaClient := &http.Client{Timeout: 30 * time.Minute}
		resp, err := ollamaClient.Post(ollamaURL+"/api/pull", "application/json", strings.NewReader(string(bodyJSON)))
		if err != nil {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{"error": err.Error()}))
			flusher.Flush()
			return
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]string{"status": "done"}))
		flusher.Flush()
	})

	// ── POST /api/admin/projects/reindex — trigger reindex ───────────────────
	mux.HandleFunc("/api/admin/projects/reindex", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Path == "" {
			http.Error(w, "path field required", http.StatusBadRequest)
			return
		}
		absPath, err := canonicalPath(req.Path)
		if err != nil {
			http.Error(w, "invalid path: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Full reindex: tear down the existing project instance (closes store,
		// watcher, MCP server) and reinitialise from scratch. This re-parses
		// all source files, rebuilds the graph, and restarts the watcher.
		reg.Delete(absPath)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "reindexing", "path": absPath}) //nolint:errcheck

		// Reinitialise in background so the HTTP response returns immediately.
		go func() {
			if _, err := reg.GetOrSet(absPath, func() (*ProjectInstance, error) {
				return initProject(absPath)
			}); err != nil {
				logutil.Warn("reindex %s failed: %v\n", absPath, err)
			} else {
				logutil.Info("reindex %s complete\n", absPath)
			}
		}()
	})

	// ── GET/POST /api/admin/update-check — update status ────────────────────
	mux.HandleFunc("/api/admin/update-check", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			// Return cached update state.
			state := getUpdateState()
			if state == nil {
				state = &UpdateState{
					CurrentVersion:  version,
					UpdateAvailable: false,
					CheckedAt:       "",
				}
			}
			// Override with live pending version (may be fresher than file).
			if v := getPendingUpdateVersion(); v != "" {
				state.UpdateAvailable = true
				state.LatestVersion = v
			}
			json.NewEncoder(w).Encode(state) //nolint:errcheck

		case http.MethodPost:
			// Force a fresh check.
			state := checkForUpdate()
			if state == nil {
				state = &UpdateState{
					CurrentVersion:  version,
					UpdateAvailable: false,
					Error:           "check skipped (dev build)",
				}
			}
			json.NewEncoder(w).Encode(state) //nolint:errcheck

		default:
			http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
		}
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// checkMCPConfigured checks if the given editor has MCP config for synapses.
func checkMCPConfigured(editor, projectPath string) bool {
	var configPath string
	switch editor {
	case "claude":
		configPath = filepath.Join(projectPath, ".mcp.json")
	case "cursor":
		configPath = filepath.Join(projectPath, ".cursor", "mcp.json")
	case "windsurf":
		configPath = filepath.Join(projectPath, ".windsurf", "mcp_config.json")
	case "antigravity":
		configPath = filepath.Join(projectPath, ".agent", "mcp.json")
	case "zed":
		configPath = filepath.Join(projectPath, ".zed", "settings.json")
	default:
		return false
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	// Check if "synapses" appears in the config
	return strings.Contains(string(data), "synapses")
}

// tailFile reads the last n lines from a file using seek-from-end to avoid
// loading the entire file into memory for large log files.
func tailFile(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()

	// For small files (< 1 MB), just read everything.
	info, err := f.Stat()
	if err != nil {
		return []string{}
	}
	const seekThreshold = 1 * 1024 * 1024 // 1 MB

	if info.Size() > seekThreshold {
		// Seek from end: read the last chunk and extract lines.
		// Start with 64KB * n/100 estimate, grow if needed.
		chunkSize := int64(n) * 1024
		if chunkSize < 64*1024 {
			chunkSize = 64 * 1024
		}
		if chunkSize > info.Size() {
			chunkSize = info.Size()
		}
		offset := info.Size() - chunkSize
		if offset < 0 {
			offset = 0
		}
		if _, err := f.Seek(offset, io.SeekStart); err == nil {
			var lines []string
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for scanner.Scan() {
				lines = append(lines, scanner.Text())
			}
			// If we started mid-file, the first line may be partial — skip it.
			if offset > 0 && len(lines) > 0 {
				lines = lines[1:]
			}
			if len(lines) > n {
				lines = lines[len(lines)-n:]
			}
			return lines
		}
	}

	// Fallback for small files: read all lines.
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// mustJSON marshals v to JSON, or returns "{}" on error.
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
