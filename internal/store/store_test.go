package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// eachBackend runs fn against an in-memory SQLite store and, when AGL_DATABASE points at a
// PostgreSQL DSN, also against that store. AGL_DATABASE is the same official override the
// gateway honors (see config.DatabaseEnv), so the tests exercise PostgreSQL exactly as a
// real deployment would select it. The portable tests below assert only on relative ids and
// counts, so both backends must satisfy them identically. Each PostgreSQL run starts from a
// TRUNCATE ... RESTART IDENTITY so BIGSERIAL ids restart at 1, matching SQLite's fresh
// in-memory AUTOINCREMENT.
func eachBackend(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		s, err := Open(":memory:")
		if err != nil {
			t.Fatalf("Open sqlite: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		fn(t, s)
	})
	dsn := os.Getenv("AGL_DATABASE")
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		t.Log("AGL_DATABASE is not a postgres DSN; skipping postgres backend")
		return
	}
	t.Run("postgres", func(t *testing.T) {
		s, err := Open(dsn)
		if err != nil {
			t.Fatalf("Open postgres: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		if _, err := s.db.Exec(`TRUNCATE request_logs, api_keys RESTART IDENTITY`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		fn(t, s)
	})
}

func TestDeleteKeyCascadesLogs(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		k, err := s.CreateKey("dev", "h", "p", []string{"openai"}, "first", "round_robin")
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		for i := 0; i < 5; i++ {
			if err := s.InsertLog(&RequestLog{APIKeyID: k.ID, KeyName: "dev", Provider: "openai", Model: "m"}); err != nil {
				t.Fatalf("InsertLog: %v", err)
			}
		}
		// A log for a different key must survive.
		s.InsertLog(&RequestLog{APIKeyID: k.ID + 1, KeyName: "other", Provider: "openai", Model: "m"})

		if _, err := s.DeleteKey(k.ID); err != nil {
			t.Fatalf("DeleteKey: %v", err)
		}
		gone, _ := s.QueryLogs(LogFilter{APIKeyID: k.ID})
		if len(gone) != 0 {
			t.Errorf("expected logs cascade-deleted, got %d", len(gone))
		}
		remaining, _ := s.QueryLogs(LogFilter{})
		if len(remaining) != 1 {
			t.Errorf("unrelated logs affected: %d remaining, want 1", len(remaining))
		}
	})
}

func TestLogMappedModelAndAttempts(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		if err := s.InsertLog(&RequestLog{
			APIKeyID: 1, KeyName: "dev", Provider: "openai",
			Model: "alias", MappedModel: "gpt-5.4", Attempts: 3, StatusCode: 200,
			APIType: "openai_chat", AssembleError: "anthropic accumulate: boom",
			RequestContentType: "application/json", ResponseContentType: "text/event-stream",
			Method: "POST", Path: "/v1/chat/completions", Query: "beta=true",
			ClientAddr: "203.0.113.7:54321", UserAgent: "agl-test/1.0",
			RequestBytes: 42, ResponseBytes: 1024,
		}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
		logs, _ := s.QueryLogs(LogFilter{})
		if len(logs) != 1 {
			t.Fatalf("logs = %d", len(logs))
		}
		if logs[0].MappedModel != "gpt-5.4" || logs[0].Attempts != 3 {
			t.Errorf("round-trip mismatch: %+v", logs[0])
		}
		// api_type and assemble_error are returned without IncludePayloads.
		if logs[0].APIType != "openai_chat" || logs[0].AssembleError != "anthropic accumulate: boom" {
			t.Errorf("api_type/assemble_error = %q/%q", logs[0].APIType, logs[0].AssembleError)
		}
		if logs[0].RequestContentType != "application/json" || logs[0].ResponseContentType != "text/event-stream" {
			t.Errorf("content types = %q/%q", logs[0].RequestContentType, logs[0].ResponseContentType)
		}
		// Request-line metadata round-trips without IncludePayloads.
		got := logs[0]
		if got.Method != "POST" || got.Path != "/v1/chat/completions" || got.Query != "beta=true" {
			t.Errorf("request line = %q %q?%q", got.Method, got.Path, got.Query)
		}
		if got.ClientAddr != "203.0.113.7:54321" || got.UserAgent != "agl-test/1.0" {
			t.Errorf("client/user-agent = %q / %q", got.ClientAddr, got.UserAgent)
		}
		if got.RequestBytes != 42 || got.ResponseBytes != 1024 {
			t.Errorf("byte counts = %d / %d", got.RequestBytes, got.ResponseBytes)
		}
	})
}

