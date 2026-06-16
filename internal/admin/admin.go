// Package admin exposes the master-key-protected control plane: API key CRUD plus log and
// usage-stat queries. It is mounted under /admin/.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-lightning/agl-gateway/internal/config"
	"github.com/agent-lightning/agl-gateway/internal/keys"
	"github.com/agent-lightning/agl-gateway/internal/store"
)

// Admin is the control-plane HTTP handler set.
type Admin struct {
	cfg    *config.Config
	store  *store.Store
	client *http.Client
	logger *slog.Logger
}

// New builds an Admin handler. A nil client uses a sensible default; a nil logger discards
// logs. The client is used only to discover provider models from upstream /v1/models.
func New(cfg *config.Config, st *store.Store, client *http.Client, logger *slog.Logger) *Admin {
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Admin{cfg: cfg, store: st, client: client, logger: logger}
}

// Handler returns the routed, master-key-authenticated control-plane handler.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/keys", a.createKey)
	mux.HandleFunc("GET /admin/keys", a.listKeys)
	mux.HandleFunc("DELETE /admin/keys/{id}", a.deleteKey)
	mux.HandleFunc("GET /admin/logs", a.listLogs)
	mux.HandleFunc("GET /admin/stats", a.stats)
	mux.HandleFunc("GET /admin/providers", a.providers)
	return a.authMiddleware(mux)
}

// authMiddleware enforces the master key on every control-plane request.
func (a *Admin) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if presented == "" {
			presented = strings.TrimSpace(r.Header.Get("X-Master-Key"))
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(a.cfg.MasterKey)) != 1 {
			writeJSON(w, http.StatusUnauthorized, errBody("invalid master key"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type createKeyRequest struct {
	Name      string   `json:"name"`
	Providers []string `json:"providers"`
}

type createKeyResponse struct {
	store.APIKey
	Key string `json:"key"` // plaintext, returned only once
}

func (a *Admin) createKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, errBody("name is required"))
		return
	}
	if len(req.Providers) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody("at least one provider is required"))
		return
	}
	for _, name := range req.Providers {
		if a.cfg.Provider(name) == nil {
			writeJSON(w, http.StatusBadRequest, errBody("unknown provider: "+name))
			return
		}
	}

	plain, display, err := keys.Generate()
	if err != nil {
		a.logger.Error("key generation failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("could not generate key"))
		return
	}
	k, err := a.store.CreateKey(req.Name, keys.Hash(plain), display, req.Providers)
	if err != nil {
		a.logger.Error("create key failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("could not store key"))
		return
	}
	writeJSON(w, http.StatusCreated, createKeyResponse{APIKey: *k, Key: plain})
}

func (a *Admin) listKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := a.store.ListKeys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("could not list keys"))
		return
	}
	if ks == nil {
		ks = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, ks)
}

func (a *Admin) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid key id"))
		return
	}
	deleted, err := a.store.DeleteKey(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("could not delete key"))
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, errBody("key not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Admin) listLogs(w http.ResponseWriter, r *http.Request) {
	f := store.LogFilter{
		APIKeyID: queryInt64(r, "api_key_id"),
		Provider: r.URL.Query().Get("provider"),
		Limit:    int(queryInt64(r, "limit")),
		Offset:   int(queryInt64(r, "offset")),
		Since:    querySince(r),
	}
	logs, err := a.store.QueryLogs(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("could not query logs"))
		return
	}
	if logs == nil {
		logs = []store.RequestLog{}
	}
	writeJSON(w, http.StatusOK, logs)
}

func (a *Admin) stats(w http.ResponseWriter, r *http.Request) {
	f := store.LogFilter{APIKeyID: queryInt64(r, "api_key_id"), Since: querySince(r)}
	st, err := a.store.Stats(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("could not query stats"))
		return
	}
	if st == nil {
		st = []store.Stat{}
	}
	writeJSON(w, http.StatusOK, st)
}

// providerModelsTimeout bounds how long the providers endpoint waits on all upstream
// /v1/models probes combined.
const providerModelsTimeout = 15 * time.Second

// providerInfo is one entry of the GET /admin/providers response: a configured provider and
// the models it exposes. Error is set (and Models may be empty) when the model probe failed.
type providerInfo struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
	Error  string   `json:"error,omitempty"`
}

// modelList is the OpenAI-/Anthropic-compatible shape of a /v1/models response; only the
// model id is needed.
type modelList struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// providers lists the configured providers and, best-effort, the models each exposes.
// Models are discovered live from every provider's OpenAI-compatible /v1/models endpoint
// (probed concurrently) and unioned with the provider's model_map aliases, which are also
// valid requestable models. A probe failure is reported per provider, never fatally.
func (a *Admin) providers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), providerModelsTimeout)
	defer cancel()

	out := make([]providerInfo, len(a.cfg.Providers))
	var wg sync.WaitGroup
	for i := range a.cfg.Providers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := &a.cfg.Providers[i]
			info := providerInfo{Name: p.Name, Models: []string{}}

			models, err := a.fetchModels(ctx, p)
			if err != nil {
				info.Error = err.Error()
				a.logger.Warn("provider model discovery failed", "provider", p.Name, "err", err)
			}

			// Union discovered models with model_map aliases, deduped and sorted.
			set := make(map[string]bool, len(models)+len(p.ModelMap))
			for _, m := range models {
				set[m] = true
			}
			for alias := range p.ModelMap {
				set[alias] = true
			}
			for m := range set {
				info.Models = append(info.Models, m)
			}
			sort.Strings(info.Models)
			out[i] = info
		}(i)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, out)
}

// fetchModels queries a provider's OpenAI-compatible /v1/models endpoint, applying the same
// upstream auth the proxy uses, and returns the model ids it lists. It is best-effort: any
// transport, status, or parse failure is returned as an error for the caller to report.
func (a *Admin) fetchModels(ctx context.Context, p *config.Provider) ([]string, error) {
	target := strings.TrimRight(p.BaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/v1/models returned HTTP %d", resp.StatusCode)
	}

	var ml modelList
	if err := json.Unmarshal(body, &ml); err != nil {
		return nil, fmt.Errorf("parse /v1/models: %w", err)
	}
	ids := make([]string, 0, len(ml.Data))
	for _, m := range ml.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errBody(msg string) any { return map[string]any{"error": map[string]string{"message": msg}} }

func queryInt64(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return n
}

// querySince accepts an RFC3339 timestamp, a Go duration window (e.g. "24h"), or unix
// milliseconds. It returns the zero time when absent or unparseable.
func querySince(r *http.Request) time.Time {
	v := strings.TrimSpace(r.URL.Query().Get("since"))
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if d, err := time.ParseDuration(v); err == nil {
		return time.Now().Add(-d)
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}
