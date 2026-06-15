package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/agent-lightning/agl-gateway/internal/config"
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
	return New(cfg, st, nil).Handler(), st
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
	var logs []store.RequestLog
	json.Unmarshal(rec.Body.Bytes(), &logs)
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}

	rec = req(t, h, "GET", "/admin/stats", master, "")
	var stats []store.Stat
	json.Unmarshal(rec.Body.Bytes(), &stats)
	if len(stats) != 1 || stats[0].Cost != 0.5 {
		t.Fatalf("stats = %+v", stats)
	}

	rec = req(t, h, "GET", "/admin/providers", master, "")
	var provs []string
	json.Unmarshal(rec.Body.Bytes(), &provs)
	if len(provs) != 2 {
		t.Errorf("providers = %+v, want 2", provs)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
