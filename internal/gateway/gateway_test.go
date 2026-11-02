package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"
	"github.com/big91987/model-relay-by-dshsa/internal/config"
	"github.com/big91987/model-relay-by-dshsa/internal/secretbox"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

type testEnv struct {
	gw     *Gateway
	st     *store.Store
	cipher *secretbox.Cipher
	token  *store.Token
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cipher, err := secretbox.New("test-secret")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	cfg := &config.Config{SecretKey: "test-secret"}
	gw := New(cfg, st, cipher, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, prefix, hash, _ := auth.GenerateAPIKey()
	token := &store.Token{
		UserID: 1, Name: "test-key", KeyHash: hash, KeyPrefix: prefix,
		Status: "active", Quota: -1,
	}
	if err := st.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return &testEnv{gw: gw, st: st, cipher: cipher, token: token}
}

func (e *testEnv) addChannel(t *testing.T, name, baseURL, typ string, priority int, models []string, mapping map[string]string, inputPrice, outputPrice float64) *store.Channel {
	t.Helper()
	encrypted, err := e.cipher.Encrypt("upstream-key")
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	channel := &store.Channel{
		Name: name, Type: typ, BaseURL: baseURL,
		APIKeyEncrypted: encrypted, APIKeyHint: "••••-key",
		Models: models, ModelMapping: mapping, Priority: priority, Weight: 1,
		Status: "enabled", InputPrice: inputPrice, OutputPrice: outputPrice,
	}
	if err := e.st.CreateChannel(channel); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return channel
}

func (e *testEnv) chat(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.gw.ChatCompletions(rec, req, e.token)
	return rec
}

func (e *testEnv) logs(t *testing.T) []store.Log {
	t.Helper()
	logs, _, err := e.st.ListLogs(store.LogFilter{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	return logs
}

func TestChatCompletionsNonStream(t *testing.T) {
	env := newTestEnv(t)
	var gotPath, gotAuth string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"public-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "primary", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)

	rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"content":"hello"`) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer upstream-key" {
		t.Fatalf("upstream request path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["model"] != "public-model" {
		t.Fatalf("upstream model = %v", gotBody["model"])
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing request id header")
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].Status != "success" || logs[0].PromptTokens != 10 || logs[0].CompletionTokens != 5 {
		t.Fatalf("unexpected logs: %+v", logs)
	}
	if logs[0].Cost != 0.00002 {
		t.Fatalf("unexpected cost: %v", logs[0].Cost)
	}
	token, err := env.st.GetTokenByID(env.token.ID)
	if err != nil || token.UsedQuota != 0.00002 {
		t.Fatalf("used quota = %v err=%v", token.UsedQuota, err)
	}
}

func TestChatCompletionsModelMapping(t *testing.T) {
	env := newTestEnv(t)
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "mapped", upstream.URL+"/v1", "openai", 0, []string{"upstream-name"}, map[string]string{"public-name": "upstream-name"}, 0, 0)

	rec := env.chat(t, `{"model":"public-name","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if gotBody["model"] != "upstream-name" {
		t.Fatalf("expected mapped upstream model, got %v", gotBody["model"])
	}
}

func TestChatCompletionsStream(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
		}
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()
	env.addChannel(t, "stream", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 0)

	rec := env.chat(t, `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected stream body: %s", body)
	}
	logs := env.logs(t)
	if len(logs) != 1 || !logs[0].Stream || logs[0].PromptTokens != 7 || logs[0].CompletionTokens != 3 {
		t.Fatalf("unexpected logs: %+v", logs)
	}
}

func TestChatCompletionsFailover(t *testing.T) {
	env := newTestEnv(t)
	primaryHits := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits++
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"from backup"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
	}))
	defer backup.Close()
	env.addChannel(t, "primary", primary.URL+"/v1", "openai", 10, []string{"public-model"}, nil, 0, 0)
	env.addChannel(t, "backup", backup.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 0)

	rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from backup") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if primaryHits != 1 {
		t.Fatalf("expected primary to be tried once, got %d", primaryHits)
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].ChannelName != "backup" || logs[0].Status != "success" {
		t.Fatalf("unexpected logs: %+v", logs)
	}
}

func TestChatCompletionsAllChannelsFail(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusBadGateway)
	}))
	defer upstream.Close()
	env.addChannel(t, "broken", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 0)

	rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].Status != "error" || logs[0].StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected logs: %+v", logs)
	}
}

func TestChatCompletionsModelNotAllowed(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called")
	}))
	defer upstream.Close()
	env.addChannel(t, "primary", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 0)
	env.token.AllowedModels = []string{"another-model"}
	if err := env.st.UpdateToken(env.token); err != nil {
		t.Fatalf("update token: %v", err)
	}

	rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatCompletionsQuotaAndRateLimit(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "primary", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)

	env.token.Quota = 0
	if err := env.st.UpdateToken(env.token); err != nil {
		t.Fatalf("update token: %v", err)
	}
	if rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected quota rejection, got %d: %s", rec.Code, rec.Body.String())
	}
	env.token.Quota = -1
	env.token.RPMLimit = 1
	if err := env.st.UpdateToken(env.token); err != nil {
		t.Fatalf("update token: %v", err)
	}
	if rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("expected first request to pass, got %d", rec.Code)
	}
	if rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected rate limit, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatCompletionsStreamFallbackForNonStreamingUpstream(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-fallback","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"streamed mock"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "fallback", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 0)

	rec := env.chat(t, `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("expected an SSE content type, got %q", rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "streamed mock") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected fallback stream: %s", body)
	}
	logs := env.logs(t)
	if len(logs) != 1 || !logs[0].Stream || logs[0].PromptTokens != 5 || logs[0].CompletionTokens != 2 {
		t.Fatalf("unexpected logs: %+v", logs)
	}
}

func TestChatCompletionsAnthropicTranslation(t *testing.T) {
	env := newTestEnv(t)
	var gotPath, gotKey, gotVersion string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"hello from claude"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":7}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "claude", upstream.URL+"/v1", "anthropic", 0, []string{"claude-upstream"}, map[string]string{"public-claude": "claude-upstream"}, 3, 15)

	rec := env.chat(t, `{"model":"public-claude","max_tokens":123,"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages" || gotKey != "upstream-key" || gotVersion == "" {
		t.Fatalf("unexpected upstream path=%q key=%q version=%q", gotPath, gotKey, gotVersion)
	}
	if gotBody["model"] != "claude-upstream" || gotBody["max_tokens"].(float64) != 123 || gotBody["system"] != "be nice" {
		t.Fatalf("unexpected upstream body: %+v", gotBody)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("client response is not JSON: %v", err)
	}
	choices := response["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello from claude" {
		t.Fatalf("unexpected translated content: %v", message["content"])
	}
	usage := response["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 12 || usage["completion_tokens"].(float64) != 7 {
		t.Fatalf("unexpected usage: %v", usage)
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].UpstreamModel != "claude-upstream" || logs[0].Model != "public-claude" {
		t.Fatalf("unexpected logs: %+v", logs)
	}
	if logs[0].Cost != 0.000141 {
		t.Fatalf("unexpected cost: %v", logs[0].Cost)
	}
}

func TestChatCompletionsAnthropicStream(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" there\"}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, event := range events {
			_, _ = io.WriteString(w, event)
			flusher.Flush()
		}
	}))
	defer upstream.Close()
	env.addChannel(t, "claude", upstream.URL+"/v1", "anthropic", 0, []string{"claude-upstream"}, map[string]string{"public-claude": "claude-upstream"}, 0, 0)

	rec := env.chat(t, `{"model":"public-claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hi"`) || !strings.Contains(body, `"content":" there"`) {
		t.Fatalf("missing translated deltas: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing finish markers: %s", body)
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].PromptTokens != 9 || logs[0].CompletionTokens != 2 || !logs[0].Stream {
		t.Fatalf("unexpected logs: %+v", logs)
	}
}

func TestBuildAnthropicRequestWithTools(t *testing.T) {
	raw := map[string]any{
		"model":      "public",
		"max_tokens": float64(64),
		"messages": []any{
			map[string]any{"role": "user", "content": "weather?"},
			map[string]any{
				"role":    "assistant",
				"content": "let me check",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "get_weather",
							"arguments": `{"city":"Paris"}`,
						},
					},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name":        "get_weather",
				"description": "Look up weather",
				"parameters":  map[string]any{"type": "object"},
			}},
		},
		"tool_choice": "required",
	}
	encoded, err := buildAnthropicRequest(raw, "claude-upstream")
	if err != nil {
		t.Fatalf("buildAnthropicRequest: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["model"] != "claude-upstream" || out["max_tokens"].(float64) != 64 {
		t.Fatalf("unexpected top-level fields: %+v", out)
	}
	messages := out["messages"].([]any)
	assistant := messages[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if len(blocks) != 2 || blocks[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("unexpected assistant blocks: %+v", blocks)
	}
	toolResult := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "call_1" {
		t.Fatalf("unexpected tool result: %+v", toolResult)
	}
	tools := out["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	choice := out["tool_choice"].(map[string]any)
	if choice["type"] != "any" {
		t.Fatalf("unexpected tool choice: %+v", choice)
	}
}

func TestBuildAnthropicRequestToolChoiceNone(t *testing.T) {
	raw := map[string]any{
		"model": "public",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name":       "lookup",
				"parameters": map[string]any{"type": "object"},
			}},
		},
		"tool_choice": "none",
	}
	encoded, err := buildAnthropicRequest(raw, "claude-upstream")
	if err != nil {
		t.Fatalf("buildAnthropicRequest: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, exists := out["tools"]; exists {
		t.Fatalf("tools must be omitted when tool_choice is none: %+v", out)
	}
}

func TestModelsListing(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "one", "https://one.example/v1", "openai", 0, []string{"model-a"}, map[string]string{"model-b": "upstream-b"}, 0, 0)
	rec := httptest.NewRecorder()
	env.gw.Models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil), env.token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"model-a"`) || !strings.Contains(rec.Body.String(), `"model-b"`) {
		t.Fatalf("unexpected models: %s", rec.Body.String())
	}
	env.token.AllowedModels = []string{"model-a"}
	rec = httptest.NewRecorder()
	env.gw.Models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil), env.token)
	if strings.Contains(rec.Body.String(), `"model-b"`) {
		t.Fatalf("restricted model leaked: %s", rec.Body.String())
	}
}
