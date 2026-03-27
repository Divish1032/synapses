package store

import (
	"strings"
	"testing"
)

func TestNodeTextEnriched_WithCallGraph(t *testing.T) {
	text := nodeTextEnriched("BuildGraph", "func()", "builds the graph",
		[]string{"main", "InitApp"},
		[]string{"AddNode", "AddEdge", "Resolve"})
	if !strings.Contains(text, "BuildGraph") {
		t.Error("missing name")
	}
	if !strings.Contains(text, "calls: AddNode, AddEdge, Resolve") {
		t.Error("missing callees")
	}
	if !strings.Contains(text, "called by: main, InitApp") {
		t.Error("missing callers")
	}
}

func TestNodeTextEnriched_NoCallGraph(t *testing.T) {
	text := nodeTextEnriched("Simple", "func()", "does stuff", nil, nil)
	if strings.Contains(text, "calls:") || strings.Contains(text, "called by:") {
		t.Error("should not have call graph context when empty")
	}
	if text != "Simple func() does stuff" {
		t.Errorf("unexpected text: %q", text)
	}
}

func TestNodeTextEnriched_CalleesMaxFive(t *testing.T) {
	callees := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf"}
	text := nodeTextEnriched("Func", "", "", nil, callees)
	// Should only include first 5
	if strings.Contains(text, "Foxtrot") || strings.Contains(text, "Golf") {
		t.Error("should cap callees at 5")
	}
	if !strings.Contains(text, "Echo") {
		t.Error("should include the 5th callee")
	}
}

func TestNodeContentHashEnriched_ChangesWithCallGraph(t *testing.T) {
	h1 := nodeContentHashEnriched("Func", "", "", nil, nil)
	h2 := nodeContentHashEnriched("Func", "", "", []string{"Caller"}, nil)
	if h1 == h2 {
		t.Error("hash should change when callers change")
	}
}
