package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

var testSpeechAudio = []byte("ID3\x03\x00\x00\x00mock-minimax-speech")
var testSpeechPCM = []byte{0x00, 0x00, 0x00, 0x01, 0x01, 0x00, 0xff, 0x7f, 0x00, 0x80, 0x10, 0x00}

type mockSpeechUpstream struct {
	mu              sync.Mutex
	ttsBodies       []map[string]any
	voiceCalls      int
	voiceCode       int
	voiceBody       string
	ttsCode         int
	ttsBody         string
	streamChunks    [][]byte
	streamCode      int64
	streamMsg       string
	streamInterrupt bool
	audio           []byte
	async           *mockAsyncSpeech
	server          *httptest.Server
}

func newMockSpeechUpstream(t *testing.T) *mockSpeechUpstream {
	t.Helper()
	m := &mockSpeechUpstream{audio: testSpeechAudio, async: newMockAsyncSpeech(testSpeechAudio)}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.async.handle(w, r) {
			return
		}
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
			if stream, _ := body["stream"].(bool); stream {
				m.writeSpeechStream(w)
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

// mockAsyncSpeech serves the MiniMax long-text task endpoints used by the
// asynchronous speech path. Each query reports Processing until
// queriesBeforeSuccess queries have been answered, then Success.
type mockAsyncSpeech struct {
	mu                   sync.Mutex
	creates              []map[string]any
	queriedTaskIDs       []string
	queries              int
	downloads            int
	fail                 bool
	createCode           int
	createBody           string
	audio                []byte
	taskID               int64
	fileID               int64
	queriesBeforeSuccess int
	durationMS           int64
}

func newMockAsyncSpeech(audio []byte) *mockAsyncSpeech {
	return &mockAsyncSpeech{
		audio:                audio,
		taskID:               910001,
		fileID:               920001,
		queriesBeforeSuccess: 1,
		durationMS:           1234,
	}
}

func (m *mockAsyncSpeech) handle(w http.ResponseWriter, r *http.Request) bool {
	asyncPath := (r.Method == http.MethodPost && r.URL.Path == "/v1/t2a_async_v2") ||
		(r.Method == http.MethodGet && r.URL.Path == "/v1/query/t2a_async_query_v2") ||
		(r.Method == http.MethodGet && r.URL.Path == "/v1/files/retrieve_content")
	if !asyncPath {
		return false
	}
	if r.Header.Get("Authorization") != "Bearer upstream-key" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`)
		return true
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/t2a_async_v2":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.creates = append(m.creates, body)
		code, raw := m.createCode, m.createBody
		taskID, fileID := m.taskID, m.fileID
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if code != 0 {
			w.WriteHeader(code)
		}
		if raw != "" {
			_, _ = io.WriteString(w, raw)
			return true
		}
		text, _ := body["text"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"task_id":          taskID,
			"file_id":          fileID,
			"usage_characters": utf8.RuneCountInString(text),
			"base_resp":        map[string]any{"status_code": 0, "status_msg": "success"},
		})
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/v1/query/t2a_async_query_v2":
		taskID := r.URL.Query().Get("task_id")
		m.mu.Lock()
		m.queriedTaskIDs = append(m.queriedTaskIDs, taskID)
		m.queries++
		queries := m.queries
		fail := m.fail
		fileID := m.fileID
		before := m.queriesBeforeSuccess
		duration := m.durationMS
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]any{
			"task_id":   taskID,
			"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
		}
		switch {
		case fail:
			payload["status"] = "Failed"
			payload["base_resp"] = map[string]any{"status_code": 0, "status_msg": "mock async speech task failed"}
		case queries <= before:
			payload["status"] = "Processing"
		default:
			payload["status"] = "Success"
			payload["file_id"] = fileID
			payload["extra_info"] = map[string]any{"audio_length": duration}
		}
		_ = json.NewEncoder(w).Encode(payload)
		return true
	case r.Method == http.MethodGet && r.URL.Path == "/v1/files/retrieve_content":
		fileID, _ := strconv.ParseInt(r.URL.Query().Get("file_id"), 10, 64)
		m.mu.Lock()
		m.downloads++
		audio := m.audio
		m.mu.Unlock()
		if fileID == 0 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"base_resp":{"status_code":2013,"status_msg":"file_id is required"}}`)
			return true
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		_, _ = w.Write(audio)
		return true
	}
	return false
}

