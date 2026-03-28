package mcp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
)

// ── handleValidatePlan ────────────────────────────────────────────────────────

func TestHandleValidatePlan_NoRules(t *testing.T) {
	s := newTestServer(t)
	// validate_plan expects changes as a JSON string.
	res, err := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": `[{"file":"pkg/auth/auth.go","action":"modify","description":"add token refresh"}]`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "violations")
	violations, _ := m["violations"].([]any)
	if len(violations) != 0 {
		t.Errorf("expected no violations with no rules, got %d", len(violations))
	}
}

func TestHandleValidatePlan_WithRule(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "test-rule",
		"description": "no direct DB calls from handlers",
		"severity":    "error",
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": `[{"file":"internal/store/store.go","action":"modify","description":"add new method"}]`,
	}))
	m := mustResult(t, res2, err2)
	hasKey(t, m, "violations")
}

func TestHandleValidatePlan_MissingChanges_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleValidatePlan(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// ── handleVerifyImplementation ────────────────────────────────────────────────

func TestHandleVerifyImplementation_SingleFile(t *testing.T) {
	s, _, _ := newPopulatedServer(t)
	// verify_implementation takes files_written (JSON string of file paths).
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["pkg/auth/auth.go"]`,
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "files")
}

func TestHandleVerifyImplementation_MissingFilesWritten_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleVerifyImplementation(ctx, callTool(nil))
	mustErrorResult(t, res, err)
}

// BUG-EVAL-8: high-fanin exported symbols must appear in signature_impact even
// when their signature didn't change (no store prev_signature record).
func TestHandleVerifyImplementation_HighFaninExport_AppearsInSignatureImpact(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Build graph: CoreService (exported function in core/service.go) with 12 callers.
	coreFile := "core/service.go"
	coreID := g.MakeNodeID(coreFile, "CoreService")
	g.AddNode(&graph.Node{
		ID: coreID, Type: graph.NodeFunction,
		Name: "CoreService", File: coreFile, Line: 1, Package: "service", Exported: true,
	})
	// Add 12 callers — above the highFaninThreshold of 10.
	for i := 0; i < 12; i++ {
		callerFile := filepath.Join("pkg", "caller"+string(rune('A'+i)), "handler.go")
		callerName := "Handle" + string(rune('A'+i))
		callerID := g.MakeNodeID(callerFile, callerName)
		g.AddNode(&graph.Node{
			ID: callerID, Type: graph.NodeFunction,
			Name: callerName, File: callerFile, Line: 1, Package: "caller", Exported: true,
		})
		g.AddEdge(&graph.Edge{From: callerID, To: coreID, Type: graph.EdgeCalls})
	}
	// Also add an unexported function in the same file — must NOT appear.
	unexportedID := g.MakeNodeID(coreFile, "internalHelper")
	g.AddNode(&graph.Node{
		ID: unexportedID, Type: graph.NodeFunction,
		Name: "internalHelper", File: coreFile, Line: 10, Package: "service", Exported: false,
	})

	s := New(g, cfg, st)
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["core/service.go"]`,
	}))
	m := mustResult(t, res, err)

	files, ok := m["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatalf("expected files array, got %v", m["files"])
	}
	fileReport, ok := files[0].(map[string]any)
	if !ok {
		t.Fatalf("expected file report map, got %T", files[0])
	}
	sigImpact, ok := fileReport["signature_impact"].([]any)
	if !ok || len(sigImpact) == 0 {
		t.Fatalf("expected signature_impact for high-fanin export, got %v (full report: %v)", fileReport["signature_impact"], fileReport)
	}

	// Verify the entry has Warning set (high-fanin, not a signature change).
	found := false
	for _, entry := range sigImpact {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if e["symbol"] == "CoreService" {
			found = true
			if e["warning"] == nil || e["warning"] == "" {
				t.Errorf("expected warning field set for high-fanin entry, got %v", e)
			}
			callers, ok2 := e["callers"].([]any)
			if !ok2 || len(callers) == 0 {
				t.Errorf("expected callers list in high-fanin entry, got %v", e["callers"])
			}
		}
	}
	if !found {
		t.Errorf("CoreService not found in signature_impact; entries: %v", sigImpact)
	}

	// Unexported function must NOT appear in signature_impact.
	for _, entry := range sigImpact {
		if e, ok := entry.(map[string]any); ok {
			if e["symbol"] == "internalHelper" {
				t.Errorf("unexported internalHelper should not appear in signature_impact")
			}
		}
	}

	// impact_warnings count should reflect the high-fanin entry.
	impactWarnings, _ := m["impact_warnings"].(float64)
	if impactWarnings < 1 {
		t.Errorf("expected impact_warnings >= 1, got %v", m["impact_warnings"])
	}
}

