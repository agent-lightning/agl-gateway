package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-lightning/agl-gateway/internal/config"
	"github.com/agent-lightning/agl-gateway/internal/probe"
	"github.com/agent-lightning/agl-gateway/internal/store"
)

const master = "mk-secret"

func newAdmin(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: master,
		Providers: []config.Provider{{Name: "openai", BaseURL: "http://x"}, {Name: "anthropic", BaseURL: "http://y"}},
	}
	return New(cfg, st, nil, nil, nil).Handler(), st
}

func req(t *testing.T, h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestAuthRequired(t *testing.T) {
	h, _ := newAdmin(t)
	if rec := req(t, h, "GET", "/admin/keys", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key status = %d, want 401", rec.Code)
	}
	if rec := req(t, h, "GET", "/admin/keys", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key status = %d, want 401", rec.Code)
	}
	if rec := req(t, h, "GET", "/admin/keys", master, ""); rec.Code != http.StatusOK {
		t.Errorf("good key status = %d, want 200", rec.Code)
	}
}

func TestCreateListDeleteKey(t *testing.T) {
	h, _ := newAdmin(t)

	rec := req(t, h, "POST", "/admin/keys", master, `{"name":"dev","providers":["openai"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created createKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Key == "" || created.ID == 0 {
		t.Fatalf("missing key/id: %+v", created)
	}
	// Omitted policy falls back to the defaults.
	if created.ProviderStart != "first" || created.ProviderOrder != "round_robin" {
		t.Errorf("default policy = %q/%q, want first/round_robin", created.ProviderStart, created.ProviderOrder)
	}

	// List omits the secret but includes the key.
	rec = req(t, h, "GET", "/admin/keys", master, "")
	var list []store.APIKey
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "dev" {
		t.Fatalf("list = %+v", list)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(created.Key)) {
		t.Error("plaintext key leaked in list response")
	}

	// Delete.
	rec = req(t, h, "DELETE", "/admin/keys/"+itoa(created.ID), master, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	rec = req(t, h, "DELETE", "/admin/keys/"+itoa(created.ID), master, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("re-delete status = %d, want 404", rec.Code)
	}
}

func TestCreateKeyValidation(t *testing.T) {
	h, _ := newAdmin(t)
	cases := map[string]string{
		"empty name":       `{"name":"","providers":["openai"]}`,
		"no providers":     `{"name":"x","providers":[]}`,
		"unknown provider": `{"name":"x","providers":["ghost"]}`,
		"bad json":         `{`,
		"bad start":        `{"name":"x","providers":["openai"],"provider_start":"nope"}`,
		"bad order":        `{"name":"x","providers":["openai"],"provider_order":"nope"}`,
	}
	for name, body := range cases {
		if rec := req(t, h, "POST", "/admin/keys", master, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestLogsAndStats(t *testing.T) {
	h, st := newAdmin(t)
	st.InsertLog(&store.RequestLog{APIKeyID: 1, KeyName: "dev", Provider: "openai", Model: "gpt-5.4",
		StatusCode: 200, InputTokens: 100, OutputTokens: 50, Cost: 0.5})

	rec := req(t, h, "GET", "/admin/logs", master, "")
	var page logsResponse
	json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(page.Logs))
	}

	rec = req(t, h, "GET", "/admin/stats", master, "")
	var stats []store.Stat
	json.Unmarshal(rec.Body.Bytes(), &stats)
	if len(stats) != 1 || stats[0].Cost != 0.5 {
		t.Fatalf("stats = %+v", stats)
	}

	rec = req(t, h, "GET", "/admin/providers", master, "")
	var provs []providerInfo
	json.Unmarshal(rec.Body.Bytes(), &provs)
	if len(provs) != 2 {
		t.Errorf("providers = %+v, want 2", provs)
	}
}

func TestLogsPaginateByKey(t *testing.T) {
	h, st := newAdmin(t)
	k, err := st.CreateKey("dev", "hash-dev", "sk-gw-dev", []string{"openai"}, "first", "round_robin")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	other, err := st.CreateKey("ops", "hash-ops", "sk-gw-ops", []string{"anthropic"}, "first", "round_robin")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := st.InsertLog(&store.RequestLog{
			APIKeyID: k.ID, KeyName: "dev", Provider: "openai", Model: "gpt-5.4",
			StatusCode: 200, RawRequest: []byte(`{"page":true}`),
		}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	if err := st.InsertLog(&store.RequestLog{APIKeyID: other.ID, KeyName: "ops", Provider: "anthropic", Model: "claude", StatusCode: 200}); err != nil {
		t.Fatalf("InsertLog(other): %v", err)
	}

	rec := req(t, h, "GET", "/admin/logs?api_key_id="+itoa(k.ID)+"&limit=2", master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var page logsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.APIKey == nil || page.APIKey.ID != k.ID || page.APIKey.Name != "dev" {
		t.Fatalf("api key = %+v, want dev key", page.APIKey)
	}
	if page.Limit != 2 || page.Offset != 0 || page.NextOffset != 2 || !page.HasMore {
		t.Fatalf("page metadata = limit:%d offset:%d next:%d has_more:%v", page.Limit, page.Offset, page.NextOffset, page.HasMore)
	}
	if len(page.Logs) != 2 {
		t.Fatalf("logs = %d, want 2", len(page.Logs))
	}
	for _, l := range page.Logs {
		if l.APIKeyID != k.ID {
			t.Errorf("logs included api_key_id=%d, want %d", l.APIKeyID, k.ID)
		}
	}

	rec = req(t, h, "GET", "/admin/logs?api_key_id="+itoa(k.ID)+"&limit=2&offset=2", master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second page status = %d, body=%s", rec.Code, rec.Body.String())
	}
	page = logsResponse{}
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.HasMore || page.NextOffset != 0 || len(page.Logs) != 1 {
		t.Fatalf("second page = %+v", page)
	}
}

func TestLogsPayloadsOptIn(t *testing.T) {
	h, st := newAdmin(t)
	k, err := st.CreateKey("dev", "hash-dev", "sk-gw-dev", []string{"openai"}, "first", "round_robin")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := st.InsertLog(&store.RequestLog{
		APIKeyID: k.ID, KeyName: "dev", Provider: "openai", Model: "gpt-5.4", StatusCode: 200,
		RawRequest: []byte(`{"secret":true}`), RawResponse: []byte("data: x\n\n"),
	}); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}

	// Default: payload blobs are omitted.
	rec := req(t, h, "GET", "/admin/logs?api_key_id="+itoa(k.ID), master, "")
	var page logsResponse
	json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(page.Logs))
	}
	if page.Logs[0].RawRequest != nil || page.Logs[0].RawResponse != nil {
		t.Errorf("payloads should be omitted by default: req=%q resp=%q", page.Logs[0].RawRequest, page.Logs[0].RawResponse)
	}

	// Opt-in: payload blobs are returned.
	rec = req(t, h, "GET", "/admin/logs?api_key_id="+itoa(k.ID)+"&include_payloads=true", master, "")
	page = logsResponse{}
	json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Logs) != 1 || string(page.Logs[0].RawRequest) != `{"secret":true}` || string(page.Logs[0].RawResponse) != "data: x\n\n" {
		t.Errorf("include_payloads did not return payloads: %+v", page.Logs)
	}
}

func TestLogsMissingKeyReturnsEmpty(t *testing.T) {
	h, _ := newAdmin(t)
	rec := req(t, h, "GET", "/admin/logs?api_key_id=404", master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var page logsResponse
	json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Logs) != 0 || page.APIKey != nil || page.HasMore {
		t.Fatalf("missing-key page = %+v, want empty with no api_key", page)
	}
}

func TestProvidersDiscoversModels(t *testing.T) {
	// One upstream serves an OpenAI-style /v1/models list; assert it is discovered, unioned
	// with the provider's model_map aliases, deduped, and sorted. The second provider is
	// unreachable and must report an error without failing the request.
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4"},{"id":"gpt-5-mini"}]}`))
	})
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: master,
		Providers: []config.Provider{
			{Name: "openai", BaseURL: srv.URL, ModelMap: map[string]string{"gpt-fast": "gpt-5-mini"}},
			{Name: "dead", BaseURL: "http://127.0.0.1:1"},
		},
	}
	h := New(cfg, st, &http.Client{Timeout: 2 * time.Second}, nil, nil).Handler()

	rec := req(t, h, "GET", "/admin/providers", master, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var provs []providerInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &provs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]providerInfo{}
	for _, p := range provs {
		byName[p.Name] = p
	}
	openai := byName["openai"]
	want := []string{"gpt-5-mini", "gpt-5.4", "gpt-fast"} // discovered ∪ alias, sorted
	if openai.Error != "" {
		t.Errorf("openai unexpected error: %q", openai.Error)
	}
	if strings.Join(openai.Models, ",") != strings.Join(want, ",") {
		t.Errorf("openai models = %v, want %v", openai.Models, want)
	}
	if dead := byName["dead"]; dead.Error == "" {
		t.Errorf("dead provider should report a probe error, got %+v", dead)
	}
}

