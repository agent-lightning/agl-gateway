// Package store is the SQLite-or-PostgreSQL persistence layer for API keys and request
// logs. The backend is chosen at Open time by the database string: a postgres://
// (or postgresql://) URL selects PostgreSQL, anything else is treated as a SQLite file
// path. The query logic is shared; a small dialect isolates the few real SQL differences.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	Method                     string    `json:"method"`
	Path                       string    `json:"path"`
	Query                      string    `json:"query"`
	ClientAddr                 string    `json:"client_addr"`
	UserAgent                  string    `json:"user_agent"`
	RequestContentType         string    `json:"request_content_type"`
	ResponseContentType        string    `json:"response_content_type"`
	RequestBytes               int64     `json:"request_bytes"`
	ResponseBytes              int64     `json:"response_bytes"`
	StatusCode                 int       `json:"status_code"`
	Streaming                  bool      `json:"streaming"`
	APIType                    string    `json:"api_type,omitempty"`
	Attempts                   int       `json:"attempts"`
	TTFTMillis                 int64     `json:"ttft_ms"`
	DurationMillis             int64     `json:"duration_ms"`
	InputTokens                int       `json:"input_tokens"`
	OutputTokens               int       `json:"output_tokens"`
	CacheReadTokens            int       `json:"cache_read_tokens"`
	CacheWriteTokens           int       `json:"cache_write_tokens"`
	Cost                       float64   `json:"cost"`
	Error                      string    `json:"error"`
	AssembleError              string    `json:"assemble_error,omitempty"`
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
	// ID selects a single log by primary key. When > 0 every other constraint
	// (key/provider/window/pagination) is still applied, but a lookup-by-id is normally
	// issued on its own to fetch one row (with payloads) for the inspector drawer.
	ID       int64
	APIKeyID int64
	Provider string
	// Since and Until bound the created_at window: created_at >= Since and created_at <
	// Until. Either may be zero to leave that side unbounded, so a fixed period is expressed
	// by setting both.
	Since  time.Time
	Until  time.Time
	Limit  int
	Offset int
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

// Store wraps the database, delegating the handful of backend-specific SQL details to a
// dialect. Query bodies are written once with neutral '?' placeholders and rebound per
// driver.
type Store struct {
	db *sql.DB
	d  dialect
}

// dialect captures the SQL differences between the SQLite and PostgreSQL backends. The
// CRUD query bodies and row scanning live in store.go and are shared by both.
type dialect interface {
	// rebind rewrites neutral '?' placeholders into the driver's parameter syntax.
	// SQLite keeps '?'; PostgreSQL rewrites them to $1, $2, … in order.
	rebind(q string) string
	// sumInt wraps an aggregate over an integer column so the result scans into int64.
	// SQLite returns SUM(col); PostgreSQL casts because SUM(bigint) is numeric.
	sumInt(col string) string
	// migrate creates the schema and applies any in-place column upgrades.
	migrate(db *sql.DB) error
	// afterDeleteKey reclaims space freed by a cascade delete. No-op on PostgreSQL.
	afterDeleteKey(db *sql.DB) error
}

