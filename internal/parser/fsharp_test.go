package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── F# fixture sources ───────────────────────────────────────────────────────

// fsharpSource is a comprehensive F# file covering all constructs we extract.
const fsharpSource = `module FinancialModeling

open System
open System.Collections.Generic

/// Represents the result of a pricing computation.
type PricingResult = {
    Price: decimal
    Confidence: float
}

/// A discriminated union for order types.
type OrderType =
    | Market
    | Limit of decimal
    | StopLoss of decimal * decimal

/// Simple type alias for an account identifier.
type AccountId = string

/// Interface for pricing engines.
type IPricingEngine =
    abstract member ComputePrice: string -> decimal
    abstract member Validate: string -> bool

/// A class representing a trading session.
type TradingSession(accountId: AccountId) =
    member this.GetAccount() = accountId
    member this.IsActive() = true

/// Computes a trade price.
let computeTradePrice symbol quantity =
    symbol + string quantity

/// Recursive Fibonacci function.
let rec fibonacci n =
    if n <= 1 then n
    else fibonacci (n - 1) + fibonacci (n - 2)

/// Private helper — not exported.
let private validateSymbol symbol =
    symbol <> ""

/// Internal helper.
let internal parseQuantity qty =
    int qty

/// Explicitly public function.
let public createOrder symbol qty =
    (symbol, qty)

/// Active pattern for even/odd classification.
let (|Even|Odd|) n =
    if n % 2 = 0 then Even else Odd
`

// fsharpMinimalSource is used to test that empty/minimal files don't crash.
const fsharpMinimalSource = `module Minimal
`

// parseFSharp is the shared test helper for parsing F# source code.
func parseFSharp(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "/tmp/Finance.fs", []byte(src)); err != nil {
		t.Fatalf("FSharpParser.Parse() error: %v", err)
	}
	return g
}

// ─── 1. Extensions ────────────────────────────────────────────────────────────

func TestFSharpParser_Extensions_IncludesFS(t *testing.T) {
	exts := parser.NewFSharpParser().Extensions()
	if !hasExtension(exts, ".fs") {
		t.Errorf("Extensions() = %v, missing .fs", exts)
	}
}

func TestFSharpParser_Extensions_IncludesFSI(t *testing.T) {
	exts := parser.NewFSharpParser().Extensions()
	if !hasExtension(exts, ".fsi") {
		t.Errorf("Extensions() = %v, missing .fsi", exts)
	}
}

func TestFSharpParser_Extensions_IncludesFSX(t *testing.T) {
	exts := parser.NewFSharpParser().Extensions()
	if !hasExtension(exts, ".fsx") {
		t.Errorf("Extensions() = %v, missing .fsx", exts)
	}
}

// ─── 2. File node ─────────────────────────────────────────────────────────────

func TestFSharpParser_FileNode(t *testing.T) {
	assertFileNode(t, parseFSharp(t, fsharpSource), "Finance.fs")
}

// ─── 3. Let bindings ──────────────────────────────────────────────────────────

func TestFSharpParser_SimpleLet(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "computeTradePrice", graph.NodeFunction)
	if !n.Exported {
		t.Error("computeTradePrice should be exported (no access modifier)")
	}
}

func TestFSharpParser_LetRec(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "fibonacci", graph.NodeFunction)
	if !n.Exported {
		t.Error("let rec fibonacci should be exported")
	}
}

func TestFSharpParser_LetPrivate_NotExported(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "validateSymbol", graph.NodeFunction)
	if n.Exported {
		t.Error("let private validateSymbol should NOT be exported")
	}
}

func TestFSharpParser_LetInternal_NotExported(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "parseQuantity", graph.NodeFunction)
	if n.Exported {
		t.Error("let internal parseQuantity should NOT be exported")
	}
}

func TestFSharpParser_LetPublic_Exported(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "createOrder", graph.NodeFunction)
	if !n.Exported {
		t.Error("let public createOrder should be exported")
	}
}

// ─── 4. Type definitions ──────────────────────────────────────────────────────

func TestFSharpParser_RecordType(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "PricingResult", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "record" {
		t.Errorf("PricingResult kind = %q, want record", n.Metadata["kind"])
	}
}

