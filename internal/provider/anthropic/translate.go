package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/abdullah-9211/spillway/internal/provider"
)

type request struct {
	Model         string         `json:"model"`
	MaxTokens     int            `json:"max_tokens"`
	System        string         `json:"system,omitempty"`
	Messages      []message      `json:"messages"`
	Tools         []tool         `json:"tools,omitempty"`
	ToolChoice    map[string]any `json:"tool_choice,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	StopSequences []string       `json:"stop_sequences,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// promptTokens counts cached tokens as prompt tokens, as OpenAI's prompt_tokens does.
func (u usage) toCanonical() *provider.Usage {
	in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	return &provider.Usage{PromptTokens: in, CompletionTokens: u.OutputTokens, TotalTokens: in + u.OutputTokens}
}

type response struct {
	ID         string  `json:"id"`
	Model      string  `json:"model"`
	Content    []block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      usage   `json:"usage"`
}

func toRequest(req *provider.ChatRequest, stream bool) (*request, error) {
	out := &request{
		Model:       req.Model,
		MaxTokens:   req.OutputLimit(),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultMaxTokens
	}
	if out.Temperature != nil && *out.Temperature > 1 { // Anthropic's range is 0..1, OpenAI's 0..2
		one := 1.0
		out.Temperature = &one
	}
	out.StopSequences = req.Stop

	var system []string
	appendBlocks := func(role string, blocks []block) {
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			return
		}
		out.Messages = append(out.Messages, message{Role: role, Content: blocks})
	}

	for i, m := range req.Messages {
		if m.Content.HasNonText() {
			return nil, fmt.Errorf("message %d: non-text content is not supported for Anthropic models", i)
		}
		text := m.Content.PlainText()
		switch m.Role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			if text != "" {
				appendBlocks("user", []block{{Type: "text", Text: text}})
			}
		case "assistant":
			var bs []block
			if text != "" {
				bs = append(bs, block{Type: "text", Text: text})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(strings.TrimSpace(tc.Function.Arguments))
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				if !json.Valid(input) {
					return nil, fmt.Errorf("message %d: tool call %q arguments are not valid JSON", i, tc.ID)
				}
				bs = append(bs, block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			if len(bs) > 0 {
				appendBlocks("assistant", bs)
			}
		case "tool":
			// Tool results travel in a user message; consecutive results share one message.
			appendBlocks("user", []block{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: text}})
		default:
			return nil, fmt.Errorf("message %d: unsupported role %q", i, m.Role)
		}
	}
	out.System = strings.Join(system, "\n\n")
	if len(out.Messages) == 0 {
		return nil, errors.New("no user or assistant messages to send")
	}

	for _, t := range req.Tools {
		if t.Type != "function" {
			return nil, fmt.Errorf("unsupported tool type %q", t.Type)
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, tool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	if len(out.Tools) > 0 {
		tc, err := toolChoice(req.ToolChoice, req.ParallelToolCalls)
		if err != nil {
			return nil, err
		}
		out.ToolChoice = tc
	}
	return out, nil
}

func toolChoice(raw json.RawMessage, parallel *bool) (map[string]any, error) {
	var tc map[string]any
	if len(raw) > 0 {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch s {
			case "auto":
				tc = map[string]any{"type": "auto"}
			case "required":
				tc = map[string]any{"type": "any"}
			case "none":
				tc = map[string]any{"type": "none"}
			default:
				return nil, fmt.Errorf("unsupported tool_choice %q", s)
			}
		} else {
			var obj struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
				return nil, errors.New("unsupported tool_choice")
			}
			tc = map[string]any{"type": "tool", "name": obj.Function.Name}
		}
	}
	if parallel != nil && !*parallel {
		if tc == nil {
			tc = map[string]any{"type": "auto"}
		}
		if tc["type"] != "none" {
			tc["disable_parallel_tool_use"] = true
		}
	}
	return tc, nil
}

func finishReason(stop string) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop" // end_turn, stop_sequence, pause_turn and anything new
}

func toChatResponse(r *response) *provider.ChatResponse {
	msg := provider.Message{Role: "assistant"}
	var text strings.Builder
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := strings.TrimSpace(string(b.Input))
			if args == "" {
				args = "{}"
			}
			msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
				ID: b.ID, Type: "function", Function: provider.FunctionCall{Name: b.Name, Arguments: args},
			})
		}
	}
	if text.Len() == 0 && len(msg.ToolCalls) > 0 {
		msg.Content = provider.Content{Null: true}
	} else {
		msg.Content = provider.TextContent(text.String())
	}
	return &provider.ChatResponse{
		ID:      r.ID,
		Object:  "chat.completion",
		Model:   r.Model,
		Choices: []provider.Choice{{Index: 0, Message: msg, FinishReason: finishReason(r.StopReason)}},
		Usage:   r.Usage.toCanonical(),
	}
}
