package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── OpenAPI YAML helpers ─────────────────────────────────────────────────────

func parseOpenAPIYAML(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewYAMLParser()
	if err := p.Parse(g, "/tmp/openapi.yaml", []byte(src)); err != nil {
		t.Fatalf("YAMLParser.Parse() error: %v", err)
	}
	return g
}

func parseOpenAPIJSON(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewJSONParser()
	if err := p.Parse(g, "/tmp/openapi.json", []byte(src)); err != nil {
		t.Fatalf("JSONParser.Parse() error: %v", err)
	}
	return g
}

// ─── Fixtures ─────────────────────────────────────────────────────────────────

const openapiV3YAML = `
openapi: "3.0.3"
info:
  title: "User API"
  version: "1.0.0"
paths:
  /users:
    get:
      operationId: listUsers
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/UserList"
    post:
      operationId: createUser
      requestBody:
        content:
          application/json:
            schema:
              $ref: "#/components/schemas/CreateUserInput"
      responses:
        "201":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/User"
  /users/{id}:
    get:
      operationId: getUser
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/User"
components:
  schemas:
    User:
      type: object
      properties:
        id:
          type: integer
        name:
          type: string
    UserList:
      type: object
      properties:
        items:
          type: array
    CreateUserInput:
      type: object
      properties:
        name:
          type: string
`

const swaggerV2YAML = `
swagger: "2.0"
info:
  title: "Order API"
  version: "2.0.0"
paths:
  /orders:
    post:
      operationId: createOrder
      parameters:
        - in: body
          schema:
            $ref: "#/definitions/OrderInput"
      responses:
        200:
          schema:
            $ref: "#/definitions/Order"
definitions:
  Order:
    type: object
  OrderInput:
    type: object
`

const openapiV3JSON = `{
  "openapi": "3.0.3",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "operationId": "listPets",
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": {"$ref": "#/components/schemas/PetList"}
              }
            }
          }
        }
      },
      "post": {
        "operationId": "createPet",
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {"$ref": "#/components/schemas/NewPet"}
            }
          }
        },
        "responses": {"201": {}}
      }
    }
  },
  "components": {
    "schemas": {
      "Pet":     {"type": "object"},
      "PetList": {"type": "object"},
      "NewPet":  {"type": "object"}
    }
  }
}`

// ─── OpenAPI 3.x YAML tests ───────────────────────────────────────────────────

func TestOpenAPIYAML_EndpointsAreNodeRoute(t *testing.T) {
	g := parseOpenAPIYAML(t, openapiV3YAML)
	for _, name := range []string{"listUsers", "createUser", "getUser"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("endpoint %q not found", name)
			continue
		}
		if nodes[0].Type != graph.NodeRoute {
			t.Errorf("endpoint %q type = %q, want NodeRoute", name, nodes[0].Type)
		}
	}
}

func TestOpenAPIYAML_EndpointsDomainAPI(t *testing.T) {
	g := parseOpenAPIYAML(t, openapiV3YAML)
	nodes := g.FindByName("listUsers")
	if len(nodes) == 0 {
		t.Fatal("listUsers not found")
	}
	if nodes[0].Domain != graph.DomainAPI {
		t.Errorf("listUsers domain = %q, want DomainAPI", nodes[0].Domain)
	}
}

func TestOpenAPIYAML_EndpointMetadata(t *testing.T) {
	g := parseOpenAPIYAML(t, openapiV3YAML)
	nodes := g.FindByName("createUser")
	if len(nodes) == 0 {
		t.Fatal("createUser not found")
	}
	n := nodes[0]
	if n.Metadata["method"] != "POST" {
		t.Errorf("method = %q, want POST", n.Metadata["method"])
	}
	if n.Metadata["path"] != "/users" {
		t.Errorf("path = %q, want /users", n.Metadata["path"])
	}
	if n.Metadata["kind"] != "openapi_endpoint" {
		t.Errorf("kind = %q, want openapi_endpoint", n.Metadata["kind"])
	}
}

func TestOpenAPIYAML_SchemasAreNodeStruct(t *testing.T) {
	g := parseOpenAPIYAML(t, openapiV3YAML)
	for _, name := range []string{"User", "UserList", "CreateUserInput"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("schema %q not found", name)
			continue
		}
		if nodes[0].Type != graph.NodeStruct {
			t.Errorf("schema %q type = %q, want NodeStruct", name, nodes[0].Type)
		}
		if nodes[0].Domain != graph.DomainAPI {
			t.Errorf("schema %q domain = %q, want DomainAPI", name, nodes[0].Domain)
		}
		if nodes[0].Metadata["kind"] != "openapi_schema" {
			t.Errorf("schema %q kind = %q, want openapi_schema", name, nodes[0].Metadata["kind"])
		}
	}
}

func TestOpenAPIYAML_EndpointToSchemaEdges(t *testing.T) {
	g := parseOpenAPIYAML(t, openapiV3YAML)

	// listUsers → UserList
	listUsers := g.FindByName("listUsers")
	if len(listUsers) == 0 {
		t.Fatal("listUsers not found")
	}
	if !hasDependsOnEdge(g, listUsers[0].ID, "UserList") {
		t.Error("listUsers should have DEPENDS_ON edge to UserList")
	}

	// createUser → CreateUserInput and User
	createUser := g.FindByName("createUser")
	if len(createUser) == 0 {
		t.Fatal("createUser not found")
	}
	if !hasDependsOnEdge(g, createUser[0].ID, "CreateUserInput") {
		t.Error("createUser should have DEPENDS_ON edge to CreateUserInput")
	}
	if !hasDependsOnEdge(g, createUser[0].ID, "User") {
		t.Error("createUser should have DEPENDS_ON edge to User")
	}
}

