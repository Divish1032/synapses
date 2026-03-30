package mcp

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestLoadCoAccessHints_NilBrain(t *testing.T) {
	g := graph.New("")
	hints := loadCoAccessHints(nil, g, "test")
	if hints != nil {
		t.Error("expected nil hints with nil brain client")
	}
}

func TestLoadCoAccessHints_EmptyRoot(t *testing.T) {
	g := graph.New("")
	hints := loadCoAccessHints(nil, g, "")
	if hints != nil {
		t.Error("expected nil hints with empty root name")
	}
}

func TestCoAccessConstants(t *testing.T) {
	if coAccessMinCount < 1 {
		t.Error("coAccessMinCount should be >= 1")
	}
	if coAccessMaxInject < 1 {
		t.Error("coAccessMaxInject should be >= 1")
	}
	if coAccessMinConf <= 0 || coAccessMinConf >= 1 {
		t.Error("coAccessMinConf should be in (0, 1)")
	}
}
