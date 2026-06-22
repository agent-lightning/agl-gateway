// Package store is the SQLite persistence layer for API keys and request logs.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// APIKey is a stored gateway credential. The plaintext key itself is never stored;
// only its SHA-256 hash (for lookup) and a short prefix (for display).
type APIKey struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Hash          string    `json:"-"`
	Prefix        string    `json:"prefix"`
	Providers     []string  `json:"providers"`
	ProviderStart string    `json:"provider_start"`
	ProviderOrder string    `json:"provider_order"`
	CreatedAt     time.Time `json:"created_at"`
	Disabled      bool      `json:"disabled"`
}

// RequestLog is the recorded metadata for a single proxied request.
type RequestLog struct {
	ID                         int64     `json:"id"`
	APIKeyID                   int64     `json:"api_key_id"`
	KeyName                    string    `json:"key_name"`
	Provider                   string    `json:"provider"`
	Model                      string    `json:"model"`
	MappedModel                string    `json:"mapped_model"`
	RequestContentType         string    `json:"request_content_type"`
	ResponseContentType        string    `json:"response_content_type"`
	StatusCode                 int       `json:"status_code"`
	Streaming                  bool      `json:"streaming"`
	Attempts                   int       `json:"attempts"`
	TTFTMillis                 int64     `json:"ttft_ms"`
	DurationMillis             int64     `json:"duration_ms"`
	InputTokens                int       `json:"input_tokens"`
	OutputTokens               int       `json:"output_tokens"`
	CacheReadTokens            int       `json:"cache_read_tokens"`
	CacheWriteTokens           int       `json:"cache_write_tokens"`
	Cost                       float64   `json:"cost"`
	Error                      string    `json:"error"`
	RawRequest                 []byte    `json:"raw_request,omitempty"`
	RawResponse                []byte    `json:"raw_response,omitempty"`
	AssembledResponse          []byte    `json:"assembled_response,omitempty"`
	RawRequestTruncated        bool      `json:"raw_request_truncated"`
	RawResponseTruncated       bool      `json:"raw_response_truncated"`
	AssembledResponseTruncated bool      `json:"assembled_response_truncated"`
	CreatedAt                  time.Time `json:"created_at"`
}

// LogFilter constrains a log query. Zero-valued fields are ignored.
type LogFilter struct {
	APIKeyID int64
	Provider string
	Since    time.Time
	Limit    int
	Offset   int
	// IncludePayloads selects the heavy raw_request/raw_response/assembled_response BLOB
	// columns. When false (the default) they are omitted from the query, so the returned
	// RequestLogs carry nil payload bytes regardless of what was captured.
	IncludePayloads bool
}

