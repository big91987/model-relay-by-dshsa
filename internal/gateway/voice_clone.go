package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
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
	// MiniMax accepts sample recordings between 10 seconds and 5 minutes,
	// encoded as mp3, m4a or wav, with a 20 MiB limit. The gateway enforces the
	// format and size limits locally; the provider validates the duration.
	MaxVoiceSampleBytes  = 20 << 20
	MaxVoicePreviewText  = 1000
	MaxVoiceCloneName    = 80
	maxVoicePreviewAudio = 32 << 20
	// voiceCloneTimeout covers uploading the sample, cloning the voice and
	// downloading the generated preview.
	voiceCloneTimeout = 10 * time.Minute
)

// VoiceCloneRequest describes one custom voice created from a sample recording.
type VoiceCloneRequest struct {
	Name        string
	FileName    string
	Sample      []byte
	Model       string
	PreviewText string
	Authorized  bool
}

// voiceUpstreamError is an upstream failure that may be retried on the next
// MiniMax channel. It is converted to a client-visible SpeechError when no
// channel can serve the request.
type voiceUpstreamError struct {
	status  int
	message string
	retry   bool
}

func (e *voiceUpstreamError) Error() string { return e.message }

// CreateVoiceClone uploads an authorized sample recording to MiniMax, clones it
// into a custom voice and stores a local preview. When the clone succeeds but
// the preview cannot be produced, it returns the stored clone together with a
// warning instead of failing the whole operation.
func (g *Gateway) CreateVoiceClone(ctx context.Context, req VoiceCloneRequest) (*store.VoiceClone, string, error) {
	format, err := normalizeVoiceCloneRequest(&req)
	if err != nil {
		return nil, "", &SpeechError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	channels, err := g.store.ListChannels()
	if err != nil {
		return nil, "", err
	}
	candidates := make([]candidate, 0)
	for _, cand := range orderCandidates(channels, req.Model) {
		if cand.channel.Type == "minimax" {
			candidates = append(candidates, cand)
		}
	}
	if len(candidates) == 0 {
		return nil, "", &SpeechError{
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("model %q is not available on any enabled MiniMax channel", req.Model),
		}
	}

	ctx, cancel := context.WithTimeout(ctx, voiceCloneTimeout)
	defer cancel()

	var lastErr string
	for _, cand := range candidates {
		ch := cand.channel
		apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
		if err != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, err)
			continue
		}
		fileID, uploadErr := g.uploadVoiceSample(ctx, &ch, apiKey, req.FileName, req.Sample)
		if uploadErr != nil {
			if !uploadErr.retry {
				return nil, "", &SpeechError{Status: uploadErr.status, Message: uploadErr.message}
			}
			lastErr = fmt.Sprintf("channel %q: %s", ch.Name, uploadErr.message)
			continue
		}
		voiceID, err := g.newVoiceCloneID()
		if err != nil {
			return nil, "", err
		}
		preview, cloneErr := g.cloneVoice(ctx, &ch, apiKey, fileID, voiceID, req.PreviewText, cand.upstream)
		if cloneErr != nil {
			if !cloneErr.retry {
				return nil, "", &SpeechError{Status: cloneErr.status, Message: cloneErr.message}
			}
			lastErr = fmt.Sprintf("channel %q: %s", ch.Name, cloneErr.message)
			continue
		}

		clone := &store.VoiceClone{
			VoiceID:        voiceID,
			Name:           req.Name,
			ChannelID:      ch.ID,
			ChannelName:    ch.Name,
			Model:          req.Model,
			UpstreamModel:  cand.upstream,
			SampleFormat:   format,
			SampleBytes:    int64(len(req.Sample)),
			SourceFilename: req.FileName,
			PreviewText:    req.PreviewText,
			Authorized:     req.Authorized,
		}
		if err := g.store.CreateVoiceClone(clone); err != nil {
			return nil, "", err
		}

		warning := ""
		switch {
		case preview.URL == "":
			warning = "The voice was created, but MiniMax returned no preview audio. Generate a preview from the cloned voice card."
		default:
			audio, audioFormat, downloadErr := g.downloadVoicePreview(ctx, preview.URL)
			if downloadErr != nil {
				warning = "The voice was created, but its preview could not be downloaded: " + downloadErr.Error()
				break
			}
			characters := preview.Characters
			if characters <= 0 {
				characters = int64(utf8.RuneCountInString(req.PreviewText))
			}
			if saveErr := g.saveVoicePreview(clone, audio, audioFormat, preview.DurationMS, characters); saveErr != nil {
				warning = "The voice was created, but its preview could not be stored: " + saveErr.Error()
			}
		}
		return clone, warning, nil
	}

	message := "no enabled MiniMax channel accepted the voice clone request"
	if lastErr != "" {
		message += ": " + lastErr
	}
	return nil, "", &SpeechError{Status: http.StatusBadGateway, Message: message}
}

