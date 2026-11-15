# Playground: usage guide and verification record

The Playground is the **Playground** page in the admin console. After the admin selects an existing gateway key, chat requests go through the admin-authenticated `POST /api/playground/chat` endpoint, which invokes the relay's normal chat handler with that key. The browser sends only the key id; the key secret stays on the server. Model allowlist, quota, RPM limit, channel routing and request logs apply exactly as they do for an external API client. The page never calls an upstream directly.

## How to use it

1. Sign in to the console and open **Playground**.
2. Select an existing gateway key from **Gateway key**. If the list is empty, create a key on **API keys** first. The browser sends only the key id; the relay uses the key server-side and never exposes its secret to the page.
3. Pick a model, optionally edit the **System prompt**, **Temperature** and **Max output tokens**, then send a message. Later turns include the earlier conversation as context.
4. While a reply is streaming, **Stop generating** aborts the browser read and cancels the relay request context, which cancels the upstream request where the provider honors it. Content received before stopping may still be billed.
5. **Regenerate** asks for confirmation before replacing the last assistant reply. It creates a new request using the parameters currently shown in the page. The page never retries automatically.
6. Each completed reply shows the relay request ID, round-trip latency and token usage. **View log** opens **Logs** filtered by that request ID. If the upstream omits usage, the relay marks its synthesized final usage chunk with `x_usage_source: "estimated"` and the page labels the number as estimated.
7. **Clear conversation** removes the messages on this page; it does not delete request logs or charges.
8. **Export Markdown** downloads the current transcript as a `.md` file: user messages and model replies in order, with each assistant reply labelled by the model that produced it. Stopped or failed replies include a short status note. The export is generated in the browser from the messages on the page; it sends nothing to the server and does not include the system prompt or the page's current parameter values.

## Key and privacy behavior

- The browser never receives the gateway key secret. It sends only the selected key's numeric id to the admin-authenticated Playground endpoints.
- Gateway keys remain hashed in the database; upstream channel keys remain encrypted. Neither secret is returned by the admin API.
- The selected key's id and metadata (name, prefix, status, quota, allowlist) are visible in the page; the secret is not. The id is not written to browser storage. The normal relay request log records the key id/name as usual.
- The Playground endpoint requires the existing admin session. An admin can therefore spend the quota of any key in the console; restrict admin access accordingly.
- Talk to the gateway over TLS in production; the admin session cookie should be protected like any other admin credential.

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
| 1 | `8c7eeb3455922e12b610fab2` | `Nice to meet you, Ada.` | `94 / 86 / 180`, upstream-reported | `1.1 s` |
| 2 | `2c0cf2569ad55d5fbf0e5971` | `Ada` | `122 / 32 / 154`, upstream-reported | `0.96 s` |

Turn 2 includes the first turn: `GET /api/logs?request_id=2c0cf2569ad55d5fbf0e5971` returned one row with `prompt_tokens: 122`, `completion_tokens: 32`, `total_tokens: 154`, `stream: true`, status `success`, channel `Pollinations (GPT-OSS 20B)` and latency `0.96 s` as shown in the log row. The higher prompt count on turn 2 (122 vs 94) confirms history was sent. The browser received incremental SSE chunks rather than one buffered body.

The same flow was also verified against a local Ollama `qwen:14b` model before the external check:

| Turn | Request ID | Assistant text | Usage |
| --- | --- | --- | --- |
| 1 | `e760c809c717c8f6d52bf759` | `Hello Ada!` | `35 / 4 / 39` |
| 2 | `0174fc040b89d39f7379ba43` | `Ada.` | `57 / 3 / 60` |

Both requests were logged as `success` with `stream: true`; the second prompt count includes the first turn.

### 2. Stop generation, then continue

Screenshot: [Stopped generation state](screenshots/playground-stopped.png)

A controlled upstream streamed word by word. After the assistant had produced `This is a deliberately slow`, **Stop generating** was clicked. The page showed `Generation stopped. Content already received may still be billed.`, kept the partial text, disabled the stop button and re-enabled **Send**. The upstream request and relay request were canceled: the request log has `request_id: f7717f3961b1caf2a8bfeed3`, `status: error`, `status_code: 200`, `error: "stream interrupted: context canceled"`, `latency_ms: 1455`. No second request was sent automatically. Sending another message after the stop succeeded and received a fresh complete response.

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
| Quota exhausted | `Insufficient quota: insufficient quota for this gateway key` | `1060b141cb59ec6870d6ed19`, log HTTP 429 |
| RPM limit | First request succeeded; second showed `Rate limited: rate limit exceeded (1 requests per minute)` | `4b76289f873c7b6bf6eeb442` success, `7e95a92cdcc8a0ffe576f291` log HTTP 429 |
| Upstream failure | `Upstream unavailable: no upstream channel succeeded. Check the serving channel and open the request log for details.` | `767d95eedbd8c5a6dc95f347`, log HTTP 502, channel connection refused |
| Connection interrupted mid-stream | `Connection interrupted: the upstream stream ended before the response finished. Partial content is shown above. (upstream stream interrupted: unexpected EOF)` | Request ID shown; log status `error`, `stream interrupted: unexpected EOF` |
| Model not allowed for the key | `This key is not allowed to use the selected model: model "abrupt-model" is not allowed by this gateway key` | `bb657361b71dfca93b9c700d`, log HTTP 403 |

