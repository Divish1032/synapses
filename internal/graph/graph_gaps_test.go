package graph

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// ── cache.go tests ───────────────────────────────────────────────────────────

func TestCacheKeyFor_DeterministicAndDistinct(t *testing.T) {
	cfg1 := CarveConfig{MaxDepth: 2, TokenBudget: 100, MinRelevance: 0.1, DecayFactor: 0.5, DirectionBoost: 0.3, IntentID: "debug"}
	cfg2 := CarveConfig{MaxDepth: 3, TokenBudget: 100, MinRelevance: 0.1, DecayFactor: 0.5, DirectionBoost: 0.3, IntentID: "debug"}

	k1a := cacheKeyFor("root1", cfg1, "fp1")
	k1b := cacheKeyFor("root1", cfg1, "fp1")
	k2 := cacheKeyFor("root1", cfg2, "fp1")

	if k1a != k1b {
		t.Fatal("same inputs should produce same key")
	}
	if k1a == k2 {
		t.Fatal("different configs should produce different keys")
	}
}

func TestCacheKeyFor_IntentIDIsolation(t *testing.T) {
	cfg := CarveConfig{MaxDepth: 2, TokenBudget: 100}
	cfgIntent := CarveConfig{MaxDepth: 2, TokenBudget: 100, IntentID: "review"}

	if cacheKeyFor("n", cfg, "fp") == cacheKeyFor("n", cfgIntent, "fp") {
		t.Fatal("different IntentIDs must produce different cache keys")
	}
}

func TestExtractFiles_CollectsUniqueFiles(t *testing.T) {
	sub := &SubGraph{
		Nodes: []CarvedNode{
			{Node: &Node{File: "a.go"}},
			{Node: &Node{File: "b.go"}},
			{Node: &Node{File: "a.go"}},
			{Node: nil},
			{Node: &Node{File: ""}},
		},
	}
	files := extractFiles(sub)
	if len(files) != 2 {
		t.Fatalf("expected 2 unique files, got %d", len(files))
	}
	if _, ok := files["a.go"]; !ok {
		t.Fatal("missing a.go")
	}
	if _, ok := files["b.go"]; !ok {
		t.Fatal("missing b.go")
	}
}

func TestSubgraphCache_PutAndGet(t *testing.T) {
	c := newSubgraphCache()
	cfg := CarveConfig{MaxDepth: 2}
	sub := &SubGraph{Root: "r1", Nodes: []CarvedNode{{Node: &Node{File: "x.go"}}}}
	fp := "testfingerprint"

	c.put("r1", cfg, fp, sub)
	got, ok := c.get("r1", cfg, fp)
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.Root != "r1" {
		t.Fatalf("expected root r1, got %s", got.Root)
	}
}

func TestSubgraphCache_MissForUnknownKey(t *testing.T) {
	c := newSubgraphCache()
	_, ok := c.get("unknown", CarveConfig{}, "")
	if ok {
		t.Fatal("expected cache miss for unknown key")
	}
}

func TestSubgraphCache_Invalidate(t *testing.T) {
	c := newSubgraphCache()
	cfg := CarveConfig{MaxDepth: 1}
	fp := "fp1"
	c.put("r1", cfg, fp, &SubGraph{Root: "r1"})
	c.invalidate()
	_, ok := c.get("r1", cfg, fp)
	if ok {
		t.Fatal("expected miss after full invalidation")
	}
}

func TestSubgraphCache_EvictsOldestWhenFull(t *testing.T) {
	c := newSubgraphCache()
	// Fill to capacity — each entry has a unique depth and fingerprint.
	for i := 0; i < cacheMaxSize+5; i++ {
		cfg := CarveConfig{MaxDepth: i}
		c.put(NodeID("root"), cfg, "fp", &SubGraph{Root: "root"})
	}
	if len(c.entries) > cacheMaxSize {
		t.Fatalf("cache should not exceed max size %d, got %d", cacheMaxSize, len(c.entries))
	}
}

func TestSubgraphCache_InvalidateForFile_EvictsOnlyMatching(t *testing.T) {
	c := newSubgraphCache()
	cfgA := CarveConfig{MaxDepth: 1}
	cfgB := CarveConfig{MaxDepth: 2}

	c.put("a", cfgA, "fpa", &SubGraph{Root: "a", Nodes: []CarvedNode{{Node: &Node{File: "pkg/foo.go"}}}})
	c.put("b", cfgB, "fpb", &SubGraph{Root: "b", Nodes: []CarvedNode{{Node: &Node{File: "pkg/bar.go"}}}})

	c.invalidateForFile("pkg/foo.go")

	if _, ok := c.get("a", cfgA, "fpa"); ok {
		t.Fatal("entry referencing foo.go should have been evicted")
	}
	if _, ok := c.get("b", cfgB, "fpb"); !ok {
		t.Fatal("entry referencing bar.go should survive")
	}
}

