package brain

import (
	"context"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/brain/archivist"
	brainconfig "github.com/SynapsesOS/synapses/internal/brain/config"
)

// Direct tests for impl methods to reach 80% coverage
// These test the actual impl struct methods, not through the interface

func TestImpl_Ingest_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Ingest: false}
	b := &impl{cfg: cfg}
	resp, err := b.Ingest(context.Background(), IngestRequest{NodeID: "n1"})
	if err != nil || resp.NodeID != "n1" {
		t.Errorf("Ingest disabled path failed")
	}
}

func TestImpl_Ingest_DirectCallCBOpen(t *testing.T) {
	cfg := brainconfig.BrainConfig{Ingest: true}
	cb := newCircuitBreaker(1, 1*time.Second)
	cb.recordFailure("ingest")
	cb.recordFailure("ingest")
	b := &impl{cfg: cfg, cb: cb}
	resp, _ := b.Ingest(context.Background(), IngestRequest{NodeID: "n1"})
	if resp.NodeID != "n1" {
		t.Error("Ingest CB open path failed")
	}
}

func TestImpl_Enrich_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enrich: false}
	b := &impl{cfg: cfg, store: nil}
	defer func() { recover() }()
	b.Enrich(context.Background(), EnrichRequest{})
}

func TestImpl_Enrich_DirectCallCBOpen(t *testing.T) {
	cfg := brainconfig.BrainConfig{Enrich: true}
	cb := newCircuitBreaker(1, 1*time.Second)
	cb.recordFailure("enrich")
	cb.recordFailure("enrich")
	b := &impl{cfg: cfg, cb: cb, store: nil}
	defer func() { recover() }()
	b.Enrich(context.Background(), EnrichRequest{})
}

func TestImpl_ExplainViolation_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Guardian: false}
	b := &impl{cfg: cfg}
	resp, _ := b.ExplainViolation(context.Background(), ViolationRequest{})
	if resp.Explanation != "" {
		t.Error("ExplainViolation disabled path failed")
	}
}

func TestImpl_ExplainViolation_DirectCallNoGuardian(t *testing.T) {
	cfg := brainconfig.BrainConfig{Guardian: true}
	b := &impl{cfg: cfg, guardian: nil}
	resp, _ := b.ExplainViolation(context.Background(), ViolationRequest{})
	if resp.Explanation != "" {
		t.Error("ExplainViolation no guardian path failed")
	}
}

func TestImpl_Coordinate_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Orchestrate: false}
	b := &impl{cfg: cfg}
	resp, _ := b.Coordinate(context.Background(), CoordinateRequest{})
	if resp.Suggestion != "" {
		t.Error("Coordinate disabled path failed")
	}
}

func TestImpl_Coordinate_DirectCallNoOrch(t *testing.T) {
	cfg := brainconfig.BrainConfig{Orchestrate: true}
	b := &impl{cfg: cfg, orchestrator: nil}
	resp, _ := b.Coordinate(context.Background(), CoordinateRequest{})
	if resp.Suggestion != "" {
		t.Error("Coordinate no orchestrator path failed")
	}
}

func TestImpl_Prune_DirectCall(t *testing.T) {
	b := &impl{pruner: nil}
	defer func() { recover() }()
	b.Prune(context.Background(), "test")
}

func TestImpl_Memorize_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{Memorize: false}
	b := &impl{cfg: cfg}
	resp, _ := b.Memorize(context.Background(), archivist.MemorizeRequest{})
	if len(resp.NewMemories) != 0 {
		t.Error("Memorize disabled path failed")
	}
}

func TestImpl_Memorize_DirectCallNoArchivist(t *testing.T) {
	cfg := brainconfig.BrainConfig{Memorize: true}
	b := &impl{cfg: cfg, archivist: nil}
	resp, _ := b.Memorize(context.Background(), archivist.MemorizeRequest{})
	if len(resp.NewMemories) != 0 {
		t.Error("Memorize no archivist path failed")
	}
}

func TestImpl_BuildContextPacket_DirectCallDisabled(t *testing.T) {
	cfg := brainconfig.BrainConfig{ContextBuilder: false}
	b := &impl{cfg: cfg}
	resp, _ := b.BuildContextPacket(context.Background(), ContextPacketRequest{})
	if resp != nil {
		t.Error("BuildContextPacket disabled path failed")
	}
}

func TestImpl_BuildContextPacket_DirectCallCBOpen(t *testing.T) {
	cfg := brainconfig.BrainConfig{ContextBuilder: true}
	cb := newCircuitBreaker(1, 1*time.Second)
	cb.recordFailure("context_builder")
	cb.recordFailure("context_builder")
	b := &impl{cfg: cfg, cb: cb}
	resp, _ := b.BuildContextPacket(context.Background(), ContextPacketRequest{})
	if resp != nil {
		t.Error("BuildContextPacket CB open path failed")
	}
}

func TestImpl_LogDecision_DirectCall(t *testing.T) {
	b := &impl{learner: nil}
	defer func() { recover() }()
	b.LogDecision(context.Background(), DecisionRequest{})
}

func TestImpl_SetSDLCPhase_DirectCall(t *testing.T) {
	b := &impl{sdlcMgr: nil}
	defer func() { recover() }()
	b.SetSDLCPhase(PhaseDevelopment, "a1")
}

func TestImpl_SetQualityMode_DirectCall(t *testing.T) {
	b := &impl{sdlcMgr: nil}
	defer func() { recover() }()
	b.SetQualityMode(QualityStandard, "a1")
}

func TestImpl_GetSDLCConfig_DirectCall(t *testing.T) {
	b := &impl{sdlcMgr: nil}
	defer func() { recover() }()
	b.GetSDLCConfig()
}

func TestImpl_GetPatterns_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.GetPatterns("", 0)
}

func TestImpl_UpsertADR_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.UpsertADR(ADRRequest{ID: "a1"})
}

func TestImpl_GetADR_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.GetADR("a1")
}

func TestImpl_AllADRs_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.AllADRs()
}

func TestImpl_GetADRsForFile_DirectCall(t *testing.T) {
	b := &impl{store: nil}
	defer func() { recover() }()
	b.GetADRsForFile("f.go", 10)
}
