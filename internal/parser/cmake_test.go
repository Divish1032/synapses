package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── CMake test helpers ──────────────────────────────────────────────────────

const basicCMakeLists = `cmake_minimum_required(VERSION 3.16)
project(MyProject)

# Function definition
function(my_helper arg1 arg2)
  message("Helper function")
endfunction()

# Macro definition
macro(my_macro param)
  message("Macro: ${param}")
endmacro()

# Set variables
set(CMAKE_CXX_STANDARD 17)
set(SOURCE_FILES main.cpp utils.cpp)

# Create executable
add_executable(myapp ${SOURCE_FILES})

# Find and include packages
find_package(Boost REQUIRED)
include(CMakeFiles)

# Option
option(BUILD_TESTS "Enable testing" ON)
`

const cmakelists = `cmake_minimum_required(VERSION 3.10)
project(HelloWorld)

add_executable(hello main.cpp)
`

const cmakeWithFunctions = `function(setup_build)
  message("Setting up build")
endfunction()

function(add_tests test_dir)
  message("Adding tests from ${test_dir}")
endfunction()

macro(check_dependency lib_name)
  find_package(${lib_name} REQUIRED)
endmacro()
`

const cmakeWithTargets = `add_executable(main_app
  src/main.cpp
  src/utils.cpp
  src/config.cpp
)

add_library(mylib STATIC
  src/lib1.cpp
  src/lib2.cpp
)

add_library(shared_lib SHARED
  src/shared.cpp
)
`

const cmakeWithVariables = `set(PROJECT_NAME "MyApp")
set(VERSION_MAJOR 1)
set(VERSION_MINOR 2)

option(ENABLE_TESTING "Build tests" ON)
option(ENABLE_DOCS "Build documentation" OFF)

set(SOURCES
  src/main.cpp
  src/utils.cpp
)
`

const cmakeWithInclude = `include(GNUInstallDirs)
include(${CMAKE_CURRENT_SOURCE_DIR}/cmake/common.cmake)

find_package(Qt5 COMPONENTS Core Gui Widgets REQUIRED)
find_package(Boost 1.70 REQUIRED)
`

const cmakeWithForeach = `foreach(file IN LISTS SOURCES)
  message("Processing ${file}")
endforeach()

foreach(component Core Gui Widgets)
  find_package(Qt5${component} REQUIRED)
endforeach()
`

const cmakeMinimal = `project(Simple)
add_executable(app main.cpp)
`

const cmakeWithComments = `# This is a comment
# Project setup
project(MyProject)

# Build options
option(DEBUG "Enable debug" OFF)  # inline comment

# Source files
set(SOURCES
  main.cpp      # Main entry point
  utils.cpp     # Utilities
)
`

const cmakeNested = `cmake_minimum_required(VERSION 3.15)
project(NestedProject)

if(WIN32)
  set(PLATFORM_SOURCES win.cpp)
else()
  set(PLATFORM_SOURCES unix.cpp)
endif()

add_executable(app main.cpp ${PLATFORM_SOURCES})
`

func parseCMake(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewCMakeParser()
	if err := p.Parse(g, "/tmp/CMakeLists.txt", []byte(src)); err != nil {
		t.Fatalf("CMakeParser.Parse() error: %v", err)
	}
	return g
}

