# agl-gateway

**One endpoint in front of every LLM provider — routed by API key, not by URL.**

`agl-gateway` is a minimalistic, provider-agnostic LLM gateway. Point your OpenAI- or
Anthropic-style client at it, authenticate with a gateway key, and the gateway forwards the
request **path and body verbatim** to the right upstream — swapping in the real credential,
retrying transient failures, failing over across providers, and metering tokens, latency, and
cost along the way. Plain JSON and SSE streaming both pass through transparently.

```
your app ──Bearer sk-gw-…──►  agl-gateway  ──►  OpenAI / Anthropic / vLLM / …
                                  │ routes by key, retries, fails over
                                  └─► logs tokens · TTFT · cost  (best-effort, never blocking)
```

### Design principles

- 🔌 **Endpoint-agnostic.** The gateway never learns the shape of `/v1/chat/completions`,
  `/v1/responses`, or `/v1/messages`. It forwards bytes and extracts metadata best-effort, so
  new endpoints and providers work on day one.
- 🔑 **Routing is purely by key.** A gateway key is bound to one or more providers; the key's
  policy (or a request header) decides where each request goes.
- 🛡️ **Proxy-first.** Faithful forwarding always wins. Metering runs off the critical path and
  can never stall, break, or crash the proxied response.
- 📦 **Self-contained.** A single static binary with an embedded SQLite database and web
  portal. No cgo, no sidecars. Scales out to PostgreSQL and ClickHouse when you need it.

### Highlights

- **Key-based routing & failover** with per-key start/order policies, or pin a provider per
  request via a header.
- **Retries** with exponential backoff + jitter on network errors and HTTP 408/429/5xx — never
  after bytes have reached the client.
- **Clear failures**: every error says whether it was a **provider** or **gateway** fault, how
  many attempts were made, via both the JSON body and `X-AGL-*` headers.
- **Best-effort metering**: model, tokens (input/output/cache), TTFT, duration, status, and
  computed **cost** logged for every request.
- **Model mapping** to rewrite a request's `model` to an upstream name per provider.
- **Optional payload capture** with best-effort SSE assembly for inspection.
- **Web portal** to manage keys and inspect logs, stats, and a live model test.

---

# Tutorial

## 1. Install

### Docker (recommended)

The image ships a single static binary with the portal baked in. Provide a config at
`/data/config.yaml` and persist `/data` (where the SQLite database lives):

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

### From source

A bare `go build` works, but the web portal is compiled separately and embedded into the
binary — build it first or `/portal` serves a "portal not built" placeholder:

```sh
npm --prefix ui ci && npm --prefix ui run build   # emit the embedded portal assets
go run ./cmd/gateway -config config.yaml          # or: go build -o gateway ./cmd/gateway
```

See [AGENTS.md](AGENTS.md) for the full development workflow.

## 2. Configure

Start from [`config.example.yaml`](config.example.yaml) — it is fully commented. A minimal
config needs a master key, at least one provider, and (for cost) pricing:

```yaml
server:
  addr: ":8080"

master_key: "mk-change-me"            # authenticates /admin and the portal (override: AGL_MASTER_KEY); use `openssl rand -hex 32`

database: "./gateway.db"              # SQLite file path (or a postgres:// URL)

providers:
  - name: openai
    base_url: https://api.openai.com  # bare origin, NO /v1 — the client's path is appended verbatim
    api_key: "sk-your-openai-key"     # injected as the upstream Authorization bearer token

pricing:
  - model: gpt-5.4
    input_cost_per_token: 2.5e-6
    output_cost_per_token: 1.5e-5
    cache_read_input_token_cost: 2.5e-7
```

> **`base_url` must be the bare origin** (scheme + host + optional port) without a path. The
> gateway appends the client's full request path, so `/v1` comes from the client. Adding `/v1`
> here produces a doubled `/v1/v1/...` path.

## 3. Run

```sh
gateway -config config.yaml          # or: docker compose up -d
curl localhost:8080/healthz          # {"status":"ok","version":"…"}
```

## 4. Create an API key

Gateway keys are minted through the master-key-protected admin API. The plaintext key is
returned **exactly once** — only its hash and a short prefix are ever stored.

```sh
curl -X POST localhost:8080/admin/keys \
  -H "Authorization: Bearer $MASTER_KEY" \
  -d '{"name":"my-app","providers":["openai"]}'
# -> {"id":1,"name":"my-app","key":"sk-gw-…","prefix":"sk-gw-…","providers":["openai"],…}
```

## 5. Send a request

