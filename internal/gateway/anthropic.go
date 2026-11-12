package gateway

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// buildAnthropicRequest translates an OpenAI chat request into an Anthropic Messages request.
func buildAnthropicRequest(raw map[string]any, upstream string) ([]byte, error) {
	out := map[string]any{"model": upstream}
	maxTokens := int64(4096)
	if v, ok := intValue(raw["max_completion_tokens"]); ok && v > 0 {
		maxTokens = v
	} else if v, ok := intValue(raw["max_tokens"]); ok && v > 0 {
		maxTokens = v
	}
	out["max_tokens"] = maxTokens
	if v, ok := raw["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := raw["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := raw["stream"].(bool); ok && v {
		out["stream"] = true
	}
	if v, ok := raw["stop"]; ok {
		if stops := stringList(v); len(stops) > 0 {
			out["stop_sequences"] = stops
		}
	}

	var systemParts []string
	var messages []any
	if list, ok := raw["messages"].([]any); ok {
		for _, item := range list {
			msg, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			switch role {
			case "system", "developer":
				if text := contentToText(msg["content"]); text != "" {
					systemParts = append(systemParts, text)
				}
			case "tool":
				callID, _ := msg["tool_call_id"].(string)
				result := contentToText(msg["content"])
				messages = append(messages, map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type":        "tool_result",
						"tool_use_id": callID,
						"content":     result,
					}},
				})
			case "assistant":
				blocks := make([]any, 0, 2)
				if text := contentToText(msg["content"]); text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
				if calls, ok := msg["tool_calls"].([]any); ok {
					for _, item := range calls {
						call, ok := item.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := call["function"].(map[string]any)
						name, _ := fn["name"].(string)
						if name == "" {
							continue
						}
						input := any(map[string]any{})
						if args, ok := fn["arguments"].(string); ok && strings.TrimSpace(args) != "" {
							var parsed any
							if json.Unmarshal([]byte(args), &parsed) == nil {
								input = parsed
							}
						}
						id, _ := call["id"].(string)
						if id == "" {
							id = "toolu_" + newRequestID()
						}
						blocks = append(blocks, map[string]any{
							"type":  "tool_use",
							"id":    id,
							"name":  name,
							"input": input,
						})
					}
				}
				if len(blocks) == 0 {
					blocks = append(blocks, map[string]any{"type": "text", "text": ""})
				}
				messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
			default:
				messages = append(messages, map[string]any{"role": "user", "content": anthropicContent(msg["content"])})
			}
		}
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("no messages provided")
	}
	out["messages"] = messages

	// Anthropic has no tool_choice "none"; omitting the tool definitions is the
	// closest equivalent and stops the model from attempting tool calls.
	toolChoice, _ := raw["tool_choice"].(string)
	if tools, ok := raw["tools"].([]any); ok && len(tools) > 0 && toolChoice != "none" {
		translated := make([]any, 0, len(tools))
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tool["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			if name == "" {
				continue
			}
			entry := map[string]any{"name": name}
			if desc, ok := fn["description"]; ok {
				entry["description"] = desc
			}
			if params, ok := fn["parameters"]; ok {
				entry["input_schema"] = params
			} else {
				entry["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			translated = append(translated, entry)
		}
		if len(translated) > 0 {
			out["tools"] = translated
			switch choice := raw["tool_choice"].(type) {
			case string:
				switch choice {
				case "auto":
					out["tool_choice"] = map[string]any{"type": "auto"}
				case "required":
					out["tool_choice"] = map[string]any{"type": "any"}
				}
			case map[string]any:
				if fn, ok := choice["function"].(map[string]any); ok {
					if name, _ := fn["name"].(string); name != "" {
						out["tool_choice"] = map[string]any{"type": "tool", "name": name}
					}
				}
			}
		}
	}
	return json.Marshal(out)
}

// anthropicContent converts an OpenAI message content value into Anthropic content blocks.
func anthropicContent(v any) []any {
	switch content := v.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": content}}
	case []any:
		blocks := make([]any, 0, len(content))
		for _, item := range content {
			part, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok {
					blocks = append(blocks, map[string]any{"type": "text", "text": s})
				}
				continue
			}
			switch part["type"] {
			case "text":
				if text, ok := part["text"].(string); ok {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				image, _ := part["image_url"].(map[string]any)
				url, _ := image["url"].(string)
				if block := imageBlock(url); block != nil {
					blocks = append(blocks, block)
				}
			}
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
		return blocks
	default:
		return []any{map[string]any{"type": "text", "text": contentToText(v)}}
	}
}

func imageBlock(url string) map[string]any {
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	comma := strings.Index(url, ",")
	if comma < 0 {
		return nil
	}
	meta := strings.TrimPrefix(url[:comma], "data:")
	if !strings.HasSuffix(meta, ";base64") {
		return nil
	}
	mediaType := strings.TrimSuffix(meta, ";base64")
	if mediaType == "" {
		return nil
	}
	data := url[comma+1:]
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		},
	}
}

func contentToText(v any) string {
	switch content := v.(type) {
	case string:
		return content
	case []any:
		var b strings.Builder
		for _, item := range content {
			if part, ok := item.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					b.WriteString(text)
					b.WriteString("\n")
				}
			} else if s, ok := item.(string); ok {
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		b, _ := json.Marshal(content)
		return string(b)
	}
}

func intValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func stringList(v any) []string {
	switch s := v.(type) {
	case string:
		if s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	case []string:
		return s
	}
	return nil
}

func mapStopReason(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence", "end_turn", "":
		return "stop"
	case "refusal":
		return "content_filter"
	}
	return "stop"
}

