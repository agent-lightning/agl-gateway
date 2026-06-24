package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// sqliteDialect is the default backend: a pure-Go SQLite file (or in-memory) database.
type sqliteDialect struct{}

// rebind is the identity for SQLite, which uses '?' placeholders natively.
func (sqliteDialect) rebind(q string) string { return q }

// sumInt needs no cast: SQLite SUM over an INTEGER column yields an integer.
func (sqliteDialect) sumInt(col string) string { return "SUM(" + col + ")" }

// afterDeleteKey returns pages freed by the cascade delete to the OS. auto_vacuum is set
// to INCREMENTAL at migrate time, so this is what actually releases the space.
func (sqliteDialect) afterDeleteKey(db *sql.DB) error {
	_, err := db.Exec(`PRAGMA incremental_vacuum;`)
	return err
}

// genLogID returns 0: SQLite assigns request_logs ids via AUTOINCREMENT.
func (sqliteDialect) genLogID() int64 { return 0 }

// deleteLogsByKey removes a key's logs; used only when SQLite is a standalone logs backend.
func (d sqliteDialect) deleteLogsByKey(db *sql.DB, apiKeyID int64) error {
	_, err := db.Exec(d.rebind(`DELETE FROM request_logs WHERE api_key_id = ?`), apiKeyID)
	return err
}

// openSQLite opens (creating if needed) the SQLite database at path and applies the schema.
// An empty path or ":memory:" yields an in-memory database (useful for tests).
func openSQLite(path string) (*Store, error) {
	if path == "" {
		path = ":memory:"
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	s := &Store{db: db, d: sqliteDialect{}}
	if err := s.d.migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (sqliteDialect) migrate(db *sql.DB) error {
	// auto_vacuum must be set before any table exists; it is a no-op on a populated DB.
	// It lets incremental_vacuum (run after cascade deletes) return freed pages to the OS.
	if _, err := db.Exec(`PRAGMA auto_vacuum=INCREMENTAL;`); err != nil {
		return fmt.Errorf("migrate (auto_vacuum): %w", err)
	}
	const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT NOT NULL,
    hash           TEXT NOT NULL UNIQUE,
    prefix         TEXT NOT NULL,
    providers      TEXT NOT NULL,
    provider_start TEXT NOT NULL DEFAULT 'first',
    provider_order TEXT NOT NULL DEFAULT 'round_robin',
    created_at     INTEGER NOT NULL,
    disabled       INTEGER NOT NULL DEFAULT 0,
    keep_logs_on_delete INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS request_logs (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id         INTEGER NOT NULL,
    key_name           TEXT NOT NULL,
    provider           TEXT NOT NULL,
    model              TEXT NOT NULL,
    mapped_model       TEXT NOT NULL DEFAULT '',
    method             TEXT NOT NULL DEFAULT '',
    path               TEXT NOT NULL DEFAULT '',
    query              TEXT NOT NULL DEFAULT '',
    client_addr        TEXT NOT NULL DEFAULT '',
    user_agent         TEXT NOT NULL DEFAULT '',
    request_content_type  TEXT NOT NULL DEFAULT '',
    response_content_type TEXT NOT NULL DEFAULT '',
    request_bytes      INTEGER NOT NULL DEFAULT 0,
    response_bytes     INTEGER NOT NULL DEFAULT 0,
    status_code        INTEGER NOT NULL,
    streaming          INTEGER NOT NULL,
    attempts           INTEGER NOT NULL DEFAULT 0,
    ttft_ms            INTEGER NOT NULL,
    duration_ms        INTEGER NOT NULL,
    input_tokens       INTEGER NOT NULL,
    output_tokens      INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL,
    cache_write_tokens INTEGER NOT NULL,
    cost               REAL NOT NULL,
    error              TEXT NOT NULL,
    api_type           TEXT NOT NULL DEFAULT '',
    assemble_error     TEXT NOT NULL DEFAULT '',
    raw_request        BLOB,
    raw_response       BLOB,
    assembled_response BLOB,
    raw_request_truncated        INTEGER NOT NULL DEFAULT 0,
    raw_response_truncated       INTEGER NOT NULL DEFAULT 0,
    assembled_response_truncated INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_logs_created_at ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_logs_api_key_id ON request_logs(api_key_id);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Additive columns for databases created by older versions. Keep this list in sync with
	// the ADD COLUMN IF NOT EXISTS list in store_postgres.go.
	cols := []struct{ table, column, decl string }{
		{"request_logs", "mapped_model", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "method", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "path", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "query", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "client_addr", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "user_agent", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "request_content_type", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "response_content_type", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "request_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "response_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "api_type", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "assemble_error", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "raw_request", "BLOB"},
		{"request_logs", "raw_response", "BLOB"},
		{"request_logs", "assembled_response", "BLOB"},
		{"request_logs", "raw_request_truncated", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "raw_response_truncated", "INTEGER NOT NULL DEFAULT 0"},
		{"request_logs", "assembled_response_truncated", "INTEGER NOT NULL DEFAULT 0"},
		{"api_keys", "provider_start", "TEXT NOT NULL DEFAULT 'first'"},
		{"api_keys", "provider_order", "TEXT NOT NULL DEFAULT 'round_robin'"},
		{"api_keys", "keep_logs_on_delete", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, c := range cols {
		if err := ensureColumn(db, c.table, c.column, c.decl); err != nil {
			return err
		}
	}
	return nil
}

// ensureColumn adds a column to a table if it does not already exist.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}
