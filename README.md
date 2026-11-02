# Model Relay

A self-hosted model gateway written in Go, with a React admin console. It exposes an OpenAI-compatible API, routes each request to a configured upstream provider, and records tokens, cost, latency and errors per API key.

![Dashboard](docs/screenshots/dashboard.png)

## Features

- **OpenAI-compatible relay** — `POST /v1/chat/completions` (streaming and non-streaming), `POST /v1/embeddings`, `GET /v1/models`.
- **Two upstream protocols** — OpenAI-compatible channels (OpenAI, DeepSeek, Moonshot, Qwen, vLLM, Ollama, …) and Anthropic Messages channels, translated to and from the OpenAI format.
- **Channel routing** — priority first, weighted load balancing inside a priority, public-to-upstream model aliases, and automatic failover on connection errors, `401`, `403`, `404`, `429` and `5xx`.
- **Gateway keys** — per-key USD quota, model allowlists, requests-per-minute limit, expiry, enable/disable, and a one-time plaintext reveal at creation.
- **Usage accounting** — prompt/completion tokens and cost from per-channel input/output prices, request logs with status, upstream model, latency and error detail.
- **Admin console** — dashboard with 7-day traffic, top models and recent requests; channel, model, API key and log pages.
- **Simple to operate** — one Go binary with the console embedded, SQLite storage, no Redis or Postgres, graceful shutdown.
- **Ship-ready basics** — Dockerfile, docker-compose example, Makefile and GitHub Actions CI.

## Quick start

Requirements for building from source: Go 1.25+, Node.js 22+ and pnpm 10+.

```bash
make build
./bin/gateway
```

Open <http://localhost:8080> and sign in with the development credentials `admin` / `admin123`.

> Set `SECRET_KEY` and `ADMIN_PASSWORD` before exposing an instance. The admin password is only used when the database is first created.

### First request

1. **Channels** → *Add channel*. Use an OpenAI-compatible base URL including the version, e.g. `https://api.openai.com/v1`, enter the upstream key and list the upstream models.
2. **API keys** → *Create key*. Copy the `sk-…` value shown once.
3. Call the relay:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

Streaming works with the usual OpenAI `"stream": true` flag. `GET /v1/models` lists the models exposed by enabled channels, filtered by the key's allowlist.

### Anthropic channels

Add a channel with protocol **Anthropic Messages**, base URL `https://api.anthropic.com/v1`, and a model mapping such as:

```text
claude-sonnet-4=claude-sonnet-4-20250514
```

Clients keep using the OpenAI chat format; the gateway translates requests and Anthropic SSE events in both directions. Public model names in mappings are also listed by `/v1/models`.

### Docker

```bash
docker build -t model-relay .
docker run --rm -p 8080:8080 \
  -e SECRET_KEY='replace-with-a-long-random-string' \
  -e ADMIN_PASSWORD='replace-me' \
  -v model-relay-data:/data \
  model-relay
```

For Compose, create a `.env` file containing `SECRET_KEY` and `ADMIN_PASSWORD` (see the configuration table), then run `docker compose up --build`. The compose file loads that file via `env_file` and refuses to start without it.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port. |
| `DATA_DIR` | `./data` | Directory for `gateway.db` (created automatically). |
| `SECRET_KEY` | `dev-secret-change-me` | Signs admin sessions and encrypts upstream API keys with AES-256-GCM. Use a stable secret. |
| `ADMIN_USERNAME` | `admin` | Admin username created on first boot. |
| `ADMIN_PASSWORD` | `admin123` | Admin password created on first boot (minimum 6 characters). |
| `SESSION_TTL_HOURS` | `24` | Admin session lifetime. |
| `REQUEST_TIMEOUT_SECONDS` | `300` | Timeout for non-streaming upstream requests. A timeout after the request may have reached the provider is booked as an unverified charge. |
| `STREAM_TIMEOUT_SECONDS` | `1800` | Hard overall limit for a streaming request. It bounds how long a stream can hold a quota reservation. Set to `0` to disable the cap. |
| `QUOTA_DEFAULT_OUTPUT_TOKENS` | `4096` | Output-token cap used for finite-quota keys when a chat request omits `max_tokens`/`max_completion_tokens`. The gateway sends this cap upstream so the quota reservation is a real upper bound. |

Changing `SECRET_KEY` invalidates admin sessions and makes existing channel keys undecryptable — re-enter the upstream keys after rotating it.

## Admin console

| Dashboard | Channels |
| --- | --- |
| ![Dashboard](docs/screenshots/dashboard.png) | ![Channels](docs/screenshots/channels.png) |

| API keys | Request logs |
| --- | --- |
| ![API keys](docs/screenshots/tokens.png) | ![Request logs](docs/screenshots/logs.png) |

The **Models** page shows the public catalog, serving channels and the lowest configured price per model. The screenshots above use the local mock upstream in `scripts/mock-upstream.py` (`python3 scripts/mock-upstream.py 19090`), so `127.0.0.1` URLs are demo data rather than real providers.

