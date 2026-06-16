# agl-gateway

A minimalistic, provider-agnostic LLM gateway in Go.

It sits in front of one or more upstream LLM providers and routes each request **by API
key** — not by URL or model. The gateway does not understand `/v1/chat/completions`,
`/v1/responses`, or `/v1/messages`; it forwards the request path and body unchanged,
swaps in the upstream credential, retries transient failures, and records best-effort
metadata (tokens, TTFT, duration, cost) to SQLite. Both plain HTTP and SSE streaming
work transparently.

## Features

- **Key-based routing.** Each gateway API key is bound to one or more providers. Requests
  are routed to a randomly chosen bound provider.
- **Model mapping.** A provider can rewrite the request's `model` to an upstream name before
  forwarding; both the requested and mapped model are logged.
- **Retries with backoff + jitter.** Network errors and HTTP 429/5xx are retried per a
  per-provider (or default) policy: `base_delay * 2^attempt` capped at `max_delay`, with
  full jitter. Retries never happen after bytes have reached the client.
- **Clear failures.** On failure the client gets a structured error stating the attempt
  count and whether it was a **provider** or **gateway** fault, plus `X-AGL-*` response
  headers. The attempt count and failure reason are recorded in the log.
- **Transparent proxying.** Plain JSON and `text/event-stream` are streamed through
  unchanged, flushed per chunk.
- **Best-effort metering.** Model, mapped model, attempts, input/output/cache-read/cache-write
  tokens, TTFT, total duration, status, error, and computed cost are logged for every
  request — extracted from OpenAI Chat/Responses and Anthropic Messages payloads (streaming
  or not) without coupling to any endpoint.
- **Cost computation.** Per-model token pricing in the config yields a dollar cost per
  request and aggregate stats.
- **One master key** authenticates the admin API and the web portal.
- **Web portal** for managing keys and inspecting logs/stats.
- **Deleting a key cascades** to its logs, reclaiming database space.
- **Pure Go.** Builds with `go build` — no cgo, no external services (SQLite via
  `modernc.org/sqlite`).

## Quick start

```sh
go build -o gateway ./cmd/gateway
go build -o modelcheck ./cmd/modelcheck   # optional: model test harness (see below)
cp config.example.yaml config.yaml      # then edit master_key, providers, pricing
./gateway -config config.yaml
```

Create a key (master key required), then use it like any OpenAI/Anthropic base URL:

```sh
# Create an API key bound to the "openai" provider.
curl -X POST localhost:8080/admin/keys \
  -H "Authorization: Bearer $MASTER_KEY" \
  -d '{"name":"my-app","providers":["openai"]}'
# -> {"id":1,"name":"my-app","key":"sk-gw-…","prefix":"sk-gw-…","providers":["openai"],…}
#    The plaintext "key" is shown only once.

# Use it. The path and body are forwarded to the provider's base_url verbatim.
curl localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-…" \
  -d '{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}'
```

Open the portal at <http://localhost:8080/portal> and paste the master key to manage keys
and browse logs/stats.

## Docker

The image is a static binary on Debian slim (non-root, ~13 MB layer for the binary). It
expects a config at `/data/config.yaml` and keeps the SQLite database in `/data`, so mount
your config read-only and persist `/data`.

```sh
docker build -t agl-gateway .
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/data/config.yaml:ro" \
  -v agl-data:/data \
  agl-gateway
```

Or with Compose ([`compose.yaml`](compose.yaml)):

```sh
cp config.example.yaml config.yaml   # edit master_key, providers, pricing
docker compose up -d
```

Pushing a `v*` tag builds and publishes a multi-arch image (linux/amd64 + linux/arm64) to
the GitHub Container Registry via [`.github/workflows/docker-publish.yml`](.github/workflows/docker-publish.yml):

```sh
docker run -p 8080:8080 -v "$PWD/config.yaml:/data/config.yaml:ro" -v agl-data:/data \
  ghcr.io/agent-lightning/agl-gateway:latest
```

## Configuration

See [`config.example.yaml`](config.example.yaml) for a fully commented example. Shape:

```yaml
server: { addr: ":8080" }
master_key: "mk-…"            # authenticates /admin and the portal
database: "./gateway.db"
defaults:
  retry: { max_retries: 3, base_delay: 200ms, max_delay: 10s }
providers:
  - name: openai
    base_url: https://api.openai.com
    api_key: sk-…            # injected as the upstream Authorization bearer token
    headers: {}              # optional extra upstream headers (e.g. anthropic x-api-key)
    model_map:               # optional: rewrite the request model before forwarding
      gpt-fast: gpt-5-mini
    retry: { max_retries: 5 } # optional per-provider override
pricing:
  - model: gpt-5.4
    input_cost_per_token: 2.5e-6
    output_cost_per_token: 1.5e-5
    cache_read_input_token_cost: 2.5e-7
    cache_creation_input_token_cost: 6.25e-6   # Anthropic-style cache writes
```

## Failure semantics

When a request cannot be completed the gateway makes the cause explicit:

- **Provider failure** — the upstream was unreachable after every retry, or returned a
  surviving 429/5xx. The response carries `X-AGL-Error-Source: provider`,
  `X-AGL-Provider`, and `X-AGL-Attempts`. If no HTTP response was obtained at all, the body
  is `{"error":{"message":"agl-gateway: upstream provider \"openai\" is unreachable after N
  attempt(s): …","source":"provider","provider":"openai","attempts":N}}`. Otherwise the
  upstream's own status and body pass through, annotated with those headers.
