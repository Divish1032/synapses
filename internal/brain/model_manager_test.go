package brain

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newPulseWithState builds a *SystemPulse and immediately overwrites its
// internal state without starting the background goroutine. This lets tests
// exercise EnsureModel with arbitrary system conditions.
func newPulseWithState(ram int64, loaded string) *SystemPulse {
	p := NewSystemPulse()
	p.mu.Lock()
	p.current = SystemState{
		AvailableRAM:      ram,
		CPULoadNorm:       0.2,
		OllamaModelLoaded: loaded,
		Health:            HealthGreen,
		SampledAt:         time.Now(),
	}
	p.mu.Unlock()
	return p
}

// newMgrWithServer builds a ModelManager that sends warmup requests to srv.
func newMgrWithServer(pulse *SystemPulse, srv *httptest.Server, primary, fallback string, keepAlive int) *ModelManager {
	return &ModelManager{
		pulse:     pulse,
		baseURL:   srv.URL,
		primary:   primary,
		fallback:  fallback,
		keepAlive: keepAlive,
		http:      &http.Client{Timeout: 5 * time.Second},
	}
}

// ---------------------------------------------------------------------------
// EnsureModel — nil pulse (NullBrain / test path)
// ---------------------------------------------------------------------------

func TestEnsureModel_NilPulse_ReturnsPrimary(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	mgr := newMgrWithServer(nil, srv, "synapses/sentry", "", -1)
	got := mgr.EnsureModel(context.Background())
	if got != "synapses/sentry" {
		t.Errorf("EnsureModel = %q, want %q", got, "synapses/sentry")
	}
	if called {
		t.Error("warmUp should not be called when pulse is nil")
	}
}

// ---------------------------------------------------------------------------
// EnsureModel — Case 1: model already loaded
// ---------------------------------------------------------------------------

func TestEnsureModel_AlreadyLoaded_ReturnLoadedModel(t *testing.T) {
	warmupCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		warmupCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A model is loaded, even if it differs from the primary.
	pulse := newPulseWithState(1*1024*1024*1024, "qwen3.5:4b") // 1 GB free — not enough to load fresh
	mgr := newMgrWithServer(pulse, srv, "synapses/sentry", "", -1)

	got := mgr.EnsureModel(context.Background())
	if got != "qwen3.5:4b" {
		t.Errorf("EnsureModel = %q, want %q (loaded model)", got, "qwen3.5:4b")
	}
	if warmupCalled {
		t.Error("warmUp should not be called when model is already loaded")
	}
}

// ---------------------------------------------------------------------------
// EnsureModel — Case 2: RAM sufficient for primary
// ---------------------------------------------------------------------------

func TestEnsureModel_SufficientRAMFor2B_ReturnsPrimaryAndWarmsUp(t *testing.T) {
	var capturedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		capturedModel = body.Model
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 3 GB free — enough for 2B model (needs 2.5 GB).
	pulse := newPulseWithState(3*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "synapses/sentry", "", 120)

	got := mgr.EnsureModel(context.Background())
	if got != "synapses/sentry" {
		t.Errorf("EnsureModel = %q, want %q", got, "synapses/sentry")
	}
	if capturedModel != "synapses/sentry" {
		t.Errorf("warmUp model = %q, want %q", capturedModel, "synapses/sentry")
	}
}