## Routing and accounting

- A channel serves a model when the model appears in its **models** list, or when it is a **public name in the model mapping**.
- Candidates are sorted by descending priority. Within one priority, weight influences a randomized order, so traffic is spread across equivalent channels.
- If an upstream call fails before any response bytes reach the client, the next candidate is tried. Streaming responses that have already started are not retried.
- Cost is calculated as `prompt_tokens × input_price / 1M + completion_tokens × output_price / 1M`, using prices configured on the channel that served the request.
- Upstream `usage` is used when present (`actual`). For streams without usage and for providers that omit it, the gateway records a conservative estimate based on the request and returned text (`estimated`).

### Quota reservations and settlement

Finite-quota keys are admitted against a balance that includes in-flight reservations:

```text
available = quota - used_quota - SUM(pending reservation amounts)
```

The reservation is created atomically in SQLite before the request is sent upstream, so two concurrent requests for the same key cannot spend the same balance. Before sending, the gateway computes a cost upper bound:

- Prompt tokens: the request body's byte length plus chat framing overhead. A token never encodes fewer than one byte, so this is a true upper bound (conservative for text and image payloads).
- Completion tokens: the client's `max_tokens`/`max_completion_tokens` (the larger one) multiplied by `n`. When the client omits a limit and the key has a finite quota, the gateway injects `max_tokens = QUOTA_DEFAULT_OUTPUT_TOKENS` so the upstream cannot generate more than was reserved.
- Price: the highest input/output price among the candidate channels, so a failover to a more expensive channel stays covered.

If the upper bound does not fit in the available balance, the request is rejected with HTTP `429`, `type: insufficient_quota`, and a message stating the required and available amounts. Requests are never serialized globally; unrelated keys and requests that fit their own balance proceed concurrently.

Every reservation is finalized exactly once, in one SQLite transaction that updates `used_quota` and writes the request log. A duplicate completion notification for the same reservation is ignored. The log records `cost_status`:

| `cost_status` | Meaning |
| --- | --- |
| `actual` | Upstream reported token usage; cost is computed from it. |
| `estimated` | Upstream omitted usage; cost is computed from locally counted request/response text. |
| `unverified` | Real usage is unknown after a timeout, cancellation, restart or a 2xx response whose body could not be read. The reserved upper bound is charged and surfaced for review. |
| `free` | No upstream usage was possible (rejected before the call, dial/DNS failure) or the upstream rejected the request before generation. No charge. |

Failure handling:

- Pre-call failures (validation, authorization, rate limit, no channel, insufficient quota, key decryption, dial/DNS errors) are not charged.
- A non-2xx upstream response is retried on the next candidate and is not charged if all candidates fail. An upstream `400`/`422` is returned without a charge.
- Once an upstream returns `2xx`, the request is never retried on another channel. A stream that breaks, a client cancellation, or a timeout is settled from the usage received so far (`actual`/`estimated`) or at the reserved upper bound (`unverified`) when usage is unknowable. This avoids double-billing the same request.
- Channel failover before any response bytes charges only the channel that finally serves the request.

### Restart recovery

This is a single-node gateway, so a reservation found `pending` at startup cannot still be running. `RecoverPendingReservations` runs on every boot and, for each such request:

1. charges the reserved upper bound to the key,
2. marks the reservation `recovered` and the charge `unverified`,
3. writes an error log explaining that the process restarted before completion.

Settled charges are committed in SQLite and are not lost. In-flight holds are therefore never released for free and never lock a key forever. The admin ledger endpoint `GET /api/ledger` lists recent reservations and their states; the dashboard shows unverified spend separately from the note in the Spend card, and the Request logs page shows the cost basis for every call.

A key with quota `-1` is unlimited: it still gets reservations and records usage, but it is never rejected for balance. `rpm_limit: 0` means unlimited. Model allowlists and RPM limits are enforced before the reservation and are unchanged.

### Known limits

- Reservations use a byte-based prompt-token upper bound. It is safe but deliberately conservative, especially for large image payloads; a finite-quota key may be able to keep less work in flight than its nominal balance suggests.
- A provider that ignores `max_tokens` (or bills for a failed request) can exceed the reserved upper bound. The gateway still settles the actual reported cost and logs a warning, but it cannot prevent an upstream that violates its own limits from overcharging.
- A non-2xx upstream response during failover is treated as unbilled because no usage is reported. Providers that charge for rejected or failed requests cannot be detected from a non-2xx response; those charges are invisible to the gateway and must be reconciled with the provider.
- Prompt upper bounds count the serialized body, so multimodal inputs and large JSON payloads reserve much more than their eventual token cost. This is deliberate: it keeps the hold conservative, and the unused amount is released at settlement.
- `unverified` charges are conservative by design: when a timeout or cancellation happens after the request may have reached the provider, the reserved upper bound is charged instead of silently recording zero. Review them in Request logs; the gateway does not currently provide an automatic refund workflow.
- The reservations are enforced with SQLite transactions on one node. Multi-node deployments would need a shared transactional store and are out of scope.

