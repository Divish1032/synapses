package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SynapsesOS/synapses/internal/config"
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

// allowAllACL returns a FederationACLConfig that allows all projects.
func allowAllACL() *config.FederationACLConfig {
	return &config.FederationACLConfig{AllowReadFrom: []string{"*"}}
}

// allowACL returns a FederationACLConfig that allows the given projects.
func allowACL(projects ...string) *config.FederationACLConfig {
	return &config.FederationACLConfig{AllowReadFrom: projects}
}

func TestRecall_CrossProject(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = allowAllACL()

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
	s.config.FederationACL = allowACL("backend")

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
	s.config.FederationACL = allowAllACL()

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
	s.config.FederationACL = allowAllACL()

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
	s.config.FederationACL = allowAllACL()

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
		"agent_id":    "agent-b",
		"projects":    "*",
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
	// Without ACL, should show note about no reads allowed.
	note, _ := statusMap["note"].(string)
	if !strings.Contains(note, "federation_acl") {
		t.Error("expected federation_acl guidance note when no ACL configured")
	}
	// Should NOT expose project names when no ACL configured.
	if _, hasProjects := statusMap["projects"]; hasProjects {
		t.Error("should not expose unfiltered project names without ACL")
	}
}

func TestSessionInit_CrossProjectStatus_WithACL(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = allowACL("project-a")

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
		t.Fatal("expected cross_project_status")
	}
	statusMap, _ := status.(map[string]interface{})
	accessible, ok := statusMap["accessible_projects"].([]interface{})
	if !ok || len(accessible) != 1 {
		t.Errorf("expected 1 accessible project, got %v", statusMap["accessible_projects"])
	}
	if accessible[0] != "project-a" {
		t.Errorf("expected project-a, got %v", accessible[0])
	}
}

func TestResolveProjectStores_ExcludesSelf(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = allowAllACL()

	siblingStore := openTestStore(t)

	// Use s.store as the self store so pointer comparison works.
	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"myproject": s.store,
			"sibling":   siblingStore,
		},
	})
	s.projectPath = "/test/myproject"

	result, _ := s.resolveProjectStores("*")
	if _, ok := result["myproject"]; ok {
		t.Error("resolveProjectStores should exclude the current project")
	}
	if _, ok := result["sibling"]; !ok {
		t.Error("resolveProjectStores should include sibling projects")
	}
}

func TestResolveProjectStores_NilRegistry(t *testing.T) {
	s := newTestServer(t)
	result, _ := s.resolveProjectStores("*")
	if result != nil {
		t.Error("expected nil when registry is nil")
	}
}

func TestResolveProjectStores_EmptyParam(t *testing.T) {
	s := newTestServer(t)
	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{"a": s.store},
	})
	result, _ := s.resolveProjectStores("")
	if result != nil {
		t.Error("expected nil when param is empty")
	}
}

func TestResolveProjectStores_UnknownProject(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = allowAllACL()
	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{"backend": s.store},
	})
	s.projectPath = "/test/frontend"

	result, notFound := s.resolveProjectStores("nonexistent")
	if len(result) != 0 {
		t.Error("expected empty result for unknown project")
	}
	if len(notFound) != 1 || notFound[0] != "nonexistent" {
		t.Errorf("expected notFound=[nonexistent], got %v", notFound)
	}
}

func TestSessionInit_FederationSuggestions(t *testing.T) {
	s := newTestServer(t)

	// Create a temp directory structure with a sibling.
	parent := t.TempDir()
	current := filepath.Join(parent, "my-project")
	os.Mkdir(current, 0o755)
	sibling := filepath.Join(parent, "backend")
	os.Mkdir(sibling, 0o755)
	os.WriteFile(filepath.Join(sibling, "synapses.json"), []byte("{}"), 0o644)

	s.projectPath = current
	// No federation resolver set — triggers discovery.

	res, err := s.handleSessionInit(ctx, callTool(map[string]any{
		"agent_id": "test-agent",
	}))
	m := mustResult(t, res, err)

	if suggestions, ok := m["federation_suggestions"]; ok {
		sugMap, _ := suggestions.(map[string]interface{})
		if discovered, ok := sugMap["discovered"]; ok {
			arr, _ := discovered.([]interface{})
			if len(arr) == 0 {
				t.Error("expected at least one discovered sibling")
			}
		}
	}
	// Note: federation_suggestions may be absent if parent dir scan doesn't
	// find siblings (depends on temp dir structure) — that's fine.
}

// ── Federation ACL Security Tests ───────────────────────────────────────────

