package gateway

import (
	"context"
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

const testFirstFrame = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lv0nAAAAAElFTkSuQmCC"

var testVideoBytes = []byte("\x00\x00\x00\x18ftypmp42mock-video-bytes")

type mockVideoUpstream struct {
	mu            sync.Mutex
	creates       []map[string]any
	queries       int
	videoStatuses []string
	server        *httptest.Server
}

func newMockVideoUpstream(t *testing.T, statuses ...string) *mockVideoUpstream {
	t.Helper()
	m := &mockVideoUpstream{videoStatuses: statuses}
	if len(m.videoStatuses) == 0 {
		m.videoStatuses = []string{"succeeded"}
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/video_generation":
			if r.Header.Get("Authorization") != "Bearer upstream-key" {
				t.Errorf("unexpected upstream auth: %q", r.Header.Get("Authorization"))
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.creates = append(m.creates, body)
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"task_id":"task-123"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/query/video_generation/"):
			m.mu.Lock()
			index := m.queries
			m.queries++
			status := m.videoStatuses[len(m.videoStatuses)-1]
			if index < len(m.videoStatuses) {
				status = m.videoStatuses[index]
			}
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if status == "succeeded" {
				_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{
					"id": "task-123", "model": "MiniMax-H3", "status": "succeeded",
					"content":    map[string]any{"url": m.server.URL + "/media.mp4"},
					"resolution": "768P", "duration": 6, "ratio": "16:9",
				}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{
				"id": "task-123", "model": "MiniMax-H3", "status": status,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/media.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(testVideoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func waitForVideoTask(t *testing.T, st *store.Store, id int64) *store.VideoTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, err := st.GetVideoTask(id)
		if err != nil {
			t.Fatalf("get video task: %v", err)
		}
		if task.Status == "failed" || task.Status == "cancelled" || task.Status == "expired" ||
			(task.Status == "succeeded" && (task.VideoPath != "" || task.Error != "")) {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("video task %d did not finish in time", id)
	return nil
}

func TestCreateVideoTextToVideo(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVideoUpstream(t, "running", "succeeded")
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"MiniMax-H3"}, nil, 0, 0)
	env.gw.videoPollInterval = 5 * time.Millisecond

	task, err := env.gw.CreateVideo(context.Background(), VideoRequest{
		Model: "MiniMax-H3", Prompt: "A lighthouse in a storm", Resolution: "768P", Duration: 6, Ratio: "16:9",
	})
	if err != nil {
		t.Fatalf("create video: %v", err)
	}
	if task.ID == 0 || task.Status != "queued" || task.Mode != "text" {
		t.Fatalf("unexpected created task: %+v", task)
	}
	upstream.mu.Lock()
	if len(upstream.creates) != 1 {
		t.Fatalf("expected one upstream create, got %d", len(upstream.creates))
	}
	create := upstream.creates[0]
	upstream.mu.Unlock()
	if create["model"] != "MiniMax-H3" || create["resolution"] != "768P" || create["duration"] != float64(6) || create["ratio"] != "16:9" {
		t.Fatalf("unexpected upstream payload: %+v", create)
	}
	content, _ := create["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected text-only content, got %+v", create["content"])
	}
	if item, _ := content[0].(map[string]any); item["type"] != "text" || item["text"] != "A lighthouse in a storm" {
		t.Fatalf("unexpected content item: %+v", content[0])
	}

	done := waitForVideoTask(t, env.st, task.ID)
	if done.Status != "succeeded" || done.VideoPath == "" || done.Error != "" {
		t.Fatalf("unexpected finished task: %+v", done)
	}
	if done.Resolution != "768P" || done.Duration != 6 || done.Ratio != "16:9" {
		t.Fatalf("task output metadata was not updated: %+v", done)
	}
	stored, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, done.VideoPath))
	if err != nil {
		t.Fatalf("read downloaded video: %v", err)
	}
	if string(stored) != string(testVideoBytes) {
		t.Fatalf("unexpected downloaded video: %q", stored)
	}
}

func TestCreateVideoFirstFrame(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVideoUpstream(t, "succeeded")
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"MiniMax-H3"}, nil, 0, 0)
	env.gw.videoPollInterval = 5 * time.Millisecond

	task, err := env.gw.CreateVideo(context.Background(), VideoRequest{
		Model: "MiniMax-H3", Prompt: "Bring the photo to life", FirstFrame: testFirstFrame,
		Resolution: "2K", Duration: 5, Ratio: "16:9",
	})
	if err != nil {
		t.Fatalf("create video: %v", err)
	}
	if task.Mode != "first_frame" || task.Ratio != "adaptive" {
		t.Fatalf("first-frame task was not normalized: %+v", task)
	}
	upstream.mu.Lock()
	create := upstream.creates[0]
	upstream.mu.Unlock()
	if create["ratio"] != "adaptive" {
		t.Fatalf("image-to-video ratio = %v, want adaptive", create["ratio"])
	}
	content, _ := create["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected text + image content, got %+v", create["content"])
	}
	image, _ := content[1].(map[string]any)
	if image["type"] != "image_url" || image["role"] != "first_frame" {
		t.Fatalf("unexpected image content: %+v", image)
	}
	imageURL, _ := image["image_url"].(map[string]any)
	if imageURL["url"] != testFirstFrame {
		t.Fatalf("first frame was not forwarded: %+v", imageURL)
	}
	waitForVideoTask(t, env.st, task.ID)
}

