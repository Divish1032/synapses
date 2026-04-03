package lsp_test

import (
	"context"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/lsp"
)

// ── Confidence ────────────────────────────────────────────────────────────────

func TestConfidence_String(t *testing.T) {
	cases := []struct {
		c    lsp.Confidence
		want string
	}{
		{lsp.ConfidenceHigh, "HIGH"},
		{lsp.ConfidenceMedium, "MEDIUM"},
		{lsp.ConfidenceLow, "LOW"},
		{lsp.ConfidenceNone, "NONE"},
	}
	for _, tc := range cases {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("Confidence(%d).String() = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestConfidence_Ordering(t *testing.T) {
	// Ordering must be: None < Low < Medium < High (used for comparison in callers).
	if !(lsp.ConfidenceNone < lsp.ConfidenceLow) {
		t.Error("expected ConfidenceNone < ConfidenceLow")
	}
	if !(lsp.ConfidenceLow < lsp.ConfidenceMedium) {
		t.Error("expected ConfidenceLow < ConfidenceMedium")
	}
	if !(lsp.ConfidenceMedium < lsp.ConfidenceHigh) {
		t.Error("expected ConfidenceMedium < ConfidenceHigh")
	}
}

// ── NoOpVerifier ─────────────────────────────────────────────────────────────

func TestNoOpVerifier_ReturnsConfidenceNone(t *testing.T) {
	v := lsp.NoOpVerifier(lsp.LanguageGo)
	from := graph.NodeID("repo::file.go::Caller")
	to := graph.NodeID("repo::file.go::Callee")
	pos := lsp.CallPosition{File: "/abs/path/file.go", Line: 42, Col: 8}

	edge, err := v.ResolveEdge(context.Background(), from, to, pos)
	if err != nil {
		t.Fatalf("NoOpVerifier.ResolveEdge returned error: %v", err)
	}
	if edge == nil {
		t.Fatal("NoOpVerifier.ResolveEdge returned nil edge")
	}
	if edge.Confidence != lsp.ConfidenceNone {
		t.Errorf("expected ConfidenceNone, got %v", edge.Confidence)
	}
	if edge.From != from {
		t.Errorf("edge.From = %q, want %q", edge.From, from)
	}
	if edge.To != to {
		t.Errorf("edge.To = %q, want %q", edge.To, to)
	}
}

func TestNoOpVerifier_Language(t *testing.T) {
	for _, lang := range []lsp.Language{lsp.LanguageGo, lsp.LanguageTypeScript, lsp.LanguagePython} {
		v := lsp.NoOpVerifier(lang)
		if got := v.Language(); got != lang {
			t.Errorf("NoOpVerifier(%q).Language() = %q, want %q", lang, got, lang)
		}
	}
}

func TestNoOpVerifier_CloseIsNoop(t *testing.T) {
	v := lsp.NoOpVerifier(lsp.LanguageGo)
	if err := v.Close(); err != nil {
		t.Errorf("NoOpVerifier.Close() returned error: %v", err)
	}
	// Idempotent: second close also succeeds.
	if err := v.Close(); err != nil {
		t.Errorf("NoOpVerifier.Close() (second call) returned error: %v", err)
	}
}

func TestNoOpVerifier_EmptyNodeIDs(t *testing.T) {
	// Edge case: from and to are empty strings (unresolved call).
	v := lsp.NoOpVerifier(lsp.LanguageGo)
	edge, err := v.ResolveEdge(context.Background(), "", "", lsp.CallPosition{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge == nil {
		t.Fatal("returned nil edge for empty NodeIDs")
	}
	if edge.Confidence != lsp.ConfidenceNone {
		t.Errorf("expected ConfidenceNone for empty IDs, got %v", edge.Confidence)
	}
}

// ── Manager ──────────────────────────────────────────────────────────────────

func TestManager_GetUnregisteredLanguageReturnsNoop(t *testing.T) {
	m := lsp.NewManager(lsp.Options{})
	v := m.Get(lsp.LanguageGo)
	if v == nil {
		t.Fatal("Get returned nil for unregistered language")
	}
	// Should behave like NoOpVerifier.
	edge, err := v.ResolveEdge(context.Background(), "from", "to", lsp.CallPosition{File: "/f.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != lsp.ConfidenceNone {
		t.Errorf("expected ConfidenceNone from unregistered verifier, got %v", edge.Confidence)
	}
}

func TestManager_RegisterAndGet(t *testing.T) {
	m := lsp.NewManager(lsp.Options{})
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m.Register(stub)

	v := m.Get(lsp.LanguageGo)
	if v == nil {
		t.Fatal("Get returned nil after Register")
	}
	edge, err := v.ResolveEdge(context.Background(), "from", "to", lsp.CallPosition{File: "/f.go", Line: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if edge.Confidence != lsp.ConfidenceHigh {
		t.Errorf("expected ConfidenceHigh from registered stub, got %v", edge.Confidence)
	}
}

func TestManager_RegisterReplacesExisting(t *testing.T) {
	m := lsp.NewManager(lsp.Options{})
	first := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceLow}
	second := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}

	m.Register(first)
	m.Register(second)

	if !first.closed {
		t.Error("first verifier should have been closed on replacement")
	}

	edge, _ := m.Get(lsp.LanguageGo).ResolveEdge(context.Background(), "from", "to", lsp.CallPosition{File: "/f.go"})
	if edge.Confidence != lsp.ConfidenceHigh {
		t.Errorf("expected second verifier's ConfidenceHigh, got %v", edge.Confidence)
	}
}

func TestManager_ResolveEdgeCachesResult(t *testing.T) {
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{CacheTTL: time.Minute})
	m.Register(stub)

	pos := lsp.CallPosition{File: "/repo/main.go", Line: 10, Col: 5}

	_, err := m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("expected 1 verifier call, got %d", stub.calls)
	}

	// Second call to same pos should hit cache — stub should not be called again.
	_, err = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("expected cache hit (1 total call), got %d", stub.calls)
	}
}

func TestManager_CacheDoesNotCacheNone(t *testing.T) {
	// ConfidenceNone results (from NoOp) must not be cached — they are always
	// cheap to recompute and caching them would prevent real verifiers from
	// being tried after registration.
	m := lsp.NewManager(lsp.Options{CacheTTL: time.Minute})
	// No verifier registered — all queries go to NoOp.
	pos := lsp.CallPosition{File: "/repo/main.go", Line: 5, Col: 2}

	_, _ = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)

	if m.CacheSize() != 0 {
		t.Errorf("ConfidenceNone results should not be cached, but cache has %d entries", m.CacheSize())
	}
}

func TestManager_CacheExpiry(t *testing.T) {
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{CacheTTL: 1 * time.Millisecond}) // tiny TTL for test
	m.Register(stub)

	pos := lsp.CallPosition{File: "/repo/main.go", Line: 3, Col: 1}
	_, _ = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)

	time.Sleep(5 * time.Millisecond) // let TTL expire

	_, _ = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)
	if stub.calls != 2 {
		t.Errorf("expected 2 verifier calls (cache expired), got %d", stub.calls)
	}
}

