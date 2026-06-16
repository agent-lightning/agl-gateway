# AGENTS.md

Guidance for AI agents and contributors working in this repository.

## What this is

`agl-gateway` is a minimalistic, **provider-agnostic** LLM gateway. The defining
constraint: it routes purely by inbound **API key**, and it must **not** grow knowledge of
specific endpoint shapes (`/v1/chat/completions`, `/v1/responses`, `/v1/messages`). It
forwards the request path/body verbatim and extracts metadata on a *best-effort* basis.
Keep that boundary intact — endpoint-specific logic does not belong here. The sole, scoped
exception is the **control plane**: `GET /admin/providers` probes each provider's
OpenAI-compatible `/v1/models` to list models. The data plane stays endpoint-agnostic.

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
client ──► proxy ──┐ auth (sha256 key lookup) ──► pick bound provider (X-AGL-Provider header, else random)
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
| `internal/probe`  | Shared model-probe logic (endpoint/body/summary, worker pool); used by `cmd/modelcheck` and `/admin/test`. |
| `internal/proxy`  | Data plane: auth, routing, retry, streaming, metering. |
| `internal/admin`  | Master-key control plane (`/admin/*`); `/admin/providers` probes upstream `/v1/models`; `/admin/test` runs the model check in-process through the proxy. |
| `internal/portal` | Embedded management/inspection SPA (keys, logs, stats, model test). |
| `internal/server` | Top-level HTTP routing. |
| `internal/version`| Build version string (`var Version`), stamped via `-ldflags` at release. |
| `cmd/gateway`     | Entrypoint. |
| `cmd/modelcheck`  | CLI that probes every provider's models through a running gateway. |

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
- **Model mapping is best-effort and provider-scoped.** `provider.model_map` rewrites only
  the top-level `model` field (via `usage.SetModel`, which preserves every other field
  byte-for-byte); the original and mapped model are both logged, and cost is priced by the
  requested model (the alias), falling back to the mapped upstream model only when the
  requested one is unpriced.
- **Failures are classified.** Every failure tells the client whether it was a `provider` or
  `gateway` fault, with the attempt count, via both the JSON body and `X-AGL-*` headers.
  Provider responses (incl. surviving 4xx/5xx) pass through; only gateway-side problems are
  synthesized. The attempt count and reason are written to the log.
- **Deleting a key cascades to its logs** in a transaction, then runs `incremental_vacuum`
  to release space (`auto_vacuum=INCREMENTAL` is set at DB creation).

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

## Versioning

The version lives in one place: `var Version` in `internal/version`. It is `"dev"` for any
local or source build and is overridden at build time via the linker — nothing imports a
hardcoded number, so there is no constant to keep in sync.

- **What surfaces it:** `GET /healthz` returns `{"status":"ok","version":"…"}`, `gateway
  -version` prints it, and it is logged on startup.
- **How releases stamp it:** the Dockerfile takes a `VERSION` build-arg and passes it via
  `-ldflags "-X …/internal/version.Version=$VERSION"`. The publish-on-tag workflow
  (`.github/workflows/docker-publish.yml`) sets `VERSION` from the pushed git tag, so a
  released image reports its tag.
- **To bump the version:** tag the commit (`git tag vX.Y.Z && git push origin vX.Y.Z`).
  Follow semver. The tag drives both the GHCR image tags and the stamped version — do **not**
  edit `internal/version` to set a release number. For a manual/local stamped build:
  `go build -ldflags "-X github.com/agent-lightning/agl-gateway/internal/version.Version=vX.Y.Z" ./cmd/gateway`.

## Adding things

- **New provider:** config only — add to `providers:`. No code.
- **Model mapping:** config only — add `model_map:` under a provider. No code.
- **New model price:** config only — add to `pricing:`.
- **Schema change:** add the column to the `CREATE TABLE` in `store.migrate` *and* an
  `ensureColumn` call so existing databases upgrade in place; extend `RequestLog`, the
  `INSERT`, and the `QueryLogs` `SELECT`/scan together.
- **New usage shape:** extend `internal/usage` (`rawUsage`/`usageEnvelope`/`normalize`) and
  add a focused test with a real-ish payload. Keep it best-effort and provider-neutral.
- **New admin endpoint:** add to `Admin.Handler`, keep it behind the master-key middleware,
  return JSON, and add a handler test.
