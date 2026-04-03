package benchmarks

import (
	"context"
	"errors"
	"testing"

	"github.com/SynapsesOS/synapses/internal/lsp"
)

// mockCallHierarchyProvider is a test double for lsp.CallHierarchyProvider.
type mockCallHierarchyProvider struct {
	prepareItems []lsp.CallHierarchyItem
	prepareErr   error
	incomingItems []lsp.CallHierarchyItem
	incomingErr   error
	outgoingItems []lsp.CallHierarchyItem
	outgoingErr   error
}

func (m *mockCallHierarchyProvider) PrepareCallHierarchy(_ context.Context, _ lsp.CallPosition) ([]lsp.CallHierarchyItem, error) {
	return m.prepareItems, m.prepareErr
}

func (m *mockCallHierarchyProvider) IncomingCalls(_ context.Context, _ lsp.CallHierarchyItem) ([]lsp.CallHierarchyItem, error) {
	return m.incomingItems, m.incomingErr
}

func (m *mockCallHierarchyProvider) OutgoingCalls(_ context.Context, _ lsp.CallHierarchyItem) ([]lsp.CallHierarchyItem, error) {
	return m.outgoingItems, m.outgoingErr
}

func newTestLSPRunner(mock *mockCallHierarchyProvider) *LSPBenchRunner {
	return &LSPBenchRunner{provider: mock, lang: "go"}
}

func TestLSPBenchRunner_QueryCallers_PrepareEmpty_ReturnsNil(t *testing.T) {
	mock := &mockCallHierarchyProvider{prepareItems: nil}
	r := newTestLSPRunner(mock)
	names := r.QueryCallers(context.Background(), "/a.go", 10)
	if names != nil {
		t.Errorf("expected nil when PrepareCallHierarchy returns empty, got %v", names)
	}
}

func TestLSPBenchRunner_QueryCallers_PrepareError_ReturnsNil(t *testing.T) {
	mock := &mockCallHierarchyProvider{prepareErr: errors.New("process died")}
	r := newTestLSPRunner(mock)
	names := r.QueryCallers(context.Background(), "/a.go", 10)
	if names != nil {
		t.Errorf("expected nil on PrepareCallHierarchy error, got %v", names)
	}
}

func TestLSPBenchRunner_QueryCallers_IncomingError_ReturnsNil(t *testing.T) {
	mock := &mockCallHierarchyProvider{
		prepareItems: []lsp.CallHierarchyItem{{Name: "Foo", File: "/a.go", Line: 10}},
		incomingErr:  errors.New("timeout"),
	}
	r := newTestLSPRunner(mock)
	names := r.QueryCallers(context.Background(), "/a.go", 10)
	if names != nil {
		t.Errorf("expected nil on IncomingCalls error, got %v", names)
	}
}

func TestLSPBenchRunner_QueryCallers_Success_FilterEmpty(t *testing.T) {
	mock := &mockCallHierarchyProvider{
		prepareItems: []lsp.CallHierarchyItem{{Name: "Foo", File: "/a.go", Line: 10}},
		incomingItems: []lsp.CallHierarchyItem{
			{Name: "main"},
			{Name: ""},      // empty — should be filtered
			{Name: "Setup"},
		},
	}
	r := newTestLSPRunner(mock)
	names := r.QueryCallers(context.Background(), "/a.go", 10)
	if len(names) != 2 {
		t.Fatalf("expected 2 names (empty filtered), got %d: %v", len(names), names)
	}
	if names[0] != "main" || names[1] != "Setup" {
		t.Errorf("unexpected names: %v", names)
	}
}

func TestLSPBenchRunner_QueryCallees_PrepareEmpty_ReturnsNil(t *testing.T) {
	mock := &mockCallHierarchyProvider{prepareItems: nil}
	r := newTestLSPRunner(mock)
	names := r.QueryCallees(context.Background(), "/a.go", 5)
	if names != nil {
		t.Errorf("expected nil when PrepareCallHierarchy returns empty, got %v", names)
	}
}

func TestLSPBenchRunner_QueryCallees_OutgoingError_ReturnsNil(t *testing.T) {
	mock := &mockCallHierarchyProvider{
		prepareItems: []lsp.CallHierarchyItem{{Name: "Bar", File: "/a.go", Line: 5}},
		outgoingErr:  errors.New("gone"),
	}
	r := newTestLSPRunner(mock)
	names := r.QueryCallees(context.Background(), "/a.go", 5)
	if names != nil {
		t.Errorf("expected nil on OutgoingCalls error, got %v", names)
	}
}

func TestLSPBenchRunner_QueryCallees_Success(t *testing.T) {
	mock := &mockCallHierarchyProvider{
		prepareItems: []lsp.CallHierarchyItem{{Name: "Bar", File: "/a.go", Line: 5}},
		outgoingItems: []lsp.CallHierarchyItem{
			{Name: "Query"},
			{Name: "Close"},
		},
	}
	r := newTestLSPRunner(mock)
	names := r.QueryCallees(context.Background(), "/a.go", 5)
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d: %v", len(names), names)
	}
	if names[0] != "Query" || names[1] != "Close" {
		t.Errorf("unexpected names: %v", names)
	}
}

func TestLSPBenchRunner_Close_NilEv(t *testing.T) {
	r := &LSPBenchRunner{} // ev is nil
	if err := r.Close(); err != nil {
		t.Errorf("Close with nil ev should return nil, got %v", err)
	}
}
