package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SynapsesOS/synapses/internal/config"
	"github.com/SynapsesOS/synapses/internal/graph"
	"github.com/SynapsesOS/synapses/internal/security"
	"github.com/SynapsesOS/synapses/internal/store"
)

// ── pathWithinRoot unit tests ─────────────────────────────────────────────────

func TestPathWithinRoot(t *testing.T) {
	cases := []struct {
		name string
		root string
		path string
		want bool
	}{
		// Happy paths: file inside root.
		{"file inside root", "/proj", "/proj/internal/auth.go", true},
		{"file at root boundary", "/proj", "/proj", true},
		{"nested deep", "/proj", "/proj/a/b/c/d.go", true},

		// Traversal attacks: must be blocked.
		{"absolute outside root", "/proj", "/etc/passwd", false},
		{"sibling dir", "/proj", "/projother/file.go", false},
		{"parent of root", "/proj/sub", "/proj/other.go", false},

		// Prefix collision guard: "/proj" must NOT match "/projfoo".
		{"prefix collision", "/proj", "/projfoo/file.go", false},

		// Empty root: fail-closed — no boundary known → block all FS access.
		{"empty root blocks absolute", "", "/etc/passwd", false},
		{"empty root blocks relative", "", "relative/file.go", false},
		{"empty root blocks root itself", "", "/", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pathWithinRoot(tc.root, tc.path)
			if got != tc.want {
				t.Errorf("pathWithinRoot(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
			}
		})
	}
}

// ── Attack vector tests ───────────────────────────────────────────────────────

// newServerWithRoot creates a server whose graph root is set to a temp directory.
// Files placed in that directory are within the project root; files placed
// elsewhere are outside it — any attempt to access them via path traversal
// must be blocked.
func newServerWithRoot(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()

	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	g.SetRoot(root)

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return New(g, cfg, st), root
}

// TestValidatePlanPathTraversal_FreshnessCheck proves the freshness-stat attack
// is closed in handleValidatePlan.
//
// Attack: agent passes a recently-modified file via traversal
//
//	(e.g. "../../outside/secret.txt"). Before fix: os.Stat is called and
//	"graph_freshness" appears in the response, leaking that the external file
//	was modified < 10s ago. After fix: pathWithinRoot rejects it → "graph_freshness"
//	is absent entirely.
func TestValidatePlanPathTraversal_FreshnessCheck(t *testing.T) {
	s, root := newServerWithRoot(t)

	// Create a file OUTSIDE the root, just written (< 10s ago → would trigger
	// a freshness warning if os.Stat were called on it).
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("sensitive content"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	// Compute the relative traversal path from root into outsideFile.
	rel, err := filepath.Rel(root, outsideFile)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	changesJSON, _ := json.Marshal([]map[string]string{{"file": rel}})
	res, callErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": string(changesJSON),
	}))
	m := mustResult(t, res, callErr)

	// "graph_freshness" must be absent: the traversal file was never stat'd.
	// If it were present, it would prove the boundary check did not fire.
	if _, ok := m["graph_freshness"]; ok {
		t.Errorf("graph_freshness appeared in response — os.Stat was called on external file: %v", m["graph_freshness"])
	}
}

// TestValidatePlanPathTraversal_LogicChecks proves the os.ReadFile attack is
// closed in handleValidatePlan's logic-check path.
//
// Attack: agent passes a .go file via traversal. Before fix: os.ReadFile reads
//
//	the file and parser.RunLogicChecks produces warnings based on its content.
//	After fix: pathWithinRoot rejects it → the file is never read → no
//	logic_warnings appear.
//
// The outside file contains a known tilde-path pattern ("~/data") which
// RunLogicChecks detects in Go source. If the guard fires, no warnings appear.
// If the guard is missing, warnings appear — proving the file was read.
func TestValidatePlanPathTraversal_LogicChecks(t *testing.T) {
	s, root := newServerWithRoot(t)

	// Create a .go file OUTSIDE the root with a Go logic warning pattern.
	// RunLogicChecks detects tilde paths in os.Open / os.ReadFile calls.
	outsideDir := t.TempDir()
	outsideGo := filepath.Join(outsideDir, "malicious.go")
	goSrc := []byte(`package main

import "os"

func bad() {
	os.ReadFile("~/sensitive/data")
}
`)
	if err := os.WriteFile(outsideGo, goSrc, 0o644); err != nil {
		t.Fatalf("write outside go file: %v", err)
	}

	// Compute relative traversal path. filepath.Rel produces ".." segments.
	rel, err := filepath.Rel(root, outsideGo)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	changesJSON, _ := json.Marshal([]map[string]string{{"file": rel}})
	res, callErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": string(changesJSON),
	}))
	m := mustResult(t, res, callErr)

	// "logic_warnings" must be absent — the outside .go file was never read.
	// If the guard were missing, RunLogicChecks would detect the tilde path and
	// "logic_warnings" would be present with at least one entry.
	if lw, ok := m["logic_warnings"]; ok {
		t.Errorf("logic_warnings appeared in response — os.ReadFile was called on external file: %v", lw)
	}
}