// BUG-EVAL-8: fanin-ordered scan — high-fanin symbol must be found even when
// many low-fanin exported symbols appear before it alphabetically.
func TestHandleVerifyImplementation_HighFaninFoundFirst_WhenManyLowFaninPrecede(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	svcFile := "svc/service.go"

	// Add ZTopFanin: alphabetically last but has 12 callers — must be found.
	topID := g.MakeNodeID(svcFile, "ZTopFanin")
	g.AddNode(&graph.Node{
		ID: topID, Type: graph.NodeFunction,
		Name: "ZTopFanin", File: svcFile, Line: 1, Package: "svc", Exported: true,
	})
	for i := 0; i < 12; i++ {
		callerID := g.MakeNodeID(filepath.Join("caller", string(rune('A'+i))+".go"), "Call"+string(rune('A'+i)))
		g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "Call" + string(rune('A'+i)), File: filepath.Join("caller", string(rune('A'+i))+".go"), Package: "caller", Exported: true})
		g.AddEdge(&graph.Edge{From: callerID, To: topID, Type: graph.EdgeCalls})
	}

	// Add AaLowFanin through AmLowFanin: alphabetically first, 0 callers each.
	// With alphabetical sort these 13 nodes would fill the scan cap before ZTopFanin.
	for i := 0; i < 13; i++ {
		lowID := g.MakeNodeID(svcFile, "Aa"+string(rune('a'+i))+"LowFanin")
		g.AddNode(&graph.Node{
			ID: lowID, Type: graph.NodeFunction,
			Name: "Aa" + string(rune('a'+i)) + "LowFanin", File: svcFile, Line: 20 + i, Package: "svc", Exported: true,
		})
	}

	s := New(g, cfg, st)
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["svc/service.go"]`,
	}))
	m := mustResult(t, res, err)

	files, ok := m["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatalf("expected files array, got %v", m["files"])
	}
	fileReport := files[0].(map[string]any)
	sigImpact, _ := fileReport["signature_impact"].([]any)

	found := false
	for _, entry := range sigImpact {
		if e, ok := entry.(map[string]any); ok && e["symbol"] == "ZTopFanin" {
			found = true
		}
	}
	if !found {
		t.Errorf("ZTopFanin (12 callers, alphabetically last) should appear in signature_impact even with 13 low-fanin symbols before it alphabetically; got entries: %v", sigImpact)
	}
}

// BUG-EVAL-8: EMBEDS-heavy structs must not fill the maxExportsToCheck scan cap
// and crowd out a function with genuine CALLS callers.
// The fix: sort by CALLS-only fanin so structs with zero CALLS edges are always
// scanned after any high-CALLS function regardless of their total (EMBEDS) fanin.
func TestHandleVerifyImplementation_EmbedsFaninNotInflatesScanBudget(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	svcFile := "svc/service.go"

	// CallsFunc: exported function with 12 CALLS callers — must appear in signature_impact.
	callsFuncID := g.MakeNodeID(svcFile, "CallsFunc")
	g.AddNode(&graph.Node{
		ID: callsFuncID, Type: graph.NodeFunction,
		Name: "CallsFunc", File: svcFile, Line: 1, Package: "svc", Exported: true,
	})
	for i := 0; i < 12; i++ {
		callerID := g.MakeNodeID(filepath.Join("caller", string(rune('A'+i))+".go"), "C"+string(rune('A'+i)))
		g.AddNode(&graph.Node{ID: callerID, Type: graph.NodeFunction, Name: "C" + string(rune('A'+i)),
			File: filepath.Join("caller", string(rune('A'+i))+".go"), Package: "caller", Exported: true})
		g.AddEdge(&graph.Edge{From: callerID, To: callsFuncID, Type: graph.EdgeCalls})
	}

	// EmbedStruct: exported struct with 20 EMBEDS-type in-edges — CALLS fanin = 0.
	// With old code (sort by total Fanin), this struct's fanin=20 > CallsFunc's fanin=12,
	// so it would be sorted first and consume a scan slot. With 50 such structs filling
	// the cap, CallsFunc would never be reached.
	for i := 0; i < 50; i++ {
		structID := g.MakeNodeID(svcFile, "EmbedStruct"+string(rune('A'+i%26))+string(rune('a'+i%26)))
		g.AddNode(&graph.Node{
			ID: structID, Type: graph.NodeStruct,
			Name: "EmbedStruct" + string(rune('A'+i%26)) + string(rune('a'+i%26)),
			File: svcFile, Line: 100 + i, Package: "svc", Exported: true,
		})
		// Add 20 EMBEDS edges (not CALLS) — high total fanin, zero CALLS fanin.
		for j := 0; j < 20; j++ {
			embedderID := g.MakeNodeID(
				filepath.Join("embed", string(rune('A'+i%26))+string(rune('a'+j%26))+".go"),
				"Embedder"+string(rune('A'+i%26))+string(rune('a'+j%26)),
			)
			g.AddNode(&graph.Node{
				ID: embedderID, Type: graph.NodeStruct,
				Name:    "Embedder" + string(rune('A'+i%26)) + string(rune('a'+j%26)),
				File:    filepath.Join("embed", string(rune('A'+i%26))+string(rune('a'+j%26))+".go"),
				Package: "embed", Exported: true,
			})
			g.AddEdge(&graph.Edge{From: embedderID, To: structID, Type: graph.EdgeEmbeds})
		}
	}

	s := New(g, cfg, st)
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["svc/service.go"]`,
	}))
	m := mustResult(t, res, err)

	files, ok := m["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatalf("expected files array, got %v", m["files"])
	}
	fileReport := files[0].(map[string]any)
	sigImpact, _ := fileReport["signature_impact"].([]any)

	// CallsFunc must appear — it has 12 real CALLS callers.
	found := false
	for _, entry := range sigImpact {
		if e, ok := entry.(map[string]any); ok && e["symbol"] == "CallsFunc" {
			found = true
		}
	}
	if !found {
		t.Errorf("CallsFunc (12 CALLS callers) not found in signature_impact — EMBEDS structs likely consumed the scan budget; entries: %v", sigImpact)
	}

	// EmbedStruct nodes must NOT appear — they have zero CALLS callers.
	for _, entry := range sigImpact {
		if e, ok := entry.(map[string]any); ok {
			sym, _ := e["symbol"].(string)
			if strings.HasPrefix(sym, "EmbedStruct") {
				t.Errorf("EmbedStruct (EMBEDS-only callers) must not appear in signature_impact, got symbol=%q", sym)
			}
		}
	}
}

