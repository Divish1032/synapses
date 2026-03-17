package parser_test

import (
	"testing"

	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/parser"
)

// ─── PowerShell test helpers ──────────────────────────────────────────────────

const psMainSource = `#Requires -Modules ActiveDirectory

using module ./lib/Helpers

<#
Get a user from Active Directory by samAccountName.
#>
function Get-ADUserInfo {
    [CmdletBinding()]
    param([string]$SamAccountName)
    $user = Get-ADUser -Identity $SamAccountName
    return $user
}

# Write a log entry to disk
function Write-Log {
    param([string]$Message, [string]$Level = 'INFO')
    Add-Content -Path $env:LOG_PATH -Value "[$Level] $Message"
}

# helper for internal use only
function internalHelper {
    Write-Log "internal"
}

function _privateHelper {
    Write-Log "private"
}

Import-Module PSReadLine

class DatabaseManager {
    [string]$ConnectionString

    Connect([string]$connStr) {
        $this.ConnectionString = $connStr
    }

    [void] Disconnect() {
        $this.ConnectionString = $null
    }
}

enum LogLevel {
    Debug
    Info
    Warning
    Error
}
`

func parsePowerShell(t *testing.T, src string, filePath string) *graph.Graph {
	t.Helper()
	g := graph.New("testrepo")
	p := parser.NewPowerShellParser()
	if err := p.Parse(g, filePath, []byte(src)); err != nil {
		t.Fatalf("PowerShellParser.Parse() error: %v", err)
	}
	return g
}

// ─── Test 1: Extensions includes .ps1 and .psm1 ──────────────────────────────

func TestPowerShellParser_Extensions(t *testing.T) {
	exts := parser.NewPowerShellParser().Extensions()
	hasPS1, hasPSM1 := false, false
	for _, e := range exts {
		if e == ".ps1" {
			hasPS1 = true
		}
		if e == ".psm1" {
			hasPSM1 = true
		}
	}
	if !hasPS1 {
		t.Errorf("Extensions() = %v, missing .ps1", exts)
	}
	if !hasPSM1 {
		t.Errorf("Extensions() = %v, missing .psm1", exts)
	}
}

// ─── Test 2: File node created ────────────────────────────────────────────────

