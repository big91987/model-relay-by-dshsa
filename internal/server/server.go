// Package server exposes the admin API, the OpenAI-compatible relay and the web console.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"
	"github.com/big91987/model-relay-by-dshsa/internal/config"
	"github.com/big91987/model-relay-by-dshsa/internal/gateway"
	"github.com/big91987/model-relay-by-dshsa/internal/secretbox"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

type ctxKey int

const (
	userKey ctxKey = iota
	tokenKey
)

// Server wires the HTTP API and static console.
type Server struct {
	cfg          *config.Config
	store        *store.Store
	secrets      *secretbox.Cipher
	gw           *gateway.Gateway
	logger       *slog.Logger
	mux          *http.ServeMux
	started      time.Time
	loginLimiter *attemptLimiter
}

// New builds a Server.
func New(cfg *config.Config, st *store.Store, secrets *secretbox.Cipher, gw *gateway.Gateway, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		cfg:          cfg,
		store:        st,
		secrets:      secrets,
		gw:           gw,
		logger:       logger,
		started:      time.Now(),
		loginLimiter: newAttemptLimiter(10, 5*time.Minute),
	}
	s.routes()
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.withCommon(s.mux))
}

func (s *Server) routes() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)

	mux.Handle("GET /api/auth/me", s.withAdmin(http.HandlerFunc(s.handleMe)))
	mux.Handle("GET /api/dashboard", s.withAdmin(http.HandlerFunc(s.handleDashboard)))

	mux.Handle("GET /api/channels", s.withAdmin(http.HandlerFunc(s.handleListChannels)))
	mux.Handle("POST /api/channels", s.withAdmin(http.HandlerFunc(s.handleCreateChannel)))
	mux.Handle("POST /api/channels/upstream-models", s.withAdmin(http.HandlerFunc(s.handleFetchUpstreamModels)))
	mux.Handle("GET /api/channels/{id}", s.withAdmin(http.HandlerFunc(s.handleGetChannel)))
	mux.Handle("PUT /api/channels/{id}", s.withAdmin(http.HandlerFunc(s.handleUpdateChannel)))
	mux.Handle("DELETE /api/channels/{id}", s.withAdmin(http.HandlerFunc(s.handleDeleteChannel)))
	mux.Handle("POST /api/channels/{id}/test", s.withAdmin(http.HandlerFunc(s.handleTestChannel)))

	mux.Handle("GET /api/tokens", s.withAdmin(http.HandlerFunc(s.handleListTokens)))
	mux.Handle("POST /api/tokens", s.withAdmin(http.HandlerFunc(s.handleCreateToken)))
	mux.Handle("PUT /api/tokens/{id}", s.withAdmin(http.HandlerFunc(s.handleUpdateToken)))
	mux.Handle("DELETE /api/tokens/{id}", s.withAdmin(http.HandlerFunc(s.handleDeleteToken)))

	mux.Handle("GET /api/logs", s.withAdmin(http.HandlerFunc(s.handleListLogs)))
	mux.Handle("GET /api/ledger", s.withAdmin(http.HandlerFunc(s.handleListReservations)))
	mux.Handle("GET /api/models", s.withAdmin(http.HandlerFunc(s.handleListModels)))

	mux.Handle("GET /api/videos", s.withAdmin(http.HandlerFunc(s.handleListVideos)))
	mux.Handle("POST /api/videos", s.withAdmin(http.HandlerFunc(s.handleCreateVideo)))
	mux.Handle("GET /api/videos/{id}", s.withAdmin(http.HandlerFunc(s.handleGetVideo)))
	mux.Handle("GET /api/videos/{id}/content", s.withAdmin(http.HandlerFunc(s.handleVideoContent)))

	mux.Handle("GET /api/speech/voices", s.withAdmin(http.HandlerFunc(s.handleListSpeechVoices)))
	mux.Handle("GET /api/speech", s.withAdmin(http.HandlerFunc(s.handleListSpeech)))
	mux.Handle("POST /api/speech", s.withAdmin(http.HandlerFunc(s.handleCreateSpeech)))
	mux.Handle("GET /api/speech/{id}", s.withAdmin(http.HandlerFunc(s.handleGetSpeech)))
	mux.Handle("GET /api/speech/{id}/content", s.withAdmin(http.HandlerFunc(s.handleSpeechContent)))

	mux.Handle("GET /api/playground/models", s.withAdmin(http.HandlerFunc(s.handlePlaygroundModels)))
	mux.Handle("POST /api/playground/chat", s.withAdmin(http.HandlerFunc(s.handlePlaygroundChat)))

	mux.Handle("GET /v1/models", s.withToken(s.gatewayHandler(s.gw.Models)))
	mux.Handle("POST /v1/chat/completions", s.withToken(s.gatewayHandler(s.gw.ChatCompletions)))
	mux.Handle("POST /v1/embeddings", s.withToken(s.gatewayHandler(s.gw.Embeddings)))

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeOpenAIError(w, http.StatusNotFound, "not found", "invalid_request_error", "not_found")
	})
	mux.Handle("/", s.staticHandler())
	s.mux = mux
}

