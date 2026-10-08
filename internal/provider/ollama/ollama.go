// Package ollama is the adapter for a local Ollama server. Chat goes through Ollama's OpenAI-compatible
// endpoint, which supports streaming, usage and tool calls for tool-capable models. Embeddings use
// Ollama's native /api/embed.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/provider/openai"
)

const defaultBaseURL = "http://localhost:11434/v1"

func init() {
	provider.Register("ollama", func(c provider.Config) (provider.Provider, error) { return New(c), nil })
}

type Client struct {
	*openai.Client
	root string // server root, without /v1
	hc   *http.Client
}

// New returns a client for Ollama. base_url should end in /v1, as for any OpenAI-compatible server.
func New(c provider.Config) *Client {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{
		Client: openai.NewCompatible(c, "ollama", defaultBaseURL),
		root:   strings.TrimSuffix(base, "/v1"),
		hc:     hc,
	}
}

var _ provider.Embedder = (*Client)(nil)

// Embed returns one vector per input, in order.
func (c *Client) Embed(ctx context.Context, model string, input []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "input": input})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.root+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, &provider.ProviderError{Kind: provider.KindBadRequest, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, provider.WrapTransport(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return nil, provider.ClassifyStatus(resp.StatusCode, resp.Header.Get("Retry-After"), false, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, provider.WrapTransport(err)
	}
	if len(out.Embeddings) != len(input) {
		return nil, &provider.ProviderError{Kind: provider.KindServer, Err: fmt.Errorf("asked for %d embeddings, got %d", len(input), len(out.Embeddings))}
	}
	return out.Embeddings, nil
}
