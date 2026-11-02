// Package gateway relays OpenAI-compatible requests to configured upstream channels.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/config"
	"github.com/big91987/model-relay-by-dshsa/internal/secretbox"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

const maxChatBody = 32 << 20

// Gateway relays requests and records usage.
type Gateway struct {
	cfg     *config.Config
	store   *store.Store
	secrets *secretbox.Cipher
	client  *http.Client
	limiter *rateLimiter
	logger  *slog.Logger

	// videoPollInterval controls how often asynchronous video tasks are polled.
	// Tests shorten it; production uses MiniMax's recommended polling cadence.
	videoPollInterval time.Duration
}

// New builds a Gateway with a tuned upstream HTTP client.
func New(cfg *config.Config, st *store.Store, secrets *secretbox.Cipher, logger *slog.Logger) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	pollInterval := cfg.VideoPollInterval
	if pollInterval <= 0 {
		pollInterval = 8 * time.Second
	}
	return &Gateway{
		cfg:               cfg,
		store:             st,
		secrets:           secrets,
		client:            &http.Client{Transport: transport},
		limiter:           newRateLimiter(),
		logger:            logger,
		videoPollInterval: pollInterval,
	}
}

// openAIChatPayload is the subset of a chat request needed for routing and accounting.
type openAIChatPayload struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

type outcome struct {
	committed  bool
	status     int
	prompt     int64
	completion int64
	errMsg     string
}

// ChatCompletions handles POST /v1/chat/completions.
func (g *Gateway) ChatCompletions(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	requestID := newRequestID()
	w.Header().Set("X-Request-Id", requestID)
	started := time.Now()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatBody))
	if err != nil {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body is too large", "invalid_request_error", "body_too_large")
		g.record(tok, nil, requestID, "", "", false, "error", http.StatusRequestEntityTooLarge, "request body is too large", 0, 0, started)
		return
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", "invalid_json")
		g.record(tok, nil, requestID, "", "", false, "error", http.StatusBadRequest, "invalid JSON body", 0, 0, started)
		return
	}
	var payload openAIChatPayload
	// A second decode into the narrow struct cannot fail when the first succeeded.
	_ = json.Unmarshal(body, &payload)
	model := strings.TrimSpace(payload.Model)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "model_required")
		g.record(tok, nil, requestID, "", "", payload.Stream, "error", http.StatusBadRequest, "model is required", 0, 0, started)
		return
	}
	if code, msg := authorize(tok, model); code != 0 {
		writeOpenAIError(w, code, msg, authErrorType(code), "")
		g.record(tok, nil, requestID, model, "", payload.Stream, "error", code, msg, 0, 0, started)
		return
	}
	if !g.limiter.Allow(tok.ID, tok.RPMLimit) {
		msg := fmt.Sprintf("rate limit exceeded (%d requests per minute)", tok.RPMLimit)
		writeOpenAIError(w, http.StatusTooManyRequests, msg, "rate_limit_exceeded", "rate_limit_exceeded")
		g.record(tok, nil, requestID, model, "", payload.Stream, "error", http.StatusTooManyRequests, msg, 0, 0, started)
		return
	}

	channels, err := g.store.ListChannels()
	if err != nil {
		g.serverError(w, tok, requestID, model, payload.Stream, started, err)
		return
	}
	candidates := chatCandidates(orderCandidates(channels, model))
	if len(candidates) == 0 {
		msg := fmt.Sprintf("model %q is not available on any enabled channel", model)
		writeOpenAIError(w, http.StatusNotFound, msg, "invalid_request_error", "model_not_found")
		g.record(tok, nil, requestID, model, "", payload.Stream, "error", http.StatusNotFound, msg, 0, 0, started)
		return
	}

	promptTokens := estimateChatPrompt(payload)
	var lastErr string
	var lastStatus int
	var lastChannel *store.Channel

	for i := range candidates {
		cand := candidates[i]
		ch := cand.channel
		lastChannel = &ch
		apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
		if err != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, err)
			continue
		}
		var out outcome
		switch ch.Type {
		case "openai":
			out = g.attemptOpenAI(w, r, &ch, apiKey, cand.upstream, raw, payload.Stream, promptTokens)
		case "anthropic":
			out = g.attemptAnthropic(w, r, &ch, apiKey, cand.upstream, raw, promptTokens)
		default:
			lastErr = fmt.Sprintf("channel %q has unsupported type %q", ch.Name, ch.Type)
			continue
		}
		if out.committed {
			if out.errMsg != "" {
				g.record(tok, &ch, requestID, model, cand.upstream, payload.Stream, "error", http.StatusOK, out.errMsg, out.prompt, out.completion, started)
			} else {
				g.record(tok, &ch, requestID, model, cand.upstream, payload.Stream, "success", http.StatusOK, "", out.prompt, out.completion, started)
			}
			return
		}
		if out.status == 0 {
			lastErr = out.errMsg
			continue
		}
		if shouldFailover(out.status) {
			lastStatus = out.status
			lastErr = fmt.Sprintf("channel %q returned %d: %s", ch.Name, out.status, out.errMsg)
			continue
		}
		g.logger.Warn("upstream rejected request", "channel", ch.Name, "status", out.status, "error", out.errMsg)
		msg := out.errMsg
		if msg == "" {
			msg = http.StatusText(out.status)
		}
		writeOpenAIError(w, out.status, msg, "upstream_error", fmt.Sprintf("upstream_status_%d", out.status))
		g.record(tok, &ch, requestID, model, cand.upstream, payload.Stream, "error", out.status, msg, 0, 0, started)
		return
	}

	status := http.StatusBadGateway
	msg := "no upstream channel succeeded"
	if lastStatus == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
		msg = "all upstream channels are rate limited; please try again later"
	}
	g.logger.Error("all upstream channels failed", "model", model, "error", lastErr)
	writeOpenAIError(w, status, msg, "upstream_error", "upstream_unavailable")
	g.record(tok, lastChannel, requestID, model, "", payload.Stream, "error", status, msg, 0, 0, started)
}

