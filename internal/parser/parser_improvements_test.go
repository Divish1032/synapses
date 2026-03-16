package parser_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// parseOne parses src using the given parser and returns the populated graph.
func parseOne(p interface {
	Parse(*graph.Graph, string, []byte) error
}, filename, src string) (*graph.Graph, error) {
	g := graph.New("test")
	err := p.Parse(g, filename, []byte(strings.TrimSpace(src)))
	return g, err
}

// ────────────────────────────────────────────────────────────────────────────
// 1. Elixir — OTP/Phoenix behaviour callback injection
// ────────────────────────────────────────────────────────────────────────────

func TestElixir_OTPBehaviourCallbacks_GenServer(t *testing.T) {
	src := `
defmodule MyApp.Worker do
  use GenServer

  def start_link(opts) do
    GenServer.start_link(__MODULE__, opts)
  end

  def init(state) do
    {:ok, state}
  end
end
`
	g, err := parseOne(parser.NewElixirParser(), "worker.ex", src)
	if err != nil {
		t.Fatal(err)
	}

	// Module should be tagged with behaviours = "GenServer"
	modID := g.MakeNodeID("worker.ex", "MyApp.Worker")
	modNode := g.GetNode(modID)
	if modNode == nil {
		t.Fatal("MyApp.Worker module node missing")
	}
	if modNode.Metadata == nil || modNode.Metadata["behaviours"] != "GenServer" {
		t.Errorf("expected behaviours=GenServer on module, got %v", modNode.Metadata)
	}

	// Virtual callbacks should be injected
	for _, cb := range []string{"handle_call", "handle_cast", "handle_info", "terminate"} {
		cbID := g.MakeNodeID("worker.ex", cb)
		if g.GetNode(cbID) == nil {
			t.Errorf("expected virtual callback %q to be injected", cb)
		} else if g.GetNode(cbID).Metadata == nil || g.GetNode(cbID).Metadata["kind"] != "behaviour_callback" {
			t.Errorf("callback %q missing kind=behaviour_callback, got %v", cb, g.GetNode(cbID).Metadata)
		}
	}

	// init/1 was explicitly defined — should not be duplicated
	count := 0
	for _, n := range g.AllNodes() {
		if n.Name == "init" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 'init' node, got %d", count)
	}
}

func TestElixir_OTPBehaviourCallbacks_Plug(t *testing.T) {
	src := `
defmodule MyApp.Router do
  use Plug.Router

  plug :match
  plug :dispatch
end
`
	g, err := parseOne(parser.NewElixirParser(), "router.ex", src)
	if err != nil {
		t.Fatal(err)
	}

	for _, cb := range []string{"init", "call"} {
		cbID := g.MakeNodeID("router.ex", cb)
		if g.GetNode(cbID) == nil {
			t.Errorf("expected Plug.Router callback %q to be injected", cb)
		}
	}
}

