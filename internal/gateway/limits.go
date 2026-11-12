package gateway

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateWindow is the sliding window used by the per-key request and token
// limits. It matches the "per minute" wording shown to API users.
const rateWindow = time.Minute

// LimitKind identifies the per-key limit that rejected a request.
type LimitKind string

const (
	// LimitRequestsPerMinute is the RPM limit.
	LimitRequestsPerMinute LimitKind = "requests_per_minute"
	// LimitTokensPerMinute is the TPM limit, counting prompt and completion tokens.
	LimitTokensPerMinute LimitKind = "tokens_per_minute"
	// LimitConcurrency bounds the number of requests a key may have in flight.
	LimitConcurrency LimitKind = "concurrency"
)

// LimitError reports a rejected request and the limit it exceeded.
type LimitError struct {
	Kind       LimitKind
	Limit      int
	Remaining  int64
	RetryAfter time.Duration
}

// Error implements the error interface.
func (e *LimitError) Error() string {
	switch e.Kind {
	case LimitRequestsPerMinute:
		return fmt.Sprintf("rate limit exceeded: %d requests per minute for this API key", e.Limit)
	case LimitTokensPerMinute:
		return fmt.Sprintf("rate limit exceeded: %d tokens per minute for this API key", e.Limit)
	case LimitConcurrency:
		return fmt.Sprintf("concurrency limit exceeded: %d requests in flight for this API key", e.Limit)
	default:
		return "rate limit exceeded"
	}
}

type tokenSample struct {
	at    time.Time
	count int64
}

// tokenWindow is the in-memory limiter state of one gateway key.
type tokenWindow struct {
	requests []time.Time
	samples  []tokenSample
	pending  map[string]int64
	inflight int
}

// prune drops samples outside the sliding window.
func (w *tokenWindow) prune(cutoff time.Time) {
	kept := w.requests[:0]
	for _, at := range w.requests {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	w.requests = kept
	samples := w.samples[:0]
	for _, sample := range w.samples {
		if sample.at.After(cutoff) {
			samples = append(samples, sample)
		}
	}
	w.samples = samples
}

// usedTokens returns settled tokens plus reservations held by in-flight requests.
func (w *tokenWindow) usedTokens() int64 {
	var total int64
	for _, sample := range w.samples {
		total += sample.count
	}
	for _, count := range w.pending {
		total += count
	}
	return total
}

type admission struct {
	TokenID          int64
	RequestID        string
	RPMLimit         int
	TPMLimit         int
	MaxConcurrency   int
	PromptTokens     int64
	CompletionTokens int64
}

// reservedTokens is the token budget a request holds while it is in flight:
// its estimated prompt plus, when the client declared one, its output limit.
func (in admission) reservedTokens() int64 {
	return in.PromptTokens + in.CompletionTokens
}

// Completion reservations are bounded so a hostile max_tokens value cannot
// overflow the token arithmetic. Real output limits are far below these caps.
const (
	maxReservedCompletionTokens = 1 << 20
	maxReservedChoices          = 128
)

// completionTokenUpper returns the output tokens the client allows, multiplied
// by n, so TPM admission can reserve them. Zero means the request declared no
// output limit, in which case only the prompt is reserved and completion
// tokens are counted when the request settles.
func completionTokenUpper(raw map[string]any) int64 {
	var limit int64
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := intValue(raw[key]); ok && v > limit {
			limit = v
		}
	}
	if limit <= 0 {
		return 0
	}
	if limit > maxReservedCompletionTokens {
		limit = maxReservedCompletionTokens
	}
	choices := int64(1)
	if v, ok := intValue(raw["n"]); ok && v > choices {
		choices = v
	}
	if choices > maxReservedChoices {
		choices = maxReservedChoices
	}
	return limit * choices
}

// limitSnapshot is the state advertised through rate-limit response headers.
type limitSnapshot struct {
	RPM          int
	RPMRemaining int
	RPMReset     time.Duration
	TPM          int
	TPMRemaining int64
	TPMReset     time.Duration
	Concurrency  int
	InFlight     int
}

