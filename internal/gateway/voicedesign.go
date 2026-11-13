package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

const (
	maxVoiceDesignPrompt      = 2000
	maxVoiceDesignPreviewText = 500
	maxVoiceDesignName        = 60
	voiceDesignRequestTimeout = 3 * time.Minute
)

// VoiceDesignRequest describes one MiniMax voice-design request: a free-text
// description of the voice and the sample text used for the preview clip.
type VoiceDesignRequest struct {
	Model       string
	Prompt      string
	PreviewText string
	// VoiceID optionally asks the provider to use a specific custom voice id.
	// An empty value lets MiniMax generate one.
	VoiceID string
}

// CreateVoiceDesign designs a custom MiniMax voice from a text description,
// stores the generated preview audio and records the voice so the console can
// replay the preview and synthesize more text with it.
func (g *Gateway) CreateVoiceDesign(ctx context.Context, req VoiceDesignRequest) (*store.VoiceDesign, error) {
	if err := normalizeVoiceDesignRequest(&req); err != nil {
		return nil, &SpeechError{Status: http.StatusBadRequest, Message: err.Error()}
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
		return nil, &SpeechError{
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("model %q is not available on any enabled MiniMax channel", req.Model),
		}
	}

	ctx, cancel := context.WithTimeout(ctx, voiceDesignRequestTimeout)
	defer cancel()

	var lastErr string
	for _, cand := range candidates {
		ch := cand.channel
		apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
		if err != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, err)
			continue
		}
		payload := map[string]any{
			"prompt":       req.Prompt,
			"preview_text": req.PreviewText,
		}
		if req.VoiceID != "" {
			payload["voice_id"] = req.VoiceID
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		upreq, err := g.upstreamRequest(ctx, &ch, http.MethodPost, "/v1/voice_design", body, apiKey, false, true)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		resp, err := g.client.Do(upreq)
		if err != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSpeechResponse))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, readErr)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			message := speechErrorMessage(data, resp.StatusCode)
			if shouldFailover(resp.StatusCode) {
				lastErr = fmt.Sprintf("channel %q returned %d: %s", ch.Name, resp.StatusCode, message)
				continue
			}
			return nil, &SpeechError{Status: resp.StatusCode, Message: message}
		}

		var parsed struct {
			VoiceID    string `json:"voice_id"`
			TrialAudio string `json:"trial_audio"`
			BaseResp   struct {
				StatusCode int64  `json:"status_code"`
				StatusMsg  string `json:"status_msg"`
			} `json:"base_resp"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			lastErr = fmt.Sprintf("channel %q returned an unexpected voice-design payload: %v", ch.Name, err)
			continue
		}
		if parsed.BaseResp.StatusCode != 0 {
			message := strings.TrimSpace(parsed.BaseResp.StatusMsg)
			if message == "" {
				message = speechErrorMessage(data, resp.StatusCode)
			}
			switch parsed.BaseResp.StatusCode {
			case 1002, 1004, 1039:
				// Rate limit or authentication: another channel may work.
				lastErr = fmt.Sprintf("channel %q: %s (%d)", ch.Name, message, parsed.BaseResp.StatusCode)
				continue
			}
			status := http.StatusBadGateway
			if parsed.BaseResp.StatusCode == 2013 || parsed.BaseResp.StatusCode == 1042 {
				status = http.StatusBadRequest
			}
			return nil, &SpeechError{Status: status, Message: message}
		}
		voiceID := strings.TrimSpace(parsed.VoiceID)
		if voiceID == "" {
			lastErr = fmt.Sprintf("channel %q returned no voice id", ch.Name)
			continue
		}
		audioHex := strings.TrimSpace(parsed.TrialAudio)
		if audioHex == "" {
			lastErr = fmt.Sprintf("channel %q returned no preview audio", ch.Name)
			continue
		}
		audio, err := hex.DecodeString(audioHex)
		if err != nil || len(audio) == 0 {
			lastErr = fmt.Sprintf("channel %q returned invalid preview audio", ch.Name)
			continue
		}
		if len(audio) > maxSpeechAudio {
			return nil, &SpeechError{
				Status:  http.StatusBadGateway,
				Message: fmt.Sprintf("the preview audio exceeds the %d MiB limit", maxSpeechAudio>>20),
			}
		}
		design, err := g.storeVoiceDesign(&ch, cand.upstream, req, voiceID, audio)
		if err != nil {
			return nil, err
		}
		return design, nil
	}

	message := "no enabled MiniMax channel accepted the voice-design request"
	if lastErr != "" {
		message += ": " + lastErr
	}
	return nil, &SpeechError{Status: http.StatusBadGateway, Message: message}
}

func normalizeVoiceDesignRequest(req *VoiceDesignRequest) error {
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.PreviewText = strings.TrimSpace(req.PreviewText)
	req.VoiceID = strings.TrimSpace(req.VoiceID)
	if req.Model == "" {
		return errors.New("model is required")
	}
	if req.Prompt == "" {
		return errors.New("prompt is required")
	}
	if utf8.RuneCountInString(req.Prompt) > maxVoiceDesignPrompt {
		return fmt.Errorf("prompt must be at most %d characters", maxVoiceDesignPrompt)
	}
	if req.PreviewText == "" {
		return errors.New("preview_text is required")
	}
	if utf8.RuneCountInString(req.PreviewText) > maxVoiceDesignPreviewText {
		return fmt.Errorf("preview_text must be at most %d characters", maxVoiceDesignPreviewText)
	}
	if len(req.VoiceID) > maxSpeechVoiceID {
		return fmt.Errorf("voice_id must be at most %d characters", maxSpeechVoiceID)
	}
	return nil
}

func (g *Gateway) storeVoiceDesign(ch *store.Channel, upstream string, req VoiceDesignRequest, voiceID string, audio []byte) (*store.VoiceDesign, error) {
	dir := filepath.Join(g.cfg.DataDir, "voice-design")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	format := detectSpeechAudioFormat(audio)
	file, err := os.CreateTemp(dir, "voice-design-*."+format)
	if err != nil {
		return nil, err
	}
	name := file.Name()
	written, writeErr := file.Write(audio)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		if writeErr != nil {
			return nil, writeErr
		}
		return nil, closeErr
	}
	if written != len(audio) {
		_ = os.Remove(name)
		return nil, errors.New("could not write the preview audio")
	}
	rel, err := filepath.Rel(g.cfg.DataDir, name)
	if err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	design := &store.VoiceDesign{
		ChannelID:         ch.ID,
		ChannelName:       ch.Name,
		Model:             req.Model,
		UpstreamModel:     upstream,
		VoiceID:           voiceID,
		Name:              voiceDesignName(req.Prompt, voiceID),
		Prompt:            req.Prompt,
		PreviewText:       req.PreviewText,
		PreviewPath:       rel,
		PreviewFormat:     format,
		PreviewCharacters: int64(utf8.RuneCountInString(req.PreviewText)),
		PreviewSizeBytes:  int64(len(audio)),
	}
	if err := g.store.CreateVoiceDesign(design); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	return design, nil
}

// detectSpeechAudioFormat sniffs the container of provider audio that arrives
// without format metadata, which is the case for voice-design previews.
func detectSpeechAudioFormat(audio []byte) string {
	switch {
	case len(audio) >= 4 && string(audio[:4]) == "fLaC":
		return "flac"
	case len(audio) >= 12 && string(audio[:4]) == "RIFF" && string(audio[8:12]) == "WAVE":
		return "wav"
	default:
		return "mp3"
	}
}

// voiceDesignName derives a short display name from the description so clips
// synthesized with the voice remain recognizable in history.
func voiceDesignName(prompt, voiceID string) string {
	name := strings.Join(strings.Fields(prompt), " ")
	if name == "" {
		return voiceID
	}
	if utf8.RuneCountInString(name) <= maxVoiceDesignName {
		return name
	}
	runes := []rune(name)
	short := strings.TrimSpace(string(runes[:maxVoiceDesignName]))
	if index := strings.LastIndexAny(short, " ,，、;；:："); index > 20 {
		short = strings.TrimSpace(short[:index])
	}
	short = strings.TrimRight(short, " ,，、;；:：")
	return short + "…"
}
