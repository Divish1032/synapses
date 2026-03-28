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

func TestLoadOrCreateAuthToken_CreatesDirIfMissing(t *testing.T) {
	// synapsesHome() returns the path without creating it.
	// loadOrCreateAuthToken must create ~/.synapses itself via MkdirAll.
	origHome := os.Getenv("HOME")
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	// Deliberately do NOT create ~/.synapses — simulate a fresh install.
	synapsesDir := filepath.Join(tmpDir, ".synapses")
	if _, err := os.Stat(synapsesDir); err == nil {
		t.Fatal("test setup error: .synapses should not exist yet")
	}

	token, err := loadOrCreateAuthToken()
	if err != nil {
		t.Fatalf("unexpected error on fresh install: %v", err)
	}
	if len(token) != 64 {
		t.Errorf("token length = %d, want 64", len(token))
	}
	// Directory must have been created.
	if _, err := os.Stat(synapsesDir); os.IsNotExist(err) {
		t.Error("~/.synapses was not created by loadOrCreateAuthToken")
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

// buildFinalHandler reproduces the exact handler composition used in cmdDaemonServe.
// Returns a handler where CORS is outermost → authMiddleware → mux.
func buildFinalHandler(token string) http.Handler {
	inner := http.NewServeMux()
	inner.Handle("/v1/tools/", okHandler)
	authProtected := authMiddleware(token, inner)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			if isCORSAllowedOrigin(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		authProtected.ServeHTTP(w, r)
	})
}

// TestFinalHandler_CORSHeadersPresentOn401 verifies that allowed-origin browser
// requests get CORS headers on 401 responses, so the browser can surface the
// auth error rather than an opaque CORS error.
func TestFinalHandler_CORSHeadersPresentOn401(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	finalHandler := buildFinalHandler(validToken)

	tests := []struct {
		name       string
		path       string
		remoteAddr string
		origin     string
		authHeader string
		wantStatus int
		wantCORS   bool // whether ACAO header should be present
	}{
		{
			name:       "allowed origin no token gets 401 with CORS headers",
			path:       "/v1/tools/session_init",
			remoteAddr: "10.0.0.1:9999",
			origin:     "tauri://localhost",
			wantStatus: http.StatusUnauthorized,
			wantCORS:   true,
		},
		{
			name:       "allowed origin wrong token gets 401 with CORS headers",
			path:       "/v1/tools/session_init",
			remoteAddr: "10.0.0.1:9999",
			origin:     "http://localhost:3000",
			authHeader: "Bearer " + strings.Repeat("b", 64),
			wantStatus: http.StatusUnauthorized,
			wantCORS:   true,
		},
		{
			name:       "localhost no token gets 200 (no origin header needed)",
			path:       "/v1/tools/session_init",
			remoteAddr: "127.0.0.1:9999",
			wantStatus: http.StatusOK,
			wantCORS:   false, // no Origin header → no CORS headers needed
		},
		{
			name:       "OPTIONS preflight from allowed origin gets 204 with CORS headers",
			path:       "/v1/tools/session_init",
			remoteAddr: "10.0.0.1:9999",
			origin:     "tauri://localhost",
			wantStatus: http.StatusNoContent,
			wantCORS:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodPost
			if tc.wantStatus == http.StatusNoContent {
				method = http.MethodOptions
			}
			req := httptest.NewRequest(method, tc.path, nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			w := httptest.NewRecorder()
			finalHandler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			acao := w.Header().Get("Access-Control-Allow-Origin")
			if tc.wantCORS {
				if acao == "" {
					t.Errorf("Access-Control-Allow-Origin missing, want %q", tc.origin)
				}
				if acao == "*" {
					t.Errorf("Access-Control-Allow-Origin = *, want specific origin (wildcard is the attack vector)")
				}
				if acah := w.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(acah, "Authorization") {
					t.Errorf("Access-Control-Allow-Headers = %q, must include Authorization", acah)
				}
			} else {
				if acao != "" {
					t.Errorf("Access-Control-Allow-Origin = %q, want empty (no Origin in request)", acao)
				}
			}
		})
	}
}

// ── isCORSAllowedOrigin ───────────────────────────────────────────────────────

// TestIsCORSAllowedOrigin_AllowedOrigins verifies that legitimate browser
// origins (Tauri, localhost) are accepted.
func TestIsCORSAllowedOrigin_AllowedOrigins(t *testing.T) {
	allowed := []string{
		"tauri://localhost",
		"http://localhost",
		"https://localhost",
		"http://localhost:3000",
		"https://localhost:8080",
		"http://127.0.0.1",
		"https://127.0.0.1",
		"http://127.0.0.1:11435",
		"https://127.0.0.1:9000",
	}
	for _, origin := range allowed {
		if !isCORSAllowedOrigin(origin) {
			t.Errorf("isCORSAllowedOrigin(%q) = false, want true", origin)
		}
	}
}

// TestIsCORSAllowedOrigin_BlockedOrigins verifies that malicious or
// non-local origins are rejected — this is the security guarantee.
func TestIsCORSAllowedOrigin_BlockedOrigins(t *testing.T) {
	blocked := []string{
		"https://evil.com",
		"http://attacker.local",
		"http://notlocalhost",
		"http://localhost.evil.com",
		"tauri://remote",
		"http://192.168.1.1",
		"null",
		"",
		"file://",
	}
	for _, origin := range blocked {
		if isCORSAllowedOrigin(origin) {
			t.Errorf("isCORSAllowedOrigin(%q) = true, want false (this origin should be blocked)", origin)
		}
	}
}

// TestCORSAttackVector_BrowserCannotCallAPIFromArbitraryPage is the security
// proof: a page at https://evil.com sends a fetch() to the local API.
// The attack relies on the browser sending Origin: https://evil.com and the
// server responding with Access-Control-Allow-Origin: * (old behavior).
// After this fix, the server must NOT set ACAO for disallowed origins.
func TestCORSAttackVector_BrowserCannotCallAPIFromArbitraryPage(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	finalHandler := buildFinalHandler(validToken)

	// Attacker's page sends a preflight.
	preflightReq := httptest.NewRequest(http.MethodOptions, "/v1/tools/session_init", nil)
	preflightReq.RemoteAddr = "127.0.0.1:9999" // browser is on the user's machine (loopback)
	preflightReq.Header.Set("Origin", "https://evil.com")
	preflightReq.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	finalHandler.ServeHTTP(w, preflightReq)

	// Server returns 204, but MUST NOT include Access-Control-Allow-Origin.
	// Without ACAO, the browser blocks the follow-up POST.
	if acao := w.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Errorf("SECURITY: Access-Control-Allow-Origin = %q for disallowed origin; attack vector is open", acao)
	}

	// Also verify the actual POST is blocked (no ACAO → browser rejects it,
	// but server-side we also verify no CORS header is set).
	actualReq := httptest.NewRequest(http.MethodPost, "/v1/tools/session_init", nil)
	actualReq.RemoteAddr = "127.0.0.1:9999"
	actualReq.Header.Set("Origin", "https://evil.com")
	w2 := httptest.NewRecorder()
	finalHandler.ServeHTTP(w2, actualReq)

	if acao := w2.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Errorf("SECURITY: Access-Control-Allow-Origin = %q on actual request from disallowed origin", acao)
	}
}

// TestCORSWildcardNeverSet proves that the wildcard is never returned,
// regardless of origin. This is the invariant that closes F3.
func TestCORSWildcardNeverSet(t *testing.T) {
	const validToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	finalHandler := buildFinalHandler(validToken)

	origins := []string{
		"https://evil.com",
		"http://attacker.local",
		"tauri://localhost",     // allowed, but should reflect, not *
		"http://localhost:3000", // allowed, but should reflect, not *
	}
	for _, origin := range origins {
		req := httptest.NewRequest(http.MethodPost, "/v1/tools/session_init", nil)
		req.RemoteAddr = "127.0.0.1:9999"
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		finalHandler.ServeHTTP(w, req)

		if acao := w.Header().Get("Access-Control-Allow-Origin"); acao == "*" {
			t.Errorf("origin %q: Access-Control-Allow-Origin = *, wildcard must never be returned", origin)
		}
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
