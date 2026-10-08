package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/anthropic"
	"github.com/abdullah-9211/spillway/internal/provider/providertest"
)

// Fixtures are written to match Anthropic's documented Messages API; they were not recorded from the live API.
const (
	msgText = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-upstream","content":[{"type":"text","text":"Hello there"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	msgTool = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-upstream","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`
)

func ev(w http.ResponseWriter, name, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
	w.(http.Flusher).Flush()
}

func start(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	ev(w, "message_start", `{"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"claude-upstream","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}`)
	ev(w, "ping", `{"type":"ping"}`)
}

func handler(s providertest.Scenario, canceled chan struct{}) http.HandlerFunc {
	apiErr := func(w http.ResponseWriter, status int, typ, msg string) {
		w.Header().Set("Content-Type", "application/json")
		if status == 429 {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch s {
		case providertest.ChatText:
			fmt.Fprint(w, msgText)
		case providertest.ChatToolCall:
			fmt.Fprint(w, msgTool)
		case providertest.StreamText:
			start(w)
			ev(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			ev(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`)
			ev(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}`)
			ev(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
			ev(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)
			ev(w, "message_stop", `{"type":"message_stop"}`)
		case providertest.StreamToolCall:
			start(w)
			ev(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"get_weather","input":{}}}`)
			ev(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
			ev(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`)
			ev(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
			ev(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`)
			ev(w, "message_stop", `{"type":"message_stop"}`)
		case providertest.StreamHang:
			start(w)
			<-r.Context().Done()
			close(canceled)
		case providertest.StreamBroken:
			start(w)
			panic(http.ErrAbortHandler)
		case providertest.Err429:
			apiErr(w, 429, "rate_limit_error", "slow down")
		case providertest.Err500:
			apiErr(w, 500, "api_error", "boom")
		case providertest.Err400:
			apiErr(w, 400, "invalid_request_error", "bad")
		case providertest.Err401:
			apiErr(w, 401, "authentication_error", "invalid x-api-key")
		case providertest.ErrContext:
			apiErr(w, 400, "invalid_request_error", "prompt is too long: 250000 tokens > 200000 maximum")
		}
	}
}

func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Harness{New: func(t *testing.T, s providertest.Scenario) (provider.Provider, <-chan struct{}) {
		canceled := make(chan struct{})
		srv := httptest.NewServer(handler(s, canceled))
		t.Cleanup(srv.Close)
		return anthropic.New(provider.Config{Name: "anthropic", APIKey: "k", BaseURL: srv.URL}), canceled
	}})
}

// send returns the JSON body and headers Anthropic would receive for req.
func send(t *testing.T, req *provider.ChatRequest) (map[string]any, http.Header) {
	t.Helper()
	var body map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		hdr = r.Header
		fmt.Fprint(w, msgText)
	}))
	defer srv.Close()
	if _, err := anthropic.New(provider.Config{APIKey: "sk-ant", BaseURL: srv.URL}).Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return body, hdr
}

func msg(role, text string) provider.Message {
	return provider.Message{Role: role, Content: provider.TextContent(text)}
}

func TestTranslation(t *testing.T) {
	temp := 1.7
	toolMsgs := []provider.Message{
		msg("system", "Be brief."), msg("system", "Use French."),
		msg("user", "weather?"),
		{Role: "assistant", Content: provider.Content{Null: true}, ToolCalls: []provider.ToolCall{
			{ID: "t1", Type: "function", Function: provider.FunctionCall{Name: "a", Arguments: `{"x":1}`}},
			{ID: "t2", Type: "function", Function: provider.FunctionCall{Name: "b", Arguments: ``}},
		}},
		{Role: "tool", ToolCallID: "t1", Content: provider.TextContent("r1")},
		{Role: "tool", ToolCallID: "t2", Content: provider.TextContent("r2")},
		msg("user", "thanks"),
	}
	req := &provider.ChatRequest{
		Model: "claude-x", Messages: toolMsgs, Temperature: &temp, Stop: provider.StringList{"END"},
		Tools:      []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "a", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"a"}}`),
	}
	body, hdr := send(t, req)

	if hdr.Get("x-api-key") != "sk-ant" || hdr.Get("anthropic-version") == "" {
		t.Errorf("headers: %v", hdr)
	}
	if body["system"] != "Be brief.\n\nUse French." {
		t.Errorf("system = %q", body["system"])
	}
	if body["max_tokens"].(float64) != 4096 {
		t.Errorf("max_tokens default = %v", body["max_tokens"])
	}
	if body["temperature"].(float64) != 1 {
		t.Errorf("temperature must be clamped to 1, got %v", body["temperature"])
	}
	if got := body["stop_sequences"].([]any); len(got) != 1 || got[0] != "END" {
		t.Errorf("stop_sequences = %v", got)
	}
	tc := body["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "a" {
		t.Errorf("tool_choice = %v", tc)
	}
	tools := body["tools"].([]any)
	if tools[0].(map[string]any)["input_schema"] == nil {
		t.Errorf("tools must use input_schema: %v", tools)
	}

	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want user, assistant, user (tool results merged), got %d: %v", len(msgs), msgs)
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if len(asst) != 2 || asst[0].(map[string]any)["type"] != "tool_use" {
		t.Errorf("assistant blocks = %v", asst)
	}
	if in := asst[1].(map[string]any)["input"]; fmt.Sprint(in) != "map[]" {
		t.Errorf("empty arguments must become {}, got %v", in)
	}
	last := msgs[2].(map[string]any)["content"].([]any)
	if len(last) != 3 || last[0].(map[string]any)["type"] != "tool_result" || last[2].(map[string]any)["type"] != "text" {
		t.Errorf("tool results then text expected in one user message: %v", last)
	}
}

func TestToolChoiceMapping(t *testing.T) {
	no := false
	tools := []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "a"}}}
	tests := []struct {
		name     string
		choice   string
		parallel *bool
		wantType string
		noPar    bool
	}{
		{"auto", `"auto"`, nil, "auto", false},
		{"required", `"required"`, nil, "any", false},
		{"none", `"none"`, nil, "none", false},
		{"serial tool use", ``, &no, "auto", true},
	}
	for _, tc := range tests {
		req := &provider.ChatRequest{Model: "m", Messages: []provider.Message{msg("user", "x")}, Tools: tools, ParallelToolCalls: tc.parallel}
		if tc.choice != "" {
			req.ToolChoice = json.RawMessage(tc.choice)
		}
		body, _ := send(t, req)
		got := body["tool_choice"].(map[string]any)
		if got["type"] != tc.wantType || (got["disable_parallel_tool_use"] == true) != tc.noPar {
			t.Errorf("%s: tool_choice = %v", tc.name, got)
		}
	}
}

