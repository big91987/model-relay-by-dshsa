package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/config"
	"github.com/big91987/model-relay-by-dshsa/internal/gateway"
	"github.com/big91987/model-relay-by-dshsa/internal/secretbox"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.EnsureAdmin("admin", "admin-pass"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	cfg := &config.Config{
		SecretKey:         "test-secret",
		SessionTTL:        time.Hour,
		Version:           "test",
		DataDir:           t.TempDir(),
		VideoPollInterval: 10 * time.Millisecond,
	}
	cipher, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw := gateway.New(cfg, st, cipher, logger)
	srv := New(cfg, st, cipher, gw, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, data
}

func TestAdminAndRelayFlow(t *testing.T) {
	ts, _ := newTestServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-key" {
			t.Errorf("unexpected upstream auth: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"hi from mock"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
	}))
	defer upstream.Close()

	client := newClient(t)

	resp, _ := doJSON(t, client, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, client, http.MethodGet, ts.URL+"/api/dashboard", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 before login, got %d", resp.StatusCode)
	}
	resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{"username": "admin", "password": "admin-pass"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}
	base, _ := url.Parse(ts.URL)
	if len(client.Jar.Cookies(base)) == 0 {
		t.Fatal("expected session cookie")
	}
	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/auth/me", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"admin"`) {
		t.Fatalf("me status %d body %s", resp.StatusCode, body)
	}

	channelPayload := map[string]any{
		"name": "mock", "type": "openai", "base_url": upstream.URL + "/v1", "api_key": "upstream-key",
		"models": []string{"public-model"}, "priority": 0, "weight": 1, "status": "enabled",
		"input_price": 1, "output_price": 2,
	}
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels", channelPayload, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel status %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "upstream-key") {
		t.Fatalf("channel response leaked API key: %s", body)
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/tokens", map[string]any{
		"name": "ci", "quota": -1, "daily_quota": 2.5, "allowed_models": []string{}, "rpm_limit": 60,
		"tpm_limit": 1000, "max_concurrency": 3, "expires_at": nil, "status": "active",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token status %d: %s", resp.StatusCode, body)
	}
	var created struct {
		Key   string      `json:"key"`
		Token store.Token `json:"token"`
	}
	if err := json.Unmarshal(body, &created); err != nil || !strings.HasPrefix(created.Key, "sk-") {
		t.Fatalf("token response %s: %v", body, err)
	}
	if created.Token.RPMLimit != 60 || created.Token.TPMLimit != 1000 || created.Token.MaxConcurrency != 3 || created.Token.DailyQuota != 2.5 {
		t.Fatalf("token limits were not persisted: %+v", created.Token)
	}
	tokenResponse := struct{ Key string }{Key: created.Key}

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/tokens", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tokens status %d: %s", resp.StatusCode, body)
	}
	var listed struct {
		Data []store.Token `json:"data"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode tokens: %v (%s)", err, body)
	}
	if len(listed.Data) != 1 || listed.Data[0].TPMLimit != 1000 || listed.Data[0].MaxConcurrency != 3 {
		t.Fatalf("listed token limits are wrong: %+v", listed.Data)
	}
	for _, payload := range []map[string]any{
		{"name": "bad-rpm", "rpm_limit": -1},
		{"name": "bad-tpm", "tpm_limit": -1},
		{"name": "bad-concurrency", "max_concurrency": -1},
		{"name": "bad-daily", "daily_quota": -0.5},
	} {
		resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/tokens", payload, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected %v to be rejected, got %d: %s", payload, resp.StatusCode, body)
		}
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/tokens", map[string]any{
		"name": "disabled-key", "quota": -1, "allowed_models": []string{}, "rpm_limit": 0, "expires_at": nil, "status": "disabled",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create disabled token status %d: %s", resp.StatusCode, body)
	}
	var disabledResponse struct {
		Token store.Token `json:"token"`
	}
	if err := json.Unmarshal(body, &disabledResponse); err != nil || disabledResponse.Token.Status != "disabled" {
		t.Fatalf("requested disabled status was not persisted: %s (%v)", body, err)
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/v1/chat/completions", map[string]any{
		"model":    "public-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, map[string]string{"Authorization": "Bearer " + tokenResponse.Key})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relay status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "hi from mock") {
		t.Fatalf("unexpected relay response: %s", body)
	}
	relayRequestID := resp.Header.Get("X-Request-Id")
	if relayRequestID == "" {
		t.Fatal("relay response must carry X-Request-Id")
	}

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/dashboard", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status %d", resp.StatusCode)
	}
	var dashboard store.Dashboard
	if err := json.Unmarshal(body, &dashboard); err != nil {
		t.Fatalf("dashboard decode: %v", err)
	}
	if dashboard.Totals.Requests != 1 || dashboard.Totals.TotalTokens != 7 {
		t.Fatalf("unexpected dashboard totals: %+v", dashboard.Totals)
	}

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/logs?page=1&page_size=10", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs status %d", resp.StatusCode)
	}
	var logsResponse struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal(body, &logsResponse)
	if logsResponse.Total != 1 {
		t.Fatalf("expected one log, got %s", body)
	}
	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/logs?request_id="+relayRequestID, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logs by request id status %d", resp.StatusCode)
	}
	_ = json.Unmarshal(body, &logsResponse)
	if logsResponse.Total != 1 || !strings.Contains(string(body), relayRequestID) {
		t.Fatalf("request id filter did not find the request log: %s", body)
	}
	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/logs?request_id=missing-request-id", nil, nil)
	_ = json.Unmarshal(body, &logsResponse)
	if logsResponse.Total != 0 {
		t.Fatalf("unknown request id must match nothing: %s", body)
	}

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "admin console was not built") {
		t.Fatalf("placeholder status %d body %s", resp.StatusCode, body)
	}

	resp, _ = doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/logout", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, client, http.MethodGet, ts.URL+"/api/auth/me", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", resp.StatusCode)
	}
}

func TestRelayRejectsBadKey(t *testing.T) {
	ts, _ := newTestServer(t)
	client := newClient(t)
	resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/v1/chat/completions", map[string]any{
		"model":    "anything",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, map[string]string{"Authorization": "Bearer sk-invalid"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "invalid_api_key") {
		t.Fatalf("unexpected error body: %s", body)
	}
}

func TestFetchUpstreamModelsEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	failing := atomic.Bool{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer upstream-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
			return
		}
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"provider exploded"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"gpt-4o-mini"},{"id":"gpt-4o"}]}`)
	}))
	defer upstream.Close()

	client := newClient(t)
	resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{"username": "admin", "password": "admin-pass"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels/upstream-models", map[string]any{
		"type": "openai", "base_url": upstream.URL + "/v1", "api_key": "upstream-key",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch models status %d: %s", resp.StatusCode, body)
	}
	var fetched struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(body, &fetched); err != nil {
		t.Fatalf("decode models: %v (%s)", err, body)
	}
	if len(fetched.Models) != 2 || fetched.Models[0] != "gpt-4o" || fetched.Models[1] != "gpt-4o-mini" {
		t.Fatalf("unexpected models: %v", fetched.Models)
	}

	// MiniMax video channels use the provider's OpenAI-compatible model list.
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels/upstream-models", map[string]any{
		"type": "minimax", "base_url": upstream.URL, "api_key": "upstream-key",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch minimax models status %d: %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &fetched); err != nil || len(fetched.Models) == 0 {
		t.Fatalf("decode minimax models: %v (%s)", err, body)
	}

	// A new channel needs its key in the request.
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels/upstream-models", map[string]any{
		"type": "openai", "base_url": upstream.URL + "/v1",
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing key status %d: %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels/upstream-models", map[string]any{
		"type": "openai", "base_url": "ftp://example.com", "api_key": "upstream-key",
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid url status %d: %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels", map[string]any{
		"name": "mock", "type": "openai", "base_url": upstream.URL + "/v1", "api_key": "upstream-key",
		"models": []string{"public-model"}, "priority": 0, "weight": 1, "status": "enabled",
		"input_price": 1, "output_price": 2,
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel status %d: %s", resp.StatusCode, body)
	}
	var channel store.Channel
	if err := json.Unmarshal(body, &channel); err != nil {
		t.Fatalf("decode channel: %v", err)
	}

	// An existing channel can reuse its stored key and config.
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels/upstream-models", map[string]any{
		"channel_id": channel.ID,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch stored channel models status %d: %s", resp.StatusCode, body)
	}

	// A failed fetch must not persist anything.
	failing.Store(true)
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels/upstream-models", map[string]any{
		"channel_id": channel.ID,
	}, nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "provider exploded") {
		t.Fatalf("failed fetch status %d: %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/channels/"+strconv.FormatInt(channel.ID, 10), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get channel status %d: %s", resp.StatusCode, body)
	}
	var reloaded store.Channel
	if err := json.Unmarshal(body, &reloaded); err != nil {
		t.Fatalf("decode channel: %v", err)
	}
	if len(reloaded.Models) != 1 || reloaded.Models[0] != "public-model" {
		t.Fatalf("failed fetch changed stored models: %v", reloaded.Models)
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels/upstream-models", map[string]any{
		"channel_id": 9999,
	}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown channel status %d: %s", resp.StatusCode, body)
	}
}

