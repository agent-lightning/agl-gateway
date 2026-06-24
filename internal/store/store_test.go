package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// eachBackend runs fn against every supported store backend combination available in the
// environment. SQLite (in-memory) always runs. When AGL_DATABASE is a PostgreSQL DSN the
// PostgreSQL keys backend is added, and when AGL_CLICKHOUSE is a ClickHouse DSN the ClickHouse
// request_logs backend is added — yielding up to four combinations:
//
//	sqlite               — keys + logs in in-memory SQLite
//	postgres             — keys + logs in PostgreSQL
//	sqlite+clickhouse    — keys in SQLite, logs in ClickHouse
//	postgres+clickhouse  — keys in PostgreSQL, logs in ClickHouse
//
// AGL_DATABASE/AGL_CLICKHOUSE are the same overrides the gateway honors at runtime, so the
// tests select backends exactly as a real deployment would. The portable cases below assert
// only on relative ids and counts, so every combination must satisfy them identically; each
// run starts from a clean slate (TRUNCATE … RESTART IDENTITY on PostgreSQL so BIGSERIAL ids
// restart at 1, matching SQLite's fresh in-memory AUTOINCREMENT; TRUNCATE on ClickHouse).
func eachBackend(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()

	pgDSN := os.Getenv("AGL_DATABASE")
	if !strings.HasPrefix(pgDSN, "postgres://") && !strings.HasPrefix(pgDSN, "postgresql://") {
		if pgDSN != "" {
			t.Logf("AGL_DATABASE=%q is not a postgres DSN; skipping postgres backends", pgDSN)
		} else {
			t.Log("AGL_DATABASE is unset; skipping postgres backends")
		}
		pgDSN = ""
	}
	chDSN := os.Getenv("AGL_CLICKHOUSE")
	if !strings.HasPrefix(chDSN, "clickhouse://") && !strings.HasPrefix(chDSN, "clickhouses://") {
		if chDSN != "" {
			t.Logf("AGL_CLICKHOUSE=%q is not a clickhouse DSN; skipping clickhouse backends", chDSN)
		} else {
			t.Log("AGL_CLICKHOUSE is unset; skipping clickhouse backends")
		}
		chDSN = ""
	}

	run := func(name, database, logsDatabase string) {
		t.Run(name, func(t *testing.T) {
			s, err := OpenWithLogs(database, logsDatabase)
			if err != nil {
				t.Fatalf("OpenWithLogs(%q, %q): %v", database, logsDatabase, err)
			}
			t.Cleanup(func() { s.Close() })
			// Clean the keys/logs tables so the relative-count assertions start from empty. The
			// keys backend (s.db) is PostgreSQL only when database is a postgres DSN; the logs
			// backend (s.logs) is ClickHouse when logsDatabase is set.
			if strings.HasPrefix(database, "postgres://") || strings.HasPrefix(database, "postgresql://") {
				if _, err := s.db.Exec(`TRUNCATE request_logs, api_keys RESTART IDENTITY`); err != nil {
					t.Fatalf("truncate postgres: %v", err)
				}
			}
			if s.logs != nil {
				if _, err := s.logs.db.Exec(`TRUNCATE TABLE IF EXISTS request_logs`); err != nil {
					t.Fatalf("truncate clickhouse: %v", err)
				}
			}
			fn(t, s)
		})
	}

	run("sqlite", ":memory:", "")
	if pgDSN != "" {
		run("postgres", pgDSN, "")
	}
	if chDSN != "" {
		run("sqlite+clickhouse", ":memory:", chDSN)
	}
	if pgDSN != "" && chDSN != "" {
		run("postgres+clickhouse", pgDSN, chDSN)
	}
}

