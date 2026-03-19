package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── authTokenPath ─────────────────────────────────────────────────────────────

func TestAuthTokenPath_ReturnsCorrectPath(t *testing.T) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpDir, ".synapses"), 0o700)

	path, err := authTokenPath()
	if err != nil {
		t.Fatalf("authTokenPath error: %v", err)
	}
	want := filepath.Join(tmpDir, ".synapses", "auth_token")
	if path != want {
		t.Errorf("got %q, want %q", path, want)
	}
}

// ── loadOrCreateAuthToken ─────────────────────────────────────────────────────

func TestLoadOrCreateAuthToken_CreatesTokenWhenAbsent(t *testing.T) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpDir, ".synapses"), 0o700)

	token, err := loadOrCreateAuthToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(token) != 64 {
		t.Errorf("token length = %d, want 64", len(token))
	}
	// Verify it's lowercase hex.
	for _, c := range token {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("token contains non-hex char %q", c)
		}
	}
	// File must exist with mode 0600.
	path, _ := authTokenPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("token file not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreateAuthToken_ReadsExistingToken(t *testing.T) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpDir, ".synapses"), 0o700)

	// Pre-write a valid token.
	existing := strings.Repeat("a", 64)
	path := filepath.Join(tmpDir, ".synapses", "auth_token")
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	token, err := loadOrCreateAuthToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != existing {
		t.Errorf("got %q, want %q", token, existing)
	}
}

func TestLoadOrCreateAuthToken_RegeneratesInvalidToken(t *testing.T) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpDir, ".synapses"), 0o700)

	// Write a too-short token (invalid).
	path := filepath.Join(tmpDir, ".synapses", "auth_token")
	if err := os.WriteFile(path, []byte("tooshort"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	token, err := loadOrCreateAuthToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(token) != 64 {
		t.Errorf("regenerated token length = %d, want 64", len(token))
	}
	if token == "tooshort" {
		t.Error("expected a new token to be generated")
	}
}

func TestLoadOrCreateAuthToken_IdempotentOnSecondCall(t *testing.T) {
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	os.MkdirAll(filepath.Join(tmpDir, ".synapses"), 0o700)

	t1, err := loadOrCreateAuthToken()
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	t2, err := loadOrCreateAuthToken()
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if t1 != t2 {
		t.Errorf("token changed between calls: %q vs %q", t1, t2)
	}
}

// ── authMiddleware ────────────────────────────────────────────────────────────

// okHandler replies 200 with {"ok":true}.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true}) //nolint:errcheck
})

func TestAuthMiddleware_DisabledWhenTokenEmpty(t *testing.T) {
	h := authMiddleware("", okHandler)
	// Any request should pass through when token is empty.
	req := httptest.NewRequest(http.MethodGet, "/v1/tools/foo", nil)
	req.RemoteAddr = "192.168.1.1:9999" // non-localhost
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (auth disabled)", w.Code)
	}
}

func TestAuthMiddleware_HealthAlwaysExempt(t *testing.T) {
	h := authMiddleware("abc123token"+strings.Repeat("x", 53), okHandler)
	// Non-localhost client hitting /api/admin/health — must always be allowed.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", w.Code)
	}
}

func TestAuthMiddleware_LocalhostAllowedWithoutToken(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := authMiddleware(validToken, okHandler)

	for _, addr := range []string{"127.0.0.1:1234", "127.0.0.2:9999", "[::1]:9999"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/tools/session_init", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("addr %q: status = %d, want 200 (localhost always allowed)", addr, w.Code)
		}
	}
}

func TestAuthMiddleware_OptionsAlwaysForwarded(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := authMiddleware(validToken, okHandler)

	req := httptest.NewRequest(http.MethodOptions, "/v1/tools/foo", nil)
	req.RemoteAddr = "10.0.0.1:9999" // non-localhost, no token
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("OPTIONS status = %d, want 200 (preflight forwarded)", w.Code)
	}
}

func TestAuthMiddleware_NonLocalhostMissingToken401(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := authMiddleware(validToken, okHandler)

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/session_init", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if hdr := w.Header().Get("WWW-Authenticate"); hdr == "" {
		t.Error("expected WWW-Authenticate header")
	}
}

func TestAuthMiddleware_NonLocalhostValidToken200(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := authMiddleware(validToken, okHandler)

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/session_init", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	req.Header.Set("Authorization", "Bearer "+validToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestAuthMiddleware_NonLocalhostWrongToken401(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := authMiddleware(validToken, okHandler)

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/session_init", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 64))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for wrong token", w.Code)
	}
}

func TestAuthMiddleware_NonLocalhostMalformedBearerPrefix401(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := authMiddleware(validToken, okHandler)

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/session_init", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	// "Token " prefix instead of "Bearer "
	req.Header.Set("Authorization", "Token "+validToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for wrong scheme", w.Code)
	}
}

func TestAuthMiddleware_AdminEndpointsProtectedForNonLocalhost(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := authMiddleware(validToken, okHandler)

	// /api/admin/projects is NOT the health endpoint — should require token.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/projects", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/api/admin/projects status = %d, want 401 for non-localhost without token", w.Code)
	}
}

func TestAuthMiddleware_ResponseBodyIsJSON(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := authMiddleware(validToken, okHandler)

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/foo", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("expected 'error' key in 401 response body")
	}
}
