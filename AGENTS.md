# AGENTS.md

Guidance for AI agents and contributors working in this repository.

## What this is

`agl-gateway` is a minimalistic, **provider-agnostic** LLM gateway. The defining
constraint: it routes purely by inbound **API key**, and it must **not** grow knowledge of
specific endpoint shapes (`/v1/chat/completions`, `/v1/responses`, `/v1/messages`). It
forwards the request path/body verbatim and extracts metadata on a *best-effort* basis.
Keep that boundary intact — endpoint-specific logic does not belong here.

## Project principles

- **Minimalistic.** Standard library first. The only third-party dependencies are
  `gopkg.in/yaml.v3` and `modernc.org/sqlite` (pure-Go, no cgo). Don't add dependencies or
  frameworks without a strong reason.
- **Robust over clever.** Failures degrade gracefully: unknown models still log (zero
  cost), unparseable usage still logs (zero tokens), a dead provider returns 502 and is
  recorded.
- **Well tested.** Every package has tests. Add/extend tests with any behavior change;
  `go test ./...` and `go vet ./...` must stay green.
- **Best-effort metering, never blocking.** Metadata extraction must never break or delay
  the proxied response. The client gets bytes first; logging happens after.

## Architecture (data flow)

```
client ──► proxy ──┐ auth (sha256 key lookup) ──► pick random bound provider
                   ├─ retry loop (backoff+jitter on net err / 429 / 5xx)
                   └─ stream upstream→client (SSE flushed), tee into usage.Accumulator
                                                  └─► pricing.Cost ─► store.InsertLog
```

Packages:

| Package           | Responsibility |
|-------------------|----------------|
| `internal/config` | YAML load, defaults, validation. |
| `internal/pricing`| Normalized `Usage` → dollar cost. |
| `internal/store`  | SQLite schema + `api_keys`/`request_logs` access. |
| `internal/usage`  | Best-effort model + usage extraction (OpenAI/Anthropic, JSON + SSE). |
| `internal/keys`   | Mint + SHA-256-hash gateway keys. |
| `internal/proxy`  | Data plane: auth, routing, retry, streaming, metering. |
| `internal/admin`  | Master-key control plane (`/admin/*`). |
| `internal/portal` | Embedded management/inspection SPA. |
| `internal/server` | Top-level HTTP routing. |
| `cmd/gateway`     | Entrypoint. |

## Key invariants — do not break these

- **Never retry after the client has received bytes.** The retry loop runs entirely before
  the response is streamed. If you touch `proxy.forward`, preserve this.
- **Usage buckets are non-overlapping.** `Usage{Input, Output, CacheRead, CacheWrite}` must
  not double-count. OpenAI cached tokens are subtracted from the input total; Anthropic
  cache tokens are separate. Cost = a linear combination of these four.
- **Plaintext keys are never persisted.** Only `sha256(key)` (lookup) + a display prefix.
  The full key is returned exactly once from `POST /admin/keys`.
- **Master key compared in constant time** (`crypto/subtle`).
- **Forward the original path + query unchanged**; only the `Authorization` header (and
  configured provider headers) are rewritten. Hop-by-hop headers are stripped both ways.

## Conventions

- Plain `net/http` + `http.ServeMux` with Go 1.22 method+wildcard patterns. No router libs.
- Errors to clients are JSON `{"error":{"message":"…"}}`.
- Timestamps stored as unix millis; exposed as RFC3339 in JSON.
- `gofmt` everything. Match the existing comment density and naming.

## Working in this repo

```sh
go build ./...
go test ./...
go vet ./...
go run ./cmd/gateway -config config.yaml
```

End-to-end check: run a mock upstream, point a provider at it, create a key via
`/admin/keys`, send a streaming and a non-streaming request, and confirm a `request_logs`
row with sane TTFT/tokens/cost (see the smoke test pattern referenced in the README).

## Adding things

- **New provider:** config only — add to `providers:`. No code.
- **New model price:** config only — add to `pricing:`.
- **New usage shape:** extend `internal/usage` (`rawUsage`/`usageEnvelope`/`normalize`) and
  add a focused test with a real-ish payload. Keep it best-effort and provider-neutral.
- **New admin endpoint:** add to `Admin.Handler`, keep it behind the master-key middleware,
  return JSON, and add a handler test.
