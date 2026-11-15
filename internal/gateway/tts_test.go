package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

var testSpeechAudio = []byte("ID3\x03\x00\x00\x00mock-minimax-speech")

type mockSpeechUpstream struct {
	mu         sync.Mutex
	ttsBodies  []map[string]any
	voiceCalls int
	voiceCode  int
	voiceBody  string
	ttsCode    int
	ttsBody    string
	audio      []byte
	server     *httptest.Server
}

func newMockSpeechUpstream(t *testing.T) *mockSpeechUpstream {
	t.Helper()
	m := &mockSpeechUpstream{audio: testSpeechAudio}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-key" {
			t.Errorf("unexpected upstream auth: %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/get_voice":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["voice_type"] != "system" {
				t.Errorf("voice_type = %v, want system", body["voice_type"])
			}
			m.mu.Lock()
			m.voiceCalls++
			code, raw := m.voiceCode, m.voiceBody
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if code != 0 {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, raw)
				return
			}
			_, _ = io.WriteString(w, `{"system_voice":[
				{"voice_id":"English_expressive_narrator","voice_name":"Expressive Narrator","description":["A clear English narrator."]},
				{"voice_id":"Chinese (Mandarin)_News_Anchor","voice_name":"News Anchor","description":[]},
				{"voice_id":"English_Graceful_Lady","voice_name":"Graceful Lady","description":["Calm and graceful."]}
			],"base_resp":{"status_code":0,"status_msg":"success"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/t2a_v2":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.ttsBodies = append(m.ttsBodies, body)
			code, raw := m.ttsCode, m.ttsBody
			audio := m.audio
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if code != 0 {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, raw)
				return
			}
			if raw != "" {
				_, _ = io.WriteString(w, raw)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"audio": hex.EncodeToString(audio), "status": 2},
				"extra_info": map[string]any{
					"audio_length":     1234,
					"audio_format":     "mp3",
					"audio_size":       len(audio),
					"usage_characters": 5,
				},
				"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func TestListSpeechVoices(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	catalog, err := env.gw.ListSpeechVoices(context.Background(), "speech-2.8-hd")
	if err != nil {
		t.Fatalf("list voices: %v", err)
	}
	if catalog.Source != "provider" || len(catalog.Voices) != 3 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if catalog.Voices[0].Language != "Chinese (Mandarin)" || catalog.Voices[0].VoiceID != "Chinese (Mandarin)_News_Anchor" {
		t.Fatalf("voices were not grouped and sorted: %+v", catalog.Voices)
	}
	if catalog.Voices[1].Name != "Expressive Narrator" || catalog.Voices[1].Description != "A clear English narrator." {
		t.Fatalf("voice metadata was not parsed: %+v", catalog.Voices[1])
	}
	upstream.mu.Lock()
	calls := upstream.voiceCalls
	upstream.mu.Unlock()
	if calls != 1 {
		t.Fatalf("voice calls = %d, want 1", calls)
	}
}

func TestListSpeechVoicesFallsBack(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	upstream.voiceCode = http.StatusInternalServerError
	upstream.voiceBody = `{"base_resp":{"status_code":1000,"status_msg":"internal error"}}`
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	catalog, err := env.gw.ListSpeechVoices(context.Background(), "speech-2.8-hd")
	if err != nil {
		t.Fatalf("list voices: %v", err)
	}
	if catalog.Source != "builtin" || len(catalog.Voices) == 0 {
		t.Fatalf("unexpected fallback catalog: %+v", catalog)
	}
	if !strings.Contains(catalog.Warning, "500") || !strings.Contains(catalog.Warning, "built-in") {
		t.Fatalf("unexpected warning: %q", catalog.Warning)
	}
	for i := 1; i < len(catalog.Voices); i++ {
		if catalog.Voices[i-1].Language > catalog.Voices[i].Language {
			t.Fatalf("fallback voices are not sorted: %+v", catalog.Voices)
		}
	}
}

func TestListSpeechVoicesWithoutChannel(t *testing.T) {
	env := newTestEnv(t)
	catalog, err := env.gw.ListSpeechVoices(context.Background(), "speech-2.8-hd")
	if err != nil {
		t.Fatalf("list voices: %v", err)
	}
	if catalog.Source != "builtin" || !strings.Contains(catalog.Warning, "No enabled MiniMax channel") {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
}

func TestCreateSpeech(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model:     "speech-2.8-hd",
		Text:      "Hello from the relay.",
		VoiceID:   "English_expressive_narrator",
		VoiceName: "Expressive Narrator",
		Speed:     1.25,
	})
	if err != nil {
		t.Fatalf("create speech: %v", err)
	}
	if clip.ID == 0 || clip.ChannelID != channel.ID || clip.ChannelName != "minimax" {
		t.Fatalf("unexpected clip: %+v", clip)
	}
	if clip.Model != "speech-2.8-hd" || clip.UpstreamModel != "speech-2.8-hd" ||
		clip.VoiceID != "English_expressive_narrator" || clip.VoiceName != "Expressive Narrator" {
		t.Fatalf("clip metadata was not stored: %+v", clip)
	}
	if clip.Format != "mp3" || clip.DurationMS != 1234 || clip.Characters != 5 || clip.SizeBytes != int64(len(testSpeechAudio)) {
		t.Fatalf("clip audio metadata was not stored: %+v", clip)
	}

	stored, err := env.st.GetSpeechClip(clip.ID)
	if err != nil {
		t.Fatalf("get speech clip: %v", err)
	}
	if stored.AudioPath != clip.AudioPath {
		t.Fatalf("stored path = %q, want %q", stored.AudioPath, clip.AudioPath)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, stored.AudioPath))
	if err != nil {
		t.Fatalf("read stored audio: %v", err)
	}
	if string(audio) != string(testSpeechAudio) {
		t.Fatalf("stored audio = %q", audio)
	}

	upstream.mu.Lock()
	if len(upstream.ttsBodies) != 1 {
		t.Fatalf("tts calls = %d, want 1", len(upstream.ttsBodies))
	}
	body := upstream.ttsBodies[0]
	upstream.mu.Unlock()
	if body["model"] != "speech-2.8-hd" || body["text"] != "Hello from the relay." ||
		body["stream"] != false || body["output_format"] != "hex" {
		t.Fatalf("unexpected upstream body: %+v", body)
	}
	voice, _ := body["voice_setting"].(map[string]any)
	if voice["voice_id"] != "English_expressive_narrator" || voice["speed"] != 1.25 {
		t.Fatalf("unexpected voice settings: %+v", voice)
	}
	audioSettings, _ := body["audio_setting"].(map[string]any)
	if audioSettings["format"] != "mp3" || audioSettings["sample_rate"] != float64(32000) {
		t.Fatalf("unexpected audio settings: %+v", audioSettings)
	}
}

func TestCreateSpeechUsesModelMapping(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"},
		map[string]string{"my-voice": "speech-2.8-hd"}, 0, 0)

	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "my-voice", Text: "Mapped model", VoiceID: "English_Graceful_Lady",
	})
	if err != nil {
		t.Fatalf("create speech: %v", err)
	}
	if clip.UpstreamModel != "speech-2.8-hd" {
		t.Fatalf("upstream model = %q", clip.UpstreamModel)
	}
	upstream.mu.Lock()
	body := upstream.ttsBodies[0]
	upstream.mu.Unlock()
	if body["model"] != "speech-2.8-hd" {
		t.Fatalf("forwarded model = %v", body["model"])
	}
}

func TestCreateSpeechValidatesInput(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "minimax", "https://api.example.invalid", "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	longText := strings.Repeat("a", maxSpeechText+1)
	cases := []struct {
		name string
		req  SpeechRequest
		want int
	}{
		{"empty text", SpeechRequest{Model: "speech-2.8-hd", VoiceID: "English_Graceful_Lady"}, http.StatusBadRequest},
		{"text too long", SpeechRequest{Model: "speech-2.8-hd", Text: longText, VoiceID: "English_Graceful_Lady"}, http.StatusBadRequest},
		{"empty voice", SpeechRequest{Model: "speech-2.8-hd", Text: "hello"}, http.StatusBadRequest},
		{"bad speed", SpeechRequest{Model: "speech-2.8-hd", Text: "hello", VoiceID: "English_Graceful_Lady", Speed: 0.4}, http.StatusBadRequest},
		{"unknown model", SpeechRequest{Model: "MiniMax-Unknown", Text: "hello", VoiceID: "English_Graceful_Lady"}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.gw.CreateSpeech(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected an error")
			}
			speechErr, ok := err.(*SpeechError)
			if !ok || speechErr.Status != tc.want {
				t.Fatalf("error = %v, want SpeechError status %d", err, tc.want)
			}
		})
	}
}

func TestCreateSpeechReportsUpstreamError(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	upstream.ttsCode = http.StatusBadRequest
	upstream.ttsBody = `{"base_resp":{"status_code":2013,"status_msg":"invalid voice id"}}`
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	_, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "hello", VoiceID: "missing-voice",
	})
	speechErr, ok := err.(*SpeechError)
	if !ok || speechErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400 SpeechError", err)
	}
	if !strings.Contains(speechErr.Message, "invalid voice id") {
		t.Fatalf("unexpected message: %q", speechErr.Message)
	}
}

func TestCreateSpeechRejectsInvalidAudio(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	upstream.ttsBody = `{"data":{"audio":"not-hex","status":2},"base_resp":{"status_code":0,"status_msg":"success"}}`
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	_, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "hello", VoiceID: "English_Graceful_Lady",
	})
	speechErr, ok := err.(*SpeechError)
	if !ok || speechErr.Status != http.StatusBadGateway {
		t.Fatalf("error = %v, want 502 SpeechError", err)
	}
}

func TestCreateSpeechFailsOverToNextChannel(t *testing.T) {
	env := newTestEnv(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":1000,"status_msg":"internal error"}}`)
	}))
	defer bad.Close()
	good := newMockSpeechUpstream(t)
	env.addChannel(t, "bad", bad.URL, "minimax", 10, []string{"speech-2.8-hd"}, nil, 0, 0)
	env.addChannel(t, "good", good.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "hello", VoiceID: "English_Graceful_Lady",
	})
	if err != nil {
		t.Fatalf("create speech: %v", err)
	}
	if clip.ChannelName != "good" {
		t.Fatalf("served by %q, want good", clip.ChannelName)
	}
}

