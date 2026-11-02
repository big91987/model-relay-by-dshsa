package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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
		SecretKey:  "test-secret",
		SessionTTL: time.Hour,
		Version:    "test",
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
		"name": "ci", "quota": -1, "allowed_models": []string{}, "rpm_limit": 0, "expires_at": nil, "status": "active",
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token status %d: %s", resp.StatusCode, body)
	}
	var tokenResponse struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &tokenResponse); err != nil || !strings.HasPrefix(tokenResponse.Key, "sk-") {
		t.Fatalf("token response %s: %v", body, err)
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
