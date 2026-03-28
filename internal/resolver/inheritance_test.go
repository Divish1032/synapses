package resolver_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/resolver"
)

// TestInheritanceMethodResolution_JavaSuperCall verifies that super.method()
// resolves to the parent class method via the inheritance chain.
func TestInheritanceMethodResolution_JavaSuperCall(t *testing.T) {
	g := graph.New("test")

	// Parent class with method doWork
	parentID := g.MakeNodeID("Base.java", "BaseService")
	parentMethodID := g.MakeNodeID("Base.java", "BaseService.doWork")
	fileBase := g.MakeNodeID("Base.java", "Base.java")

	g.AddNode(&graph.Node{ID: fileBase, Type: graph.NodeFile, Name: "Base.java", File: "Base.java"})
	g.AddNode(&graph.Node{
		ID: parentID, Type: graph.NodeStruct, Name: "BaseService",
		Package: "com.app", File: "Base.java",
	})
	g.AddNode(&graph.Node{
		ID: parentMethodID, Type: graph.NodeMethod, Name: "BaseService.doWork",
		Package: "com.app", File: "Base.java",
	})
	g.AddEdge(&graph.Edge{From: fileBase, To: parentMethodID, Type: graph.EdgeDefines})

	// Child class extends BaseService, has a method that calls super.doWork()
	childID := g.MakeNodeID("Child.java", "ChildService")
	childMethodID := g.MakeNodeID("Child.java", "ChildService.execute")
	fileChild := g.MakeNodeID("Child.java", "Child.java")

	g.AddNode(&graph.Node{ID: fileChild, Type: graph.NodeFile, Name: "Child.java", File: "Child.java"})
	g.AddNode(&graph.Node{
		ID: childID, Type: graph.NodeStruct, Name: "ChildService",
		Package: "com.app", File: "Child.java",
		Metadata: map[string]string{"heritage_extends": "BaseService"},
	})
	g.AddNode(&graph.Node{
		ID: childMethodID, Type: graph.NodeMethod, Name: "ChildService.execute",
		Package: "com.app", File: "Child.java",
	})
	g.AddEdge(&graph.Edge{From: fileChild, To: childMethodID, Type: graph.EdgeDefines})

	// Call site: super.doWork() from ChildService.execute
	g.AddCallSite(graph.CallSite{
		CallerID:   childMethodID,
		CallerFile: "Child.java",
		PkgAlias:   "super",
		FuncName:   "doWork",
	})

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 resolved edge (super.doWork → BaseService.doWork), got %d", count)
	}

	if !g.HasEdge(childMethodID, parentMethodID, graph.EdgeCalls) {
		t.Error("expected CALLS edge ChildService.execute → BaseService.doWork")
	}
}

// TestInheritanceMethodResolution_ThisInherited verifies that this.method()
// resolves to an inherited method when the current class doesn't define it.
func TestInheritanceMethodResolution_ThisInherited(t *testing.T) {
	g := graph.New("test")

	fileBase := g.MakeNodeID("Base.java", "Base.java")
	g.AddNode(&graph.Node{ID: fileBase, Type: graph.NodeFile, Name: "Base.java", File: "Base.java"})

	parentID := g.MakeNodeID("Base.java", "Animal")
	parentMethodID := g.MakeNodeID("Base.java", "Animal.breathe")
	g.AddNode(&graph.Node{
		ID: parentID, Type: graph.NodeStruct, Name: "Animal",
		Package: "zoo", File: "Base.java",
	})
	g.AddNode(&graph.Node{
		ID: parentMethodID, Type: graph.NodeMethod, Name: "Animal.breathe",
		Package: "zoo", File: "Base.java",
	})
	g.AddEdge(&graph.Edge{From: fileBase, To: parentMethodID, Type: graph.EdgeDefines})

	fileChild := g.MakeNodeID("Dog.java", "Dog.java")
	g.AddNode(&graph.Node{ID: fileChild, Type: graph.NodeFile, Name: "Dog.java", File: "Dog.java"})

	childID := g.MakeNodeID("Dog.java", "Dog")
	childMethodID := g.MakeNodeID("Dog.java", "Dog.bark")
	g.AddNode(&graph.Node{
		ID: childID, Type: graph.NodeStruct, Name: "Dog",
		Package: "zoo", File: "Dog.java",
		Metadata: map[string]string{"heritage_extends": "Animal"},
	})
	g.AddNode(&graph.Node{
		ID: childMethodID, Type: graph.NodeMethod, Name: "Dog.bark",
		Package: "zoo", File: "Dog.java",
	})
	g.AddEdge(&graph.Edge{From: fileChild, To: childMethodID, Type: graph.EdgeDefines})

	// Call site: this.breathe() from Dog.bark — breathe is on Animal, not Dog.
	g.AddCallSite(graph.CallSite{
		CallerID:   childMethodID,
		CallerFile: "Dog.java",
		PkgAlias:   "this",
		FuncName:   "breathe",
	})

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 resolved edge (this.breathe → Animal.breathe), got %d", count)
	}

	if !g.HasEdge(childMethodID, parentMethodID, graph.EdgeCalls) {
		t.Error("expected CALLS edge Dog.bark → Animal.breathe via inheritance")
	}
}

