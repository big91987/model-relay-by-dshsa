# Playground: usage guide and verification record

The Playground is the **Playground** page in the admin console. It sends chat requests through the relay's OpenAI-compatible `POST /v1/chat/completions` endpoint with a gateway key, so it uses the same model allowlist, quota, RPM limit, channel routing and request log as any other API client. The page never calls an upstream directly.

## How to use it

1. Sign in to the console and open **Playground**.
2. Create a gateway key on **API keys** if needed, paste it into **Gateway connection** and choose **Validate key & load models**. The key is sent to the same origin as an `Authorization: Bearer` header and is used only while this page is open.
3. Pick a model, optionally edit the **System prompt**, **Temperature** and **Max output tokens**, then send a message. Later turns include the earlier conversation as context.
4. While a reply is streaming, **Stop generating** aborts the browser read and cancels the relay request context, which cancels the upstream request where the provider honors it. Content received before stopping may still be billed.
5. **Regenerate** asks for confirmation before replacing the last assistant reply. It creates a new request using the parameters currently shown in the page. The page never retries automatically.
6. Each completed reply shows the relay request ID, round-trip latency and token usage. **View log** opens **Logs** filtered by that request ID. If the upstream omits usage, the relay marks its synthesized final usage chunk with `x_usage_source: "estimated"` and the page labels the number as estimated.
7. **Clear conversation** removes the messages on this page; it does not delete request logs or charges.

## Key and privacy behavior

- The gateway key is held in React component state only. It is cleared when the page unmounts or is refreshed.
- It is not written to the URL, `localStorage`, `sessionStorage` or application logs. The console makes no `console.*` calls with it.
- Upstream channel keys remain encrypted on the server and are never returned by the admin API.
- Talk to the gateway over TLS in production; the browser necessarily sends the gateway key in the request header.

## Parameter behavior

- Temperature is limited to `0–2` for OpenAI-compatible models and `0–1` when the selected model is served by an Anthropic channel. Mixing both protocols on one model uses the conservative `0–1` cap.
- Maximum output length accepts `1–32768` or an empty value (model/relay default). The exact per-model limit is enforced by the upstream; if it is exceeded, the upstream error is displayed in the conversation.
- The UI does not maintain a per-model capability database. If a model rejects a parameter (for example, a reasoning model that does not accept temperature), its error is shown rather than hidden.

## Verification environment

- Console source: this ticket branch, built with `go build -tags embed` (version `0.1.0`).
- Gateway: single node, SQLite, `http://127.0.0.1:8080`.
- Real upstream for the main chat evidence: `openai-fast` (GPT-OSS 20B) through a local pass-through to the public Pollinations OpenAI-compatible endpoint. It is real inference, not a mocked response. A local Ollama `qwen:14b` instance was also used earlier for a second independent real-chat check.
- Controlled upstreams: `scripts/mock-upstream.py`-style slow, no-usage, echo and abrupt-disconnect servers were used only for stop, estimated-usage, parameter and error-path regression; they are not used as a substitute for the real-chat check.

## Evidence

### 1. Real two-turn chat with context and streaming

Screenshot: [Playground with a real two-turn conversation](screenshots/playground.png)

The same conversation in light theme: [Playground in light theme](screenshots/playground-light.png).

The page sent a system prompt and two user turns to `openai-fast`. The transcript in the screenshot is:

| Turn | Request ID | Assistant text | Usage (prompt / completion / total) | Latency |
| --- | --- | --- | --- | --- |
| 1 | `c53b8048b95547960d25ec49` | `Nice to meet you, Ada.` | `94 / 86 / 180`, upstream-reported | `1.9 s` |
| 2 | `435ec46c1ca0f188948dceeb` | `Ada` | `122 / 32 / 154`, upstream-reported | `1.2 s` |

Turn 2 includes the first turn: `GET /api/logs?request_id=435ec46c1ca0f188948dceeb` returned one row with `prompt_tokens: 122`, `completion_tokens: 32`, `total_tokens: 154`, `stream: true`, status `success`, channel `Pollinations (GPT-OSS 20B)` and latency `1.2 s` as shown in the log row. The higher prompt count on turn 2 (122 vs 94) confirms history was sent. The browser received incremental SSE chunks rather than one buffered body.

The same flow was also verified against a local Ollama `qwen:14b` model before the external check:

| Turn | Request ID | Assistant text | Usage |
| --- | --- | --- | --- |
| 1 | `e760c809c717c8f6d52bf759` | `Hello Ada!` | `35 / 4 / 39` |
| 2 | `0174fc040b89d39f7379ba43` | `Ada.` | `57 / 3 / 60` |

Both requests were logged as `success` with `stream: true`; the second prompt count includes the first turn.

### 2. Stop generation, then continue

Screenshot: [Stopped generation state](screenshots/playground-stopped.png)

A controlled upstream streamed word by word. After the assistant had produced `This is a deliberately slow`, **Stop generating** was clicked. The page showed `Generation stopped. Content already received may still be billed.`, kept the partial text, disabled the stop button and re-enabled **Send**. The upstream request and relay request were canceled: the request log has `request_id: f2ae33127af50f1b857e9e37`, `status: error`, `status_code: 200`, `error: "stream interrupted: context canceled"`, `latency_ms: 1453`. No second request was sent automatically. Sending another message after the stop succeeded and received a fresh complete response.

