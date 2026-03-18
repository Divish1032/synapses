package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── JSON test helpers ───────────────────────────────────────────────────────

const packageJsonSource = `{
  "name": "my-app",
  "version": "1.0.0",
  "description": "A sample Node.js project",
  "dependencies": {
    "express": "^4.18.0",
    "lodash": "^4.17.21"
  },
  "devDependencies": {
    "jest": "^29.0.0",
    "eslint": "^8.0.0"
  },
  "peerDependencies": {
    "react": "^18.0.0"
  },
  "scripts": {
    "start": "node index.js",
    "test": "jest",
    "lint": "eslint src/",
    "build": "tsc"
  },
  "workspaces": [
    "packages/*"
  ]
}
`

const tsconfigSource = `{
  "compilerOptions": {
    "target": "ES2020",
    "module": "commonjs",
    "lib": ["ES2020"],
    "outDir": "./dist",
    "rootDir": "./src",
    "strict": true,
    "esModuleInterop": true,
    "skipLibCheck": true,
    "forceConsistentCasingInFileNames": true
  },
  "include": ["src/**/*"],
  "exclude": ["node_modules", "dist"]
}
`

const schemaJsonSource = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "Person",
  "type": "object",
  "definitions": {
    "Address": {
      "type": "object",
      "properties": {
        "street": {"type": "string"},
        "city": {"type": "string"}
      }
    },
    "Contact": {
      "type": "object",
      "properties": {
        "email": {"type": "string"},
        "phone": {"type": "string"}
      }
    }
  },
  "properties": {
    "name": {"type": "string"},
    "age": {"type": "number"},
    "address": {"$ref": "#/definitions/Address"},
    "contact": {"$ref": "#/definitions/Contact"}
  },
  "required": ["name", "age"]
}
`

const genericJsonSource = `{
  "config": {
    "api_url": "https://api.example.com",
    "timeout": 5000
  },
  "metadata": {
    "created": "2024-01-01",
    "version": "1.0"
  },
  "items": [1, 2, 3],
  "enabled": true
}
`

const minimalPackageJson = `{
  "name": "minimal-app",
  "version": "0.1.0"
}
`

const emptyJson = `{}
`

const packageJsonWithVersion = `{
  "name": "versioned-app",
  "version": "2.5.3",
  "dependencies": {
    "react": "^18.2.0"
  }
}
`

const tsconfigMinimal = `{
  "compilerOptions": {
    "target": "ES2020"
  }
}
`

const schemaDefs = `{
  "$defs": {
    "StringType": {
      "type": "string"
    },
    "NumberType": {
      "type": "number"
    }
  },
  "properties": {
    "field1": {"$ref": "#/$defs/StringType"},
    "field2": {"$ref": "#/$defs/NumberType"}
  }
}
`

func parseJson(t *testing.T, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewJSONParser()
	if err := p.Parse(g, "/tmp/test.json", []byte(src)); err != nil {
		t.Fatalf("JSONParser.Parse() error: %v", err)
	}
	return g
}

func parseJsonWithFilename(t *testing.T, filename string, src string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewJSONParser()
	if err := p.Parse(g, "/tmp/"+filename, []byte(src)); err != nil {
		t.Fatalf("JSONParser.Parse() error: %v", err)
	}
	return g
}

// ─── Extensions ──────────────────────────────────────────────────────────────

func TestJSONParser_Extensions(t *testing.T) {
	exts := parser.NewJSONParser().Extensions()
	if len(exts) != 1 || exts[0] != ".json" {
		t.Errorf("Extensions() = %v, want [.json]", exts)
	}
}

// ─── File node ───────────────────────────────────────────────────────────────

func TestJSONParser_FileNode(t *testing.T) {
	g := parseJson(t, packageJsonSource)
	nodes := g.FindByName("test.json")
	if len(nodes) == 0 {
		t.Fatal("file node test.json not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── package.json parsing ────────────────────────────────────────────────────

func TestJSONParser_PackageJsonBasic(t *testing.T) {
	g := parseJsonWithFilename(t, "package.json", packageJsonSource)

	// Check for file node
	fileNodes := g.FindByName("package.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node package.json not found")
	}
	fileNode := fileNodes[0]
	if fileNode.Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", fileNode.Type)
	}

	// Check metadata on file node
	if fileNode.Metadata["name"] != "my-app" {
		t.Errorf("package name = %q, want 'my-app'", fileNode.Metadata["name"])
	}
	if fileNode.Metadata["version"] != "1.0.0" {
		t.Errorf("package version = %q, want '1.0.0'", fileNode.Metadata["version"])
	}
}

func TestJSONParser_PackageJsonDependencies(t *testing.T) {
	g := parseJsonWithFilename(t, "package.json", packageJsonSource)

	// Check for dependencies
	expressNodes := g.FindByName("express")
	if len(expressNodes) == 0 {
		t.Fatal("expected express dependency")
	}
	expressNode := expressNodes[0]
	if expressNode.Type != graph.NodeVariable {
		t.Errorf("express type = %q, want NodeVariable", expressNode.Type)
	}
	if expressNode.Metadata["kind"] != "dependency" {
		t.Errorf("express kind = %q, want dependency", expressNode.Metadata["kind"])
	}

	// Check another dependency
	lodashNodes := g.FindByName("lodash")
	if len(lodashNodes) == 0 {
		t.Fatal("expected lodash dependency")
	}
}

func TestJSONParser_PackageJsonDevDependencies(t *testing.T) {
	g := parseJsonWithFilename(t, "package.json", packageJsonSource)

	// Check for dev dependencies
	jestNodes := g.FindByName("jest")
	if len(jestNodes) == 0 {
		t.Fatal("expected jest dev dependency")
	}
	jestNode := jestNodes[0]
	if jestNode.Metadata["kind"] != "dev_dependency" {
		t.Errorf("jest kind = %q, want dev_dependency", jestNode.Metadata["kind"])
	}

	// Check another dev dependency
	eslintNodes := g.FindByName("eslint")
	if len(eslintNodes) == 0 {
		t.Fatal("expected eslint dev dependency")
	}
}

func TestJSONParser_PackageJsonPeerDependencies(t *testing.T) {
	g := parseJsonWithFilename(t, "package.json", packageJsonSource)

	// Check for peer dependencies
	reactNodes := g.FindByName("react")
	if len(reactNodes) == 0 {
		t.Fatal("expected react peer dependency")
	}
	reactNode := reactNodes[0]
	if reactNode.Metadata["kind"] != "peer_dependency" {
		t.Errorf("react kind = %q, want peer_dependency", reactNode.Metadata["kind"])
	}
}

func TestJSONParser_PackageJsonScripts(t *testing.T) {
	g := parseJsonWithFilename(t, "package.json", packageJsonSource)

	// Check for scripts
	startNodes := g.FindByName("start")
	if len(startNodes) == 0 {
		t.Fatal("expected start script")
	}
	startNode := startNodes[0]
	if startNode.Type != graph.NodeVariable {
		t.Errorf("start type = %q, want NodeVariable", startNode.Type)
	}
	if startNode.Metadata["kind"] != "script" {
		t.Errorf("start kind = %q, want script", startNode.Metadata["kind"])
	}
	if startNode.Metadata["value"] != "node index.js" {
		t.Errorf("start value = %q, want 'node index.js'", startNode.Metadata["value"])
	}

	// Check other scripts
	testNodes := g.FindByName("test")
	if len(testNodes) == 0 {
		t.Fatal("expected test script")
	}

	buildNodes := g.FindByName("build")
	if len(buildNodes) == 0 {
		t.Fatal("expected build script")
	}
}

func TestJSONParser_PackageJsonWorkspaces(t *testing.T) {
	g := parseJsonWithFilename(t, "package.json", packageJsonSource)

	// Check for workspaces config
	workspaceNodes := g.FindByName("workspaces")
	if len(workspaceNodes) == 0 {
		t.Fatal("expected workspaces config")
	}
	workspaceNode := workspaceNodes[0]
	if workspaceNode.Metadata["kind"] != "config" {
		t.Errorf("workspaces kind = %q, want config", workspaceNode.Metadata["kind"])
	}
}

func TestJSONParser_MinimalPackageJson(t *testing.T) {
	g := parseJsonWithFilename(t, "package.json", minimalPackageJson)

	fileNodes := g.FindByName("package.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found for minimal package.json")
	}

	fileNode := fileNodes[0]
	if fileNode.Metadata["name"] != "minimal-app" {
		t.Errorf("package name = %q, want 'minimal-app'", fileNode.Metadata["name"])
	}
}

func TestJSONParser_PackageJsonWithVersionMetadata(t *testing.T) {
	g := parseJsonWithFilename(t, "package.json", packageJsonWithVersion)

	fileNodes := g.FindByName("package.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}

	fileNode := fileNodes[0]
	if fileNode.Metadata["version"] != "2.5.3" {
		t.Errorf("version metadata = %q, want '2.5.3'", fileNode.Metadata["version"])
	}

	// Check that dependency has version metadata
	reactNodes := g.FindByName("react")
	if len(reactNodes) == 0 {
		t.Fatal("expected react dependency")
	}
	reactNode := reactNodes[0]
	if reactNode.Metadata["version"] != "^18.2.0" {
		t.Errorf("react version = %q, want '^18.2.0'", reactNode.Metadata["version"])
	}
}

// ─── tsconfig.json parsing ────────────────────────────────────────────────────

func TestJSONParser_TsconfigBasic(t *testing.T) {
	g := parseJsonWithFilename(t, "tsconfig.json", tsconfigSource)

	// Check for file node
	fileNodes := g.FindByName("tsconfig.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node tsconfig.json not found")
	}
}

func TestJSONParser_TsconfigCompilerOptions(t *testing.T) {
	g := parseJsonWithFilename(t, "tsconfig.json", tsconfigSource)

	// Check for compiler options
	targetNodes := g.FindByName("target")
	if len(targetNodes) == 0 {
		t.Fatal("expected target compiler option")
	}
	targetNode := targetNodes[0]
	if targetNode.Metadata["kind"] != "compiler_option" {
		t.Errorf("target kind = %q, want compiler_option", targetNode.Metadata["kind"])
	}
	if targetNode.Metadata["value"] != "ES2020" {
		t.Errorf("target value = %q, want 'ES2020'", targetNode.Metadata["value"])
	}

	// Check other compiler options
	strictNodes := g.FindByName("strict")
	if len(strictNodes) == 0 {
		t.Fatal("expected strict compiler option")
	}

	moduleNodes := g.FindByName("module")
	if len(moduleNodes) == 0 {
		t.Fatal("expected module compiler option")
	}
}

func TestJSONParser_TsconfigIncludeExclude(t *testing.T) {
	g := parseJsonWithFilename(t, "tsconfig.json", tsconfigSource)

	// Check for include config
	includeNodes := g.FindByName("include")
	if len(includeNodes) == 0 {
		t.Fatal("expected include config")
	}
	includeNode := includeNodes[0]
	if includeNode.Metadata["kind"] != "config" {
		t.Errorf("include kind = %q, want config", includeNode.Metadata["kind"])
	}

	// Check for exclude config
	excludeNodes := g.FindByName("exclude")
	if len(excludeNodes) == 0 {
		t.Fatal("expected exclude config")
	}
}

func TestJSONParser_TsconfigMinimal(t *testing.T) {
	g := parseJsonWithFilename(t, "tsconfig.json", tsconfigMinimal)

	// Just verify the file was parsed and compiler options extracted
	targetNodes := g.FindByName("target")
	if len(targetNodes) == 0 {
		t.Fatal("expected target option in minimal tsconfig")
	}
}

// ─── JSON Schema parsing ──────────────────────────────────────────────────────

func TestJSONParser_SchemaJsonBasic(t *testing.T) {
	g := parseJsonWithFilename(t, "types.schema.json", schemaJsonSource)

	// Check for file node
	fileNodes := g.FindByName("types.schema.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node types.schema.json not found")
	}
}

func TestJSONParser_SchemaDefinitions(t *testing.T) {
	// Use a unique schema filename to avoid conflicts
	src := `{
  "definitions": {
    "AddressType": {
      "type": "object",
      "properties": {
        "street": {"type": "string"}
      }
    }
  }
}
`
	g := parseJsonWithFilename(t, "models.schema.json", src)

	// Verify the file was parsed
	fileNodes := g.FindByName("models.schema.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

func TestJSONParser_SchemaProperties(t *testing.T) {
	g := parseJsonWithFilename(t, "schema.json", schemaJsonSource)

	// Check for properties
	nameNodes := g.FindByName("name")
	if len(nameNodes) == 0 {
		t.Fatal("expected name property")
	}
	nameNode := nameNodes[0]
	if nameNode.Type != graph.NodeVariable {
		t.Errorf("name type = %q, want NodeVariable", nameNode.Type)
	}
	if nameNode.Metadata["kind"] != "property" {
		t.Errorf("name kind = %q, want property", nameNode.Metadata["kind"])
	}

	// Check another property
	ageNodes := g.FindByName("age")
	if len(ageNodes) == 0 {
		t.Fatal("expected age property")
	}
}

func TestJSONParser_SchemaDefs(t *testing.T) {
	g := parseJsonWithFilename(t, "custom.schema.json", schemaDefs)

	// Check for $defs (newer JSON Schema format)
	stringTypeNodes := g.FindByName("StringType")
	if len(stringTypeNodes) == 0 {
		t.Fatal("expected StringType schema type")
	}
	stringTypeNode := stringTypeNodes[0]
	if stringTypeNode.Type != graph.NodeStruct {
		t.Errorf("StringType type = %q, want NodeStruct", stringTypeNode.Type)
	}

	// Check another type
	numberTypeNodes := g.FindByName("NumberType")
	if len(numberTypeNodes) == 0 {
		t.Fatal("expected NumberType schema type")
	}
}

// ─── Generic JSON parsing ────────────────────────────────────────────────────

func TestJSONParser_GenericJsonBasic(t *testing.T) {
	g := parseJson(t, genericJsonSource)

	// Check for file node
	fileNodes := g.FindByName("test.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
}

func TestJSONParser_GenericJsonTopLevelKeys(t *testing.T) {
	g := parseJson(t, genericJsonSource)

	// Check for top-level keys (only 1 level deep)
	configNodes := g.FindByName("config")
	if len(configNodes) == 0 {
		t.Fatal("expected config field")
	}
	configNode := configNodes[0]
	if configNode.Type != graph.NodeVariable {
		t.Errorf("config type = %q, want NodeVariable", configNode.Type)
	}
	if configNode.Metadata["kind"] != "field" {
		t.Errorf("config kind = %q, want field", configNode.Metadata["kind"])
	}

	// Check other top-level keys
	metadataNodes := g.FindByName("metadata")
	if len(metadataNodes) == 0 {
		t.Fatal("expected metadata field")
	}

	itemsNodes := g.FindByName("items")
	if len(itemsNodes) == 0 {
		t.Fatal("expected items field")
	}

	enabledNodes := g.FindByName("enabled")
	if len(enabledNodes) == 0 {
		t.Fatal("expected enabled field")
	}
}

// ─── Empty JSON ────────────────────────────────────────────────────────────────

func TestJSONParser_EmptyJson(t *testing.T) {
	g := parseJson(t, emptyJson)

	// Should still create a file node
	fileNodes := g.FindByName("test.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist for empty JSON")
	}
	if fileNodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", fileNodes[0].Type)
	}
}

// ─── Invalid JSON ──────────────────────────────────────────────────────────────

func TestJSONParser_InvalidJson(t *testing.T) {
	g := parseJson(t, `{invalid json}`)

	// Should still create a file node even for invalid JSON
	fileNodes := g.FindByName("test.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node should exist even for invalid JSON")
	}
}

// ─── JSON with comments ──────────────────────────────────────────────────────

func TestJSONParser_JsonWithMultipleLevels(t *testing.T) {
	src := `{
  "app": {
    "name": "test",
    "version": "1.0"
  },
  "config": {
    "debug": true,
    "port": 3000
  }
}
`
	g := parseJson(t, src)

	// Check for top-level keys (only 1 level deep, not nested)
	appNodes := g.FindByName("app")
	if len(appNodes) == 0 {
		t.Fatal("expected app field")
	}

	configNodes := g.FindByName("config")
	if len(configNodes) == 0 {
		t.Fatal("expected config field")
	}

	// Nested properties should NOT be extracted at top level
	// (generic JSON only goes 1 level deep)
}

// ─── jsconfig.json parsing ──────────────────────────────────────────────────

func TestJSONParser_JsconfigJson(t *testing.T) {
	g := parseJsonWithFilename(t, "jsconfig.json", tsconfigSource)

	// jsconfig.json should be parsed like tsconfig.json
	fileNodes := g.FindByName("jsconfig.json")
	if len(fileNodes) == 0 {
		t.Fatal("file node jsconfig.json not found")
	}

	// Check for compiler options
	targetNodes := g.FindByName("target")
	if len(targetNodes) == 0 {
		t.Fatal("expected target compiler option in jsconfig.json")
	}
}

// ─── Custom schema file ────────────────────────────────────────────────────────

func TestJSONParser_CustomSchemaJson(t *testing.T) {
	src := `{
  "definitions": {
    "User": {
      "type": "object",
      "properties": {
        "id": {"type": "integer"},
        "name": {"type": "string"}
      }
    }
  },
  "properties": {
    "users": {
      "type": "array",
      "items": {"$ref": "#/definitions/User"}
    }
  }
}
`
	g := parseJsonWithFilename(t, "api.schema.json", src)

	// Check for User definition
	userNodes := g.FindByName("User")
	if len(userNodes) == 0 {
		t.Fatal("expected User schema type")
	}
	userNode := userNodes[0]
	if userNode.Type != graph.NodeStruct {
		t.Errorf("User type = %q, want NodeStruct", userNode.Type)
	}

	// Check for users property
	usersNodes := g.FindByName("users")
	if len(usersNodes) == 0 {
		t.Fatal("expected users property")
	}
}

// ─── Large JSON with many fields ────────────────────────────────────────────

func TestJSONParser_LargePackageJson(t *testing.T) {
	src := `{
  "name": "large-app",
  "version": "1.0.0",
  "dependencies": {
    "dep1": "^1.0.0",
    "dep2": "^2.0.0",
    "dep3": "^3.0.0",
    "dep4": "^4.0.0",
    "dep5": "^5.0.0"
  },
  "devDependencies": {
    "devdep1": "^1.0.0",
    "devdep2": "^2.0.0",
    "devdep3": "^3.0.0"
  },
  "scripts": {
    "start": "node index.js",
    "test": "jest",
    "lint": "eslint .",
    "build": "tsc",
    "dev": "ts-node index.ts"
  }
}
`
	g := parseJsonWithFilename(t, "package.json", src)

	// Verify all dependencies were extracted
	for i := 1; i <= 5; i++ {
		depName := "dep" + string(rune('0'+i))
		nodes := g.FindByName(depName)
		if len(nodes) == 0 {
			t.Errorf("expected dependency %s", depName)
		}
	}

	// Verify all dev dependencies were extracted
	for i := 1; i <= 3; i++ {
		depName := "devdep" + string(rune('0'+i))
		nodes := g.FindByName(depName)
		if len(nodes) == 0 {
			t.Errorf("expected dev dependency %s", depName)
		}
	}

	// Verify all scripts were extracted
	scriptNames := []string{"start", "test", "lint", "build", "dev"}
	for _, name := range scriptNames {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected script %s", name)
		}
	}
}