func TestEntryReferencesFile_SuffixMatch(t *testing.T) {
	e := &cacheEntry{files: map[string]struct{}{"internal/graph/cache.go": {}}}

	tests := []struct {
		file string
		want bool
	}{
		{"internal/graph/cache.go", true},
		{"graph/cache.go", true},
		{"cache.go", true}, // suffix match works both ways
		{"other.go", false},
	}
	for _, tc := range tests {
		if got := entryReferencesFile(e, tc.file); got != tc.want {
			t.Errorf("entryReferencesFile(%q) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

func TestSubgraphCache_ExpiredEntryNotReturned(t *testing.T) {
	c := newSubgraphCache()
	cfg := CarveConfig{MaxDepth: 1}
	fp := "fpexpiry"
	c.put("r", cfg, fp, &SubGraph{Root: "r"})

	// Force expiry by rewriting the expiresAt field directly.
	c.mu.Lock()
	key := cacheKeyFor("r", cfg, fp)
	c.entries[key].expiresAt = time.Now().Add(-1 * time.Second)
	c.mu.Unlock()

	_, ok := c.get("r", cfg, fp)
	if ok {
		t.Fatal("expired entry should not be returned")
	}
}

func TestSubgraphCache_LRU_EvictsLeastRecentlyUsed(t *testing.T) {
	c := newSubgraphCache()
	// Insert 3 entries with capacity constrained to test LRU behaviour.
	// We use the real cacheMaxSize (512), so fill to capacity then verify
	// the *accessed* entry survives while the untouched one is evicted.

	// Fill cache to capacity.
	for i := 0; i < cacheMaxSize; i++ {
		cfg := CarveConfig{MaxDepth: i}
		c.put(NodeID("n"), cfg, "fp", &SubGraph{Root: "n"})
	}

	// Access entry 0 (moves it to most-recently-used).
	cfg0 := CarveConfig{MaxDepth: 0}
	if _, ok := c.get("n", cfg0, "fp"); !ok {
		t.Fatal("entry 0 should be present")
	}

	// Insert one more entry — this should evict entry 1 (LRU), NOT entry 0.
	cfgNew := CarveConfig{MaxDepth: cacheMaxSize}
	c.put(NodeID("n"), cfgNew, "fp", &SubGraph{Root: "n"})

	// Entry 0 was promoted by get() so it should survive.
	if _, ok := c.get("n", cfg0, "fp"); !ok {
		t.Fatal("entry 0 was accessed (LRU promoted) but was evicted — LRU broken")
	}

	// Entry 1 was the true LRU and should have been evicted.
	cfg1 := CarveConfig{MaxDepth: 1}
	if _, ok := c.get("n", cfg1, "fp"); ok {
		t.Fatal("entry 1 should have been evicted as LRU")
	}
}

func TestSubgraphCache_ExpiredEntryCleanedFromOrder(t *testing.T) {
	c := newSubgraphCache()
	cfg := CarveConfig{MaxDepth: 1}
	fp := "fpghost"
	c.put("ghost", cfg, fp, &SubGraph{Root: "ghost"})

	// Force expiry.
	c.mu.Lock()
	key := cacheKeyFor("ghost", cfg, fp)
	c.entries[key].expiresAt = time.Now().Add(-1 * time.Second)
	orderLenBefore := len(c.order)
	c.mu.Unlock()

	// get() should detect expiry and clean from both entries AND order.
	_, ok := c.get("ghost", cfg, fp)
	if ok {
		t.Fatal("expired entry should not be returned")
	}

	c.mu.Lock()
	orderLenAfter := len(c.order)
	c.mu.Unlock()

	if orderLenAfter != orderLenBefore-1 {
		t.Fatalf("expired key should be removed from order: before=%d after=%d", orderLenBefore, orderLenAfter)
	}

	// Re-insert same key — should NOT create duplicate in order.
	c.put("ghost", cfg, fp, &SubGraph{Root: "ghost"})

	c.mu.Lock()
	dupeCount := 0
	for _, k := range c.order {
		if k == key {
			dupeCount++
		}
	}
	c.mu.Unlock()

	if dupeCount != 1 {
		t.Fatalf("key should appear exactly once in order after re-insert, got %d", dupeCount)
	}
}

// ── communities.go tests ─────────────────────────────────────────────────────

func TestDetectCommunities_DefaultParams(t *testing.T) {
	g := New("test")
	// Pass 0 values; should use defaults (maxIter=10, minSize=2)
	res, err := g.DetectCommunities(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Algorithm != "label_propagation" {
		t.Fatalf("unexpected algorithm: %s", res.Algorithm)
	}
}

func TestDetectCommunities_TwoClusters(t *testing.T) {
	g := New("test")
	// Cluster 1: A <-> B
	g.AddNode(&Node{ID: "a", Name: "A", Type: NodeFunction, Package: "pkg1"})
	g.AddNode(&Node{ID: "b", Name: "B", Type: NodeFunction, Package: "pkg1"})
	g.AddEdge(&Edge{From: "a", To: "b", Type: EdgeCalls})

	// Cluster 2: C <-> D
	g.AddNode(&Node{ID: "c", Name: "C", Type: NodeFunction, Package: "pkg2"})
	g.AddNode(&Node{ID: "d", Name: "D", Type: NodeFunction, Package: "pkg2"})
	g.AddEdge(&Edge{From: "c", To: "d", Type: EdgeCalls})

	res, err := g.DetectCommunities(10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.CommunityCount < 2 {
		t.Fatalf("expected at least 2 communities, got %d", res.CommunityCount)
	}
}

func TestDetectCommunities_MixedPackage(t *testing.T) {
	g := New("test")
	g.AddNode(&Node{ID: "a", Name: "A", Type: NodeFunction, Package: "pkg1"})
	g.AddNode(&Node{ID: "b", Name: "B", Type: NodeFunction, Package: "pkg2"})
	g.AddEdge(&Edge{From: "a", To: "b", Type: EdgeCalls})

	res, err := g.DetectCommunities(10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Communities {
		if c.Size >= 2 && len(c.PackageMix) > 1 {
			if !c.IsMixed {
				t.Fatal("community spanning two packages should be IsMixed=true")
			}
		}
	}
}

func TestComputeModularity_NoEdges(t *testing.T) {
	nodes := []*Node{{ID: "a"}}
	var edges []*Edge
	idx := map[NodeID]int{"a": 0}
	labels := []int{0}
	q := computeModularity(nodes, edges, idx, labels)
	if q != 0 {
		t.Fatalf("expected 0 modularity with no edges, got %f", q)
	}
}

func TestComputeModularity_NoNodes(t *testing.T) {
	q := computeModularity(nil, nil, nil, nil)
	if q != 0 {
		t.Fatalf("expected 0, got %f", q)
	}
}

// ── export.go tests ──────────────────────────────────────────────────────────

func TestExportDOT_SkipsEdgesOutsideNodeSet(t *testing.T) {
	nodes := []*Node{
		{ID: "a", Name: "A", Type: NodeFunction, File: "a.go"},
	}
	edges := []*Edge{
		{From: "a", To: "missing", Type: EdgeCalls},
	}
	dot := ExportDOT(nodes, edges, "", false)
	if strings.Contains(dot, "->") {
		t.Fatal("edge to missing node should be excluded")
	}
}

func TestExportDOT_RepoRootStripping(t *testing.T) {
	nodes := []*Node{{ID: "a", Name: "A", Type: NodeFunction, File: "/repo/src/a.go"}}
	dot := ExportDOT(nodes, nil, "/repo", false)
	if strings.Contains(dot, "/repo/") {
		t.Fatal("repo root should be stripped from tooltip")
	}
	if !strings.Contains(dot, "src/a.go") {
		t.Fatal("relative path should appear in tooltip")
	}
}

func TestExportMermaid_ClassDefs(t *testing.T) {
	nodes := []*Node{
		{ID: "f", Name: "F", Type: NodeFunction},
		{ID: "s", Name: "S", Type: NodeStruct},
		{ID: "i", Name: "I", Type: NodeInterface},
		{ID: "v", Name: "V", Type: NodeVariable},
	}
	out := ExportMermaid(nodes, nil, "", false)
	for _, cls := range []string{"funcStyle", "structStyle", "ifaceStyle", "varStyle"} {
		if !strings.Contains(out, cls) {
			t.Errorf("missing classDef for %s", cls)
		}
	}
}

func TestExportMermaid_IncludeMeta(t *testing.T) {
	nodes := []*Node{
		{ID: "f", Name: "F", Type: NodeFunction, Metadata: map[string]string{"signature": "func F()"}},
	}
	out := ExportMermaid(nodes, nil, "", true)
	if !strings.Contains(out, "func F()") {
		t.Fatal("signature should appear in label when includeMeta=true")
	}
}

func TestExportMermaid_LongSigOmitted(t *testing.T) {
	long := strings.Repeat("x", 50)
	nodes := []*Node{
		{ID: "f", Name: "F", Type: NodeFunction, Metadata: map[string]string{"signature": long}},
	}
	out := ExportMermaid(nodes, nil, "", true)
	if strings.Contains(out, long) {
		t.Fatal("signatures >40 chars should be omitted in mermaid")
	}
}

func TestExportGraphML_ValidXML(t *testing.T) {
	nodes := []*Node{{ID: "a", Name: "A", Type: NodeFunction, Package: "pkg", File: "a.go", Line: 10}}
	edges := []*Edge{{From: "a", To: "a", Type: EdgeCalls}}
	out := ExportGraphML(nodes, edges, "")
	if !strings.Contains(out, "<?xml") {
		t.Fatal("should contain XML header")
	}
	if !strings.Contains(out, "graphml") {
		t.Fatal("should contain graphml tag")
	}
}

func TestExportGraphML_RepoRootStripping(t *testing.T) {
	nodes := []*Node{{ID: "a", Name: "A", Type: NodeFunction, File: "/root/a.go"}}
	out := ExportGraphML(nodes, nil, "/root")
	if strings.Contains(out, "/root/") {
		t.Fatal("repo root should be stripped")
	}
}

func TestDotHelpers(t *testing.T) {
	tests := []struct {
		name string
		fn   func() string
		want string
	}{
		{"dotNodeID special chars", func() string { return dotNodeID("foo/bar::baz") }, "nfoo_bar__baz"},
		{"dotEscape newline", func() string { return dotEscape("a\nb") }, `a\nb`},
		{"dotShape function", func() string { return dotShape(NodeFunction) }, "ellipse"},
		{"dotShape struct", func() string { return dotShape(NodeStruct) }, "box"},
		{"dotShape interface", func() string { return dotShape(NodeInterface) }, "diamond"},
		{"dotShape variable", func() string { return dotShape(NodeVariable) }, "note"},
		{"dotShape default", func() string { return dotShape("unknown") }, "box"},
		{"dotNodeColor function", func() string { return dotNodeColor(NodeFunction) }, "navy"},
		{"dotNodeColor method", func() string { return dotNodeColor(NodeMethod) }, "blue"},
		{"dotNodeColor default", func() string { return dotNodeColor("unknown") }, "black"},
		{"dotEdgeColor calls", func() string { return dotEdgeColor(EdgeCalls) }, "navy"},
		{"dotEdgeColor implements", func() string { return dotEdgeColor(EdgeImplements) }, "purple"},
		{"dotEdgeColor embeds", func() string { return dotEdgeColor(EdgeEmbeds) }, "darkgreen"},
		{"dotEdgeColor imports", func() string { return dotEdgeColor(EdgeImports) }, "gray"},
		{"dotEdgeColor depends", func() string { return dotEdgeColor(EdgeDependsOn) }, "orange"},
		{"dotEdgeColor default", func() string { return dotEdgeColor("X") }, "black"},
		{"mermaidClass function", func() string { return mermaidClass(NodeFunction) }, "funcStyle"},
		{"mermaidClass struct", func() string { return mermaidClass(NodeStruct) }, "structStyle"},
		{"mermaidClass interface", func() string { return mermaidClass(NodeInterface) }, "ifaceStyle"},
		{"mermaidClass variable", func() string { return mermaidClass(NodeVariable) }, "varStyle"},
		{"mermaidClass default", func() string { return mermaidClass("unknown") }, "funcStyle"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fn(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ── serialize.go tests ───────────────────────────────────────────────────────

func TestFlatGraph_SerializeDeserialize_RoundTrip(t *testing.T) {
	fg := NewFlatGraph("test-repo")
	n1 := fg.AddNode(Pool.Intern("Foo"), NodeFunction, Pool.Intern("foo.go"), 0)
	n2 := fg.AddNode(Pool.Intern("Bar"), NodeFunction, Pool.Intern("bar.go"), 0)
	fg.AddEdge(n1, n2, 1.0)

	var buf bytes.Buffer
	if err := fg.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	fg2, err := Deserialize(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if fg2.RepoID != "test-repo" {
		t.Fatalf("expected repo test-repo, got %s", fg2.RepoID)
	}
}

func TestDeserializeMapped_Works(t *testing.T) {
	fg := NewFlatGraph("mapped")
	fg.AddNode(Pool.Intern("X"), NodeFunction, Pool.Intern("x.go"), 0)

	var buf bytes.Buffer
	if err := fg.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	fg2, err := DeserializeMapped(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if fg2.RepoID != "mapped" {
		t.Fatalf("unexpected repoID: %s", fg2.RepoID)
	}
}

func TestSerialize_EmptyFlatGraph(t *testing.T) {
	fg := NewFlatGraph("empty")
	var buf bytes.Buffer
	if err := fg.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	fg2, err := Deserialize(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if fg2.RepoID != "empty" {
		t.Fatal("empty graph round-trip failed")
	}
}

func TestWriteReadString(t *testing.T) {
	tests := []string{"", "hello", "longer string with spaces"}
	for _, s := range tests {
		var buf bytes.Buffer
		if err := writeString(&buf, s); err != nil {
			t.Fatal(err)
		}
		got, err := readString(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got != s {
			t.Errorf("writeString/readString round-trip: got %q, want %q", got, s)
		}
	}
}

// ── intern.go tests ──────────────────────────────────────────────────────────

func TestStringPool_ValueOutOfBounds(t *testing.T) {
	p := NewStringPool()
	// ID way beyond what's interned should return ""
	got := p.Value(StringID(999999))
	if got != "" {
		t.Fatalf("expected empty string for out-of-bounds ID, got %q", got)
	}
}

func TestStringPool_GhostWraparound(t *testing.T) {
	p := NewStringPool()
	// Intern enough ghosts to wrap around
	for i := 0; i < ReservedGhostRange+10; i++ {
		p.GhostIntern("ghost")
	}
	// Should not panic; ghostNext should wrap
	if p.ghostNext >= ReservedGhostRange {
		t.Fatalf("ghostNext should have wrapped, got %d", p.ghostNext)
	}
}

func TestStringPool_ConcurrentIntern(t *testing.T) {
	p := NewStringPool()
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				id := p.Intern("shared-string")
				v := p.Value(id)
				if v != "shared-string" {
					t.Errorf("concurrent Value mismatch: %q", v)
				}
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

// ── index_snapshot.go tests ──────────────────────────────────────────────────

func TestSaveLoadSnapshot_RoundTrip(t *testing.T) {
	pool := NewStringPool()
	idx := newGraphIndex(pool)

	// Add a couple of nodes
	nid1 := NodeID("test::file.go::Foo")
	nid2 := NodeID("test::file.go::Bar")
	typeID := pool.Intern("function")
	name1 := pool.Intern("Foo")
	name2 := pool.Intern("Bar")
	fileID := pool.Intern("file.go")
	pkgID := pool.Intern("pkg")

	idx.SeqIDs = append(idx.SeqIDs, nid1, nid2)
	idx.Types = append(idx.Types, typeID, typeID)
	idx.Names = append(idx.Names, name1, name2)
	idx.FileIDs = append(idx.FileIDs, fileID, fileID)
	idx.PkgIDs = append(idx.PkgIDs, pkgID, pkgID)
	idx.Lines = append(idx.Lines, 10, 20)
	idx.Exported = append(idx.Exported, true, false)
	idx.Tombstone = append(idx.Tombstone, false, false)
	idx.IDToSeq[nid1] = 1
	idx.IDToSeq[nid2] = 2

	// CSR arrays
	idx.OutStart = make([]uint32, 3)
	idx.OutEnd = make([]uint32, 3)
	idx.OutStart[1] = 0
	idx.OutEnd[1] = 1
	idx.OutTargets = []uint32{2}
	idx.OutTypes = []StringID{typeID}

	idx.InStart = make([]uint32, 3)
	idx.InEnd = make([]uint32, 3)
	idx.InStart[2] = 0
	idx.InEnd[2] = 1
	idx.InTargets = []uint32{1}
	idx.InTypes = []StringID{typeID}

	data, err := idx.SaveSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	pool2 := NewStringPool()
	idx2, err := LoadSnapshot(data, pool2)
	if err != nil {
		t.Fatal(err)
	}

	if len(idx2.SeqIDs) != len(idx.SeqIDs) {
		t.Fatalf("node count mismatch: %d vs %d", len(idx2.SeqIDs), len(idx.SeqIDs))
	}
	if idx2.SeqIDs[1] != nid1 {
		t.Fatalf("node ID mismatch at 1: got %s", idx2.SeqIDs[1])
	}
}

func TestLoadSnapshot_InvalidMagic(t *testing.T) {
	_, err := LoadSnapshot([]byte("not valid data at all"), NewStringPool())
	if err == nil {
		t.Fatal("expected error for invalid data")
	}
}
