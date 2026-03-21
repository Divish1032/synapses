package store

import (
	"os"
	"path/filepath"
	"testing"
)

// templateGraphDB and templateKnowledgeDB hold paths to pre-initialized
// SQLite databases created once in TestMain. Test helpers copy these
// files instead of re-running 50+ DDL migrations per test.
var templateGraphDB string
var templateKnowledgeDB string

func TestMain(m *testing.M) {
	// Create template store once — runs all DDL, migrations, PRAGMA setup.
	dir, err := os.MkdirTemp("", "synapses-test-template-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	templatePath := filepath.Join(dir, "template.db")
	st, err := Open(templatePath)
	if err != nil {
		panic("TestMain: Store.Open template: " + err.Error())
	}
	st.Close()

	templateGraphDB = templatePath
	templateKnowledgeDB = KnowledgePath(templatePath)

	os.Exit(m.Run())
}

// openFromTemplate creates a new test store by copying the pre-initialized
// template databases. This avoids re-running 50+ DDL migrations per test,
// reducing Store.Open() from ~1-3s to ~10-50ms (file copy + re-open).
func openFromTemplate(t testing.TB) *Store {
	t.Helper()

	dir := t.TempDir() // auto-cleaned by testing framework
	dbPath := filepath.Join(dir, "test.db")
	knowledgePath := KnowledgePath(dbPath)

	// Copy template files
	copyFile(t, templateGraphDB, dbPath)
	copyFile(t, templateKnowledgeDB, knowledgePath)

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("openFromTemplate: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// OpenFromTemplate is the exported version of openFromTemplate, accessible
// from package store_test (external test files).
func OpenFromTemplate(t testing.TB) *Store {
	t.Helper()
	return openFromTemplate(t)
}

func copyFile(t testing.TB, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("copyFile %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("copyFile → %s: %v", dst, err)
	}
}
