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
	"testing"
)

var testVoiceDesignAudio = []byte("ID3\x03\x00\x00\x00mock-voice-design-preview")

type mockVoiceDesignUpstream struct {
	designBodies []map[string]any
	speechBodies []map[string]any
	designCode   int
	designBody   string
	speechCode   int
	audio        []byte
	voiceID      string
	server       *httptest.Server
}

func newMockVoiceDesignUpstream(t *testing.T) *mockVoiceDesignUpstream {
	t.Helper()
	m := &mockVoiceDesignUpstream{
		audio:   testVoiceDesignAudio,
		voiceID: "ttv-voice-2026010100000000-mock0001",
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-key" {
			t.Errorf("unexpected upstream auth: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/voice_design":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.designBodies = append(m.designBodies, body)
			if m.designCode != 0 {
				w.WriteHeader(m.designCode)
			}
			if m.designBody != "" {
				_, _ = io.WriteString(w, m.designBody)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"voice_id":    m.voiceID,
				"trial_audio": hex.EncodeToString(m.audio),
				"base_resp":   map[string]any{"status_code": 0, "status_msg": "success"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/t2a_v2":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.speechBodies = append(m.speechBodies, body)
			if m.speechCode != 0 {
				w.WriteHeader(m.speechCode)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"audio": hex.EncodeToString(m.audio), "status": 2},
				"extra_info": map[string]any{
					"audio_length":     1500,
					"audio_format":     "mp3",
					"audio_size":       len(m.audio),
					"usage_characters": 7,
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

func TestCreateVoiceDesign(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVoiceDesignUpstream(t)
	channel := env.addChannel(t, "minimax-voice", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	design, err := env.gw.CreateVoiceDesign(context.Background(), VoiceDesignRequest{
		Model:       "speech-2.8-hd",
		Prompt:      "A warm, calm female narrator with a gentle delivery.",
		PreviewText: "Hello, this is the preview.",
	})
	if err != nil {
		t.Fatalf("create voice design: %v", err)
	}
	if len(upstream.designBodies) != 1 {
		t.Fatalf("design calls = %d, want 1", len(upstream.designBodies))
	}
	body := upstream.designBodies[0]
	if body["prompt"] != "A warm, calm female narrator with a gentle delivery." ||
		body["preview_text"] != "Hello, this is the preview." {
		t.Fatalf("unexpected design request: %+v", body)
	}
	if _, ok := body["voice_id"]; ok {
		t.Fatalf("voice_id should be omitted when not requested: %+v", body)
	}
	if design.VoiceID != upstream.voiceID || design.ChannelID != channel.ID || design.ChannelName != "minimax-voice" {
		t.Fatalf("unexpected design: %+v", design)
	}
	if design.Model != "speech-2.8-hd" || design.PreviewFormat != "mp3" {
		t.Fatalf("unexpected design metadata: %+v", design)
	}
	if design.Name != "A warm, calm female narrator with a gentle delivery." {
		t.Fatalf("design name = %q", design.Name)
	}
	if design.PreviewCharacters != int64(len("Hello, this is the preview.")) || design.PreviewSizeBytes != int64(len(upstream.audio)) {
		t.Fatalf("unexpected preview metadata: %+v", design)
	}

	path := filepath.Join(env.gw.cfg.DataDir, design.PreviewPath)
	audio, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stored preview: %v", err)
	}
	if string(audio) != string(upstream.audio) {
		t.Fatalf("stored preview does not match the provider audio")
	}

	stored, err := env.st.GetVoiceDesign(design.ID)
	if err != nil {
		t.Fatalf("load stored design: %v", err)
	}
	if stored.VoiceID != design.VoiceID || stored.Prompt != design.Prompt {
		t.Fatalf("unexpected stored design: %+v", stored)
	}
	designs, err := env.st.ListVoiceDesigns(10)
	if err != nil || len(designs) != 1 {
		t.Fatalf("list designs = %d, %v", len(designs), err)
	}
}

func TestCreateVoiceDesignValidation(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVoiceDesignUpstream(t)
	env.addChannel(t, "minimax-voice", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	cases := []struct {
		name string
		req  VoiceDesignRequest
		want string
	}{
		{
			name: "missing model",
			req:  VoiceDesignRequest{Prompt: "warm", PreviewText: "hello"},
			want: "model is required",
		},
		{
			name: "missing prompt",
			req:  VoiceDesignRequest{Model: "speech-2.8-hd", PreviewText: "hello"},
			want: "prompt is required",
		},
		{
			name: "missing preview text",
			req:  VoiceDesignRequest{Model: "speech-2.8-hd", Prompt: "warm"},
			want: "preview_text is required",
		},
		{
			name: "preview text too long",
			req: VoiceDesignRequest{
				Model:       "speech-2.8-hd",
				Prompt:      "warm",
				PreviewText: strings.Repeat("x", maxVoiceDesignPreviewText+1),
			},
			want: "preview_text must be at most 500 characters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.gw.CreateVoiceDesign(context.Background(), tc.req)
			speechErr, ok := err.(*SpeechError)
			if !ok {
				t.Fatalf("error = %v, want SpeechError", err)
			}
			if speechErr.Status != http.StatusBadRequest || speechErr.Message != tc.want {
				t.Fatalf("error = %d %q, want 400 %q", speechErr.Status, speechErr.Message, tc.want)
			}
		})
	}
	if len(upstream.designBodies) != 0 {
		t.Fatalf("validation failures should not reach the provider, got %d calls", len(upstream.designBodies))
	}
}

func TestCreateVoiceDesignUpstreamError(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVoiceDesignUpstream(t)
	upstream.designBody = `{"base_resp":{"status_code":2013,"status_msg":"prompt rejected"}}`
	env.addChannel(t, "minimax-voice", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	_, err := env.gw.CreateVoiceDesign(context.Background(), VoiceDesignRequest{
		Model:       "speech-2.8-hd",
		Prompt:      "warm",
		PreviewText: "hello",
	})
	speechErr, ok := err.(*SpeechError)
	if !ok || speechErr.Status != http.StatusBadRequest || speechErr.Message != "prompt rejected" {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestCreateVoiceDesignFailover(t *testing.T) {
	env := newTestEnv(t)
	failing := newMockVoiceDesignUpstream(t)
	failing.designCode = http.StatusBadGateway
	failing.designBody = `{"base_resp":{"status_code":1013,"status_msg":"upstream unavailable"}}`
	healthy := newMockVoiceDesignUpstream(t)
	env.addChannel(t, "minimax-failing", failing.server.URL, "minimax", 10, []string{"speech-2.8-hd"}, nil, 0, 0)
	healthyChannel := env.addChannel(t, "minimax-healthy", healthy.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	design, err := env.gw.CreateVoiceDesign(context.Background(), VoiceDesignRequest{
		Model:       "speech-2.8-hd",
		Prompt:      "warm",
		PreviewText: "hello",
	})
	if err != nil {
		t.Fatalf("create voice design with failover: %v", err)
	}
	if design.ChannelID != healthyChannel.ID || design.VoiceID != healthy.voiceID {
		t.Fatalf("design did not fail over: %+v", design)
	}
}

func TestDesignVoiceSpeechPinnedToChannel(t *testing.T) {
	env := newTestEnv(t)
	owner := newMockVoiceDesignUpstream(t)
	other := newMockVoiceDesignUpstream(t)
	ownerChannel := env.addChannel(t, "minimax-owner", owner.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	design, err := env.gw.CreateVoiceDesign(context.Background(), VoiceDesignRequest{
		Model:       "speech-2.8-hd",
		Prompt:      "warm",
		PreviewText: "hello",
	})
	if err != nil {
		t.Fatalf("create voice design: %v", err)
	}
	if design.ChannelID != ownerChannel.ID {
		t.Fatalf("design channel = %d, want %d", design.ChannelID, ownerChannel.ID)
	}
	// A higher-priority channel that serves the same model must not take the
	// speech request: the designed voice only exists on the owning account.
	env.addChannel(t, "minimax-other", other.server.URL, "minimax", 10, []string{"speech-2.8-hd"}, nil, 0, 0)

	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model:     design.Model,
		Text:      "another sentence",
		VoiceID:   design.VoiceID,
		VoiceName: design.Name,
		Speed:     1,
		ChannelID: design.ChannelID,
	})
	if err != nil {
		t.Fatalf("synthesize designed voice: %v", err)
	}
	if clip.VoiceID != design.VoiceID || clip.VoiceName != design.Name {
		t.Fatalf("unexpected clip: %+v", clip)
	}
	if len(other.speechBodies) != 0 {
		t.Fatalf("higher-priority channel was used despite the pin")
	}
	if len(owner.speechBodies) != 1 {
		t.Fatalf("owner channel calls = %d, want 1", len(owner.speechBodies))
	}
	if got := owner.speechBodies[0]["voice_setting"].(map[string]any)["voice_id"]; got != design.VoiceID {
		t.Fatalf("voice id sent upstream = %v, want %s", got, design.VoiceID)
	}
}

func TestVoiceDesignName(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{
			name:   "short prompt is kept",
			prompt: "A calm narrator.",
			want:   "A calm narrator.",
		},
		{
			name:   "long prompt is shortened at a word boundary",
			prompt: "A warm, calm female Mandarin narrator in her thirties, gentle and unhurried, with a hint of curiosity.",
			want:   "A warm, calm female Mandarin narrator in her thirties…",
		},
		{
			name:   "whitespace is collapsed",
			prompt: "  calm   narrator  ",
			want:   "calm narrator",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := voiceDesignName(tc.prompt, "ttv-voice-1"); got != tc.want {
				t.Fatalf("name = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectSpeechAudioFormat(t *testing.T) {
	cases := []struct {
		name  string
		audio []byte
		want  string
	}{
		{"mp3 id3", []byte("ID3\x03\x00\x00"), "mp3"},
		{"mp3 frame", []byte{0xFF, 0xFB, 0x90, 0x64}, "mp3"},
		{"flac", []byte("fLaC\x00\x00\x00"), "flac"},
		{"wav", append([]byte("RIFF\x00\x00\x00\x00WAVE"), 0), "wav"},
		{"unknown", []byte{0, 1, 2}, "mp3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectSpeechAudioFormat(tc.audio); got != tc.want {
				t.Fatalf("format = %q, want %q", got, tc.want)
			}
		})
	}
}
