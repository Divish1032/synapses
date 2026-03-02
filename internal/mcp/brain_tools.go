package mcp

import (
	"github.com/SynapsesOS/synapses/internal/brain"
)

// getBrainClient type-asserts the stored brainClient to *brain.Client.
// Returns nil if no brain client is configured.
func (s *Server) getBrainClient() *brain.Client {
	if s.brainClient == nil {
		return nil
	}
	bc, _ := s.brainClient.(*brain.Client)
	return bc
}