// Stat is an aggregate roll-up of request logs grouped by key and model.
type Stat struct {
	APIKeyID     int64   `json:"api_key_id"`
	KeyName      string  `json:"key_name"`
	Model        string  `json:"model"`
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheRead    int64   `json:"cache_read_tokens"`
	CacheWrite   int64   `json:"cache_write_tokens"`
	Cost         float64 `json:"cost"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies the schema.
// An empty path or ":memory:" yields an in-memory database (useful for tests).
func Open(path string) (*Store, error) {
	if path == "" {
		path = ":memory:"
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	// auto_vacuum must be set before any table exists; it is a no-op on a populated DB.
	// It lets incremental_vacuum (run after cascade deletes) return freed pages to the OS.
	if _, err := s.db.Exec(`PRAGMA auto_vacuum=INCREMENTAL;`); err != nil {
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
    disabled       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS request_logs (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id         INTEGER NOT NULL,
    key_name           TEXT NOT NULL,
    provider           TEXT NOT NULL,
    model              TEXT NOT NULL,
    mapped_model       TEXT NOT NULL DEFAULT '',
    request_content_type  TEXT NOT NULL DEFAULT '',
    response_content_type TEXT NOT NULL DEFAULT '',
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
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Additive columns for databases created by older versions.
	if err := s.ensureColumn("request_logs", "mapped_model", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "attempts", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "request_content_type", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "response_content_type", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "raw_request", "BLOB"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "raw_response", "BLOB"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "assembled_response", "BLOB"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "raw_request_truncated", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "raw_response_truncated", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("request_logs", "assembled_response_truncated", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn("api_keys", "provider_start", "TEXT NOT NULL DEFAULT 'first'"); err != nil {
		return err
	}
	if err := s.ensureColumn("api_keys", "provider_order", "TEXT NOT NULL DEFAULT 'round_robin'"); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds a column to a table if it does not already exist.
func (s *Store) ensureColumn(table, column, decl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
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
	if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		return fmt.Errorf("add column %s.%s: %w", table, column, err)
	}
	return nil
}

// CreateKey inserts a new API key and returns the stored row.
func (s *Store) CreateKey(name, hash, prefix string, providers []string, start, order string) (*APIKey, error) {
	provJSON, err := json.Marshal(providers)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO api_keys (name, hash, prefix, providers, provider_start, provider_order, created_at, disabled) VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		name, hash, prefix, string(provJSON), start, order, now.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("create key: %w", err)
	}
	id, _ := res.LastInsertId()
	return &APIKey{
		ID:            id,
		Name:          name,
		Hash:          hash,
		Prefix:        prefix,
		Providers:     providers,
		ProviderStart: start,
		ProviderOrder: order,
		CreatedAt:     time.UnixMilli(now.UnixMilli()),
	}, nil
}

// KeyByHash looks up an API key by its SHA-256 hash. It returns (nil, nil) when no
// matching key exists.
func (s *Store) KeyByHash(hash string) (*APIKey, error) {
	row := s.db.QueryRow(
		`SELECT id, name, hash, prefix, providers, provider_start, provider_order, created_at, disabled FROM api_keys WHERE hash = ?`, hash)
	return scanKey(row)
}

// KeyByID looks up an API key by numeric id. It returns (nil, nil) when no matching key
// exists.
func (s *Store) KeyByID(id int64) (*APIKey, error) {
	row := s.db.QueryRow(
		`SELECT id, name, hash, prefix, providers, provider_start, provider_order, created_at, disabled FROM api_keys WHERE id = ?`, id)
	return scanKey(row)
}

// ListKeys returns all API keys, newest first.
func (s *Store) ListKeys() ([]APIKey, error) {
	rows, err := s.db.Query(
		`SELECT id, name, hash, prefix, providers, provider_start, provider_order, created_at, disabled FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, *k)
	}
	return keys, rows.Err()
}

// DeleteKey removes the key with the given id and cascades to every request log bound to
// it, then returns freed pages to the OS. It reports whether a key row was deleted.
func (s *Store) DeleteKey(id int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM request_logs WHERE api_key_id = ?`, id); err != nil {
		return false, err
	}
	res, err := tx.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Best-effort: reclaim the space freed by the deleted logs.
		_, _ = s.db.Exec(`PRAGMA incremental_vacuum;`)
	}
	return n > 0, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanKey(sc scanner) (*APIKey, error) {
	var (
		k         APIKey
		provJSON  string
		createdMs int64
		disabled  int
	)
	err := sc.Scan(&k.ID, &k.Name, &k.Hash, &k.Prefix, &provJSON, &k.ProviderStart, &k.ProviderOrder, &createdMs, &disabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(provJSON), &k.Providers); err != nil {
		return nil, err
	}
	k.CreatedAt = time.UnixMilli(createdMs)
	k.Disabled = disabled != 0
	return &k, nil
}

// InsertLog records a request log row, stamping CreatedAt if unset.
func (s *Store) InsertLog(l *RequestLog) error {
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`
INSERT INTO request_logs
 (api_key_id, key_name, provider, model, mapped_model, request_content_type,
  response_content_type, status_code, streaming, attempts,
  ttft_ms, duration_ms, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  cost, error, raw_request, raw_response, assembled_response, raw_request_truncated,
  raw_response_truncated, assembled_response_truncated, created_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.APIKeyID, l.KeyName, l.Provider, l.Model, l.MappedModel, l.RequestContentType,
		l.ResponseContentType, l.StatusCode, b2i(l.Streaming),
		l.Attempts, l.TTFTMillis, l.DurationMillis, l.InputTokens, l.OutputTokens, l.CacheReadTokens,
		l.CacheWriteTokens, l.Cost, l.Error, l.RawRequest, l.RawResponse, l.AssembledResponse,
		b2i(l.RawRequestTruncated), b2i(l.RawResponseTruncated), b2i(l.AssembledResponseTruncated),
		l.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("insert log: %w", err)
	}
	return nil
}