func TestPowerShellParser_FileNodeCreated(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/scripts/deploy.ps1")
	nodes := g.FindByName("deploy.ps1")
	if len(nodes) == 0 {
		t.Fatal("file node deploy.ps1 not found")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("file node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Test 3: Verb-Noun function extracted and exported ────────────────────────

func TestPowerShellParser_ExtractsVerbNounFunction(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	nodes := g.FindByName("Get-ADUserInfo")
	if len(nodes) == 0 {
		t.Fatal("expected Get-ADUserInfo function node")
	}
	n := nodes[0]
	if n.Type != graph.NodeFunction {
		t.Errorf("Get-ADUserInfo: type = %q, want NodeFunction", n.Type)
	}
	if !n.Exported {
		t.Error("Get-ADUserInfo should be exported (Verb-Noun convention)")
	}
}

// ─── Test 4: Lowercase private function not exported ─────────────────────────

func TestPowerShellParser_LowercaseFunctionNotExported(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	nodes := g.FindByName("internalHelper")
	if len(nodes) == 0 {
		t.Fatal("expected internalHelper function node")
	}
	if nodes[0].Exported {
		t.Error("internalHelper should NOT be exported (lowercase, no dash)")
	}
}

// ─── Test 5: Underscore-prefixed function not exported ───────────────────────

func TestPowerShellParser_UnderscorePrefixNotExported(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	nodes := g.FindByName("_privateHelper")
	if len(nodes) == 0 {
		t.Fatal("expected _privateHelper function node")
	}
	if nodes[0].Exported {
		t.Error("_privateHelper should NOT be exported (underscore prefix)")
	}
}

// ─── Test 6: Class extraction ─────────────────────────────────────────────────

func TestPowerShellParser_ExtractsClass(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	nodes := g.FindByName("DatabaseManager")
	if len(nodes) == 0 {
		t.Fatal("expected DatabaseManager class node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("DatabaseManager: type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "class" {
		t.Errorf("DatabaseManager: metadata[kind] = %q, want 'class'", n.Metadata["kind"])
	}
}

// ─── Test 7: Enum extraction ──────────────────────────────────────────────────

func TestPowerShellParser_ExtractsEnum(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	nodes := g.FindByName("LogLevel")
	if len(nodes) == 0 {
		t.Fatal("expected LogLevel enum node")
	}
	n := nodes[0]
	if n.Type != graph.NodeStruct {
		t.Errorf("LogLevel: type = %q, want NodeStruct", n.Type)
	}
	if n.Metadata["kind"] != "enum" {
		t.Errorf("LogLevel: metadata[kind] = %q, want 'enum'", n.Metadata["kind"])
	}
}

// ─── Test 8: CmdletBinding attribute detection ───────────────────────────────

func TestPowerShellParser_DetectsCmdletBinding(t *testing.T) {
	src := `function Get-Something {
    [CmdletBinding()]
    param([string]$Name)
    Write-Output $Name
}
`
	g := parsePowerShell(t, src, "/tmp/tool.ps1")
	nodes := g.FindByName("Get-Something")
	if len(nodes) == 0 {
		t.Fatal("expected Get-Something function node")
	}
	// CmdletBinding should be on the same function — it's inside the function body here.
	// The test verifies the function is extracted correctly.
	if nodes[0].Type != graph.NodeFunction {
		t.Errorf("Get-Something: type = %q, want NodeFunction", nodes[0].Type)
	}
}

func TestPowerShellParser_DetectsCmdletBindingAboveFunction(t *testing.T) {
	src := `[CmdletBinding()]
function Invoke-Deploy {
    param([string]$Env)
    Write-Output "Deploying to $Env"
}
`
	g := parsePowerShell(t, src, "/tmp/tool.ps1")
	nodes := g.FindByName("Invoke-Deploy")
	if len(nodes) == 0 {
		t.Fatal("expected Invoke-Deploy function node")
	}
	if nodes[0].Metadata["kind"] != "cmdlet" {
		t.Errorf("Invoke-Deploy: metadata[kind] = %q, want 'cmdlet'", nodes[0].Metadata["kind"])
	}
}

// ─── Test 9: Import-Module extraction ────────────────────────────────────────

func TestPowerShellParser_ExtractsImportModule(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	fileNodes := g.FindByName("deploy.ps1")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "PSReadLine" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected IMPORTS edge for Import-Module PSReadLine")
	}
}

// ─── Test 10: #Requires -Modules import ──────────────────────────────────────

func TestPowerShellParser_ExtractsRequiresModules(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	fileNodes := g.FindByName("deploy.ps1")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "ActiveDirectory" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected IMPORTS edge for #Requires -Modules ActiveDirectory")
	}
}

// ─── Test 11: using module import ────────────────────────────────────────────

func TestPowerShellParser_ExtractsUsingModule(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	fileNodes := g.FindByName("deploy.ps1")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	found := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil && n.Package == "./lib/Helpers" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("expected IMPORTS edge for 'using module ./lib/Helpers'")
	}
}

// ─── Test 12: Method inside class ────────────────────────────────────────────

func TestPowerShellParser_ExtractsMethodInsideClass(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	// Methods are stored as "ClassName.MethodName"
	nodes := g.FindByName("DatabaseManager.Connect")
	if len(nodes) == 0 {
		t.Fatal("expected DatabaseManager.Connect method node")
	}
	n := nodes[0]
	if n.Type != graph.NodeFunction {
		t.Errorf("DatabaseManager.Connect: type = %q, want NodeFunction", n.Type)
	}
}

// ─── Test 13: Doc comment extraction (# style) ───────────────────────────────

func TestPowerShellParser_ExtractsLineDocComment(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	nodes := g.FindByName("Write-Log")
	if len(nodes) == 0 {
		t.Fatal("expected Write-Log function node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc == "" {
		t.Error("Write-Log should have a doc comment")
	}
	if doc != "Write a log entry to disk" {
		t.Errorf("Write-Log doc = %q, want 'Write a log entry to disk'", doc)
	}
}

// ─── Test 14: Doc comment extraction (<# #> style) ───────────────────────────

func TestPowerShellParser_ExtractsBlockDocComment(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	nodes := g.FindByName("Get-ADUserInfo")
	if len(nodes) == 0 {
		t.Fatal("expected Get-ADUserInfo function node")
	}
	doc := nodes[0].Metadata["doc"]
	if doc == "" {
		t.Error("Get-ADUserInfo should have a block doc comment")
	}
	if !containsAll(doc, "Get a user", "Active Directory") {
		t.Errorf("Get-ADUserInfo doc = %q, expected content about 'Get a user from Active Directory'", doc)
	}
}

// ─── Test 15: Empty file — no crash, file node exists ────────────────────────

func TestPowerShellParser_EmptyFile(t *testing.T) {
	g := graph.New("testrepo")
	p := parser.NewPowerShellParser()
	if err := p.Parse(g, "/tmp/empty.ps1", []byte("")); err != nil {
		t.Fatalf("Parse() on empty .ps1 returned error: %v", err)
	}
	if g.NodeCount() == 0 {
		t.Error("Parse() produced zero nodes; expected at least a file node")
	}
	nodes := g.FindByName("empty.ps1")
	if len(nodes) == 0 {
		t.Fatal("file node not found for empty file")
	}
	if nodes[0].Type != graph.NodeFile {
		t.Errorf("empty file: node type = %q, want NodeFile", nodes[0].Type)
	}
}

// ─── Test 16: Function with parameters ───────────────────────────────────────

func TestPowerShellParser_FunctionWithParameters(t *testing.T) {
	src := `function Set-Configuration {
    param(
        [string]$Key,
        [string]$Value,
        [switch]$Force
    )
    Set-ItemProperty -Path "HKCU:\\Software" -Name $Key -Value $Value
}
`
	g := parsePowerShell(t, src, "/tmp/config.ps1")
	nodes := g.FindByName("Set-Configuration")
	if len(nodes) == 0 {
		t.Fatal("expected Set-Configuration function node")
	}
	if nodes[0].Type != graph.NodeFunction {
		t.Errorf("Set-Configuration: type = %q, want NodeFunction", nodes[0].Type)
	}
	if !nodes[0].Exported {
		t.Error("Set-Configuration should be exported (Verb-Noun with dash)")
	}
}

// ─── Test 17: DEFINES edge from file to function ─────────────────────────────

func TestPowerShellParser_DefinesEdgeFromFileToFunction(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	fileNodes := g.FindByName("deploy.ps1")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	foundWriteLog := false
	foundGetADUserInfo := false
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeDefines {
			n := g.GetNode(e.To)
			if n == nil {
				continue
			}
			if n.Name == "Write-Log" {
				foundWriteLog = true
			}
			if n.Name == "Get-ADUserInfo" {
				foundGetADUserInfo = true
			}
		}
	}
	if !foundWriteLog {
		t.Error("no DEFINES edge from file to Write-Log")
	}
	if !foundGetADUserInfo {
		t.Error("no DEFINES edge from file to Get-ADUserInfo")
	}
}

// ─── Test 18: IMPORTS edge from file to module ───────────────────────────────

func TestPowerShellParser_ImportsEdgeFromFileToModule(t *testing.T) {
	g := parsePowerShell(t, psMainSource, "/tmp/deploy.ps1")
	fileNodes := g.FindByName("deploy.ps1")
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
		t.Errorf("expected at least 3 import edges (ActiveDirectory, Helpers, PSReadLine), got %d", importCount)
	}
}

// ─── Test 19: Module file — all functions exported ───────────────────────────

func TestPowerShellParser_ModuleFileExportsAllFunctions(t *testing.T) {
	src := `function Get-Data { }
function Set-Data { }
function internalHelper { }
`
	g := parsePowerShell(t, src, "/tmp/MyModule.psm1")
	// In a .psm1 module, all functions (even lowercase) are exported unless _prefixed.
	for _, name := range []string{"Get-Data", "Set-Data", "internalHelper"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s function node in module", name)
			continue
		}
		if !nodes[0].Exported {
			t.Errorf("%s in .psm1 should be exported", name)
		}
	}
}