func TestCreateVideoValidatesInput(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "minimax", "https://api.example.invalid", "minimax", 0, []string{"MiniMax-H3"}, nil, 0, 0)

	cases := []struct {
		name string
		req  VideoRequest
		want int
	}{
		{"empty prompt", VideoRequest{Model: "MiniMax-H3", Resolution: "768P", Duration: 5, Ratio: "16:9"}, http.StatusBadRequest},
		{"bad duration", VideoRequest{Model: "MiniMax-H3", Prompt: "x", Resolution: "768P", Duration: 30, Ratio: "16:9"}, http.StatusBadRequest},
		{"adaptive text ratio", VideoRequest{Model: "MiniMax-H3", Prompt: "x", Resolution: "768P", Duration: 5, Ratio: "adaptive"}, http.StatusBadRequest},
		{"bad first frame", VideoRequest{Model: "MiniMax-H3", Prompt: "x", FirstFrame: "https://example.com/a.png", Resolution: "768P", Duration: 5}, http.StatusBadRequest},
		{"unknown model", VideoRequest{Model: "MiniMax-Unknown", Prompt: "x", Resolution: "768P", Duration: 5, Ratio: "16:9"}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.gw.CreateVideo(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected an error")
			}
			videoErr, ok := err.(*VideoError)
			if !ok || videoErr.Status != tc.want {
				t.Fatalf("error = %v, want VideoError status %d", err, tc.want)
			}
		})
	}
}

