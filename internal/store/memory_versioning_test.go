package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Sprint 10.1: Memory Versioning Tests ────────────────────────────────────

func TestCreateMemoryVersion_BasicRoundTrip(t *testing.T) {
	st := openMemTestStore(t)

	// Insert a memory.
	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "auth service uses JWT tokens for session management",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	// Create a version snapshot.
	ver, err := st.CreateMemoryVersion(id, "auth service uses JWT tokens for session management")
	require.NoError(t, err)
	assert.Equal(t, 1, ver)

	// Retrieve versions.
	versions, err := st.GetMemoryVersions(id)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, id, versions[0].MemoryID)
	assert.Equal(t, 1, versions[0].Version)
	assert.Equal(t, "auth service uses JWT tokens for session management", versions[0].Content)
	assert.Equal(t, id, versions[0].SupersededBy) // points to current memory
}

func TestCreateMemoryVersion_MultipleVersions(t *testing.T) {
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "database uses PostgreSQL version 14 for primary storage",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	// Create version 1.
	v1, err := st.CreateMemoryVersion(id, "database uses PostgreSQL version 14 for primary storage")
	require.NoError(t, err)
	assert.Equal(t, 1, v1)

	// Create version 2.
	v2, err := st.CreateMemoryVersion(id, "database uses PostgreSQL version 15 for primary storage")
	require.NoError(t, err)
	assert.Equal(t, 2, v2)

	// Retrieve — should get both in order.
	versions, err := st.GetMemoryVersions(id)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.Equal(t, 1, versions[0].Version)
	assert.Equal(t, 2, versions[1].Version)
	assert.Contains(t, versions[0].Content, "version 14")
	assert.Contains(t, versions[1].Content, "version 15")
}

func TestGetMemoryVersionCount(t *testing.T) {
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

	_, err = st.CreateMemoryVersion(id, "old content")
	require.NoError(t, err)

	count, err = st.GetMemoryVersionCount(id)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestInsertMemory_DedupCreatesVersion(t *testing.T) {
	st := openMemTestStore(t)

	// Insert original memory.
	id1, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the authentication service handles user login via JWT",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	// Insert similar memory — should dedup and create a version.
	id2, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "the authentication service handles user login via JWT tokens",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)
	assert.Equal(t, id1, id2, "should dedup to same memory")

	// Verify a version was created.
	versions, err := st.GetMemoryVersions(id1)
	require.NoError(t, err)
	require.Len(t, versions, 1, "dedup should have created one version snapshot")
	assert.Contains(t, versions[0].Content, "the authentication service handles user login via JWT")
}

func TestInsertMemoryWithAnchors_DedupCreatesVersion(t *testing.T) {
	st := openMemTestStore(t)

	// Insert original memory with anchors.
	id1, err := st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "TokenValidator uses RS256 algorithm for JWT signature verification",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::pkg/auth.go::TokenValidator"})
	require.NoError(t, err)

	// Insert similar memory — should dedup.
	id2, err := st.InsertMemoryWithAnchors(Memory{
		Tier:    TierEntity,
		Content: "TokenValidator uses RS256 algorithm for JWT token signature verification",
		AgentID: "agent-1",
		Source:  SourceManual,
	}, []string{"repo::pkg/auth.go::TokenValidator"})
	require.NoError(t, err)
	assert.Equal(t, id1, id2)

	// Verify version was created.
	versions, err := st.GetMemoryVersions(id1)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Contains(t, versions[0].Content, "RS256 algorithm")
}

func TestGetMemoryAsOf_NoVersions(t *testing.T) {
	st := openMemTestStore(t)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "project uses Go 1.22 for the main backend service",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	// Query as_of now — should return current content.
	mems, err := st.GetMemoryAsOf([]string{id}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Contains(t, mems[0].Content, "Go 1.22")
}

func TestGetMemoryAsOf_BeforeCreation(t *testing.T) {
	st := openMemTestStore(t)

	pastTime := time.Now().Add(-24 * time.Hour)

	id, err := st.InsertMemory(Memory{
		Tier:    TierProject,
		Content: "project uses Go 1.22 for the main backend service",
		AgentID: "agent-1",
		Source:  SourceManual,
	})
	require.NoError(t, err)

	// Query before memory was created — should exclude it.
	mems, err := st.GetMemoryAsOf([]string{id}, pastTime)
	require.NoError(t, err)
	assert.Empty(t, mems, "memory should not appear for as_of before creation")
}

func TestGetMemoryAsOf_ReturnsHistoricalVersion(t *testing.T) {
	st := openMemTestStore(t)

	// Insert original memory with an explicit created_at in the past.
	pastCreation := time.Now().UTC().Add(-10 * time.Second)
	id, err := st.InsertMemory(Memory{
		Tier:      TierProject,
		Content:   "database connection pool uses maximum of ten concurrent connections",
		AgentID:   "agent-1",
		Source:    SourceManual,
		CreatedAt: pastCreation.Format(time.RFC3339),
	})
	require.NoError(t, err)

	// afterV1 is between creation (-10s) and now.
	afterV1 := time.Now().UTC().Add(-5 * time.Second)

	// Create a version snapshot. Its superseded_at = now() which is > afterV1.
	_, err = st.CreateMemoryVersion(id, "database connection pool uses maximum of ten concurrent connections")
	require.NoError(t, err)

	// Query as_of at afterV1 — memory existed (created -10s ago), and the
	// version's superseded_at (now) > afterV1 (-5s ago), so the version was
	// the active content at that time.
	mems, err := st.GetMemoryAsOf([]string{id}, afterV1)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Contains(t, mems[0].Content, "ten concurrent connections")
	assert.Equal(t, 1, mems[0].Version, "should show version 1")
}

func TestGetMemoryAsOf_EmptyIDs(t *testing.T) {
	st := openMemTestStore(t)
	mems, err := st.GetMemoryAsOf(nil, time.Now())
	require.NoError(t, err)
	assert.Nil(t, mems)
}

func TestExpireMemories_CascadesVersions(t *testing.T) {
	st := openMemTestStore(t)

	// Insert a memory with a very short TTL.
	id, err := st.InsertMemory(Memory{
		Tier:      TierProject,
		Content:   "temporary memory that will expire soon for testing purposes",
		AgentID:   "agent-1",
		Source:    SourceManual,
		ExpiresAt: time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339), // already expired
	})
	require.NoError(t, err)

	// Create a version.
	_, err = st.CreateMemoryVersion(id, "old content for version snapshot testing")
	require.NoError(t, err)

	// Verify version exists.
	versions, err := st.GetMemoryVersions(id)
	require.NoError(t, err)
	require.Len(t, versions, 1)

	// Expire.
	count, err := st.ExpireMemories()
	require.NoError(t, err)
	assert.Greater(t, count, int64(0))

	// Versions should also be gone.
	versions, err = st.GetMemoryVersions(id)
	require.NoError(t, err)
	assert.Empty(t, versions, "versions should be cascade-deleted on expire")
}
