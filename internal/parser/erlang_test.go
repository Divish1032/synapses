package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Erlang ──────────────────────────────────────────────────────────────────

// A realistic gen_server module fixture covering most features.
const erlangGenServerSource = `-module(my_server).
-behaviour(gen_server).

-export([start_link/0, stop/0, get_state/0]).
-export([init/1, handle_call/3, handle_cast/2, handle_info/2, terminate/2, code_change/3]).

-import(lists, [map/2, filter/2]).
-include("my_server.hrl").
-include_lib("stdlib/include/assert.hrl").

-record(state, {count = 0, name}).

-type count() :: non_neg_integer().

-opaque handle() :: #state{}.

%% @doc Starts the gen_server process linked to the calling process.
start_link() ->
    gen_server:start_link({local, ?MODULE}, ?MODULE, [], []).

stop() ->
    gen_server:stop(?MODULE).

get_state() ->
    gen_server:call(?MODULE, get_state).

init([]) ->
    {ok, #state{count = 0}}.

%% @doc Handles synchronous call messages.
handle_call(get_state, _From, State) ->
    {reply, State, State};
handle_call(_Request, _From, State) ->
    {reply, ok, State}.

handle_cast(_Msg, State) ->
    {noreply, State}.

handle_info(_Info, State) ->
    {noreply, State}.

terminate(_Reason, _State) ->
    ok.

code_change(_OldVsn, State, _Extra) ->
    {ok, State}.
`

// A header file with only records and types (no functions, no module attr).
const erlangHeaderSource = `-record(person, {name, age, email}).
-record(address, {street, city, country}).

-type person_id() :: integer().
-opaque token() :: binary().
`

// A minimal file with a multi-line export list.
const erlangMultiExportSource = `-module(multi_export).

-export([
    foo/0,
    bar/1,
    baz/2
]).

foo() -> ok.
bar(_X) -> ok.
baz(_X, _Y) -> ok.
internal() -> hidden.
`

// parseErlang is a test helper that parses Erlang source and returns the graph.
func parseErlang(t *testing.T, filePath string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewErlangParser()
	if err := p.Parse(g, filePath, []byte(src)); err != nil {
		t.Fatalf("ErlangParser.Parse() error: %v", err)
	}
	return g
}

// ─── 1. Extensions() has .erl and .hrl ───────────────────────────────────────

func TestErlangParser_Extensions(t *testing.T) {
	exts := parser.NewErlangParser().Extensions()
	if !hasExtension(exts, ".erl") {
		t.Errorf("Extensions() = %v, missing .erl", exts)
	}
	if !hasExtension(exts, ".hrl") {
		t.Errorf("Extensions() = %v, missing .hrl", exts)
	}
}

// ─── 2. File node created ─────────────────────────────────────────────────────

func TestErlangParser_FileNode(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	assertFileNode(t, g, "my_server.erl")
}

// ─── 3. Simple function clause extracted ──────────────────────────────────────

func TestErlangParser_SimpleFunction(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	assertNode(t, g, "start_link/0", graph.NodeFunction)
}

// ─── 4. Multiple clauses of same function → only one node created ─────────────

func TestErlangParser_MultipleClausesDeduped(t *testing.T) {
	// handle_call has two clauses in the fixture; both have arity 3.
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	nodes := g.FindByName("handle_call/3")
	count := 0
	for _, n := range nodes {
		if n.Type == graph.NodeFunction {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 handle_call/3 function node, got %d", count)
	}
}

// ─── 5. module attribute extracted as NodePackage ──────────────────────────────

func TestErlangParser_ModuleAttribute(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	nodes := g.FindByName("my_server")
	found := false
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("module node 'my_server' (NodePackage) not found")
	}
}

// ─── 6. export marks functions as Exported true ────────────────────────────────

func TestErlangParser_ExportedFunctions(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	// Names are now arity-qualified (name/arity) to reflect Erlang's identity model.
	for _, name := range []string{"start_link/0", "stop/0", "get_state/0", "init/1", "handle_call/3", "handle_cast/2", "handle_info/2", "terminate/2", "code_change/3"} {
		n := assertNode(t, g, name, graph.NodeFunction)
		if !n.Exported {
			t.Errorf("function %q should be Exported=true (listed in -export)", name)
		}
	}
}

// ─── 7. Unexported function → Exported false ──────────────────────────────────

func TestErlangParser_UnexportedFunctionFalse(t *testing.T) {
	g := parseErlang(t, "/tmp/multi_export.erl", erlangMultiExportSource)
	n := assertNode(t, g, "internal/0", graph.NodeFunction)
	if n.Exported {
		t.Error("function 'internal/0' should be Exported=false (not in -export)")
	}
}

