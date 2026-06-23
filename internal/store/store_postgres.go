package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// postgresDialect backs the store with PostgreSQL via the pure-Go pgx driver.
type postgresDialect struct{}

// rebind rewrites neutral '?' placeholders into PostgreSQL's $1, $2, … positional form.
// None of the store's SQL contains a literal '?', so a sequential scan is safe.
func (postgresDialect) rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// sumInt casts the aggregate back to bigint: PostgreSQL's SUM over a BIGINT column yields
// numeric, which does not scan into a Go int64.
func (postgresDialect) sumInt(col string) string { return "(SUM(" + col + "))::bigint" }

// afterDeleteKey is a no-op: PostgreSQL reclaims space via autovacuum.
func (postgresDialect) afterDeleteKey(db *sql.DB) error { return nil }

// openPostgres connects to PostgreSQL using the given DSN (a postgres:// or postgresql://
// URL, or a keyword/value string) and applies the schema.
func openPostgres(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// sql.Open is lazy for pgx; ping eagerly so a bad DSN or unreachable server surfaces
	// as a clear startup error rather than on the first request.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	// Bound the pool so the gateway under load cannot exhaust the server's max_connections.
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute)

	s := &Store{db: db, d: postgresDialect{}}
	if err := s.d.migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate creates the schema and applies any additive column upgrades. A PostgreSQL
// database created before a column was introduced keeps the old table, and CREATE ... IF
// NOT EXISTS is a no-op on it, so new columns are added in place with ALTER TABLE ... ADD
// COLUMN IF NOT EXISTS (idempotent), mirroring the SQLite backend's ensureColumn path.
//
// All integer columns are BIGINT (not INTEGER): PostgreSQL INTEGER is 32-bit, and the
// unix-millis timestamps (created_at/ttft_ms/duration_ms) overflow it; BIGINT throughout
// also keeps Go's row-scanning identical to the SQLite backend.
func (postgresDialect) migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL,
    hash           TEXT NOT NULL UNIQUE,
    prefix         TEXT NOT NULL,
    providers      TEXT NOT NULL,
    provider_start TEXT NOT NULL DEFAULT 'first',
    provider_order TEXT NOT NULL DEFAULT 'round_robin',
    created_at     BIGINT NOT NULL,
    disabled       BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS request_logs (
    id                 BIGSERIAL PRIMARY KEY,
    api_key_id         BIGINT NOT NULL,
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
    request_bytes      BIGINT NOT NULL DEFAULT 0,
    response_bytes     BIGINT NOT NULL DEFAULT 0,
    status_code        BIGINT NOT NULL,
    streaming          BIGINT NOT NULL,
    attempts           BIGINT NOT NULL DEFAULT 0,
    ttft_ms            BIGINT NOT NULL,
    duration_ms        BIGINT NOT NULL,
    input_tokens       BIGINT NOT NULL,
    output_tokens      BIGINT NOT NULL,
    cache_read_tokens  BIGINT NOT NULL,
    cache_write_tokens BIGINT NOT NULL,
    cost               DOUBLE PRECISION NOT NULL,
    error              TEXT NOT NULL,
    api_type           TEXT NOT NULL DEFAULT '',
    assemble_error     TEXT NOT NULL DEFAULT '',
    raw_request        BYTEA,
    raw_response       BYTEA,
    assembled_response BYTEA,
    raw_request_truncated        BIGINT NOT NULL DEFAULT 0,
    raw_response_truncated       BIGINT NOT NULL DEFAULT 0,
    assembled_response_truncated BIGINT NOT NULL DEFAULT 0,
    created_at         BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_logs_created_at ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_logs_api_key_id ON request_logs(api_key_id);
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Additive columns for databases created by older versions, mirroring the SQLite backend's
	// ensureColumn set (with PostgreSQL types: INTEGER→BIGINT, BLOB→BYTEA, REAL→DOUBLE
	// PRECISION). ADD COLUMN IF NOT EXISTS is idempotent, so this is a no-op on an up-to-date
	// schema. Keep this list in sync with the ensureColumn list in store_sqlite.go.
	alters := []struct{ table, column, decl string }{
		{"request_logs", "mapped_model", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "method", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "path", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "query", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "client_addr", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "user_agent", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "attempts", "BIGINT NOT NULL DEFAULT 0"},
		{"request_logs", "request_content_type", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "response_content_type", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "request_bytes", "BIGINT NOT NULL DEFAULT 0"},
		{"request_logs", "response_bytes", "BIGINT NOT NULL DEFAULT 0"},
		{"request_logs", "api_type", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "assemble_error", "TEXT NOT NULL DEFAULT ''"},
		{"request_logs", "raw_request", "BYTEA"},
		{"request_logs", "raw_response", "BYTEA"},
		{"request_logs", "assembled_response", "BYTEA"},
		{"request_logs", "raw_request_truncated", "BIGINT NOT NULL DEFAULT 0"},
		{"request_logs", "raw_response_truncated", "BIGINT NOT NULL DEFAULT 0"},
		{"request_logs", "assembled_response_truncated", "BIGINT NOT NULL DEFAULT 0"},
		{"api_keys", "provider_start", "TEXT NOT NULL DEFAULT 'first'"},
		{"api_keys", "provider_order", "TEXT NOT NULL DEFAULT 'round_robin'"},
	}
	for _, c := range alters {
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", c.table, c.column, c.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}
