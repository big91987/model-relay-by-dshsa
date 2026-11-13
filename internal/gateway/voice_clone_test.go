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

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

var testVoiceSample = []byte("ID3\x03\x00\x00\x00mock-authorized-voice-sample")

type mockVoiceCloneUpstream struct {
	mu sync.Mutex

	uploadPurpose  string
	uploadFilename string
	uploadBytes    []byte
	uploadCode     int
	uploadBody     string

	cloneBody    map[string]any
	cloneCode    int
	cloneBodyRaw string
	noPreview    bool

	t2aBodies []map[string]any
	t2aCode   int
	t2aBody   string

	previewAudio []byte
	previewCode  int

	server *httptest.Server
}

func newMockVoiceCloneUpstream(t *testing.T) *mockVoiceCloneUpstream {
	t.Helper()
	m := &mockVoiceCloneUpstream{
		previewAudio: testSpeechAudio,
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/files/upload":
			if r.Header.Get("Authorization") != "Bearer upstream-key" {
				t.Errorf("unexpected upload auth: %q", r.Header.Get("Authorization"))
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse upload: %v", err)
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			purpose := r.FormValue("purpose")
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Errorf("upload file: %v", err)
				http.Error(w, "missing file", http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(file)
			file.Close()
			m.mu.Lock()
			m.uploadPurpose = purpose
			m.uploadFilename = header.Filename
			m.uploadBytes = data
			code, raw := m.uploadCode, m.uploadBody
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if code != 0 {
				w.WriteHeader(code)
			}
			if raw != "" {
				_, _ = io.WriteString(w, raw)
				return
			}
			_, _ = io.WriteString(w, `{"file":{"file_id":424242,"bytes":319,"filename":"sample.mp3","purpose":"voice_clone"},"base_resp":{"status_code":0,"status_msg":"success"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/voice_clone":
			if r.Header.Get("Authorization") != "Bearer upstream-key" {
				t.Errorf("unexpected clone auth: %q", r.Header.Get("Authorization"))
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.cloneBody = body
			code, raw := m.cloneCode, m.cloneBodyRaw
			noPreview := m.noPreview
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if code != 0 {
				w.WriteHeader(code)
			}
			if raw != "" {
				_, _ = io.WriteString(w, raw)
				return
			}
			demo := m.server.URL + "/preview.mp3"
			if noPreview {
				demo = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"input_sensitive": false,
				"demo_audio":      demo,
				"extra_info": map[string]any{
					"audio_length":     2000,
					"usage_characters": 12,
				},
				"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/preview.mp3":
			m.mu.Lock()
			code, audio := m.previewCode, m.previewAudio
			m.mu.Unlock()
			if code != 0 {
				w.WriteHeader(code)
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(audio)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/t2a_v2":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.t2aBodies = append(m.t2aBodies, body)
			code, raw := m.t2aCode, m.t2aBody
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if code != 0 {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, raw)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"audio": hex.EncodeToString(m.previewAudio), "status": 2},
				"extra_info": map[string]any{
					"audio_length":     2000,
					"audio_format":     "mp3",
					"usage_characters": 12,
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

func TestCreateVoiceClone(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVoiceCloneUpstream(t)
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	previewText := "This is what the cloned voice sounds like."
	clone, warning, err := env.gw.CreateVoiceClone(context.Background(), VoiceCloneRequest{
		Name:        "Narrator",
		FileName:    "sample.mp3",
		Sample:      testVoiceSample,
		Model:       "speech-2.8-hd",
		PreviewText: previewText,
		Authorized:  true,
	})
	if err != nil {
		t.Fatalf("create voice clone: %v", err)
	}
	if warning != "" {
		t.Fatalf("unexpected warning: %q", warning)
	}
	if clone.ID == 0 || clone.ChannelID != channel.ID || clone.ChannelName != "minimax" {
		t.Fatalf("unexpected clone: %+v", clone)
	}
	if !strings.HasPrefix(clone.VoiceID, "clone_") || len(clone.VoiceID) < 8 {
		t.Fatalf("invalid voice id: %q", clone.VoiceID)
	}
	if clone.Name != "Narrator" || clone.Model != "speech-2.8-hd" || clone.UpstreamModel != "speech-2.8-hd" {
		t.Fatalf("clone metadata: %+v", clone)
	}
	if clone.SampleFormat != "mp3" || clone.SampleBytes != int64(len(testVoiceSample)) || !clone.Authorized {
		t.Fatalf("sample metadata: %+v", clone)
	}
	if clone.PreviewText != previewText || clone.PreviewFormat != "mp3" || clone.PreviewDurationMS != 2000 {
		t.Fatalf("preview metadata: %+v", clone)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, clone.PreviewPath))
	if err != nil {
		t.Fatalf("read stored preview: %v", err)
	}
	if string(audio) != string(testSpeechAudio) {
		t.Fatalf("stored preview = %q", audio)
	}
	stored, err := env.st.GetVoiceCloneByVoiceID(clone.VoiceID)
	if err != nil {
		t.Fatalf("look up clone: %v", err)
	}
	if stored.ID != clone.ID || stored.PreviewPath != clone.PreviewPath {
		t.Fatalf("stored clone = %+v", stored)
	}

	upstream.mu.Lock()
	purpose, filename, sample := upstream.uploadPurpose, upstream.uploadFilename, upstream.uploadBytes
	body := upstream.cloneBody
	upstream.mu.Unlock()
	if purpose != "voice_clone" || filename != "sample.mp3" || string(sample) != string(testVoiceSample) {
		t.Fatalf("upload purpose=%q filename=%q bytes=%q", purpose, filename, sample)
	}
	if body["file_id"] != float64(424242) || body["voice_id"] != clone.VoiceID {
		t.Fatalf("clone body = %+v", body)
	}
	if body["text"] != previewText || body["model"] != "speech-2.8-hd" {
		t.Fatalf("clone body = %+v", body)
	}
}

func TestCreateVoiceCloneValidatesInput(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "minimax", "https://api.example.invalid", "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	valid := VoiceCloneRequest{
		Name:        "Narrator",
		FileName:    "sample.mp3",
		Sample:      testVoiceSample,
		Model:       "speech-2.8-hd",
		PreviewText: "Preview",
		Authorized:  true,
	}
	cases := []struct {
		name string
		edit func(*VoiceCloneRequest)
	}{
		{"unauthorized", func(req *VoiceCloneRequest) { req.Authorized = false }},
		{"no name", func(req *VoiceCloneRequest) { req.Name = "  " }},
		{"no model", func(req *VoiceCloneRequest) { req.Model = "" }},
		{"no preview text", func(req *VoiceCloneRequest) { req.PreviewText = "" }},
		{"too long preview", func(req *VoiceCloneRequest) { req.PreviewText = strings.Repeat("a", MaxVoicePreviewText+1) }},
		{"no sample", func(req *VoiceCloneRequest) { req.Sample = nil }},
		{"too large sample", func(req *VoiceCloneRequest) {
			req.Sample = make([]byte, MaxVoiceSampleBytes+1)
			copy(req.Sample, []byte("ID3"))
		}},
		{"unsupported extension", func(req *VoiceCloneRequest) { req.FileName = "sample.ogg" }},
		{"content mismatch", func(req *VoiceCloneRequest) {
			req.FileName = "sample.wav"
			req.Sample = testVoiceSample
		}},
		{"not audio", func(req *VoiceCloneRequest) { req.Sample = []byte("plain text") }},
		{"long name", func(req *VoiceCloneRequest) { req.Name = strings.Repeat("n", MaxVoiceCloneName+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := valid
			tc.edit(&req)
			_, _, err := env.gw.CreateVoiceClone(context.Background(), req)
			if err == nil {
				t.Fatal("expected an error")
			}
			speechErr, ok := err.(*SpeechError)
			if !ok || speechErr.Status != http.StatusBadRequest {
				t.Fatalf("error = %v, want 400 SpeechError", err)
			}
		})
	}

	t.Run("unknown model", func(t *testing.T) {
		req := valid
		req.Model = "MiniMax-Unknown"
		_, _, err := env.gw.CreateVoiceClone(context.Background(), req)
		speechErr, ok := err.(*SpeechError)
		if !ok || speechErr.Status != http.StatusNotFound {
			t.Fatalf("error = %v, want 404 SpeechError", err)
		}
	})

	t.Run("no channel", func(t *testing.T) {
		emptyEnv := newTestEnv(t)
		_, _, err := emptyEnv.gw.CreateVoiceClone(context.Background(), valid)
		speechErr, ok := err.(*SpeechError)
		if !ok || speechErr.Status != http.StatusNotFound {
			t.Fatalf("error = %v, want 404 SpeechError", err)
		}
	})
}

func TestCreateVoiceCloneReportsProviderError(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVoiceCloneUpstream(t)
	upstream.cloneBodyRaw = `{"base_resp":{"status_code":2038,"status_msg":"no cloning permission, please check account verification status"}}`
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	_, _, err := env.gw.CreateVoiceClone(context.Background(), VoiceCloneRequest{
		Name: "Narrator", FileName: "sample.mp3", Sample: testVoiceSample,
		Model: "speech-2.8-hd", PreviewText: "Preview", Authorized: true,
	})
	speechErr, ok := err.(*SpeechError)
	if !ok || speechErr.Status != http.StatusForbidden {
		t.Fatalf("error = %v, want 403 SpeechError", err)
	}
	if !strings.Contains(speechErr.Message, "no cloning permission") {
		t.Fatalf("unexpected message: %q", speechErr.Message)
	}
	clones, err := env.st.ListVoiceClones()
	if err != nil {
		t.Fatalf("list clones: %v", err)
	}
	if len(clones) != 0 {
		t.Fatalf("failed clone was stored: %+v", clones)
	}
}

func TestCreateVoiceCloneKeepsVoiceWithoutPreview(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVoiceCloneUpstream(t)
	upstream.noPreview = true
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	clone, warning, err := env.gw.CreateVoiceClone(context.Background(), VoiceCloneRequest{
		Name: "Narrator", FileName: "sample.mp3", Sample: testVoiceSample,
		Model: "speech-2.8-hd", PreviewText: "Preview", Authorized: true,
	})
	if err != nil {
		t.Fatalf("create voice clone: %v", err)
	}
	if !strings.Contains(warning, "no preview audio") {
		t.Fatalf("unexpected warning: %q", warning)
	}
	if clone.ID == 0 {
		t.Fatal("voice was not stored")
	}
}

func TestCreateVoiceCloneReportsPreviewDownloadFailure(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVoiceCloneUpstream(t)
	upstream.previewCode = http.StatusInternalServerError
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	clone, warning, err := env.gw.CreateVoiceClone(context.Background(), VoiceCloneRequest{
		Name: "Narrator", FileName: "sample.mp3", Sample: testVoiceSample,
		Model: "speech-2.8-hd", PreviewText: "Preview", Authorized: true,
	})
	if err != nil {
		t.Fatalf("create voice clone: %v", err)
	}
	if !strings.Contains(warning, "preview could not be downloaded") {
		t.Fatalf("unexpected warning: %q", warning)
	}
	if clone.PreviewPath != "" {
		t.Fatalf("preview path should be empty: %q", clone.PreviewPath)
	}
}

func TestPreviewVoiceClone(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVoiceCloneUpstream(t)
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	clone := &store.VoiceClone{
		VoiceID: "clone_previewvoice", Name: "Narrator", ChannelID: channel.ID,
		ChannelName: channel.Name, Model: "speech-2.8-hd", UpstreamModel: "speech-2.8-hd",
		PreviewText: "Old preview", Authorized: true,
	}
	if err := env.st.CreateVoiceClone(clone); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	updated, err := env.gw.PreviewVoiceClone(context.Background(), clone.ID, "A fresh preview.")
	if err != nil {
		t.Fatalf("preview voice clone: %v", err)
	}
	if updated.PreviewText != "A fresh preview." || updated.PreviewDurationMS != 2000 || updated.PreviewFormat != "mp3" {
		t.Fatalf("updated clone: %+v", updated)
	}
	if _, err := os.Stat(filepath.Join(env.gw.cfg.DataDir, updated.PreviewPath)); err != nil {
		t.Fatalf("stored preview: %v", err)
	}
	upstream.mu.Lock()
	if len(upstream.t2aBodies) != 1 {
		t.Fatalf("t2a calls = %d, want 1", len(upstream.t2aBodies))
	}
	body := upstream.t2aBodies[0]
	upstream.mu.Unlock()
	voice, _ := body["voice_setting"].(map[string]any)
	if body["model"] != "speech-2.8-hd" || body["text"] != "A fresh preview." || voice["voice_id"] != clone.VoiceID {
		t.Fatalf("unexpected t2a body: %+v", body)
	}
}

func TestCreateSpeechPinsClonedVoiceChannel(t *testing.T) {
	env := newTestEnv(t)
	preferred := newMockSpeechUpstream(t)
	fallback := newMockSpeechUpstream(t)
	cloneChannel := env.addChannel(t, "clone-channel", preferred.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	env.addChannel(t, "other-channel", fallback.server.URL, "minimax", 10, []string{"speech-2.8-hd"}, nil, 0, 0)
	clone := &store.VoiceClone{
		VoiceID: "clone_pinnedvoice", Name: "Mine", ChannelID: cloneChannel.ID,
		ChannelName: cloneChannel.Name, Model: "speech-2.8-hd", UpstreamModel: "speech-2.8-hd",
		Authorized: true,
	}
	if err := env.st.CreateVoiceClone(clone); err != nil {
		t.Fatalf("seed clone: %v", err)
	}

	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "Read this with my cloned voice.", VoiceID: clone.VoiceID, VoiceName: clone.Name,
	})
	if err != nil {
		t.Fatalf("create speech: %v", err)
	}
	if clip.ChannelID != cloneChannel.ID {
		t.Fatalf("clip channel = %d, want %d", clip.ChannelID, cloneChannel.ID)
	}
	preferred.mu.Lock()
	preferredCalls := len(preferred.ttsBodies)
	preferred.mu.Unlock()
	fallback.mu.Lock()
	fallbackCalls := len(fallback.ttsBodies)
	fallback.mu.Unlock()
	if preferredCalls != 1 || fallbackCalls != 0 {
		t.Fatalf("tts calls preferred=%d fallback=%d", preferredCalls, fallbackCalls)
	}
}
