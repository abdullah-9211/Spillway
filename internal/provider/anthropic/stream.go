package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/abdullah-9211/spillway/internal/provider"
)

type event struct {
	Type         string    `json:"type"`
	Index        int       `json:"index"`
	Message      *response `json:"message"`
	ContentBlock *block    `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// stream converts Anthropic's typed events into OpenAI-style chunks. One event can produce zero or
// more chunks, so they are queued.
type stream struct {
	body   io.ReadCloser
	sse    *provider.SSEReader
	cancel context.CancelFunc
	ctx    context.Context
	model  string

	id        string
	in, out   usage
	toolIndex map[int]int // anthropic content-block index -> OpenAI tool_calls index
	nTools    int
	queue     []*provider.ChatChunk
	done      bool
}

func (s *stream) chunk(d provider.Delta, finish *string) *provider.ChatChunk {
	return &provider.ChatChunk{
		ID: s.id, Object: "chat.completion.chunk", Model: s.model,
		Choices: []provider.ChunkChoice{{Index: 0, Delta: d, FinishReason: finish}},
	}
}

func (s *stream) Next() (*provider.ChatChunk, error) {
	for len(s.queue) == 0 {
		if s.done {
			return nil, io.EOF
		}
		if err := s.readEvent(); err != nil {
			return nil, err
		}
	}
	c := s.queue[0]
	s.queue = s.queue[1:]
	return c, nil
}

func (s *stream) readEvent() error {
	_, data, err := s.sse.Next()
	if err != nil {
		if s.ctx.Err() != nil {
			return &provider.ProviderError{Kind: provider.KindCanceled, Err: s.ctx.Err()}
		}
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF // closed before message_stop
		}
		return provider.WrapTransport(err)
	}
	var ev event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return &provider.ProviderError{Kind: provider.KindServer, Err: err}
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			s.id = ev.Message.ID
			s.in = ev.Message.Usage
		}
		empty := ""
		s.queue = append(s.queue, s.chunk(provider.Delta{Role: "assistant", Content: &empty}, nil))
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			if s.toolIndex == nil {
				s.toolIndex = map[int]int{}
			}
			idx := s.nTools
			s.toolIndex[ev.Index] = idx
			s.nTools++
			s.queue = append(s.queue, s.chunk(provider.Delta{ToolCalls: []provider.ToolCall{{
				Index: &idx, ID: ev.ContentBlock.ID, Type: "function",
				Function: provider.FunctionCall{Name: ev.ContentBlock.Name, Arguments: ""},
			}}}, nil))
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			t := ev.Delta.Text
			s.queue = append(s.queue, s.chunk(provider.Delta{Content: &t}, nil))
		case "input_json_delta":
			idx := s.toolIndex[ev.Index]
			s.queue = append(s.queue, s.chunk(provider.Delta{ToolCalls: []provider.ToolCall{{
				Index: &idx, Function: provider.FunctionCall{Arguments: ev.Delta.PartialJSON},
			}}}, nil))
		}
	case "message_delta":
		if ev.Usage != nil {
			s.out = *ev.Usage
		}
		fr := finishReason(ev.Delta.StopReason)
		s.queue = append(s.queue, s.chunk(provider.Delta{}, &fr))
	case "message_stop":
		u := usage{InputTokens: s.in.InputTokens, OutputTokens: s.out.OutputTokens,
			CacheCreationInputTokens: s.in.CacheCreationInputTokens, CacheReadInputTokens: s.in.CacheReadInputTokens}
		if s.out.InputTokens > 0 { // some responses repeat the final input count in message_delta
			u.InputTokens = s.out.InputTokens
		}
		last := s.chunk(provider.Delta{}, nil)
		last.Choices = []provider.ChunkChoice{}
		last.Usage = u.toCanonical()
		s.queue = append(s.queue, last)
		s.done = true
	case "error":
		if ev.Error != nil {
			return errorFromEvent(ev.Error.Type, ev.Error.Message)
		}
		return &provider.ProviderError{Kind: provider.KindServer, Err: errors.New("anthropic stream error")}
	}
	return nil // ping and unknown events are ignored
}

func (s *stream) Close() error {
	s.cancel()
	return s.body.Close()
}