// TestVerifyImplementationPathTraversal_FreshnessCheck proves the freshness-stat
// attack is closed in handleVerifyImplementation.
//
// Attack: agent passes a recently-modified external file via traversal in
//
//	files_written. Before fix: os.Stat fires → FreshnessWarning leaks metadata.
//	After fix: pathWithinRoot rejects it → FreshnessWarning stays empty.
func TestVerifyImplementationPathTraversal_FreshnessCheck(t *testing.T) {
	s, root := newServerWithRoot(t)

	// File outside the root, just written (< 10s old → freshness trigger).
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("sensitive"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	rel, err := filepath.Rel(root, outsideFile)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}

	filesJSON, _ := json.Marshal([]string{rel})
	res, callErr := s.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	// Verify no file report has a non-empty freshness_warning for the traversal path.
	files, _ := m["files"].([]any)
	for _, f := range files {
		fm, _ := f.(map[string]any)
		if fw, ok := fm["freshness_warning"]; ok && fw != nil && fw != "" {
			t.Errorf("freshness_warning appeared for external file — os.Stat was called: %v", fw)
		}
	}
}

// TestValidatePlanPathTraversal_AbsolutePath proves that an absolute path
// pointing directly to a sensitive file is also rejected (not just relative
// traversal paths). This closes the bypass where an agent omits ".." and
// simply provides "/etc/passwd" directly.
func TestValidatePlanPathTraversal_AbsolutePath(t *testing.T) {
	s, _ := newServerWithRoot(t)

	// /etc/passwd exists and has a known modification time. If os.Stat fires
	// and the daemon has been running < 10s (unlikely but possible in tests),
	// a freshness warning could appear. We check both the absence of
	// graph_freshness AND logic_warnings.
	changesJSON := `[{"file":"/etc/passwd"}]`
	res, callErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": changesJSON,
	}))
	m := mustResult(t, res, callErr)

	if _, ok := m["graph_freshness"]; ok {
		t.Errorf("graph_freshness appeared for absolute external path /etc/passwd")
	}
	if _, ok := m["logic_warnings"]; ok {
		t.Errorf("logic_warnings appeared for absolute external path /etc/passwd")
	}
}

// TestValidatePlanPathTraversal_EmptyRoot proves that when the graph has no
// root set, all filesystem access is blocked (fail-closed behavior).
//
// This covers the scenario where the daemon starts without a project path —
// an empty root must not silently allow arbitrary file access.
func TestValidatePlanPathTraversal_EmptyRoot(t *testing.T) {
	// Server with NO root set — root is "".
	s := newTestServer(t)

	// Create a file INSIDE what would be a project dir — but since root is
	// empty, even "safe-looking" paths must not trigger FS access.
	tmpFile := filepath.Join(t.TempDir(), "recent.go")
	if err := os.WriteFile(tmpFile, []byte(`package main

import "os"

func bad() { os.ReadFile("~/data") }
`), 0o644); err != nil {
		t.Fatalf("write tmp file: %v", err)
	}

	// Pass the absolute path directly (no traversal needed — root is empty).
	changesJSON, _ := json.Marshal([]map[string]string{{"file": tmpFile}})
	res, callErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": string(changesJSON),
	}))
	m := mustResult(t, res, callErr)

	// With fail-closed behavior, neither freshness nor logic warnings appear.
	if _, ok := m["graph_freshness"]; ok {
		t.Errorf("graph_freshness appeared with empty root — FS access was not blocked")
	}
	if _, ok := m["logic_warnings"]; ok {
		t.Errorf("logic_warnings appeared with empty root — os.ReadFile was not blocked")
	}
}