func TestElixir_OTPBehaviourCallbacks_PhoenixLiveView(t *testing.T) {
	src := `
defmodule MyAppWeb.CounterLive do
  use Phoenix.LiveView

  def render(assigns) do
    ~H"""<div><%= @count %></div>"""
  end
end
`
	g, err := parseOne(parser.NewElixirParser(), "counter_live.ex", src)
	if err != nil {
		t.Fatal(err)
	}

	for _, cb := range []string{"mount", "handle_event", "handle_info"} {
		cbID := g.MakeNodeID("counter_live.ex", cb)
		if g.GetNode(cbID) == nil {
			t.Errorf("expected LiveView callback %q to be injected", cb)
		}
	}

	// render was explicitly defined — must not be duplicated
	count := 0
	for _, n := range g.AllNodes() {
		if n.Name == "render" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 'render' node, got %d", count)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// 2. Lua — LuaCATS annotation pre-pass
// ────────────────────────────────────────────────────────────────────────────

func TestLua_LuaCATS_ClassAndFields(t *testing.T) {
	src := `
---@class Vector2
---@field x number The x component
---@field y number The y component
local Vector2 = {}

---@class Color : Vector2
---@field r number
---@field g number
---@field b number
local Color = {}

---@alias Callback fun(event: string): void

function Vector2.new(x, y)
  return setmetatable({x=x, y=y}, Vector2)
end
`
	g, err := parseOne(parser.NewLuaParser(), "vec.lua", src)
	if err != nil {
		t.Fatal(err)
	}

	// Vector2 class node
	v2ID := g.MakeNodeID("vec.lua", "Vector2")
	if g.GetNode(v2ID) == nil {
		t.Fatal("Vector2 class node missing")
	}
	if g.GetNode(v2ID).Metadata["kind"] != "class" {
		t.Errorf("expected kind=class on Vector2, got %v", g.GetNode(v2ID).Metadata)
	}

	// Vector2 fields
	for _, field := range []string{"Vector2.x", "Vector2.y"} {
		id := g.MakeNodeID("vec.lua", field)
		if g.GetNode(id) == nil {
			t.Errorf("expected field node %q", field)
		}
	}

	// Color with parent
	colorID := g.MakeNodeID("vec.lua", "Color")
	if g.GetNode(colorID) == nil {
		t.Fatal("Color class node missing")
	}
	if g.GetNode(colorID).Metadata["extends"] != "Vector2" {
		t.Errorf("expected extends=Vector2 on Color, got %v", g.GetNode(colorID).Metadata)
	}
	for _, field := range []string{"Color.r", "Color.g", "Color.b"} {
		id := g.MakeNodeID("vec.lua", field)
		if g.GetNode(id) == nil {
			t.Errorf("expected Color field node %q", field)
		}
	}

	// Alias
	cbID := g.MakeNodeID("vec.lua", "Callback")
	if g.GetNode(cbID) == nil {
		t.Fatal("Callback alias node missing")
	}
	if g.GetNode(cbID).Metadata["kind"] != "alias" {
		t.Errorf("expected kind=alias, got %v", g.GetNode(cbID).Metadata)
	}
}

func TestLua_LuaCATS_VisibilityModifiers(t *testing.T) {
	src := `
---@class MyService
---@field public name string
---@field protected _id integer
---@field private _secret string
`
	g, err := parseOne(parser.NewLuaParser(), "service.lua", src)
	if err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{"MyService.name", "MyService._id", "MyService._secret"} {
		id := g.MakeNodeID("service.lua", field)
		if g.GetNode(id) == nil {
			t.Errorf("expected field %q with visibility modifier", field)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────────
// 3. Ruby — .rbi extension + RBS parser
// ────────────────────────────────────────────────────────────────────────────

func TestRuby_RBI_Extension(t *testing.T) {
	src := `
class PaymentService
  def charge(amount); end
  def refund; end
end
`
	g, err := parseOne(parser.NewRubyParser(), "payment_service.rbi", src)
	if err != nil {
		t.Fatal(err)
	}

	classID := g.MakeNodeID("payment_service.rbi", "PaymentService")
	if g.GetNode(classID) == nil {
		t.Fatal("PaymentService class missing from .rbi file")
	}
	chargeID := g.MakeNodeID("payment_service.rbi", "PaymentService.charge")
	if g.GetNode(chargeID) == nil {
		t.Error("PaymentService.charge method missing from .rbi file")
	}
}

func TestRuby_RBS_Parser(t *testing.T) {
	src := `
class Animal
  attr_accessor name: String
  def speak: () -> String
  def self.create: (String name) -> Animal
end

module Walkable
  def walk: () -> void
end

interface _Serializable
  def to_json: () -> String
end

type Color = :red | :green | :blue
`
	g, err := parseOne(parser.NewRBSParser(), "animal.rbs", src)
	if err != nil {
		t.Fatal(err)
	}

	// class
	animalID := g.MakeNodeID("animal.rbs", "Animal")
	if g.GetNode(animalID) == nil {
		t.Fatal("Animal class missing")
	}
	if g.GetNode(animalID).Metadata["kind"] != "class" {
		t.Errorf("expected kind=class, got %v", g.GetNode(animalID).Metadata)
	}

	// module
	if g.GetNode(g.MakeNodeID("animal.rbs", "Walkable")) == nil {
		t.Error("Walkable module missing")
	}

	// interface
	serID := g.MakeNodeID("animal.rbs", "_Serializable")
	if g.GetNode(serID) == nil {
		t.Fatal("_Serializable interface missing")
	}
	if g.GetNode(serID).Metadata["kind"] != "interface" {
		t.Errorf("expected kind=interface, got %v", g.GetNode(serID).Metadata)
	}

	// type alias
	colorID := g.MakeNodeID("animal.rbs", "Color")
	if g.GetNode(colorID) == nil {
		t.Fatal("Color type alias missing")
	}
	if g.GetNode(colorID).Metadata["kind"] != "alias" {
		t.Errorf("expected kind=alias, got %v", g.GetNode(colorID).Metadata)
	}

	// instance method
	if g.GetNode(g.MakeNodeID("animal.rbs", "Animal.speak")) == nil {
		t.Error("Animal.speak method missing")
	}

	// singleton method
	createID := g.MakeNodeID("animal.rbs", "Animal.create")
	if g.GetNode(createID) == nil {
		t.Error("Animal.create singleton method missing")
	} else if g.GetNode(createID).Metadata["kind"] != "singleton" {
		t.Errorf("expected kind=singleton on Animal.create, got %v", g.GetNode(createID).Metadata)
	}

	// attr
	if g.GetNode(g.MakeNodeID("animal.rbs", "Animal.name")) == nil {
		t.Error("Animal.name attr missing")
	}
}

// ────────────────────────────────────────────────────────────────────────────
// 4. C/C++ — all-branch #ifdef walk + guard annotation
// ────────────────────────────────────────────────────────────────────────────

func TestC_Ifdef_AllBranches(t *testing.T) {
	src := `
#ifdef _WIN32
int win_only_func(int x) { return x; }
struct WinHandle { int h; };
#else
int posix_only_func(int y) { return y; }
struct PosixHandle { int fd; };
#endif

void common_func(void) {}
`
	g, err := parseOne(parser.NewCParser(), "platform.c", src)
	if err != nil {
		t.Fatal(err)
	}

	// ALL branches must be present
	for _, name := range []string{"win_only_func", "posix_only_func", "common_func"} {
		if g.GetNode(g.MakeNodeID("platform.c", name)) == nil {
			t.Errorf("expected function %q from all ifdef branches", name)
		}
	}
	for _, name := range []string{"WinHandle", "PosixHandle"} {
		if g.GetNode(g.MakeNodeID("platform.c", name)) == nil {
			t.Errorf("expected struct %q from all ifdef branches", name)
		}
	}

	// win_only_func → meta["ifdef"] = "_WIN32"
	winNode := g.GetNode(g.MakeNodeID("platform.c", "win_only_func"))
	if winNode == nil {
		t.Fatal("win_only_func missing")
	}
	if winNode.Metadata == nil || winNode.Metadata["ifdef"] != "_WIN32" {
		t.Errorf("expected meta[ifdef]=_WIN32 on win_only_func, got %v", winNode.Metadata)
	}

	// posix_only_func is in #else → meta["ifdef"] = "!_WIN32"
	posixNode := g.GetNode(g.MakeNodeID("platform.c", "posix_only_func"))
	if posixNode == nil {
		t.Fatal("posix_only_func missing")
	}
	if posixNode.Metadata == nil || posixNode.Metadata["ifdef"] != "!_WIN32" {
		t.Errorf("expected meta[ifdef]=!_WIN32 on posix_only_func, got %v", posixNode.Metadata)
	}

	// common_func — no guard
	commonNode := g.GetNode(g.MakeNodeID("platform.c", "common_func"))
	if commonNode == nil {
		t.Fatal("common_func missing")
	}
	if commonNode.Metadata != nil && commonNode.Metadata["ifdef"] != "" {
		t.Errorf("expected no ifdef guard on common_func, got %v", commonNode.Metadata["ifdef"])
	}
}

func TestCpp_Ifdef_AllBranches(t *testing.T) {
	src := `
#ifdef __linux__
class LinuxSocket {
public:
  void connect();
};
#else
class WinSocket {
public:
  void connect();
};
#endif

class CommonUtil {
  void helper() {}
};
`
	g, err := parseOne(parser.NewCppParser(), "socket.cpp", src)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"LinuxSocket", "WinSocket", "CommonUtil"} {
		if g.GetNode(g.MakeNodeID("socket.cpp", name)) == nil {
			t.Errorf("expected class %q from all ifdef branches", name)
		}
	}

	linuxNode := g.GetNode(g.MakeNodeID("socket.cpp", "LinuxSocket"))
	if linuxNode == nil {
		t.Fatal("LinuxSocket missing")
	}
	if linuxNode.Metadata == nil || linuxNode.Metadata["ifdef"] == "" {
		t.Errorf("expected ifdef guard on LinuxSocket, got %v", linuxNode.Metadata)
	}
}

// ────────────────────────────────────────────────────────────────────────────
// 5. Groovy — Gradle DSL detection
// ────────────────────────────────────────────────────────────────────────────

func TestGroovy_GradleDSL_Tasks(t *testing.T) {
	src := `
plugins {
    id 'java'
    id 'org.springframework.boot' version '3.0.0'
}

dependencies {
    implementation 'org.springframework.boot:spring-boot-starter-web:3.0.0'
    testImplementation 'org.junit.jupiter:junit-jupiter:5.9.0'
}

task fatJar(type: Jar) {
    archiveClassifier = 'all'
}

task clean {
    doLast { delete buildDir }
}
`
	g, err := parseOne(parser.NewGroovyParser(), "build.gradle", src)
	if err != nil {
		t.Fatal(err)
	}

	// Tasks
	fatJarNode := g.GetNode(g.MakeNodeID("build.gradle", "fatJar"))
	if fatJarNode == nil {
		t.Error("fatJar task missing")
	} else {
		if fatJarNode.Metadata["kind"] != "gradle_task" {
			t.Errorf("expected kind=gradle_task on fatJar, got %v", fatJarNode.Metadata)
		}
		if fatJarNode.Metadata["task_type"] != "Jar" {
			t.Errorf("expected task_type=Jar on fatJar, got %v", fatJarNode.Metadata)
		}
	}
	if g.GetNode(g.MakeNodeID("build.gradle", "clean")) == nil {
		t.Error("clean task missing")
	}

	// Plugin IDs
	if g.GetNode(g.MakeNodeID("java", "java")) == nil {
		t.Error("'java' plugin node missing")
	}
	if g.GetNode(g.MakeNodeID("org.springframework.boot", "org.springframework.boot")) == nil {
		t.Error("spring boot plugin node missing")
	}

	// Dependencies
	webNode := g.GetNode(g.MakeNodeID("org.springframework.boot:spring-boot-starter-web", "org.springframework.boot:spring-boot-starter-web"))
	if webNode == nil {
		t.Error("spring-boot-starter-web dependency node missing")
	} else if webNode.Metadata["version"] != "3.0.0" {
		t.Errorf("expected version=3.0.0, got %v", webNode.Metadata["version"])
	}
	if g.GetNode(g.MakeNodeID("org.junit.jupiter:junit-jupiter", "org.junit.jupiter:junit-jupiter")) == nil {
		t.Error("junit-jupiter dependency node missing")
	}
}

func TestGroovy_GradleDSL_ParenthesisForm(t *testing.T) {
	src := `
dependencies {
    implementation("com.google.guava:guava:31.1-jre")
    api("io.ktor:ktor-server-core:2.3.0")
}

task("generateSources") {
    doFirst { println("Generating...") }
}
`
	g, err := parseOne(parser.NewGroovyParser(), "build.gradle", src)
	if err != nil {
		t.Fatal(err)
	}

	if g.GetNode(g.MakeNodeID("com.google.guava:guava", "com.google.guava:guava")) == nil {
		t.Error("guava dependency missing from parenthesized form")
	}
	if g.GetNode(g.MakeNodeID("build.gradle", "generateSources")) == nil {
		t.Error("generateSources task missing from task(\"name\") form")
	}
}
