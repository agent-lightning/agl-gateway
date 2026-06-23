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

- **Minimalistic.** Standard library first. The core dependencies are `gopkg.in/yaml.v3` and
  `modernc.org/sqlite` (pure-Go, no cgo). The store can alternatively be backed by
  **PostgreSQL** via `github.com/jackc/pgx/v5/stdlib` (also pure-Go, no cgo) — selected at
  runtime by the `database` config value, with no other code path changes. `internal/capture`
  additionally depends on the official
  provider SDKs — `github.com/openai/openai-go/v3` and `github.com/anthropics/anthropic-sdk-go` —
  for their stream-accumulator and usage types, so response assembly and metering track the real
  APIs instead of hand-rolled shapes. Don't add other dependencies or frameworks without a strong
  reason.
- **Robust over clever.** Failures degrade gracefully: unknown models still log (zero
  cost), unparseable usage still logs (zero tokens), a dead provider returns 502 and is
  recorded.
- **Well tested.** Every package has tests. Add/extend tests with any behavior change;
  `go test ./...` and `go vet ./...` must stay green.
- **Best-effort metering, never blocking.** Metadata extraction must never break or delay
  the proxied response. The client gets bytes first; logging happens after.

## Architecture (data flow)

```
client ──► proxy ──┐ auth (sha256 key lookup) ──► build provider sequence (X-AGL-Provider pins one, else the key's start/order policy)
                   ├─ retry loop (backoff+jitter on net err / 408 / 429 / 5xx / LiteLLM tag-bug 401 / LiteLLM-Azure "unsupported" 400); each retry fails over to the next provider in the sequence
                   └─ stream upstream→client (SSE flushed), tee into the metering sink
                      (capture.Accumulator for recognized formats — usage + assembled body;
                       else usage.Accumulator)
                                                  └─► pricing.Cost ─► store.InsertLog
```

Packages:

| Package           | Responsibility |
|-------------------|----------------|
| `internal/config` | YAML load, defaults, validation. |
| `internal/pricing`| Normalized `Usage` → dollar cost. |
| `internal/store`  | SQLite **or** PostgreSQL persistence for `api_keys`/`request_logs`. Backend chosen by the `database` config value (a `postgres://`/`postgresql://` URL selects PostgreSQL via pgx; anything else is a SQLite file path), overridable by the `AGL_DATABASE` env var. Query logic is shared; a small `dialect` (`store_sqlite.go`/`store_postgres.go`) isolates placeholder rebinding, the `SUM` cast, schema/migrate, and post-delete vacuum. |
| `internal/usage`  | Generic best-effort fallback for model + usage extraction on *unrecognized* endpoints (JSON + SSE), plus request-side `RequestModel`/`SetModel`. Recognized formats are metered by `internal/capture`. |
| `internal/capture`| For recognized API formats (chat/responses/messages), wraps the provider SDK accumulators to derive both the assembled non-streaming response and normalized token usage from one pass; records `api_type` and any `assemble_error`. |
| `internal/keys`   | Mint + SHA-256-hash gateway keys. |
| `internal/probe`  | Shared model-probe logic (endpoint/body/summary, worker pool); used by `cmd/modelcheck` and `/admin/test`. |
| `internal/proxy`  | Data plane: auth, routing, retry, streaming, metering. |
| `internal/admin`  | Master-key control plane (`/admin/*`); `/admin/providers` probes upstream `/v1/models`; `/admin/test` runs the model check in-process through the proxy; `/admin/logs/{id}` returns one log with its captured payloads for the portal's inspector. |
| `internal/portal` | Embedded management/inspection SPA (keys, logs, stats, model test). The UI is a Vite + React + Tailwind/shadcn app whose source lives at the repo root in `/ui`; its production build is emitted to `internal/portal/dist` (gitignored) and embedded via `go:embed`. A committed `.gitkeep` anchors the embed so `go build` works before the UI is compiled; when the build is absent the handler serves a "portal not built" placeholder. |
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
- **Provider selection is per-key and policy-driven.** When a request does not pin a
  provider via `X-AGL-Provider`, the key's `provider_start` (`first`|`random`) and
  `provider_order` (`round_robin`|`random`) build the ordered sequence the retry loop walks;
  each retry fails over to the next provider in that sequence (wrapping if the attempt budget
  exceeds the provider count). A pinned `X-AGL-Provider` uses exactly that one provider with
  no failover. The retry budget (`MaxRetries`/backoff) comes from the *starting* provider, so
  every provider is tried once only when `MaxRetries >= len(providers)-1`. The provider logged
  and reported in `X-AGL-Provider` is the one that served (or was last tried).
- **Failures are classified.** Every failure tells the client whether it was a `provider` or
  `gateway` fault, with the attempt count, via both the JSON body and `X-AGL-*` headers.
  Provider responses (incl. surviving 4xx/5xx) pass through; only gateway-side problems are
  synthesized. The attempt count and reason are written to the log.
- **Deleting a key cascades to its logs** in a transaction. On SQLite it then runs
  `incremental_vacuum` to release space (`auto_vacuum=INCREMENTAL` is set at DB creation); on
  PostgreSQL the post-delete `afterDeleteKey` hook is a no-op (autovacuum reclaims space).

## Conventions

