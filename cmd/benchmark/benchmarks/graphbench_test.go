package benchmarks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGraphBenchData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	data := `{"repo":"test/repo","commit":"abc123","language":"python","tests":[{"query_type":"find_imports","query":"main.py","expected_names":["os","sys"]}]}
{"repo":"test/repo2","commit":"def456","language":"go","tests":[{"query_type":"find_callers","query":"Foo.Bar","expected_names":["main"],"expected_files":["cmd/main.go"]}]}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	suites, err := loadGraphBenchData(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(suites) != 2 {
		t.Fatalf("expected 2 suites, got %d", len(suites))
	}
	if suites[0].Repo != "test/repo" {
		t.Errorf("expected repo test/repo, got %s", suites[0].Repo)
	}
	if len(suites[0].Tests) != 1 {
		t.Fatalf("expected 1 test, got %d", len(suites[0].Tests))
	}
	if suites[0].Tests[0].QueryType != "find_imports" {
		t.Errorf("expected find_imports, got %s", suites[0].Tests[0].QueryType)
	}
}

func TestLoadGraphBenchData_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte("\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadGraphBenchData(path)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestLoadGraphBenchData_BadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	data := `not json
{"repo":"ok/repo","commit":"abc","language":"go","tests":[{"query_type":"find_callers","query":"Foo","expected_names":["Bar"]}]}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	suites, err := loadGraphBenchData(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(suites) != 1 {
		t.Errorf("expected 1 valid suite, got %d", len(suites))
	}
}

func TestSetOverlap(t *testing.T) {
	hits, total := setOverlap(
		[]string{"foo", "bar", "baz"},
		[]string{"FOO", "bar", "qux"},
	)
	if total != 3 {
		t.Errorf("expected total=3, got %d", total)
	}
	if hits != 2 {
		t.Errorf("expected hits=2, got %d", hits)
	}
}

func TestSetOverlap_PartialMatch(t *testing.T) {
	// "Flask.__init__" should match actual "__init__" via suffix matching.
	hits, _ := setOverlap(
		[]string{"Flask.__init__"},
		[]string{"__init__"},
	)
	if hits != 1 {
		t.Errorf("expected partial match hit=1, got %d", hits)
	}
}

func TestSetOverlapFiles(t *testing.T) {
	hits, total := setOverlapFiles(
		[]string{"src/flask/app.py", "src/flask/cli.py"},
		[]string{"flask/app.py", "flask/testing.py"},
	)
	if total != 2 {
		t.Errorf("expected total=2, got %d", total)
	}
	if hits != 1 {
		t.Errorf("expected hits=1 (app.py suffix match), got %d", hits)
	}
}

func TestNormalizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Flask.run", "flask.run"},
		{"`Session.request`", "session.request"},
		{"**Blueprint**", "blueprint"},
		{"[NodeName]", "nodename"},
	}
	for _, tt := range tests {
		got := normalizeName(tt.in)
		if got != tt.want {
			t.Errorf("normalizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeFile(t *testing.T) {
	tests := []struct{ in, want string }{
		{"./src/flask/app.py", "src/flask/app.py"},
		{"/absolute/path.go", "absolute/path.go"},
		{"  path/to/file.js  ", "path/to/file.js"},
	}
	for _, tt := range tests {
		got := normalizeFile(tt.in)
		if got != tt.want {
			t.Errorf("normalizeFile(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExtractNamesFromText(t *testing.T) {
	text := `[Flask] class · app.py:42
Calls: run_simple · get_debug_flag · show_server_banner
Called by: (none)
DIRECT: Session, HTTPAdapter, Response`

	names := extractNamesFromText(text)
	if len(names) == 0 {
		t.Fatal("expected names, got none")
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[normalizeName(n)] = true
	}
	for _, expected := range []string{"flask", "run_simple", "get_debug_flag", "session", "httpadapter"} {
		if !nameSet[expected] {
			t.Errorf("missing expected name %q in %v", expected, names)
		}
	}
}

func TestExtractFilesFromText(t *testing.T) {
	text := `[Flask] class · src/flask/app.py:42
[Blueprint] class · src/flask/blueprints.py:10
some random text without files`

	files := extractFilesFromText(text)
	if len(files) < 2 {
		t.Errorf("expected at least 2 files, got %d: %v", len(files), files)
	}
}

func TestLooksLikeFile(t *testing.T) {
	if !looksLikeFile("app.py") {
		t.Error("app.py should look like a file")
	}
	if !looksLikeFile("router/index.js") {
		t.Error("router/index.js should look like a file")
	}
	if looksLikeFile("README.md") {
		t.Error("README.md should not look like a code file")
	}
	if looksLikeFile("noext") {
		t.Error("noext should not look like a file")
	}
}

func TestMetricAccum(t *testing.T) {
	acc := &metricAccum{}
	acc.add(1.0, 0.5, 0.667)
	acc.add(0.8, 0.6, 0.686)

	if acc.n != 2 {
		t.Errorf("n=%d, want 2", acc.n)
	}
	if got := acc.avgP(); got < 0.89 || got > 0.91 {
		t.Errorf("avgP=%f, want ~0.9", got)
	}
}

func TestMetricAccum_Empty(t *testing.T) {
	acc := &metricAccum{}
	if acc.avgP() != 0 {
		t.Error("empty avgP should be 0")
	}
	if acc.avgR() != 0 {
		t.Error("empty avgR should be 0")
	}
	if acc.avgF1() != 0 {
		t.Error("empty avgF1 should be 0")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 100); got != "short" {
		t.Errorf("expected 'short', got %q", got)
	}
	if got := truncate("this is a long string", 10); got != "this is a ..." {
		t.Errorf("expected truncated string, got %q", got)
	}
}

func TestAppendUniqueName(t *testing.T) {
	names := []string{"Flask"}
	names = appendUniqueName(names, "flask") // duplicate
	names = appendUniqueName(names, "Blueprint")
	if len(names) != 2 {
		t.Errorf("expected 2 names, got %d: %v", len(names), names)
	}
}

func TestAppendUniqueFile(t *testing.T) {
	files := []string{"src/app.py"}
	files = appendUniqueFile(files, "src/app.py") // duplicate
	files = appendUniqueFile(files, "src/cli.py")
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d: %v", len(files), files)
	}
}

func TestParseImpactJSON(t *testing.T) {
	raw := `{
		"root": {"name": "Flask", "type": "class", "file": "app.py", "line": 42},
		"tiers": [
			{
				"depth": 1, "label": "direct", "confidence": 1.0,
				"nodes": [
					{"name": "FlaskClient", "type": "class", "file": "testing.py", "line": 10},
					{"name": "Blueprint", "type": "class", "file": "blueprints.py", "line": 5}
				]
			}
		],
		"affected_files": ["testing.py", "blueprints.py", "cli.py"],
		"total_affected": 3
	}`

	var ir impactResponse
	if err := json.Unmarshal([]byte(raw), &ir); err != nil {
		t.Fatal(err)
	}
	if ir.Root.Name != "Flask" {
		t.Errorf("root name=%q, want Flask", ir.Root.Name)
	}
	if len(ir.Tiers) != 1 {
		t.Fatalf("expected 1 tier, got %d", len(ir.Tiers))
	}
	if len(ir.Tiers[0].Nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(ir.Tiers[0].Nodes))
	}
	if len(ir.AffectedFiles) != 3 {
		t.Errorf("expected 3 affected files, got %d", len(ir.AffectedFiles))
	}
}

func TestParseContextJSON(t *testing.T) {
	raw := `{
		"root": {"name": "Flask.run", "type": "method", "file": "app.py", "line": 100},
		"callees": [
			{"node": {"name": "run_simple", "type": "function", "file": "werkzeug/serving.py"}, "relevance": 0.9},
			{"node": {"name": "get_debug_flag", "type": "function", "file": "helpers.py"}, "relevance": 0.8}
		],
		"callers": [
			{"node": {"name": "Flask.__call__", "type": "method", "file": "app.py"}}
		],
		"related": []
	}`

	var cr contextResponse
	if err := json.Unmarshal([]byte(raw), &cr); err != nil {
		t.Fatal(err)
	}
	if len(cr.Callees) != 2 {
		t.Errorf("expected 2 callees, got %d", len(cr.Callees))
	}
	if cr.Callees[0].Node.Name != "run_simple" {
		t.Errorf("callee[0] name=%q, want run_simple", cr.Callees[0].Node.Name)
	}
	if len(cr.Callers) != 1 {
		t.Errorf("expected 1 caller, got %d", len(cr.Callers))
	}
}

func TestMatchesFileSet(t *testing.T) {
	set := makeNormFileSet([]string{"src/flask/app.py", "src/flask/cli.py"})

	if !matchesFileSet("src/flask/app.py", set) {
		t.Error("exact match should work")
	}
	if !matchesFileSet("flask/app.py", set) {
		t.Error("suffix match should work")
	}
	if matchesFileSet("src/django/app.py", set) {
		t.Error("non-matching file should not match")
	}
}

func TestExtractPackageName(t *testing.T) {
	tests := []struct {
		callee, queryFile, want string
	}{
		{"werkzeug.serving.run_simple", "src/flask/app.py", "werkzeug"},
		{"click.command", "src/flask/cli.py", "click"},
		{"Flask.run", "src/flask/app.py", ""}, // same package
		{"json.dumps", "src/flask/app.py", "json"},
		{"simple_func", "src/flask/app.py", ""}, // no dots
	}
	for _, tt := range tests {
		got := extractPackageName(tt.callee, tt.queryFile)
		if got != tt.want {
			t.Errorf("extractPackageName(%q, %q) = %q, want %q", tt.callee, tt.queryFile, got, tt.want)
		}
	}
}
