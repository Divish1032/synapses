package store_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetMemoriesByIDs_EmptySlice(t *testing.T) {
	st := openTestStore(t)
	mems, err := st.GetMemoriesByIDs(nil)
	require.NoError(t, err)
	assert.Nil(t, mems)
}

func TestGetMemoriesByIDs_ReturnsFullStructs(t *testing.T) {
	st := openTestStore(t)

	id1, err := st.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "auth service handles JWT tokens",
		AgentID: "agent-1",
		Source:  store.SourceManual,
		Tags:    `["auth"]`,
	})
	require.NoError(t, err)

	id2, err := st.InsertMemory(store.Memory{
		Tier:     store.TierEntity,
		Content:  "TokenValidator switched to RS256",
		EntityID: "repo::pkg/auth.go::TokenValidator",
		AgentID:  "agent-2",
		Source:   store.SourceManual,
	})
	require.NoError(t, err)

	mems, err := st.GetMemoriesByIDs([]string{id1, id2})
	require.NoError(t, err)
	require.Len(t, mems, 2)

	// Verify full fields are populated (not just ID/Content/Tier).
	byID := make(map[string]store.Memory)
	for _, m := range mems {
		byID[m.ID] = m
	}

	m1 := byID[id1]
	assert.Equal(t, store.TierProject, m1.Tier)
	assert.Equal(t, "auth service handles JWT tokens", m1.Content)
	assert.Equal(t, "agent-1", m1.AgentID)
	assert.Equal(t, store.SourceManual, m1.Source)
	assert.NotEmpty(t, m1.CreatedAt, "CreatedAt should be populated")
	assert.Equal(t, `["auth"]`, m1.Tags)

	m2 := byID[id2]
	assert.Equal(t, store.TierEntity, m2.Tier)
	assert.Equal(t, "repo::pkg/auth.go::TokenValidator", m2.EntityID)
	assert.Equal(t, "agent-2", m2.AgentID)
	assert.NotEmpty(t, m2.CreatedAt, "CreatedAt should be populated")
}

func TestGetMemoriesByIDs_MissingIDsSkipped(t *testing.T) {
	st := openTestStore(t)

	id1, err := st.InsertMemory(store.Memory{
		Tier:    store.TierProject,
		Content: "existing memory content here",
		AgentID: "test",
		Source:  store.SourceManual,
	})
	require.NoError(t, err)

	mems, err := st.GetMemoriesByIDs([]string{id1, "nonexistent-id-xyz"})
	require.NoError(t, err)
	assert.Len(t, mems, 1)
	assert.Equal(t, id1, mems[0].ID)
}