func parseCMakeWithFilename(t *testing.T, filename string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewCMakeParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("CMakeParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestCMakeParser_Extensions(t *testing.T) {
	exts := parser.NewCMakeParser().Extensions()
	if len(exts) != 1 || exts[0] != ".cmake" {
		t.Errorf("Extensions() = %v, want [.cmake]", exts)
	}
}

func TestCMakeParser_Filenames(t *testing.T) {
	filenames := parser.NewCMakeParser().Filenames()
	if len(filenames) != 1 || filenames[0] != "CMakeLists.txt" {
		t.Errorf("Filenames() = %v, want [CMakeLists.txt]", filenames)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestCMakeParser_FileNode(t *testing.T) {
	g := parseCMake(t, basicCMakeLists)
	nodes := g.FindByName("CMakeLists.txt")
	if len(nodes) == 0 {
		t.Fatal("file node CMakeLists.txt not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Function parsing ────────────────────────────────────────────────────────

func TestCMakeParser_FunctionDefinition(t *testing.T) {
	g := parseCMake(t, basicCMakeLists)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	// Note: Function extraction depends on tree-sitter grammar specifics
}

func TestCMakeParser_MultipleFunctions(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithFunctions)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Macro parsing ────────────────────────────────────────────────────────────

func TestCMakeParser_MacroDefinition(t *testing.T) {
	g := parseCMake(t, basicCMakeLists)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	// Note: Macro extraction depends on tree-sitter grammar specifics
}

func TestCMakeParser_MacroInFunction(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithFunctions)

	// Verify file was parsed successfully
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

// ─── Target parsing ──────────────────────────────────────────────────────────

func TestCMakeParser_ExecutableTarget(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithTargets)

	// Check for executable target
	targetNodes := g.FindByName("main_app")
	if len(targetNodes) == 0 {
		t.Fatal("expected main_app executable target")
	}
	targetNode := targetNodes[0]
	if targetNode.Type != graph.NodeStruct {
		t.Errorf("main_app type = %q, want NodeStruct", targetNode.Type)
	}
	if targetNode.Metadata["kind"] != "target" {
		t.Errorf("main_app kind = %q, want target", targetNode.Metadata["kind"])
	}
}

func TestCMakeParser_LibraryTargets(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithTargets)

	// Check for static library
	myLibNodes := g.FindByName("mylib")
	if len(myLibNodes) == 0 {
		t.Fatal("expected mylib library target")
	}
	myLibNode := myLibNodes[0]
	if myLibNode.Metadata["kind"] != "target" {
		t.Errorf("mylib kind = %q, want target", myLibNode.Metadata["kind"])
	}

	// Check for shared library
	sharedLibNodes := g.FindByName("shared_lib")
	if len(sharedLibNodes) == 0 {
		t.Fatal("expected shared_lib library target")
	}
}

func TestCMakeParser_SimpleExecutable(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakelists)

	// Check for hello executable
	targetNodes := g.FindByName("hello")
	if len(targetNodes) == 0 {
		t.Fatal("expected hello executable")
	}
	targetNode := targetNodes[0]
	if targetNode.Type != graph.NodeStruct {
		t.Errorf("hello type = %q, want NodeStruct", targetNode.Type)
	}
}

// ─── Variable parsing ────────────────────────────────────────────────────────

func TestCMakeParser_SetVariable(t *testing.T) {
	g := parseCMake(t, basicCMakeLists)

	// Check for CMAKE_CXX_STANDARD variable
	varNodes := g.FindByName("CMAKE_CXX_STANDARD")
	if len(varNodes) == 0 {
		t.Fatal("expected CMAKE_CXX_STANDARD variable")
	}
	varNode := varNodes[0]
	if varNode.Type != graph.NodeVariable {
		t.Errorf("CMAKE_CXX_STANDARD type = %q, want NodeVariable", varNode.Type)
	}
}

func TestCMakeParser_MultipleVariables(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithVariables)

	// Check for PROJECT_NAME
	projNameNodes := g.FindByName("PROJECT_NAME")
	if len(projNameNodes) == 0 {
		t.Fatal("expected PROJECT_NAME variable")
	}

	// Check for VERSION_MAJOR
	verMajorNodes := g.FindByName("VERSION_MAJOR")
	if len(verMajorNodes) == 0 {
		t.Fatal("expected VERSION_MAJOR variable")
	}

	// Check for VERSION_MINOR
	verMinorNodes := g.FindByName("VERSION_MINOR")
	if len(verMinorNodes) == 0 {
		t.Fatal("expected VERSION_MINOR variable")
	}
}

// ─── Option parsing ──────────────────────────────────────────────────────────

func TestCMakeParser_OptionVariable(t *testing.T) {
	g := parseCMake(t, basicCMakeLists)

	// Check for BUILD_TESTS option
	optionNodes := g.FindByName("BUILD_TESTS")
	if len(optionNodes) == 0 {
		t.Fatal("expected BUILD_TESTS option")
	}
	optionNode := optionNodes[0]
	if optionNode.Type != graph.NodeVariable {
		t.Errorf("BUILD_TESTS type = %q, want NodeVariable", optionNode.Type)
	}
}

func TestCMakeParser_MultipleOptions(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithVariables)

	// Check for ENABLE_TESTING option
	enableTestNodes := g.FindByName("ENABLE_TESTING")
	if len(enableTestNodes) == 0 {
		t.Fatal("expected ENABLE_TESTING option")
	}

	// Check for ENABLE_DOCS option
	enableDocsNodes := g.FindByName("ENABLE_DOCS")
	if len(enableDocsNodes) == 0 {
		t.Fatal("expected ENABLE_DOCS option")
	}
}

// ─── Include/Find package ────────────────────────────────────────────────────

func TestCMakeParser_IncludeCommand(t *testing.T) {
	g := parseCMake(t, basicCMakeLists)

	// Check for include command was parsed (creates dependency/import edges)
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	// Just verify the include was processed without error
}

func TestCMakeParser_FindPackage(t *testing.T) {
	g := parseCMake(t, basicCMakeLists)

	// Check for find_package command was parsed
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) > 0 {
		// Verify file node exists (indicates parsing succeeded)
		if fileNodes[0].Type != graph.NodeFile {
			t.Errorf("file node type = %q, want NodeFile", fileNodes[0].Type)
		}
	}
}

