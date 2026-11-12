package gateway

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

func (e *testEnv) setQuota(t *testing.T, quota float64) {
	t.Helper()
	e.token.Quota = quota
	if err := e.st.UpdateToken(e.token); err != nil {
		t.Fatalf("update token quota: %v", err)
	}
}

func (e *testEnv) freshToken(t *testing.T) *store.Token {
	t.Helper()
	tok, err := e.st.GetTokenByID(e.token.ID)
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	return tok
}

// postChat calls the handler directly so tests can launch concurrent requests
// without relying on testing.T from a goroutine.
func (e *testEnv) postChat(body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.gw.ChatCompletions(rec, req, e.token)
	return rec
}

func (e *testEnv) postChatWithContext(ctx context.Context, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.gw.ChatCompletions(rec, req, e.token)
	return rec
}

func (e *testEnv) postEmbeddings(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.gw.Embeddings(rec, req, e.token)
	return rec
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestConcurrentRequestsCannotDoubleSpendQuota(t *testing.T) {
	env := newTestEnv(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "primary", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 1)

	body := `{"model":"public-model","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`
	promptUpper := promptTokenUpperBound([]byte(body), 1)
	oneReservation := roundUp6((float64(promptUpper) + 1000) / 1_000_000)
	env.setQuota(t, oneReservation*1.5)

	results := make(chan *httptest.ResponseRecorder, 5)
	for i := 0; i < 5; i++ {
		go func() { results <- env.postChat(body) }()
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no request reached the upstream")
	}

	rejected := 0
	deadline := time.After(3 * time.Second)
	for rejected < 4 {
		select {
		case rec := <-results:
			if rec.Code == http.StatusTooManyRequests {
				rejected++
				continue
			}
			t.Fatalf("unexpected concurrent response %d: %s", rec.Code, rec.Body.String())
		case <-deadline:
			t.Fatalf("only %d of 4 extra requests were rejected", rejected)
		}
	}
	inFlight := env.freshToken(t)
	if !near(inFlight.ReservedQuota, oneReservation) {
		t.Fatalf("in-flight reservation = %v, want %v", inFlight.ReservedQuota, oneReservation)
	}
	if hits != 1 {
		t.Fatalf("upstream was called %d times, want 1", hits)
	}

	close(release)
	var success *httptest.ResponseRecorder
	select {
	case success = <-results:
	case <-time.After(3 * time.Second):
		t.Fatal("admitted request did not finish")
	}
	if success.Code != http.StatusOK {
		t.Fatalf("admitted request status %d: %s", success.Code, success.Body.String())
	}

	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 0.000002) {
		t.Fatalf("used quota = %v, want 0.000002", settled.UsedQuota)
	}
	if settled.ReservedQuota != 0 {
		t.Fatalf("reservation was not released: %v", settled.ReservedQuota)
	}
	logs := env.logs(t)
	if len(logs) != 5 {
		t.Fatalf("expected 5 logs, got %d: %+v", len(logs), logs)
	}
	successes := 0
	for _, log := range logs {
		if log.Status == "success" {
			successes++
			if log.CostStatus != store.CostActual || !near(log.Cost, 0.000002) {
				t.Fatalf("unexpected success log: %+v", log)
			}
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one success, got %d", successes)
	}
}

func TestConcurrentAllowedRequestsSettleExactlyOnce(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "primary", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)
	env.setQuota(t, -1)

	body := `{"model":"public-model","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- env.postChat(body)
		}()
	}
	wg.Wait()
	close(results)
	for rec := range results {
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 10 input tokens * $1/M + 5 output tokens * $2/M = $0.00002 per request.
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 0.0001) {
		t.Fatalf("used quota = %v, want 0.0001", settled.UsedQuota)
	}
	if settled.ReservedQuota != 0 {
		t.Fatalf("reservations left behind: %v", settled.ReservedQuota)
	}
	logs := env.logs(t)
	if len(logs) != 5 {
		t.Fatalf("expected 5 logs, got %d", len(logs))
	}
}

func TestPreCallFailureDoesNotCharge(t *testing.T) {
	env := newTestEnv(t)
	// Port 1 on loopback refuses the connection before any request is sent.
	env.addChannel(t, "dead", "http://127.0.0.1:1/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)
	env.setQuota(t, 1)

	rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].Status != "error" || logs[0].CostStatus != store.CostFree || logs[0].Cost != 0 {
		t.Fatalf("unexpected log: %+v", logs)
	}
	settled := env.freshToken(t)
	if settled.UsedQuota != 0 || settled.ReservedQuota != 0 {
		t.Fatalf("pre-call failure was charged: used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
	}
}

func TestUpstreamTimeoutIsUnverified(t *testing.T) {
	env := newTestEnv(t)
	env.gw.cfg.RequestTimeout = 50 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "slow", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 1)
	env.setQuota(t, 1)

	body := `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	promptUpper := promptTokenUpperBound([]byte(body), 1)
	reserved := roundUp6((float64(promptUpper) + 10) / 1_000_000)

	rec := env.chat(t, body)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].CostStatus != store.CostUnverified || !near(logs[0].Cost, reserved) {
		t.Fatalf("timeout was not recorded as unverified: %+v", logs)
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, reserved) || settled.ReservedQuota != 0 {
		t.Fatalf("timeout charge = used %v reserved %v, want used %v", settled.UsedQuota, settled.ReservedQuota, reserved)
	}
}

