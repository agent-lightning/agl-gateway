package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-lightning/agl-gateway/internal/config"
	"github.com/agent-lightning/agl-gateway/internal/store"
)

// These tests exercise the admin API against the committed portal fixture
// (testdata/portal-mock.db) — the same database used to debug/verify the portal by hand. They
// guard that the endpoints the portal relies on (keys, the final-only and all-attempts log
// lists, the single-log inspector with its attached failover attempts, the provider filter,
// and stats) keep working against a realistic, multi-attempt dataset.

const mockDBPath = "../../testdata/portal-mock.db"

// newAdminMock opens a *copy* of the committed fixture (so the test never mutates the tracked
// file or leaves WAL sidecars in testdata/) and returns a handler whose config knows the
// providers the fixture references, so provider-filter validation passes.
func newAdminMock(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	src, err := os.ReadFile(mockDBPath)
	if err != nil {
		t.Fatalf("read fixture %s: %v", mockDBPath, err)
	}
	dst := filepath.Join(t.TempDir(), "mock.db")
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	st, err := store.Open(dst)
	if err != nil {
		t.Fatalf("open fixture copy: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: master,
		Providers: []config.Provider{
			{Name: "openai", BaseURL: "http://openai"},
			{Name: "anthropic", BaseURL: "http://anthropic"},
			{Name: "azure", BaseURL: "http://azure"},
		},
	}
	return New(cfg, st, nil, nil, nil).Handler(), st
}

func TestMockDBKeys(t *testing.T) {
	h, _ := newAdminMock(t)

	rec := req(t, h, "GET", "/admin/keys", master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var list []store.APIKey
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]bool{"prod-app": true, "staging": true, "data-pipeline": true, "eval-harness": true}
	if len(list) != len(want) {
		t.Fatalf("got %d keys, want %d: %+v", len(list), len(want), list)
	}
	for _, k := range list {
		if !want[k.Name] {
			t.Errorf("unexpected key name %q", k.Name)
		}
		if k.Prefix == "" || len(k.Providers) == 0 {
			t.Errorf("key %q missing prefix/providers: %+v", k.Name, k)
		}
	}
}

// TestMockDBLogsFinalOnly checks the default listing returns only final attempts (the portal's
// default view), all carrying a trace id, and that all_attempts surfaces strictly more rows
// (the earlier failover siblings) including non-final ones.
func TestMockDBLogsFinalOnly(t *testing.T) {
	h, _ := newAdminMock(t)

	finalOnly := listLogsHelper(t, h, "/admin/logs?limit=1000")
	if len(finalOnly) == 0 {
		t.Fatal("no logs returned")
	}
	for _, l := range finalOnly {
		if !l.FinalAttempt {
			t.Errorf("log %d in default list is not a final attempt", l.ID)
		}
		if l.TraceID == 0 {
			t.Errorf("log %d has zero trace_id", l.ID)
		}
	}

	all := listLogsHelper(t, h, "/admin/logs?limit=1000&all_attempts=true")
	if len(all) <= len(finalOnly) {
		t.Fatalf("all_attempts (%d) should exceed final-only (%d)", len(all), len(finalOnly))
	}
	var nonFinal int
	for _, l := range all {
		if !l.FinalAttempt {
			nonFinal++
		}
	}
	if nonFinal == 0 {
		t.Error("all_attempts returned no non-final rows; fixture should contain failover siblings")
	}
}

// TestMockDBSingleLogAttempts finds a multi-attempt trace and verifies the single-log fetch
// attaches its earlier failover attempts (provider+status, no payloads), oldest first — the
// data the inspector drawer renders and lets the user click through to.
func TestMockDBSingleLogAttempts(t *testing.T) {
	h, _ := newAdminMock(t)

	all := listLogsHelper(t, h, "/admin/logs?limit=1000&all_attempts=true")
	byTrace := map[int64][]store.RequestLog{}
	for _, l := range all {
		byTrace[l.TraceID] = append(byTrace[l.TraceID], l)
	}
	var finalID int64
	for _, rows := range byTrace {
		if len(rows) > 1 {
			for _, l := range rows {
				if l.FinalAttempt {
					finalID = l.ID
				}
			}
			if finalID != 0 {
				break
			}
		}
	}
	if finalID == 0 {
		t.Fatal("no multi-attempt trace found in fixture")
	}

	rec := req(t, h, "GET", "/admin/logs/"+itoa(finalID), master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("getLog status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var full logWithAttempts
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !full.FinalAttempt || full.AttemptSeq < 2 {
		t.Fatalf("expected a final attempt with seq>=2, got seq=%d final=%v", full.AttemptSeq, full.FinalAttempt)
	}
	if len(full.Attempts) == 0 {
		t.Fatal("multi-attempt trace returned no earlier attempts")
	}
	for i, a := range full.Attempts {
		if a.FinalAttempt {
			t.Errorf("attempt %d is marked final; only earlier attempts belong here", i)
		}
		if a.TraceID != full.TraceID {
			t.Errorf("attempt %d trace_id %d != final %d", i, a.TraceID, full.TraceID)
		}
		if a.Provider == "" || a.StatusCode == 0 {
			t.Errorf("attempt %d missing provider/status: %+v", i, a)
		}
		if i > 0 && a.AttemptSeq <= full.Attempts[i-1].AttemptSeq {
			t.Errorf("attempts not ordered by seq: %d then %d", full.Attempts[i-1].AttemptSeq, a.AttemptSeq)
		}
	}
}

func TestMockDBProviderFilter(t *testing.T) {
	h, _ := newAdminMock(t)

	rows := listLogsHelper(t, h, "/admin/logs?limit=1000&all_attempts=true&provider=openai")
	if len(rows) == 0 {
		t.Fatal("no openai rows; fixture should route some traffic to openai")
	}
	for _, l := range rows {
		if l.Provider != "openai" {
			t.Errorf("provider filter leaked a %q row", l.Provider)
		}
	}

	// A provider not in the config is a fail-loud 400, not a silently dropped filter.
	if rec := req(t, h, "GET", "/admin/logs?provider=nope", master, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown provider status = %d, want 400", rec.Code)
	}
}

// TestMockDBSingleLogPayloads confirms the inspector fetch returns the captured request/response
// bodies for a successful row, and that the id round-trips as a JSON string (the portal relies
// on string ids to avoid JS precision loss).
func TestMockDBSingleLogPayloads(t *testing.T) {
	h, _ := newAdminMock(t)

	all := listLogsHelper(t, h, "/admin/logs?limit=1000")
	var withBody int64
	for _, l := range all {
		if l.StatusCode == 200 && l.RequestBytes > 0 {
			withBody = l.ID
			break
		}
	}
	if withBody == 0 {
		t.Fatal("no successful row with a captured body found")
	}

	rec := req(t, h, "GET", "/admin/logs/"+itoa(withBody), master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The id and trace_id are serialized as JSON strings.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	for _, field := range []string{"id", "trace_id"} {
		if b := raw[field]; len(b) == 0 || b[0] != '"' {
			t.Errorf("%s not serialized as a JSON string: %s", field, b)
		}
	}
	var full store.RequestLog
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if len(full.RawRequest) == 0 {
		t.Error("inspector fetch returned no raw_request payload")
	}
	if len(full.RawResponse) == 0 {
		t.Error("inspector fetch returned no raw_response payload")
	}
}

func TestMockDBStats(t *testing.T) {
	h, _ := newAdminMock(t)

	rec := req(t, h, "GET", "/admin/stats", master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var stats []store.Stat
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("stats empty; fixture should aggregate to non-empty rows")
	}
	var totalReq int64
	for _, s := range stats {
		if s.Requests <= 0 {
			t.Errorf("stat row has non-positive requests: %+v", s)
		}
		totalReq += s.Requests
	}
	if totalReq == 0 {
		t.Error("stats aggregate to zero requests")
	}
}

// listLogsHelper issues a GET against the logs endpoint and returns the decoded rows, failing
// the test on a non-200 or decode error.
func listLogsHelper(t *testing.T, h http.Handler, path string) []store.RequestLog {
	t.Helper()
	rec := req(t, h, "GET", path, master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, body=%s", path, rec.Code, rec.Body.String())
	}
	var resp logsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return resp.Logs
}