func waitSpeechClip(t *testing.T, env *testEnv, id int64) *store.SpeechClip {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		clip, err := env.st.GetSpeechClip(id)
		if err != nil {
			t.Fatalf("get speech job: %v", err)
		}
		if clip.Status == "succeeded" || clip.Status == "failed" {
			return clip
		}
		if time.Now().After(deadline) {
			t.Fatalf("speech job %d did not finish; status %q, progress %d/%d", id, clip.Status, clip.DoneChunks, clip.TotalChunks)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSplitSpeechText(t *testing.T) {
	text := strings.Repeat("这是一个用于测试长文本切分的句子。", 500)
	chunks := splitSpeechText(text, speechChunkRunes)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want at least 2", len(chunks))
	}
	if strings.Join(chunks, "") != text {
		t.Fatal("chunking changed the text")
	}
	for index, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > speechChunkRunes {
			t.Fatalf("chunk %d has %d runes, want at most %d", index, utf8.RuneCountInString(chunk), speechChunkRunes)
		}
		if strings.TrimSpace(chunk) == "" {
			t.Fatalf("chunk %d is blank", index)
		}
	}

	short := "A short sentence."
	if got := splitSpeechText(short, speechChunkRunes); len(got) != 1 || got[0] != short {
		t.Fatalf("short text split into %#v", got)
	}
}