// Open opens (creating if needed) the persistence backend and applies the schema. A
// postgres:// or postgresql:// URL selects PostgreSQL; anything else (including an empty
// string or ":memory:") is a SQLite file path, yielding an in-memory DB when empty or
// ":memory:" (useful for tests).
func Open(database string) (*Store, error) {
	if strings.HasPrefix(database, "postgres://") || strings.HasPrefix(database, "postgresql://") {
		return openPostgres(database)
	}
	return openSQLite(database)
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// CreateKey inserts a new API key and returns the stored row.
func (s *Store) CreateKey(name, hash, prefix string, providers []string, start, order string) (*APIKey, error) {
	provJSON, err := json.Marshal(providers)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var id int64
	err = s.db.QueryRow(s.d.rebind(
		`INSERT INTO api_keys (name, hash, prefix, providers, provider_start, provider_order, created_at, disabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0) RETURNING id`),
		name, hash, prefix, string(provJSON), start, order, now.UnixMilli(),
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("create key: %w", err)
	}
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
	row := s.db.QueryRow(s.d.rebind(
		`SELECT id, name, hash, prefix, providers, provider_start, provider_order, created_at, disabled FROM api_keys WHERE hash = ?`), hash)
	return scanKey(row)
}

// KeyByID looks up an API key by numeric id. It returns (nil, nil) when no matching key
// exists.
func (s *Store) KeyByID(id int64) (*APIKey, error) {
	row := s.db.QueryRow(s.d.rebind(
		`SELECT id, name, hash, prefix, providers, provider_start, provider_order, created_at, disabled FROM api_keys WHERE id = ?`), id)
	return scanKey(row)
}

// ListKeys returns all API keys, newest first.
func (s *Store) ListKeys() ([]APIKey, error) {
	rows, err := s.db.Query(s.d.rebind(
		`SELECT id, name, hash, prefix, providers, provider_start, provider_order, created_at, disabled FROM api_keys ORDER BY id DESC`))
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

	if _, err := tx.Exec(s.d.rebind(`DELETE FROM request_logs WHERE api_key_id = ?`), id); err != nil {
		return false, err
	}
	res, err := tx.Exec(s.d.rebind(`DELETE FROM api_keys WHERE id = ?`), id)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Best-effort: reclaim the space freed by the deleted logs (SQLite only).
		_ = s.d.afterDeleteKey(s.db)
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
	_, err := s.db.Exec(s.d.rebind(`
INSERT INTO request_logs
 (api_key_id, key_name, provider, model, mapped_model, method, path, query,
  client_addr, user_agent, request_content_type, response_content_type,
  request_bytes, response_bytes, status_code, streaming, attempts,
  ttft_ms, duration_ms, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
  cost, error, api_type, assemble_error, raw_request, raw_response, assembled_response, raw_request_truncated,
  raw_response_truncated, assembled_response_truncated, created_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		l.APIKeyID, l.KeyName, l.Provider, l.Model, l.MappedModel, l.Method, l.Path, l.Query,
		l.ClientAddr, l.UserAgent, l.RequestContentType, l.ResponseContentType,
		l.RequestBytes, l.ResponseBytes, l.StatusCode, b2i(l.Streaming),
		l.Attempts, l.TTFTMillis, l.DurationMillis, l.InputTokens, l.OutputTokens, l.CacheReadTokens,
		l.CacheWriteTokens, l.Cost, l.Error, l.APIType, l.AssembleError, l.RawRequest, l.RawResponse, l.AssembledResponse,
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
	cols := `id, api_key_id, key_name, provider, model, mapped_model, method, path, query,
	             client_addr, user_agent, status_code, streaming,
	             request_content_type, response_content_type, request_bytes, response_bytes,
	             attempts, ttft_ms, duration_ms,
	             input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost, error,
	             api_type, assemble_error,`
	if f.IncludePayloads {
		cols += ` raw_request, raw_response, assembled_response,`
	}
	cols += ` raw_request_truncated, raw_response_truncated, assembled_response_truncated, created_at`
	q := `SELECT ` + cols + ` FROM request_logs WHERE 1=1`
	var args []any
	if f.ID > 0 {
		q += " AND id = ?"
		args = append(args, f.ID)
	}
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
	if !f.Until.IsZero() {
		q += " AND created_at < ?"
		args = append(args, f.Until.UnixMilli())
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
	rows, err := s.db.Query(s.d.rebind(q), args...)
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
			&l.Method, &l.Path, &l.Query, &l.ClientAddr, &l.UserAgent,
			&l.StatusCode, &streaming, &l.RequestContentType, &l.ResponseContentType,
			&l.RequestBytes, &l.ResponseBytes,
			&l.Attempts, &l.TTFTMillis, &l.DurationMillis, &l.InputTokens, &l.OutputTokens,
			&l.CacheReadTokens, &l.CacheWriteTokens, &l.Cost, &l.Error, &l.APIType, &l.AssembleError}
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
	             ` + s.d.sumInt("input_tokens") + `, ` + s.d.sumInt("output_tokens") + `, ` +
		s.d.sumInt("cache_read_tokens") + `, ` + s.d.sumInt("cache_write_tokens") + `, SUM(cost)
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
	if !f.Until.IsZero() {
		q += " AND created_at < ?"
		args = append(args, f.Until.UnixMilli())
	}
	q += " GROUP BY api_key_id, key_name, model ORDER BY SUM(cost) DESC"
	rows, err := s.db.Query(s.d.rebind(q), args...)
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