// ─── 8. record definition extracted as NodeStruct kind=record ─────────────────

func TestErlangParser_RecordDefinition(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	n := assertNode(t, g, "state", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "record" {
		t.Errorf("record 'state' metadata kind = %q, want 'record'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("records should always be Exported=true")
	}
}

// ─── 9. type definition extracted ─────────────────────────────────────────────

func TestErlangParser_TypeDefinition(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	n := assertNode(t, g, "count", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "type" {
		t.Errorf("type 'count' metadata kind = %q, want 'type'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("types should always be Exported=true")
	}
}

// ─── 10. opaque type extracted ─────────────────────────────────────────────────

func TestErlangParser_OpaqueType(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	n := assertNode(t, g, "handle", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "opaque" {
		t.Errorf("opaque 'handle' metadata kind = %q, want 'opaque'", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("opaques should always be Exported=true")
	}
}

// ─── 11. behaviour attribute captured in metadata ──────────────────────────────

func TestErlangParser_BehaviourMetadata(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	nodes := g.FindByName("my_server")
	var modNode *graph.Node
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			modNode = n
			break
		}
	}
	if modNode == nil {
		t.Fatal("module node not found")
	}
	if modNode.Metadata == nil || modNode.Metadata["behaviours"] == "" {
		t.Error("module node should have 'behaviours' metadata set")
	}
	if modNode.Metadata["behaviours"] != "gen_server" {
		t.Errorf("behaviours = %q, want 'gen_server'", modNode.Metadata["behaviours"])
	}
}

// ─── 12. import from other module → EdgeImports ────────────────────────────────

func TestErlangParser_ImportEdge(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	fileNodes := g.FindByName("my_server.erl")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type != graph.EdgeImports {
			continue
		}
		n := g.GetNode(e.To)
		if n != nil && n.Name == "lists" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected EdgeImports to 'lists' module (from -import)")
	}
}

// ─── 13. include file → EdgeImports ───────────────────────────────────────────

func TestErlangParser_IncludeEdge(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	fileNodes := g.FindByName("my_server.erl")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type != graph.EdgeImports {
			continue
		}
		n := g.GetNode(e.To)
		if n != nil && n.Name == "my_server.hrl" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected EdgeImports to 'my_server.hrl' (from -include)")
	}
}

// ─── 14. include_lib file → EdgeImports ───────────────────────────────────────

func TestErlangParser_IncludeLibEdge(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	fileNodes := g.FindByName("my_server.erl")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type != graph.EdgeImports {
			continue
		}
		n := g.GetNode(e.To)
		if n != nil && n.Name == "stdlib/include/assert.hrl" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected EdgeImports to 'stdlib/include/assert.hrl' (from -include_lib)")
	}
}

// ─── 15. EDoc %% @doc comment extracted ──────────────────────────────────────

func TestErlangParser_EdocComment(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	nodes := g.FindByName("start_link/0")
	if len(nodes) == 0 {
		t.Fatal("start_link/0 node not found")
	}
	n := nodes[0]
	if n.Metadata == nil || n.Metadata["doc"] == "" {
		t.Error("start_link/0 should have doc metadata from %% @doc comment")
	}
}

// ─── 16. Empty file → no crash ────────────────────────────────────────────────

func TestErlangParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewErlangParser(), ".erl", "")
}

// ─── 17. Header file (.hrl) with only records and types ──────────────────────

func TestErlangParser_HrlFile(t *testing.T) {
	g := parseErlang(t, "/tmp/types.hrl", erlangHeaderSource)

	// File node should exist.
	assertFileNode(t, g, "types.hrl")

	// Records should be extracted.
	assertNode(t, g, "person", graph.NodeStruct)
	assertNode(t, g, "address", graph.NodeStruct)

	// Types should be extracted.
	assertNode(t, g, "person_id", graph.NodeStruct)
	assertNode(t, g, "token", graph.NodeStruct)
}

// ─── 18. DEFINES edge file→function ──────────────────────────────────────────

func TestErlangParser_DefinesEdge(t *testing.T) {
	g := parseErlang(t, "/tmp/my_server.erl", erlangGenServerSource)
	fileNodes := g.FindByName("my_server.erl")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	assertDefinesEdge(t, g, fileID, "start_link/0")
	assertDefinesEdge(t, g, fileID, "init/1")
	assertDefinesEdge(t, g, fileID, "state")
}

// ─── 19. Multi-line export list handled ───────────────────────────────────────

