package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	modelsFetchTimeout = 30 * time.Second
	maxModelsPages     = 10
	maxModelsBody      = 4 << 20
)

// MiniMax's OpenAI-compatible model list only returns language models, but the
// V2 video endpoint also accepts the H3 family. Add those so the imported
// catalog is complete for the video console.
var minimaxVideoModels = []string{
	"MiniMax-H3",
	"MiniMax-H3-Highspeed",
	"MiniMax-H3-Lite",
	"MiniMax-H3-Max",
}

// Ark's /api/v3/models endpoint lists the models an account can call. Seedance
// is newer than most listings, so keep a small fallback catalog to make the
// video page usable right after a fetch.
var seedanceVideoModels = []string{
	"doubao-seedance-2-5-260628",
	"doubao-seedance-2-0-260128",
	"doubao-seedance-1-0-pro-250528",
	"doubao-seedance-1-0-pro-fast-251015",
}

// FetchModels asks an upstream provider for the models it serves.
//
// OpenAI-compatible providers expose GET {base}/models with a bearer token.
// Anthropic uses the same path with x-api-key and its version header, and
// paginates with after_id/last_id/has_more. The returned list is deduplicated
// and sorted so the console can present a stable checkbox list.
func (g *Gateway) FetchModels(ctx context.Context, channelType, baseURL, apiKey string) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, errors.New("base URL is required")
	}
	ctx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
	defer cancel()

	seen := map[string]bool{}
	models := make([]string, 0, 32)
	afterID := ""
	for page := 0; page < maxModelsPages; page++ {
		endpoint := base + "/models"
		if channelType == "minimax" {
			// MiniMax exposes its OpenAI-compatible model list below /v1 while
			// its video tasks live at the API root.
			endpoint = base + "/v1/models"
			if strings.HasSuffix(base, "/v1") {
				endpoint = base + "/models"
			}
		}
		if afterID != "" {
			endpoint += "?after_id=" + url.QueryEscape(afterID)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if channelType == "anthropic" {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch models: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			msg := readErrorBody(resp)
			resp.Body.Close()
			return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, msg)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read models response: %w", err)
		}
		ids, hasMore, lastID, err := parseModelList(data)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, id)
		}
		if !hasMore || lastID == "" {
			break
		}
		afterID = lastID
	}
	if channelType == "minimax" {
		for _, id := range minimaxVideoModels {
			if seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, id)
		}
	}
	if channelType == "seedance" {
		for _, id := range seedanceVideoModels {
			if seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, id)
		}
	}
	sort.Strings(models)
	return models, nil
}

// parseModelList accepts the common model-list shapes: {"data":[{"id":…}]},
// {"models":[…]}, a bare array, and items that are either strings or objects.
func parseModelList(data []byte) (ids []string, hasMore bool, lastID string, err error) {
	var payload struct {
		Data    []json.RawMessage `json:"data"`
		Models  []json.RawMessage `json:"models"`
		HasMore bool              `json:"has_more"`
		LastID  string            `json:"last_id"`
	}
	if json.Unmarshal(data, &payload) != nil {
		var list []json.RawMessage
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, false, "", errors.New("upstream returned an unexpected models payload")
		}
		payload.Data = list
	}
	items := payload.Data
	if len(items) == 0 {
		items = payload.Models
	}
	ids = make([]string, 0, len(items))
	for _, item := range items {
		if id := modelID(item); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, payload.HasMore, payload.LastID, nil
}

func modelID(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return strings.TrimSpace(id)
	}
	var obj struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	for _, candidate := range []string{obj.ID, obj.Model, obj.Name} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	return ""
}
