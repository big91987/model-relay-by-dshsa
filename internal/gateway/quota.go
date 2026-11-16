package gateway

import (
	"encoding/json"
	"math"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

const defaultQuotaOutputTokens int64 = 4096

// quotaPlan is the cost upper bound computed before a request may reach an
// upstream. promptUpper is a byte-based upper bound on prompt tokens (a token
// never encodes fewer than one UTF-8 byte), and completionUpper is the output
// limit declared by the client or applied by the gateway.
type quotaPlan struct {
	amount             float64
	capOutput          bool
	outputCapPerChoice int64
	// bounds is the per-candidate cost upper bound. A request may fail over
	// through several candidates and each one can bill, so the reservation
	// covers the sum, not just the most expensive single attempt.
	bounds map[int64]float64
}

func (g *Gateway) defaultOutputTokens() int64 {
	if g.cfg != nil && g.cfg.QuotaDefaultOutputTokens > 0 {
		return int64(g.cfg.QuotaDefaultOutputTokens)
	}
	return defaultQuotaOutputTokens
}

// promptTokenUpperBound returns a conservative upper bound on the number of
// tokens a request body can consume. Every byte can be at most one token, and
// the per-message overhead covers chat framing tokens.
func promptTokenUpperBound(body []byte, messages int) int64 {
	upper := int64(len(body)) + int64(messages)*16 + 64
	if upper < 1 {
		upper = 1
	}
	return upper
}

// requestedOutputTokens returns the output limit declared by the client. The
// larger of max_completion_tokens and max_tokens wins because providers differ
// in which field they honour.
func requestedOutputTokens(raw map[string]any) int64 {
	var limit int64
	if v, ok := intValue(raw["max_completion_tokens"]); ok && v > limit {
		limit = v
	}
	if v, ok := intValue(raw["max_tokens"]); ok && v > limit {
		limit = v
	}
	if limit < 0 {
		limit = 0
	}
	return limit
}

// requestedChoices returns the number of completions requested with n.
func requestedChoices(raw map[string]any) int64 {
	n, ok := intValue(raw["n"])
	if !ok || n < 1 {
		return 1
	}
	return n
}

// saturatingMul multiplies positive token counts without overflowing int64.
// Overflow would turn a cost upper bound negative and bypass the quota check.
func saturatingMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

// candidateBounds calculates the cost upper bound of every candidate. Explicit
// free channels contribute zero; unknown-price channels contribute zero here
// but are rejected earlier for finite-quota keys. The sum is the amount that
// must be reserved because failover can bill more than one attempt.
func candidateBounds(promptUpper, completionUpper int64, candidates []candidate) (float64, map[int64]float64) {
	bounds := make(map[int64]float64, len(candidates))
	total := 0.0
	for i := range candidates {
		ch := &candidates[i].channel
		bound := 0.0
		if ch.PriceMode == store.PriceModePriced {
			bound = roundUp6((float64(promptUpper)*ch.InputPrice + float64(completionUpper)*ch.OutputPrice) / 1_000_000)
		}
		bounds[ch.ID] = bound
		total += bound
	}
	return roundUp6(total), bounds
}

// unpricedCandidateNames lists candidates whose pricing mode is unknown. A
// finite-quota key must not call them, because neither the admission upper
// bound nor the final charge can be computed.
func unpricedCandidateNames(candidates []candidate) []string {
	names := make([]string, 0)
	for i := range candidates {
		mode := candidates[i].channel.PriceMode
		if mode == "" || mode == store.PriceModeUnset {
			names = append(names, candidates[i].channel.Name)
		}
	}
	return names
}

// planChatQuota computes the upper bound used to reserve quota for a chat
// request. When the client omits an output limit, the configured default is
// used and capOutput asks the gateway to send that limit upstream so the bound
// is real rather than assumed.
func (g *Gateway) planChatQuota(body []byte, raw map[string]any, messages int, candidates []candidate) quotaPlan {
	perChoice := requestedOutputTokens(raw)
	capOutput := perChoice == 0
	if capOutput {
		perChoice = g.defaultOutputTokens()
	}
	completionUpper := saturatingMul(perChoice, requestedChoices(raw))
	promptUpper := promptTokenUpperBound(body, messages)
	amount, bounds := candidateBounds(promptUpper, completionUpper, candidates)
	return quotaPlan{
		amount:             amount,
		capOutput:          capOutput,
		outputCapPerChoice: perChoice,
		bounds:             bounds,
	}
}

// planEmbeddingQuota computes the upper bound for an embeddings request, which
// has input tokens but no completion tokens.
func (g *Gateway) planEmbeddingQuota(body []byte, candidates []candidate) quotaPlan {
	promptUpper := promptTokenUpperBound(body, 0)
	amount, bounds := candidateBounds(promptUpper, 0, candidates)
	return quotaPlan{
		amount: amount,
		bounds: bounds,
	}
}

// reservationExpiry is informational: startup recovery reaps every pending
// reservation, while StreamTimeout/RequestTimeout bound how long a live request
// can keep its hold.
func (g *Gateway) reservationExpiry(stream bool) int64 {
	timeout := g.cfg.RequestTimeout
	if stream {
		timeout = g.cfg.StreamTimeout
	}
	seconds := int64(timeout / time.Second)
	if seconds <= 0 {
		seconds = 300
	}
	return time.Now().Unix() + seconds + 120
}

// reserveQuota creates the atomic hold for a request. The amount is the upper
// bound cost; any unused part is released when the request settles.
func (g *Gateway) reserveQuota(tok *store.Token, requestID, model string, plan quotaPlan, stream bool) (*store.Reservation, error) {
	return g.store.ReserveQuota(requestID, tok.ID, requestID, model, plan.amount, g.reservationExpiry(stream))
}

// costFor converts token usage into USD with the selected channel's prices.
func costFor(ch *store.Channel, prompt, completion int64) float64 {
	if ch == nil {
		return 0
	}
	return round6((float64(prompt)*ch.InputPrice + float64(completion)*ch.OutputPrice) / 1_000_000)
}

// buildAttemptCharge records one upstream call. billed=false marks a failed
// attempt whose response contained no usage: it is visible for attribution but
// contributes no charge. billed=true with a missing price component is marked
// unverified rather than pretending the missing part cost zero.
func buildAttemptCharge(ch *store.Channel, upstream string, statusCode int, prompt, completion int64, usageReported, billed bool) store.ChargeAttempt {
	a := store.ChargeAttempt{
		ChannelID:        ch.ID,
		ChannelName:      ch.Name,
		UpstreamModel:    upstream,
		StatusCode:       statusCode,
		PromptTokens:     prompt,
		CompletionTokens: completion,
	}
	if !billed {
		a.CostStatus = store.CostFree
		a.PromptTokens = 0
		a.CompletionTokens = 0
		return a
	}
	switch ch.PriceMode {
	case store.PriceModeFree:
		a.CostStatus = store.CostFree
	case store.PriceModePriced:
		a.Cost = costFor(ch, prompt, completion)
		if !usageReported {
			a.CostStatus = store.CostEstimated
			return a
		}
		a.CostStatus = store.CostActual
		if (prompt == 0 && ch.InputPrice > 0) || (completion == 0 && ch.OutputPrice > 0) {
			a.CostStatus = store.CostUnverified
		}
	default:
		a.CostStatus = store.CostUnpriced
	}
	return a
}

// combineCostStatus keeps the least trustworthy component visible in the
// request-level status.
func combineCostStatus(attempts []store.ChargeAttempt) string {
	seen := map[string]bool{}
	for i := range attempts {
		seen[attempts[i].CostStatus] = true
	}
	switch {
	case seen[store.CostUnpriced]:
		return store.CostUnpriced
	case seen[store.CostUnverified]:
		return store.CostUnverified
	case seen[store.CostEstimated]:
		return store.CostEstimated
	case seen[store.CostActual]:
		return store.CostActual
	case seen[store.CostFree]:
		return store.CostFree
	default:
		return store.CostFree
	}
}

func sumAttempts(attempts []store.ChargeAttempt) (prompt, completion int64, cost float64) {
	for i := range attempts {
		prompt += attempts[i].PromptTokens
		completion += attempts[i].CompletionTokens
		cost += attempts[i].Cost
	}
	return prompt, completion, round6(cost)
}

// upstreamUsage is a parsed usage block plus whether the upstream actually
// reported it.
type upstreamUsage struct {
	prompt     int64
	completion int64
	reported   bool
}

func usageFromJSON(data []byte) upstreamUsage {
	var parsed struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(data, &parsed)
	if parsed.Usage == nil {
		return upstreamUsage{}
	}
	prompt := parsed.Usage.PromptTokens
	if prompt == 0 {
		prompt = parsed.Usage.InputTokens
	}
	completion := parsed.Usage.CompletionTokens
	if completion == 0 {
		completion = parsed.Usage.OutputTokens
	}
	return upstreamUsage{
		prompt:     prompt,
		completion: completion,
		reported:   prompt > 0 || completion > 0 || parsed.Usage.TotalTokens > 0,
	}
}

func roundUp6(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return math.Ceil(v*1e6-1e-9) / 1e6
}
