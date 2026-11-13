package gateway

import (
	"math"
	"math/rand/v2"
	"sort"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

type candidate struct {
	channel  store.Channel
	upstream string
	rank     float64
}

// resolveModel returns the upstream model name for a public model.
func resolveModel(ch *store.Channel, model string) (string, bool) {
	if upstream, ok := ch.ModelMapping[model]; ok && upstream != "" {
		return upstream, true
	}
	for _, m := range ch.Models {
		if m == model {
			return model, true
		}
	}
	return "", false
}

// orderCandidates selects channels supporting model: priority first, then a
// weighted random order so load spreads across channels of the same priority.
func orderCandidates(channels []store.Channel, model string) []candidate {
	out := make([]candidate, 0, len(channels))
	for i := range channels {
		ch := channels[i]
		if ch.Status != "enabled" {
			continue
		}
		upstream, ok := resolveModel(&ch, model)
		if !ok {
			continue
		}
		weight := ch.Weight
		if weight < 1 {
			weight = 1
		}
		u := rand.Float64()
		if u <= 0 {
			u = 1e-12
		}
		out = append(out, candidate{
			channel:  ch,
			upstream: upstream,
			rank:     -math.Log(u) / float64(weight),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].channel.Priority != out[j].channel.Priority {
			return out[i].channel.Priority > out[j].channel.Priority
		}
		return out[i].rank < out[j].rank
	})
	return out
}

// isChatChannel reports whether a channel type serves chat completions.
func isChatChannel(typ string) bool { return typ == "openai" || typ == "anthropic" }

// isVideoChannel reports whether a channel type serves the asynchronous video
// workflow instead of chat completions.
func isVideoChannel(typ string) bool { return typ == "minimax" || typ == "seedance" }

// isMusicChannel reports whether a channel type serves the synchronous music
// generation workflow.
func isMusicChannel(typ string) bool { return typ == "minimax-music" }

// chatCandidates keeps only channels that speak a chat-capable protocol.
// MiniMax and Seedance video channels are served by the separate video task
// flow.
func chatCandidates(candidates []candidate) []candidate {
	out := make([]candidate, 0, len(candidates))
	for _, cand := range candidates {
		if isChatChannel(cand.channel.Type) {
			out = append(out, cand)
		}
	}
	return out
}

func sortStrings(v []string) { sort.Strings(v) }
