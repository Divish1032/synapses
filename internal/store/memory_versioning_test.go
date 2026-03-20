package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Sprint 10.1: Memory Versioning Tests ────────────────────────────────────

func TestCreateMemoryVersion_BasicRoundTrip(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "auth service uses JWT tokens for session management",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	// Snapshot old content with known activeFrom.
	activeFrom := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	ver, err := st.CreateMemoryVersion(id, "auth service uses JWT tokens for session management", activeFrom)
	require.NoError(t, err)
	assert.Equal(t, 1, ver)

	versions, err := st.GetMemoryVersions(id)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, id, versions[0].MemoryID)
	assert.Equal(t, 1, versions[0].Version)
	assert.Equal(t, "auth service uses JWT tokens for session management", versions[0].Content)
	assert.Equal(t, id, versions[0].SupersededBy)
	assert.Equal(t, activeFrom, versions[0].CreatedAt, "version created_at should be activeFrom")
	assert.NotEmpty(t, versions[0].SupersededAt, "superseded_at should be set")
}

func TestCreateMemoryVersion_MultipleVersions(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "database uses PostgreSQL version 16 for primary storage",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	t0 := time.Now().UTC().Add(-20 * time.Second).Format(time.RFC3339)
	t1 := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)

	v1, err := st.CreateMemoryVersion(id, "database uses PostgreSQL version 14 for primary storage", t0)
	require.NoError(t, err)
	assert.Equal(t, 1, v1)

	v2, err := st.CreateMemoryVersion(id, "database uses PostgreSQL version 15 for primary storage", t1)
	require.NoError(t, err)
	assert.Equal(t, 2, v2)

	versions, err := st.GetMemoryVersions(id)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, 1, versions[0].Version)
	assert.Equal(t, 2, versions[1].Version)
	assert.Contains(t, versions[0].Content, "version 14")
	assert.Contains(t, versions[1].Content, "version 15")
	assert.Equal(t, t0, versions[0].CreatedAt)
	assert.Equal(t, t1, versions[1].CreatedAt)
}

func TestGetMemoryVersionCount(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "auth mechanism uses OAuth2 for external identity providers",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	count, err := st.GetMemoryVersionCount(id)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = st.CreateMemoryVersion(id, "old content", now)
	require.NoError(t, err)

	count, err = st.GetMemoryVersionCount(id)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestInsertMemory_DedupCreatesVersionAndUpdatesContent(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert original memory.
	id1, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the authentication service handles user login via JWT",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	// Insert similar but different memory — should dedup, version old, update content.
	id2, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the authentication service handles user login via JWT tokens",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)
	assert.Equal(t, id1, id2, "should dedup to same memory")

	// Verify a version was created with the OLD content.
	versions, err := st.GetMemoryVersions(id1)
	require.NoError(t, err)
	require.Len(t, versions, 1, "dedup should have created one version snapshot")
	assert.Equal(t, "the authentication service handles user login via JWT", versions[0].Content,
		"version should contain the OLD content before dedup")

	// Verify the LIVE memory now has the NEW content.
	mems, err := st.GetMemoriesByIDs([]string{id1})
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Equal(t, "the authentication service handles user login via JWT tokens", mems[0].Content,
		"live memory should be updated to new content")
}

func TestInsertMemoryWithAnchors_DedupCreatesVersionAndUpdatesContent(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id1, err := st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "TokenValidator uses RS256 algorithm for JWT signature verification",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::pkg/auth.go::TokenValidator"})
	require.NoError(t, err)

	id2, err := st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "TokenValidator uses RS256 algorithm for JWT token signature verification",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::pkg/auth.go::TokenValidator"})
	require.NoError(t, err)
	assert.Equal(t, id1, id2)

	// Version has the OLD content.
	versions, err := st.GetMemoryVersions(id1)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, "TokenValidator uses RS256 algorithm for JWT signature verification", versions[0].Content)

	// Live memory has the NEW content.
	mems, err := st.GetMemoriesByIDs([]string{id1})
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Contains(t, mems[0].Content, "JWT token signature verification")
}