Call the gateway exactly as you'd call the upstream — same path, query, and body.
Authenticate with the gateway key via `Authorization: Bearer` or `x-api-key`:

```sh
curl localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-…" \
  -d '{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}'
```

Streaming (`"stream":true`, `text/event-stream`) is flushed through chunk-by-chunk. Successful
responses carry `X-AGL-Provider` and `X-AGL-Attempts` so a request that only succeeded after a
retry is still visible.

Point any SDK at the gateway by setting its base URL and using the gateway key, e.g.:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-gw-…")
```

## 6. Open the portal

Everything you can do over the admin API, you can do in the browser. The root path redirects
to the portal:

```sh
open http://localhost:8080        # redirects to /portal
```

Paste the **master key** to unlock it (kept in this browser's `localStorage` only), then
create and manage keys, browse request logs with their captured payloads, view usage/cost
stats, and run the live model test.

---

# Advanced configuration

## Routing across multiple providers

Bind a key to several providers and it is routed by the key's policy. `provider_start` decides
the first attempt; `provider_order` decides how retries walk the rest:

| Field            | Values                  | Default        |
|------------------|-------------------------|----------------|
| `provider_start` | `first` \| `random`     | `first`        |
| `provider_order` | `round_robin` \| `random` | `round_robin` |

```sh
curl -X POST localhost:8080/admin/keys \
  -H "Authorization: Bearer $MASTER_KEY" \
  -d '{"name":"my-app","providers":["openai","azure"],
       "provider_start":"first","provider_order":"round_robin"}'
```

Each retry fails over to the next provider in the sequence. The retry budget comes from the
**starting** provider's policy, so every provider is tried exactly once when
`max_retries >= providers-1`. The provider that served (or was last tried) is logged and
reported in `X-AGL-Provider`.

## Log retention on key deletion

Deleting a key removes its `request_logs` by default. To preserve a key's usage history past
its deletion, create the key with `keep_logs_on_delete: true` — the key row is removed but its
logs are kept as orphaned records (they retain the `key_name` captured at request time). The
default for keys that don't specify it comes from `defaults.keep_logs_on_key_delete` (itself
`false` unless set). It is a per-key setting fixed at creation:

```sh
curl -X POST localhost:8080/admin/keys \
  -H "Authorization: Bearer $MASTER_KEY" \
  -d '{"name":"audited","providers":["openai"],"keep_logs_on_delete":true}'
```

## Pinning a provider per request

To bypass the policy, send `X-AGL-Provider: <name>`. It must name one of the key's bound
providers (otherwise `400`), uses **only** that provider with no failover, and is consumed by
the gateway — never forwarded upstream.

## Model mapping

A provider can rewrite the request's top-level `model` field before forwarding, leaving every
other field byte-for-byte unchanged. Both the requested alias and the mapped upstream model
are logged.

```yaml
providers:
  - name: openai
    base_url: https://api.openai.com
    api_key: "sk-…"
    model_map:
      gpt-fast: gpt-5-mini      # clients ask for "gpt-fast"; upstream sees "gpt-5-mini"
```

## Retries & failover

```yaml
defaults:
  retry: { max_retries: 3, base_delay: 200ms, max_delay: 10s }
providers:
  - name: openai
    # …
    retry: { max_retries: 5 }   # optional per-provider override (unset fields fall back)
```

Network errors and HTTP 408/429/5xx (plus a couple of known LiteLLM/Azure quirks) are retried
with delay `base_delay * 2^attempt` capped at `max_delay`, with full jitter. **Retries never
happen after bytes have reached the client.**

### Failure semantics

When a request cannot be completed, the cause is explicit:

- **Provider failure** — unreachable after every retry, or a surviving 429/5xx. Carries
  `X-AGL-Error-Source: provider`, `X-AGL-Provider`, `X-AGL-Attempts`. If an HTTP response was
  obtained, the upstream's status and body pass through verbatim; otherwise the gateway
  synthesizes `{"error":{…,"source":"provider","attempts":N}}`.
- **Gateway failure** — missing/invalid key, a key bound to no configured provider, a body over
  the size limit, or an internal error. `source` is `gateway`, message prefixed `agl-gateway:`.

## Cost & metering

Token usage is normalized into four non-overlapping buckets — input, output, cache-read,
cache-write — and cost is a linear combination of the per-token rates in your `pricing` config.
With `model_map` in effect, cost is priced by the **requested** model (the alias), falling back
to the mapped upstream model only when the alias has no configured price. Models without
pricing are still logged, at zero cost. Metering is best-effort and never delays or breaks the
proxied response.

```yaml
pricing:
  - model: claude-opus-4-8
    input_cost_per_token: 5e-6
    output_cost_per_token: 2.5e-5
    cache_read_input_token_cost: 5e-7
    cache_creation_input_token_cost: 6.25e-6   # Anthropic-style cache writes
