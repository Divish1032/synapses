package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
)

// TestRecallChannelWeights_NilPulse verifies that recallChannelWeights returns
// DefaultRRFWeights when no pulse client is configured (nil pulseClient).
func TestRecallChannelWeights_NilPulse(t *testing.T) {
	srv := newTestServer(t) // pulseClient is nil by default

	weights := srv.recallChannelWeights()
	if weights == nil {
		t.Fatal("expected non-nil weights when pulse is nil")
	}
	for ch, want := range store.DefaultRRFWeights {
		if got := weights[ch]; got != want {
			t.Errorf("channel %q: got %.3f, want %.3f (DefaultRRFWeights)", ch, got, want)
		}
	}
}

// TestRecallChannelWeights_EmptyPulse verifies that recallChannelWeights falls
// back to DefaultRRFWeights when the pulse store has no channel attribution data
// yet — cold start before any recall_hit events have been recorded.
func TestRecallChannelWeights_EmptyPulse(t *testing.T) {
	srv := newTestServer(t)
	pc := newPulseClient(t)
	defer pc.Close()
	srv.SetPulseClient(pc)
	srv.projectID = "test-proj"

	weights := srv.recallChannelWeights()
	for ch, want := range store.DefaultRRFWeights {
		if got := weights[ch]; got != want {
			t.Errorf("cold start: channel %q weight %.3f, want default %.3f", ch, got, want)
		}
	}
}

// TestQuadRecallSearch_AttributionNoMetadataKeys verifies that the attribution
// map returned by quadRecallSearch never contains metadata pseudo-channel names
// (keys starting with "_") as attribution values. Before Sprint 15 #4, the
// _vector_search_ms metadata key leaked through RRFMergeWeighted and polluted
// the attribution map with fake memory-ID entries.
func TestQuadRecallSearch_AttributionNoMetadataKeys(t *testing.T) {
	srv := newTestServer(t)

	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierEntity,
		Content: "AuthService handles token validation for payment flows",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "auth payment", "", 5, false, 7, nil, 0)
	if attr == nil {
		t.Fatal("expected non-nil attribution map")
	}

	// Every attribution value must be a real channel name — no "_*" metadata keys.
	validChannels := map[string]bool{
		"bm25": true, "semantic": true, "graph": true, "temporal": true,
	}
	for memID, channels := range attr {
		for _, ch := range channels {
			if strings.HasPrefix(ch, "_") {
				t.Errorf("memory %q has metadata channel %q in attribution — metadata must not leak through RRF", memID, ch)
			}
			if !validChannels[ch] {
				t.Errorf("memory %q attributed to unknown channel %q (want one of bm25/semantic/graph/temporal)", memID, ch)
			}
		}
	}

	// Rank-1 attribution must be non-empty when results exist.
	if len(mems) > 0 && len(attr[mems[0].ID]) == 0 {
		t.Errorf("rank-1 memory %q has no attribution channels", mems[0].ID)
	}
}

// TestTopChannelExtraction_RankOneIsRealChannelName verifies the TopChannel
// extraction logic from Sprint 15 #4: taking the first non-metadata channel
// from the rank-1 memory's attribution produces a valid channel name that can
// be recorded in pulse's recall_channel_weights table.
func TestTopChannelExtraction_RankOneIsRealChannelName(t *testing.T) {
	srv := newTestServer(t)

	_, err := srv.store.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "database schema migration adds index on user_id column",
		AgentID: "agent-1",
		Source:  store.SourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}

	mems, attr, _, _ := srv.quadRecallSearch(context.Background(), "database migration index", "", 5, false, 7, nil, 0)
	if len(mems) == 0 {
		t.Skip("no results — skipping top-channel extraction check")
	}

	validChannels := map[string]bool{
		"bm25": true, "semantic": true, "graph": true, "temporal": true,
	}

	// Replicate the Sprint 15 #4 TopChannel extraction from episode_tools.go.
	var topChan string
	for _, ch := range attr[mems[0].ID] {
		if !strings.HasPrefix(ch, "_") {
			topChan = ch
			break
		}
	}

	if topChan == "" {
		t.Fatalf("TopChannel is empty for rank-1 result %q (attribution: %v) — no valid channel name found", mems[0].ID, attr[mems[0].ID])
	}
	if !validChannels[topChan] {
		t.Errorf("TopChannel %q is not a known channel name — would corrupt recall_channel_weights stats", topChan)
	}
}