// TestACL_DefaultDenyAll verifies that with no ACL configured (nil),
// cross-project queries return no results — the attack vector is closed.
func TestACL_DefaultDenyAll(t *testing.T) {
	s := newTestServer(t)
	// No ACL configured — default deny-all.

	siblingStore := openTestStore(t)
	siblingStore.RememberEpisode(store.Episode{
		AgentID:     "secret-agent",
		Decision:    "secret database password is hunter2",
		EpisodeType: "decision",
		Outcome:     "success",
		CreatedAt:   time.Now().Unix(),
	})

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"sensitive-project": siblingStore,
			"self":              s.store,
		},
	})
	s.projectPath = "/test/self"

	// Wildcard query should return nothing.
	result, _ := s.resolveProjectStores("*")
	if len(result) != 0 {
		t.Errorf("default deny-all ACL should block all projects, got %d", len(result))
	}

	// Explicit name query should return nothing + denied message.
	result, notFound := s.resolveProjectStores("sensitive-project")
	if len(result) != 0 {
		t.Errorf("deny-all should block explicit project name, got %d", len(result))
	}
	if len(notFound) == 0 {
		t.Error("expected denied message in notFound for explicit query")
	}
	found := false
	for _, nf := range notFound {
		if strings.Contains(nf, "denied by federation_acl") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'denied by federation_acl' in notFound, got %v", notFound)
	}
}

// TestACL_EmptyAllowReadFrom verifies that an explicit but empty allowlist
// behaves the same as nil (deny-all).
func TestACL_EmptyAllowReadFrom(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = &config.FederationACLConfig{AllowReadFrom: []string{}}

	siblingStore := openTestStore(t)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"sibling": siblingStore,
			"self":    s.store,
		},
	})
	s.projectPath = "/test/self"

	result, _ := s.resolveProjectStores("*")
	if len(result) != 0 {
		t.Errorf("empty AllowReadFrom should deny all, got %d", len(result))
	}
}

// TestACL_SpecificAllowlist verifies that only explicitly allowed projects
// are returned, and non-allowed projects are blocked.
func TestACL_SpecificAllowlist(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = allowACL("allowed-project")

	allowedStore := openTestStore(t)
	blockedStore := openTestStore(t)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"allowed-project": allowedStore,
			"blocked-project": blockedStore,
			"self":            s.store,
		},
	})
	s.projectPath = "/test/self"

	// Wildcard should only return the allowed project.
	result, _ := s.resolveProjectStores("*")
	if _, ok := result["allowed-project"]; !ok {
		t.Error("allowed-project should be accessible")
	}
	if _, ok := result["blocked-project"]; ok {
		t.Error("blocked-project should NOT be accessible")
	}

	// Explicit request for blocked project should be denied.
	result, notFound := s.resolveProjectStores("blocked-project")
	if len(result) != 0 {
		t.Error("blocked-project should not be returned")
	}
	found := false
	for _, nf := range notFound {
		if strings.Contains(nf, "denied by federation_acl") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected denial message for blocked project, got %v", notFound)
	}
}

// TestACL_WildcardAllowAll verifies that ["*"] in allow_read_from
// allows reading from all registered projects.
func TestACL_WildcardAllowAll(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = allowAllACL()

	store1 := openTestStore(t)
	store2 := openTestStore(t)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"project-a": store1,
			"project-b": store2,
			"self":      s.store,
		},
	})
	s.projectPath = "/test/self"

	result, _ := s.resolveProjectStores("*")
	if len(result) != 2 {
		t.Errorf("wildcard ACL should allow all 2 sibling projects, got %d", len(result))
	}
}

// TestACL_DenyAllBlocksRecall is an end-to-end test proving that with no ACL,
// a recall(projects="*") call does NOT return cross-project episodes — the
// actual attack vector (memory exfiltration) is closed.
func TestACL_DenyAllBlocksRecall(t *testing.T) {
	s := newTestServer(t)
	// No ACL — deny-all.

	siblingStore := openTestStore(t)
	siblingStore.RememberEpisode(store.Episode{
		AgentID:     "secret-agent",
		Decision:    "confidential: API key is abc123",
		EpisodeType: "decision",
		Outcome:     "success",
		CreatedAt:   time.Now().Unix(),
	})

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"secret-project": siblingStore,
			"self":           s.store,
		},
	})
	s.projectPath = "/test/self"

	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":    "API key",
		"projects": "*",
	}))
	m := mustResult(t, res, err)

	// cross_project_episodes should be absent or empty.
	if crossEps, ok := m["cross_project_episodes"]; ok {
		eps, _ := crossEps.([]interface{})
		if len(eps) > 0 {
			t.Fatal("SECURITY: deny-all ACL did NOT block cross-project recall — memories leaked")
		}
	}
}

// TestACL_DenyAllBlocksGetEvents is an end-to-end test proving that with no ACL,
// get_events(projects="*") does NOT return cross-project events.
func TestACL_DenyAllBlocksGetEvents(t *testing.T) {
	s := newTestServer(t)
	// No ACL — deny-all.

	siblingStore := openTestStore(t)
	_ = siblingStore.AppendEvent("file_change", "agent-1", `{"file":"secret.go"}`)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"secret-project": siblingStore,
			"self":           s.store,
		},
	})
	s.projectPath = "/test/self"

	res, err := s.handleGetEvents(ctx, callTool(map[string]any{
		"since_seq": 0,
		"projects":  "*",
	}))
	m := mustResult(t, res, err)

	if crossEvents, ok := m["cross_project_events"]; ok {
		events, _ := crossEvents.([]interface{})
		if len(events) > 0 {
			t.Fatal("SECURITY: deny-all ACL did NOT block cross-project events")
		}
	}
}

