package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

const (
	maxVideoPrompt   = 7000
	maxVideoResponse = 1 << 20
	maxVideoDownload = 512 << 20
	videoPollTimeout = 45 * time.Minute
	maxPollFailures  = 5
)

// firstFrameDataURL matches the image data URIs MiniMax-H3 accepts as a first frame.
var firstFrameDataURL = regexp.MustCompile(`^data:image/(png|jpeg|jpg|webp|heic|heif);base64,[A-Za-z0-9+/=]+$`)

// VideoError is a client-visible failure while creating a video task.
type VideoError struct {
	Status  int
	Message string
}

func (e *VideoError) Error() string { return e.Message }

// VideoRequest describes one MiniMax-H3 text-to-video or image-to-video task.
type VideoRequest struct {
	Model      string
	Prompt     string
	FirstFrame string // optional image data URL; empty means text-to-video
	Resolution string
	Duration   int
	Ratio      string
}

// remoteVideoTask is the subset of the MiniMax task payload the relay needs.
type remoteVideoTask struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
	Duration   int    `json:"duration"`
	Ratio      string `json:"ratio"`
	Error      *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Content struct {
		URL string `json:"url"`
	} `json:"content"`
}

// CreateVideo validates a request, submits it to an enabled MiniMax channel and
// starts polling the asynchronous task. The stored task is returned immediately.
func (g *Gateway) CreateVideo(ctx context.Context, req VideoRequest) (*store.VideoTask, error) {
	if err := normalizeVideoRequest(&req); err != nil {
		return nil, &VideoError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	channels, err := g.store.ListChannels()
	if err != nil {
		return nil, err
	}
	candidates := make([]candidate, 0)
	for _, cand := range orderCandidates(channels, req.Model) {
		if cand.channel.Type == "minimax" {
			candidates = append(candidates, cand)
		}
	}
	if len(candidates) == 0 {
		return nil, &VideoError{
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("model %q is not available on any enabled MiniMax channel", req.Model),
		}
	}

	content := []map[string]any{{"type": "text", "text": req.Prompt}}
	mode := "text"
	if req.FirstFrame != "" {
		mode = "first_frame"
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": req.FirstFrame},
			"role":      "first_frame",
		})
	}

	var lastErr string
	for _, cand := range candidates {
		ch := cand.channel
		apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
		if err != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, err)
			continue
		}
		body, err := json.Marshal(map[string]any{
			"model":      cand.upstream,
			"content":    content,
			"resolution": req.Resolution,
			"duration":   req.Duration,
			"ratio":      req.Ratio,
		})
		if err != nil {
			return nil, err
		}
		upreq, err := g.upstreamRequest(ctx, &ch, http.MethodPost, "/v2/video_generation", body, apiKey, false, true)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		resp, err := g.client.Do(upreq)
		if err != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxVideoResponse))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, readErr)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			message := minimaxErrorMessage(data, resp.StatusCode)
			if shouldFailover(resp.StatusCode) {
				lastErr = fmt.Sprintf("channel %q returned %d: %s", ch.Name, resp.StatusCode, message)
				continue
			}
			return nil, &VideoError{Status: resp.StatusCode, Message: message}
		}
		var created struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(data, &created); err != nil || strings.TrimSpace(created.TaskID) == "" {
			lastErr = fmt.Sprintf("channel %q returned no task_id", ch.Name)
			continue
		}

		task := &store.VideoTask{
			UpstreamTaskID: created.TaskID,
			ChannelID:      ch.ID,
			ChannelName:    ch.Name,
			Model:          req.Model,
			UpstreamModel:  cand.upstream,
			Prompt:         req.Prompt,
			Mode:           mode,
			Resolution:     req.Resolution,
			Duration:       req.Duration,
			Ratio:          req.Ratio,
			Status:         "queued",
		}
		if err := g.store.CreateVideoTask(task); err != nil {
			return nil, err
		}
		go g.watchVideoTask(task.ID, ch)
		return task, nil
	}

	message := "no enabled MiniMax channel accepted the video task"
	if lastErr != "" {
		message += ": " + lastErr
	}
	return nil, &VideoError{Status: http.StatusBadGateway, Message: message}
}