func TestInsertMemory_IdenticalContentNoVersion(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Insert a memory.
	id1, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the cache layer uses Redis for session storage management",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	// Insert IDENTICAL memory — dedup should NOT create a version (content unchanged).
	id2, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the cache layer uses Redis for session storage management",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)
	assert.Equal(t, id1, id2)

	// No version should exist — content didn't change.
	versions, err := st.GetMemoryVersions(id1)
	require.NoError(t, err)
	assert.Empty(t, versions, "identical content should not create a version")
}

func TestGetMemoryAsOf_NoVersions(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "project uses Go 1.22 for the main backend service",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	mems, err := st.GetMemoryAsOf([]string{id}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Contains(t, mems[0].Content, "Go 1.22")
}

func TestGetMemoryAsOf_BeforeCreation(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	pastTime := time.Now().Add(-24 * time.Hour)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "project uses Go 1.22 for the main backend service",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	mems, err := st.GetMemoryAsOf([]string{id}, pastTime)
	require.NoError(t, err)
	assert.Empty(t, mems, "memory should not appear for as_of before creation")
}

func TestGetMemoryAsOf_ReturnsHistoricalVersion(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Memory created at T0 = -20s with content "v1 content".
	t0 := time.Now().UTC().Add(-20 * time.Second)
	id, err := st.InsertMemory(Memory{
		Tier:      TierProject,
		Content:   "database pool uses maximum of ten concurrent connections",
		AgentID:   "agent-1",
		Source:    SourceManual,
		CreatedAt: t0.Format(time.RFC3339),
	})
	require.NoError(t, err)

	// Create version: old content was active from T0, superseded at ~now.
	_, err = st.CreateMemoryVersion(id, "database pool uses maximum of ten concurrent connections", t0.Format(time.RFC3339))
	require.NoError(t, err)

	// Query as_of = T0 + 5s (between creation and supersession).
	queryTime := t0.Add(5 * time.Second)
	mems, err := st.GetMemoryAsOf([]string{id}, queryTime)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Contains(t, mems[0].Content, "ten concurrent connections")
	assert.Equal(t, 1, mems[0].Version)
}

func TestGetMemoryAsOf_MultiVersionCorrectSelection(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Memory created at T0 with content "A".
	t0 := time.Now().UTC().Add(-30 * time.Second)
	id, err := st.InsertMemory(Memory{
		Tier:      TierProject,
		Content:   "current content C for testing multi-version temporal queries",
		AgentID:   "agent-1",
		Source:    SourceManual,
		CreatedAt: t0.Format(time.RFC3339),
	})
	require.NoError(t, err)

	// Version 1: content "A" was active from T0 to T1.
	t1 := time.Now().UTC().Add(-20 * time.Second)
	_, err = st.knowledgeDB.Exec(`
		INSERT INTO memory_versions (id, memory_id, version, content, superseded_by, created_at, superseded_at)
		VALUES (?, ?, 1, ?, ?, ?, ?)`,
		"v1-id", id, "content A for testing multi-version temporal queries", id,
		t0.Format(time.RFC3339), t1.Format(time.RFC3339))
	require.NoError(t, err)

	// Version 2: content "B" was active from T1 to T2.
	t2 := time.Now().UTC().Add(-10 * time.Second)
	_, err = st.knowledgeDB.Exec(`
		INSERT INTO memory_versions (id, memory_id, version, content, superseded_by, created_at, superseded_at)
		VALUES (?, ?, 2, ?, ?, ?, ?)`,
		"v2-id", id, "content B for testing multi-version temporal queries", id,
		t1.Format(time.RFC3339), t2.Format(time.RFC3339))
	require.NoError(t, err)

	// Query at T0 + 5s → should get version 1 (content "A").
	mems, err := st.GetMemoryAsOf([]string{id}, t0.Add(5*time.Second))
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Contains(t, mems[0].Content, "content A")
	assert.Equal(t, 1, mems[0].Version)

	// Query at T1 + 5s → should get version 2 (content "B").
	mems, err = st.GetMemoryAsOf([]string{id}, t1.Add(5*time.Second))
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Contains(t, mems[0].Content, "content B")
	assert.Equal(t, 2, mems[0].Version)

	// Query at T2 + 5s → no version covers this time, should get current "C".
	mems, err = st.GetMemoryAsOf([]string{id}, t2.Add(5*time.Second))
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Contains(t, mems[0].Content, "current content C")
	assert.Equal(t, 0, mems[0].Version, "current content has version 0")
}

func TestGetMemoryAsOf_EmptyIDs(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)
	mems, err := st.GetMemoryAsOf(nil, time.Now())
	require.NoError(t, err)
	assert.Nil(t, mems)
}

