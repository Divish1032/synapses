package mcp

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain"
)

func TestClassifyFile(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"internal/mcp/server_test.go", "test"},
		{"src/App.test.tsx", "test"},
		{"tests/unit/foo.py", "test"},
		{"__tests__/bar.js", "test"},
		{"lib/auth_spec.rb", "test"},
		{"Dockerfile", "config"},
		{"docker-compose.yml", "config"},
		{".github/workflows/ci.yml", "config"},
		{"deploy/k8s/service.yaml", "config"},
		{"Makefile", "config"},
		{"internal/mcp/server.go", "code"},
		{"src/components/Button.tsx", "code"},
		{"docs/README.md", "doc"},
		{"ARCHITECTURE.rst", "doc"},
		{"notes.txt", "doc"},
	}
	for _, tt := range tests {
		got := classifyFile(tt.path)
		if got != tt.want {
			t.Errorf("classifyFile(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestPhaseToQualityMode(t *testing.T) {
	tests := []struct {
		phase brain.SDLCPhase
		want  brain.QualityMode
	}{
		{brain.PhasePlanning, brain.QualityQuick},
		{brain.PhaseDevelopment, brain.QualityStandard},
		{brain.PhaseTesting, brain.QualityEnterprise},
		{brain.PhaseReview, brain.QualityEnterprise},
		{brain.PhaseDeployment, brain.QualityEnterprise},
	}
	for _, tt := range tests {
		got := phaseToQualityMode(tt.phase)
		if got != tt.want {
			t.Errorf("phaseToQualityMode(%q) = %q, want %q", tt.phase, got, tt.want)
		}
	}
}

func TestDetector_DevelopmentPhase(t *testing.T) {
	d := newSDLCDetector()
	// Simulate 5 development-like calls.
	for i := 0; i < 3; i++ {
		d.recordCall("get_context", []string{"AuthService"}, []string{"internal/auth/service.go"})
	}
	d.recordCall("find_entity", []string{"UserRepo"}, nil)
	phase, mode, changed := d.recordCall("link_task_nodes", []string{"AuthService"}, nil)
	if !changed {
		t.Fatal("expected phase change on 5th call")
	}
	if phase != brain.PhaseDevelopment {
		t.Errorf("expected development, got %q", phase)
	}
	if mode != brain.QualityStandard {
		t.Errorf("expected standard mode, got %q", mode)
	}
}

func TestDetector_TestingPhase(t *testing.T) {
	d := newSDLCDetector()
	d.recordCall("validate", nil, []string{"internal/auth/service_test.go"})
	d.recordCall("validate", nil, []string{"internal/auth/handler_test.go"})
	d.recordCall("verify_implementation", nil, []string{"internal/auth/service_test.go"})
	d.recordCall("get_impact", []string{"AuthService"}, nil)
	phase, mode, changed := d.recordCall("validate", nil, []string{"internal/mcp/server_test.go"})
	if !changed {
		t.Fatal("expected phase change")
	}
	if phase != brain.PhaseTesting {
		t.Errorf("expected testing, got %q", phase)
	}
	if mode != brain.QualityEnterprise {
		t.Errorf("expected enterprise mode, got %q", mode)
	}
}

func TestDetector_PlanningPhase(t *testing.T) {
	d := newSDLCDetector()
	d.recordCall("create_plan", nil, nil)
	d.recordCall("upsert_adr", nil, nil)
	d.recordCall("rules", nil, nil)
	d.recordCall("get_adrs", nil, nil)
	phase, _, changed := d.recordCall("upsert_rule", nil, nil)
	if !changed {
		t.Fatal("expected phase change")
	}
	if phase != brain.PhasePlanning {
		t.Errorf("expected planning, got %q", phase)
	}
}

func TestDetector_DeploymentPhase(t *testing.T) {
	d := newSDLCDetector()
	d.recordCall("validate", nil, []string{"Dockerfile"})
	d.recordCall("get_context", nil, []string{"docker-compose.yml"})
	d.recordCall("validate", nil, []string{".github/workflows/ci.yml"})
	d.recordCall("get_context", nil, []string{"deploy/k8s/service.yaml"})
	phase, _, changed := d.recordCall("validate", nil, []string{"infra/terraform/main.tf"})
	if !changed {
		t.Fatal("expected phase change")
	}
	if phase != brain.PhaseDeployment {
		t.Errorf("expected deployment, got %q", phase)
	}
}

func TestDetector_ExplicitOverride(t *testing.T) {
	d := newSDLCDetector()
	d.markExplicit()

	// Even with strong development signals, auto-detection should be suppressed.
	for i := 0; i < 5; i++ {
		_, _, changed := d.recordCall("get_context", []string{"X"}, []string{"main.go"})
		if changed {
			t.Fatal("expected no change after explicit override")
		}
	}
}

func TestDetector_Reset(t *testing.T) {
	d := newSDLCDetector()
	d.markExplicit()
	d.callCount = 10
	d.lastPhase = brain.PhaseTesting

	d.reset()

	if d.explicitlySet {
		t.Error("expected explicitlySet=false after reset")
	}
	if d.callCount != 0 {
		t.Error("expected callCount=0 after reset")
	}
	if d.lastPhase != "" {
		t.Error("expected empty lastPhase after reset")
	}
}

func TestDetector_Hysteresis(t *testing.T) {
	d := newSDLCDetector()

	// First: strong development signal.
	for i := 0; i < 3; i++ {
		d.recordCall("get_context", []string{"X"}, []string{"main.go"})
	}
	d.recordCall("find_entity", []string{"Y"}, nil)
	d.recordCall("link_task_nodes", []string{"Z"}, nil)

	// Now: weak planning signal — should NOT override.
	d.recordCall("search", nil, nil)
	d.recordCall("search", nil, nil)
	d.recordCall("get_context", []string{"A"}, []string{"main.go"})
	d.recordCall("search", nil, nil)
	phase, _, changed := d.recordCall("rules", nil, nil)

	// Ambiguous signal — development should stay (hysteresis).
	if changed && phase == brain.PhasePlanning {
		t.Error("expected hysteresis to prevent weak transition to planning")
	}
}

func TestDetector_WeakSignal(t *testing.T) {
	d := newSDLCDetector()

	// 5 session_init calls — no meaningful signal.
	for i := 0; i < 4; i++ {
		d.recordCall("session_init", nil, nil)
	}
	_, _, changed := d.recordCall("end_session", nil, nil)
	if changed {
		t.Error("expected no phase change from session management calls only")
	}
}