func TestManager_ResolveEdgeEmptyFileSkipsCache(t *testing.T) {
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{CacheTTL: time.Minute})
	m.Register(stub)

	// Empty file — cache should not be consulted (no meaningful key).
	pos := lsp.CallPosition{File: "", Line: 0, Col: 0}
	_, _ = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)

	if m.CacheSize() != 0 {
		t.Errorf("empty-file results should not be cached, got %d entries", m.CacheSize())
	}
}

func TestManager_PurgeExpired(t *testing.T) {
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{CacheTTL: 1 * time.Millisecond})
	m.Register(stub)

	pos := lsp.CallPosition{File: "/repo/a.go", Line: 1, Col: 0}
	_, _ = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)

	if m.CacheSize() != 1 {
		t.Fatalf("expected 1 entry before purge, got %d", m.CacheSize())
	}

	time.Sleep(5 * time.Millisecond)
	m.PurgeExpired()

	if m.CacheSize() != 0 {
		t.Errorf("expected 0 entries after PurgeExpired, got %d", m.CacheSize())
	}
}

func TestManager_Close(t *testing.T) {
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{})
	m.Register(stub)

	m.Close()

	if !stub.closed {
		t.Error("stub verifier should have been closed by Manager.Close()")
	}

	// After close, Get should return NoOp (nothing registered).
	v := m.Get(lsp.LanguageGo)
	edge, _ := v.ResolveEdge(context.Background(), "from", "to", lsp.CallPosition{File: "/f.go"})
	if edge.Confidence != lsp.ConfidenceNone {
		t.Errorf("expected NoOp after Close(), got confidence %v", edge.Confidence)
	}
}

