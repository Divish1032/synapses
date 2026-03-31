package mcp

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestProblemScorer_Score(t *testing.T) {
	scorer := NewProblemScorer("authentication token refresh on expiry handling")

	// High relevance: name and doc match.
	authNode := &graph.Node{
		Name: "RefreshToken",
		File: "internal/auth/token.go",
		Metadata: map[string]string{
			"doc":       "RefreshToken handles token refresh when the current token expires",
			"signature": "func RefreshToken(ctx context.Context, token string) (string, error)",
		},
	}
	authScore := scorer.Score(authNode)

	// Low relevance: unrelated node.
	dbNode := &graph.Node{
		Name: "CreateTable",
		File: "internal/db/migration.go",
		Metadata: map[string]string{
			"doc":       "CreateTable runs a database migration",
			"signature": "func CreateTable(ctx context.Context, name string) error",
		},
	}
	dbScore := scorer.Score(dbNode)

	if authScore <= dbScore {
		t.Errorf("auth node (%.3f) should score higher than db node (%.3f)", authScore, dbScore)
	}
	if authScore <= 0 {
		t.Errorf("auth node score should be > 0, got %.3f", authScore)
	}
}

func TestProblemScorer_EmptyProblem(t *testing.T) {
	scorer := NewProblemScorer("")
	node := &graph.Node{Name: "Foo", Metadata: map[string]string{}}
	score := scorer.Score(node)
	if score != 0 {
		t.Errorf("empty problem should score 0, got %.3f", score)
	}
}

func TestProblemScorer_FilePathMatch(t *testing.T) {
	scorer := NewProblemScorer("artwork placeholder fallback")

	artworkNode := &graph.Node{
		Name: "Get",
		File: "core/artwork/artwork.go",
		Metadata: map[string]string{},
	}
	configNode := &graph.Node{
		Name: "Get",
		File: "config/settings.go",
		Metadata: map[string]string{},
	}

	artScore := scorer.Score(artworkNode)
	cfgScore := scorer.Score(configNode)

	if artScore <= cfgScore {
		t.Errorf("artwork file (%.3f) should score higher than config file (%.3f)", artScore, cfgScore)
	}
}

func TestCombinedScore(t *testing.T) {
	// Equal blend.
	score := CombinedScore(0.8, 0.6)
	expected := 0.5*0.8 + 0.5*0.6
	if score != expected {
		t.Errorf("CombinedScore(0.8, 0.6) = %.3f, want %.3f", score, expected)
	}
}