func TestModelTestEndpoint(t *testing.T) {
	// Upstream advertises three models; gpt-image-1 must be excluded by default.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"id":"gpt-5.4"},{"id":"gpt-image-1"},{"id":"claude-opus-4-8"}]}`))
	}))
	t.Cleanup(up.Close)

	// Fake data plane: assert each probe is authenticated and provider-pinned, echo usage.
	var mu sync.Mutex
	seenProviders := map[string]bool{}
	var sawAuth bool
	dataPlane := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenProviders[r.Header.Get("X-AGL-Provider")] = true
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			sawAuth = true
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-AGL-Attempts", "1")
		w.Write([]byte(`{"usage":{"input_tokens":4,"output_tokens":1}}`))
	})

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		MasterKey: master,
		Providers: []config.Provider{{Name: "p", BaseURL: up.URL}},
	}
	h := New(cfg, st, &http.Client{Timeout: 2 * time.Second}, dataPlane, nil).Handler()

	rec := req(t, h, "POST", "/admin/test", master, `{"exclude":"gpt-image*","concurrency":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content-type = %q, want application/x-ndjson", ct)
	}

	// Parse the newline-delimited event stream.
	var start, done *testEvent
	results := map[string]probe.Result{}
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		var ev testEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		switch ev.Type {
		case "start":
			e := ev
			start = &e
		case "result":
			results[ev.Result.Model] = *ev.Result
		case "done":
			e := ev
			done = &e
		default:
			t.Errorf("unexpected event type %q", ev.Type)
		}
	}
	if start == nil || done == nil {
		t.Fatalf("missing start/done events (start=%v done=%v)", start, done)
	}
	if start.Total != 2 || start.Skipped != 1 {
		t.Errorf("start = %+v, want total=2 skipped=1", start)
	}
	if done.Passed != 2 || done.Failed != 0 || done.Skipped != 1 || len(results) != 2 {
		t.Fatalf("done = %+v, results=%d", done, len(results))
	}
	// Endpoints chosen per model.
	if results["claude-opus-4-8"].Endpoint != "/v1/messages" {
		t.Errorf("claude endpoint = %q", results["claude-opus-4-8"].Endpoint)
	}
	if results["gpt-5.4"].Endpoint != "/v1/responses" {
		t.Errorf("gpt endpoint = %q", results["gpt-5.4"].Endpoint)
	}
	if results["gpt-5.4"].Detail != "in=4 out=1" || results["gpt-5.4"].Attempts != "1" {
		t.Errorf("gpt result = %+v", results["gpt-5.4"])
	}
	if !sawAuth || !seenProviders["p"] {
		t.Errorf("data plane auth=%v providers=%v", sawAuth, seenProviders)
	}

	// The temporary key must be cleaned up.
	ks, _ := st.ListKeys()
	if len(ks) != 0 {
		t.Errorf("temporary key not deleted: %+v", ks)
	}
}

func TestModelTestUnavailableWithoutDataPlane(t *testing.T) {
	h, _ := newAdmin(t) // newAdmin passes a nil data plane
	rec := req(t, h, "POST", "/admin/test", master, `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