func TestJSONIDString(t *testing.T) {
	cases := map[string]string{
		`12345678901234`:   "12345678901234",
		`"12345678901234"`: "12345678901234",
		`"  abc  "`:        "abc",
		`null`:             "",
		`""`:               "",
		`0`:                "0",
	}
	for raw, want := range cases {
		if got := jsonIDString(json.RawMessage(raw)); got != want {
			t.Fatalf("jsonIDString(%s) = %q, want %q", raw, got, want)
		}
	}
}

// writeSpeechStream mimics MiniMax's SSE stream: one JSON payload per data
// line, status 2 with extra_info on the final chunk.
func (m *mockSpeechUpstream) writeSpeechStream(w http.ResponseWriter) {
	m.mu.Lock()
	chunks := append([][]byte(nil), m.streamChunks...)
	code, msg := m.streamCode, m.streamMsg
	interrupt := m.streamInterrupt
	m.mu.Unlock()
	if len(chunks) == 0 {
		chunks = [][]byte{testSpeechPCM[:4], testSpeechPCM[4:8], testSpeechPCM[8:]}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if code != 0 {
		payload, _ := json.Marshal(map[string]any{
			"data":      nil,
			"base_resp": map[string]any{"status_code": code, "status_msg": msg},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	for index, chunk := range chunks {
		payload := map[string]any{
			"data":      map[string]any{"audio": hex.EncodeToString(chunk), "status": 1},
			"base_resp": map[string]any{"status_code": 0, "status_msg": ""},
		}
		if index == len(chunks)-1 && !interrupt {
			payload["data"] = map[string]any{"audio": hex.EncodeToString(chunk), "status": 2}
			payload["extra_info"] = map[string]any{
				"audio_length":     1234,
				"audio_format":     "pcm",
				"audio_size":       len(testSpeechPCM),
				"usage_characters": 5,
			}
		}
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		if interrupt {
			return
		}
	}
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

func TestCreateSpeechRunsLongTextAsOneAsyncTask(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	text := strings.Repeat("这是一个用于测试长文本异步配音的句子。", 300)
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
	if clip.Status != "running" || clip.UpstreamTaskID == "" || clip.AudioPath != "" {
		t.Fatalf("created contract = %+v, want one running task", clip)
	}
	if clip.TotalChunks != 0 || clip.DoneChunks != 0 || clip.Characters != int64(utf8.RuneCountInString(text)) {
		t.Fatalf("job metadata = %+v", clip)
	}

	stored := waitSpeechClip(t, env, clip.ID)
	if stored.Status != "succeeded" {
		t.Fatalf("job failed: %s", stored.Error)
	}
	if stored.UpstreamTaskID != clip.UpstreamTaskID || stored.TotalChunks != 0 || stored.DoneChunks != 0 {
		t.Fatalf("finished job lost its task identity: %+v", stored)
	}
	if stored.AudioPath == "" || stored.SizeBytes != int64(len(testSpeechAudio)) || stored.DurationMS != 1234 {
		t.Fatalf("unexpected finished job: %+v", stored)
	}
	if stored.ChannelID != channel.ID || stored.Speed != 1.1 || stored.VoiceName != "Expressive Narrator" {
		t.Fatalf("job settings were not preserved: %+v", stored)
	}

	upstream.mu.Lock()
	syncCalls := len(upstream.ttsBodies)
	upstream.mu.Unlock()
	upstream.async.mu.Lock()
	creates := append([]map[string]any(nil), upstream.async.creates...)
	queries := upstream.async.queries
	downloads := upstream.async.downloads
	queriedIDs := append([]string(nil), upstream.async.queriedTaskIDs...)
	upstream.async.mu.Unlock()

	if syncCalls != 0 {
		t.Fatalf("long text used %d synchronous t2a calls, want 0", syncCalls)
	}
	if len(creates) != 1 {
		t.Fatalf("async create calls = %d, want exactly 1", len(creates))
	}
	if body := creates[0]; body["model"] != "speech-2.8-hd" || body["text"] != text {
		t.Fatalf("unexpected async create body: %+v", body)
	}
	voice, _ := creates[0]["voice_setting"].(map[string]any)
	if voice["voice_id"] != "English_expressive_narrator" || voice["speed"] != 1.1 {
		t.Fatalf("unexpected async voice settings: %+v", voice)
	}
	audio, _ := creates[0]["audio_setting"].(map[string]any)
	if audio["format"] != "mp3" || audio["audio_sample_rate"] != float64(32000) {
		t.Fatalf("unexpected async audio settings: %+v", audio)
	}
	if queries < 2 || downloads != 1 {
		t.Fatalf("queries = %d, downloads = %d, want at least 2 queries and 1 download", queries, downloads)
	}
	for _, id := range queriedIDs {
		if id != clip.UpstreamTaskID {
			t.Fatalf("queried task id %q, want %q", id, clip.UpstreamTaskID)
		}
	}

	storedAudio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, stored.AudioPath))
	if err != nil {
		t.Fatalf("read async audio: %v", err)
	}
	if !bytes.Equal(storedAudio, testSpeechAudio) {
		t.Fatalf("stored audio = %q, want the downloaded file", storedAudio)
	}
}

func TestCreateSpeechRejectsLongTextTask(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	upstream.async.createCode = http.StatusBadRequest
	upstream.async.createBody = `{"base_resp":{"status_code":2013,"status_msg":"the long text was rejected"}}`
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	_, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: strings.Repeat("长文本。", 1000), VoiceID: "English_Graceful_Lady",
	})
	speechErr, ok := err.(*SpeechError)
	if !ok || speechErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400 SpeechError", err)
	}
	if !strings.Contains(speechErr.Message, "long text was rejected") {
		t.Fatalf("unexpected message: %q", speechErr.Message)
	}
	upstream.async.mu.Lock()
	queries := upstream.async.queries
	upstream.async.mu.Unlock()
	if queries != 0 {
		t.Fatalf("rejected task was polled %d times", queries)
	}
}

