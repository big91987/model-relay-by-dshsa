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

// maxVideoBody bounds a create request: the upstream limit is 30 MiB per input
// image, which grows by about a third when the console sends it as a data URL.
const maxVideoBody = 48 << 20

type videoInput struct {
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	FirstFrame string `json:"first_frame"`
	Resolution string `json:"resolution"`
	Duration   int    `json:"duration"`
	Ratio      string `json:"ratio"`
}

// videoTaskView adds console-facing fields to a stored video task.
type videoTaskView struct {
	store.VideoTask
	VideoReady bool   `json:"video_ready"`
	ContentURL string `json:"content_url,omitempty"`
}

func (s *Server) videoTaskView(task *store.VideoTask) videoTaskView {
	view := videoTaskView{VideoTask: *task}
	if task.VideoPath == "" {
		return view
	}
	path := filepath.Join(s.cfg.DataDir, task.VideoPath)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		view.VideoReady = true
		view.ContentURL = fmt.Sprintf("/api/videos/%d/content", task.ID)
	}
	return view
}

func (s *Server) handleCreateVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxVideoBody)
	var in videoInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	task, err := s.gw.CreateVideo(r.Context(), gateway.VideoRequest{
		Model:      in.Model,
		Prompt:     in.Prompt,
		FirstFrame: in.FirstFrame,
		Resolution: in.Resolution,
		Duration:   in.Duration,
		Ratio:      in.Ratio,
	})
	if err != nil {
		var videoErr *gateway.VideoError
		if errors.As(err, &videoErr) {
			writeErr(w, videoErr.Status, videoErr.Message)
			return
		}
		s.logger.Error("create video task failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to create video task")
		return
	}
	writeJSON(w, http.StatusCreated, s.videoTaskView(task))
}

func (s *Server) handleListVideos(w http.ResponseWriter, r *http.Request) {
	page := queryInt(r, "page", 1)
	pageSize := queryInt(r, "page_size", 20)
	tasks, total, err := s.store.ListVideoTasks(page, pageSize)
	if err != nil {
		s.logger.Error("list video tasks failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load videos")
		return
	}
	items := make([]videoTaskView, 0, len(tasks))
	for i := range tasks {
		items = append(items, s.videoTaskView(&tasks[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

func (s *Server) handleGetVideo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid video id")
		return
	}
	task, err := s.store.GetVideoTask(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "video not found")
		return
	}
	if err != nil {
		s.logger.Error("load video task failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load video")
		return
	}
	writeJSON(w, http.StatusOK, s.videoTaskView(task))
}

func (s *Server) handleVideoContent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid video id")
		return
	}
	task, err := s.store.GetVideoTask(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "video not found")
		return
	}
	if err != nil {
		s.logger.Error("load video task failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "failed to load video")
		return
	}
	if task.VideoPath != "" {
		path := filepath.Join(s.cfg.DataDir, task.VideoPath)
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Cache-Control", "private, max-age=3600")
			if r.URL.Query().Get("download") != "" {
				w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("%s-%d.mp4", videoFileStem(task.Model), task.ID)))
			}
			http.ServeFile(w, r, path)
			return
		}
	}
	// A time-limited upstream URL keeps the result playable if the local
	// download could not be stored.
	if task.RemoteURL != "" {
		http.Redirect(w, r, task.RemoteURL, http.StatusFound)
		return
	}
	writeErr(w, http.StatusNotFound, "the video is not ready yet")
}

// videoFileStem derives a friendly download filename from the public model.
func videoFileStem(model string) string {
	name := strings.ToLower(model)
	switch {
	case strings.HasPrefix(name, "doubao-seedance"):
		return "seedance"
	case strings.HasPrefix(name, "minimax"):
		return "minimax-h3"
	default:
		return "video"
	}
}