func TestLogPayloadBytesRoundTrip(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		if err := s.InsertLog(&RequestLog{
			APIKeyID: 1, KeyName: "dev", Provider: "openai", Model: "gpt-5.4",
			StatusCode:                 200,
			RawRequest:                 []byte(`{"model":"gpt-5.4"}`),
			RawResponse:                []byte("data: x\n\n"),
			AssembledResponse:          []byte(`{"output_text":"x"}`),
			RawRequestTruncated:        true,
			RawResponseTruncated:       true,
			AssembledResponseTruncated: true,
		}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
		// Without IncludePayloads the heavy blob columns are omitted.
		bare, err := s.QueryLogs(LogFilter{})
		if err != nil {
			t.Fatalf("QueryLogs(bare): %v", err)
		}
		if len(bare) != 1 || bare[0].RawRequest != nil || bare[0].RawResponse != nil || bare[0].AssembledResponse != nil {
			t.Errorf("payloads should be omitted by default: %+v", bare)
		}
		logs, err := s.QueryLogs(LogFilter{IncludePayloads: true})
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		if len(logs) != 1 {
			t.Fatalf("logs = %d, want 1", len(logs))
		}
		got := logs[0]
		if string(got.RawRequest) != `{"model":"gpt-5.4"}` ||
			string(got.RawResponse) != "data: x\n\n" ||
			string(got.AssembledResponse) != `{"output_text":"x"}` {
			t.Errorf("payload bytes = %q / %q / %q", got.RawRequest, got.RawResponse, got.AssembledResponse)
		}
		if !got.RawRequestTruncated || !got.RawResponseTruncated || !got.AssembledResponseTruncated {
			t.Errorf("truncation flags = %+v", got)
		}
	})
}

func TestKeyLifecycle(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		k, err := s.CreateKey("dev", "hash-abc", "sk-gw-ab", []string{"openai", "anthropic"}, "first", "round_robin")
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if k.ID == 0 {
			t.Fatal("CreateKey returned id 0")
		}

		got, err := s.KeyByHash("hash-abc")
		if err != nil {
			t.Fatalf("KeyByHash: %v", err)
		}
		if got == nil {
			t.Fatal("KeyByHash returned nil for existing key")
		}
		if got.Name != "dev" || len(got.Providers) != 2 || got.Providers[0] != "openai" {
			t.Errorf("unexpected key: %+v", got)
		}
		byID, err := s.KeyByID(k.ID)
		if err != nil {
			t.Fatalf("KeyByID: %v", err)
		}
		if byID == nil || byID.Name != "dev" {
			t.Fatalf("KeyByID returned %+v, want dev key", byID)
		}
		if got.CreatedAt.IsZero() {
			t.Error("CreatedAt is zero")
		}

		missing, err := s.KeyByHash("nope")
		if err != nil {
			t.Fatalf("KeyByHash(missing): %v", err)
		}
		if missing != nil {
			t.Error("KeyByHash(missing) != nil")
		}
		missingID, err := s.KeyByID(k.ID + 999)
		if err != nil {
			t.Fatalf("KeyByID(missing): %v", err)
		}
		if missingID != nil {
			t.Error("KeyByID(missing) != nil")
		}

		keys, err := s.ListKeys()
		if err != nil {
			t.Fatalf("ListKeys: %v", err)
		}
		if len(keys) != 1 {
			t.Fatalf("ListKeys len = %d, want 1", len(keys))
		}

		deleted, err := s.DeleteKey(k.ID)
		if err != nil || !deleted {
			t.Fatalf("DeleteKey = %v, %v", deleted, err)
		}
		deleted, _ = s.DeleteKey(k.ID)
		if deleted {
			t.Error("second DeleteKey reported deleted")
		}
	})
}

