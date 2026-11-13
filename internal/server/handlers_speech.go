package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/big91987/model-relay-by-dshsa/internal/gateway"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

// maxSpeechBody bounds a synthesis request: the text itself is capped at
// 10,000 characters, so a couple of MiB leaves room for metadata.
const maxSpeechBody = 2 << 20

type speechInput struct {
	Model     string  `json:"model"`
	Text      string  `json:"text"`
	VoiceID   string  `json:"voice_id"`
	VoiceName string  `json:"voice_name"`
	Speed     float64 `json:"speed"`
}

// speechClipView adds console-facing fields to a stored speech clip.
type speechClipView struct {
	store.SpeechClip
	AudioReady bool   `json:"audio_ready"`
	ContentURL string `json:"content_url,omitempty"`
}

func (s *Server) speechClipView(clip *store.SpeechClip) speechClipView {
	view := speechClipView{SpeechClip: *clip}
	if clip.AudioPath == "" {
		return view
	}
	path := filepath.Join(s.cfg.DataDir, clip.AudioPath)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		view.AudioReady = true
		view.ContentURL = fmt.Sprintf("/api/speech/%d/content", clip.ID)
	}
	return view
}

func (s *Server) handleCreateSpeech(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSpeechBody)
	var in speechInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	clip, err := s.gw.CreateSpeech(r.Context(), gateway.SpeechRequest{
		Model:     in.Model,
		Text:      in.Text,
		VoiceID:   in.VoiceID,
		VoiceName: in.VoiceName,
		Speed:     in.Speed,
	})
	if err != nil {
		var speechErr *gateway.SpeechError
		if errors.As(err, &speechErr) {
			writeErr(w, speechErr.Status, speechErr.Message)
			return
		}
		s.logger.Error("create speech clip failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to generate speech")
		return
	}
	// Long text is accepted for background synthesis; the console polls the
	// clip until the merged audio is ready.
	status := http.StatusCreated
	if clip.Status == "queued" || clip.Status == "running" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, s.speechClipView(clip))
}

func (s *Server) handleListSpeech(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1)
	pageSize := queryInt(r, "page_size", 20)
	clips, total, err := s.store.ListSpeechClips(page, pageSize)
	if err != nil {
		s.logger.Error("list speech clips failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load speech history")
		return
	}
	items := make([]speechClipView, 0, len(clips))
	for i := range clips {
		items = append(items, s.speechClipView(&clips[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

func (s *Server) handleGetSpeech(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid speech id")
		return
	}
	clip, err := s.store.GetSpeechClip(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "speech clip not found")
		return
	}
	if err != nil {
		s.logger.Error("load speech clip failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load speech clip")
		return
	}
	writeJSON(w, http.StatusOK, s.speechClipView(clip))
}

func (s *Server) handleSpeechContent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid speech id")
		return
	}
	clip, err := s.store.GetSpeechClip(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "speech clip not found")
		return
	}
	if err != nil {
		s.logger.Error("load speech clip failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load speech clip")
		return
	}
	if clip.Status == "queued" || clip.Status == "running" {
		writeErr(w, http.StatusNotFound, "the audio is not ready yet")
		return
	}
	if clip.AudioPath == "" {
		writeErr(w, http.StatusNotFound, "the audio is not ready yet")
		return
	}
	path := filepath.Join(s.cfg.DataDir, clip.AudioPath)
	if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "the stored audio file is missing")
		return
	}
	format := strings.ToLower(strings.TrimSpace(clip.Format))
	if format == "" {
		format = "mp3"
	}
	w.Header().Set("Content-Type", speechContentType(format))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.URL.Query().Get("download") != "" {
		filename := fmt.Sprintf("minimax-speech-%d.%s", clip.ID, format)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}
	http.ServeFile(w, r, path)
}

func speechContentType(format string) string {
	switch format {
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	default:
		return "audio/mpeg"
	}
}

func (s *Server) handleListSpeechVoices(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.gw.ListSpeechVoices(r.Context(), r.URL.Query().Get("model"))
	if err != nil {
		s.logger.Error("list speech voices failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load voices")
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}
