package fake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
)

// NewHandler serves the fake over HTTP in the OpenAI wire format, plus two control endpoints:
//
//	POST /_script  body: [{"status":429,"retry_after_ms":1000,"delay_ms":50,"text":"..","break_after":2,"hang":true}, ...]
//	GET  /_requests  the requests received so far
func NewHandler(p *Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { chat(p, w, r) })
	mux.HandleFunc("POST /_script", func(w http.ResponseWriter, r *http.Request) {
		var in []struct {
			Status       int                 `json:"status"`
			RetryAfterMs int                 `json:"retry_after_ms"`
			DelayMs      int                 `json:"delay_ms"`
			Text         string              `json:"text"`
			ToolCalls    []provider.ToolCall `json:"tool_calls"`
			InputTokens  int                 `json:"input_tokens"`
			OutputTokens int                 `json:"output_tokens"`
			BreakAfter   int                 `json:"break_after"`
			Hang         bool                `json:"hang"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, b := range in {
			p.Script(Behavior{
				Status: b.Status, RetryAfter: time.Duration(b.RetryAfterMs) * time.Millisecond,
				Delay: time.Duration(b.DelayMs) * time.Millisecond, Text: b.Text, ToolCalls: b.ToolCalls,
				InputTokens: b.InputTokens, OutputTokens: b.OutputTokens, BreakAfter: b.BreakAfter, Hang: b.Hang,
			})
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_requests", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.Requests())
	})
	return mux
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		if pe.Status != 0 {
			status = pe.Status
		}
		if pe.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(pe.RetryAfter.Round(time.Second).Seconds())))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": err.Error(), "type": "fake_error"}})
}

func chat(p *Provider, w http.ResponseWriter, r *http.Request) {
	var req provider.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !req.Stream {
		resp, err := p.Chat(r.Context(), &req)
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	rd, err := p.ChatStream(r.Context(), &req)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rd.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	for {
		c, err := rd.Next()
		if errors.Is(err, io.EOF) {
			fmt.Fprint(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
			return
		}
		if err != nil {
			// A broken stream: drop the connection without [DONE], as a failing upstream would.
			panic(http.ErrAbortHandler)
		}
		b, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
}