func TestSpeechAsyncTaskResumesAfterRestart(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	upstream.async.queriesBeforeSuccess = 0

	text := strings.Repeat("这是一段在重启后继续查询的文案。", 200)
	clip := &store.SpeechClip{
		ChannelID:      channel.ID,
		ChannelName:    channel.Name,
		Model:          "speech-2.8-hd",
		UpstreamModel:  "speech-2.8-hd",
		VoiceID:        "English_Graceful_Lady",
		VoiceName:      "Graceful Lady",
		Text:           text,
		Speed:          1,
		Status:         "running",
		UpstreamTaskID: "910001",
		UpstreamFileID: 920001,
		Characters:     int64(utf8.RuneCountInString(text)),
	}
	if err := env.st.CreateSpeechClip(clip); err != nil {
		t.Fatalf("store running async job: %v", err)
	}

	env.gw.ResumeSpeechJobs()
	stored := waitSpeechClip(t, env, clip.ID)
	if stored.Status != "succeeded" || stored.UpstreamTaskID != "910001" {
		t.Fatalf("resumed job = %+v", stored)
	}
	upstream.async.mu.Lock()
	creates := len(upstream.async.creates)
	queries := upstream.async.queries
	downloads := upstream.async.downloads
	queriedIDs := append([]string(nil), upstream.async.queriedTaskIDs...)
	upstream.async.mu.Unlock()
	if creates != 0 || queries != 1 || downloads != 1 {
		t.Fatalf("creates=%d queries=%d downloads=%d, want 0/1/1", creates, queries, downloads)
	}
	if len(queriedIDs) != 1 || queriedIDs[0] != "910001" {
		t.Fatalf("queried task ids = %v, want the stored task", queriedIDs)
	}
}

