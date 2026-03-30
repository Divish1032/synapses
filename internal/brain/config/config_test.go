package config_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain/config"
)

func TestAutoConfigureModels(t *testing.T) {
	tests := []struct {
		name         string
		ramGB        float64
		wantIngest   string
		wantGuardian string
		wantEnrich   string
		wantOrch     string
	}{
		{
			name:         "RAM 0 (sentinel) -> ingest 0.8b, rest 2b",
			ramGB:        0,
			wantIngest:   "qwen3.5:0.8b",
			wantGuardian: "qwen3.5:2b",
			wantEnrich:   "qwen3.5:2b",
			wantOrch:     "qwen3.5:2b",
		},
		{
			name:         "RAM 4GB (<=16GB) -> ingest 0.8b, rest 2b",
			ramGB:        4,
			wantIngest:   "qwen3.5:0.8b",
			wantGuardian: "qwen3.5:2b",
			wantEnrich:   "qwen3.5:2b",
			wantOrch:     "qwen3.5:2b",
		},
		{
			name:         "RAM 12GB (<=16GB) -> ingest 0.8b, rest 2b",
			ramGB:        12,
			wantIngest:   "qwen3.5:0.8b",
			wantGuardian: "qwen3.5:2b",
			wantEnrich:   "qwen3.5:2b",
			wantOrch:     "qwen3.5:2b",
		},
		{
			name:         "RAM 20GB (16-24GB) -> ingest 0.8b, guardian 2b, enrich 4b, orch 2b",
			ramGB:        20,
			wantIngest:   "qwen3.5:0.8b",
			wantGuardian: "qwen3.5:2b",
			wantEnrich:   "qwen3.5:4b",
			wantOrch:     "qwen3.5:2b",
		},
		{
			name:         "RAM 48GB (>24GB) -> ingest 0.8b, guardian 2b, enrich/orch 4b",
			ramGB:        48,
			wantIngest:   "qwen3.5:0.8b",
			wantGuardian: "qwen3.5:2b",
			wantEnrich:   "qwen3.5:4b",
			wantOrch:     "qwen3.5:4b",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.AutoConfigureModels(tc.ramGB)

			if cfg.ModelIngest != tc.wantIngest {
				t.Errorf("ModelIngest = %q, want %q", cfg.ModelIngest, tc.wantIngest)
			}
			if cfg.ModelGuardian != tc.wantGuardian {
				t.Errorf("ModelGuardian = %q, want %q", cfg.ModelGuardian, tc.wantGuardian)
			}
			if cfg.ModelEnrich != tc.wantEnrich {
				t.Errorf("ModelEnrich = %q, want %q", cfg.ModelEnrich, tc.wantEnrich)
			}
			if cfg.ModelOrchestrate != tc.wantOrch {
				t.Errorf("ModelOrchestrate = %q, want %q", cfg.ModelOrchestrate, tc.wantOrch)
			}
		})
	}
}

func TestDefaultConfig_OllamaBackend(t *testing.T) {
	cfg := config.DefaultConfig()

	if cfg.Backend != "ollama" {
		t.Errorf("Backend = %q, want %q", cfg.Backend, "ollama")
	}
	if cfg.Model != "qwen3.5:2b" {
		t.Errorf("Model = %q, want %q", cfg.Model, "qwen3.5:2b")
	}
}
