package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// Rust: impl Trait for Type — are methods attributed to the struct?
func TestHonest_Rust_ImplTraitForType(t *testing.T) {
	src := []byte(`use std::fmt;
struct Point { x: f64, y: f64 }
impl fmt::Display for Point {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "({}, {})", self.x, self.y)
    }
}
impl Point {
    fn distance(&self) -> f64 { (self.x*self.x + self.y*self.y).sqrt() }
}
`)
	g := graph.New("r")
	if err := parser.NewRustParser().Parse(g, "point.rs", src); err != nil {
		t.Fatal(err)
	}
	// fmt method from trait impl — should be linked to Point
	if nodes := g.FindByName("Point.fmt"); len(nodes) == 0 {
		t.Errorf("MISSING trait impl method: Point.fmt (impl Display for Point)")
	}
	// own impl method — must work
	if nodes := g.FindByName("Point.distance"); len(nodes) == 0 {
		t.Errorf("MISSING impl method: Point.distance")
	}
}

// Scala 3: enum syntax — names and case values
func TestHonest_Scala3_Enum(t *testing.T) {
	src := []byte(`enum Color:
  case Red
  case Green
  case Blue

enum Direction:
  case North, South, East, West
`)
	g := graph.New("r")
	if err := parser.NewScalaParser().Parse(g, "color.scala", src); err != nil {
		t.Fatal(err)
	}
	for _, sym := range []string{
		"Color", "Color.Red", "Color.Green", "Color.Blue",
		"Direction", "Direction.North", "Direction.South", "Direction.East", "Direction.West",
	} {
		if nodes := g.FindByName(sym); len(nodes) == 0 {
			t.Errorf("MISSING: %q", sym)
		}
	}
}

// Protobuf: streaming RPC metadata
func TestHonest_Protobuf_StreamingRPC(t *testing.T) {
	src := []byte(`syntax = "proto3";
service StreamService {
    rpc ServerStream(Request) returns (stream Response);
    rpc BidiStream(stream Request) returns (stream Response);
}
`)
	g := graph.New("r")
	if err := parser.NewProtobufParser().Parse(g, "svc.proto", src); err != nil {
		t.Fatal(err)
	}
	nodes := g.FindByName("StreamService.ServerStream")
	if len(nodes) == 0 {
		t.Errorf("MISSING streaming RPC: StreamService.ServerStream")
	} else {
		meta := nodes[0].Metadata
		if meta["streams_response"] != "true" {
			t.Errorf("ServerStream: want streams_response=true, got metadata=%v", meta)
		}
	}
	nodes = g.FindByName("StreamService.BidiStream")
	if len(nodes) == 0 {
		t.Errorf("MISSING streaming RPC: StreamService.BidiStream")
	} else {
		meta := nodes[0].Metadata
		if meta["streams_request"] != "true" || meta["streams_response"] != "true" {
			t.Errorf("BidiStream: want both stream flags, got metadata=%v", meta)
		}
	}
}

// C#: extension methods should have kind=extension in metadata
func TestHonest_CSharp_ExtensionMethod(t *testing.T) {
	src := []byte(`public static class StringExtensions
{
    public static string ToSlug(this string s) => s.ToLower().Replace(" ", "-");
    public static bool IsEmpty(this string s) => string.IsNullOrEmpty(s);
}
`)
	g := graph.New("r")
	if err := parser.NewCSharpParser().Parse(g, "ext.cs", src); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"StringExtensions.ToSlug", "StringExtensions.IsEmpty"} {
		nodes := g.FindByName(m)
		if len(nodes) == 0 {
			t.Errorf("MISSING extension method: %q", m)
			continue
		}
		if nodes[0].Metadata == nil || nodes[0].Metadata["kind"] != "extension" {
			t.Errorf("%q: want kind=extension, got metadata=%v", m, nodes[0].Metadata)
		}
	}
}