func TestLegacyChunkedSpeechJobResumesCompletedSegments(t *testing.T) {
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

func TestStreamSpeech(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	var starts []SpeechStreamInfo
	var received []byte
	clip, err := env.gw.StreamSpeech(context.Background(), SpeechRequest{
		Model:     "speech-2.8-hd",
		Text:      "Hello from the relay.",
		VoiceID:   "English_expressive_narrator",
		VoiceName: "Expressive Narrator",
		Speed:     1.25,
	}, SpeechStreamHandlers{
		OnStart: func(info SpeechStreamInfo) error {
			starts = append(starts, info)
			return nil
		},
		OnAudio: func(pcm []byte) error {
			received = append(received, pcm...)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("stream speech: %v", err)
	}
	if len(starts) != 1 || starts[0].Format != "pcm" || starts[0].SampleRate != speechStreamSampleRate || starts[0].Channels != 1 {
		t.Fatalf("unexpected stream start: %+v", starts)
	}
	if string(received) != string(testSpeechPCM) {
		t.Fatalf("streamed PCM = %x, want %x", received, testSpeechPCM)
	}
	if clip.ID == 0 || clip.ChannelID != channel.ID || clip.ChannelName != "minimax" {
		t.Fatalf("unexpected clip: %+v", clip)
	}
	if clip.Status != "succeeded" || clip.Format != "wav" || clip.DurationMS != 1234 || clip.Characters != 5 {
		t.Fatalf("unexpected clip metadata: %+v", clip)
	}
	if clip.Speed != 1.25 || clip.VoiceName != "Expressive Narrator" {
		t.Fatalf("clip settings were not preserved: %+v", clip)
	}
	if clip.SizeBytes != int64(len(testSpeechPCM)+44) {
		t.Fatalf("clip size = %d, want %d", clip.SizeBytes, len(testSpeechPCM)+44)
	}

	stored, err := env.st.GetSpeechClip(clip.ID)
	if err != nil {
		t.Fatalf("get speech clip: %v", err)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, stored.AudioPath))
	if err != nil {
		t.Fatalf("read stored audio: %v", err)
	}
	if string(audio[0:4]) != "RIFF" || string(audio[8:12]) != "WAVE" {
		t.Fatalf("stored audio is not a WAV file: %x", audio[:12])
	}
	if got := binary.LittleEndian.Uint32(audio[24:28]); got != speechStreamSampleRate {
		t.Fatalf("stored sample rate = %d", got)
	}
	if string(audio[44:]) != string(testSpeechPCM) {
		t.Fatalf("stored PCM = %x, want %x", audio[44:], testSpeechPCM)
	}

	upstream.mu.Lock()
	if len(upstream.ttsBodies) != 1 {
		t.Fatalf("tts calls = %d, want 1", len(upstream.ttsBodies))
	}
	body := upstream.ttsBodies[0]
	upstream.mu.Unlock()
	if body["stream"] != true || body["output_format"] != "hex" {
		t.Fatalf("unexpected upstream body: %+v", body)
	}
	audioSettings, _ := body["audio_setting"].(map[string]any)
	if audioSettings["format"] != "pcm" || audioSettings["sample_rate"] != float64(speechStreamSampleRate) ||
		audioSettings["channel"] != float64(1) {
		t.Fatalf("unexpected audio settings: %+v", audioSettings)
	}
	voice, _ := body["voice_setting"].(map[string]any)
	if voice["voice_id"] != "English_expressive_narrator" || voice["speed"] != 1.25 {
		t.Fatalf("unexpected voice settings: %+v", voice)
	}
}

func TestStreamSpeechUsesClonedVoiceChannel(t *testing.T) {
	env := newTestEnv(t)
	owner := newMockSpeechUpstream(t)
	other := newMockSpeechUpstream(t)
	ownerChannel := env.addChannel(t, "clone-owner", owner.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	env.addChannel(t, "other", other.server.URL, "minimax", 10, []string{"speech-2.8-hd"}, nil, 0, 0)
	clone := &store.VoiceClone{
		VoiceID:        "moss_audio_stream_clone",
		Name:           "Stream Clone",
		ChannelID:      ownerChannel.ID,
		ChannelName:    ownerChannel.Name,
		Model:          "speech-2.8-hd",
		UpstreamModel:  "speech-2.8-hd",
		SampleFormat:   "mp3",
		SampleBytes:    128,
		SourceFilename: "sample.mp3",
		PreviewText:    "preview",
		Authorized:     true,
	}
	if err := env.st.CreateVoiceClone(clone); err != nil {
		t.Fatalf("store clone: %v", err)
	}

	clip, err := env.gw.StreamSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "hello with a clone", VoiceID: clone.VoiceID, VoiceName: clone.Name,
	}, SpeechStreamHandlers{})
	if err != nil {
		t.Fatalf("stream speech: %v", err)
	}
	if clip.ChannelID != ownerChannel.ID || clip.ChannelName != "clone-owner" {
		t.Fatalf("clip served by %+v, want the clone owner channel", clip)
	}
	other.mu.Lock()
	otherCalls := len(other.ttsBodies)
	other.mu.Unlock()
	if otherCalls != 0 {
		t.Fatalf("the non-owning channel received %d calls", otherCalls)
	}
}

func TestStreamSpeechRejectsLongText(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	_, err := env.gw.StreamSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: strings.Repeat("长", speechSyncLimit+1), VoiceID: "English_Graceful_Lady",
	}, SpeechStreamHandlers{})
	speechErr, ok := err.(*SpeechError)
	if !ok || speechErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400 SpeechError", err)
	}
	if !strings.Contains(speechErr.Message, "background") {
		t.Fatalf("unexpected message: %q", speechErr.Message)
	}
	upstream.mu.Lock()
	calls := len(upstream.ttsBodies)
	upstream.mu.Unlock()
	if calls != 0 {
		t.Fatalf("long text reached the streaming upstream %d times", calls)
	}
}