// limitState tracks per-key windows. It is deliberately in memory: the relay
// is a single binary backed by SQLite, and limits only need to be accurate for
// the requests this process is serving.
type limitState struct {
	mu      sync.Mutex
	byToken map[int64]*tokenWindow
	now     func() time.Time
}

func newLimitState() *limitState {
	return &limitState{byToken: map[int64]*tokenWindow{}, now: time.Now}
}

// lease represents an admitted request. Release must be called when the
// request finishes; Settle records its token usage first.
type lease struct {
	limits    *limitState
	tokenID   int64
	requestID string
	snapshot  limitSnapshot
	once      sync.Once
}

// Release drops any reservation the request still holds and frees its
// concurrency slot. It is safe to call more than once.
func (l *lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() { l.limits.release(l.tokenID, l.requestID) })
}

// Snapshot returns the limit state captured when the request was admitted.
func (l *lease) Snapshot() limitSnapshot {
	if l == nil {
		return limitSnapshot{}
	}
	return l.snapshot
}

// Admit checks every per-key limit and records the request when it fits.
func (l *limitState) Admit(in admission) (*lease, *LimitError) {
	if in.RPMLimit <= 0 && in.TPMLimit <= 0 && in.MaxConcurrency <= 0 {
		return &lease{limits: l, tokenID: in.TokenID, requestID: in.RequestID}, nil
	}
	now := l.now()
	cutoff := now.Add(-rateWindow)

	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.byToken[in.TokenID]
	if w == nil {
		w = &tokenWindow{pending: map[string]int64{}}
		l.byToken[in.TokenID] = w
	}
	w.prune(cutoff)

	if lerr := w.check(in, now); lerr != nil {
		return nil, lerr
	}

	if in.RPMLimit > 0 {
		w.requests = append(w.requests, now)
	}
	if reserve := in.reservedTokens(); in.TPMLimit > 0 && reserve > 0 {
		w.pending[in.RequestID] += reserve
	}
	w.inflight++

	snapshot := limitSnapshot{Concurrency: in.MaxConcurrency, InFlight: w.inflight}
	if in.RPMLimit > 0 {
		snapshot.RPM = in.RPMLimit
		snapshot.RPMRemaining = in.RPMLimit - len(w.requests)
		if len(w.requests) > 0 {
			snapshot.RPMReset = w.requests[0].Add(rateWindow).Sub(now)
		}
	}
	if in.TPMLimit > 0 {
		snapshot.TPM = in.TPMLimit
		snapshot.TPMRemaining = int64(in.TPMLimit) - w.usedTokens()
		if len(w.samples) > 0 {
			snapshot.TPMReset = w.samples[0].at.Add(rateWindow).Sub(now)
		}
	}
	return &lease{limits: l, tokenID: in.TokenID, requestID: in.RequestID, snapshot: snapshot}, nil
}

func (w *tokenWindow) check(in admission, now time.Time) *LimitError {
	if in.MaxConcurrency > 0 && w.inflight >= in.MaxConcurrency {
		remaining := int64(in.MaxConcurrency - w.inflight)
		if remaining < 0 {
			remaining = 0
		}
		return &LimitError{Kind: LimitConcurrency, Limit: in.MaxConcurrency, Remaining: remaining, RetryAfter: time.Second}
	}
	if in.RPMLimit > 0 && len(w.requests) >= in.RPMLimit {
		retry := time.Second
		if len(w.requests) > 0 {
			retry = w.requests[0].Add(rateWindow).Sub(now)
		}
		return &LimitError{Kind: LimitRequestsPerMinute, Limit: in.RPMLimit, Remaining: 0, RetryAfter: retry}
	}
	if in.TPMLimit > 0 {
		reserve := in.reservedTokens()
		used := w.usedTokens()
		limit := int64(in.TPMLimit)
		if used+reserve > limit {
			remaining := limit - used
			if remaining < 0 {
				remaining = 0
			}
			return &LimitError{
				Kind:       LimitTokensPerMinute,
				Limit:      in.TPMLimit,
				Remaining:  remaining,
				RetryAfter: w.tokensRetry(now, used+reserve-limit),
			}
		}
	}
	return nil
}

