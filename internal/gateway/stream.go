package gateway

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/usage"
)

// Stream is a streaming call in progress. The caller reads chunks with Next and must call Finish exactly once.
type Stream struct {
	c          *call
	rd         provider.StreamReader
	attempts   []usage.Attempt
	wantUsage  bool // the client asked for stream_options.include_usage
	started    time.Time
	ttfb       *int
	overhead   *time.Duration
	usage      *provider.Usage
	chars      int // characters of generated text, for estimating output tokens
	inputChars int
	finished   bool
	requestID  string

	// What was generated, kept so a clean finish can fill the cache. A broken stream is never cached.
	text   strings.Builder
	calls  []provider.ToolCall
	finish string

	hit   usage.CacheStatus // set when the answer came from a cache
	entry *Entry
}

// Attempts is the number of provider calls made to get this stream going; 0 for a cache hit.
func (s *Stream) Attempts() int { return countAttempts(s.attempts) }

// Provider and Model say which model is answering.
func (s *Stream) Provider() string { return s.c.model.Provider }
func (s *Stream) Model() string    { return s.c.model.ID }

// Cache says how the cache was involved: hit_exact, hit_semantic, miss or bypass.
func (s *Stream) Cache() usage.CacheStatus {
	if s.hit != "" {
		return s.hit
	}
	return s.c.cacheStatus
}

// ChatStream opens an upstream stream and reads its first chunk, retrying and failing over until one
// candidate has produced data. Only then does it return, so a total failure is still a plain HTTP error.
func (g *Gateway) ChatStream(ctx context.Context, key keys.Key, id uuid.UUID, bucket string, req *provider.ChatRequest) (*Stream, error) {
	ctx, c, err := g.begin(ctx, key, id, bucket, req)
	if err != nil {
		return nil, err
	}
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	s := &Stream{c: c, started: g.now(), requestID: "chatcmpl-" + id.String(),
		wantUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage, inputChars: requestChars(req)}

	if e, status := c.lookup(ctx); e != nil {
		c.useHit(e)
		s.hit, s.entry = status, e
		s.rd = replay(e, s.requestID)
		return s, nil
	}

	c.plan.HedgeAfter = 0 // hedging is for non-streaming requests only
	res, err := g.exec.run(ctx, c.plan, g.exec.tryStream(ctx, req))
	if err != nil {
		return nil, c.fail(err)
	}
	c.model = res.Winner
	s.rd, s.attempts = res.Value.(*primedStream), res.Attempts
	if countAttempts(res.Attempts) <= 1 {
		var upstream time.Duration
		for _, a := range res.Attempts {
			upstream += time.Duration(a.LatencyMs) * time.Millisecond
		}
		o := max(g.now().Sub(c.started)-upstream, 0)
		s.overhead = &o
	}
	return s, nil
}

// Next returns the next chunk to send to the client, io.EOF at the normal end, or a provider error.
func (s *Stream) Next() (*provider.ChatChunk, error) {
	for {
		ch, err := s.rd.Next()
		if err != nil {
			return nil, err
		}
		if s.ttfb == nil {
			ms := int(s.c.g.now().Sub(s.c.started).Milliseconds())
			s.ttfb = &ms
		}
		if ch.Usage != nil {
			s.usage = ch.Usage
		}
		s.collect(ch)
		ch.Model = s.c.model.ID
		ch.Object = "chat.completion.chunk"
		if ch.ID == "" {
			ch.ID = s.requestID
		}
		if ch.Created == 0 {
			ch.Created = s.c.g.now().Unix()
		}
		// We always ask the provider for usage. Only forward the usage-only chunk if the client wanted it,
		// since SDK code that reads choices[0] would trip over a chunk with no choices.
		if len(ch.Choices) == 0 && !s.wantUsage {
			continue
		}
		if !s.wantUsage {
			ch.Usage = nil
		}
		return ch, nil
	}
}

// collect accumulates what the stream says, the way a client SDK would.
func (s *Stream) collect(ch *provider.ChatChunk) {
	for _, choice := range ch.Choices {
		if choice.Delta.Content != nil {
			s.chars += len(*choice.Delta.Content)
			s.text.WriteString(*choice.Delta.Content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			s.chars += len(tc.Function.Arguments)
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			for len(s.calls) <= idx {
				s.calls = append(s.calls, provider.ToolCall{Type: "function"})
			}
			cur := &s.calls[idx]
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Function.Name = tc.Function.Name
			}
			cur.Function.Arguments += tc.Function.Arguments
		}
		if choice.FinishReason != nil {
			s.finish = *choice.FinishReason
		}
	}
}

