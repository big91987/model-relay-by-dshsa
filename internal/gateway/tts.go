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
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

const (
	maxSpeechText     = 10000
	maxSpeechVoiceID  = 200
	maxSpeechResponse = 96 << 20
	maxSpeechAudio    = 48 << 20
	// speechRequestTimeout bounds one synchronous synthesis. Long-form speech
	// is generated before the response starts, so it needs more time than a
	// normal chat request.
	speechRequestTimeout = 10 * time.Minute
)

// SpeechError is a client-visible failure while synthesizing speech.
type SpeechError struct {
	Status  int
	Message string
}

func (e *SpeechError) Error() string { return e.Message }

// SpeechRequest describes one MiniMax text-to-speech request.
type SpeechRequest struct {
	Model     string
	Text      string
	VoiceID   string
	VoiceName string
	Speed     float64
}

// SpeechVoice is one selectable MiniMax system voice.
type SpeechVoice struct {
	VoiceID     string `json:"voice_id"`
	Name        string `json:"name"`
	Language    string `json:"language"`
	Description string `json:"description"`
}

// SpeechVoiceCatalog is the system-voice list shown by the console. Source is
// "provider" when MiniMax answered and "builtin" when the relay fell back to
// its bundled list.
type SpeechVoiceCatalog struct {
	Voices  []SpeechVoice `json:"voices"`
	Source  string        `json:"source"`
	Warning string        `json:"warning,omitempty"`
}

// builtinSpeechVoices keeps the Speech page usable when the provider's
// get_voice endpoint cannot be reached. The live catalog remains authoritative
// and includes every voice the account can use.
var builtinSpeechVoices = []SpeechVoice{
	{VoiceID: "English_expressive_narrator", Name: "Expressive Narrator", Language: "English"},
	{VoiceID: "English_radiant_girl", Name: "Radiant Girl", Language: "English"},
	{VoiceID: "English_magnetic_voiced_man", Name: "Magnetic-voiced Male", Language: "English"},
	{VoiceID: "English_Graceful_Lady", Name: "Graceful Lady", Language: "English"},
	{VoiceID: "English_CalmWoman", Name: "Calm Woman", Language: "English"},
	{VoiceID: "English_Trustworth_Man", Name: "Trustworthy Man", Language: "English"},
	{VoiceID: "English_WiseScholar", Name: "Wise Scholar", Language: "English"},
	{VoiceID: "English_CaptivatingStoryteller", Name: "Captivating Storyteller", Language: "English"},
	{VoiceID: "Chinese (Mandarin)_Reliable_Executive", Name: "Reliable Executive", Language: "Chinese (Mandarin)"},
	{VoiceID: "Chinese (Mandarin)_News_Anchor", Name: "News Anchor", Language: "Chinese (Mandarin)"},
	{VoiceID: "Chinese (Mandarin)_Warm_Girl", Name: "Warm Girl", Language: "Chinese (Mandarin)"},
	{VoiceID: "Chinese (Mandarin)_Gentleman", Name: "Gentleman", Language: "Chinese (Mandarin)"},
	{VoiceID: "Chinese (Mandarin)_Lyrical_Voice", Name: "Lyrical Voice", Language: "Chinese (Mandarin)"},
	{VoiceID: "Chinese (Mandarin)_Radio_Host", Name: "Radio Host", Language: "Chinese (Mandarin)"},
	{VoiceID: "Chinese (Mandarin)_Mature_Woman", Name: "Mature Woman", Language: "Chinese (Mandarin)"},
	{VoiceID: "Chinese (Mandarin)_Kind-hearted_Elder", Name: "Kind-hearted Elder", Language: "Chinese (Mandarin)"},
	{VoiceID: "Japanese_IntellectualSenior", Name: "Intellectual Senior", Language: "Japanese"},
	{VoiceID: "Japanese_CalmLady", Name: "Calm Lady", Language: "Japanese"},
	{VoiceID: "Japanese_GentleButler", Name: "Gentle Butler", Language: "Japanese"},
	{VoiceID: "Japanese_KindLady", Name: "Kind Lady", Language: "Japanese"},
	{VoiceID: "Korean_CalmGentleman", Name: "Calm Gentleman", Language: "Korean"},
	{VoiceID: "Korean_CheerfulBoyfriend", Name: "Cheerful Boyfriend", Language: "Korean"},
	{VoiceID: "Korean_CalmLady", Name: "Calm Lady", Language: "Korean"},
	{VoiceID: "Cantonese_GentleLady", Name: "Gentle Lady", Language: "Cantonese"},
	{VoiceID: "Cantonese_ProfessionalHost (F)", Name: "Professional Female Host", Language: "Cantonese"},
	{VoiceID: "Spanish_SereneWoman", Name: "Serene Woman", Language: "Spanish"},
	{VoiceID: "Spanish_Narrator", Name: "Narrator", Language: "Spanish"},
	{VoiceID: "Portuguese_SentimentalLady", Name: "Sentimental Lady", Language: "Portuguese"},
}

