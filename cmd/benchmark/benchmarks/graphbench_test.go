package benchmarks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGraphBenchData(t *testing.T) {
	// Write a small test JSONL file.
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
	if len(suites[0].Tests[0].ExpectedNames) != 2 {
		t.Errorf("expected 2 expected_names, got %d", len(suites[0].Tests[0].ExpectedNames))
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

func TestNormalizeName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Flask.run", "flask.run"},
		{"`Session.request`", "session.request"},
		{"**Blueprint**", "blueprint"},
	}
	for _, tt := range tests {
		got := normalizeName(tt.in)
		if got != tt.want {
			t.Errorf("normalizeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestExtractFilesFromResponse(t *testing.T) {
	text := `
## Impact Analysis
- src/flask/app.py:42 (Flask class)
- src/flask/testing.py:10 (FlaskClient)
- src/flask/cli.py:55 (run_command)
`
	files := extractFilesFromResponse(text)
	if len(files) < 3 {
		t.Errorf("expected at least 3 files, got %d: %v", len(files), files)
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
