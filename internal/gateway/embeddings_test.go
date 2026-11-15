package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

type embeddingsErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// TestEmbeddingsRejectsMissingOrEmptyInput proves an embeddings request without
// usable input is rejected at the public entry point: no upstream channel is
// contacted, no quota is reserved, and the client error names the `input`
// field.
func TestEmbeddingsRejectsMissingOrEmptyInput(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		message string
		code    string
	}{
		{"absent", `{"model":"embed-model"}`, "input is required", "input_required"},
		{"null", `{"model":"embed-model","input":null}`, "input is required", "input_required"},
		{"empty string", `{"model":"embed-model","input":""}`, "input must not be empty", "invalid_input"},
		{"blank string", `{"model":"embed-model","input":"   "}`, "input must not be empty", "invalid_input"},
		{"empty array", `{"model":"embed-model","input":[]}`, "input must not be empty", "invalid_input"},
		{"empty string element", `{"model":"embed-model","input":["hello",""]}`, "input must not contain empty strings", "invalid_input"},
		{"number", `{"model":"embed-model","input":5}`, "input must be a string, an array of strings, or an array of token ids", "invalid_input"},
		{"object", `{"model":"embed-model","input":{"text":"hello"}}`, "input must be a string, an array of strings, or an array of token ids", "invalid_input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			var hits int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				_, _ = io.WriteString(w, `{"object":"list","data":[{"embedding":[0.1]}]}`)
			}))
			defer upstream.Close()
			env.addChannel(t, "embed", upstream.URL+"/v1", "openai", 0, []string{"embed-model"}, nil, 2, 0)
			// A zero balance turns any quota reservation into a 429, so a 400
			// here shows the request was rejected before quota accounting.
			env.setQuota(t, 0)

			rec := env.postEmbeddings(t, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			var resp embeddingsErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error response: %v: %s", err, rec.Body.String())
			}
			if resp.Error.Message != tc.message || resp.Error.Code != tc.code {
				t.Fatalf("error = %+v, want message %q code %q", resp.Error, tc.message, tc.code)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Fatalf("upstream called %d times, want 0", got)
			}
			logs := env.logs(t)
			if len(logs) != 1 || logs[0].Status != "error" || logs[0].StatusCode != http.StatusBadRequest || logs[0].CostStatus != store.CostFree {
				t.Fatalf("unexpected logs: %+v", logs)
			}
			settled := env.freshToken(t)
			if settled.UsedQuota != 0 || settled.ReservedQuota != 0 {
				t.Fatalf("rejected request changed balance: used=%v reserved=%v", settled.UsedQuota, settled.ReservedQuota)
			}
		})
	}
}

// TestEmbeddingsAcceptsSupportedInputFormats keeps the existing legal input
// shapes working and confirms they are still forwarded, metered and logged.
func TestEmbeddingsAcceptsSupportedInputFormats(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		input any
	}{
		{"string", `{"model":"embed-model","input":"hello"}`, "hello"},
		{"string array", `{"model":"embed-model","input":["hello","world"]}`, []any{"hello", "world"}},
		{"token ids", `{"model":"embed-model","input":[1,2,3]}`, []any{1.0, 2.0, 3.0}},
		{"token id arrays", `{"model":"embed-model","input":[[1,2],[3]]}`, []any{[]any{1.0, 2.0}, []any{3.0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			var hits int32
			var forwarded map[string]any
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				forwarded = nil
				if err := json.NewDecoder(r.Body).Decode(&forwarded); err != nil {
					t.Errorf("decode forwarded body: %v", err)
				}
				_, _ = io.WriteString(w, `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":10,"total_tokens":10}}`)
			}))
			defer upstream.Close()
			env.addChannel(t, "embed", upstream.URL+"/v1", "openai", 0, []string{"embed-model"}, map[string]string{"embed-model": "upstream-embed"}, 2, 0)

			rec := env.postEmbeddings(t, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Fatalf("upstream called %d times, want 1", got)
			}
			if forwarded["model"] != "upstream-embed" {
				t.Fatalf("upstream model = %v, want mapped model", forwarded["model"])
			}
			if !reflect.DeepEqual(forwarded["input"], tc.input) {
				t.Fatalf("upstream input = %#v, want %#v", forwarded["input"], tc.input)
			}
			logs := env.logs(t)
			if len(logs) != 1 || logs[0].Status != "success" || logs[0].PromptTokens != 10 || logs[0].CostStatus != store.CostActual {
				t.Fatalf("unexpected logs: %+v", logs)
			}
		})
	}
}
