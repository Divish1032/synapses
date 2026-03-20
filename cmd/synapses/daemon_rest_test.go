package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	mcpsrv "github.com/SynapsesOS/synapses/internal/mcp"
	"github.com/SynapsesOS/synapses/internal/store"
)

// setupRESTTest creates a test project directory (with .git marker), a
// projectRegistry with a pre-registered ProjectInstance backed by a real
// MCP server and SQLite store, and an httptest.Server serving restToolsHandler.
//
// The returned cleanup function must be deferred by the caller.
func setupRESTTest(t *testing.T) (ts *httptest.Server, projectPath string) {
	t.Helper()

	// Create temp project dir with .git marker so isValidProjectPath passes.
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".git"), 0o755); err != nil {
		t.Fatalf("create .git dir: %v", err)
	}

	// Resolve to canonical form (same as the handler does).
	absPath, err := canonicalPath(projectDir)
	if err != nil {
		t.Fatalf("canonicalPath: %v", err)
	}

	// Create real store, graph, config, and MCP server.
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// Register in project registry.
	reg := newProjectRegistry()
	pi := &ProjectInstance{
		AbsPath:   absPath,
		Graph:     g,
		Store:     st,
		MCPServer: srv,
	}
	reg.Set(pi)

	// Build handler.
	handler := restToolsHandler(reg, func(path string) (*ProjectInstance, error) {
		return nil, fmt.Errorf("unexpected lazy init for %s", path)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tools/", handler)
	ts = httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return ts, absPath
}

// decodeJSONResponse reads and JSON-decodes the response body into a map.
func decodeJSONResponse(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal body: %v\nraw: %s", err, string(body))
	}
	return m
}

// ── Test: 200 valid tool call ────────────────────────────────────────────────

