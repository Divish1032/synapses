package parser_test

import (
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── R language parser tests ──────────────────────────────────────────────────

// rSource is a realistic R script covering data manipulation and statistical
// modelling — representative of real-world data science R code.
const rSource = `# Data manipulation and statistical modelling
# Requires: dplyr, ggplot2

library(dplyr)
library(ggplot2)
require(stats)

#' Fit a linear regression model to the supplied data frame.
#'
#' @param data A data.frame with columns x and y.
#' @param formula The model formula.
#' @return An lm object.
fitLinearModel <- function(data, formula = y ~ x) {
  model <- lm(formula, data = data)
  return(model)
}

# Compute residuals for a fitted model
computeResiduals <- function(model) {
  residuals(model)
}

# Private helper — not exported
.prepareData <- function(df) {
  df %>% filter(!is.na(y))
}

# Global assignment operator
globalCache <<- function(key, value) {
  assign(key, value, envir = .GlobalEnv)
}

# Equals-style assignment
normalise = function(x) {
  (x - mean(x)) / sd(x)
}

# S3 generic for summarising model results
summary.linearModelResult <- function(x, ...) {
  cat("Coefficients:", x$coef, "\n")
}

# S3 generic for printing model results
print.linearModelResult <- function(x, ...) {
  cat("LinearModelResult\n")
}
`

// rS4Source is a realistic S4 class definition for bioinformatics.
const rS4Source = `library(methods)

setClass("GenomicRegion",
  representation(
    chromosome = "character",
    start      = "integer",
    end        = "integer",
    strand     = "character"
  )
)

setGeneric("getLength", function(x, ...) standardGeneric("getLength"))

setMethod("getLength", "GenomicRegion", function(x, ...) {
  x@end - x@start + 1L
})

setGeneric("overlaps", function(x, y, ...) standardGeneric("overlaps"))

setMethod("overlaps", "GenomicRegion", function(x, y, ...) {
  x@chromosome == y@chromosome &&
    x@start <= y@end &&
    x@end >= y@start
})
`

// rR5Source tests R5 / Reference Class extraction.
const rR5Source = `library(R6)

#' A mutable data pipeline stage.
DataPipeline <- setRefClass("DataPipeline",
  fields = list(
    data   = "ANY",
    steps  = "list"
  ),
  methods = list(
    initialize = function(data) {
      data   <<- data
      steps  <<- list()
    },
    addStep = function(fn) {
      steps <<- c(steps, list(fn))
    },
    run = function() {
      result <- data
      for (step in steps) {
        result <- step(result)
      }
      result
    }
  )
)
`

// rNamespaceSource tests namespace (pkg::func) import extraction.
const rNamespaceSource = `
processData <- function(df) {
  df |>
    dplyr::mutate(z = x + y) |>
    tidyr::pivot_longer(cols = c(x, y))
}
`

// ─── helpers ──────────────────────────────────────────────────────────────────

func parseR(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewRParser()
	if err := p.Parse(g, "/tmp/analysis.R", []byte(src)); err != nil {
		t.Fatalf("RParser.Parse() error: %v", err)
	}
	return g
}

func parseRFile(t *testing.T, src, filename string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewRParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("RParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ───────────────────────────────────────────────────────────────

// TestRParser_ExtensionsLower verifies .r (lowercase) is registered.
func TestRParser_ExtensionsLower(t *testing.T) {
	exts := parser.NewRParser().Extensions()
	if !hasExtension(exts, ".r") {
		t.Errorf("Extensions() = %v, missing .r", exts)
	}
}

// TestRParser_ExtensionsUpper verifies .R (uppercase) is registered.
func TestRParser_ExtensionsUpper(t *testing.T) {
	exts := parser.NewRParser().Extensions()
	if !hasExtension(exts, ".R") {
		t.Errorf("Extensions() = %v, missing .R", exts)
	}
}

// ─── File node ────────────────────────────────────────────────────────────────

// TestRParser_FileNode verifies a NodeFile is created for the parsed file.
func TestRParser_FileNode(t *testing.T) {
	assertFileNode(t, parseR(t, rSource), "analysis.R")
}

// ─── Function extraction ──────────────────────────────────────────────────────

// TestRParser_StandardAssignment verifies <- function(...) extraction.
func TestRParser_StandardAssignment(t *testing.T) {
	g := parseR(t, rSource)
	n := assertNode(t, g, "fitLinearModel", graph.NodeFunction)
	if !n.Exported {
		t.Error("fitLinearModel should be exported (no leading dot)")
	}
}

// TestRParser_EqualsAssignment verifies name = function(...) extraction.
func TestRParser_EqualsAssignment(t *testing.T) {
	g := parseR(t, rSource)
	n := assertNode(t, g, "normalise", graph.NodeFunction)
	if !n.Exported {
		t.Error("normalise should be exported")
	}
}

// TestRParser_GlobalAssignment verifies <<- function(...) extraction.
func TestRParser_GlobalAssignment(t *testing.T) {
	g := parseR(t, rSource)
	n := assertNode(t, g, "globalCache", graph.NodeFunction)
	if !n.Exported {
		t.Error("globalCache should be exported (no leading dot)")
	}
}

// TestRParser_SecondFunction verifies multiple plain functions are captured.
func TestRParser_SecondFunction(t *testing.T) {
	assertNode(t, parseR(t, rSource), "computeResiduals", graph.NodeFunction)
}

// ─── S3 generics ──────────────────────────────────────────────────────────────

// TestRParser_S3Generic verifies summary.myClass is extracted with kind=s3generic.
func TestRParser_S3Generic(t *testing.T) {
	g := parseR(t, rSource)
	n := assertNode(t, g, "summary.linearModelResult", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "s3generic" {
		t.Errorf("summary.linearModelResult kind = %q, want s3generic", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("S3 generic should be exported")
	}
}

// TestRParser_S3GenericPrint verifies a second S3 generic (print.*) is extracted.
func TestRParser_S3GenericPrint(t *testing.T) {
	g := parseR(t, rSource)
	n := assertNode(t, g, "print.linearModelResult", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "s3generic" {
		t.Errorf("print.linearModelResult kind = %q, want s3generic", n.Metadata["kind"])
	}
}

// ─── S4 classes ───────────────────────────────────────────────────────────────

// TestRParser_S4SetClass verifies setClass() creates a NodeStruct with kind=s4class.
func TestRParser_S4SetClass(t *testing.T) {
	g := parseRFile(t, rS4Source, "genomics.R")
	// Use FindByType + exact name match to avoid the suffix-match in FindByName
	// picking up "getLength.GenomicRegion" when querying "GenomicRegion".
	var found *graph.Node
	for _, n := range g.FindByType(graph.NodeStruct) {
		if n.Name == "GenomicRegion" {
			found = n
			break
		}
	}
	if found == nil {
		t.Fatal("NodeStruct 'GenomicRegion' not found")
	}
	if found.Metadata == nil || found.Metadata["kind"] != "s4class" {
		t.Errorf("GenomicRegion kind = %q, want s4class", found.Metadata["kind"])
	}
	if !found.Exported {
		t.Error("S4 class should be exported")
	}
}

// TestRParser_S4SetGeneric verifies setGeneric() creates a NodeFunction with kind=s4generic.
func TestRParser_S4SetGeneric(t *testing.T) {
	g := parseRFile(t, rS4Source, "genomics.R")
	n := assertNode(t, g, "getLength", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "s4generic" {
		t.Errorf("getLength kind = %q, want s4generic", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("S4 generic should be exported")
	}
}

// TestRParser_S4SetMethod verifies setMethod() creates a NodeFunction with kind=s4method.
func TestRParser_S4SetMethod(t *testing.T) {
	g := parseRFile(t, rS4Source, "genomics.R")
	n := assertNode(t, g, "getLength.GenomicRegion", graph.NodeFunction)
	if n.Metadata == nil || n.Metadata["kind"] != "s4method" {
		t.Errorf("getLength.GenomicRegion kind = %q, want s4method", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("S4 method should be exported")
	}
}

// TestRParser_S4SetMethodMultiple verifies a second setMethod() is captured.
func TestRParser_S4SetMethodMultiple(t *testing.T) {
	g := parseRFile(t, rS4Source, "genomics.R")
	assertNode(t, g, "overlaps.GenomicRegion", graph.NodeFunction)
}

// ─── R5 / Reference classes ───────────────────────────────────────────────────

// TestRParser_R5SetRefClass verifies setRefClass() creates a NodeStruct with kind=r5class.
func TestRParser_R5SetRefClass(t *testing.T) {
	g := parseRFile(t, rR5Source, "pipeline.R")
	n := assertNode(t, g, "DataPipeline", graph.NodeStruct)
	if n.Metadata == nil || n.Metadata["kind"] != "r5class" {
		t.Errorf("DataPipeline kind = %q, want r5class", n.Metadata["kind"])
	}
	if !n.Exported {
		t.Error("R5 class should be exported")
	}
}

// ─── Imports ──────────────────────────────────────────────────────────────────

// TestRParser_LibraryImport verifies library(pkg) creates a NodePackage + EdgeImports.
func TestRParser_LibraryImport(t *testing.T) {
	g := parseR(t, rSource)
	nodes := g.FindByName("dplyr")
	found := false
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("NodePackage for 'dplyr' not found")
	}
}

// TestRParser_RequireImport verifies require(pkg) also creates an import.
func TestRParser_RequireImport(t *testing.T) {
	g := parseR(t, rSource)
	nodes := g.FindByName("stats")
	found := false
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("NodePackage for 'stats' not found")
	}
}

// TestRParser_LibraryQuotedImport verifies library("pkg") with a quoted name.
func TestRParser_LibraryQuotedImport(t *testing.T) {
	src := `library("Matrix")
x <- function() NULL
`
	g := parseR(t, src)
	nodes := g.FindByName("Matrix")
	found := false
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("NodePackage for quoted library('Matrix') not found")
	}
}

// ─── Private functions ────────────────────────────────────────────────────────

// TestRParser_PrivateFunction verifies dot-prefixed functions are not exported.
func TestRParser_PrivateFunction(t *testing.T) {
	g := parseR(t, rSource)
	n := assertNode(t, g, ".prepareData", graph.NodeFunction)
	if n.Exported {
		t.Error(".prepareData should NOT be exported (dot prefix = private)")
	}
}

// ─── Roxygen doc comments ─────────────────────────────────────────────────────

// TestRParser_RoxygenDocComment verifies #' lines above a function are extracted.
func TestRParser_RoxygenDocComment(t *testing.T) {
	g := parseR(t, rSource)
	n := assertNode(t, g, "fitLinearModel", graph.NodeFunction)
	if n.Metadata == nil {
		t.Fatal("fitLinearModel should have metadata")
	}
	doc := n.Metadata["doc"]
	if doc == "" {
		t.Error("fitLinearModel should have a roxygen doc comment")
	}
	// The doc should include text from the first #' line.
	if !containsSubstring(doc, "linear regression") && !containsSubstring(doc, "Fit") {
		t.Errorf("unexpected doc content %q", doc)
	}
}

// ─── Multiline function body ──────────────────────────────────────────────────

// TestRParser_MultilineFunctionBody verifies functions with multi-line bodies
// are extracted correctly and do not get truncated or duplicated.
func TestRParser_MultilineFunctionBody(t *testing.T) {
	src := `
fitModel <- function(data, formula = y ~ x, weights = NULL) {
  # Validate inputs
  stopifnot(is.data.frame(data))
  stopifnot(inherits(formula, "formula"))

  # Fit via weighted least squares if weights provided
  if (!is.null(weights)) {
    lm(formula, data = data, weights = weights)
  } else {
    lm(formula, data = data)
  }
}

predictValues <- function(model, newdata) {
  predict(model, newdata = newdata, se.fit = TRUE)
}
`
	g := parseR(t, src)
	assertNode(t, g, "fitModel", graph.NodeFunction)
	assertNode(t, g, "predictValues", graph.NodeFunction)
	// Ensure only 2 function nodes (+ 1 file node).
	funcs := g.FindByType(graph.NodeFunction)
	if len(funcs) != 2 {
		t.Errorf("expected 2 function nodes, got %d", len(funcs))
	}
}

// ─── Empty file ───────────────────────────────────────────────────────────────

// TestRParser_EmptyFile verifies an empty file does not crash and creates a file node.
func TestRParser_EmptyFile(t *testing.T) {
	assertNoCrash(t, parser.NewRParser(), ".R", "")
}

// ─── DEFINES edges ────────────────────────────────────────────────────────────

// TestRParser_DefinesEdgeFileToFunction verifies a DEFINES edge is created
// from the file node to each extracted function.
func TestRParser_DefinesEdgeFileToFunction(t *testing.T) {
	g := parseR(t, rSource)
	fileNodes := g.FindByName("analysis.R")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID
	assertDefinesEdge(t, g, fileID, "fitLinearModel")
	assertDefinesEdge(t, g, fileID, "computeResiduals")
	assertDefinesEdge(t, g, fileID, ".prepareData")
}

// ─── IMPORTS edges ────────────────────────────────────────────────────────────

// TestRParser_ImportsEdgeFileToPackage verifies an IMPORTS edge is created
// from the file node to each imported package.
func TestRParser_ImportsEdgeFileToPackage(t *testing.T) {
	g := parseR(t, rSource)
	fileNodes := g.FindByName("analysis.R")
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
		t.Errorf("expected at least 3 import edges (dplyr, ggplot2, stats), got %d", importCount)
	}
}

// TestRParser_NamespaceImport verifies pkg::func references create import nodes.
func TestRParser_NamespaceImport(t *testing.T) {
	g := parseRFile(t, rNamespaceSource, "pipeline.R")
	// dplyr::mutate → dplyr should be imported
	nodes := g.FindByName("dplyr")
	found := false
	for _, n := range nodes {
		if n.Type == graph.NodePackage {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("NodePackage for dplyr (via namespace) not found")
	}
}

// ─── R 4.1+ lambda shorthand ──────────────────────────────────────────────────

// TestRParserLambdaShorthand verifies that R 4.1+ \(x) expr lambda assignments
// are extracted as NodeFunction nodes with kind=lambda.
func TestRParserLambdaShorthand(t *testing.T) {
	src := []byte(`
# R 4.1+ lambda shorthand
double <- \(x) x * 2
add <- \(x, y) x + y
.internal_helper <- \(x) x + 1
greet <- \(name) paste("Hello", name)

# These should NOT be captured as lambda (no assignment)
result <- sapply(1:10, \(x) x^2)
`)
	g := graph.New("testrepo")
	p := parser.NewRParser()
	if err := p.Parse(g, "test.R", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name     string
		exported bool
	}{
		{"double", true},
		{"add", true},
		{".internal_helper", false},
		{"greet", true},
	}
	for _, tc := range tests {
		nodes := g.FindByName(tc.name)
		var n *graph.Node
		for _, candidate := range nodes {
			if candidate.Name == tc.name {
				n = candidate
				break
			}
		}
		if n == nil {
			t.Errorf("expected function %q not found", tc.name)
			continue
		}
		if n.Type != graph.NodeFunction {
			t.Errorf("%q: expected NodeFunction, got %v", tc.name, n.Type)
		}
		if n.Exported != tc.exported {
			t.Errorf("%q: expected Exported=%v, got %v", tc.name, tc.exported, n.Exported)
		}
		if n.Metadata["kind"] != "lambda" {
			t.Errorf("%q: expected kind=lambda, got %q", tc.name, n.Metadata["kind"])
		}
	}
}

// ─── setOldClass ──────────────────────────────────────────────────────────────

// TestRParserSetOldClass verifies setOldClass() creates a NodeStruct with kind=s3class.
func TestRParserSetOldClass(t *testing.T) {
	src := []byte(`
# Simple setOldClass
setOldClass("myS3Class")

# With inheritance vector
setOldClass(c("derivedClass", "baseClass"))
`)
	g := graph.New("testrepo")
	p := parser.NewRParser()
	if err := p.Parse(g, "/src/classes.R", src); err != nil {
		t.Fatalf("RParser.Parse() error: %v", err)
	}

	for _, name := range []string{"myS3Class", "derivedClass"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected node %q from setOldClass", name)
			continue
		}
		if nodes[0].Type != graph.NodeStruct {
			t.Errorf("%q: type = %v, want NodeStruct", name, nodes[0].Type)
		}
		if nodes[0].Metadata["kind"] != "s3class" {
			t.Errorf("%q: kind = %q, want s3class", name, nodes[0].Metadata["kind"])
		}
	}
}

// ─── Utilities ────────────────────────────────────────────────────────────────

// containsSubstring checks whether s contains substr (case-insensitive).
func containsSubstring(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}
