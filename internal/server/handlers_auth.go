package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "username and password are required")
		return
	}
	ip := clientIP(r)
	if !s.loginLimiter.Allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "too many login attempts, please try again later")
		return
	}
	user, err := s.store.GetUserByUsername(username)
	if err != nil || user.Status != "enabled" || !auth.VerifyPassword(user.PasswordHash, req.Password) {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.logger.Error("login lookup failed", "error", err)
		}
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.loginLimiter.Reset(ip)
	now := time.Now()
	claims := auth.Claims{
		Sub:      user.ID,
		Username: user.Username,
		Role:     user.Role,
		IssuedAt: now.Unix(),
		Expires:  now.Add(s.cfg.SessionTTL).Unix(),
	}
	token, err := auth.Sign(s.cfg.SecretKey, claims)
	if err != nil {
		s.logger.Error("sign session failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "internal server error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
		Expires:  now.Add(s.cfg.SessionTTL),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": userFrom(r)})
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}
