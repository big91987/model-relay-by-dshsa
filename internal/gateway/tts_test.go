package gateway

import (
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
