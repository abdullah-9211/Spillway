package google_test

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
	"github.com/abdullah-9211/spillway/internal/provider/google"
	"github.com/abdullah-9211/spillway/internal/provider/providertest"
)

// Fixtures follow Google's documented generateContent format; they were not recorded from the live API.
const (
	textBody = `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`
	toolBody = `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`
)

func event(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()
}

func part(text string) string {
	return fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"text":%q}]},"index":0}]}`, text)
}

func handler(s providertest.Scenario, canceled chan struct{}) http.HandlerFunc {
	apiErr := func(w http.ResponseWriter, status int, st, msg string) {
		w.Header().Set("Content-Type", "application/json")
		if status == 429 {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"status":%q}}`, status, msg, st)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch s {
		case providertest.ChatText:
			fmt.Fprint(w, textBody)
		case providertest.ChatToolCall:
			fmt.Fprint(w, toolBody)
		case providertest.StreamText:
			event(w, part("Hello"))
			event(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":" there"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`)
		case providertest.StreamToolCall:
			event(w, toolBody)
		case providertest.StreamHang:
			event(w, part("Hello"))
			<-r.Context().Done()
			close(canceled)
		case providertest.StreamBroken:
			event(w, part("Hello"))
			panic(http.ErrAbortHandler)
		case providertest.Err429:
			apiErr(w, 429, "RESOURCE_EXHAUSTED", "quota")
		case providertest.Err500:
			apiErr(w, 500, "INTERNAL", "boom")
		case providertest.Err400:
			apiErr(w, 400, "INVALID_ARGUMENT", "bad field")
		case providertest.Err401:
			apiErr(w, 400, "INVALID_ARGUMENT", "API key not valid. Please pass a valid API key.")
		case providertest.ErrContext:
			apiErr(w, 400, "INVALID_ARGUMENT", "The input token count (250000) exceeds the maximum number of tokens allowed (200000).")
		}
	}
}

func TestConformance(t *testing.T) {
	providertest.Run(t, providertest.Harness{New: func(t *testing.T, s providertest.Scenario) (provider.Provider, <-chan struct{}) {
		canceled := make(chan struct{})
		srv := httptest.NewServer(handler(s, canceled))
		t.Cleanup(srv.Close)
		return google.New(provider.Config{Name: "google", APIKey: "k", BaseURL: srv.URL}), canceled
	}})
}

func capture(t *testing.T, stream bool, req *provider.ChatRequest) (body map[string]any, path, query, keyHeader string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		path, query, keyHeader = r.URL.Path, r.URL.RawQuery, r.Header.Get("x-goog-api-key")
		if stream {
			event(w, `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`)
			return
		}
		fmt.Fprint(w, textBody)
	}))
	defer srv.Close()
	p := google.New(provider.Config{APIKey: "secret", BaseURL: srv.URL})
	if stream {
		rd, err := p.ChatStream(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		_ = rd.Close()
	} else if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return body, path, query, keyHeader
}

func msg(role, text string) provider.Message {
	return provider.Message{Role: role, Content: provider.TextContent(text)}
}