type speechPayload struct {
	Data struct {
		Audio  string `json:"audio"`
		Status int    `json:"status"`
	} `json:"data"`
	ExtraInfo struct {
		AudioLength     int64  `json:"audio_length"`
		AudioFormat     string `json:"audio_format"`
		AudioSize       int64  `json:"audio_size"`
		UsageCharacters int64  `json:"usage_characters"`
	} `json:"extra_info"`
	BaseResp struct {
		StatusCode int64  `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// ListSpeechVoices reads the account's system voices from an enabled MiniMax
// channel. When no channel answers, it returns the built-in list together with
// a warning instead of failing the console page.
func (g *Gateway) ListSpeechVoices(ctx context.Context, model string) (*SpeechVoiceCatalog, error) {
	model = strings.TrimSpace(model)
	channels, err := g.store.ListChannels()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	attempted := false
	var lastErr error
	for _, cand := range orderCandidates(channels, model) {
		ch := cand.channel
		if ch.Type != "minimax" {
			continue
		}
		attempted = true
		apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
		if err != nil {
			lastErr = fmt.Errorf("channel %q: %w", ch.Name, err)
			continue
		}
		req, err := g.upstreamRequest(ctx, &ch, http.MethodPost, "/v1/get_voice", []byte(`{"voice_type":"system"}`), apiKey, false, true)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := g.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("channel %q: %w", ch.Name, err)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxSpeechResponse))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("channel %q: %w", ch.Name, readErr)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("channel %q returned %d: %s", ch.Name, resp.StatusCode, speechErrorMessage(data, resp.StatusCode))
			continue
		}
		var payload struct {
			SystemVoice []struct {
				VoiceID     string   `json:"voice_id"`
				VoiceName   string   `json:"voice_name"`
				Description []string `json:"description"`
			} `json:"system_voice"`
			BaseResp struct {
				StatusCode int64  `json:"status_code"`
				StatusMsg  string `json:"status_msg"`
			} `json:"base_resp"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			lastErr = fmt.Errorf("channel %q returned an unexpected voice payload: %w", ch.Name, err)
			continue
		}
		if payload.BaseResp.StatusCode != 0 {
			lastErr = fmt.Errorf("channel %q: %s (%d)", ch.Name, payload.BaseResp.StatusMsg, payload.BaseResp.StatusCode)
			continue
		}
		voices := make([]SpeechVoice, 0, len(payload.SystemVoice))
		for _, item := range payload.SystemVoice {
			id := strings.TrimSpace(item.VoiceID)
			if id == "" {
				continue
			}
			name := strings.TrimSpace(item.VoiceName)
			if name == "" {
				name = id
			}
			voices = append(voices, SpeechVoice{
				VoiceID:     id,
				Name:        name,
				Language:    speechVoiceLanguage(id),
				Description: truncate(strings.TrimSpace(strings.Join(item.Description, " ")), 500),
			})
		}
		if len(voices) == 0 {
			lastErr = fmt.Errorf("channel %q returned no system voices", ch.Name)
			continue
		}
		sortSpeechVoices(voices)
		return &SpeechVoiceCatalog{Voices: voices, Source: "provider"}, nil
	}

	warning := ""
	switch {
	case !attempted && model != "":
		warning = fmt.Sprintf("No enabled MiniMax channel serves model %q.", model)
	case !attempted:
		warning = "No enabled MiniMax channel is available."
	case lastErr != nil:
		warning = "Could not load voices from MiniMax: " + lastErr.Error() + ". Showing the built-in system voice list."
	}
	voices := append([]SpeechVoice(nil), builtinSpeechVoices...)
	sortSpeechVoices(voices)
	return &SpeechVoiceCatalog{Voices: voices, Source: "builtin", Warning: warning}, nil
}

