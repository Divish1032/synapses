package store

import (
	"os"
	"sync"
	"testing"
)

// TestRWDB_ConcurrentReaders verifies that multiple goroutines can read
// simultaneously through the reader pool without blocking. With the old
// MaxOpenConns(2), only 1 concurrent reader was possible. With the reader
// pool at MaxOpenConns=8, all 8 goroutines should proceed in parallel.
func TestRWDB_ConcurrentReaders(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp("", "rwdb-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	st, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Seed a memory so queries return data.
	if _, err := st.InsertMemory(Memory{
		Tier:    "entity",
		Content: "rwdb test memory",
	}); err != nil {
		t.Fatalf("save memory: %v", err)
	}

	// Launch 8 concurrent readers — all should complete without deadlock.
	const readers = 8
	var wg sync.WaitGroup
	wg.Add(readers)
	errs := make(chan error, readers)

	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			rows, err := st.knowledgeDB.Query(`SELECT id, content FROM memories LIMIT 1`)
			if err != nil {
				errs <- err
				return
			}
			defer rows.Close()
			for rows.Next() {
				var id, content string
				if scanErr := rows.Scan(&id, &content); scanErr != nil {
					errs <- scanErr
					return
				}
			}
			if rows.Err() != nil {
				errs <- rows.Err()
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent reader error: %v", err)
	}
}

// TestRWDB_WriterSerializes verifies that writes go through the writer pool
// (MaxOpenConns=1) while reads continue through the reader pool.
func TestRWDB_WriterSerializes(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp("", "rwdb-write-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	st, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Concurrent writes + reads should not deadlock or error.
	const workers = 4
	var wg sync.WaitGroup
	wg.Add(workers * 2) // writers + readers
	errs := make(chan error, workers*2)

	// Writers
	for i := 0; i < workers; i++ {
		go func(n int) {
			defer wg.Done()
			_, err := st.InsertMemory(Memory{
				Tier:    "entity",
				Content: "concurrent write test",
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}

	// Readers (concurrent with writers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			var count int
			err := st.knowledgeDB.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count)
			if err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent r/w error: %v", err)
	}
}

// TestRWDB_ReaderPoolSize verifies the reader pool has the expected
// MaxOpenConns configuration (8 for primary stores).
func TestRWDB_ReaderPoolSize(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp("", "rwdb-pool-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	st, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Verify reader and writer are separate DB handles.
	if st.graphDB.reader == nil {
		t.Error("graphDB reader pool should not be nil for primary store")
	}
	if st.knowledgeDB.reader == nil {
		t.Error("knowledgeDB reader pool should not be nil for primary store")
	}
	if st.graphDB.writer == st.graphDB.reader {
		t.Error("graphDB writer and reader should be different *sql.DB instances")
	}
	if st.knowledgeDB.writer == st.knowledgeDB.reader {
		t.Error("knowledgeDB writer and reader should be different *sql.DB instances")
	}
}

// TestRWDB_ReadOnlyWrapsSingleDB verifies that OpenReadOnly uses a single
// DB (no separate reader pool) since all operations are reads.
func TestRWDB_ReadOnlyWrapsSingleDB(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp("", "rwdb-ro-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	// First create a valid store so we have schema.
	st, err := Open(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	st.Close()

	// Open read-only.
	ro, err := OpenReadOnly(f.Name())
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()

	// Read-only stores use wrapSingleDB: reader is nil (readerPool() falls back to writer).
	if ro.graphDB.reader != nil {
		t.Error("read-only graphDB should use single DB (reader should be nil)")
	}
}
