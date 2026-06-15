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
- **Retries with backoff + jitter.** Network errors and HTTP 429/5xx are retried per a
  per-provider (or default) policy: `base_delay * 2^attempt` capped at `max_delay`, with
  full jitter. Retries never happen after bytes have reached the client.
- **Transparent proxying.** Plain JSON and `text/event-stream` are streamed through
  unchanged, flushed per chunk.
- **Best-effort metering.** Model, input/output/cache-read/cache-write tokens, TTFT, total
  duration, status, and computed cost are logged for every request — extracted from OpenAI
  Chat/Responses and Anthropic Messages payloads (streaming or not) without coupling to any
  endpoint.
- **Cost computation.** Per-model token pricing in the config yields a dollar cost per
  request and aggregate stats.
- **One master key** authenticates the admin API and the web portal.
- **Web portal** for managing keys and inspecting logs/stats.
- **Pure Go.** Builds with `go build` — no cgo, no external services (SQLite via
  `modernc.org/sqlite`).

## Quick start

```sh
go build -o gateway ./cmd/gateway
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
    base_url: http://copilot-api:4141
    api_key: dummy            # injected as the upstream Authorization bearer token
    headers: {}               # optional extra upstream headers
    retry: { max_retries: 5 } # optional per-provider override
pricing:
  - model: gpt-5.4
    input_cost_per_token: 2.5e-6
    output_cost_per_token: 1.5e-5
    cache_read_input_token_cost: 2.5e-7
    cache_creation_input_token_cost: 6.25e-6   # Anthropic-style cache writes
```

## HTTP surface

### Data plane (gateway API key)

| Method | Path  | Notes |
|--------|-------|-------|
| any    | `/*`  | Authenticated by gateway key (`Authorization: Bearer` or `x-api-key`), proxied to a bound provider. |

### Control plane (master key)

| Method | Path                | Body / query |
|--------|---------------------|--------------|
| POST   | `/admin/keys`       | `{"name","providers":[...]}` → returns the plaintext key once |
| GET    | `/admin/keys`       | list keys (no secret) |
| DELETE | `/admin/keys/{id}`  | delete a key |
| GET    | `/admin/logs`       | `?limit&offset&api_key_id&provider&since` |
| GET    | `/admin/stats`      | `?api_key_id&since` — aggregates grouped by key + model |
| GET    | `/admin/providers`  | configured provider names |
| GET    | `/healthz`          | liveness (no auth) |

`since` accepts an RFC3339 timestamp, a Go duration window (e.g. `24h`), or unix millis.

## How metering works

Token usage is normalized into four non-overlapping buckets — input, output, cache-read,
cache-write — so cost is a simple linear combination of per-token rates. OpenAI counts
cached tokens *within* the prompt/input total (the gateway subtracts them out); Anthropic
counts cache reads/creations separately (kept as-is). For SSE, usage fields are combined
across events by maximum, so Anthropic's split `message_start` (input/cache) and
`message_delta` (output) events reassemble correctly. Models without configured pricing are
still logged, at zero cost.

## Development

```sh
go test ./...     # all packages have tests
go vet ./...
```

Layout:

```
cmd/gateway        entrypoint (config load, wiring, graceful shutdown)
internal/config    YAML load, defaults, validation
internal/pricing   normalized Usage → cost
internal/store     SQLite: api_keys + request_logs, CRUD/query/stats
internal/usage     best-effort usage/model extraction (JSON + SSE)
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