func TestStreamSpeechFallsBackToJSON(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	// A provider that ignores stream=true answers with one JSON payload.
	upstream.ttsBody = `{"data":{"audio":"` + hex.EncodeToString(testSpeechPCM) + `","status":2},
		"extra_info":{"audio_length":900,"audio_format":"pcm","usage_characters":4},
		"base_resp":{"status_code":0,"status_msg":"success"}}`
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	var received []byte
	clip, err := env.gw.StreamSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "hello", VoiceID: "English_Graceful_Lady",
	}, SpeechStreamHandlers{
		OnAudio: func(pcm []byte) error { received = append(received, pcm...); return nil },
	})
	if err != nil {
		t.Fatalf("stream speech: %v", err)
	}
	if string(received) != string(testSpeechPCM) || clip.DurationMS != 900 || clip.Characters != 4 {
		t.Fatalf("unexpected fallback result: received=%x clip=%+v", received, clip)
	}
}

func TestStreamSpeechFailsOverBeforeAudio(t *testing.T) {
	env := newTestEnv(t)
	bad := newMockSpeechUpstream(t)
	bad.ttsCode = http.StatusInternalServerError
	bad.ttsBody = `{"base_resp":{"status_code":1000,"status_msg":"internal error"}}`
	good := newMockSpeechUpstream(t)
	env.addChannel(t, "bad", bad.server.URL, "minimax", 10, []string{"speech-2.8-hd"}, nil, 0, 0)
	env.addChannel(t, "good", good.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	clip, err := env.gw.StreamSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "hello", VoiceID: "English_Graceful_Lady",
	}, SpeechStreamHandlers{})
	if err != nil {
		t.Fatalf("stream speech: %v", err)
	}
	if clip.ChannelName != "good" {
		t.Fatalf("served by %q, want good", clip.ChannelName)
	}
}

func TestStreamSpeechReportsBusinessErrorBeforeStart(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	upstream.streamCode = 2013
	upstream.streamMsg = "invalid voice id"
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	started := false
	_, err := env.gw.StreamSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "hello", VoiceID: "missing-voice",
	}, SpeechStreamHandlers{
		OnStart: func(SpeechStreamInfo) error { started = true; return nil },
	})
	speechErr, ok := err.(*SpeechError)
	if !ok || speechErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400 SpeechError", err)
	}
	if !strings.Contains(speechErr.Message, "invalid voice id") {
		t.Fatalf("unexpected message: %q", speechErr.Message)
	}
	if started {
		t.Fatal("stream started before the provider rejected the request")
	}
}

func TestStreamSpeechInterruptedAfterAudio(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	upstream.streamInterrupt = true
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)

	var received []byte
	_, err := env.gw.StreamSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: "hello", VoiceID: "English_Graceful_Lady",
	}, SpeechStreamHandlers{
		OnAudio: func(pcm []byte) error { received = append(received, pcm...); return nil },
	})
	if len(received) == 0 {
		t.Fatal("expected the first chunk to reach the caller")
	}
	speechErr, ok := err.(*SpeechError)
	if !ok || speechErr.Status != http.StatusBadGateway {
		t.Fatalf("error = %v, want 502 SpeechError", err)
	}
	if clips, _, err := env.st.ListSpeechClips(1, 10); err != nil || len(clips) != 0 {
		t.Fatalf("interrupted stream stored a clip: %+v, %v", clips, err)
	}
}

func TestPCMToWAV(t *testing.T) {
	wav := pcmToWAV(testSpeechPCM, speechStreamSampleRate, speechStreamChannels)
	if len(wav) != len(testSpeechPCM)+44 {
		t.Fatalf("wav length = %d", len(wav))
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[12:16]) != "fmt " ||
		string(wav[36:40]) != "data" {
		t.Fatalf("unexpected wav header: %x", wav[:44])
	}
	if got := binary.LittleEndian.Uint32(wav[4:8]); got != uint32(36+len(testSpeechPCM)) {
		t.Fatalf("RIFF size = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[20:22]); got != 1 {
		t.Fatalf("audio format = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Fatalf("channels = %d", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != speechStreamSampleRate {
		t.Fatalf("sample rate = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Fatalf("bits per sample = %d", got)
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(len(testSpeechPCM)) {
		t.Fatalf("data size = %d", got)
	}
	if string(wav[44:]) != string(testSpeechPCM) {
		t.Fatalf("wav payload = %x", wav[44:])
	}
}