// tokensRetry estimates how long until enough token capacity leaves the
// window. Reservations from in-flight requests cannot be aged out, so those
// report a short retry delay instead.
func (w *tokenWindow) tokensRetry(now time.Time, excess int64) time.Duration {
	if excess <= 0 {
		return time.Second
	}
	if len(w.pending) > 0 {
		return time.Second
	}
	var freed int64
	for _, sample := range w.samples {
		freed += sample.count
		if freed >= excess {
			wait := sample.at.Add(rateWindow).Sub(now)
			if wait < time.Second {
				wait = time.Second
			}
			if wait > rateWindow {
				wait = rateWindow
			}
			return wait
		}
	}
	return rateWindow
}

// release frees the concurrency slot and drops the token reservation of a
// request that finished without settling.
func (l *limitState) release(tokenID int64, requestID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.byToken[tokenID]
	if w == nil {
		return
	}
	if w.inflight > 0 {
		w.inflight--
	}
	delete(w.pending, requestID)
}

// Settle replaces the admission reservation of a finished request with its
// actual token usage. It is safe to call for requests that were never admitted.
func (l *limitState) Settle(tokenID int64, requestID string, prompt, completion int64) {
	total := prompt + completion
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.byToken[tokenID]
	if w == nil {
		// No limit was active when the request was admitted; keeping samples
		// here would grow the state of unlimited keys without bound.
		return
	}
	w.prune(now.Add(-rateWindow))
	delete(w.pending, requestID)
	if total > 0 {
		w.samples = append(w.samples, tokenSample{at: now, count: total})
	}
}

// writeRateLimitHeaders advertises the limits applied to a request.
func writeRateLimitHeaders(w http.ResponseWriter, snapshot limitSnapshot) {
	if snapshot.RPM > 0 {
		w.Header().Set("X-RateLimit-Limit-Requests", strconv.Itoa(snapshot.RPM))
		w.Header().Set("X-RateLimit-Remaining-Requests", strconv.Itoa(maxInt(snapshot.RPMRemaining, 0)))
		w.Header().Set("X-RateLimit-Reset-Requests", resetValue(snapshot.RPMReset))
	}
	if snapshot.TPM > 0 {
		w.Header().Set("X-RateLimit-Limit-Tokens", strconv.Itoa(snapshot.TPM))
		w.Header().Set("X-RateLimit-Remaining-Tokens", strconv.FormatInt(maxInt64(snapshot.TPMRemaining, 0), 10))
		w.Header().Set("X-RateLimit-Reset-Tokens", resetValue(snapshot.TPMReset))
	}
	if snapshot.Concurrency > 0 {
		w.Header().Set("X-RateLimit-Limit-Concurrency", strconv.Itoa(snapshot.Concurrency))
		w.Header().Set("X-RateLimit-Remaining-Concurrency", strconv.Itoa(maxInt(snapshot.Concurrency-snapshot.InFlight, 0)))
	}
}

// writeLimitError writes an OpenAI-compatible 429 plus the corresponding
// rate-limit headers.
func writeLimitError(w http.ResponseWriter, err *LimitError) {
	w.Header().Set("Retry-After", strconv.Itoa(maxInt(int(math.Ceil(err.RetryAfter.Seconds())), 1)))
	switch err.Kind {
	case LimitRequestsPerMinute:
		w.Header().Set("X-RateLimit-Limit-Requests", strconv.Itoa(err.Limit))
		w.Header().Set("X-RateLimit-Remaining-Requests", "0")
	case LimitTokensPerMinute:
		w.Header().Set("X-RateLimit-Limit-Tokens", strconv.Itoa(err.Limit))
		w.Header().Set("X-RateLimit-Remaining-Tokens", strconv.FormatInt(maxInt64(err.Remaining, 0), 10))
	case LimitConcurrency:
		w.Header().Set("X-RateLimit-Limit-Concurrency", strconv.Itoa(err.Limit))
		w.Header().Set("X-RateLimit-Remaining-Concurrency", "0")
	}
	writeOpenAIError(w, http.StatusTooManyRequests, err.Error(), "rate_limit_exceeded", "rate_limit_exceeded")
}

// resetValue formats a reset hint as whole seconds until capacity returns.
func resetValue(d time.Duration) string {
	seconds := int(math.Ceil(d.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds) + "s"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
