package google

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/abdullah-9211/spillway/internal/provider"
)

type request struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Tools             []toolSet         `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             string        `json:"text,omitempty"`
	FunctionCall     *functionCall `json:"functionCall,omitempty"`
	FunctionResponse *functionResp `json:"functionResponse,omitempty"`
}

type functionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type functionResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type toolSet struct {
	FunctionDeclarations []funcDecl `json:"functionDeclarations"`
}

type funcDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type generationConfig struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"topP,omitempty"`
	MaxOutputTokens  int      `json:"maxOutputTokens,omitempty"`
	StopSequences    []string `json:"stopSequences,omitempty"`
	ResponseMimeType string   `json:"responseMimeType,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
}

type response struct {
	Candidates     []candidate     `json:"candidates"`
	UsageMetadata  *usageMetadata  `json:"usageMetadata"`
	PromptFeedback *promptFeedback `json:"promptFeedback"`
}

type promptFeedback struct {
	BlockReason string `json:"blockReason"`
}

type candidate struct {
	Content      content `json:"content"`
	FinishReason string  `json:"finishReason"`
}

type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// Thinking tokens are billed as output, so they count toward completion_tokens.
func (u *usageMetadata) toCanonical() *provider.Usage {
	out := u.CandidatesTokenCount + u.ThoughtsTokenCount
	return &provider.Usage{PromptTokens: u.PromptTokenCount, CompletionTokens: out, TotalTokens: u.PromptTokenCount + out}
}

// callID makes up tool call ids, which Gemini does not have. They only need to be unique within a response.
func callID(i int) string { return fmt.Sprintf("call_%d", i+1) }

func toRequest(req *provider.ChatRequest) (*request, error) {
	out := &request{}
	toolNames := map[string]string{} // tool_call_id -> function name, to label tool results
	var system []string

	appendParts := func(role string, parts []part) {
		if n := len(out.Contents); n > 0 && out.Contents[n-1].Role == role {
			out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
			return
		}
		out.Contents = append(out.Contents, content{Role: role, Parts: parts})
	}

	for i, m := range req.Messages {
		if m.Content.HasNonText() {
			return nil, fmt.Errorf("message %d: non-text content is not supported for Gemini models", i)
		}
		text := m.Content.PlainText()
		switch m.Role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			if text != "" {
				appendParts("user", []part{{Text: text}})
			}
		case "assistant":
			var ps []part
			if text != "" {
				ps = append(ps, part{Text: text})
			}
			for _, tc := range m.ToolCalls {
				args := map[string]any{}
				if a := strings.TrimSpace(tc.Function.Arguments); a != "" {
					if err := json.Unmarshal([]byte(a), &args); err != nil {
						return nil, fmt.Errorf("message %d: tool call %q arguments are not a JSON object", i, tc.ID)
					}
				}
				toolNames[tc.ID] = tc.Function.Name
				ps = append(ps, part{FunctionCall: &functionCall{Name: tc.Function.Name, Args: args}})
			}
			if len(ps) > 0 {
				appendParts("model", ps)
			}
		case "tool":
			name := toolNames[m.ToolCallID]
			if name == "" {
				name = m.Name
			}
			if name == "" {
				return nil, fmt.Errorf("message %d: cannot tell which function tool_call_id %q answers", i, m.ToolCallID)
			}
			// Gemini wants an object; wrap plain text results.
			resp := map[string]any{}
			if err := json.Unmarshal([]byte(text), &resp); err != nil {
				resp = map[string]any{"result": text}
			}
			appendParts("user", []part{{FunctionResponse: &functionResp{Name: name, Response: resp}}})
		default:
			return nil, fmt.Errorf("message %d: unsupported role %q", i, m.Role)
		}
	}
	if len(out.Contents) == 0 {
		return nil, errors.New("no user or assistant messages to send")
	}
	if len(system) > 0 {
		out.SystemInstruction = &content{Parts: []part{{Text: strings.Join(system, "\n\n")}}}
	}

	if len(req.Tools) > 0 {
		var decls []funcDecl
		for _, t := range req.Tools {
			decls = append(decls, funcDecl{Name: t.Function.Name, Description: t.Function.Description, Parameters: cleanSchema(t.Function.Parameters)})
		}
		out.Tools = []toolSet{{FunctionDeclarations: decls}}
		tc, err := toToolConfig(req.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.ToolConfig = tc
	}

	gc := &generationConfig{Temperature: req.Temperature, TopP: req.TopP, MaxOutputTokens: req.OutputLimit(),
		StopSequences: req.Stop, Seed: req.Seed}
	if len(req.ResponseFormat) > 0 {
		var rf struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(req.ResponseFormat, &rf) == nil && (rf.Type == "json_object" || rf.Type == "json_schema") {
			gc.ResponseMimeType = "application/json"
		}
	}
	out.GenerationConfig = gc
	return out, nil
}

// cleanSchema drops JSON Schema keywords Gemini's function declarations reject.
func cleanSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			delete(t, "additionalProperties")
			delete(t, "$schema")
			delete(t, "strict")
			for k, vv := range t {
				t[k] = walk(vv)
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return x
	}
	b, err := json.Marshal(walk(v))
	if err != nil {
		return raw
	}
	return b
}

func toToolConfig(raw json.RawMessage) (*toolConfig, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	tc := &toolConfig{}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			tc.FunctionCallingConfig.Mode = "AUTO"
		case "required":
			tc.FunctionCallingConfig.Mode = "ANY"
		case "none":
			tc.FunctionCallingConfig.Mode = "NONE"
		default:
			return nil, fmt.Errorf("unsupported tool_choice %q", s)
		}
		return tc, nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
		return nil, errors.New("unsupported tool_choice")
	}
	tc.FunctionCallingConfig.Mode = "ANY"
	tc.FunctionCallingConfig.AllowedFunctionNames = []string{obj.Function.Name}
	return tc, nil
}

func finishReason(r string, hasTool bool) string {
	switch r {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	}
	if hasTool {
		return "tool_calls"
	}
	return "stop"
}

func toChatResponse(r *response, model string) *provider.ChatResponse {
	msg := provider.Message{Role: "assistant"}
	var text strings.Builder
	finish := "stop"
	if len(r.Candidates) > 0 {
		c := r.Candidates[0]
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				args, _ := json.Marshal(p.FunctionCall.Args)
				msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
					ID: callID(len(msg.ToolCalls)), Type: "function",
					Function: provider.FunctionCall{Name: p.FunctionCall.Name, Arguments: string(args)},
				})
			case p.Text != "":
				text.WriteString(p.Text)
			}
		}
		finish = finishReason(c.FinishReason, len(msg.ToolCalls) > 0)
	}
	if text.Len() == 0 && len(msg.ToolCalls) > 0 {
		msg.Content = provider.Content{Null: true}
	} else {
		msg.Content = provider.TextContent(text.String())
	}
	resp := &provider.ChatResponse{Object: "chat.completion", Model: model,
		Choices: []provider.Choice{{Message: msg, FinishReason: finish}}}
	if r.UsageMetadata != nil {
		resp.Usage = r.UsageMetadata.toCanonical()
	}
	return resp
}