func (s *Server) withCommon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		rec.Header().Set("X-Content-Type-Options", "nosniff")
		rec.Header().Set("Referrer-Policy", "no-referrer")
		rec.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			rec.Header().Set("Access-Control-Allow-Origin", "*")
			rec.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-api-key, anthropic-version")
			rec.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			rec.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After, X-RateLimit-Limit-Requests, X-RateLimit-Remaining-Requests, X-RateLimit-Reset-Requests, X-RateLimit-Limit-Tokens, X-RateLimit-Remaining-Tokens, X-RateLimit-Reset-Tokens, X-RateLimit-Limit-Concurrency, X-RateLimit-Remaining-Concurrency")
			if r.Method == http.MethodOptions {
				rec.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(rec, r)
		status := rec.status
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", time.Since(started).Milliseconds()}
		switch {
		case status >= 500:
			s.logger.Error("request", attrs...)
		case status >= 400:
			s.logger.Warn("request", attrs...)
		default:
			s.logger.Info("request", attrs...)
		}
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic handling request", "error", rec, "path", r.URL.Path)
				writeErr(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) gatewayHandler(fn func(http.ResponseWriter, *http.Request, *store.Token)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fn(w, r, tokenFrom(r))
	})
}

func (s *Server) withAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.SessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		claims, err := auth.Parse(s.cfg.SecretKey, cookie.Value)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "session expired, please sign in again")
			return
		}
		user, err := s.store.GetUserByID(claims.Sub)
		if err != nil || user.Status != "enabled" || user.Role != "admin" {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) withToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			writeOpenAIError(w, http.StatusUnauthorized, "missing API key, send it as an Authorization: Bearer header", "invalid_api_key", "missing_api_key")
			return
		}
		tok, err := s.store.GetTokenByHash(auth.HashAPIKey(raw))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid API key", "invalid_api_key", "invalid_api_key")
				return
			}
			writeOpenAIError(w, http.StatusInternalServerError, "internal server error", "server_error", "")
			return
		}
		if tok.Status != "active" {
			writeOpenAIError(w, http.StatusUnauthorized, "this API key is disabled", "invalid_api_key", "key_disabled")
			return
		}
		if tok.ExpiresAt != nil && *tok.ExpiresAt <= time.Now().Unix() {
			writeOpenAIError(w, http.StatusUnauthorized, "this API key has expired", "invalid_api_key", "key_expired")
			return
		}
		ctx := context.WithValue(r.Context(), tokenKey, tok)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func tokenFrom(r *http.Request) *store.Token {
	t, _ := r.Context().Value(tokenKey).(*store.Token)
	return t
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeOpenAIError(w http.ResponseWriter, status int, message, typ, code string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
			"param":   nil,
			"code":    code,
		},
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func queryInt(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type attemptLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int
	window   time.Duration
}

func newAttemptLimiter(limit int, window time.Duration) *attemptLimiter {
	return &attemptLimiter{attempts: map[string][]time.Time{}, limit: limit, window: window}
}

func (l *attemptLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	kept := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.attempts[key] = kept
		return false
	}
	l.attempts[key] = append(kept, time.Now())
	return true
}

func (l *attemptLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":           "Model Relay",
		"version":        s.cfg.Version,
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
	})
}
