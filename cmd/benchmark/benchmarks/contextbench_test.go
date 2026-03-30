package benchmarks

import (
	"os"
	"testing"
)

// ─── gold context parsing ────────────────────────────────────────────────────

func TestParseGoldContext(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int // number of valid blocks
		wantErr bool
	}{
		{"empty string", "", 0, false},
		{"empty array", "[]", 0, false},
		{"null", "null", 0, false},
		{"single block", `[{"file": "foo.py", "start_line": 10, "end_line": 20, "content": "code"}]`, 1, false},
		{"multiple blocks", `[{"file": "a.py", "start_line": 1, "end_line": 5, "content": "a"}, {"file": "b.py", "start_line": 10, "end_line": 15, "content": "b"}]`, 2, false},
		{"invalid json", `not json`, 0, true},
		{"missing file", `[{"file": "", "start_line": 1, "end_line": 5}]`, 0, false},     // filtered out
		{"zero start", `[{"file": "a.py", "start_line": 0, "end_line": 5}]`, 0, false},   // filtered out
		{"end < start", `[{"file": "a.py", "start_line": 10, "end_line": 5}]`, 0, false}, // filtered out
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks, err := parseGoldContext(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseGoldContext() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(blocks) != tt.want {
				t.Errorf("parseGoldContext() = %d blocks, want %d", len(blocks), tt.want)
			}
		})
	}
}

// ─── search response parsing ─────────────────────────────────────────────────

func TestCollectSearchMentions(t *testing.T) {
	// Valid search response — collect raw (file, line) mentions (no windowing).
	resp := `{"query":"test","count":2,"results":[{"file":"src/auth.py","line":42,"name":"authenticate","type":"function"},{"file":"src/utils.py","line":10,"name":"helper","type":"function"}]}`
	type mention struct {
		file string
		line int
	}
	var got []mention
	collectSearchMentions(resp, func(file string, line int) {
		got = append(got, mention{file, line})
	})

	// Should have the exact lines reported.
	foundAuth, foundUtils := false, false
	for _, m := range got {
		if m.file == "src/auth.py" && m.line == 42 {
			foundAuth = true
		}
		if m.file == "src/utils.py" && m.line == 10 {
			foundUtils = true
		}
	}
	if !foundAuth {
		t.Error("expected src/auth.py:42 in output")
	}
	if !foundUtils {
		t.Error("expected src/utils.py:10 in output")
	}

	// Empty/invalid response — no mentions.
	var got2 []mention
	collectSearchMentions("not json", func(f string, l int) { got2 = append(got2, mention{f, l}) })
	if len(got2) != 0 {
		t.Errorf("invalid JSON should produce no mentions, got %d", len(got2))
	}

	// Response with no results.
	var got3 []mention
	collectSearchMentions(`{"query":"x","count":0,"results":[]}`, func(f string, l int) { got3 = append(got3, mention{f, l}) })
	if len(got3) != 0 {
		t.Errorf("empty results should produce no mentions, got %d", len(got3))
	}
}

// ─── impact response parsing ─────────────────────────────────────────────────

func TestCollectImpactMentions(t *testing.T) {
	resp := `{
		"total_affected": 3,
		"tiers": [
			{
				"label": "direct",
				"depth": 1,
				"nodes": [
					{"file": "src/handler.go", "line": 55, "name": "Handle", "type": "function"},
					{"file": "src/model.go", "line": 120, "name": "Model", "type": "struct"}
				]
			},
			{
				"label": "indirect",
				"depth": 2,
				"nodes": [
					{"file": "src/service.go", "line": 30, "name": "Process", "type": "function"}
				]
			}
		],
		"affected_files": ["src/handler.go", "src/model.go", "src/service.go"]
	}`

	type mention struct {
		file string
		line int
	}
	var got []mention
	collectImpactMentions(resp, func(f string, l int) { got = append(got, mention{f, l}) })

	find := func(file string, line int) bool {
		for _, m := range got {
			if m.file == file && m.line == line {
				return true
			}
		}
		return false
	}
	if !find("src/handler.go", 55) {
		t.Error("expected src/handler.go:55")
	}
	if !find("src/model.go", 120) {
		t.Error("expected src/model.go:120")
	}
	if !find("src/service.go", 30) {
		t.Error("expected src/service.go:30")
	}

	// Nodes with missing file or zero line should be skipped.
	resp2 := `{"tiers":[{"nodes":[{"file":"","line":10},{"file":"a.go","line":0}]}]}`
	var got2 []mention
	collectImpactMentions(resp2, func(f string, l int) { got2 = append(got2, mention{f, l}) })
	if len(got2) != 0 {
		t.Errorf("invalid nodes should produce no mentions, got %d", len(got2))
	}
}

