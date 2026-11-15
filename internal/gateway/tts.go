package gateway

import (
	"bufio"
	"context"
	"encoding/binary"
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

	// speechStreamSampleRate is the PCM rate used for progressive playback.
	// MiniMax streams 16-bit little-endian mono PCM, which the browser can
	// schedule through the Web Audio API without a container.
	speechStreamSampleRate = 32000
	speechStreamChannels   = 1
)

// SpeechError is a client-visible failure while synthesizing speech.
type SpeechError struct {
	Status  int
	Message string
}

func (e *SpeechError) Error() string { return e.Message }

// SpeechStreamInfo describes the progressive audio a StreamSpeech call emits.
type SpeechStreamInfo struct {
	Format     string
	SampleRate int
	Channels   int
}

// SpeechStreamHandlers receive audio while MiniMax is still synthesizing it.
// OnStart runs once, immediately before the first chunk; an error from either
// callback stops the stream, for example when the browser has disconnected.
type SpeechStreamHandlers struct {
	OnStart func(info SpeechStreamInfo) error
	OnAudio func(pcm []byte) error
}

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

// StreamSpeech synthesizes speech with MiniMax streaming and hands PCM chunks
// to the caller while the provider is still generating them. A clip is stored
// only after the provider reports the final chunk, so stopping playback never
// leaves a truncated history entry.
func (g *Gateway) StreamSpeech(ctx context.Context, req SpeechRequest, handlers SpeechStreamHandlers) (*store.SpeechClip, error) {
	if err := normalizeSpeechRequest(&req); err != nil {
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

	ctx, cancel := context.WithTimeout(ctx, speechRequestTimeout)
	defer cancel()

	var lastErr string
	for _, cand := range candidates {
		clip, emitted, retryable, err := g.streamSpeechAttempt(ctx, cand, req, handlers)
		if err == nil {
			return clip, nil
		}
		if emitted {
			// Audio already reached the caller, so another channel would
			// concatenate unrelated speech into the same clip.
			var speechErr *SpeechError
			if errors.As(err, &speechErr) {
				return nil, speechErr
			}
			return nil, &SpeechError{Status: http.StatusBadGateway, Message: err.Error()}
		}
		if !retryable {
			var speechErr *SpeechError
			if errors.As(err, &speechErr) {
				return nil, speechErr
			}
			return nil, &SpeechError{Status: http.StatusBadGateway, Message: err.Error()}
		}
		lastErr = err.Error()
	}

	message := "no enabled MiniMax channel accepted the speech request"
	if lastErr != "" {
		message += ": " + lastErr
	}
	return nil, &SpeechError{Status: http.StatusBadGateway, Message: message}
}

// streamSpeechAttempt tries one channel. It reports whether audio was already
// handed to the caller (no failover is possible afterwards) and whether the
// failure may be retried on another channel.
func (g *Gateway) streamSpeechAttempt(ctx context.Context, cand candidate, req SpeechRequest, handlers SpeechStreamHandlers) (*store.SpeechClip, bool, bool, error) {
	ch := cand.channel
	apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
	if err != nil {
		return nil, false, true, fmt.Errorf("channel %q: %w", ch.Name, err)
	}
	body, err := json.Marshal(map[string]any{
		"model":         cand.upstream,
		"text":          req.Text,
		"stream":        true,
		"output_format": "hex",
		"voice_setting": map[string]any{
			"voice_id": req.VoiceID,
			"speed":    req.Speed,
			"vol":      1,
			"pitch":    0,
		},
		"audio_setting": map[string]any{
			"format":      "pcm",
			"sample_rate": speechStreamSampleRate,
			"channel":     speechStreamChannels,
		},
	})
	if err != nil {
		return nil, false, false, err
	}
	upreq, err := g.upstreamRequest(ctx, &ch, http.MethodPost, "/v1/t2a_v2", body, apiKey, true, false)
	if err != nil {
		return nil, false, true, err
	}
	resp, err := g.speechClient.Do(upreq)
	if err != nil {
		return nil, false, true, fmt.Errorf("channel %q: %w", ch.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxSpeechResponse))
		message := speechErrorMessage(data, resp.StatusCode)
		if shouldFailover(resp.StatusCode) {
			return nil, false, true, fmt.Errorf("channel %q returned %d: %s", ch.Name, resp.StatusCode, message)
		}
		return nil, false, false, &SpeechError{Status: resp.StatusCode, Message: message}
	}

	collector := &speechStreamCollector{handlers: handlers}
	retryable, err := collector.read(resp)
	if err != nil {
		return nil, collector.started, retryable, err
	}
	if !collector.started || len(collector.audio) == 0 {
		return nil, collector.started, true, fmt.Errorf("channel %q returned no audio", ch.Name)
	}
	if !collector.complete {
		return nil, true, false, &SpeechError{
			Status:  http.StatusBadGateway,
			Message: fmt.Sprintf("channel %q ended the speech stream before synthesis completed", ch.Name),
		}
	}

	durationMS := collector.durationMS
	if durationMS <= 0 {
		durationMS = int64(len(collector.audio)/2) * 1000 / speechStreamSampleRate
	}
	audio := pcmToWAV(collector.audio, speechStreamSampleRate, speechStreamChannels)
	clip, err := g.storeSpeechClip(&ch, cand.upstream, req, audio, "wav", durationMS, collector.characters)
	if err != nil {
		g.logger.Error("store streamed speech clip failed", "channel", ch.Name, "error", err)
		return nil, true, false, &SpeechError{Status: http.StatusInternalServerError, Message: "failed to store the generated speech"}
	}
	return clip, true, false, nil
}

