package gateway

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

const (
	maxMusicPrompt   = 2000
	maxMusicLyrics   = 3500
	maxMusicResponse = 64 << 20
	maxMusicDownload = 64 << 20
	musicSampleRate  = 44100
	musicBitrate     = 256000
)

// musicContentTypes maps the formats the console can preview and download.
var musicContentTypes = map[string]string{
	"mp3": "audio/mpeg",
	"wav": "audio/wav",
}

// MusicContentType returns the response Content-Type for a stored song.
func MusicContentType(format string) string {
	if contentType, ok := musicContentTypes[strings.ToLower(format)]; ok {
		return contentType
	}
	return "audio/mpeg"
}

// MusicError is a client-visible failure while creating a music task.
type MusicError struct {
	Status  int
	Message string
}

func (e *MusicError) Error() string { return e.Message }

// MusicRequest describes one text-to-music generation. Prompt carries the
// style/mood/scenario description and Lyrics the song text.
type MusicRequest struct {
	Model        string
	Prompt       string
	Lyrics       string
	Instrumental bool
	Format       string
}

// musicGenerationResponse is the synchronous MiniMax Music response. The audio
// is hex-encoded by default, but a URL is accepted too because a compatible
// provider may prefer output_format=url.
type musicGenerationResponse struct {
	Data struct {
		Audio  string `json:"audio"`
		Status int    `json:"status"`
	} `json:"data"`
	ExtraInfo struct {
		MusicDuration int64 `json:"music_duration"`
	} `json:"extra_info"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// CreateMusic validates a request, creates a task and starts the synchronous
// upstream call in the background so the console can poll it like a video task.
func (g *Gateway) CreateMusic(req MusicRequest) (*store.MusicTask, error) {
	if err := normalizeMusicRequest(&req); err != nil {
		return nil, &MusicError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	channels, err := g.store.ListChannels()
	if err != nil {
		return nil, err
	}
	var selected *candidate
	for _, cand := range orderCandidates(channels, req.Model) {
		if isMusicChannel(cand.channel.Type) && isMusicUpstreamModel(cand.upstream) {
			cand := cand
			selected = &cand
			break
		}
	}
	if selected == nil {
		return nil, &MusicError{
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("model %q is not available on any enabled MiniMax Music channel", req.Model),
		}
	}
	ch := selected.channel
	if _, err := g.secrets.Decrypt(ch.APIKeyEncrypted); err != nil {
		return nil, &MusicError{
			Status:  http.StatusInternalServerError,
			Message: fmt.Sprintf("channel %q: cannot decrypt the API key", ch.Name),
		}
	}
	task := &store.MusicTask{
		ChannelID:     ch.ID,
		ChannelName:   ch.Name,
		Model:         req.Model,
		UpstreamModel: selected.upstream,
		Prompt:        req.Prompt,
		Lyrics:        req.Lyrics,
		Instrumental:  req.Instrumental,
		Format:        req.Format,
		Status:        "queued",
	}
	if err := g.store.CreateMusicTask(task); err != nil {
		return nil, err
	}
	go g.generateMusicTask(task.ID, ch)
	return task, nil
}

// normalizeMusicRequest validates the provider-independent parts of a request.
func normalizeMusicRequest(req *MusicRequest) error {
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Lyrics = strings.TrimSpace(req.Lyrics)
	req.Format = strings.ToLower(strings.TrimSpace(req.Format))
	if req.Model == "" {
		return errors.New("model is required")
	}
	if req.Prompt == "" {
		return errors.New("prompt is required")
	}
	if utf8.RuneCountInString(req.Prompt) > maxMusicPrompt {
		return fmt.Errorf("prompt must be at most %d characters", maxMusicPrompt)
	}
	if req.Instrumental {
		req.Lyrics = ""
	} else {
		if req.Lyrics == "" {
			return errors.New("lyrics are required unless the song is instrumental")
		}
		if utf8.RuneCountInString(req.Lyrics) > maxMusicLyrics {
			return fmt.Errorf("lyrics must be at most %d characters", maxMusicLyrics)
		}
	}
	if req.Format == "" {
		req.Format = "mp3"
	}
	if _, ok := musicContentTypes[req.Format]; !ok {
		return errors.New("format must be mp3 or wav")
	}
	return nil
}

// isMusicUpstreamModel keeps the music workflow on text-to-music models and
// excludes cover models, which need reference audio instead of lyrics.
func isMusicUpstreamModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(name, "music-") && !strings.Contains(name, "cover")
}

// musicGenerationPath accepts both the API root and a base URL that already
// ends in /v1, because users commonly paste either form.
func musicGenerationPath(baseURL string) string {
	if strings.HasSuffix(strings.TrimRight(baseURL, "/"), "/v1") {
		return "/music_generation"
	}
	return "/v1/music_generation"
}

func (g *Gateway) musicTimeout() time.Duration {
	if g.cfg.MusicTimeout > 0 {
		return g.cfg.MusicTimeout
	}
	return 10 * time.Minute
}

// generateMusicTask performs the synchronous upstream generation and stores the
// resulting audio. It runs outside the console request so a slow song cannot
// time out the browser.
func (g *Gateway) generateMusicTask(taskID int64, ch store.Channel) {
	task, err := g.store.GetMusicTask(taskID)
	if err != nil {
		g.logger.Error("load music task failed", "task", taskID, "error", err)
		return
	}
	task.Status = "generating"
	g.saveMusicTask(task)

	apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
	if err != nil {
		g.failMusicTask(task, "cannot decrypt the channel API key: "+err.Error())
		return
	}
	payload := map[string]any{
		"model":           task.UpstreamModel,
		"prompt":          task.Prompt,
		"is_instrumental": task.Instrumental,
		"output_format":   "hex",
		"audio_setting": map[string]any{
			"sample_rate": musicSampleRate,
			"bitrate":     musicBitrate,
			"format":      task.Format,
		},
	}
	if !task.Instrumental {
		payload["lyrics"] = task.Lyrics
	}
	body, err := json.Marshal(payload)
	if err != nil {
		g.failMusicTask(task, "encode the upstream request: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.musicTimeout())
	defer cancel()
	req, err := g.upstreamRequest(ctx, &ch, http.MethodPost, musicGenerationPath(ch.BaseURL), body, apiKey, false, true)
	if err != nil {
		g.failMusicTask(task, err.Error())
		return
	}
	resp, err := g.mediaClient.Do(req)
	if err != nil {
		g.failMusicTask(task, "could not reach the music provider: "+err.Error())
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxMusicResponse+1))
	resp.Body.Close()
	if readErr != nil {
		g.failMusicTask(task, "read the music provider response: "+readErr.Error())
		return
	}
	if len(data) > maxMusicResponse {
		g.failMusicTask(task, fmt.Sprintf("the music provider response exceeds the %d MiB limit", maxMusicResponse>>20))
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		g.failMusicTask(task, musicErrorMessage(data, resp.StatusCode))
		return
	}
	generated, err := parseMusicResponse(data)
	if err != nil {
		g.failMusicTask(task, err.Error())
		return
	}
	task.DurationMS = generated.durationMS
	task.Status = "succeeded"

	if generated.url != "" {
		task.RemoteURL = generated.url
		g.saveMusicTask(task)
		rel, err := g.downloadMusic(ctx, generated.url, task.ID, task.Format)
		if err != nil {
			task.Error = "the song was generated, but downloading it failed: " + err.Error()
			g.saveMusicTask(task)
			g.logger.Error("download generated music failed", "task", task.ID, "error", err)
			return
		}
		task.AudioPath = rel
		task.RemoteURL = ""
		task.Error = ""
		g.saveMusicTask(task)
		return
	}
	if len(generated.audio) > maxMusicDownload {
		g.failMusicTask(task, fmt.Sprintf("the generated song exceeds the %d MiB limit", maxMusicDownload>>20))
		return
	}
	rel, err := g.saveMusicAudio(task.ID, task.Format, generated.audio)
	if err != nil {
		g.failMusicTask(task, "store the generated song: "+err.Error())
		return
	}
	task.AudioPath = rel
	task.RemoteURL = ""
	task.Error = ""
	g.saveMusicTask(task)
}

type parsedMusic struct {
	audio      []byte
	url        string
	durationMS int64
}

// parseMusicResponse validates the provider status and decodes the hex audio (or
// keeps the URL for a later download).
func parseMusicResponse(data []byte) (*parsedMusic, error) {
	var parsed musicGenerationResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, errors.New("the music provider returned an unexpected response")
	}
	if parsed.BaseResp.StatusCode != 0 {
		message := strings.TrimSpace(parsed.BaseResp.StatusMsg)
		if message == "" {
			message = fmt.Sprintf("the music provider returned status %d", parsed.BaseResp.StatusCode)
		}
		return nil, errors.New("the music provider rejected the request: " + message)
	}
	audio := strings.TrimSpace(parsed.Data.Audio)
	if audio == "" {
		return nil, errors.New("the music provider returned no audio")
	}
	out := &parsedMusic{durationMS: parsed.ExtraInfo.MusicDuration}
	if strings.HasPrefix(audio, "http://") || strings.HasPrefix(audio, "https://") {
		out.url = audio
		return out, nil
	}
	decoded, err := decodeMusicAudio(audio)
	if err != nil {
		return nil, errors.New("the music provider returned audio that could not be decoded")
	}
	out.audio = decoded
	return out, nil
}

// decodeMusicAudio accepts the documented hex payload, plus base64 for
// compatible providers that encode the same bytes differently.
func decodeMusicAudio(audio string) ([]byte, error) {
	if len(audio)%2 == 0 && isHexAudio(audio) {
		return hex.DecodeString(audio)
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(audio); err == nil && len(decoded) > 0 {
			return decoded, nil
		}
	}
	return nil, errors.New("unsupported audio encoding")
}

func isHexAudio(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

func (g *Gateway) saveMusicAudio(taskID int64, format string, data []byte) (string, error) {
	dir := filepath.Join(g.cfg.DataDir, "music")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	rel := filepath.Join("music", fmt.Sprintf("%d.%s", taskID, format))
	dest := filepath.Join(g.cfg.DataDir, rel)
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return rel, nil
}

func (g *Gateway) downloadMusic(ctx context.Context, remoteURL string, taskID int64, format string) (string, error) {
	parsed, err := url.Parse(remoteURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("the music provider returned an invalid audio URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return "", err
	}
	result, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer result.Body.Close()
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return "", fmt.Errorf("music download returned status %d", result.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, maxMusicDownload+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxMusicDownload {
		return "", fmt.Errorf("the song exceeds the %d MiB download limit", maxMusicDownload>>20)
	}
	return g.saveMusicAudio(taskID, format, data)
}

// ResumeMusicTasks marks tasks left in flight by a previous process as failed:
// a synchronous generation cannot be polled or resumed safely, and retrying it
// could bill the channel twice.
func (g *Gateway) ResumeMusicTasks() {
	tasks, err := g.store.ListInterruptedMusicTasks()
	if err != nil {
		g.logger.Error("list interrupted music tasks failed", "error", err)
		return
	}
	for i := range tasks {
		task := tasks[i]
		task.Status = "failed"
		task.Error = "the gateway restarted before the song finished generating; the provider may still have completed it"
		g.saveMusicTask(&task)
	}
	if len(tasks) > 0 {
		g.logger.Warn("marked interrupted music tasks as failed", "count", len(tasks))
	}
}

func (g *Gateway) saveMusicTask(task *store.MusicTask) {
	if err := g.store.UpdateMusicTask(task); err != nil {
		g.logger.Error("update music task failed", "task", task.ID, "error", err)
	}
}

func (g *Gateway) failMusicTask(task *store.MusicTask, message string) {
	task.Status = "failed"
	task.Error = truncate(message, 1000)
	g.saveMusicTask(task)
	g.logger.Warn("music generation failed", "task", task.ID, "error", task.Error)
}

// musicErrorMessage extracts the human-readable message from a MiniMax Music
// error response.
func musicErrorMessage(data []byte, status int) string {
	var parsed struct {
		BaseResp struct {
			StatusMsg string `json:"status_msg"`
		} `json:"base_resp"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &parsed) == nil {
		if message := strings.TrimSpace(parsed.BaseResp.StatusMsg); message != "" {
			return truncate(message, 500)
		}
		if message := strings.TrimSpace(parsed.Error.Message); message != "" {
			return truncate(message, 500)
		}
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = http.StatusText(status)
	}
	return truncate(message, 500)
}
