package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

func randomFakeCredential(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random credential: %v", err)
	}
	return "fake-cred-" + hex.EncodeToString(buf)
}

func TestRedactSensitive(t *testing.T) {
	fake := randomFakeCredential(t)
	text := fmt.Sprintf(
		`error for %s: Authorization: Bearer %s api_key=%s sk-abcdefghijklmnop`,
		fake, fake, fake,
	)
	redacted := redactSensitive(text, fake)
	for _, secret := range []string{fake, "sk-abcdefghijklmnop"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redacted text still contains %q: %s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("expected a redaction marker: %s", redacted)
	}
}

func TestUpstreamErrorRedactsCredentials(t *testing.T) {
	env := newTestEnv(t)
	fake := randomFakeCredential(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"bad credential %s; Authorization: Bearer %s"}}`, fake, fake)
	}))
	defer upstream.Close()
	channel := env.addChannelWithKey(t, "leaky", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2, fake)

	rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, fake) {
		t.Fatalf("response leaked the channel credential: %s", body)
	} else if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("response message was not redacted: %s", body)
	}

	logs := env.logs(t)
	if len(logs) != 1 {
		t.Fatalf("expected one log, got %+v", logs)
	}
	if strings.Contains(logs[0].Error, fake) || !strings.Contains(logs[0].Error, "[REDACTED]") {
		t.Fatalf("request log did not redact the credential: %+v", logs[0])
	}
	if serverLog := env.logBuf.String(); strings.Contains(serverLog, fake) {
		t.Fatalf("server log leaked the channel credential: %s", serverLog)
	} else if !strings.Contains(serverLog, "[REDACTED]") {
		t.Fatalf("server log did not record a redacted upstream error: %s", serverLog)
	}

	result := env.gw.TestChannel(context.Background(), channel, "public-model")
	if strings.Contains(result.Message, fake) || !strings.Contains(result.Message, "[REDACTED]") {
		t.Fatalf("channel test message did not redact the credential: %+v", result)
	}
}

func TestOpenAI2xxErrorBodyRedacted(t *testing.T) {
	env := newTestEnv(t)
	fake := randomFakeCredential(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"bad credential %s"}}`, fake)
	}))
	defer upstream.Close()
	env.addChannelWithKey(t, "error-doc", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2, fake)

	rec := env.chat(t, `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, fake) {
		t.Fatalf("2xx error document leaked the credential: %s", body)
	}
	logs := env.logs(t)
	if len(logs) != 1 || strings.Contains(logs[0].Error, fake) {
		t.Fatalf("request log leaked the 2xx error document: %+v", logs)
	}
	if serverLog := env.logBuf.String(); strings.Contains(serverLog, fake) || !strings.Contains(serverLog, "[REDACTED]") {
		t.Fatalf("server log did not redact the 2xx error document: %s", serverLog)
	}
}

func TestChatCompletionsStreamFinishReasonCompletes(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		flusher.Flush()
		// No [DONE]. finish_reason is still a completion marker.
	}))
	defer upstream.Close()
	env.addChannel(t, "finish-only", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)

	rec := env.chat(t, `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"done"`) || strings.Contains(body, "stream_incomplete") {
		t.Fatalf("finish_reason stream was not completed: %s", body)
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].Status != "success" || logs[0].CompletionTokens == 0 {
		t.Fatalf("unexpected completion log: %+v", logs)
	}
}

type commitRecorder struct {
	*httptest.ResponseRecorder
	once      sync.Once
	committed chan struct{}
}

func newCommitRecorder() *commitRecorder {
	return &commitRecorder{ResponseRecorder: httptest.NewRecorder(), committed: make(chan struct{})}
}

func (r *commitRecorder) WriteHeader(code int) {
	r.once.Do(func() { close(r.committed) })
	r.ResponseRecorder.WriteHeader(code)
}

func (r *commitRecorder) Write(p []byte) (int, error) {
	r.once.Do(func() { close(r.committed) })
	return r.ResponseRecorder.Write(p)
}

func TestChatCompletionsStreamCanceled(t *testing.T) {
	env := newTestEnv(t)
	firstDelta := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		flusher.Flush()
		firstDelta <- struct{}{}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	env.addChannel(t, "cancelable", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)
	env.setQuota(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *commitRecorder, 1)
	rec := newCommitRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public-model","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	go func() {
		env.gw.ChatCompletions(rec, req, env.token)
		done <- rec
	}()
	select {
	case <-firstDelta:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not send the first delta")
	}
	select {
	case <-rec.committed:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not commit the stream")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled stream did not finish")
	}

	logs := env.logs(t)
	if len(logs) != 1 {
		t.Fatalf("expected one log, got %+v", logs)
	}
	log := logs[0]
	if log.Status != "cancelled" || !strings.Contains(log.Error, "cancelled") {
		t.Fatalf("cancellation was not recorded distinctly: %+v", log)
	}
	if log.CostStatus != store.CostEstimated || log.CompletionTokens == 0 || log.Cost <= 0 {
		t.Fatalf("canceled stream did not account for partial usage: %+v", log)
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, log.Cost) || settled.ReservedQuota != 0 {
		t.Fatalf("cancellation charge = used %v reserved %v, log cost %v", settled.UsedQuota, settled.ReservedQuota, log.Cost)
	}
}

func TestUpstreamStreamErrorFrameRedactsAndFails(t *testing.T) {
	env := newTestEnv(t)
	fake := randomFakeCredential(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: {\"error\":{\"message\":\"bad credential %s\",\"type\":\"upstream_error\"}}\n\n", fake)
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()
	env.addChannelWithKey(t, "stream-leaky", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2, fake)

	rec := env.chat(t, `{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, fake) || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("stream error frame was not redacted: %s", body)
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].Status != "interrupted" {
		t.Fatalf("upstream stream error was not recorded as interrupted: %+v", logs)
	}
}
