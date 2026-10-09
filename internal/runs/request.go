package runs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/abdullah-9211/spillway/internal/money"
	"github.com/abdullah-9211/spillway/internal/provider"
)

// Input is the run's task: a string, or a messages array when the caller brings earlier turns.
type Input struct {
	Text     string
	Messages []provider.Message
}

func (i *Input) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &i.Text)
	}
	return json.Unmarshal(b, &i.Messages)
}

func (i Input) MarshalJSON() ([]byte, error) {
	if i.Messages != nil {
		return json.Marshal(i.Messages)
	}
	return json.Marshal(i.Text)
}

type RequestLimits struct {
	MaxSteps        *int         `json:"max_steps,omitempty"`
	MaxCostUSD      *json.Number `json:"max_cost_usd,omitempty"`
	DeadlineSeconds *int         `json:"deadline_seconds,omitempty"`
}

type CompactionConfig struct {
	ThresholdTokens int `json:"threshold_tokens,omitempty"`
	KeepLastTurns   int `json:"keep_last_turns,omitempty"`
}

// Request is the body of POST /v1/runs.
type Request struct {
	Input            Input             `json:"input"`
	System           string            `json:"system,omitempty"`
	Model            string            `json:"model,omitempty"`
	Tools            []string          `json:"tools,omitempty"`
	ApprovalRequired []string          `json:"approval_required,omitempty"`
	Limits           *RequestLimits    `json:"limits,omitempty"`
	Compaction       *CompactionConfig `json:"compaction,omitempty"`
	Metadata         json.RawMessage   `json:"metadata,omitempty"`
}

// ModelName is the policy or model the run's calls ask for.
func (r Request) ModelName() string {
	if r.Model == "" {
		return "default"
	}
	return r.Model
}

const maxInputBytes = 512 << 10

// Validation is a problem with the request itself, reported to the caller as a 400.
type Validation struct{ Param, Message string }

func (v *Validation) Error() string { return v.Param + ": " + v.Message }

func bad(param, format string, a ...any) error {
	return &Validation{Param: param, Message: fmt.Sprintf(format, a...)}
}

// ValidateOptions says what this deployment can do, so a request is refused up front rather than failing later.
type ValidateOptions struct {
	Known      func(model string) bool // a model id or policy name in the catalog
	ToolsReady bool                    // false until the tool registry exists
}

// Validate checks the request. It does not mutate it.
func (r Request) Validate(o ValidateOptions) error {
	switch {
	case r.Input.Messages != nil:
		if len(r.Input.Messages) == 0 {
			return bad("input", "give at least one message")
		}
		for i, m := range r.Input.Messages {
			if m.Role != "user" && m.Role != "assistant" {
				return bad("input", "message %d has role %q; use user or assistant (the system prompt is the system field)", i, m.Role)
			}
			if strings.TrimSpace(m.Content.PlainText()) == "" || m.Content.HasNonText() {
				return bad("input", "message %d needs text content", i)
			}
		}
		if r.Input.Messages[len(r.Input.Messages)-1].Role != "user" {
			return bad("input", "the last message must be from the user")
		}
	case strings.TrimSpace(r.Input.Text) == "":
		return bad("input", "input is required: a string, or a messages array")
	}
	size := len(r.Input.Text) + len(r.System)
	for _, m := range r.Input.Messages {
		size += len(m.Content.PlainText())
	}
	if size > maxInputBytes {
		return bad("input", "input and system together are limited to %d KB", maxInputBytes>>10)
	}
	if o.Known != nil && !o.Known(r.ModelName()) {
		return bad("model", "unknown model or policy %q", r.ModelName())
	}
	if len(r.Tools) > 0 && !o.ToolsReady {
		return bad("tools", "tools are not available yet")
	}
	for _, t := range append(append([]string(nil), r.Tools...), r.ApprovalRequired...) {
		if strings.TrimSpace(t) == "" {
			return bad("tools", "a tool name is empty")
		}
	}
	if l := r.Limits; l != nil {
		if l.MaxSteps != nil && *l.MaxSteps < 1 {
			return bad("limits.max_steps", "must be at least 1")
		}
		if l.DeadlineSeconds != nil && *l.DeadlineSeconds < 1 {
			return bad("limits.deadline_seconds", "must be at least 1")
		}
		if l.MaxCostUSD != nil {
			c, err := money.ParseUSD(l.MaxCostUSD.String())
			if err != nil || c <= 0 {
				return bad("limits.max_cost_usd", "must be a positive dollar amount")
			}
		}
	}
	return nil
}

// Caps are the server's ceilings. A request may ask for less, never more.
type Caps struct {
	MaxSteps int
	MaxCost  money.Micros
	Deadline time.Duration
}

// ResolveLimits is the request's limits capped by the server's, with the caps as the defaults.
func (r Request) ResolveLimits(c Caps) Limits {
	out := Limits(c)
	l := r.Limits
	if l == nil {
		return out
	}
	if l.MaxSteps != nil && *l.MaxSteps < out.MaxSteps {
		out.MaxSteps = *l.MaxSteps
	}
	if l.MaxCostUSD != nil {
		if v, err := money.ParseUSD(l.MaxCostUSD.String()); err == nil && v < out.MaxCost {
			out.MaxCost = v
		}
	}
	if l.DeadlineSeconds != nil && time.Duration(*l.DeadlineSeconds)*time.Second < out.Deadline {
		out.Deadline = time.Duration(*l.DeadlineSeconds) * time.Second
	}
	return out
}

// InitialMessages is the conversation a fresh run starts from.
func (r Request) InitialMessages() []provider.Message {
	var out []provider.Message
	if r.System != "" {
		out = append(out, provider.Message{Role: "system", Content: provider.TextContent(r.System)})
	}
	if r.Input.Messages != nil {
		return append(out, r.Input.Messages...)
	}
	return append(out, provider.Message{Role: "user", Content: provider.TextContent(r.Input.Text)})
}
