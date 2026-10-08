// Package fake is a scriptable provider for tests, the chaos test and the load-test baseline. It works
// in-process as a provider.Provider and, through NewHandler, as an HTTP server speaking the OpenAI API.
package fake

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
)

// Behavior says how the fake answers one request. The zero value is a plain successful answer.
type Behavior struct {
	Status       int           // non-zero: fail with this HTTP status (429, 500, 503, 400, 401 ...)
	RetryAfter   time.Duration // sent with a 429
	Delay        time.Duration // wait before answering (before the first chunk when streaming)
	Text         string        // answer text; default "Hello from the fake provider"
	ToolCalls    []provider.ToolCall
	InputTokens  int  // default 10
	OutputTokens int  // default 5
	BreakAfter   int  // streaming: fail after this many content chunks; 0 means never
	Hang         bool // streaming: send one chunk, then block until the request is cancelled
}

const defaultText = "Hello from the fake provider"

// Provider is the in-process fake. Behaviors are consumed in order; Default applies once they run out.
type Provider struct {
	name string

	mu       sync.Mutex
	script   []Behavior
	Default  Behavior
	requests []*provider.ChatRequest

	canceled atomic.Int64
	seq      atomic.Int64
}

func New(name string) *Provider { return &Provider{name: name} }

func (p *Provider) Name() string { return p.name }

// Script appends behaviors for the next requests.
func (p *Provider) Script(bs ...Behavior) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script = append(p.script, bs...)
}

// Requests returns the requests received so far, as the fake saw them.
func (p *Provider) Requests() []*provider.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*provider.ChatRequest(nil), p.requests...)
}

// Canceled counts requests whose context ended while the fake was still working on them.
func (p *Provider) Canceled() int { return int(p.canceled.Load()) }

func (p *Provider) next(req *provider.ChatRequest) Behavior {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := *req
	p.requests = append(p.requests, &cp)
	if len(p.script) > 0 {
		b := p.script[0]
		p.script = p.script[1:]
		return b
	}
	return p.Default
}

func (b Behavior) tokens() (int, int) {
	in, out := b.InputTokens, b.OutputTokens
	if in == 0 {
		in = 10
	}
	if out == 0 {
		out = 5
	}
	return in, out
}

func (b Behavior) text() string {
	if b.Text == "" && len(b.ToolCalls) == 0 {
		return defaultText
	}
	return b.Text
}

func (b Behavior) failure() *provider.ProviderError {
	if b.Status == 0 {
		return nil
	}
	return provider.ClassifyStatus(b.Status, fmt.Sprint(b.RetryAfter.Seconds()), false, fmt.Sprintf("fake provider scripted status %d", b.Status))
}

func (p *Provider) wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		p.canceled.Add(1)
		return &provider.ProviderError{Kind: provider.KindCanceled, Err: err}
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		p.canceled.Add(1)
		return &provider.ProviderError{Kind: provider.KindCanceled, Err: ctx.Err()}
	}
}

func (p *Provider) id() string { return fmt.Sprintf("chatcmpl-fake-%d", p.seq.Add(1)) }

func (p *Provider) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	b := p.next(req)
	if err := p.wait(ctx, b.Delay); err != nil {
		return nil, err
	}
	if pe := b.failure(); pe != nil {
		return nil, pe
	}
	in, out := b.tokens()
	msg := provider.Message{Role: "assistant", Content: provider.TextContent(b.text()), ToolCalls: b.ToolCalls}
	finish := "stop"
	if len(b.ToolCalls) > 0 {
		finish = "tool_calls"
		if b.Text == "" {
			msg.Content = provider.Content{Null: true}
		}
	}
	return &provider.ChatResponse{
		ID: p.id(), Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []provider.Choice{{Message: msg, FinishReason: finish}},
		Usage:   &provider.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out},
	}, nil
}

func (p *Provider) ChatStream(ctx context.Context, req *provider.ChatRequest) (provider.StreamReader, error) {
	b := p.next(req)
	if err := p.wait(ctx, b.Delay); err != nil {
		return nil, err
	}
	if pe := b.failure(); pe != nil {
		return nil, pe
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &stream{p: p, ctx: ctx, cancel: cancel, id: p.id(), model: req.Model, b: b}
	s.build()
	return s, nil
}

type stream struct {
	p      *Provider
	ctx    context.Context
	cancel context.CancelFunc
	id     string
	model  string
	b      Behavior

	chunks []*provider.ChatChunk
	pos    int
	sent   int // content chunks delivered

	counted atomic.Bool
}

func (s *stream) chunk(d provider.Delta, finish *string) *provider.ChatChunk {
	return &provider.ChatChunk{ID: s.id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: s.model,
		Choices: []provider.ChunkChoice{{Delta: d, FinishReason: finish}}}
}

func (s *stream) build() {
	empty := ""
	s.chunks = append(s.chunks, s.chunk(provider.Delta{Role: "assistant", Content: &empty}, nil))
	if t := s.b.text(); t != "" {
		for _, w := range strings.SplitAfter(t, " ") {
			w := w
			s.chunks = append(s.chunks, s.chunk(provider.Delta{Content: &w}, nil))
		}
	}
	for i, tc := range s.b.ToolCalls {
		i := i
		tc.Index = &i
		s.chunks = append(s.chunks, s.chunk(provider.Delta{ToolCalls: []provider.ToolCall{tc}}, nil))
	}
	finish := "stop"
	if len(s.b.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	s.chunks = append(s.chunks, s.chunk(provider.Delta{}, &finish))
	in, out := s.b.tokens()
	last := &provider.ChatChunk{ID: s.id, Object: "chat.completion.chunk", Created: time.Now().Unix(), Model: s.model,
		Choices: []provider.ChunkChoice{}, Usage: &provider.Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out}}
	s.chunks = append(s.chunks, last)
}

func (s *stream) Next() (*provider.ChatChunk, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindCanceled, Err: err}
	}
	if s.b.Hang && s.pos >= 1 {
		<-s.ctx.Done() // hold the stream open until the caller cancels
		s.markCanceled()
		return nil, &provider.ProviderError{Kind: provider.KindCanceled, Err: s.ctx.Err()}
	}
	if s.b.BreakAfter > 0 && s.sent >= s.b.BreakAfter {
		return nil, &provider.ProviderError{Kind: provider.KindServer, Err: io.ErrUnexpectedEOF}
	}
	if s.pos >= len(s.chunks) {
		return nil, io.EOF
	}
	c := s.chunks[s.pos]
	s.pos++
	if len(c.Choices) > 0 && c.Choices[0].Delta.Content != nil && *c.Choices[0].Delta.Content != "" {
		s.sent++
	}
	return c, nil
}

// markCanceled counts the stream once, however it learns that the caller went away.
func (s *stream) markCanceled() {
	if s.counted.CompareAndSwap(false, true) {
		s.p.canceled.Add(1)
	}
}

func (s *stream) Close() error {
	if s.pos < len(s.chunks) { // closed before the end: the upstream request is cut short
		s.markCanceled()
	}
	s.cancel()
	return nil
}