- **Gateway failure** — a missing/invalid key, a key bound to no configured provider, or an
  internal error. `source` is `gateway` and the message is prefixed `agl-gateway:`.

Successful responses also carry `X-AGL-Provider` and `X-AGL-Attempts`, so a request that
quietly succeeded only after retries is still visible.

## HTTP surface

### Data plane (gateway API key)

| Method | Path  | Notes |
|--------|-------|-------|
| any    | `/*`  | Authenticated by gateway key (`Authorization: Bearer` or `x-api-key`), proxied to a bound provider. |

A key bound to several providers is normally routed to a randomly chosen one. To pin a
specific provider, send the `X-AGL-Provider: <name>` request header — it must name one of
the key's bound providers (otherwise the request is rejected `400`). The header is consumed
by the gateway and never forwarded upstream.

### Control plane (master key)

| Method | Path                | Body / query |
|--------|---------------------|--------------|
| POST   | `/admin/keys`       | `{"name","providers":[...]}` → returns the plaintext key once |
| GET    | `/admin/keys`       | list keys (no secret) |
| DELETE | `/admin/keys/{id}`  | delete a key |
| GET    | `/admin/logs`       | `?limit&offset&api_key_id&provider&since` |
| GET    | `/admin/stats`      | `?api_key_id&since` — aggregates grouped by key + model |
| GET    | `/admin/providers`  | configured providers + their models (see below) |
| POST   | `/admin/test`       | run the model test server-side; streams NDJSON progress events (body: `{provider,exclude,path,max_tokens,concurrency,stream}`, all optional) |
| GET    | `/healthz`          | liveness (no auth) |

`since` accepts an RFC3339 timestamp, a Go duration window (e.g. `24h`), or unix millis.

`GET /admin/providers` returns `[{"name","models":[...],"error":"…"}]`. The model list is
discovered live and best-effort from each provider's OpenAI-compatible `/v1/models`
endpoint (probed concurrently, using that provider's own credentials) unioned with its
configured `model_map` aliases. A provider whose probe fails still appears, with `error`
set and `models` limited to any aliases.

### Testing every model

`cmd/modelcheck` exercises every model of every provider through a running gateway: it reads
`/admin/providers`, mints a temporary gateway key, sends one small request per
`(provider, model)` (pinned with `X-AGL-Provider`, run in parallel), streams a `[done/total]`
pass/fail line as each completes, prints a failures table, deletes the key, and exits
non-zero if anything failed. The endpoint is chosen per model: `claude*` models are sent as
Anthropic Messages requests to `/v1/messages`, everything else as OpenAI Responses requests
to `/v1/responses`.

```sh
go build -o modelcheck ./cmd/modelcheck
./modelcheck -url http://localhost:8080 -master-key "$AGL_MASTER_KEY"
# -concurrency N     parallel probes (default 8)  -path /v1/chat/completions  force one endpoint
# -provider <name>   only that provider           -max-tokens N               probe size (default 16)
# -exclude globs     skip models (default gpt-image*, comma-separated)  -stream  send stream:true
```

The same test is available **in the web portal** (the "Test models" tab) and as a control-plane
endpoint, `POST /admin/test`, which runs it server-side by driving the proxy in-process and
streams newline-delimited JSON events (`start`, one `result` per probe, `done`) so the portal
fills in rows live. The per-model logic — endpoint selection, request body, and result
interpretation — is shared by the CLI and the endpoint via the `internal/probe` package, so
both behave identically. The probes run under a temporary key that is deleted when the run
finishes, which cascades away the request logs they produced — so a test run leaves no
lasting `request_logs` entries.

## How metering works

Token usage is normalized into four non-overlapping buckets — input, output, cache-read,
cache-write — so cost is a simple linear combination of per-token rates. OpenAI counts
cached tokens *within* the prompt/input total (the gateway subtracts them out); Anthropic
counts cache reads/creations separately (kept as-is). For SSE, usage fields are combined
across events by maximum, so Anthropic's split `message_start` (input/cache) and
`message_delta` (output) events reassemble correctly. When `model_map` is in effect, cost is
priced by the **requested** model (the alias), falling back to the mapped upstream model only
when the alias has no configured price. Models without configured pricing are still logged,
at zero cost.

## Development

```sh
go test ./...     # all packages have tests
go vet ./...
```

Layout:

```
cmd/gateway        entrypoint (config load, wiring, graceful shutdown)
cmd/modelcheck     test every provider's models through a running gateway
internal/config    YAML load, defaults, validation
internal/pricing   normalized Usage → cost
internal/store     SQLite: api_keys + request_logs, CRUD/query/stats
internal/usage     best-effort usage/model extraction (JSON + SSE)
internal/probe     shared model-probe logic (endpoint/body/summary), used by the CLI and /admin/test
internal/keys      mint + sha256-hash API keys
internal/proxy     auth, routing, retry, streaming, metering
internal/admin     master-key control-plane handlers
internal/portal    embedded inspection/management SPA
internal/server    top-level routing
```

## Security notes

- Plaintext gateway keys are never stored — only their SHA-256 hash and a short display
  prefix. The full key is shown once at creation.
- The master key is compared in constant time.
- The portal keeps the master key in the browser's `localStorage` only; it is sent solely
  to this gateway's `/admin` endpoints. Serve the gateway over TLS in production
  (e.g. behind a reverse proxy).
