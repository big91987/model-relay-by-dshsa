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
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

var testMusicBytes = []byte("ID3\x04\x00\x00\x00\x00\x00\x00mock-mp3-audio-bytes")

type mockMusicUpstream struct {
	mu      sync.Mutex
	creates []map[string]any
	mode    string
	audio   []byte
	server  *httptest.Server
}

func newMockMusicUpstream(t *testing.T, mode string) *mockMusicUpstream {
	t.Helper()
	m := &mockMusicUpstream{mode: mode, audio: testMusicBytes}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"music-3.0"},{"id":"gpt-4o"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/music_generation":
			if r.Header.Get("Authorization") != "Bearer upstream-key" {
				t.Errorf("unexpected upstream auth: %q", r.Header.Get("Authorization"))
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.creates = append(m.creates, body)
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch m.mode {
			case "provider-error":
				_, _ = io.WriteString(w, `{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`)
			case "http-error":
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"base_resp":{"status_code":2013,"status_msg":"the prompt was rejected by the safety filter"}}`)
			case "url":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data":       map[string]any{"audio": m.server.URL + "/media.mp3", "status": 2},
					"extra_info": map[string]any{"music_duration": 25364},
					"base_resp":  map[string]any{"status_code": 0, "status_msg": "success"},
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data":       map[string]any{"audio": hex.EncodeToString(m.audio), "status": 2},
					"extra_info": map[string]any{"music_duration": 25364},
					"base_resp":  map[string]any{"status_code": 0, "status_msg": "success"},
				})
			}
		case r.Method == http.MethodGet && r.URL.Path == "/media.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(m.audio)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func waitForMusicTask(t *testing.T, st *store.Store, id int64) *store.MusicTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := st.GetMusicTask(id)
		if err != nil {
			t.Fatalf("get music task: %v", err)
		}
		if task.Status == "failed" || (task.Status == "succeeded" && (task.AudioPath != "" || task.Error != "")) {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("music task %d did not finish in time", id)
	return nil
}

func TestCreateMusicHex(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockMusicUpstream(t, "hex")
	env.addChannel(t, "minimax-music", upstream.server.URL, "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)

	task, err := env.gw.CreateMusic(MusicRequest{
		Model:  "music-3.0",
		Prompt: "Indie folk, melancholic, rainy night",
		Lyrics: "[verse]\nStreetlights flicker",
		Format: "mp3",
	})
	if err != nil {
		t.Fatalf("create music: %v", err)
	}
	if task.ID == 0 || task.Status != "queued" || task.Instrumental {
		t.Fatalf("unexpected created task: %+v", task)
	}

	done := waitForMusicTask(t, env.st, task.ID)
	if done.Status != "succeeded" || done.AudioPath == "" || done.Error != "" {
		t.Fatalf("unexpected finished task: %+v", done)
	}
	upstream.mu.Lock()
	if len(upstream.creates) != 1 {
		t.Fatalf("expected one upstream create, got %d", len(upstream.creates))
	}
	create := upstream.creates[0]
	upstream.mu.Unlock()
	if create["model"] != "music-3.0" || create["prompt"] != "Indie folk, melancholic, rainy night" ||
		create["lyrics"] != "[verse]\nStreetlights flicker" || create["output_format"] != "hex" ||
		create["is_instrumental"] != false {
		t.Fatalf("unexpected upstream payload: %+v", create)
	}
	audio, _ := create["audio_setting"].(map[string]any)
	if audio["sample_rate"] != float64(44100) || audio["bitrate"] != float64(256000) || audio["format"] != "mp3" {
		t.Fatalf("unexpected audio_setting: %+v", audio)
	}
	if done.DurationMS != 25364 || done.Format != "mp3" {
		t.Fatalf("task output metadata was not updated: %+v", done)
	}
	stored, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, done.AudioPath))
	if err != nil {
		t.Fatalf("read stored audio: %v", err)
	}
	if string(stored) != string(testMusicBytes) {
		t.Fatalf("unexpected stored audio: %q", stored)
	}
}

func TestCreateMusicInstrumentalOmitsLyrics(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockMusicUpstream(t, "hex")
	env.addChannel(t, "minimax-music", upstream.server.URL, "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)

	task, err := env.gw.CreateMusic(MusicRequest{
		Model:        "music-3.0",
		Prompt:       "Sparse piano, cinematic",
		Lyrics:       "leftover lyrics that should be dropped",
		Instrumental: true,
		Format:       "wav",
	})
	if err != nil {
		t.Fatalf("create music: %v", err)
	}
	done := waitForMusicTask(t, env.st, task.ID)
	if done.Status != "succeeded" || done.AudioPath == "" {
		t.Fatalf("unexpected finished task: %+v", done)
	}
	if done.Lyrics != "" {
		t.Fatalf("instrumental task kept lyrics: %q", done.Lyrics)
	}
	upstream.mu.Lock()
	create := upstream.creates[0]
	upstream.mu.Unlock()
	if _, ok := create["lyrics"]; ok {
		t.Fatalf("instrumental request forwarded lyrics: %+v", create)
	}
	if create["is_instrumental"] != true {
		t.Fatalf("is_instrumental = %v, want true", create["is_instrumental"])
	}
	audio, _ := create["audio_setting"].(map[string]any)
	if audio["format"] != "wav" {
		t.Fatalf("unexpected audio format: %+v", audio)
	}
}

func TestCreateMusicDownloadsURL(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockMusicUpstream(t, "url")
	env.addChannel(t, "minimax-music", upstream.server.URL, "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)

	task, err := env.gw.CreateMusic(MusicRequest{
		Model:  "music-3.0",
		Prompt: "Pop, bright",
		Lyrics: "la la la",
		Format: "mp3",
	})
	if err != nil {
		t.Fatalf("create music: %v", err)
	}
	done := waitForMusicTask(t, env.st, task.ID)
	if done.Status != "succeeded" || done.AudioPath == "" || done.RemoteURL != "" {
		t.Fatalf("URL response was not stored locally: %+v", done)
	}
	stored, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, done.AudioPath))
	if err != nil || string(stored) != string(testMusicBytes) {
		t.Fatalf("unexpected stored audio: %q err=%v", stored, err)
	}
}

func TestCreateMusicProviderError(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockMusicUpstream(t, "provider-error")
	env.addChannel(t, "minimax-music", upstream.server.URL, "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)

	task, err := env.gw.CreateMusic(MusicRequest{Model: "music-3.0", Prompt: "x", Lyrics: "y"})
	if err != nil {
		t.Fatalf("create music: %v", err)
	}
	done := waitForMusicTask(t, env.st, task.ID)
	if done.Status != "failed" || !strings.Contains(done.Error, "login fail") {
		t.Fatalf("unexpected failed task: %+v", done)
	}
}

func TestCreateMusicHTTPError(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockMusicUpstream(t, "http-error")
	env.addChannel(t, "minimax-music", upstream.server.URL, "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)

	task, err := env.gw.CreateMusic(MusicRequest{Model: "music-3.0", Prompt: "x", Lyrics: "y"})
	if err != nil {
		t.Fatalf("create music: %v", err)
	}
	done := waitForMusicTask(t, env.st, task.ID)
	if done.Status != "failed" || !strings.Contains(done.Error, "safety filter") {
		t.Fatalf("unexpected failed task: %+v", done)
	}
}

func TestCreateMusicValidatesInput(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "minimax-music", "https://api.example.invalid", "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)
	env.addChannel(t, "music-cover", "https://api.example.invalid", "minimax-music", 0, []string{"music-cover"}, nil, 0, 0)
	env.addChannel(t, "video", "https://api.example.invalid", "minimax", 0, []string{"MiniMax-H3"}, nil, 0, 0)

	cases := []struct {
		name string
		req  MusicRequest
		want int
	}{
		{"empty model", MusicRequest{Prompt: "x", Lyrics: "y"}, http.StatusBadRequest},
		{"empty prompt", MusicRequest{Model: "music-3.0", Lyrics: "y"}, http.StatusBadRequest},
		{"missing lyrics", MusicRequest{Model: "music-3.0", Prompt: "x"}, http.StatusBadRequest},
		{"instrumental without prompt", MusicRequest{Model: "music-3.0", Instrumental: true}, http.StatusBadRequest},
		{"bad format", MusicRequest{Model: "music-3.0", Prompt: "x", Lyrics: "y", Format: "pcm"}, http.StatusBadRequest},
		{"long lyrics", MusicRequest{Model: "music-3.0", Prompt: "x", Lyrics: strings.Repeat("a", maxMusicLyrics+1)}, http.StatusBadRequest},
		{"unknown model", MusicRequest{Model: "music-9", Prompt: "x", Lyrics: "y"}, http.StatusNotFound},
		{"cover model", MusicRequest{Model: "music-cover", Prompt: "x", Lyrics: "y"}, http.StatusNotFound},
		{"video model", MusicRequest{Model: "MiniMax-H3", Prompt: "x", Lyrics: "y"}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.gw.CreateMusic(tc.req)
			if err == nil {
				t.Fatal("expected an error")
			}
			musicErr, ok := err.(*MusicError)
			if !ok || musicErr.Status != tc.want {
				t.Fatalf("error = %v, want MusicError status %d", err, tc.want)
			}
		})
	}
}

func TestMusicModelWithAlias(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockMusicUpstream(t, "hex")
	env.addChannel(t, "minimax-music", upstream.server.URL, "minimax-music", 0, nil,
		map[string]string{"my-song": "music-3.0"}, 0, 0)

	task, err := env.gw.CreateMusic(MusicRequest{Model: "my-song", Prompt: "x", Lyrics: "y"})
	if err != nil {
		t.Fatalf("create music with alias: %v", err)
	}
	done := waitForMusicTask(t, env.st, task.ID)
	if done.Status != "succeeded" || done.UpstreamModel != "music-3.0" {
		t.Fatalf("unexpected task: %+v", done)
	}
}

func TestResumeMusicTasksMarksFailed(t *testing.T) {
	env := newTestEnv(t)
	channel := env.addChannel(t, "minimax-music", "https://api.example.invalid", "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)
	task := &store.MusicTask{
		ChannelID: channel.ID, ChannelName: channel.Name, Model: "music-3.0",
		UpstreamModel: "music-3.0", Prompt: "x", Lyrics: "y", Format: "mp3", Status: "generating",
	}
	if err := env.st.CreateMusicTask(task); err != nil {
		t.Fatalf("seed music task: %v", err)
	}
	env.gw.ResumeMusicTasks()
	done, err := env.st.GetMusicTask(task.ID)
	if err != nil {
		t.Fatalf("get music task: %v", err)
	}
	if done.Status != "failed" || !strings.Contains(done.Error, "restarted") {
		t.Fatalf("interrupted task was not marked failed: %+v", done)
	}
}

func TestMusicModelsAreNotChatModels(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "minimax-music", "https://api.example.invalid", "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)

	rec := env.chat(t, `{"model":"music-3.0","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("chat status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestTestChannelMusicUsesModelList(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockMusicUpstream(t, "hex")
	channel := env.addChannel(t, "minimax-music", upstream.server.URL, "minimax-music", 0, []string{"music-3.0"}, nil, 0, 0)

	result := env.gw.TestChannel(context.Background(), channel, "music-3.0")
	if !result.OK || result.StatusCode != http.StatusOK {
		t.Fatalf("unexpected test result: %+v", result)
	}
}