// ─── markdown response parsing ───────────────────────────────────────────────

func TestCollectMarkdownMentions(t *testing.T) {
	md := `## Entity: authenticate
**File:** src/auth.py:42
**Type:** function

### Callees
- src/db.py:100 — query_user
- src/crypto.py:55 — verify_hash

### Callers
- tests/test_auth.py:20 — test_login
`
	type mention struct {
		file string
		line int
	}
	var got []mention
	collectMarkdownMentions(md, func(f string, l int) { got = append(got, mention{f, l}) })

	find := func(file string, line int) bool {
		for _, m := range got {
			if m.file == file && m.line == line {
				return true
			}
		}
		return false
	}
	if !find("src/auth.py", 42) {
		t.Error("expected src/auth.py:42")
	}
	if !find("src/db.py", 100) {
		t.Error("expected src/db.py:100")
	}
	if !find("src/crypto.py", 55) {
		t.Error("expected src/crypto.py:55")
	}
	if !find("tests/test_auth.py", 20) {
		t.Error("expected tests/test_auth.py:20")
	}

	// No file:line patterns.
	var got2 []mention
	collectMarkdownMentions("just some text without file references", func(f string, l int) { got2 = append(got2, mention{f, l}) })
	if len(got2) != 0 {
		t.Errorf("no patterns should produce no mentions, got %d", len(got2))
	}
}

// ─── entity extraction ───────────────────────────────────────────────────────

func TestExtractEntitiesFromProblem(t *testing.T) {
	tests := []struct {
		name    string
		problem string
		want    []string // subset that must be present
		notWant []string // must NOT be present
	}{
		{
			"backtick identifiers",
			"The `separability_matrix` function in `astropy.modeling.separable` is broken",
			[]string{"separability_matrix", "astropy.modeling.separable"},
			nil,
		},
		{
			"dotted paths",
			"Issue with django.contrib.auth when using the authentication backend",
			[]string{"django.contrib.auth"},
			[]string{"when", "using", "the"},
		},
		{
			"snake_case",
			"Bug in get_user_permissions function causing access denial",
			[]string{"get_user_permissions"},
			nil,
		},
		{
			"skip URLs",
			"See https://github.com/example/repo for details about the bug in `MyClass`",
			[]string{"MyClass"},
			[]string{"https://github.com/example/repo"},
		},
		{
			"cap at 8",
			"`a_func` `b_func` `c_func` `d_func` `e_func` `f_func` `g_func` `h_func` `i_func`",
			[]string{"a_func", "b_func", "c_func", "d_func", "e_func", "f_func", "g_func", "h_func"},
			[]string{"i_func"},
		},
		{
			"CamelCase identifiers",
			"URLValidator should reject invalid characters; QuerySet union causes ValueError",
			[]string{"URLValidator", "QuerySet", "ValueError"},
			[]string{"Django", "Python", "should", "causes"},
		},
		{
			"plain English words excluded",
			"Django Python Description Unicode Simple Count",
			nil,
			[]string{"Django", "Python", "Description", "Unicode", "Simple", "Count"},
		},
		{
			"empty problem",
			"",
			nil,
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entities := extractEntitiesFromProblem(tt.problem)
			entitySet := make(map[string]bool)
			for _, e := range entities {
				entitySet[e] = true
			}
			for _, w := range tt.want {
				if !entitySet[w] {
					t.Errorf("expected entity %q not found in %v", w, entities)
				}
			}
			for _, nw := range tt.notWant {
				if entitySet[nw] {
					t.Errorf("unexpected entity %q found in %v", nw, entities)
				}
			}
			if len(entities) > 8 {
				t.Errorf("entities should be capped at 8, got %d", len(entities))
			}
		})
	}
}

// ─── keyword extraction ──────────────────────────────────────────────────────