// Embeddings handles POST /v1/embeddings (OpenAI-compatible channels only).
func (g *Gateway) Embeddings(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	requestID := newRequestID()
	w.Header().Set("X-Request-Id", requestID)
	started := time.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatBody))
	if err != nil {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body is too large", "invalid_request_error", "body_too_large")
		return
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", "invalid_json")
		return
	}
	model := strings.TrimSpace(payload.Model)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "model_required")
		return
	}
	if code, msg := authorize(tok, model); code != 0 {
		writeOpenAIError(w, code, msg, authErrorType(code), "")
		g.record(tok, nil, requestID, model, "", false, "error", code, msg, 0, 0, started)
		return
	}
	if !g.limiter.Allow(tok.ID, tok.RPMLimit) {
		msg := fmt.Sprintf("rate limit exceeded (%d requests per minute)", tok.RPMLimit)
		writeOpenAIError(w, http.StatusTooManyRequests, msg, "rate_limit_exceeded", "rate_limit_exceeded")
		return
	}
	channels, err := g.store.ListChannels()
	if err != nil {
		g.serverError(w, tok, requestID, model, false, started, err)
		return
	}
	var lastErr string
	var lastChannel *store.Channel
	for _, cand := range orderCandidates(channels, model) {
		ch := cand.channel
		if ch.Type != "openai" {
			continue
		}
		lastChannel = &ch
		apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		raw["model"] = cand.upstream
		forwardBody, _ := json.Marshal(raw)
		req, err := g.upstreamRequest(r.Context(), &ch, http.MethodPost, "/embeddings", forwardBody, apiKey, false, true)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		resp, err := g.client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			msg := readErrorBody(resp)
			resp.Body.Close()
			if shouldFailover(resp.StatusCode) {
				lastErr = msg
				continue
			}
			writeOpenAIError(w, resp.StatusCode, msg, "upstream_error", "upstream_error")
			g.record(tok, &ch, requestID, model, cand.upstream, false, "error", resp.StatusCode, msg, 0, 0, started)
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			lastErr = err.Error()
			continue
		}
		var parsed struct {
			Usage struct {
				PromptTokens int64 `json:"prompt_tokens"`
				TotalTokens  int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(data, &parsed)
		prompt := parsed.Usage.PromptTokens
		if prompt == 0 {
			prompt = estimateTokens(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
		g.record(tok, &ch, requestID, model, cand.upstream, false, "success", resp.StatusCode, "", prompt, 0, started)
		return
	}
	status := http.StatusBadGateway
	msg := "no upstream channel supports embeddings for this model"
	if lastErr != "" {
		msg = lastErr
	}
	writeOpenAIError(w, status, msg, "upstream_error", "upstream_unavailable")
	g.record(tok, lastChannel, requestID, model, "", false, "error", status, msg, 0, 0, started)
}

// Models handles GET /v1/models.
func (g *Gateway) Models(w http.ResponseWriter, r *http.Request, tok *store.Token) {
	channels, err := g.store.ListChannels()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to load channels", "server_error", "")
		return
	}
	seen := map[string]bool{}
	ids := make([]string, 0)
	add := func(id string) {
		if id == "" || seen[id] || !modelAllowed(tok, id) {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, ch := range channels {
		if ch.Status != "enabled" || !isChatChannel(ch.Type) {
			continue
		}
		for _, m := range ch.Models {
			add(m)
		}
		for m := range ch.ModelMapping {
			add(m)
		}
	}
	sortStrings(ids)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "model-relay",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// TestResult reports the outcome of an upstream connectivity test.
type TestResult struct {
	OK         bool   `json:"ok"`
	LatencyMS  int64  `json:"latency_ms"`
	StatusCode int    `json:"status_code"`
	Message    string `json:"message"`
}

// TestChannel sends a minimal request to a channel to verify credentials.
func (g *Gateway) TestChannel(ctx context.Context, ch *store.Channel, model string) TestResult {
	apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
	if err != nil {
		return TestResult{Message: "cannot decrypt API key: " + err.Error()}
	}
	upstream, ok := resolveModel(ch, model)
	if model == "" {
		if len(ch.Models) > 0 {
			upstream, ok = ch.Models[0], true
		} else {
			for public := range ch.ModelMapping {
				upstream, ok = resolveModel(ch, public)
				break
			}
		}
	}
	if !ok || upstream == "" {
		return TestResult{Message: fmt.Sprintf("model %q is not configured on this channel", model)}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	started := time.Now()
	var req *http.Request
	switch ch.Type {
	case "anthropic":
		body, _ := json.Marshal(map[string]any{
			"model":      upstream,
			"max_tokens": 8,
			"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		})
		req, err = g.upstreamRequest(ctx, ch, http.MethodPost, "/messages", body, apiKey, false, true)
	case "minimax":
		// A query for an unknown task verifies reachability and credentials
		// without creating a billable video generation task.
		req, err = g.upstreamRequest(ctx, ch, http.MethodGet, "/v2/query/video_generation/connectivity-probe", nil, apiKey, false, true)
	default:
		body, _ := json.Marshal(map[string]any{
			"model":      upstream,
			"max_tokens": 8,
			"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		})
		req, err = g.upstreamRequest(ctx, ch, http.MethodPost, "/chat/completions", body, apiKey, false, true)
	}
	if err != nil {
		return TestResult{Message: err.Error()}
	}
	resp, err := g.client.Do(req)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return TestResult{LatencyMS: latency, Message: err.Error()}
	}
	defer resp.Body.Close()
	excerpt := readErrorBody(resp)
	message := strings.TrimSpace(excerpt)
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	ok = resp.StatusCode >= 200 && resp.StatusCode < 300
	if ch.Type == "minimax" {
		// The probe task does not exist, so a validation error still proves
		// that the endpoint and key are valid; 401/403 do not.
		ok = ok || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity
	}
	return TestResult{OK: ok, LatencyMS: latency, StatusCode: resp.StatusCode, Message: message}
}

func (g *Gateway) attemptOpenAI(w http.ResponseWriter, r *http.Request, ch *store.Channel, apiKey, upstream string, raw map[string]any, stream bool, promptTokens int64) outcome {
	ctx, cancel := g.requestContext(r.Context(), stream)
	defer cancel()
	forward := make(map[string]any, len(raw))
	for k, v := range raw {
		forward[k] = v
	}
	forward["model"] = upstream
	body, err := json.Marshal(forward)
	if err != nil {
		return outcome{errMsg: "encode upstream request: " + err.Error()}
	}
	req, err := g.upstreamRequest(ctx, ch, http.MethodPost, "/chat/completions", body, apiKey, stream, true)
	if err != nil {
		return outcome{errMsg: err.Error()}
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return outcome{errMsg: err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := readErrorBody(resp)
		resp.Body.Close()
		return outcome{status: resp.StatusCode, errMsg: msg}
	}
	if stream {
		prompt, completion, err := g.streamOpenAI(w, resp, promptTokens, rawModel(raw))
		if err != nil {
			return outcome{committed: true, status: http.StatusOK, prompt: prompt, completion: completion, errMsg: "stream interrupted: " + err.Error()}
		}
		return outcome{committed: true, status: http.StatusOK, prompt: prompt, completion: completion}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	resp.Body.Close()
	if err != nil {
		return outcome{errMsg: "read upstream response: " + err.Error()}
	}
	prompt, completion := openAIUsage(data, promptTokens)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return outcome{committed: true, status: http.StatusOK, prompt: prompt, completion: completion}
}

func (g *Gateway) attemptAnthropic(w http.ResponseWriter, r *http.Request, ch *store.Channel, apiKey, upstream string, raw map[string]any, promptTokens int64) outcome {
	stream, _ := raw["stream"].(bool)
	ctx, cancel := g.requestContext(r.Context(), stream)
	defer cancel()
	body, err := buildAnthropicRequest(raw, upstream)
	if err != nil {
		return outcome{errMsg: "translate request for Anthropic: " + err.Error()}
	}
	req, err := g.upstreamRequest(ctx, ch, http.MethodPost, "/messages", body, apiKey, stream, false)
	if err != nil {
		return outcome{errMsg: err.Error()}
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return outcome{errMsg: err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := readErrorBody(resp)
		resp.Body.Close()
		return outcome{status: resp.StatusCode, errMsg: msg}
	}
	if stream {
		prompt, completion, err := g.streamAnthropic(w, resp, promptTokens, rawModel(raw))
		if err != nil {
			return outcome{committed: true, status: http.StatusOK, prompt: prompt, completion: completion, errMsg: "stream interrupted: " + err.Error()}
		}
		return outcome{committed: true, status: http.StatusOK, prompt: prompt, completion: completion}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	resp.Body.Close()
	if err != nil {
		return outcome{errMsg: "read upstream response: " + err.Error()}
	}
	translated, prompt, completion, err := anthropicToOpenAI(data, rawModel(raw), promptTokens)
	if err != nil {
		return outcome{errMsg: "translate Anthropic response: " + err.Error()}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(translated)
	return outcome{committed: true, status: http.StatusOK, prompt: prompt, completion: completion}
}

func rawModel(raw map[string]any) string {
	m, _ := raw["model"].(string)
	return m
}

// requestContext applies the configured timeout to non-streaming upstream calls.
func (g *Gateway) requestContext(ctx context.Context, stream bool) (context.Context, context.CancelFunc) {
	if stream || g.cfg.RequestTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, g.cfg.RequestTimeout)
}

func (g *Gateway) upstreamRequest(ctx context.Context, ch *store.Channel, method, path string, body []byte, apiKey string, stream, acceptJSON bool) (*http.Request, error) {
	base := strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("channel %q has an empty base URL", ch.Name)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else if acceptJSON {
		req.Header.Set("Accept", "application/json")
	}
	switch ch.Type {
	case "anthropic":
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

func (g *Gateway) record(tok *store.Token, ch *store.Channel, requestID, model, upstream string, stream bool, status string, code int, errMsg string, prompt, completion int64, started time.Time) {
	total := prompt + completion
	cost := 0.0
	if ch != nil {
		cost = round6((float64(prompt)*ch.InputPrice + float64(completion)*ch.OutputPrice) / 1_000_000)
	}
	l := &store.Log{
		RequestID:        requestID,
		TokenID:          tok.ID,
		TokenName:        tok.Name,
		Model:            model,
		UpstreamModel:    upstream,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		Cost:             cost,
		Status:           status,
		StatusCode:       code,
		Error:            truncate(errMsg, 1000),
		LatencyMS:        time.Since(started).Milliseconds(),
		Stream:           stream,
	}
	if ch != nil {
		l.ChannelID = ch.ID
		l.ChannelName = ch.Name
	}
	if err := g.store.InsertLog(l); err != nil {
		g.logger.Error("failed to write request log", "error", err)
	}
	if status == "success" {
		if err := g.store.AddTokenUsage(tok.ID, cost); err != nil {
			g.logger.Error("failed to update key usage", "error", err)
		}
		return
	}
	if err := g.store.MarkTokenUsed(tok.ID); err != nil {
		g.logger.Error("failed to update key last-used time", "error", err)
	}
}

func (g *Gateway) serverError(w http.ResponseWriter, tok *store.Token, requestID, model string, stream bool, started time.Time, err error) {
	g.logger.Error("gateway internal error", "error", err)
	writeOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "")
	g.record(tok, nil, requestID, model, "", stream, "error", http.StatusInternalServerError, err.Error(), 0, 0, started)
}

func authorize(tok *store.Token, model string) (int, string) {
	if tok.Status != "active" {
		return http.StatusUnauthorized, "this gateway key is disabled"
	}
	if tok.ExpiresAt != nil && *tok.ExpiresAt <= time.Now().Unix() {
		return http.StatusUnauthorized, "this gateway key has expired"
	}
	if !modelAllowed(tok, model) {
		return http.StatusForbidden, fmt.Sprintf("model %q is not allowed by this gateway key", model)
	}
	if tok.Quota >= 0 && tok.UsedQuota >= tok.Quota {
		return http.StatusTooManyRequests, "insufficient quota for this gateway key"
	}
	return 0, ""
}

func modelAllowed(tok *store.Token, model string) bool {
	if len(tok.AllowedModels) == 0 {
		return true
	}
	for _, m := range tok.AllowedModels {
		if m == model {
			return true
		}
	}
	return false
}

func authErrorType(code int) string {
	if code == http.StatusTooManyRequests {
		return "insufficient_quota"
	}
	return "invalid_api_key"
}

func shouldFailover(status int) bool {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusNotFound,
		status == http.StatusRequestTimeout, status == http.StatusConflict, status == http.StatusTooEarly,
		status == http.StatusTooManyRequests, status >= 500:
		return true
	}
	return false
}

func writeOpenAIError(w http.ResponseWriter, status int, message, typ, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
			"param":   nil,
			"code":    code,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readErrorBody(resp *http.Response) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	msg := strings.TrimSpace(string(data))
	if msg == "" {
		return http.StatusText(resp.StatusCode)
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &parsed) == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return truncate(msg, 500)
}

func newRequestID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
