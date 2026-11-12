package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

// playgroundToken loads the gateway key selected in the console. The browser
// only sends the key id; the key secret is never returned to the browser.
func (s *Server) playgroundToken(w http.ResponseWriter, r *http.Request) (*store.Token, bool) {
	id := int64(queryInt(r, "token_id", 0))
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, "token_id query parameter is required")
		return nil, false
	}
	token, err := s.store.GetTokenByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "gateway key not found")
		return nil, false
	}
	if err != nil {
		s.logger.Error("load playground gateway key failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load gateway key")
		return nil, false
	}
	return token, true
}

// handlePlaygroundModels shows the models available to the selected key. It
// uses the gateway's own model filtering so the list matches /v1/models.
func (s *Server) handlePlaygroundModels(w http.ResponseWriter, r *http.Request) {
	token, ok := s.playgroundToken(w, r)
	if !ok {
		return
	}
	if token.Status != "active" {
		writeOpenAIError(w, http.StatusUnauthorized, "this gateway key is disabled", "invalid_api_key", "key_disabled")
		return
	}
	if token.ExpiresAt != nil && *token.ExpiresAt <= time.Now().Unix() {
		writeOpenAIError(w, http.StatusUnauthorized, "this gateway key has expired", "invalid_api_key", "key_expired")
		return
	}
	s.gw.Models(w, r, token)
}

// handlePlaygroundChat runs the selected key through the relay's chat handler.
// The request bytes are forwarded unchanged, so routing, model permissions,
// quota, rate limiting, usage accounting and request logs all behave like a
// normal /v1/chat/completions call.
func (s *Server) handlePlaygroundChat(w http.ResponseWriter, r *http.Request) {
	token, ok := s.playgroundToken(w, r)
	if !ok {
		return
	}
	s.gw.ChatCompletions(w, r, token)
}