// PreviewVoiceClone regenerates the preview for a stored clone by synthesizing
// its preview text with the cloned voice on the channel that cloned it.
func (g *Gateway) PreviewVoiceClone(ctx context.Context, id int64, text string) (*store.VoiceClone, error) {
	clone, err := g.store.GetVoiceClone(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &SpeechError{Status: http.StatusNotFound, Message: "cloned voice not found"}
	}
	if err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		text = strings.TrimSpace(clone.PreviewText)
	}
	if text == "" {
		return nil, &SpeechError{Status: http.StatusBadRequest, Message: "preview text is required"}
	}
	if utf8.RuneCountInString(text) > MaxVoicePreviewText {
		return nil, &SpeechError{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("preview text must be at most %d characters", MaxVoicePreviewText),
		}
	}
	ch, err := g.store.GetChannel(clone.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &SpeechError{
			Status:  http.StatusNotFound,
			Message: "the channel that owns this cloned voice is no longer available",
		}
	}
	if err != nil {
		return nil, err
	}
	if ch.Type != "minimax" {
		return nil, &SpeechError{
			Status:  http.StatusConflict,
			Message: "the channel that owns this cloned voice is not a MiniMax channel",
		}
	}
	apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, speechRequestTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model":         clone.UpstreamModel,
		"text":          text,
		"stream":        false,
		"output_format": "hex",
		"voice_setting": map[string]any{
			"voice_id": clone.VoiceID,
			"speed":    1,
			"vol":      1,
			"pitch":    0,
		},
		"audio_setting": map[string]any{
			"format":      "mp3",
			"sample_rate": 32000,
			"bitrate":     128000,
			"channel":     1,
		},
	})
	if err != nil {
		return nil, err
	}
	upreq, err := g.upstreamRequest(ctx, ch, http.MethodPost, "/v1/t2a_v2", body, apiKey, false, true)
	if err != nil {
		return nil, &SpeechError{Status: http.StatusBadGateway, Message: err.Error()}
	}
	resp, err := g.speechClient.Do(upreq)
	if err != nil {
		return nil, &SpeechError{Status: http.StatusBadGateway, Message: err.Error()}
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSpeechResponse))
	resp.Body.Close()
	if readErr != nil {
		return nil, &SpeechError{Status: http.StatusBadGateway, Message: readErr.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := speechErrorMessage(data, resp.StatusCode)
		return nil, &SpeechError{Status: voiceCloneFailureStatus(int64(resp.StatusCode)), Message: message}
	}
	var payload speechPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &SpeechError{
			Status:  http.StatusBadGateway,
			Message: "the provider returned an unexpected speech payload",
		}
	}
	if payload.BaseResp.StatusCode != 0 {
		message := strings.TrimSpace(payload.BaseResp.StatusMsg)
		if message == "" {
			message = speechErrorMessage(data, resp.StatusCode)
		}
		return nil, &SpeechError{Status: voiceCloneFailureStatus(payload.BaseResp.StatusCode), Message: message}
	}
	audioHex := strings.TrimSpace(payload.Data.Audio)
	if audioHex == "" {
		return nil, &SpeechError{Status: http.StatusBadGateway, Message: "the provider returned no preview audio"}
	}
	audio, err := hex.DecodeString(audioHex)
	if err != nil || len(audio) == 0 {
		return nil, &SpeechError{Status: http.StatusBadGateway, Message: "the provider returned invalid preview audio"}
	}
	if len(audio) > maxVoicePreviewAudio {
		return nil, &SpeechError{Status: http.StatusBadGateway, Message: "the preview audio is too large"}
	}
	format := strings.ToLower(strings.TrimSpace(payload.ExtraInfo.AudioFormat))
	switch format {
	case "mp3", "flac", "wav":
	default:
		format = "mp3"
	}
	characters := payload.ExtraInfo.UsageCharacters
	if characters <= 0 {
		characters = int64(utf8.RuneCountInString(text))
	}
	clone.PreviewText = text
	if err := g.saveVoicePreview(clone, audio, format, payload.ExtraInfo.AudioLength, characters); err != nil {
		return nil, err
	}
	return clone, nil
}

