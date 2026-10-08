// Package faults makes a provider fail on purpose, for one request. The playground uses it to let an admin watch
// fallback happen. A wrapped provider is never shared: it is built for a request and dropped with it.
package faults

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abdullah-9211/spillway/internal/provider"
)

type Kind string

const (
	RateLimit   Kind = "rate_limit"   // the provider answers 429
	ServerError Kind = "server_error" // the provider answers 503
	Slow        Kind = "slow"         // the provider answers, but late
	CutStream   Kind = "cut_stream"   // a stream breaks partway through
)

var Kinds = []Kind{RateLimit, ServerError, Slow, CutStream}

func (k Kind) Valid() bool {
	for _, v := range Kinds {
		if k == v {
			return true
		}
	}
	return false
}

// Fault is one thing to do to one provider.
type Fault struct {
	Provider string `json:"provider"`
	Kind     Kind   `json:"kind"`
}

const (
	DefaultSlowDelay = 3 * time.Second
	DefaultCutAfter  = 2 // content chunks delivered before a stream breaks
)

// Settings tune the faults; the zero value uses the defaults.
type Settings struct {
	SlowDelay time.Duration
	CutAfter  int
}

func (s Settings) withDefaults() Settings {
	if s.SlowDelay <= 0 {
		s.SlowDelay = DefaultSlowDelay
	}
	if s.CutAfter <= 0 {
		s.CutAfter = DefaultCutAfter
	}
	return s
}

// Wrap returns p with the given fault applied to every call made through it.
func Wrap(p provider.Provider, f Fault, s Settings) provider.Provider {
	return &faulty{inner: p, kind: f.Kind, set: s.withDefaults()}
}

type faulty struct {
	inner provider.Provider
	kind  Kind
	set   Settings
}

func (f *faulty) Name() string { return f.inner.Name() }

func injected(kind provider.ErrKind, status int, msg string, retry time.Duration) *provider.ProviderError {
	return &provider.ProviderError{Kind: kind, Status: status, RetryAfter: retry, Err: errors.New(msg), Injected: true}
}

// early is the failure a fault produces before the provider is called, if it fails at all.
func (f *faulty) early() *provider.ProviderError {
	switch f.kind {
	case RateLimit:
		return injected(provider.KindRateLimited, 429, "injected fault: the provider returned 429", time.Second)
	case ServerError:
		return injected(provider.KindServer, 503, "injected fault: the provider returned 503", 0)
	}
	return nil
}

func (f *faulty) delay(ctx context.Context) error {
	if f.kind != Slow {
		return nil
	}
	t := time.NewTimer(f.set.SlowDelay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return provider.WrapTransport(ctx.Err())
	}
}

func (f *faulty) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	if pe := f.early(); pe != nil {
		return nil, pe
	}
	if err := f.delay(ctx); err != nil {
		return nil, err
	}
	return f.inner.Chat(ctx, req) // cut_stream only applies to streams
}

func (f *faulty) ChatStream(ctx context.Context, req *provider.ChatRequest) (provider.StreamReader, error) {
	if pe := f.early(); pe != nil {
		return nil, pe
	}
	if err := f.delay(ctx); err != nil {
		return nil, err
	}
	rd, err := f.inner.ChatStream(ctx, req)
	if err != nil || f.kind != CutStream {
		return rd, err
	}
	return &cutReader{inner: rd, after: f.set.CutAfter}, nil
}

// cutReader delivers a few content chunks and then fails, as a connection dropped mid-answer would.
type cutReader struct {
	inner provider.StreamReader
	after int
	sent  int
}

func (c *cutReader) Next() (*provider.ChatChunk, error) {
	if c.sent >= c.after {
		return nil, injected(provider.KindServer, 0, fmt.Sprintf("injected fault: the stream was cut after %d chunks", c.after), 0)
	}
	ch, err := c.inner.Next()
	if err != nil {
		return nil, err
	}
	for _, choice := range ch.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			c.sent++
		}
	}
	return ch, nil
}

func (c *cutReader) Close() error { return c.inner.Close() }