func TestErlangParser_MultiLineExport(t *testing.T) {
	g := parseErlang(t, "/tmp/multi_export.erl", erlangMultiExportSource)

	// Names are arity-qualified to match Erlang's identity model.
	for _, name := range []string{"foo/0", "bar/1", "baz/2"} {
		n := assertNode(t, g, name, graph.NodeFunction)
		if !n.Exported {
			t.Errorf("function %q should be Exported=true (listed in multi-line -export)", name)
		}
	}

	// internal/0 is NOT in the export list.
	n := assertNode(t, g, "internal/0", graph.NodeFunction)
	if n.Exported {
		t.Error("function 'internal/0' should be Exported=false")
	}
}

// ─── 20. HRL opaque type extracted correctly ──────────────────────────────────

func TestErlangParser_HrlOpaqueType(t *testing.T) {
	g := parseErlang(t, "/tmp/types.hrl", erlangHeaderSource)
	n := assertNode(t, g, "token", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "opaque" {
		t.Errorf("token kind = %q, want 'opaque'", n.Metadata["kind"])
	}
}

// ─── 21. -type, -opaque, and OTP behaviour nodes ─────────────────────────────

func TestErlangParserTypesAndBehaviour(t *testing.T) {
	src := `-module(myserver).
-behaviour(gen_server).

-export([start_link/0, handle_call/3, handle_cast/2]).

-type server_state() :: #{count => integer(), name => binary()}.
-type result(T) :: {ok, T} | {error, term()}.
-opaque handle() :: reference().

-record(state, {count = 0, name = <<>>}).

start_link() ->
    gen_server:start_link({local, ?MODULE}, ?MODULE, [], []).

handle_call(_Request, _From, State) ->
    {reply, ok, State}.

handle_cast(_Msg, State) ->
    {noreply, State}.
`
	g := parseErlang(t, "/src/myserver.erl", src)

	// -type declarations should have kind=type (the parser's existing convention).
	for _, name := range []string{"server_state", "result"} {
		nodes := g.FindByName(name)
		found := false
		for _, n := range nodes {
			if n.Metadata["kind"] == "type" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected -type node %q with kind=type", name)
		}
	}

	// -opaque declaration should have kind=opaque.
	{
		nodes := g.FindByName("handle")
		found := false
		for _, n := range nodes {
			if n.Metadata["kind"] == "opaque" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected -opaque node \"handle\" with kind=opaque")
		}
	}

	// OTP behaviour: a NodeVariable named gen_server with kind=otp_behaviour.
	{
		nodes := g.FindByName("gen_server")
		found := false
		for _, n := range nodes {
			if n.Metadata["kind"] == "otp_behaviour" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected OTP behaviour node gen_server with kind=otp_behaviour")
		}
	}
}

// ─── 22. Arity in node names, -spec metadata, -callback extraction ────────────

// erlangAritySpecSource is a fixture with same-name different-arity functions,
// a -spec annotation, and a -callback declaration.
const erlangAritySpecSource = `-module(math_ops).

-export([add/2, add/3]).

-spec add(integer(), integer()) -> integer().
add(A, B) ->
    A + B.

add(A, B, C) ->
    A + B + C.

internal() ->
    ok.

-callback init(Args :: term()) -> {ok, State :: term()} | {error, Reason :: term()}.
`

func TestErlangParserArityAndSpec(t *testing.T) {
	g := parseErlang(t, "/tmp/math_ops.erl", erlangAritySpecSource)

	// 1. Two functions with same name but different arities → distinct nodes.
	add2 := assertNode(t, g, "add/2", graph.NodeFunction)
	if !add2.Exported {
		t.Error("add/2 should be Exported=true (listed in -export)")
	}

	add3 := assertNode(t, g, "add/3", graph.NodeFunction)
	if !add3.Exported {
		t.Error("add/3 should be Exported=true (listed in -export)")
	}

	// They must be different nodes.
	if add2.ID == add3.ID {
		t.Error("add/2 and add/3 must be distinct nodes")
	}

	// 2. -spec add(...) → spec metadata attached to add/2 (the first matching arity).
	if add2.Metadata == nil || add2.Metadata["spec"] == "" {
		t.Error("add/2 should have spec metadata from -spec declaration")
	}

	// 3. internal/0 is not exported.
	internal := assertNode(t, g, "internal/0", graph.NodeFunction)
	if internal.Exported {
		t.Error("internal/0 should be Exported=false (not in -export)")
	}

	// 4. -callback init(...) → NodeFunction with kind=callback, Exported=true.
	// The callback node is init/1 (one argument).
	cb := assertNode(t, g, "init/1", graph.NodeFunction)
	if !cb.Exported {
		t.Error("callback init/1 should be Exported=true")
	}
	if cb.Metadata == nil || cb.Metadata["kind"] != "callback" {
		t.Errorf("callback init/1 metadata kind = %q, want callback", cb.Metadata["kind"])
	}
}
