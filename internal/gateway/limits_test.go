package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestLimitState(now *time.Time) *limitState {
	state := newLimitState()
	state.now = func() time.Time { return *now }
	return state
}

func TestLimitStateRejectsRequestsPerMinute(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := newTestLimitState(&now)
	for i := 0; i < 2; i++ {
		requestLease, err := state.Admit(admission{TokenID: 1, RequestID: "req-" + string(rune('a'+i)), RPMLimit: 2})
		if err != nil {
			t.Fatalf("request %d should be admitted: %v", i, err)
		}
		if i == 0 {
			snapshot := requestLease.Snapshot()
			if snapshot.RPM != 2 || snapshot.RPMRemaining != 1 {
				t.Fatalf("unexpected snapshot: %+v", snapshot)
			}
		}
	}
	if _, err := state.Admit(admission{TokenID: 1, RequestID: "req-c", RPMLimit: 2}); err == nil || err.Kind != LimitRequestsPerMinute {
		t.Fatalf("expected RPM rejection, got %v", err)
	} else if err.RetryAfter <= 0 || err.RetryAfter > time.Minute {
		t.Fatalf("unexpected retry after: %v", err.RetryAfter)
	}

	now = now.Add(time.Minute + time.Second)
	if _, err := state.Admit(admission{TokenID: 1, RequestID: "req-d", RPMLimit: 2}); err != nil {
		t.Fatalf("window should have rolled over: %v", err)
	}
}

func TestLimitStateTPMCountsSettledAndReservedTokens(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := newTestLimitState(&now)
	first, err := state.Admit(admission{TokenID: 1, RequestID: "req-a", TPMLimit: 100, PromptTokens: 60})
	if err != nil {
		t.Fatalf("first request should be admitted: %v", err)
	}
	// The first request is still in flight, so its prompt is reserved.
	if _, err := state.Admit(admission{TokenID: 1, RequestID: "req-b", TPMLimit: 100, PromptTokens: 50}); err == nil || err.Kind != LimitTokensPerMinute {
		t.Fatalf("expected TPM rejection while the reservation is held, got %v", err)
	}
	state.Settle(1, "req-a", 10, 5)
	first.Release()
	second, err := state.Admit(admission{TokenID: 1, RequestID: "req-b", TPMLimit: 100, PromptTokens: 50})
	if err != nil {
		t.Fatalf("settled request should free capacity: %v", err)
	}
	state.Settle(1, "req-b", 20, 10)
	second.Release()

	// 45 settled tokens remain; a 60-token prompt no longer fits.
	if _, err := state.Admit(admission{TokenID: 1, RequestID: "req-c", TPMLimit: 100, PromptTokens: 60}); err == nil || err.Kind != LimitTokensPerMinute {
		t.Fatalf("expected TPM rejection, got %v", err)
	}
	now = now.Add(time.Minute + time.Second)
	if _, err := state.Admit(admission{TokenID: 1, RequestID: "req-d", TPMLimit: 100, PromptTokens: 100}); err != nil {
		t.Fatalf("token window should have rolled over: %v", err)
	}
}

func TestLimitStateConcurrency(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := newTestLimitState(&now)
	first, err := state.Admit(admission{TokenID: 7, RequestID: "req-a", MaxConcurrency: 1})
	if err != nil {
		t.Fatalf("first request should be admitted: %v", err)
	}
	if _, err := state.Admit(admission{TokenID: 7, RequestID: "req-b", MaxConcurrency: 1}); err == nil || err.Kind != LimitConcurrency {
		t.Fatalf("expected concurrency rejection, got %v", err)
	}
	first.Release()
	if _, err := state.Admit(admission{TokenID: 7, RequestID: "req-c", MaxConcurrency: 1}); err != nil {
		t.Fatalf("released slot should be reusable: %v", err)
	}
}

func TestLimitStateReleaseDropsTokenReservation(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := newTestLimitState(&now)
	requestLease, err := state.Admit(admission{TokenID: 3, RequestID: "req-a", TPMLimit: 100, PromptTokens: 90})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	requestLease.Release()
	if _, err := state.Admit(admission{TokenID: 3, RequestID: "req-b", TPMLimit: 100, PromptTokens: 90}); err != nil {
		t.Fatalf("released reservation should not count: %v", err)
	}
}

