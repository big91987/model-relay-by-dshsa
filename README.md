# Model Relay

A self-hosted model gateway written in Go, with a React admin console. It exposes an OpenAI-compatible API, routes each request to a configured upstream provider, and records tokens, cost, latency and errors per API key.

![Dashboard](docs/screenshots/dashboard.png)

## Features

- **OpenAI-compatible relay** — `POST /v1/chat/completions` (streaming and non-streaming), `POST /v1/embeddings`, `GET /v1/models`.
- **Two chat upstream protocols** — OpenAI-compatible channels (OpenAI, DeepSeek, Moonshot, Qwen, vLLM, Ollama, …) and Anthropic Messages channels, translated to and from the OpenAI format.
- **MiniMax-H3 video generation** — text-to-video and first-frame image-to-video from the console, with asynchronous tasks, live status, video preview and MP4 download.
- **Channel routing** — priority first, weighted load balancing inside a priority, public-to-upstream model aliases, and automatic failover on connection errors, `401`, `403`, `404`, `429` and `5xx`.
- **Upstream model discovery** — fetch the provider's model list in the channel form, tick the models to import, and keep manual entries and public aliases.
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

1. **Channels** → *Add channel*. Use an OpenAI-compatible base URL including the version, e.g. `https://api.openai.com/v1`, enter the upstream key and list the upstream models — or click *Fetch models* and tick the ones to import.
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

### MiniMax-H3 video generation

Add a channel with protocol **MiniMax H3 (video)**, base URL `https://api.minimax.io` and model `MiniMax-H3` (or one of the H3 variants). The API key is sent as `Authorization: Bearer …` to the MiniMax V2 video endpoints. Video channels are separate from chat routing, so video models are not exposed by `/v1/chat/completions` or `/v1/models`.

Open **Video** in the console to:

1. pick a MiniMax-H3 model and enter a prompt;
2. optionally attach a first-frame image (PNG, JPEG, WEBP, HEIC or HEIF, up to 30 MB) for image-to-video;
3. choose resolution (768P/2K for `MiniMax-H3`), duration (4–15 s) and, for text-to-video, an aspect ratio.

The gateway submits the asynchronous task, polls it in the background, and stores the finished MP4 under `DATA_DIR/videos/`. Finished videos can be previewed in the browser and downloaded with the **Download MP4** action. Tasks left in flight are resumed after a restart. Video generation is console-only for now: it is not metered per gateway key and does not appear in request logs.

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
| `DATA_DIR` | `./data` | Directory for `gateway.db` and downloaded videos (created automatically). |
| `SECRET_KEY` | `dev-secret-change-me` | Signs admin sessions and encrypts upstream API keys with AES-256-GCM. Use a stable secret. |
| `ADMIN_USERNAME` | `admin` | Admin username created on first boot. |
| `ADMIN_PASSWORD` | `admin123` | Admin password created on first boot (minimum 6 characters). |
| `SESSION_TTL_HOURS` | `24` | Admin session lifetime. |
| `REQUEST_TIMEOUT_SECONDS` | `300` | Timeout for non-streaming upstream requests. Streaming is bounded by the client connection. |
| `VIDEO_POLL_SECONDS` | `8` | How often asynchronous MiniMax video tasks are polled. |

Changing `SECRET_KEY` invalidates admin sessions and makes existing channel keys undecryptable — re-enter the upstream keys after rotating it.

## Admin console

| Dashboard | Channels |
| --- | --- |
| ![Dashboard](docs/screenshots/dashboard.png) | ![Channels](docs/screenshots/channels.png) |

| API keys | Request logs |
| --- | --- |
| ![API keys](docs/screenshots/tokens.png) | ![Request logs](docs/screenshots/logs.png) |

| Video generation | MiniMax H3 channel |
| --- | --- |
| ![Video generation](docs/screenshots/video-generation.png) | ![MiniMax H3 channel](docs/screenshots/video-channel.png) |

The **Models** page shows the public catalog, serving channels and the lowest configured price per model. The screenshots above use the local mock upstream in `scripts/mock-upstream.py` (`python3 scripts/mock-upstream.py 19090`), so `127.0.0.1` URLs are demo data rather than real providers. The mock also answers MiniMax video create/query calls and serves `scripts/media/demo.mp4` for local previews; the video screenshot additionally includes one real MiniMax-H3 generation.

Use the theme control in the top bar, or on the sign-in screen, to switch between light, dark and follow-system themes. The console remembers the selection in the browser.

The channel form can fetch the provider's model list and import selected models with checkboxes. Manual entries and model aliases stay editable, and a failed fetch leaves the form and the saved channel untouched.

![Fetching upstream models in the channel form](docs/screenshots/channel-modal-fetch.png)

## Routing and accounting

- A channel serves a model when the model appears in its **models** list, or when it is a **public name in the model mapping**.
- Candidates are sorted by descending priority. Within one priority, weight influences a randomized order, so traffic is spread across equivalent channels.
- If an upstream call fails before any response bytes reach the client, the next candidate is tried. Streaming responses that have already started are not retried.
- Cost is calculated as `prompt_tokens × input_price / 1M + completion_tokens × output_price / 1M`, using prices configured on the channel that served the request.
- Upstream `usage` is used when present. For streams without usage and for providers that omit it, the gateway records a conservative estimate based on the request and returned text.
- A key with quota `-1` is unlimited. A negative balance does not block in-flight work; the next request is rejected until the quota is raised. `rpm_limit: 0` means unlimited.
- Quota is enforced from recorded usage, so concurrent in-flight requests can slightly overshoot a key's quota before the next request is blocked.

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
| `POST /api/channels/upstream-models` | session | Fetch the provider's model list for the form; an existing channel reuses its stored key. |
| `POST /api/channels/{id}/test` | session | Send a minimal upstream request and report latency. |
| `GET/POST /api/tokens` | session | List/create gateway keys. Creation returns the plaintext key once. |
| `PUT/DELETE /api/tokens/{id}` | session | Update/delete a key. |
| `GET /api/logs` | session | Paginated logs; filters: `model`, `status`, `token_id`, `start`, `end`, `page`, `page_size`. |
| `GET /api/models` | session | Model catalog with channels, protocols and prices. |
| `GET/POST /api/videos` | session | List video tasks / submit a MiniMax-H3 generation task. |
| `GET /api/videos/{id}` | session | One video task with status and metadata. |
| `GET /api/videos/{id}/content` | session | Preview the stored MP4; add `?download=1` for an attachment. |
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

Project layout:

```text
cmd/server/            entrypoint
internal/auth/         password hashing, API keys, session JWTs
internal/config/       environment configuration
internal/gateway/      relay, routing, OpenAI/Anthropic adapters, MiniMax video tasks, accounting
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
- MiniMax-H3 video tasks are managed from the admin console; there is no gateway-key video API and video generation is not charged against key quotas. Task history and downloaded MP4 files stay in `DATA_DIR` until removed manually.
- Cost accounting trusts the configured prices and upstream token counts; it is an operational estimate, not an invoice.
- Channel failover is retried for listed transient statuses. A `400`/`422` from an upstream is returned to the client because it usually reflects the request rather than channel health.

## Design notes

The channel/key/log model and admin flows are inspired by [new-api](https://github.com/QuantumNous/new-api); the "translate each provider to the OpenAI format" adapter approach follows the spirit of [LiteLLM](https://github.com/BerriAI/litellm). This repository is an independent Go + React implementation.