// BUG-EVAL-8: interface with high IMPLEMENTS fanin must appear in signature_impact.
// This proves the CALLS+IMPLEMENTS callFanin fix works for the IMPLEMENTS branch —
// a NodeInterface with zero CALLS callers but 12 IMPLEMENTS callers is a high
// blast-radius symbol and must be surfaced as a signature-change warning.
func TestHandleVerifyImplementation_ImplementsFaninFoundInSignatureImpact(t *testing.T) {
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	svcFile := "svc/service.go"

	// IfaceService: exported interface with 12 IMPLEMENTS callers and 0 CALLS callers.
	ifaceID := g.MakeNodeID(svcFile, "IfaceService")
	g.AddNode(&graph.Node{
		ID: ifaceID, Type: graph.NodeInterface,
		Name: "IfaceService", File: svcFile, Line: 1, Package: "svc", Exported: true,
	})
	for i := 0; i < 12; i++ {
		implFile := filepath.Join("impl", string(rune('A'+i))+".go")
		implName := "Impl" + string(rune('A'+i))
		implID := g.MakeNodeID(implFile, implName)
		g.AddNode(&graph.Node{
			ID: implID, Type: graph.NodeStruct,
			Name: implName, File: implFile, Package: "impl", Exported: true,
		})
		g.AddEdge(&graph.Edge{From: implID, To: ifaceID, Type: graph.EdgeImplements})
	}

	// LowFanin: add 13 exported functions with 0 callers — would fill scan cap alphabetically.
	for i := 0; i < 13; i++ {
		lowID := g.MakeNodeID(svcFile, "Aa"+string(rune('a'+i))+"LowFanin")
		g.AddNode(&graph.Node{
			ID: lowID, Type: graph.NodeFunction,
			Name: "Aa" + string(rune('a'+i)) + "LowFanin", File: svcFile, Line: 20 + i, Package: "svc", Exported: true,
		})
	}

	s := New(g, cfg, st)
	res, err := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": `["svc/service.go"]`,
	}))
	m := mustResult(t, res, err)

	files, ok := m["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatalf("expected files array, got %v", m["files"])
	}
	fileReport := files[0].(map[string]any)
	sigImpact, _ := fileReport["signature_impact"].([]any)

	// IfaceService must appear — it has 12 IMPLEMENTS callers.
	found := false
	for _, entry := range sigImpact {
		if e, ok := entry.(map[string]any); ok && e["symbol"] == "IfaceService" {
			found = true
			if e["warning"] == nil || e["warning"] == "" {
				t.Errorf("expected warning field set for IfaceService (high IMPLEMENTS fanin), got %v", e)
			}
		}
	}
	if !found {
		t.Errorf("IfaceService (12 IMPLEMENTS callers, 0 CALLS callers) not found in signature_impact — IMPLEMENTS edges may not be counted by callFanin; entries: %v", sigImpact)
	}
}

