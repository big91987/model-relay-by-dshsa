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

// maxMusicBody bounds a create request; labels and lyrics are far smaller than
// the console's chat bodies, so one mebibyte is generous.
const maxMusicBody = 1 << 20

type musicInput struct {
	Model        string `json:"model"`
	Prompt       string `json:"prompt"`
	Lyrics       string `json:"lyrics"`
	Instrumental bool   `json:"instrumental"`
	Format       string `json:"format"`
}

// musicTaskView adds console-facing fields to a stored music task.
type musicTaskView struct {
	store.MusicTask
	AudioReady bool   `json:"audio_ready"`
	ContentURL string `json:"content_url,omitempty"`
}

func (s *Server) musicTaskView(task *store.MusicTask) musicTaskView {
	view := musicTaskView{MusicTask: *task}
	if task.AudioPath != "" {
		path := filepath.Join(s.cfg.DataDir, task.AudioPath)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			view.AudioReady = true
			view.ContentURL = fmt.Sprintf("/api/music/%d/content", task.ID)
			return view
		}
	}
	// A time-limited provider URL keeps the result playable while the local
	// download is missing or failed.
	if task.RemoteURL != "" {
		view.AudioReady = true
		view.ContentURL = fmt.Sprintf("/api/music/%d/content", task.ID)
	}
	return view
}

func (s *Server) handleCreateMusic(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMusicBody)
	var in musicInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	task, err := s.gw.CreateMusic(gateway.MusicRequest{
		Model:        in.Model,
		Prompt:       in.Prompt,
		Lyrics:       in.Lyrics,
		Instrumental: in.Instrumental,
		Format:       in.Format,
	})
	if err != nil {
		var musicErr *gateway.MusicError
		if errors.As(err, &musicErr) {
			writeErr(w, musicErr.Status, musicErr.Message)
			return
		}
		s.logger.Error("create music task failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to create music task")
		return
	}
	writeJSON(w, http.StatusCreated, s.musicTaskView(task))
}

func (s *Server) handleListMusic(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1)
	pageSize := queryInt(r, "page_size", 20)
	tasks, total, err := s.store.ListMusicTasks(page, pageSize)
	if err != nil {
		s.logger.Error("list music tasks failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load music")
		return
	}
	items := make([]musicTaskView, 0, len(tasks))
	for i := range tasks {
		items = append(items, s.musicTaskView(&tasks[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

func (s *Server) handleGetMusic(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid music id")
		return
	}
	task, err := s.store.GetMusicTask(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "music not found")
		return
	}
	if err != nil {
		s.logger.Error("load music task failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load music")
		return
	}
	writeJSON(w, http.StatusOK, s.musicTaskView(task))
}

func (s *Server) handleMusicContent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid music id")
		return
	}
	task, err := s.store.GetMusicTask(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "music not found")
		return
	}
	if err != nil {
		s.logger.Error("load music task failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load music")
		return
	}
	format := strings.ToLower(task.Format)
	if format != "wav" {
		format = "mp3"
	}
	if task.AudioPath != "" {
		path := filepath.Join(s.cfg.DataDir, task.AudioPath)
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			w.Header().Set("Content-Type", gateway.MusicContentType(format))
			w.Header().Set("Cache-Control", "private, max-age=3600")
			if r.URL.Query().Get("download") != "" {
				w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("music-%d.%s", task.ID, format)))
			}
			http.ServeFile(w, r, path)
			return
		}
	}
	if task.RemoteURL != "" {
		http.Redirect(w, r, task.RemoteURL, http.StatusFound)
		return
	}
	writeErr(w, http.StatusNotFound, "the song is not ready yet")
}
