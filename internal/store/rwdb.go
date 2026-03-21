package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
)

// rwDB separates read and write SQLite connections for the same database file.
// Write operations (Exec, Begin) use a single-connection writer pool to
// serialize writes. Read operations (Query, QueryRow) use a multi-connection
// reader pool for concurrency. WAL mode guarantees readers never block writers
// and vice versa — the connection pool was the only bottleneck.
type rwDB struct {
	writer *sql.DB
	reader *sql.DB // nil when reader == writer (e.g. in-memory DBs)
}

// readerPool returns the reader DB, falling back to writer when no separate
// reader exists (in-memory databases, federation read-only stores).
func (rw *rwDB) readerPool() *sql.DB {
	if rw.reader != nil {
		return rw.reader
	}
	return rw.writer
}

// --- Read operations → reader pool ---

func (rw *rwDB) Query(query string, args ...any) (*sql.Rows, error) {
	return rw.readerPool().Query(query, args...)
}

func (rw *rwDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return rw.readerPool().QueryContext(ctx, query, args...)
}

func (rw *rwDB) QueryRow(query string, args ...any) *sql.Row {
	return rw.readerPool().QueryRow(query, args...)
}

func (rw *rwDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return rw.readerPool().QueryRowContext(ctx, query, args...)
}

// --- Write operations → writer pool ---

func (rw *rwDB) Exec(query string, args ...any) (sql.Result, error) {
	return rw.writer.Exec(query, args...)
}

func (rw *rwDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return rw.writer.ExecContext(ctx, query, args...)
}

func (rw *rwDB) Begin() (*sql.Tx, error) {
	return rw.writer.Begin()
}

func (rw *rwDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return rw.writer.BeginTx(ctx, opts)
}

// --- Shared operations ---

func (rw *rwDB) Close() error {
	var firstErr error
	if rw.reader != nil {
		if err := rw.reader.Close(); err != nil {
			firstErr = err
		}
	}
	if err := rw.writer.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (rw *rwDB) Ping() error {
	return rw.writer.Ping()
}

func (rw *rwDB) SetMaxOpenConns(n int) {
	rw.writer.SetMaxOpenConns(n)
}

// Writer returns the underlying writer *sql.DB for callers that need direct
// access (e.g. tests, PRAGMA calls, federation probes).
func (rw *rwDB) Writer() *sql.DB {
	return rw.writer
}

// newRWDB creates a reader/writer pool pair for the given database path.
// The writer pool has MaxOpenConns=1 (serialized writes).
// The reader pool has MaxOpenConns=maxReaders for concurrent reads.
// Both share the same WAL-mode database file.
func newRWDB(path string, writerDB *sql.DB, maxReaders int) (*rwDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create parent dir for reader %s: %w", path, err)
	}
	// Open same file with WAL + busy_timeout for the reader pool.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	readerDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open reader db %s: %w", path, err)
	}
	readerDB.SetMaxOpenConns(maxReaders)
	// Verify the reader connection is healthy.
	if err := readerDB.Ping(); err != nil {
		readerDB.Close()
		return nil, fmt.Errorf("ping reader db %s: %w", path, err)
	}

	writerDB.SetMaxOpenConns(1)

	return &rwDB{writer: writerDB, reader: readerDB}, nil
}

// wrapSingleDB creates an rwDB where writer and reader share the same *sql.DB.
// Used for in-memory databases and read-only federation stores.
func wrapSingleDB(db *sql.DB) *rwDB {
	return &rwDB{writer: db, reader: nil}
}