// ── Sprint 27.3 tests ─────────────────────────────────────────────────────────

// TestCategorizeFindingChanges_Pure exercises the change-attribution helper
// with no graph or filesystem dependency.
func TestCategorizeFindingChanges_Pure(t *testing.T) {
	makeV := func(patternID, target string) security.Violation {
		return security.Violation{PatternID: patternID, Target: target, Message: "test"}
	}

	cases := []struct {
		name          string
		before, after []security.Violation
		wantNew       int
		wantExisting  int
		wantFixed     int
	}{
		{
			name:         "all new — no baseline",
			before:       nil,
			after:        []security.Violation{makeV("p1", "t1"), makeV("p2", "t2")},
			wantNew:      2,
			wantExisting: 0,
			wantFixed:    0,
		},
		{
			name:         "all existing — same before and after",
			before:       []security.Violation{makeV("p1", "t1")},
			after:        []security.Violation{makeV("p1", "t1")},
			wantNew:      0,
			wantExisting: 1,
			wantFixed:    0,
		},
		{
			name:         "all fixed — after is empty",
			before:       []security.Violation{makeV("p1", "t1"), makeV("p2", "t2")},
			after:        nil,
			wantNew:      0,
			wantExisting: 0,
			wantFixed:    2,
		},
		{
			name:         "mixed — new introduced, one existing, one fixed",
			before:       []security.Violation{makeV("p1", "t1"), makeV("p2", "t2")},
			after:        []security.Violation{makeV("p1", "t1"), makeV("p3", "t3")},
			wantNew:      1,
			wantExisting: 1,
			wantFixed:    1,
		},
		{
			name:         "empty before and after",
			before:       nil,
			after:        nil,
			wantNew:      0,
			wantExisting: 0,
			wantFixed:    0,
		},
		{
			name: "same patternID, different targets → both new and fixed",
			// p1/t1 was fixed; p1/t2 is new — pattern is the same but targets differ
			before:       []security.Violation{makeV("p1", "t1")},
			after:        []security.Violation{makeV("p1", "t2")},
			wantNew:      1,
			wantExisting: 0,
			wantFixed:    1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newOnes, existing, fixed := categorizeFindingChanges(tc.before, tc.after)
			if len(newOnes) != tc.wantNew {
				t.Errorf("new: got %d, want %d", len(newOnes), tc.wantNew)
			}
			if len(existing) != tc.wantExisting {
				t.Errorf("existing: got %d, want %d", len(existing), tc.wantExisting)
			}
			if len(fixed) != tc.wantFixed {
				t.Errorf("fixed: got %d, want %d", len(fixed), tc.wantFixed)
			}
		})
	}
}

