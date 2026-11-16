package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

func (e *testEnv) addChannelMode(t *testing.T, name, baseURL, typ string, priority int, models []string, mapping map[string]string, inputPrice, outputPrice float64, mode string) *store.Channel {
	t.Helper()
	encrypted, err := e.cipher.Encrypt("upstream-key")
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	channel := &store.Channel{
		Name: name, Type: typ, BaseURL: baseURL,
		APIKeyEncrypted: encrypted, APIKeyHint: "••••-key",
		Models: models, ModelMapping: mapping, Priority: priority, Weight: 1,
		Status: "enabled", PriceMode: mode, InputPrice: inputPrice, OutputPrice: outputPrice,
	}
	if err := e.st.CreateChannel(channel); err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return channel
}

func TestFailedAttemptReportedUsageIsCharged(t *testing.T) {
	env := newTestEnv(t)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"primary failed"},"usage":{"prompt_tokens":0,"completion_tokens":3}}`, http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"from backup"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":6}}`)
	}))
	defer backup.Close()
	// The report configures $1 per output token ($1,000,000 per 1M tokens):
	// 3 tokens from the failed primary plus 6 from the backup must total $9.
	env.addChannel(t, "primary", primary.URL+"/v1", "openai", 10, []string{"public-model"}, nil, 0, 1_000_000)
	env.addChannel(t, "backup", backup.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 1_000_000)
	env.setQuota(t, 100)

	rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 {
		t.Fatalf("expected one log, got %+v", logs)
	}
	log := logs[0]
	if !near(log.Cost, 9) || log.CompletionTokens != 9 || log.CostStatus != store.CostActual {
		t.Fatalf("failed attempt usage was not charged: %+v", log)
	}
	if len(log.Attempts) != 2 {
		t.Fatalf("expected two attributable attempts, got %+v", log.Attempts)
	}
	if log.Attempts[0].StatusCode != http.StatusInternalServerError || !near(log.Attempts[0].Cost, 3) || log.Attempts[0].CostStatus != store.CostActual {
		t.Fatalf("primary attempt = %+v", log.Attempts[0])
	}
	if log.Attempts[1].StatusCode != http.StatusOK || !near(log.Attempts[1].Cost, 6) || log.Attempts[1].CostStatus != store.CostActual {
		t.Fatalf("backup attempt = %+v", log.Attempts[1])
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 9) || settled.ReservedQuota != 0 {
		t.Fatalf("used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
	}
}

func TestAllFailedAttemptsWithUsageAreCharged(t *testing.T) {
	env := newTestEnv(t)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"first failed"},"usage":{"completion_tokens":2}}`, http.StatusInternalServerError)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"second failed"},"usage":{"completion_tokens":3}}`, http.StatusBadGateway)
	}))
	defer second.Close()
	env.addChannel(t, "first", first.URL+"/v1", "openai", 10, []string{"public-model"}, nil, 0, 1)
	env.addChannel(t, "second", second.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 1)
	env.setQuota(t, 1)

	rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 || logs[0].Status != "error" || !near(logs[0].Cost, 0.000005) || logs[0].CostStatus != store.CostActual {
		t.Fatalf("unexpected log: %+v", logs)
	}
	if len(logs[0].Attempts) != 2 {
		t.Fatalf("expected two attempts, got %+v", logs[0].Attempts)
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 0.000005) || settled.ReservedQuota != 0 {
		t.Fatalf("used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
	}
}