func normalizeVoiceCloneRequest(req *VoiceCloneRequest) (string, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.Model = strings.TrimSpace(req.Model)
	req.PreviewText = strings.TrimSpace(req.PreviewText)
	req.FileName = filepath.Base(strings.TrimSpace(req.FileName))
	if !req.Authorized {
		return "", errors.New("confirm that you have permission to clone this voice")
	}
	if req.Name == "" {
		return "", errors.New("name is required")
	}
	if utf8.RuneCountInString(req.Name) > MaxVoiceCloneName {
		return "", fmt.Errorf("name must be at most %d characters", MaxVoiceCloneName)
	}
	if req.Model == "" {
		return "", errors.New("model is required")
	}
	if req.PreviewText == "" {
		return "", errors.New("preview text is required")
	}
	if utf8.RuneCountInString(req.PreviewText) > MaxVoicePreviewText {
		return "", fmt.Errorf("preview text must be at most %d characters", MaxVoicePreviewText)
	}
	if len(req.Sample) == 0 {
		return "", errors.New("sample audio is required")
	}
	if len(req.Sample) > MaxVoiceSampleBytes {
		return "", fmt.Errorf("sample audio must be at most %d MiB", MaxVoiceSampleBytes>>20)
	}
	format, err := detectVoiceSampleFormat(req.FileName, req.Sample)
	if err != nil {
		return "", err
	}
	if req.FileName == "" {
		req.FileName = "voice-sample." + format
	}
	return format, nil
}

func detectVoiceSampleFormat(filename string, data []byte) (string, error) {
	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	switch extension {
	case "mp3", "m4a", "wav":
	default:
		return "", errors.New("sample audio must be an mp3, m4a or wav file")
	}
	detected := sniffVoiceSampleFormat(data)
	if detected == "" {
		return "", errors.New("the uploaded file is not valid mp3, m4a or wav audio")
	}
	if detected != extension {
		return "", errors.New("the sample file extension does not match its audio content")
	}
	return extension, nil
}

func sniffVoiceSampleFormat(data []byte) string {
	switch {
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "wav"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return "m4a"
	case len(data) >= 3 && string(data[0:3]) == "ID3":
		return "mp3"
	case len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return "mp3"
	default:
		return ""
	}
}

