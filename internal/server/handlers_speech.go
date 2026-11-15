package server

import (
	"encoding/base64"
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
// 50,000 characters, so a couple of MiB leaves room for metadata.
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

// handleStreamSpeech synthesizes short text and relays PCM audio to the
// browser as server-sent events while MiniMax is still generating it. The
// event sequence is a "meta" event, zero or more "audio" events, and a final
// "done" event carrying the stored clip. A failure after the response has
// started arrives as an "error" event; validation and routing failures keep
// their normal JSON status because streaming only begins with the first audio
// chunk. Long text keeps using the background task returned by POST
// /api/speech.
func (s *Server) handleStreamSpeech(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSpeechBody)
	var in speechInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	started := false
	clip, err := s.gw.StreamSpeech(r.Context(), gateway.SpeechRequest{
		Model:     in.Model,
		Text:      in.Text,
		VoiceID:   in.VoiceID,
		VoiceName: in.VoiceName,
		Speed:     in.Speed,
	}, gateway.SpeechStreamHandlers{
		OnStart: func(info gateway.SpeechStreamInfo) error {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			started = true
			return writeSpeechEvent(w, "meta", map[string]any{
				"format":      info.Format,
				"sample_rate": info.SampleRate,
				"channels":    info.Channels,
			})
		},
		OnAudio: func(pcm []byte) error {
			return writeSpeechEvent(w, "audio", map[string]any{
				"audio": base64.StdEncoding.EncodeToString(pcm),
			})
		},
	})
	if err != nil {
		if started {
			message := "speech generation failed"
			var speechErr *gateway.SpeechError
			if errors.As(err, &speechErr) {
				message = speechErr.Message
			} else {
				s.logger.Error("stream speech failed after playback started", "error", err)
			}
			_ = writeSpeechEvent(w, "error", map[string]any{"error": message})
			return
		}
		var speechErr *gateway.SpeechError
		if errors.As(err, &speechErr) {
			writeErr(w, speechErr.Status, speechErr.Message)
			return
		}
		s.logger.Error("stream speech failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to generate speech")
		return
	}
	_ = writeSpeechEvent(w, "done", map[string]any{"clip": s.speechClipView(clip)})
}

func writeSpeechEvent(w http.ResponseWriter, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
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
	case "m4a":
		return "audio/mp4"
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

type voiceDesignInput struct {
	Model       string `json:"model"`
	Prompt      string `json:"prompt"`
	PreviewText string `json:"preview_text"`
	VoiceID     string `json:"voice_id"`
}

// voiceDesignView adds console-facing fields to a stored designed voice.
type voiceDesignView struct {
	store.VoiceDesign
	PreviewReady bool   `json:"preview_ready"`
	PreviewURL   string `json:"preview_url,omitempty"`
}

func (s *Server) voiceDesignView(design *store.VoiceDesign) voiceDesignView {
	view := voiceDesignView{VoiceDesign: *design}
	if design.PreviewPath == "" {
		return view
	}
	path := filepath.Join(s.cfg.DataDir, design.PreviewPath)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		view.PreviewReady = true
		view.PreviewURL = fmt.Sprintf("/api/voice-designs/%d/preview", design.ID)
	}
	return view
}

func (s *Server) handleListVoiceDesigns(w http.ResponseWriter, r *http.Request) {
	designs, err := s.store.ListVoiceDesigns(queryInt(r, "limit", 50))
	if err != nil {
		s.logger.Error("list designed voices failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load designed voices")
		return
	}
	items := make([]voiceDesignView, 0, len(designs))
	for i := range designs {
		items = append(items, s.voiceDesignView(&designs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateVoiceDesign(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSpeechBody)
	var in voiceDesignInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	design, err := s.gw.CreateVoiceDesign(r.Context(), gateway.VoiceDesignRequest{
		Model:       in.Model,
		Prompt:      in.Prompt,
		PreviewText: in.PreviewText,
		VoiceID:     in.VoiceID,
	})
	if err != nil {
		var speechErr *gateway.SpeechError
		if errors.As(err, &speechErr) {
			writeErr(w, speechErr.Status, speechErr.Message)
			return
		}
		s.logger.Error("design voice failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to design the voice")
		return
	}
	writeJSON(w, http.StatusCreated, s.voiceDesignView(design))
}

func (s *Server) handleGetVoiceDesign(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid voice design id")
		return
	}
	design, err := s.store.GetVoiceDesign(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "designed voice not found")
		return
	}
	if err != nil {
		s.logger.Error("load designed voice failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the designed voice")
		return
	}
	writeJSON(w, http.StatusOK, s.voiceDesignView(design))
}

func (s *Server) handleVoiceDesignPreview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid voice design id")
		return
	}
	design, err := s.store.GetVoiceDesign(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "designed voice not found")
		return
	}
	if err != nil {
		s.logger.Error("load designed voice failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the designed voice")
		return
	}
	if design.PreviewPath == "" {
		writeErr(w, http.StatusNotFound, "the preview audio is not available")
		return
	}
	path := filepath.Join(s.cfg.DataDir, design.PreviewPath)
	if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "the stored preview audio file is missing")
		return
	}
	format := strings.ToLower(strings.TrimSpace(design.PreviewFormat))
	if format == "" {
		format = "mp3"
	}
	w.Header().Set("Content-Type", speechContentType(format))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.URL.Query().Get("download") != "" {
		filename := fmt.Sprintf("minimax-voice-design-%d.%s", design.ID, format)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}
	http.ServeFile(w, r, path)
}

type voiceDesignSpeechInput struct {
	Text  string  `json:"text"`
	Speed float64 `json:"speed"`
}

// handleVoiceDesignSpeech reads a new text with the designed voice. Synthesis is
// pinned to the channel that created the voice because MiniMax custom voices
// belong to the account that designed them.
func (s *Server) handleVoiceDesignSpeech(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid voice design id")
		return
	}
	design, err := s.store.GetVoiceDesign(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "designed voice not found")
		return
	}
	if err != nil {
		s.logger.Error("load designed voice failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the designed voice")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSpeechBody)
	var in voiceDesignSpeechInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	clip, err := s.gw.CreateSpeech(r.Context(), gateway.SpeechRequest{
		Model:     design.Model,
		Text:      in.Text,
		VoiceID:   design.VoiceID,
		VoiceName: design.Name,
		Speed:     in.Speed,
	})
	if err != nil {
		var speechErr *gateway.SpeechError
		if errors.As(err, &speechErr) {
			writeErr(w, speechErr.Status, speechErr.Message)
			return
		}
		s.logger.Error("synthesize designed voice failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to generate speech")
		return
	}
	// Long text is accepted as a background job; the page polls the clip.
	status := http.StatusCreated
	if clip.Status == "queued" || clip.Status == "running" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, s.speechClipView(clip))
}
