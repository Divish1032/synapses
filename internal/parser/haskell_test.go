package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Haskell ──────────────────────────────────────────────────────────────────

// haskellSource is a realistic Haskell fixture covering all extraction cases.
// The module is named Data.BinTree (not Data.Tree) so that FindByName("Tree")
// only matches the data type node, not the module package node (suffix match).
const haskellSource = `module Data.BinTree
  ( Tree(..)
  , insert
  , lookup
  , toList
  ) where

import Data.Maybe (Maybe(..), fromMaybe)
import qualified Data.Map as Map
import Data.List hiding (sort)

-- | A simple binary search tree.
data Tree a
  = Leaf
  | Node (Tree a) a (Tree a)
  deriving (Show, Eq)

-- | A newtype wrapper for a name.
newtype Name = Name String

-- | Class for things that can be compared.
class Comparable a where
  compareWith :: a -> a -> Int

-- | Typeclass instance for Int comparison.
instance Comparable Int where
  compareWith x y = x - y

-- | Insert a value into the tree.
insert :: Ord a => a -> Tree a -> Tree a
insert x Leaf = Node Leaf x Leaf
insert x (Node l v r)
  | x < v    = Node (insert x l) v r
  | x > v    = Node l v (insert x r)
  | otherwise = Node l v r

-- | Lookup a value in the tree.
lookup :: Ord a => a -> Tree a -> Maybe a
lookup _ Leaf = Nothing
lookup x (Node l v r)
  | x == v    = Just v
  | x < v     = lookup x l
  | otherwise  = lookup x r

-- | Convert a tree to a sorted list.
toList :: Tree a -> [a]
toList Leaf = []
toList (Node l v r) = toList l ++ [v] ++ toList r

-- This function is private (starts with underscore).
_internalHelper :: Int -> Int
_internalHelper x = x + 1
`

// haskellNoExportSource is a module without an explicit export list.
const haskellNoExportSource = `module MyModule where

import Data.List (sort)

helper :: Int -> Int
helper x = x * 2

publicFunc :: String -> String
publicFunc s = s ++ "!"
`

// haskellDocBlockSource uses block Haddock comments.
const haskellDocBlockSource = `module BlockDoc where

{- | This is a block haddock comment
     spanning multiple lines. -}
myFunc :: Int -> Int
myFunc x = x + 1
`

// haskellLiterateSource is a literate Haskell (.lhs) fixture using bird-track style.
const haskellLiterateSource = `This is a literate Haskell file.

We can write prose anywhere, and code lines start with ">".

> module LiterateModule where
>
> -- | A simple function.
> addOne :: Int -> Int
> addOne x = x + 1
>
> -- | A data type.
> data Color = Red | Green | Blue

This is more prose text.
`