// newVoiceCloneID returns a provider voice id that satisfies MiniMax's rules:
// it starts with a letter, is longer than 8 characters and only uses letters,
// digits and underscores.
func (g *Gateway) newVoiceCloneID() (string, error) {
	for range 5 {
		random := make([]byte, 10)
		if _, err := rand.Read(random); err != nil {
			return "", err
		}
		voiceID := "clone_" + hex.EncodeToString(random)
		if _, err := g.store.GetVoiceCloneByVoiceID(voiceID); errors.Is(err, store.ErrNotFound) {
			return voiceID, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique voice id")
}

func (g *Gateway) uploadVoiceSample(ctx context.Context, ch *store.Channel, apiKey, filename string, sample []byte) (int64, *voiceUpstreamError) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("purpose", "voice_clone"); err != nil {
		return 0, &voiceUpstreamError{status: http.StatusInternalServerError, message: err.Error()}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return 0, &voiceUpstreamError{status: http.StatusInternalServerError, message: err.Error()}
	}
	if _, err := part.Write(sample); err != nil {
		return 0, &voiceUpstreamError{status: http.StatusInternalServerError, message: err.Error()}
	}
	if err := writer.Close(); err != nil {
		return 0, &voiceUpstreamError{status: http.StatusInternalServerError, message: err.Error()}
	}
	req, err := g.upstreamRequest(ctx, ch, http.MethodPost, "/v1/files/upload", body.Bytes(), apiKey, false, true)
	if err != nil {
		return 0, &voiceUpstreamError{status: http.StatusBadGateway, message: err.Error(), retry: true}
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := g.speechClient.Do(req)
	if err != nil {
		return 0, &voiceUpstreamError{status: http.StatusBadGateway, message: err.Error(), retry: true}
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSpeechResponse))
	resp.Body.Close()
	if readErr != nil {
		return 0, &voiceUpstreamError{status: http.StatusBadGateway, message: readErr.Error(), retry: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, &voiceUpstreamError{
			status:  voiceCloneFailureStatus(int64(resp.StatusCode)),
			message: speechErrorMessage(data, resp.StatusCode),
			retry:   shouldFailover(resp.StatusCode),
		}
	}
	var payload struct {
		File struct {
			FileID int64 `json:"file_id"`
		} `json:"file"`
		BaseResp struct {
			StatusCode int64  `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, &voiceUpstreamError{
			status:  http.StatusBadGateway,
			message: "the provider returned an unexpected upload payload",
			retry:   true,
		}
	}
	if payload.BaseResp.StatusCode != 0 {
		return 0, providerVoiceFailure(payload.BaseResp.StatusCode, payload.BaseResp.StatusMsg, data, resp.StatusCode)
	}
	if payload.File.FileID <= 0 {
		return 0, &voiceUpstreamError{
			status:  http.StatusBadGateway,
			message: "the provider did not return a file id",
			retry:   true,
		}
	}
	return payload.File.FileID, nil
}

type voiceClonePreview struct {
	URL        string
	DurationMS int64
	Characters int64
}

func (g *Gateway) cloneVoice(ctx context.Context, ch *store.Channel, apiKey string, fileID int64, voiceID, text, upstreamModel string) (*voiceClonePreview, *voiceUpstreamError) {
	body, err := json.Marshal(map[string]any{
		"file_id":  fileID,
		"voice_id": voiceID,
		"text":     text,
		"model":    upstreamModel,
	})
	if err != nil {
		return nil, &voiceUpstreamError{status: http.StatusInternalServerError, message: err.Error()}
	}
	req, err := g.upstreamRequest(ctx, ch, http.MethodPost, "/v1/voice_clone", body, apiKey, false, true)
	if err != nil {
		return nil, &voiceUpstreamError{status: http.StatusBadGateway, message: err.Error(), retry: true}
	}
	resp, err := g.speechClient.Do(req)
	if err != nil {
		return nil, &voiceUpstreamError{status: http.StatusBadGateway, message: err.Error(), retry: true}
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSpeechResponse))
	resp.Body.Close()
	if readErr != nil {
		return nil, &voiceUpstreamError{status: http.StatusBadGateway, message: readErr.Error(), retry: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &voiceUpstreamError{
			status:  voiceCloneFailureStatus(int64(resp.StatusCode)),
			message: speechErrorMessage(data, resp.StatusCode),
			retry:   shouldFailover(resp.StatusCode),
		}
	}
	var payload struct {
		InputSensitive json.RawMessage `json:"input_sensitive"`
		DemoAudio      string          `json:"demo_audio"`
		ExtraInfo      struct {
			AudioLength     int64 `json:"audio_length"`
			UsageCharacters int64 `json:"usage_characters"`
		} `json:"extra_info"`
		BaseResp struct {
			StatusCode int64  `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, &voiceUpstreamError{
			status:  http.StatusBadGateway,
			message: "the provider returned an unexpected clone payload",
			retry:   true,
		}
	}
	if payload.BaseResp.StatusCode != 0 {
		return nil, providerVoiceFailure(payload.BaseResp.StatusCode, payload.BaseResp.StatusMsg, data, resp.StatusCode)
	}
	sensitive := false
	if len(payload.InputSensitive) > 0 {
		var flagged bool
		if err := json.Unmarshal(payload.InputSensitive, &flagged); err == nil {
			sensitive = flagged
		} else {
			var detail struct {
				Type int `json:"type"`
			}
			if err := json.Unmarshal(payload.InputSensitive, &detail); err == nil && detail.Type != 0 {
				sensitive = true
			}
		}
	}
	if sensitive {
		return nil, &voiceUpstreamError{
			status:  http.StatusBadRequest,
			message: "MiniMax rejected the sample recording in its content safety check",
		}
	}
	return &voiceClonePreview{
		URL:        strings.TrimSpace(payload.DemoAudio),
		DurationMS: payload.ExtraInfo.AudioLength,
		Characters: payload.ExtraInfo.UsageCharacters,
	}, nil
}

func providerVoiceFailure(code int64, message string, data []byte, httpStatus int) *voiceUpstreamError {
	message = strings.TrimSpace(message)
	if message == "" {
		message = speechErrorMessage(data, httpStatus)
	}
	failure := &voiceUpstreamError{status: voiceCloneFailureStatus(code), message: message}
	switch code {
	case 1000, 1002, 1004, 1013, 1039, 2049:
		failure.retry = true
	}
	return failure
}

// voiceCloneFailureStatus maps MiniMax speech/voice error codes to the closest
// HTTP status for the console.
func voiceCloneFailureStatus(code int64) int {
	switch code {
	case 1002:
		return http.StatusTooManyRequests
	case 1008:
		return http.StatusPaymentRequired
	case 2013, 1043:
		return http.StatusBadRequest
	case 2038:
		return http.StatusForbidden
	case 1004, 2049, 401:
		return http.StatusBadGateway
	case 1000, 1013, 1039:
		return http.StatusBadGateway
	default:
		if code >= 400 && code < 600 {
			return int(code)
		}
		return http.StatusBadGateway
	}
}

func (g *Gateway) downloadVoicePreview(ctx context.Context, rawURL string) ([]byte, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, "", errors.New("the provider returned an invalid preview URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := g.speechClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxVoicePreviewAudio+1))
	resp.Body.Close()
	if readErr != nil {
		return nil, "", readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("preview download returned %d", resp.StatusCode)
	}
	if len(data) == 0 {
		return nil, "", errors.New("the preview download was empty")
	}
	if len(data) > maxVoicePreviewAudio {
		return nil, "", errors.New("the preview audio is too large")
	}
	return data, previewFormat(resp.Header.Get("Content-Type"), parsed.Path), nil
}

func previewFormat(contentType, path string) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/mp4", "audio/x-m4a", "audio/m4a", "audio/aac":
		return "m4a"
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	}
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "wav":
		return "wav"
	case "m4a":
		return "m4a"
	case "mp3":
		return "mp3"
	}
	return "mp3"
}

func (g *Gateway) saveVoicePreview(clone *store.VoiceClone, audio []byte, format string, durationMS, characters int64) error {
	dir := filepath.Join(g.cfg.DataDir, "voice-clones")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, fmt.Sprintf("voice-%d-*.%s", clone.ID, format))
	if err != nil {
		return err
	}
	name := file.Name()
	written, writeErr := file.Write(audio)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if written != len(audio) {
		_ = os.Remove(name)
		return errors.New("could not write the preview audio")
	}
	rel, err := filepath.Rel(g.cfg.DataDir, name)
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	previous := clone.PreviewPath
	clone.PreviewPath = rel
	clone.PreviewFormat = format
	clone.PreviewDurationMS = durationMS
	clone.PreviewCharacters = characters
	if clone.PreviewCharacters <= 0 {
		clone.PreviewCharacters = int64(utf8.RuneCountInString(clone.PreviewText))
	}
	if err := g.store.UpdateVoiceClonePreview(clone); err != nil {
		_ = os.Remove(name)
		clone.PreviewPath = previous
		return err
	}
	if previous != "" && previous != rel {
		_ = os.Remove(filepath.Join(g.cfg.DataDir, previous))
	}
	return nil
}
