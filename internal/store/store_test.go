package store

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKeyLifecycle(t *testing.T) {
	s := newTestStore(t)

	k, err := s.CreateKey("dev", "hash-abc", "sk-gw-ab", []string{"openai", "anthropic"})
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
}

func TestDuplicateHashRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateKey("a", "dup", "p", []string{"x"}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := s.CreateKey("b", "dup", "p", []string{"x"}); err == nil {
		t.Error("expected error inserting duplicate hash")
	}
}

func TestLogsInsertAndQuery(t *testing.T) {
	s := newTestStore(t)
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
}

func TestStats(t *testing.T) {
	s := newTestStore(t)
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
}