func TestRequestShape(t *testing.T) {
	temp := 0.2
	max := 100
	req := &provider.ChatRequest{
		Model: "gemini-x", Temperature: &temp, MaxTokens: &max, Stop: provider.StringList{"END"},
		Messages: []provider.Message{msg("system", "Be brief."), msg("user", "hi"), msg("assistant", "hello"), msg("user", "again")},
	}
	body, path, query, key := capture(t, false, req)
	if path != "/v1beta/models/gemini-x:generateContent" || query != "" {
		t.Errorf("url = %s?%s", path, query)
	}
	if key != "secret" {
		t.Errorf("the key belongs in a header, got %q", key)
	}
	if si := body["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"]; si != "Be brief." {
		t.Errorf("systemInstruction = %v", si)
	}
	contents := body["contents"].([]any)
	roles := []string{}
	for _, c := range contents {
		roles = append(roles, c.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "user,model,user" {
		t.Errorf("roles = %v (assistant must become model)", roles)
	}
	gc := body["generationConfig"].(map[string]any)
	if gc["temperature"].(float64) != 0.2 || gc["maxOutputTokens"].(float64) != 100 || gc["stopSequences"].([]any)[0] != "END" {
		t.Errorf("generationConfig = %v", gc)
	}
}

func TestStreamURL(t *testing.T) {
	_, path, query, _ := capture(t, true, &provider.ChatRequest{Model: "gemini-x", Messages: []provider.Message{msg("user", "hi")}})
	if path != "/v1beta/models/gemini-x:streamGenerateContent" || query != "alt=sse" {
		t.Errorf("url = %s?%s", path, query)
	}
}

func TestToolRoundTripTranslation(t *testing.T) {
	req := &provider.ChatRequest{
		Model: "g",
		Messages: []provider.Message{
			msg("user", "weather?"),
			{Role: "assistant", Content: provider.Content{Null: true}, ToolCalls: []provider.ToolCall{
				{ID: "call_1", Type: "function", Function: provider.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}},
			{Role: "tool", ToolCallID: "call_1", Content: provider.TextContent(`{"temp":21}`)},
			msg("user", "thanks"),
		},
		Tools: []provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "get_weather",
			Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"city":{"type":"string"}}}`)}}},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`),
	}
	body, _, _, _ := capture(t, false, req)

	contents := body["contents"].([]any)
	model := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if fc := model["functionCall"].(map[string]any); fc["name"] != "get_weather" || fc["args"].(map[string]any)["city"] != "Paris" {
		t.Errorf("functionCall = %v", fc)
	}
	user := contents[2].(map[string]any)["parts"].([]any)
	fr := user[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "get_weather" || fr["response"].(map[string]any)["temp"].(float64) != 21 {
		t.Errorf("functionResponse = %v (the name comes from the earlier tool call)", fr)
	}
	if len(user) != 2 {
		t.Errorf("tool result and the next user text share one turn: %v", user)
	}
	decl := body["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	if strings.Contains(fmt.Sprint(decl["parameters"]), "additionalProperties") {
		t.Errorf("unsupported schema keywords must be dropped: %v", decl["parameters"])
	}
	cfg := body["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
	if cfg["mode"] != "ANY" || cfg["allowedFunctionNames"].([]any)[0] != "get_weather" {
		t.Errorf("toolConfig = %v", cfg)
	}
}

func TestPlainTextToolResultIsWrapped(t *testing.T) {
	req := &provider.ChatRequest{Model: "g", Messages: []provider.Message{
		msg("user", "x"),
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Function: provider.FunctionCall{Name: "f", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: provider.TextContent("sunny")},
	}}
	body, _, _, _ := capture(t, false, req)
	last := body["contents"].([]any)[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if last["response"].(map[string]any)["result"] != "sunny" {
		t.Errorf("response = %v", last)
	}
}

func TestRejectsWhatItCannotTranslate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must not reach upstream") }))
	defer srv.Close()
	p := google.New(provider.Config{BaseURL: srv.URL})
	img := provider.Message{Role: "user", Content: provider.Content{Parts: []provider.ContentPart{{Type: "image_url"}}}}
	cases := map[string]*provider.ChatRequest{
		"image":              {Model: "g", Messages: []provider.Message{img}},
		"orphan tool result": {Model: "g", Messages: []provider.Message{msg("user", "x"), {Role: "tool", ToolCallID: "nope", Content: provider.TextContent("r")}}},
		"no messages":        {Model: "g", Messages: []provider.Message{msg("system", "only")}},
	}
	for name, req := range cases {
		if _, err := p.Chat(context.Background(), req); provider.KindOf(err) != provider.KindBadRequest {
			t.Errorf("%s: want KindBadRequest, got %v", name, err)
		}
	}
}

func TestSafetyBlockIsABadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	}))
	defer srv.Close()
	_, err := google.New(provider.Config{BaseURL: srv.URL}).Chat(context.Background(),
		&provider.ChatRequest{Model: "g", Messages: []provider.Message{msg("user", "x")}})
	if provider.KindOf(err) != provider.KindBadRequest || !strings.Contains(err.Error(), "SAFETY") {
		t.Errorf("err = %v", err)
	}
}

func TestFinishReasonsAndThinkingTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"cut"}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":6,"thoughtsTokenCount":10,"totalTokenCount":20}}`)
	}))
	defer srv.Close()
	resp, err := google.New(provider.Config{BaseURL: srv.URL}).Chat(context.Background(),
		&provider.ChatRequest{Model: "g", Messages: []provider.Message{msg("user", "x")}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].FinishReason != "length" {
		t.Errorf("finish = %s", resp.Choices[0].FinishReason)
	}
	if resp.Usage.CompletionTokens != 16 || resp.Usage.TotalTokens != 20 {
		t.Errorf("thinking tokens are billed as output: %+v", resp.Usage)
	}
}