// TestInheritanceMethodResolution_MultiLevel verifies resolution through
// grandparent: C extends B extends A, calling A's method from C.
func TestInheritanceMethodResolution_MultiLevel(t *testing.T) {
	g := graph.New("test")

	fileA := g.MakeNodeID("A.java", "A.java")
	g.AddNode(&graph.Node{ID: fileA, Type: graph.NodeFile, Name: "A.java", File: "A.java"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("A.java", "GrandParent"), Type: graph.NodeStruct,
		Name: "GrandParent", Package: "app", File: "A.java",
	})
	gpMethodID := g.MakeNodeID("A.java", "GrandParent.legacy")
	g.AddNode(&graph.Node{
		ID: gpMethodID, Type: graph.NodeMethod,
		Name: "GrandParent.legacy", Package: "app", File: "A.java",
	})
	g.AddEdge(&graph.Edge{From: fileA, To: gpMethodID, Type: graph.EdgeDefines})

	fileB := g.MakeNodeID("B.java", "B.java")
	g.AddNode(&graph.Node{ID: fileB, Type: graph.NodeFile, Name: "B.java", File: "B.java"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("B.java", "Parent"), Type: graph.NodeStruct,
		Name: "Parent", Package: "app", File: "B.java",
		Metadata: map[string]string{"heritage_extends": "GrandParent"},
	})

	fileC := g.MakeNodeID("C.java", "C.java")
	g.AddNode(&graph.Node{ID: fileC, Type: graph.NodeFile, Name: "C.java", File: "C.java"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("C.java", "Child"), Type: graph.NodeStruct,
		Name: "Child", Package: "app", File: "C.java",
		Metadata: map[string]string{"heritage_extends": "Parent"},
	})
	childMethodID := g.MakeNodeID("C.java", "Child.run")
	g.AddNode(&graph.Node{
		ID: childMethodID, Type: graph.NodeMethod,
		Name: "Child.run", Package: "app", File: "C.java",
	})
	g.AddEdge(&graph.Edge{From: fileC, To: childMethodID, Type: graph.EdgeDefines})

	// Call site: this.legacy() from Child.run — should resolve through Parent → GrandParent
	g.AddCallSite(graph.CallSite{
		CallerID:   childMethodID,
		CallerFile: "C.java",
		PkgAlias:   "this",
		FuncName:   "legacy",
	})

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 resolved edge (multi-level inheritance), got %d", count)
	}

	if !g.HasEdge(childMethodID, gpMethodID, graph.EdgeCalls) {
		t.Error("expected CALLS edge Child.run → GrandParent.legacy via multi-level inheritance")
	}
}

// TestInheritanceMethodResolution_VarTypeInherited verifies that obj.method()
// resolves via inheritance when the declared type doesn't define the method.
func TestInheritanceMethodResolution_VarTypeInherited(t *testing.T) {
	g := graph.New("test")

	fileBase := g.MakeNodeID("Base.java", "Base.java")
	g.AddNode(&graph.Node{ID: fileBase, Type: graph.NodeFile, Name: "Base.java", File: "Base.java"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("Base.java", "Repository"), Type: graph.NodeStruct,
		Name: "Repository", Package: "dao", File: "Base.java",
	})
	repoMethodID := g.MakeNodeID("Base.java", "Repository.save")
	g.AddNode(&graph.Node{
		ID: repoMethodID, Type: graph.NodeMethod,
		Name: "Repository.save", Package: "dao", File: "Base.java",
	})
	g.AddEdge(&graph.Edge{From: fileBase, To: repoMethodID, Type: graph.EdgeDefines})

	fileChild := g.MakeNodeID("UserRepo.java", "UserRepo.java")
	g.AddNode(&graph.Node{ID: fileChild, Type: graph.NodeFile, Name: "UserRepo.java", File: "UserRepo.java"})
	g.AddNode(&graph.Node{
		ID: g.MakeNodeID("UserRepo.java", "UserRepository"), Type: graph.NodeStruct,
		Name: "UserRepository", Package: "dao", File: "UserRepo.java",
		Metadata: map[string]string{"heritage_extends": "Repository"},
	})

	// Caller in a service file
	fileSvc := g.MakeNodeID("Svc.java", "Svc.java")
	g.AddNode(&graph.Node{ID: fileSvc, Type: graph.NodeFile, Name: "Svc.java", File: "Svc.java"})
	callerID := g.MakeNodeID("Svc.java", "UserService.create")
	g.AddNode(&graph.Node{
		ID: callerID, Type: graph.NodeMethod,
		Name: "UserService.create", Package: "svc", File: "Svc.java",
	})
	g.AddEdge(&graph.Edge{From: fileSvc, To: callerID, Type: graph.EdgeDefines})

	// Var type: repo has type UserRepository
	g.AddVarType("Svc.java", "repo", "UserRepository")

	// Call site: repo.save() — save is on Repository, not UserRepository
	g.AddCallSite(graph.CallSite{
		CallerID:   callerID,
		CallerFile: "Svc.java",
		PkgAlias:   "repo",
		FuncName:   "save",
	})

	count := resolver.ResolveCallEdges(g)
	if count != 1 {
		t.Fatalf("expected 1 resolved edge (var type + inheritance), got %d", count)
	}

	if !g.HasEdge(callerID, repoMethodID, graph.EdgeCalls) {
		t.Error("expected CALLS edge UserService.create → Repository.save via var type + inheritance")
	}
}