- Plain `net/http` + `http.ServeMux` with Go 1.22 method+wildcard patterns. No router libs.
- Errors to clients are JSON. Gateway-synthesized errors use a shape valid for both the
  OpenAI and Anthropic SDKs: `{"type":"error","error":{"type":"…","message":"…", …}}` (the
  inner object also carries `source`/`attempts`/`provider`). Upstream provider errors pass
  through verbatim.
- Timestamps stored as unix millis; exposed as RFC3339 in JSON.
- `gofmt` everything. Match the existing comment density and naming.

## Working in this repo

```sh
go build ./...
go test ./...                          # all packages have tests
go vet ./...
go run ./cmd/gateway -config config.yaml
```

### Portal UI (`/ui`)

The management portal is a Vite + React + TypeScript + Tailwind/shadcn app under `/ui`. Its
build output (`internal/portal/dist`) is **gitignored** and embedded by Go via `go:embed`, so
rebuild it whenever the UI source changes:

```sh
npm --prefix ui ci          # first time / lockfile change
npm --prefix ui run build   # emits internal/portal/dist, then re-anchors the .gitkeep
npm --prefix ui run dev      # hot-reload dev server (proxy /admin and /healthz to a gateway)
```

CI and the Docker image build the UI automatically before the Go build, so a fresh checkout
needs no manual step to ship. A bare `go build`/`go test` without a prior UI build still works
— `go:embed` falls back to the committed `.gitkeep` anchor and the portal serves a
"not built" placeholder; the portal tests skip rather than fail.

The `internal/store` tests run against in-memory SQLite by default. To also exercise the
PostgreSQL backend, point `AGL_DATABASE` at a throwaway database (the same env var the
gateway honors at runtime); the store tests then run every portable case against both
backends, and skip PostgreSQL when it is unset:

```sh
docker run -d -e POSTGRES_PASSWORD=pw -p 5432:5432 postgres:18-alpine
AGL_DATABASE='postgres://postgres:pw@localhost:5432/postgres?sslmode=disable' go test ./internal/store/...
```

Build the binaries from source:

```sh
go build -o gateway ./cmd/gateway
go build -o modelcheck ./cmd/modelcheck   # optional model-test harness, see below
```

End-to-end check: run a mock upstream, point a provider at it, create a key via
`/admin/keys`, send a streaming and a non-streaming request, and confirm a `request_logs`
row with sane TTFT/tokens/cost.

### `cmd/modelcheck`

`cmd/modelcheck` exercises every model of every provider through a running gateway: it reads
`/admin/providers`, mints a temporary gateway key, sends one small request per
`(provider, model)` (pinned with `X-AGL-Provider`, run in parallel), streams a `[done/total]`
pass/fail line as each completes, prints a failures table, deletes the key, and exits
non-zero if anything failed. The endpoint is chosen per model: `claude*` models go to
`/v1/messages` as Anthropic Messages requests, everything else to `/v1/responses` as OpenAI
Responses requests.

```sh
go build -o modelcheck ./cmd/modelcheck
./modelcheck -url http://localhost:8080 -master-key "$AGL_MASTER_KEY"
# -concurrency N     parallel probes (default 8)  -path /v1/chat/completions  force one endpoint
# -provider <name>   only that provider           -max-tokens N               probe size (default 16)
# -exclude globs     skip models (default gpt-image*, comma-separated)  -stream  send stream:true
```

The same run is exposed in-process via `POST /admin/test` (and the portal's "Test models"
tab), which drives the proxy directly and streams newline-delimited JSON events (`start`,
one `result` per probe, `done`). Per-model logic — endpoint selection, request body, result
interpretation — is shared by the CLI and the endpoint via `internal/probe`, so both behave
identically.

### Docker image

The image is a static binary on Debian slim (non-root, ~13 MB layer for the binary). A first
`node` build stage compiles the portal UI (`/ui` → `internal/portal/dist`) and the Go stage
embeds it, so the final image still ships a single static binary with the SPA baked in. It
expects a config at `/data/config.yaml` and keeps the SQLite database in `/data` (when
`database` is a `postgres://` URL the gateway uses that instead and the `/data` SQLite file is
unused). Build it
locally with `docker build -t agl-gateway .`. Pushing a `v*` tag builds and publishes a
multi-arch image (linux/amd64 + linux/arm64) to GHCR via the publish-on-tag workflow (see
[Versioning](#versioning)).

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
- **Schema change:** add the column to the SQLite `CREATE TABLE` *and* an `ensureColumn` call
  in `store_sqlite.go` so existing databases upgrade in place, **and** add it to the
  `postgresSchema` DDL in `store_postgres.go` (keep the types in sync — SQLite
  `INTEGER`/`BLOB`/`REAL` map to PostgreSQL `BIGINT`/`BYTEA`/`DOUBLE PRECISION`); extend
  `RequestLog`, the `INSERT`, and the `QueryLogs` `SELECT`/scan together.
- **New usage shape:** for a *recognized* format, prefer bumping the provider SDK version in
  `internal/capture` (the SDK accumulator/usage types own these shapes). For an *unrecognized*
  endpoint, extend the generic fallback in `internal/usage` (`rawUsage`/`usageEnvelope`/`normalize`).
  Either way add a focused test with a real-ish payload and keep it best-effort.
- **New admin endpoint:** add to `Admin.Handler`, keep it behind the master-key middleware,
  return JSON, and add a handler test.