// Finish closes the upstream stream, which cancels its request, and records usage. err is nil for a clean
// end. Calling Finish more than once is harmless.
func (s *Stream) Finish(ctx context.Context, err error) {
	if s.finished {
		return
	}
	s.finished = true
	_ = s.rd.Close()

	if s.hit != "" { // a cache hit costs nothing and saves what the original call cost
		zero := money.Micros(0)
		s.c.record(finished{outcome: outcomeFor(ctx, err), cost: &zero, saved: s.entry.Cost, cache: s.hit, ttfb: s.ttfb})
		return
	}

	outcome := outcomeFor(ctx, err)
	var in, out int
	attempts := append([]usage.Attempt(nil), s.attempts...)
	last := &attempts[winnerIndex(attempts)]
	last.LatencyMs = int(s.c.g.now().Sub(s.started).Milliseconds())
	if s.usage != nil {
		in, out = s.usage.PromptTokens, s.usage.CompletionTokens
	} else {
		in, out, last.Estimated = estimate(s.inputChars), estimate(s.chars), true
	}
	if err != nil {
		last.Error, last.ErrorKind = err.Error(), provider.KindOf(err).String()
	}
	cost := s.c.record(finished{outcome: outcome, in: in, out: out, ttfb: s.ttfb, attempts: attempts, overhead: s.overhead})

	if outcome == usage.OutcomeOK && s.usage != nil {
		msg := provider.Message{Role: "assistant", Content: provider.TextContent(s.text.String()), ToolCalls: s.calls}
		if s.text.Len() == 0 && len(s.calls) > 0 {
			msg.Content = provider.Content{Null: true}
		}
		s.c.fill(provider.ChatResponse{
			ID: s.requestID, Object: "chat.completion", Created: s.c.g.now().Unix(), Model: s.c.model.ID,
			Choices: []provider.Choice{{Message: msg, FinishReason: s.finish}},
			Usage:   s.usage,
		}, cost)
	}
}

func outcomeFor(ctx context.Context, err error) usage.Outcome {
	switch {
	case err == nil:
		return usage.OutcomeOK
	case ctx.Err() != nil || provider.KindOf(err) == provider.KindCanceled:
		return usage.OutcomeClientCancelled
	}
	return usage.OutcomeUpstreamError
}

// ErrorPayload builds the error sent mid-stream, after which the stream ends with [DONE].
func ErrorPayload(m string) map[string]any {
	return map[string]any{"error": map[string]any{"message": m, "type": "api_error", "code": "upstream_error", "param": nil}}
}

// replay turns a cached completion back into the chunks a live stream would have produced, with no pacing.
func replay(e *Entry, id string) provider.StreamReader {
	r := &replayReader{}
	add := func(d provider.Delta, finish *string) {
		r.chunks = append(r.chunks, &provider.ChatChunk{ID: id, Object: "chat.completion.chunk", Model: e.Model,
			Choices: []provider.ChunkChoice{{Delta: d, FinishReason: finish}}})
	}
	empty := ""
	add(provider.Delta{Role: "assistant", Content: &empty}, nil)
	if len(e.Response.Choices) > 0 {
		ch := e.Response.Choices[0]
		if t := ch.Message.Content.PlainText(); t != "" {
			add(provider.Delta{Content: &t}, nil)
		}
		for i, tc := range ch.Message.ToolCalls {
			i, tc := i, tc
			tc.Index = &i
			add(provider.Delta{ToolCalls: []provider.ToolCall{tc}}, nil)
		}
		fr := ch.FinishReason
		add(provider.Delta{}, &fr)
	}
	if e.Response.Usage != nil {
		r.chunks = append(r.chunks, &provider.ChatChunk{ID: id, Object: "chat.completion.chunk", Model: e.Model,
			Choices: []provider.ChunkChoice{}, Usage: e.Response.Usage})
	}
	return r
}

type replayReader struct {
	chunks []*provider.ChatChunk
	pos    int
}

func (r *replayReader) Next() (*provider.ChatChunk, error) {
	if r.pos >= len(r.chunks) {
		return nil, io.EOF
	}
	c := r.chunks[r.pos]
	r.pos++
	return c, nil
}

func (r *replayReader) Close() error { return nil }