func TestTrimMP3Tags(t *testing.T) {
	tagged := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x0a"), bytes.Repeat([]byte{0}, 10)...)
	tagged = append(tagged, testSpeechAudio...)
	tagged = append(tagged, []byte("TAG")...)
	tagged = append(tagged, bytes.Repeat([]byte{'x'}, 125)...)
	if got := trimMP3Tags(tagged); !bytes.Equal(got, testSpeechAudio) {
		t.Fatalf("trimmed audio = %q, want %q", got, testSpeechAudio)
	}
	if got := trimMP3Tags(testSpeechAudio); !bytes.Equal(got, testSpeechAudio) {
		t.Fatalf("raw audio changed: %q", got)
	}
}

func TestCreateSpeechRunsLongTextAsBackgroundJob(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	text := strings.Repeat("这是一个用于测试长文本后台配音的句子。", 300)
	wantChunks := len(splitSpeechText(text, speechChunkRunes))
	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model:     "speech-2.8-hd",
		Text:      text,
		VoiceID:   "English_expressive_narrator",
		VoiceName: "Expressive Narrator",
		Speed:     1.1,
	})
	if err != nil {
		t.Fatalf("create long speech: %v", err)
	}
	if clip.Status != "queued" || clip.AudioPath != "" {
		t.Fatalf("created job = %+v, want queued without audio", clip)
	}
	if clip.TotalChunks != wantChunks || clip.Characters != int64(utf8.RuneCountInString(text)) {
		t.Fatalf("job metadata = %+v, want %d chunks", clip, wantChunks)
	}

	stored := waitSpeechClip(t, env, clip.ID)
	if stored.Status != "succeeded" {
		t.Fatalf("job failed: %s", stored.Error)
	}
	if stored.DoneChunks != wantChunks || stored.TotalChunks != wantChunks {
		t.Fatalf("progress = %d/%d, want %d/%d", stored.DoneChunks, stored.TotalChunks, wantChunks, wantChunks)
	}
	if stored.AudioPath == "" || stored.SizeBytes == 0 || stored.DurationMS != int64(1234*wantChunks) {
		t.Fatalf("unexpected finished job: %+v", stored)
	}
	if stored.Speed != 1.1 || stored.VoiceName != "Expressive Narrator" {
		t.Fatalf("job settings were not preserved: %+v", stored)
	}

	upstream.mu.Lock()
	bodies := append([]map[string]any(nil), upstream.ttsBodies...)
	upstream.mu.Unlock()
	if len(bodies) != wantChunks {
		t.Fatalf("tts calls = %d, want %d", len(bodies), wantChunks)
	}
	for index, body := range bodies {
		chunk, _ := body["text"].(string)
		if utf8.RuneCountInString(chunk) > speechChunkRunes || strings.TrimSpace(chunk) == "" {
			t.Fatalf("upstream chunk %d is invalid: %q", index, chunk)
		}
	}

	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, stored.AudioPath))
	if err != nil {
		t.Fatalf("read merged audio: %v", err)
	}
	wantAudio := bytes.Repeat(trimMP3Tags(testSpeechAudio), wantChunks)
	if !bytes.Equal(audio, wantAudio) {
		t.Fatalf("merged audio length = %d, want %d", len(audio), len(wantAudio))
	}
}