// ── handleGetViolations ───────────────────────────────────────────────────────

func TestHandleGetViolations_Empty(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "violations")
	// R32: open_quality_gaps key must always be present (empty slice when no gaps).
	hasKey(t, m, "open_quality_gaps")
	if m["quality_gap_count"].(float64) != 0 {
		t.Errorf("expected 0 quality gaps in fresh store, got %v", m["quality_gap_count"])
	}
}

func TestHandleGetViolations_SurfacesOpenQualityGaps(t *testing.T) {
	s := newTestServer(t)
	// Insert a quality gap.
	_, _ = s.handleUpsertGap(ctx, callTool(map[string]any{
		"node_id":     "parser.go:DetectProvenance",
		"gap_id":      "dist-relative-path",
		"description": "dist/ relative path not matched",
		"severity":    "medium",
	}))

	res, err := s.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res, err)
	hasKey(t, m, "open_quality_gaps")
	if m["quality_gap_count"].(float64) != 1 {
		t.Errorf("expected 1 open quality gap, got %v", m["quality_gap_count"])
	}
}

func TestHandleGetViolations_AfterRuleUpsert(t *testing.T) {
	// Build a server with a graph that actually violates a rule.
	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	cfg, _ := config.Load(t.TempDir())

	// Add a forbidden import edge: cmd imports internal.
	cmdID := g.MakeNodeID("cmd/main.go", "main")
	g.AddNode(&graph.Node{ID: cmdID, Name: "main", File: "cmd/main.go", Package: "main", Type: graph.NodeFunction})
	internalID := g.MakeNodeID("internal/secret/secret.go", "Secret")
	g.AddNode(&graph.Node{ID: internalID, Name: "Secret", File: "internal/secret/secret.go", Package: "secret", Type: graph.NodeFunction})
	g.AddEdge(&graph.Edge{From: cmdID, To: internalID, Type: graph.EdgeImports})

	s := New(g, cfg, st)

	// Add rule that catches this.
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "no-secret-import",
		"description": "cmd must not import secret package",
		"severity":    "error",
		"forbidden_imports": []any{
			map[string]any{"from": "cmd/.*", "to": "internal/secret/.*"},
		},
	}))
	mustResult(t, res, err)

	res2, err2 := s.handleGetViolations(ctx, callTool(nil))
	m := mustResult(t, res2, err2)
	hasKey(t, m, "violations")
}

// ── handleUpsertRule ──────────────────────────────────────────────────────────

func TestHandleUpsertRule_CreatesRule(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "test-rule-1",
		"description": "no circular imports",
		"severity":    "error",
	}))
	m := mustResult(t, res, err)
	hasKey(t, m, "rule_id")

	// Rule should be retrievable and affect future validate_plan calls.
	res2, err2 := s.handleGetViolations(ctx, callTool(nil))
	m2 := mustResult(t, res2, err2)
	hasKey(t, m2, "violations")
}

func TestHandleUpsertRule_MissingRuleID_ReturnsError(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"description": "some rule",
		"severity":    "error",
	}))
	mustErrorResult(t, res, err)
}

// R28: Semantic Firewall — upsert_rule must block when context_source is external or generated.
func TestHandleUpsertRule_BlocksExternalContextSource(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":        "firewall-test",
		"description":    "rule from external page",
		"severity":       "error",
		"context_source": "external",
	}))
	mustErrorResult(t, res, err)
}

func TestHandleUpsertRule_BlocksGeneratedContextSource(t *testing.T) {
	s := newTestServer(t)
	res, err := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":        "firewall-test",
		"description":    "rule derived from generated protobuf",
		"severity":       "warning",
		"context_source": "generated",
	}))
	mustErrorResult(t, res, err)
}