```

## Payload capture

Off by default — payloads can contain secrets and grow the database quickly. When enabled, the
inbound request body and final upstream response body are stored per log, capped by the
configured byte limits. With `assemble_streams: true`, recognized SSE responses for
`/v1/chat/completions`, `/v1/responses`, and `/v1/messages` also store a best-effort assembled
JSON response; unrecognized stream paths still store the raw stream.

```yaml
payload_capture:
  enabled: false
  max_request_bytes: 1048576
  max_response_bytes: 1048576
  assemble_streams: false
  max_assembled_bytes: 1048576
```

## Request-size limit

The gateway buffers the request body in memory so it can be replayed across failover, so it
caps the body to protect against OOM. Over-limit requests are rejected with `413` before any
upstream call.

```yaml
server:
  addr: ":8080"
  max_request_bytes: 104857600   # 100 MiB (default); 0 = default, negative = unlimited
```

## Scaling the datastore

The defaults (a single SQLite file) need no external services. Two config values scale it out
independently, with no other code changes — and each has an env-var override so secrets stay
out of the YAML:

```yaml
# api_keys + request_logs in PostgreSQL (override: AGL_DATABASE)
database: "postgres://user:pass@host:5432/agl"

# keep keys in `database`, but stream high-volume request_logs to ClickHouse, an append-only
# OLAP store suited to log analytics (override: AGL_LOGS_DATABASE)
logs_database: "clickhouse://user:pass@host:9000/agl"
```

A `postgres://` / `postgresql://` URL selects PostgreSQL; anything else is a SQLite path. For
`logs_database`, a `clickhouse://` / `clickhouses://` URL selects ClickHouse; a `postgres://`
URL or a file path are also accepted; empty keeps logs co-located with keys.

---

# Admin API

The control plane is authenticated by the **master key** (`Authorization: Bearer $MASTER_KEY`).

| Method | Path                | Body / query |
|--------|---------------------|--------------|
| POST   | `/admin/keys`       | `{"name","providers":[...],"provider_start","provider_order","keep_logs_on_delete"}` → returns the plaintext key once |
| GET    | `/admin/keys`       | list keys (no secret) |
| DELETE | `/admin/keys/{id}`  | delete a key (cascades to its logs and reclaims space, unless the key was created with `keep_logs_on_delete`, which retains them) |
| GET    | `/admin/logs`       | `?limit&offset&api_key_id&provider&since&include_payloads` — returns `{logs, limit, offset, has_more, next_offset}`; payload columns omitted unless `include_payloads=true` |
| GET    | `/admin/logs/{id}`  | one log with its captured request/response payloads |
| GET    | `/admin/stats`      | `?api_key_id&since` — aggregates grouped by key + model |
| GET    | `/admin/providers`  | configured providers + their live-discovered models |
| POST   | `/admin/test`       | run the model test server-side; streams NDJSON progress events |
| GET    | `/healthz`          | liveness (no auth); reports status and build version |

`since` accepts an RFC3339 timestamp, a Go duration window (e.g. `24h`), or unix millis.

**`GET /admin/providers`** returns `[{"name","models":[...],"error":"…"}]`. The model list is
discovered live and best-effort from each provider's OpenAI-compatible `/v1/models` endpoint
(probed concurrently with that provider's credentials), unioned with its configured `model_map`
aliases. A provider whose probe fails still appears, with `error` set.

**`POST /admin/test`** (the portal's *Test models* tab) exercises every model of every provider
through the gateway: one small request per `(provider, model)`, streaming pass/fail as each
completes. `claude*` models are sent as Anthropic Messages requests to `/v1/messages`,
everything else as OpenAI Responses requests to `/v1/responses`. Probes run under a temporary
key deleted when the run finishes, so a test run leaves no lasting log entries.

---

# Security

- Plaintext gateway keys are never stored — only their SHA-256 hash and a short display prefix.
  The full key is shown once at creation.
- The master key is compared in constant time.
- The portal keeps the master key in the browser's `localStorage` only; it is sent solely to
  this gateway's `/admin` endpoints. Serve the gateway over TLS in production (e.g. behind a
  reverse proxy).

# Development & contributing

Build, test, the portal UI workflow, the `modelcheck` harness, the Docker image, and
versioning are documented in **[AGENTS.md](AGENTS.md)**.