func normalizeVideoRequest(req *VideoRequest) error {
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.FirstFrame = strings.TrimSpace(req.FirstFrame)
	req.Resolution = strings.ToUpper(strings.TrimSpace(req.Resolution))
	req.Ratio = strings.TrimSpace(req.Ratio)
	if req.Model == "" {
		return errors.New("model is required")
	}
	if req.Prompt == "" {
		return errors.New("prompt is required")
	}
	if utf8.RuneCountInString(req.Prompt) > maxVideoPrompt {
		return fmt.Errorf("prompt must be at most %d characters", maxVideoPrompt)
	}
	switch req.Resolution {
	case "480P", "768P", "2K":
	default:
		return errors.New("resolution must be 480P, 768P or 2K")
	}
	if req.Duration < 4 || req.Duration > 15 {
		return errors.New("duration must be between 4 and 15 seconds")
	}
	if req.FirstFrame != "" {
		if !firstFrameDataURL.MatchString(req.FirstFrame) {
			return errors.New("first_frame must be a base64 image data URL (PNG, JPEG, WEBP, HEIC or HEIF)")
		}
		// Image-to-video derives its aspect ratio from the first frame.
		req.Ratio = "adaptive"
		return nil
	}
	switch req.Ratio {
	case "21:9", "16:9", "4:3", "1:1", "3:4", "9:16":
		return nil
	case "":
		return errors.New("ratio is required for text-to-video")
	case "adaptive":
		return errors.New("ratio cannot be adaptive for text-to-video")
	default:
		return errors.New("ratio must be one of 21:9, 16:9, 4:3, 1:1, 3:4, 9:16")
	}
}

// watchVideoTask polls the upstream task until it reaches a terminal state,
// stores status changes and downloads the generated video on success.
func (g *Gateway) watchVideoTask(taskID int64, ch store.Channel) {
	ctx, cancel := context.WithTimeout(context.Background(), videoPollTimeout)
	defer cancel()
	failures := 0
	for {
		task, err := g.store.GetVideoTask(taskID)
		if err != nil {
			g.logger.Error("load video task failed", "task", taskID, "error", err)
			return
		}
		switch task.Status {
		case "succeeded":
			if task.VideoPath != "" {
				return
			}
			if task.RemoteURL != "" {
				g.finishVideoDownload(ctx, task, task.RemoteURL)
				return
			}
		case "failed", "cancelled":
			return
		}

		remote, err := g.queryVideoTask(ctx, &ch, task.UpstreamTaskID)
		if err != nil {
			failures++
			if failures >= maxPollFailures {
				g.markVideoFailed(task, "could not check the video task status: "+err.Error())
				return
			}
		} else {
			failures = 0
			switch remote.Status {
			case "queued", "running":
				if task.Status != remote.Status {
					task.Status = remote.Status
					g.saveVideoTask(task)
				}
			case "succeeded":
				if strings.TrimSpace(remote.Content.URL) == "" {
					g.markVideoFailed(task, "the upstream task succeeded without a video URL")
					return
				}
				task.Status = "succeeded"
				task.RemoteURL = remote.Content.URL
				if remote.Resolution != "" {
					task.Resolution = remote.Resolution
				}
				if remote.Duration > 0 {
					task.Duration = remote.Duration
				}
				if remote.Ratio != "" {
					task.Ratio = remote.Ratio
				}
				g.saveVideoTask(task)
				g.finishVideoDownload(ctx, task, remote.Content.URL)
				return
			case "failed", "cancelled":
				task.Status = remote.Status
				task.Error = remoteErrorMessage(remote)
				g.saveVideoTask(task)
				return
			}
		}
		select {
		case <-ctx.Done():
			g.markVideoFailed(task, "timed out waiting for the video generation task")
			return
		case <-time.After(g.videoPollInterval):
		}
	}
}

