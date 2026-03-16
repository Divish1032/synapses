package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func TestAudit_TSDecorators_Metadata(t *testing.T) {
	src := []byte("@Component({ selector: 'app-root' })\nclass AppComponent {}\n\n@Injectable()\nexport class UserService {}\n")
	g := graph.New("r")
	if err := parser.NewTypeScriptParser().Parse(g, "app.ts", src); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ class, wantDec string }{{"AppComponent", "Component"}, {"UserService", "Injectable"}} {
		nodes := g.FindByName(tc.class)
		if len(nodes) == 0 {
			t.Errorf("MISSING class: %q", tc.class)
			continue
		}
		if nodes[0].Metadata == nil || nodes[0].Metadata["decorators"] != tc.wantDec {
			t.Errorf("class %q: want decorator=%q, got metadata=%v", tc.class, tc.wantDec, nodes[0].Metadata)
		}
	}
}

func TestAudit_Protobuf_ExtendBlocks(t *testing.T) {
	src := []byte("syntax = \"proto2\";\nextend google.protobuf.FieldOptions {\n  optional bool sensitive = 50000;\n  optional string category = 50001;\n}\n")
	g := graph.New("r")
	if err := parser.NewProtobufParser().Parse(g, "ext.proto", src); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"google.protobuf.FieldOptions.sensitive", "google.protobuf.FieldOptions.category"} {
		if nodes := g.FindByName(field); len(nodes) == 0 {
			t.Errorf("MISSING extend field: %q", field)
		}
	}
}

func TestAudit_Scala3_Given(t *testing.T) {
	src := []byte("given intOrdering: Ordering[Int] = Ordering.Int\ngiven stringOrdering: Ordering[String] = Ordering.String\ndef sort[A](xs: List[A])(using ord: Ordering[A]): List[A] = xs.sorted\n")
	g := graph.New("r")
	if err := parser.NewScalaParser().Parse(g, "ord.scala", src); err != nil {
		t.Fatal(err)
	}
	for _, sym := range []string{"intOrdering", "stringOrdering", "sort"} {
		nodes := g.FindByName(sym)
		if len(nodes) == 0 {
			t.Errorf("MISSING Scala 3 symbol: %q", sym)
			continue
		}
		if sym != "sort" && (nodes[0].Metadata == nil || nodes[0].Metadata["kind"] != "given") {
			t.Errorf("%q: want kind=given, got metadata=%v", sym, nodes[0].Metadata)
		}
	}
}

func TestAudit_Elixir_UseGenServer(t *testing.T) {
	src := []byte("defmodule MyServer do\n  use GenServer\n\n  def start_link(opts) do\n    GenServer.start_link(__MODULE__, opts)\n  end\nend\n")
	g := graph.New("r")
	if err := parser.NewElixirParser().Parse(g, "server.ex", src); err != nil {
		t.Fatal(err)
	}
	if nodes := g.FindByName("MyServer"); len(nodes) == 0 {
		t.Errorf("MISSING module: MyServer")
	}
	// use GenServer must emit an EdgeImports to GenServer node
	if nodes := g.FindByName("GenServer"); len(nodes) == 0 {
		t.Errorf("MISSING GenServer import (use GenServer must emit EdgeImports)")
	}
	if nodes := g.FindByName("start_link"); len(nodes) == 0 {
		t.Errorf("MISSING function: start_link")
	}
}
