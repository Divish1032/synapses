//go:build !llamacpp

package llm

// local_stub.go — pure-Go stub for LocalClient when CGO_ENABLED=0.
//
// When built without CGo (e.g. in CI, or when go-llama.cpp is not yet added
// to go.mod), these stubs make the package compile cleanly.
// LocalClient will report itself as unavailable so the pipeline falls back
// to Ollama or the no-LLM path automatically.

import (
	"context"
	"errors"
)

var errCGODisabled = errors.New("local LLM: CGo is disabled — rebuild with CGO_ENABLED=1 and go-llama.cpp in go.mod")

func (c *LocalClient) loadModel() error {
	c.available = false
	return errCGODisabled
}

func (c *LocalClient) generate(_ context.Context, _ string) (string, error) {
	return "", errCGODisabled
}
