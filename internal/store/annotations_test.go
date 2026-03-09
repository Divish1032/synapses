package store_test

import (
	"testing"
)

func TestAddAnnotation_SourceIsAgent(t *testing.T) {
	st := openTestStore(t)

	nodeID := "test::file.go::MyFunc"
	_, err := st.AddAnnotation(nodeID, "agent-1", "a note from an agent")
	if err != nil {
		t.Fatalf("AddAnnotation: %v", err)
	}

	m, err := st.GetAnnotationsForNodes([]string{nodeID})
	if err != nil {
		t.Fatalf("GetAnnotationsForNodes: %v", err)
	}
	anns := m[nodeID]
	if len(anns) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(anns))
	}
	if anns[0].Source != "agent" {
		t.Errorf("expected source='agent', got %q", anns[0].Source)
	}
	if anns[0].AgentID != "agent-1" {
		t.Errorf("expected agent_id='agent-1', got %q", anns[0].AgentID)
	}
	if anns[0].Note != "a note from an agent" {
		t.Errorf("unexpected note: %q", anns[0].Note)
	}
}

func TestAddSystemAnnotation_SourceIsSystem(t *testing.T) {
	st := openTestStore(t)

	nodeID := "test::file.go::MyFunc"
	_, err := st.AddSystemAnnotation(nodeID, "[Auditor] Task done: Refactor auth")
	if err != nil {
		t.Fatalf("AddSystemAnnotation: %v", err)
	}

	m, err := st.GetAnnotationsForNodes([]string{nodeID})
	if err != nil {
		t.Fatalf("GetAnnotationsForNodes: %v", err)
	}
	anns := m[nodeID]
	if len(anns) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(anns))
	}
	if anns[0].Source != "system" {
		t.Errorf("expected source='system', got %q", anns[0].Source)
	}
	if anns[0].AgentID != "" {
		t.Errorf("expected empty agent_id, got %q", anns[0].AgentID)
	}
}

func TestGetAnnotationsForNodes_MixedSources(t *testing.T) {
	st := openTestStore(t)

	nodeID := "test::file.go::Hub"

	if _, err := st.AddAnnotation(nodeID, "claude-1", "agent note"); err != nil {
		t.Fatalf("AddAnnotation: %v", err)
	}
	if _, err := st.AddSystemAnnotation(nodeID, "[Auditor] Task done: Add login flow"); err != nil {
		t.Fatalf("AddSystemAnnotation: %v", err)
	}

	m, err := st.GetAnnotationsForNodes([]string{nodeID})
	if err != nil {
		t.Fatalf("GetAnnotationsForNodes: %v", err)
	}
	anns := m[nodeID]
	if len(anns) != 2 {
		t.Fatalf("expected 2 annotations, got %d", len(anns))
	}

	// Verify both sources are returned correctly.
	sources := map[string]bool{}
	for _, a := range anns {
		sources[a.Source] = true
	}
	if !sources["agent"] {
		t.Error("expected an annotation with source='agent'")
	}
	if !sources["system"] {
		t.Error("expected an annotation with source='system'")
	}
}

func TestGetAnnotationsForNodes_EmptyInput(t *testing.T) {
	st := openTestStore(t)
	m, err := st.GetAnnotationsForNodes(nil)
	if err != nil {
		t.Fatalf("GetAnnotationsForNodes(nil): %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}
