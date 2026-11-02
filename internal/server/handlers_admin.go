package server

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.store.GetDashboard()
	if err != nil {
		s.logger.Error("load dashboard failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load dashboard")
		return
	}
	writeJSON(w, http.StatusOK, dashboard)
}

type channelInput struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key"`
	Models       []string          `json:"models"`
	ModelMapping map[string]string `json:"model_mapping"`
	Priority     int               `json:"priority"`
	Weight       int               `json:"weight"`
	Status       string            `json:"status"`
	InputPrice   float64           `json:"input_price"`
	OutputPrice  float64           `json:"output_price"`
}

func (in *channelInput) normalize(requireKey bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return errors.New("name is required")
	}
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	if in.Type == "" {
		in.Type = "openai"
	}
	if in.Type != "openai" && in.Type != "anthropic" {
		return errors.New("type must be openai or anthropic")
	}
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if in.BaseURL == "" {
		return errors.New("base_url is required")
	}
	u, err := url.Parse(in.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("base_url must be an absolute http(s) URL")
	}
	if requireKey && strings.TrimSpace(in.APIKey) == "" {
		return errors.New("api_key is required")
	}
	if in.Status == "" {
		in.Status = "enabled"
	}
	if in.Status != "enabled" && in.Status != "disabled" {
		return errors.New("status must be enabled or disabled")
	}
	if in.Weight < 1 {
		in.Weight = 1
	}
	if in.Priority < 0 {
		return errors.New("priority must be zero or greater")
	}
	if in.InputPrice < 0 || in.OutputPrice < 0 {
		return errors.New("prices must be zero or greater")
	}
	in.Models = cleanList(in.Models)
	in.ModelMapping = cleanMapping(in.ModelMapping)
	if len(in.Models) == 0 && len(in.ModelMapping) == 0 {
		return errors.New("configure at least one model or model mapping")
	}
	return nil
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.store.ListChannels()
	if err != nil {
		s.logger.Error("list channels failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load channels")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": channels})
}

func (s *Server) handleGetChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	channel, err := s.store.GetChannel(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load channel")
		return
	}
	writeJSON(w, http.StatusOK, channel)
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var in channelInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := in.normalize(true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	encrypted, err := s.secrets.Encrypt(strings.TrimSpace(in.APIKey))
	if err != nil {
		s.logger.Error("encrypt channel key failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to store channel key")
		return
	}
	channel := &store.Channel{
		Name:            in.Name,
		Type:            in.Type,
		BaseURL:         in.BaseURL,
		APIKeyEncrypted: encrypted,
		APIKeyHint:      keyHint(in.APIKey),
		Models:          in.Models,
		ModelMapping:    in.ModelMapping,
		Priority:        in.Priority,
		Weight:          in.Weight,
		Status:          in.Status,
		InputPrice:      in.InputPrice,
		OutputPrice:     in.OutputPrice,
	}
	if err := s.store.CreateChannel(channel); err != nil {
		s.logger.Error("create channel failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to create channel")
		return
	}
	writeJSON(w, http.StatusCreated, channel)
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	channel, err := s.store.GetChannel(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load channel")
		return
	}
	var in channelInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := in.normalize(false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	channel.Name = in.Name
	channel.Type = in.Type
	channel.BaseURL = in.BaseURL
	channel.Models = in.Models
	channel.ModelMapping = in.ModelMapping
	channel.Priority = in.Priority
	channel.Weight = in.Weight
	channel.Status = in.Status
	channel.InputPrice = in.InputPrice
	channel.OutputPrice = in.OutputPrice
	if strings.TrimSpace(in.APIKey) != "" {
		encrypted, err := s.secrets.Encrypt(strings.TrimSpace(in.APIKey))
		if err != nil {
			s.logger.Error("encrypt channel key failed", "error", err)
			writeErr(w, http.StatusInternalServerError, "failed to store channel key")
			return
		}
		channel.APIKeyEncrypted = encrypted
		channel.APIKeyHint = keyHint(in.APIKey)
	}
	if err := s.store.UpdateChannel(channel); err != nil {
		s.logger.Error("update channel failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to update channel")
		return
	}
	writeJSON(w, http.StatusOK, channel)
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	if err := s.store.DeleteChannel(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "channel not found")
			return
		}
		s.logger.Error("delete channel failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to delete channel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	channel, err := s.store.GetChannel(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "channel not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load channel")
		return
	}
	result := s.gw.TestChannel(r.Context(), channel, r.URL.Query().Get("model"))
	writeJSON(w, http.StatusOK, result)
}

type tokenInput struct {
	Name          string   `json:"name"`
	Quota         *float64 `json:"quota"`
	AllowedModels []string `json:"allowed_models"`
	RPMLimit      int      `json:"rpm_limit"`
	ExpiresAt     *int64   `json:"expires_at"`
	Status        string   `json:"status"`
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListTokens()
	if err != nil {
		s.logger.Error("list tokens failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": tokens})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var in tokenInput
	if !decodeJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	status := "active"
	if in.Status != "" {
		if in.Status != "active" && in.Status != "disabled" {
			writeErr(w, http.StatusBadRequest, "status must be active or disabled")
			return
		}
		status = in.Status
	}
	if in.RPMLimit < 0 {
		writeErr(w, http.StatusBadRequest, "rpm_limit must be zero or greater")
		return
	}
	quota := -1.0
	if in.Quota != nil {
		quota = *in.Quota
	}
	if quota < -1 {
		writeErr(w, http.StatusBadRequest, "quota must be -1 (unlimited) or greater")
		return
	}
	plain, prefix, hash, err := auth.GenerateAPIKey()
	if err != nil {
		s.logger.Error("generate API key failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to generate API key")
		return
	}
	token := &store.Token{
		UserID:        userFrom(r).ID,
		Name:          name,
		KeyHash:       hash,
		KeyPrefix:     prefix,
		Status:        status,
		Quota:         quota,
		AllowedModels: cleanList(in.AllowedModels),
		RPMLimit:      in.RPMLimit,
		ExpiresAt:     in.ExpiresAt,
	}
	if err := s.store.CreateToken(token); err != nil {
		s.logger.Error("create token failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to create token")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "key": plain})
}

