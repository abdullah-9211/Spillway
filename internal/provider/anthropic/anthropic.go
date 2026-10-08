// Package anthropic is the adapter for the Anthropic Messages API.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/abdullah-9211/spillway/internal/provider"
)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	apiVersion       = "2023-06-01"
	defaultMaxTokens = 4096 // the Messages API requires max_tokens; OpenAI clients often omit it
)

func init() {
	provider.Register("anthropic", func(c provider.Config) (provider.Provider, error) { return New(c), nil })
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
		name = "anthropic"
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
	var ar response
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, provider.WrapTransport(fmt.Errorf("decode anthropic response: %w", err))
	}
	return toChatResponse(&ar), nil
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
	ar, err := toRequest(req, stream)
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	body, err := json.Marshal(ar)
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("anthropic-version", apiVersion)
	if c.apiKey != "" {
		hr.Header.Set("x-api-key", c.apiKey)
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
		Type    string `json:"type"`
		Message string `json:"message"`
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
	lower := strings.ToLower(msg)
	ctxLen := resp.StatusCode == http.StatusBadRequest &&
		(strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "exceed context limit") ||
			strings.Contains(lower, "context window"))
	pe := provider.ClassifyStatus(resp.StatusCode, resp.Header.Get("Retry-After"), ctxLen, msg)
	if eb.Error.Type == "overloaded_error" { // 529
		pe.Kind = provider.KindServer
	}
	return pe
}

// errorFromEvent maps an in-stream `error` event, which carries a type but no HTTP status.
func errorFromEvent(typ, msg string) *provider.ProviderError {
	kind := provider.KindServer
	switch typ {
	case "rate_limit_error":
		kind = provider.KindRateLimited
	case "invalid_request_error":
		kind = provider.KindBadRequest
	case "authentication_error", "permission_error":
		kind = provider.KindAuth
	}
	return &provider.ProviderError{Kind: kind, Err: fmt.Errorf("%s: %s", typ, msg)}
}
