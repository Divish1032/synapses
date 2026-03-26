package agent

import (
	"encoding/json"
	"testing"
)

func TestExtractContextAccesses_Search(t *testing.T) {
	resp := map[string]interface{}{
		"results": []map[string]interface{}{
			{"file": "pkg/foo/bar.go", "line": 42, "name": "Bar", "type": "function"},
			{"file": "pkg/foo/baz.go", "line": 7, "name": "Baz", "type": "method"},
		},
	}
	b, _ := json.Marshal(resp)
	accesses := extractContextAccesses("task-1", "search", string(b))
	if len(accesses) != 2 {
		t.Fatalf("want 2 accesses, got %d", len(accesses))
	}
	if accesses[0].File != "pkg/foo/bar.go" || accesses[0].LineStart != 42 {
		t.Errorf("wrong first access: %+v", accesses[0])
	}
	if accesses[1].File != "pkg/foo/baz.go" || accesses[1].LineStart != 7 {
		t.Errorf("wrong second access: %+v", accesses[1])
	}
}

func TestExtractContextAccesses_GetImpact(t *testing.T) {
	resp := map[string]interface{}{
		"tiers": []map[string]interface{}{
			{"nodes": []map[string]interface{}{
				{"file": "internal/x.go", "line": 10},
				{"file": "internal/y.go", "line": 20},
			}},
			{"nodes": []map[string]interface{}{
				{"file": "internal/z.go", "line": 30},
			}},
		},
	}
	b, _ := json.Marshal(resp)
	accesses := extractContextAccesses("task-2", "get_impact", string(b))
	if len(accesses) != 3 {
		t.Fatalf("want 3 accesses, got %d", len(accesses))
	}
}

func TestExtractContextAccesses_PrepareContext(t *testing.T) {
	text := "## Context for Bar\n\npkg/foo/bar.go:42\nSome code here.\npkg/other/util.py:100\n"
	accesses := extractContextAccesses("task-3", "prepare_context", text)
	if len(accesses) != 2 {
		t.Fatalf("want 2 accesses, got %d: %v", len(accesses), accesses)
	}
	if accesses[0].File != "pkg/foo/bar.go" || accesses[0].LineStart != 42 {
		t.Errorf("wrong access[0]: %+v", accesses[0])
	}
}

func TestExtractContextAccesses_Fallback(t *testing.T) {
	// Empty response — should return exactly one placeholder record.
	accesses := extractContextAccesses("task-4", "search", "")
	if len(accesses) != 1 {
		t.Fatalf("want 1 fallback access, got %d", len(accesses))
	}
	if accesses[0].File != "" {
		t.Errorf("expected empty file in fallback, got %q", accesses[0].File)
	}
}

func TestExtractContextAccesses_InvalidJSON(t *testing.T) {
	// Invalid JSON for search — should fallback.
	accesses := extractContextAccesses("task-5", "search", "not json")
	if len(accesses) != 1 {
		t.Fatalf("want 1 fallback access, got %d", len(accesses))
	}
}

func TestExtractContextAccesses_EmptyResults(t *testing.T) {
	// Valid JSON but no results.
	b, _ := json.Marshal(map[string]interface{}{"results": []interface{}{}})
	accesses := extractContextAccesses("task-6", "search", string(b))
	if len(accesses) != 1 {
		t.Fatalf("want 1 fallback, got %d", len(accesses))
	}
}

func TestExtractContextAccesses_TaskID(t *testing.T) {
	b, _ := json.Marshal(map[string]interface{}{
		"results": []map[string]interface{}{
			{"file": "a.go", "line": 1},
		},
	})
	accesses := extractContextAccesses("my-task", "search", string(b))
	for _, a := range accesses {
		if a.TaskID != "my-task" {
			t.Errorf("expected TaskID=my-task, got %q", a.TaskID)
		}
		if a.Tool != "search" {
			t.Errorf("expected Tool=search, got %q", a.Tool)
		}
	}
}