func TestEnsureModel_WarmUp_SendsCorrectKeepAlive(t *testing.T) {
	var capturedKA *int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			KeepAlive *int `json:"keep_alive"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		capturedKA = body.KeepAlive
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pulse := newPulseWithState(3*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "synapses/sentry", "", 300)

	mgr.EnsureModel(context.Background())
	if capturedKA == nil {
		t.Fatal("keep_alive not sent in warmup request")
	}
	if *capturedKA != 300 {
		t.Errorf("keep_alive = %d, want 300", *capturedKA)
	}
}

// ---------------------------------------------------------------------------
// EnsureModel — Case 3: 4B → 2B downgrade
// ---------------------------------------------------------------------------

func TestEnsureModel_4BNotEnoughRAM_DowngradesTo2B(t *testing.T) {
	var warmupModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		warmupModels = append(warmupModels, body.Model)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 3 GB free — not enough for 4B (needs 4.7 GB), but enough for 2B (needs 2.5 GB).
	pulse := newPulseWithState(3*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "qwen3.5:4b", "qwen3.5:2b", 300)

	got := mgr.EnsureModel(context.Background())
	if got != "qwen3.5:2b" {
		t.Errorf("EnsureModel = %q, want %q (downgraded)", got, "qwen3.5:2b")
	}
	if len(warmupModels) != 1 || warmupModels[0] != "qwen3.5:2b" {
		t.Errorf("warmup models = %v, want [qwen3.5:2b]", warmupModels)
	}
}

func TestEnsureModel_4BSufficientRAM_UsesPreferred(t *testing.T) {
	var capturedModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		capturedModel = body.Model
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 5 GB free — enough for 4B (needs 4.7 GB).
	pulse := newPulseWithState(5*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "qwen3.5:4b", "qwen3.5:2b", -1)

	got := mgr.EnsureModel(context.Background())
	if got != "qwen3.5:4b" {
		t.Errorf("EnsureModel = %q, want qwen3.5:4b", got)
	}
	if capturedModel != "qwen3.5:4b" {
		t.Errorf("warmup model = %q, want qwen3.5:4b", capturedModel)
	}
}

// ---------------------------------------------------------------------------
// EnsureModel — Case 4: insufficient RAM
// ---------------------------------------------------------------------------

func TestEnsureModel_InsufficientRAM_ReturnsEmpty(t *testing.T) {
	warmupCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		warmupCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 1 GB free — not enough for 2B (needs 2.5 GB) or 4B.
	pulse := newPulseWithState(1*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "synapses/sentry", "", 120)

	got := mgr.EnsureModel(context.Background())
	if got != "" {
		t.Errorf("EnsureModel = %q, want \"\" (insufficient RAM)", got)
	}
	if warmupCalled {
		t.Error("warmUp should not be called when RAM is insufficient")
	}
}

func TestEnsureModel_4BAndFallbackBothInsufficientRAM_ReturnsEmpty(t *testing.T) {
	warmupCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		warmupCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 1 GB free — not enough for 4B or 2B.
	pulse := newPulseWithState(1*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "qwen3.5:4b", "qwen3.5:2b", 300)

	got := mgr.EnsureModel(context.Background())
	if got != "" {
		t.Errorf("EnsureModel = %q, want \"\" (no model fits)", got)
	}
	if warmupCalled {
		t.Error("warmUp should not be called when no model fits")
	}
}

// ---------------------------------------------------------------------------
// EnsureModel — warmup HTTP failure is non-fatal
// ---------------------------------------------------------------------------

func TestEnsureModel_WarmupFails_StillReturnsModel(t *testing.T) {
	// Server always returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	pulse := newPulseWithState(3*1024*1024*1024, "")
	mgr := newMgrWithServer(pulse, srv, "synapses/sentry", "", 120)

	// EnsureModel should still return the primary — warmup failure is advisory.
	got := mgr.EnsureModel(context.Background())
	if got != "synapses/sentry" {
		t.Errorf("EnsureModel = %q, want %q (warmup failure non-fatal)", got, "synapses/sentry")
	}
}

// ---------------------------------------------------------------------------
// is4BModel
// ---------------------------------------------------------------------------

func TestIs4BModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"qwen3.5:4b", true},
		{"qwen3.5:4B", true}, // case-insensitive
		{"qwen3.5:2b", false},
		{"qwen3.5:14b", false},  // 14B must NOT be treated as 4B (regression)
		{"llama3.1:40b", false}, // 40B must NOT be treated as 4B (regression)
		{"synapses/librarian", false},
		{"synapses/sentry", false},
		{"qwen3.5-4b-instruct", true},
		{"model_4b_q4", true},
		{"qwen3.5:4b-q4_k_m", true},
		{"", false},
	}
	for _, tc := range cases {
		got := is4BModel(tc.model)
		if got != tc.want {
			t.Errorf("is4BModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// modelRAMRequired
// ---------------------------------------------------------------------------

func TestModelRAMRequired(t *testing.T) {
	want08B := model08BRAMRequired // 1073741824 bytes (1 GB)
	want2B := model2BRAMRequired   // 2684354560 bytes (2.5 GB)
	want4B := model4BRAMRequired   // 5047730790 bytes (4.7 GB)

	if got := modelRAMRequired("qwen3.5:0.8b"); got != want08B {
		t.Errorf("modelRAMRequired(0.8b) = %d, want %d", got, want08B)
	}
	if got := modelRAMRequired("qwen3.5:2b"); got != want2B {
		t.Errorf("modelRAMRequired(2b) = %d, want %d", got, want2B)
	}
	if got := modelRAMRequired("qwen3.5:4b"); got != want4B {
		t.Errorf("modelRAMRequired(4b) = %d, want %d", got, want4B)
	}
	// Unknown model names default to the 2B bucket.
	if got := modelRAMRequired("unknown-model"); got != want2B {
		t.Errorf("modelRAMRequired(unknown) = %d, want %d", got, want2B)
	}
}

// ---------------------------------------------------------------------------
// fallback2BFor
// ---------------------------------------------------------------------------

func TestFallback2BFor(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"qwen3.5:4b", "qwen3.5:2b"},
		{"qwen3.5:4b-q4_k_m", "qwen3.5:2b-q4_k_m"},
		{"model-4b-instruct", "model-2b-instruct"},
		{"unknown", "qwen3.5:2b"}, // universal fallback
	}
	for _, tc := range cases {
		got := fallback2BFor(tc.input)
		if got != tc.want {
			t.Errorf("fallback2BFor(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// NewModelManager — wiring from BrainConfig
// ---------------------------------------------------------------------------

func TestNewModelManager_OptimalMode(t *testing.T) {
	cfg := brainconfig.DefaultConfig()
	cfg.IntelligenceMode = brainconfig.ModeOptimal
	cfg.ModelIngest = "synapses/sentry"

	mgr := NewModelManager(nil, cfg)
	if mgr.keepAlive != 120 {
		t.Errorf("keepAlive = %d, want 120 (optimal mode)", mgr.keepAlive)
	}
	// NewModelManager uses BaseModelTag() (qwen3.5:2b for optimal), not ModelIngest.
	if mgr.primary != "qwen3.5:2b" {
		t.Errorf("primary = %q, want qwen3.5:2b (BaseModelTag for optimal)", mgr.primary)
	}
}

func TestNewModelManager_StandardMode_4BPrimaryHasFallback(t *testing.T) {
	cfg := brainconfig.DefaultConfig()
	cfg.IntelligenceMode = brainconfig.ModeStandard

	mgr := NewModelManager(nil, cfg)
	if mgr.keepAlive != 300 {
		t.Errorf("keepAlive = %d, want 300 (standard mode)", mgr.keepAlive)
	}
	// BaseModelTag returns "qwen3.5:4b" for standard mode.
	if mgr.primary != "qwen3.5:4b" {
		t.Errorf("primary = %q, want qwen3.5:4b (BaseModelTag for standard)", mgr.primary)
	}
	if mgr.fallback != "qwen3.5:2b" {
		t.Errorf("fallback = %q, want qwen3.5:2b", mgr.fallback)
	}
}

func TestNewModelManager_FullMode(t *testing.T) {
	cfg := brainconfig.DefaultConfig()
	cfg.IntelligenceMode = brainconfig.ModeFull

	mgr := NewModelManager(nil, cfg)
	if mgr.keepAlive != -1 {
		t.Errorf("keepAlive = %d, want -1 (full mode = pinned)", mgr.keepAlive)
	}
	// Full mode also uses 4B base model.
	if mgr.primary != "qwen3.5:4b" {
		t.Errorf("primary = %q, want qwen3.5:4b (BaseModelTag for full)", mgr.primary)
	}
}