func TestExtractKeywords(t *testing.T) {
	kw := extractKeywords("The authentication module should handle token refresh correctly when using OAuth2", 3)
	if len(kw) == 0 {
		t.Fatal("expected at least 1 keyword")
	}
	if len(kw) > 3 {
		t.Errorf("expected at most 3 keywords, got %d", len(kw))
	}
	// "authentication" should be a keyword (long, not a stop word).
	found := false
	for _, k := range kw {
		if k == "authentication" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'authentication' in keywords %v", kw)
	}

	// Stop words should be filtered.
	kw2 := extractKeywords("the and for are not this that with from", 5)
	if len(kw2) != 0 {
		t.Errorf("all stop words should produce empty keywords, got %v", kw2)
	}

	// Empty input.
	kw3 := extractKeywords("", 3)
	if len(kw3) != 0 {
		t.Errorf("empty input should produce empty keywords, got %v", kw3)
	}
}

// ─── context F1 calculation ──────────────────────────────────────────────────

func TestContextF1Calculation(t *testing.T) {
	// Test the F1 math directly.
	tests := []struct {
		name      string
		gold      int // number of gold lines
		retrieved int // number of retrieved lines
		hits      int // overlap
		wantP     float64
		wantR     float64
		wantF1    float64
	}{
		{"perfect", 10, 10, 10, 1.0, 1.0, 1.0},
		{"no overlap", 10, 5, 0, 0.0, 0.0, 0.0},
		{"half recall", 10, 5, 5, 1.0, 0.5, 0.6667},
		{"half precision", 10, 20, 10, 0.5, 1.0, 0.6667},
		{"empty gold", 0, 5, 0, 0.0, 0.0, 0.0},
		{"empty retrieved", 10, 0, 0, 0.0, 0.0, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p, r, f1 float64
			if tt.retrieved > 0 {
				p = float64(tt.hits) / float64(tt.retrieved)
			}
			if tt.gold > 0 {
				r = float64(tt.hits) / float64(tt.gold)
			}
			if p+r > 0 {
				f1 = 2 * p * r / (p + r)
			}
			if abs(p-tt.wantP) > 0.01 {
				t.Errorf("precision = %.4f, want %.4f", p, tt.wantP)
			}
			if abs(r-tt.wantR) > 0.01 {
				t.Errorf("recall = %.4f, want %.4f", r, tt.wantR)
			}
			if abs(f1-tt.wantF1) > 0.01 {
				t.Errorf("f1 = %.4f, want %.4f", f1, tt.wantF1)
			}
		})
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// ─── task filtering ──────────────────────────────────────────────────────────

func TestFilterTasks(t *testing.T) {
	tasks := []ContextBenchTask{
		{InstanceID: "1", Language: "python", Source: "Verified"},
		{InstanceID: "2", Language: "Java", Source: "Pro"},
		{InstanceID: "3", Language: "Python", Source: "Pro"},
		{InstanceID: "4", Language: "go", Source: "Verified"},
	}

	// No filter → all tasks.
	result := filterTasks(tasks, nil, nil)
	if len(result) != 4 {
		t.Errorf("no filter: got %d, want 4", len(result))
	}

	// Language filter (case-insensitive).
	result = filterTasks(tasks, []string{"python"}, nil)
	if len(result) != 2 {
		t.Errorf("python filter: got %d, want 2", len(result))
	}

	// Source filter (case-insensitive).
	result = filterTasks(tasks, nil, []string{"verified"})
	if len(result) != 2 {
		t.Errorf("verified filter: got %d, want 2", len(result))
	}

	// Combined filter.
	result = filterTasks(tasks, []string{"python"}, []string{"verified"})
	if len(result) != 1 {
		t.Errorf("combined filter: got %d, want 1", len(result))
	}
	if result[0].InstanceID != "1" {
		t.Errorf("combined filter: got %s, want 1", result[0].InstanceID)
	}
}

// ─── JSONL loading ───────────────────────────────────────────────────────────

func TestLoadContextBenchTasks(t *testing.T) {
	// Write a temp JSONL file.
	tmpFile := t.TempDir() + "/test.jsonl"
	content := `{"instance_id":"test-1","repo":"org/repo","language":"python","base_commit":"abc123","gold_context":"[]","problem_statement":"Bug in foo"}
{"instance_id":"test-2","repo":"org/repo2","language":"go","base_commit":"def456","gold_context":"[{\"file\":\"main.go\",\"start_line\":1,\"end_line\":10,\"content\":\"code\"}]","problem_statement":"Issue with bar"}
`
	if err := writeTestFile(tmpFile, content); err != nil {
		t.Fatal(err)
	}

	tasks, err := loadContextBenchTasks(tmpFile)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].InstanceID != "test-1" {
		t.Errorf("task 0 id = %q, want test-1", tasks[0].InstanceID)
	}
	if tasks[1].Language != "go" {
		t.Errorf("task 1 language = %q, want go", tasks[1].Language)
	}

	// Invalid JSONL.
	tmpFile2 := t.TempDir() + "/bad.jsonl"
	if err := writeTestFile(tmpFile2, "not json\n"); err != nil {
		t.Fatal(err)
	}
	_, err = loadContextBenchTasks(tmpFile2)
	if err == nil {
		t.Error("expected error for invalid JSONL")
	}

	// Missing file.
	_, err = loadContextBenchTasks("/nonexistent/path.jsonl")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// ─── isIdentifier ────────────────────────────────────────────────────────────

func TestIsIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"foo_bar", true},
		{"FooBar", true},
		{"foo123", true},
		{"", false},
		{"foo-bar", false},
		{"foo.bar", false},
		{"foo bar", false},
	}
	for _, tt := range tests {
		if got := isIdentifier(tt.input); got != tt.want {
			t.Errorf("isIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// ─── edge case: file line pattern regex ──────────────────────────────────────

func TestFileLinePatternEdgeCases(t *testing.T) {
	type mention struct {
		file string
		line int
	}
	collect := func(text string) []mention {
		var out []mention
		collectMarkdownMentions(text, func(f string, l int) { out = append(out, mention{f, l}) })
		return out
	}

	// Should match a simple file:line reference.
	got := collect("src/auth/handler.go:142")
	found := false
	for _, m := range got {
		if m.file == "src/auth/handler.go" && m.line == 142 {
			found = true
		}
	}
	if !found {
		t.Error("expected src/auth/handler.go:142")
	}

	// Should handle deeply nested paths.
	got2 := collect("internal/pkg/handler/auth.go:99")
	found2 := false
	for _, m := range got2 {
		if m.file == "internal/pkg/handler/auth.go" && m.line == 99 {
			found2 = true
		}
	}
	if !found2 {
		t.Error("expected internal/pkg/handler/auth.go:99")
	}
}

// ─── buildRetrievedLines ────────────────────────────────────────────────────

func TestBuildRetrievedLines(t *testing.T) {
	mentions := map[string]map[int]bool{
		"src/auth.py": {10: true, 20: true, 30: true},
		"src/db.py":   {5: true, 100: true},
	}
	scored := []fileScore{
		{file: "src/auth.py", score: 10, source: true, depth: 1},
		{file: "src/db.py", score: 5, source: true, depth: 1},
	}

	// Budget 50 should produce <= 50 lines.
	r50 := buildRetrievedLines(scored, mentions, 50)
	if len(r50) > 50 {
		t.Errorf("budget 50: got %d lines, want <= 50", len(r50))
	}
	if len(r50) == 0 {
		t.Error("budget 50: got 0 lines, want > 0")
	}

	// Budget 500 should produce more lines than budget 50.
	r500 := buildRetrievedLines(scored, mentions, 500)
	if len(r500) <= len(r50) {
		t.Errorf("budget 500 (%d) should produce more lines than budget 50 (%d)", len(r500), len(r50))
	}

	// All retrieved lines should be from the scored files.
	for key := range r50 {
		parts := splitFileLine(key)
		if parts[0] != "src/auth.py" && parts[0] != "src/db.py" {
			t.Errorf("unexpected file in retrieved: %s", parts[0])
		}
	}
}

func splitFileLine(key string) [2]string {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == ':' {
			return [2]string{key[:i], key[i+1:]}
		}
	}
	return [2]string{key, ""}
}

// ─── entity extraction from impact responses ────────────────────────────────

func TestExtractEntitiesFromImpactResponse(t *testing.T) {
	// Impact responses contain entity names in tier nodes.
	// extractEntitiesFromResponse should find bracketed names.
	resp := `## Impact Analysis
Tier 1 (direct):
- [Handle] in src/handler.go:55
- [Process] in src/service.go:30

Calls: Validate, Transform`

	entities := extractEntitiesFromResponse(resp)
	entitySet := make(map[string]bool)
	for _, e := range entities {
		entitySet[e] = true
	}

	if !entitySet["Handle"] {
		t.Errorf("expected Handle in entities %v", entities)
	}
	if !entitySet["Process"] {
		t.Errorf("expected Process in entities %v", entities)
	}
	if !entitySet["Validate"] {
		t.Errorf("expected Validate in entities %v", entities)
	}
	if !entitySet["Transform"] {
		t.Errorf("expected Transform in entities %v", entities)
	}
}
