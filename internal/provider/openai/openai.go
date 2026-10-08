// Package openai is the adapter for the OpenAI chat-completions API and any server that speaks it.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/abdullah-9211/spillway/internal/provider"
)

const defaultBaseURL = "https://api.openai.com/v1"

func init() {
	provider.Register("openai", func(c provider.Config) (provider.Provider, error) { return New(c), nil })
}

type Client struct {
	name   string
	apiKey string
	base   string
	hc     *http.Client
}

func New(c provider.Config) *Client { return NewCompatible(c, "openai", defaultBaseURL) }

// NewCompatible builds a client for any server that speaks the OpenAI chat API (Ollama, vLLM, ...),
// with its own default name and base URL.
func NewCompatible(c provider.Config, defaultName, defaultBase string) *Client {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = defaultBase
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{} // no overall timeout: streams are bounded by the request context
	}
	name := c.Name
	if name == "" {
		name = defaultName
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
	var out provider.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, provider.WrapTransport(fmt.Errorf("decode openai response: %w", err))
	}
	return &out, nil
}

func (c *Client) ChatStream(ctx context.Context, req *provider.ChatRequest) (provider.StreamReader, error) {
	ctx, cancel := context.WithCancel(ctx)
	resp, err := c.post(ctx, req, true)
	if err != nil {
		cancel()
		return nil, err
	}
	return &stream{body: resp.Body, sse: provider.NewSSEReader(resp.Body), cancel: cancel, ctx: ctx}, nil
}

// post sends the request. The caller owns the returned body on success.
func (c *Client) post(ctx context.Context, req *provider.ChatRequest, stream bool) (*http.Response, error) {
	up := *req
	up.Stream = stream
	if stream {
		up.StreamOptions = &provider.StreamOptions{IncludeUsage: true} // token counts for streams
	} else {
		up.StreamOptions = nil
	}
	body, err := json.Marshal(&up)
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	hr.Header.Set("Content-Type", "application/json")
	if stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	if c.apiKey != "" {
		hr.Header.Set("Authorization", "Bearer "+c.apiKey)
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

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
		Type    string `json:"type"`
	} `json:"error"`
}

func errorFromResponse(resp *http.Response) *provider.ProviderError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var eb errorBody
	_ = json.Unmarshal(raw, &eb)
	msg := eb.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	if msg == "" {
		msg = resp.Status
	}
	code, _ := eb.Error.Code.(string)
	ctxLen := code == "context_length_exceeded" || strings.Contains(strings.ToLower(msg), "maximum context length")
	return provider.ClassifyStatus(resp.StatusCode, resp.Header.Get("Retry-After"), ctxLen, msg)
}

type stream struct {
	body   io.ReadCloser
	sse    *provider.SSEReader
	cancel context.CancelFunc
	ctx    context.Context
}

func (s *stream) Next() (*provider.ChatChunk, error) {
	_, data, err := s.sse.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			// The stream ended without [DONE]: the connection was cut.
			err = io.ErrUnexpectedEOF
		}
		if s.ctx.Err() != nil {
			return nil, &provider.ProviderError{Kind: provider.KindCanceled, Err: s.ctx.Err()}
		}
		return nil, provider.WrapTransport(err)
	}
	if data == "[DONE]" {
		return nil, io.EOF
	}
	var probe struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &probe) == nil && probe.Error != nil {
		return nil, &provider.ProviderError{Kind: provider.KindServer, Err: errors.New(probe.Error.Message)}
	}
	var chunk provider.ChatChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindServer, Err: fmt.Errorf("bad stream chunk: %w", err)}
	}
	return &chunk, nil
}

func (s *stream) Close() error {
	s.cancel() // cancels the upstream request so the provider stops generating
	return s.body.Close()
}