## HTTP API

Admin endpoints use the `mr_session` HttpOnly cookie created by `POST /api/auth/login`. Gateway endpoints use `Authorization: Bearer sk-…` (or `x-api-key`).

| Endpoint | Auth | Purpose |
| --- | --- | --- |
| `POST /api/auth/login` | — | Sign in; sets the session cookie. |
| `POST /api/auth/logout` | session | Clear the session cookie. |
| `GET /api/auth/me` | session | Current admin. |
| `GET /api/dashboard` | session | Totals, 7-day series, top models and recent logs. |
| `GET/POST /api/channels` | session | List/create channels. |
| `GET/PUT/DELETE /api/channels/{id}` | session | Read/update/delete a channel. |
| `POST /api/channels/{id}/test` | session | Send a minimal upstream request and report latency. |
| `GET/POST /api/tokens` | session | List/create gateway keys. Creation returns the plaintext key once. |
| `PUT/DELETE /api/tokens/{id}` | session | Update/delete a key. |
| `GET /api/logs` | session | Paginated logs; filters: `model`, `status`, `token_id`, `start`, `end`, `page`, `page_size`. |
| `GET /api/ledger` | session | Recent quota reservations and pending cost; `limit` defaults to 100. |
| `GET /api/models` | session | Model catalog with channels, protocols and prices. |
| `GET /v1/models` | gateway key | OpenAI model list, filtered by the key. |
| `POST /v1/chat/completions` | gateway key | OpenAI-compatible chat completions, streaming supported. |
| `POST /v1/embeddings` | gateway key | Embeddings passthrough for OpenAI-compatible channels. |
| `GET /healthz` | — | Liveness check. |

Gateway errors use the OpenAI error envelope, e.g. `{"error":{"message":"…","type":"invalid_api_key","code":"invalid_api_key"}}`.

## Development

```bash
make test        # Go tests + TypeScript typecheck
make vet         # go vet
make build       # web build + embedded single binary
make dev         # Vite dev server on :5173, proxying /api and /v1 to :8080
```

Run the backend separately with `go run ./cmd/server` (serves a "console not built" placeholder unless built with `-tags embed`).

`scripts/quota-test-upstream.py` is a deterministic local upstream for quota, streaming and recovery checks. Model names select behaviour (`ok-model`, `no-usage-model`, `stream-break-model`, `fail-model`, `slow-model`, `embed-model`); see the script header for details.

Project layout:

```text
cmd/server/            entrypoint
internal/auth/         password hashing, API keys, session JWTs
internal/config/       environment configuration
internal/gateway/      relay, routing, OpenAI/Anthropic adapters, accounting
internal/secretbox/    AES-GCM encryption for upstream keys
internal/server/       HTTP API, middleware, static console
internal/store/        SQLite schema and queries
web/                   React + TypeScript admin console (Vite)
scripts/               development helpers
docs/screenshots/      console screenshots used in this README
```

## Security notes and limitations

- This is a focused self-hosted gateway, not a hosted billing platform. There is **one admin account**; API keys do not belong to separate end users, and there is no registration, team or role system.
- SQLite uses a single writer. It is a good fit for a small team or a single-node deployment; horizontal scaling would require an external database and shared rate-limit state, which are not implemented.
- The default `SECRET_KEY` and `ADMIN_PASSWORD` are development values. Use a long random secret and a strong password, terminate TLS in front of the gateway, and treat `SECRET_KEY` as a credential.
- Upstream channel URLs are configured by an authenticated admin. The gateway will fetch whatever URL is configured, so restrict admin access accordingly.
- The Anthropic adapter translates text, image data URLs, tool calls and tool results. Remote image URLs are not converted. OpenAI's Responses API, audio, rerank and native Anthropic endpoints are out of scope.
- Cost accounting trusts the configured prices and upstream token counts; it is an operational estimate, not an invoice. `estimated` and `unverified` charges are marked explicitly and should be reviewed before using the numbers for billing.
- Channel failover is retried for listed transient statuses. A `400`/`422` from an upstream is returned to the client because it usually reflects the request rather than channel health.

## Upgrading an existing installation

The SQLite schema is migrated automatically on startup: the gateway adds `logs.cost_status`, `logs.reserved_cost`, `logs.reservation_id` and the `token_reservations` table when they are missing. No manual database step is required.

Existing keys and channels keep their configuration. Because the previous version had no reservations, the first boot after the upgrade has no pending reservations to recover; settled historical costs remain as recorded. New optional environment variables (`STREAM_TIMEOUT_SECONDS`, `QUOTA_DEFAULT_OUTPUT_TOKENS`) default to the values in the table above. Restart the binary (or rebuild the Docker image) as usual.

## Design notes

The channel/key/log model and admin flows are inspired by [new-api](https://github.com/QuantumNous/new-api); the "translate each provider to the OpenAI format" adapter approach follows the spirit of [LiteLLM](https://github.com/BerriAI/litellm). This repository is an independent Go + React implementation.
