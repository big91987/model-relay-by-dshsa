package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
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

// streamOpenAI copies an SSE stream to the client while tracking usage. Some
// providers ignore stream=true and return a normal JSON completion; in that
// case the completion is replayed as OpenAI SSE chunks so clients still receive
// a well-formed stream.
func (g *Gateway) streamOpenAI(w http.ResponseWriter, resp *http.Response, promptFallback int64, requestedModel string) (int64, int64, error) {
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return g.streamOpenAIFallback(w, resp, promptFallback, requestedModel)
	}
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

// streamOpenAIFallback turns a non-streaming completion into OpenAI SSE chunks.
func (g *Gateway) streamOpenAIFallback(w http.ResponseWriter, resp *http.Response, promptFallback int64, requestedModel string) (int64, int64, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, 0, err
	}
	var payload struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, 0, fmt.Errorf("upstream returned a non-streaming body that is not JSON: %w", err)
	}
	if len(payload.Choices) == 0 {
		return 0, 0, fmt.Errorf("upstream completion contained no choices")
	}
	choice := payload.Choices[0]
	id := payload.ID
	if id == "" {
		id = "chatcmpl-" + newRequestID()
	}
	created := payload.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	model := requestedModel
	if model == "" {
		model = "unknown"
	}
	prompt := payload.Usage.PromptTokens
	if prompt == 0 {
		prompt = promptFallback
	}
	completion := payload.Usage.CompletionTokens
	if completion == 0 {
		var text strings.Builder
		text.WriteString(choice.Message.Content)
		for _, call := range choice.Message.ToolCalls {
			text.WriteString(call.Function.Arguments)
		}
		completion = estimateTokens(text.String())
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	writeChunk := func(delta map[string]any, finishReason any) error {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			}},
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
			return err
		}
		flush()
		return nil
	}
	role := choice.Message.Role
	if role == "" {
		role = "assistant"
	}
	if err := writeChunk(map[string]any{"role": role, "content": ""}, nil); err != nil {
		return prompt, completion, err
	}
	remaining := choice.Message.Content
	for remaining != "" {
		runes := []rune(remaining)
		count := len(runes)
		if count > 24 {
			count = 24
		}
		part := string(runes[:count])
		remaining = string(runes[count:])
		if err := writeChunk(map[string]any{"content": part}, nil); err != nil {
			return prompt, completion, err
		}
	}
	for index, call := range choice.Message.ToolCalls {
		callType := call.Type
		if callType == "" {
			callType = "function"
		}
		if err := writeChunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index,
			"id":    call.ID,
			"type":  callType,
			"function": map[string]any{
				"name":      call.Function.Name,
				"arguments": "",
			},
		}}}, nil); err != nil {
			return prompt, completion, err
		}
		remainingArgs := call.Function.Arguments
		for remainingArgs != "" {
			runes := []rune(remainingArgs)
			count := len(runes)
			if count > 24 {
				count = 24
			}
			part := string(runes[:count])
			remainingArgs = string(runes[count:])
			if err := writeChunk(map[string]any{"tool_calls": []any{map[string]any{
				"index":    index,
				"function": map[string]any{"arguments": part},
			}}}, nil); err != nil {
				return prompt, completion, err
			}
		}
	}
	finishReason := choice.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	if err := writeChunk(map[string]any{}, finishReason); err != nil {
		return prompt, completion, err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return prompt, completion, err
	}
	flush()
	return prompt, completion, nil
}
