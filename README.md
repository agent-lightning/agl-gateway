# agl-gateway

A minimalistic, provider-agnostic LLM gateway.

It sits in front of one or more upstream LLM providers and routes each request **by API
key** — not by URL or model. The gateway does not understand `/v1/chat/completions`,
`/v1/responses`, or `/v1/messages`; it forwards the request path and body unchanged, swaps
in the upstream credential, retries transient failures, and records best-effort metadata
(tokens, TTFT, duration, cost) to SQLite. Both plain HTTP and SSE streaming work
transparently.

## Features

- **Key-based routing.** Each gateway API key is bound to one or more providers. Requests
  are routed to a randomly chosen bound provider, or pinned to one with a request header.
- **Model mapping.** A provider can rewrite the request's `model` to an upstream name before
  forwarding; both the requested and mapped model are logged.
- **Retries with backoff + jitter.** Network errors and HTTP 429/5xx are retried per a
  per-provider (or default) policy. Retries never happen after bytes have reached the client.
- **Clear failures.** On failure the client gets a structured error stating the attempt
  count and whether it was a **provider** or **gateway** fault, plus `X-AGL-*` response
  headers.
- **Transparent proxying.** Plain JSON and `text/event-stream` are streamed through
  unchanged, flushed per chunk.
- **Best-effort metering.** Model, attempts, input/output/cache tokens, TTFT, duration,
  status, error, and computed cost are logged for every request — extracted from OpenAI
  Chat/Responses and Anthropic Messages payloads (streaming or not).
- **Cost computation.** Per-model token pricing in the config yields a dollar cost per
  request and aggregate stats.
- **Web portal** for managing keys and inspecting logs, stats, and a live model test.
- **Self-contained.** A single static binary with an embedded SQLite database — no cgo, no
  external services to run alongside it.

## Quick start

The gateway ships as a container image. Provide a config at `/data/config.yaml` and persist
`/data` (where the SQLite database lives):

```sh
cp config.example.yaml config.yaml   # then edit master_key, providers, pricing
docker run -p 8080:8080 \
  -v "$PWD/config.yaml:/data/config.yaml:ro" \
  -v agl-data:/data \
  ghcr.io/agent-lightning/agl-gateway:latest
```

Or with Compose ([`compose.yaml`](compose.yaml)):

```sh
cp config.example.yaml config.yaml
docker compose up -d
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
and browse logs, stats, and the model test.

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

The retry policy is `base_delay * 2^attempt` capped at `max_delay`, with full jitter.

## Using the gateway

Send requests to the gateway exactly as you would to the upstream provider — the path,
query, and body are forwarded verbatim. Authenticate with a gateway key via either
`Authorization: Bearer <key>` or `x-api-key: <key>`.

A key bound to several providers is normally routed to a randomly chosen one. To pin a
specific provider, send the `X-AGL-Provider: <name>` request header — it must name one of
the key's bound providers (otherwise the request is rejected `400`). The header is consumed
by the gateway and never forwarded upstream.

Successful responses carry `X-AGL-Provider` and `X-AGL-Attempts`, so a request that quietly
succeeded only after retries is still visible.

### Failure semantics

When a request cannot be completed the gateway makes the cause explicit:

- **Provider failure** — the upstream was unreachable after every retry, or returned a
  surviving 429/5xx. The response carries `X-AGL-Error-Source: provider`, `X-AGL-Provider`,
  and `X-AGL-Attempts`. If no HTTP response was obtained at all, the body is
  `{"error":{"message":"agl-gateway: upstream provider \"openai\" is unreachable after N
  attempt(s): …","source":"provider","provider":"openai","attempts":N}}`. Otherwise the
  upstream's own status and body pass through, annotated with those headers.
- **Gateway failure** — a missing/invalid key, a key bound to no configured provider, or an
  internal error. `source` is `gateway` and the message is prefixed `agl-gateway:`.

### Cost & metering

Token usage is normalized into four non-overlapping buckets — input, output, cache-read,
cache-write — and cost is a simple linear combination of the per-token rates from your
`pricing` config. When `model_map` is in effect, cost is priced by the **requested** model
(the alias), falling back to the mapped upstream model only when the alias has no configured
price. Models without configured pricing are still logged, at zero cost. Metering is
best-effort and never delays or breaks the proxied response.

## Admin API

The control plane is authenticated by the **master key** (`Authorization: Bearer
$MASTER_KEY`).

| Method | Path                | Body / query |
|--------|---------------------|--------------|
| POST   | `/admin/keys`       | `{"name","providers":[...]}` → returns the plaintext key once |
| GET    | `/admin/keys`       | list keys (no secret) |
| DELETE | `/admin/keys/{id}`  | delete a key (cascades to its logs, reclaiming space) |
| GET    | `/admin/logs`       | `?limit&offset&api_key_id&provider&since` |
| GET    | `/admin/stats`      | `?api_key_id&since` — aggregates grouped by key + model |
| GET    | `/admin/providers`  | configured providers + their models (see below) |
| POST   | `/admin/test`       | run the model test server-side; streams NDJSON progress events |
| GET    | `/healthz`          | liveness (no auth); reports status and build version |

`since` accepts an RFC3339 timestamp, a Go duration window (e.g. `24h`), or unix millis.

`GET /admin/providers` returns `[{"name","models":[...],"error":"…"}]`. The model list is
discovered live and best-effort from each provider's OpenAI-compatible `/v1/models` endpoint
(probed concurrently, using that provider's own credentials) unioned with its configured
`model_map` aliases. A provider whose probe fails still appears, with `error` set and
`models` limited to any aliases.

### Testing every model

The portal's **Test models** tab (and the `POST /admin/test` endpoint behind it) exercises
every model of every provider through the gateway: it sends one small request per
`(provider, model)`, streams a pass/fail result as each completes, and reports a failures
summary. The endpoint is chosen per model: `claude*` models are sent as Anthropic Messages
requests to `/v1/messages`, everything else as OpenAI Responses requests to `/v1/responses`.
Probes run under a temporary key that is deleted when the run finishes, which cascades away
the request logs they produced — so a test run leaves no lasting entries.

## Security

- Plaintext gateway keys are never stored — only their SHA-256 hash and a short display
  prefix. The full key is shown once at creation.
- The master key is compared in constant time.
- The portal keeps the master key in the browser's `localStorage` only; it is sent solely to
  this gateway's `/admin` endpoints. Serve the gateway over TLS in production (e.g. behind a
  reverse proxy).