// CreateSpeech validates a request, synthesizes it on an enabled MiniMax
// channel, stores the returned audio and returns the saved clip.
func (g *Gateway) CreateSpeech(ctx context.Context, req SpeechRequest) (*store.SpeechClip, error) {
	if err := normalizeSpeechRequest(&req); err != nil {
		return nil, &SpeechError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	channels, err := g.store.ListChannels()
	if err != nil {
		return nil, err
	}
	// A cloned voice only exists on the channel that cloned it, so pin
	// synthesis there instead of letting routing pick another MiniMax account.
	var cloneChannelID int64
	if clone, cloneErr := g.store.GetVoiceCloneByVoiceID(req.VoiceID); cloneErr == nil {
		cloneChannelID = clone.ChannelID
	} else if !errors.Is(cloneErr, store.ErrNotFound) {
		return nil, cloneErr
	}
	candidates := make([]candidate, 0)
	for _, cand := range orderCandidates(channels, req.Model) {
		if cand.channel.Type != "minimax" {
			continue
		}
		if cloneChannelID != 0 && cand.channel.ID != cloneChannelID {
			continue
		}
		candidates = append(candidates, cand)
	}
	if len(candidates) == 0 {
		message := fmt.Sprintf("model %q is not available on any enabled MiniMax channel", req.Model)
		if cloneChannelID != 0 {
			message = fmt.Sprintf("the channel that owns cloned voice %q does not serve model %q", req.VoiceID, req.Model)
		}
		return nil, &SpeechError{Status: http.StatusNotFound, Message: message}
	}

	ctx, cancel := context.WithTimeout(ctx, speechRequestTimeout)
	defer cancel()

	var lastErr string
	for _, cand := range candidates {
		ch := cand.channel
		apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
		if err != nil {
			lastErr = fmt.Sprintf("channel %q: %v", ch.Name, err)
			continue
		}
		body, err := json.Marshal(map[string]any{
			"model":         cand.upstream,
			"text":          req.Text,
			"stream":        false,
			"output_format": "hex",
			"voice_setting": map[string]any{
				"voice_id": req.VoiceID,
				"speed":    req.Speed,
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
		upreq, err := g.upstreamRequest(ctx, &ch, http.MethodPost, "/v1/t2a_v2", body, apiKey, false, true)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		resp, err := g.speechClient.Do(upreq)
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

		var payload speechPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			lastErr = fmt.Sprintf("channel %q returned an unexpected speech payload: %v", ch.Name, err)
			continue
		}
		if payload.BaseResp.StatusCode != 0 {
			message := strings.TrimSpace(payload.BaseResp.StatusMsg)
			if message == "" {
				message = speechErrorMessage(data, resp.StatusCode)
			}
			switch payload.BaseResp.StatusCode {
			case 1002, 1004, 1039:
				// Rate limit or authentication: another channel may work.
				lastErr = fmt.Sprintf("channel %q: %s (%d)", ch.Name, message, payload.BaseResp.StatusCode)
				continue
			}
			status := http.StatusBadGateway
			if payload.BaseResp.StatusCode == 2013 || payload.BaseResp.StatusCode == 1042 {
				status = http.StatusBadRequest
			}
			return nil, &SpeechError{Status: status, Message: message}
		}
		if payload.Data.Status != 0 && payload.Data.Status != 2 {
			lastErr = fmt.Sprintf("channel %q did not finish synthesizing (status %d)", ch.Name, payload.Data.Status)
			continue
		}
		audioHex := strings.TrimSpace(payload.Data.Audio)
		if audioHex == "" {
			lastErr = fmt.Sprintf("channel %q returned no audio", ch.Name)
			continue
		}
		audio, err := hex.DecodeString(audioHex)
		if err != nil || len(audio) == 0 {
			lastErr = fmt.Sprintf("channel %q returned invalid audio data", ch.Name)
			continue
		}
		if len(audio) > maxSpeechAudio {
			return nil, &SpeechError{
				Status:  http.StatusBadGateway,
				Message: fmt.Sprintf("the generated audio exceeds the %d MiB limit", maxSpeechAudio>>20),
			}
		}
		format := strings.ToLower(strings.TrimSpace(payload.ExtraInfo.AudioFormat))
		switch format {
		case "mp3", "flac", "wav":
		default:
			format = "mp3"
		}
		clip, err := g.storeSpeechClip(&ch, cand.upstream, req, audio, format, payload.ExtraInfo.AudioLength, payload.ExtraInfo.UsageCharacters)
		if err != nil {
			return nil, err
		}
		return clip, nil
	}

	message := "no enabled MiniMax channel accepted the speech request"
	if lastErr != "" {
		message += ": " + lastErr
	}
	return nil, &SpeechError{Status: http.StatusBadGateway, Message: message}
}

func normalizeSpeechRequest(req *SpeechRequest) error {
	req.Model = strings.TrimSpace(req.Model)
	req.Text = strings.TrimSpace(req.Text)
	req.VoiceID = strings.TrimSpace(req.VoiceID)
	req.VoiceName = strings.TrimSpace(req.VoiceName)
	if req.Model == "" {
		return errors.New("model is required")
	}
	if req.Text == "" {
		return errors.New("text is required")
	}
	if utf8.RuneCountInString(req.Text) > maxSpeechText {
		return fmt.Errorf("text must be at most %d characters", maxSpeechText)
	}
	if req.VoiceID == "" {
		return errors.New("voice_id is required")
	}
	if len(req.VoiceID) > maxSpeechVoiceID {
		return fmt.Errorf("voice_id must be at most %d characters", maxSpeechVoiceID)
	}
	if req.Speed == 0 {
		req.Speed = 1
	}
	if req.Speed < 0.5 || req.Speed > 2 {
		return errors.New("speed must be between 0.5 and 2")
	}
	return nil
}

func (g *Gateway) storeSpeechClip(ch *store.Channel, upstream string, req SpeechRequest, audio []byte, format string, durationMS, characters int64) (*store.SpeechClip, error) {
	dir := filepath.Join(g.cfg.DataDir, "speech")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, "speech-*."+format)
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
		return nil, errors.New("could not write the generated audio")
	}
	rel, err := filepath.Rel(g.cfg.DataDir, name)
	if err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	voiceName := truncate(req.VoiceName, 200)
	if voiceName == "" {
		voiceName = req.VoiceID
	}
	clip := &store.SpeechClip{
		ChannelID:     ch.ID,
		ChannelName:   ch.Name,
		Model:         req.Model,
		UpstreamModel: upstream,
		VoiceID:       req.VoiceID,
		VoiceName:     voiceName,
		Text:          req.Text,
		Format:        format,
		AudioPath:     rel,
		DurationMS:    durationMS,
		Characters:    characters,
		SizeBytes:     int64(len(audio)),
	}
	if clip.Characters <= 0 {
		clip.Characters = int64(utf8.RuneCountInString(req.Text))
	}
	if err := g.store.CreateSpeechClip(clip); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	return clip, nil
}

// speechVoiceLanguage derives a grouping label from a MiniMax voice id such as
// "Chinese (Mandarin)_News_Anchor" or "English_calm_narrator".
func speechVoiceLanguage(voiceID string) string {
	switch {
	case strings.HasPrefix(voiceID, "Chinese (Mandarin)"):
		return "Chinese (Mandarin)"
	case strings.HasPrefix(voiceID, "Cantonese"), strings.HasPrefix(voiceID, "Chinese (Cantonese)"):
		return "Cantonese"
	case strings.HasPrefix(voiceID, "moss_audio_"):
		return "Other"
	}
	if index := strings.Index(voiceID, "_"); index > 0 && index <= 24 {
		return voiceID[:index]
	}
	return "Other"
}

func sortSpeechVoices(voices []SpeechVoice) {
	sort.SliceStable(voices, func(i, j int) bool {
		if voices[i].Language != voices[j].Language {
			return voices[i].Language < voices[j].Language
		}
		if voices[i].Name != voices[j].Name {
			return voices[i].Name < voices[j].Name
		}
		return voices[i].VoiceID < voices[j].VoiceID
	})
}

// speechErrorMessage extracts the human-readable message from a MiniMax error.
// The provider often returns HTTP 200 with the failure in base_resp, so callers
// also use this for non-2xx bodies.
func speechErrorMessage(data []byte, status int) string {
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
