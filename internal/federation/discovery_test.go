package federation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverSiblings_FindsSynapsesJSON(t *testing.T) {
	// Create temp parent with two siblings.
	parent := t.TempDir()
	current := filepath.Join(parent, "my-project")
	os.Mkdir(current, 0o755)

	// Sibling with synapses.json.
	sibling := filepath.Join(parent, "backend")
	os.Mkdir(sibling, 0o755)
	os.WriteFile(filepath.Join(sibling, "synapses.json"), []byte("{}"), 0o644)

	results := DiscoverSiblings(current)
	if len(results) != 1 {
		t.Fatalf("expected 1 sibling, got %d", len(results))
	}
	if results[0].Name != "backend" {
		t.Errorf("expected name=backend, got %s", results[0].Name)
	}
	if results[0].Hint != "has synapses.json" {
		t.Errorf("expected hint about synapses.json, got %s", results[0].Hint)
	}
}

func TestDiscoverSiblings_FindsSynapsesCache(t *testing.T) {
	parent := t.TempDir()
	current := filepath.Join(parent, "frontend")
	os.Mkdir(current, 0o755)

	sibling := filepath.Join(parent, "api")
	os.MkdirAll(filepath.Join(sibling, ".synapses"), 0o755)

	results := DiscoverSiblings(current)
	if len(results) != 1 {
		t.Fatalf("expected 1 sibling, got %d", len(results))
	}
	if results[0].Hint != "has .synapses/ cache" {
		t.Errorf("expected hint about .synapses/ cache, got %s", results[0].Hint)
	}
}

func TestDiscoverSiblings_ExcludesSelf(t *testing.T) {
	parent := t.TempDir()
	current := filepath.Join(parent, "self")
	os.Mkdir(current, 0o755)
	os.WriteFile(filepath.Join(current, "synapses.json"), []byte("{}"), 0o644)

	results := DiscoverSiblings(current)
	for _, r := range results {
		if r.Name == "self" {
			t.Error("should not include the current project")
		}
	}
}

func TestDiscoverSiblings_SkipsNonSynapsesProjects(t *testing.T) {
	parent := t.TempDir()
	current := filepath.Join(parent, "mine")
	os.Mkdir(current, 0o755)

	// A directory without synapses.json or .synapses/
	other := filepath.Join(parent, "random")
	os.Mkdir(other, 0o755)

	results := DiscoverSiblings(current)
	if len(results) != 0 {
		t.Errorf("expected 0 siblings, got %d", len(results))
	}
}

func TestDiscoverSiblings_SkipsHiddenDirs(t *testing.T) {
	parent := t.TempDir()
	current := filepath.Join(parent, "proj")
	os.Mkdir(current, 0o755)

	hidden := filepath.Join(parent, ".hidden")
	os.MkdirAll(filepath.Join(hidden, ".synapses"), 0o755)

	results := DiscoverSiblings(current)
	for _, r := range results {
		if r.Name == ".hidden" {
			t.Error("should skip hidden directories")
		}
	}
}
