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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

const (
	// maxSpeechText is the combined limit for one clip, long enough for an
	// article or a short book chapter.
	maxSpeechText    = 50000
	maxSpeechVoiceID = 200
	// speechSyncLimit is the largest text synthesized while the console waits.
	// Longer text becomes a background job so a refresh (or a closed tab) does
	// not interrupt it.
	speechSyncLimit   = 2000
	speechChunkRunes  = 3000
	maxSpeechResponse = 96 << 20
	maxSpeechAudio    = 48 << 20
	// speechRequestTimeout bounds one upstream synthesis request. Long-form
	// speech is generated before the response starts, so it needs more time
	// than a normal chat request.
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

// speechSynthesis is the decoded result of one upstream t2a_v2 request.
type speechSynthesis struct {
	channel    store.Channel
	upstream   string
	audio      []byte
	format     string
	durationMS int64
	characters int64
}

// CreateSpeech validates a request and either synthesizes it immediately or,
// for long text, stores a background job and returns it while synthesis runs.
func (g *Gateway) CreateSpeech(ctx context.Context, req SpeechRequest) (*store.SpeechClip, error) {
	if err := normalizeSpeechRequest(&req); err != nil {
		return nil, &SpeechError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	candidates, err := g.speechCandidates(req.Model)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, &SpeechError{
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("model %q is not available on any enabled MiniMax channel", req.Model),
		}
	}
	if utf8.RuneCountInString(req.Text) > speechSyncLimit {
		return g.createSpeechJob(&req, candidates)
	}

	ctx, cancel := context.WithTimeout(ctx, speechRequestTimeout)
	defer cancel()
	result, err := g.synthesizeSpeech(ctx, candidates, req, req.Text)
	if err != nil {
		return nil, err
	}
	return g.storeSpeechClip(result, req)
}

// createSpeechJob persists a long-text job before synthesis starts, so the
// console can list it immediately and find it again after a refresh.
func (g *Gateway) createSpeechJob(req *SpeechRequest, candidates []candidate) (*store.SpeechClip, error) {
	clip := &store.SpeechClip{
		ChannelID:     candidates[0].channel.ID,
		ChannelName:   candidates[0].channel.Name,
		Model:         req.Model,
		UpstreamModel: candidates[0].upstream,
		VoiceID:       req.VoiceID,
		VoiceName:     speechVoiceName(req),
		Text:          req.Text,
		Speed:         req.Speed,
		Status:        "queued",
		TotalChunks:   len(splitSpeechText(req.Text, speechChunkRunes)),
		Characters:    int64(utf8.RuneCountInString(req.Text)),
	}
	if err := g.store.CreateSpeechClip(clip); err != nil {
		return nil, err
	}
	go g.runSpeechJob(clip.ID)
	return clip, nil
}

// speechCandidates lists the enabled MiniMax channels that can serve a model.
func (g *Gateway) speechCandidates(model string) ([]candidate, error) {
	channels, err := g.store.ListChannels()
	if err != nil {
		return nil, err
	}
	out := make([]candidate, 0)
	for _, cand := range orderCandidates(channels, model) {
		if cand.channel.Type == "minimax" {
			out = append(out, cand)
		}
	}
	return out, nil
}

// synthesizeSpeech tries each candidate until one returns usable audio. A
// provider rejection that another channel cannot fix is returned immediately.
func (g *Gateway) synthesizeSpeech(ctx context.Context, candidates []candidate, req SpeechRequest, text string) (*speechSynthesis, error) {
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
			"text":          text,
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
		return &speechSynthesis{
			channel:    ch,
			upstream:   cand.upstream,
			audio:      audio,
			format:     format,
			durationMS: payload.ExtraInfo.AudioLength,
			characters: payload.ExtraInfo.UsageCharacters,
		}, nil
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

func (g *Gateway) storeSpeechClip(result *speechSynthesis, req SpeechRequest) (*store.SpeechClip, error) {
	dir := filepath.Join(g.cfg.DataDir, "speech")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, "speech-*."+result.format)
	if err != nil {
		return nil, err
	}
	name := file.Name()
	written, writeErr := file.Write(result.audio)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		if writeErr != nil {
			return nil, writeErr
		}
		return nil, closeErr
	}
	if written != len(result.audio) {
		_ = os.Remove(name)
		return nil, errors.New("could not write the generated audio")
	}
	rel, err := filepath.Rel(g.cfg.DataDir, name)
	if err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	clip := &store.SpeechClip{
		ChannelID:     result.channel.ID,
		ChannelName:   result.channel.Name,
		Model:         req.Model,
		UpstreamModel: result.upstream,
		VoiceID:       req.VoiceID,
		VoiceName:     speechVoiceName(&req),
		Text:          req.Text,
		Speed:         req.Speed,
		Status:        "succeeded",
		Format:        result.format,
		AudioPath:     rel,
		DurationMS:    result.durationMS,
		Characters:    result.characters,
		SizeBytes:     int64(len(result.audio)),
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

// speechVoiceName falls back to the voice id when the console did not send a
// display name.
func speechVoiceName(req *SpeechRequest) string {
	voiceName := truncate(req.VoiceName, 200)
	if voiceName == "" {
		voiceName = req.VoiceID
	}
	return voiceName
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

// ResumeSpeechJobs continues background speech jobs that were queued or
// running when the gateway stopped. Finished chunks are reused, so a restart
// does not synthesize the same text twice.
func (g *Gateway) ResumeSpeechJobs() {
	jobs, err := g.store.ListResumableSpeechClips()
	if err != nil {
		g.logger.Error("list resumable speech jobs failed", "error", err)
		return
	}
	for i := range jobs {
		go g.runSpeechJob(jobs[i].ID)
	}
	if len(jobs) > 0 {
		g.logger.Info("resumed speech jobs", "count", len(jobs))
	}
}

// runSpeechJob synthesizes one stored long-text job chunk by chunk. Everything
// it needs is persisted, so the console can close, refresh, or reconnect while
// the job keeps running.
func (g *Gateway) runSpeechJob(id int64) {
	if _, loaded := g.speechJobs.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	defer g.speechJobs.Delete(id)

	clip, err := g.store.GetSpeechClip(id)
	if err != nil {
		g.logger.Error("load speech job failed", "job", id, "error", err)
		return
	}
	if clip.Status != "queued" && clip.Status != "running" {
		return
	}
	chunks := splitSpeechText(clip.Text, speechChunkRunes)
	if len(chunks) == 0 {
		g.failSpeechJob(clip, "the job has no text to synthesize")
		return
	}
	clip.TotalChunks = len(chunks)
	if clip.Speed == 0 {
		clip.Speed = 1
	}

	dir := g.speechJobDir(id)
	durations := make([]int64, 0, len(chunks))
	resume := clip.DoneChunks > 0 && clip.DoneChunks <= len(chunks)
	if resume {
		stored, ok := readSpeechDurations(dir, clip.DoneChunks)
		if ok && speechSegmentsReady(dir, clip.DoneChunks) {
			durations = stored
			clip.SizeBytes = speechSegmentsSize(dir, clip.DoneChunks)
			clip.DurationMS = 0
			for _, duration := range durations {
				clip.DurationMS += duration
			}
		} else {
			// The segments are missing or incomplete; synthesize from the start
			// rather than risk assembling truncated audio.
			resume = false
		}
	}
	if !resume {
		clip.DoneChunks = 0
		clip.DurationMS = 0
		clip.SizeBytes = 0
	}
	if clip.DoneChunks == 0 {
		if err := os.RemoveAll(dir); err != nil {
			g.logger.Error("reset speech job storage failed", "job", id, "error", err)
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		g.failSpeechJob(clip, "could not prepare speech storage: "+err.Error())
		return
	}

	clip.Status = "running"
	clip.Error = ""
	if err := g.store.UpdateSpeechClip(clip); err != nil {
		g.logger.Error("update speech job failed", "job", id, "error", err)
		return
	}

	candidates, err := g.speechCandidates(clip.Model)
	if err != nil {
		g.failSpeechJob(clip, "could not load channels: "+err.Error())
		return
	}
	if len(candidates) == 0 {
		g.failSpeechJob(clip, fmt.Sprintf("model %q is not available on any enabled MiniMax channel", clip.Model))
		return
	}
	req := SpeechRequest{
		Model:     clip.Model,
		Text:      clip.Text,
		VoiceID:   clip.VoiceID,
		VoiceName: clip.VoiceName,
		Speed:     clip.Speed,
	}

	for index := clip.DoneChunks; index < len(chunks); index++ {
		ctx, cancel := context.WithTimeout(context.Background(), speechRequestTimeout)
		result, synthErr := g.synthesizeSpeech(ctx, candidates, req, chunks[index])
		cancel()
		if synthErr != nil {
			message := synthErr.Error()
			var speechErr *SpeechError
			if errors.As(synthErr, &speechErr) {
				message = speechErr.Message
			}
			g.failSpeechJob(clip, message)
			_ = os.RemoveAll(dir)
			return
		}
		if result.format != "mp3" {
			g.failSpeechJob(clip, fmt.Sprintf("long-form synthesis needs MP3 audio, but the provider returned %s", result.format))
			_ = os.RemoveAll(dir)
			return
		}
		audio := trimMP3Tags(result.audio)
		if err := writeSpeechSegment(dir, index, audio); err != nil {
			g.failSpeechJob(clip, "could not store a generated segment: "+err.Error())
			_ = os.RemoveAll(dir)
			return
		}
		durations = append(durations, result.durationMS)
		if err := writeSpeechDurations(dir, durations); err != nil {
			g.logger.Error("store speech segment durations failed", "job", id, "error", err)
		}
		clip.DoneChunks = index + 1
		clip.DurationMS += result.durationMS
		clip.SizeBytes += int64(len(audio))
		clip.ChannelID = result.channel.ID
		clip.ChannelName = result.channel.Name
		clip.UpstreamModel = result.upstream
		if err := g.store.UpdateSpeechClip(clip); err != nil {
			g.logger.Error("update speech job progress failed", "job", id, "error", err)
		}
	}

	rel, size, err := g.mergeSpeechSegments(dir, len(chunks))
	if err != nil {
		g.failSpeechJob(clip, "could not assemble the generated audio: "+err.Error())
		_ = os.RemoveAll(dir)
		return
	}
	clip.AudioPath = rel
	clip.SizeBytes = size
	clip.Format = "mp3"
	clip.Status = "succeeded"
	clip.Error = ""
	if err := g.store.UpdateSpeechClip(clip); err != nil {
		g.logger.Error("finish speech job failed", "job", id, "error", err)
		_ = os.Remove(filepath.Join(g.cfg.DataDir, rel))
		return
	}
	_ = os.RemoveAll(dir)
}

func (g *Gateway) speechJobDir(id int64) string {
	return filepath.Join(g.cfg.DataDir, "speech", "jobs", strconv.FormatInt(id, 10))
}

func (g *Gateway) failSpeechJob(clip *store.SpeechClip, message string) {
	clip.Status = "failed"
	clip.Error = truncate(message, 1000)
	if err := g.store.UpdateSpeechClip(clip); err != nil {
		g.logger.Error("mark speech job failed", "job", clip.ID, "error", err)
	}
}

func speechSegmentPath(dir string, index int) string {
	return filepath.Join(dir, fmt.Sprintf("segment-%06d.mp3", index))
}

func speechSegmentsReady(dir string, count int) bool {
	for index := 0; index < count; index++ {
		info, err := os.Stat(speechSegmentPath(dir, index))
		if err != nil || info.IsDir() || info.Size() == 0 {
			return false
		}
	}
	return true
}

func speechSegmentsSize(dir string, count int) int64 {
	var total int64
	for index := 0; index < count; index++ {
		if info, err := os.Stat(speechSegmentPath(dir, index)); err == nil {
			total += info.Size()
		}
	}
	return total
}

func writeSpeechSegment(dir string, index int, audio []byte) error {
	file, err := os.CreateTemp(dir, "segment-*.tmp")
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
		return errors.New("short write")
	}
	return os.Rename(name, speechSegmentPath(dir, index))
}

func writeSpeechDurations(dir string, durations []int64) error {
	data, err := json.Marshal(durations)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "durations.json"), data)
}

func readSpeechDurations(dir string, want int) ([]int64, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "durations.json"))
	if err != nil {
		return nil, false
	}
	var durations []int64
	if err := json.Unmarshal(data, &durations); err != nil || len(durations) != want {
		return nil, false
	}
	return durations, true
}

func writeFileAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	written, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if written != len(data) {
		_ = os.Remove(name)
		return errors.New("short write")
	}
	return os.Rename(name, path)
}

// mergeSpeechSegments concatenates the per-chunk MP3 files into one playable
// clip and returns its path relative to DATA_DIR.
func (g *Gateway) mergeSpeechSegments(dir string, count int) (string, int64, error) {
	outDir := filepath.Join(g.cfg.DataDir, "speech")
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return "", 0, err
	}
	out, err := os.CreateTemp(outDir, "speech-long-*.mp3")
	if err != nil {
		return "", 0, err
	}
	name := out.Name()
	var total int64
	failed := true
	defer func() {
		if failed {
			_ = os.Remove(name)
		}
	}()
	for index := 0; index < count; index++ {
		segment, err := os.Open(speechSegmentPath(dir, index))
		if err != nil {
			_ = out.Close()
			return "", 0, err
		}
		written, copyErr := io.Copy(out, segment)
		_ = segment.Close()
		if copyErr != nil {
			_ = out.Close()
			return "", 0, copyErr
		}
		total += written
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	rel, err := filepath.Rel(g.cfg.DataDir, name)
	if err != nil {
		return "", 0, err
	}
	failed = false
	return rel, total, nil
}

// splitSpeechText divides long text into provider-sized chunks, preferring
// paragraph, sentence and clause boundaries over a hard cut.
func splitSpeechText(text string, limit int) []string {
	if limit < 1 {
		return []string{text}
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}
	chunks := make([]string, 0, len(runes)/limit+1)
	for len(runes) > limit {
		cut := speechSplitPoint(runes, limit)
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}

func speechSplitPoint(runes []rune, limit int) int {
	min := limit / 2
	if min < 1 {
		min = 1
	}
	for class := 4; class >= 1; class-- {
		for index := limit; index >= min; index-- {
			if speechBreakClass(runes[index-1]) == class {
				return index
			}
		}
	}
	return limit
}

func speechBreakClass(r rune) int {
	switch r {
	case '\n':
		return 4
	case '。', '！', '？', '!', '?', '.', '；', ';':
		return 3
	case '，', ',', '、', '：', ':':
		return 2
	case ' ', '\t', '\r':
		return 1
	}
	return 0
}

// trimMP3Tags removes ID3 metadata so segments can be concatenated as a clean
// MPEG frame stream. MiniMax may wrap each chunk in a tag; without this the
// merged clip would contain metadata blocks between audio frames.
func trimMP3Tags(audio []byte) []byte {
	if len(audio) >= 10 && string(audio[:3]) == "ID3" {
		size := int(audio[6]&0x7f)<<21 | int(audio[7]&0x7f)<<14 | int(audio[8]&0x7f)<<7 | int(audio[9]&0x7f)
		end := 10 + size
		if end <= len(audio) {
			audio = audio[end:]
		}
	}
	if len(audio) >= 128 && string(audio[len(audio)-128:len(audio)-125]) == "TAG" {
		audio = audio[:len(audio)-128]
	}
	return audio
}