func TestMinimaxVideoFlow(t *testing.T) {
	ts, _ := newTestServer(t)
	videoBytes := []byte("\x00\x00\x00\x18ftypmp42server-test-video")
	firstFrame := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lv0nAAAAAElFTkSuQmCC"
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/video_generation":
			if r.Header.Get("Authorization") != "Bearer upstream-key" {
				t.Errorf("unexpected upstream auth: %q", r.Header.Get("Authorization"))
			}
			var body struct {
				Model   string `json:"model"`
				Content []struct {
					Type string `json:"type"`
					Role string `json:"role"`
				} `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Model != "MiniMax-H3" || len(body.Content) != 2 || body.Content[1].Role != "first_frame" {
				t.Errorf("unexpected upstream body: %+v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"task_id":"task-server-1"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/query/video_generation/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{
				"id": "task-server-1", "model": "MiniMax-H3", "status": "succeeded",
				"content":    map[string]any{"url": upstream.URL + "/media.mp4"},
				"resolution": "768P", "duration": 5, "ratio": "adaptive",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/media.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	client := newClient(t)
	if resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{"username": "admin", "password": "admin-pass"}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}
	resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/channels", map[string]any{
		"name": "minimax", "type": "minimax", "base_url": upstream.URL, "api_key": "upstream-key",
		"models": []string{"MiniMax-H3"}, "priority": 0, "weight": 1, "status": "enabled",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel status %d: %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/models", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"minimax"`) || !strings.Contains(string(body), `"MiniMax-H3"`) {
		t.Fatalf("models response %d: %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/videos", map[string]any{
		"model": "MiniMax-H3", "prompt": "A tiny server-generated clip", "first_frame": firstFrame,
		"resolution": "768P", "duration": 5, "ratio": "16:9",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create video status %d: %s", resp.StatusCode, body)
	}
	var created struct {
		ID         int64  `json:"id"`
		Status     string `json:"status"`
		Mode       string `json:"mode"`
		VideoReady bool   `json:"video_ready"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created video: %v", err)
	}
	if created.ID == 0 || created.Status != "queued" || created.Mode != "first_frame" || created.VideoReady {
		t.Fatalf("unexpected created task: %s", body)
	}

	deadline := time.Now().Add(3 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, body = doJSON(t, client, http.MethodGet, ts.URL+fmt.Sprintf("/api/videos/%d", created.ID), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get video status %d: %s", resp.StatusCode, body)
		}
		var task struct {
			Status     string `json:"status"`
			VideoReady bool   `json:"video_ready"`
			ContentURL string `json:"content_url"`
		}
		_ = json.Unmarshal(body, &task)
		if task.Status == "succeeded" && task.VideoReady {
			ready = true
			if task.ContentURL != fmt.Sprintf("/api/videos/%d/content", created.ID) {
				t.Fatalf("unexpected content URL: %s", body)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("video task did not become ready")
	}

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+fmt.Sprintf("/api/videos/%d/content", created.ID), nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, videoBytes) {
		t.Fatalf("content status %d body %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("content type = %q", got)
	}
	resp, _ = doJSON(t, client, http.MethodGet, ts.URL+fmt.Sprintf("/api/videos/%d/content?download=1", created.ID), nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download response status %d disposition %q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/videos", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"MiniMax-H3"`) {
		t.Fatalf("list videos status %d: %s", resp.StatusCode, body)
	}
}

func TestSeedanceVideoFlow(t *testing.T) {
	ts, _ := newTestServer(t)
	videoBytes := []byte("\x00\x00\x00\x18ftypmp42seedance-test-video")
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/contents/generations/tasks":
			if r.Header.Get("Authorization") != "Bearer ark-key" {
				t.Errorf("unexpected upstream auth: %q", r.Header.Get("Authorization"))
			}
			var body struct {
				Model      string  `json:"model"`
				Resolution string  `json:"resolution"`
				Duration   float64 `json:"duration"`
				Content    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Model != "doubao-seedance-1-0-pro-250528" || body.Resolution != "720p" ||
				body.Duration != 5 || len(body.Content) != 1 || body.Content[0].Text == "" {
				t.Errorf("unexpected upstream body: %+v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"cgt-server-1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/contents/generations/tasks/cgt-server-1":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cgt-server-1", "model": "doubao-seedance-1-0-pro-250528", "status": "succeeded",
				"content":    map[string]any{"video_url": upstream.URL + "/media.mp4"},
				"resolution": "720p", "duration": 5, "ratio": "16:9",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"doubao-seedance-1-0-pro-250528"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/media.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(videoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	client := newClient(t)
	if resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{"username": "admin", "password": "admin-pass"}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}
	resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/channels", map[string]any{
		"name": "seedance", "type": "seedance", "base_url": upstream.URL + "/api/v3", "api_key": "ark-key",
		"models": []string{"doubao-seedance-1-0-pro-250528"}, "priority": 0, "weight": 1, "status": "enabled",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel status %d: %s", resp.StatusCode, body)
	}
	var seedanceChannel struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &seedanceChannel); err != nil || seedanceChannel.ID == 0 {
		t.Fatalf("decode channel: %v (%s)", err, body)
	}
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels/upstream-models", map[string]any{
		"channel_id": seedanceChannel.ID,
	}, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "doubao-seedance-2-5-260628") {
		t.Fatalf("fetch seedance models status %d: %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/models", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"seedance"`) || !strings.Contains(string(body), `"doubao-seedance-1-0-pro-250528"`) {
		t.Fatalf("models response %d: %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/videos", map[string]any{
		"model": "doubao-seedance-1-0-pro-250528", "prompt": "A tiny Seedance clip",
		"resolution": "720P", "duration": 5, "ratio": "16:9",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create video status %d: %s", resp.StatusCode, body)
	}
	var created struct {
		ID         int64  `json:"id"`
		Status     string `json:"status"`
		Mode       string `json:"mode"`
		VideoReady bool   `json:"video_ready"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created video: %v", err)
	}
	if created.ID == 0 || created.Status != "queued" || created.Mode != "text" || created.VideoReady {
		t.Fatalf("unexpected created task: %s", body)
	}

	deadline := time.Now().Add(3 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, body = doJSON(t, client, http.MethodGet, ts.URL+fmt.Sprintf("/api/videos/%d", created.ID), nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get video status %d: %s", resp.StatusCode, body)
		}
		var task struct {
			Status     string `json:"status"`
			VideoReady bool   `json:"video_ready"`
		}
		_ = json.Unmarshal(body, &task)
		if task.Status == "succeeded" && task.VideoReady {
			ready = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("Seedance video task did not become ready")
	}

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+fmt.Sprintf("/api/videos/%d/content?download=1", created.ID), nil, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, videoBytes) {
		t.Fatalf("content status %d body %q", resp.StatusCode, body)
	}
	if disposition := resp.Header.Get("Content-Disposition"); !strings.Contains(disposition, "seedance") {
		t.Fatalf("download disposition = %q", disposition)
	}
}

func TestPlaygroundAdminProxy(t *testing.T) {
	ts, st := newTestServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-playground","choices":[{"index":0,"message":{"role":"assistant","content":"played back"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	}))
	defer upstream.Close()

	client := newClient(t)
	resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{
		"username": "admin", "password": "admin-pass",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels", map[string]any{
		"name": "playground-upstream", "type": "openai", "base_url": upstream.URL + "/v1", "api_key": "upstream-key",
		"models": []string{"public-model"}, "priority": 0, "weight": 1, "status": "enabled",
		"input_price": 1, "output_price": 2,
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel status %d: %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/tokens", map[string]any{
		"name": "playground-key", "quota": -1, "allowed_models": []string{}, "rpm_limit": 0, "expires_at": nil, "status": "active",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token status %d: %s", resp.StatusCode, body)
	}
	var tokenResponse struct {
		Token store.Token `json:"token"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	tokenID := strconv.FormatInt(tokenResponse.Token.ID, 10)

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/playground/models?token_id="+tokenID, nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"public-model"`) {
		t.Fatalf("playground models status %d: %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/playground/chat?token_id="+tokenID, map[string]any{
		"model":    "public-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "played back") {
		t.Fatalf("playground chat status %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("playground chat must preserve the request id header")
	}
	logs, _, err := st.ListLogs(store.LogFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 || logs[0].TokenName != "playground-key" || logs[0].Status != "success" {
		t.Fatalf("unexpected playground logs: %+v", logs)
	}

	// The admin session is required; the browser does not send a gateway key.
	anonymous := newClient(t)
	resp, _ = doJSON(t, anonymous, http.MethodGet, ts.URL+"/api/playground/models?token_id="+tokenID, nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous playground access status %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, client, http.MethodGet, ts.URL+"/api/playground/models?token_id=9999", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown playground key status %d", resp.StatusCode)
	}
}

func TestLogsSummaryStableAcrossPages(t *testing.T) {
	ts, st := newTestServer(t)
	now := time.Now().Unix()
	for i, cost := range []float64{0.25, 0.5, 0.75} {
		if err := st.InsertLog(&store.Log{
			RequestID: fmt.Sprintf("summary-req-%d", i), Model: "m", Status: "success",
			Cost: cost, CostStatus: "actual", CreatedAt: now - int64(i),
		}); err != nil {
			t.Fatalf("seed log: %v", err)
		}
	}
	client := newClient(t)
	resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{
		"username": "admin", "password": "admin-pass",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}

	type logsPage struct {
		Total   int              `json:"total"`
		Summary store.LogSummary `json:"summary"`
	}
	fetch := func(query string) logsPage {
		t.Helper()
		resp, body := doJSON(t, client, http.MethodGet, ts.URL+"/api/logs?"+query, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("logs %s status %d: %s", query, resp.StatusCode, body)
		}
		var page logsPage
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatalf("decode logs %s: %v", query, err)
		}
		return page
	}

	first := fetch("page=1&page_size=1")
	if first.Total != 3 || first.Summary.Requests != 3 || math.Abs(first.Summary.Cost-1.5) > 1e-9 {
		t.Fatalf("unexpected first-page summary: %+v", first)
	}
	second := fetch("page=2&page_size=1")
	if second.Total != 3 || second.Summary != first.Summary {
		t.Fatalf("summary must stay constant while paging: first=%+v second=%+v", first, second)
	}
	filtered := fetch(fmt.Sprintf("start=%d&end=%d", now-1, now))
	if filtered.Summary.Requests != 2 || math.Abs(filtered.Summary.Cost-0.75) > 1e-9 {
		t.Fatalf("period filter must narrow the summary: %+v", filtered.Summary)
	}
}
