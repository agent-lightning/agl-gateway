// Package admin exposes the master-key-protected control plane: API key CRUD plus log and
// usage-stat queries. It is mounted under /admin/.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kiki/agl-gateway/internal/config"
	"github.com/kiki/agl-gateway/internal/keys"
	"github.com/kiki/agl-gateway/internal/store"
)

// Admin is the control-plane HTTP handler set.
type Admin struct {
	cfg    *config.Config
	store  *store.Store
	logger *slog.Logger
}

// New builds an Admin handler.
func New(cfg *config.Config, st *store.Store, logger *slog.Logger) *Admin {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Admin{cfg: cfg, store: st, logger: logger}
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

func (a *Admin) providers(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(a.cfg.Providers))
	for _, p := range a.cfg.Providers {
		names = append(names, p.Name)
	}
	writeJSON(w, http.StatusOK, names)
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
