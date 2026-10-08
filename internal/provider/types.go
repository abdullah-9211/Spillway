// Package provider defines the canonical request and response types, which follow the OpenAI
// chat-completions schema, and the Provider interface every adapter implements.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Provider is one upstream LLM backend. Adapters translate between the canonical types and the
// provider's wire format. The request's Model is already the provider's own model id.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req *ChatRequest) (StreamReader, error)
}

// StreamReader yields chunks until io.EOF. Close must cancel the upstream request.
type StreamReader interface {
	Next() (*ChatChunk, error)
	Close() error
}

type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                StringList      `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	Seed                *int            `json:"seed,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	N                   *int            `json:"n,omitempty"`
	User                string          `json:"user,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// OutputLimit returns the requested cap on generated tokens, or 0 when none was given.
func (r *ChatRequest) OutputLimit() int {
	switch {
	case r.MaxCompletionTokens != nil:
		return *r.MaxCompletionTokens
	case r.MaxTokens != nil:
		return *r.MaxTokens
	}
	return 0
}

// StringList accepts a JSON string or an array of strings (the OpenAI `stop` field).
type StringList []string

func (s *StringList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if string(b) == "null" {
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Content is a message body: a plain string, an array of parts, or null (an assistant turn that
// only calls tools).
type Content struct {
	Text  string
	Parts []ContentPart
	Null  bool
}

type ContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

func TextContent(s string) Content { return Content{Text: s} }

// PlainText returns the text of the content, joining text parts. Non-text parts are skipped.
func (c Content) PlainText() string {
	if c.Parts == nil {
		return c.Text
	}
	var out string
	for _, p := range c.Parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out
}

// HasNonText reports whether any part is not plain text.
func (c Content) HasNonText() bool {
	for _, p := range c.Parts {
		if p.Type != "text" {
			return true
		}
	}
	return false
}

func (c Content) MarshalJSON() ([]byte, error) {
	switch {
	case c.Null:
		return []byte("null"), nil
	case c.Parts != nil:
		return json.Marshal(c.Parts)
	}
	return json.Marshal(c.Text)
}

func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*c = Content{}
	switch {
	case string(b) == "null":
		c.Null = true
	case len(b) > 0 && b[0] == '[':
		var parts []ContentPart
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		if parts == nil {
			parts = []ContentPart{}
		}
		c.Parts = parts
	case len(b) > 0 && b[0] == '"':
		return json.Unmarshal(b, &c.Text)
	default:
		return fmt.Errorf("message content must be a string, an array or null")
	}
	return nil
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ToolCall struct {
	// Index is set only inside streaming deltas.
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type ChatResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatChunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	Choices           []ChunkChoice `json:"choices"`
	Usage             *Usage        `json:"usage,omitempty"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Embedder is implemented by providers that can embed text. Ollama is required to; others may.
type Embedder interface {
	Embed(ctx context.Context, model string, input []string) ([][]float32, error)
}