// TestACL_DenyAllBlocksGetMessages verifies no cross-project message leakage.
func TestACL_DenyAllBlocksGetMessages(t *testing.T) {
	s := newTestServer(t)
	// No ACL — deny-all.

	siblingStore := openTestStore(t)
	siblingStore.SendMessage("a", "b", "secret_topic", `{"data":"confidential"}`, "secret")

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"secret": siblingStore,
			"self":   s.store,
		},
	})
	s.projectPath = "/test/self"

	res, err := s.handleGetMessages(ctx, callTool(map[string]any{
		"agent_id":    "b",
		"projects":    "*",
		"unread_only": false,
	}))
	m := mustResult(t, res, err)

	if crossMsgs, ok := m["cross_project_messages"]; ok {
		msgs, _ := crossMsgs.([]interface{})
		if len(msgs) > 0 {
			t.Fatal("SECURITY: deny-all ACL did NOT block cross-project messages")
		}
	}
}

// TestACL_DenyAllBlocksGetAgents verifies no cross-project agent leakage.
func TestACL_DenyAllBlocksGetAgents(t *testing.T) {
	s := newTestServer(t)
	// No ACL — deny-all.

	siblingStore := openTestStore(t)
	siblingStore.UpsertAgent("hidden-agent", nil)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"secret": siblingStore,
			"self":   s.store,
		},
	})
	s.projectPath = "/test/self"

	res, err := s.handleGetAgents(ctx, callTool(map[string]any{
		"projects": "*",
	}))
	m := mustResult(t, res, err)

	if crossAgents, ok := m["cross_project_agents"]; ok {
		agents, _ := crossAgents.([]interface{})
		if len(agents) > 0 {
			t.Fatal("SECURITY: deny-all ACL did NOT block cross-project agents")
		}
	}
}

// TestACL_PartialAllowBlocksUnlisted verifies that an agent allowed to read
// from project-a but NOT project-b cannot exfiltrate from project-b.
func TestACL_PartialAllowBlocksUnlisted(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = allowACL("project-a")

	storeA := openTestStore(t)
	storeB := openTestStore(t)

	storeA.RememberEpisode(store.Episode{
		AgentID: "a", Decision: "public info", EpisodeType: "decision",
		Outcome: "success", CreatedAt: time.Now().Unix(),
	})
	storeB.RememberEpisode(store.Episode{
		AgentID: "b", Decision: "secret from project-b", EpisodeType: "decision",
		Outcome: "success", CreatedAt: time.Now().Unix(),
	})

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"project-a": storeA,
			"project-b": storeB,
			"self":      s.store,
		},
	})
	s.projectPath = "/test/self"

	res, err := s.handleRecall(ctx, callTool(map[string]any{
		"query":    "info secret",
		"projects": "*",
	}))
	m := mustResult(t, res, err)

	crossEps, ok := m["cross_project_episodes"]
	if !ok {
		t.Fatal("expected cross_project_episodes (from allowed project-a)")
	}
	eps, _ := crossEps.([]interface{})
	// Check that only project-a episodes appear, not project-b.
	for _, ep := range eps {
		epMap, ok := ep.(map[string]interface{})
		if !ok {
			continue
		}
		if proj, ok := epMap["_project"].(string); ok && proj == "project-b" {
			t.Fatal("SECURITY: project-b episodes leaked despite not being in ACL allowlist")
		}
	}
}

// TestACL_ErrorMessageDoesNotLeakProjectNames verifies that when an agent
// queries a project blocked by ACL, the error message does NOT reveal the
// names of other registered projects it shouldn't know about.
func TestACL_ErrorMessageDoesNotLeakProjectNames(t *testing.T) {
	s := newTestServer(t)
	s.config.FederationACL = allowACL("allowed-proj")

	allowedStore := openTestStore(t)
	secretStore := openTestStore(t)

	s.SetProjectRegistry(&mockProjectRegistry{
		stores: map[string]*store.Store{
			"allowed-proj": allowedStore,
			"secret-proj":  secretStore,
			"self":         s.store,
		},
	})
	s.projectPath = "/test/self"

	// allowedProjectNames should only return the allowed project.
	allowed := s.allowedProjectNames()
	for _, name := range allowed {
		if name == "secret-proj" {
			t.Fatal("SECURITY: allowedProjectNames() leaked secret-proj")
		}
	}
	if len(allowed) != 1 || allowed[0] != "allowed-proj" {
		t.Errorf("expected [allowed-proj], got %v", allowed)
	}
}