func TestCreateVideoReportsUpstreamError(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"unprocessable_entity_error","message":"prompt rejected by the safety filter (2013)"}}`)
	}))
	defer upstream.Close()
	env.addChannel(t, "minimax", upstream.URL, "minimax", 0, []string{"MiniMax-H3"}, nil, 0, 0)

	_, err := env.gw.CreateVideo(context.Background(), VideoRequest{
		Model: "MiniMax-H3", Prompt: "x", Resolution: "768P", Duration: 5, Ratio: "16:9",
	})
	videoErr, ok := err.(*VideoError)
	if !ok || videoErr.Status != http.StatusUnprocessableEntity {
		t.Fatalf("error = %v, want 422 VideoError", err)
	}
	if !strings.Contains(videoErr.Message, "safety filter") {
		t.Fatalf("unexpected message: %q", videoErr.Message)
	}
}

func TestResumeVideoTasks(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockVideoUpstream(t, "succeeded")
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"MiniMax-H3"}, nil, 0, 0)

	task := &store.VideoTask{
		UpstreamTaskID: "task-123", ChannelID: channel.ID, ChannelName: channel.Name,
		Model: "MiniMax-H3", UpstreamModel: "MiniMax-H3", Prompt: "resume me",
		Mode: "text", Resolution: "768P", Duration: 5, Ratio: "16:9", Status: "running",
	}
	if err := env.st.CreateVideoTask(task); err != nil {
		t.Fatalf("seed video task: %v", err)
	}
	env.gw.videoPollInterval = 5 * time.Millisecond
	env.gw.ResumeVideoTasks()

	done := waitForVideoTask(t, env.st, task.ID)
	if done.Status != "succeeded" || done.VideoPath == "" {
		t.Fatalf("resumed task did not finish: %+v", done)
	}
}

func TestVideoModelsAreNotChatModels(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "minimax", "https://api.example.invalid", "minimax", 0, []string{"MiniMax-H3"}, nil, 0, 0)

	rec := env.chat(t, `{"model":"MiniMax-H3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("chat status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// mockSeedanceUpstream answers Ark's content-generation task protocol.
type mockSeedanceUpstream struct {
	mu       sync.Mutex
	creates  []map[string]any
	queries  int
	statuses []string
	server   *httptest.Server
}

func newMockSeedanceUpstream(t *testing.T, statuses ...string) *mockSeedanceUpstream {
	t.Helper()
	m := &mockSeedanceUpstream{statuses: statuses}
	if len(m.statuses) == 0 {
		m.statuses = []string{"succeeded"}
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/contents/generations/tasks":
			if r.Header.Get("Authorization") != "Bearer upstream-key" {
				t.Errorf("unexpected upstream auth: %q", r.Header.Get("Authorization"))
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.mu.Lock()
			m.creates = append(m.creates, body)
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"cgt-123"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/"):
			m.mu.Lock()
			index := m.queries
			m.queries++
			status := m.statuses[len(m.statuses)-1]
			if index < len(m.statuses) {
				status = m.statuses[index]
			}
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			payload := map[string]any{
				"id": "cgt-123", "model": "doubao-seedance-1-0-pro-250528", "status": status,
			}
			if status == "succeeded" {
				payload["content"] = map[string]any{"video_url": m.server.URL + "/media.mp4"}
				payload["resolution"] = "720p"
				payload["duration"] = 5
				payload["ratio"] = "16:9"
			}
			if status == "failed" {
				payload["error"] = map[string]any{"code": "OutputVideoSensitiveContentDetected", "message": "the generated video was blocked by the safety filter"}
			}
			_ = json.NewEncoder(w).Encode(payload)
		case r.Method == http.MethodGet && r.URL.Path == "/media.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(testVideoBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func TestCreateVideoSeedanceTextToVideo(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSeedanceUpstream(t, "running", "succeeded")
	env.addChannel(t, "seedance", upstream.server.URL+"/api/v3", "seedance", 0, []string{"doubao-seedance-1-0-pro-250528"}, nil, 0, 0)
	env.gw.videoPollInterval = 5 * time.Millisecond

	task, err := env.gw.CreateVideo(context.Background(), VideoRequest{
		Model: "doubao-seedance-1-0-pro-250528", Prompt: "A lighthouse in a storm",
		Resolution: "720P", Duration: 5, Ratio: "16:9",
	})
	if err != nil {
		t.Fatalf("create video: %v", err)
	}
	if task.ID == 0 || task.Status != "queued" || task.Mode != "text" || task.Resolution != "720p" {
		t.Fatalf("unexpected created task: %+v", task)
	}
	upstream.mu.Lock()
	if len(upstream.creates) != 1 {
		t.Fatalf("expected one upstream create, got %d", len(upstream.creates))
	}
	create := upstream.creates[0]
	upstream.mu.Unlock()
	if create["model"] != "doubao-seedance-1-0-pro-250528" || create["resolution"] != "720p" ||
		create["duration"] != float64(5) || create["ratio"] != "16:9" {
		t.Fatalf("unexpected upstream payload: %+v", create)
	}
	content, _ := create["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected text-only content, got %+v", create["content"])
	}
	if item, _ := content[0].(map[string]any); item["type"] != "text" || item["text"] != "A lighthouse in a storm" {
		t.Fatalf("unexpected content item: %+v", content[0])
	}

	done := waitForVideoTask(t, env.st, task.ID)
	if done.Status != "succeeded" || done.VideoPath == "" || done.Error != "" {
		t.Fatalf("unexpected finished task: %+v", done)
	}
	if done.Resolution != "720p" || done.Duration != 5 || done.Ratio != "16:9" {
		t.Fatalf("task output metadata was not updated: %+v", done)
	}
	stored, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, done.VideoPath))
	if err != nil {
		t.Fatalf("read downloaded video: %v", err)
	}
	if string(stored) != string(testVideoBytes) {
		t.Fatalf("unexpected downloaded video: %q", stored)
	}
}