func (g *Gateway) finishVideoDownload(ctx context.Context, task *store.VideoTask, remoteURL string) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		path, err := g.downloadVideo(ctx, remoteURL, task.ID)
		if err == nil {
			task.VideoPath = path
			task.Error = ""
			g.saveVideoTask(task)
			return
		}
		lastErr = err
		if attempt == 3 {
			break
		}
		select {
		case <-ctx.Done():
			attempt = 3
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	task.Status = "succeeded"
	task.Error = "the video was generated, but downloading it failed: " + lastErr.Error()
	g.saveVideoTask(task)
	g.logger.Error("download generated video failed", "task", task.ID, "error", lastErr)
}

func (g *Gateway) downloadVideo(ctx context.Context, remoteURL string, taskID int64) (string, error) {
	parsed, err := url.Parse(remoteURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("upstream returned an invalid video URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("video download returned status %d", resp.StatusCode)
	}
	dir := filepath.Join(g.cfg.DataDir, "videos")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	rel := filepath.Join("videos", fmt.Sprintf("%d.mp4", taskID))
	dest := filepath.Join(g.cfg.DataDir, rel)
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	written, copyErr := io.Copy(out, io.LimitReader(resp.Body, maxVideoDownload+1))
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", closeErr
	}
	if written > maxVideoDownload {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("video exceeds the %d MiB download limit", maxVideoDownload>>20)
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return rel, nil
}

func (g *Gateway) queryVideoTask(ctx context.Context, ch *store.Channel, upstreamTaskID string) (*remoteVideoTask, error) {
	apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
	if err != nil {
		return nil, err
	}
	path := "/v2/query/video_generation/" + url.PathEscape(upstreamTaskID)
	req, err := g.upstreamRequest(ctx, ch, http.MethodGet, path, nil, apiKey, false, true)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVideoResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New(minimaxErrorMessage(data, resp.StatusCode))
	}
	var wrapper struct {
		Task remoteVideoTask `json:"task"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("decode upstream task: %w", err)
	}
	return &wrapper.Task, nil
}

// ResumeVideoTasks restarts polling for tasks that were in flight when the
// gateway stopped, and retries downloads for videos that are not stored yet.
func (g *Gateway) ResumeVideoTasks() {
	tasks, err := g.store.ListResumableVideoTasks()
	if err != nil {
		g.logger.Error("list resumable video tasks failed", "error", err)
		return
	}
	for i := range tasks {
		task := tasks[i]
		ch, err := g.store.GetChannel(task.ChannelID)
		if err != nil {
			g.markVideoFailed(&task, "the channel that created this video no longer exists")
			continue
		}
		go g.watchVideoTask(task.ID, *ch)
	}
	if len(tasks) > 0 {
		g.logger.Info("resumed video tasks", "count", len(tasks))
	}
}

func (g *Gateway) saveVideoTask(task *store.VideoTask) {
	if err := g.store.UpdateVideoTask(task); err != nil {
		g.logger.Error("update video task failed", "task", task.ID, "error", err)
	}
}

func (g *Gateway) markVideoFailed(task *store.VideoTask, message string) {
	task.Status = "failed"
	task.Error = truncate(message, 1000)
	g.saveVideoTask(task)
}

func remoteErrorMessage(task *remoteVideoTask) string {
	if task.Error != nil && strings.TrimSpace(task.Error.Message) != "" {
		return task.Error.Message
	}
	if task.Status == "cancelled" {
		return "video generation was cancelled"
	}
	return "video generation failed"
}

func minimaxErrorMessage(data []byte, status int) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &parsed) == nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return truncate(parsed.Error.Message, 500)
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = http.StatusText(status)
	}
	return truncate(message, 500)
}