// speechStreamCollector accumulates one channel's progressive audio and
// forwards each chunk to the caller.
type speechStreamCollector struct {
	handlers   SpeechStreamHandlers
	started    bool
	complete   bool
	audio      []byte
	durationMS int64
	characters int64
}

// read consumes an upstream response: SSE when the provider streams, or a
// single JSON payload when it ignored the stream flag.
func (c *speechStreamCollector) read(resp *http.Response) (bool, error) {
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechResponse))
		if err != nil {
			return true, fmt.Errorf("read the speech response: %w", err)
		}
		var payload speechPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			return true, fmt.Errorf("the provider returned an unexpected speech payload: %w", err)
		}
		if retryable, err := c.handle(payload); err != nil {
			return retryable, err
		}
		// A non-streaming body is complete by definition.
		c.complete = true
		return false, nil
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var payload speechPayload
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			if c.started {
				return false, &SpeechError{Status: http.StatusBadGateway, Message: "the provider returned a malformed speech stream"}
			}
			return true, fmt.Errorf("the provider returned a malformed speech stream: %w", err)
		}
		if retryable, err := c.handle(payload); err != nil {
			return retryable, err
		}
		if c.complete {
			return false, nil
		}
	}
	if err := scanner.Err(); err != nil {
		if c.started {
			return false, &SpeechError{Status: http.StatusBadGateway, Message: "the speech stream was interrupted: " + err.Error()}
		}
		return true, fmt.Errorf("read the speech stream: %w", err)
	}
	return false, nil
}

// handle processes one provider payload and returns whether the failure may be
// retried on another channel.
func (c *speechStreamCollector) handle(payload speechPayload) (bool, error) {
	if payload.BaseResp.StatusCode != 0 {
		message := strings.TrimSpace(payload.BaseResp.StatusMsg)
		if message == "" {
			message = "speech synthesis failed"
		}
		if c.started {
			return false, &SpeechError{Status: http.StatusBadGateway, Message: message}
		}
		switch payload.BaseResp.StatusCode {
		case 1002, 1004, 1039:
			// Rate limit or authentication: another channel may work.
			return true, fmt.Errorf("%s (%d)", message, payload.BaseResp.StatusCode)
		}
		status := http.StatusBadGateway
		if payload.BaseResp.StatusCode == 2013 || payload.BaseResp.StatusCode == 1042 {
			status = http.StatusBadRequest
		}
		return false, &SpeechError{Status: status, Message: message}
	}
	if payload.ExtraInfo.AudioLength > 0 {
		c.durationMS = payload.ExtraInfo.AudioLength
	}
	if payload.ExtraInfo.UsageCharacters > 0 {
		c.characters = payload.ExtraInfo.UsageCharacters
	}
	if payload.Data.Status == 2 {
		c.complete = true
	}
	audioHex := strings.TrimSpace(payload.Data.Audio)
	if audioHex == "" {
		return false, nil
	}
	audio, err := hex.DecodeString(audioHex)
	if err != nil || len(audio) == 0 {
		if c.started {
			return false, &SpeechError{Status: http.StatusBadGateway, Message: "the provider returned invalid audio data"}
		}
		return true, errors.New("the provider returned invalid audio data")
	}
	if len(c.audio)+len(audio) > maxSpeechAudio {
		return false, &SpeechError{
			Status:  http.StatusBadGateway,
			Message: fmt.Sprintf("the generated audio exceeds the %d MiB limit", maxSpeechAudio>>20),
		}
	}
	if !c.started {
		c.started = true
		if c.handlers.OnStart != nil {
			if err := c.handlers.OnStart(SpeechStreamInfo{
				Format:     "pcm",
				SampleRate: speechStreamSampleRate,
				Channels:   speechStreamChannels,
			}); err != nil {
				return false, err
			}
		}
	}
	c.audio = append(c.audio, audio...)
	if c.handlers.OnAudio != nil {
		if err := c.handlers.OnAudio(audio); err != nil {
			return false, err
		}
	}
	return false, nil
}

// pcmToWAV wraps 16-bit little-endian PCM in a WAV container so the stored
// clip can be replayed with a normal <audio> element.
func pcmToWAV(pcm []byte, sampleRate, channels int) []byte {
	const headerSize = 44
	// A WAV data chunk with an odd size is padded to an even boundary.
	padding := len(pcm) % 2
	out := make([]byte, headerSize+len(pcm)+padding)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(pcm)+padding))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*channels*2))
	binary.LittleEndian.PutUint16(out[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(out[34:36], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
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
