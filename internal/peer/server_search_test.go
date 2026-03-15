package peer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
)

func TestHandleSearchEntities_MissingQuery(t *testing.T) {
	ps := NewPeerServer(graph.New("test"), nil, nil)
	req := httptest.NewRequest("GET", "/api/search_entities", nil)
	w := httptest.NewRecorder()

	ps.handleSearchEntities(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleSearchEntities_EmptyQuery(t *testing.T) {
	ps := NewPeerServer(graph.New("test"), nil, nil)
	req := httptest.NewRequest("GET", "/api/search_entities?q=", nil)
	w := httptest.NewRecorder()

	ps.handleSearchEntities(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleSearchEntities_ValidSearch(t *testing.T) {
	g := graph.New("test")
	nid := g.MakeNodeID("test", "MyFunc")
	g.AddNode(&graph.Node{
		ID:       nid,
		Name:     "MyFunc",
		Type:     graph.NodeFunction,
		File:     "test.go",
		Line:     10,
		Exported: true,
		Domain:   "code",
	})

	ps := NewPeerServer(g, nil, nil)
	req := httptest.NewRequest("GET", "/api/search_entities?q=MyFunc", nil)
	w := httptest.NewRecorder()

	ps.handleSearchEntities(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp SearchEntitiesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Total == 0 || len(resp.Entities) == 0 {
		t.Errorf("expected results, got none")
	}

	if resp.Entities[0].Name != "MyFunc" {
		t.Errorf("expected MyFunc, got %s", resp.Entities[0].Name)
	}

	if resp.Entities[0].Domain != "code" {
		t.Errorf("expected domain 'code', got %s", resp.Entities[0].Domain)
	}
}

func TestHandleSearchEntities_NoResults(t *testing.T) {
	g := graph.New("test")
	ps := NewPeerServer(g, nil, nil)
	req := httptest.NewRequest("GET", "/api/search_entities?q=NonexistentEntity", nil)
	w := httptest.NewRecorder()

	ps.handleSearchEntities(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp SearchEntitiesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Total != 0 {
		t.Errorf("expected 0 results, got %d", resp.Total)
	}
}

func TestHandleSearchEntities_DomainField(t *testing.T) {
	g := graph.New("test")
	nid := g.MakeNodeID("test", "APIEndpoint")
	g.AddNode(&graph.Node{
		ID:       nid,
		Name:     "APIEndpoint",
		Type:     graph.NodeFunction,
		File:     "api.go",
		Line:     20,
		Exported: true,
		Domain:   "api",
	})

	ps := NewPeerServer(g, nil, nil)
	req := httptest.NewRequest("GET", "/api/search_entities?q=API", nil)
	w := httptest.NewRecorder()

	ps.handleSearchEntities(w, req)

	var resp SearchEntitiesResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Entities) > 0 && resp.Entities[0].Domain != "api" {
		t.Errorf("expected domain 'api', got %s", resp.Entities[0].Domain)
	}
}
