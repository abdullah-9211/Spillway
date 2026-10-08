// Package providertest is the conformance suite every provider.Provider adapter must pass. Each
// adapter supplies a Harness that serves its own wire format for a named Scenario.
package providertest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
)

type Scenario string

const (
	ChatText       Scenario = "chat_text"        // "Hello there", 10 prompt / 5 completion tokens
	ChatToolCall   Scenario = "chat_tool_call"   // get_weather {"city":"Paris"}, id call_1
	StreamText     Scenario = "stream_text"      // "Hello there" in pieces, then usage 10/5
	StreamToolCall Scenario = "stream_tool_call" // same tool call, arguments split across chunks
	StreamHang     Scenario = "stream_hang"      // one chunk, then the server blocks until cancelled
	StreamBroken   Scenario = "stream_broken"    // one chunk, then the connection drops
	Err429         Scenario = "err_429"          // Retry-After: 2
	Err500         Scenario = "err_500"
	Err400         Scenario = "err_400"
	Err401         Scenario = "err_401"
	ErrContext     Scenario = "err_context" // prompt too long
)

// Harness builds a provider wired to a server that plays the scenario. The returned channel, used only
// for StreamHang, is closed when the server sees the request cancelled.
type Harness struct {
	New func(t *testing.T, s Scenario) (p provider.Provider, upstreamCanceled <-chan struct{})
}

func request() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "upstream-model",
		Messages: []provider.Message{{Role: "user", Content: provider.TextContent("Say hello")}},
	}
}

