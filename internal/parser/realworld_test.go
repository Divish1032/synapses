package parser_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// parseDir walks a directory, parses all files matching the given extensions,
// and returns the populated graph. Panics are caught and reported.
func parseDir(t *testing.T, p interface {
	Parse(*graph.Graph, string, []byte) error
	Extensions() []string
}, dir string) *graph.Graph {
	t.Helper()
	g := graph.New("rw")
	ext := map[string]bool{}
	for _, e := range p.Extensions() {
		ext[e] = true
	}
	count := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !ext[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if parseErr := func() (retErr error) {
			defer func() {
				if r := recover(); r != nil {
					retErr = fmt.Errorf("PANIC parsing %s: %v", path, r)
				}
			}()
			return p.Parse(g, path, src)
		}(); parseErr != nil {
			t.Logf("PARSE ERROR %s: %v", path, parseErr)
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("walk error: %v", err)
	}
	t.Logf("parsed %d files, graph has %d nodes", count, len(g.AllNodes()))
	return g
}

// requireSymbol fails the test if no node with the given name exists.
func requireSymbol(t *testing.T, g *graph.Graph, name string) {
	t.Helper()
	if nodes := g.FindByName(name); len(nodes) == 0 {
		t.Errorf("MISSING symbol: %q", name)
	}
}

// requireMeta fails if the symbol exists but lacks the expected metadata key/value.
func requireMeta(t *testing.T, g *graph.Graph, name, key, val string) {
	t.Helper()
	nodes := g.FindByName(name)
	if len(nodes) == 0 {
		t.Errorf("MISSING symbol: %q", name)
		return
	}
	got := nodes[0].Metadata[key]
	if got != val {
		t.Errorf("symbol %q: want meta[%s]=%q, got %q", name, key, val, got)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// TypeORM — TypeScript with class decorators
// ──────────────────────────────────────────────────────────────────────────────

func TestRealWorld_TypeScript_TypeORM(t *testing.T) {
	srcDir := "/tmp/rw_typeorm/src"
	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		t.Skip("typeorm not cloned")
	}

	// Parse library source: core classes and interface methods
	gSrc := parseDir(t, parser.NewTypeScriptParser(), srcDir)
	requireSymbol(t, gSrc, "EntityMetadataBuilder")
	requireSymbol(t, gSrc, "DefaultNamingStrategy")
	requireSymbol(t, gSrc, "NamingStrategyInterface")
	requireSymbol(t, gSrc, "NamingStrategyInterface.tableName")

	// Parse entity files — these have @Entity() @Column() decorators on exported classes
	entityDir := "/tmp/rw_typeorm/test/benchmark/multiple-joins-querybuilder/entity"
	gEnt := parseDir(t, parser.NewTypeScriptParser(), entityDir)
	decorated := 0
	for _, n := range gEnt.AllNodes() {
		if n.Metadata != nil && n.Metadata["decorators"] != "" {
			decorated++
		}
	}
	if decorated == 0 {
		for _, n := range gEnt.AllNodes() {
			if n.Type == graph.NodeStruct {
				t.Logf("  class %q meta=%v", n.Name, n.Metadata)
			}
		}
		t.Errorf("NO decorated classes in TypeORM entity files — decorator metadata broken")
	} else {
		t.Logf("found %d decorated classes in entity files", decorated)
	}
	requireSymbol(t, gEnt, "Eight")
	requireSymbol(t, gEnt, "One")
}

// ──────────────────────────────────────────────────────────────────────────────
// FastAPI — Python with type-annotated class fields
// ──────────────────────────────────────────────────────────────────────────────

func TestRealWorld_Python_FastAPI(t *testing.T) {
	dir := "/tmp/rw_fastapi/fastapi"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skip("fastapi not cloned")
	}
	g := parseDir(t, parser.NewPythonParser(), dir)

	// Core FastAPI class
	requireSymbol(t, g, "FastAPI")

	// Key methods on FastAPI
	requireSymbol(t, g, "FastAPI.get")
	requireSymbol(t, g, "FastAPI.post")
	requireSymbol(t, g, "FastAPI.include_router")

	// Params module has type-annotated fields (dataclass-style)
	requireSymbol(t, g, "Query")
	requireSymbol(t, g, "Path")
	requireSymbol(t, g, "Body")

	// Count type-annotated fields across all files
	fieldCount := 0
	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["kind"] == "field" {
			fieldCount++
		}
	}
	if fieldCount == 0 {
		t.Errorf("NO type-annotated class fields found in FastAPI — dataclass field detection broken")
	} else {
		t.Logf("found %d type-annotated class fields", fieldCount)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Plug — Elixir with use, defdelegate, defstruct
// ──────────────────────────────────────────────────────────────────────────────

func TestRealWorld_Elixir_Plug(t *testing.T) {
	dir := "/tmp/rw_plug/lib"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skip("plug not cloned")
	}
	g := parseDir(t, parser.NewElixirParser(), dir)

	// Core modules
	requireSymbol(t, g, "Plug")
	requireSymbol(t, g, "Plug.Conn")

	// Plug.Conn has defdelegate entries
	delegates := 0
	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["kind"] == "delegate" {
			delegates++
		}
	}
	if delegates == 0 {
		t.Errorf("NO defdelegate entries found in Plug — defdelegate detection broken")
	} else {
		t.Logf("found %d defdelegate entries", delegates)
	}

	// Struct fields (defstruct)
	fields := 0
	for _, n := range g.AllNodes() {
		if n.Metadata != nil && n.Metadata["kind"] == "field" {
			fields++
		}
	}
	if fields == 0 {
		t.Errorf("NO defstruct fields found in Plug")
	} else {
		t.Logf("found %d defstruct fields", fields)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// ripgrep — Rust with impl Trait for Type
// ──────────────────────────────────────────────────────────────────────────────

func TestRealWorld_Rust_Ripgrep(t *testing.T) {
	dir := "/tmp/rw_ripgrep/crates"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skip("ripgrep not cloned")
	}
	g := parseDir(t, parser.NewRustParser(), dir)

	// Key structs
	requireSymbol(t, g, "Searcher")
	requireSymbol(t, g, "SearcherBuilder")

	// Methods on Searcher (from impl blocks)
	requireSymbol(t, g, "Searcher.search_path")
	requireSymbol(t, g, "Searcher.search_reader")
	requireSymbol(t, g, "SearcherBuilder.build")

	// Traits
	requireSymbol(t, g, "Sink")

	// Count total methods to verify impl-block walking works broadly
	methods := 0
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeMethod {
			methods++
		}
	}
	t.Logf("found %d method nodes across ripgrep", methods)
	if methods < 50 {
		t.Errorf("suspiciously few methods (%d) — impl block walking may be broken", methods)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// gRPC protos — streaming RPCs
// ──────────────────────────────────────────────────────────────────────────────

func TestRealWorld_Protobuf_gRPC(t *testing.T) {
	dir := "/tmp/rw_grpc/examples/protos"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skip("grpc not cloned")
	}
	g := parseDir(t, parser.NewProtobufParser(), dir)

	// hellostreamingworld.proto has streaming RPCs
	requireSymbol(t, g, "MultiGreeter")
	requireSymbol(t, g, "MultiGreeter.SayHello")
	requireMeta(t, g, "MultiGreeter.SayHello", "streams_response", "true")

	// route_guide.proto has all 4 RPC types
	requireSymbol(t, g, "RouteGuide")
	requireSymbol(t, g, "RouteGuide.GetFeature")   // unary
	requireSymbol(t, g, "RouteGuide.ListFeatures") // server streaming
	requireSymbol(t, g, "RouteGuide.RecordRoute")  // client streaming
	requireSymbol(t, g, "RouteGuide.RouteChat")    // bidi streaming
	requireMeta(t, g, "RouteGuide.ListFeatures", "streams_response", "true")
	requireMeta(t, g, "RouteGuide.RecordRoute", "streams_request", "true")
	requireMeta(t, g, "RouteGuide.RouteChat", "streams_request", "true")
	requireMeta(t, g, "RouteGuide.RouteChat", "streams_response", "true")
}

// ──────────────────────────────────────────────────────────────────────────────
// Ktor — Kotlin with suspend functions and data classes
// ──────────────────────────────────────────────────────────────────────────────

func TestRealWorld_Kotlin_Ktor(t *testing.T) {
	// Use a focused sub-module to avoid parsing 2000+ files
	dir := "/tmp/rw_ktor/ktor-http/common/src"
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skip("ktor not cloned or path changed")
	}
	g := parseDir(t, parser.NewKotlinParser(), dir)

	// Key classes in ktor-http
	requireSymbol(t, g, "HttpStatusCode")
	requireSymbol(t, g, "ContentType")

	// Count suspend functions
	suspendCount := 0
	for _, n := range g.AllNodes() {
		// suspend functions are regular NodeFunction/NodeMethod with no special meta
		// The key test: are there methods on real classes?
		if n.Type == graph.NodeMethod {
			suspendCount++
		}
	}
	t.Logf("found %d method nodes in ktor-http", suspendCount)
	if suspendCount < 10 {
		t.Errorf("suspiciously few methods (%d) in ktor-http", suspendCount)
	}

	// Data class companion member
	requireSymbol(t, g, "HttpStatusCode.OK")
}