func TestRejectsWhatItCannotTranslate(t *testing.T) {
	img := provider.Message{Role: "user", Content: provider.Content{Parts: []provider.ContentPart{{Type: "image_url"}}}}
	cases := map[string]*provider.ChatRequest{
		"image":         {Model: "m", Messages: []provider.Message{img}},
		"bad tool args": {Model: "m", Messages: []provider.Message{msg("user", "x"), {Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "1", Function: provider.FunctionCall{Name: "a", Arguments: "{nope"}}}}}},
		"no messages":   {Model: "m", Messages: []provider.Message{msg("system", "only system")}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must not reach upstream") }))
	defer srv.Close()
	p := anthropic.New(provider.Config{BaseURL: srv.URL})
	for name, req := range cases {
		if _, err := p.Chat(context.Background(), req); provider.KindOf(err) != provider.KindBadRequest {
			t.Errorf("%s: want KindBadRequest, got %v", name, err)
		}
	}
}

func TestFinishReasons(t *testing.T) {
	for stop, want := range map[string]string{"end_turn": "stop", "max_tokens": "length", "tool_use": "tool_calls", "stop_sequence": "stop"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"id":"m","content":[{"type":"text","text":"x"}],"stop_reason":%q,"usage":{"input_tokens":1,"output_tokens":1}}`, stop)
		}))
		resp, err := anthropic.New(provider.Config{BaseURL: srv.URL}).Chat(context.Background(),
			&provider.ChatRequest{Model: "m", Messages: []provider.Message{msg("user", "x")}})
		srv.Close()
		if err != nil || resp.Choices[0].FinishReason != want {
			t.Errorf("stop_reason %s: got %v, %v want %s", stop, resp, err, want)
		}
	}
}

func TestCacheTokensCountAsPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"m","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":30,"output_tokens":5}}`)
	}))
	defer srv.Close()
	resp, err := anthropic.New(provider.Config{BaseURL: srv.URL}).Chat(context.Background(),
		&provider.ChatRequest{Model: "m", Messages: []provider.Message{msg("user", "x")}})
	if err != nil || resp.Usage.PromptTokens != 60 || resp.Usage.TotalTokens != 65 {
		t.Fatalf("usage = %+v, %v", resp.Usage, err)
	}
}
