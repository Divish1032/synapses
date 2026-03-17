package parser_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── GraphQL test helpers ─────────────────────────────────────────────────────

func parseGraphQL(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewGraphQLParser()
	if err := p.Parse(g, "/tmp/schema.graphql", []byte(src)); err != nil {
		t.Fatalf("GraphQLParser.Parse() error: %v", err)
	}
	return g
}

// graphqlSource is a realistic GraphQL schema used as the main fixture.
const graphqlSource = `# Auth service schema

scalar DateTime
scalar UUID

interface Node {
  id: ID!
}

interface Auditable {
  createdAt: DateTime!
  updatedAt: DateTime!
}

type User implements Node & Auditable {
  id: ID!
  name: String!
  email: String!
  createdAt: DateTime!
  updatedAt: DateTime!
  role: Role!
}

type Post implements Node {
  id: ID!
  title: String!
  body: String
  author: User!
}

input CreateUserInput {
  name: String!
  email: String!
  password: String!
}

input UpdateUserInput {
  name: String
  email: String
}

enum Role {
  ADMIN
  EDITOR
  VIEWER
}

enum Status {
  ACTIVE
  INACTIVE
  PENDING
}

union SearchResult = User | Post

schema {
  query: Query
  mutation: Mutation
}

type Query {
  user(id: ID!): User
  users: [User!]!
}

type Mutation {
  createUser(input: CreateUserInput!): User
  updateUser(id: ID!, input: UpdateUserInput!): User
}

fragment UserFields on User {
  id
  name
  email
  role
}

directive @auth(requires: Role = ADMIN) on FIELD_DEFINITION | OBJECT
`

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestGraphQLParser_Extensions(t *testing.T) {
	exts := parser.NewGraphQLParser().Extensions()
	if !hasExtension(exts, ".graphql") {
		t.Errorf("Extensions() = %v, missing .graphql", exts)
	}
	if !hasExtension(exts, ".gql") {
		t.Errorf("Extensions() = %v, missing .gql", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestGraphQLParser_FileNode(t *testing.T) {
	assertFileNode(t, parseGraphQL(t, graphqlSource), "schema.graphql")
}

// ─── Scalar ──────────────────────────────────────────────────────────────────

func TestGraphQLParser_Scalar(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	for _, name := range []string{"DateTime", "UUID"} {
		n := assertNode(t, g, name, graph.NodeStruct)
		if n.Metadata == nil || n.Metadata["kind"] != "scalar" {
			t.Errorf("%s kind = %q, want 'scalar'", name, n.Metadata["kind"])
		}
		if !n.Exported {
			t.Errorf("%s should be exported", name)
		}
	}
}

// ─── Interface ───────────────────────────────────────────────────────────────

func TestGraphQLParser_Interface(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "Node", graph.NodeInterface)
	if n.Metadata == nil || n.Metadata["kind"] != "interface" {
		t.Errorf("Node kind = %q, want 'interface'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("Node interface should be exported")
	}
}

func TestGraphQLParser_MultipleInterfaces(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	assertNode(t, g, "Node", graph.NodeInterface)
	assertNode(t, g, "Auditable", graph.NodeInterface)
}

// ─── Object type ─────────────────────────────────────────────────────────────

func TestGraphQLParser_ObjectType(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "User", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "type" {
		t.Errorf("User kind = %q, want 'type'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("User type should be exported")
	}
}

func TestGraphQLParser_MultipleObjectTypes(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	for _, name := range []string{"User", "Post", "Query", "Mutation"} {
		assertNode(t, g, name, graph.NodeStruct)
	}
}

// ─── Input type ──────────────────────────────────────────────────────────────

func TestGraphQLParser_InputType(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "CreateUserInput", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "input" {
		t.Errorf("CreateUserInput kind = %q, want 'input'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("CreateUserInput should be exported")
	}
}

func TestGraphQLParser_MultipleInputTypes(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	assertNode(t, g, "CreateUserInput", graph.NodeStruct)
	assertNode(t, g, "UpdateUserInput", graph.NodeStruct)
}

// ─── Enum ─────────────────────────────────────────────────────────────────────

func TestGraphQLParser_Enum(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "Role", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "enum" {
		t.Errorf("Role kind = %q, want 'enum'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("Role enum should be exported")
	}
}

func TestGraphQLParser_EnumValuesInMetadata(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "Role", graph.NodeStruct)
	if n.Metadata == nil {
		t.Fatal("Role enum metadata is nil")
	}
	values := n.Metadata["values"]
	if values == "" {
		t.Error("Role enum should have values in metadata")
	}
	for _, v := range []string{"ADMIN", "EDITOR", "VIEWER"} {
		if !strings.Contains(values, v) {
			t.Errorf("Role enum values %q missing %q", values, v)
		}
	}
}

func TestGraphQLParser_MultipleEnums(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	assertNode(t, g, "Role", graph.NodeStruct)
	assertNode(t, g, "Status", graph.NodeStruct)
}

// ─── Union ────────────────────────────────────────────────────────────────────

func TestGraphQLParser_Union(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "SearchResult", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "union" {
		t.Errorf("SearchResult kind = %q, want 'union'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("SearchResult union should be exported")
	}
}

func TestGraphQLParser_UnionMembersInMetadata(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "SearchResult", graph.NodeStruct)
	if n.Metadata == nil {
		t.Fatal("SearchResult union metadata is nil")
	}
	members := n.Metadata["members"]
	if members == "" {
		t.Error("SearchResult union should have members in metadata")
	}
	for _, m := range []string{"User", "Post"} {
		if !strings.Contains(members, m) {
			t.Errorf("SearchResult members %q missing %q", members, m)
		}
	}
}

// ─── Schema definition ───────────────────────────────────────────────────────

func TestGraphQLParser_SchemaDefinition(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "schema", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "schema" {
		t.Errorf("schema kind = %q, want 'schema'", n.Metadata["kind"])
	}
}

func TestGraphQLParser_SchemaRootTypes(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "schema", graph.NodeStruct)
	if n.Metadata == nil {
		t.Fatal("schema metadata is nil")
	}
	if n.Metadata["query"] != "Query" {
		t.Errorf("schema.query = %q, want 'Query'", n.Metadata["query"])
	}
	if n.Metadata["mutation"] != "Mutation" {
		t.Errorf("schema.mutation = %q, want 'Mutation'", n.Metadata["mutation"])
	}
}

// ─── Fragment ─────────────────────────────────────────────────────────────────

func TestGraphQLParser_Fragment(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "UserFields", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "fragment" {
		t.Errorf("UserFields kind = %q, want 'fragment'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("UserFields fragment should be exported")
	}
}

func TestGraphQLParser_FragmentOnType(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "UserFields", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["on"] != "User" {
		t.Errorf("UserFields fragment on = %q, want 'User'", n.Metadata["on"])
	}
}

// ─── Directive definition ────────────────────────────────────────────────────

func TestGraphQLParser_DirectiveDefinition(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "@auth", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "directive" {
		t.Errorf("@auth kind = %q, want 'directive'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("@auth directive should be exported")
	}
}

func TestGraphQLParser_DirectiveLocations(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	n := assertNode(t, g, "@auth", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("@auth metadata is nil")
	}
	locs := n.Metadata["locations"]
	if locs == "" {
		t.Error("@auth directive should have locations in metadata")
	}
	for _, loc := range []string{"FIELD_DEFINITION", "OBJECT"} {
		if !strings.Contains(locs, loc) {
			t.Errorf("@auth locations %q missing %q", locs, loc)
		}
	}
}

// ─── Field extraction ─────────────────────────────────────────────────────────

func TestGraphQLParser_FieldsExtracted(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	// User.id and User.name should exist as NodeFunction nodes.
	assertNode(t, g, "User.id", graph.NodeFunction)
	assertNode(t, g, "User.name", graph.NodeFunction)
	assertNode(t, g, "User.email", graph.NodeFunction)
}

func TestGraphQLParser_FieldMetadataHasType(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	nodes := g.FindByName("User.name")
	if len(nodes) == 0 {
		t.Fatal("User.name field not found")
	}
	n := nodes[0]
	if n.Metadata == nil || n.Metadata["kind"] != "field" {
		t.Errorf("User.name kind = %q, want 'field'", n.Metadata["kind"])
	}
	// Field type should be non-empty (String! or String).
	if n.Metadata["type"] == "" {
		t.Error("User.name should have type in metadata")
	}
}

func TestGraphQLParser_InputFieldsExtracted(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	assertNode(t, g, "CreateUserInput.name", graph.NodeFunction)
	assertNode(t, g, "CreateUserInput.email", graph.NodeFunction)
	assertNode(t, g, "CreateUserInput.password", graph.NodeFunction)
}

// ─── Edge: DEFINES (file → entity) ──────────────────────────────────────────

func TestGraphQLParser_DefinesEdges(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	fileNodes := g.FindByName("schema.graphql")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	for _, name := range []string{"User", "Post", "Role", "SearchResult", "DateTime", "Node", "schema"} {
		assertDefinesEdge(t, g, fileID, name)
	}
}

// ─── Edge: IMPLEMENTS ────────────────────────────────────────────────────────

func TestGraphQLParser_ImplementsEdge(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	// Use the exact User struct node (not User.id, User.name, etc.)
	userNode := assertNode(t, g, "User", graph.NodeStruct)

	// User implements Node — check for EdgeImplements to Node.
	foundNode := false
	for _, e := range g.OutEdges(userNode.ID) {
		if e.Type == graph.EdgeImplements {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "Node" {
				foundNode = true
			}
		}
	}
	if !foundNode {
		t.Error("expected EdgeImplements from User to Node")
	}
}

func TestGraphQLParser_ImplementsMultipleInterfaces(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	// Use the exact User struct node (not User.id, User.name, etc.)
	userNode := assertNode(t, g, "User", graph.NodeStruct)

	implemented := map[string]bool{}
	for _, e := range g.OutEdges(userNode.ID) {
		if e.Type == graph.EdgeImplements {
			n := g.GetNode(e.To)
			if n != nil {
				implemented[n.Name] = true
			}
		}
	}
	for _, iface := range []string{"Node", "Auditable"} {
		if !implemented[iface] {
			t.Errorf("expected User to implement %s via EdgeImplements", iface)
		}
	}
}

// ─── GQL extension ───────────────────────────────────────────────────────────

func TestGraphQLParser_GQLExtension(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGraphQLParser()
	src := []byte(`type Query { ping: String }`)
	if err := p.Parse(g, "/tmp/api.gql", []byte(src)); err != nil {
		t.Fatalf("Parse() on .gql file returned error: %v", err)
	}
	assertFileNode(t, g, "api.gql")
	assertNode(t, g, "Query", graph.NodeStruct)
}

// ─── Comment handling ────────────────────────────────────────────────────────

func TestGraphQLParser_HashComments(t *testing.T) {
	src := `# This is a top-level comment
# Multi-line comment here

# Comment before type
type Foo {
  # Comment inside type
  bar: String
}
`
	g := parseGraphQL(t, src)
	assertNode(t, g, "Foo", graph.NodeStruct)
	assertNode(t, g, "Foo.bar", graph.NodeFunction)
}

// ─── Standalone schema file (operations only) ────────────────────────────────

func TestGraphQLParser_FragmentDependsOnEdge(t *testing.T) {
	src := `type User {
  id: ID!
  name: String!
}

fragment UserFrag on User {
  id
  name
}
`
	g := graph.New("testrepo")
	p := parser.NewGraphQLParser()
	if err := p.Parse(g, "/tmp/ops.graphql", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	fragNodes := g.FindByName("UserFrag")
	if len(fragNodes) == 0 {
		t.Fatal("UserFrag fragment not found")
	}
	fragID := fragNodes[0].ID

	// When target type is in the same file, should have EdgeDependsOn → User.
	found := false
	for _, e := range g.OutEdges(fragID) {
		if e.Type == graph.EdgeDependsOn {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "User" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected EdgeDependsOn from UserFrag to User")
	}
}

// ─── Union member DependsOn edges ────────────────────────────────────────────

func TestGraphQLParser_UnionMemberDependsOnEdge(t *testing.T) {
	src := `type Cat { name: String }
type Dog { name: String }
union Animal = Cat | Dog
`
	g := graph.New("testrepo")
	p := parser.NewGraphQLParser()
	if err := p.Parse(g, "/tmp/animal.graphql", []byte(src)); err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	unionNodes := g.FindByName("Animal")
	if len(unionNodes) == 0 {
		t.Fatal("Animal union not found")
	}
	unionID := unionNodes[0].ID

	targets := map[string]bool{}
	for _, e := range g.OutEdges(unionID) {
		if e.Type == graph.EdgeDependsOn {
			n := g.GetNode(e.To)
			if n != nil {
				targets[n.Name] = true
			}
		}
	}
	for _, member := range []string{"Cat", "Dog"} {
		if !targets[member] {
			t.Errorf("expected EdgeDependsOn from Animal to %s", member)
		}
	}
}

// ─── Edge: DEFINES (parent type → field) ────────────────────────────────────

func TestGraphQLParser_DefinesEdgeTypeToField(t *testing.T) {
	g := parseGraphQL(t, graphqlSource)
	userNode := assertNode(t, g, "User", graph.NodeStruct)

	found := false
	for _, e := range g.OutEdges(userNode.ID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "User.id" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected DEFINES edge from User to User.id field")
	}
}

// ─── Minimal schema ──────────────────────────────────────────────────────────

func TestGraphQLParser_MinimalType(t *testing.T) {
	src := `type Ping { ok: Boolean }`
	g := parseGraphQL(t, src)
	assertNode(t, g, "Ping", graph.NodeStruct)
}

// ─── Empty file ──────────────────────────────────────────────────────────────

func TestGraphQLParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewGraphQLParser(), ".graphql", "")
}

// ─── Comments-only file ──────────────────────────────────────────────────────

func TestGraphQLParser_CommentsOnlyFile(t *testing.T) {
	src := `# Just a comment
# Nothing else here
`
	g := graph.New("testrepo")
	p := parser.NewGraphQLParser()
	if err := p.Parse(g, "/tmp/comments.graphql", []byte(src)); err != nil {
		t.Fatalf("Parse() on comments-only file returned error: %v", err)
	}
	// Should at minimum have the file node.
	nodes := g.FindByName("comments.graphql")
	if len(nodes) == 0 {
		t.Error("expected file node even for comments-only file")
	}
}

// ─── Multiple files parsed into same graph ───────────────────────────────────

func TestGraphQLParser_TwoFilesIntoOneGraph(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewGraphQLParser()

	src1 := []byte(`type User { id: ID! }`)
	src2 := []byte(`type Post { id: ID! title: String! }`)

	if err := p.Parse(g, "/tmp/users.graphql", src1); err != nil {
		t.Fatalf("Parse() file1 error: %v", err)
	}
	if err := p.Parse(g, "/tmp/posts.graphql", src2); err != nil {
		t.Fatalf("Parse() file2 error: %v", err)
	}

	assertNode(t, g, "User", graph.NodeStruct)
	assertNode(t, g, "Post", graph.NodeStruct)
}

// ─── Interface fields extraction ─────────────────────────────────────────────

func TestGraphQLParser_InterfaceFieldsExtracted(t *testing.T) {
	src := `interface Node {
  id: ID!
  createdAt: String
}
`
	g := parseGraphQL(t, src)
	assertNode(t, g, "Node", graph.NodeInterface)
	assertNode(t, g, "Node.id", graph.NodeFunction)
	assertNode(t, g, "Node.createdAt", graph.NodeFunction)
}

// ─── Nested list and non-null types ──────────────────────────────────────────

func TestGraphQLParser_ListAndNonNullFieldTypes(t *testing.T) {
	src := `type Catalog {
  items: [String!]!
  tags: [String]
  name: String!
}
`
	g := parseGraphQL(t, src)
	assertNode(t, g, "Catalog", graph.NodeStruct)
	// All three fields should be extracted.
	assertNode(t, g, "Catalog.items", graph.NodeFunction)
	assertNode(t, g, "Catalog.tags", graph.NodeFunction)
	assertNode(t, g, "Catalog.name", graph.NodeFunction)
}

// ─── Schema without explicit schema block ────────────────────────────────────

func TestGraphQLParser_SchemaWithoutSchemaKeyword(t *testing.T) {
	// Many GraphQL schemas use conventional root type names without a schema block.
	src := `type Query {
  hello: String
}

type Mutation {
  noop: Boolean
}
`
	g := parseGraphQL(t, src)
	assertNode(t, g, "Query", graph.NodeStruct)
	assertNode(t, g, "Mutation", graph.NodeStruct)
}
