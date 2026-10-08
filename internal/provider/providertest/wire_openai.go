package providertest

import (
	"fmt"
	"net/http"
)

// OpenAIWire plays a Scenario in OpenAI's chat-completions wire format. Adapters for OpenAI and for
// OpenAI-compatible servers share it. Fixtures follow the documented format; they were not recorded live.
// Fixtures are written to match OpenAI's documented wire format; they were not recorded from the live API.
const (
	openaiChatText = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	openaiChatTool = `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
)

func openaiSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprintf(w, "data: %s\n\n", e)
		w.(http.Flusher).Flush()
	}
}

func openaiChunk(delta, finish string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-3","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`, delta, finish)
}

const openaiUsageChunk = `{"id":"chatcmpl-3","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

func OpenAIWire(s Scenario, canceled chan struct{}) http.HandlerFunc {
	apiErr := func(w http.ResponseWriter, status int, code, msg string) {
		w.Header().Set("Content-Type", "application/json")
		if status == 429 {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":%q,"type":"x","code":%q}}`, msg, code)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch s {
		case ChatText:
			fmt.Fprint(w, openaiChatText)
		case ChatToolCall:
			fmt.Fprint(w, openaiChatTool)
		case StreamText:
			openaiSSE(w, openaiChunk(`{"role":"assistant","content":""}`, "null"), openaiChunk(`{"content":"Hello"}`, "null"),
				openaiChunk(`{"content":" there"}`, "null"), openaiChunk(`{}`, `"stop"`), openaiUsageChunk, "[DONE]")
		case StreamToolCall:
			openaiSSE(w, openaiChunk(`{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`, "null"),
				openaiChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`, "null"),
				openaiChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}`, "null"),
				openaiChunk(`{}`, `"tool_calls"`), openaiUsageChunk, "[DONE]")
		case StreamHang:
			openaiSSE(w, openaiChunk(`{"role":"assistant","content":"Hello"}`, "null"))
			<-r.Context().Done()
			close(canceled)
		case StreamBroken:
			openaiSSE(w, openaiChunk(`{"role":"assistant","content":"Hello"}`, "null"))
			panic(http.ErrAbortHandler)
		case Err429:
			apiErr(w, 429, "rate_limit_exceeded", "slow down")
		case Err500:
			apiErr(w, 500, "server_error", "boom")
		case Err400:
			apiErr(w, 400, "invalid_value", "bad")
		case Err401:
			apiErr(w, 401, "invalid_api_key", "bad key")
		case ErrContext:
			apiErr(w, 400, "context_length_exceeded", "This model's maximum context length is 8192 tokens")
		}
	}
}
