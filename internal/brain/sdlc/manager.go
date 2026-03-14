// Package sdlc provides SDLC phase awareness and quality mode profiles.
// This file contains the Manager — the stateful component that persists
// phase and quality mode to brain.sqlite via the store.
package sdlc

import (
	"fmt"

	"github.com/SynapsesOS/synapses/internal/brain/store"
)

// Manager reads and writes the project SDLC state in brain.sqlite.
// There is exactly one row in sdlc_config per project database (id='default').
// All values are plain strings; pkg/brain converts them to typed SDLCPhase/QualityMode.
type Manager struct {
	store *store.Store
}

// NewManager creates a Manager backed by the given store.
func NewManager(s *store.Store) *Manager {
	return &Manager{store: s}
}

// GetConfig returns the current project SDLC config as a store row.
// Falls back to {phase: development, mode: standard} if no row exists.
func (m *Manager) GetConfig() store.SDLCConfigRow {
	row := m.store.GetSDLCConfig()
	if !isValidPhase(row.Phase) {
		row.Phase = PhaseDevelopment
	}
	if !isValidMode(row.QualityMode) {
		row.QualityMode = ModeStandard
	}
	return row
}

// SetPhase updates the current project SDLC phase.
// Returns an error if phase is not one of the five valid phases.
func (m *Manager) SetPhase(phase, agentID string) error {
	if !isValidPhase(phase) {
		return fmt.Errorf("unknown SDLC phase %q: must be one of planning, development, testing, review, deployment", phase)
	}
	row := m.store.GetSDLCConfig()
	mode := row.QualityMode
	if !isValidMode(mode) {
		mode = ModeStandard
	}
	return m.store.UpsertSDLCConfig(phase, mode, agentID)
}

// SetQualityMode updates the current project quality mode.
// Returns an error if mode is not one of quick / standard / enterprise.
func (m *Manager) SetQualityMode(mode, agentID string) error {
	if !isValidMode(mode) {
		return fmt.Errorf("unknown quality mode %q: must be one of quick, standard, enterprise", mode)
	}
	row := m.store.GetSDLCConfig()
	phase := row.Phase
	if !isValidPhase(phase) {
		phase = PhaseDevelopment
	}
	return m.store.UpsertSDLCConfig(phase, mode, agentID)
}

// ResolvePhase returns the effective phase: uses override if non-empty and valid,
// otherwise falls back to the stored project phase.
func (m *Manager) ResolvePhase(override string) string {
	if override != "" && isValidPhase(override) {
		return override
	}
	row := m.store.GetSDLCConfig()
	if isValidPhase(row.Phase) {
		return row.Phase
	}
	return PhaseDevelopment
}

// ResolveMode returns the effective quality mode: uses override if non-empty and valid,
// otherwise falls back to the stored project mode.
func (m *Manager) ResolveMode(override string) string {
	if override != "" && isValidMode(override) {
		return override
	}
	row := m.store.GetSDLCConfig()
	if isValidMode(row.QualityMode) {
		return row.QualityMode
	}
	return ModeStandard
}

// --- helpers ---

func isValidPhase(p string) bool {
	switch p {
	case PhasePlanning, PhaseDevelopment, PhaseTesting, PhaseReview, PhaseDeployment:
		return true
	}
	return false
}

func isValidMode(m string) bool {
	switch m {
	case ModeQuick, ModeStandard, ModeEnterprise:
		return true
	}
	return false
}
