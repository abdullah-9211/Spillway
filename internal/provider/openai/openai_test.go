package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/openai"
	"github.com/abdullah-9211/spillway/internal/provider/providertest"
)

func handler(s providertest.Scenario, canceled chan struct{}) http.HandlerFunc {
	return providertest.OpenAIWire(s, canceled)
}

const chatText = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
		w.(http.Flusher).Flush()
	}
}

func chunk(delta, finish string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-3","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`, delta, finish)
}

func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Harness{New: func(t *testing.T, s providertest.Scenario) (provider.Provider, <-chan struct{}) {
		canceled := make(chan struct{})
		srv := httptest.NewServer(handler(s, canceled))
		t.Cleanup(srv.Close)
		return openai.New(provider.Config{Name: "openai", APIKey: "k", BaseURL: srv.URL + "/v1"}), canceled
	}})
}

func capture(t *testing.T, stream bool) (body map[string]any, auth, path string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		if stream {
			sse(w, chunk(`{"content":"x"}`, `"stop"`), "[DONE]")
			return
		}
		fmt.Fprint(w, chatText)
	}))
	defer srv.Close()
	p := openai.New(provider.Config{APIKey: "sk-test", BaseURL: srv.URL + "/v1/"})
	req := &provider.ChatRequest{
		Model: "gpt-x", Messages: []provider.Message{{Role: "user", Content: provider.TextContent("hi")}},
		StreamOptions: &provider.StreamOptions{IncludeUsage: false}, Stream: stream,
	}
	if stream {
		rd, err := p.ChatStream(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		_ = rd.Close()
	} else if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return body, auth, path
}

func TestRequestShape(t *testing.T) {
	body, auth, path := capture(t, false)
	if path != "/v1/chat/completions" || auth != "Bearer sk-test" {
		t.Errorf("path=%q auth=%q", path, auth)
	}
	if body["model"] != "gpt-x" {
		t.Errorf("model = %v", body["model"])
	}
	if _, ok := body["stream"]; ok {
		t.Errorf("non-streaming request must not set stream: %v", body["stream"])
	}
	if _, ok := body["stream_options"]; ok {
		t.Errorf("non-streaming request must not carry stream_options")
	}
}

func TestStreamForcesUsage(t *testing.T) {
	body, _, _ := capture(t, true)
	opts, _ := body["stream_options"].(map[string]any)
	if body["stream"] != true || opts["include_usage"] != true {
		t.Errorf("stream=%v stream_options=%v; include_usage must be forced on", body["stream"], body["stream_options"])
	}
}

func TestErrorMessageIsKept(t *testing.T) {
	srv := httptest.NewServer(handler(providertest.Err400, nil))
	defer srv.Close()
	_, err := openai.New(provider.Config{BaseURL: srv.URL + "/v1"}).Chat(context.Background(), &provider.ChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("error should carry the upstream message: %v", err)
	}
}

func TestStreamWithoutDoneIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, chunk(`{"content":"x"}`, "null")) // clean close, but no [DONE]
	}))
	defer srv.Close()
	rd, err := openai.New(provider.Config{BaseURL: srv.URL + "/v1"}).ChatStream(context.Background(), &provider.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	_, _ = rd.Next()
	if _, err := rd.Next(); err == nil || err == io.EOF {
		t.Fatalf("want an error for a stream with no [DONE], got %v", err)
	}
}