func TestDuplicateHashRejected(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		if _, err := s.CreateKey("a", "dup", "p", []string{"x"}, "first", "round_robin"); err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if _, err := s.CreateKey("b", "dup", "p", []string{"x"}, "first", "round_robin"); err == nil {
			t.Error("expected error inserting duplicate hash")
		}
	})
}

func TestLogsInsertAndQuery(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		base := time.Now()
		for i := 0; i < 3; i++ {
			err := s.InsertLog(&RequestLog{
				APIKeyID: 1, KeyName: "dev", Provider: "openai", Model: "gpt-5.4",
				StatusCode: 200, Streaming: i%2 == 0, TTFTMillis: int64(10 + i),
				DurationMillis: int64(100 + i), InputTokens: 10, OutputTokens: 5,
				Cost: 0.01, CreatedAt: base.Add(time.Duration(i) * time.Second),
			})
			if err != nil {
				t.Fatalf("InsertLog: %v", err)
			}
		}
		// Different key/provider row.
		if err := s.InsertLog(&RequestLog{APIKeyID: 2, KeyName: "ops", Provider: "anthropic", Model: "claude", StatusCode: 200}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}

		all, err := s.QueryLogs(LogFilter{})
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		if len(all) != 4 {
			t.Fatalf("QueryLogs len = %d, want 4", len(all))
		}
		// Newest first.
		if all[0].ID < all[1].ID {
			t.Error("logs not ordered newest-first")
		}

		byKey, err := s.QueryLogs(LogFilter{APIKeyID: 1})
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		if len(byKey) != 3 {
			t.Errorf("filtered by key len = %d, want 3", len(byKey))
		}

		byProv, _ := s.QueryLogs(LogFilter{Provider: "anthropic"})
		if len(byProv) != 1 {
			t.Errorf("filtered by provider len = %d, want 1", len(byProv))
		}

		limited, _ := s.QueryLogs(LogFilter{Limit: 2})
		if len(limited) != 2 {
			t.Errorf("limited len = %d, want 2", len(limited))
		}

		since, _ := s.QueryLogs(LogFilter{Since: base.Add(2 * time.Second)})
		if len(since) != 1 {
			t.Errorf("since filter len = %d, want 1", len(since))
		}
	})
}

func TestStats(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		for i := 0; i < 2; i++ {
			s.InsertLog(&RequestLog{APIKeyID: 1, KeyName: "dev", Provider: "openai", Model: "gpt-5.4",
				InputTokens: 100, OutputTokens: 50, Cost: 0.5})
		}
		s.InsertLog(&RequestLog{APIKeyID: 1, KeyName: "dev", Provider: "openai", Model: "gpt-5-mini",
			InputTokens: 10, OutputTokens: 5, Cost: 0.01})

		stats, err := s.Stats(LogFilter{})
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if len(stats) != 2 {
			t.Fatalf("Stats groups = %d, want 2", len(stats))
		}
		// Highest cost first -> gpt-5.4 aggregated.
		top := stats[0]
		if top.Model != "gpt-5.4" || top.Requests != 2 || top.InputTokens != 200 || top.Cost != 1.0 {
			t.Errorf("unexpected top stat: %+v", top)
		}
	})
}

