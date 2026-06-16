package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agent-lightning/agl-gateway/internal/probe"
)

// fakeGateway stands in for a running agl-gateway: the control-plane endpoints modelcheck
// needs plus the data-plane probe endpoints, recording the pinned provider header.
func fakeGateway(t *testing.T, deleted *bool, sawProvider *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/providers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"openai","models":["gpt-5.4"]},{"name":"anthropic","models":["claude-opus-4-8"]}]`))
	})
	mux.HandleFunc("POST /admin/keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":42,"key":"sk-gw-temp"}`))
	})
	mux.HandleFunc("DELETE /admin/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "42" {
			*deleted = true
		}
		w.WriteHeader(http.StatusNoContent)
	})
	probeHandler := func(w http.ResponseWriter, r *http.Request) {
		*sawProvider = r.Header.Get("X-AGL-Provider")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-AGL-Attempts", "1")
		w.Write([]byte(`{"usage":{"input_tokens":4,"output_tokens":1}}`))
	}
	mux.HandleFunc("POST /v1/responses", probeHandler)
	mux.HandleFunc("POST /v1/messages", probeHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestControlPlaneHelpers(t *testing.T) {
	var deleted bool
	var sawProvider string
	srv := fakeGateway(t, &deleted, &sawProvider)
	client := &http.Client{Timeout: 2 * time.Second}

	provs, err := fetchProviders(client, srv.URL, "mk")
	if err != nil {
		t.Fatalf("fetchProviders: %v", err)
	}
	if len(provs) != 2 || provs[0].Name != "openai" || provs[1].Models[0] != "claude-opus-4-8" {
		t.Fatalf("providers = %+v", provs)
	}

	id, key, err := createKey(client, srv.URL, "mk", []string{"openai", "anthropic"})
	if err != nil || id != 42 || key != "sk-gw-temp" {
		t.Fatalf("createKey = (%d,%q,%v)", id, key, err)
	}
	if err := deleteKey(client, srv.URL, "mk", id); err != nil {
		t.Fatalf("deleteKey: %v", err)
	}
	if !deleted {
		t.Error("deleteKey did not reach the gateway")
	}
}

func TestGatewaySenderProbe(t *testing.T) {
	var deleted bool
	var sawProvider string
	srv := fakeGateway(t, &deleted, &sawProvider)
	send := gatewaySender(srv.Client(), srv.URL, "sk-gw-temp")

	// Non-claude model -> /v1/responses, provider pinned, usage summarized.
	r := probe.Probe(context.Background(), probe.Job{Provider: "openai", Model: "gpt-5.4"}, "", 16, false, send)
	if r.Endpoint != "/v1/responses" || !r.OK || r.Detail != "in=4 out=1" {
		t.Errorf("responses probe = %+v", r)
	}
	if sawProvider != "openai" {
		t.Errorf("gateway saw X-AGL-Provider = %q, want openai", sawProvider)
	}

	// Claude model -> /v1/messages.
	r = probe.Probe(context.Background(), probe.Job{Provider: "anthropic", Model: "claude-opus-4-8"}, "", 16, false, send)
	if r.Endpoint != "/v1/messages" || sawProvider != "anthropic" {
		t.Errorf("messages probe = %+v, sawProvider=%q", r, sawProvider)
	}
}
