package resolver

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestParseTSConfigJSON_BasicPaths(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"baseUrl": ".",
			"paths": {
				"@/*": ["src/*"],
				"~/*": ["lib/*"]
			}
		}
	}`)

	cfg := parseTSConfigJSON(data, "/project")
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if len(cfg.matchers) != 2 {
		t.Fatalf("expected 2 matchers, got %d", len(cfg.matchers))
	}
}

func TestResolvePathAlias_BasicMatch(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"baseUrl": ".",
			"paths": { "@/*": ["src/*"] }
		}
	}`)
	cfg := parseTSConfigJSON(data, "/project")

	resolved, matched := cfg.resolvePathAlias("@/components/Button")
	if !matched {
		t.Fatal("expected match for @/components/Button")
	}
	if resolved != "src/components/Button" {
		t.Errorf("expected 'src/components/Button', got %q", resolved)
	}
}

func TestResolvePathAlias_NoMatch(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"baseUrl": ".",
			"paths": { "@/*": ["src/*"] }
		}
	}`)
	cfg := parseTSConfigJSON(data, "/project")

	resolved, matched := cfg.resolvePathAlias("react")
	if matched {
		t.Error("expected no match for 'react'")
	}
	if resolved != "react" {
		t.Errorf("expected unchanged 'react', got %q", resolved)
	}
}

func TestResolvePathAlias_WithBaseUrl(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"baseUrl": "src",
			"paths": { "@/*": ["./*"] }
		}
	}`)
	cfg := parseTSConfigJSON(data, "/project")

	resolved, matched := cfg.resolvePathAlias("@/utils/format")
	if !matched {
		t.Fatal("expected match")
	}
	if resolved != "src/utils/format" {
		t.Errorf("expected 'src/utils/format', got %q", resolved)
	}
}

func TestResolvePathAlias_TildeAlias(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"paths": { "~/*": ["lib/*"] }
		}
	}`)
	cfg := parseTSConfigJSON(data, "/project")

	resolved, matched := cfg.resolvePathAlias("~/helpers/date")
	if !matched {
		t.Fatal("expected match for ~/helpers/date")
	}
	if resolved != "lib/helpers/date" {
		t.Errorf("expected 'lib/helpers/date', got %q", resolved)
	}
}

func TestResolvePathAlias_NilConfig(t *testing.T) {
	var cfg *tsConfigPaths
	resolved, matched := cfg.resolvePathAlias("@/foo")
	if matched {
		t.Error("expected no match on nil config")
	}
	if resolved != "@/foo" {
		t.Errorf("expected unchanged path, got %q", resolved)
	}
}

func TestParseTSConfigJSON_NoPaths(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"target": "ES2020",
			"module": "commonjs"
		}
	}`)
	cfg := parseTSConfigJSON(data, "/project")
	if cfg != nil {
		t.Error("expected nil config when no paths")
	}
}

func TestParseTSConfigJSON_InvalidJSON(t *testing.T) {
	data := []byte(`not json`)
	cfg := parseTSConfigJSON(data, "/project")
	if cfg != nil {
		t.Error("expected nil config for invalid JSON")
	}
}

func TestResolvePathAlias_ExactMatch(t *testing.T) {
	// Exact match without wildcard: "utils" → "src/shared/utils"
	data := []byte(`{
		"compilerOptions": {
			"paths": { "utils": ["src/shared/utils"] }
		}
	}`)
	cfg := parseTSConfigJSON(data, "/project")
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	resolved, matched := cfg.resolvePathAlias("utils")
	if !matched {
		t.Fatal("expected match for exact alias")
	}
	if resolved != "src/shared/utils" {
		t.Errorf("expected 'src/shared/utils', got %q", resolved)
	}
}

func TestResolvePathAlias_MultipleTargets(t *testing.T) {
	// Multiple targets: first one should be used
	data := []byte(`{
		"compilerOptions": {
			"paths": { "@/*": ["src/*", "lib/*"] }
		}
	}`)
	cfg := parseTSConfigJSON(data, "/project")
	resolved, matched := cfg.resolvePathAlias("@/foo")
	if !matched || resolved != "src/foo" {
		t.Errorf("expected 'src/foo', got %q (matched=%v)", resolved, matched)
	}
}

func TestResolvePathAlias_EmptyTargets(t *testing.T) {
	data := []byte(`{
		"compilerOptions": {
			"paths": { "@/*": [] }
		}
	}`)
	cfg := parseTSConfigJSON(data, "/project")
	// Empty targets should be skipped
	if cfg != nil {
		_, matched := cfg.resolvePathAlias("@/foo")
		if matched {
			t.Error("expected no match for empty targets")
		}
	}
}

func TestResolvePathAliases_GraphIntegration(t *testing.T) {
	// Test the full ResolvePathAliases flow with a graph.
	// This won't find a tsconfig.json (project root is fake), so should return 0.
	g := graph.New("/nonexistent/project")
	count := ResolvePathAliases(g)
	if count != 0 {
		t.Errorf("expected 0 for nonexistent project, got %d", count)
	}
}

func TestResolvePathAliases_EmptyRepoID(t *testing.T) {
	g := graph.New("")
	count := ResolvePathAliases(g)
	if count != 0 {
		t.Errorf("expected 0 for empty repo ID, got %d", count)
	}
}
