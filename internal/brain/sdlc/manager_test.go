package sdlc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/brain/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "brain.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(newTestStore(t))
}

func TestGetConfig_DefaultsWhenEmpty(t *testing.T) {
	mgr := newTestManager(t)
	row := mgr.GetConfig()
	if row.Phase != PhaseDevelopment {
		t.Errorf("want default phase %q, got %q", PhaseDevelopment, row.Phase)
	}
	if row.QualityMode != ModeStandard {
		t.Errorf("want default mode %q, got %q", ModeStandard, row.QualityMode)
	}
}

func TestSetPhase_Valid(t *testing.T) {
	mgr := newTestManager(t)
	for _, phase := range []string{PhasePlanning, PhaseDevelopment, PhaseTesting, PhaseReview, PhaseDeployment} {
		if err := mgr.SetPhase(phase, "agent-1"); err != nil {
			t.Errorf("SetPhase(%q) unexpected error: %v", phase, err)
			continue
		}
		row := mgr.GetConfig()
		if row.Phase != phase {
			t.Errorf("after SetPhase(%q), GetConfig().Phase = %q", phase, row.Phase)
		}
	}
}

func TestSetPhase_Invalid(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SetPhase("invalid_phase", "agent-1"); err == nil {
		t.Error("expected error for invalid phase, got nil")
	}
}

func TestSetPhase_PreservesMode(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SetQualityMode(ModeEnterprise, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetPhase(PhaseTesting, "agent-1"); err != nil {
		t.Fatal(err)
	}
	row := mgr.GetConfig()
	if row.QualityMode != ModeEnterprise {
		t.Errorf("SetPhase should preserve quality mode, got %q", row.QualityMode)
	}
}

func TestSetQualityMode_Valid(t *testing.T) {
	mgr := newTestManager(t)
	for _, mode := range []string{ModeQuick, ModeStandard, ModeEnterprise} {
		if err := mgr.SetQualityMode(mode, "agent-1"); err != nil {
			t.Errorf("SetQualityMode(%q) unexpected error: %v", mode, err)
			continue
		}
		row := mgr.GetConfig()
		if row.QualityMode != mode {
			t.Errorf("after SetQualityMode(%q), GetConfig().QualityMode = %q", mode, row.QualityMode)
		}
	}
}

func TestSetQualityMode_Invalid(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SetQualityMode("ultra", "agent-1"); err == nil {
		t.Error("expected error for invalid mode, got nil")
	}
}

func TestSetQualityMode_PreservesPhase(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SetPhase(PhasePlanning, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetQualityMode(ModeQuick, "agent-1"); err != nil {
		t.Fatal(err)
	}
	row := mgr.GetConfig()
	if row.Phase != PhasePlanning {
		t.Errorf("SetQualityMode should preserve phase, got %q", row.Phase)
	}
}

func TestResolvePhase_Override(t *testing.T) {
	mgr := newTestManager(t)
	// stored is development (default); override to testing
	got := mgr.ResolvePhase(PhaseTesting)
	if got != PhaseTesting {
		t.Errorf("ResolvePhase with valid override want %q, got %q", PhaseTesting, got)
	}
}

func TestResolvePhase_FallbackToStored(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SetPhase(PhaseReview, ""); err != nil {
		t.Fatal(err)
	}
	got := mgr.ResolvePhase("") // empty override → use stored
	if got != PhaseReview {
		t.Errorf("ResolvePhase with empty override want %q, got %q", PhaseReview, got)
	}
}

func TestResolvePhase_InvalidOverrideFallsBack(t *testing.T) {
	mgr := newTestManager(t)
	got := mgr.ResolvePhase("garbage")
	if got != PhaseDevelopment {
		t.Errorf("ResolvePhase with invalid override should fall back to stored default %q, got %q", PhaseDevelopment, got)
	}
}

func TestResolveMode_Override(t *testing.T) {
	mgr := newTestManager(t)
	got := mgr.ResolveMode(ModeEnterprise)
	if got != ModeEnterprise {
		t.Errorf("ResolveMode with valid override want %q, got %q", ModeEnterprise, got)
	}
}

func TestResolveMode_FallbackToStored(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SetQualityMode(ModeQuick, ""); err != nil {
		t.Fatal(err)
	}
	got := mgr.ResolveMode("")
	if got != ModeQuick {
		t.Errorf("ResolveMode with empty override want %q, got %q", ModeQuick, got)
	}
}

// Ensure unused os import doesn't cause issues — os.DevNull check as smoke test.
var _ = os.DevNull