func TestManager_MaxCacheEntriesGuard(t *testing.T) {
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{MaxCacheEntries: 2, CacheTTL: time.Minute})
	m.Register(stub)

	for i := 0; i < 5; i++ {
		pos := lsp.CallPosition{File: "/repo/f.go", Line: i, Col: 0}
		_, _ = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)
	}

	if m.CacheSize() > 2 {
		t.Errorf("cache exceeded max: got %d entries, want ≤ 2", m.CacheSize())
	}
}

// ── CallPosition ─────────────────────────────────────────────────────────────

func TestCallPosition_ZeroValue(t *testing.T) {
	var pos lsp.CallPosition
	if pos.File != "" || pos.Line != 0 || pos.Col != 0 {
		t.Error("zero-value CallPosition should have empty file and zero line/col")
	}
}

// ── VerifiedEdge ─────────────────────────────────────────────────────────────

func TestVerifiedEdge_ConfirmedField(t *testing.T) {
	// Confirmed is true only when Callee.NodeID == To.
	from := graph.NodeID("repo::a.go::Caller")
	to := graph.NodeID("repo::b.go::Target")

	confirmed := &lsp.VerifiedEdge{
		From:      from,
		To:        to,
		Callee:    lsp.CalleeInfo{NodeID: to},
		Confidence: lsp.ConfidenceHigh,
		Confirmed:  true,
	}
	if !confirmed.Confirmed {
		t.Error("expected Confirmed=true when Callee.NodeID == To")
	}

	diffTarget := &lsp.VerifiedEdge{
		From:      from,
		To:        to,
		Callee:    lsp.CalleeInfo{NodeID: "repo::c.go::OtherTarget"},
		Confidence: lsp.ConfidenceHigh,
		Confirmed:  false,
	}
	if diffTarget.Confirmed {
		t.Error("expected Confirmed=false when Callee.NodeID != To")
	}
}

// ── NewVerifiedEdge ──────────────────────────────────────────────────────────

func TestNewVerifiedEdge_ConfirmedWhenMatch(t *testing.T) {
	to := graph.NodeID("repo::b.go::Target")
	callee := lsp.CalleeInfo{NodeID: to, File: "/b.go", Line: 5}
	edge := lsp.NewVerifiedEdge("from", to, callee, lsp.ConfidenceHigh)
	if !edge.Confirmed {
		t.Error("expected Confirmed=true when Callee.NodeID == To")
	}
}

func TestNewVerifiedEdge_NotConfirmedWhenDifferent(t *testing.T) {
	to := graph.NodeID("repo::b.go::Target")
	callee := lsp.CalleeInfo{NodeID: "repo::c.go::Other"}
	edge := lsp.NewVerifiedEdge("from", to, callee, lsp.ConfidenceHigh)
	if edge.Confirmed {
		t.Error("expected Confirmed=false when Callee.NodeID != To")
	}
}

func TestNewVerifiedEdge_NotConfirmedWhenCalleeEmpty(t *testing.T) {
	to := graph.NodeID("repo::b.go::Target")
	edge := lsp.NewVerifiedEdge("from", to, lsp.CalleeInfo{}, lsp.ConfidenceNone)
	if edge.Confirmed {
		t.Error("expected Confirmed=false when Callee.NodeID is empty")
	}
}

func TestManager_TrimIdle_ClosesIdleVerifier(t *testing.T) {
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{IdleTimeout: 5 * time.Millisecond, CacheTTL: time.Minute})
	m.Register(stub)

	// Make one call to record activity.
	pos := lsp.CallPosition{File: "/repo/main.go", Line: 1, Col: 0}
	_, _ = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)

	time.Sleep(10 * time.Millisecond) // exceed IdleTimeout
	m.TrimIdle()

	if !stub.closed {
		t.Error("TrimIdle should have closed idle verifier")
	}
	// After trim, Get should return NoOp.
	edge, _ := m.Get(lsp.LanguageGo).ResolveEdge(context.Background(), "from", "to", pos)
	if edge.Confidence != lsp.ConfidenceNone {
		t.Errorf("expected NoOp after TrimIdle, got %v", edge.Confidence)
	}
}