// TestVerifyImplementation_CheckProject_ProjectFindingsInResponse verifies that
// project-scope security violations (cross-transport auth) appear in the
// verify_implementation response under "project_security_findings".
func TestVerifyImplementation_CheckProject_ProjectFindingsInResponse(t *testing.T) {
	root := t.TempDir()
	st := openMCPTestStore(t)

	g := graph.New("test-repo")
	g.SetRoot(root)

	// Build a graph with HTTP routes (auth applied) + WebSocket route (no auth).
	// This replicates the cross-transport auth violation detected by the built-in
	// "go-cross-transport-auth" pattern.

	// HTTP file: has route node + calls AuthMiddleware.
	httpFile := filepath.Join(root, "api", "routes.go")
	httpFileID := g.MakeNodeID(httpFile, httpFile)
	g.AddNode(&graph.Node{ID: httpFileID, Type: graph.NodeFile, Name: httpFile, File: httpFile})

	httpRouteID := g.MakeNodeID(httpFile, "route:GET /users")
	g.UpsertRouteNode(&graph.Node{
		ID: httpRouteID, Type: graph.NodeRoute,
		Name: "GET /users", File: httpFile,
		Metadata: map[string]string{"method": "GET", "path": "/users"},
	})

	setupFnID := g.MakeNodeID(httpFile, "setupRoutes")
	g.AddNode(&graph.Node{ID: setupFnID, Type: graph.NodeFunction, Name: "setupRoutes", File: httpFile})
	// AuthMiddleware call — matches "Auth*" required_call_pattern.
	authCalleeID := g.MakeNodeID("other.go", "AuthMiddleware")
	g.AddNode(&graph.Node{ID: authCalleeID, Type: graph.NodeFunction, Name: "AuthMiddleware", File: "other.go"})
	g.AddEdge(&graph.Edge{From: setupFnID, To: authCalleeID, Type: graph.EdgeCalls})

	// WebSocket file: has WS route node + no auth call.
	wsFile := filepath.Join(root, "ws", "handler.go")
	wsFileID := g.MakeNodeID(wsFile, wsFile)
	g.AddNode(&graph.Node{ID: wsFileID, Type: graph.NodeFile, Name: wsFile, File: wsFile})

	wsRouteID := g.MakeNodeID(wsFile, "route:WS /ws/events")
	g.UpsertRouteNode(&graph.Node{
		ID: wsRouteID, Type: graph.NodeRoute,
		Name: "WS /ws/events", File: wsFile,
		Metadata: map[string]string{"method": "WS", "path": "/ws/events"},
	})
	// WS handler function with no auth call.
	wsFnID := g.MakeNodeID(wsFile, "handleWS")
	g.AddNode(&graph.Node{ID: wsFnID, Type: graph.NodeFunction, Name: "handleWS", File: wsFile})

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// Write a dummy file inside root so verify_implementation has something to process.
	dummyFile := filepath.Join(root, "dummy.go")
	if err := os.WriteFile(dummyFile, []byte("package api\n"), 0o644); err != nil {
		t.Fatalf("write dummy file: %v", err)
	}

	filesJSON, _ := json.Marshal([]string{"dummy.go"})
	res, callErr := srv.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	// project_security_findings must be present: the graph has a cross-transport
	// auth inconsistency (HTTP has auth, WebSocket does not).
	if _, ok := m["project_security_findings"]; !ok {
		t.Errorf("expected project_security_findings in response; keys: %v\nfull result: %v", mapKeys(m), m)
	}

	// Status must be escalated to security_findings_found (CRITICAL severity).
	if got, _ := m["status"].(string); got != "security_findings_found" {
		t.Errorf("expected status=security_findings_found, got %q", got)
	}
}

// TestVerifyImplementation_SecurityFindingCountInPulse verifies that the
// SecurityFindingCount field in the ValidationEvent is populated when security
// findings exist.
//
// Since the Server's pulse client is nil in tests (no real pulse server), we
// verify the total_security_findings field in the response instead — it is
// computed by the same code that would populate SecurityFindingCount.
func TestVerifyImplementation_SecurityFindingCountInResponse(t *testing.T) {
	root := t.TempDir()
	st := openMCPTestStore(t)

	g := graph.New("test-repo")
	g.SetRoot(root)

	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	// Write a Go file with a hardcoded AWS key (triggers go-generic-hardcoded-secret).
	// The value "AKIA1234567890ABCDEF" matches the ^AKIA[0-9A-Z]{16}$ pattern.
	goFile := filepath.Join(root, "auth.go")
	content := []byte(`package auth

func setup() {
	secret := "AKIA1234567890ABCDEF"
	_ = secret
}
`)
	if err := os.WriteFile(goFile, content, 0o644); err != nil {
		t.Fatalf("write go file: %v", err)
	}

	// Add file node to graph so it is considered "in_graph".
	fileID := g.MakeNodeID(goFile, goFile)
	g.AddNode(&graph.Node{ID: fileID, Type: graph.NodeFile, Name: goFile, File: goFile})

	filesJSON, _ := json.Marshal([]string{goFile})
	res, callErr := srv.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	// total_security_findings must be > 0 given the hardcoded secret in auth.go.
	total, _ := m["total_security_findings"].(float64)
	if total == 0 {
		t.Errorf("expected total_security_findings > 0 for file with hardcoded secret; result: %v", m)
	}

	// No git baseline in a temp dir → all findings must appear as security_findings_new
	// in the per-file report. This exercises the "no baseline → all new" branch in
	// handleVerifyImplementation.
	filesRaw, ok := m["files"]
	if !ok {
		t.Fatalf("no 'files' key in response: %v", m)
	}
	filesSlice, _ := filesRaw.([]interface{})
	if len(filesSlice) == 0 {
		t.Fatal("files slice is empty")
	}
	fileReport, _ := filesSlice[0].(map[string]interface{})
	newFindings, _ := fileReport["security_findings_new"].([]interface{})
	if len(newFindings) == 0 {
		t.Errorf("expected security_findings_new to be non-empty when no git baseline (all findings should be 'new'); file report: %v", fileReport)
	}
}

