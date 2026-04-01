package parser_test

import (
    "testing"
    "github.com/SynapsesOS/synapses/internal/graph"
    "github.com/SynapsesOS/synapses/internal/parser"
)

func TestGoParser_StructMethodDefinesEdges(t *testing.T) {
    g := graph.New("/tmp/test")
    p := parser.NewGoParser()
    src := []byte(`package gin

type Context struct {
    index int
}

func (c *Context) JSON(code int, obj any) {}
func (c *Context) Next() {}
func (c *Context) Render(code int, r any) {}
`)
    if err := p.Parse(g, "/tmp/test/context.go", src); err != nil {
        t.Fatalf("parse error: %v", err)
    }
    
    nodes := g.FindByName("Context")
    var ctxNode *graph.Node
    for _, n := range nodes {
        if n.Type == graph.NodeStruct {
            ctxNode = n
        }
    }
    if ctxNode == nil {
        t.Fatal("Context struct node not found")
    }
    
    var methodNames []string
    for _, e := range g.OutEdges(ctxNode.ID) {
        if e.Type == graph.EdgeDefines {
            target := g.GetNode(e.To)
            if target != nil && target.Type == graph.NodeMethod {
                methodNames = append(methodNames, target.Name)
            }
        }
    }
    
    if len(methodNames) != 3 {
        t.Errorf("expected 3 DEFINES edges from Context struct to methods, got %d: %v", len(methodNames), methodNames)
    }
}