func TestREST_ValidToolCall_200(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	// Call get_project_identity — a lightweight tool that always works.
	resp, err := http.Post(
		ts.URL+"/v1/tools/get_project_identity?project="+projectPath,
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	m := decodeJSONResponse(t, resp)
	// get_project_identity returns a CallToolResult with content array.
	if _, ok := m["content"]; !ok {
		t.Errorf("expected 'content' key in response, got keys: %v", mapKeysREST(m))
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", resp.Header.Get("Content-Type"))
	}
}

// ── Test: empty body treated as {} ───────────────────────────────────────────

func TestREST_EmptyBody_200(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	resp, err := http.Post(
		ts.URL+"/v1/tools/get_project_identity?project="+projectPath,
		"application/json",
		nil, // no body at all
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// ── Test: 405 wrong method (GET instead of POST) ────────────────────────────

func TestREST_WrongMethod_405(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	resp, err := http.Get(ts.URL + "/v1/tools/session_init?project=" + projectPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
	m := decodeJSONResponse(t, resp)
	if errMsg, ok := m["error"].(string); !ok || !strings.Contains(errMsg, "method not allowed") {
		t.Errorf("expected 'method not allowed' error, got: %v", m)
	}
}

// ── Test: 400 missing ?project= ──────────────────────────────────────────────

func TestREST_MissingProject_400(t *testing.T) {
	ts, _ := setupRESTTest(t)

	resp, err := http.Post(
		ts.URL+"/v1/tools/session_init",
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	m := decodeJSONResponse(t, resp)
	if errMsg, ok := m["error"].(string); !ok || !strings.Contains(errMsg, "missing ?project=") {
		t.Errorf("expected 'missing ?project=' error, got: %v", m)
	}
}

// ── Test: 404 unknown tool name ──────────────────────────────────────────────

func TestREST_UnknownTool_404(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	resp, err := http.Post(
		ts.URL+"/v1/tools/definitely_nonexistent_tool_xyz?project="+projectPath,
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 404, got %d: %s", resp.StatusCode, body)
	}
	m := decodeJSONResponse(t, resp)
	if errMsg, ok := m["error"].(string); !ok || !strings.Contains(errMsg, "unknown tool") {
		t.Errorf("expected 'unknown tool' error, got: %v", m)
	}
}

// ── Test: 400 invalid JSON body ──────────────────────────────────────────────

func TestREST_InvalidJSON_400(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	resp, err := http.Post(
		ts.URL+"/v1/tools/get_project_identity?project="+projectPath,
		"application/json",
		strings.NewReader("{not valid json"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	m := decodeJSONResponse(t, resp)
	if errMsg, ok := m["error"].(string); !ok || !strings.Contains(errMsg, "invalid JSON body") {
		t.Errorf("expected 'invalid JSON body' error, got: %v", m)
	}
}

// ── Test: 400 body exceeds 1 MiB limit ───────────────────────────────────────

func TestREST_OversizedBody_400(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	// Build a valid JSON object that exceeds 1 MiB. We use a single key with
	// a large string value to ensure the JSON decoder hits the limit.
	// 1 MiB = 1048576 bytes. The JSON envelope adds ~20 bytes.
	bigValue := strings.Repeat("x", 1<<20) // exactly 1 MiB of 'x'
	body := `{"big":"` + bigValue + `"}`

	resp, err := http.Post(
		ts.URL+"/v1/tools/get_project_identity?project="+projectPath,
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	// The LimitReader truncates the stream at 1 MiB. The JSON decoder sees
	// truncated JSON → parse error → 400.
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	m := decodeJSONResponse(t, resp)
	if errMsg, ok := m["error"].(string); !ok || !strings.Contains(errMsg, "invalid JSON body") {
		t.Errorf("expected 'invalid JSON body' error from truncated body, got: %v", m)
	}
}

// ── Test: 400 invalid tool path ──────────────────────────────────────────────

func TestREST_InvalidToolPath(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	cases := []struct {
		name string
		path string
	}{
		{"empty name", "/v1/tools/"},
		{"nested slash", "/v1/tools/foo/bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(
				ts.URL+tc.path+"?project="+projectPath,
				"application/json",
				strings.NewReader("{}"),
			)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("expected 400 for path %q, got %d", tc.path, resp.StatusCode)
			}
			m := decodeJSONResponse(t, resp)
			if errMsg, ok := m["error"].(string); !ok || !strings.Contains(errMsg, "invalid tool path") {
				t.Errorf("expected 'invalid tool path' error, got: %v", m)
			}
		})
	}
}

// ── Test: 400 invalid project path (no .git or synapses.json) ────────────────

func TestREST_InvalidProjectPath_400(t *testing.T) {
	ts, _ := setupRESTTest(t)

	// Use a temp dir that does NOT have a .git or synapses.json marker.
	noMarkerDir := t.TempDir()

	resp, err := http.Post(
		ts.URL+"/v1/tools/get_project_identity?project="+noMarkerDir,
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	m := decodeJSONResponse(t, resp)
	if errMsg, ok := m["error"].(string); !ok || !strings.Contains(errMsg, "not a valid project root") {
		t.Errorf("expected 'not a valid project root' error, got: %v", m)
	}
}

// ── Test: canonicalPath resolution ───────────────────────────────────────────

func TestREST_CanonicalPathResolution(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	// Create a symlink pointing to the project dir.
	symlinkDir := t.TempDir()
	symlinkPath := filepath.Join(symlinkDir, "symlink-project")
	if err := os.Symlink(projectPath, symlinkPath); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// Calling via symlink should resolve to the same canonical project.
	resp, err := http.Post(
		ts.URL+"/v1/tools/get_project_identity?project="+symlinkPath,
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	// Should succeed because symlink resolves to the registered project.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200 via symlink, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// ── Test: session ID injection (unique per-request) ──────────────────────────

func TestREST_SessionIDInjection(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	// Record the counter before and after two requests.
	before := restSessionCounter.Load()

	for i := 0; i < 3; i++ {
		resp, err := http.Post(
			ts.URL+"/v1/tools/get_project_identity?project="+projectPath,
			"application/json",
			strings.NewReader("{}"),
		)
		if err != nil {
			t.Fatalf("POST #%d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST #%d: expected 200, got %d", i, resp.StatusCode)
		}
	}

	after := restSessionCounter.Load()
	delta := after - before
	if delta != 3 {
		t.Errorf("expected restSessionCounter to increment by 3, got delta=%d (before=%d, after=%d)", delta, before, after)
	}
}

// ── Test: Content-Type is always application/json ────────────────────────────

func TestREST_ContentTypeJSON(t *testing.T) {
	ts, projectPath := setupRESTTest(t)

	cases := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"success", http.MethodPost, "/v1/tools/get_project_identity?project=" + projectPath, 200},
		{"405", http.MethodGet, "/v1/tools/session_init?project=" + projectPath, 405},
		{"400 missing project", http.MethodPost, "/v1/tools/session_init", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			if resp.StatusCode != tc.status {
				t.Errorf("expected %d, got %d", tc.status, resp.StatusCode)
			}
			ct := resp.Header.Get("Content-Type")
			if ct != "application/json" {
				t.Errorf("expected Content-Type application/json, got %q", ct)
			}
		})
	}
}

// ── Test: URL-encoded project path ───────────────────────────────────────────

func TestREST_URLEncodedProjectPath(t *testing.T) {
	// Create a project dir with a space in its name.
	base := t.TempDir()
	spacedDir := filepath.Join(base, "my project")
	if err := os.MkdirAll(filepath.Join(spacedDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	absPath, _ := canonicalPath(spacedDir)

	// Set up registry with this project.
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	srv := mcpsrv.New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	reg := newProjectRegistry()
	reg.Set(&ProjectInstance{AbsPath: absPath, Graph: g, Store: st, MCPServer: srv})

	handler := restToolsHandler(reg, func(path string) (*ProjectInstance, error) {
		return nil, fmt.Errorf("unexpected init")
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tools/", handler)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// The space in the path must be URL-encoded in the query string.
	// http.Post will handle query encoding, but we can also test that the
	// handler's QueryUnescape works correctly.
	encodedPath := strings.ReplaceAll(absPath, " ", "%20")
	resp, err := http.Post(
		ts.URL+"/v1/tools/get_project_identity?project="+encodedPath,
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// mapKeysREST returns sorted keys for diagnostics (avoids name collision with daemon_test.go helpers).
func mapKeysREST(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ── Sprint 9 #7: REST API body size edge cases ────────────────────────────────

func TestREST_OversizedBody_2MiB_400(t *testing.T) {
	// Verify the 1 MiB body cap rejects a 2 MiB payload.
	// The handler uses io.LimitReader(r.Body, 1<<20), so a 2 MiB body
	// should be truncated, producing invalid JSON → 400 Bad Request.
	ts, projectPath := setupRESTTest(t)

	// Build a 2 MiB JSON body: {"entity": "<2MB of data>"}
	bigValue := strings.Repeat("x", 2*1024*1024)
	body := `{"entity":"` + bigValue + `"}`

	resp, err := http.Post(
		ts.URL+"/v1/tools/get_context?project="+projectPath,
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for 2 MiB body, got %d: %s", resp.StatusCode, bodyBytes)
	}
}

func TestREST_BodyExactly1MiB_Accepted(t *testing.T) {
	// A body at exactly 1 MiB should be fully read by io.LimitReader.
	// Build a valid JSON body that is exactly 1 MiB.
	ts, projectPath := setupRESTTest(t)

	// Build JSON: {"agent_id":"a"} padded to just under 1 MiB.
	// The important thing is that it's valid JSON and ≤ 1 MiB.
	padding := strings.Repeat(" ", (1<<20)-30) // pad with whitespace
	body := `{"agent_id":"a"` + padding + `}`

	resp, err := http.Post(
		ts.URL+"/v1/tools/session_init?project="+projectPath,
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// Should be 200 (the JSON is valid and under the limit).
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for 1 MiB body, got %d: %s", resp.StatusCode, bodyBytes)
	}
}

func TestREST_UnicodeInToolBody_200(t *testing.T) {
	// Verify REST API handles CJK/emoji content in request body.
	ts, projectPath := setupRESTTest(t)

	body := `{"agent_id":"test-agent","decision":"認証をOAuth 2.0に切り替えました 🔒","outcome":"success"}`

	resp, err := http.Post(
		ts.URL+"/v1/tools/remember?project="+projectPath,
		"application/json",
		strings.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for Unicode body, got %d: %s", resp.StatusCode, bodyBytes)
	}
}

func TestREST_UnicodeInProjectPath_400(t *testing.T) {
	// Unicode in project path — the path won't exist, so expect 400 from
	// isValidProjectPath (no .git marker). Important: no panic.
	ts, _ := setupRESTTest(t)

	resp, err := http.Post(
		ts.URL+"/v1/tools/session_init?project=/tmp/テストプロジェクト",
		"application/json",
		strings.NewReader("{}"),
	)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// 400 because the path doesn't exist or has no .git marker.
	if resp.StatusCode != http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for non-existent Unicode path, got %d: %s", resp.StatusCode, bodyBytes)
	}
}