func TestClientCancellationIsUnverified(t *testing.T) {
	env := newTestEnv(t)
	entered := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "slow", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 1)
	env.setQuota(t, 1)

	body := `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	promptUpper := promptTokenUpperBound([]byte(body), 1)
	reserved := roundUp6((float64(promptUpper) + 10) / 1_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- env.postChatWithContext(ctx, body) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach the upstream")
	}
	cancel()
	select {
	case rec := <-done:
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled request did not finish")
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].CostStatus != store.CostUnverified || !near(logs[0].Cost, reserved) {
		t.Fatalf("cancellation was not recorded as unverified: %+v", logs)
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, reserved) || settled.ReservedQuota != 0 {
		t.Fatalf("cancellation charge = used %v reserved %v", settled.UsedQuota, settled.ReservedQuota)
	}
}

func TestStreamBreakChargesPartialUsageAsEstimated(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answer\"}}]}\n\n")
		flusher.Flush()
		// Close without a finish chunk or [DONE]: the stream is broken.
	}))
	defer upstream.Close()
	env.addChannel(t, "stream", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)
	env.setQuota(t, 1)

	rec := env.chat(t, `{"model":"public-model","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 {
		t.Fatalf("expected one log, got %+v", logs)
	}
	log := logs[0]
	// A clean EOF without [DONE] is treated as a completed stream by the
	// relay, but the missing upstream usage still makes the charge estimated.
	if log.CostStatus != store.CostEstimated || log.CompletionTokens == 0 || log.Cost <= 0 {
		t.Fatalf("stream without usage was not billed as estimated: %+v", log)
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, log.Cost) || settled.ReservedQuota != 0 {
		t.Fatalf("stream charge = used %v reserved %v, log cost %v", settled.UsedQuota, settled.ReservedQuota, log.Cost)
	}
}

func TestChannelSwitchChargesOnlyBackupUsage(t *testing.T) {
	env := newTestEnv(t)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"from backup"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
	}))
	defer backup.Close()
	env.addChannel(t, "primary", primary.URL+"/v1", "openai", 10, []string{"public-model"}, nil, 1, 2)
	env.addChannel(t, "backup", backup.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)
	env.setQuota(t, 1)

	rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	// 2 * $1/M + 2 * $2/M = $0.000006, charged once for the backup only.
	if len(logs) != 1 || logs[0].ChannelName != "backup" || !near(logs[0].Cost, 0.000006) {
		t.Fatalf("unexpected log: %+v", logs)
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 0.000006) || settled.ReservedQuota != 0 {
		t.Fatalf("used %v reserved %v", settled.UsedQuota, settled.ReservedQuota)
	}
}

func TestMissingUsageIsEstimated(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"hello there"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "primary", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)
	env.setQuota(t, 1)

	rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].CostStatus != store.CostEstimated || logs[0].Cost <= 0 {
		t.Fatalf("unexpected log: %+v", logs)
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, logs[0].Cost) {
		t.Fatalf("used %v != log cost %v", settled.UsedQuota, logs[0].Cost)
	}
}

func TestEmbeddingsReservationAndSettlement(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":10,"total_tokens":10}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "embed", upstream.URL+"/v1", "openai", 0, []string{"embed-model"}, nil, 2, 0)
	env.setQuota(t, 1)

	rec := env.postEmbeddings(t, `{"model":"embed-model","input":"hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	// 10 * $2/M = $0.00002.
	if len(logs) != 1 || logs[0].PromptTokens != 10 || !near(logs[0].Cost, 0.00002) || logs[0].CostStatus != store.CostActual {
		t.Fatalf("unexpected log: %+v", logs)
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 0.00002) || settled.ReservedQuota != 0 {
		t.Fatalf("used %v reserved %v", settled.UsedQuota, settled.ReservedQuota)
	}
}

func TestEmbeddingsQuotaRejection(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream must not be called when quota is insufficient")
	}))
	defer upstream.Close()
	env.addChannel(t, "embed", upstream.URL+"/v1", "openai", 0, []string{"embed-model"}, nil, 2, 0)
	env.setQuota(t, 0)

	rec := env.postEmbeddings(t, `{"model":"embed-model","input":"hello"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient quota") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}
	settled := env.freshToken(t)
	if settled.UsedQuota != 0 || settled.ReservedQuota != 0 {
		t.Fatalf("rejected embedding changed balance: used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
	}
}

func TestUnlimitedKeyStillRecordsUsage(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "primary", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 1, 2)
	env.setQuota(t, -1)

	for i := 0; i < 3; i++ {
		if rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
			t.Fatalf("request %d status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	// (4 * $1/M + 2 * $2/M) * 3 = $0.000024.
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 0.000024) || settled.ReservedQuota != 0 {
		t.Fatalf("limited key accounting: used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
	}
}