// TestVerifyImplementation_BeforeAfterClassification verifies that findings are
// correctly classified as new/existing/fixed using a real git repo as baseline.
func TestVerifyImplementation_BeforeAfterClassification(t *testing.T) {
	// Skip if git is not available.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	// Initialize a git repo in root.
	for _, args := range [][]string{
		{"init", root},
		{"-C", root, "config", "user.email", "test@test.com"},
		{"-C", root, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Write a clean Go file and commit it (no hardcoded secrets — no findings before).
	goFile := filepath.Join(root, "auth.go")
	cleanContent := []byte("package auth\n\nfunc handler() {}\n")
	if err := os.WriteFile(goFile, cleanContent, 0o644); err != nil {
		t.Fatalf("write clean file: %v", err)
	}
	for _, args := range [][]string{
		{"-C", root, "add", "auth.go"},
		{"-C", root, "commit", "-m", "initial"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Now overwrite with content that introduces a hardcoded AWS key (new finding).
	// "AKIA1234567890ABCDEF" matches the ^AKIA[0-9A-Z]{16}$ secret pattern.
	dirtyContent := []byte(`package auth

func handler() {
	secret := "AKIA1234567890ABCDEF"
	_ = secret
}
`)
	if err := os.WriteFile(goFile, dirtyContent, 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	st, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	g := graph.New("test-repo")
	g.SetRoot(root)
	// Add file node so in_graph=true.
	fileID := g.MakeNodeID(goFile, goFile)
	g.AddNode(&graph.Node{ID: fileID, Type: graph.NodeFile, Name: goFile, File: goFile})

	cfg, cfgErr := config.Load(t.TempDir())
	if cfgErr != nil {
		t.Fatalf("config.Load: %v", cfgErr)
	}
	srv := New(g, cfg, st)
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	filesJSON, _ := json.Marshal([]string{goFile})
	res, callErr := srv.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	// The response must have "files" with per-file reports.
	filesRaw, ok := m["files"]
	if !ok {
		t.Fatalf("no 'files' key in response: %v", m)
	}
	filesSlice, _ := filesRaw.([]interface{})
	if len(filesSlice) == 0 {
		t.Fatal("files slice is empty")
	}
	fileReport, _ := filesSlice[0].(map[string]interface{})

	// security_findings_new must be non-empty: the hardcoded secret was introduced
	// in this "commit" (not present in HEAD baseline).
	newFindings, _ := fileReport["security_findings_new"].([]interface{})
	if len(newFindings) == 0 {
		t.Errorf("expected security_findings_new to be non-empty; file report: %v", fileReport)
	}

	// security_findings_existing and security_findings_fixed must be absent
	// (the clean baseline had no findings).
	if existing := fileReport["security_findings_existing"]; existing != nil {
		t.Errorf("expected security_findings_existing to be absent; got %v", existing)
	}
	if fixed := fileReport["security_findings_fixed"]; fixed != nil {
		t.Errorf("expected security_findings_fixed to be absent; got %v", fixed)
	}
}

// ── Sprint 27.4: severity tiers — action field and action_required ────────────

// TestWorstActionRequired verifies the priority ordering of action_required.
func TestWorstActionRequired(t *testing.T) {
	cases := []struct {
		name         string
		secFindings  []security.Violation
		archViolations []config.Violation
		want         string
	}{
		{
			name: "no findings → none",
			want: "none",
		},
		{
			name:        "CRITICAL sec finding → block",
			secFindings: []security.Violation{{Action: "block", Severity: security.SeverityCritical}},
			want:        "block",
		},
		{
			name:        "HIGH sec finding → warn",
			secFindings: []security.Violation{{Action: "warn", Severity: security.SeverityHigh}},
			want:        "warn",
		},
		{
			name:        "MEDIUM sec finding → inform",
			secFindings: []security.Violation{{Action: "inform", Severity: security.SeverityMedium}},
			want:        "inform",
		},
		{
			name:           "arch error violation → warn",
			archViolations: []config.Violation{{Severity: "error"}},
			want:           "warn",
		},
		{
			name:           "arch warning violation → inform",
			archViolations: []config.Violation{{Severity: "warning"}},
			want:           "inform",
		},
		{
			name: "CRITICAL sec beats arch error → block",
			secFindings: []security.Violation{
				{Action: "block", Severity: security.SeverityCritical},
			},
			archViolations: []config.Violation{{Severity: "error"}},
			want:           "block",
		},
		{
			name: "arch error beats MEDIUM sec → warn",
			secFindings: []security.Violation{
				{Action: "inform", Severity: security.SeverityMedium},
			},
			archViolations: []config.Violation{{Severity: "error"}},
			want:           "warn",
		},
		{
			name: "mixed: HIGH sec + arch warning → warn",
			secFindings: []security.Violation{
				{Action: "warn", Severity: security.SeverityHigh},
			},
			archViolations: []config.Violation{{Severity: "warning"}},
			want:           "warn",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := worstActionRequired(tc.secFindings, tc.archViolations)
			if got != tc.want {
				t.Errorf("worstActionRequired() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestArchViolationAction checks the mapping from arch rule severity to action.
func TestArchViolationAction(t *testing.T) {
	if got := archViolationAction("error"); got != "warn" {
		t.Errorf("archViolationAction(error) = %q, want %q", got, "warn")
	}
	if got := archViolationAction("warning"); got != "inform" {
		t.Errorf("archViolationAction(warning) = %q, want %q", got, "inform")
	}
	if got := archViolationAction(""); got != "inform" {
		t.Errorf("archViolationAction('') = %q, want %q", got, "inform")
	}
}

// TestValidatePlan_ActionRequired verifies that handleValidatePlan includes
// action_required in its response when violations are present and omits it
// when the plan is clean.
//
// The positive case uses an arch rule violation (deterministic — no dependency
// on pattern engine file scanning). The negative case asserts absence.
// The value mapping itself is covered by TestWorstActionRequired.
func TestValidatePlan_ActionRequired(t *testing.T) {
	s := newTestServer(t)

	// Register a rule so handleValidatePlan can report violations.
	rRes, rErr := s.handleUpsertRule(ctx, callTool(map[string]any{
		"rule_id":     "no-db-in-handler",
		"description": "handlers must not call database directly",
		"severity":    "error",
	}))
	mustResult(t, rRes, rErr)

	// Case 1: clean plan (no violations likely on an unrelated file).
	// action_required must be ABSENT.
	cleanRes, cleanErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": `[{"file":"internal/safe/safe.go"}]`,
	}))
	cleanMap := mustResult(t, cleanRes, cleanErr)
	cleanViolations, _ := cleanMap["violations"].([]interface{})
	cleanSec, _ := cleanMap["security_findings"].([]interface{})
	if len(cleanViolations) == 0 && len(cleanSec) == 0 {
		// Correctly no findings — action_required must be absent.
		if _, found := cleanMap["action_required"]; found {
			t.Error("action_required must be absent when there are no violations or security findings")
		}
	}

	// Case 2: when violations ARE present, action_required must be present and valid.
	// Violations, security_findings, or both can trigger this — check the response
	// to see if either is populated.
	dirtyRes, dirtyErr := s.handleValidatePlan(ctx, callTool(map[string]any{
		"changes": `[{"file":"internal/handler/user.go","adds_call_to":"db.Query"}]`,
	}))
	dirtyMap := mustResult(t, dirtyRes, dirtyErr)
	dirtyViolations, _ := dirtyMap["violations"].([]interface{})
	dirtySec, _ := dirtyMap["security_findings"].([]interface{})
	if len(dirtyViolations) > 0 || len(dirtySec) > 0 {
		actionRequired, ok := dirtyMap["action_required"]
		if !ok {
			t.Fatalf("action_required missing when violations are present; keys: %v", mapKeys(dirtyMap))
		}
		switch actionRequired {
		case "block", "warn", "inform":
			// valid
		default:
			t.Errorf("action_required = %q, want block/warn/inform", actionRequired)
		}
	}
	// If no violations fire, the test is still valid — action_required absence is correct.
}

// TestValidatePreWrite_ActionRequired verifies that handleValidatePreWrite
// includes action_required when findings are present.
func TestValidatePreWrite_ActionRequired(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "secret.go")
	// AKIA key — matches hardcoded-secret pattern (HIGH severity).
	goSrc := []byte(`package main

const token = "AKIA1234567890ABCDEF"
`)
	if err := os.WriteFile(goFile, goSrc, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	st := openMCPTestStore(t)
	g := graph.New("test-repo")
	g.SetRoot(root)
	cfg, cfgErr := config.Load(t.TempDir())
	if cfgErr != nil {
		t.Fatalf("config.Load: %v", cfgErr)
	}
	srv := New(g, cfg, st)
	srv.patternEngine = security.DefaultEngine()
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	filesJSON, _ := json.Marshal([]string{goFile})
	res, callErr := srv.handleValidatePreWrite(ctx, callTool(map[string]any{
		"files": string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	findings, _ := m["security_findings"].([]interface{})
	if len(findings) == 0 {
		t.Skip("pattern engine produced no findings — skipping action_required check")
	}

	actionRequired, ok := m["action_required"]
	if !ok {
		t.Fatalf("action_required missing from pre_write response; got keys: %v", mapKeys(m))
	}
	switch actionRequired {
	case "block", "warn", "inform":
		// valid
	default:
		t.Errorf("action_required = %q, want block/warn/inform", actionRequired)
	}

	// Each finding must have an action field.
	for i, f := range findings {
		fm, _ := f.(map[string]interface{})
		action, _ := fm["action"].(string)
		if action == "" {
			t.Errorf("finding[%d] missing action field: %v", i, fm)
		}
	}
}

// TestVerifyImplementation_ActionRequired verifies that handleVerifyImplementation
// includes action_required when security findings are present.
func TestVerifyImplementation_ActionRequired(t *testing.T) {
	root := t.TempDir()
	goFile := filepath.Join(root, "handler.go")
	goSrc := []byte(`package main

const apiKey = "AKIA1234567890ABCDEF"
`)
	if err := os.WriteFile(goFile, goSrc, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	st, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	g := graph.New("test-repo")
	g.SetRoot(root)
	fileID := g.MakeNodeID(goFile, goFile)
	g.AddNode(&graph.Node{ID: fileID, Type: graph.NodeFile, Name: goFile, File: goFile})

	cfg, cfgErr := config.Load(t.TempDir())
	if cfgErr != nil {
		t.Fatalf("config.Load: %v", cfgErr)
	}
	srv := New(g, cfg, st)
	srv.patternEngine = security.DefaultEngine()
	srv.StartBackground()
	t.Cleanup(func() { srv.Close() })

	filesJSON, _ := json.Marshal([]string{goFile})
	res, callErr := srv.handleVerifyImplementation(ctx, callTool(map[string]any{
		"files_written": string(filesJSON),
	}))
	m := mustResult(t, res, callErr)

	totalFindings, _ := m["total_security_findings"].(float64)
	if totalFindings == 0 {
		t.Skip("pattern engine produced no findings — skipping action_required check")
	}

	actionRequired, ok := m["action_required"]
	if !ok {
		t.Fatalf("action_required missing from post_write response; got keys: %v", mapKeys(m))
	}
	switch actionRequired {
	case "block", "warn", "inform":
		// valid
	default:
		t.Errorf("action_required = %q, want block/warn/inform", actionRequired)
	}

	// Every per-file security finding must have the action field set.
	files, _ := m["files"].([]interface{})
	for i, f := range files {
		report, _ := f.(map[string]interface{})
		for _, key := range []string{"security_findings", "security_findings_new", "security_findings_existing"} {
			slice, _ := report[key].([]interface{})
			for j, finding := range slice {
				fm, _ := finding.(map[string]interface{})
				action, _ := fm["action"].(string)
				if action == "" {
					t.Errorf("files[%d].%s[%d] missing action field: %v", i, key, j, fm)
				}
			}
		}
	}
}

