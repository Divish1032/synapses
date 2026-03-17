package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

func TestObjCParser(t *testing.T) {
	src := []byte(`
#import <Foundation/Foundation.h>
#import "MyHelper.h"

@interface MyClass : NSObject <NSCopying>
@property (nonatomic, strong) NSString *name;
@property (nonatomic, assign) NSInteger count;
- (instancetype)initWithName:(NSString *)name count:(NSInteger)count;
- (void)doWork;
+ (instancetype)sharedInstance;
@end

@implementation MyClass
- (void)doWork { }
@end

@protocol MyProtocol <NSObject>
- (void)requiredMethod;
@end
`)

	p := parser.NewObjCParser()
	if exts := p.Extensions(); len(exts) != 1 || exts[0] != ".m" {
		t.Errorf("expected extensions [.m], got %v", exts)
	}

	g := graph.New("testrepo")
	if err := p.Parse(g, "/tmp/MyClass.m", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// nodeExists returns true if a node with the given name and type exists in the graph.
	nodeExists := func(name string, nodeType graph.NodeType) bool {
		nodes := g.FindByName(name)
		for _, n := range nodes {
			if n.Type == nodeType {
				return true
			}
		}
		return false
	}

	// Check expected nodes.
	tests := []struct {
		name     string
		nodeType graph.NodeType
	}{
		{"MyClass.m", graph.NodeFile},
		{"Foundation/Foundation.h", graph.NodePackage},
		{"MyHelper.h", graph.NodePackage},
		{"MyClass", graph.NodeStruct},
		{"MyClass.name", graph.NodeMethod},
		{"MyClass.count", graph.NodeMethod},
		{"MyClass.initWithName:count:", graph.NodeMethod},
		{"MyClass.doWork", graph.NodeMethod},
		{"MyClass.sharedInstance", graph.NodeMethod},
		{"MyProtocol", graph.NodeInterface},
		{"MyProtocol.requiredMethod", graph.NodeMethod},
	}

	for _, tt := range tests {
		if !nodeExists(tt.name, tt.nodeType) {
			t.Errorf("expected node %q (type=%s) not found", tt.name, tt.nodeType)
		}
	}

	// Verify MyClass has superclass and protocol metadata.
	if nodes := g.FindByName("MyClass"); len(nodes) > 0 {
		n := nodes[0]
		if n.Metadata["superclass"] != "NSObject" {
			t.Errorf("MyClass superclass: expected NSObject, got %q", n.Metadata["superclass"])
		}
		if n.Metadata["protocols"] != "NSCopying" {
			t.Errorf("MyClass protocols: expected NSCopying, got %q", n.Metadata["protocols"])
		}
	}

	if nodes := g.FindByName("MyClass.initWithName:count:"); len(nodes) > 0 {
		if nodes[0].Metadata["scope"] != "instance" {
			t.Errorf("initWithName:count: scope: expected instance, got %q", nodes[0].Metadata["scope"])
		}
	}

	if nodes := g.FindByName("MyClass.sharedInstance"); len(nodes) > 0 {
		if nodes[0].Metadata["scope"] != "class" {
			t.Errorf("sharedInstance scope: expected class, got %q", nodes[0].Metadata["scope"])
		}
	}
}

func TestThriftParser(t *testing.T) {
	src := []byte(`
namespace go myservice
namespace java com.example.myservice
typedef string UUID
enum Status { ACTIVE = 1, INACTIVE = 2 }
struct User { 1: required UUID id, 2: optional string name }
exception NotFoundException { 1: string message }
union MyUnion { 1: string text, 2: i32 number }
const string DEFAULT_NAME = "guest"
service UserService {
    User getUser(1: UUID id) throws (1: NotFoundException nfe),
    void deleteUser(1: UUID id),
    list<User> listUsers(),
}
`)

	p := parser.NewThriftParser()
	if exts := p.Extensions(); len(exts) != 1 || exts[0] != ".thrift" {
		t.Errorf("expected extensions [.thrift], got %v", exts)
	}

	g := graph.New("testrepo")
	if err := p.Parse(g, "/tmp/service.thrift", src); err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	// nodeExists returns true if a node with the given name and type exists in the graph.
	nodeExists := func(name string, nodeType graph.NodeType) bool {
		nodes := g.FindByName(name)
		for _, n := range nodes {
			if n.Type == nodeType {
				return true
			}
		}
		return false
	}

	// Check expected nodes.
	tests := []struct {
		name     string
		nodeType graph.NodeType
	}{
		{"service.thrift", graph.NodeFile},
		{"go.myservice", graph.NodePackage},
		{"java.com.example.myservice", graph.NodePackage},
		{"UUID", graph.NodeMethod},
		{"Status", graph.NodeStruct},
		{"Status.ACTIVE", graph.NodeMethod},
		{"Status.INACTIVE", graph.NodeMethod},
		{"User", graph.NodeStruct},
		{"User.id", graph.NodeMethod},
		{"User.name", graph.NodeMethod},
		{"NotFoundException", graph.NodeStruct},
		{"NotFoundException.message", graph.NodeMethod},
		{"MyUnion", graph.NodeStruct},
		{"MyUnion.text", graph.NodeMethod},
		{"DEFAULT_NAME", graph.NodeMethod},
		{"UserService", graph.NodeInterface},
		{"UserService.getUser", graph.NodeMethod},
		{"UserService.deleteUser", graph.NodeMethod},
		{"UserService.listUsers", graph.NodeMethod},
	}

	for _, tt := range tests {
		if !nodeExists(tt.name, tt.nodeType) {
			t.Errorf("expected node %q (type=%s) not found", tt.name, tt.nodeType)
		}
	}

	// Verify metadata.
	if nodes := g.FindByName("UUID"); len(nodes) > 0 {
		if nodes[0].Metadata["kind"] != "typedef" {
			t.Errorf("UUID kind: expected typedef, got %q", nodes[0].Metadata["kind"])
		}
	}
	if nodes := g.FindByName("Status"); len(nodes) > 0 {
		if nodes[0].Metadata["kind"] != "enum" {
			t.Errorf("Status kind: expected enum, got %q", nodes[0].Metadata["kind"])
		}
	}
	if nodes := g.FindByName("User"); len(nodes) > 0 {
		if nodes[0].Metadata["kind"] != "struct" {
			t.Errorf("User kind: expected struct, got %q", nodes[0].Metadata["kind"])
		}
	}
	if nodes := g.FindByName("NotFoundException"); len(nodes) > 0 {
		if nodes[0].Metadata["kind"] != "exception" {
			t.Errorf("NotFoundException kind: expected exception, got %q", nodes[0].Metadata["kind"])
		}
	}
	if nodes := g.FindByName("MyUnion"); len(nodes) > 0 {
		if nodes[0].Metadata["kind"] != "union" {
			t.Errorf("MyUnion kind: expected union, got %q", nodes[0].Metadata["kind"])
		}
	}
	if nodes := g.FindByName("UserService"); len(nodes) > 0 {
		if nodes[0].Metadata["kind"] != "service" {
			t.Errorf("UserService kind: expected service, got %q", nodes[0].Metadata["kind"])
		}
	}
	if nodes := g.FindByName("UserService.getUser"); len(nodes) > 0 {
		if nodes[0].Metadata["throws"] == "" {
			t.Errorf("UserService.getUser should have throws metadata")
		}
	}
	if nodes := g.FindByName("User.id"); len(nodes) > 0 {
		if nodes[0].Metadata["modifier"] != "required" {
			t.Errorf("User.id modifier: expected required, got %q", nodes[0].Metadata["modifier"])
		}
	}
	if nodes := g.FindByName("go.myservice"); len(nodes) > 0 {
		if nodes[0].Metadata["lang"] != "go" {
			t.Errorf("go.myservice lang: expected go, got %q", nodes[0].Metadata["lang"])
		}
	}
}
