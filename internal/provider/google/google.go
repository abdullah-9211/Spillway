// Package google is the adapter for the Gemini generateContent API.
package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/abdullah-9211/spillway/internal/provider"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com"

func init() {
	provider.Register("google", func(c provider.Config) (provider.Provider, error) { return New(c), nil })
}

type Client struct {
	name   string
	apiKey string
	base   string
	hc     *http.Client
}

func New(c provider.Config) *Client {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	name := c.Name
	if name == "" {
		name = "google"
	}
	return &Client{name: name, apiKey: c.APIKey, base: base, hc: hc}
}

func (c *Client) Name() string { return c.name }

func (c *Client) Chat(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	resp, err := c.post(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var gr response
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, provider.WrapTransport(fmt.Errorf("decode gemini response: %w", err))
	}
	if gr.PromptFeedback != nil && gr.PromptFeedback.BlockReason != "" && len(gr.Candidates) == 0 {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: fmt.Errorf("prompt blocked: %s", gr.PromptFeedback.BlockReason)}
	}
	return toChatResponse(&gr, req.Model), nil
}

func (c *Client) ChatStream(ctx context.Context, req *provider.ChatRequest) (provider.StreamReader, error) {
	ctx, cancel := context.WithCancel(ctx)
	resp, err := c.post(ctx, req, true)
	if err != nil {
		cancel()
		return nil, err
	}
	return &stream{body: resp.Body, sse: provider.NewSSEReader(resp.Body), cancel: cancel, ctx: ctx, model: req.Model}, nil
}

func (c *Client) post(ctx context.Context, req *provider.ChatRequest, stream bool) (*http.Response, error) {
	gr, err := toRequest(req)
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	body, err := json.Marshal(gr)
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	method, query := "generateContent", ""
	if stream {
		method, query = "streamGenerateContent", "?alt=sse"
	}
	u := fmt.Sprintf("%s/v1beta/models/%s:%s%s", c.base, url.PathEscape(req.Model), method, query)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	hr.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		hr.Header.Set("x-goog-api-key", c.apiKey) // a header, so the key never lands in a URL or a log
	}
	resp, err := c.hc.Do(hr)
	if err != nil {
		return nil, provider.WrapTransport(err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, errorFromResponse(resp)
	}
	return resp, nil
}

func errorFromResponse(resp *http.Response) *provider.ProviderError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var eb struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &eb)
	msg := eb.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = resp.Status
	}
	lower := strings.ToLower(msg)
	status := resp.StatusCode
	// Gemini reports a bad API key as 400 INVALID_ARGUMENT; it is really an authentication problem.
	if status == http.StatusBadRequest && (strings.Contains(lower, "api key not valid") || strings.Contains(lower, "api_key_invalid")) {
		status = http.StatusUnauthorized
	}
	ctxLen := resp.StatusCode == http.StatusBadRequest &&
		(strings.Contains(lower, "token count") && strings.Contains(lower, "exceeds") || strings.Contains(lower, "context length"))
	pe := provider.ClassifyStatus(status, resp.Header.Get("Retry-After"), ctxLen, msg)
	if eb.Error.Status == "RESOURCE_EXHAUSTED" {
		pe.Kind = provider.KindRateLimited
	}
	return pe
}

type stream struct {
	body   io.ReadCloser
	sse    *provider.SSEReader
	cancel context.CancelFunc
	ctx    context.Context
	model  string

	started bool
	queue   []*provider.ChatChunk
	usage   *provider.Usage
	nTools  int
	done    bool
	finish  string
}

func (s *stream) chunk(d provider.Delta, finish *string) *provider.ChatChunk {
	return &provider.ChatChunk{Object: "chat.completion.chunk", Model: s.model,
		Choices: []provider.ChunkChoice{{Delta: d, FinishReason: finish}}}
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
			// Gemini ends the stream by closing it. That is only clean after a finishReason arrived.
			if s.finish != "" {
				s.flushEnd()
				return nil
			}
			err = io.ErrUnexpectedEOF
		}
		return provider.WrapTransport(err)
	}
	var gr response
	if err := json.Unmarshal([]byte(data), &gr); err != nil {
		return &provider.ProviderError{Kind: provider.KindServer, Err: fmt.Errorf("bad stream chunk: %w", err)}
	}
	if !s.started {
		s.started = true
		empty := ""
		s.queue = append(s.queue, s.chunk(provider.Delta{Role: "assistant", Content: &empty}, nil))
	}
	if gr.UsageMetadata != nil {
		s.usage = gr.UsageMetadata.toCanonical()
	}
	if len(gr.Candidates) == 0 {
		return nil
	}
	cand := gr.Candidates[0]
	hasTool := false
	for _, p := range cand.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			hasTool = true
			idx := s.nTools
			s.nTools++
			args, _ := json.Marshal(p.FunctionCall.Args)
			s.queue = append(s.queue, s.chunk(provider.Delta{ToolCalls: []provider.ToolCall{{
				Index: &idx, ID: callID(idx), Type: "function",
				Function: provider.FunctionCall{Name: p.FunctionCall.Name, Arguments: string(args)},
			}}}, nil))
		case p.Text != "":
			t := p.Text
			s.queue = append(s.queue, s.chunk(provider.Delta{Content: &t}, nil))
		}
	}
	if cand.FinishReason != "" {
		fr := finishReason(cand.FinishReason, hasTool || s.nTools > 0)
		s.finish = fr
		s.queue = append(s.queue, s.chunk(provider.Delta{}, &fr))
	}
	return nil
}

// flushEnd emits the usage-only chunk and marks the stream finished.
func (s *stream) flushEnd() {
	last := &provider.ChatChunk{Object: "chat.completion.chunk", Model: s.model, Choices: []provider.ChunkChoice{}}
	if s.usage != nil {
		last.Usage = s.usage
	} else {
		last.Usage = &provider.Usage{}
	}
	s.queue = append(s.queue, last)
	s.done = true
}

func (s *stream) Close() error {
	s.cancel()
	return s.body.Close()
}