func (s *Server) handleUpdateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid token id")
		return
	}
	token, err := s.store.GetTokenByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load token")
		return
	}
	var in tokenInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if name := strings.TrimSpace(in.Name); name != "" {
		token.Name = name
	}
	if in.Quota != nil {
		if *in.Quota < -1 {
			writeErr(w, http.StatusBadRequest, "quota must be -1 (unlimited) or greater")
			return
		}
		token.Quota = *in.Quota
	}
	if in.RPMLimit < 0 {
		writeErr(w, http.StatusBadRequest, "rpm_limit must be zero or greater")
		return
	}
	token.RPMLimit = in.RPMLimit
	token.AllowedModels = cleanList(in.AllowedModels)
	token.ExpiresAt = in.ExpiresAt
	if in.Status != "" {
		if in.Status != "active" && in.Status != "disabled" {
			writeErr(w, http.StatusBadRequest, "status must be active or disabled")
			return
		}
		token.Status = in.Status
	}
	if err := s.store.UpdateToken(token); err != nil {
		s.logger.Error("update token failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to update token")
		return
	}
	writeJSON(w, http.StatusOK, token)
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid token id")
		return
	}
	if err := s.store.DeleteToken(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "token not found")
			return
		}
		s.logger.Error("delete token failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to delete token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListLogs(w http.ResponseWriter, r *http.Request) {
	filter := store.LogFilter{
		Page:     queryInt(r, "page", 1),
		PageSize: queryInt(r, "page_size", 50),
		Model:    strings.TrimSpace(r.URL.Query().Get("model")),
		Status:   strings.TrimSpace(r.URL.Query().Get("status")),
		TokenID:  int64(queryInt(r, "token_id", 0)),
	}
	if v := r.URL.Query().Get("start"); v != "" {
		if n, err := parseInt64(v); err == nil {
			filter.Start = n
		}
	}
	if v := r.URL.Query().Get("end"); v != "" {
		if n, err := parseInt64(v); err == nil {
			filter.End = n
		}
	}
	logs, total, err := s.store.ListLogs(filter)
	if err != nil {
		s.logger.Error("list logs failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load logs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     logs,
		"total":     total,
		"page":      filter.Page,
		"page_size": filter.PageSize,
	})
}

func (s *Server) handleListReservations(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListReservations(queryInt(r, "limit", 100))
	if err != nil {
		s.logger.Error("list quota ledger failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load quota ledger")
		return
	}
	pending := 0.0
	for _, item := range items {
		if item.Status == store.ReservationPending {
			pending += item.ReservedCost
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items, "pending_cost": pending})
}

type modelSummary struct {
	ID          string   `json:"id"`
	Channels    []string `json:"channels"`
	Types       []string `json:"types"`
	InputPrice  float64  `json:"input_price"`
	OutputPrice float64  `json:"output_price"`
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.store.ListChannels()
	if err != nil {
		s.logger.Error("list models failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load models")
		return
	}
	byID := map[string]*modelSummary{}
	for i := range channels {
		channel := &channels[i]
		if channel.Status != "enabled" {
			continue
		}
		ids := channelModelIDs(channel)
		for _, id := range ids {
			summary, ok := byID[id]
			if !ok {
				summary = &modelSummary{ID: id, InputPrice: channel.InputPrice, OutputPrice: channel.OutputPrice}
				byID[id] = summary
			}
			if channel.InputPrice < summary.InputPrice {
				summary.InputPrice = channel.InputPrice
			}
			if channel.OutputPrice < summary.OutputPrice {
				summary.OutputPrice = channel.OutputPrice
			}
			summary.Channels = appendUnique(summary.Channels, channel.Name)
			summary.Types = appendUnique(summary.Types, channel.Type)
		}
	}
	out := make([]*modelSummary, 0, len(byID))
	for _, summary := range byID {
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func channelModelIDs(channel *store.Channel) []string {
	seen := map[string]bool{}
	ids := make([]string, 0, len(channel.Models)+len(channel.ModelMapping))
	for _, m := range channel.Models {
		if m != "" && !seen[m] {
			seen[m] = true
			ids = append(ids, m)
		}
	}
	for m := range channel.ModelMapping {
		if m != "" && !seen[m] {
			seen[m] = true
			ids = append(ids, m)
		}
	}
	sort.Strings(ids)
	return ids
}

func cleanList(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func cleanMapping(in map[string]string) map[string]string {
	out := map[string]string{}
	for public, upstream := range in {
		public = strings.TrimSpace(public)
		upstream = strings.TrimSpace(upstream)
		if public == "" || upstream == "" {
			continue
		}
		out[public] = upstream
	}
	return out
}

func keyHint(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 4 {
		return "••••"
	}
	return "••••" + key[len(key)-4:]
}

func appendUnique(list []string, value string) []string {
	for _, item := range list {
		if item == value {
			return list
		}
	}
	return append(list, value)
}

func parseInt64(v string) (int64, error) {
	return strconv.ParseInt(v, 10, 64)
}
