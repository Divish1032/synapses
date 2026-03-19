package mcp

import (
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/store"
)

// mockProjectRegistry implements ProjectStoreProvider for testing.
type mockProjectRegistry struct {
	stores map[string]*store.Store
}

func (m *mockProjectRegistry) ListProjects() []string {
	names := make([]string, 0, len(m.stores))
	for name := range m.stores {
		names = append(names, name)
	}
	return names
}

func (m *mockProjectRegistry) GetStore(name string) *store.Store {
	return m.stores[name]
}

func TestRecall_CrossProject(t *testing.T) {
	s := newTestServer(t)

	// Create sibling store.
	siblingStore := openTestStore(t)

	// Add an episode to the sibling store.
	siblingStore.RememberEpisode(store.Episode{
		AgentID:     "sibling-agent",
		Decision:    "shipped login feature",
		Rationale:   "completed auth flow",
		EpisodeType: "decision",
		Outcome:     "success",
		Trigger:     "building auth",
		CreatedAt:   time.Now().Unix(),
	})

	// Wire the registry.
	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"sibling-project": siblingStore,
			"main-project":    s.store,
		},
	})
	s.projectPath = "/test/main-project"

	// Recall with projects="*".
	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":    "login",
		"projects": "*",
	}))
	m := mustResult(t, res, err)

	// Should have cross-project results.
	crossEps, ok := m["cross_project_episodes"]
	if !ok {
		t.Fatal("expected cross_project_episodes in response")
	}
	eps, ok := crossEps.([]interface{})
	if !ok || len(eps) == 0 {
		t.Error("expected at least one cross-project episode")
	}
}

func TestRecall_CrossProject_SpecificName(t *testing.T) {
	s := newTestServer(t)

	siblingStore := openTestStore(t)
	siblingStore.RememberEpisode(store.Episode{
		AgentID:     "agent-1",
		Decision:    "fixed database migration bug",
		EpisodeType: "failure",
		Outcome:     "failure",
		CreatedAt:   time.Now().Unix(),
	})

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"backend":      siblingStore,
			"main-project": s.store,
		},
	})
	s.projectPath = "/test/main-project"

	// Query specific project by name.
	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":    "database",
		"projects": "backend",
	}))
	m := mustResult(t, res, err)

	crossEps, ok := m["cross_project_episodes"]
	if !ok {
		t.Fatal("expected cross_project_episodes in response")
	}
	eps, ok := crossEps.([]interface{})
	if !ok || len(eps) == 0 {
		t.Error("expected at least one cross-project episode from backend")
	}
}

func TestGetEvents_CrossProject(t *testing.T) {
	s := newTestServer(t)

	siblingStore := openTestStore(t)
	_ = siblingStore.AppendEvent("file_change", "agent-1", `{"file":"main.go"}`)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"sibling": siblingStore,
			"self":    s.store,
		},
	})
	s.projectPath = "/test/self"

	res, err := s.handleGetEvents(ctx, callTool(map[string]any{
		"since_seq": 0,
		"projects":  "*",
	}))
	m := mustResult(t, res, err)

	crossEvents, ok := m["cross_project_events"]
	if !ok {
		t.Fatal("expected cross_project_events in response")
	}
	events, ok := crossEvents.([]interface{})
	if !ok || len(events) == 0 {
		t.Error("expected at least one cross-project event")
	}
}

func TestGetAgents_CrossProject(t *testing.T) {
	s := newTestServer(t)

	siblingStore := openTestStore(t)
	siblingStore.UpsertAgent("remote-agent", nil)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"sibling": siblingStore,
			"self":    s.store,
		},
	})
	s.projectPath = "/test/self"

	res, err := s.handleGetAgents(ctx, callTool(map[string]any{
		"projects": "*",
	}))
	m := mustResult(t, res, err)

	crossAgents, ok := m["cross_project_agents"]
	if !ok {
		t.Fatal("expected cross_project_agents in response")
	}
	agents, ok := crossAgents.([]interface{})
	if !ok || len(agents) == 0 {
		t.Error("expected at least one cross-project agent")
	}
}

func TestGetMessages_CrossProject(t *testing.T) {
	s := newTestServer(t)

	siblingStore := openTestStore(t)
	siblingStore.SendMessage("agent-a", "agent-b", "api_changed", `{"endpoint":"/users"}`, "sibling")

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"sibling": siblingStore,
			"self":    s.store,
		},
	})
	s.projectPath = "/test/self"

	res, err := s.handleGetMessages(ctx, callTool(map[string]any{
		"agent_id":   "agent-b",
		"projects":   "*",
		"unread_only": false,
	}))
	m := mustResult(t, res, err)

	crossMsgs, ok := m["cross_project_messages"]
	if !ok {
		t.Fatal("expected cross_project_messages in response")
	}
	msgs, ok := crossMsgs.([]interface{})
	if !ok || len(msgs) == 0 {
		t.Error("expected at least one cross-project message")
	}
}

func TestSessionInit_CrossProjectStatus(t *testing.T) {
	s := newTestServer(t)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"project-a": s.store,
			"project-b": s.store,
		},
	})

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)

	status, ok := m["cross_project_status"]
	if !ok {
		t.Fatal("expected cross_project_status in session_init response")
	}
	statusMap, ok := status.(map[string]interface{})
	if !ok {
		t.Fatal("cross_project_status should be a map")
	}
	count, ok := statusMap["registered_projects"].(float64)
	if !ok || count < 2 {
		t.Errorf("expected registered_projects >= 2, got %v", statusMap["registered_projects"])
	}
}

func TestResolveProjectStores_ExcludesSelf(t *testing.T) {
	s := newTestServer(t)

	selfStore := openTestStore(t)
	siblingStore := openTestStore(t)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"myproject": selfStore,
			"sibling":   siblingStore,
		},
	})
	s.projectPath = "/test/myproject"

	result := s.resolveProjectStores("*")
	if _, ok := result["myproject"]; ok {
		t.Error("resolveProjectStores should exclude the current project")
	}
	if _, ok := result["sibling"]; !ok {
		t.Error("resolveProjectStores should include sibling projects")
	}
}

func TestResolveProjectStores_NilRegistry(t *testing.T) {
	s := newTestServer(t)
	result := s.resolveProjectStores("*")
	if result != nil {
		t.Error("expected nil when registry is nil")
	}
}

func TestResolveProjectStores_EmptyParam(t *testing.T) {
	s := newTestServer(t)
	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{"a": s.store},
	})
	result := s.resolveProjectStores("")
	if result != nil {
		t.Error("expected nil when param is empty")
	}
}