func TestCreateKeyPersistsPolicy(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		k, err := s.CreateKey("dev", "h1", "p", []string{"a", "b"}, "random", "random")
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if k.ProviderStart != "random" || k.ProviderOrder != "random" {
			t.Errorf("returned policy = %q/%q, want random/random", k.ProviderStart, k.ProviderOrder)
		}
		got, err := s.KeyByHash("h1")
		if err != nil || got == nil {
			t.Fatalf("KeyByHash: %v (key=%v)", err, got)
		}
		if got.ProviderStart != "random" || got.ProviderOrder != "random" {
			t.Errorf("stored policy = %q/%q, want random/random", got.ProviderStart, got.ProviderOrder)
		}
	})
}

// TestLegacyDBUpgradesPolicyColumns is SQLite-only: it exercises the ensureColumn upgrade
// path that brings databases created by older versions up to the current schema. PostgreSQL
// has no legacy databases, so there is nothing to upgrade.
func TestLegacyDBUpgradesPolicyColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	// Seed an api_keys table without the policy columns, as an older version would.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	_, err = raw.Exec(`CREATE TABLE api_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, hash TEXT NOT NULL UNIQUE,
		prefix TEXT NOT NULL, providers TEXT NOT NULL, created_at INTEGER NOT NULL,
		disabled INTEGER NOT NULL DEFAULT 0);
		INSERT INTO api_keys (name, hash, prefix, providers, created_at) VALUES ('old','legacy','p','["a"]',0);`)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	k, err := s.KeyByHash("legacy")
	if err != nil || k == nil {
		t.Fatalf("KeyByHash: %v (key=%v)", err, k)
	}
	if k.ProviderStart != "first" || k.ProviderOrder != "round_robin" {
		t.Errorf("upgraded policy = %q/%q, want first/round_robin", k.ProviderStart, k.ProviderOrder)
	}
}

// TestLegacyDBUpgradesLogColumns is SQLite-only: it seeds a request_logs table from an older
// version (missing the request-metadata columns) and confirms ensureColumn adds them so the
// new fields insert and round-trip. PostgreSQL's matching ADD COLUMN IF NOT EXISTS path runs
// on every Open and is covered by the eachBackend round-trip tests.
func TestLegacyDBUpgradesLogColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-logs.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// An older request_logs without method/path/query/client_addr/user_agent/request_bytes/
	// response_bytes (only the columns that have never been additive).
	_, err = raw.Exec(`CREATE TABLE request_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, api_key_id INTEGER NOT NULL, key_name TEXT NOT NULL,
		provider TEXT NOT NULL, model TEXT NOT NULL, status_code INTEGER NOT NULL,
		streaming INTEGER NOT NULL, ttft_ms INTEGER NOT NULL, duration_ms INTEGER NOT NULL,
		input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
		cache_read_tokens INTEGER NOT NULL, cache_write_tokens INTEGER NOT NULL,
		cost REAL NOT NULL, error TEXT NOT NULL, created_at INTEGER NOT NULL);`)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// The new columns now exist, so a row carrying them inserts and reads back unchanged.
	if err := s.InsertLog(&RequestLog{
		APIKeyID: 1, KeyName: "dev", Provider: "openai", Model: "m", StatusCode: 200,
		Method: "POST", Path: "/v1/messages", Query: "x=1",
		ClientAddr: "203.0.113.7:443", UserAgent: "ua/2", RequestBytes: 7, ResponseBytes: 99,
	}); err != nil {
		t.Fatalf("InsertLog after upgrade: %v", err)
	}
	logs, err := s.QueryLogs(LogFilter{})
	if err != nil || len(logs) != 1 {
		t.Fatalf("QueryLogs: %v (n=%d)", err, len(logs))
	}
	got := logs[0]
	if got.Method != "POST" || got.Path != "/v1/messages" || got.Query != "x=1" ||
		got.ClientAddr != "203.0.113.7:443" || got.UserAgent != "ua/2" ||
		got.RequestBytes != 7 || got.ResponseBytes != 99 {
		t.Errorf("upgraded log round-trip mismatch: %+v", got)
	}
}