func TestLimitStateNoLimitsIsAlwaysAdmitted(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := newTestLimitState(&now)
	for i := 0; i < 100; i++ {
		requestLease, err := state.Admit(admission{TokenID: 4, RequestID: "req", PromptTokens: 1 << 20})
		if err != nil {
			t.Fatalf("unlimited key should be admitted: %v", err)
		}
		state.Settle(4, "req", 10_000, 5_000)
		requestLease.Release()
	}
	if len(state.byToken) != 0 {
		t.Fatalf("unlimited keys must not accumulate limiter state: %+v", state.byToken)
	}
}

func TestWriteRateLimitHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRateLimitHeaders(rec, limitSnapshot{
		RPM: 60, RPMRemaining: 12, RPMReset: 30 * time.Second,
		TPM: 100000, TPMRemaining: 4200, TPMReset: 5 * time.Second,
		Concurrency: 4, InFlight: 1,
	})
	header := rec.Header()
	for name, want := range map[string]string{
		"X-Ratelimit-Limit-Requests":        "60",
		"X-Ratelimit-Remaining-Requests":    "12",
		"X-Ratelimit-Reset-Requests":        "30s",
		"X-Ratelimit-Limit-Tokens":          "100000",
		"X-Ratelimit-Remaining-Tokens":      "4200",
		"X-Ratelimit-Reset-Tokens":          "5s",
		"X-Ratelimit-Limit-Concurrency":     "4",
		"X-Ratelimit-Remaining-Concurrency": "3",
	} {
		if got := header.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestWriteLimitError(t *testing.T) {
	for _, test := range []struct {
		kind   LimitKind
		header string
	}{
		{LimitRequestsPerMinute, "X-Ratelimit-Limit-Requests"},
		{LimitTokensPerMinute, "X-Ratelimit-Limit-Tokens"},
		{LimitConcurrency, "X-Ratelimit-Limit-Concurrency"},
	} {
		rec := httptest.NewRecorder()
		writeLimitError(rec, &LimitError{Kind: test.kind, Limit: 5, RetryAfter: 2300 * time.Millisecond})
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s: status = %d", test.kind, rec.Code)
		}
		if got := rec.Header().Get("Retry-After"); got != "3" {
			t.Fatalf("%s: Retry-After = %q", test.kind, got)
		}
		if got := rec.Header().Get(test.header); got != "5" {
			t.Fatalf("%s: %s = %q", test.kind, test.header, got)
		}
		if !strings.Contains(rec.Body.String(), `"code":"rate_limit_exceeded"`) {
			t.Fatalf("%s: unexpected body %s", test.kind, rec.Body.String())
		}
	}
}

func TestCompletionTokenUpper(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  map[string]any
		want int64
	}{
		{"absent", map[string]any{}, 0},
		{"max_tokens", map[string]any{"max_tokens": float64(100)}, 100},
		{"larger field wins", map[string]any{"max_tokens": float64(100), "max_completion_tokens": float64(250)}, 250},
		{"choices multiply", map[string]any{"max_tokens": float64(100), "n": float64(3)}, 300},
		{"negative is ignored", map[string]any{"max_tokens": float64(-5)}, 0},
		{"huge output is capped", map[string]any{"max_tokens": float64(1 << 40)}, maxReservedCompletionTokens},
		{"choices are capped", map[string]any{"max_tokens": float64(100), "n": float64(1000)}, 100 * maxReservedChoices},
	} {
		if got := completionTokenUpper(test.raw); got != test.want {
			t.Fatalf("%s: got %d, want %d", test.name, got, test.want)
		}
	}
}

func TestLimitStateReservesDeclaredOutputTokens(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := newTestLimitState(&now)
	// 10 prompt tokens plus a 150-token output limit cannot fit a 100 TPM budget.
	if _, err := state.Admit(admission{TokenID: 5, RequestID: "req-a", TPMLimit: 100, PromptTokens: 10, CompletionTokens: 150}); err == nil || err.Kind != LimitTokensPerMinute {
		t.Fatalf("expected declared output to be reserved, got %v", err)
	}
}
