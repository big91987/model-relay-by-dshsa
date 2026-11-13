package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/big91987/model-relay-by-dshsa/internal/gateway"
	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

// maxVoiceCloneBody leaves room for the multipart envelope around a 20 MiB
// sample recording.
const maxVoiceCloneBody = 21 << 20

// voiceCloneView adds console-facing preview fields to a stored clone.
type voiceCloneView struct {
	store.VoiceClone
	PreviewReady   bool   `json:"preview_ready"`
	PreviewURL     string `json:"preview_url,omitempty"`
	PreviewWarning string `json:"preview_warning,omitempty"`
}

func (s *Server) voiceCloneView(clone *store.VoiceClone) voiceCloneView {
	view := voiceCloneView{VoiceClone: *clone}
	if clone.PreviewPath == "" {
		return view
	}
	path := filepath.Join(s.cfg.DataDir, clone.PreviewPath)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		view.PreviewReady = true
		view.PreviewURL = fmt.Sprintf("/api/voice-clones/%d/preview", clone.ID)
	}
	return view
}

func formBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

func (s *Server) handleListVoiceClones(w http.ResponseWriter, r *http.Request) {
	clones, err := s.store.ListVoiceClones()
	if err != nil {
		s.logger.Error("list voice clones failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load cloned voices")
		return
	}
	items := make([]voiceCloneView, 0, len(clones))
	for i := range clones {
		items = append(items, s.voiceCloneView(&clones[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleCreateVoiceClone(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxVoiceCloneBody)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "the sample recording must be at most 20 MiB")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid upload: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "choose a sample recording to clone")
		return
	}
	defer file.Close()
	sample, err := io.ReadAll(io.LimitReader(file, gateway.MaxVoiceSampleBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read the sample recording")
		return
	}
	if len(sample) > gateway.MaxVoiceSampleBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "the sample recording must be at most 20 MiB")
		return
	}
	authorized := formBool(r.FormValue("authorized"))
	clone, warning, err := s.gw.CreateVoiceClone(r.Context(), gateway.VoiceCloneRequest{
		Name:        r.FormValue("name"),
		FileName:    header.Filename,
		Sample:      sample,
		Model:       r.FormValue("model"),
		PreviewText: r.FormValue("preview_text"),
		Authorized:  authorized,
	})
	if err != nil {
		var speechErr *gateway.SpeechError
		if errors.As(err, &speechErr) {
			writeErr(w, speechErr.Status, speechErr.Message)
			return
		}
		s.logger.Error("create voice clone failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to clone the voice")
		return
	}
	view := s.voiceCloneView(clone)
	view.PreviewWarning = warning
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) handleVoiceClonePreview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid cloned voice id")
		return
	}
	clone, err := s.store.GetVoiceClone(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "cloned voice not found")
		return
	}
	if err != nil {
		s.logger.Error("load voice clone failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the cloned voice")
		return
	}
	if clone.PreviewPath == "" {
		writeErr(w, http.StatusNotFound, "the preview is not ready")
		return
	}
	path := filepath.Join(s.cfg.DataDir, clone.PreviewPath)
	if info, statErr := os.Stat(path); statErr != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, "the stored preview is missing")
		return
	}
	format := strings.ToLower(strings.TrimSpace(clone.PreviewFormat))
	if format == "" {
		format = "mp3"
	}
	w.Header().Set("Content-Type", speechContentType(format))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.URL.Query().Get("download") != "" {
		filename := fmt.Sprintf("cloned-voice-%d-preview.%s", clone.ID, format)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}
	http.ServeFile(w, r, path)
}

func (s *Server) handleRegenerateVoiceClonePreview(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid cloned voice id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	clone, err := s.gw.PreviewVoiceClone(r.Context(), id, in.Text)
	if err != nil {
		var speechErr *gateway.SpeechError
		if errors.As(err, &speechErr) {
			writeErr(w, speechErr.Status, speechErr.Message)
			return
		}
		s.logger.Error("generate voice preview failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to generate a voice preview")
		return
	}
	writeJSON(w, http.StatusOK, s.voiceCloneView(clone))
}