func TestUnknownPriceRejectedButExplicitFreeAllowed(t *testing.T) {
	t.Run("finite quota with unknown price is rejected", func(t *testing.T) {
		env := newTestEnv(t)
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
		defer upstream.Close()
		env.addChannel(t, "unpriced", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 0)
		env.setQuota(t, 1)

		rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "channel_price_not_configured") {
			t.Fatalf("unexpected error: %s", rec.Body.String())
		}
		if calls != 0 {
			t.Fatalf("unpriced upstream was called %d times", calls)
		}
		logs := env.logs(t)
		if len(logs) != 1 || logs[0].Cost != 0 || logs[0].CostStatus != store.CostFree {
			t.Fatalf("unexpected log: %+v", logs)
		}
		settled := env.freshToken(t)
		if settled.UsedQuota != 0 || settled.ReservedQuota != 0 {
			t.Fatalf("rejected request changed balance: used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
		}
	})

	t.Run("finite quota embeddings with unknown price is rejected", func(t *testing.T) {
		env := newTestEnv(t)
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			_, _ = io.WriteString(w, `{"object":"list","data":[],"usage":{"prompt_tokens":1}}`)
		}))
		defer upstream.Close()
		env.addChannel(t, "embed-unpriced", upstream.URL+"/v1", "openai", 0, []string{"embed-model"}, nil, 0, 0)
		env.setQuota(t, 1)

		rec := env.postEmbeddings(t, `{"model":"embed-model","input":"hi"}`)
		if rec.Code != http.StatusServiceUnavailable || calls != 0 {
			t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
		}
	})

	t.Run("explicit free channel is admitted for a finite quota", func(t *testing.T) {
		env := newTestEnv(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"free"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		}))
		defer upstream.Close()
		env.addChannelMode(t, "free", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 0, store.PriceModeFree)
		env.setQuota(t, 0.000001)

		rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		logs := env.logs(t)
		if len(logs) != 1 || logs[0].Cost != 0 || logs[0].CostStatus != store.CostFree {
			t.Fatalf("unexpected log: %+v", logs)
		}
		if len(logs[0].Attempts) != 1 || logs[0].Attempts[0].CostStatus != store.CostFree {
			t.Fatalf("unexpected attempts: %+v", logs[0].Attempts)
		}
	})

	t.Run("unlimited key records unknown price as unpriced", func(t *testing.T) {
		env := newTestEnv(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"unpriced"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		}))
		defer upstream.Close()
		env.addChannel(t, "unpriced", upstream.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 0)
		env.setQuota(t, -1)

		rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		logs := env.logs(t)
		if len(logs) != 1 || logs[0].Cost != 0 || logs[0].CostStatus != store.CostUnpriced {
			t.Fatalf("unexpected log: %+v", logs)
		}
		if len(logs[0].Attempts) != 1 || logs[0].Attempts[0].CostStatus != store.CostUnpriced {
			t.Fatalf("unexpected attempts: %+v", logs[0].Attempts)
		}
	})
}

func TestAmbiguousAttemptKeepsEarlierChargedAttempts(t *testing.T) {
	env := newTestEnv(t)
	env.gw.cfg.RequestTimeout = 50 * time.Millisecond
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"primary failed"},"usage":{"completion_tokens":3}}`, http.StatusInternalServerError)
	}))
	defer primary.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"late"},"finish_reason":"stop"}],"usage":{"completion_tokens":6}}`)
	}))
	defer slow.Close()
	env.addChannel(t, "primary", primary.URL+"/v1", "openai", 10, []string{"public-model"}, nil, 0, 1)
	env.addChannel(t, "slow", slow.URL+"/v1", "openai", 0, []string{"public-model"}, nil, 0, 1)
	env.setQuota(t, 1)

	rec := env.chat(t, `{"model":"public-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 {
		t.Fatalf("expected one log, got %+v", logs)
	}
	log := logs[0]
	// $0.000003 from the failed primary plus the second attempt's $0.00001
	// upper bound, marked unverified rather than dropping the first charge.
	if !near(log.Cost, 0.000013) || log.CostStatus != store.CostUnverified || len(log.Attempts) != 2 {
		t.Fatalf("unexpected log: %+v", log)
	}
	if !near(log.Attempts[0].Cost, 0.000003) || log.Attempts[0].CostStatus != store.CostActual {
		t.Fatalf("primary attempt = %+v", log.Attempts[0])
	}
	if !near(log.Attempts[1].Cost, 0.00001) || log.Attempts[1].CostStatus != store.CostUnverified {
		t.Fatalf("ambiguous attempt = %+v", log.Attempts[1])
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 0.000013) || settled.ReservedQuota != 0 {
		t.Fatalf("used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
	}
}

func TestEmbeddingsFailedAttemptUsageIsCharged(t *testing.T) {
	env := newTestEnv(t)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"primary failed"},"usage":{"prompt_tokens":3}}`, http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":6,"total_tokens":6}}`)
	}))
	defer backup.Close()
	env.addChannel(t, "primary", primary.URL+"/v1", "openai", 10, []string{"embed-model"}, nil, 1, 0)
	env.addChannel(t, "backup", backup.URL+"/v1", "openai", 0, []string{"embed-model"}, nil, 1, 0)
	env.setQuota(t, 1)

	rec := env.postEmbeddings(t, `{"model":"embed-model","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	logs := env.logs(t)
	if len(logs) != 1 {
		t.Fatalf("expected one log, got %+v", logs)
	}
	log := logs[0]
	if !near(log.Cost, 0.000009) || log.PromptTokens != 9 || log.CostStatus != store.CostActual || len(log.Attempts) != 2 {
		t.Fatalf("unexpected log: %+v", log)
	}
	if !near(log.Attempts[0].Cost, 0.000003) || log.Attempts[0].StatusCode != http.StatusInternalServerError {
		t.Fatalf("primary attempt = %+v", log.Attempts[0])
	}
	settled := env.freshToken(t)
	if !near(settled.UsedQuota, 0.000009) || settled.ReservedQuota != 0 {
		t.Fatalf("used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
	}
}
