package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestFetchModelsOpenAI(t *testing.T) {
	env := newTestEnv(t)
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o-mini"},{"id":"gpt-4o"},{"id":"gpt-4o"}]}`)
	}))
	defer upstream.Close()

	models, err := env.gw.FetchModels(context.Background(), "openai", upstream.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if gotPath != "/v1/models" || gotAuth != "Bearer sk-test" {
		t.Fatalf("upstream request path=%q auth=%q", gotPath, gotAuth)
	}
	want := []string{"gpt-4o", "gpt-4o-mini"}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
}

func TestFetchModelsAnthropicPagination(t *testing.T) {
	env := newTestEnv(t)
	var afterIDs []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-ant" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q", got)
		}
		afterIDs = append(afterIDs, r.URL.Query().Get("after_id"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-b"},{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-c"}],"has_more":false,"last_id":"claude-c"}`)
	}))
	defer upstream.Close()

	models, err := env.gw.FetchModels(context.Background(), "anthropic", upstream.URL+"/v1", "sk-ant")
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	want := []string{"claude-a", "claude-b", "claude-c"}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	if len(afterIDs) != 2 || afterIDs[1] != "claude-a" {
		t.Fatalf("after_id sequence = %v", afterIDs)
	}
}

func TestFetchModelsUpstreamError(t *testing.T) {
	env := newTestEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer upstream.Close()

	_, err := env.gw.FetchModels(context.Background(), "openai", upstream.URL+"/v1", "bad")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"401", "invalid api key"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestParseModelListShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{name: "openai data objects", body: `{"data":[{"id":"a"},{"id":"b"}]}`, want: []string{"a", "b"}},
		{name: "string items", body: `{"data":["a","b"]}`, want: []string{"a", "b"}},
		{name: "bare array", body: `[{"id":"a"},{"name":"b"},{"model":"c"},{"id":""}]`, want: []string{"a", "b", "c"}},
		{name: "models key", body: `{"models":[{"id":"a"}]}`, want: []string{"a"}},
		{name: "empty", body: `{"data":[]}`, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _, err := parseModelList([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("models = %v, want %v", got, tc.want)
			}
		})
	}
	if _, _, _, err := parseModelList([]byte(`not-json`)); err == nil {
		t.Fatal("expected an error for an invalid payload")
	}
}