// QueryLogs returns request logs matching the filter, newest first. The heavy payload
// BLOB columns are only read when f.IncludePayloads is set.
func (s *Store) QueryLogs(f LogFilter) ([]RequestLog, error) {
	cols := `id, api_key_id, key_name, provider, model, mapped_model, status_code, streaming,
	             request_content_type, response_content_type, attempts, ttft_ms, duration_ms,
	             input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost, error,`
	if f.IncludePayloads {
		cols += ` raw_request, raw_response, assembled_response,`
	}
	cols += ` raw_request_truncated, raw_response_truncated, assembled_response_truncated, created_at`
	q := `SELECT ` + cols + ` FROM request_logs WHERE 1=1`
	var args []any
	if f.APIKeyID > 0 {
		q += " AND api_key_id = ?"
		args = append(args, f.APIKeyID)
	}
	if f.Provider != "" {
		q += " AND provider = ?"
		args = append(args, f.Provider)
	}
	if !f.Since.IsZero() {
		q += " AND created_at >= ?"
		args = append(args, f.Since.UnixMilli())
	}
	q += " ORDER BY id DESC"
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	q += " LIMIT ?"
	args = append(args, limit)
	if f.Offset > 0 {
		q += " OFFSET ?"
		args = append(args, f.Offset)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []RequestLog
	for rows.Next() {
		var (
			l                  RequestLog
			streaming          int
			rawReqTruncated    int
			rawRespTruncated   int
			assembledTruncated int
			createdMs          int64
		)
		dest := []any{&l.ID, &l.APIKeyID, &l.KeyName, &l.Provider, &l.Model, &l.MappedModel,
			&l.StatusCode, &streaming, &l.RequestContentType, &l.ResponseContentType,
			&l.Attempts, &l.TTFTMillis, &l.DurationMillis, &l.InputTokens, &l.OutputTokens,
			&l.CacheReadTokens, &l.CacheWriteTokens, &l.Cost, &l.Error}
		if f.IncludePayloads {
			dest = append(dest, &l.RawRequest, &l.RawResponse, &l.AssembledResponse)
		}
		dest = append(dest, &rawReqTruncated, &rawRespTruncated, &assembledTruncated, &createdMs)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		l.Streaming = streaming != 0
		l.RawRequestTruncated = rawReqTruncated != 0
		l.RawResponseTruncated = rawRespTruncated != 0
		l.AssembledResponseTruncated = assembledTruncated != 0
		l.CreatedAt = time.UnixMilli(createdMs)
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// Stats returns aggregate usage grouped by (api_key_id, model), highest cost first.
func (s *Store) Stats(f LogFilter) ([]Stat, error) {
	q := `SELECT api_key_id, key_name, model, COUNT(*),
	             SUM(input_tokens), SUM(output_tokens), SUM(cache_read_tokens),
	             SUM(cache_write_tokens), SUM(cost)
	      FROM request_logs WHERE 1=1`
	var args []any
	if f.APIKeyID > 0 {
		q += " AND api_key_id = ?"
		args = append(args, f.APIKeyID)
	}
	if !f.Since.IsZero() {
		q += " AND created_at >= ?"
		args = append(args, f.Since.UnixMilli())
	}
	q += " GROUP BY api_key_id, model ORDER BY SUM(cost) DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stats []Stat
	for rows.Next() {
		var st Stat
		if err := rows.Scan(&st.APIKeyID, &st.KeyName, &st.Model, &st.Requests,
			&st.InputTokens, &st.OutputTokens, &st.CacheRead, &st.CacheWrite, &st.Cost); err != nil {
			return nil, err
		}
		stats = append(stats, st)
	}
	return stats, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