func parseHaskell(t *testing.T, src, filename string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewHaskellParser()
	if err := p.Parse(g, filename, []byte(src)); err != nil {
		t.Fatalf("HaskellParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestHaskellParser_Extensions_HasHs(t *testing.T) {
	exts := parser.NewHaskellParser().Extensions()
	if !hasExtension(exts, ".hs") {
		t.Errorf("Extensions() = %v, missing .hs", exts)
	}
}

func TestHaskellParser_Extensions_HasLhs(t *testing.T) {
	exts := parser.NewHaskellParser().Extensions()
	if !hasExtension(exts, ".lhs") {
		t.Errorf("Extensions() = %v, missing .lhs", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestHaskellParser_FileNode(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	assertFileNode(t, g, "BinTree.hs")
}

// ─── Module declaration ──────────────────────────────────────────────────────

func TestHaskellParser_ModuleDeclaration(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	nodes := g.FindByName("Data.BinTree")
	if len(nodes) == 0 {
		t.Fatal("module node for Data.BinTree not found")
	}
	found := false
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected NodePackage for module Data.BinTree")
	}
}

// ─── Import statements ───────────────────────────────────────────────────────

func TestHaskellParser_SimpleImport(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	// import Data.Maybe
	nodes := g.FindByName("Data.Maybe")
	if len(nodes) == 0 {
		t.Fatal("import node for Data.Maybe not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestHaskellParser_QualifiedImport(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	// import qualified Data.Map as Map
	nodes := g.FindByName("Data.Map")
	if len(nodes) == 0 {
		t.Fatal("import node for qualified Data.Map not found")
	}
	if nodes[0].Type != graph.NodePackage {
		t.Errorf("import node type = %q, want NodePackage", nodes[0].Type)
	}
}

func TestHaskellParser_HidingImport(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	// import Data.List hiding (sort)
	nodes := g.FindByName("Data.List")
	if len(nodes) == 0 {
		t.Fatal("import node for Data.List hiding (...) not found")
	}
}

func TestHaskellParser_ImportEdge(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	fileNodes := g.FindByName("BinTree.hs")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	importCount := 0
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			importCount++
		}
	}
	if importCount < 3 {
		t.Errorf("expected at least 3 IMPORTS edges, got %d", importCount)
	}
}

// ─── Top-level functions with type signatures ─────────────────────────────────

func TestHaskellParser_FunctionWithTypeSig(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	n := assertNode(t, g, "insert", graph.NodeFunction)
	if n.Line == 0 {
		t.Error("insert should have a non-zero line number")
	}
}

func TestHaskellParser_FunctionTypeSigLine(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	nodes := g.FindByName("insert")
	if len(nodes) == 0 {
		t.Fatal("function node insert not found")
	}
	// The type signature line for "insert" should be captured.
	if nodes[0].Line == 0 {
		t.Error("insert should have a valid line number from type signature")
	}
}

// ─── Top-level function without type signature ────────────────────────────────

func TestHaskellParser_FunctionWithoutTypeSig(t *testing.T) {
	// A module where a function has no type sig but only a definition.
	src := `module NoSig where

greet name = "Hello, " ++ name ++ "!"
`
	g := parseHaskell(t, src, "/src/NoSig.hs")
	assertNode(t, g, "greet", graph.NodeFunction)
}

// ─── Data declarations ────────────────────────────────────────────────────────

func TestHaskellParser_DataDeclaration(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	n := assertNode(t, g, "Tree", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "data" {
		t.Errorf("Tree metadata kind = %q, want data", n.Metadata["kind"])
	}
}

func TestHaskellParser_DataIsExported(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	nodes := g.FindByName("Tree")
	if len(nodes) == 0 {
		t.Fatal("Tree not found")
	}
	if !nodes[0].Exported {
		t.Error("data type Tree should be exported (uppercase)")
	}
}

// ─── Newtype declarations ─────────────────────────────────────────────────────

func TestHaskellParser_NewtypeDeclaration(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	n := assertNode(t, g, "Name", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "newtype" {
		t.Errorf("Name metadata kind = %q, want newtype", n.Metadata["kind"])
	}
}

// ─── Typeclass declarations ───────────────────────────────────────────────────

func TestHaskellParser_TypeclassDeclaration(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	n := assertNode(t, g, "Comparable", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "typeclass" {
		t.Errorf("Comparable metadata kind = %q, want typeclass", n.Metadata["kind"])
	}
}

// ─── Typeclass instances ──────────────────────────────────────────────────────

func TestHaskellParser_TypeclassInstance(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	// Instance node should be created. The name is normalized.
	found := false
	for _, n := range g.AllNodes() {
		if n.Type == graph.NodeFunction && n.Metadata != nil && n.Metadata["kind"] == "instance" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a NodeFunction with kind=instance for 'instance Comparable Int'")
	}
}

// ─── Private functions (underscore prefix) ────────────────────────────────────

func TestHaskellParser_PrivateFunction_UnderscorePrefix(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	nodes := g.FindByName("_internalHelper")
	if len(nodes) == 0 {
		t.Fatal("_internalHelper not found in graph")
	}
	if nodes[0].Exported {
		t.Error("_internalHelper should be Exported=false (underscore prefix convention)")
	}
}

// ─── Haddock doc comments (-- |) ─────────────────────────────────────────────

func TestHaskellParser_HaddockLineComment(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	nodes := g.FindByName("insert")
	if len(nodes) == 0 {
		t.Fatal("insert not found")
	}
	n := nodes[0]
	if n.Metadata == nil {
		t.Fatal("insert should have metadata")
	}
	doc := n.Metadata["doc"]
	if doc == "" {
		t.Error("insert should have a Haddock doc comment extracted")
	}
	if !containsSubstr(doc, "Insert") && !containsSubstr(doc, "tree") && !containsSubstr(doc, "value") {
		t.Errorf("insert doc = %q, expected to mention Insert/tree/value", doc)
	}
}

// ─── Block Haddock doc comments ({- | ... -}) ─────────────────────────────────

func TestHaskellParser_BlockHaddockComment(t *testing.T) {
	g := parseHaskell(t, haskellDocBlockSource, "/src/BlockDoc.hs")
	nodes := g.FindByName("myFunc")
	if len(nodes) == 0 {
		t.Fatal("myFunc not found")
	}
	n := nodes[0]
	if n.Metadata == nil {
		t.Fatal("myFunc should have metadata")
	}
	doc := n.Metadata["doc"]
	if doc == "" {
		t.Errorf("myFunc should have a block Haddock doc comment, got empty")
	}
}

// ─── Empty file ───────────────────────────────────────────────────────────────

func TestHaskellParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewHaskellParser(), ".hs", "")
}

// ─── Literate Haskell (.lhs) bird-track style ─────────────────────────────────

func TestHaskellParser_LiterateHaskell_BirdTrack(t *testing.T) {
	g := parseHaskell(t, haskellLiterateSource, "/src/LiterateModule.lhs")
	// Module should be extracted from bird-track code.
	nodes := g.FindByName("LiterateModule")
	if len(nodes) == 0 {
		t.Fatal("module LiterateModule not found in literate Haskell file")
	}
	// Function should be extracted.
	assertNode(t, g, "addOne", graph.NodeFunction)
}

func TestHaskellParser_LiterateHaskell_DataDecl(t *testing.T) {
	g := parseHaskell(t, haskellLiterateSource, "/src/LiterateModule.lhs")
	// Data type should be extracted from bird-track code.
	n := assertNode(t, g, "Color", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "data" {
		t.Errorf("Color metadata kind = %q, want data", n.Metadata["kind"])
	}
}

// ─── DEFINES edges ───────────────────────────────────────────────────────────

func TestHaskellParser_DefinesEdge_FileToFunction(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	fileNodes := g.FindByName("BinTree.hs")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	assertDefinesEdge(t, g, fileID, "insert")
}

func TestHaskellParser_DefinesEdge_FileToDataType(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	fileNodes := g.FindByName("BinTree.hs")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	assertDefinesEdge(t, g, fileID, "Tree")
}

// ─── IMPORTS edges ────────────────────────────────────────────────────────────

func TestHaskellParser_ImportsEdge_FileToModule(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	fileNodes := g.FindByName("BinTree.hs")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Name == "Data.Maybe" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected IMPORTS edge from file to Data.Maybe")
	}
}

// ─── No-explicit-export module: all top-level names exported ─────────────────

func TestHaskellParser_NoExplicitExports_AllExported(t *testing.T) {
	g := parseHaskell(t, haskellNoExportSource, "/src/MyModule.hs")
	// helper and publicFunc should both be exported when no export list.
	helperNodes := g.FindByName("helper")
	if len(helperNodes) == 0 {
		t.Fatal("helper not found")
	}
	if !helperNodes[0].Exported {
		t.Error("helper should be exported when no explicit export list")
	}
}

// ─── Explicit export list: only listed names exported ────────────────────────

func TestHaskellParser_ExplicitExports_OnlyListedExported(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	// The module exports: Tree(..), insert, lookup, toList — but NOT _internalHelper.
	// lookup should be exported.
	lookupNodes := g.FindByName("lookup")
	if len(lookupNodes) == 0 {
		t.Fatal("lookup not found")
	}
	// lookup is in the export list → should be exported.
	if !lookupNodes[0].Exported {
		t.Error("lookup should be exported (it's in the explicit export list)")
	}
}

// ─── Multiple functions extracted ────────────────────────────────────────────

func TestHaskellParser_MultipleFunctions(t *testing.T) {
	g := parseHaskell(t, haskellSource, "/src/Data/BinTree.hs")
	for _, name := range []string{"insert", "lookup", "toList"} {
		assertNode(t, g, name, graph.NodeFunction)
	}
}

// ─── Type alias and operator extraction ──────────────────────────────────────

const haskellTypeAliasSource = `module Aliases where

-- | A simple type alias.
type Name = String

-- | A parametric type alias.
type Pair a b = (a, b)

-- | A type family (extension).
type family F a

-- | An operator with a type signature.
(+++) :: [a] -> [a] -> [a]
(+++) xs ys = xs ++ ys
`

func TestHaskellParserTypeAliasAndOperators(t *testing.T) {
	g := parseHaskell(t, haskellTypeAliasSource, "/src/Aliases.hs")

	// 1. Simple type alias: type Name = String → NodeStruct with kind=type_alias
	n := assertNode(t, g, "Name", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "type_alias" {
		t.Errorf("Name metadata kind = %q, want type_alias", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("type alias Name should be Exported=true (uppercase)")
	}

	// 2. Parametric type alias: type Pair a b = (a, b) → NodeStruct with kind=type_alias
	p := assertNode(t, g, "Pair", graph.NodeStruct)
	if p.Metadata == nil || p.Metadata["kind"] != "type_alias" {
		t.Errorf("Pair metadata kind = %q, want type_alias", p.Metadata["kind"])
	}

	// 3. Type family: type family F a → NodeStruct with kind=type_alias
	f := assertNode(t, g, "F", graph.NodeStruct)
	if f.Metadata == nil || f.Metadata["kind"] != "type_alias" {
		t.Errorf("F metadata kind = %q, want type_alias", f.Metadata["kind"])
	}

	// 4. Operator type signature: (+++) :: [a] -> [a] -> [a] → NodeFunction with kind=operator
	op := assertNode(t, g, "(+++)", graph.NodeFunction)
	if op.Metadata == nil || op.Metadata["kind"] != "operator" {
		t.Errorf("(+++) metadata kind = %q, want operator", op.Metadata["kind"])
	}
	if op.Name != "(+++)" {
		t.Errorf("operator node name = %q, want (+++)", op.Name)
	}
}

// ─── Foreign import (FFI) extraction ─────────────────────────────────────────

func TestHaskellParserForeignImport(t *testing.T) {
	src := `module FFIBindings where

import Foreign.C.Types
import Foreign.Ptr

-- | C sin function
foreign import ccall "math.h sin"
    c_sin :: Double -> Double

foreign import ccall "string.h strlen"
    c_strlen :: CString -> IO CSize

foreign import ccall safe "free"
    c_free :: Ptr a -> IO ()
`
	g := parseHaskell(t, src, "/src/FFIBindings.hs")

	for _, name := range []string{"c_sin", "c_strlen", "c_free"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected foreign import node %q", name)
			continue
		}
		if nodes[0].Metadata["kind"] != "foreign_import" {
			t.Errorf("%q: kind = %q, want foreign_import", name, nodes[0].Metadata["kind"])
		}
		if nodes[0].Type != graph.NodeFunction {
			t.Errorf("%q: type = %v, want NodeFunction", name, nodes[0].Type)
		}
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub ||
		len(s) > 0 && containsSubstrInner(s, sub))
}

func containsSubstrInner(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