func TestCreateVideoSeedanceFirstFrame(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSeedanceUpstream(t, "succeeded")
	env.addChannel(t, "seedance", upstream.server.URL+"/api/v3", "seedance", 0, []string{"doubao-seedance-1-0-pro-250528"}, nil, 0, 0)
	env.gw.videoPollInterval = 5 * time.Millisecond

	task, err := env.gw.CreateVideo(context.Background(), VideoRequest{
		Model: "doubao-seedance-1-0-pro-250528", Prompt: "Bring the photo to life",
		FirstFrame: testFirstFrame, Resolution: "1080p", Duration: 5, Ratio: "16:9",
	})
	if err != nil {
		t.Fatalf("create video: %v", err)
	}
	if task.Mode != "first_frame" || task.Ratio != "adaptive" {
		t.Fatalf("first-frame task was not normalized: %+v", task)
	}
	upstream.mu.Lock()
	create := upstream.creates[0]
	upstream.mu.Unlock()
	content, _ := create["content"].([]any)
	if len(content) != 2 || create["ratio"] != "adaptive" {
		t.Fatalf("unexpected upstream payload: %+v", create)
	}
	image, _ := content[1].(map[string]any)
	if image["type"] != "image_url" || image["role"] != "first_frame" {
		t.Fatalf("unexpected image content: %+v", image)
	}
	imageURL, _ := image["image_url"].(map[string]any)
	if imageURL["url"] != testFirstFrame {
		t.Fatalf("first frame was not forwarded: %+v", imageURL)
	}
	waitForVideoTask(t, env.st, task.ID)
}

func TestCreateVideoSeedanceValidatesModelLimits(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "seedance", "https://api.example.invalid", "seedance", 0,
		[]string{"doubao-seedance-1-0-pro-250528", "doubao-seedance-2-0-260128"}, nil, 0, 0)

	cases := []struct {
		name string
		req  VideoRequest
	}{
		{"minimax resolution", VideoRequest{Model: "doubao-seedance-1-0-pro-250528", Prompt: "x", Resolution: "768P", Duration: 5, Ratio: "16:9"}},
		{"1.0 duration too long", VideoRequest{Model: "doubao-seedance-1-0-pro-250528", Prompt: "x", Resolution: "720p", Duration: 13, Ratio: "16:9"}},
		{"1.0 adaptive text ratio", VideoRequest{Model: "doubao-seedance-1-0-pro-250528", Prompt: "x", Resolution: "720p", Duration: 5, Ratio: "adaptive"}},
		{"1.0 no 4k", VideoRequest{Model: "doubao-seedance-1-0-pro-250528", Prompt: "x", Resolution: "4k", Duration: 5, Ratio: "16:9"}},
		{"2.0 duration too short", VideoRequest{Model: "doubao-seedance-2-0-260128", Prompt: "x", Resolution: "720p", Duration: 2, Ratio: "16:9"}},
		{"2.0 duration too long", VideoRequest{Model: "doubao-seedance-2-0-260128", Prompt: "x", Resolution: "720p", Duration: 16, Ratio: "16:9"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.gw.CreateVideo(context.Background(), tc.req)
			videoErr, ok := err.(*VideoError)
			if !ok || videoErr.Status != http.StatusBadRequest {
				t.Fatalf("error = %v, want 400 VideoError", err)
			}
		})
	}
}

func TestSeedanceModelsAreNotChatModels(t *testing.T) {
	env := newTestEnv(t)
	env.addChannel(t, "seedance", "https://api.example.invalid/api/v3", "seedance", 0,
		[]string{"doubao-seedance-1-0-pro-250528"}, nil, 0, 0)

	rec := env.chat(t, `{"model":"doubao-seedance-1-0-pro-250528","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("chat status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}