func TestSpeechJobFailureIsPersisted(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	upstream.ttsBody = `{"base_resp":{"status_code":2013,"status_msg":"the text was rejected"}}`
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: strings.Repeat("长文本。", 1000), VoiceID: "English_Graceful_Lady",
	})
	if err != nil {
		t.Fatalf("create long speech: %v", err)
	}
	stored := waitSpeechClip(t, env, clip.ID)
	if stored.Status != "failed" || !strings.Contains(stored.Error, "the text was rejected") {
		t.Fatalf("failed job = %+v", stored)
	}
	if stored.AudioPath != "" {
		t.Fatalf("failed job kept an audio path: %q", stored.AudioPath)
	}
}

func TestSpeechJobResumesCompletedSegments(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	text := strings.Repeat("这是一个用于测试断点续传的句子。", 200)
	chunks := splitSpeechText(text, speechChunkRunes)
	if len(chunks) < 2 {
		t.Fatalf("test needs at least two chunks, got %d", len(chunks))
	}
	segment := trimMP3Tags(testSpeechAudio)
	clip := &store.SpeechClip{
		ChannelID:     channel.ID,
		ChannelName:   channel.Name,
		Model:         "speech-2.8-hd",
		UpstreamModel: "speech-2.8-hd",
		VoiceID:       "English_Graceful_Lady",
		VoiceName:     "Graceful Lady",
		Text:          text,
		Speed:         1,
		Status:        "running",
		TotalChunks:   len(chunks),
		DoneChunks:    1,
		Characters:    int64(utf8.RuneCountInString(text)),
		DurationMS:    1234,
		SizeBytes:     int64(len(segment)),
	}
	if err := env.st.CreateSpeechClip(clip); err != nil {
		t.Fatalf("store speech job: %v", err)
	}
	dir := env.gw.speechJobDir(clip.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create job dir: %v", err)
	}
	if err := os.WriteFile(speechSegmentPath(dir, 0), segment, 0o600); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	if err := writeSpeechDurations(dir, []int64{1234}); err != nil {
		t.Fatalf("write durations: %v", err)
	}

	env.gw.runSpeechJob(clip.ID)
	stored := waitSpeechClip(t, env, clip.ID)
	if stored.Status != "succeeded" {
		t.Fatalf("resumed job failed: %s", stored.Error)
	}
	if stored.DoneChunks != len(chunks) || stored.DurationMS != int64(1234*len(chunks)) {
		t.Fatalf("resumed progress = %+v", stored)
	}

	upstream.mu.Lock()
	calls := len(upstream.ttsBodies)
	upstream.mu.Unlock()
	if calls != len(chunks)-1 {
		t.Fatalf("tts calls = %d, want %d (one completed segment reused)", calls, len(chunks)-1)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, stored.AudioPath))
	if err != nil {
		t.Fatalf("read merged audio: %v", err)
	}
	if !bytes.Equal(audio, bytes.Repeat(segment, len(chunks))) {
		t.Fatalf("merged resumed audio is invalid (%d bytes)", len(audio))
	}
}
