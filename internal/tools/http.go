package tools

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/runs"
)

// MaxResponseBytes caps what a tool's answer may add to the conversation.
const MaxResponseBytes = 256 << 10

// SigningSecretHeader may be stored among a tool's headers to give that tool its own signing secret. It is used to sign
// and is never sent.
const SigningSecretHeader = "x-spillway-signing-secret"

// Source finds registered tools by name.
type Source interface {
	Get(ctx context.Context, names []string) ([]Tool, error)
}

// Executor calls registered HTTP tools. It satisfies runs.ToolRunner.
type Executor struct {
	Source        Source
	Client        *http.Client
	WebhookSecret string // signs every call that has no secret of its own; empty means unsigned
	MaxResponse   int    // default MaxResponseBytes
	MCP           *MCPClient
}

var _ runs.ToolRunner = (*Executor)(nil)

func (e *Executor) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *Executor) maxResponse() int {
	if e.MaxResponse > 0 {
		return e.MaxResponse
	}
	return MaxResponseBytes
}

// Specs describes the named tools to the model.
func (e *Executor) Specs(names []string) ([]provider.Tool, error) {
	ts, err := e.Source.Get(context.Background(), names)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Tool, 0, len(ts))
	for _, t := range ts {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, provider.Tool{Type: "function", Function: provider.ToolFunction{Name: runs.ModelName(t.Name), Description: t.Description, Parameters: schema}})
	}
	return out, nil
}

// RequiresApproval says whether calls to the named tool wait for a person. An MCP tool takes the setting of its server.
func (e *Executor) RequiresApproval(ctx context.Context, name string) (bool, error) {
	ts, err := e.Source.Get(ctx, []string{name})
	if err != nil {
		return false, err
	}
	return ts[0].RequiresApproval, nil
}

var _ runs.ApprovalPolicy = (*Executor)(nil)

// Discover fetches an MCP server's tool list.
func (e *Executor) Discover(ctx context.Context, t Tool) ([]MCPToolInfo, error) {
	if t.Kind != MCP {
		return nil, fmt.Errorf("tool %q is not an MCP server", t.Name)
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return e.mcp().ListTools(cctx, t.Endpoint, t.Headers)
}

func (e *Executor) mcp() *MCPClient {
	if e.MCP == nil {
		e.MCP = &MCPClient{HTTP: e.Client}
	}
	return e.MCP
}

// Sign is the value of X-Spillway-Signature for a body: "sha256=" and the hex HMAC-SHA256 of the body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Verify checks a signature in constant time. A receiver should call it before acting.
func Verify(secret string, body []byte, signature string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(signature))
}

type callBody struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	RunID     string          `json:"run_id"`
	StepNo    int             `json:"step_no"`
}

// Call makes one tool call. The idempotency key is sent as Idempotency-Key: a step re-issued after a crash sends the
// same key, and it is up to the receiver to apply the effect only once. A non-2xx answer, a timeout or an unreadable
// reply is an error, which the engine feeds back to the model.
func (e *Executor) Call(ctx context.Context, inv runs.ToolInvocation) (string, error) {
	ts, err := e.Source.Get(ctx, []string{inv.Name})
	if err != nil {
		return "", err
	}
	t := ts[0]
	if t.Kind == MCP {
		return e.callMCP(ctx, t, inv)
	}
	args := inv.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	body, err := json.Marshal(callBody{Tool: t.Name, Arguments: args, RunID: inv.RunID.String(), StepNo: inv.StepNo})
	if err != nil {
		return "", err
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	signing := e.WebhookSecret
	for k, v := range t.Headers {
		if strings.EqualFold(k, SigningSecretHeader) {
			signing = v
			continue
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", inv.IdempotencyKey)
	if signing != "" {
		req.Header.Set("X-Spillway-Signature", Sign(signing, body))
	}

	resp, err := e.client().Do(req)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return "", fmt.Errorf("tool %q timed out after %s", t.Name, timeout)
		}
		return "", fmt.Errorf("tool %q: %w", t.Name, err)
	}
	defer resp.Body.Close()
	limit := e.maxResponse()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return "", fmt.Errorf("tool %q timed out after %s", t.Name, timeout)
		}
		return "", fmt.Errorf("tool %q: reading the answer: %w", t.Name, err)
	}
	truncated := len(raw) > limit
	if truncated {
		raw = raw[:limit]
	}
	text := string(raw)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("tool %q answered %d: %s", t.Name, resp.StatusCode, snippet(text))
	}
	if truncated {
		text += fmt.Sprintf("\n[truncated: the answer was longer than %d bytes]", limit)
	}
	return text, nil
}

func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func (e *Executor) callMCP(ctx context.Context, t Tool, inv runs.ToolInvocation) (string, error) {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	extra := map[string]string{"Idempotency-Key": inv.IdempotencyKey}
	signing := e.WebhookSecret
	for k, v := range t.Headers {
		if strings.EqualFold(k, SigningSecretHeader) {
			signing = v
		}
	}
	if signing != "" {
		extra["X-Spillway-Signature"] = Sign(signing, []byte(inv.IdempotencyKey))
	}
	text, err := e.mcp().CallTool(cctx, t.Endpoint, t.Headers, t.MCPTool, inv.Arguments, inv.IdempotencyKey, extra)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return "", fmt.Errorf("tool %q timed out after %s", t.Name, timeout)
		}
		return "", fmt.Errorf("tool %q: %w", t.Name, err)
	}
	if limit := e.maxResponse(); len(text) > limit {
		text = text[:limit] + fmt.Sprintf("\n[truncated: the answer was longer than %d bytes]", limit)
	}
	return text, nil
}

// DiscoverAndStore fetches a server's tools and stores the list on its registry row. A server that cannot be read is a
// *DiscoverError and the stored list is left as it was.
func DiscoverAndStore(ctx context.Context, st *Store, e *Executor, id uuid.UUID, now time.Time) (Tool, error) {
	t, err := st.ByID(ctx, id)
	if err != nil {
		return Tool{}, err
	}
	if t.Kind != MCP {
		return Tool{}, fmt.Errorf("%w: %q is not an MCP server", ErrInvalid, t.Name)
	}
	list, err := e.Discover(ctx, t)
	if err != nil {
		return Tool{}, &DiscoverError{Err: err}
	}
	if err := st.SetDiscovered(ctx, id, list, now); err != nil {
		return Tool{}, err
	}
	return st.ByID(ctx, id)
}