func TestExpireMemories_CascadesVersions(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:      TierProject,
		Content:   "temporary memory that will expire soon for testing purposes",
		AgentID:   "agent-1",
		Source:    SourceManual,
		ExpiresAt: time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339),
	})
	require.NoError(t, err)

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = st.CreateMemoryVersion(id, "old content for version snapshot testing", now)
	require.NoError(t, err)

	versions, err := st.GetMemoryVersions(id)
	require.NoError(t, err)
	require.Len(t, versions, 1)

	count, err := st.ExpireMemories()
	require.NoError(t, err)
	assert.Greater(t, count, int64(0))

	versions, err = st.GetMemoryVersions(id)
	require.NoError(t, err)
	assert.Empty(t, versions, "versions should be cascade-deleted on expire")
}

func TestUpdateMemoryContent(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "original content for testing the update memory content function",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	err = st.UpdateMemoryContent(id, "updated content for testing the update memory content function")
	require.NoError(t, err)

	mems, err := st.GetMemoriesByIDs([]string{id})
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Equal(t, "updated content for testing the update memory content function", mems[0].Content)
}

func TestVersionCap_PrunesOldest(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "memory for testing version cap pruning at maximum versions",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	now := time.Now().UTC().Format(time.RFC3339)
	// Create maxVersionsPerMemory + 5 versions.
	for i := 0; i < maxVersionsPerMemory+5; i++ {
		_, err := st.CreateMemoryVersion(id, "version content for cap test", now)
		require.NoError(t, err)
	}

	// Should be capped at maxVersionsPerMemory.
	count, err := st.GetMemoryVersionCount(id)
	require.NoError(t, err)
	assert.LessOrEqual(t, count, maxVersionsPerMemory, "versions should be capped")
}

func TestDedupChain_ThreeWrites(t *testing.T) {
	t.Parallel()
	st := openMemTestStore(t)

	// Write 1: original.
	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the API gateway routes requests to the backend microservices",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	// Write 2: similar enough to dedup, but different content.
	id2, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the API gateway routes requests to backend microservices cluster",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)
	assert.Equal(t, id, id2)

	// Write 3: another similar dedup.
	id3, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the API gateway routes all requests to backend microservices cluster",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)
	assert.Equal(t, id, id3)

	// Should have 2 versions (write 1 content + write 2 content).
	versions, err := st.GetMemoryVersions(id)
	require.NoError(t, err)
	require.Len(t, versions, 2, "three writes with two content changes = two versions")

	// Version 1 = original content.
	assert.Contains(t, versions[0].Content, "routes requests to the backend microservices")
	// Version 2 = second write's content.
	assert.Contains(t, versions[1].Content, "routes requests to backend microservices cluster")

	// Live memory = third write's content.
	mems, err := st.GetMemoriesByIDs([]string{id})
	require.NoError(t, err)
	assert.Contains(t, mems[0].Content, "routes all requests to backend microservices cluster")
}