func TestFSharpParser_DiscriminatedUnion(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "OrderType", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "union" {
		t.Errorf("OrderType kind = %q, want union", n.Metadata["kind"])
	}
}

func TestFSharpParser_TypeAlias(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "AccountId", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "alias" {
		t.Errorf("AccountId kind = %q, want alias", n.Metadata["kind"])
	}
}

func TestFSharpParser_InterfaceType(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "IPricingEngine", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "interface" {
		t.Errorf("IPricingEngine kind = %q, want interface", n.Metadata["kind"])
	}
}

func TestFSharpParser_ClassType(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "TradingSession", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "class" {
		t.Errorf("TradingSession kind = %q, want class", n.Metadata["kind"])
	}
}

// ─── 5. Module declarations ───────────────────────────────────────────────────

func TestFSharpParser_ModuleDeclaration(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	nodes := g.FindByName("FinancialModeling")
	if len(nodes) == 0 {
		t.Fatal("module FinancialModeling not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("module type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestFSharpParser_ModuleRec(t *testing.T) {
	src := `module rec Helpers

let add x y = x + y
`
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "/tmp/Helpers.fs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("Helpers")
	if len(nodes) == 0 {
		t.Fatal("module rec Helpers not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("module type = %q, want NodePackage", nodes[0].Type)
	}
}

// ─── 6. Open statements (imports) ────────────────────────────────────────────

func TestFSharpParser_OpenStatement_ImportsEdge(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	fileID := g.FindByName("Finance.fs")[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 2 {
		t.Errorf("expected ≥2 IMPORTS edges (System, System.Collections.Generic), got %d", importCount)
	}
}

func TestFSharpParser_OpenStatement_NodeCreated(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	nodes := g.FindByName("System")
	if len(nodes) == 0 {
		t.Fatal("import node for System not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

// ─── 7. Member methods ────────────────────────────────────────────────────────

func TestFSharpParser_MemberMethod(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	// TradingSession has member this.GetAccount() and member this.IsActive()
	nodes := g.FindByName("GetAccount")
	if len(nodes) == 0 {
		// Also check qualified form.
		nodes = g.FindByName("TradingSession.GetAccount")
	}
	if len(nodes) == 0 {
		t.Fatal("member method GetAccount not found (checked bare and qualified)")
	}
	if nodes[0].Type != graph.NodeFunction {
		t.Errorf("member type = %q, want NodeFunction", nodes[0].Type)
	}
}

// ─── 8. Active patterns ───────────────────────────────────────────────────────

func TestFSharpParser_ActivePattern(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	// Active pattern name is "(|Even|Odd|)"
	nodes := g.FindByName("(|Even|Odd|)")
	if len(nodes) == 0 {
		t.Fatal("active pattern (|Even|Odd|) not found")
	}
	n := nodes[0]
	if n.Type != graph.NodeFunction {
		t.Errorf("active pattern type = %q, want NodeFunction", n.Type)
	}
	if n.Metadata == nil || n.Metadata["kind"] != "active_pattern" {
		t.Errorf("active pattern kind = %q, want active_pattern", n.Metadata["kind"])
	}
}

// ─── 9. Doc comments ─────────────────────────────────────────────────────────

func TestFSharpParser_TripleSlashDocComment(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	n := assertNode(t, g, "computeTradePrice", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("computeTradePrice should have metadata")
	}
	if n.Metadata["doc"] == "" {
		t.Error("computeTradePrice should have a /// doc comment")
	}
}

func TestFSharpParser_BlockDocComment(t *testing.T) {
	src := `module Docs

(** Computes the net present value. *)
let npv rate cashflows =
    List.sum cashflows
`
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "/tmp/Docs.fs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	nodes := g.FindByName("npv")
	if len(nodes) == 0 {
		t.Fatal("npv function not found")
	}
	if nodes[0].Metadata == nil || nodes[0].Metadata["doc"] == "" {
		t.Error("npv should have a (** *) block doc comment")
	}
}

// ─── 10. Empty file / crash safety ───────────────────────────────────────────

func TestFSharpParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewFSharpParser(), ".fs", "")
}

func TestFSharpParser_MinimalFile(t *testing.T) {
	assertNoCrash(t, parser.NewFSharpParser(), ".fs", fsharpMinimalSource)
}

// ─── 11. Edge assertions ──────────────────────────────────────────────────────

func TestFSharpParser_DefinesEdge_LetBinding(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	fileID := g.FindByName("Finance.fs")[0].ID
	assertDefinesEdge(t, g, fileID, "computeTradePrice")
}

func TestFSharpParser_ImportsEdge_OpenStatement(t *testing.T) {
	g := parseFSharp(t, fsharpSource)
	fileID := g.FindByName("Finance.fs")[0].ID
	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "System" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected IMPORTS edge from file to System")
	}
}

// ─── 12. Complex realistic fixtures ──────────────────────────────────────────

func TestFSharpParser_NestedModulesAndTypes(t *testing.T) {
	src := `module Banking.Core

open System

type Currency = USD | EUR | GBP

type Account = {
    Id: string
    Balance: decimal
    Currency: Currency
}

type IRepository<'T> =
    abstract member Get: string -> 'T option
    abstract member Save: 'T -> unit

type AccountRepository(connectionString: string) =
    member this.Get(id: string) = None
    member this.Save(account: Account) = ()

/// Calculates compound interest.
let compoundInterest principal rate periods =
    principal * (1.0 + rate) ** periods

let private hashAccount (acc: Account) =
    acc.Id.GetHashCode()
`
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "/tmp/Banking.fs", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	// Module
	assertNode(t, g, "Banking.Core", graph.NodePackage)

	// DU
	n := assertNode(t, g, "Currency", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "union" {
		t.Errorf("Currency kind = %q, want union", n.Metadata["kind"])
	}

	// Record
	n2 := assertNode(t, g, "Account", graph.NodeStruct)
	if n2.Metadata == nil || n2.Metadata["kind"] != "record" {
		t.Errorf("Account kind = %q, want record", n2.Metadata["kind"])
	}

	// Interface (generic)
	n3 := assertNode(t, g, "IRepository", graph.NodeStruct)
	if n3.Metadata == nil || n3.Metadata["kind"] != "interface" {
		t.Errorf("IRepository kind = %q, want interface", n3.Metadata["kind"])
	}

	// Class
	n4 := assertNode(t, g, "AccountRepository", graph.NodeStruct)
	if n4.Metadata == nil || n4.Metadata["kind"] != "class" {
		t.Errorf("AccountRepository kind = %q, want class", n4.Metadata["kind"])
	}

	// Top-level function with doc comment
	fn := assertNode(t, g, "compoundInterest", graph.NodeFunction)
	if !fn.Exported {
		t.Error("compoundInterest should be exported")
	}
	if fn.Metadata == nil || fn.Metadata["doc"] == "" {
		t.Error("compoundInterest should have a doc comment")
	}

	// Private function
	fn2 := assertNode(t, g, "hashAccount", graph.NodeFunction)
	if fn2.Exported {
		t.Error("hashAccount (let private) should NOT be exported")
	}

	// Import from open System
	fileID := g.FindByName("Banking.fs")[0].ID
	importFound := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "System" {
				importFound = true
				break
			}
		}
	}
	if !importFound {
		t.Error("expected IMPORTS edge for System")
	}
}

func TestFSharpParser_FSharpScriptFile(t *testing.T) {
	// .fsx script files should also be parsed.
	src := `open System.IO

let readLines path =
    File.ReadAllLines(path)

let writeLines path lines =
    File.WriteAllLines(path, lines)
`
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "/tmp/script.fsx", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertFileNode(t, g, "script.fsx")
	assertNode(t, g, "readLines", graph.NodeFunction)
	assertNode(t, g, "writeLines", graph.NodeFunction)
}

func TestFSharpParser_InterfaceFile(t *testing.T) {
	// .fsi interface/signature files should also be parsed.
	src := `module Services.Contracts

/// Core authentication service contract.
type IAuthService =
    abstract member Login: string * string -> bool
    abstract member Logout: string -> unit
`
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "/tmp/Contracts.fsi", []byte(src)); err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	assertFileNode(t, g, "Contracts.fsi")
	n := assertNode(t, g, "IAuthService", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "interface" {
		t.Errorf("IAuthService kind = %q, want interface", n.Metadata["kind"])
	}
}

func TestFSharpParserInlineAndDUCases(t *testing.T) {
	src := []byte(`
module MyModule

/// Inline addition — compiler will inline this at call sites.
let inline add x y = x + y

/// Generic inline identity function.
let inline identity (x: 'a) = x

type Shape =
    | Circle of float
    | Rectangle of float * float
    | Point

type Color =
    | Red
    | Green
    | Blue
`)
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "test.fs", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Inline functions should be extracted.
	for _, name := range []string{"add", "identity"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected inline function %q not found", name)
			continue
		}
		n := nodes[0]
		if n.Type != graph.NodeFunction {
			t.Errorf("%q: expected NodeFunction, got %v", name, n.Type)
		}
		if n.Metadata["inline"] != "true" {
			t.Errorf("%q: expected metadata inline=true, got %q", name, n.Metadata["inline"])
		}
	}

	// DU cases should be extracted.
	for _, caseName := range []string{"Shape.Circle", "Shape.Rectangle", "Shape.Point", "Color.Red", "Color.Green", "Color.Blue"} {
		nodes := g.FindByName(caseName)
		if len(nodes) == 0 {
			t.Errorf("expected DU case %q not found", caseName)
			continue
		}
		n := nodes[0]
		if n.Metadata["kind"] != "union_case" {
			t.Errorf("%q: expected kind=union_case, got %q", caseName, n.Metadata["kind"])
		}
	}
}

func TestFSharpParserScriptRefs(t *testing.T) {
	src := []byte(`
#r "nuget:Newtonsoft.Json, 13.0.1"
#r "nuget:FSharp.Data"
#r "/path/to/MyLibrary.dll"

open Newtonsoft.Json

let serialize x = JsonConvert.SerializeObject(x)
`)
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "script.fsx", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, pkgName := range []string{"Newtonsoft.Json", "FSharp.Data", "MyLibrary"} {
		nodes := g.FindByName(pkgName)
		if len(nodes) == 0 {
			t.Errorf("expected package %q from #r not found", pkgName)
		}
	}
}

func TestFSharpParserMembers(t *testing.T) {
	src := []byte(`module MyModule

type Shape =
    abstract member Area: unit -> float
    abstract Perimeter: unit -> float

type Circle(radius: float) =
    member this.Radius = radius
    member this.Area() = System.Math.PI * radius * radius
    override this.ToString() = sprintf "Circle(%f)" radius
    static member UnitCircle() = Circle(1.0)

type Rectangle(w: float, h: float) =
    member this.Width = w
    member this.Height = h
    override this.ToString() = sprintf "Rect(%f x %f)" w h
`)
	g := graph.New("testrepo")
	p := parser.NewFSharpParser()
	if err := p.Parse(g, "/src/Shapes.fs", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Abstract member
	nodes := g.FindByName("Area")
	if len(nodes) == 0 {
		t.Error("expected abstract member 'Area'")
	} else if nodes[0].Metadata["kind"] != "abstract_member" {
		t.Errorf("Area kind = %q, want abstract_member", nodes[0].Metadata["kind"])
	}

	// Instance member
	radiusNodes := g.FindByName("Radius")
	if len(radiusNodes) == 0 {
		t.Error("expected member 'Radius'")
	}

	// Override
	toStringNodes := g.FindByName("ToString")
	found := false
	for _, n := range toStringNodes {
		if n.Metadata["kind"] == "override" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected override node 'ToString'")
	}

	// Static member
	unitNodes := g.FindByName("UnitCircle")
	if len(unitNodes) == 0 {
		t.Error("expected static member 'UnitCircle'")
	} else if unitNodes[0].Metadata["kind"] != "static_member" {
		t.Errorf("UnitCircle kind = %q, want static_member", unitNodes[0].Metadata["kind"])
	}
}
