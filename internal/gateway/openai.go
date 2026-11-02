package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"
)

// estimateChatPrompt approximates prompt tokens when an upstream omits usage.
func estimateChatPrompt(p openAIChatPayload) int64 {
	var total int64
	for _, m := range p.Messages {
		total += 4
		total += estimateTokens(m.Role)
		total += estimateTokens(contentText(m.Content))
	}
	return total + 3
}

// estimateTokens is a coarse fallback used only when the upstream reports no usage.
func estimateTokens(text string) int64 {
	if text == "" {
		return 0
	}
	n := int64(math.Ceil(float64(utf8.RuneCountInString(text)) / 3.5))
	if n < 1 {
		n = 1
	}
	return n
}

// contentText extracts plain text from an OpenAI message content value.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			b.WriteString(p.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// openAIUsage extracts usage from a non-streaming completion, estimating missing numbers.
func openAIUsage(data []byte, promptFallback int64) (prompt, completion int64) {
	var parsed struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(data, &parsed)
	prompt, completion = parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens
	if prompt == 0 {
		prompt = promptFallback
	}
	if completion == 0 {
		var b strings.Builder
		for _, c := range parsed.Choices {
			b.WriteString(c.Message.Content)
			for _, tc := range c.Message.ToolCalls {
				b.WriteString(tc.Function.Arguments)
			}
		}
		completion = estimateTokens(b.String())
	}
	return prompt, completion
}

// streamOpenAI copies an SSE stream to the client while tracking usage.
func (g *Gateway) streamOpenAI(w http.ResponseWriter, resp *http.Response, promptFallback int64) (int64, int64, error) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var prompt, completion int64
	var content, toolArgs strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return prompt, completion, err
		}
		if flusher != nil {
			flusher.Flush()
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				prompt = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				completion = chunk.Usage.CompletionTokens
			}
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
			for _, tc := range c.Delta.ToolCalls {
				toolArgs.WriteString(tc.Function.Arguments)
			}
		}
	}
	if prompt == 0 {
		prompt = promptFallback
	}
	if completion == 0 {
		completion = estimateTokens(content.String()) + estimateTokens(toolArgs.String())
	}
	return prompt, completion, scanner.Err()
}
