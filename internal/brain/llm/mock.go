package llm

import (
	"context"
	"io"
)

// MockClient is a deterministic LLM client for tests.
// It returns a fixed response for every Generate call.
type MockClient struct {
	Response  string
	Err       error
	model     string
	available bool
}

// NewMockClient creates a MockClient that always returns the given response.
func NewMockClient(response string) *MockClient {
	return &MockClient{
		Response:  response,
		model:     "mock:test",
		available: true,
	}
}

// NewUnavailableMockClient creates a MockClient that reports itself unavailable.
func NewUnavailableMockClient() *MockClient {
	return &MockClient{
		model:     "mock:test",
		available: false,
	}
}

// Generate returns the configured mock response.
func (m *MockClient) Generate(_ context.Context, _ string) (string, error) {
	return m.Response, m.Err
}

// Available reports whether the mock client is configured as available.
func (m *MockClient) Available(_ context.Context) bool {
	return m.available
}

// ModelName returns the mock model name.
func (m *MockClient) ModelName() string {
	return m.model
}

// ModelPulled reports whether the mock model is available.
func (m *MockClient) ModelPulled(_ context.Context) bool {
	return m.available
}

// PullModel is a no-op for the mock client.
func (m *MockClient) PullModel(_ context.Context, _ io.Writer) error {
	return nil
}