func TestOpenAPIYAML_InfoMetadata(t *testing.T) {
	g := parseOpenAPIYAML(t, openapiV3YAML)
	fileNodes := g.FindByName("openapi.yaml")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	n := fileNodes[0]
	if n.Metadata["title"] != "User API" {
		t.Errorf("title = %q, want 'User API'", n.Metadata["title"])
	}
	if n.Metadata["version"] != "1.0.0" {
		t.Errorf("version = %q, want '1.0.0'", n.Metadata["version"])
	}
}

// ─── Swagger 2.x YAML tests ───────────────────────────────────────────────────

func TestSwaggerV2YAML_Endpoints(t *testing.T) {
	g := parseOpenAPIYAML(t, swaggerV2YAML)
	nodes := g.FindByName("createOrder")
	if len(nodes) == 0 {
		t.Fatal("createOrder not found")
	}
	if nodes[0].Type != graph.NodeRoute {
		t.Errorf("type = %q, want NodeRoute", nodes[0].Type)
	}
}

func TestSwaggerV2YAML_DefinitionsExtracted(t *testing.T) {
	g := parseOpenAPIYAML(t, swaggerV2YAML)
	for _, name := range []string{"Order", "OrderInput"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("definition %q not found", name)
		}
	}
}

func TestSwaggerV2YAML_EndpointToSchemaEdges(t *testing.T) {
	g := parseOpenAPIYAML(t, swaggerV2YAML)
	createOrder := g.FindByName("createOrder")
	if len(createOrder) == 0 {
		t.Fatal("createOrder not found")
	}
	if !hasDependsOnEdge(g, createOrder[0].ID, "OrderInput") {
		t.Error("createOrder should have DEPENDS_ON edge to OrderInput")
	}
	if !hasDependsOnEdge(g, createOrder[0].ID, "Order") {
		t.Error("createOrder should have DEPENDS_ON edge to Order")
	}
}

// ─── OpenAPI JSON tests ───────────────────────────────────────────────────────

func TestOpenAPIJSON_EndpointsAreNodeRoute(t *testing.T) {
	g := parseOpenAPIJSON(t, openapiV3JSON)
	for _, name := range []string{"listPets", "createPet"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("endpoint %q not found", name)
			continue
		}
		if nodes[0].Type != graph.NodeRoute {
			t.Errorf("endpoint %q type = %q, want NodeRoute", name, nodes[0].Type)
		}
		if nodes[0].Domain != graph.DomainAPI {
			t.Errorf("endpoint %q domain = %q, want DomainAPI", name, nodes[0].Domain)
		}
	}
}

func TestOpenAPIJSON_SchemasExtracted(t *testing.T) {
	g := parseOpenAPIJSON(t, openapiV3JSON)
	for _, name := range []string{"Pet", "PetList", "NewPet"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("schema %q not found", name)
			continue
		}
		if nodes[0].Type != graph.NodeStruct {
			t.Errorf("schema %q type = %q, want NodeStruct", name, nodes[0].Type)
		}
		if nodes[0].Domain != graph.DomainAPI {
			t.Errorf("schema %q domain = %q, want DomainAPI", name, nodes[0].Domain)
		}
	}
}

func TestOpenAPIJSON_EndpointToSchemaEdge(t *testing.T) {
	g := parseOpenAPIJSON(t, openapiV3JSON)
	listPets := g.FindByName("listPets")
	if len(listPets) == 0 {
		t.Fatal("listPets not found")
	}
	if !hasDependsOnEdge(g, listPets[0].ID, "PetList") {
		t.Error("listPets should have DEPENDS_ON edge to PetList")
	}
	createPet := g.FindByName("createPet")
	if len(createPet) == 0 {
		t.Fatal("createPet not found")
	}
	if !hasDependsOnEdge(g, createPet[0].ID, "NewPet") {
		t.Error("createPet should have DEPENDS_ON edge to NewPet")
	}
}

func TestOpenAPIJSON_InfoMetadata(t *testing.T) {
	g := parseOpenAPIJSON(t, openapiV3JSON)
	fileNodes := g.FindByName("openapi.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	n := fileNodes[0]
	if n.Metadata["title"] != "Pet API" {
		t.Errorf("title = %q, want 'Pet API'", n.Metadata["title"])
	}
}

func TestOpenAPIJSON_NonOpenAPIFallsThrough(t *testing.T) {
	// Regular JSON should not be treated as OpenAPI.
	src := `{"name": "foo", "version": "1.0"}`
	g := graph.New("testrepo")
	p := parser.NewJSONParser()
	if err := p.Parse(g, "/tmp/config.json", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	// Should produce generic field nodes, NOT route nodes.
	for _, n := range g.FindByName("name") {
		if n.Type == graph.NodeRoute {
			t.Error("non-OpenAPI JSON should not produce NodeRoute nodes")
		}
	}
}

// ─── Shared edge helpers ──────────────────────────────────────────────────────

func hasDependsOnEdge(g *graph.Graph, fromID graph.NodeID, targetName string) bool {
	for _, e := range g.OutEdges(fromID) {
		if e.Type != graph.EdgeDependsOn {
			continue
		}
		n := g.GetNode(e.To)
		if n != nil && n.Name == targetName {
			return true
		}
	}
	return false
}