// TestLogsBackendSelectable confirms logs_database may be set explicitly to SQLite or
// PostgreSQL — not only ClickHouse. OpenWithLogs routes request_logs to whatever backend the
// primary accepts (here keys live in in-memory SQLite, logs in a separate backend), so a log
// written through the Store lands in, and reads back from, that separate logs backend. The
// PostgreSQL case runs only when AGL_DATABASE points at one.
func TestLogsBackendSelectable(t *testing.T) {
	const sentinel = 778899 // a distinctive api_key_id so a shared logs table isn't ambiguous
	check := func(t *testing.T, logsDatabase string) {
		s, err := OpenWithLogs(":memory:", logsDatabase)
		if err != nil {
			t.Fatalf("OpenWithLogs(\":memory:\", %q): %v", logsDatabase, err)
		}
		t.Cleanup(func() { s.Close() })
		if s.logs == nil {
			t.Fatal("expected a separate logs backend to be attached")
		}
		// A shared logs backend (PostgreSQL) persists across runs, so clear any sentinel rows a
		// prior run left before asserting an exact count.
		if err := s.logs.d.deleteLogsByKey(s.logs.db, sentinel); err != nil {
			t.Fatalf("clear logs: %v", err)
		}
		if err := s.InsertLog(&RequestLog{APIKeyID: sentinel, KeyName: "dev", Provider: "openai", Model: "m", StatusCode: 200}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
		logs, err := s.QueryLogs(LogFilter{APIKeyID: sentinel})
		if err != nil {
			t.Fatalf("QueryLogs: %v", err)
		}
		if len(logs) != 1 || logs[0].Provider != "openai" {
			t.Fatalf("logs = %+v, want one openai row", logs)
		}
	}

	t.Run("sqlite", func(t *testing.T) {
		check(t, filepath.Join(t.TempDir(), "logs.db"))
	})

	pg := os.Getenv("AGL_DATABASE")
	if strings.HasPrefix(pg, "postgres://") || strings.HasPrefix(pg, "postgresql://") {
		t.Run("postgres", func(t *testing.T) { check(t, pg) })
	}
}

// TestClickHouseRejectedAsPrimary asserts the store refuses ClickHouse as the keys backend —
// it has no auto-increment, no enforced UNIQUE, and only async mutations, so it cannot own
// api_keys. ClickHouse is only valid as a logs backend (logs_database / the second arg to
// OpenWithLogs). The rejection is a URL-prefix check, so this needs no running server.
func TestClickHouseRejectedAsPrimary(t *testing.T) {
	const ch = "clickhouse://localhost:9000/db"
	if _, err := Open(ch); err == nil {
		t.Error("Open(clickhouse://) should be rejected as a keys backend")
	}
	if _, err := OpenWithLogs(ch, ""); err == nil {
		t.Error("OpenWithLogs with a clickhouse primary should be rejected")
	}
	if _, err := OpenWithLogs(ch, "clickhouse://localhost:9000/logs"); err == nil {
		t.Error("OpenWithLogs with a clickhouse primary should be rejected even with a logs DSN")
	}
}

// TestIDGenMonotonic asserts the ClickHouse client-side id generator yields strictly
// increasing ids — including when the clock does not advance between calls (the tight loop
// far outpaces the millisecond tick, so the counter-bump branch dominates). This is what
// keeps request_logs ORDER BY id DESC newest-first on a backend without auto-increment.
func TestIDGenMonotonic(t *testing.T) {
	g := &idGen{}
	prev := int64(0)
	for i := 0; i < 100_000; i++ {
		id := g.next()
		if id <= prev {
			t.Fatalf("id not strictly increasing at %d: got %d after %d", i, id, prev)
		}
		prev = id
	}
}

func TestDeleteKeyCascadesLogs(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		k, err := s.CreateKey("dev", "h", "p", []string{"openai"}, "first", "round_robin", false)
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

// A key created with keepLogsOnDelete=true is removed without touching its logs, which stay
// queryable (orphaned) under their captured key name.
func TestDeleteKeyKeepsLogsWhenConfigured(t *testing.T) {
	eachBackend(t, func(t *testing.T, s *Store) {
		k, err := s.CreateKey("keep", "h", "p", []string{"openai"}, "first", "round_robin", true)
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if !k.KeepLogsOnDelete {
			t.Fatalf("KeepLogsOnDelete not set on returned key")
		}
		if got, _ := s.KeyByID(k.ID); got == nil || !got.KeepLogsOnDelete {
			t.Fatalf("KeepLogsOnDelete not round-tripped via KeyByID: %+v", got)
		}
		for i := 0; i < 3; i++ {
			if err := s.InsertLog(&RequestLog{APIKeyID: k.ID, KeyName: "keep", Provider: "openai", Model: "m"}); err != nil {
				t.Fatalf("InsertLog: %v", err)
			}
		}
		deleted, err := s.DeleteKey(k.ID)
		if err != nil {
			t.Fatalf("DeleteKey: %v", err)
		}
		if !deleted {
			t.Fatalf("DeleteKey reported no row deleted")
		}
		// The key is gone but its logs are retained, still carrying the captured key name.
		if got, _ := s.KeyByID(k.ID); got != nil {
			t.Errorf("key not deleted: %+v", got)
		}
		kept, _ := s.QueryLogs(LogFilter{APIKeyID: k.ID})
		if len(kept) != 3 {
			t.Errorf("expected 3 retained logs, got %d", len(kept))
		}
		for _, l := range kept {
			if l.KeyName != "keep" {
				t.Errorf("retained log lost its key-name snapshot: %q", l.KeyName)
			}
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
		k, err := s.CreateKey("dev", "hash-abc", "sk-gw-ab", []string{"openai", "anthropic"}, "first", "round_robin", false)
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
		if _, err := s.CreateKey("a", "dup", "p", []string{"x"}, "first", "round_robin", false); err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		if _, err := s.CreateKey("b", "dup", "p", []string{"x"}, "first", "round_robin", false); err == nil {
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
		k, err := s.CreateKey("dev", "h1", "p", []string{"a", "b"}, "random", "random", false)
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