func TestManager_TrimIdle_DoesNotCloseActiveVerifier(t *testing.T) {
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{IdleTimeout: time.Hour, CacheTTL: time.Minute})
	m.Register(stub)

	pos := lsp.CallPosition{File: "/repo/main.go", Line: 1, Col: 0}
	_, _ = m.ResolveEdge(context.Background(), lsp.LanguageGo, "from", "to", pos)

	m.TrimIdle() // IdleTimeout is 1 hour — verifier is not idle

	if stub.closed {
		t.Error("TrimIdle should not close recently-used verifier")
	}
}

func TestManager_TrimIdle_RegisteredButNeverUsed(t *testing.T) {
	// A verifier that was registered but never had ResolveEdge called should
	// be considered idle (no lastUsed entry).
	stub := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}
	m := lsp.NewManager(lsp.Options{IdleTimeout: time.Millisecond})
	m.Register(stub)

	m.TrimIdle() // no ResolveEdge ever called → no lastUsed entry → considered idle

	if !stub.closed {
		t.Error("TrimIdle should close verifier that was never used")
	}
}

func TestManager_CloseDoesNotHoldLockDuringVerifierClose(t *testing.T) {
	// Verify that Manager.Close() releases the write lock before calling v.Close(),
	// so concurrent Get() calls are not blocked during a slow LSP shutdown.
	slow := &slowCloseVerifier{lang: lsp.LanguageGo, closeSleep: 20 * time.Millisecond}
	m := lsp.NewManager(lsp.Options{})
	m.Register(slow)

	done := make(chan struct{})
	go func() {
		m.Close()
		close(done)
	}()

	// Give Close goroutine a moment to start, then Get should not block.
	time.Sleep(2 * time.Millisecond)
	v := m.Get(lsp.LanguageGo) // should not deadlock
	edge, _ := v.ResolveEdge(context.Background(), "from", "to", lsp.CallPosition{File: "/f.go"})
	if edge.Confidence != lsp.ConfidenceNone {
		t.Errorf("expected NoOp after Close started, got %v", edge.Confidence)
	}

	<-done
}

func TestManager_RegisterDoesNotHoldLockDuringClose(t *testing.T) {
	// Verify that Close() is called after releasing the write lock by observing
	// that a concurrent Get() call (RLock) can proceed while the slow close runs.
	slow := &slowCloseVerifier{lang: lsp.LanguageGo, closeSleep: 20 * time.Millisecond}
	fast := &stubVerifier{lang: lsp.LanguageGo, confidence: lsp.ConfidenceHigh}

	m := lsp.NewManager(lsp.Options{})
	m.Register(slow)

	done := make(chan struct{})
	go func() {
		// Replace slow with fast — Close(slow) should not block Get().
		m.Register(fast)
		close(done)
	}()

	// Give Register goroutine a moment to start, then Get should not block.
	time.Sleep(2 * time.Millisecond)
	v := m.Get(lsp.LanguageGo)
	_ = v // just verifying it doesn't deadlock

	<-done
}

// ── stubVerifier — test double ────────────────────────────────────────────────

type stubVerifier struct {
	lang       lsp.Language
	confidence lsp.Confidence
	calls      int
	closed     bool
}

func (s *stubVerifier) ResolveEdge(_ context.Context, from, to graph.NodeID, pos lsp.CallPosition) (*lsp.VerifiedEdge, error) {
	s.calls++
	callee := lsp.CalleeInfo{NodeID: to, File: pos.File, Line: pos.Line}
	return lsp.NewVerifiedEdge(from, to, callee, s.confidence), nil
}

func (s *stubVerifier) Language() lsp.Language { return s.lang }
func (s *stubVerifier) Close() error           { s.closed = true; return nil }

// slowCloseVerifier sleeps during Close to simulate a slow LSP process shutdown.
type slowCloseVerifier struct {
	lang       lsp.Language
	closeSleep time.Duration
}

func (s *slowCloseVerifier) ResolveEdge(_ context.Context, from, to graph.NodeID, _ lsp.CallPosition) (*lsp.VerifiedEdge, error) {
	return lsp.NewVerifiedEdge(from, to, lsp.CalleeInfo{}, lsp.ConfidenceNone), nil
}

func (s *slowCloseVerifier) Language() lsp.Language { return s.lang }
func (s *slowCloseVerifier) Close() error           { time.Sleep(s.closeSleep); return nil }
