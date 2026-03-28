package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── Makefile test helpers ───────────────────────────────────────────────────

const basicMakefile = `.PHONY: all build test

all: build test
	@echo "Done"

build:
	go build -o app

test:
	go test ./...

clean:
	rm -f app
`

const makefileWithVariables = `CC = gcc
CFLAGS = -Wall -O2
TARGET = myapp
OBJECTS = main.o utils.o

$(TARGET): $(OBJECTS)
	$(CC) $(CFLAGS) -o $@ $^

main.o: main.c
	$(CC) $(CFLAGS) -c main.c

utils.o: utils.c
	$(CC) $(CFLAGS) -c utils.c

clean:
	rm -f $(OBJECTS) $(TARGET)
`

const makefileWithDefine = `define build_target
	@echo "Building $$@"
	$(CC) $(CFLAGS) -o $$@ $$^
endef

define run_tests
	@echo "Running tests"
	go test -v ./...
endef

all: binary tests

binary: $(SOURCES)
	$(build_target)

tests:
	$(run_tests)
`

const makefileWithInclude = `# Main Makefile

include config.mk
include rules.mk
-include optional.mk

sinclude vendor.mk

.PHONY: all build

all: build

build:
	@echo "Building..."
`

const makefileComplex = `# Complex project Makefile

.PHONY: all build test clean install

SRC_DIR = src
BIN_DIR = bin
OBJ_DIR = obj

CC = gcc
CFLAGS = -Wall -Wextra -O2
LDFLAGS = -lm

SOURCES = $(SRC_DIR)/main.c $(SRC_DIR)/utils.c
OBJECTS = $(OBJECTS:.c=.o)

all: build test

build: $(BIN_DIR)/app

$(BIN_DIR)/app: $(OBJECTS)
	mkdir -p $(BIN_DIR)
	$(CC) $(LDFLAGS) -o $@ $^

$(OBJ_DIR)/%.o: $(SRC_DIR)/%.c
	mkdir -p $(OBJ_DIR)
	$(CC) $(CFLAGS) -c $< -o $@

test:
	go test ./...

clean:
	rm -rf $(OBJ_DIR) $(BIN_DIR)

install: $(BIN_DIR)/app
	cp $(BIN_DIR)/app /usr/local/bin/

.PHONY: clean

define build_lib
	ar rcs $$@ $$^
endef

lib: libutils.a

libutils.a: $(OBJ_DIR)/utils.o
	$(build_lib)
`

const simpleGoMakefile = `.PHONY: all build test clean

all: build test

build:
	go build -v .

test:
	go test -v ./...

clean:
	go clean
	rm -f ./app
`

func parseMakefile(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewMakefileParser()
	if err := p.Parse(g, "/tmp/Makefile", []byte(src)); err != nil {
		t.Fatalf("MakefileParser.Parse() error: %v", err)
	}
	return g
}

