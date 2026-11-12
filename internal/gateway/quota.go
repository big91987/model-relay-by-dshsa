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
	promptUpper        int64
	completionUpper    int64
	maxInputPrice      float64
	maxOutputPrice     float64
	amount             float64
	capOutput          bool
	outputCapPerChoice int64
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

func maxCandidatePrices(candidates []candidate) (input, output float64) {
	for i := range candidates {
		if candidates[i].channel.InputPrice > input {
			input = candidates[i].channel.InputPrice
		}
		if candidates[i].channel.OutputPrice > output {
			output = candidates[i].channel.OutputPrice
		}
	}
	return input, output
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
	inputPrice, outputPrice := maxCandidatePrices(candidates)
	promptUpper := promptTokenUpperBound(body, messages)
	return quotaPlan{
		promptUpper:        promptUpper,
		completionUpper:    completionUpper,
		maxInputPrice:      inputPrice,
		maxOutputPrice:     outputPrice,
		amount:             roundUp6((float64(promptUpper)*inputPrice + float64(completionUpper)*outputPrice) / 1_000_000),
		capOutput:          capOutput,
		outputCapPerChoice: perChoice,
	}
}

// planEmbeddingQuota computes the upper bound for an embeddings request, which
// has input tokens but no completion tokens.
func (g *Gateway) planEmbeddingQuota(body []byte, candidates []candidate) quotaPlan {
	inputPrice, _ := maxCandidatePrices(candidates)
	promptUpper := promptTokenUpperBound(body, 0)
	return quotaPlan{
		promptUpper:   promptUpper,
		maxInputPrice: inputPrice,
		amount:        roundUp6(float64(promptUpper) * inputPrice / 1_000_000),
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

// usageStatus reports whether usage came from the upstream or was estimated
// locally from content.
func usageStatus(reported bool) string {
	if reported {
		return store.CostActual
	}
	return store.CostEstimated
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
		} `json:"usage"`
	}
	_ = json.Unmarshal(data, &parsed)
	if parsed.Usage == nil {
		return upstreamUsage{}
	}
	return upstreamUsage{
		prompt:     parsed.Usage.PromptTokens,
		completion: parsed.Usage.CompletionTokens,
		reported:   parsed.Usage.PromptTokens > 0 || parsed.Usage.CompletionTokens > 0 || parsed.Usage.TotalTokens > 0,
	}
}

func roundUp6(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return math.Ceil(v*1e6-1e-9) / 1e6
}