### 6. Request-log correlation

Screenshot: [Logs filtered by the request ID from Playground](screenshots/playground-logs.png)

The **View log** link from the second real turn opened `/logs?request_id=2c0cf2569ad55d5fbf0e5971`, pre-filled the new request-ID filter and showed exactly one row for that request (model `openai-fast`, channel `Pollinations (GPT-OSS 20B)`, 154 tokens, 0.96 s, success). The request-detail dialog also shows the full request ID with a **Copy** action. The API supports the same lookup:

```bash
curl -b cookies.txt 'http://127.0.0.1:8080/api/logs?request_id=2c0cf2569ad55d5fbf0e5971'
```

### 7. Key exposure check

The page has no secret-key input; it selects a key by id. A browser evaluation after selecting a key reported `localStorage` containing only `mr-theme` (the theme preference), empty `sessionStorage`, and URL `http://127.0.0.1:8080/playground`. Network inspection of both Playground chat requests shows `POST /api/playground/chat?token_id=1`; the request headers contain no `Authorization` header and the request body contains no key material. The gateway key is used server-side, and the rendered HTML does not contain it.

### 8. Markdown export

Screenshots: [baseline header without the export action](screenshots/playground-export-before.png) and [export action after a two-turn conversation](screenshots/playground-export-after.png).

**Export Markdown** downloads a file named `playground-conversation-<local date-time>.md`. The two-turn conversation in the screenshots produced this file:

```markdown
# Playground conversation

_Exported 2026-11-13 20:23 (local time)._

## User

这个网关能做什么？用要点说明。

## Assistant (gpt-4o)

Model Relay 是一个自托管的多模型网关，主要能力：

- 通过 OpenAI 兼容接口统一转发聊天与向量请求
- 在多个上游渠道之间按优先级和权重路由，并在失败时自动切换
- 按 API Key 统计 token、费用和延迟，并执行配额与限流
- 在管理台里试用模型、查看请求日志和生成视频

## User

给我一个最短的使用步骤。

## Assistant (gpt-4o)

三步即可开始：

1. 在 **Channels** 添加一个可用的上游渠道。
2. 在 **API keys** 创建一个网关 Key。
3. 用 `Authorization: Bearer sk-...` 调用 `/v1/chat/completions`。

> 提示：生产环境请更换 `SECRET_KEY` 和管理员密码。
```

Message order, role labels, the serving model and the replies' Markdown formatting are preserved. The download is generated in the browser from the in-memory transcript; the export action made no server request. Both captures use a 1440×1000 viewport, the dark console theme, the same mock upstream (`gpt-4o`) and the same two prompts.

## Before/after UI evidence

- **Before** (`88d42a6`, main): the sidebar had no Playground entry; `/playground` redirected to Dashboard. [Baseline screenshot](screenshots/playground-before.png)
- **After** (this branch): the sidebar exposes **Playground**; the page provides the flow documented above. [Same Dashboard viewport with the new navigation](screenshots/playground-after-nav.png) and [Playground](screenshots/playground.png).

Both pairs use a 1440×1000 browser viewport, the dark console theme, the same clean demo data and the same admin account. The Playground was also captured in light theme after switching the top-bar control; see [Playground in light theme](screenshots/playground-light.png) and [the matching light request log](screenshots/playground-logs-light.png).

## Known limitations

- A stopped generation is not guaranteed to be free; upstreams may bill tokens produced before cancellation. The UI says so explicitly.
- Estimated usage is approximate. The page labels it, but the estimate can differ from provider billing.
- Model-specific parameter limits are enforced by the upstream. The page constrains protocol-level ranges and displays the upstream error; it does not maintain a model capability catalogue.
- The Playground is an admin convenience page, not a multi-user chat product. It has no server-side conversation history and no sharing. The Markdown export covers only the messages currently on the page; reloading or clearing removes them.
- The external verification used a free public inference endpoint, so its latency is provider-dependent; the relay still records the real upstream request and response metadata.