func parseMakefileWithFilename(t *testing.T, filename string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewMakefileParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("MakefileParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions and Filenames ────────────────────────────────────────────────

func TestMakefileParser_Extensions(t *testing.T) {
	exts := parser.NewMakefileParser().Extensions()
	found := false
	for _, ext := range exts {
		if ext == ".mk" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Extensions() = %v, want to include .mk", exts)
	}
}

func TestMakefileParser_Filenames(t *testing.T) {
	filenames := parser.NewMakefileParser().Filenames()
	found := false
	for _, name := range filenames {
		if name == "Makefile" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Filenames() = %v, want to include Makefile", filenames)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestMakefileParser_FileNode(t *testing.T) {
	g := parseMakefile(t, basicMakefile)
	nodes := g.FindByName("Makefile")
	if len(nodes) == 0 {
		t.Fatal("file node Makefile not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Phony targets ────────────────────────────────────────────────────────────

func TestMakefileParser_PhonyTargets(t *testing.T) {
	g := parseMakefile(t, basicMakefile)

	// Check for phony targets (.PHONY: all build test)
	allNodes := g.FindByName("all")
	if len(allNodes) == 0 {
		t.Fatal("expected 'all' target")
	}

	buildNodes := g.FindByName("build")
	if len(buildNodes) == 0 {
		t.Fatal("expected 'build' target")
	}

	testNodes := g.FindByName("test")
	if len(testNodes) == 0 {
		t.Fatal("expected 'test' target")
	}
}

// ─── Variables ────────────────────────────────────────────────────────────────

func TestMakefileParser_Variables(t *testing.T) {
	g := parseMakefileWithFilename(t, "build.mk", makefileWithVariables)

	// Check for CC variable
	ccNodes := g.FindByName("CC")
	if len(ccNodes) == 0 {
		t.Fatal("expected CC variable")
	}
	ccNode := ccNodes[0]
	if ccNode.Type != graph.NodeVariable {
		t.Errorf("CC type = %q, want NodeVariable", ccNode.Type)
	}

	// Check for CFLAGS
	cflagsNodes := g.FindByName("CFLAGS")
	if len(cflagsNodes) == 0 {
		t.Fatal("expected CFLAGS variable")
	}

	// Check for TARGET
	targetNodes := g.FindByName("TARGET")
	if len(targetNodes) == 0 {
		t.Fatal("expected TARGET variable")
	}
}

// ─── Rules/Targets ────────────────────────────────────────────────────────────

func TestMakefileParser_TargetRules(t *testing.T) {
	g := parseMakefileWithFilename(t, "build.mk", makefileWithVariables)

	// Check for build rules (targets with recipes)
	// The parser may extract targets like "main.o", "utils.o"
	mainONodes := g.FindByName("main.o")
	if len(mainONodes) == 0 {
		t.Fatal("expected main.o target")
	}

	utilsONodes := g.FindByName("utils.o")
	if len(utilsONodes) == 0 {
		t.Fatal("expected utils.o target")
	}
}

// ─── Define directive ─────────────────────────────────────────────────────────

func TestMakefileParser_DefineDirective(t *testing.T) {
	g := parseMakefileWithFilename(t, "functions.mk", makefileWithDefine)

	// Check for define macros (should be extracted as functions)
	buildTargetNodes := g.FindByName("build_target")
	if len(buildTargetNodes) == 0 {
		t.Fatal("expected build_target define")
	}
	buildTargetNode := buildTargetNodes[0]
	if buildTargetNode.Type != graph.NodeFunction {
		t.Errorf("build_target type = %q, want NodeFunction", buildTargetNode.Type)
	}
	if buildTargetNode.Metadata["kind"] != "define" {
		t.Errorf("build_target kind = %q, want define", buildTargetNode.Metadata["kind"])
	}

	// Check for run_tests define
	runTestsNodes := g.FindByName("run_tests")
	if len(runTestsNodes) == 0 {
		t.Fatal("expected run_tests define")
	}
}

// ─── Include directive ─────────────────────────────────────────────────────────

func TestMakefileParser_IncludeDirective(t *testing.T) {
	g := parseMakefileWithFilename(t, "main.mk", makefileWithInclude)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("main.mk")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}

	// Check for targets defined in this Makefile
	buildNodes := g.FindByName("build")
	if len(buildNodes) == 0 {
		t.Fatal("expected build target")
	}
}

// ─── Go Makefile ───────────────────────────────────────────────────────────────

func TestMakefileParser_GoMakefile(t *testing.T) {
	g := parseMakefile(t, simpleGoMakefile)

	// Check for common Go targets
	allNodes := g.FindByName("all")
	if len(allNodes) == 0 {
		t.Fatal("expected 'all' target")
	}

	buildNodes := g.FindByName("build")
	if len(buildNodes) == 0 {
		t.Fatal("expected 'build' target")
	}

	testNodes := g.FindByName("test")
	if len(testNodes) == 0 {
		t.Fatal("expected 'test' target")
	}

	cleanNodes := g.FindByName("clean")
	if len(cleanNodes) == 0 {
		t.Fatal("expected 'clean' target")
	}
}

// ─── Complex C project Makefile ──────────────────────────────────────────────

func TestMakefileParser_ComplexProject(t *testing.T) {
	g := parseMakefileWithFilename(t, "Makefile", makefileComplex)

	// Check for variables
	srcDirNodes := g.FindByName("SRC_DIR")
	if len(srcDirNodes) == 0 {
		t.Fatal("expected SRC_DIR variable")
	}

	binDirNodes := g.FindByName("BIN_DIR")
	if len(binDirNodes) == 0 {
		t.Fatal("expected BIN_DIR variable")
	}

	// Check for targets
	buildNodes := g.FindByName("build")
	if len(buildNodes) == 0 {
		t.Fatal("expected 'build' target")
	}

	testNodes := g.FindByName("test")
	if len(testNodes) == 0 {
		t.Fatal("expected 'test' target")
	}

	cleanNodes := g.FindByName("clean")
	if len(cleanNodes) == 0 {
		t.Fatal("expected 'clean' target")
	}

	installNodes := g.FindByName("install")
	if len(installNodes) == 0 {
		t.Fatal("expected 'install' target")
	}

	// Check for define macro
	buildLibNodes := g.FindByName("build_lib")
	if len(buildLibNodes) == 0 {
		t.Fatal("expected build_lib define")
	}
}

// ─── Minimal Makefile ───────────────────────────────────────────────────────

func TestMakefileParser_Minimal(t *testing.T) {
	src := `.PHONY: all

all:
	echo "Done"
`
	g := parseMakefile(t, src)

	fileNodes := g.FindByName("Makefile")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist")
	}

	allNodes := g.FindByName("all")
	if len(allNodes) == 0 {
		t.Fatal("expected 'all' target")
	}
}

// ─── Empty Makefile ───────────────────────────────────────────────────────────

func TestMakefileParser_Empty(t *testing.T) {
	g := parseMakefile(t, "")

	// Should still create file node
	fileNodes := g.FindByName("Makefile")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist for empty Makefile")
	}
}

// ─── Makefile with comments ──────────────────────────────────────────────────

func TestMakefileParser_WithComments(t *testing.T) {
	src := `# This is the build Makefile
# It handles compilation

.PHONY: all build clean

all: build  # Default target

build:      # Build the project
	@echo "Building..."

clean:      # Clean up
	rm -f *.o
`
	g := parseMakefileWithFilename(t, "Makefile", src)

	allNodes := g.FindByName("all")
	if len(allNodes) == 0 {
		t.Fatal("expected 'all' target")
	}

	buildNodes := g.FindByName("build")
	if len(buildNodes) == 0 {
		t.Fatal("expected 'build' target")
	}

	cleanNodes := g.FindByName("clean")
	if len(cleanNodes) == 0 {
		t.Fatal("expected 'clean' target")
	}
}

// ─── Pattern rules ────────────────────────────────────────────────────────────

func TestMakefileParser_PatternRules(t *testing.T) {
	src := `.SUFFIXES: .c .o

.c.o:
	gcc -c $< -o $@

main.o: main.c
	gcc -Wall -c main.c

utils.o: utils.c
	gcc -Wall -c utils.c
`
	g := parseMakefileWithFilename(t, "Makefile", src)

	// Check for explicit targets
	mainONodes := g.FindByName("main.o")
	if len(mainONodes) == 0 {
		t.Fatal("expected main.o target")
	}

	utilsONodes := g.FindByName("utils.o")
	if len(utilsONodes) == 0 {
		t.Fatal("expected utils.o target")
	}
}
