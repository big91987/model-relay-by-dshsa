package server

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var serverTestAudio = []byte("ID3\x03\x00\x00\x00mock-server-speech")

func newVoiceCloneUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/files/upload":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, "bad multipart", http.StatusBadRequest)
				return
			}
			if r.FormValue("purpose") != "voice_clone" {
				t.Errorf("purpose = %q", r.FormValue("purpose"))
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, "missing file", http.StatusBadRequest)
				return
			}
			file.Close()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"file":{"file_id":777,"bytes":34,"filename":"sample.mp3","purpose":"voice_clone"},"base_resp":{"status_code":0,"status_msg":"success"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/voice_clone":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["voice_id"] == "" || body["text"] == "" {
				t.Errorf("clone body = %+v", body)
			}
			_, _ = io.WriteString(w, `{"input_sensitive":false,"demo_audio":"`+server.URL+`/preview.mp3","extra_info":{"audio_length":2000,"usage_characters":18},"base_resp":{"status_code":0,"status_msg":"success"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/preview.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(serverTestAudio)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/t2a_v2":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"audio":"`+hex.EncodeToString(serverTestAudio)+`","status":2},"extra_info":{"audio_length":2000,"audio_format":"mp3","usage_characters":18},"base_resp":{"status_code":0,"status_msg":"success"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func doMultipart(t *testing.T, client *http.Client, url string, fields map[string]string, filename string, fileBytes []byte) (*http.Response, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create file field: %v", err)
	}
	if _, err := part.Write(fileBytes); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post multipart: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, data
}

func TestVoiceCloneFlow(t *testing.T) {
	ts, _ := newTestServer(t)
	upstream := newVoiceCloneUpstream(t)
	client := newClient(t)

	resp, body := doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{"username": "admin", "password": "admin-pass"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}
	resp, body = doJSON(t, client, http.MethodPost, ts.URL+"/api/channels", map[string]any{
		"name": "minimax", "type": "minimax", "base_url": upstream.URL, "api_key": "upstream-key",
		"models": []string{"speech-2.8-hd"}, "model_mapping": map[string]string{},
		"priority": 0, "weight": 1, "status": "enabled", "input_price": 0, "output_price": 0,
	}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel status %d: %s", resp.StatusCode, body)
	}

	resp, body = doMultipart(t, client, ts.URL+"/api/voice-clones", map[string]string{
		"name": "Narrator", "model": "speech-2.8-hd",
		"preview_text": "This is the preview.", "authorized": "true",
	}, "sample.mp3", []byte("ID3\x03\x00\x00\x00server-sample"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create clone status %d: %s", resp.StatusCode, body)
	}
	var created struct {
		ID           int64  `json:"id"`
		VoiceID      string `json:"voice_id"`
		Name         string `json:"name"`
		PreviewReady bool   `json:"preview_ready"`
		PreviewURL   string `json:"preview_url"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode clone: %v", err)
	}
	if created.ID == 0 || created.Name != "Narrator" || !created.PreviewReady || created.PreviewURL == "" {
		t.Fatalf("unexpected clone: %s", body)
	}

	resp, body = doJSON(t, client, http.MethodGet, ts.URL+"/api/voice-clones", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), created.VoiceID) {
		t.Fatalf("list clones status %d: %s", resp.StatusCode, body)
	}

	previewResp, err := client.Get(ts.URL + created.PreviewURL)
	if err != nil {
		t.Fatalf("get preview: %v", err)
	}
	audio, _ := io.ReadAll(previewResp.Body)
	previewResp.Body.Close()
	if previewResp.StatusCode != http.StatusOK || string(audio) != string(serverTestAudio) {
		t.Fatalf("preview status %d audio=%q", previewResp.StatusCode, audio)
	}
	if previewResp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("preview content type = %q", previewResp.Header.Get("Content-Type"))
	}

	resp, body = doJSON(t, client, http.MethodPost, ts.URL+created.PreviewURL, map[string]string{"text": "A second preview."}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("regenerate preview status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "A second preview.") {
		t.Fatalf("regenerated preview body: %s", body)
	}
}

func TestVoiceCloneRequiresAuthorization(t *testing.T) {
	ts, _ := newTestServer(t)
	client := newClient(t)
	doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{"username": "admin", "password": "admin-pass"}, nil)

	resp, body := doMultipart(t, client, ts.URL+"/api/voice-clones", map[string]string{
		"name": "Narrator", "model": "speech-2.8-hd",
		"preview_text": "Preview", "authorized": "false",
	}, "sample.mp3", []byte("ID3\x03\x00\x00\x00server-sample"))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "permission") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	resp, _ = doJSON(t, client, http.MethodGet, ts.URL+"/api/voice-clones", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status %d", resp.StatusCode)
	}
}

func TestVoiceCloneEndpointsRequireSession(t *testing.T) {
	ts, _ := newTestServer(t)
	client := newClient(t)
	resp, _ := doJSON(t, client, http.MethodGet, ts.URL+"/api/voice-clones", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}
