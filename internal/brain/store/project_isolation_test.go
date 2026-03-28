package store

import (
	"path/filepath"
	"testing"
)

func openTestStoreIso(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProjectIsolation_SummariesScopedByProject(t *testing.T) {
	s := openTestStoreIso(t)

	// Same node_id, different projects, different summaries.
	if err := s.UpsertSummary("proj-A", "node1", "MyFunc", "summary for A", []string{"api"}); err != nil {
		t.Fatalf("UpsertSummary proj-A: %v", err)
	}
	if err := s.UpsertSummary("proj-B", "node1", "MyFunc", "summary for B", []string{"core"}); err != nil {
		t.Fatalf("UpsertSummary proj-B: %v", err)
	}

	gotA := s.GetSummary("proj-A", "node1")
	if gotA != "summary for A" {
		t.Errorf("proj-A GetSummary = %q, want %q", gotA, "summary for A")
	}
	gotB := s.GetSummary("proj-B", "node1")
	if gotB != "summary for B" {
		t.Errorf("proj-B GetSummary = %q, want %q", gotB, "summary for B")
	}
}

func TestProjectIsolation_GetSummariesBulk(t *testing.T) {
	s := openTestStoreIso(t)

	// 3 nodes for proj-A
	for _, n := range []struct{ id, name, sum string }{
		{"n1", "Func1", "s1"}, {"n2", "Func2", "s2"}, {"n3", "Func3", "s3"},
	} {
		if err := s.UpsertSummary("proj-A", n.id, n.name, n.sum, nil); err != nil {
			t.Fatal(err)
		}
	}
	// 2 nodes for proj-B
	for _, n := range []struct{ id, name, sum string }{
		{"n1", "Func1", "b1"}, {"n4", "Func4", "b4"},
	} {
		if err := s.UpsertSummary("proj-B", n.id, n.name, n.sum, nil); err != nil {
			t.Fatal(err)
		}
	}

	bulkA := s.GetSummaries("proj-A", []string{"n1", "n2", "n3", "n4"})
	if len(bulkA) != 3 {
		t.Errorf("proj-A bulk count = %d, want 3", len(bulkA))
	}
	if bulkA["n1"] != "s1" {
		t.Errorf("proj-A n1 = %q, want s1", bulkA["n1"])
	}

	bulkB := s.GetSummaries("proj-B", []string{"n1", "n2", "n3", "n4"})
	if len(bulkB) != 2 {
		t.Errorf("proj-B bulk count = %d, want 2", len(bulkB))
	}
	if bulkB["n1"] != "b1" {
		t.Errorf("proj-B n1 = %q, want b1", bulkB["n1"])
	}
}

func TestProjectIsolation_UpdateDoesNotCrossProjects(t *testing.T) {
	s := openTestStoreIso(t)

	_ = s.UpsertSummary("proj-A", "node1", "Fn", "original-A", nil)
	_ = s.UpsertSummary("proj-B", "node1", "Fn", "original-B", nil)

	// Update proj-A only.
	_ = s.UpsertSummary("proj-A", "node1", "Fn", "updated-A", nil)

	if got := s.GetSummary("proj-A", "node1"); got != "updated-A" {
		t.Errorf("proj-A after update = %q, want updated-A", got)
	}
	if got := s.GetSummary("proj-B", "node1"); got != "original-B" {
		t.Errorf("proj-B should be unchanged = %q, want original-B", got)
	}
}