func Run(t *testing.T, h Harness) {
	ctx := context.Background()

	t.Run("chat", func(t *testing.T) {
		p, _ := h.New(t, ChatText)
		resp, err := p.Chat(ctx, request())
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Choices) != 1 || resp.Choices[0].Message.Content.PlainText() != "Hello there" {
			t.Fatalf("unexpected choices: %+v", resp.Choices)
		}
		if resp.Choices[0].Message.Role != "assistant" || resp.Choices[0].FinishReason != "stop" {
			t.Errorf("role/finish: %+v", resp.Choices[0])
		}
		if resp.Usage == nil || resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 15 {
			t.Errorf("usage: %+v", resp.Usage)
		}
	})

	t.Run("chat tool call", func(t *testing.T) {
		p, _ := h.New(t, ChatToolCall)
		resp, err := p.Chat(ctx, request())
		if err != nil {
			t.Fatal(err)
		}
		ch := resp.Choices[0]
		if ch.FinishReason != "tool_calls" || len(ch.Message.ToolCalls) != 1 {
			t.Fatalf("want one tool call, got %+v", ch)
		}
		assertToolCall(t, ch.Message.ToolCalls[0])
	})

	t.Run("stream", func(t *testing.T) {
		p, _ := h.New(t, StreamText)
		rd, err := p.ChatStream(ctx, request())
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Close()
		acc := drain(t, rd)
		if acc.text != "Hello there" || acc.finish != "stop" {
			t.Errorf("text=%q finish=%q", acc.text, acc.finish)
		}
		if acc.usage == nil || acc.usage.PromptTokens != 10 || acc.usage.CompletionTokens != 5 {
			t.Errorf("stream usage: %+v", acc.usage)
		}
		if acc.role != "assistant" {
			t.Errorf("first delta role = %q", acc.role)
		}
	})

	t.Run("stream tool call", func(t *testing.T) {
		p, _ := h.New(t, StreamToolCall)
		rd, err := p.ChatStream(ctx, request())
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Close()
		acc := drain(t, rd)
		if acc.finish != "tool_calls" || len(acc.calls) != 1 {
			t.Fatalf("finish=%q calls=%+v", acc.finish, acc.calls)
		}
		assertToolCall(t, acc.calls[0])
	})

	t.Run("broken stream surfaces an error", func(t *testing.T) {
		p, _ := h.New(t, StreamBroken)
		rd, err := p.ChatStream(ctx, request())
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Close()
		for {
			_, err := rd.Next()
			if err == nil {
				continue
			}
			if errors.Is(err, io.EOF) {
				t.Fatal("a truncated stream must not look like a clean end")
			}
			if provider.KindOf(err) != provider.KindServer {
				t.Errorf("kind = %s, want server", provider.KindOf(err))
			}
			return
		}
	})

	t.Run("close cancels the upstream request", func(t *testing.T) {
		p, canceled := h.New(t, StreamHang)
		rd, err := p.ChatStream(ctx, request())
		if err != nil {
			t.Fatal(err)
		}
		// One chunk proves the stream is open; the server then holds it until cancelled.
		if _, err := rd.Next(); err != nil {
			t.Fatalf("first chunk: %v", err)
		}
		_ = rd.Close()
		select {
		case <-canceled:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream request was not cancelled after Close")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		p, _ := h.New(t, ChatText)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := p.Chat(cctx, request())
		if err == nil || provider.KindOf(err) != provider.KindCanceled {
			t.Fatalf("want KindCanceled, got %v", err)
		}
	})

	errCases := []struct {
		s      Scenario
		kind   provider.ErrKind
		status int
	}{
		{Err429, provider.KindRateLimited, 429},
		{Err500, provider.KindServer, 500},
		{Err400, provider.KindBadRequest, 400},
		{Err401, provider.KindAuth, 401},
		{ErrContext, provider.KindContextLength, 400},
	}
	for _, tc := range errCases {
		t.Run("error "+string(tc.s), func(t *testing.T) {
			for name, call := range map[string]func(p provider.Provider) error{
				"chat":   func(p provider.Provider) error { _, err := p.Chat(ctx, request()); return err },
				"stream": func(p provider.Provider) error { _, err := p.ChatStream(ctx, request()); return err },
			} {
				p, _ := h.New(t, tc.s)
				err := call(p)
				var pe *provider.ProviderError
				if !errors.As(err, &pe) {
					t.Fatalf("%s: want ProviderError, got %v", name, err)
				}
				if pe.Kind != tc.kind || pe.Status != tc.status {
					t.Errorf("%s: kind=%s status=%d, want %s/%d", name, pe.Kind, pe.Status, tc.kind, tc.status)
				}
				if tc.s == Err429 && pe.RetryAfter != 2*time.Second {
					t.Errorf("%s: RetryAfter = %v, want 2s", name, pe.RetryAfter)
				}
			}
		})
	}
}

func assertToolCall(t *testing.T, tc provider.ToolCall) {
	t.Helper()
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call id/name: %+v", tc)
	}
	var got, want map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &got); err != nil {
		t.Fatalf("arguments are not JSON: %q", tc.Function.Arguments)
	}
	_ = json.Unmarshal([]byte(`{"city":"Paris"}`), &want)
	if got["city"] != want["city"] || len(got) != 1 {
		t.Errorf("arguments = %q", tc.Function.Arguments)
	}
}

type accumulated struct {
	role, text, finish string
	calls              []provider.ToolCall
	usage              *provider.Usage
}

// drain reads a stream to its end and assembles the pieces the way a client SDK would.
func drain(t *testing.T, rd provider.StreamReader) accumulated {
	t.Helper()
	var acc accumulated
	for {
		c, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return acc
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		if c.Usage != nil {
			acc.usage = c.Usage
		}
		for _, ch := range c.Choices {
			if ch.Delta.Role != "" && acc.role == "" {
				acc.role = ch.Delta.Role
			}
			if ch.Delta.Content != nil {
				acc.text += *ch.Delta.Content
			}
			for _, tc := range ch.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				for len(acc.calls) <= idx {
					acc.calls = append(acc.calls, provider.ToolCall{})
				}
				cur := &acc.calls[idx]
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Function.Name = tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				acc.finish = *ch.FinishReason
			}
		}
	}
}
