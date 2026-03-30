package mcp

import (
	"sync"
	"time"
)

// recallFootprint records the entities/files surfaced by a single recall call.
// Used to correlate subsequent tool calls with recall results ("did the agent
// act on what it recalled?").
type recallFootprint struct {
	RecallID   string
	EntityIDs  []string
	FilePaths  []string
	Timestamp  time.Time
	ActedOn    bool
	ActedWeight float64 // 1.0 for <2min, 0.5 for 2-5min
	TopChannel string
	Query      string
	ResultCount int
	CrossProjectHits int
}

// recallFootprintRing is a bounded ring buffer of recent recall footprints
// per session. Keeps the last maxRecallFootprints entries.
type recallFootprintRing struct {
	mu    sync.Mutex
	items [maxRecallFootprints]recallFootprint
	count int
	next  int // write cursor
}

const maxRecallFootprints = 5

// push adds a new footprint, overwriting the oldest if full.
func (r *recallFootprintRing) push(fp recallFootprint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[r.next] = fp
	r.next = (r.next + 1) % maxRecallFootprints
	if r.count < maxRecallFootprints {
		r.count++
	}
}

// checkActedOn tests if the given entities/files overlap with any un-acted-on
// footprint within the time window. Returns the first matching footprint and
// the signal weight (1.0 for strong, 0.5 for weak, 0.0 for no match).
func (r *recallFootprintRing) checkActedOn(entityIDs, filePaths []string, now time.Time) (*recallFootprint, float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	const strongWindow = 2 * time.Minute
	const weakWindow = 5 * time.Minute

	for i := 0; i < r.count; i++ {
		fp := &r.items[i]
		if fp.ActedOn {
			continue
		}
		elapsed := now.Sub(fp.Timestamp)
		if elapsed > weakWindow {
			continue
		}

		if overlaps(entityIDs, fp.EntityIDs) || overlaps(filePaths, fp.FilePaths) {
			fp.ActedOn = true
			if elapsed <= strongWindow {
				fp.ActedWeight = 1.0
			} else {
				fp.ActedWeight = 0.5
			}
			return fp, fp.ActedWeight
		}
	}
	return nil, 0
}

// effectiveness computes the acted-on rate for this session's recalls.
// Returns (rate, totalWithResults, actedOnCount).
func (r *recallFootprintRing) effectiveness() (float64, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var total, actedOn int
	for i := 0; i < r.count; i++ {
		if r.items[i].ResultCount > 0 {
			total++
			if r.items[i].ActedOn {
				actedOn++
			}
		}
	}
	if total == 0 {
		return 0, 0, 0
	}
	return float64(actedOn) / float64(total), total, actedOn
}

// overlaps returns true if any element in a appears in b.
func overlaps(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(b))
	for _, s := range b {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	for _, s := range a {
		if _, ok := set[s]; ok {
			return true
		}
	}
	return false
}

// recordRecallFootprint stores a recall result footprint for later correlation.
func (s *Server) recordRecallFootprint(sessionID string, fp recallFootprint) {
	if sessionID == "" || (len(fp.EntityIDs) == 0 && len(fp.FilePaths) == 0) {
		return
	}
	ringI, _ := s.recallFootprints.LoadOrStore(sessionID, &recallFootprintRing{})
	ring := ringI.(*recallFootprintRing)
	ring.push(fp)
}

// checkRecallActedOn checks if current tool call entities/files match a recent
// recall footprint. Returns the matched footprint and weight, or nil/0.
func (s *Server) checkRecallActedOn(sessionID string, entityIDs, filePaths []string) (*recallFootprint, float64) {
	if sessionID == "" {
		return nil, 0
	}
	ringI, ok := s.recallFootprints.Load(sessionID)
	if !ok {
		return nil, 0
	}
	ring := ringI.(*recallFootprintRing)
	return ring.checkActedOn(entityIDs, filePaths, time.Now())
}

// getRecallEffectiveness returns the recall effectiveness rate for a session.
func (s *Server) getRecallEffectiveness(sessionID string) (rate float64, total, actedOn int) {
	if sessionID == "" {
		return 0, 0, 0
	}
	ringI, ok := s.recallFootprints.Load(sessionID)
	if !ok {
		return 0, 0, 0
	}
	ring := ringI.(*recallFootprintRing)
	return ring.effectiveness()
}

// clearRecallFootprints removes footprints for a session (called on session end).
func (s *Server) clearRecallFootprints(sessionID string) {
	s.recallFootprints.Delete(sessionID)
}