### 3. Parameters and regeneration

A controlled echo upstream returned the parameters it received. With the page showing temperature `0.3` and max output `64`, the assistant reply contained:

```text
received temperature=0.3 max_tokens=64 model=echo-model system='sys-params' history=2
```

After changing the UI to temperature `0.4` and max output `32`, **Regenerate** opened a confirmation dialog that showed those current values and the sentence “Nothing is retried automatically. A request is sent only after you click ‘Send new request’.” After confirmation, the new reply contained:

```text
received temperature=0.4 max_tokens=32 model=echo-model system='sys-params' history=2
```

The assistant metadata also showed `echo-model · temp 0.4 · max output 32`. This confirms that a retry is a new request using the current UI settings and that it does not silently duplicate a submission.

### 4. Token usage source

- Real external and Ollama replies that reported usage were shown without an “estimated” label and matched the relay log values.
- A controlled upstream that omitted usage produced the page label `Tokens 35 (15 in / 20 out) · estimated`, with the tooltip “The upstream did not report usage; the relay estimated it from the request and returned text.” The gateway's final SSE chunk contained `"x_usage_source":"estimated"`. No exact-looking number is invented.

### 5. Error feedback

All messages below were observed in the page, not only in server code:

| Scenario | UI feedback | Request ID / log |
| --- | --- | --- |
| Invalid gateway key | `Invalid or disabled gateway key: invalid API key` in the connection panel; chat disabled | `GET /v1/models` → 401 |
| Key without available models | Badge `No models`; `This key has no available models: its model allowlist may not match any enabled channel, or the channels are disabled.` | `GET /v1/models` → empty `data` |
| Quota exhausted | `Insufficient quota: insufficient quota for this gateway key` | `f410b2a5480ca7d7a2db69ef`, log HTTP 429 |
| RPM limit | First request succeeded; second showed `Rate limited: rate limit exceeded (1 requests per minute)` | `4b76289f873c7b6bf6eeb442` success, `7e95a92cdcc8a0ffe576f291` log HTTP 429 |
| Upstream failure | `Upstream unavailable: no upstream channel succeeded. Check the serving channel and open the request log for details.` | `767d95eedbd8c5a6dc95f347`, log HTTP 502, channel connection refused |
| Connection interrupted mid-stream | `Connection interrupted: the upstream stream ended before the response finished. Partial content is shown above. (upstream stream interrupted: unexpected EOF)` | Request ID shown; log status `error`, `stream interrupted: unexpected EOF` |
| Model not allowed for the key | `This key is not allowed to use the selected model: model "abrupt-model" is not allowed by this gateway key` | `bb657361b71dfca93b9c700d`, log HTTP 403 |

### 6. Request-log correlation

Screenshot: [Logs filtered by the request ID from Playground](screenshots/playground-logs.png)

The **View log** link from the second real turn opened `/logs?request_id=435ec46c1ca0f188948dceeb`, pre-filled the new request-ID filter and showed exactly one row for that request (model `openai-fast`, channel `Pollinations (GPT-OSS 20B)`, 154 tokens, 1.2 s, success). The request-detail dialog also shows the full request ID with a **Copy** action. The API supports the same lookup:

```bash
curl -b cookies.txt 'http://127.0.0.1:8080/api/logs?request_id=435ec46c1ca0f188948dceeb'
```

### 7. Key persistence check

After validating a key, a browser evaluation reported `localStorage: {}`, `sessionStorage: {}` and URL `http://127.0.0.1:8080/playground`. Reloading the page left the gateway-key input empty, reset the model selector and left both storage objects empty. The page source and rendered HTML do not contain the key. The only copy of the key in the browser is the component's in-memory state.

## Before/after UI evidence

- **Before** (`78c6dfb`, main): the sidebar had no Playground entry; `/playground` redirected to Dashboard. [Baseline screenshot](screenshots/playground-before.png)
- **After** (this branch): the sidebar exposes **Playground**; the page provides the flow documented above. [Same Dashboard viewport with the new navigation](screenshots/playground-after-nav.png) and [Playground](screenshots/playground.png).

Both pairs use a 1440×1000 browser viewport, the dark console theme, the same clean demo data and the same admin account. The Playground was also captured in light theme after switching the top-bar control; see [Playground in light theme](screenshots/playground-light.png) and [the matching light request log](screenshots/playground-logs-light.png).

## Known limitations

- A stopped generation is not guaranteed to be free; upstreams may bill tokens produced before cancellation. The UI says so explicitly.
- Estimated usage is approximate. The page labels it, but the estimate can differ from provider billing.
- Model-specific parameter limits are enforced by the upstream. The page constrains protocol-level ranges and displays the upstream error; it does not maintain a model capability catalogue.
- The Playground is an admin convenience page, not a multi-user chat product. It has no server-side conversation history and no sharing.
- The external verification used a free public inference endpoint, so its latency is provider-dependent; the relay still records the real upstream request and response metadata.