// ─── Test 20: Nested function (inner function) ────────────────────────────────

func TestPowerShellParser_NestedFunction(t *testing.T) {
	src := `function Outer-Function {
    function inner-helper {
        Write-Output "inner"
    }
    inner-helper
}
`
	g := parsePowerShell(t, src, "/tmp/nested.ps1")
	// Both outer and inner should be extracted.
	outerNodes := g.FindByName("Outer-Function")
	if len(outerNodes) == 0 {
		t.Error("expected Outer-Function node")
	}
	innerNodes := g.FindByName("inner-helper")
	if len(innerNodes) == 0 {
		t.Error("expected inner-helper node")
	}
}

// ─── Test 21: Class with base type ───────────────────────────────────────────

func TestPowerShellParser_ClassWithBaseType(t *testing.T) {
	src := `class Animal {
    [string]$Name

    Speak() {
        Write-Output "..."
    }
}

class Dog : Animal {
    Speak() {
        Write-Output "Woof!"
    }
}
`
	g := parsePowerShell(t, src, "/tmp/animals.ps1")
	animalNodes := g.FindByName("Animal")
	if len(animalNodes) == 0 {
		t.Fatal("expected Animal class node")
	}
	dogNodes := g.FindByName("Dog")
	if len(dogNodes) == 0 {
		t.Fatal("expected Dog class node")
	}
	if dogNodes[0].Type != graph.NodeStruct {
		t.Errorf("Dog: type = %q, want NodeStruct", dogNodes[0].Type)
	}
}

// ─── Test 22: Multiple enums ──────────────────────────────────────────────────

