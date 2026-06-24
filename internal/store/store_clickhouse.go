package store

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // registers the "clickhouse" database/sql driver
)

// clickhouseDialect backs request_logs with ClickHouse, an append-only OLAP column store. It
// is only ever a logs backend (api_keys stay in SQLite/PostgreSQL): ClickHouse has no
// auto-increment, no enforced UNIQUE, and only asynchronous ALTER … DELETE mutations, none of
// which suit the small, mutable keys table. The ORDER-BY/aggregation that request_logs needs,
// on the other hand, is exactly what it is built for.
type clickhouseDialect struct {
	ids *idGen
}

// rebind is the identity: the clickhouse-go database/sql driver uses '?' placeholders.
func (clickhouseDialect) rebind(q string) string { return q }

// sumInt casts the aggregate back to a signed 64-bit int so it scans into Go int64;
// ClickHouse sum() over an Int64 column widens to a different (often unsigned) type.
func (clickhouseDialect) sumInt(col string) string { return "toInt64(sum(" + col + "))" }

// afterDeleteKey is a no-op: ClickHouse reclaims space via background merges of the mutation.
func (clickhouseDialect) afterDeleteKey(db *sql.DB) error { return nil }

// genLogID returns the next monotonic, time-ordered id (ClickHouse has no auto-increment).
func (d clickhouseDialect) genLogID() int64 { return d.ids.next() }

// deleteLogsByKey removes a key's logs via a synchronous lightweight mutation. mutations_sync
// = 2 waits for the mutation (and active replicas) to finish so the delete is visible at once,
// matching the transactional cascade behavior of the SQL backends.
func (clickhouseDialect) deleteLogsByKey(db *sql.DB, apiKeyID int64) error {
	_, err := db.Exec(`ALTER TABLE request_logs DELETE WHERE api_key_id = ? SETTINGS mutations_sync = 2`, apiKeyID)
	return err
}

// idGen hands out strictly increasing, time-ordered int64 ids. Each id is the current unix
// millisecond shifted left 20 bits (≈1e6 ids of headroom per ms), bumped past the previous id
// when the clock has not advanced. Because the high bits track wall-clock time, ids keep
// rising across process restarts, so request_logs ORDER BY id DESC stays newest-first.
type idGen struct {
	mu   sync.Mutex
	last int64
}

func (g *idGen) next() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := time.Now().UnixMilli() << 20
	if id <= g.last {
		id = g.last + 1
	}
	g.last = id
	return id
}

// openClickHouse connects to ClickHouse using the given clickhouse:// DSN and creates the
// request_logs table. It only ever owns request_logs, so it does not create api_keys.
func openClickHouse(dsn string) (*Store, error) {
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	// sql.Open is lazy; ping eagerly so a bad DSN or unreachable server surfaces as a clear
	// startup error rather than on the first request.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect clickhouse: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute)

	d := clickhouseDialect{ids: &idGen{}}
	s := &Store{db: db, d: d}
	if err := d.migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate creates the request_logs table and applies any additive column upgrades. The column
// set mirrors the SQLite/PostgreSQL request_logs (SQLite INTEGER/BLOB/REAL map to ClickHouse
// Int64/String/Float64; the bool-as-int flags are UInt8), so the shared INSERT and the
// QueryLogs/Stats SELECT+scan work unchanged. ids are supplied client-side (see idGen), so id
// is a plain Int64 with no auto-increment.
func (clickhouseDialect) migrate(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS request_logs (
    id                 Int64,
    api_key_id         Int64,
    key_name           String,
    provider           String,
    model              String,
    mapped_model       String DEFAULT '',
    method             String DEFAULT '',
    path               String DEFAULT '',
    query              String DEFAULT '',
    client_addr        String DEFAULT '',
    user_agent         String DEFAULT '',
    request_content_type  String DEFAULT '',
    response_content_type String DEFAULT '',
    request_bytes      Int64 DEFAULT 0,
    response_bytes     Int64 DEFAULT 0,
    status_code        Int64,
    streaming          UInt8,
    attempts           Int64 DEFAULT 0,
    ttft_ms            Int64,
    duration_ms        Int64,
    input_tokens       Int64,
    output_tokens      Int64,
    cache_read_tokens  Int64,
    cache_write_tokens Int64,
    cost               Float64,
    error              String,
    api_type           String DEFAULT '',
    assemble_error     String DEFAULT '',
    raw_request        String DEFAULT '',
    raw_response       String DEFAULT '',
    assembled_response String DEFAULT '',
    raw_request_truncated        UInt8 DEFAULT 0,
    raw_response_truncated       UInt8 DEFAULT 0,
    assembled_response_truncated UInt8 DEFAULT 0,
    created_at         Int64
) ENGINE = MergeTree ORDER BY (api_key_id, id)
`
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Additive columns for tables created by older versions. ClickHouse's ADD COLUMN IF NOT
	// EXISTS is idempotent, so this is a no-op on an up-to-date schema. Keep this list in sync
	// with the ensureColumn list in store_sqlite.go and the ALTER list in store_postgres.go
	// (SQLite INTEGER/BLOB/REAL → ClickHouse Int64/String/Float64; bool flags → UInt8).
	alters := []struct{ column, decl string }{
		{"mapped_model", "String DEFAULT ''"},
		{"method", "String DEFAULT ''"},
		{"path", "String DEFAULT ''"},
		{"query", "String DEFAULT ''"},
		{"client_addr", "String DEFAULT ''"},
		{"user_agent", "String DEFAULT ''"},
		{"attempts", "Int64 DEFAULT 0"},
		{"request_content_type", "String DEFAULT ''"},
		{"response_content_type", "String DEFAULT ''"},
		{"request_bytes", "Int64 DEFAULT 0"},
		{"response_bytes", "Int64 DEFAULT 0"},
		{"api_type", "String DEFAULT ''"},
		{"assemble_error", "String DEFAULT ''"},
		{"raw_request", "String DEFAULT ''"},
		{"raw_response", "String DEFAULT ''"},
		{"assembled_response", "String DEFAULT ''"},
		{"raw_request_truncated", "UInt8 DEFAULT 0"},
		{"raw_response_truncated", "UInt8 DEFAULT 0"},
		{"assembled_response_truncated", "UInt8 DEFAULT 0"},
	}
	for _, c := range alters {
		stmt := fmt.Sprintf("ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS %s %s", c.column, c.decl)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column request_logs.%s: %w", c.column, err)
		}
	}
	return nil
}