func chatID(id string) string {
	if id == "" {
		return "chatcmpl-" + newRequestID()
	}
	if strings.HasPrefix(id, "chatcmpl-") {
		return id
	}
	return "chatcmpl-" + strings.TrimPrefix(id, "msg_")
}

// anthropicToOpenAI converts a non-streaming Anthropic response into an OpenAI response.
func anthropicToOpenAI(data []byte, model string, promptFallback int64) ([]byte, int64, int64, bool, error) {
	var ar anthropicResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, 0, 0, false, err
	}
	var text strings.Builder
	var args strings.Builder
	toolCalls := make([]any, 0)
	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			input := "{}"
			if len(block.Input) > 0 && string(block.Input) != "null" {
				input = string(block.Input)
			}
			args.WriteString(input)
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": input,
				},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	reported := ar.Usage.InputTokens > 0 && ar.Usage.OutputTokens > 0
	prompt := ar.Usage.InputTokens
	if prompt == 0 {
		prompt = promptFallback
	}
	completion := ar.Usage.OutputTokens
	if completion == 0 {
		completion = estimateTokens(text.String() + args.String())
	}
	out := map[string]any{
		"id":      chatID(ar.ID),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": mapStopReason(ar.StopReason),
		}},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	}
	encoded, err := json.Marshal(out)
	return encoded, prompt, completion, reported, err
}

// streamAnthropic translates an Anthropic SSE stream into OpenAI chat completion chunks.
func (g *Gateway) streamAnthropic(w http.ResponseWriter, resp *http.Response, promptFallback int64, model string) (int64, int64, bool, error) {
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

	id := "chatcmpl-" + newRequestID()
	created := time.Now().Unix()
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
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	if err := writeChunk(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
		return 0, 0, false, err
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var prompt, completion int64
	var usageInput, usageOutput int64
	var text, toolArgs strings.Builder
	reported := false
	toolIndexByBlock := map[int]int{}
	nextTool := 0
	stopReason := "end_turn"
	done := false
	usageSent := false
	event := ""

	writeUsage := func() error {
		p, c := prompt, completion
		source := "upstream"
		if p == 0 {
			p = promptFallback
			source = "estimated"
		}
		if c == 0 {
			c = estimateTokens(text.String() + toolArgs.String())
			source = "estimated"
		}
		if err := writeUsageChunk(w, id, created, model, p, c, source); err != nil {
			return err
		}
		usageSent = true
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		typ := ev.Type
		if typ == "" {
			typ = event
		}
		switch typ {
		case "message_start":
			prompt = ev.Message.Usage.InputTokens
			completion = ev.Message.Usage.OutputTokens
			usageInput = ev.Message.Usage.InputTokens
			if ev.Message.Usage.OutputTokens > 0 {
				usageOutput = ev.Message.Usage.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				toolIndex := nextTool
				nextTool++
				toolIndexByBlock[ev.Index] = toolIndex
				delta := map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIndex,
					"id":    ev.ContentBlock.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      ev.ContentBlock.Name,
						"arguments": "",
					},
				}}}
				if err := writeChunk(delta, nil); err != nil {
					return prompt, completion, reported, err
				}
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				text.WriteString(ev.Delta.Text)
				if err := writeChunk(map[string]any{"content": ev.Delta.Text}, nil); err != nil {
					return prompt, completion, reported, err
				}
			case "input_json_delta":
				toolIndex := toolIndexByBlock[ev.Index]
				toolArgs.WriteString(ev.Delta.PartialJSON)
				delta := map[string]any{"tool_calls": []any{map[string]any{
					"index":    toolIndex,
					"function": map[string]any{"arguments": ev.Delta.PartialJSON},
				}}}
				if err := writeChunk(delta, nil); err != nil {
					return prompt, completion, reported, err
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage.OutputTokens > 0 {
				completion = ev.Usage.OutputTokens
				usageOutput = ev.Usage.OutputTokens
			}
			if usageInput > 0 && usageOutput > 0 {
				reported = true
			}
		case "message_stop":
			if err := writeChunk(map[string]any{}, mapStopReason(stopReason)); err != nil {
				return prompt, completion, reported, err
			}
			if !usageSent {
				if err := writeUsage(); err != nil {
					return prompt, completion, reported, err
				}
			}
			if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
				return prompt, completion, reported, err
			}
			if flusher != nil {
				flusher.Flush()
			}
			done = true
		case "error":
			msg := ev.Error.Message
			if msg == "" {
				msg = "upstream stream error"
			}
			encoded, _ := json.Marshal(map[string]any{"error": map[string]any{
				"message": msg,
				"type":    "upstream_error",
				"code":    ev.Error.Type,
			}})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", encoded)
			if flusher != nil {
				flusher.Flush()
			}
			return prompt, completion, reported, fmt.Errorf("%s", msg)
		}
		event = ""
		if done {
			break
		}
	}
	if !done {
		if streamErr := scanner.Err(); streamErr != nil {
			_ = writeStreamError(w, "upstream stream interrupted: "+streamErr.Error())
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		} else {
			_ = writeChunk(map[string]any{}, mapStopReason(stopReason))
			if !usageSent {
				_ = writeUsage()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if prompt == 0 {
		prompt = promptFallback
	}
	if completion == 0 {
		completion = estimateTokens(text.String() + toolArgs.String())
	}
	reported = usageInput > 0 && usageOutput > 0
	if err := scanner.Err(); err != nil {
		return prompt, completion, reported, err
	}
	if !done {
		return prompt, completion, reported, fmt.Errorf("upstream stream ended before message_stop")
	}
	return prompt, completion, reported, nil
}