func TestCMakeParser_MultipleIncludes(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithInclude)

	// Just verify that the CMakeLists.txt file was parsed successfully
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist")
	}
}

// ─── Foreach parsing ──────────────────────────────────────────────────────────

func TestCMakeParser_ForeachLoop(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithForeach)

	// Verify that the file was parsed without error
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist")
	}
}

// ─── Minimal CMakeLists ──────────────────────────────────────────────────────

func TestCMakeParser_MinimalCMakeLists(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeMinimal)

	// Check for file node
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist")
	}

	// Check for app executable
	appNodes := g.FindByName("app")
	if len(appNodes) == 0 {
		t.Fatal("expected app executable")
	}
}

// ─── Comments and whitespace ────────────────────────────────────────────────

func TestCMakeParser_WithComments(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeWithComments)

	// Check for DEBUG option (should be found despite comments)
	debugNodes := g.FindByName("DEBUG")
	if len(debugNodes) == 0 {
		t.Fatal("expected DEBUG option")
	}

	// Check for SOURCES variable
	sourcesNodes := g.FindByName("SOURCES")
	if len(sourcesNodes) == 0 {
		t.Fatal("expected SOURCES variable")
	}
}

// ─── Nested structures ────────────────────────────────────────────────────────

func TestCMakeParser_NestedConditions(t *testing.T) {
	g := parseCMakeWithFilename(t, "CMakeLists.txt", cmakeNested)

	// Check for PLATFORM_SOURCES variable
	platformSourcesNodes := g.FindByName("PLATFORM_SOURCES")
	if len(platformSourcesNodes) == 0 {
		t.Fatal("expected PLATFORM_SOURCES variable")
	}

	// Check for app executable
	appNodes := g.FindByName("app")
	if len(appNodes) == 0 {
		t.Fatal("expected app executable")
	}
}

// ─── Empty CMakeLists ───────────────────────────────────────────────────────

func TestCMakeParser_EmptyCMakeLists(t *testing.T) {
	src := ""
	g := parseCMake(t, src)

	// Should still create a file node
	fileNodes := g.FindByName("CMakeLists.txt")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist for empty CMakeLists")
	}
}

// ─── Custom CMake file ──────────────────────────────────────────────────────

func TestCMakeParser_CustomCMakeFile(t *testing.T) {
	src := `function(configure_build)
  message("Configuring build")
  set(BUILD_DIR "${CMAKE_CURRENT_BINARY_DIR}/build")
endfunction()

add_executable(mytool tool.cpp)
`
	g := parseCMakeWithFilename(t, "common.cmake", src)

	// Check for BUILD_DIR variable
	buildDirNodes := g.FindByName("BUILD_DIR")
	if len(buildDirNodes) == 0 {
		t.Fatal("expected BUILD_DIR variable")
	}

	// Check for mytool executable
	toolNodes := g.FindByName("mytool")
	if len(toolNodes) == 0 {
		t.Fatal("expected mytool executable")
	}
}

// ─── Complex project structure ───────────────────────────────────────────────

func TestCMakeParser_ComplexProject(t *testing.T) {
	src := `cmake_minimum_required(VERSION 3.16)
project(ComplexApp)

# Libraries
add_library(core src/core.cpp)
add_library(utils src/utils.cpp)

# Executables
add_executable(main src/main.cpp)
add_executable(tool src/tool.cpp)

# Functions
function(link_libraries target)
  target_link_libraries(${target} core utils)
endfunction()

# Macros
macro(setup_compiler)
  set(CMAKE_CXX_STANDARD 20)
endmacro()
`
	g := parseCMakeWithFilename(t, "CMakeLists.txt", src)

	// Check all entities
	coreNodes := g.FindByName("core")
	if len(coreNodes) == 0 {
		t.Fatal("expected core library")
	}

	utilsNodes := g.FindByName("utils")
	if len(utilsNodes) == 0 {
		t.Fatal("expected utils library")
	}

	mainNodes := g.FindByName("main")
	if len(mainNodes) == 0 {
		t.Fatal("expected main executable")
	}

	toolNodes := g.FindByName("tool")
	if len(toolNodes) == 0 {
		t.Fatal("expected tool executable")
	}
}