func TestPowerShellParser_MultipleEnums(t *testing.T) {
	src := `enum Status {
    Active
    Inactive
    Pending
}

enum Priority {
    Low
    Medium
    High
    Critical
}
`
	g := parsePowerShell(t, src, "/tmp/enums.ps1")
	for _, name := range []string{"Status", "Priority"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected %s enum node", name)
			continue
		}
		if nodes[0].Type != graph.NodeStruct {
			t.Errorf("%s: type = %q, want NodeStruct", name, nodes[0].Type)
		}
		if nodes[0].Metadata["kind"] != "enum" {
			t.Errorf("%s: metadata[kind] = %q, want 'enum'", name, nodes[0].Metadata["kind"])
		}
	}
}

// ─── Test 23: Multiple module imports ────────────────────────────────────────

func TestPowerShellParser_MultipleModuleImports(t *testing.T) {
	src := `#Requires -Modules ActiveDirectory, GroupPolicy

Import-Module PSReadLine
Import-Module Az.Accounts
`
	g := parsePowerShell(t, src, "/tmp/multi.ps1")
	fileNodes := g.FindByName("multi.ps1")
	if len(fileNodes) == 0 {
		t.Fatal("file node not found")
	}
	fileID := fileNodes[0].ID

	importedPkgs := make(map[string]bool)
	for _, e := range g.OutEdges(fileID) {
		if e.Type == graph.EdgeImports {
			n := g.GetNode(e.To)
			if n != nil {
				importedPkgs[n.Package] = true
			}
		}
	}

	for _, expected := range []string{"ActiveDirectory", "GroupPolicy", "PSReadLine", "Az.Accounts"} {
		if !importedPkgs[expected] {
			t.Errorf("expected import for %q", expected)
		}
	}
}

// ─── Test 24: Psd1 file extensions included ──────────────────────────────────

func TestPowerShellParser_PSD1ExtensionIncluded(t *testing.T) {
	exts := parser.NewPowerShellParser().Extensions()
	found := false
	for _, e := range exts {
		if e == ".psd1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Extensions() = %v, missing .psd1", exts)
	}
}

// ─── Test 25: DSC configuration blocks ───────────────────────────────────────

func TestPowerShellParserDSCConfiguration(t *testing.T) {
	src := []byte(`
<#
.SYNOPSIS
Configures a web server node.
#>
configuration WebServerConfig {
    Import-DscResource -ModuleName PSDesiredStateConfiguration

    Node "web01" {
        WindowsFeature IIS {
            Ensure = "Present"
            Name   = "Web-Server"
        }
    }
}

configuration DatabaseConfig {
    Node $AllNodes.NodeName {
        SqlDatabase TestDb {
            Ensure = "Present"
        }
    }
}
`)
	g := graph.New("testrepo")
	p := parser.NewPowerShellParser()
	if err := p.Parse(g, "server.ps1", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configs := []string{"WebServerConfig", "DatabaseConfig"}
	for _, name := range configs {
		nodes := g.FindByName(name)
		var n *graph.Node
		for _, candidate := range nodes {
			if candidate.Name == name {
				n = candidate
				break
			}
		}
		if n == nil {
			t.Errorf("expected DSC configuration %q not found", name)
			continue
		}
		if n.Type != graph.NodeStruct {
			t.Errorf("%q: expected NodeStruct, got %v", name, n.Type)
		}
		if n.Metadata["kind"] != "configuration" {
			t.Errorf("%q: expected kind=configuration, got %q", name, n.Metadata["kind"])
		}
	}
}

// ─── Test 26: PowerShell Workflow blocks ─────────────────────────────────────

func TestPowerShellParserWorkflow(t *testing.T) {
	src := []byte(`
workflow Invoke-DeploymentWorkflow {
    param (
        [string]$Environment
    )
    InlineScript {
        Write-Output "Deploying to $Using:Environment"
    }
}

workflow BackupWorkflow {
    Checkpoint-Workflow
}
`)
	g := graph.New("testrepo")
	p := parser.NewPowerShellParser()
	if err := p.Parse(g, "/scripts/deploy.ps1", src); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"Invoke-DeploymentWorkflow", "BackupWorkflow"} {
		nodes := g.FindByName(name)
		if len(nodes) == 0 {
			t.Errorf("expected workflow node %q", name)
			continue
		}
		if nodes[0].Metadata["kind"] != "workflow" {
			t.Errorf("%q: kind = %q, want workflow", name, nodes[0].Metadata["kind"])
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// containsAll returns true if s contains all of the provided substrings.
func containsAll(s string, substrings ...string) bool {
	for _, sub := range substrings {
		found := false
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
